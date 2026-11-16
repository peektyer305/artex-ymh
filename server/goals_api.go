package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 概要「ゴール管理」の人手 CRUD API。agent 側の set_goals ツールと同じ goal ノード群を
// 書き込むが、入口は人間が UI 上で直接追加・編集・削除するもの。追加・編集後は
// 「タスク復活」ロジックを再利用し（admitTask resume：終了状態→running、一時停止解除、
// 必要ならキュー投入）、削除は復活させない（製品判断）。各変更ハンドラは
// beginTaskOperation/decInflight を通し、タスク削除との競合を避ける（意図 CRUD と同じ）。

// listGoals はこのタスクの全ゴールを返す（text/vulnclass/state に分解済み）。ゴール管理カードの描画用。
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal は人手でゴールを 1 件追加する：永続化（タスクルートの spawns 配下に接続）→
// ゴール追加のトリガーイベントを記録して planner を起こす → タスクを復活させ、planner に
// 新しいゴールで達成判定をやり直させる。
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクは削除中です。ゴールを追加できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "ゴールの内容を入力してください")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // 「人がゴールを追加した」トリガーを記録して planner を起こす
	s.reviveTask(t)              // 完了・一時停止したタスクを実行状態に戻して続行させる
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "ゴール書き込み後の読み込みに失敗しました")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal は人手でゴールのテキスト（と vulnclass）を変更する：DB を更新 → ゴール編集の
// トリガーイベントを記録して planner を起こす → タスクを復活させ、planner に新しいゴールで
// 方向を調整させる。
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクは削除中です。ゴールを変更できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "ゴールの内容を入力してください")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "ゴールが存在しません")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // ゴール編集のトリガーを記録して planner を起こす
	s.reviveTask(t)                   // 追加時と同じ：タスクを復活させ、新ゴールで再判定させる
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "ゴール更新後の読み込みに失敗しました")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal は人手でゴールを 1 件削除する（ハード削除、辺/アンカーもカスケード削除）：DB から
// 削除 → ゴール削除のトリガーイベントを記録して planner を起こし、残りのゴールで再判定させる。
// 製品判断により、削除ではタスクを【復活させない】。
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクは削除中です。ゴールを削除できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "ゴールが存在しません")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // ゴール削除のトリガーを記録して planner を起こす（タスクは復活させない）
	writeJSON(w, 200, map[string]bool{"ok": true})
}
