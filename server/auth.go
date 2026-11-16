package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	jwtKeyFilename = "jwt.key"
	authPassKey    = "auth.password_hash"
	// authKeyVersionKey は settings 内の鍵バージョン番号:パスワード変更/リセット時に +1 し、JWT と
	// refresh token はいずれも発行時のバージョンを持つ。照合して不一致なら失効扱い(F6 パスワード変更による失効)。
	authKeyVersionKey = "auth.key_version"
	// access token は 2 時間;refresh token は 14 日、ランダム 32 バイト(非 JWT)で、SHA-256
	// ハッシュを settings に保存する(HttpOnly cookie で配信、JS からは読めない)。
	jwtTTL            = 2 * time.Hour
	refreshTTL        = 14 * 24 * time.Hour
	refreshCookieName = "artex_refresh"
	refreshCookiePath = "/api/auth"
	sseTicketTTL      = time.Minute
	keyChars          = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// loadOrCreateJWTKey reads the 32-byte signing key from keyDir/jwt.key. keyDir is
// the project base dir (next to the executable), NOT the browsable workspace root
// (dataDir) — the signing key must never be listable/downloadable via the file
// manager. Legacy installs kept it at dataDir/jwt.key; if present there and not yet
// at the new location, it is migrated (key preserved, so sessions stay valid) and
// the old file removed so it disappears from the workspace. On first run a random
// key is generated and persisted.
func loadOrCreateJWTKey(keyDir, dataDir string) ([]byte, error) {
	path := filepath.Join(keyDir, jwtKeyFilename)
	// one-time migration out of the old in-workspace location.
	if legacy := filepath.Join(dataDir, jwtKeyFilename); legacy != path {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if data, rerr := os.ReadFile(legacy); rerr == nil {
				if werr := os.WriteFile(path, data, 0o600); werr == nil {
					_ = os.Remove(legacy)
					log.Printf("[auth] JWT key を %s から %s へ移行しました（閲覧可能なワークスペースから移動）", legacy, path)
				}
			}
		}
	}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return []byte(strings.TrimSpace(string(data))), nil
	}
	buf := make([]byte, 32)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyChars))))
		if err != nil {
			return nil, fmt.Errorf("generate jwt key: %w", err)
		}
		buf[i] = keyChars[n.Int64()]
	}
	if err := os.WriteFile(path, buf, 0600); err != nil {
		return nil, fmt.Errorf("write jwt key: %w", err)
	}
	log.Printf("[auth] 新しい JWT key を %s に書き込みました", path)
	return buf, nil
}

// authClaims は access token の JWT claims:ver は発行時の auth.key_version で、
// 検証時に現在のバージョンと照合し、不一致なら 401(パスワード変更で発行済みの全認証情報を失効)。
type authClaims struct {
	Ver int64 `json:"ver"`
	jwt.RegisteredClaims
}

// signJWT issues a 2-hour HS256 access token for user ARTEX, stamped with the
// current auth key version.
func signJWT(key []byte, ver int64) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, authClaims{
		Ver: ver,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "ARTEX",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}).SignedString(key)
}

// parseAccessToken validates signature (HMAC alg whitelist), expiry, and
// returns the claims. Version checking is the caller's job (needs the store).
func parseAccessToken(tokenStr string, key []byte) (*authClaims, error) {
	claims := &authClaims{}
	if _, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return key, nil
	}); err != nil {
		return nil, err
	}
	return claims, nil
}

// validAccessToken reports whether tok is a well-formed, non-expired access
// token whose version matches the current auth key version. バージョンの読み取りに失敗したら
// fail-closed(失効とみなす);永続層がない場合(テスト/縮退)は署名+有効期限の検証のみ行う。
func (s *Server) validAccessToken(tok string) bool {
	claims, err := parseAccessToken(tok, s.jwtKey)
	if err != nil {
		return false
	}
	kv := s.authSettings()
	if kv == nil {
		return true
	}
	ver, ok := authKeyVersion(kv)
	if !ok {
		return false
	}
	return claims.Ver == ver
}

// extractToken reads the access token from the Authorization: Bearer header.
// (SSE connections use one-time ?ticket= instead — see sseTicketStore.)
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// requireAuth wraps h with access-token validation.
// /api/auth/* and /api/health are exempt.
// 无 Bearer ヘッダーのときは使い捨ての ?ticket= を受け付ける(SSE 専用、60s 有効、使用後は即破棄);
// 旧来の ?token= 形式は削除済み(token は URL/アクセスログに残すべきではない)。
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/auth/") || p == "/api/health" {
			h.ServeHTTP(w, r)
			return
		}
		if tok := extractToken(r); tok != "" {
			if !s.validAccessToken(tok) {
				writeErr(w, 401, "token が無効か期限切れです")
				return
			}
			h.ServeHTTP(w, r)
			return
		}
		if t := r.URL.Query().Get("ticket"); t != "" && s.tickets.consume(t) {
			h.ServeHTTP(w, r)
			return
		}
		writeErr(w, 401, "未認証")
	})
}

// ---------- refresh token(ランダム 32 バイト、ハッシュを settings に保存) ----------

// authKV は auth 永続層の最小インターフェース：*db.DB がそのまま満たし、テストではメモリ実装を注入する。
type authKV interface {
	GetSetting(key string) (value string, ok bool, err error)
	SetSetting(key, value string) error
	DeleteSetting(key string) error
}

// authSettings returns the auth KV store: the test-injected one if set,
// otherwise the PG handle (nil when PG is unavailable).
func (s *Server) authSettings() authKV {
	if s.authkv != nil {
		return s.authkv
	}
	if s.m != nil && s.m.pg != nil {
		return s.m.pg
	}
	return nil
}

// refreshRecord は settings 内の refresh token ハッシュに対応する内容。
type refreshRecord struct {
	Exp int64 `json:"exp"` // unix の有効期限
	Ver int64 `json:"ver"` // 発行時の auth.key_version
}

// authKeyVersion reads the current key version (default 0 when unset);
// ok=false は保存層のエラーを表す(呼び出し側は fail-closed にすべき)。
func authKeyVersion(kv authKV) (int64, bool) {
	v, ok, err := kv.GetSetting(authKeyVersionKey)
	if err != nil {
		return 0, false
	}
	if !ok {
		return 0, true
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// bumpAuthKeyVersion はバージョン番号を +1 し、新しいバージョンを返す(パスワード変更後に呼び、旧い認証情報をすべて失効させる)。
func bumpAuthKeyVersion(kv authKV) (int64, error) {
	ver, ok := authKeyVersion(kv)
	if !ok {
		return 0, fmt.Errorf("read auth key version")
	}
	ver++
	if err := kv.SetSetting(authKeyVersionKey, strconv.FormatInt(ver, 10)); err != nil {
		return 0, err
	}
	return ver, nil
}

func hashRefreshToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func refreshSettingKey(hash string) string { return "auth.refresh." + hash }

// issueRefreshToken はランダム 32 バイトの refresh token を生成し、ハッシュを settings に保存して原文を返す。
func issueRefreshToken(kv authKV, ver int64) (string, error) {
	tok, err := randomGateToken(32)
	if err != nil {
		return "", err
	}
	rec, _ := json.Marshal(refreshRecord{Exp: time.Now().Add(refreshTTL).Unix(), Ver: ver})
	if err := kv.SetSetting(refreshSettingKey(hashRefreshToken(tok)), string(rec)); err != nil {
		return "", err
	}
	return tok, nil
}

// validateRefreshToken は refresh token を検証する:存在、未失効、バージョン一致。
func validateRefreshToken(kv authKV, tok string, curVer int64) (refreshRecord, bool) {
	v, ok, err := kv.GetSetting(refreshSettingKey(hashRefreshToken(tok)))
	if err != nil || !ok {
		return refreshRecord{}, false
	}
	var rec refreshRecord
	if json.Unmarshal([]byte(v), &rec) != nil {
		return refreshRecord{}, false
	}
	if time.Now().Unix() >= rec.Exp || rec.Ver != curVer {
		return refreshRecord{}, false
	}
	return rec, true
}

// setRefreshCookie は HttpOnly の refresh cookie を配信する：Path を /api/auth に絞り(refresh/logout
// だけが送る)、SameSite=Lax、https では Secure を付ける。
func setRefreshCookie(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    tok,
		Path:     refreshCookiePath,
		MaxAge:   int(refreshTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

func clearRefreshCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

// issueSession はログイン/初期化成功後に共通で呼ぶ：access token を署名し(現在のバージョン付き)+
// refresh cookie を配信する。レスポンス本文は access token のみ。
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, kv authKV) {
	ver, ok := authKeyVersion(kv)
	if !ok {
		writeErr(w, 500, "鍵バージョンの読み取りに失敗しました")
		return
	}
	access, err := signJWT(s.jwtKey, ver)
	if err != nil {
		writeErr(w, 500, "token の生成に失敗しました")
		return
	}
	rt, err := issueRefreshToken(kv, ver)
	if err != nil {
		writeErr(w, 500, "refresh token の生成に失敗しました: "+err.Error())
		return
	}
	setRefreshCookie(w, r, rt)
	writeJSON(w, 200, map[string]any{"token": access})
}

// ---------- SSE 使い捨てチケット(メモリ保存、60s 有効、使用後は即破棄) ----------

type sseTicketStore struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (s *sseTicketStore) issue() (string, error) {
	tok, err := randomGateToken(16)
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]time.Time{}
	}
	// メモリ膨張の防止:チケットが多いときは期限切れをまとめて掃除する。
	if len(s.m) > 10000 {
		for k, exp := range s.m {
			if now.After(exp) {
				delete(s.m, k)
			}
		}
	}
	s.m[tok] = now.Add(sseTicketTTL)
	return tok, nil
}

// consume はチケットを消し込む:存在し未失効のときだけ通し、成否にかかわらずチケットは一度きり。
func (s *sseTicketStore) consume(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	if !ok {
		return false
	}
	delete(s.m, tok)
	return time.Now().Before(exp)
}

// POST /api/sse-ticket — access token が必要(requireAuth はこのパスを除外しない);
// 60s 有効の使い捨て ticket を返し、フロントの EventSource が ?ticket= で連結する。
func (s *Server) sseTicket(w http.ResponseWriter, r *http.Request) {
	t, err := s.tickets.issue()
	if err != nil {
		writeErr(w, 500, "ticket の生成に失敗しました")
		return
	}
	writeJSON(w, 200, map[string]any{"ticket": t})
}

// GET /api/auth/status — reports whether the admin password has been initialised.
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	hash, _, _ := pg.GetSetting(authPassKey)
	writeJSON(w, 200, map[string]any{"initialized": hash != ""})
}

// POST /api/auth/init — sets the password for the first time; rejected if already set.
func (s *Server) authInit(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	existing, _, _ := pg.GetSetting(authPassKey)
	if existing != "" {
		writeErr(w, 403, "パスワードは設定済みです")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil || req.Password == "" {
		writeErr(w, 400, "パスワードを入力してください")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "パスワードの暗号化に失敗しました")
		return
	}
	if err := pg.SetSetting(authPassKey, string(hash)); err != nil {
		writeErr(w, 500, "保存に失敗しました: "+err.Error())
		return
	}
	s.issueSession(w, r, pg)
}

// POST /api/auth/change-password — changes the admin password. Requires a valid
// token (this route is under /api/auth/* which requireAuth exempts, so the token
// is validated here) AND the current password.
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if !s.validAccessToken(extractToken(r)) {
		writeErr(w, 401, "未認証")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "リクエスト形式が不正です")
		return
	}
	if req.NewPassword == "" {
		writeErr(w, 400, "新しいパスワードを入力してください")
		return
	}
	hash, ok, _ := pg.GetSetting(authPassKey)
	if !ok || hash == "" {
		writeErr(w, 403, "パスワードが未初期化です。先にパスワードを設定してください")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)); err != nil {
		writeErr(w, 401, "現在のパスワードが正しくありません")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "パスワードの暗号化に失敗しました")
		return
	}
	if err := pg.SetSetting(authPassKey, string(newHash)); err != nil {
		writeErr(w, 500, "保存に失敗しました: "+err.Error())
		return
	}
	// 鍵バージョンを +1:これ以前に発行したすべての access/refresh token を即時失効させる(F6)。
	// フロントは成功を受け取ったら強制的に再ログインさせる。
	if _, err := bumpAuthKeyVersion(pg); err != nil {
		writeErr(w, 500, "旧い認証情報の失効に失敗しました: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/auth/refresh — HttpOnly の refresh cookie で新しい access token に交換し、
// refresh もスライド更新する(旧チケットを失効させ、新チケットで 14 日をリセット)。バージョン不一致/失効/不明は一律 401。
func (s *Server) authRefresh(w http.ResponseWriter, r *http.Request) {
	kv := s.authSettings()
	if kv == nil {
		writeErr(w, 503, "管理画面のデータソース(PostgreSQL)が未接続です")
		return
	}
	c, err := r.Cookie(refreshCookieName)
	if err != nil || c.Value == "" {
		writeErr(w, 401, "refresh token がありません")
		return
	}
	ver, ok := authKeyVersion(kv)
	if !ok {
		writeErr(w, 500, "鍵バージョンの読み取りに失敗しました")
		return
	}
	if _, ok := validateRefreshToken(kv, c.Value, ver); !ok {
		clearRefreshCookie(w, r)
		writeErr(w, 401, "refresh token が無効か期限切れです")
		return
	}
	// スライド更新:旧チケットを失効させてから新しいものを発行する(ローテーション)。旧チケットが漏れても再送では通らない。
	_ = kv.DeleteSetting(refreshSettingKey(hashRefreshToken(c.Value)))
	rt, err := issueRefreshToken(kv, ver)
	if err != nil {
		writeErr(w, 500, "refresh token の生成に失敗しました: "+err.Error())
		return
	}
	access, err := signJWT(s.jwtKey, ver)
	if err != nil {
		writeErr(w, 500, "token の生成に失敗しました")
		return
	}
	setRefreshCookie(w, r, rt)
	writeJSON(w, 200, map[string]any{"token": access})
}

// POST /api/auth/logout — 現在の refresh token を失効させ、cookie を消す。
// access token は短寿命のステートレスな認証情報なので、フロントが捨てればよい。
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if kv := s.authSettings(); kv != nil {
		if c, err := r.Cookie(refreshCookieName); err == nil && c.Value != "" {
			_ = kv.DeleteSetting(refreshSettingKey(hashRefreshToken(c.Value)))
		}
	}
	clearRefreshCookie(w, r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/auth/login — validates username/password and returns a JWT.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "リクエスト形式が不正です")
		return
	}
	if req.Username != "ARTEX" {
		writeErr(w, 401, "ユーザー名またはパスワードが正しくありません")
		return
	}
	hash, ok, _ := pg.GetSetting(authPassKey)
	if !ok || hash == "" {
		writeErr(w, 403, "パスワードが未初期化です。先にパスワードを設定してください")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		writeErr(w, 401, "ユーザー名またはパスワードが正しくありません")
		return
	}
	s.issueSession(w, r, pg)
}
