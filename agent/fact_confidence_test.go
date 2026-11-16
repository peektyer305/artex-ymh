package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 紅日3 の振り返り R3:fact の信頼度等級。純粋なロジック部分（正規化/陰性判定）は PG に依存せず、
// DB への書き込み・降格・昇格は testDB 経由（PG が使えない場合は自動で skip）。

func TestNormalizeFactConfidence(t *testing.T) {
	cases := map[string]string{
		"":          FactConfidenceObserved, // 既定の等級
		"observed":  FactConfidenceObserved,
		" Observed": FactConfidenceObserved, // 大文字小文字/空白を許容
		"inferred":  FactConfidenceInferred,
		"INFERRED":  FactConfidenceInferred,
		"confirmed": FactConfidenceConfirmed,
		"garbage":   FactConfidenceObserved, // 未認識 → 既定の等級、第 4 の等級は作らない
	}
	for in, want := range cases {
		if got := normalizeFactConfidence(in); got != want {
			t.Errorf("normalizeFactConfidence(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIsNegativeConclusion(t *testing.T) {
	negative := []string{
		"目标 445 端口被封死",
		"该服务不可用",
		"参数 id 不存在",
		"服务器无响应",
		"目标不支持该协议",
		"Connection blocked by firewall",
		"host UNREACHABLE",
		"no response from target",
		"445/tcp filtered",
		"Feature Not Supported",
	}
	for _, s := range negative {
		if !isNegativeConclusion(s) {
			t.Errorf("isNegativeConclusion(%q)=false want true", s)
		}
	}
	// 正常/前向きな結論を誤って陰性と判定しない
	positive := []string{
		"",
		"445 ポートは開放され、正常に接続できる",
		"ターゲットは TLS1.3 に対応している",
		"応答は正常 200 OK",
		"インタラクティブシェルを取得できた",
		"nginx 1.25 / Vue3 の技術スタックを識別した",
	}
	for _, s := range positive {
		if isNegativeConclusion(s) {
			t.Errorf("isNegativeConclusion(%q)=true want false", s)
		}
	}
}

// factTestRig は実際のタスク + ToolSet を作る。PG が要る fact テスト用。
func factTestRig(t *testing.T) *ToolSet {
	t.Helper()
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTaskWithOptions(fmt.Sprintf("fact-conf-%s", t.Name()), "fact confidence test", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	ts := NewToolSet(d.Exploration(task.ExplorationID), "worker-x")
	ts.SetTaskID(task.ID)
	return ts
}

func callFactTool(t *testing.T, tool actool.CoreTool, input string) string {
	t.Helper()
	res, err := tool.Call(context.Background(), json.RawMessage(input), nil)
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	return res.Flatten()
}

func factPayload(t *testing.T, ts *ToolSet, id int64) map[string]any {
	t.Helper()
	node, err := ts.ts.GetNode(id)
	if err != nil || node == nil {
		t.Fatalf("GetNode(%d): node=%+v err=%v", id, node, err)
	}
	var p map[string]any
	if err := json.Unmarshal(node.Payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	return p
}

func TestRecordFactNegativeDowngrade(t *testing.T) {
	ts := factTestRig(t)

	// 1) 陰性の結論 + observed の自己申告 → 自動で inferred に降格し、戻り値で通知する
	text := callFactTool(t, ts.recordFact(), `{"summary":"目标 445 端口被封死,无法直连","confidence":"observed"}`)
	var id int64
	if _, err := fmt.Sscanf(text, "fact recorded: %d", &id); err != nil || id <= 0 {
		t.Fatalf("record_fact return: %q", text)
	}
	if !strings.Contains(text, "inferred") || !strings.Contains(text, "confirm_fact") {
		t.Fatalf("downgrade notice missing: %q", text)
	}
	if got := factPayload(t, ts, id)["confidence"]; got != FactConfidenceInferred {
		t.Fatalf("negative fact confidence=%v want inferred", got)
	}

	// 2) 前向きな結論 + observed → observed のまま、降格の通知なし
	text = callFactTool(t, ts.recordFact(), `{"summary":"nginx 1.25 を識別、応答 200","confidence":"observed"}`)
	if strings.Contains(text, "⚠️") {
		t.Fatalf("positive fact must not be downgraded: %q", text)
	}
	var id2 int64
	_, _ = fmt.Sscanf(text, "fact recorded: %d", &id2)
	if got := factPayload(t, ts, id2)["confidence"]; got != FactConfidenceObserved {
		t.Fatalf("positive fact confidence=%v want observed", got)
	}

	// 3) confidence 省略 → observed で保存（常に値が入る）
	text = callFactTool(t, ts.recordFact(), `{"summary":"ターゲットは TLS1.3 に対応"}`)
	var id3 int64
	_, _ = fmt.Sscanf(text, "fact recorded: %d", &id3)
	if got := factPayload(t, ts, id3)["confidence"]; got != FactConfidenceObserved {
		t.Fatalf("default confidence=%v want observed", got)
	}

	// 4) inferred を明示した陰性の結論 → inferred のまま（通知の重複は許容、昇格は不可）
	text = callFactTool(t, ts.recordFact(), `{"summary":"ポートが応答しない","confidence":"inferred"}`)
	var id4 int64
	_, _ = fmt.Sscanf(text, "fact recorded: %d", &id4)
	if got := factPayload(t, ts, id4)["confidence"]; got != FactConfidenceInferred {
		t.Fatalf("explicit inferred confidence=%v want inferred", got)
	}

	// 5) 一括:陰性項目の note が notes マップに入る
	batch := callFactTool(t, ts.recordFact(), `{"facts":[{"summary":"ホスト unreachable","confidence":"observed"},{"summary":"フィンガープリント識別が完了","confidence":"observed"}]}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(batch), &out); err != nil {
		t.Fatalf("batch result: %v raw=%q", err, batch)
	}
	notes, _ := out["notes"].(map[string]any)
	if len(notes) != 1 || notes["0"] == nil {
		t.Fatalf("batch notes=%v want only index 0", notes)
	}
	ids, _ := out["ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("batch ids=%v", out["ids"])
	}
	fid := int64(ids[0].(float64))
	if got := factPayload(t, ts, fid)["confidence"]; got != FactConfidenceInferred {
		t.Fatalf("batch negative confidence=%v want inferred", got)
	}
}

func TestConfirmFact(t *testing.T) {
	ts := factTestRig(t)

	// inferred の事実 → confirm_fact で confirmed へ昇格し、監査フィールドも付く
	text := callFactTool(t, ts.recordFact(), `{"summary":"445 被封","confidence":"observed"}`)
	var id int64
	_, _ = fmt.Sscanf(text, "fact recorded: %d", &id)
	if got := factPayload(t, ts, id)["confidence"]; got != FactConfidenceInferred {
		t.Fatalf("pre-confirm confidence=%v want inferred", got)
	}
	msg := callFactTool(t, ts.confirmFact(), fmt.Sprintf(`{"fact_id":%d}`, id))
	if !strings.Contains(msg, "confirmed") {
		t.Fatalf("confirm_fact return: %q", msg)
	}
	p := factPayload(t, ts, id)
	if p["confidence"] != FactConfidenceConfirmed {
		t.Fatalf("post-confirm confidence=%v", p["confidence"])
	}
	if p["confirmed_by"] != "worker-x" || p["confirmed_at"] == nil || p["confirmed_at"] == "" {
		t.Fatalf("audit fields missing: %+v", p)
	}

	// 重複確認 → 冪等な通知を返し、エラーにしない
	msg = callFactTool(t, ts.confirmFact(), fmt.Sprintf(`{"fact_id":%d}`, id))
	if !strings.Contains(msg, "すでに confirmed") {
		t.Fatalf("re-confirm return: %q", msg)
	}

	// fact 以外のノード（intent）→ 拒否
	intentID, err := ts.ts.AddIntent(map[string]any{"text": "probe"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	msg = callFactTool(t, ts.confirmFact(), fmt.Sprintf(`{"fact_id":%d}`, intentID))
	if !strings.Contains(msg, "本タスクの事実ノード") {
		t.Fatalf("confirm on intent must be rejected: %q", msg)
	}

	// fact_id が無い → 拒否
	msg = callFactTool(t, ts.confirmFact(), `{}`)
	if !strings.Contains(msg, "fact_id は必須です") {
		t.Fatalf("missing fact_id must be rejected: %q", msg)
	}
}

func TestGraphOverviewMarksInferredFact(t *testing.T) {
	ts := factTestRig(t)
	text := callFactTool(t, ts.recordFact(), `{"summary":"端口无响应","confidence":"observed"}`)
	var id int64
	_, _ = fmt.Sscanf(text, "fact recorded: %d", &id)

	overview := ts.graphOverviewData()
	facts, _ := overview["recent_facts"].([]map[string]any)
	var found map[string]any
	for _, f := range facts {
		if fid, _ := f["id"].(int64); fid == id {
			found = f
			break
		}
	}
	if found == nil {
		t.Fatalf("fact %d missing from recent_facts: %#v", id, facts)
	}
	conf, _ := found["confidence"].(string)
	if !strings.Contains(conf, "inferred") || !strings.Contains(conf, "要再確認") {
		t.Fatalf("inferred fact must carry 要再確認 marker, confidence=%q", conf)
	}
}
