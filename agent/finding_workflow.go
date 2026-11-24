package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**脆弱性番号の取り決め**：finding_id は独立した脆弱性レコードの ID で、finding_node_id は探索ノードの ID である。list_findings / list_task_findings / node_detail / get_task_node_detail の id は探索ノード ID として保持されるため、同一の戻り値から独立番号である finding_id を読み取る。get_finding_traffic / bind_finding_traffic は独立した finding_id を使う。旧 update_finding_report の finding_id パラメータには引き続き finding_node_id を渡す。report_finding の先頭行の数字をエビデンスツールに使わない。番号の誤りに遭遇した後も他の数字を推測しない。"

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\n既定ではレポート Agent がレポート作成前にトラフィックを裏付け確認して関連付ける。報告者は evidence に検証コマンド、主要な出力、既存の実際のトラフィック ID とその用途を残し、レポート Agent が実行記録と突き合わせて裏付けを確認できるようにする。関連付けのためにわざわざパケットを確認し直す必要はない。明示的な即時関連付けにも対応する。traffic_refs または evidence_hint_id で確認済みの参照を提出でき、後者は本タスクで指定した hint の構造化参照を読み取る。いずれかが無効な場合、今回の報告はすべて失敗する。TCP/パケットなしの場合、これらの任意パラメータは不要である。戻り値の finding_id と finding_node_id は、それぞれ独立レコードと探索ノードを表す。"
		case "add_hint", "add_task_hint":
			note = "\n確認済みの脆弱性を引き継ぐ際は、対応するヒントの traffic_refs に、裏付け確認済みトラフィックの ID、用途、説明、順序を残す（1 件の場合は最上位に、複数の場合は対応する hints 要素に置く）。さらに text で、それが証明する具体的な脆弱性を説明する。呼び出し側は文字だけを引き継いで既存のトラフィック参照を捨ててはならない。未確認の候補はエビデンスとして渡せない。"
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**トラフィックエビデンスの引き継ぎ（任意）**：自動関連付けは既定ではレポート Agent が脆弱性をデータベースへ書き込んだ後、レポート作成前に完了させる。報告者は evidence に検証コマンド、主要な出力、既存の実際のトラフィック ID とその用途を残し、タスクに intent_id を含めてレポート Agent が追跡できるようにする。関連付けのためにわざわざパケットを確認し直す必要はない。Auto / Planner が代わりに報告する場合は、実行者が既に持つ参照を捨ててはならない。add_hint / add_task_hint は traffic_refs で引き継げる。明示的な即時関連付けは report_finding の traffic_refs / evidence_hint_id にも対応する。TCP またはパケットなしの場合は通常どおり登録し、ID を推測せず、パケットを補うためだけに重複してプローブしない。"
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nプラットフォームの会話にタスクコンテキストがない場合は、report_finding を直接呼び出さない。add_task_hint で既存の対応するタスクへ引き継ぎ、タスク Agent に登録させ、list_task_findings で結果を裏付け確認する。"
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\n目標の完了を判定する前に、今回の既存エビデンスの報告/引き継ぎを先に完了させる。エビデンスの引き継ぎがまだ完了していないのに、文字として脆弱性が登録されたことだけでタスクを終了したり Worker をキャンセルしたりしない。パケットなしの場合は、待機や強引なパケット取得を求めない。"
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**レポート前のトラフィック自動関連付け（有効）**：あなたは今回トリガーされた脆弱性についてトラフィックを裏付け確認して関連付けてから、レポートを作成する。まず report_finding が返す JSON または get_task_node_detail / list_task_findings から、明確な finding_id と finding_node_id を取得する。脆弱性の詳細、対応する意図の実行記録、既存のエビデンス一覧を読み取り、報告者が引き継いだ実際の ID を優先して使う。今回の検証が HTTP でトラフィックツールが利用できる場合は、traffic_search で候補を絞り込み、traffic_get でリクエスト/レスポンスが実際にその脆弱性を裏付けているかを 1 件ずつ確認する。ドメインと時刻は絞り込みにのみ使い、帰属を証明するものではない。確認したエビデンスを再現順に bind_finding_traffic(finding_id, traffic_refs) で関連付け、baseline / proof / verification / supporting を選んで用途を説明する。操作できるのは今回の脆弱性のみで、脆弱性を重複作成したり対象を再プローブしたりしない。関連付けに成功したら、再度 get_finding_traffic を呼び出して最新の version を取得し、必要な本文を読み取り、実際に読み取った version を evidence_version として update_finding_report に渡す（その finding_id パラメータには引き続き finding_node_id を使う）。既存の関連付けがあれば重複して追記しない。TCP、未収集、ツールが利用できない、または確実な一致がない場合は自動関連付けをスキップし、文字/コマンドのエビデンスに基づいて通常どおりレポートを作成して理由を説明する。トラフィックを揃えるために推測してはならない。関連付けに失敗したら成功と主張しない。既存のエビデンスを残し、レポートで未関連付けの理由を説明する。"
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "任意。裏付け確認済みで本ヒント内の具体的な脆弱性に対応するトラフィック参照。順序を保持する。引き継ぎ後、report_finding で evidence_hint_id を渡してこれらの参照を持ち込める。", "items": obj(map[string]any{"traffic_id": str("実際のトラフィック ID"), "role": str("baseline / proof / verification / supporting"), "note": str("そのトラフィックが裏付ける結論")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d は本タスクのヒントノードである必要があります（継承したヒントは直接バインドに使えません）", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
