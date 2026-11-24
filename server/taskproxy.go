// taskproxy.go フェーズ 3b: worker のトラフィックをタスク単位で上流にバインドする。タスクごとに独立した traffic.Traffic
// (MITM) インスタンスを遅延生成し、上流 = そのタスクで現在 alive な socks トンネルの入口とする。トンネルがなければグローバル出口プロキシ
// (globalProxy) または直接接続へフォールバックする。経路: worker Bash/WebFetch → タスク MITM（ローカルでは従来どおり記録し、
// 証跡の連鎖は途切れない） → socks5 トンネル → イントラネットの対象（docs/architecture/intranet-pivot.md §4.3「トンネル消費」）。
//
// spike の結論（docs/roadmap/roadmap.md 6.3）の実装:
//   - go-mitmproxy の複数インスタンス共存は可能だが、初回生成の競合を避けるためインスタンスごとに独立した CaRootPath が必要
//     → インスタンスディレクトリ data/traffic-tasks/<taskID>/（_ca/_index/_auth をすべてここに置く）
//   - SetUpstreamProxy はインスタンスのフィールドで、アトミックにホットスイッチできる → トンネルの追加／消失／再起動では上流だけを差し替え、
//     インスタンスは再生成しない（tunnel.Manager.OnStateChange 経由で Refresh をトリガー）
//   - Proxy.Close() には attacker goroutine のリークがある → インスタンスのライフサイクルをタスクに紐づけ
//     （タスク削除／アーカイブ時のみ CloseTask、プロセス終了時は Close で全消去）、インスタンス数はタスク数で有界なので、
//     リークは許容範囲。再起動後にインスタンスは自動復元しない（遅延生成で、トンネル状態はヘルスチェックが担う）。
package server

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/artex/tunnel"
)

// tunnelLister はタスクのトンネル台帳に対する最小の読み取りインターフェース（*db.TunnelStore が満たす。テストでは偽実装を注入できる）。
type tunnelLister interface {
	ListByTask(ctx context.Context, taskID int64) ([]*db.TunnelRecord, error)
}

// taskProxyInst は 1 つのタスクレベルの MITM インスタンスと、その管理ポート／現在の上流。
type taskProxyInst struct {
	tr       *traffic.Traffic
	port     int
	upstream string // 現在の上流の原文（""/globalProxy/socks5://...）。変化検出用
}

// taskProxyManager は taskID ごとにタスクレベルの MITM インスタンスを遅延生成して管理する。ゼロ値は使用不可で、
// newTaskProxyManager で構築する。すべてのメソッドは並行安全。
type taskProxyManager struct {
	dir         string        // インスタンスのルートディレクトリ（data/traffic-tasks）
	store       tunnelLister  // トンネル台帳（上流選択のデータソース）
	globalProxy func() string // トンネルがないときのフォールバック上流（Manager.GlobalProxy、毎回読む）
	portMin     int           // インスタンスのリッスンポートプール（デフォルト 21100-21199、config.TaskProxyPortRange を参照）
	portMax     int
	mu          sync.Mutex
	proxies     map[int64]*taskProxyInst
}

func newTaskProxyManager(dir string, store tunnelLister, globalProxy func() string, portMin, portMax int) *taskProxyManager {
	return &taskProxyManager{
		dir: filepath.Join(dir, "traffic-tasks"), store: store, globalProxy: globalProxy,
		portMin: portMin, portMax: portMax, proxies: map[int64]*taskProxyInst{},
	}
}

// pickUpstream はタスクの上流を選ぶ: 最新（id 最大）の alive な socks トンネル入口。利用可能なトンネルがなければ
// グローバル出口プロキシへフォールバックする（"" なら直接接続）。portfwd は上流の形態ではないのでスキップする。
// suo5 の入口は --auth 認証付きなので、上流 URL には user:pass を埋め込む必要がある（台帳の deploy_params にある）。純粋関数。
func pickUpstream(recs []*db.TunnelRecord, globalProxy string) string {
	bestID := int64(-1)
	socksPort := 0
	var user, pass string
	for _, r := range recs {
		if r.State != db.TunnelAlive || r.Kind != tunnel.KindSocks || r.ID <= bestID {
			continue
		}
		var p tunnel.Plan
		if json.Unmarshal(r.DeployParams, &p) != nil || p.SocksPort <= 0 {
			continue
		}
		bestID, socksPort = r.ID, p.SocksPort
		user, pass = "", ""
		if r.Adapter == tunnel.AdapterSuo5 {
			user, pass = p.AuthUser, p.AuthPass
		}
	}
	if socksPort > 0 {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort))
		if user != "" {
			return "socks5://" + user + ":" + pass + "@" + addr
		}
		return "socks5://" + addr
	}
	return globalProxy
}

// resolveUpstream は台帳を読んでタスクの上流を再計算する（寛容: 台帳の読み取りに失敗したら現状維持し、判断は呼び出し元に任せる）。
func (m *taskProxyManager) resolveUpstream(ctx context.Context, taskID int64) (string, error) {
	recs, err := m.store.ListByTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	gp := ""
	if m.globalProxy != nil {
		gp = m.globalProxy()
	}
	return pickUpstream(recs, gp), nil
}

// ForTask はタスクの worker が使うべきプロキシアドレスと CA を返す。初回呼び出しでそのタスクの MITM
// インスタンスを遅延生成し（独立した CA ディレクトリで初回生成の競合を防ぎ、ポートプールから割り当ててポート監査台帳へ登録）、以降の呼び出しでは
// ついでに上流を再計算する（イベント間隙の保険で、OnStateChange の Refresh と相補的）。インスタンス生成に失敗したら
// ログだけ残して "","" を返す。呼び出し元はグローバルプロキシへフォールバックし、録画サブシステムが worker を止めることは決してない。
func (m *taskProxyManager) ForTask(ctx context.Context, taskID int64) (addr, caCert string) {
	if taskID <= 0 {
		return "", ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.proxies[taskID]
	if inst == nil {
		created, err := m.buildLocked(ctx, taskID)
		if err != nil {
			log.Printf("[taskproxy] タスク %d の MITM インスタンス生成に失敗（グローバルプロキシへフォールバック）: %v", taskID, err)
			return "", ""
		}
		inst = created
		m.proxies[taskID] = inst
		log.Printf("[taskproxy] タスク %d の MITM インスタンス準備完了 %s（CA %s）", taskID, inst.tr.ProxyAddrRedacted(), inst.tr.CACertPath())
	}
	m.refreshLocked(ctx, taskID, inst)
	return inst.tr.ProxyAddr(), inst.tr.CACertPath()
}

// buildLocked はインスタンスを生成する: ポート割り当て → traffic.Open（独立ディレクトリ／CA） → 初期上流 → リッスン開始
// → ポートを監査台帳へ登録。呼び出し元は m.mu を保持している必要がある。
func (m *taskProxyManager) buildLocked(ctx context.Context, taskID int64) (*taskProxyInst, error) {
	used := map[int]bool{}
	for _, inst := range m.proxies {
		used[inst.port] = true
	}
	port, err := tunnel.AllocatePort(m.portMin, m.portMax, used, tunnel.CanListen)
	if err != nil {
		return nil, err
	}
	tr, err := traffic.Open(filepath.Join(m.dir, strconv.FormatInt(taskID, 10)),
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	inst := &taskProxyInst{tr: tr, port: port}
	if up, err := m.resolveUpstream(ctx, taskID); err != nil {
		log.Printf("[taskproxy] タスク %d の台帳読み取りに失敗、初期上流はフォールバックとして扱います: %v", taskID, err)
		gp := ""
		if m.globalProxy != nil {
			gp = m.globalProxy()
		}
		inst.upstream = gp
	} else {
		inst.upstream = up
	}
	if err := tr.SetUpstreamProxy(inst.upstream); err != nil {
		_ = tr.Close()
		return nil, err
	}
	go func() {
		if err := tr.Start(); err != nil {
			log.Printf("[taskproxy] タスク %d の MITM リッスンが終了しました: %v", taskID, err)
		}
	}()
	registerManagedPort(port)
	return inst, nil
}

// refreshLocked は単一インスタンスの上流を再計算してホットスイッチする（変化があったときだけ SetUpstreamProxy）。呼び出し元は m.mu を保持する。
func (m *taskProxyManager) refreshLocked(ctx context.Context, taskID int64, inst *taskProxyInst) {
	up, err := m.resolveUpstream(ctx, taskID)
	if err != nil {
		log.Printf("[taskproxy] タスク %d の台帳読み取りに失敗、上流は %q を維持します: %v", taskID, inst.upstream, err)
		return
	}
	if up == inst.upstream {
		return
	}
	if err := inst.tr.SetUpstreamProxy(up); err != nil {
		log.Printf("[taskproxy] タスク %d の上流 %q は無効、%q を維持します: %v", taskID, up, inst.upstream, err)
		return
	}
	log.Printf("[taskproxy] タスク %d の上流をホットスイッチ: %q → %q", taskID, inst.upstream, up)
	inst.upstream = up
}

// Refresh はトンネル状態の変化（追加／消失／再起動／teardown）時にそのタスクインスタンスの上流をホットスイッチする。
// インスタンスが存在しないときは生成しない（遅延生成の意味: worker が実際に動いたときだけポートとインスタンスを消費する）。
func (m *taskProxyManager) Refresh(ctx context.Context, taskID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst := m.proxies[taskID]; inst != nil {
		m.refreshLocked(ctx, taskID, inst)
	}
}

// RefreshAll は全インスタンスの上流を再計算する（グローバル出口プロキシが変わると、トンネルのないタスクのフォールバック上流も変わる）。
func (m *taskProxyManager) RefreshAll(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, inst := range m.proxies {
		m.refreshLocked(ctx, id, inst)
	}
}

// CloseTask はタスクインスタンスを破棄する: リッスン停止、インデックスを閉じ、ポートを登録解除する。CA ディレクトリは残す（録画の証跡
// データはインスタンス破棄に伴って消さない）。冪等。
func (m *taskProxyManager) CloseTask(taskID int64) {
	m.mu.Lock()
	inst := m.proxies[taskID]
	delete(m.proxies, taskID)
	m.mu.Unlock()
	if inst == nil {
		return
	}
	unregisterManagedPort(inst.port)
	if err := inst.tr.Close(); err != nil {
		log.Printf("[taskproxy] タスク %d の MITM インスタンスをクローズ: %v", taskID, err)
	}
}

// Close はプロセス終了時にすべて片付ける（冪等）。
func (m *taskProxyManager) Close() {
	m.mu.Lock()
	insts := m.proxies
	m.proxies = map[int64]*taskProxyInst{}
	m.mu.Unlock()
	for id, inst := range insts {
		unregisterManagedPort(inst.port)
		if err := inst.tr.Close(); err != nil {
			log.Printf("[taskproxy] タスク %d の MITM インスタンスをクローズ: %v", id, err)
		}
	}
}
