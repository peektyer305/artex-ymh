// tunnels.go は多段プロキシトンネルの台帳の永続化層（schema.sql §L、イントラネット侵入フェーズ 3a、
// INTRANET-PIVOT-DESIGN.md §4.3/4.3a）。deploy_params は完全なデプロイパラメータの JSON
// （auth、server_pid、stage_token、リモートパス、検証ターゲットを含む）——切断時の自動再デプロイはこれに依存する
// （PivotHub の教訓:パラメータを列に分散して保存し、セッション参照を保存しなければ、再デプロイは有名無実;ここでは必ず全部保存する）。
// deploy_params にはトンネルの auth token が含まれ、プラットフォームの信頼ドメイン内データなので平文 JSONB で保存する
// （sessions/credentials のターゲット側の認証情報とは異なる:あれはユーザーの資産、これはプラットフォームが生成する使い捨てトークン）。
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// トンネルの状態遷移：deploying → alive → (error → 再デプロイ → alive)×N → stopped。
const (
	TunnelDeploying = "deploying"
	TunnelAlive     = "alive"
	TunnelError     = "error"
	TunnelStopped   = "stopped"
)

// TunnelRecord は tunnels 表の 1 行。DeployParams は完全なデプロイパラメータの JSON
// （tunnel.Plan のシリアライズ）。
type TunnelRecord struct {
	ID           int64           `json:"id"`
	TaskID       int64           `json:"task_id"`
	Kind         string          `json:"kind"` // socks | portfwd
	Adapter      string          `json:"adapter"`
	ListenHost   string          `json:"listen_host"`
	ListenPort   int             `json:"listen_port"`
	TargetHost   string          `json:"target_host,omitempty"`
	TargetPort   int             `json:"target_port,omitempty"`
	ViaSessionID int64           `json:"via_session_id"`
	RemotePID    int             `json:"remote_pid,omitempty"`
	DeployParams json.RawMessage `json:"deploy_params"`
	State        string          `json:"state"`
	LastCheck    time.Time       `json:"last_check"`
	Error        string          `json:"error,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// TunnelStore は tunnels 表のアクセス層。
type TunnelStore struct {
	d *DB
}

// NewTunnelStore はアクセス層を生成する。
func NewTunnelStore(d *DB) *TunnelStore {
	return &TunnelStore{d: d}
}

// Create はトンネルを 1 件挿入して主キーを返す（初期 state は通常 deploying）。
func (s *TunnelStore) Create(ctx context.Context, rec *TunnelRecord) (int64, error) {
	if rec.Kind != "socks" && rec.Kind != "portfwd" {
		return 0, fmt.Errorf("不正なトンネル種別 %q（socks|portfwd）", rec.Kind)
	}
	adapter := rec.Adapter
	if adapter == "" {
		adapter = "chisel"
	}
	state := rec.State
	if state == "" {
		state = TunnelDeploying
	}
	params := rec.DeployParams
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	var id int64
	err := s.d.QueryRowContext(ctx, `
INSERT INTO tunnels(task_id, kind, adapter, listen_host, listen_port,
	target_host, target_port, via_session_id, remote_pid, deploy_params, state, error)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
RETURNING id`,
		rec.TaskID, rec.Kind, adapter, rec.ListenHost, rec.ListenPort,
		rec.TargetHost, rec.TargetPort, rec.ViaSessionID, rec.RemotePID,
		string(params), state, rec.Error).Scan(&id)
	return id, err
}

const tunnelCols = `id, task_id, kind, adapter, listen_host, listen_port,
	target_host, target_port, via_session_id, remote_pid, deploy_params,
	state, last_check, error, created_at`

func scanTunnel(row interface{ Scan(...any) error }) (*TunnelRecord, error) {
	var r TunnelRecord
	if err := row.Scan(&r.ID, &r.TaskID, &r.Kind, &r.Adapter, &r.ListenHost, &r.ListenPort,
		&r.TargetHost, &r.TargetPort, &r.ViaSessionID, &r.RemotePID, &r.DeployParams,
		&r.State, &r.LastCheck, &r.Error, &r.CreatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// Get は 1 件取得する;存在しない場合は (nil, nil) を返す。
func (s *TunnelStore) Get(ctx context.Context, id int64) (*TunnelRecord, error) {
	r, err := scanTunnel(s.d.QueryRowContext(ctx,
		`SELECT `+tunnelCols+` FROM tunnels WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// UpdateState はステータスとエラーメッセージを更新し、last_check を更新する。
func (s *TunnelStore) UpdateState(ctx context.Context, id int64, state, errMsg string) error {
	switch state {
	case TunnelDeploying, TunnelAlive, TunnelError, TunnelStopped:
	default:
		return fmt.Errorf("不正なトンネル状態 %q", state)
	}
	_, err := s.d.ExecContext(ctx,
		`UPDATE tunnels SET state=$2, error=$3, last_check=now() WHERE id=$1`, id, state, errMsg)
	return err
}

// UpdateDeployParams はデプロイパラメータを全量書き戻す（デプロイ進行時に pid/stage_token などを段階的に補完）、
// remote_pid 列も同時に書き戻せる。
func (s *TunnelStore) UpdateDeployParams(ctx context.Context, id int64, remotePID int, params json.RawMessage) error {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	_, err := s.d.ExecContext(ctx,
		`UPDATE tunnels SET deploy_params=$2, remote_pid=$3, last_check=now() WHERE id=$1`,
		id, string(params), remotePID)
	return err
}

// ListByTask はあるタスクの全トンネルを取得する（id 昇順）。
func (s *TunnelStore) ListByTask(ctx context.Context, taskID int64) ([]*TunnelRecord, error) {
	return s.list(ctx, ` WHERE task_id=$1`, taskID)
}

// ListAlive は生存中（alive）の全トンネルを取得する——ヘルスチェックとポート監査用。
func (s *TunnelStore) ListAlive(ctx context.Context) ([]*TunnelRecord, error) {
	return s.list(ctx, ` WHERE state='alive'`)
}

// ListActive は未終結（deploying|alive|error）の全トンネルを取得する。
func (s *TunnelStore) ListActive(ctx context.Context) ([]*TunnelRecord, error) {
	return s.list(ctx, ` WHERE state IN ('deploying','alive','error')`)
}

// List は台帳の全行を取得する（stopped を含む）。id 昇順——イントラネット運用ページの台帳ビュー用。
func (s *TunnelStore) List(ctx context.Context) ([]*TunnelRecord, error) {
	return s.list(ctx, ``)
}

func (s *TunnelStore) list(ctx context.Context, where string, args ...any) ([]*TunnelRecord, error) {
	rows, err := s.d.QueryContext(ctx, `SELECT `+tunnelCols+` FROM tunnels`+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TunnelRecord{}
	for rows.Next() {
		r, err := scanTunnel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete は台帳の行を削除する（デプロイ失敗時の段階的ロールバックの最後の一手）。
func (s *TunnelStore) Delete(ctx context.Context, id int64) error {
	_, err := s.d.ExecContext(ctx, `DELETE FROM tunnels WHERE id=$1`, id)
	return err
}
