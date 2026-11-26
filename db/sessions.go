// sessions.go はフットホールドセッションの永続化層（schema.sql §J）。secret（接続パラメータの JSON）は
// AES-GCM で暗号化して保存する:鍵は jwt.key から派生（SHA-256 ドメイン分離）し、server/auth.go の
// 鍵管理方式に倣う。master key は keyDir/jwt.key にのみ存在し、DB には平文を残さない。
package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// SessionRecord は sessions 表の 1 行。Secret は Go 層では平文 JSON;Create が暗号化し、
// Get/List が復号する。鍵が SessionStore の外に出ることはない。
type SessionRecord struct {
	ID              int64           `json:"id"`
	Kind            string          `json:"kind"` // http_php/http_jsp/http_aspx/ssh/reverse（予約）
	HostAssetID     int64           `json:"host_asset_id,omitempty"`
	URL             string          `json:"url"`
	Secret          json.RawMessage `json:"secret"` // 平文 JSON;保存時に暗号化
	Lang            string          `json:"lang"`
	Status          string          `json:"status"` // alive | dead
	CreatedByTask   int64           `json:"created_by_task,omitempty"`
	CreatedByIntent int64           `json:"created_by_intent,omitempty"`
	LastBeat        time.Time       `json:"last_beat"`
	CreatedAt       time.Time       `json:"created_at"`
}

const (
	SessionAlive = "alive"
	SessionDead  = "dead"
)

// SessionSecretKey は master key（jwt.key の内容）からセッション暗号化鍵を派生する:
// SHA-256 ドメイン分離であり、JWT 署名の用途とは隔離している。
func SessionSecretKey(master []byte) []byte {
	sum := sha256.Sum256(append([]byte("artex/sessions/v1\x00"), master...))
	return sum[:]
}

// SessionStore は sessions 表のアクセス層。key は 32 バイトの AES 鍵
// （SessionSecretKey で派生）。key がない場合、暗号化/復号は即座にエラーになる（誠実に失敗し、平文を保存しない）。
type SessionStore struct {
	d   *DB
	key []byte
}

// NewSessionStore はアクセス層を生成する。masterKey には jwt.key の内容を渡す;派生は内部で行う。
func NewSessionStore(d *DB, masterKey []byte) *SessionStore {
	var key []byte
	if len(masterKey) > 0 {
		key = SessionSecretKey(masterKey)
	}
	return &SessionStore{d: d, key: key}
}

func (s *SessionStore) encrypt(plain []byte) (string, error) {
	return encodeSecret(s.key, plain)
}

func (s *SessionStore) decrypt(stored string) ([]byte, error) {
	return decodeSecret(s.key, stored)
}

// nilIfZero は 0 を NULL に変換する（NULL 許容の外部キー列）。
func nilIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// Create はセッションを 1 件挿入して主キーを返す。Secret は暗号化して保存する。
func (s *SessionStore) Create(ctx context.Context, rec *SessionRecord) (int64, error) {
	if rec.Kind == "" {
		return 0, fmt.Errorf("kind は必須です")
	}
	enc, err := s.encrypt(rec.Secret)
	if err != nil {
		return 0, err
	}
	status := rec.Status
	if status == "" {
		status = SessionAlive
	}
	var id int64
	err = s.d.QueryRowContext(ctx, `
INSERT INTO sessions(kind, host_asset_id, url, secret, lang, status, created_by_task, created_by_intent)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id`,
		rec.Kind, nilIfZero(rec.HostAssetID), rec.URL, enc, rec.Lang, status,
		nilIfZero(rec.CreatedByTask), nilIfZero(rec.CreatedByIntent)).Scan(&id)
	return id, err
}

const sessionCols = `id, kind, COALESCE(host_asset_id,0), url, secret, lang, status,
	COALESCE(created_by_task,0), COALESCE(created_by_intent,0), last_beat, created_at`

func (s *SessionStore) scan(row interface{ Scan(...any) error }) (*SessionRecord, error) {
	var r SessionRecord
	var enc string
	if err := row.Scan(&r.ID, &r.Kind, &r.HostAssetID, &r.URL, &enc, &r.Lang, &r.Status,
		&r.CreatedByTask, &r.CreatedByIntent, &r.LastBeat, &r.CreatedAt); err != nil {
		return nil, err
	}
	plain, err := s.decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("session %d: %w", r.ID, err)
	}
	r.Secret = plain
	return &r, nil
}

// Get は 1 件取得する（secret は復号済み）;存在しない場合は (nil, nil) を返す。
func (s *SessionStore) Get(ctx context.Context, id int64) (*SessionRecord, error) {
	r, err := s.scan(s.d.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// List は全件取得する（id 昇順）;status が空でない場合はステータスで絞り込む。
func (s *SessionStore) List(ctx context.Context, status string) ([]*SessionRecord, error) {
	query := `SELECT ` + sessionCols + ` FROM sessions`
	args := []any{}
	if status != "" {
		query += ` WHERE status=$1`
		args = append(args, status)
	}
	query += ` ORDER BY id`
	rows, err := s.d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SessionRecord{}
	for rows.Next() {
		r, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateStatus は alive/dead のステータスを更新する。
func (s *SessionStore) UpdateStatus(ctx context.Context, id int64, status string) error {
	if status != SessionAlive && status != SessionDead {
		return fmt.Errorf("不正なセッションステータス %q（alive|dead）", status)
	}
	_, err := s.d.ExecContext(ctx, `UPDATE sessions SET status=$2 WHERE id=$1`, id, status)
	return err
}

// Touch は last_beat を更新する（セッションを使うたびに呼ぶ）。
func (s *SessionStore) Touch(ctx context.Context, id int64) {
	_, _ = s.d.ExecContext(ctx, `UPDATE sessions SET last_beat=now() WHERE id=$1`, id)
}

// SetHostAsset は webshell に関連付く endpoint アセット id を書き戻す。
func (s *SessionStore) SetHostAsset(ctx context.Context, id, assetID int64) {
	_, _ = s.d.ExecContext(ctx, `UPDATE sessions SET host_asset_id=$2 WHERE id=$1`, id, assetID)
}

// Delete は 1 件削除する。
func (s *SessionStore) Delete(ctx context.Context, id int64) error {
	_, err := s.d.ExecContext(ctx, `DELETE FROM sessions WHERE id=$1`, id)
	return err
}
