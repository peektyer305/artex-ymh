package db

import (
	"testing"
	"time"
)

// pendingScopeTaskID はタイムスタンプから隔離された task_id を派生する（pending_scope に外部キーはなく、
// tasks 表にも依存しない）。テストを何度実行しても互いに干渉しない。
func pendingScopeTaskID() int64 { return time.Now().UnixNano() }

// ライフサイクル全体：upsert の冪等性（pending のみ累積）→ 一覧の絞り込み → decide → 決定済みは再累積しない。
func TestPendingScopeLifecycle(t *testing.T) {
	d, as, _ := testSetup(t)
	defer d.Close()
	taskID := pendingScopeTaskID()

	created, err := as.UpsertPendingScope(taskID, "cidr", "10.9.8.0/24")
	if err != nil || !created {
		t.Fatalf("初回の upsert は created=true であるべき: created=%v err=%v", created, err)
	}
	// 重複登録：created=false で、hits が累積される。
	created, err = as.UpsertPendingScope(taskID, "cidr", "10.9.8.0/24")
	if err != nil || created {
		t.Fatalf("重複した upsert は created=false であるべき: created=%v err=%v", created, err)
	}
	items, err := as.ListPendingScope(taskID, false)
	if err != nil || len(items) != 1 {
		t.Fatalf("pending 一覧は 1 件であるべき: %v err=%v", items, err)
	}
	row := items[0]
	if row.Status != "pending" || row.Hits != 2 || row.Value != "10.9.8.0/24" {
		t.Fatalf("エントリのステータスが一致しません: %+v", row)
	}
	if row.DecidedAt != nil {
		t.Fatalf("pending のエントリに decided_at があってはなりません: %+v", row)
	}

	// approve:ステータス遷移 + decided_at;以降の upsert は累積しない（重複通知の防止）。
	decided, err := as.DecidePendingScope(row.ID, true)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decided.Status != "approved" || decided.DecidedAt == nil {
		t.Fatalf("approve 後のステータスが一致しません: %+v", decided)
	}
	if created, err = as.UpsertPendingScope(taskID, "cidr", "10.9.8.0/24"); err != nil || created {
		t.Fatalf("決定済みエントリの upsert は created=false であるべき: created=%v err=%v", created, err)
	}
	again, err := as.GetPendingScope(row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if again.Hits != 2 || again.Status != "approved" {
		t.Fatalf("決定済みエントリはそれ以上累積されてはなりません: %+v", again)
	}
	// 競合する決定は元の状態を変えない（現状を返す）。
	conflict, err := as.DecidePendingScope(row.ID, false)
	if err != nil || conflict.Status != "approved" {
		t.Fatalf("競合する決定は現状の approved を返すべき: %+v err=%v", conflict, err)
	}

	// デフォルト一覧は決定済みを含まない;includeDecided は含む。
	if items, _ = as.ListPendingScope(taskID, false); len(items) != 0 {
		t.Fatalf("デフォルトの一覧は決定済みを含まないべき: %v", items)
	}
	if items, _ = as.ListPendingScope(taskID, true); len(items) != 1 {
		t.Fatalf("includeDecided は決定済みを含むべき: %v", items)
	}

	// dismiss の経路。
	created, err = as.UpsertPendingScope(taskID, "cidr", "10.9.9.0/24")
	if err != nil || !created {
		t.Fatalf("新規エントリの upsert: created=%v err=%v", created, err)
	}
	items, _ = as.ListPendingScope(taskID, false)
	if len(items) != 1 {
		t.Fatalf("新規 pending が 1 件あるべき: %v", items)
	}
	dismissed, err := as.DecidePendingScope(items[0].ID, false)
	if err != nil || dismissed.Status != "dismissed" || dismissed.DecidedAt == nil {
		t.Fatalf("dismiss 後のステータスが一致しません: %+v err=%v", dismissed, err)
	}
}

// 引数のガード:不正な入力は no-op;該当なしの場合は sql.ErrNoRows を返す。
func TestPendingScopeGuards(t *testing.T) {
	d, as, _ := testSetup(t)
	defer d.Close()

	for _, args := range [][3]any{{int64(0), "cidr", "1.2.3.0/24"}, {int64(1), "", "1.2.3.0/24"}, {int64(1), "cidr", ""}} {
		created, err := as.UpsertPendingScope(args[0].(int64), args[1].(string), args[2].(string))
		if err != nil || created {
			t.Fatalf("不正な引数は no-op であるべき: %v → created=%v err=%v", args, created, err)
		}
	}
	if _, err := as.GetPendingScope(-1); err == nil {
		t.Fatalf("該当レコードがなければエラーになるべきです")
	}
}
