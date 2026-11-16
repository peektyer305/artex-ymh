package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/agent/chainskel"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/jackc/pgx/v5/pgconn"
)

// isFKViolation reports whether err is a Postgres foreign-key violation (SQLSTATE
// 23503) — e.g. an activity insert whose exploration_id has no parent row.
func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// dropReason classifies why an activity write was dropped, so the log can be
// grouped/analysed by cause rather than by raw error text.
func dropReason(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			return "fk_violation(23503,親explorationが存在しない)"
		case "23505":
			return "unique_violation(23505)"
		default:
			return "pg_error(" + pgErr.Code + ")"
		}
	}
	return "write_error"
}

// bumpDrop increments and returns the running count of dropped (unpersistable)
// activity records for a task. Concurrent planner + worker emits race here, so the
// counter is an atomic behind sync.Map. The count in the log shows loss scale at a
// glance instead of forcing a grep-and-count.
func (e *Engine) bumpDrop(taskID string) int64 {
	v, _ := e.dropCnt.LoadOrStore(taskID, new(int64))
	return atomic.AddInt64(v.(*int64), 1)
}

// preview collapses newlines and trims s to a short rune-safe snippet for one-line
// log output (avoids dumping a multi-KB summary/detail into the log).
func preview(s string, n int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// model_error（provider/API の障害：LLM 層の一時的な再試行を使い切った、またはストリーム
// 開始後に途中で切断された）で終了処理した work は「試したが終わらなかった」でも真の失敗でも
// なく、外部の一時的な揺らぎである。既定でこれを恒久的な blocked 扱いにすると意図を 1 件
// 無駄に失うので、この終了状態に対してだけ追加で数回再実行し、その間にバックオフして provider
// の復旧時間を確保する。再試行中に一時停止/終了/キャンセルされた場合は、直ちに対応する
// 分岐へ処理を譲る。
const (
	modelErrorRetries      = 2               // model_error 終了処理後に追加で再試行する回数
	modelErrorRetryBackoff = 3 * time.Second // 各再試行前のバックオフ
	workControlWaitTimeout = 30 * time.Second
)

var errWorkControlConflict = errors.New("work control conflict")

// retryableWorkerModelError excludes errors already handled by the task router.
// In particular, a quota error after partial streaming advances the task cursor
// for the next LLM call but must not replay this whole intent on the backup.
func retryableWorkerModelError(reason harness.TerminalReason, err error) bool {
	return reason == harness.ReasonModelError && !isTaskLLMRuntimeError(err)
}

// Engine drives the event-driven exploration loop with real LLM agents
// (docs §4.3/§4.4): on asset/exploration-graph change (debounced) it wakes the
// planner, which reads the route, queries assets, judges goals and emits intents;
// N concurrent work agents claim intents and execute them. There is no
// simulation mode — an LLM provider is required. The planner/worker can be
// (re)installed at runtime (LLM configured from the UI); the loops always run
// but idle until an LLM is set.
type Engine struct {
	m        *Manager
	debounce time.Duration

	bc *Broadcaster // live activity pub/sub (SSE)

	started  sync.Map // taskID -> bool, so Run is idempotent per task
	lastAct  sync.Map // taskID -> int64 unix, last planner/worker activity (heartbeat)
	llmCalls sync.Map // taskID -> *int64, actual planner/worker/main-agent LLM calls
	paused   sync.Map // taskID -> bool, user-paused (planner + workers idle but loops alive)
	deleting sync.Map // taskID -> bool, delete barrier (no new task-owned writes)
	dropCnt  sync.Map // taskID -> *int64, running count of dropped (unpersistable) activity records

	// deleteMu makes installing the delete barrier atomic with registering a new
	// task operation. Once BeginDelete returns, every admitted writer is reflected
	// in inflight and every later writer is rejected.
	deleteMu sync.RWMutex

	// Every long-lived task goroutine (planner, workers and deadline coordinator)
	// runs under one task-scoped context. Successful deletion cancels that context,
	// waits for all goroutines, then releases every task-level Engine reference.
	runtimeMu sync.Mutex
	runtimes  map[string]*taskRuntime

	// per-task execution context: each planner.Plan / worker.Execute runs under it,
	// so pausing can CANCEL an in-flight run (not just skip the next one). Recreated
	// on resume since cancelling is one-shot. Every cancellation carries a named
	// cause so the activity trace can identify the initiating control path.
	execMu     sync.Mutex
	execCancel map[string]context.CancelCauseFunc
	execCtx    map[string]context.Context

	// Per-work control lets the planner kill a worker and lets the UI pause/cancel
	// one intent without pausing the whole task. The done channel closes only after
	// runWorkerStep has stopped writing and committed its final state.
	workMu sync.Mutex
	work   map[int64]*workExecution

	// steerBox queues planner course-corrections for a running work (keyed by intent
	// id). The worker's PreToolUse hook drains it before its next tool call and hands
	// the message to the model (blocking that call) so it re-plans — no kill needed.
	steerMu  sync.Mutex
	steerBox map[int64][]string

	plannerRound sync.Map // taskID -> int, planner round counter (for UI round separators)

	// P0 意図の未取得タイムアウト通知(红日 3 R2:決勝意図 #207 が一度も取得されない)の
	// クールダウン台帳：taskID -> *sync.Map(intentID -> bool),同じ意図は 1 回だけ通知する。
	// intent_watchdog.go を参照。
	p0Reported sync.Map

	// タスクレベルのタイムアウト（docs/任务级超时与收尾设计.md を参照）:
	settling     sync.Map // taskID -> bool, タスクが終了処理段階に入った（新規意図の派出/取得を停止）
	deadline     sync.Map // taskID -> int64 unix, 絶対期限（初回実行時に刻印;0/未設定=無制限）
	stamped      sync.Map // taskID -> bool, first_run_at を刻印済みか（本プロセス内で 1 回だけ）
	inflight     sync.Map // taskID -> *int64, 実行中の planner.Plan + worker.Execute 数（drain 用）
	coordStarted sync.Map // taskID -> bool, deadline コーディネータを起動済みか（Run/reload の重複排除）

	// resolve returns a task's dedicated planner/worker (wired by the server as the
	// authoritative task-router). nil,nil means this task is deliberately unavailable
	// (for example an exhausted failover chain) — there is no global-pair fallback.
	resolve              func(t *Task) (*agent.Planner, *agent.Worker)
	resolveAuthoritative bool
	// readiness reports whether a global LLM provider is configured — the signal behind
	// Ready()/the llm_configured indicator. Wired once at startup; nil → not ready.
	readiness func() bool
}

type taskRuntime struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type workExecution struct {
	cancel context.CancelCauseFunc
	done   chan error
	action string // user action: pause | cancel
}

// nextPlannerRound returns the next planner round number for a task (1-based).
func (e *Engine) nextPlannerRound(taskID string) int {
	v, _ := e.plannerRound.LoadOrStore(taskID, 0)
	n := v.(int) + 1
	e.plannerRound.Store(taskID, n)
	return n
}

// Pause stops a task: marks it paused AND cancels any in-flight planner/worker run
// for it (a long worker.Execute would otherwise keep going until it finishes).
func (e *Engine) Pause(taskID string, cause error) {
	e.paused.Store(taskID, true)
	e.cancelExec(taskID, cause)
}

// BeginDelete installs an execution barrier before task data/files are removed.
// The temporary pause is not a user pause. The server serializes this transition
// with lifecycle admission and tells AbortDelete whether the persisted task is
// paused/queued if cleanup fails.
func (e *Engine) BeginDelete(taskID string) bool {
	e.deleteMu.Lock()
	if _, loaded := e.deleting.LoadOrStore(taskID, true); loaded {
		e.deleteMu.Unlock()
		return false
	}
	e.paused.Store(taskID, true)
	e.deleteMu.Unlock()
	e.cancelExec(taskID, agent.AbortTaskDeleted)
	return true
}

func (e *Engine) AbortDelete(taskID string, keepPaused bool) {
	e.deleteMu.Lock()
	if !e.IsDeleting(taskID) {
		e.deleteMu.Unlock()
		return
	}
	e.deleting.Delete(taskID)
	if !keepPaused {
		e.paused.Delete(taskID)
	}
	e.deleteMu.Unlock()
	if !keepPaused && e.m != nil {
		if t, ok := e.m.Task(taskID); ok {
			t.Notify()
		}
	}
}

func (e *Engine) IsDeleting(taskID string) bool {
	_, ok := e.deleting.Load(taskID)
	return ok
}

// registerTaskRoutines reserves count goroutines in the task runtime. Callers
// hold deleteMu for reading so StopTask cannot race WaitGroup.Add with Wait.
func (e *Engine) registerTaskRoutines(parent context.Context, taskID string, count int) *taskRuntime {
	e.runtimeMu.Lock()
	defer e.runtimeMu.Unlock()
	rt := e.runtimes[taskID]
	if rt == nil {
		ctx, cancel := context.WithCancel(parent)
		rt = &taskRuntime{ctx: ctx, cancel: cancel}
		e.runtimes[taskID] = rt
	}
	rt.wg.Add(count)
	return rt
}

func runTaskRoutine(rt *taskRuntime, fn func(context.Context)) {
	go func() {
		defer rt.wg.Done()
		fn(rt.ctx)
	}()
}

// StopTask permanently stops every long-lived goroutine and removes all Engine
// state for a successfully deleted task. The delete barrier remains installed
// until cleanup finishes, so no new task operation can race the teardown.
func (e *Engine) StopTask(taskID string) {
	e.deleteMu.Lock()
	e.deleting.Store(taskID, true)
	e.deleteMu.Unlock()

	e.cancelExec(taskID, agent.AbortTaskDeleted)
	e.runtimeMu.Lock()
	rt := e.runtimes[taskID]
	if rt != nil {
		rt.cancel()
	}
	e.runtimeMu.Unlock()
	if rt != nil {
		rt.wg.Wait()
	}

	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(agent.AbortTaskDeleted)
	}
	delete(e.execCancel, taskID)
	delete(e.execCtx, taskID)
	e.execMu.Unlock()

	e.runtimeMu.Lock()
	if e.runtimes[taskID] == rt {
		delete(e.runtimes, taskID)
	}
	e.runtimeMu.Unlock()

	e.started.Delete(taskID)
	e.lastAct.Delete(taskID)
	e.llmCalls.Delete(taskID)
	e.paused.Delete(taskID)
	e.dropCnt.Delete(taskID)
	e.plannerRound.Delete(taskID)
	e.p0Reported.Delete(taskID)
	e.settling.Delete(taskID)
	e.deadline.Delete(taskID)
	e.stamped.Delete(taskID)
	e.inflight.Delete(taskID)
	e.coordStarted.Delete(taskID)
	e.deleteMu.Lock()
	e.deleting.Delete(taskID)
	e.deleteMu.Unlock()

	// フェーズ 3b:タスク削除/アーカイブ時にその MITM インスタンスも破棄する(ポート解除;CA/録画ツリーは保持)。
	if e.m != nil {
		if id, err := strconv.ParseInt(taskID, 10, 64); err == nil {
			e.m.CloseTaskProxy(id)
		}
	}
}

// cancelExec cancels a task's current per-task exec context (any in-flight
// planner.Plan / worker.Execute), if present. Shared by Pause and the settle
// sequence's hard-drain backstop.
func (e *Engine) cancelExec(taskID string, cause error) {
	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(cause)
	}
	e.execMu.Unlock()
}

// Resume un-pauses a task and nudges a fresh planning round. The next exec under
// it gets a fresh (uncancelled) context.
func (e *Engine) Resume(t *Task) {
	// BeginDelete owns the pause barrier once deletion starts. A concurrent
	// resume must never clear it and let a planner/worker re-enter while cleanup
	// is waiting for task operations to drain.
	if t == nil {
		return
	}
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(t.ID) {
		return
	}
	e.paused.Delete(t.ID)
	t.Notify()
}

// execContextFor returns a live per-task context derived from parent, recreating
// it if a prior pause cancelled it.
func (e *Engine) execContextFor(parent context.Context, taskID string) context.Context {
	e.execMu.Lock()
	defer e.execMu.Unlock()
	if e.IsPaused(taskID) {
		// never hand out a live context while paused (guards the claim→Execute race)
		c, cancel := context.WithCancelCause(parent)
		cancel(agent.AbortPausedRaceGuard)
		return c
	}
	if c := e.execCtx[taskID]; c != nil && c.Err() == nil {
		return c
	}
	c, cancel := context.WithCancelCause(parent)
	e.execCtx[taskID] = c
	e.execCancel[taskID] = cancel
	return c
}

// IsPaused reports whether a task is user-paused.
func (e *Engine) IsPaused(taskID string) bool {
	v, ok := e.paused.Load(taskID)
	return ok && v.(bool)
}

// Started reports whether the engine loops are running for a task.
func (e *Engine) Started(taskID string) bool {
	_, ok := e.started.Load(taskID)
	return ok
}

// LastActivity returns the unix time of the last planner/worker activity for a
// task (0 if none yet).
func (e *Engine) LastActivity(taskID string) int64 {
	if v, ok := e.lastAct.Load(taskID); ok {
		return v.(int64)
	}
	return 0
}

// BeginLLMCall/EndLLMCall track actual provider calls separately from the
// scheduler's task-operation counter. A task can have live loops while all of
// them are waiting for a trigger; that state must remain idle in the UI.
func (e *Engine) BeginLLMCall(taskID string) {
	v, _ := e.llmCalls.LoadOrStore(taskID, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

func (e *Engine) EndLLMCall(taskID string) {
	if v, ok := e.llmCalls.Load(taskID); ok {
		p := v.(*int64)
		if atomic.AddInt64(p, -1) <= 0 {
			atomic.StoreInt64(p, 0)
		}
	}
}

func (e *Engine) ActiveLLMCalls(taskID string) int64 {
	if v, ok := e.llmCalls.Load(taskID); ok {
		return atomic.LoadInt64(v.(*int64))
	}
	return 0
}

func (e *Engine) touch(taskID string) { e.lastAct.Store(taskID, time.Now().Unix()) }

func NewEngine(m *Manager) *Engine {
	return &Engine{m: m, debounce: 800 * time.Millisecond, bc: NewBroadcaster(),
		execCancel: map[string]context.CancelCauseFunc{}, execCtx: map[string]context.Context{},
		work: map[int64]*workExecution{}, steerBox: map[int64][]string{},
		runtimes: map[string]*taskRuntime{}}
}

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
// chains L1、CHAINS-INTEGRATION-DESIGN.md §3)も担う。
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
	// det は同質レスポンスの停滞検出器(chains L1、POSTMORTEM-RED-SUN-3.md R4):PostToolUse
	// が各ステップの tool_result を渡し、「情報の勾配がゼロ」と判定したら PreToolUse の
	// drain 経由で方向転換の指示を注入する。ポインタで nudges と同様——しかも意図的に
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
// stopHookActive が連続空転を 1 回だけに制限しており——その 1 回を押しても空転なら
// Stop フックはもう呼ばれず、run はそのまま終わる。クォータが更新されるのは実際に
// ツールターンが 1 回起きたときだけ(norma/harness/query.go:534)。つまりこのゲートが
// 止めるのは「ツール → 空転 → 押し → ツール → 空転」という病的なループで、意図の
// 予算を使い切らせないためのもの。
const defaultEmptyTurnNudges = 2

// emptyTurnNudge は空転ターンで注入する続行指示。
//
// この種のターンは harness から見れば自然終了(stop_reason=end_turn で tool_use なし)で、
// 5 層の LLM 再試行はどれも当てはまらない——エラーではなく、モデルが「考え終えたが
// 動かなかった」だけ。SDK の空レスポンス再試行も届かない:空かどうかを「イベントを
// yield したか」で判定し、思考デルタ自体がイベント(norma/llm/openai.go の
// SEThinkingDelta)なので thinking-only は空にならない。しかもその層は prompt 全体を
// そのまま再送するため、コンテキストの形に起因するこの空転では再送してもモデルが
// もう一度考えるだけ。ここでは指示を 1 つ追記する形に変え、既に出た思考を抱えたまま
// 先へ進ませる——入力が変わって初めて違う振る舞いをする理由が生まれる。
const emptyTurnNudge = "【空转提醒】你上一轮只输出了思考过程，既没有给出正文回复，也没有调用任何工具，" +
	"这一轮等于没有产出。请直接执行你刚才想好的下一步：要么调用工具，要么给出结论文字。不要重复思考。"

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
		return true, "【规划者实时纠偏】" + msg +
			"\n（这是规划者对本意图的即时指令；本次工具调用未执行，请据此调整下一步。若与你当前打算冲突，以此为准。）", nil
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

// Broadcaster exposes the engine's live activity pub/sub (used by the SSE handler).
func (e *Engine) Broadcaster() *Broadcaster { return e.bc }

// emitActivity persists one captured step AND fans it out to live subscribers,
// from a single point so storage and the SSE stream never diverge.
func (e *Engine) emitActivity(t *Task, r db.Activity) db.Activity {
	id, err := e.appendActivity(t, r)
	if err != nil {
		// NO LONGER SILENT: dropping a record breaks command↔result pairing in the
		// trace — a tool_use whose tool_result was lost shows as "実行中" forever, and
		// a lost 'result'/'round' record leaves the session with no summary ("要約なし").
		// Everything needed to analyse the root cause goes into ONE error-level line:
		// reason class, summary preview, running drop count for this task, and — on the
		// FK case — a live probe of WHY the parent exploration is unreachable.
		n := e.bumpDrop(t.ID)
		diag := ""
		// On the FK-parent failure (23503) probe the live DB so the log records WHY the
		// exploration is unreachable (row gone / wrong expID) instead of just that it is.
		if isFKViolation(err) {
			storeID := t.Store.ID()
			if exists, refs, maxID, dErr := e.m.pg.ExplorationDiag(storeID); dErr != nil {
				diag = fmt.Sprintf(" | FK 診断クエリに失敗(store.expID=%d task.ExpID=%d): %v", storeID, t.ExpID, dErr)
			} else {
				diag = fmt.Sprintf(" | FK 診断: store.expID=%d task.ExpID=%d exploration存在=%v これを参照するtask数=%d MAX(exploration.id)=%d",
					storeID, t.ExpID, exists, refs, maxID)
			}
		}
		log.Printf("[activity] task %s アクティビティ記録を破棄(このタスクで累計 %d 件) worker=%s kind=%s tool=%s tuid=%s reason=%s summary=%q: %v%s",
			t.ID, n, r.Worker, r.Kind, r.Tool, r.ToolUseID, dropReason(err), preview(r.Summary, 80), err, diag)
		e.touch(t.ID)
		return r
	}
	r.ID = id
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	e.bc.Publish(t.ID, r)
	e.touch(t.ID)
	return r
}

// appendActivity persists one activity row, retrying briefly on write failure.
// Concurrent planner + worker inserts into the same exploration's activity log
// occasionally fail; a couple of quick retries recover most. Crucially, every
// failure is now LOGGED (it used to be swallowed by an `if err == nil`), so the
// underlying DB error is finally visible for diagnosis.
func (e *Engine) appendActivity(t *Task, r db.Activity) (int64, error) {
	var id int64
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if id, err = t.Store.AppendActivity(r); err == nil {
			if attempt > 1 {
				log.Printf("[activity] task %s 書き込みが %d 回目の再試行で成功 (worker=%s kind=%s tool=%s)",
					t.ID, attempt, r.Worker, r.Kind, r.Tool)
			}
			return id, nil
		}
		log.Printf("[activity] task %s 書き込みに失敗 (%d/3 回目, worker=%s kind=%s tool=%s expID=%d): %v",
			t.ID, attempt, r.Worker, r.Kind, r.Tool, t.Store.ID(), err)
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
	return 0, err
}

// SetReadiness wires the global "an LLM provider is configured" predicate (read by
// Ready() / the llm_configured indicator). Called once at startup.
func (e *Engine) SetReadiness(fn func() bool) { e.readiness = fn }

// SetAgentResolver installs a per-task planner/worker resolver (wired by the server).
// Called once at startup before any task loop runs, so no lock is needed on reads.
func (e *Engine) SetAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = false
}

// SetAuthoritativeAgentResolver installs a resolver whose nil result must not
// fall through to the global provider. Task-level failover chains use this so a
// fully exhausted chain cannot silently bypass its configured boundary.
func (e *Engine) SetAuthoritativeAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = true
}

// snapshotFor returns the planner/worker a task should run on, from the task-router
// resolver. nil,nil means the task is deliberately unavailable (e.g. an exhausted
// failover chain); there is no global-pair fallback.
func (e *Engine) snapshotFor(t *Task) (*agent.Planner, *agent.Worker) {
	if e.resolve != nil {
		p, w := e.resolve(t)
		if (p != nil && w != nil) || e.resolveAuthoritative {
			return p, w
		}
	}
	return nil, nil
}

// Ready reports whether a global LLM provider is configured (via the readiness
// predicate wired at startup).
func (e *Engine) Ready() bool {
	return e.readiness != nil && e.readiness()
}

// ReadyFor reports whether a specific task can resolve a planner/worker pair.
// An explicit task profile chain can be runnable even when no global default
// provider is configured, so task status must not rely on Ready alone.
func (e *Engine) ReadyFor(t *Task) bool {
	p, w := e.snapshotFor(t)
	return p != nil && w != nil
}

// Run starts the planner loop + N worker loops for a task. The loops always run
// but no-op until an LLM is configured (so a task created while idle picks up
// automatically once LLM is set from the UI).
func (e *Engine) Run(ctx context.Context, t *Task) {
	workers := e.m.Workers()
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return
	}
	if _, loaded := e.started.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		t.Notify() // already running — just nudge a planning round
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1+workers)
	e.deleteMu.RUnlock()
	e.touch(t.ID)
	runTaskRoutine(rt, func(loopCtx context.Context) { e.plannerLoop(loopCtx, t) })
	for i := 0; i < workers; i++ {
		name := fmt.Sprintf("work#%d", i+1)
		runTaskRoutine(rt, func(loopCtx context.Context) { e.workerLoop(loopCtx, t, name) })
	}
	e.startDeadlineCoordinator(ctx, t) // タスクレベルのタイムアウトタイマー(timeout>0 のみ;重複排除)
	// 「活動中の意図(open+running)が皆無」のときだけ初回計画を kick する。シード意図付きの
	// タスクは、シードが既に open か、上で起動した worker に先に claim されて running の
	// どちらかで「やることがある」ので初回 planner を一律に飛ばし、worker がシード意図を
	// 取って走り、終わったら NotifyDone/ハートビートが planner を起こす。
	// ⚠️ Frontier(open しか数えない)は使えない：worker の取得(open→running)とこの検査が
	// 競合して誤 kick する。再起動時の自動復旧では running 意図しか残らないこともあり、
	// 同じく飛ばすべき。
	if has, _ := t.Store.HasActiveIntent(); !has {
		t.Notify() // kick the first planning round (acted on once LLM is ready)
	}
}

// plannerHeartbeatInterval はタスクの planner ハートビート間隔を解決する。db.CreateTask で
// 正規化済み(600 未満は一律 600 へ引き上げ)だが、ここでもう一度下限を掛けてメモリ上の
// 異常値を防ぐ。
func plannerHeartbeatInterval(t *Task) time.Duration {
	sec := t.PlanHeartbeatSeconds
	if sec < db.MinPlanHeartbeatSeconds { // 下限=既定=600(10min)
		sec = db.MinPlanHeartbeatSeconds
	}
	return time.Duration(sec) * time.Second
}

// resetPlannerTimer は発火済みかもしれない Timer を安全に再アームする(標準の Stop→drain→Reset パターン)。
func resetPlannerTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (e *Engine) plannerLoop(ctx context.Context, t *Task) {
	interval := plannerHeartbeatInterval(t)
	// ハートビートタイマーは loop 入口でアーム = タスク start からの計時:初回 planner を
	// 飛ばすシードタスク(Run で frontier が非空なら初回を kick しない)でここがずっと
	// ブロックしていても、「タスク start + interval」で初回の計画が発火する。以降は毎回の
	// 起床(エッジ/ハートビート)で再アーム = 直前の任意の計画発火からの経過時間になる。
	heartbeat := time.NewTimer(interval)
	defer heartbeat.Stop()

	// runRound は計画を 1 ラウンド実行する(debounce の合流 + 各 guard を含む)。src はログで発火元を区別するためだけに使う。
	runRound := func(src string) {
		// debounce: coalesce a burst of changes into one planning round
		timer := time.NewTimer(e.debounce)
	drain:
		for {
			select {
			case <-t.notify:
			case <-timer.C:
				break drain
			}
		}
		planner, _ := e.snapshotFor(t)
		if planner == nil {
			return // idle until LLM configured
		}
		if e.IsPaused(t.ID) {
			return // user-paused: don't plan
		}
		if e.IsDeleting(t.ID) {
			return
		}
		// terminal task (goals all met → done, or failed): the run is over. A
		// resume/nudge — e.g. auto-resume of the active task on restart — must NOT
		// re-plan (it would burn an LLM round and re-confirm a settled result).
		if isTerminalStatus(t.lifecycleSnapshot().Status) {
			return
		}
		// タスクタイムアウトの終了処理中:通常の起床は捨てる——worker の終了処理書き戻しも
		// Resume の Notify も通常の計画ラウンドを発火しない。終局の 1 ラウンドは
		// コーディネータ(settleTask)が直接駆動し、ここは通らない。
		if e.isSettling(t.ID) {
			return
		}
		// goalless(人が直接投入)分岐:タスクに open のゴールが無いとき planner は走らない
		// ——走ると再判定で met→cancelExec となり、ユーザーがメイン agent 経由で直接投入
		// した意図を殺してしまう。終了可否は frontier が決める：open/running の意図が残って
		// いれば running のまま静かに待ち、意図をすべて走り切ったら done にする。この一連は
		// 純 Go で、LLM 呼び出しも計画ラウンドの marker も発生しない。
		if open, err := t.Store.HasOpenGoal(); err == nil && !open {
			t.drainTriggers() // 溜まった done/finding トリガーを捨て、goalless の長いセッションで無制限に増やさない
			if active, err := t.Store.HasActiveIntent(); err == nil && !active {
				// frontier が尽きて実行中の意図も無い → 終了処理。Guarded 版で CAS し、
				// 並行する pause/delete/タイムアウト終了処理の状態遷移を踏まない。
				if won, err := e.m.SetTaskStatusGuarded(t.ID, "done"); err != nil {
					log.Printf("[goalless] task %s 終了処理の done 保存に失敗: %v", t.ID, err)
				} else if won {
					e.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
						Summary: "ゴールはすべて達成済みで、直接投入した意図も実行し終えたため、タスクを終了します"})
				}
			}
			return // goalless 分岐は planner.Plan に決して入らない
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		defer e.decInflight(t.ID)
		e.stampFirstRun(t) // 初回の本格的な計画 → first_run_at を刻印 + deadline を算出(timeout 付きのタスクのみ)
		e.touch(t.ID)
		emit := func(r db.Activity) { e.emitActivity(t, r) }
		ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // cancellable by Pause; タスク deadline 付き
		if ectx.Err() != nil || e.IsDeleting(t.ID) {
			return
		}
		log.Printf("[planner] task %s 計画中…(%s により発火)", t.ID, src)
		// round marker: each Plan() is one planner round; emit a boundary so the
		// UI can separate rounds in the transcript (kind='round').
		e.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
			Summary: fmt.Sprintf("第 %d 回計画", e.nextPlannerRound(t.ID))})
		// what fired this round (worker done / finding; may be several — debounce
		// coalesces a burst; empty for time/heartbeat wakes).
		triggers := t.drainTriggers()
		taskIDInt, _ := strconv.ParseInt(t.ID, 10, 64)
		e.BeginLLMCall(t.ID)
		met, reason, err := planner.Plan(ectx, taskIDInt, e.m.assets, t.Store, t.Goal, triggers, emit)
		e.EndLLMCall(t.ID)
		switch {
		case err != nil && ectx.Err() == nil:
			log.Printf("[planner] task %s 計画でエラー: %v", t.ID, err)
		case met:
			log.Printf("[planner] task %s ゴール達成と判定: %s", t.ID, reason)
			// すべてのゴール達成 → タスク状態を done として永続化(フロントの DTO はこの終了状態を優先表示する)。
			if err := e.m.SetTaskStatus(t.ID, "done"); err != nil {
				log.Printf("[planner] task %s 完了マークの保存に失敗: %v", t.ID, err)
			}
			// タスクが完了判定になった → 実行中の worker を即座にキャンセルする。手元の意図を
			// 走らせてももう意味がない。次に worker ループが終了状態ゲートに当たれば新規意図は
			// 取らず、キャンセルされた分は下の「タスク完了」分岐で stopped(blocked ではない)
			// として扱われる。
			e.cancelExec(t.ID, agent.AbortGoalMet)
		default:
			log.Printf("[planner] task %s 計画完了", t.ID)
		}
		e.touch(t.ID)
	}

	// P0 意図の未取得タイムアウト巡回:計画ハートビートとは独立(ハートビート間隔が ≥10min
	// に対し P0 のタイムアウトは 5min。しかも全 worker が多忙だとどの worker ループも
	// runIntent の中にいて、frontier の P0 滞留に誰も気づかない)。db の読み取り + 一度
	// きりの hint 書き込みのみで、LLM 呼び出しは発生しない。intent_watchdog.go を参照。
	watchdog := time.NewTicker(p0WatchInterval)
	defer watchdog.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-watchdog.C:
			e.reportOverdueP0Intents(t)
			continue // 巡回は計画の発火ではないので、計画ハートビートを再アームしない
		case <-t.notify:
			runRound("edge") // worker 終了 / finding / kill / resume / シード初回
		case <-heartbeat.C:
			// 周期的な安全網:デッドロックの保険 + 実行中の worker を監督する起床(steer/kill) + 定期再確認。
			runRound("heartbeat")
		}
		// 毎回の起床(エッジまたはハートビート)の後にハートビートを再アーム:任意の計画発火でこの静止計時を計算し直す。
		resetPlannerTimer(heartbeat, interval)
	}
}

func (e *Engine) workerLoop(ctx context.Context, t *Task, name string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, worker := e.snapshotFor(t)
		if worker == nil {
			if sleepCtx(ctx, 1500*time.Millisecond) {
				return
			}
			continue
		}
		if e.IsPaused(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // user-paused: don't claim/execute intents
		}
		if e.IsDeleting(t.ID) {
			return
		}
		if e.isSettling(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // タスクタイムアウト終了処理中:新規意図は取らない(実行中のものは自力で終了処理し、コーディネータがその drain を待つ)
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // タスクが終了状態(done/failed/timeout):残った意図の取得を止め、完了後に frontier を空回ししない
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		claimed := e.runWorkerStep(ctx, t, name, worker)
		e.decInflight(t.ID)
		if !claimed && sleepCtx(ctx, 800*time.Millisecond) {
			return
		}
	}
}

// runWorkerStep claims one intent from the frontier and fully settles it via
// runIntent. Returns false when nothing was claimable. The pool worker loop is its
// only caller.
func (e *Engine) runWorkerStep(ctx context.Context, t *Task, name string, worker *agent.Worker) bool {
	intent := e.claimNext(t, name)
	if intent == nil {
		return false
	}
	log.Printf("[worker %s] task %s 意図 #%d を取得", name, t.ID, intent.ID)
	return e.runIntent(ctx, t, name, worker, intent, "", "")
}

// runIntent executes and fully settles one already-claimed (state=running) intent.
// Both the pool worker loop (via runWorkerStep) and the human-message handler (via
// runDetachedIntent, a dedicated goroutine outside the worker pool) call it, so the
// execute/retry/state-write logic lives in exactly one place. A non-empty message
// is injected as this turn's input through ExecuteWithMessage; requestID keys the
// transcript marker that dedups re-injection across model_error retries. The caller
// must already hold one task-operation admission for the whole sequence so a delete
// cannot observe quiescence between the LLM return and the final DB writes.
func (e *Engine) runIntent(ctx context.Context, t *Task, name string, worker *agent.Worker, intent *db.Node, requestID, message string) bool {
	hasChatMessage := message != ""
	e.stampFirstRun(t) // 初回の本格的な実行 → first_run_at を刻印 + deadline を算出(timeout 付きのタスクのみ)
	e.touch(t.ID)
	emit := func(r db.Activity) { e.emitActivity(t, r) }
	ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // cancellable by Pause; タスク deadline 付き
	if ectx.Err() != nil || e.IsDeleting(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d の取得後の巻き戻しに失敗: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// per-work child context so the planner's kill_work can stop just this work.
	workCtx, workCancel := context.WithCancelCause(ectx)
	e.registerWork(intent.ID, workCancel)
	// wrap the guard hooks so steer_work can inject a mid-run course-correction
	// for THIS intent (drained before the worker's next tool call).
	iid := intent.ID
	taskEmit := func(a db.Activity) {
		nid := iid
		a.NodeID, a.Worker = &nid, name
		emit(a)
	}
	label := fmt.Sprintf("%s · #%d", name, iid)
	workCtx = intercept.WithTaskContext(workCtx, t.ID, label, taskEmit)
	// nudges は意図的に model_error の再実行ループの外に置く:空転続行の上限は「この意図」
	// の総量で、再実行 1 回で枠をゼロに戻すべきではない。
	hooks := steerHooks{
		inner:  t.Guard.Hooks(),
		drain:  func() (string, bool) { return e.drainSteer(iid) },
		nudges: &atomic.Int64{},
		limit:  e.emptyTurnNudgeLimit(),
		label:  label,
	}
	// chains L1 停滞検出器も同様:再実行ループの外に置き、クールダウン/発動上限は
	// 「この意図」単位で数え、model_error の再実行をまたいで共有する(steerHooks.det の
	// コメント参照)。発動時は方向転換の指示を注入するほか、planner に伝える痕跡を 2 つ
	// 残す:
	//  1) system activity —— worker trace に入り、planner が get_worker_output で見える;
	//  2) hint ノードを探索グラフに付ける —— TriggerEvent に system 種別のイベントは
	//     無く(manager.go の pendingTriggers は done/finding/goal*/cancelled のみ)、
	//     planner は毎ラウンド graph overview で hints を必ず読む(agent/tools.go
	//     graphOverviewData)。これが既存構造のうち検出信号を planner の視野に入れる
	//     既存の経路(add_hint の経路を参照)。
	hooks.det = agent.NewStuckDetector(agent.DefaultStuckConfig())
	// chains L3 → L1:停滞の発動時にカテゴリ別の方向転換案を添える(意図の chain_tags に
	// 応じたカテゴリの骨格から「方向転換」項目を抜き出し、一致が無ければ空のままで汎用の
	// 文言を維持)。一度だけ算出し、この意図の間は変わらない。
	hooks.det.PivotHint = chainskel.PivotHintsForIntent(intent.Payload, 3)
	hooks.onStuck = func(ev *agent.StuckEvent) {
		taskEmit(db.Activity{Kind: "system", Tool: ev.Tool,
			Summary: fmt.Sprintf("停滞検出:同質レスポンスが連続 %d 回(%s,シグネチャ %s)", ev.Count, ev.Kind, ev.Signature),
			Detail:  agent.StuckIntervention(*ev)})
		if t.Store != nil {
			if _, err := t.Store.AddNode(db.KindHint, map[string]any{"text": fmt.Sprintf(
				"【停滞检测】意图 #%d 的 worker 连续 %d 次同质/相似失败响应(签名 %s,工具 %s),信息梯度为零,平台已注入转向指令(第 %d 次)。规划时请避开同轴方向(换参数/信道/漏洞类/入口),或确认该方向已榨干再收尾。",
				iid, ev.Count, ev.Signature, ev.Tool, ev.N)}, 0, "active", "system", nil); err != nil {
				log.Printf("[work %s] 停滞検出 hint のグラフ書き込みに失敗: %v", label, err)
			}
		}
	}
	wTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
	e.BeginLLMCall(t.ID)
	var reason harness.TerminalReason
	var wrote agent.WriteCounts
	var err error
	if hasChatMessage {
		reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
	} else {
		reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
	}
	e.EndLLMCall(t.ID)
	// model_error で終了処理 → 追加で数回再実行する(バックオフしてから再試行)。意図がまだ
	// 本 work に属し、タスクが一時停止/終了/キャンセルされておらず【かつ終了処理に入って
	// いない】ときだけ再試行する。それ以外は対応する分岐に譲る(終了処理期は再試行せず、
	// バックオフが他の worker の正常な終了処理の窓を圧迫しないようにする)。
	maxRetries, retryBackoff := e.modelErrorRetryPolicy()
	for attempt := 1; attempt <= maxRetries &&
		retryableWorkerModelError(reason, err) &&
		workCtx.Err() == nil && ectx.Err() == nil && !e.IsPaused(t.ID) && !e.isSettling(t.ID); attempt++ {
		log.Printf("[worker %s] task %s 意図 #%d が model_error で終了処理、%v 後に再試行 (%d/%d)",
			name, t.ID, intent.ID, retryBackoff, attempt, maxRetries)
		if sleepCtx(workCtx, retryBackoff) {
			break // バックオフ中にキャンセルされた(終了/一時停止)→ 下の分岐に処理を譲る
		}
		e.BeginLLMCall(t.ID)
		if hasChatMessage {
			reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
		} else {
			reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
		}
		e.EndLLMCall(t.ID)
	}
	// Capture kill state before detachWork cancels workCtx. kill = this work's
	// ctx was cancelled (planner kill_work) while the TASK ctx kept running; a
	// pause cancels the task ctx (ectx) instead. Checking workCtx.Err() AFTER
	// unregister would always be true (unregister cancels it) → every completed
	// work would be wrongly marked stopped.
	workCause := context.Cause(workCtx)
	killed := workCtx.Err() != nil && ectx.Err() == nil
	action, completeWork := e.detachWork(intent.ID)
	// A caller may stop waiting and release its in-memory reservation before the
	// agent honors cancellation. The named context cause remains authoritative and
	// still settles the stopped run into a recoverable state.
	if action == "" {
		switch {
		case errors.Is(workCause, agent.AbortWorkPausedByUser):
			action = "pause"
		case errors.Is(workCause, agent.AbortWorkCancelledByUser):
			action = "cancel"
		}
	}
	var controlErr error
	defer func() { completeWork(controlErr) }()
	if action == "pause" {
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 意図 #%d の一時停止状態の保存に失敗: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 意図 #%d を一時停止しました", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	if action == "cancel" {
		// Park the stopped run in paused before handing cleanup to the API. If the
		// request disconnects after cancellation, the intent remains recoverable and
		// a later cancel can finish cleanup instead of leaving a phantom running row.
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 意図 #%d のキャンセルフェンス保存に失敗: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 意図 #%d を停止しました。キャンセル時のクリーンアップを待機", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// if a pause cancelled this run mid-flight, return the intent to the frontier
	// so it is re-claimed on resume — the worker will resume the prior LLM
	// conversation from its transcript instead of restarting from scratch.
	if ectx.Err() != nil && taskExecutionPaused(context.Cause(ectx)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d のタスク一時停止による巻き戻しに失敗: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// タスクタイムアウト終了処理のハードな保険 cancel(pause でも kill でもない)が本 run を
	// キャンセルした → exhausted(終了処理済み)として扱い、誤って blocked にしない。この場合
	// worker は通常 settlement 段階で結果を書き戻し済み。
	if ectx.Err() != nil && e.isSettling(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "exhausted"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d のタイムアウト終了処理状態の保存に失敗: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 意図 #%d はタスクタイムアウト終了処理で終了(exhausted)、書き戻し %s", name, t.ID, intent.ID, wrote)
		e.touch(t.ID)
		return true
	}
	// タスクが完了判定になった(通常経路で done)→ 上の cancelExec が本 run をキャンセル。
	// 意図の結果はもう意味が無いので stopped(blocked ではない)とし、完了済みタスクの
	// 意図状態を汚さない。
	if ectx.Err() != nil && isTerminalStatus(e.m.TaskStatus(t.ID)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d の終了状態停止の保存に失敗: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 意図 #%d はタスク完了によりキャンセル(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// killed by the planner: mark stopped (don't write back results, don't auto-reclaim).
	if killed {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d の planner 停止の保存に失敗: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 意図 #%d を終了させました(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		t.Notify()
		return true
	}
	if err != nil {
		log.Printf("[worker %s] intent %d: %v", name, intent.ID, err)
	}
	// terminal の分岐:ステップ上限到達 ≠ 完了。max_turns→exhausted(planner はこの方向を
	// 試したが本当には終わっていないと分かり、角度を変える必要がある。既にカバー済みとして
	// 恒久的に飛ばすのではない);エラー→blocked;正常→done。
	state := "done"
	switch {
	case err != nil:
		state = "blocked"
	case reason == harness.ReasonMaxTurns:
		state = "exhausted"
		log.Printf("[worker %s] intent %d ステップ上限に到達(exhausted)、今回の書き戻し %s", name, intent.ID, wrote)
	case reason == harness.ReasonTimeout:
		state = "exhausted"
		log.Printf("[worker %s] intent %d 実行タイムアウト(exhausted)、終了処理後に書き戻し %s", name, intent.ID, wrote)
	}
	if state == "blocked" && isTaskLLMChainExhausted(err) {
		_ = t.Store.SetIntentBlockedReason(intent.ID, db.IntentBlockedLLMQuota)
	} else {
		if stateErr := transitionIntentState(t.Store, intent.ID, "running", state); stateErr != nil {
			log.Printf("[worker %s] task %s 意図 #%d の終了状態 %s の保存に失敗: %v", name, t.ID, intent.ID, state, stateErr)
		}
	}
	log.Printf("[worker %s] task %s 意図 #%d 終了: %s (書き戻し %s)", name, t.ID, intent.ID, state, wrote)
	e.touch(t.ID)
	t.NotifyDone(intent.ID) // results changed the graph -> wake the planner (with the just-finished intent id)
	return true
}

// runDetachedIntent runs one paused intent OUTSIDE the worker pool in its own
// goroutine — the human-message path. It transitions the intent paused->running
// itself (never through 'open'), so the pool, which only claims 'open', can never
// race it; the "at most one run per intent" invariant still holds because winning
// the CAS is the sole entry and work[intentID] was cleared when the pause settled.
// Because it does not compete for a frontier slot, a user message continues the
// worker immediately even when all pool slots are busy (mirroring how the
// main-agent chat handler starts its run directly). The spawned goroutine owns one
// task-operation admission for the whole run and roots its context at ctx (pass the
// server root, never the HTTP request, so a disconnect cannot strand the run while
// task pause/delete/shutdown still stops it). Returns an error if the run could not
// be started; the intent is left untouched in that case.
func (e *Engine) runDetachedIntent(ctx context.Context, t *Task, intentID int64, requestID, message, agentMessage string) error {
	if !e.beginTaskOperation(t.ID) {
		return fmt.Errorf("task is being deleted")
	}
	release := true
	defer func() {
		if release {
			e.decInflight(t.ID)
		}
	}()
	_, worker := e.snapshotFor(t)
	if worker == nil {
		return fmt.Errorf("worker がまだ準備できていません")
	}
	node, err := t.Store.GetNode(intentID)
	if err != nil {
		return err
	}
	if node == nil || node.Kind != db.KindIntent {
		return fmt.Errorf("intent not found")
	}
	changed, err := t.Store.CompareAndSetIntentState(intentID, "paused", "running")
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 意図がもう paused 状態ではありません", db.ErrIntentStateConflict)
	}
	node.State, node.Owner = "running", "chat"
	// Record the human turn as a visible activity BEFORE the run starts, so it is
	// ordered ahead of any worker step and never appears without the run happening.
	// Keep the UI copy concise; ExecuteWithMessage writes the server-resolved
	// reference snapshot into the intent transcript as the LLM input.
	uid := intentID
	e.emitActivity(t, db.Activity{NodeID: &uid, Worker: "user", Kind: "user", Summary: message, Detail: message})
	release = false // ownership of the admission passes to the goroutine
	go func() {
		defer e.decInflight(t.ID)
		e.runIntent(ctx, t, "chat", worker, node, requestID, agentMessage)
	}()
	return nil
}

func taskExecutionPaused(cause error) bool {
	var abort *agent.AbortCause
	if !errors.As(cause, &abort) {
		return false
	}
	switch abort.Code {
	case "paused_by_user", "paused_by_orchestrator", "paused_on_reload", "paused_race_guard",
		"queued_for_admission", "llm_unavailable_queued", "task_deleted":
		return true
	default:
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) (done bool) {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func (e *Engine) claimNext(t *Task, name string) *db.Node {
	fr, _ := t.Store.Frontier(20)
	for _, in := range fr {
		if ok, _ := t.Store.ClaimIntent(in.ID, name); ok {
			return in
		}
	}
	return nil
}
