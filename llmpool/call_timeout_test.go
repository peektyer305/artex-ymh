package llmpool

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 分類は純粋関数：ウォールクロックのタイムアウト（呼び出し側 ctx が生存 + DeadlineExceeded）
// は「フェイルオーバー可能な一時失敗」に分類する。能動的なキャンセル / 上位の締め切り
// （呼び出し側 ctx が終了済み）は依然として決してフェイルオーバーしない。
func TestShouldFailoverCallTimeout(t *testing.T) {
	// 下層の派生 ctx のウォールクロックが期限到来：agent パッケージの
	// ARTEX_LLM_CALL_TIMEOUT が正規化した call_timeout（Unwrap で DeadlineExceeded）、
	// または SDK 内部の attempt タイムアウト。
	wrapped := fmt.Errorf("llm call_timeout: anthropic/m 全体のウォールクロックタイムアウト: %w", context.DeadlineExceeded)
	if !shouldFailover(context.Background(), context.DeadlineExceeded) {
		t.Error("呼び出し側が生存 + DeadlineExceeded は転送可能な一時失敗と判定すべきです(call_timeout)")
	}
	if !shouldFailover(context.Background(), wrapped) {
		t.Error("正規化後の call_timeout エラーは転送可能であるべきです")
	}
	// 呼び出し側 ctx は生存しているがエラーが Canceled：フェイルオーバーしない。
	if shouldFailover(context.Background(), context.Canceled) {
		t.Error("Canceled は決して転送しない")
	}
	// ユーザーの能動的キャンセル：ctx は終了済みで、エラーが DeadlineExceeded でもフェイルオーバーしない。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if shouldFailover(ctx, context.DeadlineExceeded) {
		t.Error("能動的なキャンセル後の DeadlineExceeded は転送すべきではありません")
	}
	if shouldFailover(ctx, context.Canceled) {
		t.Error("能動的なキャンセルは転送すべきではありません")
	}
	// タスクレベルの締め切り（上位のウォールクロック）：ctx 自体が期限切れで、これも決してフェイルオーバーしない。
	dctx, dcancel := context.WithTimeout(context.Background(), 0)
	defer dcancel()
	if shouldFailover(dctx, context.DeadlineExceeded) {
		t.Error("上位の締め切り(ctx の期限切れ)は転送すべきではありません")
	}
	// call_timeout は hard failure ではない：ステータスコードがなく、ソフト遮断のカウントに回る。
	if isHardFailure(wrapped) {
		t.Error("call_timeout は一時失敗(ソフト遮断)に分類すべきで、hard failure ではありません")
	}
	if statusOf(wrapped) != 0 {
		t.Error("call_timeout のエラーテキストから HTTP ステータスコードを解釈すべきではありません")
	}
}

// ウォールクロックでハングした member が安全ウィンドウ内（イベント未配信）に失敗 → 次へ
// フェイルオーバーし、一時失敗としてソフト遮断のカウントに入る（即時には開放しない）。
func TestFailoverOnCallTimeout(t *testing.T) {
	a := &fakeProv{name: "a", errs: []error{context.DeadlineExceeded}}
	b := okProv("b", "hello")
	reg := NewRegistry(nil, nil)
	p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, reg)

	got, err := drain(p.Stream(context.Background(), llm.CompletionRequest{}))
	if err != nil {
		t.Fatalf("call_timeout はフェイルオーバーを発動して成功すべきです, got %v", err)
	}
	if got != "hello" {
		t.Fatalf("text=%q, want hello", got)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("calls: a=%d b=%d, want 1/1", a.calls, b.calls)
	}
	st := reg.Get(1)
	if st.Fails != 1 {
		t.Fatalf("call_timeout は一時失敗としてカウントすべきです, Fails=%d, want 1", st.Fails)
	}
	if st.Open() {
		t.Fatal("単発の call_timeout は一時失敗であり、即座に遮断すべきではありません(ソフト遮断は連続複数回が必要)")
	}
}

// 安全ウィンドウの意味論は call_timeout にも同じく成立する：出力を配信した後にウォール
// クロックが期限到来すると、エラーはそのまま上に返し、他の profile へ再送しない（そう
// しないとモデル出力が重複する）。
func TestNoFailoverOnCallTimeoutAfterEmit(t *testing.T) {
	a := &fakeProv{
		name:   "a",
		events: [][]llm.StreamEvent{{{Type: llm.SETextDelta, Text: "partial"}}},
		errs:   []error{context.DeadlineExceeded},
	}
	b := okProv("b", "full")
	p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, NewRegistry(nil, nil))

	got, err := drain(p.Stream(context.Background(), llm.CompletionRequest{}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want DeadlineExceeded をそのまま上位へ返す", err)
	}
	if got != "partial" {
		t.Fatalf("text=%q, want 配信済みの partial", got)
	}
	if b.calls != 0 {
		t.Fatalf("出力の配信後は call_timeout で転送すべきではありません(backup calls=%d)", b.calls)
	}
}

// Complete 経路：ウォールクロックの失敗でも同様にフェイルオーバーする（非ストリーミング呼び出しは原子的で、出力重複のリスクがない）。
func TestCompleteFailoverOnCallTimeout(t *testing.T) {
	a := &fakeProv{name: "a", errs: []error{context.DeadlineExceeded}}
	b := okProv("b", "done")
	p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, NewRegistry(nil, nil))

	msg, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("Complete は call_timeout 後に転送して成功すべきです, got %v", err)
	}
	if msg.Text() != "done" {
		t.Fatalf("text=%q, want done", msg.Text())
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("calls: a=%d b=%d, want 1/1", a.calls, b.calls)
	}
}
