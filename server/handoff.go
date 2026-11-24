// handoff.go 期 4：外部ネットワーク→内部ネットワークのタスク引き継ぎ。フットホールドの
// セッションを起点にサブタスクを作成する：初期 scope はフットホールドホストの /32
// （source='manual'）のみを登録し、元タスクを読み取り専用で継承、parent_ref で親子を
// 関連付け、launchTask と同じ作成後フローを通す。新しいセグメントはすべて
// pending_scope による人手承認に回す（pendingscope.go 参照）。引き継ぎ goal にこの規律を明記する。
package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// handoffHostIP はフットホールドホストの IP を特定する：host_asset_id が指す資産
// （資産には解決済み IP が入る）を優先し、なければセッション URL の IP リテラルの
// ホストにフォールバックする。どちらも無ければ ""。
func (s *Server) handoffHostIP(rec *db.SessionRecord) string {
	if rec.HostAssetID > 0 && s.m.Assets() != nil {
		if assets, err := s.m.Assets().GetByIDs([]int64{rec.HostAssetID}); err == nil {
			for _, a := range assets {
				if a.IP != "" {
					return a.IP
				}
			}
		}
	}
	if u, err := url.Parse(rec.URL); err == nil {
		if h := u.Hostname(); net.ParseIP(h) != nil {
			return h
		}
	}
	return ""
}

// handoffGoal は引き継ぎ goal テキスト（日本語テンプレート）を組み立てる：フットホールド
// 情報 + 検出済みだが未認可の pending セグメント + 認可済み scope + 内部ネットワークの規律。
func handoffGoal(rec *db.SessionRecord, hostIP string, pending []db.PendingScope, scope []db.TaskScope) string {
	var b strings.Builder
	fmt.Fprintf(&b, "内部ネットワーク横展開：セッション #%d(%s,%s) の存在する足場ホスト %s を起点として、内部ネットワークの奥へ展開します。\n",
		rec.ID, rec.Kind, rec.URL, hostIP)
	fmt.Fprintf(&b, "足場：session_id=%d kind=%s url=%s host_asset_id=%d。\n",
		rec.ID, rec.Kind, rec.URL, rec.HostAssetID)
	if len(pending) > 0 {
		segs := make([]string, 0, len(pending))
		for _, p := range pending {
			segs = append(segs, p.Value)
		}
		fmt.Fprintf(&b, "元タスクで発見済みだが【未認可】のネットワークセグメント(直接触れるのは厳禁。人手の承認が必要):%s。\n", strings.Join(segs, "、"))
	}
	if len(scope) > 0 {
		items := make([]string, 0, len(scope))
		for _, sc := range scope {
			v := sc.Domain + sc.Net + sc.Value
			if v != "" {
				items = append(items, fmt.Sprintf("%s=%s", sc.Kind, v))
			}
		}
		if len(items) > 0 {
			fmt.Fprintf(&b, "元タスクで認可済みの scope(背景の参考のみ。認可を自動継承しない):%s。\n", strings.Join(items, "、"))
		}
	}
	b.WriteString("\n規律:\n")
	b.WriteString("1. 本タスクの初期認可 scope は足場ホスト " + hostIP + "/32 のみです。受動的偵察(session_recon)で発見した新しいネットワークセグメントは、いったんすべて pending_scope に登録して人手の承認を待ちます。承認前は新しいネットワークセグメントへの能動的プローブ/パケット送信を行わないでください。\n")
	b.WriteString("2. 受動優先：まず session_recon の読み取り専用コマンド群で NIC/ルート/隣接/リスナーを取得します。受動情報が不十分な場合のみ、能動的な低速スキャンを検討します(別途 guard の承認が必要)。\n")
	b.WriteString("3. 足場の外へ出るネットワークアクセスは tunnel_deploy の 4 点セット(トンネル展開 → タスクレベルのプロキシが自動で socks 経由で内部ネットワークに入る)を使用し、足場の外で直接やみくもにスキャンしないでください。\n")
	b.WriteString("4. 動静の制御：セッション経由のコマンドでは OPSEC(ログ/プロセス/ネットワークの痕跡)に注意し、大量で高ノイズな動作を避けてください。\n")
	return b.String()
}

// POST /api/sessions/{id}/handoff — 外部ネットワーク→内部ネットワークのタスク引き継ぎ。{task_id} を返す。
func (s *Server) sessionHandoff(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rec, err := s.sessStore.Get(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "セッションの読み込みに失敗しました: "+err.Error())
		return
	}
	if rec == nil {
		writeErr(w, 404, fmt.Sprintf("セッション %d は存在しません", id))
		return
	}
	srcTask, ok := s.m.Task(strconv.FormatInt(rec.CreatedByTask, 10))
	if rec.CreatedByTask <= 0 || !ok {
		writeErr(w, 404, fmt.Sprintf("セッション %d のソースタスク #%d は存在しません", id, rec.CreatedByTask))
		return
	}
	hostIP := s.handoffHostIP(rec)
	if hostIP == "" {
		writeErr(w, 400, "フットホールドホストの IP を特定できません（host_asset に IP がなく、セッション URL も IP リテラルではありません）")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "asset store は有効になっていません")
		return
	}
	pending, err := as.ListPendingScope(rec.CreatedByTask, false)
	if err != nil {
		writeErr(w, 500, "pending_scope の読み込みに失敗しました: "+err.Error())
		return
	}
	scope, err := as.ListTaskScope(rec.CreatedByTask)
	if err != nil {
		writeErr(w, 500, "task_scope の読み込みに失敗しました: "+err.Error())
		return
	}

	desc := fmt.Sprintf("内部ネットワーク横移動（フットホールドセッション #%d %s）", rec.ID, hostIP)
	goal := handoffGoal(rec, hostIP, pending, scope)
	// オーケストレーションの spawn_task と同じ経路：元タスクを読み取り専用で継承 + parent_ref の親子関連 + launchTask。
	t, err := s.m.CreateTaskWithOptions(desc, goal, db.TaskCreateOptions{
		SourceTaskIDs: []int64{rec.CreatedByTask},
	})
	if err != nil {
		writeErr(w, 500, "サブタスクの作成に失敗しました: "+err.Error())
		return
	}
	t.ParentRef = srcTask.ID
	if newID, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
		_ = s.m.PG().SetParentRef(newID, srcTask.ID)
		// 初期 scope はフットホールドホストの /32 のみを登録（手動ソース。guard は scope を都度参照するため即時反映）。
		if _, e := as.AddAgentScope(newID, "ip", hostIP,
			fmt.Sprintf("handoff フットホールド（session #%d）", rec.ID), "manual"); e != nil {
			writeErr(w, 500, "フットホールドの scope 登録に失敗しました: "+e.Error())
			return
		}
	}
	s.launchTask(t, desc+" "+goal, false)
	s.auditIntranet(r, "handoff session=%d host=%s → task=%s（ソースタスク %s）", rec.ID, hostIP, t.ID, srcTask.ID)
	writeJSON(w, 200, map[string]any{"task_id": t.ID})
}
