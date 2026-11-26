package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) listGoals() actool.CoreTool {
	return t.readExpTool("list_goals", "本タスクのゴールノードとその状態（open/met）を列挙する。達成判定に使う。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			g, _ := t.ts.ListByKind(db.KindGoal, 100)
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return t.writeExpTool("prove_goal", "あるファインディング/事実がゴール達成を証明すると判断したときに呼ぶ。エビデンスノードをゴールノードに繋ぎ、ゴールを met にする。（エビデンスが脆弱性ノードなら confirmed 済みでなければならない。pending の未検証や誤検知は突破済みに数えず、エビデンスにできない）",
		obj(map[string]any{
			"goal_id":     idp("ゴールノード id"),
			"evidence_id": idp("それを証明するファインディング/事実ノード id"),
			"reason":      str("このエビデンスがそのゴールを満たす理由"),
		}, "goal_id", "evidence_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id と evidence_id は必須です"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id は本タスクのゴールノードである必要があります（関連タスクのゴールは読み取り専用）"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id は本タスクまたは直接関連タスクの事実/脆弱性ノードである必要があります"), nil
			}
			// C4:未検証(pending)/誤検知(false_positive)の脆弱性は「突破済み」に数えず、ゴール達成の
			// エビデンスにはできない。反証検証(verifier)が confirmed を書き戻して初めて有効になる。ノードの state は
			// 報告時点で 'confirmed' なので、triage 状態は findings テーブルを調べる。独立行が無い(合成/レガシー)
			// ノードは従来の動作を保つ。
			if evidenceNode.Kind == db.KindFinding && t.as != nil {
				if st, serr := t.as.FindingStatusByNodeID(ev); serr == nil && !provenFinding(st) {
					return actool.Errorf(fmt.Sprintf("この脆弱性はまだ検証を通っていません（現在のステータス %s）。ゴール達成のエビデンスにはできません。反証検証で confirmed が確認されてから再試行してください", st)), nil
				}
			}
			_ = t.ts.Link(ev, db.RelProves, goal)
			_ = t.ts.SetNodeState(goal, "met")
			// ゴールを1つ met にするたびに、本タスクの【すべてのゴール】が met かを確認し、そうなら
			// タスク完了を自動判定する（GoalMet を立てる）。モデルが明示的に goal_met を呼ぶ必要はない。
			if goals, err := t.ts.ListByKind(db.KindGoal, 1000); err == nil && len(goals) > 0 {
				allMet := true
				for _, g := range goals {
					if g.State != "met" {
						allMet = false
						break
					}
				}
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("%d 個のゴールがすべて met（最後は goal %d が契機）", len(goals), goal)
					return actool.Text(fmt.Sprintf("goal %d marked met。本タスクのすべてのゴールが達成され、タスクは完了と自動判定された", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "【タスク全体を即座に終了する】。タスクの【すべてのゴールが本当に達成され、全体を締めくくれる】と確認できたときだけ呼ぶ（あくまでタスク【全体】の完了。そのうちの1つのゴール/1つの flag/1つの脆弱性を達成しただけでは【該当しない】。その場合は prove_goal でそのゴールをマークすればよい）。⚠️これは「今ラウンドの計画を終える」ためのものではない。今ラウンドに出す新しい意図が無い、worker の成果を待っている、いずれも【そのまま今ラウンドを終えればよく、このツールは呼ばない】（意図が0件なのは完全に正常）。通常の判定は prove_goal でゴールを個別に証明するのを優先し、goal_met は個別証明を省いて全体を締める手段にすぎない。",
		obj(map[string]any{"reason": str("達成理由（ゴールが本当に達成されたエビデンスであること。「今ラウンドに新方向が無い」のようなラウンド終了の理由は不可）")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}
