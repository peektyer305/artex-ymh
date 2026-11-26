package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent/chainskel"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// addOneIntent は意図ノードを1つ作成して上流の血縁を結び、id を返す。
// 制約：意図は確認済みの知識にしかアンカーできない。各 parent_id は既存の fact/finding
// ノードでなければならない（他の意図/ゴール/ヒントには付けられない）。最上位の新規方向は parent_ids を空にし、origin fact に繋ぐ。
// これにより「すべての意図が fact ノードに繋がり、見かけの計画ではなく発見駆動である」ことが作成経路で強制される。
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary を入力してください")
	}
	// chain_tags（L3 サイドクエスチョン骨格のルーティングタグ）:値 ∈ chainskel.Categories、最大 2 個。
	tags := chainskel.NormalizeChainTags(it.ChainTags)
	if err := chainskel.ValidateChainTags(tags); err != nil {
		return 0, err
	}
	// 先にアンカーを検証する（ノード作成前に、壊れたアンカーで孤立した意図を残さないため）。
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d は本タスクまたは直接関連タスクに存在しません：parent_ids には既存の【事実(fact)/ファインディング(finding)】ノード id を指定してください。最上位の新規方向は parent_ids を空にしてください", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d は %q ノードであり、意図のアンカーにはできません：意図は確認済みの【事実(fact)/ファインディング(finding)】にしかアンカーできず、意図/ゴール/ヒントには付けられません。最上位の新規方向は parent_ids を空にしてください", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	if len(tags) > 0 {
		payload["chain_tags"] = tags
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return t.writeExpTool("add_intent", "【探索方向】を生成して frontier に書き込み、探索チェーンに繋ぐ。意図は固定の型ではなく開かれた探索方向であり、summary で何を探索/検証/悪用するかを一文で自由に記述する。\n"+
		"★バッチを優先：1ラウンドで絞り出した複数の新方向を intents 配列に入れて一度に送信する（逐次呼び出しより往復を節約できる）。ids 配列を返し、intents と同じ長さ・同じ順序（失敗した項目は id=0、詳細は errors）。単条の場合は intents を省略しトップレベルの summary を渡す。",
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "【こちらを優先】追加する探索方向の配列で、順に処理される。各要素のフィールドは下のトップレベルと同じ（summary/asset_ids/parent_ids/priority/chain_tags）。ids はこの配列と同じ長さ・同じ順序で返る。", "items": map[string]any{"type": "object"}},
			"summary":    str("[単条] この探索方向を一文で記述する。何をするか+なぜか。方向が書けていれば十分で、アセット id には依存しない。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "この方向でテスト/攻撃する【対象アセット id】（**できるだけ渡す**。0/1/複数可。list_assets が返すアセット id で、探索ノード id ではない）：この探索方向が向かうアセット（サイト/API/パラメータ/ホストなど）。具体的なアセットを対象とする方向なら必ず渡す。これは「この探索がどの対象を狙うか」の構造化マークで、カバレッジの重複排除と意図のアセットチェーン接続に使う。純粋に全体偵察で対象アセットが本当に無い場合のみ空にする。"},
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "上流アンカー id（任意、0/1/複数）：この方向がどの【確認済みの事実(fact)/ファインディング(finding)】から総合されたか。**既存の fact/finding ノード id のみ指定でき、意図/ゴール/ヒントは指定できない**。意図は確認済みの知識にアンカーし、見かけの計画ではなく発見駆動でなければならない。複数の事実から新たな意図を生む場合は複数渡す。最上位の全く新しい偵察方向は空にする（タスク起点の origin fact に自動で繋がる）。"},
			"priority":   intp("優先度 0〜10、既定は 5"),
			"chain_tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "攻撃面チェーンのタグ（任意、1-2 個。値は web/ad/app/priv/pwn/recon/rev/pivot/meta のみ）：この方向の主要な攻撃面でタグ付けすると、エンジンが実行 worker にそのシナリオ向けのサイドクエスチョン意思決定骨格（シナリオ→判定基準→方向転換）を注入する。迷ったらキーワードで選ぶ（sql/インジェクション→web、ドメイン/kerberos→ad、権限昇格→priv、トンネル/プロキシ/横展開→pivot、リバース/アンパック→rev、ヒープ/スタック/ctf→pwn、偵察/フィンガープリント→recon、apk/frida/ミニアプリ→app）。どうしても迷うなら空にすれば、エンジンが summary のキーワードで自動マッチする。"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // 単条モード：トップレベルの summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// 人間がメイン agent 経由で意図を直接投入 → タスクが done 済み（open ゴールの無い goalless 分岐）なら
			// running に戻し、worker がこの意図を引き受けられるようにする。resumeTask はメイン agent の Chat のみが
			// 設定する(SetResumeTask)。planner の ToolSet では nil なので、planner 自身の add_intent ではこの部分が
			// no-op となり、通常の意図生成に影響しない。意図ノードは上で(open)作成済みなので、復活時に枯渇と誤判定されない。
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // 単条：従来の返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}
