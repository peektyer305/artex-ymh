package db

import (
	"database/sql"
	"time"
)

// PendingScope は【未認証セグメント】の登録（フェーズ 4）:イントラネットのパッシブ偵察が
// 現在の task_scope 外の新セグメントを検出したとき /24 単位で集約登録する（server.reconRegisterPendingScope 参照）。
// 手動承認（approved）後にのみ task_scope へ書き込んで範囲を拡張し、拒否（dismissed）後は
// 同一エントリの hits を加算せず、同一セグメントの反復偵察による重複通知を防ぐ。
type PendingScope struct {
	ID        int64      `json:"id"`
	TaskID    int64      `json:"task_id"`
	Kind      string     `json:"kind"`   // 現状は cidr のみ（/24 単位で集約）
	Value     string     `json:"value"`  // ネットワークセグメント、例: 10.0.3.0/24
	Status    string     `json:"status"` // pending|approved|dismissed
	Hits      int        `json:"hits"`
	FirstSeen time.Time  `json:"first_seen"`
	LastSeen  time.Time  `json:"last_seen"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

const pendingScopeCols = `id, task_id, kind, value, status, hits, first_seen, last_seen, decided_at`

func scanPendingScope(row interface{ Scan(...any) error }) (PendingScope, error) {
	var p PendingScope
	err := row.Scan(&p.ID, &p.TaskID, &p.Kind, &p.Value, &p.Status, &p.Hits,
		&p.FirstSeen, &p.LastSeen, &p.DecidedAt)
	return p, err
}

// UpsertPendingScope は未認証セグメントを 1 件冪等に登録する:新規挿入なら created=true、
// 既存時は pending のままのときだけ hits+1 して last_seen を更新する（created=false）。
// approved/dismissed 済みのエントリは変更しない（重複通知の防止）。taskID<=0 または
// kind/value が空 → no-op。
func (s *AssetStore) UpsertPendingScope(taskID int64, kind, value string) (bool, error) {
	if taskID <= 0 || kind == "" || value == "" {
		return false, nil
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO pending_scope(task_id, kind, value) VALUES ($1, $2, $3)
ON CONFLICT (task_id, kind, value) DO NOTHING
RETURNING id`, taskID, kind, value).Scan(&id)
	if err == nil {
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	if _, err := s.db.Exec(`
UPDATE pending_scope SET hits = hits + 1, last_seen = now()
WHERE task_id=$1 AND kind=$2 AND value=$3 AND status='pending'`, taskID, kind, value); err != nil {
		return false, err
	}
	return false, nil
}

// ListPendingScope はタスクの未承認登録を一覧する。includeDecided=false なら pending のみ。
func (s *AssetStore) ListPendingScope(taskID int64, includeDecided bool) ([]PendingScope, error) {
	query := `SELECT ` + pendingScopeCols + ` FROM pending_scope WHERE task_id=$1`
	if !includeDecided {
		query += ` AND status='pending'`
	}
	query += ` ORDER BY id`
	rows, err := s.db.Query(query, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingScope{}
	for rows.Next() {
		p, err := scanPendingScope(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPendingScope は 1 件読み出す。存在しない場合は sql.ErrNoRows を返す。
func (s *AssetStore) GetPendingScope(id int64) (PendingScope, error) {
	return scanPendingScope(s.db.QueryRow(
		`SELECT `+pendingScopeCols+` FROM pending_scope WHERE id=$1`, id))
}

// DecidePendingScope は登録 1 件を処理する：approve なら approved、それ以外は dismissed とし、decided_at を書く。
// status='pending' の行だけを遷移させ、重複・競合した決定でも原状は変えずエントリの現状を返す（呼び出し側は
// Status を比較すれば競合を識別できる。server.pendingScopeDecide 参照）。
func (s *AssetStore) DecidePendingScope(id int64, approve bool) (PendingScope, error) {
	status := "dismissed"
	if approve {
		status = "approved"
	}
	if _, err := s.db.Exec(`
UPDATE pending_scope SET status=$2, decided_at=now() WHERE id=$1 AND status='pending'`,
		id, status); err != nil {
		return PendingScope{}, err
	}
	return s.GetPendingScope(id)
}
