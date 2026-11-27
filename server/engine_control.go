package server

import (
	"context"
	"fmt"
	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

// registerWork records the cancel for the work currently running intentID.
func (e *Engine) registerWork(intentID int64, cancel context.CancelCauseFunc) {
	e.workMu.Lock()
	e.work[intentID] = &workExecution{cancel: cancel, done: make(chan error, 1)}
	e.workMu.Unlock()
}

// detachWork removes the live control handle once Execute has returned. complete
// must be called after the final intent state write so a waiting cancel handler can
// safely delete the worker's blackboard output without racing a late write.
func (e *Engine) detachWork(intentID int64) (action string, complete func(error)) {
	e.workMu.Lock()
	run := e.work[intentID]
	if run != nil {
		delete(e.work, intentID)
		action = run.action
		run.cancel(agent.AbortWorkFinished) // release resources (no-op if already cancelled)
	}
	e.workMu.Unlock()
	e.steerMu.Lock()
	delete(e.steerBox, intentID) // drop any undelivered steering for a finished work
	e.steerMu.Unlock()
	if run == nil {
		return action, func(error) {}
	}
	return action, func(err error) { run.done <- err }
}

// ControlWork requests a user-visible pause or cancellation and waits until the
// worker has fully stopped writing. Cancellation cleanup is performed by the API
// handler after this returns; pause state is committed by runWorkerStep itself.
func (e *Engine) ControlWork(ctx context.Context, intentID int64, action string) error {
	if action != "pause" && action != "cancel" {
		return fmt.Errorf("unsupported work action %q", action)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	e.workMu.Lock()
	run := e.work[intentID]
	if run == nil {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 意図 %d には現在実行中の work がありません（終了済みか未取得の可能性）", errWorkControlConflict, intentID)
	}
	if run.action != "" {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 意図 %d は %s 操作を実行中です", errWorkControlConflict, intentID, run.action)
	}
	run.action = action
	done := run.done
	cause := error(agent.AbortWorkPausedByUser)
	if action == "cancel" {
		cause = agent.AbortWorkCancelledByUser
	}
	run.cancel(cause)
	e.workMu.Unlock()

	timer := time.NewTimer(workControlWaitTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("意図 %d の %s の終了処理待ち: %w", intentID, action, ctx.Err())
	case <-timer.C:
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("意図 %d の %s の終了処理待ち: %w", intentID, action, context.DeadlineExceeded)
	}
}

// releaseWorkControl drops only this caller's reservation after its wait is
// cancelled. The work context stays cancelled; runWorkerStep recognizes the
// named cancellation cause and settles the intent into the recoverable paused
// state even if the HTTP caller has gone away.
func (e *Engine) releaseWorkControl(intentID int64, run *workExecution, action string) {
	e.workMu.Lock()
	if current := e.work[intentID]; current == run && current.action == action {
		current.action = ""
	}
	e.workMu.Unlock()
}

func transitionIntentState(store *db.ExplorationStore, intentID int64, expected, state string) error {
	changed, err := store.CompareAndSetIntentState(intentID, expected, state)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 意図 %d はもう %s 状態ではありません", db.ErrIntentStateConflict, intentID, expected)
	}
	return nil
}

// SteerWork queues a mid-run course-correction for the work running intentID (the
// planner's steer_work tool). The worker delivers it before its next tool call and
// re-plans — no kill. Errors if no work is currently running that intent.
func (e *Engine) SteerWork(intentID int64, msg string) error {
	if strings.TrimSpace(msg) == "" {
		return fmt.Errorf("訂正メッセージを入力してください")
	}
	e.workMu.Lock()
	running := e.work[intentID] != nil
	e.workMu.Unlock()
	if !running {
		return fmt.Errorf("意図 %d には現在実行中の work がありません（終了済みか未取得の可能性）", intentID)
	}
	e.steerMu.Lock()
	e.steerBox[intentID] = append(e.steerBox[intentID], msg)
	e.steerMu.Unlock()
	return nil
}

// drainSteer pops the oldest queued steering message for intentID (FIFO), if any.
func (e *Engine) drainSteer(intentID int64) (string, bool) {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	q := e.steerBox[intentID]
	if len(q) == 0 {
		return "", false
	}
	msg := q[0]
	if len(q) == 1 {
		delete(e.steerBox, intentID)
	} else {
		e.steerBox[intentID] = q[1:]
	}
	return msg, true
}

// steerHooks wraps the guard's hook runner so the planner can steer a running work:
// before each tool call it drains a queued course-correction (if any) and blocks the
// call, handing the message back to the model — which re-plans its next step instead
// of running the tool. No queued message → the guard behaves exactly as before.
// また「空転ターン」の続行(Stop 参照)と「同質レスポンスの停滞検出」(PostToolUse 参照、
// chains L1、docs/architecture/chains-integration.md §3)も担う。
type steerHooks struct {
	inner harness.HookRunner
	drain func() (string, bool)
	// nudges はこの意図で注入した空転続行の回数で、上限は limit。ポインタ：harness が
	// 保持するのは steerHooks の値コピーなので、カウントは同じ 1 つを共有する必要がある。
	nudges *atomic.Int64
	// limit は空転続行の回数上限で、Engine.emptyTurnNudgeLimit() が「空レスポンス
	// 再試行回数」から解決する。<=0 = 介入しない(ユーザーがこの層を明示的に切った)。
	limit int
	// label は "worker-1 · #42" の形式で、ログ専用。
	label string
	// det は同質レスポンスの停滞検出器(chains L1、docs/operations/postmortem-red-sun-3.md R4):PostToolUse
	// が各ステップの tool_result を渡し、「情報の勾配がゼロ」と判定したら PreToolUse の
	// drain 経由で方向転換の指示を注入する。ポインタで nudges と同様。しかも意図的に
	// model_error の再実行ループの外に置き(runIntent 参照)、1 つの意図の発動枠(クール
	// ダウン/上限)を再実行をまたいで共有し、再実行 1 回では零に戻らない。nil = 無効。
	det *agent.StuckDetector
	// onStuck は検出器の発動時に呼ばれるコールバック(engine 側の配線：system activity を
	// 書き、hint を探索グラフに付けて planner に伝える)。nil = 注入のみで痕跡を残さない。
	onStuck func(ev *agent.StuckEvent)
}

// 空転ターン(思考だけで本文もツール呼び出しもない)の続行回数の既定値。SDK の空
// レスポンス再試行の組み込み既定(norma/llm/openai.go の emptyResponseRetries)と
// 揃えており、2 層が同じノブを共有するので未設定時の挙動も一致させるべき。解決は
// Engine.emptyTurnNudgeLimit を参照。
//
// この数は「1 つの意図の総量」であり「連続何回」ではない：harness 自身の
// stopHookActive が連続空転を 1 回だけに制限しており。その 1 回を押しても空転なら
// Stop フックはもう呼ばれず、run はそのまま終わる。クォータが更新されるのは実際に
// ツールターンが 1 回起きたときだけ(norma/harness/query.go:534)。つまりこのゲートが
// 止めるのは「ツール → 空転 → 押し → ツール → 空転」という病的なループで、意図の
// 予算を使い切らせないためのもの。
const defaultEmptyTurnNudges = 2

// emptyTurnNudge は空転ターンで注入する続行指示。
//
// この種のターンは harness から見れば自然終了(stop_reason=end_turn で tool_use なし)で、
// 5 層の LLM 再試行はどれも当てはまらない。エラーではなく、モデルが「考え終えたが
// 動かなかった」だけ。SDK の空レスポンス再試行も届かない:空かどうかを「イベントを
// yield したか」で判定し、思考デルタ自体がイベント(norma/llm/openai.go の
// SEThinkingDelta)なので thinking-only は空にならない。しかもその層は prompt 全体を
// そのまま再送するため、コンテキストの形に起因するこの空転では再送してもモデルが
// もう一度考えるだけ。ここでは指示を 1 つ追記する形に変え、既に出た思考を抱えたまま
// 先へ進ませる。入力が変わって初めて違う振る舞いをする理由が生まれる。
const emptyTurnNudge = "【空転リマインド】前のターンでは思考プロセスを出力しただけで、本文の返答もツールの呼び出しもありませんでした。" +
	"このターンは成果を生んでいません。先ほど考えた次の手順をそのまま実行してください。ツールを呼び出すか、結論の文章を返してください。思考を繰り返さないでください。"

// isThinkingOnlyTurn reports whether the latest assistant turn produced neither
// text nor a tool call — i.e. the model spent the whole round thinking.
func isThinkingOnlyTurn(messages []llm.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		return strings.TrimSpace(m.Text()) == "" && len(m.ToolUses()) == 0
	}
	return false
}

func (h steerHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if msg, ok := h.drain(); ok {
		return true, "【プランナーのリアルタイム修正】" + msg +
			"\n（これはプランナーから本意図への即時指示です。今回のツール呼び出しは実行されていません。これに従って次の手順を調整してください。現在の予定と衝突する場合は、こちらを優先してください。）", nil
	}
	// 停滞検出の介入メッセージは planner の訂正より後ろに置く:どちらも「まず動かず
	// 聞け」だが、planner の指示の方が優先度が高い。メッセージ自体に完全な文脈
	// (シグネチャ/回数/未実行の説明)があるので、前置きは足さない。
	if h.det != nil {
		if msg, ok := h.det.Drain(); ok {
			return true, msg, nil
		}
	}
	if h.inner != nil {
		return h.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

func (h steerHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	// chains L1:各ステップの tool_result をまず停滞検出器に渡す(純関数、~0 コスト)。
	// 発動時の注入メッセージは検出器が pending に積んでおり(次の PreToolUse drain)、
	// ここは痕跡を残すコールバックだけを担う。
	if h.det != nil {
		if ev := h.det.Observe(name, string(result), isErr); ev != nil {
			log.Printf("[work %s] 停滞検出が発動(%s,連続 %d 回,シグネチャ %s,%d 回目)", h.label, ev.Kind, ev.Count, ev.Signature, ev.N)
			if h.onStuck != nil {
				h.onStuck(ev)
			}
		}
	}
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}

// Stop は guard の元の意味に「空転ターン」の続行を 1 層足す:モデルが思考だけを出力し、
// 本文もツール呼び出しもなかった場合、harness はそれを自然終了とみなして空の summary で
// 終える(query.go の ReasonCompleted + asst.Text())。本来まだ終わっていない意図が
// そこで途切れてしまうので、続行指示を 1 つ注入して既存の思考のまま先へ進ませる。
func (h steerHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	var (
		prevent  bool
		blocking []string
		msg      string
	)
	if h.inner != nil {
		prevent, blocking, msg = h.inner.Stop(ctx, messages)
	}
	// inner が既にハード停止を決めた、または独自の続行メッセージを注入する → 尊重して
	// 重ねない。limit<=0 = ユーザーが「空レスポンス再試行回数」を -1 に設定し、この層を
	// 明示的に切っている。
	if prevent || len(blocking) > 0 || h.nudges == nil || h.limit <= 0 || !isThinkingOnlyTurn(messages) {
		return prevent, blocking, msg
	}
	n := h.nudges.Add(1)
	if n > int64(h.limit) {
		log.Printf("[work %s] 空転ターン(思考のみ、本文もツールもなし)が続行上限 %d に達したため、そのまま終了させる", h.label, h.limit)
		return prevent, blocking, msg
	}
	log.Printf("[work %s] 空転ターン(思考のみ、本文もツールもなし)、続行指示を注入 (%d/%d)", h.label, n, h.limit)
	return false, []string{emptyTurnNudge}, ""
}

// KillWork cancels the in-flight work running intentID (planner's kill_work tool).
// The work's agent-core session honors ctx cancellation and aborts promptly.
func (e *Engine) KillWork(intentID int64) error {
	return e.KillWorkAs(intentID, agent.AbortKilledByPlanner)
}

// KillWorkAs is KillWork with an explicit named cause, so the activity trace can
// tell WHO terminated the work (planner vs main agent — cancelcause.go の名前付き
// キャンセルの規約では、各キャンセル箇所が原因を登録することを求める).
func (e *Engine) KillWorkAs(intentID int64, cause *agent.AbortCause) error {
	e.workMu.Lock()
	run := e.work[intentID]
	e.workMu.Unlock()
	if run == nil {
		return fmt.Errorf("意図 %d には現在実行中の work がありません（終了済みか未取得の可能性）", intentID)
	}
	run.cancel(cause)
	return nil
}
