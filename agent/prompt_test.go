package agent

import (
	"strings"
	"testing"
)

func TestRenderSystemOverrideAndFallback(t *testing.T) {
	t.Cleanup(func() { PromptOverride = nil })

	// no override → built-in default
	PromptOverride = nil
	if got := renderSystem("planner", "DEFAULT", PlannerVars{Goal: "g"}); got != "DEFAULT" {
		t.Fatalf("no override should give default, got %q", got)
	}

	// override → rendered with vars
	PromptOverride = func(k string) (string, bool) {
		if k == "planner" {
			return "目標:{{.Goal}} 範囲:{{.Scope}}", true
		}
		return "", false
	}
	if got := renderSystem("planner", "DEFAULT", PlannerVars{Goal: "X を攻略", Scope: "*.x.com"}); got != "目標：X を攻略 範囲：*.x.com" {
		t.Fatalf("override render: %q", got)
	}

	// override referencing a non-catalog var → execution error → fallback to default
	PromptOverride = func(k string) (string, bool) { return "{{.NotInCatalog}}", true }
	if got := renderSystem("planner", "DEFAULT", PlannerVars{Goal: "x"}); got != "DEFAULT" {
		t.Fatalf("bad var should fall back to default, got %q", got)
	}

	// full plannerSystem path: DB body [A] is honored, then the code-owned tail
	// [C]（中間成果物の出力規約）is ALWAYS appended — editing the body can't drop it.
	PromptOverride = func(k string) (string, bool) { return "PLANNER {{.Goal}}", true }
	got := plannerSystem("X を攻略", "/data", "/data")
	if !strings.HasPrefix(got, "PLANNER X を攻略") {
		t.Fatalf("plannerSystem body not honored: %q", got)
	}
	if !strings.Contains(got, "中间产物输出规约") || !strings.Contains(got, "/data") {
		t.Fatalf("plannerSystem missing code-owned artifact tail: %q", got)
	}

	// worker dual-text via {{if .ProxyAddr}} in a user template, plus the code tail:
	// [B] trafficTool present only when RECORDING (caCert set — the MITM is on, so
	// the traffic_* tools exist), [C] artifact spec always present. The trafficTool
	// block is gated on the CA (arg 2), NOT on ProxyAddr — a global egress proxy
	// with capture off routes traffic but records nothing.
	PromptOverride = func(k string) (string, bool) {
		return "{{if .ProxyAddr}}プロキシ経由 {{.ProxyAddr}}{{else}}手動{{end}}", true
	}
	recording := workerSystem("127.0.0.1:8080", "/ca.pem", "/data", "/data", false)
	if !strings.HasPrefix(recording, "プロキシ経由 127.0.0.1:8080") {
		t.Fatalf("worker proxy branch body: %q", recording)
	}
	if !strings.Contains(recording, "traffic_search") {
		t.Fatalf("worker while recording should inject trafficTool: %q", recording)
	}
	if strings.Contains(recording, "traffic_refs") {
		t.Fatalf("worker bypassed shared optional evidence policy: %q", recording)
	}
	if !strings.Contains(recording, "中间产物输出规约") {
		t.Fatalf("worker missing artifact tail: %q", recording)
	}
	// Egress proxy set but capture OFF (no CA): the ProxyAddr template branch still
	// renders, but the trafficTool block must NOT — those tools are not registered.
	egressOnly := workerSystem("127.0.0.1:8080", "", "/data", "/data", false)
	if !strings.HasPrefix(egressOnly, "プロキシ経由 127.0.0.1:8080") {
		t.Fatalf("worker egress-only branch body: %q", egressOnly)
	}
	if strings.Contains(egressOnly, "traffic_search") {
		t.Fatalf("worker without recording must NOT inject trafficTool: %q", egressOnly)
	}
	noProxy := workerSystem("", "", "/data", "/data", false)
	if !strings.HasPrefix(noProxy, "手動") {
		t.Fatalf("worker no-proxy branch body: %q", noProxy)
	}
	if strings.Contains(noProxy, "traffic_search") {
		t.Fatalf("worker without proxy must NOT inject trafficTool: %q", noProxy)
	}
}

// フェーズ 4: intranet=true のとき本文を worker.intranet の変種へ差し替える（DB 上書きが優先、なければ
// worker のチェーンへ戻す）。コード固定のイントラネット・レッドラインテールは ALWAYS 追加され、本文編集では消せない。
func TestWorkerSystemIntranetVariant(t *testing.T) {
	t.Cleanup(func() { PromptOverride = nil })

	// DB 上書きなし: worker のデフォルト本文 + イントラネット・レッドラインテールへ戻る。
	PromptOverride = nil
	got := workerSystem("", "", "/data", "/data", true)
	if !strings.Contains(got, "内网红线") {
		t.Fatalf("intranet run missing code-owned intranet redline tail: %q", got)
	}

	// worker.intranet の上書きは worker の上書きより優先される。
	PromptOverride = func(k string) (string, bool) {
		switch k {
		case "worker.intranet":
			return "イントラネット本文 {{.Intranet}}", true
		case "worker":
			return "外部ネットワーク本文", true
		}
		return "", false
	}
	if got = workerSystem("", "", "/data", "/data", true); !strings.HasPrefix(got, "イントラネット本文 true") {
		t.Fatalf("worker.intranet override should win: %q", got)
	}
	if !strings.Contains(got, "内网红线") {
		t.Fatalf("edited variant body must not drop the code-owned intranet redline tail: %q", got)
	}
	// 変種がないときは worker 自身の上書きへ戻る。
	PromptOverride = func(k string) (string, bool) {
		if k == "worker" {
			return "外部ネットワーク本文", true
		}
		return "", false
	}
	if got = workerSystem("", "", "/data", "/data", true); !strings.HasPrefix(got, "外部ネットワーク本文") {
		t.Fatalf("missing variant should fall back to worker key: %q", got)
	}
	// intranet=false ではレッドラインテールを追加しない。
	if got = workerSystem("", "", "/data", "/data", false); strings.Contains(got, "内网红线") {
		t.Fatalf("non-intranet run must not append the intranet redline tail: %q", got)
	}
}
