package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "確認済みの脆弱性を記録する。evidence にはコマンド出力やログなど検証可能なエビデンスを渡す。タスクコンテキストでは現在の intent_id を渡す。返る finding_id は独立した脆弱性レコード ID、finding_node_id は探索ノード ID（先頭行はそのノード番号を保つ）。", obj(map[string]any{
		"vulnclass": str("脆弱性の分類"), "name": str("脆弱性名"), "severity": str("critical|high|medium|low"), "summary": str("ファインディングの要約"),
		"intent_id": idp("現在のタスクの意図 id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "影響を受けるアセット id。可能な限り付ける（同一エントリのマージに影響する）。空の場合、システムが報告テキスト中の URL から自動でアンカーする"},
		"evidence":         str("エビデンス/PoC テキスト"),
		"evidence_hint_id": idp("任意：本タスクでこの脆弱性に対応するヒントノード ID。その構造化 traffic_refs を自動で引き継ぐ。継承ヒントや他の脆弱性のヒントは参照できない"),
		"traffic_refs": map[string]any{"type": "array", "description": "任意。HTTP/HTTPS の脆弱性では、まず検索してリクエスト/レスポンスが本当にその結論を裏付けるか1件ずつ確認し、再現順に実際の ID を記入する。TCP など非 HTTP の脆弱性、未取得、該当レコードが見つからない場合は省略するか [] を渡す（報告は妨げない）。evidence に理由と他の検証可能なエビデンスを書いてよい。ID を推測したり、ドメイン/時刻から関連を推定したり、パケットを補うためだけに再探索したりしない。用途は baseline 正常対照 / proof 脆弱性の証明 / verification 補足検証 / supporting 補助エビデンス。",
			"items": obj(map[string]any{"traffic_id": str("traffic_search が返した実在のトラフィック ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("そのトラフィックが脆弱性の結論をどう裏付けるか")}, "traffic_id")},
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding はタスクコンテキストが必要です。プラットフォーム対話では add_task_hint で該当タスクに脆弱性を引き継ぎ、ヒントに既存の traffic_refs を載せてタスクの Agent に登録させてください。登録済みの脆弱性は bind_finding_traffic で追加バインドできます。"), nil
		}
		// Auto-binding off: ignore the evidence params instead of rejecting the call.
		// stripTrafficParameters already removes them from the advertised schema, but
		// models routinely emit fields anyway — failing here would discard a confirmed
		// finding over a stray parameter. The success path below reports evidence_status
		// "not_bound" with the "オフ。ページで手動関連付け可能" note, which is what the caller needs.
		if !t.findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id は有効なヒントノード ID である必要があります。引き継ぎヒントが無い場合は省略してください"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: pid(a.IntentID), VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs)}
		anchorText := strings.Join([]string{input.Name, input.Summary, input.Evidence}, "\n")
		if strings.TrimSpace(input.Name) == "" {
			input.Name = fallbackFindingName(input.VulnClass, anchorText)
		}
		// asset_ids が空なら主アセットを自動でアンカーする。段階Aの同一エントリ併合(db/finding_merge.go)は
		// (task, 正規化した vulnclass, 主アセット=AssetIDs[0]) で同一判定するため、asset_ids が空だと
		// 併合キーが全体として無効になる。アセットグラフはグローバル共有で、アンカー候補は全庫のアセット
		// (QueryByHost が SQL 層で host により粗く絞る)。task_ids の制約は受けず、無理に新規作成もしない。
		var anchoredID int64
		if len(input.AssetIDs) == 0 && t.as != nil {
			if tgt := extractAnchorTarget(anchorText); tgt.host != "" {
				if assets, aerr := t.as.QueryByHost(tgt.host, 0); aerr == nil {
					if id, _ := anchorFindingAsset(anchorText, assets); id > 0 {
						input.AssetIDs = []int64{id}
						anchoredID = id
					}
				}
			}
		}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("トラフィックエビデンスの保存が利用できません。脆弱性は登録していません"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if t.notify != nil {
			t.notify()
		}
		t.writes.Findings++
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string `json:"evidence_status"`
			EvidenceNote   string `json:"evidence_note,omitempty"`
			AutoAnchored   bool   `json:"auto_anchored,omitempty"`
			AssetID        int64  `json:"asset_id,omitempty"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if recorded.Merged {
			// 段階Aの同一エントリ併合：エビデンス/血縁は既存の confirmed 集約に追記され、システムが自動で
			// 増分再検証(C2)を起動する。同じエントリで脆弱性を重複作成しないようモデルに明示する。
			result.EvidenceNote = "この報告は既存の確認済み脆弱性と同一エントリ（同一タスク、同一の正規化カテゴリ、同一の主アセット）のため併合した。エビデンスは追記、severity は最大値を採用、血縁は既存ノードを指し、merged_into が集約先。システムは自動で増分再検証する。このため脆弱性を重複作成しないこと。"
		}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "脆弱性は保存したが、トラフィックは未バインド。TCP/パケットが無い場合はそのまま続行してよい。検証済みの HTTP トラフィックがあるなら、利用可能な bind_finding_traffic または脆弱性ページで追加バインドしてからエビデンスの引き継ぎを完了する。脆弱性を重複作成しないこと。"
			if recorded.Merged {
				result.EvidenceNote = "この報告は既存の確認済み脆弱性に併合された(merged=true)。今回は新しいトラフィックをバインドしていない。検証済みの HTTP トラフィックがあれば bind_finding_traffic で merged_into が指す脆弱性に追加バインドする。重複作成しないこと。"
			}
			if !t.findingTrafficBindingEnabled() {
				result.EvidenceNote = "脆弱性は保存した。Agent による自動トラフィックバインドはオフのため、ページで手動で関連付けられる。"
			}
		}
		if anchoredID > 0 {
			result.AutoAnchored = true
			result.AssetID = anchoredID
			note := fmt.Sprintf("asset_ids が空だったため、システムが報告テキストの URL から主アセット %d を自動でアンカーした（同一エントリ併合は主アセットで同一判定する）。次回の報告では asset_ids を直接渡すこと。", anchoredID)
			if result.EvidenceNote != "" {
				result.EvidenceNote = note + " " + result.EvidenceNote
			} else {
				result.EvidenceNote = note
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem は record_fact のバッチ/単条における1つの事実。
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // 1行の要点エビデンス（コマンド+要点の出力行）。結論を裏付け、後から照合しやすくする
	Confidence string            `json:"confidence"` // observed（直接見た）| inferred（現象からの推測）| confirmed（再確認で確定）
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// fact の信頼度は3段階（紅日3 の振り返り R3：自作ツールの否定的結論「445 は塞がれている」が確定事実として
// 扱われ、planner が誤った事実の上で数ラウンド空転した）。inferred と confirmed は直接観察と区別する。
const (
	FactConfidenceObserved  = "observed"  // 出力に直接現れたエビデンス（既定）
	FactConfidenceInferred  = "inferred"  // 推測/間接エビデンス/自作ツールの否定的結論
	FactConfidenceConfirmed = "confirmed" // 再確認/成熟ツールの検証で確定
)
