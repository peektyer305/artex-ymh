package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// タスクレベルの実行コンテキスト。
	AbortPausedByUser = cause("paused_by_user", "ユーザーがタスクを一時停止しました",
		"ユーザーがタスク制御 API（POST /api/tasks/{id}/control、action=pause）でタスクを一時停止しました。今回の Planner/Worker の実行は能動的に取り消され、実行中だった意図は frontier(open) に戻されます。タスクを再開すると再取得され、最初から実行されます")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "編成 Agent がタスクを一時停止しました",
		"編成 Agent が pause_task ツールを呼び出して本タスクを一時停止しました。今回の Planner/Worker の実行は能動的に取り消され、実行中だった意図は frontier(open) に戻され、再開後に再実行されます")
	AbortTaskDeleted = cause("task_deleted", "タスクが削除されました",
		"タスクを削除中（DELETE /api/tasks/{id}）で、削除バリアがそのタスクで実行中の Planner、Worker、メイン Agent を取り消しました。今回の実行結果はもう使われません")
	AbortPausedOnReload = cause("paused_on_reload", "バックエンドがタスクの一時停止状態を復元しました",
		"バックエンド起動時に DB へ永続化された状態からタスクの一時停止を復元しました。今回の実行は取り消されます。通常、復元の段階で実行中の Agent はありません")
	AbortGoalMet = cause("goal_met", "Planner がタスク目標の達成を判定しました",
		"Planner がタスク目標の達成を判定してタスクを done にし、その後も実行中だった Worker を取り消しました。これらの意図は失敗ではなく stopped として記録されます")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "タスクのタイムアウト終了処理の待機時間を使い切りました",
		"タスクが timeout に達した後、実行中の Worker の正常な終了処理を待ちましたが、90 秒の drain 猶予でも足りないためハードキャンセルしました。意図は exhausted として記録され、終了処理段階で書き込み済みの事実とアセットは保持されます")

	// 意図（work）レベルのコンテキスト。
	AbortKilledByPlanner = cause("killed_by_planner", "Planner がこの意図を終了しました",
		"Planner が kill_work を呼び出してこの意図を能動的に終了しました。通常は方向が逸れた、または続ける価値が無いと判断したことを意味します。意図は stopped として記録され、自動で再取得されません")
	AbortKilledByMainagent = cause("killed_by_mainagent", "メイン Agent がこの意図を終了しました",
		"メイン Agent（人間の操作者とのインターフェース）が kill_work を呼び出してこの意図を能動的に終了しました。通常は人間が方向の逸脱や続行価値の無さを判断したものです。意図は stopped として記録され、自動で再取得されません")
	AbortWorkPausedByUser = cause("work_paused_by_user", "ユーザーがこの Worker 意図を一時停止しました",
		"ユーザーが実行中の Worker を一時停止しました。今回の呼び出しは取り消され、意図は paused に変わります。登録済みの意図、事実、脆弱性、アクティビティ記録はすべて保持され、再開後は最初から実行されます")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "ユーザーがこの Worker 意図を削除しました",
		"ユーザーが実行中の Worker を削除しました。今回の呼び出しは取り消されます。Worker が書き込み区間を抜けた後、サーバはユーザーが選んだ削除モードでこの意図を処理します。論理削除は削除済みと記録して全成果物を保持し、物理削除はその意図と、それだけに支えられた下流ノードを連鎖的に削除します")
	AbortWorkFinished = cause("work_finished", "Worker が正常終了して context を解放しました",
		"Worker が正常終了し、エンジンは detachWork でその context 資源を解放しています。実行の中断ではありません。もし中断メッセージに現れたら、取り消しと終了処理のイベントが競合したことを意味します")
	AbortPausedRaceGuard = cause("paused_race_guard", "タスクの一時停止中は新しい実行の開始を拒否します",
		"タスクが一時停止状態のとき、エンジンは新しい実行 context の発行を拒否します。claim と一時停止の競合で Worker が起動し続けるのを防ぐためで、取得済みの意図は frontier に戻されます")

	// メイン Agent と単独会話のコンテキスト。
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "ユーザーがこのターンの会話を停止しました",
		"ユーザーが停止を押し、このターンのメイン Agent または会話 Agent の実行を能動的に中止しました。生成済みのアクティビティ記録は保持され、次のメッセージを続けて送信できます")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "タスクの一時停止でメイン Agent の会話も中止しました",
		"ユーザーがタスクを一時停止した際、実行中だったメイン Agent の会話も同時に取り消されました。生成済みのアクティビティ記録は保持されますが、タスクを再開してもこのターンのメッセージは自動で再送されません")
	AbortChatTurnFinished = cause("chat_turn_finished", "このターンの会話が正常終了して context を解放しました",
		"このターンの会話が正常終了し、サーバがそのターンの context 資源を解放しています。実行の中断ではありません。もし中断メッセージに現れたら、取り消しと終了処理のイベントが競合したことを意味します")

	// プロセス単位と実行単位のハードバックストップ。
	AbortShutdown = cause("shutdown", "バックエンドプロセスが終了処理中です",
		"バックエンドプロセスが SIGINT または SIGTERM を受け、再起動・更新・終了のいずれかを実行中です。実行中の Agent はすべて取り消され、再起動後に残った running の意図は open に戻されて再実行されます")
	AbortRunHardTimeout = cause("run_hard_timeout", "1 回の実行のハードタイムアウトのバックストップが発動しました",
		"1 回の実行がソフトのウォールクロック予算と追加猶予を超えました。モデル要求かどこかのツールが長時間返らず、通常のターン境界での終了処理を実行できない状態です。中断直前に返らなかった最後のツール呼び出しを重点的に確認してください")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "上流の context が deadline に到達",
			"上流の context が deadline に到達しましたが、設定側が WithTimeoutCause で名前付きの理由を付与していません: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "取り消し側が名前付きの理由を付与していません",
			"上流の context が取り消されましたが、取り消し側が context.WithCancelCause で名前付きの理由を付与していません。agent/cancelcause.go に理由を登録し、その取り消し箇所へ接続してください", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
