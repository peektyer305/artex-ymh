package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/hook"
)

// RoEConfig wires the RoE authorization-scope check (F5 / AUDIT-REPORT H2) into
// the PreToolUse chain. All fields are optional; a nil Scope disables the check
// entirely (現行セマンティクスを維持).
type RoEConfig struct {
	// Mode returns the enforcement mode from settings キー roe_enforcement:
	// "off" | "warn" | "strict"。空/不明な値は warn（デフォルト）。
	Mode func() string
	// Scope returns the task's registered scope rules; server の組み立て時に
	// db task_scope から取得する。空リスト = 未登録範囲 → Unknown → 通過。
	Scope func(taskID int64) []ScopeRule
	// TaskIDOf extracts the task id from the hook ctx (server が
	// agent.RunInfoFrom を注入。guard は agent パッケージに逆依存しない)。nil → 0 → Unknown。
	TaskIDOf func(ctx context.Context) int64
}

// SetRoE installs the RoE scope check. Zero RoEConfig disables it.
func (g *Guard) SetRoE(cfg RoEConfig) {
	g.mu.Lock()
	g.roe = cfg
	g.mu.Unlock()
}

// roeMode normalizes the configured mode; デフォルトは warn。
func (c RoEConfig) mode() string {
	if c.Mode == nil {
		return "warn"
	}
	switch strings.ToLower(strings.TrimSpace(c.Mode())) {
	case "off":
		return "off"
	case "strict":
		return "strict"
	default:
		return "warn"
	}
}

// checkRoE is the PreToolUse RoE scope gate: extract interaction targets from
// the tool call and compare them against the task's registered scope. Returns
// stop=true with a hook.Result when the call is gated (strict ask / block).
// In / Unknown / no-target all pass through.
func (g *Guard) checkRoE(ctx context.Context, ev hook.Event, cmd string) (hook.Result, bool) {
	g.mu.Lock()
	cfg := g.roe
	g.mu.Unlock()
	if cfg.Scope == nil {
		return hook.Result{}, false
	}
	mode := cfg.mode()
	if mode == "off" {
		return hook.Result{}, false
	}
	var taskID int64
	if cfg.TaskIDOf != nil {
		taskID = cfg.TaskIDOf(ctx)
	}
	if taskID <= 0 {
		return hook.Result{}, false // タスク実行以外（chat など）で照合する範囲がない → Unknown → 通過
	}
	hosts := roeHosts(ev.ToolName, ev.Input, cmd)
	if len(hosts) == 0 {
		return hook.Result{}, false // ターゲットを抽出できない = ターゲットなし。ブロックしない（保守的方針）
	}
	matcher := NewScopeMatcher(cfg.Scope(taskID))
	var out []string
	unknown := false
	for _, h := range hosts {
		switch matcher.Check(h) {
		case ScopeOut:
			out = append(out, h)
		case ScopeUnknown:
			unknown = true
		}
	}
	if len(out) == 0 {
		return hook.Result{}, false // In または Unknown → 通過
	}
	if unknown {
		// 一部のターゲットは登録範囲外、一部は適用できるルールがない —— 既存の違反は Out として扱えばよい。
		log.Printf("[guard][roe] task %d 一部のターゲットに適用できる範囲ルールがありません: %v", taskID, hosts)
	}
	reason := roeMessage(out, cfg.Scope(taskID))
	if mode == "strict" {
		return g.roeAsk(ctx, ev, reason)
	}
	// warn:通過させるが、guard の監査 + ログは記録する。
	g.record(ev.ToolName, "allow", reason, cmd)
	log.Printf("[guard][roe] task %d 範囲外のターゲット（warn で通過）: %s", taskID, reason)
	return hook.Result{}, false
}

// roeAsk routes a strict-mode out-of-scope call through the same human-approval
// machinery as intercept ask rules; without an interceptor it hard-blocks.
func (g *Guard) roeAsk(ctx context.Context, ev hook.Event, reason string) (hook.Result, bool) {
	if g.interceptor == nil {
		return g.block(ev.ToolName, systemBlockMessage(reason), ""), true
	}
	if ctx.Err() != nil {
		return g.block(ev.ToolName, systemBlockMessage("作業はキャンセルされました。プラットフォームの安全制御により実行を阻止します"), ""), true
	}
	dec := intercept.Decision{Action: "ask", RuleName: "ビルトイン RoE 範囲強制", Message: reason}
	convID := intercept.ConvIDFromContext(ctx)
	if !g.interceptor.HandleAsk(ctx, convID, dec, ev.ToolName, ev.Input) {
		return g.block(ev.ToolName, systemBlockMessage("RoE 範囲外のターゲットです。手動承認が得られませんでした（ユーザーによる拒否または承認タイムアウト）"), ""), true
	}
	// 承認通過は RoE のこの関門だけを免除する。他のインターセプトルールは通常どおり評価する（RoE 承認で deny ルールを迂回できない）。
	g.interceptor.Log(ctx, convID, dec, ev.ToolName, ev.Input, "allowed")
	return hook.Result{}, false
}

// roeMessage builds the human/agent-facing justification for an out-of-scope
// verdict: 命中した host、登録された範囲ルール、無効化/緩和の方法。
func roeMessage(out []string, rules []ScopeRule) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "RoE 認可範囲チェック:ターゲット %s はこのタスクの登録範囲外です", strings.Join(out, ", "))
	if len(rules) > 0 {
		parts := make([]string, 0, len(rules))
		for _, r := range rules {
			parts = append(parts, r.Kind+"="+r.Value)
		}
		fmt.Fprintf(&sb, "（登録範囲: %s）", strings.Join(parts, ", "))
	}
	sb.WriteString("。通過させるには:ターゲットをタスク範囲に追加する（add_task_scope）か、システム設定で roe_enforcement を warn（警告のみ）/off（無効）に変更してください。")
	return sb.String()
}

// roeHosts returns the interaction targets of a tool call: ExtractHosts over
// the shell surface for Bash 系ツール、WebFetch は URL の host。
func roeHosts(toolName string, input []byte, cmd string) []string {
	if cmd != "" {
		return ExtractHosts(cmd)
	}
	if toolName == "WebFetch" {
		var in struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(input, &in) == nil && in.URL != "" {
			if u, err := url.Parse(in.URL); err == nil && u.Hostname() != "" {
				return []string{u.Hostname()}
			}
		}
	}
	return nil
}
