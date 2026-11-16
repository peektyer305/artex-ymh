// tunnel.go は多層プロキシトンネルサブシステムを組み込む（イントラネット侵攻フェーズ 3a,INTRANET-PIVOT-DESIGN.md
// §4.3/4.3a):tunnel.Manager（プロセス監視 + ヘルスチェック）+ 4 つの host ツール
// (tunnel_probe/tunnel_deploy/tunnel_list/tunnel_teardown、デフォルトで worker に紐付け）。
// トンネルは長寿命の管理対象リソース:プラットフォームの server プロセス、ターゲットの client プロセス、stage 配信エントリを
// すべて tunnels 台帳に登録する。ツール説明でも「使い終わったら teardown、裸の Bash でトンネルプロセスを自前起動しない」と明示している。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/tunnel"
	actool "github.com/Autumn-27/norma/tool"
)

// initTunnels はトンネルランタイムを構築する：chisel バイナリの解決（ARTEX_CHISEL_PATH > data/tools/chisel)、
// 台帳アクセス層、ポート監査フック、プラットフォーム再起動時の孤児行を error に（正直に、生きているふりはしない）、ヘルスチェック起動。
// 失敗時はログのみで、起動を止めない。
func (s *Server) initTunnels(dataDir string) {
	if s.m.pg == nil || s.sessReg == nil {
		return
	}
	chiselPath := strings.TrimSpace(os.Getenv("ARTEX_CHISEL_PATH"))
	if chiselPath == "" {
		chiselPath = filepath.Join(dataDir, "tools", "chisel")
	}
	suo5Path := strings.TrimSpace(os.Getenv("ARTEX_SUO5_PATH"))
	if suo5Path == "" {
		suo5Path = filepath.Join(dataDir, "tools", "suo5")
	}
	suo5PayloadsDir := strings.TrimSpace(os.Getenv("ARTEX_SUO5_PAYLOADS_DIR"))
	if suo5PayloadsDir == "" {
		suo5PayloadsDir = filepath.Join(dataDir, "tools", "suo5-payloads")
	}
	mgr, err := tunnel.NewManager(tunnel.Options{
		Store:           db.NewTunnelStore(s.m.pg),
		Stage:           s.stage,
		Sessions:        s.sessReg,
		ChiselPath:      chiselPath,
		Suo5Path:        suo5Path,
		Suo5PayloadsDir: suo5PayloadsDir,
		DataDir:         dataDir,
	})
	if err != nil {
		log.Printf("[tunnel] disabled: %v", err)
		return
	}
	// トンネル server の待受ポートをポート監査台帳に登録する（管理対象リソースの原則：台帳外の待受だけを警告）。
	mgr.OnListenStart = registerManagedPort
	mgr.OnListenStop = unregisterManagedPort
	// フェーズ 3b:トンネル状態の変化(追加/停止/再起動/teardown)→ そのタスクの MITM インスタンス上流をホットスイッチ。
	mgr.OnStateChange = s.m.RefreshTaskProxy
	s.tunnels = mgr
	mgr.MarkOrphansError(s.ctx) // プラットフォーム再起動 = server プロセス全滅、正直に error を付け、ヘルスチェックが deploy_params で再起動
	mgr.StartHealth(s.ctx)
	log.Printf("[tunnel] 多層プロキシトンネルサブシステム準備完了（ポートプール %s、ヘルスチェック %v)", config.TunnelPortRange(), tunnel.HealthInterval)
}

// callbackHost はターゲットがプラットフォームへ接続する際のホストアドレスを取得する（ARTEX_CALLBACK_ADDR の host 部分）。
func callbackHost() (string, error) {
	addr := config.CallbackAddr()
	if addr == "" {
		return "", fmt.Errorf("プラットフォームのコールバックアドレスが未設定です：ARTEX_CALLBACK_ADDR=<ターゲットから到達可能なプラットフォームIP:ポート> を設定して再試行してください")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, nil //  ベア host（ポートなし）も受け付ける
	}
	return host, nil
}

// roeCheckHost はターゲット host が本タスクの RoE 範囲内かを検証する（register_session と同じ仕組み：
// strict はブロック、warn は警告して通過、fail-open)。警告テキストとブロックエラーを返す。
func (s *Server) roeCheckHost(ctx context.Context, host string) (string, error) {
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID <= 0 || s.m.pg == nil || host == "" {
		return "", nil
	}
	mode, _, _ := s.m.pg.GetSetting(settingRoEEnforcement)
	if strings.EqualFold(strings.TrimSpace(mode), "off") {
		return "", nil
	}
	rows, err := s.m.pg.Assets().ListTaskScope(ri.TaskID)
	if err != nil {
		return "", nil // 読み取り失敗は未登録範囲として扱う（fail-open、guard と同様）
	}
	verdict := guard.NewScopeMatcher(scopeRulesFromRows(rows)).Check(host)
	if verdict != guard.ScopeOut {
		return "", nil
	}
	msg := fmt.Sprintf("RoE 認可範囲チェック：トンネルのターゲット %s は本タスク(task %d)の登録範囲外です。"+
		"通過させるには：ターゲットをタスク範囲に追加(add_task_scope)するか、システム設定で roe_enforcement を warn/off に変更してください。",
		host, ri.TaskID)
	if strings.EqualFold(strings.TrimSpace(mode), "strict") {
		return "", fmt.Errorf("%s", msg)
	}
	return "⚠ " + msg, nil
}

// sessionVerifyAddr はフットホールドセッションの webshell host:port を socks の決定的な検証対象として取得する
// （ターゲットは必ず自分の web ポートへ接続できる——これが「トンネルが本当に通っている」最も硬い証拠）。
func (s *Server) sessionVerifyAddr(ctx context.Context, sessionID int64) string {
	if s.sessStore == nil {
		return ""
	}
	rec, err := s.sessStore.Get(ctx, sessionID)
	if err != nil || rec == nil || rec.URL == "" {
		return ""
	}
	u, err := url.Parse(rec.URL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// --- host ツール（デフォルトで worker に紐付け;tools テーブルに seed し、agent ごとに絞り込む） ---

// tunnelTools はトンネル 4 ツールを返す。tunnels が nil(DB 未準備）のときは提供しない。
func (s *Server) tunnelTools() []actool.CoreTool {
	if s.tunnels == nil {
		return nil
	}
	return []actool.CoreTool{
		s.tunnelProbeTool(),
		s.tunnelDeployTool(),
		s.tunnelListTool(),
		s.tunnelTeardownTool(),
	}
}

// tunnelProbeTool は外向き通信の疎通確認 + 方式選定の提案（4 点セットの probe)。
func (s *Server) tunnelProbeTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "tunnel_probe",
		Description: "【トンネル 4 点セット 1/4·probe】フットホールドセッション経由でターゲット側の外向き通信能力を実測する：TCP でプラットフォームの常用ポート(443/80/53)" +
			"とトンネルポートプールのサンプルポート（tunnel_port_range の開始・終了）、HTTP の外向き通信、DNS 再帰解決、任意で ICMP。" +
			"TCP プローブは 3 状態：接続成功、または refused 拒否（RST は経路が通る証拠で、単に待受がないだけ）も通とみなす。タイムアウトのみフィルタ済みと判定。" +
			"プローブごとのエビデンス + エビデンスの段階に基づく方式選定の結論（含 suggested_adapter):" +
			"TCP 外向きが可能→tunnel_deploy adapter=chisel のリバース socks を推奨；外向き通信が全断だが webshell が使える→" +
			"tunnel_deploy adapter=suo5(HTTP トンネル、外向き通信に依存しない）を推奨；DNS のみ→コマンドチャネル級の説明。" +
			"トンネルを立てる前にまずこれを実行し、闇雲に作らない。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "integer", "description": "フットホールドセッション id(register_session の戻り値）"},
				"icmp":       map[string]any{"type": "boolean", "description": "ICMP プローブを追加するか（ノイズが多い、デフォルト false)"},
			},
			"required": []any{"session_id"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID int64 `json:"session_id"`
				ICMP      bool  `json:"icmp"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(a.SessionID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			host, err := callbackHost()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// プローブ対象 = 常用コールバックポート 443/80/53 + トンネルポートプールのサンプル（開始・終了を各 1 個）:
			// トンネルが実際に使う待受ポートもプローブしないと、「常用ポートがフィルタされている」と誤判定して TCP 外向き不可と結論してしまう。
			addrs := tunnel.ProbeAddrs(host, config.TunnelPortRange())
			rep, err := tunnel.Probe(ctx, sess, addrs, a.ICMP)
			if err != nil {
				return actool.Errorf("プローブに失敗しました：" + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"report":     rep,
				"conclusion": tunnel.Conclude(rep),
			})
		},
	})
}

// tunnelDeployTool は計画+配備+決定的検証（4 点セットの plan+deploy)。
func (s *Server) tunnelDeployTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "tunnel_deploy",
		Description: "【トンネル 4 点セット 2/4·plan+deploy】フットホールドセッション経由でリバーストンネルを配備する。アダプタは 2 つ：" +
			"adapter=chisel（デフォルトの能力）：ターゲットが TCP で外向き通信できるときに使い、プラットフォームが chisel server を起動、バイナリは管理下の配信でターゲットに送って接続させる；" +
			"adapter=suo5（ターゲットに外向き通信がない状況、RED-SUN-3 の振り返りで見つかった穴）:HTTP トンネル。payload(suo5.jsp/php/aspx、セッションの言語に応じて自動選択）" +
			"を webshell と同じディレクトリに書き込み、プラットフォームの suo5 CLI が受信方向の HTTP で socks5 を張る。ターゲットの外向き通信には一切依存せず、kind=socks のみ対応；" +
			"adapter=auto（デフォルト）：現地で probe を実行し、TCP 外向きが可能→chisel、でなければ→suo5。" +
			"kind=socks はリバース socks5 を張る（プラットフォームのループバック socks_addr を返す);kind=portfwd は単一ポート転送を張る（chisel のみ、target=host:port 必須）。" +
			"流れ：配備 → トンネル内での決定的検証（socks 経由で webshell のホストへ実際に CONNECT）、通って初めて alive を付ける；どれか失敗したら段階的にロールバック。" +
			"【トンネルは長寿命の管理対象リソース】使い終わったら必ず tunnel_teardown で回収する；裸の Bash で自分でトンネルプロセスを起動しない（台帳に載らず回収される）。" +
			"portfwd の target host は RoE タスク範囲の検証を受ける。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "integer", "description": "フットホールドセッション id"},
				"kind":       strParam("トンネル種別：socks（リバース socks5) / portfwd（単一ポート転送、chisel のみ)、必須"),
				"target":     strParam("portfwd 必須：トンネル経由でアクセスするイントラネットのターゲット host:port"),
				"adapter":    strParam("アダプタ：auto（デフォルト、probe の結論で決定） / chisel（ターゲットが外向き通信可能） / suo5（ターゲットに外向き通信なし、socks のみ)"),
			},
			"required": []any{"session_id", "kind"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID int64  `json:"session_id"`
				Kind      string `json:"kind"`
				Target    string `json:"target"`
				Adapter   string `json:"adapter"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(a.SessionID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			cbHost, err := callbackHost()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			kind := strings.TrimSpace(a.Kind)
			adapter := strings.ToLower(strings.TrimSpace(a.Adapter))
			if adapter == "" {
				adapter = "auto"
			}
			switch adapter {
			case "auto", tunnel.AdapterChisel, tunnel.AdapterSuo5:
			default:
				return actool.Errorf(fmt.Sprintf("adapter が不正です： %q(auto|chisel|suo5)", a.Adapter)), nil
			}
			if adapter == tunnel.AdapterSuo5 && kind != tunnel.KindSocks {
				return actool.Errorf("suo5 アダプタは kind=socks のみ対応（外向き通信がない状況向けの HTTP socks トンネル）;portfwd は chisel を使ってください"), nil
			}
			if adapter == "auto" {
				// probe の結論で決定：portfwd は chisel のみ;socks は現地で probe を実行し、
				// TCP 外向きが可能→chisel、全て filtered/DNS のみ/ICMP のみ→suo5（外向き通信に依存しない）。
				adapter = tunnel.AdapterChisel
				if kind == tunnel.KindSocks {
					rep, perr := tunnel.Probe(ctx, sess, tunnel.ProbeAddrs(cbHost, config.TunnelPortRange()), false)
					if perr != nil {
						return actool.Errorf("auto の方式選定プローブに失敗（adapter=chisel|suo5 を明示すればスキップ可能）：" + perr.Error()), nil
					}
					if concl := tunnel.Conclude(rep); concl.Grade != tunnel.GradeTCPEgress {
						adapter = tunnel.AdapterSuo5
						log.Printf("[tunnel] auto 選定：probe の段階 %s（ターゲットは TCP 外向き不可）→ adapter=suo5", concl.Grade)
					}
				}
			}
			ri := agent.RunInfoFrom(ctx)
			opts := &tunnel.PlanOpts{
				TaskID:       ri.TaskID,
				ViaSessionID: a.SessionID,
				CallbackHost: cbHost,
				DataDir:      s.tunnels.PlanDataDir(),
			}
			opts.PortMin, opts.PortMax, err = tunnel.ParsePortRange(config.TunnelPortRange())
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			used, err := s.tunnels.OccupiedPorts(ctx)
			if err != nil {
				return actool.Errorf("ポート使用状況の集計に失敗しました：" + err.Error()), nil
			}
			opts.UsedPorts = used
			opts.CanListen = tunnel.CanListen
			var roeWarn string
			var plan *tunnel.Plan
			if adapter == tunnel.AdapterSuo5 {
				shellURL := s.sessionShellURL(ctx, a.SessionID)
				if shellURL == "" {
					return actool.Errorf(fmt.Sprintf("セッション %d の webshell URL が取得できません（suo5 payload は同じディレクトリに書く必要があります）、suo5 を配備できません", a.SessionID)), nil
				}
				plan, err = tunnel.PlanSuo5(&tunnel.Suo5PlanOpts{
					TaskID:       ri.TaskID,
					ViaSessionID: a.SessionID,
					ShellURL:     shellURL,
					SessionKind:  sess.Kind(),
					VerifyAddr:   s.sessionVerifyAddr(ctx, a.SessionID),
					PortMin:      opts.PortMin,
					PortMax:      opts.PortMax,
					UsedPorts:    opts.UsedPorts,
					CanListen:    opts.CanListen,
					DataDir:      opts.DataDir,
				})
			} else {
				switch kind {
				case tunnel.KindSocks:
					opts.VerifyAddr = s.sessionVerifyAddr(ctx, a.SessionID)
					plan, err = tunnel.PlanSocks(opts)
				case tunnel.KindPortfwd:
					host, portStr, perr := net.SplitHostPort(strings.TrimSpace(a.Target))
					if perr != nil || host == "" {
						return actool.Errorf("portfwd には target=host:port が必要です（例 10.0.0.8:3306)"), nil
					}
					port, perr := strconv.Atoi(portStr)
					if perr != nil || port <= 0 {
						return actool.Errorf("target のポートが不正です： " + portStr), nil
					}
					warn, roeErr := s.roeCheckHost(ctx, host)
					if roeErr != nil {
						return actool.Errorf(roeErr.Error()), nil
					}
					roeWarn = warn
					opts.TargetHost, opts.TargetPort = host, port
					plan, err = tunnel.PlanPortfwd(opts)
				default:
					return actool.Errorf("kind は必須です：socks | portfwd"), nil
				}
			}
			if err != nil {
				return actool.Errorf("計画の生成に失敗しました：" + err.Error()), nil
			}
			rec, err := s.tunnels.Deploy(ctx, plan)
			if err != nil {
				return actool.Errorf("配備に失敗しました（段階的にロールバック済み）：" + err.Error()), nil
			}
			out := map[string]any{
				"tunnel_id": rec.ID,
				"kind":      rec.Kind,
				"adapter":   rec.Adapter,
				"state":     rec.State,
				"note": "トンネルは管理対象の長寿命リソースです：使い終わったら tunnel_teardown で回収してください；切断されるとプラットフォームが自動で再起動します（上限 3 回）。" +
					"auth token は台帳の deploy_params にのみ保存します。",
			}
			if rec.Kind == tunnel.KindSocks {
				addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(plan.SocksPort))
				out["socks_addr"] = addr
				out["socks_url"] = "socks5://" + addr // フェーズ 3b でタスクの proxyEnv がこの形式を直接消費する
				if rec.Adapter == tunnel.AdapterSuo5 {
					// suo5 の入口は --auth 認証付き:消費側は必ずパスワードを付ける（台帳の deploy_params にあり、プラットフォーム内部での消費用）。
					out["socks_url"] = "socks5://" + plan.AuthUser + ":" + plan.AuthPass + "@" + addr
					out["payload_url"] = plan.PayloadURL
				}
			} else {
				out["forward_addr"] = net.JoinHostPort("127.0.0.1", strconv.Itoa(plan.LocalPort))
				out["forward_to"] = net.JoinHostPort(plan.TargetHost, strconv.Itoa(plan.TargetPort))
			}
			if roeWarn != "" {
				out["roe_warning"] = roeWarn
			}
			return jsonResult(out)
		},
	})
}

// sessionShellURL はフットホールドセッションの webshell URL を取得する（suo5 payload を同ディレクトリに置くため）。
func (s *Server) sessionShellURL(ctx context.Context, sessionID int64) string {
	if s.sessStore == nil {
		return ""
	}
	rec, err := s.sessStore.Get(ctx, sessionID)
	if err != nil || rec == nil {
		return ""
	}
	return strings.TrimSpace(rec.URL)
}

// tunnelListTool はトンネル台帳を一覧する（4 点セットの health の可視面）。
func (s *Server) tunnelListTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "tunnel_list",
		Description: "【トンネル 4 点セット 3/4】本プラットフォームのトンネル台帳を一覧する：id/種別/状態（deploying/alive/error/stopped)/" +
			"入口ポート/ターゲット/経由セッション/エラーメッセージ/最終ヘルスチェック時刻。タスク内ではデフォルトで本タスクの分だけを一覧し、タスク実行外では未終了のものをすべて一覧する。",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			ri := agent.RunInfoFrom(ctx)
			var recs []*db.TunnelRecord
			var err error
			if ri.TaskID > 0 {
				recs, err = s.tunnels.TaskRecords(ctx, ri.TaskID)
			} else {
				recs, err = s.tunnels.ActiveRecords(ctx)
			}
			if err != nil {
				return actool.Errorf("台帳の読み取りに失敗しました：" + err.Error()), nil
			}
			out := []map[string]any{}
			for _, r := range recs {
				var p tunnel.Plan
				_ = json.Unmarshal(r.DeployParams, &p)
				row := map[string]any{
					"tunnel_id":      r.ID,
					"kind":           r.Kind,
					"adapter":        r.Adapter,
					"state":          r.State,
					"listen_port":    r.ListenPort,
					"via_session_id": r.ViaSessionID,
					"last_check":     r.LastCheck.Format("2006-01-02 15:04:05"),
				}
				if r.Kind == tunnel.KindSocks && p.SocksPort > 0 {
					row["socks_addr"] = net.JoinHostPort("127.0.0.1", strconv.Itoa(p.SocksPort))
				}
				if r.Kind == tunnel.KindPortfwd {
					row["forward_addr"] = net.JoinHostPort("127.0.0.1", strconv.Itoa(p.LocalPort))
					row["target"] = net.JoinHostPort(r.TargetHost, strconv.Itoa(r.TargetPort))
				}
				if r.Error != "" {
					row["error"] = r.Error
				}
				out = append(out, row)
			}
			return jsonResult(map[string]any{"tunnels": out, "count": len(out)})
		},
	})
}

// tunnelTeardownTool はトンネルを回収する（4 点セットの管理回収）。
func (s *Server) tunnelTeardownTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "tunnel_teardown",
		Description: "【トンネル 4 点セット 4/4】トンネルを 1 本回収する：セッション経由でターゲット側の client プロセスを停止（リモートを確認し、コマンドラインを照合して誤停止を防ぐ）→ " +
			"プラットフォーム側の server を停止 → stage 配信エントリを削除 → authfile を削除 → 台帳を stopped に。トンネルを使い終わったら必ずこれを呼ぶ；" +
			"トンネルを放置せず、裸の Bash で自分でトンネルプロセスを停止/起動しない。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tunnel_id": map[string]any{"type": "integer", "description": "tunnel_deploy/tunnel_list が返すトンネル id"},
			},
			"required": []any{"tunnel_id"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				TunnelID int64 `json:"tunnel_id"`
			}
			_ = json.Unmarshal(in, &a)
			if a.TunnelID <= 0 {
				return actool.Errorf("tunnel_id は必須で 0 より大きい値"), nil
			}
			if err := s.tunnels.Teardown(ctx, a.TunnelID); err != nil {
				return actool.Errorf("回収に失敗しました：" + err.Error()), nil
			}
			return jsonResult(map[string]any{"tunnel_id": a.TunnelID, "state": db.TunnelStopped})
		},
	})
}

// seedTunnelTools はトンネル 4 ツールを tools テーブルに seed し、デフォルトで worker に紐付ける
// （sessionTools と同じ SeedTool の初回挿入の意味論：既存 DB のユーザー編集は上書きされない）。
func (s *Server) seedTunnelTools() {
	if s.tunnels == nil {
		return
	}
	workerAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range s.tunnelTools() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, workerAgents); err != nil {
			log.Printf("[tools] seed %s に失敗しました： %v", t.Name(), err)
		}
	}
}
