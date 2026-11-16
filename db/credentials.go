// credentials.go は認証情報の一級エンティティの永続化層（schema.sql §K、イントラネット侵入フェーズ 2、
// docs/architecture/intranet-pivot.md §4.4）。secret は AES-GCM で暗号化して保存する：sessions と同様の
// 派生方式（jwt.key → SHA-256 ドメイン分離）だがドメインラベルが異なり、両サブシステムの鍵は独立している。
package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"time"
)

// 認証情報タイプの全体（schema の CHECK と Go の検証は同じ定義源）。
const (
	CredPassword = "password"
	CredHashNT   = "hash_nt"
	CredHashLM   = "hash_lm"
	CredHashSHA1 = "hash_sha1"
	CredTicket   = "ticket"
	CredSSHKey   = "ssh_key"
	CredToken    = "token"
)

// CredentialRecord は credentials 表の 1 行。Secret は Go 層では平文;Create が暗号化し、
// Get/ListByTask が復号する。鍵が CredentialStore の外に出ることはない。
type CredentialRecord struct {
	ID          int64     `json:"id"`
	TaskID      int64     `json:"task_id"`
	HostAssetID int64     `json:"host_asset_id,omitempty"`
	Username    string    `json:"username"`
	CredType    string    `json:"cred_type"`
	Secret      string    `json:"secret"` // 平文;保存時に暗号化
	Domain      string    `json:"domain,omitempty"`
	Source      string    `json:"source"`
	Verified    bool      `json:"verified"`
	CreatedAt   time.Time `json:"created_at"`
}

// ValidCredType は認証情報タイプを検証する。
func ValidCredType(t string) bool {
	switch t {
	case CredPassword, CredHashNT, CredHashLM, CredHashSHA1, CredTicket, CredSSHKey, CredToken:
		return true
	}
	return false
}

// CredentialSecretKey は master key（jwt.key の内容）から認証情報の暗号化鍵を派生する:
// SessionSecretKey と同様の SHA-256 ドメイン分離だがドメインラベルが異なり（"artex/credentials/v1"）、
// 両サブシステムの暗号文に互換性はない。
func CredentialSecretKey(master []byte) []byte {
	sum := sha256.Sum256(append([]byte("artex/credentials/v1\x00"), master...))
	return sum[:]
}

// CredentialStore は credentials 表のアクセス層。key は CredentialSecretKey で派生した
// 32 バイトの AES 鍵;key がない場合、暗号化/復号は即座にエラーになる（誠実に失敗し、平文を保存しない）。
type CredentialStore struct {
	d   *DB
	key []byte
}

// NewCredentialStore はアクセス層を生成する。masterKey には jwt.key の内容を渡す;派生は内部で行う。
func NewCredentialStore(d *DB, masterKey []byte) *CredentialStore {
	var key []byte
	if len(masterKey) > 0 {
		key = CredentialSecretKey(masterKey)
	}
	return &CredentialStore{d: d, key: key}
}

func (s *CredentialStore) encrypt(plain string) (string, error) {
	if len(s.key) != 32 {
		return "", fmt.Errorf("認証情報の暗号化鍵が未初期化のため、平文での保存を拒否します")
	}
	gcm, err := newGCM(s.key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (s *CredentialStore) decrypt(stored string) (string, error) {
	if len(stored) == 0 {
		return "", nil
	}
	if len(s.key) != 32 {
		return "", fmt.Errorf("認証情報の暗号化鍵が未初期化のため、復号できません")
	}
	if !hasPrefix(stored, secretPrefix) {
		return "", fmt.Errorf("未知の secret 形式（gcm1: 接頭辞がない）のため、平文としての読み取りを拒否します")
	}
	raw, err := base64.StdEncoding.DecodeString(stored[len(secretPrefix):])
	if err != nil {
		return "", fmt.Errorf("secret の base64 デコードに失敗しました: %w", err)
	}
	gcm, err := newGCM(s.key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("secret の暗号文が短すぎます")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("secret の復号に失敗しました（鍵の不一致またはデータの改ざん）: %w", err)
	}
	return string(plain), nil
}

// Create は認証情報を 1 件挿入して主キーを返す。Secret は暗号化して保存する。
func (s *CredentialStore) Create(ctx context.Context, rec *CredentialRecord) (int64, error) {
	if rec.TaskID <= 0 {
		return 0, fmt.Errorf("task_id は必須です")
	}
	if !ValidCredType(rec.CredType) {
		return 0, fmt.Errorf("不正な cred_type %q（password/hash_nt/hash_lm/hash_sha1/ticket/ssh_key/token）", rec.CredType)
	}
	if rec.Secret == "" {
		return 0, fmt.Errorf("secret は必須です")
	}
	enc, err := s.encrypt(rec.Secret)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.d.QueryRowContext(ctx, `
INSERT INTO credentials(task_id, host_asset_id, username, cred_type, secret, domain, source, verified)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id`,
		rec.TaskID, nilIfZero(rec.HostAssetID), rec.Username, rec.CredType, enc,
		rec.Domain, rec.Source, rec.Verified).Scan(&id)
	return id, err
}

const credentialCols = `id, task_id, COALESCE(host_asset_id,0), username, cred_type, secret,
	domain, source, verified, created_at`

func (s *CredentialStore) scan(row interface{ Scan(...any) error }) (*CredentialRecord, error) {
	var r CredentialRecord
	var enc string
	if err := row.Scan(&r.ID, &r.TaskID, &r.HostAssetID, &r.Username, &r.CredType, &enc,
		&r.Domain, &r.Source, &r.Verified, &r.CreatedAt); err != nil {
		return nil, err
	}
	plain, err := s.decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("credential %d: %w", r.ID, err)
	}
	r.Secret = plain
	return &r, nil
}

// Get は 1 件取得する（secret は復号済み）;存在しない場合は (nil, nil) を返す。
func (s *CredentialStore) Get(ctx context.Context, id int64) (*CredentialRecord, error) {
	r, err := s.scan(s.d.QueryRowContext(ctx,
		`SELECT `+credentialCols+` FROM credentials WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ListByTask は 1 つのタスクの全認証情報を取得する（id 昇順、secret は復号済み）。
func (s *CredentialStore) ListByTask(ctx context.Context, taskID int64) ([]*CredentialRecord, error) {
	rows, err := s.d.QueryContext(ctx,
		`SELECT `+credentialCols+` FROM credentials WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CredentialRecord{}
	for rows.Next() {
		r, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAll は全認証情報を取得する（id 昇順、secret は復号済み）——イントラネット運用ページのタスク横断ビュー用。
// マスク（MaskSecret）は呼び出し側の責務であり、secret が平文で server の外に出ることはない。
func (s *CredentialStore) ListAll(ctx context.Context) ([]*CredentialRecord, error) {
	rows, err := s.d.QueryContext(ctx,
		`SELECT `+credentialCols+` FROM credentials ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CredentialRecord{}
	for rows.Next() {
		r, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetVerified は認証情報が実際にログイン可能と検証済みかを記録する（再利用スコアの verified 加点がこれを参照する）。
func (s *CredentialStore) SetVerified(ctx context.Context, id int64, verified bool) error {
	_, err := s.d.ExecContext(ctx, `UPDATE credentials SET verified=$2 WHERE id=$1`, id, verified)
	return err
}

// Delete は 1 件削除する。
func (s *CredentialStore) Delete(ctx context.Context, id int64) error {
	_, err := s.d.ExecContext(ctx, `DELETE FROM credentials WHERE id=$1`, id)
	return err
}

// MaskSecret はマスク表示用:先頭と末尾の各 2 文字だけ見せ、残りはマスクする;短すぎる場合（≤4 文字）は全マスク。
func MaskSecret(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:2]) + "****" + string(r[len(r)-2:])
}
