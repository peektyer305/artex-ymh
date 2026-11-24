package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 本ファイルは意図スケジューリング用のツール（红日 3 の振り返り R2:決勝意図 #207 が
// スケジューリング待ちで死んだ。frontier に上限が無く、P0 を誰も引き受けず、mainagent に
// kill/cancel の手段も無かった）。別ファイルに切り出したのは tools.go の並行変更と
// 衝突しないため。
//
//   - frontier のハード上限：planner の add_intent は open の意図数が MaxOpenIntents 以上の
//     とき派遣を拒否する（capFrontierTools が planner の組み立て層で門番を 1 枚被せる形で、
//     add_intent 本体は変えない。mainagent の手動直投は制限外。これは滞留を消化する際の
//     緊急経路）。
//   - cancel_intent:【未開始（open）】の意図を 1 件無効化する。mainagent/planner の
//     どちらにも束縛できる。
//   - kill_work（mainagent 束縛）:tools.go の killWorkTool を再利用し、engine コールバックに
//     killed_by_mainagent の名前付き理由を載せるだけ（server の配線を参照）。

// MaxOpenIntents は frontier のハード上限：open の意図がこの数に達すると planner の
// add_intent は派遣を拒否する（まず滞留を消化する）。红日 3 では frontier に open が
// 36 件滞留し、決勝意図が待ち行列で死んだ。
const MaxOpenIntents = 8

// FrontierFull は frontier ハード上限の純粋な判定：open の意図数が上限に達しているか。
func FrontierFull(openCount int) bool { return openCount >= MaxOpenIntents }

// frontierCapTool は add_intent を包む:呼び出し前に open の件数を 1 回数え、満杯なら派遣を拒否する。
// 包むのはツールの挙動だけで、Name/Description/Schema などの識別情報はすべて内側のツールへ委譲する。
type frontierCapTool struct {
	actool.CoreTool
	ts *db.ExplorationStore
}

func (f *frontierCapTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if f.ts != nil {
		if open, err := f.ts.CountOpenIntents(); err == nil && FrontierFull(open) {
			return actool.Errorf(fmt.Sprintf(
				"frontier が満杯です(%d open)。先に滞留を消化してください：kill_work で逸脱した実行中の意図を終了し、cancel_intent で不要になった open の意図を無効化するか、worker が消化するまで待ってから派遣してください",
				MaxOpenIntents)), nil
		}
	}
	return f.CoreTool.Call(ctx, in, tc)
}

// capFrontierTools は planner のドメインツール群に frontier のガバナンスを追加する:
// add_intent にハード上限の門番を被せ、cancel_intent を追加する（planner 自身が滞留を
// 消化する手段）。他のツールはそのまま残す。呼ぶのは planner の組み立て箇所（Plan）だけで、
// toolcatalog の seed 構築はここを通らない（seed は元の説明/schema だけで足りる）。
func (t *ToolSet) capFrontierTools(tools []actool.CoreTool) []actool.CoreTool {
	out := make([]actool.CoreTool, 0, len(tools)+1)
	for _, tool := range tools {
		if tool.Name() == "add_intent" {
			tool = &frontierCapTool{CoreTool: tool, ts: t.ts}
		}
		out = append(out, tool)
	}
	return append(out, t.cancelIntentTool())
}

// CancelIntentTool exposes cancel_intent for assemblies outside this file
// (planner's Plan / toolcatalog's seed construction).
func (t *ToolSet) CancelIntentTool() actool.CoreTool { return t.cancelIntentTool() }

// cancelIntentTool は【未開始（open）】の意図を 1 件無効化する。db 層のトランザクションは
// db.ExplorationStore.CancelOpenIntent を参照：open → stopped とし、無効化の理由をグラフに
// 付けて痕跡を残す（監査目的）。データは破棄しない。running の意図は拒否する（それには
// kill_work を使う）。
func (t *ToolSet) cancelIntentTool() actool.CoreTool {
	return t.writeExpTool("cancel_intent", "【まだ開始していない（open）】意図を 1 件無効化する:状態を stopped にして worker に取得されないようにし、無効化の理由を事実としてグラフに残す。frontier に溜まった期限切れ/重複/不要になった方向を片付けるのに使う。\n"+
		"steer_work / kill_work との違い：steer_work は【中断しない】リアルタイムの軌道修正、kill_work は実行中（running）の意図を【即時終了】する。cancel_intent が無効化できるのは【未開始（open）】のものだけで、running の意図には効かない（それには kill_work を使う）。",
		obj(map[string]any{
			"intent_id": idp("無効化する意図の id（open で未取得の状態であること）"),
			"reason":    str("無効化の理由（任意だが記入を推奨。事実として痕跡が残り、Planner が後から参照できる）"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Reason   string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須です"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id は本タスクの意図である必要があります(関連タスクの意図は読み取り専用)"), nil
			}
			origin := t.worker
			if origin == "" {
				origin = "mainagent"
			}
			factID, err := t.ts.CancelOpenIntent(id, a.Reason, origin)
			if err != nil {
				if errors.Is(err, db.ErrIntentStateConflict) {
					return actool.Errorf(fmt.Sprintf("意図 %d は現在 open 状態ではないため無効化できません。実行中の意図には kill_work を使ってください", id)), nil
				}
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("意図 %d を無効化しました(open → stopped、以後取得されません;理由は事実 #%d として痕跡を残しました)", id, factID)), nil
		})
}
