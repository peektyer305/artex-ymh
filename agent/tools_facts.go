package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// normalizeFactConfidence はモデルが自己申告した confidence を3段階に正規化する。空/未認識 → observed（既定段階）。
func normalizeFactConfidence(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case FactConfidenceInferred:
		return FactConfidenceInferred
	case FactConfidenceConfirmed:
		return FactConfidenceConfirmed
	default:
		return FactConfidenceObserved
	}
}

// negativeConclusionPatterns は否定的結論の保守的なキーワード検出（日本語・英語・中国語）。誤って拾う（inferred を多めに付ける）
// のは許容し、見逃さないことを優先する。命中したら observed→inferred を強制し、成熟ツールで再確認してから confirm_fact で confirmed に上げる。
var negativeConclusionPatterns = []string{
	// 日本語（エージェントの要約向け）
	"利用不可", "応答なし", "応答しない", "存在しない", "非対応", "封鎖", "ブロック",
	// 英語（ツール出力向け）
	"not supported", "blocked", "unreachable", "no response", "filtered",
	// 中国語（中国語プロバイダ/ツールの出力に備える）
	"不可用", "被封", "无响应", "不存在", "不支持",
}

// isNegativeConclusion はテキスト（summary+detail）が「X が利用不可/応答なし/存在しない」のような否定的結論かを判定する。
func isNegativeConclusion(text string) bool {
	s := strings.ToLower(text)
	for _, p := range negativeConclusionPatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// factDowngradeNote は否定的結論が自動降格されたときにツールの返却でモデルへ伝える文言。
const factDowngradeNote = "否定的結論を inferred にした（確定事実としては扱わない）。成熟ツール/直接エビデンスで再確認したら confirm_fact で confirmed に上げられる。"

// recordOneFact は fact ノードを1つ書き、意図に繋ぐ（intent→yields→fact）。defaultIntent は
// バッチ時の既定意図（この項目に intent_id が無いときに使う）。note が非空なら、ツールの返却とともに
// モデルへ伝える注意書き（現状は否定的結論が inferred に自動降格された場合のみ）。
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, string, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, "", fmt.Errorf("summary を入力してください")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	// confidence は常に保存する（既定 observed）。紅日3 の振り返り：否定的結論は inferred へ強制降格。
	conf := normalizeFactConfidence(it.Confidence)
	var note string
	if conf == FactConfidenceObserved && isNegativeConclusion(it.Summary+"\n"+it.Detail) {
		conf = FactConfidenceInferred
		note = factDowngradeNote
	}
	payload["confidence"] = conf
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, "", fmt.Errorf("intent_id は本タスクの意図である必要があります（関連タスクの意図は読み取り専用）")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, "", err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact
	}
	t.writes.Facts++
	return id, note, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "探索の【事実/結論】を探索グラフに書き込み、それを生んだ意図（intent_id）に繋ぐ。フィンガープリント/列挙などの【肯定的結論】と、「ポート閉」/「パラメータはインジェクション不可」/「ログイン入口が見つからない」などの【否定的結論】の両方を記録するために使う。\n"+
		"⚠️1回の探索で得た複数の観察は【1つの事実に集約】し、分割しない。1つにまとめられるなら極力1つで表す：summary=今回の結論をまとめた一文、detail=関連する詳細（複数の具体項目を含めてよい）。例：フィンガープリントの意図→1つの事実 {summary:'X サイトの技術スタックと応答特性を識別', detail:'nginx 1.25 / Vue3 / 200 / title=.. / body_len=..'} とし、ステータスコード・フィンガープリント・タイトルを別々に記録しない。1つの意図から出る事実は通常1つで、細かく割るとグラフが無限に膨らむ。\n"+
		"★facts 配列は【互いに異なる】結論を一度に複数書くときに使う（各項目の intent_id は省略でき、省略時はトップレベルの intent_id を使う）。ids 配列を返し、facts と同じ長さ・同じ順序。\n"+
		"⚠️ツール出力で【実際に見た】結論だけを書き、推測で補わない。evidence と confidence は不正確な結論でグラフを汚さないためのもの：\n"+
		"  · evidence=この結論を支える【1行】の要点エビデンス（コマンド+最も証明力のある1〜2行の出力）。**必ず簡潔に**。詳細は detail にあるので、ここに長い出力を貼らない。\n"+
		"  · confidence=observed（出力に直接現れた）| inferred（現象からの推測/間接エビデンス/自作ツールの否定的結論）| confirmed（再確認/成熟ツールで確定。通常は confirm_fact による昇格で、自分では付けない）。\n"+
		"  · **否定的な結論**（インジェクション不可/ポート閉/入口が見つからないなど）は「観察 + 試行的な読み」だけを書く。実際に何を見たかを述べ、方向を捨てるかは planner が全体を見て決める。evidence は必ず付け、手段を尽くしていない、またはエビデンスが弱い（1回だけ試した、それらしいだけ）場合は inferred、本当に尽くして直接見た場合だけ observed にする。\n"+
		"  · システム規則：テキストに「利用不可/封鎖/応答なし/存在しない/非対応/blocked/unreachable」などの否定的結論の特徴が含まれると、observed は【自動で inferred に降格】され返却時に通知される。その後に成熟ツール/直接エビデンスで再確認して確定したら、confirm_fact で confirmed に上げる。",
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "【異なる結論が複数あるときに使う】事実の配列。要素のフィールドは下のトップレベルと同じ（summary/detail/evidence/confidence/intent_id/asset_ids）。intent_id を省略するとトップレベルの intent_id を使う。ids はこの配列と同じ長さ・同じ順序で返る。", "items": map[string]any{"type": "object"}},
			"summary":    str("今回の探索結論の【まとめの一文】（detail の要約）"),
			"intent_id":  idp("この事実を生んだ意図 id（自分が引き受けた意図。バッチ時は各項目の既定値）"),
			"detail":     str("この事実の関連詳細：今回の探索で得た複数の観察事実をここにまとめて書く"),
			"evidence":   str("【1行】の要点エビデンス：コマンド + 結論を最もよく証明する1〜2行の出力。必ず簡潔にし、長い出力を貼らない（詳細は detail）。"),
			"confidence": str("observed（出力に直接現れた）| inferred（現象からの推測/間接エビデンス/自作ツールの否定的結論）| confirmed（通常は confirm_fact での再確認・昇格。自分では付けない）。否定的結論は必ず正直に付ける。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "関連アセット id（任意、0/1/複数）：この事実が関わるアセット"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 単条モード + バッチ既定 intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // トップレベル intent_id = バッチ既定

			ids := make([]int64, len(items))
			errs := map[string]string{}
			notes := map[string]string{}
			for i, it := range items {
				id, note, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				if note != "" {
					notes[strconv.Itoa(i)] = note
				}
			}

			if !batch { // 単条：従来の返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				msg := fmt.Sprintf("fact recorded: %d", ids[0])
				if n := notes["0"]; n != "" {
					msg += "\n⚠️" + n
				}
				return actool.Text(msg), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			if len(notes) > 0 {
				out["notes"] = notes
			}
			return jsonResult(out)
		})
}

// confirmFact は inferred の事実を confirmed に上げる（紅日3 の振り返り R3 の受け皿：否定的結論が
// 自動降格された後、worker/planner が成熟ツール/直接エビデンスで再確認して確定し、本ツールで承認する）。監査
// （confirmed_by/confirmed_at を payload に保存）を伴い、fact は本タスクに属する必要がある。
func (t *ToolSet) confirmFact() actool.CoreTool {
	return writeTool("confirm_fact",
		"inferred の事実を confirmed に上げる。【その結論を成熟ツール/直接エビデンスで再確認済みの場合のみ】呼べる。例えば record_fact の否定的結論がシステムに自動降格された後、nmap/nc などの成熟ツールで再測定して確定してから本ツールを呼ぶ。入力 fact_id（graph_overview/list_facts にある本タスクの fact ノード id）。",
		obj(map[string]any{
			"fact_id": idp("confirmed に上げる事実ノード id（本タスクに属すること）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("confirm_fact は有効になっていません: ExplorationStore が初期化されていません"), nil
			}
			var a struct {
				FactID json.RawMessage `json:"fact_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.FactID)
			if id <= 0 {
				return actool.Errorf("fact_id は必須です"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindFact {
				return actool.Errorf("fact_id は本タスクの事実ノードである必要があります（関連タスクのノードは読み取り専用）"), nil
			}
			var p map[string]any
			_ = json.Unmarshal(node.Payload, &p)
			if prev, _ := p["confidence"].(string); prev == FactConfidenceConfirmed {
				return actool.Text(fmt.Sprintf("fact %d はすでに confirmed です。再確認は不要", id)), nil
			}
			if err := t.ts.ConfirmFact(id, t.worker); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if t.notify != nil {
				t.notify() // planner を起こす：inferred の事実が確定したので、計画の方向に影響しうる
			}
			return actool.Text(fmt.Sprintf("fact %d を confirmed に昇格した", id)), nil
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}
