package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// frontier の上限判定（純関数）: open の件数が MaxOpenIntents に達したら満杯。
func TestFrontierFull(t *testing.T) {
	if MaxOpenIntents != 8 {
		t.Fatalf("MaxOpenIntents=%d, want 8（紅日 3 R2 修正契約: frontier の上限は 8 open）", MaxOpenIntents)
	}
	for _, tc := range []struct {
		open int
		want bool
	}{
		{0, false}, {1, false}, {MaxOpenIntents - 1, false},
		{MaxOpenIntents, true}, {MaxOpenIntents + 5, true}, {36, true},
	} {
		if got := FrontierFull(tc.open); got != tc.want {
			t.Errorf("FrontierFull(%d)=%v, want %v", tc.open, got, tc.want)
		}
	}
}

// capFrontierTools は add_intent をゲートで包み、cancel_intent を追加し、他のツールはそのまま返す必要がある。
func TestCapFrontierToolsWrapsAddIntentAndAppendsCancel(t *testing.T) {
	ts := NewToolSet(nil, "planner")
	base := ts.PlannerTools()
	out := ts.capFrontierTools(base)
	if len(out) != len(base)+1 {
		t.Fatalf("capFrontierTools len=%d, want %d(+cancel_intent)", len(out), len(base)+1)
	}
	var wrapped, hasCancel bool
	for _, tool := range out {
		switch tool.Name() {
		case "add_intent":
			if _, ok := tool.(*frontierCapTool); !ok {
				t.Fatal("add_intent が frontierCapTool で包まれていない")
			}
			wrapped = true
		case "cancel_intent":
			hasCancel = true
		}
	}
	if !wrapped || !hasCancel {
		t.Fatalf("wrapped=%v hasCancel=%v", wrapped, hasCancel)
	}
}

// nil store（toolcatalog の seed 構築など）ではゲートはそのまま通し、panic してはならない。
func TestFrontierCapToolPassesThroughWithNilStore(t *testing.T) {
	inner := &stubTool{name: "add_intent"}
	tool := &frontierCapTool{CoreTool: inner, ts: nil}
	if _, err := tool.Call(context.Background(), json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("nil store のときゲートが内側のツールへ通していない")
	}
	if tool.Name() != "add_intent" {
		t.Fatalf("Name()=%q, want add_intent（name は委譲する必要がある）", tool.Name())
	}
}

// MainAgentTools は kill_work/cancel_intent を備える。cancel_intent の schema が要求するのは intent_id のみ。
func TestMainAgentToolsIncludeKillAndCancel(t *testing.T) {
	ts := NewToolSet(nil, "human")
	var kill, cancel actool.CoreTool
	for _, tool := range ts.MainAgentTools() {
		switch tool.Name() {
		case "kill_work":
			kill = tool
		case "cancel_intent":
			cancel = tool
		}
	}
	if kill == nil || cancel == nil {
		t.Fatalf("kill_work=%v cancel_intent=%v, mainagent は両方をバインドする必要がある", kill != nil, cancel != nil)
	}
	if !strings.Contains(kill.Description(), "cancel_intent") || !strings.Contains(kill.Description(), "steer_work") {
		t.Fatal("kill_work の説明には steer_work/cancel_intent との違いを明記する必要がある")
	}
	if !strings.Contains(cancel.Description(), "kill_work") || !strings.Contains(cancel.Description(), "open") {
		t.Fatal("cancel_intent の説明には open のみを無効化することと、kill_work との違いを明記する必要がある")
	}
}

type stubTool struct {
	actool.CoreTool
	name   string
	called bool
}

func (s *stubTool) Name() string { return s.name }
func (s *stubTool) Call(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
	s.called = true
	return actool.Text("ok"), nil
}
