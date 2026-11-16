package db

import (
	"context"
	"encoding/json"
	"testing"
)

// tunnels テーブルの CRUD には実物の dev PG(ARTEX_PG_DSN)が必要。DSN が無い場合はパッケージの約束どおり skip する。
func TestTunnelStoreCRUD(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("dev PG が利用できません: %v", err)
	}
	defer d.Close()
	ctx := context.Background()
	s := NewTunnelStore(d)

	params, _ := json.Marshal(map[string]any{
		"auth_user": "artex", "auth_pass": "deadbeef", "server_pid": 1234,
		"socks_port": 20002, "stage_token": "tok",
	})
	id, err := s.Create(ctx, &TunnelRecord{
		TaskID: 999001, Kind: "socks", ListenHost: "0.0.0.0", ListenPort: 20001,
		ViaSessionID: 3, State: TunnelDeploying, DeployParams: params,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _, _ = d.Exec(`DELETE FROM tunnels WHERE task_id=999001`) }()

	rec, err := s.Get(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("Get: %v rec=%v", err, rec)
	}
	if rec.State != TunnelDeploying || rec.ListenPort != 20001 || rec.Adapter != "chisel" {
		t.Errorf("行の内容が一致しません: %+v", rec)
	}
	var p map[string]any
	if err := json.Unmarshal(rec.DeployParams, &p); err != nil || p["auth_pass"] != "deadbeef" {
		t.Errorf("deploy_params の往復に失敗しました: %v %v", err, p)
	}

	// 状態遷移と last_check の更新
	if err := s.UpdateState(ctx, id, TunnelAlive, ""); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	if err := s.UpdateState(ctx, id, "bogus", ""); err == nil {
		t.Error("不正な状態はエラーになるべきです")
	}
	alive, err := s.ListAlive(ctx)
	if err != nil {
		t.Fatalf("ListAlive: %v", err)
	}
	found := false
	for _, r := range alive {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("ListAlive に新しいトンネルが含まれていません")
	}

	// ListByTask
	byTask, err := s.ListByTask(ctx, 999001)
	if err != nil || len(byTask) != 1 {
		t.Errorf("ListByTask = %d 件, %v", len(byTask), err)
	}

	// UpdateDeployParams で remote_pid を書き戻す
	params2, _ := json.Marshal(map[string]any{"remote_pid": 4321})
	if err := s.UpdateDeployParams(ctx, id, 4321, params2); err != nil {
		t.Fatalf("UpdateDeployParams: %v", err)
	}
	rec, _ = s.Get(ctx, id)
	if rec.RemotePID != 4321 {
		t.Errorf("RemotePID = %d, want 4321", rec.RemotePID)
	}

	// 不正な kind
	if _, err := s.Create(ctx, &TunnelRecord{TaskID: 999001, Kind: "gre"}); err == nil {
		t.Error("不正な kind はエラーになるべきです")
	}

	// Delete
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rec, _ = s.Get(ctx, id)
	if rec != nil {
		t.Error("Delete 後の Get は nil になるべきです")
	}
}
