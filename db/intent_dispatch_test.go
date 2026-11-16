package db

import (
	"errors"
	"testing"
)

// open 意図のカウント + open 意図の取り消しトランザクション(cancel_intent の db パス)。
// PG 必須：DB が無ければ skip し、パッケージ既存の skip 方式に従う。
func TestCountOpenIntentsAndCancelOpenIntent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "intent dispatch")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	if n, err := es.CountOpenIntents(); err != nil || n != 0 {
		t.Fatalf("empty frontier count=%d err=%v, want 0", n, err)
	}

	open1, err := es.AddIntent(map[string]any{"summary": "stale direction"}, 8, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	open2, err := es.AddIntent(map[string]any{"summary": "dup direction"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	running, err := es.AddIntent(map[string]any{"summary": "claimed direction"}, 9, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := es.ClaimIntent(running, "worker-1"); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if n, err := es.CountOpenIntents(); err != nil || n != 2 {
		t.Fatalf("count=%d err=%v, want 2(running は数えない)", n, err)
	}

	// open 意図の取り消し:状態は stopped、payload に取り消しマーク、理由はファクトとしてグラフに接続、frontier から除外。
	factID, err := es.CancelOpenIntent(open1, "方向は期限切れ", "mainagent")
	if err != nil {
		t.Fatal(err)
	}
	if factID <= 0 {
		t.Fatalf("fact id=%d, want >0", factID)
	}
	node, err := es.GetNode(open1)
	if err != nil || node == nil {
		t.Fatalf("intent node=%v err=%v", node, err)
	}
	if node.State != "stopped" {
		t.Fatalf("state=%q, want stopped", node.State)
	}
	fact, err := es.GetNode(factID)
	if err != nil || fact == nil || fact.Kind != KindFact {
		t.Fatalf("audit fact=%v err=%v", fact, err)
	}
	if n, err := es.CountOpenIntents(); err != nil || n != 1 {
		t.Fatalf("count after cancel=%d err=%v, want 1", n, err)
	}
	frontier, err := es.Frontier(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range frontier {
		if fn.ID == open1 {
			t.Fatal("取り消し済みの意図は frontier に入ってはならない")
		}
	}

	// 重複取り消し → 状態競合、running 意図の取り消し → 状態競合(kill_work の責務)。
	if _, err := es.CancelOpenIntent(open1, "again", "mainagent"); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("re-cancel err=%v, want ErrIntentStateConflict", err)
	}
	if _, err := es.CancelOpenIntent(running, "kill instead", "mainagent"); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("cancel running err=%v, want ErrIntentStateConflict", err)
	}
	if _, err := es.CancelOpenIntent(999999999, "ghost", "mainagent"); err == nil {
		t.Fatal("存在しない意図はエラーになるべきです")
	}

	// その他の意図は影響を受けない。
	keep, err := es.GetNode(open2)
	if err != nil || keep == nil || keep.State != "open" {
		t.Fatalf("unrelated intent=%+v err=%v, want open", keep, err)
	}
}
