package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 空転ターン(思考のみで本文もツールもない)の識別と継続実行については steerHooks.Stop を参照。

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "まずポートスキャン"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"思考のみ", []llm.Message{llm.UserText("開始"), assistantThinking("考え中")}, true},
		{"思考+ツール", []llm.Message{llm.UserText("開始"), toolUse}, false},
		{"思考+本文", []llm.Message{assistantThinking("考え中"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("結論")},
		}}, false},
		{"本文が空白文字のみ", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"完全に空の assistant ターン", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// ツール結果は user ロールなので、判定はその直前の assistant まで遡る必要があり、直近のメッセージだけで誤判定してはならない。
		{"最後の1件がツール結果", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant メッセージなし", []llm.Message{llm.UserText("開始")}, false},
		{"履歴が空", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks はプログラム可能な inner の HookRunner で、steerHooks が inner の決定を尊重することを検証するために使う。
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("まずサブドメインを列挙すべきだ")}

	t.Run("空転ターンに継続指示を注入する", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("空転ターンは強制停止すべきでない")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("本文かツールがあれば介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("スキャン完了、開いているポートは見つからない")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("正常な終了が空転と誤判定された: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("介入しない場合はカウントしない, got %d", n)
		}
	})

	t.Run("上限到達後は終了を許可する", func(t *testing.T) {
		const limit = 5 // ユーザーが「空レスポンスの再試行回数」を 5 に設定した想定
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d 回目はまだ上限内のはず, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("上限を超えても注入している: %v", blocking)
		}
	})

	// 「空レスポンスの再試行回数」を -1 にするとこの層が無効になり、emptyTurnNudgeLimit は 0 と解釈される。
	t.Run("設定がオフなら介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("オフなのに注入している: %v", blocking)
		}
	})

	t.Run("inner が強制停止を決めたら重ねない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard が終了を拒否"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard が終了を拒否" || blocking != nil {
			t.Fatalf("inner の強制停止が書き換えられた: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner に譲るときは上限を消費しない, got %d", n)
		}
	})

	t.Run("inner が継続を要求済みなら重ねない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard の継続理由"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard の継続理由" {
			t.Fatalf("inner の継続メッセージが書き換えられた: %v", blocking)
		}
	})

	t.Run("カウンタ未設定でも挙動は変わらない", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // 例えば将来ほかの呼び出し元が nudges を渡し忘れた場合
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("カウンタがない場合は注入しない: %v", blocking)
		}
	})
}
