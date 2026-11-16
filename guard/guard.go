// Package guard implements the safety boundary layer (docs §11): an audit log,
// user-configured intercept-rule evaluation, RoE authorization-scope enforcement
// (F5, see roe.go — Bash/HTTP ツールの相互作用ターゲットとタスクの task_scope の照合), and
// Observer/G5 failure attribution. Every tool call passes through the PreToolUse
// hook before executing. Destructive/exfil gating is no longer hard-coded here —
// it lives in the DB intercept rules (seeded as ordinary built-in rules, so users
// can disable or delete them), evaluated via applyIntercept.
package guard

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"sync"
	"time"

	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/hook"
)

// AuditEntry records one gated tool call.
type AuditEntry struct {
	TS      int64  `json:"ts"`
	Tool    string `json:"tool"`
	Action  string `json:"action"` // allow|block
	Reason  string `json:"reason,omitempty"`
	Command string `json:"command,omitempty"`
}

// Guard enforces the side-effect policy via agent-core hooks.
type Guard struct {
	mu          sync.Mutex
	audit       []AuditEntry
	attrib      map[string]int // failure attribution counts (Observer / G5)
	reg         *hook.Registry
	interceptor *intercept.Interceptor // optional; nil disables user-configured rules
	roe         RoEConfig              // optional; Scope==nil disables the RoE check
	egress      *EgressGuard           // optional; バッチ 5 B2 出口審査（機密フィンガープリントのインターセプト）、nil で無効
	tarpit      *tarpitState           // optional; バッチ 5 B3 tarpit サーキットブレーカー、nil で無効
}

// New creates a Guard without user-configured intercept rules (used for pentest
// tasks where the Interceptor is not yet available).
func New() *Guard { return newGuard(nil) }

// NewWithInterceptor creates a Guard with user-configured intercept rules.
func NewWithInterceptor(ic *intercept.Interceptor) *Guard { return newGuard(ic) }

func newGuard(ic *intercept.Interceptor) *Guard {
	g := &Guard{attrib: map[string]int{}, interceptor: ic}
	g.reg = hook.NewRegistry().
		On(hook.PreToolUse, g.preToolUse).
		On(hook.PostToolUse, g.postToolUse)
	return g
}

// Hooks returns the hook registry to attach to an agent session.
func (g *Guard) Hooks() *hook.Registry { return g.reg }

func (g *Guard) preToolUse(ctx context.Context, ev hook.Event) hook.Result {
	// Extract the shell-command surface for the audit log: Bash + the interactive-shell
	// tools (shell_open's command, shell_send's text). Destructive/exfil gating is no
	// longer hard-coded here — it now lives in the DB intercept rules, evaluated by
	// applyIntercept below. Other tools record an empty command.
	var cmd string
	switch ev.ToolName {
	case "Bash", "shell_open":
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(ev.Input, &in)
		cmd = in.Command
	case "shell_send":
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Input, &in)
		cmd = in.Text
	}
	// バッチ 5 B2 出口審査:送信内容にプラットフォームの機密フィンガープリント（LLM key/jwt.key/gate パス/
	// PG パスワード/callback_addr）が含まれれば deny。他のあらゆるチェックより先に評価する（下の allow 監査も含む——
	// 命中時はコマンド原文自体に秘密が含まれ得るため、保存できない）。この種の deny はインターセプトルールを
	// 通さず、一括許可の対象にもならない。監査には kind のみを記録し、フィンガープリント/原文は残さない。
	if res, stop := g.checkEgress(ev, cmd); stop {
		return res
	}
	g.record(ev.ToolName, "allow", "", cmd)
	// RoE 認可範囲チェック（F5）:Bash/HTTP 系ツールの相互作用ターゲットを task_scope と照合し、
	// ユーザーのインターセプトルールより先に評価する。Out+strict は ask、Out+warn は監査のみ記録。
	if res, stop := g.checkRoE(ctx, ev, cmd); stop {
		return res
	}
	// バッチ 5 B3 tarpit サーキットブレーカー：host ごとに取得回数を数え、予算超過時に一度だけ hint を書いて
	// planner に促す。以降その host へは warn で通過させる（ハード block はせず、実在する大規模サイトの誤爆を防ぐ）。
	g.checkTarpit(ev)
	return g.applyIntercept(ctx, ev)
}

// SetEgress は出口審査の機密フィンガープリント集合を設定する（server 側のプロセス内で 1 つを共有。egress.go 参照）。
// nil = 無効。
func (g *Guard) SetEgress(e *EgressGuard) {
	g.mu.Lock()
	g.egress = e
	g.mu.Unlock()
}

// checkEgress は PreToolUse の最初の関門：Bash 系コマンド全文と WebFetch の url/body に
// 機密フィンガープリントの検査を行い、命中 → deny（固定文言でブロックする）。
func (g *Guard) checkEgress(ev hook.Event, cmd string) (hook.Result, bool) {
	g.mu.Lock()
	e := g.egress
	g.mu.Unlock()
	if e == nil {
		return hook.Result{}, false
	}
	var texts []string
	if cmd != "" {
		texts = append(texts, cmd)
	}
	if ev.ToolName == "WebFetch" {
		var in struct {
			URL  string `json:"url"`
			Body string `json:"body"`
		}
		if json.Unmarshal(ev.Input, &in) == nil {
			texts = append(texts, in.URL, in.Body)
		}
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		if kind, hit := e.Check(text); hit {
			// 監査/ログには kind のみを記録する:コマンド原文自体に機密が含まれ得るため保存しない。
			// block の文言にもヒット内容を含めず、モデルへフィンガープリントをエコーしない。
			log.Printf("[guard][egress] プラットフォームの機密フィンガープリント(%s)に命中しました。%s の外部送信をブロックしました", kind, ev.ToolName)
			return g.block(ev.ToolName, systemBlockMessage("操作包含平台敏感信息，禁止外发"), ""), true
		}
	}
	return hook.Result{}, false
}

// applyIntercept evaluates user-configured intercept rules against the tool call.
// Both rules and the fallback judge receive the complete tool input.
func (g *Guard) applyIntercept(ctx context.Context, ev hook.Event) hook.Result {
	if g.interceptor == nil {
		return hook.Result{}
	}
	if !g.interceptor.IsToolEnabled(ev.ToolName) {
		return hook.Result{}
	}
	ctx = intercept.WithCall(ctx, ev.ToolName, ev.Input)
	dec, matched := g.interceptor.Match(ev.ToolName, ev.Input)
	if !matched {
		// No rule matched. Ask the LLM fallback judge (if enabled); when it is off
		// or unwired, keep current behavior and allow.
		d, judged := g.interceptor.Judge(ctx, ev.ToolName, ev.Input)
		if !judged {
			return hook.Result{}
		}
		dec = d
	}
	switch dec.Action {
	case "deny":
		// 観測：deny 命中は承認を待たず、denied を 1 件記録する（履歴/タスクのインターセプトページで参照可能）。
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), dec, ev.ToolName, ev.Input, "denied")
		return g.block(ev.ToolName, systemBlockMessage(dec.Message), "")
	case "allow":
		// Record explicit rule and model approvals so review details remain auditable.
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), dec, ev.ToolName, ev.Input, "allowed")
		return hook.Result{}
	case "ask":
		// If the worker context is already cancelled (task stopped / killed), block
		// immediately without creating a pending record — avoids orphaned DB entries
		// and makes execOne complete fast, reducing the race against drainSynthetic.
		if ctx.Err() != nil {
			return g.block(ev.ToolName, systemBlockMessage("作業はキャンセルされました。プラットフォームの安全制御により実行を阻止します"), "")
		}
		convID := intercept.ConvIDFromContext(ctx)
		if !g.interceptor.HandleAsk(ctx, convID, dec, ev.ToolName, ev.Input) {
			return g.block(ev.ToolName, systemBlockMessage("手動承認が得られませんでした（ユーザーによる拒否または承認タイムアウト）"), "")
		}
		return hook.Result{}
	}
	return hook.Result{}
}

// systemBlockMessage frames an intercept block as an ARTEX platform-governance
// decision so the agent does not mistake it for a target-side defense.
//
// The bare reasons ("tool execution forbidden" / "user denied") read exactly like
// a WAF/403 on the target, so a pentest agent's instinct is to bypass them — rewrite
// the command, swap the payload, re-encode, retry. That is both futile (the platform
// blocks the class of action, not one string) and wrong (it's a policy decision,
// not an obstacle to defeat). This prefix states plainly that the block comes
// from the platform, is not the target's protection, and that the operation is
// forbidden — so the agent pivots to another approach instead of evading it.
// Audit/history rows keep the raw reason (see Interceptor.Log); only the
// model-facing tool_result carries this framing.
func systemBlockMessage(reason string) string {
	return "【ARTEX プラットフォーム制御・対象側の防御ではありません】この呼び出しはプラットフォームによりブロックされました。" +
		"理由：" + reason + "。この操作は禁止されています。"
}

var reBlocked = regexp.MustCompile(`(?i)\b(403|forbidden|waf|blocked|rate.?limit|429|captcha|denied)\b`)

// postToolUse is the Observer failure-attribution hook (G5): it classifies tool
// results into blocked / error / ok so the planner can change strategy instead
// of giving up at a WAF.
func (g *Guard) postToolUse(_ context.Context, ev hook.Event) hook.Result {
	if ev.ToolName != "Bash" {
		return hook.Result{}
	}
	class := "ok"
	switch {
	case reBlocked.Match(ev.Result):
		class = "blocked"
	case ev.IsError:
		class = "error"
	}
	g.mu.Lock()
	g.attrib[class]++
	g.mu.Unlock()
	return hook.Result{}
}

// Attributions returns failure-attribution counts (Observer / G5).
func (g *Guard) Attributions() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int, len(g.attrib))
	for k, v := range g.attrib {
		out[k] = v
	}
	return out
}

func (g *Guard) block(tool, reason, cmd string) hook.Result {
	g.record(tool, "block", reason, cmd)
	return hook.Result{Decision: "block", Message: reason}
}

func (g *Guard) record(tool, action, reason, cmd string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.audit = append(g.audit, AuditEntry{TS: time.Now().Unix(), Tool: tool, Action: action, Reason: reason, Command: cmd})
	if len(g.audit) > 2000 {
		g.audit = g.audit[len(g.audit)-2000:]
	}
}

// Audit returns a snapshot of recent gated calls (most recent last).
func (g *Guard) Audit() []AuditEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]AuditEntry, len(g.audit))
	copy(out, g.audit)
	return out
}
