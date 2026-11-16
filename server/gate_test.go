package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestGate は固定パラメータのゲートを構築する（環境変数の解析は経由しない）。
func newTestGate() *Gate {
	return newGate("/g-testtoken1234567890ab", "testtoken1234567890ab",
		[]byte("0123456789abcdef0123456789abcdef"), time.Hour)
}

// innerReached はゲートで保護される内側の handler。レスポンスボディで「許可」と「偽装」を区別する。
func innerReached() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("INNER-REACHED"))
	})
}

// TestGateDecoyWithoutCookie は cookie が無いとき、各パス（SPA ルート、/api/*、health、
// favicon、誤ったパス）がすべてバイト単位で同一の偽装ページを返すことを確認する。
func TestGateDecoyWithoutCookie(t *testing.T) {
	h := newTestGate().wrap(innerReached())
	var ref []byte
	for _, p := range []string{"/", "/api/health", "/api/tasks", "/favicon.ico", "/login", "/g-wrongpath"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", p, rec.Code)
		}
		if got := rec.Header().Get("Server"); got != "nginx" {
			t.Fatalf("%s: Server = %q, want nginx", p, got)
		}
		if !bytes.Equal(rec.Body.Bytes(), decoyPage) {
			t.Fatalf("%s: body != decoy page", p)
		}
		if ref == nil {
			ref = rec.Body.Bytes()
		} else if !bytes.Equal(rec.Body.Bytes(), ref) {
			t.Fatalf("%s: body differs across paths", p)
		}
	}
}

// TestGateLoginPage は GET の入口パスでゲート画面を表示する：外部リンクのリソースが無く、
// title に artex を含まない。
func TestGateLoginPage(t *testing.T) {
	g := newTestGate()
	rec := httptest.NewRecorder()
	g.wrap(innerReached()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, g.path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>Access</title>") {
		t.Fatalf("login page title leaked or missing: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "artex") {
		t.Fatalf("login page contains artex fingerprint")
	}
}

// TestGateHandshakeAndCookiePassthrough は正しいパスワードでハンドシェイク → 署名済み cookie → 許可。
func TestGateHandshakeAndCookiePassthrough(t *testing.T) {
	g := newTestGate()
	h := g.wrap(innerReached())

	form := url.Values{"password": {"testtoken1234567890ab"}}
	req := httptest.NewRequest(http.MethodPost, g.path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == gateCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no gate cookie issued")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie attrs: HttpOnly=%v SameSite=%v", cookie.HttpOnly, cookie.SameSite)
	}
	if cookie.Secure {
		t.Fatal("Secure must not be set on plain http")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Body.String() != "INNER-REACHED" {
		t.Fatalf("valid cookie not passed through: %q", rec2.Body.String())
	}
}

// TestGateWrongPasswordIdenticalToWrongPath は誤ったパスワードと誤ったパスのレスポンスが
// バイト単位で一致することを確認する。
func TestGateWrongPasswordIdenticalToWrongPath(t *testing.T) {
	g := newTestGate()
	h := g.wrap(innerReached())

	form := url.Values{"password": {"wrong-password"}}
	req := httptest.NewRequest(http.MethodPost, g.path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recBad := httptest.NewRecorder()
	h.ServeHTTP(recBad, req)

	recWrongPath := httptest.NewRecorder()
	h.ServeHTTP(recWrongPath, httptest.NewRequest(http.MethodGet, "/no-such-path", nil))

	if recBad.Code != recWrongPath.Code {
		t.Fatalf("status mismatch: %d vs %d", recBad.Code, recWrongPath.Code)
	}
	if !bytes.Equal(recBad.Body.Bytes(), recWrongPath.Body.Bytes()) {
		t.Fatal("wrong-password body != wrong-path body")
	}
	if !bytes.Equal(recBad.Body.Bytes(), decoyPage) {
		t.Fatal("wrong-password body != decoy page")
	}
	if recBad.Header().Get("Server") != recWrongPath.Header().Get("Server") {
		t.Fatal("Server header mismatch")
	}
	if len(recBad.Result().Cookies()) != 0 {
		t.Fatal("wrong password must not issue a cookie")
	}
}

// TestGateTamperedCookie は署名を改ざんした cookie が拒否されること（偽装ページに戻る）を確認する。
func TestGateTamperedCookie(t *testing.T) {
	g := newTestGate()
	h := g.wrap(innerReached())

	good := g.signCookie()
	// 署名部分の最後の 16 進文字を反転する。
	last := good[len(good)-1]
	flip := byte('0')
	if last == '0' {
		flip = '1'
	}
	tampered := good[:len(good)-1] + string(flip)

	for name, v := range map[string]string{
		"tampered-sig": tampered,
		"garbage":      "not-a-cookie",
		"forged-exp":   "9999999999.deadbeef",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: gateCookieName, Value: v})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !bytes.Equal(rec.Body.Bytes(), decoyPage) {
			t.Fatalf("%s: tampered cookie passed through", name)
		}
	}
}

// TestGateExpiredCookie は期限切れの cookie が拒否されること（ttl<0 で期限切れの値を直接署名する）を確認する。
func TestGateExpiredCookie(t *testing.T) {
	g := newTestGate()
	g.ttl = -time.Hour
	expired := g.signCookie()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: gateCookieName, Value: expired})
	rec := httptest.NewRecorder()
	g.wrap(innerReached()).ServeHTTP(rec, req)
	if !bytes.Equal(rec.Body.Bytes(), decoyPage) {
		t.Fatal("expired cookie passed through")
	}
}

// TestGateRateLimit はパスワード試行が IP あたり毎分の上限を超えると、正しいパスワードでも
// 偽装ページが返ることを確認する。
func TestGateRateLimit(t *testing.T) {
	g := newTestGate()
	h := g.wrap(innerReached())
	post := func(password string) *httptest.ResponseRecorder {
		form := url.Values{"password": {password}}
		req := httptest.NewRequest(http.MethodPost, g.path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < gateRateLimit; i++ {
		post("wrong")
	}
	if rec := post("testtoken1234567890ab"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), decoyPage) {
		t.Fatalf("rate limiter did not kick in: code=%d", rec.Code)
	}
}

// TestGateOffPassthrough はゲートが無効（nil Gate）のとき、すべて素通りすることを確認する。
func TestGateOffPassthrough(t *testing.T) {
	var g *Gate
	h := g.wrap(innerReached())
	for _, p := range []string{"/", "/api/health", "/anything"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Body.String() != "INNER-REACHED" {
			t.Fatalf("%s: gate=off did not pass through", p)
		}
	}
}

// TestNewGateResolution は環境変数と待ち受けアドレスによるスイッチ解決 + 入口ファイルの保存。
func TestNewGateResolution(t *testing.T) {
	dataDir := t.TempDir()
	keyDir := t.TempDir()

	// 明示的な無効化は、非 loopback での既定有効より優先される。
	t.Setenv("ARTEX_GATE", "off")
	g, err := NewGate("0.0.0.0:8787", dataDir, keyDir)
	if err != nil || g != nil {
		t.Fatalf("ARTEX_GATE=off: gate=%v err=%v", g, err)
	}

	// loopback では既定で無効。
	t.Setenv("ARTEX_GATE", "")
	g, err = NewGate("127.0.0.1:8787", dataDir, keyDir)
	if err != nil || g != nil {
		t.Fatalf("loopback default: gate=%v err=%v", g, err)
	}

	// 非 loopback では既定で有効。ランダムな /g- 入口を生成し、gate.path と gate.key を保存する。
	g, err = NewGate("0.0.0.0:8787", dataDir, keyDir)
	if err != nil || g == nil {
		t.Fatalf("non-loopback default: gate=%v err=%v", g, err)
	}
	if !strings.HasPrefix(g.path, "/g-") || len(g.path) < 3+16 {
		t.Fatalf("unexpected gate path %q", g.path)
	}
	if g.token != strings.TrimPrefix(g.path, "/g-") {
		t.Fatalf("default token should equal the path token: %q vs %q", g.token, g.path)
	}
	pf, err := os.ReadFile(filepath.Join(dataDir, gatePathFilename))
	if err != nil || strings.TrimSpace(string(pf)) != g.path {
		t.Fatalf("gate.path file: %q err=%v", pf, err)
	}
	kf, err := os.ReadFile(filepath.Join(keyDir, gateKeyFilename))
	if err != nil || len(strings.TrimSpace(string(kf))) < 32 {
		t.Fatalf("gate.key file: len=%d err=%v", len(kf), err)
	}

	// 明示的な入口パスとパスワード。
	t.Setenv("ARTEX_GATE_PATH", "/custom-entrance")
	t.Setenv("ARTEX_GATE_TOKEN", "s3cret")
	g, err = NewGate("127.0.0.1:8787", dataDir, keyDir) // ARTEX_GATE 未設定 → loopback では既定で無効？明示的な PATH はスイッチとはみなさない
	if err != nil {
		t.Fatal(err)
	}
	if g != nil {
		t.Fatal("explicit path alone must not force-enable the gate on loopback")
	}
	t.Setenv("ARTEX_GATE", "on")
	g, err = NewGate("127.0.0.1:8787", dataDir, keyDir)
	if err != nil || g == nil {
		t.Fatalf("ARTEX_GATE=on: gate=%v err=%v", g, err)
	}
	if g.path != "/custom-entrance" || g.token != "s3cret" {
		t.Fatalf("explicit path/token not honored: %q %q", g.path, g.token)
	}
	// 鍵の再利用：2 回の NewGate が同じ keyDir を使うため、cookie は相互に検証できる。
	g2, err := NewGate("127.0.0.1:8787", dataDir, keyDir)
	if err != nil || g2 == nil {
		t.Fatalf("second NewGate: %v", err)
	}
	if !g2.validCookie(g.signCookie()) {
		t.Fatal("gate key not reused across NewGate calls")
	}
}
