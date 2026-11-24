package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// 本ファイルは「組み込みツール」を純粋なコードから、列挙でき DB で上書きできる
// カタログに変える：
//   - BuiltinToolSeeds()：3 つの実行 agent の組み込みツール群を seed レコード（key +
//     説明 + パラメータ schema + 既定で束縛する agent）に展開し、サーバ起動時に冪等に
//     tools テーブルへ播種できるようにする。
//   - ToolResolve フック：実行時に DB の tools 行に従い、組み立て済みのツールへ
//     「agent 単位の絞り込み + 説明/schema の上書き + パラメータ既定値の注入」を行う。
//     key/handler はコード層に残り、DB が変えられるのは「説明文と既定値」だけ。
// handler（Call の挙動）は常にコード由来。DB では変えられず、変えられるのはモデルが
// 見る説明と省略時の引数だけ。

// ToolSeed は組み込みツールの播種用スナップショット：key は CoreTool.Name()（handler と
// 固定で結び付き、UI からは読み取り専用）、Desc/Schema はコード上のツール定義から取り、
// Agents はコードが既定でどの agent に与えているかを示す。
type ToolSeed struct {
	Key    string         // = CoreTool.Name()、主キー、変更不可
	Desc   string         // 最上位の説明（UI で上書き可能）
	Schema map[string]any // パラメータ JSON-Schema（構造は読み取り専用、description/default は UI で変更可）
	Agents []string       // 既定で束縛する agent key（worker/planner/mainagent）
}

// builtinToolsByAgent は「読み取り専用の空シェル」ToolSet（nil stores）で各実行 agent の
// ドメインツール群を構築する。ツールのコンストラクタはクロージャを Spec に入れるだけで、
// 構築時に store を参照しないため nil でも安全。ここでは Name()/Description()/
// InputSchema() を読むためだけに使い、Call は決してしない。
//
// SDK 汎用ツール actool.DefaultTools()（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）は
// 意図的に【含めない】：どの agent も常に持ち「誰に束縛するか」の選択が無い上、説明の
// 大半は Prompt() 側にある（本表は Description() しか扱わず、中途半端な上書きは誤解を
// 招く）。seed しない → DB 行が無い → ToolResolve はそのまま通し、上書きもしないので
// 挙動は従来どおり。カタログで管理できるのは artex 自身のドメインツールだけ。
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		// cancel_intent は PlannerTools()（tools.go）には無く、planner の組み立て層
		// （Plan の capFrontierTools）が追加するツールである。seed でここに補っておけば
		// DB の束縛が planner を覆い、実行時に ToolResolve が planner のツール一覧から
		// 落とすこともなくなる。
		"planner": append(ts.PlannerTools(), ts.cancelIntentTool()),
		"worker":  ts.WorkerTools(),
		// goals（ゴール分解器）は既定で set_goals + set_constraints を束縛し、これらで分解した
		// ゴールと抽出した操作制約を DB に書き込む。mainagent と同じ管理対象ツールを共有し、
		// web 側で説明/schema を変えたり agent ごとにチェックしたりできる。
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto は既定で脆弱性報告 + アセット管理ツールを束縛し、他のドメインツールは UI で
		// 必要に応じてチェックできる。新規 DB はこの seed で書き込み、既存 DB は
		// seedAutoDefaultBindings で移行する。
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest（独立ペネトレーション agent）の既定束縛：アセット照会 / アセット挿入 /
		// 脆弱性報告 / 脆弱性照会 / 企業照会。新規 DB はこの seed で書き込み、既存 DB は
		// seedPentestDefaultBindings で移行する。
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound：これらの system ツールは通常どおりカタログに入り（web 側で見え、agent ごとに
// 手動でチェックできる）が、既定では【どの agent にも束縛しない】。ToolResolve は空束縛の
// ツールをすべての agent から一律に破棄するため、明示的な opt-in が要る。それでもある agent の
// base ツール群に残している理由は 2 つ：seed がそれを構築して desc/schema を取れること、
// ユーザーが手動で束縛し直した後 base に在れば ToolResolve が残せること。
//
// goal_met：個別の prove_goal を飛ばして【タスク全体の完了】を全局的に宣言するもので、
// 影響が大きく誤判定の恐れもある上、「prove_goal で最後のゴールを met にすると自動終了処理」と
// 重複するため、既定ではどの agent にも与えず、必要時に手動で束縛する。
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds は各 agent の組み込みツール群を重複排除して 1 つの seed リストにまとめる：
// 同名ツール（list_assets のように複数 agent が持つもの）は 1 件に統合し、Agents は和集合を
// 取る。defaultUnbound のツールは束縛を強制的に空にする。
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // カタログには入り手動で束縛もできるが、既定ではどの agent にも与えない（null ではなく [] を保存し、他のツールと揃える）
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// default arguments get injected. Tools with no matching DB row (MCP/skill/host tools
// like traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only defaults are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
