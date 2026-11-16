package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestCallTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		set  bool
		want time.Duration
	}{
		{set: false, want: 600 * time.Second}, // 未設定 → デフォルト 600s
		{env: "0", set: true, want: 0},        // 0 = 無制限
		{env: "30", set: true, want: 30 * time.Second},
		{env: " 120 ", set: true, want: 120 * time.Second},
		{env: "abc", set: true, want: 600 * time.Second}, // 不正値 → デフォルト
		{env: "-5", set: true, want: 600 * time.Second},  // 負値 → デフォルト
	}
	for _, c := range cases {
		if c.set {
			t.Setenv("ARTEX_LLM_CALL_TIMEOUT", c.env)
		} else {
			t.Setenv("ARTEX_LLM_CALL_TIMEOUT", "")
			// 空文字列は未設定と同じ（TrimSpace 後に空 → デフォルト）
		}
		if got := callTimeoutFromEnv(); got != c.want {
			t.Errorf("env=%q set=%v: callTimeoutFromEnv()=%v, want %v", c.env, c.set, got, c.want)
		}
	}
}

// 分類の純粋関数：能動的なキャンセル（呼び出し元 ctx の終了）は絶対に正規化しない。
// 呼び出し元が生存したまま派生 ctx が期限切れになった場合だけが本層のウォールクロックで、
// call_timeout に分類する。
func TestNormalizeDeadlineClassification(t *testing.T) {
	start := time.Now()
	constTO := 600 * time.Second

	t.Run("ユーザーが能動キャンセル→そのまま伝播", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		callCtx, callCancel := context.WithTimeout(ctx, constTO)
		defer callCancel()
		cancel() // ユーザー停止：親 ctx が先に終わり、派生 ctx も続いて終わる
		err := normalizeDeadline(ctx, callCtx, context.Canceled, "anthropic", "m", constTO, start)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want context.Canceled をそのまま伝播", err)
		}
		var cto *CallTimeoutError
		if errors.As(err, &cto) {
			t.Fatal("能動キャンセルが誤って call_timeout に正規化された")
		}
	})

	t.Run("上位のタスクレベル期限→そのまま伝播", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		callCtx, callCancel := context.WithTimeout(ctx, constTO)
		defer callCancel()
		time.Sleep(5 * time.Millisecond) // 親 ctx を期限切れにする
		err := normalizeDeadline(ctx, callCtx, context.DeadlineExceeded, "anthropic", "m", constTO, start)
		var cto *CallTimeoutError
		if errors.As(err, &cto) {
			t.Fatal("上位の期限が誤って call_timeout に正規化された(能動停止でフェイルオーバーしてしまう)")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v, want DeadlineExceeded を伝播", err)
		}
	})

	t.Run("この層のウォールクロック満了→call_timeout", func(t *testing.T) {
		ctx := context.Background() // 呼び出し元は生存
		callCtx, callCancel := context.WithTimeout(ctx, time.Millisecond)
		defer callCancel()
		time.Sleep(5 * time.Millisecond) // callCtx をキャンセルできるのは派生タイマーだけ
		err := normalizeDeadline(ctx, callCtx, context.DeadlineExceeded, "openai", "gpt-x", constTO, start)
		var cto *CallTimeoutError
		if !errors.As(err, &cto) {
			t.Fatalf("err=%v, want *CallTimeoutError", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("CallTimeoutError は下流の errors.Is のために DeadlineExceeded を Unwrap すべき")
		}
		if cto.Provider != "openai" || cto.Model != "gpt-x" || cto.Timeout != constTO {
			t.Fatalf("CallTimeoutError のフィールドが一致しない: %+v", cto)
		}
	})

	t.Run("通常のエラー→そのまま伝播", func(t *testing.T) {
		ctx := context.Background()
		callCtx, callCancel := context.WithTimeout(ctx, constTO)
		defer callCancel()
		orig := errors.New("dial tcp: connection reset by peer")
		if got := normalizeDeadline(ctx, callCtx, orig, "anthropic", "m", constTO, start); got != orig {
			t.Fatalf("通常の伝送エラーはそのまま伝播すべき, got %v", got)
		}
	})

	t.Run("nil→nil", func(t *testing.T) {
		callCtx, callCancel := context.WithTimeout(context.Background(), constTO)
		defer callCancel()
		if got := normalizeDeadline(context.Background(), callCtx, nil, "anthropic", "m", constTO, start); got != nil {
			t.Fatalf("nil err は nil を伝播すべき, got %v", got)
		}
	})
}

// hangProv は半開きのまま固まる provider を模す：Stream/Complete はどちらも ctx の終了まで
// ブロックし、その後 ctx.Err() をエラーとして返す。ウォールクロックの満了でもユーザーによる
// キャンセルでも目覚めるため、2 つの経路をちょうど区別できる。
type hangProv struct{}

func (hangProv) Stream(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		<-ctx.Done()
		yield(llm.StreamEvent{}, ctx.Err())
	}
}

func (hangProv) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	<-ctx.Done()
	return llm.Message{}, "", llm.Usage{}, ctx.Err()
}

func TestStreamWallClockNormalizes(t *testing.T) {
	p := &callTimeoutProvider{inner: hangProv{}, timeout: 30 * time.Millisecond, provider: "anthropic", model: "claude-x"}
	start := time.Now()
	var gotErr error
	for _, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
		gotErr = err
	}
	elapsed := time.Since(start)
	var cto *CallTimeoutError
	if !errors.As(gotErr, &cto) {
		t.Fatalf("err=%v, want *CallTimeoutError", gotErr)
	}
	if !errors.Is(gotErr, context.DeadlineExceeded) {
		t.Fatal("call_timeout は DeadlineExceeded の意味を保つべき")
	}
	if elapsed < 30*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("ウォールクロックは ~30ms で発火すべき, 実測値 %v", elapsed)
	}
}

func TestCompleteWallClockNormalizes(t *testing.T) {
	p := &callTimeoutProvider{inner: hangProv{}, timeout: 30 * time.Millisecond, provider: "openai", model: "gpt-x"}
	_, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{})
	var cto *CallTimeoutError
	if !errors.As(err, &cto) {
		t.Fatalf("err=%v, want *CallTimeoutError", err)
	}
}

// ユーザーによる能動的キャンセル：ウォールクロックで包んでいても、エラーはそのまま
// Canceled でなければならない。絶対に正規化しない（さもないと llmpool が「ユーザー停止」で
// 予備の key を消費する）。
func TestUserCancelNotNormalized(t *testing.T) {
	p := &callTimeoutProvider{inner: hangProv{}, timeout: 10 * time.Second, provider: "anthropic", model: "claude-x"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	var gotErr error
	for _, err := range p.Stream(ctx, llm.CompletionRequest{}) {
		gotErr = err
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", gotErr)
	}
	var cto *CallTimeoutError
	if errors.As(gotErr, &cto) {
		t.Fatal("能動キャンセルが call_timeout に正規化された — 「キャンセルは決して転送しない」意味が壊れる")
	}
}

// ストリーム途中でウォールクロックが満了した場合（すでにイベントを渡した後）も同様に正規化する。
// 転送するかどうかは llmpool の安全ウィンドウが決め、本層は「満了で必ず切断 + エラーの分類」
// だけを担う。逆方向の用例：正常に速く完了するストリームはウォールクロックの影響を受けない。
func TestStreamNormalFlowUnaffected(t *testing.T) {
	inner := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}}
	p := &callTimeoutProvider{inner: inner, timeout: 30 * time.Second, provider: "anthropic", model: "m"}
	var text string
	var gotErr error
	for ev, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
		if err != nil {
			gotErr = err
			continue
		}
		if ev.Type == llm.SETextDelta {
			text += ev.Text
		}
	}
	if gotErr != nil {
		t.Fatalf("正常に速く完了するストリームはウォールクロックを発火させない: %v", gotErr)
	}
	if text != "partial" {
		t.Fatalf("text=%q, want partial(ウォールクロックは正常な流れに影響しない)", text)
	}
}

func TestWrapCallTimeout(t *testing.T) {
	inner := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}}
	if got := wrapCallTimeout(nil, Config{}); got != nil {
		t.Fatal("nil inner は nil を返すべき")
	}
	// env=0 → 無制限でそのまま返す（ウォールクロックで包まない）。captureUsageProvider は
	// 比較できない func フィールドを持つため、型アサーションで「*callTimeoutProvider に
	// 包まれていない」ことを確かめる。
	t.Setenv("ARTEX_LLM_CALL_TIMEOUT", "0")
	if got := wrapCallTimeout(inner, Config{Model: "m"}); got == nil {
		t.Fatal("env=0 で nil を返してはならない")
	} else if _, wrapped := got.(*callTimeoutProvider); wrapped {
		t.Fatal("ARTEX_LLM_CALL_TIMEOUT=0 は inner をそのまま返すべき, ウォールクロックで包まない")
	}
	// env に値あり → 1 層包み、フィールドは Config から取る
	t.Setenv("ARTEX_LLM_CALL_TIMEOUT", "5")
	got := wrapCallTimeout(inner, Config{Format: llm.FormatAnthropic, Model: "m"})
	ctp, ok := got.(*callTimeoutProvider)
	if !ok {
		t.Fatalf("env=5 は *callTimeoutProvider を返すべき, got %T", got)
	}
	if ctp.timeout != 5*time.Second || ctp.provider != "anthropic" || ctp.model != "m" {
		t.Fatalf("wrap のフィールドが一致しない: %+v", ctp)
	}
}

// 回帰防止：正規化したエラーのテキストに "status NNN" の形を含めてはならない。含めると
// llmpool が HTTP ステータスコードで誤分類する（call_timeout は「ステータスコードを持たない
// トランスポート層の一過性失敗」のままであるべき）。
func TestCallTimeoutErrorTextHasNoStatus(t *testing.T) {
	err := &CallTimeoutError{Provider: "anthropic", Model: "m", Timeout: 600 * time.Second, Elapsed: 600 * time.Second}
	got := err.Error()
	for _, want := range []string{"call_timeout", "anthropic", "m", "10m0s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("エラーテキストに %q がない: %q", want, got)
		}
	}
	if strings.Contains(got, "status ") {
		t.Fatalf("エラーテキストに HTTP ステータスコードの形を含めてはならない: %q", got)
	}
}
