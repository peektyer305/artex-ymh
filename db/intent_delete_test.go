package db

import "testing"

// mustIntent / mustNode / mustLink are terse builders for the delete-cascade tests.
func mustIntent(t *testing.T, es *ExplorationStore, summary string) int64 {
	t.Helper()
	id, err := es.AddIntent(map[string]any{"summary": summary}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustNode(t *testing.T, es *ExplorationStore, kind, summary string) int64 {
	t.Helper()
	id, err := es.AddNode(kind, map[string]any{"summary": summary}, 1, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustLink(t *testing.T, es *ExplorationStore, from int64, rel string, to int64) {
	t.Helper()
	if err := es.Link(from, rel, to); err != nil {
		t.Fatal(err)
	}
}

func gone(t *testing.T, es *ExplorationStore, id int64) bool {
	t.Helper()
	n, err := es.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	return n == nil
}

// TestSoftDeleteIntent は論理削除で deleted と delete_reason を設定し、ノードは残ることを確認する。
func TestSoftDeleteIntent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("soft delete", "論理削除")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	intent := mustIntent(t, es, "削除待ちの意図")
	if err := es.SetIntentState(intent, "paused"); err != nil {
		t.Fatal(err)
	}
	summary, err := es.SoftDeleteIntent(intent, "方向の判断を誤った")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "削除待ちの意図" {
		t.Fatalf("summary=%q, want 削除待ちの意図", summary)
	}
	n, err := es.GetNode(intent)
	if err != nil || n == nil {
		t.Fatalf("intent removed by soft delete: n=%+v err=%v", n, err)
	}
	if n.State != StateIntentDeleted || n.DeleteReason != "方向の判断を誤った" {
		t.Fatalf("state=%q delete_reason=%q, want deleted/方向の判断を誤った", n.State, n.DeleteReason)
	}
	// 未割り当て（open）の意図も論理削除できる。
	openIntent := mustIntent(t, es, "未割り当ての意図")
	if _, err := es.SoftDeleteIntent(openIntent, "方向が不要になった"); err != nil {
		t.Fatalf("soft delete open intent: %v", err)
	}
	if n, err := es.GetNode(openIntent); err != nil || n == nil || n.State != StateIntentDeleted {
		t.Fatalf("open intent not soft-deleted: n=%+v err=%v", n, err)
	}

	// 削除済み（deleted）など他の状態は論理削除できない。
	if _, err := es.SoftDeleteIntent(intent, "再削除"); err == nil {
		t.Fatal("soft-deleting an already-deleted intent unexpectedly succeeded")
	}
}

// TestHardDeleteCascadesExclusiveDescendants は物理削除がリンクをたどり排他的な子孫を葉まで連鎖削除することを確認する。
func TestHardDeleteCascadesExclusiveDescendants(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("hard cascade", "カスケード削除")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	// intent1 --yields--> fact1 --derived_from--> intent2 --yields--> fact2（葉ノード）
	intent1 := mustIntent(t, es, "ルート意図")
	fact1 := mustNode(t, es, KindFact, "事実 1")
	mustLink(t, es, intent1, RelYields, fact1)
	intent2 := mustIntent(t, es, "派生意図")
	mustLink(t, es, fact1, RelDerivedFrom, intent2)
	fact2 := mustNode(t, es, KindFact, "事実 2")
	mustLink(t, es, intent2, RelYields, fact2)

	cleanup, err := es.CancelIntent(intent1)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Intents != 2 || cleanup.Facts != 2 {
		t.Fatalf("cleanup=%+v, want 2 intents / 2 facts", cleanup)
	}
	for _, id := range []int64{intent1, fact1, intent2, fact2} {
		if !gone(t, es, id) {
			t.Fatalf("node %d survived cascade", id)
		}
	}
}

// TestHardDeletePreservesSharedAndGoal は物理削除が共有の子孫（他にも親がある）とゴールを残すことを確認する。
func TestHardDeletePreservesSharedAndGoal(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("hard preserve", "共有/ゴールを保持")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	goal, err := es.AddGoal(map[string]any{"text": "管理画面の攻略"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	// intent1 が finding（proves goal）を独占し、intent1 と intentX が fact1 を共有する（fact1 から intent2 が派生）。
	intent1 := mustIntent(t, es, "削除待ちの意図")
	intentX := mustIntent(t, es, "サイドクエスチョン意図")
	finding := mustNode(t, es, KindFinding, "脆弱性")
	mustLink(t, es, intent1, RelYields, finding)
	mustLink(t, es, finding, RelProves, goal)
	shared := mustNode(t, es, KindFact, "共有事実")
	mustLink(t, es, intent1, RelYields, shared)
	mustLink(t, es, intentX, RelYields, shared)
	intent2 := mustIntent(t, es, "共有事実から派生")
	mustLink(t, es, shared, RelDerivedFrom, intent2)

	cleanup, err := es.CancelIntent(intent1)
	if err != nil {
		t.Fatal(err)
	}
	// intent1 とそれが独占する finding だけを削除する;shared（intentX という親がある）とその下流の intent2、goal はすべて残る。
	if cleanup.Intents != 1 || cleanup.Findings != 1 || cleanup.Facts != 0 {
		t.Fatalf("cleanup=%+v, want 1 intent / 1 finding / 0 fact", cleanup)
	}
	if !gone(t, es, intent1) || !gone(t, es, finding) {
		t.Fatal("intent1/finding should be removed")
	}
	for _, id := range []int64{goal, intentX, shared, intent2} {
		if gone(t, es, id) {
			t.Fatalf("node %d was wrongly cascaded", id)
		}
	}
}
