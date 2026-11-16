// 対スキャン用の偽装ゲート（F14）: ゲートを通過していないリクエストには ARTEX の特徴を一切返さない——静的 SPA、
// /api/*、SSE、favicon、エラーページは一律でバイト単位に固定した偽の nginx デフォルトウェルカムページを返す。
// ゲートは秘匿層であり、Handler() の最外周（requireAuth の前）に被せる。ゲートを通ればあとは従来どおりで、
// JWT は依然として認証層であり、2 つは互いに代替しない。秘匿 ≠ 認証: 入口 URL が漏れれば無効になり、自動化された
// フィンガープリント照合（fofa/shodan/quake）は防げても、標的型攻撃は防げない。
package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	gatePathFilename = "gate.path" // dataDir 配下、0600、現在の入口パスを記録
	gateKeyFilename  = "gate.key"  // keyDir 配下（jwt.key と同じ場所、ワークスペース外）、0600
	gateCookieName   = "artex_gate"
	defaultGateTTL   = 7 * 24 * time.Hour
	gateRateLimit    = 10 // IP あたり 1 分間のパスワード試行上限
	gateRateWindow   = time.Minute
)

// decoyPage はバイト単位に固定した偽装レスポンスボディで、実際の nginx デフォルトウェルカムページと一致する。偽装レスポンスは
// 固定でなければならない——固定でない偽装はそれ自体が新たなフィンガープリントになる。ゲートを通過していない全リクエスト（誤ったパスワード、
// 誤ったパス、/api/*）はこの 1 つを返し、バイト単位で一致させて列挙を防ぐ。
var decoyPage = []byte(`<!DOCTYPE html>
<html>
<head>
<title>Welcome to nginx!</title>
<style>
html { color-scheme: light dark; }
body { width: 35em; margin: 0 auto;
font-family: Tahoma, Verdana, Arial, sans-serif; }
</style>
</head>
<body>
<h1>Welcome to nginx!</h1>
<p>If you see this page, the nginx web server is successfully installed and
working. Further configuration is required.</p>

<p>For online documentation and support please refer to
<a href="http://nginx.org/">nginx.org</a>.<br/>
Commercial support is available at
<a href="http://nginx.com/">nginx.com</a>.</p>

<p><em>Thank you for using nginx.</em></p>
</body>
</html>
`)

// gateLoginPage はゲートのログインページ: 最小限の素の HTML、外部リソースなし、title に artex を含まない。
// action="" で現在の URL へ送信し、ページのバイト列を入口パスから切り離す。
var gateLoginPage = []byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Access</title>
<style>
body { margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center; background: #f5f5f5; font-family: Tahoma, Verdana, Arial, sans-serif; }
form { background: #fff; padding: 2em; border: 1px solid #ddd; border-radius: 4px; }
input { padding: .5em; width: 16em; border: 1px solid #ccc; border-radius: 3px; }
button { padding: .5em 1em; margin-left: .5em; border: 1px solid #ccc; border-radius: 3px; background: #fff; cursor: pointer; }
</style>
</head>
<body>
<form method="post" action="">
<input type="password" name="password" autocomplete="off" autofocus>
<button type="submit">Enter</button>
</form>
</body>
</html>
`)

// Gate は偽装ゲート。path はランダムな入口（デフォルトは /g-<token>）、token はハンドシェイク用のパスワード、
// key は cookie の HMAC-SHA256 署名鍵。
type Gate struct {
	path    string
	token   string
	key     []byte
	ttl     time.Duration
	limiter *ipRateLimiter
}

// NewGate は環境変数とリッスンアドレスからゲート設定を解決する。(nil, nil) を返すとゲートは無効。
//
//	ARTEX_GATE        off/0/false で無効、on/1/true で強制有効。未設定時はリッスンアドレスから
//	                  自動判定: loopback → 無効、非 loopback → 有効にしてログで通知。
//	ARTEX_GATE_PATH   入口パスを明示指定。未設定時は /g-<16 バイトのランダム token> を生成。
//	ARTEX_GATE_TOKEN  ハンドシェイク用のパスワード。未設定時 = 入口パスのランダム token そのもの
//	                  （「入口を知っていれば入れる」）。
//	ARTEX_GATE_TTL_HOURS  cookie の有効期間（時間）、デフォルト 168（7 日）。
//
// 有効時は入口パスを dataDir/gate.path（0600）へ書き込み、起動ログに一度出力する。
func NewGate(addr, dataDir, keyDir string) (*Gate, error) {
	enabled, explicit := gateEnabled(addr)
	if !enabled {
		if explicit {
			log.Printf("[gate] ARTEX_GATE=off — 偽装ゲートは無効です")
		}
		return nil, nil
	}
	if !explicit {
		log.Printf("[gate] リッスンアドレス %s は loopback ではないため、偽装ゲートをデフォルトで有効にします（ARTEX_GATE=off で無効化できます）", addr)
	}

	path := strings.TrimSpace(os.Getenv("ARTEX_GATE_PATH"))
	if path == "" {
		// 入口パスを永続化する。gate.path に保存済みのものがあれば優先して再利用し（再起動してもアドレスが変わらず、
		// ブックマークも無効にならない）、初回かファイル欠落時のみ新しいランダムパスを生成する（UX の負債を解消——以前は再起動のたびに再ランダム化していた）。
		if prev, err := os.ReadFile(filepath.Join(dataDir, gatePathFilename)); err == nil {
			if p := strings.TrimSpace(string(prev)); strings.HasPrefix(p, "/g-") {
				path = p
			}
		}
	}
	if path == "" {
		tok, err := randomGateToken(16) // 16 バイト → 22 文字の URL-safe 文字列
		if err != nil {
			return nil, fmt.Errorf("generate gate path: %w", err)
		}
		path = "/g-" + tok
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	token := strings.TrimSpace(os.Getenv("ARTEX_GATE_TOKEN"))
	if token == "" {
		token = strings.TrimPrefix(strings.TrimPrefix(path, "/"), "g-")
	}

	ttl := defaultGateTTL
	if v := strings.TrimSpace(os.Getenv("ARTEX_GATE_TTL_HOURS")); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			ttl = time.Duration(h) * time.Hour
		}
	}

	key, err := loadOrCreateGateKey(keyDir)
	if err != nil {
		return nil, err
	}
	g := newGate(path, token, key, ttl)

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	pf := filepath.Join(dataDir, gatePathFilename)
	if err := os.WriteFile(pf, []byte(path+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write gate path file: %w", err)
	}
	log.Printf("[gate] 偽装ゲートを有効にしました。入口パス: %s（%s へ書き込み、cookie 有効期間 %s）", path, pf, ttl)
	return g, nil
}

// newGate は Gate を直接構築する（設定の解決は NewGate を参照）。テストでも使う。
func newGate(path, token string, key []byte, ttl time.Duration) *Gate {
	return &Gate{
		path:    path,
		token:   token,
		key:     key,
		ttl:     ttl,
		limiter: &ipRateLimiter{hits: map[string][]time.Time{}, limit: gateRateLimit, window: gateRateWindow},
	}
}

// gateEnabled は ARTEX_GATE を解決する。未設定時はリッスンアドレスから自動判定する。explicit はスイッチが
// 明示的な設定によるもので、自動判定ではないことを示す。
func gateEnabled(addr string) (enabled, explicit bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_GATE"))) {
	case "off", "0", "false":
		return false, true
	case "on", "1", "true":
		return true, true
	}
	return !isLoopbackAddr(addr), false
}

// isLoopbackAddr はリッスンアドレスがループバックのみにバインドされているか判定する。空の host（:8787）= 全 NIC で、ループバックとはみなさない。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// randomGateToken は crypto/rand の n バイト乱数を URL-safe base64（パディングなし）にする。
func randomGateToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// loadOrCreateGateKey は keyDir/gate.key を読む（jwt.key と同じ場所で、閲覧可能な
// dataDir ワークスペースには置かない）。初回実行時は 32 文字のランダム鍵を生成し 0600 で永続化する。
func loadOrCreateGateKey(keyDir string) ([]byte, error) {
	path := filepath.Join(keyDir, gateKeyFilename)
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return []byte(strings.TrimSpace(string(data))), nil
	}
	buf := make([]byte, 32)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyChars))))
		if err != nil {
			return nil, fmt.Errorf("generate gate key: %w", err)
		}
		buf[i] = keyChars[n.Int64()]
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return nil, fmt.Errorf("write gate key: %w", err)
	}
	log.Printf("[gate] 新しい gate key を %s へ書き込みました", path)
	return buf, nil
}

// wrap はゲートを next の最外周に被せる。Gate が nil（ゲート無効）ならそのまま通す。
func (g *Gate) wrap(next http.Handler) http.Handler {
	if g == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /s/ は管理下ステージングのダウンロード面（F13）: 対象マシンが curl/wget でファイルを取得し、ゲートの
		// cookie も JWT も持たない——安全性は 24 バイトのランダム token パス + token ごとのレート制限 +
		// 一貫した 404（stage.Manager.Download）で担保する。ゲートの【前】に除外する必要がある。
		if strings.HasPrefix(r.URL.Path, "/s/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == g.path {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write(gateLoginPage)
			case http.MethodPost:
				g.handleLogin(w, r)
			default:
				writeDecoy(w)
			}
			return
		}
		if c, err := r.Cookie(gateCookieName); err == nil && g.validCookie(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		writeDecoy(w)
	})
}

// handleLogin はパスワードを検証して署名付き cookie を発行する。パスワード誤り、レート制限に該当、
// 誤ったパスのときのレスポンスはバイト単位で一致し（いずれも偽装ページ）、列挙の手がかりを一切与えない。
func (g *Gate) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || ip == "" {
		ip = r.RemoteAddr
	}
	if !g.limiter.allow(ip) {
		writeDecoy(w)
		return
	}
	if err := r.ParseForm(); err != nil ||
		subtle.ConstantTimeCompare([]byte(r.PostForm.Get("password")), []byte(g.token)) != 1 {
		writeDecoy(w)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     gateCookieName,
		Value:    g.signCookie(),
		Path:     "/",
		MaxAge:   int(g.ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// isHTTPS は現在のリクエストが TLS 経由で届いたか判定する（前置リバースプロキシで TLS を終端する場合を含む）。
// Secure 属性は https のときだけ設定する。
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// signCookie は "expiry.hmac" 形式の署名付き cookie 値を発行する。
func (g *Gate) signCookie() string {
	exp := time.Now().Add(g.ttl).Unix()
	mac := hmac.New(sha256.New, g.key)
	fmt.Fprintf(mac, "gate|%d", exp)
	return fmt.Sprintf("%d.%s", exp, hex.EncodeToString(mac.Sum(nil)))
}

// validCookie は署名と有効期間を検証する（定数時間比較）。
func (g *Gate) validCookie(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, g.key)
	fmt.Fprintf(mac, "gate|%d", exp)
	want, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	return hmac.Equal(want, mac.Sum(nil))
}

// writeDecoy は偽装レスポンスを書き込む: 固定の nginx ウェルカムページ + nginx の典型ヘッダ（Go はデフォルトで
// Server ヘッダを出さないので、ここで明示的に補う）。ゲートを通過していない全レスポンスはここを通し、バイト単位の一致を保証する。
func writeDecoy(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Server", "nginx")
	h.Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(decoyPage)
}

// ipRateLimiter はメモリ上の IP ごとの固定ウィンドウカウンタ（パスワード試行のレート制限用）。
type ipRateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

// allow は 1 回の試行を記録し、まだ制限内かどうかを返す。
func (l *ipRateLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	// メモリ肥大の防止: IP 数が異常に多いとき全体を一度クリアする（レート制限の意味は変わらず、極端な攻撃面にのみ影響する）。
	if len(l.hits) > 100000 {
		l.hits = map[string][]time.Time{}
	}
	cutoff := now.Add(-l.window)
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	return true
}
