package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/honeydetect"
	actool "github.com/Autumn-27/norma/tool"
)

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- service 任意: プロトコル banner / HTTP Server ヘッダ（バッチ6 L1 のハニーポット静的署名識別用、DB へは保存しない） ----
	Banner string `json:"banner"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"新たに発見したアセットを一括登録します。1 回で複数種別を混在できます（type は列挙を参照）。\n"+
			"各種別の必須フィールド：root_domain→domain；ip→ip（IPv4/IPv6 であること。ホスト名不可）；subdomain→domain；app→app_name；service(HTTP)→url；service(HTTP 以外)→service_name+port（ip/domain の少なくとも一方）；endpoint→url+method。その他のフィールドの意味は各説明を参照。\n"+
			"auth/technologies/params は追加マージ(append)で、元の値を上書きしません。\n"+
			"戻り値：{results:[{index,id,type}], errors:[{index,error}]}",
		obj(map[string]any{
			// task_id はモデルに公開しない: worker がどの task に属するかはプログラムが SetTaskID で
			// 正式に代入する（handler 参照）。
			"assets": map[string]any{
				"type":        "array",
				"description": "アセットの配列。各要素が 1 件のアセットレコードに対応します",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "アセットの種類",
					},
					// root_domain / subdomain
					"domain":      str("ルートドメインまたはサブドメイン（root_domain/subdomain では必須）"),
					"icp":         str("ICP 登録番号（任意）"),
					"record_type": str("DNS レコード種別：A/AAAA/CNAME/MX など（subdomain では任意）"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS レコード値の配列（subdomain では任意。例 [\"1.2.3.4\",\"2.3.4.5\"]）",
					},
					// ip
					"ip": str("IP アドレス。IPv4/IPv6 であること。ホスト名は不可（ホスト名は type=subdomain の domain フィールドを使う）。ip 種別では必須。service/endpoint 種別でも指定可で、IP の関連付けに使う"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "この IP に紐付くドメインの配列（ip 種別では任意）",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "開放ポートの配列（ip 種別では任意）",
						"items": obj(map[string]any{
							"port":    intp("ポート番号"),
							"service": str("サービス名（例 http/ssh/mysql など、任意）"),
						}, "port"),
					},
					// app
					"app_name":    str("アプリケーション名（app 種別では必須）"),
					"bundle_id":   str("Bundle ID（app 種別では任意）"),
					"category":    str("アプリケーションのカテゴリ（任意）"),
					"description": str("アプリケーションの説明（任意）"),
					"app_icp":     str("アプリケーションの ICP 登録（任意）"),
					"company_id":  intp("帰属企業の id（app 種別では任意。app は scope による自動帰属ができないため明示指定が必要。id は add_company_scope が返す）"),
					// service (http)
					"url":         str("プロトコルとポートを含む完全な URL（HTTP サービスでは必須。service_type は自動で http になる）"),
					"status_code": intp("HTTP レスポンスのステータスコード（例 200/301/403/404、任意）"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP レスポンスボディのバイト数（任意）",
					},
					"page_title":   str("ページの <title> 内容（任意）"),
					"favicon_mmh3": str("favicon の MMH3 ハッシュ（任意）"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "フィンガープリント/技術スタックの配列（例 [\"Nginx\",\"Vue\",\"Bootstrap\"]、任意）",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "発見した認証情報の配列。各要素に type/username/password などのフィールド（任意、追加のみで上書きしない）",
						"items":       map[string]any{"type": "object"},
					},
					// service（その他、HTTP 以外）
					"service_name": str("サービス名（例 ssh/mysql/redis。service が HTTP 以外のとき必須）"),
					"port":         intp("ポート番号（service が HTTP 以外のとき必須）"),
					// service 任意（HTTP でも HTTP 以外でも可）
					"banner": str("サービスの banner、または HTTP Server レスポンスヘッダの原文（任意。例 \"SSH-2.0-OpenSSH_6.0p1 Debian-4+deb7u2\"。ハニーポットの静的シグネチャ識別に使い、偵察時に見かけたら付ける）"),
					// endpoint
					"method": str("HTTP メソッド：GET/POST/PUT/PATCH/DELETE など（endpoint では必須）"),
					"params": map[string]any{
						"type":        "array",
						"description": "リクエストパラメータの配列。各要素に location(query/body/header/path)/name/value/type（任意、追加のみで上書きしない）",
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets は無効です: AssetStore が未初期化です"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id はプログラムが正式に代入する(worker: SetTaskID)。モデルからの指定は受け付けない——
			// モデルの未指定・誤指定でアセットがタスクに紐付かない/誤ったタスクに紐付くのを防ぐ。
			// タスクコンテキストのない呼び出し元(auto/pentest/chat)では t.taskID=0 になる。
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			for i, item := range a.Assets {
				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
						if err == nil {
							// バッチ6 L1: 保存後にハニーポット静的署名を実行（title/banner。httpx の書き戻しも同じ経路）。
							t.evaluateHoneypot(id, honeydetect.ServiceView{
								Port: urlPort(item.URL), Banner: item.Banner, Title: item.PageTitle,
							})
						}
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
						if err == nil {
							// バッチ6 L1: 保存後にハニーポット静的署名を実行（port/banner）。
							t.evaluateHoneypot(id, honeydetect.ServiceView{
								Port: item.Port, Banner: item.Banner,
							})
						}
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent が insert_assets で登録"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker の意図 #%d が insert_assets で登録", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// テスト範囲(source='auto')へ自動追加: worker 最上位で明示的に挿入されたこの項目だけを、
				// その型に応じて保守的に範囲へ加える。副作用で派生したアセットはここを通らないため範囲は
				// 無闇に広がらない。taskID=0 のときは何もしない。
				// カバレッジのスイッチとは無関係: task_scope はタスクの範囲境界(list/検索の絞り込み基準)であり、
				// カバレッジのスイッチはそれを分母に指標を計算するかどうかだけを決め、範囲自体の累積は決めない。
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"ドメイン/IP/CIDR/ICP 登録/企業キーワードを、ある企業の【アセット範囲】に追加します。ドメイン・ネットワーク・ICP は一致したアセットを自動で取り込み、キーワードは範囲のヒントとして Agent に渡すだけです。\n"+
			"企業名は一意です。company が存在しなければ新規作成し、存在すれば再利用します（範囲をマージするだけ）。\n"+
			"scope は 1 行 1 件で、システムが自動判別します：ルートドメイン / URL / 単一 IP / CIDR ネットワーク / ICP 登録 / 企業キーワード。\n"+
			"reason には帰属の根拠（whois/証明書/ASN など）を必ず書いてください。\n"+
			"ガードレール：裸の TLD と広すぎるネットワーク（IPv4 プレフィックスは /16〜/32、IPv6 プレフィックスは /32〜/128）は拒否します。不正な行はスキップされ errors で返ります。",
		obj(map[string]any{
			"company": str("企業名（存在しなければ新規作成、存在すれば再利用。名前は一意）"),
			"scope":   str("アセット範囲。1 行 1 件：ドメイン / URL / IP / CIDR / ICP 登録 / 企業キーワード"),
			"reason":  str("帰属の根拠（エビデンス/ソース）。必ず記入"),
			"logo":    str("企業ロゴの URL（任意。新規作成時のみ有効）"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope は無効です: CompanyStore が未初期化です"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company を指定してください"), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("企業の作成/取得に失敗しました: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"テスト範囲を【本タスク】に追加します。これは本タスクの認可境界であり、アセットのテストカバレッジの分母でもあります。\n"+
			"kind は company(企業配下のアセット全体) / root_domain(ルートドメイン全体、全サブドメインを含む) / subdomain(単一のサブドメイン) / ip / cidr / icp / keyword に対応します。\n"+
			"補足：worker が個別に遭遇したホストはシステムが【自動】で範囲に追加します(サブドメイン単位)。本ツールは【能動的な拡大】用です——ルートドメイン全体/企業全体を取り込む、または特定のサブドメイン/IP を補足指定します。\n"+
			"value：company は企業名または id(企業が既存であること)、root_domain/subdomain はドメイン、ip/cidr は IP またはネットワーク、icp/keyword は登録番号または企業キーワード。\n"+
			"reason には根拠を必ず書いてください(監査可能)。複数件は entries 配列を使います。",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "一括：[{kind, value}]。kind∈company/root_domain/subdomain/ip/cidr/icp/keyword。", "items": map[string]any{"type": "object"}},
			"kind":    str("[単条] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[単条] 企業名または id / ドメイン / IP / CIDR / ICP / キーワード"),
			"reason":  str("追加の根拠(監査用)。必ず記入"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope は無効です: AssetStore が未初期化です"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope にはタスクコンテキストが必要です（現在タスクなし）"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // 単一指定モード
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"【本タスクおよび直接関連タスク】の範囲内で、まだ事実のアンカーに覆われていないアセットを検索します（関連範囲は読み取り専用。補測の要否を自身で判断するための材料で、判断を代行するものではありません）。\n"+
			"任意でアセット種別の絞り込みが可能：root_domain/subdomain/service/app/endpoint/ip。\n"+
			"ページング：page は 1 始まり、page_size のデフォルトは 10。{assets:[{id,type,label}], total, page, page_size} を返します。タスクコンテキストでのみ利用可。",
		obj(map[string]any{
			"type":      str("アセット種別の絞り込み（任意）：root_domain/subdomain/service/app/endpoint/ip"),
			"page":      intp("ページ番号（1 始まり、デフォルト 1）"),
			"page_size": intp("1 ページあたりの件数（デフォルト 10）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets は無効です: AssetStore が未初期化です"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets にはタスクコンテキストが必要です"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"アセットライブラリを検索します：DSL 式の検索、または id/ids による直接取得。ページング対応。【本タスクおよび直接関連タスク】のテスト範囲内のアセットだけを返します。\n"+
			"DSL：field=value 部分一致(ILIKE) | field==value 完全一致 | field!=value 除外 | 数値フィールドは > >= < <= に対応 | 裸の語=全文部分一致；AND/OR で組み合わせ(AND が優先)、括弧でグループ化可。アセット種別は独立した type パラメータで指定し、DSL には書かない。\n"+
			"id/ids を渡さない場合、dsl は必須です（条件なしの全件検索は不可）。\n"+
			"利用可能なフィールド：domain(ルート/サブ/サービスドメイン)、root_domain、ip、url、page_title、icp、service_name、app_name、method(例 GET/POST)、service_type(http|other)、record_type(例 A/CNAME)、technology(配列、=部分一致 ==完全一致)、port/status_code/company_id(整数)。\n"+
			"例：status_code>=400 AND technology=shiro ；(port==80 OR port==443) AND technology=nginx",
		obj(map[string]any{
			"dsl":    str(`DSL 検索式（構文とフィールドはツール説明を参照）。id/ids を渡さない場合は必須。`),
			"type":   str("アセット種別の絞り込み：root_domain|ip|subdomain|app|service|endpoint（独立フィールドで dsl と併用可。type だけでは検索できず、dsl が必要）"),
			"id":     intp("単一のアセット id で直接取得（任意、dsl/type と排他）"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "複数のアセット id で直接取得（任意、dsl/type と排他）"},
			"limit":  intp("取得件数の上限（デフォルト 10、任意）"),
			"offset": intp("ページングのオフセット（デフォルト 0、任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets は無効です: AssetStore が未初期化です"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("id/ids を指定しない場合 dsl は必須です：条件なしで全アセットを検索することはできません。検索条件を指定してください"), nil
			}
			if err != nil {
				return actool.Errorf("DSL の誤り: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"アセットライブラリ内の【企業/会社】とそのアセット範囲(scope)、帰属済みアセット数を一覧表示します。企業の確認や、"+
			"company_id の取得（insert_assets で app を紐付けるとき、list_assets を company_id で絞り込むときに使う）に利用します。"+
			"任意で search による企業名の部分一致絞り込み(大文字小文字を区別しない)が可能。空なら全件を返します。",
		obj(map[string]any{
			"search": str("企業名の部分一致絞り込み(任意、大文字小文字を区別しない)。空なら全件を返す"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies は無効です: CompanyStore が未初期化です"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("企業の取得に失敗しました: " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings は維持: 脆弱性を報告する前に本タスクで確認済みの脆弱性を調べ、同じ脆弱性の重複報告を避ける。
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// confirm_fact: worker が確立したツールで降格された陰性結論を再検証した後、confirmed へ格上げする。
		t.confirmFact(),
		// asset management (handlers guard nil store internally)。
		// add_company_scope は worker に渡さない: 企業のアセット範囲の定義は計画/メイン制御/Auto の役割で、worker は探索を実行するだけ。
		t.insertAssets(), t.listAssets(),
		// 以下のツールは worker には渡さず planner/main だけに残す（コンテキストの把握や work を
		// 跨いだ振り返りは計画の役割で、worker は単一の意図の実行と書き戻しだけを行う）:
		// list_facts / node_detail / list_companies / work を跨ぐ検索
		// search_all_worker_traces / list_worker_traces / get_worker_trace。
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), t.expandIndex(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work: 人は実行中の意図(work)へオンラインで軌道修正の指示を注入できる（中断せず、進捗も失わない）。
		t.steerWorkTool(),
		// kill_work: 人は実行中の意図(work)を即座に終了できる。説明文は DecorateTool で書き換えられ、
		// steer_work(中断しない)/cancel_intent(open のみ無効化) との境界を明確にしている。handler は
		// 不変で、engine のコールバックには killed_by_mainagent という名前付きの理由が付く(server 側の配線)。
		DecorateTool(t.killWorkTool(), "【即時終了】実行中(running)の意図(work)を 1 件終了します：worker は即座に停止し、意図は stopped になり自動で再受領されません。方向が逸脱した、継続する価値がないと人が判断したときの損切りに使います。steer_work との違い：steer_work は中断せず、軌道修正の指示を注入するだけ。cancel_intent との違い：cancel_intent は【未開始(open)】の意図だけを無効化し、kill_work は【実行中】のものを終了します。まず get_worker_output で何をしているか確認してから決めてください。", nil),
		// cancel_intent: 人は frontier 内の未開始(open)の滞留意図を無効化できる（理由はグラフに残る）。
		t.cancelIntentTool(),
		// set_goals: 人は実行中に本タスクへ新たな最終ゴールを追加できる（計画者はこれに基づき達成を再判定する）。
		t.setGoals(),
		// set_constraints: 人は実行中に本タスクの操作制約(allow/deny)を追加・変更でき、planner/worker の探索境界を拘束する。
		t.setConstraints(),
		// asset management (handlers guard nil store internally)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets: 本タスク範囲内の未テストアセットをオンデマンドで調べ(型+ページング)、追加テストは自分で判断する。
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
