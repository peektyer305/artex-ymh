package stage

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(context.Background(), filepath.Join(t.TempDir(), "stage"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestPutFileAndDownload(t *testing.T) {
	m := newTestManager(t)
	src := filepath.Join(t.TempDir(), "payload.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := m.PutFile(src, PutOpts{TaskID: "42"})
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if len(e.Token) != 32 {
		t.Fatalf("token length = %d, want 32 (24 bytes base64url)", len(e.Token))
	}
	if e.Name != "payload.sh" || e.Size != 18 {
		t.Fatalf("entry = %+v", e)
	}
	if !strings.HasPrefix(e.URL, "/s/"+e.Token+"/") {
		t.Fatalf("relative URL expected without baseURL, got %q", e.URL)
	}
	want := time.Until(e.ExpiresAt)
	if want < 14*time.Minute || want > 15*time.Minute {
		t.Fatalf("default TTL off: expires in %v", want)
	}
	// 書き込み権限: ディレクトリ 0700、ファイル 0600（Windows には Unix のパーミッションビットの意味がないためスキップ）。
	if runtime.GOOS != "windows" {
		st, err := os.Stat(m.dir)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("dir perm = %v, err=%v", st.Mode().Perm(), err)
		}
		fst, err := os.Stat(filepath.Join(m.dir, e.Token, e.Name))
		if err != nil || fst.Mode().Perm() != 0o600 {
			t.Fatalf("file perm = %v, err=%v", fst.Mode().Perm(), err)
		}
	}

	// ダウンロード: 内容一致、attachment、カウントの記録。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", e.URL, nil)
	req.RemoteAddr = "10.1.2.3:5555"
	m.Download(rec, req, e.Token, e.Name)
	if rec.Code != 200 {
		t.Fatalf("download status = %d", rec.Code)
	}
	if rec.Body.String() != "#!/bin/sh\necho hi\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	got := m.List()
	if len(got) != 1 || got[0].Hits != 1 || got[0].Bytes != 18 {
		t.Fatalf("accounting = %+v", got)
	}
}

func TestDownloadConsistent404(t *testing.T) {
	m := newTestManager(t)
	e, err := m.PutContent("a.txt", []byte("x"), PutOpts{})
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{}
	for name, p := range map[string][2]string{
		"bad token":  {"nosuchtoken0000000000000000000", e.Name},
		"bad name":   {e.Token, "other.txt"},
		"both wrong": {"nosuchtoken0000000000000000000", "other.txt"},
	} {
		rec := httptest.NewRecorder()
		m.Download(rec, httptest.NewRequest("GET", "/s/x/y", nil), p[0], p[1])
		if rec.Code != 404 {
			t.Fatalf("%s: status = %d", name, rec.Code)
		}
		bodies[name] = rec.Body.String()
	}
	if bodies["bad token"] != bodies["bad name"] || bodies["bad name"] != bodies["both wrong"] {
		t.Fatalf("404 応答が一致していません。列挙に手掛かりを与えています: %v", bodies)
	}
}

func TestOneShotBurnAfterFullDownload(t *testing.T) {
	m := newTestManager(t)
	e, err := m.PutContent("once.bin", []byte("0123456789"), PutOpts{OneShot: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	m.Download(rec, httptest.NewRequest("GET", e.URL, nil), e.Token, e.Name)
	if rec.Code != 200 || rec.Body.Len() != 10 {
		t.Fatalf("first download: %d %d", rec.Code, rec.Body.Len())
	}
	if len(m.List()) != 0 {
		t.Fatal("one-shot entry should be burned after first complete download")
	}
	if _, err := os.Stat(filepath.Join(m.dir, e.Token)); !os.IsNotExist(err) {
		t.Fatalf("staged file should be removed, stat err = %v", err)
	}
	// 消去後、同じ URL は 404。
	rec2 := httptest.NewRecorder()
	m.Download(rec2, httptest.NewRequest("GET", e.URL, nil), e.Token, e.Name)
	if rec2.Code != 404 {
		t.Fatalf("after burn: status = %d", rec2.Code)
	}
}

func TestTTLClampAndExpirySweep(t *testing.T) {
	m := newTestManager(t)
	hi, err := m.PutContent("hi.txt", []byte("x"), PutOpts{TTL: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(hi.ExpiresAt); d > MaxTTL || d < MaxTTL-time.Minute {
		t.Fatalf("TTL should clamp to MaxTTL, got %v", d)
	}
	lo, err := m.PutContent("lo.txt", []byte("x"), PutOpts{TTL: -time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(lo.ExpiresAt); d > DefaultTTL || d < DefaultTTL-time.Minute {
		t.Fatalf("TTL<=0 should default to DefaultTTL, got %v", d)
	}
	// 期限切れのレコードを直接 1 件入れ、sweep が破棄して記録するはず。
	exp, err := m.PutContent("exp.txt", []byte("x"), PutOpts{TTL: minTTL})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.entries[exp.Token].ExpiresAt = time.Now().Add(-time.Second)
	m.mu.Unlock()
	var events []Event
	m.OnEvent = func(ev Event) { events = append(events, ev) }
	m.sweep(time.Now())
	if len(m.List()) != 2 { // hi + lo が生存、exp は回収済み
		t.Fatalf("after sweep: %d entries left", len(m.List()))
	}
	if len(events) != 1 || events[0].Kind != EventExpire || events[0].Entry.Token != exp.Token {
		t.Fatalf("expire events = %+v", events)
	}
	if _, err := os.Stat(filepath.Join(m.dir, exp.Token)); !os.IsNotExist(err) {
		t.Fatalf("expired file should be removed, stat err = %v", err)
	}
}

func TestDownloadRateLimitPerToken(t *testing.T) {
	m := newTestManager(t)
	e, err := m.PutContent("r.txt", []byte("x"), PutOpts{})
	if err != nil {
		t.Fatal(err)
	}
	req := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		m.Download(rec, httptest.NewRequest("GET", e.URL, nil), e.Token, e.Name)
		return rec
	}
	for i := 0; i < tokenRateLimit; i++ {
		if rec := req(); rec.Code != 200 {
			t.Fatalf("attempt %d: status = %d", i+1, rec.Code)
		}
	}
	if rec := req(); rec.Code != 404 { // レート制限の応答は「存在しない」と一致する
		t.Fatalf("rate-limited attempt should get the uniform 404, got %d", rec.Code)
	}
}

func TestRestartSweepClearsOrphans(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stage")
	m1, err := New(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := m1.PutContent("orphan.bin", []byte("x"), PutOpts{TTL: MaxTTL})
	if err != nil {
		t.Fatal(err)
	}
	m1.Close()
	// 台帳はプロセス内のもの: マネージャを開き直すと前プロセスの残留ファイルが消えているはず。
	m2, err := New(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if _, err := os.Stat(filepath.Join(dir, e.Token)); !os.IsNotExist(err) {
		t.Fatalf("orphan from previous process should be swept, stat err = %v", err)
	}
}

func TestPutEventAndDelete(t *testing.T) {
	m := newTestManager(t)
	var events []Event
	m.OnEvent = func(ev Event) { events = append(events, ev) }
	e, err := m.PutContent("d.txt", []byte("x"), PutOpts{TaskID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != EventPut || events[0].Entry.TaskID != "7" {
		t.Fatalf("put events = %+v", events)
	}
	if !m.Delete(e.Token) {
		t.Fatal("Delete should hit")
	}
	if m.Delete(e.Token) {
		t.Fatal("Delete of missing token should miss")
	}
	if len(events) != 2 || events[1].Kind != EventDestroy {
		t.Fatalf("destroy events = %+v", events)
	}
}

func TestBaseURLComposition(t *testing.T) {
	m := newTestManager(t)
	m.SetBaseURL("http://10.0.0.2:8787/")
	e, err := m.PutContent("f name.sh", []byte("x"), PutOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := "http://10.0.0.2:8787/s/" + e.Token + "/f%20name.sh"
	if e.URL != want {
		t.Fatalf("URL = %q, want %q", e.URL, want)
	}
	// ディスク上のファイルは元のファイル名のまま（未エスケープ）。
	if _, err := os.Stat(filepath.Join(m.dir, e.Token, "f name.sh")); err != nil {
		t.Fatalf("on-disk file should keep raw name: %v", err)
	}
	// ディスクからの読み取りパスは URL エスケープ名ではなく元の name を使う。
	rec := httptest.NewRecorder()
	m.Download(rec, httptest.NewRequest("GET", e.URL, nil), e.Token, "f name.sh")
	if rec.Code != 200 {
		t.Fatalf("download with raw name: %d", rec.Code)
	}
}

func TestInvalidNameRejected(t *testing.T) {
	m := newTestManager(t)
	for _, name := range []string{"", " "} {
		if _, err := m.PutContent(name, []byte("x"), PutOpts{}); err == nil {
			t.Fatalf("name %q should be rejected", name)
		}
	}
	// "../escape" は basename を取ると "escape" になる——許可され（パス脱出なし）、dir 内に収まることを確認する。
	e, err := m.PutContent("../escape", []byte("x"), PutOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "escape" {
		t.Fatalf("name = %q", e.Name)
	}
	if _, err := os.Stat(filepath.Join(m.dir, e.Token, "escape")); err != nil {
		t.Fatalf("file should land inside stage dir: %v", err)
	}
}
