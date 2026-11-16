// pendingscope.go 期 4：未認可セグメントの登録（pending_scope）の検出側フックと承認 API。
//
// 検出側：session_recon のパッシブ偵察が保存された後、現在の task_scope を外れる新規 IP を
// /24 単位に集約して登録する（upsert は冪等で、重複偵察は hits を加算するだけで通知は増やさない）。
// 初回登録時はタスクのアクティビティフィードに通知カードを書く。
// 承認側：GET 一覧 + POST decide。approve はセグメントを task_scope に書き込み
// （source='manual'、guard はツール呼び出しごとに scope を参照するため承認は即時反映）、dismiss は状態だけ変更する。
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
)

// cSegment24 は db の calcCSegment の server 側の小さな複製（この 1 呼び出しのためだけに
// db 内部関数をエクスポートしない）：IPv4 → /24、IPv6 → /48、不正 → ""。
func cSegment24(ipStr string) string {
	if ipStr == "" {
		return ""
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		parts := strings.Split(ipStr, ".")
		if len(parts) == 4 {
			return parts[0] + "." + parts[1] + "." + parts[2] + ".0/24"
		}
		return ""
	}
	_, ipnet, err := net.ParseCIDR(ipStr + "/48")
	if err != nil {
		return ""
	}
	return ipnet.String()
}

// reconRegisterPendingScope は偵察で新たに検出した IP を現在のタスク scope に個別に照合し
// （guard と同じ scopeRulesFromRows + ScopeMatcher）、ScopeOut のものをセグメント単位で集約して
// UpsertPendingScope する。初回登録（created）ではタスクのアクティビティフィードに
// 承認待ち通知カードを 1 枚書く。ScopeUnknown（タスクに scope ルールが一切無い）の場合
// Check は ScopeOut を返さないため、自然にバッチ全体がスキップされる。
func (s *Server) reconRegisterPendingScope(taskID int64, ips map[string]bool) {
	as := s.m.Assets()
	if as == nil || taskID <= 0 || len(ips) == 0 {
		return
	}
	rows, err := as.ListTaskScope(taskID)
	if err != nil {
		return
	}
	matcher := guard.NewScopeMatcher(scopeRulesFromRows(rows))
	segExample := map[string]string{} // セグメント → 例示 IP（各セグメントの先頭）
	for ip := range ips {
		if matcher.Check(ip) != guard.ScopeOut {
			continue
		}
		if seg := cSegment24(ip); seg != "" {
			if _, ok := segExample[seg]; !ok {
				segExample[seg] = ip
			}
		}
	}
	for seg, exampleIP := range segExample {
		created, err := as.UpsertPendingScope(taskID, "cidr", seg)
		if err != nil {
			log.Printf("[pendingscope] タスク %d の %s の登録に失敗しました: %v", taskID, seg, err)
			continue
		}
		if created {
			s.notifyPendingScope(taskID, seg, exampleIP)
		}
	}
}

// notifyPendingScope はタスクのアクティビティフィードに「新セグメントが承認待ち」通知カードを
// 書く（初回登録時のみ呼ばれ、upsert の冪等性が通知の増殖を防ぐ）。
func (s *Server) notifyPendingScope(taskID int64, seg, exampleIP string) {
	t, ok := s.m.Task(strconv.FormatInt(taskID, 10))
	if !ok {
		return
	}
	detail, _ := json.Marshal(map[string]any{
		"cidr": seg, "example_ip": exampleIP,
		"list_api": fmt.Sprintf("/api/tasks/%d/pending-scope", taskID),
	})
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
		Summary: fmt.Sprintf("未認可のセグメント %s（例示 IP %s）を検出し、承認待ちとして登録しました。人手で承認されてから scope を拡張します", seg, exampleIP),
		Detail:  string(detail)})
}

// GET /api/tasks/{id}/pending-scope — 承認待ちセグメントの一覧（既定は pending のみ、?all=1 で決着済みも含む）。
func (s *Server) pendingScopeList(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "asset store は有効になっていません")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	items, err := as.ListPendingScope(taskID, r.URL.Query().Get("all") == "1")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// POST /api/pending-scope/{id}/decide {action:"approve"|"dismiss"} — 1 件の登録を承認する。
// approve はまずセグメントを task_scope に書き込み（kind=cidr、source='manual'）、次に状態を
// 遷移させる。dismiss は状態だけを遷移させる。冪等：同じ決定の繰り返しは現状を返す（200）、
// 矛盾する決定（approved 済みを dismiss、またはその逆）は 409 を返す。
func (s *Server) pendingScopeDecide(w http.ResponseWriter, r *http.Request) {
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "asset store は有効になっていません")
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	approve := body.Action == "approve"
	if body.Action != "approve" && body.Action != "dismiss" {
		writeErr(w, 400, "action は approve または dismiss である必要があります")
		return
	}
	row, err := as.GetPendingScope(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "pending_scope のエントリが存在しません")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	desired := "dismissed"
	if approve {
		desired = "approved"
	}
	if row.Status != "pending" {
		if row.Status == desired {
			writeJSON(w, 200, row) // 同じ決定の繰り返し：冪等に現状を返す
			return
		}
		writeErr(w, 409, "エントリは処理済みです（status="+row.Status+"）。矛盾する決定は受け付けません")
		return
	}
	if approve {
		if _, err := as.AddAgentScope(row.TaskID, row.Kind, row.Value,
			fmt.Sprintf("人手承認による scope 拡張（pending_scope #%d）", row.ID), "manual"); err != nil {
			writeErr(w, 400, "task_scope への書き込みに失敗しました: "+err.Error())
			return
		}
	}
	row, err = as.DecidePendingScope(id, approve)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, row)
}
