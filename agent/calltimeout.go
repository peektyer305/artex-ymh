package agent

import (
	"context"
	"fmt"
	"iter"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// 呼び出し全体のウォールクロック（call-level hard wall clock）。
//
// 実運用で LLM 接続が数時間ハングする事象が起きた：TCP は切れず、SSE もフレームを送らず、
// SDK の接続リトライも per-attempt HTTP timeout も「ストリームは開始済みだがデータが
// 永遠に来ない」半開状態をカバーできず、worker/planner/終了処理の時系列が丸ごと凍り付いた。
// 本層は provider の組み立て箇所（Config.NewProvider）で各 Stream/Complete 呼び出しに
// context.WithTimeout を適用する:発行時点から計時し、ストリーミング全体のライフサイクルを
// 覆う。期限到達で cancel し、エラーは *CallTimeoutError に正規化する（call_timeout 種別、
// 意味は再試行可能な一過性の失敗。429→402 の正規化パターンに倣う）。llmpool はこれを受け
// てフェイルオーバーする。
//
// 既存の per-attempt HTTP timeout とは競合しない:あちらは単発の HTTP 試行の保険、本層は
// 単一の論理呼び出し（SDK 内部のリトライとストリーミング全体を含む）の上位保険である。
//
// 設定：env ARTEX_LLM_CALL_TIMEOUT、単位は秒、既定 600、0 = 制限なし（本層を被せない）。
const defaultCallTimeoutSeconds = 600

// callTimeoutFromEnv は ARTEX_LLM_CALL_TIMEOUT（秒）を解釈する。未設定/不正 → 既定 600s;
// 0 → 0（制限なし）。負値は不正とみなし既定にフォールバックする。provider の構築時に
// 1 回だけ読む（非ホットパス）ので、env を変えた後に作成/再構築した provider から有効になる。
func callTimeoutFromEnv() time.Duration {
	s := strings.TrimSpace(os.Getenv("ARTEX_LLM_CALL_TIMEOUT"))
	if s == "" {
		return defaultCallTimeoutSeconds * time.Second
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		log.Printf("[llm-call] WARN ARTEX_LLM_CALL_TIMEOUT=%q が不正（非負の整数秒であること）,既定の %ds として扱います", s, defaultCallTimeoutSeconds)
		return defaultCallTimeoutSeconds * time.Second
	}
	return time.Duration(n) * time.Second
}

// CallTimeoutError はウォールクロック到達後の正規化エラー（call_timeout 種別）。意味:本層が
// その呼び出しをハングと判定したもので、再試行可能な一過性の失敗——llmpool はこれを伝送
// レベルの一過性障害として扱いフェイルオーバーする（ソフトサーキットブレーカの計数対象で、
// hard failure ではない）。Unwrap は context.DeadlineExceeded を返すため
// errors.Is(err, context.DeadlineExceeded) が成立する。「呼び出し側の能動的な取り消し/
// 上位の期限」との区別はエラー自体ではなく、呼び出し側の ctx が既に終了しているかによる
// （normalizeDeadline と llmpool.shouldFailover を参照）。
type CallTimeoutError struct {
	Provider string        // 提供元の書式名(anthropic/openai/…)
	Model    string        // モデル id
	Timeout  time.Duration // ウォールクロックの上限
	Elapsed  time.Duration // 発動時の経過時間(≈ Timeout、実測値の保持は照合用)
}

func (e *CallTimeoutError) Error() string {
	return fmt.Sprintf("llm call_timeout: %s/%s ウォールクロック超過(経過 %s,上限 %s)、接続ハングと判定し一過性の失敗として扱います",
		e.Provider, e.Model, e.Elapsed.Round(time.Millisecond), e.Timeout)
}

// Unwrap は context.DeadlineExceeded を公開する:下流の errors.Is による期限判定は引き続き成立する。
func (e *CallTimeoutError) Unwrap() error { return context.DeadlineExceeded }

// normalizeDeadline はタイムアウト分類の純関数で、3 つの場合を区別する:
//
//   - 呼び出し側 ctx が終了済み（ユーザー停止 / タスク単位の期限 / 上位のウォールクロック）:
//     そのまま透過する。能動的な取り消しは決して正規化せず、フェイルオーバーもしない
//     ——llmpool の既存の意味論であり、本層の存在によって壊してはならない。
//   - 呼び出し側 ctx は生存し、派生した callCtx が終了:本層のタイマーだけが callCtx を
//     取り消す、つまりウォールクロック到達であり、*CallTimeoutError に正規化する。
//   - 両方の ctx が生存:通常のエラー（SDK 内部の per-attempt タイムアウトを含む）を透過する。
func normalizeDeadline(ctx, callCtx context.Context, err error, provider, model string, timeout time.Duration, start time.Time) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil || callCtx.Err() == nil {
		return err
	}
	return &CallTimeoutError{Provider: provider, Model: model, Timeout: timeout, Elapsed: time.Since(start)}
}

// callTimeoutProvider は単一呼び出しの外側にウォールクロックを被せる。recorder
// （llmrec.Wrap）の下に位置するため、正規化後のエラーは status=error と実際の所要時間を
// 伴って llm_usage に記録される。
type callTimeoutProvider struct {
	inner    llm.Provider
	timeout  time.Duration
	provider string
	model    string
}

// wrapCallTimeout は env 設定に従って inner にウォールクロックを被せる;0（制限なし）または nil の inner はそのまま返す。
func wrapCallTimeout(inner llm.Provider, c Config) llm.Provider {
	if inner == nil {
		return nil
	}
	timeout := callTimeoutFromEnv()
	if timeout <= 0 {
		return inner
	}
	return &callTimeoutProvider{inner: inner, timeout: timeout, provider: c.Provider(), model: c.Model}
}

func (p *callTimeoutProvider) warnTimeout(elapsed time.Duration) {
	// 注意：provider の組み立て層では profile 名を取得できない（Config に無い）。profile 名は
	// 外側の llmpool フェイルオーバーログと llm_usage の記録（recorder が profile 名でタグ付け）
	// で補完される。
	log.Printf("[llm-call] WARN ウォールクロック超過(call_timeout):provider=%s model=%s 経過=%s 上限=%s — 接続ハングと判定し、一過性の失敗として正規化して上位の再試行/フェイルオーバーに委ねます",
		p.provider, p.model, elapsed.Round(time.Millisecond), p.timeout)
}

// Stream は発行時点から計時し、ストリーミング全体のライフサイクルを覆う;ウォールクロック
// 到達で即 cancel し、エラーを call_timeout に正規化する。呼び出し側が途中で消費を止めた
// 場合（yield=false）は defer の cancel が直ちに下位ストリームを切断し、半開接続を残さない。
func (p *callTimeoutProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		start := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, p.timeout)
		defer cancel()
		for ev, err := range p.inner.Stream(callCtx, req) {
			if err != nil {
				if nerr := normalizeDeadline(ctx, callCtx, err, p.provider, p.model, p.timeout, start); nerr != err {
					p.warnTimeout(time.Since(start))
					yield(llm.StreamEvent{}, nerr)
					return
				}
			}
			if !yield(ev, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// Complete は Stream と同じ意味論:非ストリーミング呼び出しは原子的なので、ウォールクロック到達時はそのまま正規化して返す。
func (p *callTimeoutProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	start := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	msg, stop, usage, err := p.inner.Complete(callCtx, req)
	if err != nil {
		if nerr := normalizeDeadline(ctx, callCtx, err, p.provider, p.model, p.timeout, start); nerr != err {
			p.warnTimeout(time.Since(start))
			return llm.Message{}, "", llm.Usage{}, nerr
		}
	}
	return msg, stop, usage, err
}
