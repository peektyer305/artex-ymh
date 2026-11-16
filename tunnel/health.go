// health.go は 4 点セットのうち health。周期的な実疎通確認（プラットフォーム側
// server プロセスの生存 + トンネル入口への接続可否 + 数ラウンドごとに 1 回 socks
// 経由の実際の疎通確認）を行い、死んだら error を付けて deploy_params で再 Deploy する
// （指数バックオフ、上限 3 回）——死んだリンクを決して生きた上流として扱わない。
// さらに Teardown（リモートプロセスをセッション経由で pid 指定 kill、プラットフォーム
// server の kill、stage の削除、state=stopped）と Cleanup（タスク停止/プロセス
// 終了時に manager から呼ばれる）を含む。
package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
)

// 巡回確認のパラメータ。
const (
	HealthInterval     = 30 * time.Second
	decisiveEveryNTick = 5                // 5 ラウンドごとに socks 経由の決定的な疎通確認
	maxRedeploys       = 3                // 自動再起動の上限
	redeployBaseDelay  = 30 * time.Second // バックオフの基数：30s → 60s → 120s
)

type redeployState struct {
	count     int
	nextAt    time.Time
	exhausted bool // 上限到達済み（ログは 1 回だけ出力）
}

// StartHealth は周期巡回確認の goroutine を起動する。ctx のキャンセルで終了する（server のライフサイクルに従う）。
func (m *Manager) StartHealth(ctx context.Context) {
	retry := map[int64]*redeployState{}
	tick := 0
	go func() {
		t := time.NewTicker(HealthInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tick++
				m.healthRound(ctx, tick%decisiveEveryNTick == 0, retry)
			}
		}
	}()
}

// healthRound は巡回確認を 1 ラウンド行う。alive 行はプロセス + 入口の疎通確認
// （decisive ラウンドは実際の疎通確認も）を行い、失敗したら error を付けて再起動へ回す。
// error 行（プラットフォーム再起動後の孤児を含む）はバックオフ予算内で再起動を
// 試みる。
func (m *Manager) healthRound(ctx context.Context, decisive bool, retry map[int64]*redeployState) {
	recs, err := m.store.ListActive(ctx)
	if err != nil {
		log.Printf("[tunnel] 巡回確認で台帳の読み込みに失敗しました： %v", err)
		return
	}
	for _, rec := range recs {
		if rec.State == db.TunnelDeploying {
			continue // デプロイ実行中（ツール呼び出し内で同期実行）なので介入しない
		}
		var p Plan
		if err := json.Unmarshal(rec.DeployParams, &p); err != nil {
			_ = m.store.UpdateState(ctx, rec.ID, db.TunnelError, "deploy_params の解析に失敗しました： "+err.Error())
			m.notifyState(rec.TaskID)
			continue
		}
		if rec.State == db.TunnelAlive {
			err = m.checkAlive(&p)
			if err == nil && decisive {
				err = m.checkDecisive(&p)
			}
			if err == nil {
				continue
			}
			log.Printf("[tunnel] トンネル %d の疎通確認に失敗しました： %v", rec.ID, err)
			_ = m.store.UpdateState(ctx, rec.ID, db.TunnelError, err.Error())
			m.notifyState(rec.TaskID)
		}
		m.maybeRedeploy(ctx, rec.ID, &p, retry)
	}
}

// checkAlive は軽量な疎通確認。プラットフォーム server プロセスが存在し、トンネル入口に接続できること。
func (m *Manager) checkAlive(p *Plan) error {
	if p.ServerPID > 0 && !processAlive(p.ServerPID) {
		return fmt.Errorf("プラットフォーム側 chisel server(pid %d）は終了しました", p.ServerPID)
	}
	conn, err := net.DialTimeout("tcp", m.entryAddr(p), 2*time.Second)
	if err != nil {
		return fmt.Errorf("トンネル入口 %s に接続できません（目標のコールバック接続が切断済み）: %w", m.entryAddr(p), err)
	}
	_ = conn.Close()
	return nil
}

// checkDecisive は決定的な疎通確認。socks の場合はトンネル経由で実際に CONNECT
// して目標を検証する。
// suo5 の入口は user/pass 認証を伴う（--auth が必須）ため、認証版で疎通確認する。
func (m *Manager) checkDecisive(p *Plan) error {
	if p.Kind != KindSocks || p.VerifyAddr == "" {
		return nil
	}
	if p.Adapter == AdapterSuo5 {
		return socks5DialAuth(m.entryAddr(p), p.VerifyAddr, p.AuthUser, p.AuthPass, verifyTimeout)
	}
	return socks5Dial(m.entryAddr(p), p.VerifyAddr, verifyTimeout)
}

// maybeRedeploy は永続化された deploy_params で再 Deploy する。指数バックオフ、上限 3 回。
func (m *Manager) maybeRedeploy(ctx context.Context, id int64, p *Plan, retry map[int64]*redeployState) {
	rs := retry[id]
	if rs == nil {
		rs = &redeployState{}
		retry[id] = rs
	}
	if rs.count >= maxRedeploys {
		if !rs.exhausted {
			rs.exhausted = true
			log.Printf("[tunnel] トンネル %d は再起動の上限 %d 回に達しました。error のまま維持し、手動の tunnel_teardown/再デプロイを待ちます", id, maxRedeploys)
		}
		return
	}
	if time.Now().Before(rs.nextAt) {
		return
	}
	rs.count++
	rs.nextAt = time.Now().Add(redeployBaseDelay << (rs.count - 1)) // 30s/60s/120s
	sess, ok := m.sessions.Get(p.ViaSessionID)
	if !ok {
		log.Printf("[tunnel] トンネル %d の再起動に失敗しました：セッション %d がレジストリにありません", id, p.ViaSessionID)
		return
	}
	log.Printf("[tunnel] トンネル %d の自動再起動 %d/%d 回目（deploy_params の永続化パラメータ）…", id, rs.count, maxRedeploys)
	// 再起動 = 同じ計画での再デプロイ。まず旧レイヤーを片付け（リモートに残留の可能性）、次に新レイヤーを起動する。
	if p.Adapter == AdapterSuo5 {
		// suo5 には目標側プロセスがない。再起動 = 旧 CLI を kill + payload を再投入
		// （目標の payload が削除されたケースもここで自然にカバー）+ CLI を再起動 +
		// 決定的な検証をやり直す。
		m.killServer(id, p)
		if err := m.deploySuo5Into(ctx, id, sess, p); err != nil {
			_ = m.store.UpdateState(ctx, id, db.TunnelError, "再起動に失敗しました： "+err.Error())
			m.killServer(id, p)
			m.removePayload(ctx, sess, p)
			return
		}
	} else {
		m.killRemote(ctx, sess, p)
		m.killServer(id, p)
		if err := m.deployInto(ctx, id, sess, p); err != nil {
			_ = m.store.UpdateState(ctx, id, db.TunnelError, "再起動に失敗しました： "+err.Error())
			m.rollbackKeepRow(ctx, id, sess, p)
			return
		}
	}
	m.saveParams(ctx, id, p)
	if err := m.store.UpdateState(ctx, id, db.TunnelAlive, ""); err == nil {
		rs.count = 0 // 復活に成功したのでバックオフ回数をクリア
		log.Printf("[tunnel] トンネル %d の再起動に成功し、alive に復帰しました", id)
	}
	m.notifyState(p.TaskID)
}

// rollbackKeepRow は再起動の失敗時にレイヤーを片付けるが、台帳行は残す（error 状態で次のラウンドを待つ）。
func (m *Manager) rollbackKeepRow(ctx context.Context, id int64, sess session.Session, p *Plan) {
	if p.RemotePID > 0 {
		m.killRemote(ctx, sess, p)
	}
	m.killServer(id, p)
}

// Teardown はトンネル 1 本を回収する：リモートプロセスをセッション経由で pid
// 指定 kill（リモートを照会し cmdline を照合）→ プラットフォーム server を kill
// → stage エントリを削除 → authfile を削除 → state=stopped。冪等：stopped 済み
// ならそのまま返す。
func (m *Manager) Teardown(ctx context.Context, id int64) error {
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("トンネル %d は存在しません", id)
	}
	if rec.State == db.TunnelStopped {
		return nil
	}
	var p Plan
	_ = json.Unmarshal(rec.DeployParams, &p)
	if p.Adapter == AdapterSuo5 {
		// suo5：目標の payload を削除 + プラットフォーム CLI を kill（目標側に kill 対象のプロセスはない）。
		if p.PayloadPath != "" {
			if sess, ok := m.sessions.Get(p.ViaSessionID); ok {
				m.removePayload(ctx, sess, &p)
			} else {
				log.Printf("[tunnel] Teardown: セッション %d がレジストリにないため、目標の payload %s をセッション経由で片付けられません", p.ViaSessionID, p.PayloadPath)
			}
		}
		m.killServer(id, &p)
		err = m.store.UpdateState(ctx, id, db.TunnelStopped, "")
		m.notifyState(rec.TaskID)
		return err
	}
	if p.RemotePID > 0 {
		if sess, ok := m.sessions.Get(p.ViaSessionID); ok {
			m.killRemote(ctx, sess, &p)
			if p.RemoteDir != "" {
				_, _, _ = sess.Exec(ctx, "rm -rf "+p.RemoteDir, probeTimeout)
			}
		} else {
			log.Printf("[tunnel] Teardown: セッション %d がレジストリにないため、リモート pid %d をセッション経由で片付けられません（セッションの消滅に伴い既に消えている可能性）", p.ViaSessionID, p.RemotePID)
		}
	}
	m.killServer(id, &p)
	if p.StageToken != "" && m.stage != nil {
		m.stage.Delete(p.StageToken)
	}
	if p.AuthFile != "" {
		_ = os.Remove(p.AuthFile)
	}
	err = m.store.UpdateState(ctx, id, db.TunnelStopped, "")
	m.notifyState(rec.TaskID)
	return err
}

// Cleanup はトンネルをまとめて回収する（タスク停止/プロセス終了時に manager が
// 呼ぶ）。taskID=0 なら未終了のトンネルをすべて回収する。best-effort で、1 件の
// 失敗はログに残すだけ。
func (m *Manager) Cleanup(ctx context.Context, taskID int64) {
	var recs []*db.TunnelRecord
	var err error
	if taskID > 0 {
		recs, err = m.store.ListByTask(ctx, taskID)
	} else {
		recs, err = m.store.ListActive(ctx)
	}
	if err != nil {
		log.Printf("[tunnel] Cleanup で台帳の読み込みに失敗しました： %v", err)
		return
	}
	for _, r := range recs {
		if r.State == db.TunnelStopped {
			continue
		}
		if err := m.Teardown(ctx, r.ID); err != nil {
			log.Printf("[tunnel] Cleanup のトンネル %d に失敗しました： %v", r.ID, err)
		}
	}
}

// MarkOrphansError はプラットフォーム再起動後に呼ぶ：alive/deploying の台帳行は
// プラットフォーム側プロセスが再起動で消滅しているため、正直に error を付ける
// （生きたふりをしない）。ヘルスチェックが deploy_params に従い自動で再起動する。
func (m *Manager) MarkOrphansError(ctx context.Context) {
	recs, err := m.store.ListActive(ctx)
	if err != nil {
		return
	}
	for _, r := range recs {
		_ = m.store.UpdateState(ctx, r.ID, db.TunnelError, "プラットフォーム再起動により server プロセスが消滅しました。ヘルスチェックによる自動再起動を待ちます")
		m.notifyState(r.TaskID)
	}
}

// processAlive はプラットフォーム側プロセスの生存を確認する（linux:/proc、
// windows:FindProcess が失敗すれば不在、その他の unix:Signal(0)）。
func processAlive(pid int) bool {
	if runtime.GOOS == "linux" {
		_, err := os.Stat("/proc/" + strconv.Itoa(pid))
		return err == nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // FindProcess がハンドルを開けない場合は前のステップで既に返却済み
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// serverCmdlineMatches は kill 前に /proc/<pid>/cmdline を照合し、台帳にあるトンネル
// プロセスであることを確かめる（pid 再利用による誤 kill の防止。非 linux では
// 保守的に false を返し、自プロセスの台帳ハンドルだけに頼る）。
// chisel は authfile のパス、suo5 は CLI 名と payload URL を照合する。
func serverCmdlineMatches(p *Plan) bool {
	if runtime.GOOS != "linux" || p.ServerPID <= 0 {
		return false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(p.ServerPID) + "/cmdline")
	if err != nil {
		return false
	}
	cl := strings.ReplaceAll(string(data), "\x00", " ")
	if p.Adapter == AdapterSuo5 {
		return strings.Contains(cl, "suo5") && (p.PayloadURL == "" || strings.Contains(cl, p.PayloadURL))
	}
	return strings.Contains(cl, "chisel") && strings.Contains(cl, p.AuthFile)
}
