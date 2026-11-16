// intranet_probe_test.go は手動ヘルスチェック API(POST /api/sessions/{id}/probe)
// の生存/死亡パス(httptest で生きた webshell と 404 を模擬)と、起動時チェックのしきい値判定の純粋関数を対象にする。
// handler のテストはセッション台帳のある Postgres(db.DSN)を必要とし、DB がない場合はスキップする
// (assembly_test.go と同じ skip セマンティクス)。
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
)

// probeSentinelRe は php eval webshell の生存プローブ用ペイロードに一致する：echo "START".(20+22)."END";
// (session.newSentinel:start は 8 桁 hex、end は 12 桁 hex)。
var probeSentinelRe = regexp.MustCompile(`"([0-9a-f]{8})"\.\(20\+22\)\."([0-9a-f]{12})"`)

// liveShellMock は生存している php eval のワンライナー webshell を模擬:プローブのセンチネルを解析して START42END を返す。
func liveShellMock(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m := probeSentinelRe.FindStringSubmatch(r.Form.Get("cmd"))
		if m == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(m[1] + "42" + m[2]))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadShellMock は webshell が削除された後や演習場が停止した後でも HTTP 404 を返す対象を模擬する。
func deadShellMock(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 Not Found"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newProbeTestServer は最小の Server(セッションサブシステムのみ)+ 実際の SessionStore を構築する。
func newProbeTestServer(t *testing.T) (*Server, *db.SessionStore) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { pg.Close() })
	store := db.NewSessionStore(pg, []byte("probe-test-master-key"))
	return &Server{sessStore: store, sessReg: session.NewRegistry()}, store
}

// registerProbeSession は http_php セッションを 1 件永続化し、targetURL を指すドライバをレジストリに登録する。
func registerProbeSession(t *testing.T, s *Server, store *db.SessionStore, targetURL string) int64 {
	t.Helper()
	ctx := context.Background()
	sh, err := session.NewHTTPShell(targetURL, "cmd", "php")
	if err != nil {
		t.Fatalf("NewHTTPShell: %v", err)
	}
	secret, _ := json.Marshal(sh.Secret())
	id, err := store.Create(ctx, &db.SessionRecord{
		Kind: "http_php", URL: targetURL, Secret: secret, Lang: "php", Status: db.SessionAlive,
	})
	if err != nil {
		t.Fatalf("セッションの永続化に失敗しました: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
	sh.SetID(id)
	s.sessReg.Add(sh)
	return id
}

func doProbe(t *testing.T, s *Server, id int64) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+strconv.FormatInt(id, 10)+"/probe", nil)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	s.intranetProbeSession(w, req)
	var body map[string]any
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("レスポンス JSON の解析に失敗しました: %v (%q)", err, w.Body.String())
		}
	}
	return w, body
}

// TestProbeMockSpeaksProtocol は DB に依存しない事前チェック：mock の webshell が実際の
// HTTPShell.Test() プローブを通過できること(DB のある環境で handler の生存パスのテストが有効になることを保証する)。
func TestProbeMockSpeaksProtocol(t *testing.T) {
	sh, err := session.NewHTTPShell(liveShellMock(t).URL, "cmd", "php")
	if err != nil {
		t.Fatalf("NewHTTPShell: %v", err)
	}
	if err := sh.Test(context.Background()); err != nil {
		t.Fatalf("生存シェルの mock は Test プローブを通過すべき: %v", err)
	}
	dead, err := session.NewHTTPShell(deadShellMock(t).URL, "cmd", "php")
	if err != nil {
		t.Fatalf("NewHTTPShell: %v", err)
	}
	if err := dead.Test(context.Background()); err == nil {
		t.Fatal("404 の対象では Test がエラーになるべき")
	}
}

// TestIntranetProbeSessionAlive は生存パス:生きた webshell → alive=true、status=alive、last_beat が更新される。
func TestIntranetProbeSessionAlive(t *testing.T) {
	s, store := newProbeTestServer(t)
	id := registerProbeSession(t, s, store, liveShellMock(t).URL)

	w, body := doProbe(t, s, id)
	if w.Code != http.StatusOK {
		t.Fatalf("ステータスコード = %d, body = %q", w.Code, w.Body.String())
	}
	if alive, _ := body["alive"].(bool); !alive {
		t.Fatalf("生存シェルは alive=true になるべき: %v", body)
	}
	if _, hasErr := body["error"]; hasErr {
		t.Fatalf("生存パスに error を含めない: %v", body)
	}
	rec, err := store.Get(context.Background(), id)
	if err != nil || rec == nil {
		t.Fatalf("セッションの読み取りに失敗しました: %v", err)
	}
	if rec.Status != db.SessionAlive {
		t.Fatalf("status は alive になるべき: %s", rec.Status)
	}
	if time.Since(rec.LastBeat) > time.Minute {
		t.Fatalf("last_beat は更新直後であるべき: %v", rec.LastBeat)
	}
}

// TestIntranetProbeSessionDead は死亡パス:対象が 404 → alive=false + error、status=dead。
func TestIntranetProbeSessionDead(t *testing.T) {
	s, store := newProbeTestServer(t)
	id := registerProbeSession(t, s, store, deadShellMock(t).URL)

	w, body := doProbe(t, s, id)
	if w.Code != http.StatusOK {
		t.Fatalf("ステータスコード = %d, body = %q", w.Code, w.Body.String())
	}
	if alive, _ := body["alive"].(bool); alive {
		t.Fatalf("404 の対象は alive=false になるべき: %v", body)
	}
	if e, _ := body["error"].(string); e == "" {
		t.Fatalf("死亡パスは error の理由を含むべき: %v", body)
	}
	rec, err := store.Get(context.Background(), id)
	if err != nil || rec == nil {
		t.Fatalf("セッションの読み取りに失敗しました: %v", err)
	}
	if rec.Status != db.SessionDead {
		t.Fatalf("status は dead になるべき: %s", rec.Status)
	}
}

// TestIntranetProbeSessionNotInRegistry はセッションがレジストリにない → 404(exec と同じセマンティクス)。
func TestIntranetProbeSessionNotInRegistry(t *testing.T) {
	s, _ := newProbeTestServer(t)
	w, _ := doProbe(t, s, 999999)
	if w.Code != http.StatusNotFound {
		t.Fatalf("ステータスコード = %d, body = %q", w.Code, w.Body.String())
	}
}

// TestSessionNeedsStartupProbe は起動時チェックのしきい値判定(純粋関数)。
func TestSessionNeedsStartupProbe(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		rec  *db.SessionRecord
		age  time.Duration
		want bool
	}{
		{"alive かつしきい値超過", &db.SessionRecord{Status: db.SessionAlive, LastBeat: now.Add(-25 * time.Hour)}, 24 * time.Hour, true},
		{"alive だが新しい", &db.SessionRecord{Status: db.SessionAlive, LastBeat: now.Add(-time.Hour)}, 24 * time.Hour, false},
		{"dead はしきい値超過でもプローブしない", &db.SessionRecord{Status: db.SessionDead, LastBeat: now.Add(-72 * time.Hour)}, 24 * time.Hour, false},
		{"ちょうど境界は古いと見なさない", &db.SessionRecord{Status: db.SessionAlive, LastBeat: now.Add(-24 * time.Hour)}, 24 * time.Hour, false},
		{"last_beat がゼロ値なら古いと見なす", &db.SessionRecord{Status: db.SessionAlive}, 24 * time.Hour, true},
		{"nil レコード", nil, 24 * time.Hour, false},
	}
	for _, c := range cases {
		if got := sessionNeedsStartupProbe(c.rec, now, c.age); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
