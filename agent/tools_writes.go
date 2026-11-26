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

// addOneHint は hint ノード(active/human)を探索グラフに1つ付け、アセットをアンカーでき、id を返す。
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !t.findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("Agent の自動トラフィックバインドはオフのため、traffic_refs を含むヒントは保存していません。システム設定で有効にするか、テキストのみで引き継いでください")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text を入力してください")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	id, err := t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
	if err == nil && t.notify != nil {
		t.notify() // wake the planner so the new hint is read promptly (debounced)
	}
	return id, err
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal は goal ノード(open)を探索グラフに1つ付け、タスクのルート(origin fact, rel spawns)に繋ぐ。
// origin は t.worker(既定 system):goals 分解器が書くものは "goals"、メイン agent の実行時は
// "human" を記録する。planner の起動は setGoals がバッチ全体を書き終えてから一括で行う(下記)。ここは保存のみを担う。
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text を入力してください")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)
	}
	return id, nil
}

// setGoals は【本タスク】に探索ゴール(goal ノード)を追加する。ゴール分解器の送信ツールでもあり、メイン
// agent の実行中にゴールを補うツールでもある。同一の管理対象ツールで、web 側で説明/schema の変更や agent 単位のバインドができる。
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"【本タスク】に探索ゴール(goal)を追加する。ゴール=最終的に納品/検証できる結果であり、攻撃ステップや偵察行動ではない。\n"+
			"★バッチを優先:複数のゴールを goals 配列に入れて一度に送信し、ids を同じ長さ・同じ順序で返す(失敗した項目は id=0、詳細は errors)。単条の場合は goals を省略しトップレベルの text を渡す。\n"+
			"vulnclass は任意:対応する脆弱性クラス(SQLi/IDOR など)。ビジネスロジック系のゴールは空にする。ゴール達成の判定はシステムが行い met を立てる。本ツールは追加のみを担う。",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【こちらを優先】追加するゴールの配列で、順に処理される。各要素：text(必須、独立して検証できる最終ゴール)+ vulnclass(任意)。ids はこの配列と同じ長さ・同じ順序で返る。", "items": map[string]any{"type": "object"}},
			"text":      str("[単条] 独立して検証できる最終ゴール"),
			"vulnclass": str("[単条] 対応する脆弱性クラス(明確な場合)。SQLi/IDOR など。ビジネスロジック系のゴールは空でよい"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals は有効になっていません: ExplorationStore が初期化されていません"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 単条モード:トップレベルの text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner を起こす(バッチで1回)。notifyGoal を優先：1回の set_goals で「人が
				// N 個のゴールを追加:…」のトリガを1件だけ記録し、1件ずつの氾濫を避ける。分解器/worker にはこのコールバックが無く → 素の notify に戻る(分解器は
				// round-0 では notify も未接続＝何もしない。このとき planner はまだ起動していない)。
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// メイン agent の実行中にゴールが追加された → 完了/一時停止したタスクを running に戻して続行させる(終端ゲートが
				// 通常の notify を飲み込むため、明示的に復活させる必要がある)。このコールバックを設定するのは mainagent のみ。分解器/worker は nil。
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 単条:従来の返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint は操作制約を1件 task_constraints に保存する。origin は t.worker(既定 system):
// 分解器は "goals"、メイン agent は "human" を書く。
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text を入力してください")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // 既定は禁止扱い:種類が未指定ならより保守的に
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type は allow または deny である必要があります")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints は【本タスク】に操作制約を追加する(allow=何をしてよいか / deny=何をしてはいけないか)。ゴール
// 分解器が round-0 で制約を抽出する送信ツールであり、メイン agent の実行中に制約を補うツールでもある。同一の管理対象ツールで、web
// 側で説明/schema を変更し agent 単位でバインドできる。制約は planner/worker のシステムプロンプトに注入され、探索の境界を定める。
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"【本タスク】に操作制約を追加し、探索の境界を定める：type=allow(許可する操作)または deny(禁止する操作)。\n"+
			"制約=「どの操作をしてよく、何をしてはいけないか」の規定(例：「現在のポートのみ試験し、他のポートはスキャンしない」「本番 DB への書き込みは禁止」「受動的偵察のみ許可」)であり、ゴールでも攻撃ステップでもない。\n"+
			"★バッチを優先:複数は constraints 配列に入れて一度に送信し、ids を同じ長さ・同じ順序で返す(失敗した項目は id=0、詳細は errors)。単条の場合は constraints を省略しトップレベルの text/type を渡す。\n"+
			"タスクのゴール/説明に【明記されている】制約だけを登録し、作り出さない。種類に迷ったら deny(より保守的)を使う。",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【こちらを優先】追加する制約の配列で、順に処理される。各要素：text(必須、1つの制約)+ type(allow|deny)。ids はこの配列と同じ長さ・同じ順序で返る。", "items": map[string]any{"type": "object"}},
			"text":        str("[単条] 1つの操作制約の内容"),
			"type":        str("[単条] allow(許可)または deny(禁止)。省略時は deny 扱い"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints は有効になっていません: ExplorationStore が初期化されていません"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 単条モード:トップレベルの text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 単条:シンプルな返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "人間/メイン agent の戦略ヒントを探索グラフに付ける。planner が次に意図を生成するときに読む。\n"+
		"★バッチを優先：複数のヒントを hints 配列に入れて一度に送信する（逐次呼び出しより往復を節約）。ids 配列を返し、hints と同じ長さ・同じ順序（失敗した項目は id=0、詳細は errors）。単条の場合は hints を省略しトップレベルの text を渡す。",
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先】追加するヒントの配列で、順に処理される。各要素のフィールドは下のトップレベルと同じ（text/asset_ids/traffic_refs）。ids はこの配列と同じ長さ・同じ順序で返る。", "items": obj(map[string]any{"text": str("ヒント内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})},
			"text":         str("[単条] ヒント内容。例：「認証後の API を重点的に掘る」"),
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "アンカーするアセット id（任意、0/1/複数）"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 単条モード：トップレベルの text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // 単条：従来の返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}
