package agent

import (
	"bytes"
	"text/template"
	"time"
)

// PromptOverride, if set, returns the stored system-prompt template for an agent
// key and whether one exists. The server wires it to the PG agent_prompts table.
// When nil or no override exists, agents use their built-in default prompt — so
// behavior is identical until a user edits a prompt in the UI.
var PromptOverride func(agentKey string) (string, bool)

// Prompt-variable structs — fields mirror each agent's catalog (docs §5a) so a
// user template referencing a catalog variable renders; referencing anything else
// fails template execution and falls back to the built-in default.
type PlannerVars struct{ Goal, Scope, AssetSummary, DataDir, Now string }
type WorkerVars struct {
	ProxyAddr, WorkerName, DataDir, Now string
	// Intranet はタスクがイントラネット期（フットホールドのセッション/有効なトンネルが存在）かどうかを示す。true のとき
	// workerSystem は worker.intranet の変種本文を使い、コード固定のイントラネット規律テールを追加する。
	Intranet bool
}
type MainVars struct{ Goal, AssetSummary, FindingsSummary, DataDir, Now string }
type GoalsVars struct{ EngagementDescription, DataDir, Now string }

// nowStr is the server-local wall-clock string exposed as the universal {{.Now}}
// prompt variable. renderSystem runs on every agent turn/round, so this is fresh
// each run — a prompt can subtract it from a fixed start stamp to reason about
// elapsed time (e.g. a timed benchmark's "last N hours" window).
func nowStr() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

// renderSystem returns the rendered system-prompt BODY (section [A]) for agentKey.
// Precedence: the DB-stored template (if any) over the built-in default template
// (def). BOTH are Go templates now — the built-in default is seeded into the DB
// verbatim, so the two paths render identically until a user edits the prompt.
// Rendering always runs (def used to be pre-substituted plain text; it is now a
// {{.Var}} template like the DB one). On any render error we fall back to the
// default template, then to the raw default string — an agent never starts with a
// half-rendered prompt. Callers append the code-owned tail (trafficTool / 中間成果物
// 出力規約) AFTER this, so those can't be edited away via the DB body.
func renderSystem(agentKey, def string, vars any, dependencies ...*Dependencies) string {
	overrides := dependencySet(dependencies)
	tmpl := def
	if overrides.PromptOverride != nil {
		if t, ok := overrides.PromptOverride(agentKey); ok && t != "" {
			tmpl = t
		}
	}
	if out, err := renderTmpl(tmpl, vars); err == nil {
		return out
	}
	// DB template broke (e.g. references an out-of-catalog var) → code default.
	if out, err := renderTmpl(def, vars); err == nil {
		return out
	}
	return def
}

// renderSystemVariant は renderSystem と同じだが、変種 key（worker の "intranet" など）を扱う:
// まず agentKey+"."+variant の DB 上書きを試し、なければ agentKey 自身の解決チェーン
// （DB 上書き → コード既定）へ戻す。PromptOverride の仕組み自体は変えず、ここでは key 解決を 1 段足すだけ。
// 変種テンプレートの描画失敗（カタログ外の変数を参照）も同様に key 自身へ戻し、半描画のプロンプトは絶対に出さない。
func renderSystemVariant(agentKey, variant, def string, vars any, dependencies ...*Dependencies) string {
	overrides := dependencySet(dependencies)
	if variant != "" && overrides.PromptOverride != nil {
		if t, ok := overrides.PromptOverride(agentKey + "." + variant); ok && t != "" {
			if out, err := renderTmpl(t, vars); err == nil {
				return out
			}
		}
	}
	return renderSystem(agentKey, def, vars, dependencies...)
}

func renderTmpl(tmpl string, vars any) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
