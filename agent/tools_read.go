package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "本タスクと直接関連タスクの【確認済み脆弱性】を列挙する（コンパクト：id+task_id+intent_id+vulnclass+severity+要約+ステータス）。関連タスクの項目は source_task_id/inherited=true を持ち読み取り専用。ここには脆弱性のみ。通常の探索事実は list_facts、詳細は node_detail(id)。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> それを生んだ intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "本タスクと直接関連タスクの【探索事実/結論】をページングで列挙する（新しい順。コンパクト：id+要約+ステータス。要約が長すぎる場合は切り詰めるので全文は node_detail(id)）。パラメータはすべて任意：limit（既定 20、上限 100）、before（カーソル。前ページが返した next_before を渡すとより古いページを取る。省略/0=最新ページ）、q（要約のキーワードで絞り込み）。{facts, total, has_more, next_before} を返す。total は絞り込み後の総数、has_more=true のときは next_before でページを進める。関連タスクの項目は source_task_id/inherited=true を持ち読み取り専用。脆弱性は list_findings。",
		obj(map[string]any{
			"limit":  intp("返す件数。既定は 20、上限 100"),
			"before": intp("ページングカーソル。id がこの値より小さい、より古い事実のみ返す。省略または 0 = 最新ページ"),
			"q":      str("事実の要約のキーワードで絞り込み（大文字小文字を区別しない）。省略 = 絞り込みなし"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // これを返して次のページ（より古い）を取得する
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "id で本タスクまたは直接関連タスクの【探索グラフノード】の完全な内容を取得する。継承ノードは source_task_id/inherited=true を持ち読み取り専用。対象は list_facts/list_findings/graph_overview が返した探索ノード id のみ。アセットは list_assets/asset_neighbors を使う。",
		obj(map[string]any{"id": idp("探索グラフノード id（アセット id ではない）")}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id は必須です"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("探索ノード %d が見つかりません。アセットを調べたい場合は list_assets / asset_neighbors を使用してください（アセットと探索ノードは別の id 空間で、アセット id を node_detail に渡すことはできません）。", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// intentItem は add_intent のバッチ/単条における1つの探索方向。
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
	ChainTags []string          `json:"chain_tags"`
}
