package agent

import "testing"

func TestDependencySetsStayIsolated(t *testing.T) {
	first := &Dependencies{PromptOverride: func(string) (string, bool) { return "first", true }}
	second := &Dependencies{PromptOverride: func(string) (string, bool) { return "second", true }}
	if got := renderSystem("custom", "default", nil, first); got != "first" {
		t.Fatal(got)
	}
	if got := renderSystem("custom", "default", nil, second); got != "second" {
		t.Fatal(got)
	}
	previous := PromptOverride
	t.Cleanup(func() { PromptOverride = previous })
	PromptOverride = func(string) (string, bool) { return "legacy", true }
	if got := renderSystem("custom", "default", nil, &Dependencies{}); got != "default" {
		t.Fatal("explicit dependencies inherited globals", got)
	}
}
