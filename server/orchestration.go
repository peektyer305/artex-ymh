package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// 本ファイルは P2「クロスタスク編成ツールセット」(docs/跑分编排 §2 P2)を実装する。これらは host ツールで、
// Manager(任意のタスクの Store)、Engine(一時停止)、タスク作成フローへのアクセスが必要なため server 層に置く。
// 読み取り系ツールは「既存の per-task ツール」を対象タスクの store 上へリダイレクトして実行する(一時 ToolSet を
// 作って対応するツールを Call する)ことで、まったく同じロジックを再利用する。制御系(spawn/pause)は Manager/Engine を直接呼ぶ。
// tools テーブルへはトラフィックツールと同様に seed され、agent 単位でバインドされる(編成 agent にのみ見える)。

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.findingCheckTools()...) // 反証検証(verifier)host ツール
	tools = append(tools, s.platformTools()...)     // プラットフォーム操作ツール(skill/ツール/MCP の作成・変更、Auto 用)
	if s.stage != nil {
		tools = append(tools, s.stageShareTool()) // 管理ファイル配信(F13)、デフォルトで worker にバインド
	}
	tools = append(tools, s.sessionTools()...)    // フットホールドセッション用ツール(第 1a 期)、デフォルトで worker にバインド
	tools = append(tools, s.credentialTools()...) // 認証情報ツール(第 2 期)、デフォルトで worker にバインド
	tools = append(tools, s.tunnelTools()...)     // トンネル 4 点セット(第 3a 期)、デフォルトで worker にバインド
	tools = append(tools, s.reverseTools()...)    // リバースリスナー(第 5 期)、デフォルトで worker にバインド
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 読み込みに失敗しました: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id は必須です"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task が存在しません: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify) // hint writes wake this task's planner (no-op for read tools)
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"すべてのタスク(id/説明/ゴール/ステータス/実行時間/親タスク/LLM 設定)を一覧する。編成 agent はこれで全体を把握し、どのタスクが長時間止まっているか、それぞれがどの LLM を使っているかを確認する。実行時間：実行中=作成→現在、終了状態=作成→最終アクティビティ(秒)。llm_profile：タスクの planner/worker が使う設定名。(有効な設定)=グローバルで有効な設定に従う。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(有効な設定)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(削除済み)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"利用可能な LLM 設定(profile)を一覧する：id、名前、モデル、形式、現在有効な設定かどうか。id を spawn_task の llm_profile_id パラメータに渡すと、サブタスク専用の LLM を指定できる（偵察には安価なモデル、エクスプロイトには強力なモデルなど）。API Key は含まない。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"サブタスクを新規作成して探索エンジンを起動し、task_id を返す。1 つの事柄(1 問/1 ゴールなど)を独立したタスクとして割り振るときに使う。parent_ref は任意：現在の編成が関連する親タスク id を入れると親子関連を張る。",
		objSchema(map[string]any{
			"description":            strParam("タスクの説明(短いタイトル)"),
			"goal":                   strParam("タスクのゴール(何を達成するか)"),
			"parent_ref":             strParam("任意：親タスク id(親子関連を張る)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("任意：読み取り専用で継承するソースタスク id のリスト(最大 %d 個)。サブタスクはこれらのタスクで判明済みのアセット/結論を起点として読み取り専用で参照できる。parent_ref の純粋な親子ポインタとは異なり、こちらは内容の継承。", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "任意：このサブタスクの planner/worker が使う LLM 設定 id を指定する(list_llm_profiles を参照)。空なら親タスクを継承し、それも無ければグローバルで有効な設定にフォールバックする"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "任意：タスク単位のタイムアウト(秒)。到達すると正常終了処理を起動して timeout の終了状態に入る。空または 0 = 無制限"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "任意：planner のハートビート間隔(秒)。前回の計画終了/タスク開始からこの値が経過し、その間トリガーが無い → 計画を 1 ラウンド起動する(デッドロックの保険 + 実行中の worker を監督させる)。空または 0 = デフォルト 600(10min)。"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "任意：単純なタスクでは有効にできる。作成時にシード意図を 1 件直接発行し(内容=説明+ゴール)、worker が初回 planner を待たずにテストを開始できる。デフォルト false(標準の「まず計画、次に実行」)。"},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "名称未設定のタスク"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal は必須です"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 読み取り専用で継承するソースタスク：件数の上限 + 各 id の有効性/重複/存在を確認する。検証ルールは HTTP のタスク作成と同一。
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("関連タスクは最大 %d 個まで選択できます", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("関連タスク id が不正か重複しています"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("関連タスク #%d が存在しません", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM 設定 #%d が存在しないか API Key が未設定です", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// 共有の作成後フロー。HTTP のタスク作成(server.go createTask)と同じ launchTask を再利用する：
			// seed + バックグラウンドで可視的にゴール分解(第 0 ラウンド/LLM ステップ/ゴールごと) + engine.Run。
			// seed_first_intent はデフォルト false(標準の「まず計画、次に実行」)。単純なタスクでは有効にして work を 1 件直接発行できる。
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "指定したタスクを一時停止する(planner/worker のループを停止)。",
		objSchema(map[string]any{"task_id": strParam("一時停止するタスク id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task が存在しません: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "指定したタスクの探索グラフの全体像を読む(graph_overview と同じ：アセット数/frontier/ファインディング/カバレッジなど)。task_id でタスクを指定する。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "指定したタスクの確認済み脆弱性を読む(flag/PoC を含む。各件は id/task_id/intent_id/vulnclass/severity/要約/ステータスを持つ)。task_id でタスクを指定する。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "指定したタスクに戦略ヒントを注入する(そのタスクの planner が次ラウンドの意図生成時に読む)。\n"+
		"★バッチを優先：複数のヒントは hints 配列に入れて一度に送信する（ids 配列を返し、hints と同じ長さ・同じ順序。失敗した項目の id=0）。1 件だけなら hints を省略して最上位の text に直接入れる。",
		objSchema(map[string]any{
			"task_id":      strParam("タスク id"),
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先】ヒント配列。各要素のフィールドは最上位と同じ（text/asset_ids/traffic_refs）。", "items": objSchema(map[string]any{"text": strParam("プロンプト本文"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[単件] プロンプト本文"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "アンカーするアセット id（任意、0/1/複数。そのタスク内のアセット id）"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"指定したタスク内の特定の work(意図)の実行過程を見る：get_task_worker_trace(task_id, intent_id) でステップの要約を見て、さらに step_ids=[...] を付けるとそのステップの内容全体を取得する(1 回に最大 5 個。多く渡すと先頭 5 個のみ返す)。",
		objSchema(map[string]any{
			"task_id":   strParam("タスク id"),
			"intent_id": map[string]any{"type": "integer", "description": "意図 id(そのタスク内の work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "任意：内容全体を取得するステップ id(1 回に最大 5 個。多く渡すと先頭 5 個のみ返し、残りは omitted_step_ids に列挙される)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "指定したタスクで実行された work(意図)と各ステップ数を一覧する。どの work を読む価値があるかの把握に使い、その後 get_task_worker_trace を使う。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "指定したタスク内で全 work の実行過程をキーワードで検索する(ヒットしたステップの要約 + intent_id を返す)。",
		objSchema(map[string]any{"task_id": strParam("タスク id"), "q": strParam("検索キーワード")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"指定したタスク内の特定の探索グラフノードの内容全体を読む(ファインディング/事実/意図/ゴール：要約 + 詳細/エビデンス/PoC)。id は探索ノード id(report_finding が返す値、または list_task_findings の id)。脆弱性レポートを書く前にこれでその脆弱性のエビデンス全体を取得する。",
		objSchema(map[string]any{
			"task_id": strParam("タスク id"),
			"id":      map[string]any{"type": "integer", "description": "探索グラフのノード id(アセット id ではない)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"登録済みの脆弱性に【詳細レポート】を書き込む/更新する(Markdown 全文。旧内容を全面的に上書き)。finding_id には report_finding が返した id(\"finding recorded: <id>\" の数字)を渡す。レポートには、脆弱性の概要、影響と被害、再現手順、エビデンス/PoC、修正提案を含めることを推奨する。",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "対象の脆弱性 id(report_finding が返す id)"},
			"report":           strParam("詳細レポート全文、Markdown 形式"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic が返すエビデンスの version。新しいエビデンス変更をレポートが上書きするのを防ぐ"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 「数値または数値文字列」の解析を再利用
			if nodeID <= 0 {
				return actool.Errorf("finding_id が不正です"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("finding_id=%d に対応する脆弱性レコードが見つかりません(まず report_finding で登録してください)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (元々プラットフォーム
	// 操作に使う)。SeedTool は初回挿入のみ有効で、既存 DB の seed 済み行は seedAutoDefaultBindings が補完バインドする。
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/node_detail/list_companies/クロス work 検索を worker からデフォルトでバインド解除(1 回のみ)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals プロンプトに「操作制約の抽出」ステップを追加 → 既存 DB に新デフォルトを 1 版追加(1 回のみ)
	s.reseedMainAgentPrompt()         // mainagent プロンプトに「ゴール達成後の add_intent で目標登録の要否を問い返す」を追加(1 回のみ)
	s.reseedPlannerPrompt()           // planner プロンプト：「意図 0 件」の正当理由の書き直し + 定量的な受け入れ確認を追加(1 回のみ)
	s.reseedWorkerPrompt()            // worker プロンプト：否定結論のエビデンス要件を追加(1 回のみ)
	s.seedReporterAgent()             // 「レポート作成」agent + ツールバインド + finding トリガーをプリセット(1 回のみ)
	s.upgradeReporterTriggerMessage() // 既存 DB の補完マイグレーション：reporter に evidence_version を返させる(1 回のみ)
	s.seedFindingTrafficTools()       // 任意のエビデンスパラメータと読み取り専用エビデンスツールを追加、ユーザー設定は維持
	s.seedFindingWorkflowTools()
	// 注：pentest のデフォルトツールバインドはマイグレーション不要 —— BuiltinToolSeeds が新規初期化時に
	// list_assets/insert_assets/report_finding/list_findings/list_companies を
	// pentest と一緒に seed 済み（旧 DB は無いため、マイグレーションは行わない）。
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task's llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// あわせて一部の組み込み agent ツールもコード既定へ更新する：
	//   - goal_met：既存 DB の seed 済み説明が「今ラウンドの計画を終了」という誤解を招く文言で、
	//     planner がこれを「空ラウンドを終わらせる」手段と受け取り、開始直後にタスク全体の完了と誤判定する。
	//   - insert_assets：related 入力(アセットが現在のタスクに関係するか、カバレッジに入れるかを示す)を追加したが、
	//     SeedTool は初回挿入のみのため、既存 DB の seed 済み schema はこの新パラメータを受け取れない。
	//   - list_facts：ページングに変更し limit/before/q 入力を追加したが、既存 DB の seed 済み空 schema では
	//     ツール管理ページに「パラメータなし」と表示され、モデルもこれらの説明を得られない。
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform ツールの schema をコード既定へ更新しました(1 回のみ)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met を planner からバインド解除できませんでした: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt は goals ゴール分解器のプロンプトを【現在のコード既定】へ更新する —— 既定本文に
// 「先に操作制約(set_constraints)を抽出してからゴールを分解する」ステップが増えたが、SeedPromptIfEmpty は初回挿入のみで、
// 既存 DB の version 1 はこのステップを受け取れないため。ここではバージョン管理で【新しいバージョンを追加】して切り替える(ResetPromptToDefault)。
// 旧バージョンは履歴に残るので、ユーザーがカスタマイズしていた場合はバージョン履歴から復元できる。settings flag でガード → 1 回だけ。
// 以後また既定が変わったらこの flag を bump する。新規 DB では不要(SeedPromptIfEmpty が最新の既定を seed 済み)。
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず 1 回だけ試行する
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 新規 DB で agent 行が未作成の場合、seedPrompts が最新の既定を直接 seed するためこのマイグレーションは不要
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 新規 DB では seedPrompts が最新の既定を seed 済み → 現在のバージョンがすでにコード既定と等しいため、重複バージョンは追加しない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals プロンプトを新しい既定へ更新できませんでした: %v", err)
		return
	}
	log.Printf("[prompts] goals プロンプトに新しい既定バージョンを追加しました(操作制約の抽出ステップを追加、1 回のみ)")
}

// reseedMainAgentPrompt は mainagent プロンプトを【現在のコード既定】へ更新する —— 既定本文に「ゴールが全部
// 達成された後に add_intent で意図を直接投入するとき、人に正式なゴールとして登録するか問い返す」誘導が増えたが、SeedPromptIfEmpty は
// 初回挿入のみで、既存 DB のバージョンは受け取れない。バージョン管理で【新しいバージョンを追加】して切り替え(ResetPromptToDefault)、旧バージョンは
// 履歴に残るので、ユーザーがカスタマイズしていた場合はバージョン履歴から復元できる。settings flag でガード → 1 回だけ。新規 DB では不要
// (SeedPromptIfEmpty が最新の既定を seed 済み)。reseedGoalsPrompt と同じ構造。
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず 1 回だけ試行する
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 新規 DB で agent 行が未作成の場合、seedPrompts が最新の既定を直接 seed するためこのマイグレーションは不要
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 新規 DB では seedPrompts が最新の既定を seed 済み → 現在のバージョンがすでにコード既定と等しいため、重複バージョンは追加しない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent プロンプトを新しい既定へ更新できませんでした: %v", err)
		return
	}
	log.Printf("[prompts] mainagent プロンプトに新しい既定バージョンを追加しました(ゴール達成後の目標登録の問い返しを追加、1 回のみ)")
}

// reseedPlannerPrompt は planner プロンプトを【現在のコード既定】へ更新する —— 既定本文を簡潔に再構成し、「抑制」を
// 重複排除のみに格下げ、「深さをカバレッジより優先」「ハードルール：ゴール未達成で実行中の意図が無ければ必ず産出する」を追加、否定結論の再確認に上限を設けた。
// 既定に実質的な変更があるたびに下の flag(現在 v2)を bump し、既存 DB にもう一度適用する。SeedPromptIfEmpty は初回挿入のみで既存 DB のバージョンは受け取れないため、
// バージョン管理で【新しいバージョンを追加】して切り替え(ResetPromptToDefault)、旧バージョンは履歴に残るので、ユーザーがカスタマイズしていた場合はバージョン履歴から
// 復元できる。settings flag でガード → 1 回だけ。新規 DB では不要(SeedPromptIfEmpty が最新の既定を seed 済み)。
// reseedGoalsPrompt と同じ構造。
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず 1 回だけ試行する
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 新規 DB で agent 行が未作成の場合、seedPrompts が最新の既定を直接 seed するためこのマイグレーションは不要
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 新規 DB では seedPrompts が最新の既定を seed 済み → 現在のバージョンがすでにコード既定と等しいため、重複バージョンは追加しない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner プロンプトを新しい既定へ更新できませんでした: %v", err)
		return
	}
	log.Printf("[prompts] planner プロンプトに新しい既定バージョンを追加しました(簡潔化+抑制の重複排除化+深さ優先+否定再確認の上限、1 回のみ)")
}

// reseedWorkerPrompt は worker プロンプトを【現在のコード既定】へ更新する —— 既定本文の record_fact 段から「否定系の結論は
// 観察+試探的な読み方で書く」という一文を削り、confidence(observed/inferred)と「この意図の手段を出し切ったか」を切り離した(これらは planner を誤らせやすい)。
// あわせて facts 配列の分割を「互いに完全に独立で、統合できない」ごく少数の例外に絞り込んだ。flag を v3 に bump し、既存 DB にもう一度適用する。
// SeedPromptIfEmpty は初回挿入のみで既存 DB のバージョンは受け取れないため、バージョン管理で【新しいバージョンを追加】して切り替える。旧バージョンは履歴に残り復元できる。
// settings flag でガード → 1 回だけ。新規 DB では不要。reseedGoalsPrompt と同じ構造。
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず 1 回だけ試行する
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 新規 DB で agent 行が未作成の場合、seedPrompts が最新の既定を直接 seed するためこのマイグレーションは不要
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 新規 DB では seedPrompts が最新の既定を seed 済み → 現在のバージョンがすでにコード既定と等しいため、重複バージョンは追加しない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker プロンプトを新しい既定へ更新できませんでした: %v", err)
		return
	}
	log.Printf("[prompts] worker プロンプトに新しい既定バージョンを追加しました(コンテキスト参照段を list_assets/list_findings に絞り、list_facts/node_detail/asset_neighbors を削除、1 回のみ)")
}

// reporterToolCallMessage は、必ず先に get_finding_traffic を 1 回読んでから報告を書くよう無条件に要求する。
// このツールは読み取り専用で「キャプチャのスイッチに依存しない」ため、自動バインドのオン/オフにかかわらず手動バインドされたエビデンスを読める。ここを
// 「自動バインドが有効なときだけ読む」と書くと、デフォルトが無効の設定では reporter が evidence_version を渡さず、
// SetFindingReportVersionByNodeID が legacy の意味論どおり -1 を書き込み、脆弱性の詳細と Markdown エクスポートが
// 以後ずっと「エビデンスが変更されました、レポートの更新待ち」のままになり、UI 上にそれを消せる入口が無くなる。
const reporterToolCallMessage = "上面刚有一个漏洞被 report_finding 登记。请读取返回 JSON 的 finding_id（独立漏洞记录 ID）与 finding_node_id（探索节点 ID），" +
	"先用 get_finding_traffic(finding_id) 读取当前证据清单及其 version（空清单是正常情况，照常写报告）；" +
	"若运行指引启用自动绑定，在读取前先核实并关联本次漏洞的流量。节点详情使用 finding_node_id。" +
	"最后调用 update_finding_report(finding_id=finding_node_id, report, evidence_version=实际读取版本) 保存，" +
	"evidence_version 必须传，否则报告会被永久标记为待更新。不要混用两种编号。"

// 旧版のトリガーメッセージ(0.3.8 以前)。これと逐字一致するレコードだけが移行で上書きされ、ユーザーが変更したものはそのまま残る。
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage は、既存 DB でまだ既定文言のままの reporter トリガーメッセージを新バージョンへ更新する。
// seedReporterAgent は reporter_agent_seed_v1 でガードされ、agent 新規作成時にしかトリガーを書かないため、
// アップグレードしてきた DB は新しい文言を受け取れない —— ツール schema は seedFindingTrafficTools が
// evidence_version を補ったが、reporter にそれを使わせるものは何も無い。1 回のみで、未変更の文言だけを上書きする。
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 1 回だけ試行する
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] トリガーの読み込みに失敗しました: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // ユーザーが変更したか、または finding トリガーではない場合は触らない。
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] トリガーメッセージの更新に失敗しました: %v", err)
			return
		}
		log.Printf("[reporter] トリガーメッセージを evidence_version を読み取って返す版へ更新しました")
	}
}

// seedReporterAgent は「レポート作成」カスタム agent をプリセットする(builtin=false、UI で編集/削除可能)：
// update_finding_report + タスク照会ツールをバインドし、「report_finding が呼ばれたら起動」する
// トリガーを付ける —— 脆弱性が登録されるたびに起動して詳細レポートを書かせる。1 回のみ(settings flag でガード)：ユーザーが削除した後は再作成しない。
// 依存：orchestration ツールは本関数より上で SeedTool により登録済みのため、バインドできる。
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず 1 回だけ試行する

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key は既に使用済み(ユーザーが手動作成した)——上書きしない
	}
	a, err := s.m.pg.CreateAgent("reporter", "レポート作成",
		"脆弱性の詳細レポート作成：脆弱性の発見時に自動で起動し、エビデンスと実行過程を取得して Markdown レポートを書いて書き戻す。")
	if err != nil {
		log.Printf("[reporter] agent の作成に失敗しました: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] prompt の seed に失敗しました: %v", err)
	}
	// トリガーの実行戦略：parallel + none —— 1 脆弱性 1 レポート、複数の finding をそれぞれ並行して書く。
	// merge は必ず none：そうでないと(デフォルト all)finding の一斉発生が 1 回の実行にまとめられ、並行の意味がなくなる。
	// maxParallel=5：同時に最大 5 つのレポートセッションとし、瞬間的に LLM 呼び出しが増えすぎるのを避ける。
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] トリガーの実行戦略の設定に失敗しました: %v", err)
	}
	// 必要なツールをバインドする：レポートの書き込み + エビデンス/実行過程/状況の読み取り。
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] ツールのバインドに失敗しました: %v", err)
	}
	// トリガー：report_finding が呼ばれると起動する（ツールは "finding recorded: <id>" を返して finding_id を渡し、
	// タスク id もトリガーメッセージに含まれる）。
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] トリガーの作成に失敗しました: %v", err)
	}
	log.Printf("[reporter] 「レポート作成」agent + finding トリガーをプリセットしました")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding のデフォルトバインドに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding のデフォルトバインドに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets のデフォルトバインドに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // worker→planner のデフォルトバインド切り替え
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope のデフォルトバインドに失敗しました: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope のバインド解除に失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context / cross-work tools off worker's
// default binding ONCE on existing DBs (guarded by a settings flag): a worker executes
// one intent and writes back — reading facts/nodes/companies and pulling other workers'
// traces is a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who deliberately
// re-binds worker. Each RemoveAgentFromTool is per-tool + membership-guarded, so
// planner/mainagent bindings of the same tool are untouched.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "node_detail", "list_companies",
		"search_all_worker_traces", "list_worker_traces", "get_worker_trace",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s を worker からバインド解除できませんでした: %v", k, err)
			return // エラー時は flag を立てず、次回起動時に再試行する
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 旧アセットツール名を置き換え、insert_assets/add_company_scope を追加
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// アセットツール：Auto はプラットフォーム操作でアセットの確認/登録や企業スコープの管理をよく行う。
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] デフォルトのバインド設定に失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
