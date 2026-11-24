// session_recon.go はイントラネットのパッシブ偵察ツール(イントラネット侵攻フェーズ 2、docs/architecture/intranet-pivot.md §5 フェーズ 2):
// フットホールドセッション経由で【読み取り専用】の偵察コマンドパッケージ(ip addr/ip route/arp/hosts/netstat、
// 対象に ip コマンドがなければ自動で ifconfig/route -n にフォールバック)を 1 件ずつ実行し、recon パッケージが構造化した結果を解析したうえで:
//
//	① 新しい IP/ネイバー → ip アセット、待ち受けポート → service アセット(insert_assets の永続化経路を再利用);
//	② 高価値な推論と構造化レポートを record_fact で探索グラフへ;
//	③ 要約を返す。
//
// 設計判断は chains の pivot 骨格「パッシブ六表」(agent/chainskel/pivot.md)に従う:まずパケットを送らず、
// パッシブで取れたものだけで判断する。1 件ずつ実行するのは session_exec の 64KB 出力切り詰めを避けるため。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/recon"
	actool "github.com/Autumn-27/norma/tool"
)

// reconCommands はパッシブ偵察コマンドパッケージ(すべて読み取り専用。1 件ずつ実行して出力を個別に取得し、64KB の切り詰めを避ける)。
// shell レベルの || フォールバック:対象に ip/ss がなければ自動で ifconfig/route -n/netstat に落ちる。
var reconCommands = []struct {
	key string
	cmd string
}{
	{"nics", "ip -o -4 addr show 2>/dev/null || ifconfig -a 2>/dev/null"},
	{"routes", "ip route 2>/dev/null || route -n 2>/dev/null"},
	{"neighbors", "ip neigh 2>/dev/null || arp -a 2>/dev/null"},
	{"hosts", "cat /etc/hosts 2>/dev/null"},
	{"conns", "(ss -ant 2>/dev/null; ss -anu 2>/dev/null) || netstat -an 2>/dev/null"},
}

// sessionReconTool はセッション経由でパッシブ偵察コマンドパッケージを実行し、構造化して DB に戻す。
func (s *Server) sessionReconTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "session_recon",
		Description: "【パッシブ優先】フットホールドセッション経由で対象側で【読み取り専用】のパッシブ偵察コマンドパッケージ(ip addr / ip route / arp -a / /etc/hosts / netstat," +
			"対象に ip コマンドがなければ自動で ifconfig/route -n にフォールバック)を実行し、プローブパケットは一切送らない。" +
			"構造化レポート(NIC/ルート/ネイバー/hosts/待ち受けポート/確立済みピア)+ 高価値な推論(デフォルト以外の静的ルートが指すセグメント、" +
			"到達可能と証明されたピアホスト、複数 NIC の踏み台)を出力し、" +
			"発見した生存 IP は自動で ip アセット、待ち受けポートは service アセットとして登録し、レポートと推論は record_fact で探索グラフへ書き込む。" +
			"パッシブ情報で足りない場合に初めて能動的な低速スキャンを検討する(別途 guard を通る)。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "integer", "description": "register_session が返すセッション id"},
			},
			"required": []any{"session_id"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID int64 `json:"session_id"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(a.SessionID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// 1 件ずつ実行:各コマンドの出力を個別に取得し(64KB の切り詰めを回避)、失敗しても全体を止めない。
			// パッシブ偵察の原則は「取れたものだけで判断する」であり、1 つのコマンドが落ちても残りは通常どおり実行する。
			outputs := map[string]string{}
			cmdErrs := map[string]string{}
			for _, c := range reconCommands {
				stdout, _, err := sess.Exec(ctx, c.cmd, 15*time.Second)
				if err != nil {
					cmdErrs[c.key] = err.Error()
					continue
				}
				outputs[c.key] = stdout
			}
			s.sessStore.Touch(ctx, a.SessionID)

			report := &recon.ReconReport{
				NICs:         recon.ParseNICsAuto(outputs["nics"]),
				Routes:       recon.ParseRoutesAuto(outputs["routes"]),
				Neighbors:    recon.ParseNeighbors(outputs["neighbors"]),
				HostsEntries: recon.ParseHosts(outputs["hosts"]),
			}
			report.Listening, report.Peers = recon.ParseConnTable(outputs["conns"])
			inferences := recon.Infer(report)

			ri := agent.RunInfoFrom(ctx)
			assetIDs, assetSummary, assetErrs := s.reconInsertAssets(ctx, ri.TaskID, report)
			factID := s.reconRecordFact(ri, a.SessionID, report, inferences, assetIDs)

			out := map[string]any{
				"report":     report,
				"inferences": inferences,
				"summary":    reconSummary(report, inferences),
			}
			if assetSummary != "" {
				out["assets"] = assetSummary
			}
			if factID > 0 {
				out["fact_id"] = factID
			}
			if len(cmdErrs) > 0 {
				out["command_errors"] = cmdErrs
			}
			if len(assetErrs) > 0 {
				out["asset_errors"] = assetErrs
			}
			return jsonResult(out)
		},
	})
}

// reconSummary はモデル向けの一言要約。
func reconSummary(r *recon.ReconReport, inf []recon.Inference) string {
	static := 0
	for _, rt := range r.Routes {
		if rt.IsStatic {
			static++
		}
	}
	return fmt.Sprintf("NIC %d / ルート %d(静的 %d)/ ネイバー %d / hosts %d / 待ち受け %d / 確立済みピア %d;高価値な推論 %d 件",
		len(r.NICs), len(r.Routes), static, len(r.Neighbors), len(r.HostsEntries),
		len(r.Listening), len(r.Peers), len(inf))
}

// reconInsertAssets は偵察結果をアセットストアへ書き込む(best-effort):ネイバー/ピア/hosts/自ホスト NIC の
// 新しい IP → ip アセット;待ち受けポート → 自ホスト主 IP の service アセット。アンカーしたアセット id 一覧、
// 要約、エラーを返す(1 件のエラーで全体を止めない)。
func (s *Server) reconInsertAssets(ctx context.Context, taskID int64, r *recon.ReconReport) ([]int64, string, []string) {
	if s.m.Assets() == nil || taskID <= 0 {
		return nil, "", nil
	}
	assets := s.m.Assets()
	ids := []int64{}
	errs := []string{}
	seenIP := map[string]bool{}
	ipCount := 0
	addIP := func(ip string) {
		if ip == "" || seenIP[ip] {
			return
		}
		seenIP[ip] = true
		id, err := assets.UpsertIP(db.UpsertIPReq{IP: ip, TaskID: taskID})
		if err != nil {
			errs = append(errs, fmt.Sprintf("ip %s: %v", ip, err))
			return
		}
		ids = append(ids, id)
		ipCount++
	}

	// 自ホストの非ループバック NIC IP(デュアル NIC はすべて登録)。主 IP(先頭)に待ち受けポートの service アセットを紐付ける。
	hostIP := ""
	for _, n := range r.NICs {
		addr := n.IP
		if strings.HasPrefix(addr, "127.") || strings.HasPrefix(addr, "169.254.") {
			continue
		}
		if hostIP == "" {
			hostIP = addr
		}
		addIP(addr)
	}
	for _, nb := range r.Neighbors {
		addIP(nb.IP)
	}
	for _, p := range r.Peers {
		addIP(p.IP)
	}
	for _, h := range r.HostsEntries {
		if strings.HasPrefix(h.IP, "127.") {
			continue
		}
		addIP(h.IP)
	}

	svcCount := 0
	if hostIP != "" {
		for _, l := range r.Listening {
			name := recon.ServiceOf(l.Port)
			if name == "" {
				name = fmt.Sprintf("unknown-%d", l.Port)
			}
			id, err := assets.UpsertOtherService(db.UpsertOtherServiceReq{
				IP: hostIP, Port: l.Port, ServiceName: name, TaskID: taskID,
			})
			if err != nil {
				errs = append(errs, fmt.Sprintf("service %s:%d: %v", hostIP, l.Port, err))
				continue
			}
			ids = append(ids, id)
			svcCount++
		}
	}
	summary := fmt.Sprintf("ip アセット %d 件、service アセット %d 件を登録", ipCount, svcCount)
	if len(ids) > 20 {
		ids = ids[:20] // アンカー一覧の規模を制限
	}
	// フェーズ 4:現在の認可範囲外の新しい IP は /24 単位で pending_scope として登録し、手動承認を待つ(pendingscope.go 参照)。
	s.reconRegisterPendingScope(taskID, seenIP)
	return ids, summary, errs
}

// reconRecordFact は偵察レポートと推論を探索グラフに書き込む(record_fact と同じ経路：fact ノード +
// intent→yields エッジ)。タスク実行外/グラフ利用不可ならスキップする(0 を返す)。
func (s *Server) reconRecordFact(ri agent.RunInfo, sessionID int64, r *recon.ReconReport, inf []recon.Inference, anchors []int64) int64 {
	if ri.ExplorationID <= 0 || s.m.pg == nil {
		return 0
	}
	lines := []string{}
	for _, i := range inf {
		line := "· " + i.Summary
		if i.Detail != "" {
			line += " — " + i.Detail
		}
		lines = append(lines, line)
	}
	detail := reconSummary(r, inf)
	if len(lines) > 0 {
		detail += "\n高価値な推論:\n" + strings.Join(lines, "\n")
	}
	payload := map[string]any{
		"summary":    fmt.Sprintf("session %d イントラネットパッシブ偵察:%s", sessionID, reconSummary(r, inf)),
		"detail":     detail,
		"evidence":   "session_recon パッシブコマンドパッケージ(ip addr/ip route/arp -a/hosts/netstat、読み取り専用)",
		"confidence": "observed",
	}
	store := s.m.pg.Exploration(ri.ExplorationID)
	id, err := store.AddNode(db.KindFact, payload, 5, "confirmed", "worker", anchors)
	if err != nil {
		return 0
	}
	if ri.IntentID > 0 {
		if node, nerr := store.GetNode(ri.IntentID); nerr == nil && node != nil && node.Kind == db.KindIntent {
			_ = store.Link(ri.IntentID, db.RelYields, id)
		}
	}
	return id
}
