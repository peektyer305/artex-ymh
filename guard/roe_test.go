package guard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// RoE 統合テスト（interceptor なしで試験可能な部分）:strict+Out で承認チャネルなし → ハードブロック;
// warn → 通過するが監査を記録。In / Unknown / off / ターゲットなし / taskID なし → 通過。

func roeGuard(mode string, rules []ScopeRule) *Guard {
	g := New()
	g.SetRoE(RoEConfig{
		Mode:     func() string { return mode },
		Scope:    func(taskID int64) []ScopeRule { return rules },
		TaskIDOf: func(ctx context.Context) int64 { return 42 },
	})
	return g
}

func bashBlocked(t *testing.T, g *Guard, cmd string) (bool, string) {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"command": cmd})
	blocked, msg, _ := g.Hooks().PreToolUse(context.Background(), "Bash", input)
	return blocked, msg
}

func TestRoEStrictBlocksOutOfScope(t *testing.T) {
	g := roeGuard("strict", []ScopeRule{{Kind: "domain", Value: "target.com"}})
	blocked, msg := bashBlocked(t, g, `curl https://evil.com/`)
	if !blocked {
		t.Fatal("strict モードでは範囲外のターゲットをブロックすべきです")
	}
	if !strings.Contains(msg, "evil.com") || !strings.Contains(msg, "roe_enforcement") {
		t.Errorf("ブロックメッセージには host と無効化/降格の方法を明記すべきです, got: %s", msg)
	}
}

func TestRoEStrictAllowsInScope(t *testing.T) {
	g := roeGuard("strict", []ScopeRule{{Kind: "domain", Value: "target.com"}})
	for _, cmd := range []string{
		`curl https://api.target.com/`,
		`nmap 127.0.0.1`,           // 本機操作は通過
		`ls -la`,                   // ターゲットなし
		`curl https://target.com/`, //
	} {
		if blocked, _ := bashBlocked(t, g, cmd); blocked {
			t.Errorf("strict モードでは %q をブロックすべきではありません", cmd)
		}
	}
}

func TestRoEUnknownScopeAllows(t *testing.T) {
	// 未登録範囲（空ルール）→ Unknown → 通過。現行セマンティクスを維持。
	g := roeGuard("strict", nil)
	if blocked, _ := bashBlocked(t, g, `curl https://anything.com/`); blocked {
		t.Error("未登録の範囲はブロックすべきではありません")
	}
}

func TestRoEWarnAllowsButAudits(t *testing.T) {
	g := roeGuard("warn", []ScopeRule{{Kind: "cidr", Value: "10.0.0.0/8"}})
	if blocked, _ := bashBlocked(t, g, `nmap 172.16.0.5`); blocked {
		t.Fatal("warn モードでは通過させるべきです")
	}
	var found bool
	for _, e := range g.Audit() {
		if e.Action == "allow" && strings.Contains(e.Reason, "RoE") && strings.Contains(e.Reason, "172.16.0.5") {
			found = true
		}
	}
	if !found {
		t.Error("warn の通過は guard 監査に記録すべきです(RoE と host を明記)")
	}
}

func TestRoEOffDisables(t *testing.T) {
	g := roeGuard("off", []ScopeRule{{Kind: "domain", Value: "target.com"}})
	if blocked, _ := bashBlocked(t, g, `curl https://evil.com/`); blocked {
		t.Error("off モードではブロックすべきではありません")
	}
}

func TestRoENoTaskIDAllows(t *testing.T) {
	g := roeGuard("strict", []ScopeRule{{Kind: "domain", Value: "target.com"}})
	g.SetRoE(RoEConfig{
		Mode:     func() string { return "strict" },
		Scope:    func(int64) []ScopeRule { return []ScopeRule{{Kind: "domain", Value: "target.com"}} },
		TaskIDOf: func(context.Context) int64 { return 0 }, // chat などタスク実行以外
	})
	if blocked, _ := bashBlocked(t, g, `curl https://evil.com/`); blocked {
		t.Error("taskID なしは通過させるべきです")
	}
}

func TestRoEWebFetch(t *testing.T) {
	g := roeGuard("strict", []ScopeRule{{Kind: "domain", Value: "target.com"}})
	input, _ := json.Marshal(map[string]string{"url": "https://evil.com/x"})
	blocked, _, _ := g.Hooks().PreToolUse(context.Background(), "WebFetch", input)
	if !blocked {
		t.Error("WebFetch の URL host も同様に RoE チェックを受けるべきです")
	}
}
