package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "モデルがこのターンを正常に終了しましたが、文字のまとめを残していません。事実とアセットはこのターンのツール呼び出し記録を基準にします",
	harness.ReasonMaxTurns:          "ステップ上限(MaxTurns)に到達：SDK が終了処理を実行して事実とアセットを書き戻しました。意図は exhausted として記録され、Planner が方向を変えて続行できます（失敗扱いではありません）",
	harness.ReasonTimeout:           "1 回の実行のウォールクロック予算(MaxDuration)に到達:期限到達で実行中のツールを打ち切り、その場で終了処理に入って判明済みの事実とアセットを書き戻します。意図は exhausted として記録されます",
	harness.ReasonModelError:        "モデルまたは API の呼び出しに失敗しました（ネットワーク、認証、レート制限、プロバイダの 5xx など）。再試行を使い切ると意図は blocked として記録されます——伝送層の障害によりこの意図はほぼ実質的に探索できていません。実行過程（get_worker_trace）を確認してから再派遣か手段の変更を判断してください",
	harness.ReasonBlockingLimit:     "コンテキスト長がハード上限に達し、リクエストは送信前にブロックされました。意図の粒度を細かくするか、ツールの戻り値を圧縮してください",
	harness.ReasonPromptTooLong:     "プロンプトが長すぎ、コンテキスト圧縮の再試行も使い切ったため実行を継続できません",
	harness.ReasonImageError:        "現在のモデルはこのターンのマルチモーダル内容に対応していません。視覚対応モデルに切り替えるか、ツールが画像を返さないようにしてください",
	harness.ReasonStopHookPrevented: "Stop フックがこのターンの終了を阻止し、その後続行できませんでした。タスクの Guard ルールが厳しすぎないか確認してください",
	harness.ReasonHookStopped:       "ツールまたはフックが続行を能動的に停止しました。範囲外の目標や禁止コマンドなどです。最後の tool_result のインターセプト説明を確認してください",
	harness.ReasonAbortedStreaming:  "モデルの出力ストリーミング生成段階で実行が取り消されました",
	harness.ReasonAbortedTools:      "ツール実行段階で実行が取り消されました",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "取り消し理由を取得できませんでした"
		}
		stage := "実行中に停止"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "モデル出力段階で停止"
		case harness.ReasonAbortedTools:
			stage = "ツール実行段階で停止"
		}
		sum = "（実行が中断されました：" + short + "、" + stage + progressSuffix(term, tr) + "、未完了）"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "（実行予算の上限に到達(" + string(reason) + ")、終了処理して事実を書き戻し済み" + progressSuffix(term, tr) + "。今回は文字のまとめなし）"
	} else {
		hint := terminalReasonHint(reason)
		sum = "（文字のまとめなし、終了状態 " + terminalReasonLabel(reason) + "：" + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **終了状態**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **中断原因** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **中断原因**: 取得できません。取り消し側が context.WithCancelCause で名前付きの理由を付与していない可能性があります\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **下位エラー**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **取り消し前に生成済みの部分出力**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **実行済み**: %d ターンのモデル往復\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **今回の実行時間**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **累計 token**: 入力 %d / 出力 %d / キャッシュ読 %d / キャッシュ書 %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **ツール呼び出し**: 今回の実行はツール呼び出しを出す前に終了しました\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **中断時に実行中だったツール**: `%s`（実行 %s、**結果未返却**）\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **中断前の最後のツール**: `%s`（正常に復帰済み）\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "実行の context は取り消されましたが、下位から Terminal イベントが発生していません"
	}
	return "不明な終了状態です。harness が TerminalReason を追加した可能性があるので reasonHint を補ってください"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d ターン", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "、実行 " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
