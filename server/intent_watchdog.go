package server

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

// P0 意図の取得タイムアウトの報告（レッドサン 3 の振り返り R2：決勝意図 #207 は PTH が
// 可能だと証明済みなのに、36 件の open な frontier に埋もれて一度も取得されず、
// ユーザーの 3 回の問いかけがすべて空振りした）。
//
// 仕組み：plannerLoop の watchdog ticker が周期的に frontier を走査し、priority が閾値以上
// かつ open のまま p0ClaimTimeout を超えて取得されていない意図には、hint ノードを探索グラフに
// 書き込む。planner は graph_overview の hints から自然に気付く（mainagent と人も見える）。
// 追加の LLM 呼び出しは発生しない。クールダウン：同じ意図は一度だけ報告する
// （プロセス内の台帳、Engine.p0Reported）。
const (
	// p0PriorityThreshold は「決勝枠」の priority 閾値である：add_intent の priority は 0-10、
	// mainagent の人手直投入は 8-10、planner の既定は 5。8 以上を P0 とみなす。
	p0PriorityThreshold = 8
	// p0ClaimTimeout は P0 意図の取得タイムアウトである：open のままこの時間を超えて
	// 取得されなければ報告する。
	p0ClaimTimeout = 5 * time.Minute
	// p0WatchInterval は巡回周期である：タイムアウトより十分小さければよく、1 回は
	// frontier のクエリ 1 回だけ。
	p0WatchInterval = time.Minute
)

// p0ClaimOverdue はタイムアウト報告の純粋な判定である：open な意図（createdAt に作成、
// 優先度 priority）が now 時点で「P0 の取得期限超過」になっているか、またどれだけ待ったか。
func p0ClaimOverdue(priority int, createdAt, now time.Time) (overdue bool, waited time.Duration) {
	if priority < p0PriorityThreshold {
		return false, 0
	}
	waited = now.Sub(createdAt)
	return waited >= p0ClaimTimeout, waited
}

// p0MarkReported は意図の「報告済み」クールダウンを登録する。false = 報告済みで、呼び出し元はスキップする。
func (e *Engine) p0MarkReported(taskID string, intentID int64) bool {
	v, _ := e.p0Reported.LoadOrStore(taskID, &sync.Map{})
	_, loaded := v.(*sync.Map).LoadOrStore(intentID, true)
	return !loaded
}

// p0Unmark は登録を取り消す（hint の書き込みに失敗した場合、次の周期で再試行できるようにする）。
func (e *Engine) p0Unmark(taskID string, intentID int64) {
	if v, ok := e.p0Reported.Load(taskID); ok {
		v.(*sync.Map).Delete(intentID)
	}
}

// reportOverdueP0Intents は frontier を 1 回走査し、取得期限を過ぎた P0 意図それぞれに
// 「【スケジュール】P0 意図 #N は X 分間未取得のまま待機しています」の hint を書く（意図ごとに 1 回だけ）。
// DB の読み書きだけを行い、planner は起こさない（graph_overview から自然に気付く）。
// LLM 呼び出しも発生しない。
func (e *Engine) reportOverdueP0Intents(t *Task) {
	if t == nil || t.Store == nil {
		return
	}
	if e.IsPaused(t.ID) || e.IsDeleting(t.ID) || e.isSettling(t.ID) {
		return
	}
	if e.m != nil && isTerminalStatus(e.m.TaskStatus(t.ID)) {
		return
	}
	if _, worker := e.snapshotFor(t); worker == nil {
		return // LLM が無いと誰も取得できないため、報告はノイズになる
	}
	frontier, err := t.Store.Frontier(50)
	if err != nil {
		log.Printf("[scheduler] task %s の P0 巡回で frontier の読み込みに失敗しました: %v", t.ID, err)
		return
	}
	now := time.Now()
	for _, n := range frontier {
		overdue, waited := p0ClaimOverdue(n.Priority, n.CreatedAt, now)
		if !overdue {
			continue
		}
		if !e.p0MarkReported(t.ID, n.ID) {
			continue // クールダウン：同じ意図は一度だけ報告する
		}
		msg := fmt.Sprintf("【スケジュール】P0 意図 #%d は %d 分間未取得のまま待機しています。mainagent による指名配信または人手での確認を推奨します",
			n.ID, int(waited.Minutes()))
		_, err := t.Store.AddNode(db.KindHint, map[string]any{
			"text":      msg,
			"kind":      "p0_unclaimed",
			"intent_id": n.ID,
		}, 0, "active", "scheduler", nil)
		if err != nil {
			e.p0Unmark(t.ID, n.ID) // 書き込み失敗時はクールダウンを消費せず、次の周期で再試行する
			log.Printf("[scheduler] task %s の P0 意図 #%d のタイムアウト報告で hint の書き込みに失敗しました: %v", t.ID, n.ID, err)
			continue
		}
		log.Printf("[scheduler] task %s の P0 意図 #%d は %v 取得されていないため、スケジューラのヒントを書き込みました", t.ID, n.ID, waited.Round(time.Second))
	}
}
