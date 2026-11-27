package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/agent/chainskel"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/harness"
	"log"
	"strconv"
	"sync/atomic"
	"time"
)

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
		// タスクタイムアウトの終了処理中:通常の起床は捨てる。worker の終了処理書き戻しも
		// Resume の Notify も通常の計画ラウンドを発火しない。終局の 1 ラウンドは
		// コーディネータ(settleTask)が直接駆動し、ここは通らない。
		if e.isSettling(t.ID) {
			return
		}
		// goalless(人が直接投入)分岐:タスクに open のゴールが無いとき planner は走らない
		//。走ると再判定で met→cancelExec となり、ユーザーがメイン agent 経由で直接投入
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
	//  1) system activity。worker trace に入り、planner が get_worker_output で見える;
	//  2) hint ノードを探索グラフに付ける。TriggerEvent に system 種別のイベントは
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
				"【停滞検出】意図 #%d の worker が同質/類似の失敗応答を連続 %d 回繰り返しています(シグネチャ %s、ツール %s)。情報の勾配がゼロになったため、プラットフォームが方向転換の指示を注入しました(%d 回目)。計画時は同じ軸の方向(パラメータ変更/チャネル/脆弱性クラス/入口)を避けるか、その方向をやり尽くしたことを確認してから収束してください。",
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
