package server

import (
	"context"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

// P0 受領タイムアウトの純粋な判定:優先度のしきい値 + 5 分の待機時間。
func TestP0ClaimOverdue(t *testing.T) {
	now := time.Now()
	created := now.Add(-6 * time.Minute)
	for _, tc := range []struct {
		name      string
		priority  int
		createdAt time.Time
		want      bool
	}{
		{"P0 が 6 分待ち → 期限超過", 8, created, true},
		{"最高段 10 が 6 分待ち → 期限超過", 10, created, true},
		{"P0 が 4 分待ち → 期限内", 8, now.Add(-4 * time.Minute), false},
		{"しきい値未満の 7 はいくら待っても報告しない", 7, now.Add(-time.Hour), false},
		{"デフォルト段 5 はいくら待っても報告しない", 5, now.Add(-time.Hour), false},
		{"しきい値境界 8 がちょうど 5 分 → 期限超過", 8, now.Add(-p0ClaimTimeout), true},
	} {
		overdue, waited := p0ClaimOverdue(tc.priority, tc.createdAt, now)
		if overdue != tc.want {
			t.Errorf("%s: overdue=%v, want %v", tc.name, overdue, tc.want)
		}
		if tc.want && waited < p0ClaimTimeout {
			t.Errorf("%s: waited=%v < p0ClaimTimeout", tc.name, waited)
		}
	}
	if p0ClaimTimeout != 5*time.Minute {
		t.Fatalf("p0ClaimTimeout=%v, want 5m(R2 修正の契約)", p0ClaimTimeout)
	}
}

// クールダウン:同じ意図は 1 回だけ通知する。取り消し後は再通知を許可する(hint の DB 書き込み失敗時の再試行経路)。
func TestP0ReportedCooldown(t *testing.T) {
	e := NewEngine(nil)
	if !e.p0MarkReported("t1", 207) {
		t.Fatal("初回の記録は true を返すべき")
	}
	if e.p0MarkReported("t1", 207) {
		t.Fatal("同じ意図の再記録は false を返すべき(1 回だけ報告)")
	}
	if !e.p0MarkReported("t1", 208) {
		t.Fatal("異なる意図同士は影響しない")
	}
	if !e.p0MarkReported("t2", 207) {
		t.Fatal("異なるタスク同士は影響しない")
	}
	e.p0Unmark("t1", 207)
	if !e.p0MarkReported("t1", 207) {
		t.Fatal("unmark 後は再報告できるべき")
	}
}

// mainagent の kill_work は killed_by_mainagent という名前付き理由を必ず伴い、その work だけをキャンセルし、
// タスク全体の ctx には波及しない(planner の killed_by_planner と対比)。
func TestKillWorkAsCarriesMainagentCause(t *testing.T) {
	e := NewEngine(nil)
	execCtx := e.execContextFor(context.Background(), "t1")
	workCtx, workCancel := context.WithCancelCause(execCtx)
	e.registerWork(42, workCancel)
	if err := e.KillWorkAs(42, agent.AbortKilledByMainagent); err != nil {
		t.Fatal(err)
	}
	if code, _, _, ok := agent.AbortReason(workCtx); !ok || code != "killed_by_mainagent" {
		t.Fatalf("code=%q ok=%v, want killed_by_mainagent", code, ok)
	}
	if execCtx.Err() != nil {
		t.Fatal("mainagent kill_work cancelled the entire task context")
	}
	// planner の経路は元の理由のまま。
	workCtx2, workCancel2 := context.WithCancelCause(execCtx)
	e.registerWork(43, workCancel2)
	if err := e.KillWork(43); err != nil {
		t.Fatal(err)
	}
	if code, _, _, _ := agent.AbortReason(workCtx2); code != "killed_by_planner" {
		t.Fatalf("planner kill code=%q, want killed_by_planner", code)
	}
	_, complete := e.detachWork(42)
	complete(nil)
	_, complete2 := e.detachWork(43)
	complete2(nil)
}
