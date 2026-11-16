package agent

import (
	"strings"
	"testing"
)

// add_intent の chain_tags: 正当な値はインテントの payload に書き込み（既存の契約を壊さない）、不正な値/2 個超はエラー。
func TestAddOneIntentChainTags(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	task, err := d.CreateTask("chain tags", "test chain_tags", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	tools := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	tools.SetTaskID(task.ID)

	id, err := tools.addOneIntent(intentItem{Summary: "example.com のログイン画面で SQL インジェクションを試す", ChainTags: []string{" Web ", "sqli?"}})
	_ = id
	if err == nil || !strings.Contains(err.Error(), "chain_tags") {
		t.Fatalf("不正な tag は chain_tags エラーになるべき, got err=%v", err)
	}

	if _, err := tools.addOneIntent(intentItem{Summary: "x", ChainTags: []string{"web", "ad", "priv"}}); err == nil {
		t.Fatal("2 個を超える tag はエラーになるべき")
	}

	id, err = tools.addOneIntent(intentItem{Summary: "example.com のログイン画面で SQL インジェクションを試す", ChainTags: []string{"web", "PRIV"}})
	if err != nil {
		t.Fatalf("正当な tags はエラーにならないはず: %v", err)
	}
	n, err := tools.ts.GetNode(id)
	if err != nil || n == nil {
		t.Fatalf("インテントノードの取得: %v", err)
	}
	payload := string(n.Payload)
	if !strings.Contains(payload, `"chain_tags":["web","priv"]`) {
		t.Fatalf("payload は正規化後の chain_tags を含むべき, got: %s", payload)
	}
	// 既存フィールドは影響を受けない
	if !strings.Contains(payload, `"summary"`) {
		t.Fatalf("payload に summary がない: %s", payload)
	}
}

// L2 の meta 鉄則: 静的定数を planner/worker system のコード固定テールに追加する。
func TestSystemPromptMetaRules(t *testing.T) {
	p := plannerSystem("X を攻略", "/data", "/data")
	if !strings.Contains(p, "meta 铁律(反问纪律,全程在场)") {
		t.Fatal("plannerSystem に planner 版 meta 鉄則がない")
	}
	w := workerSystem("", "", "/data", "/data", false)
	if !strings.Contains(w, "meta 铁律(观测真伪与卡住自检)") {
		t.Fatal("workerSystem に worker 版 meta 鉄則がない")
	}
}
