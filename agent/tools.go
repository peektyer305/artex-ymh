package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent/chainskel"
	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// balancedCap decides how many done-intents and facts to keep when the combined
// unfolded settled/cold region exceeds cap (§6.3). The smaller side is kept whole;
// the larger side takes the remaining budget; neither is starved below cap/2.
// Returns the keep counts and whether truncation applied. Newest-first slices, so
// callers keep the head.
func balancedCap(done, facts, cap int) (keepDone, keepFacts int, truncated bool) {
	if done+facts <= cap {
		return done, facts, false
	}
	half := cap / 2
	keepDone, keepFacts = done, facts
	switch {
	case done > half && facts > half:
		keepDone, keepFacts = half, cap-half
	case done > half:
		keepDone = cap - facts // facts fit in ≤half; intents take the rest
	default:
		keepFacts = cap - done // intents fit in ≤half; facts take the rest
	}
	return keepDone, keepFacts, true
}

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // 上流：この意図が派生元としたノード（複数の事実が1つの意図を生むこともある）
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // 下流：この意図が生み出した事実/ファインディング
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	findingRecorder FindingRecorder
	as              *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs              *db.CompanyStore // company store (optional)
	ts              *db.ExplorationStore
	worker          string
	taskID          int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode int64
	GoalMet   bool
	Reason    string
	writes    WriteCounts
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal). It flips a terminal/
	// paused task back to running and (re)starts the engine loops — a plain notify()
	// can't, because the planner's terminal gate swallows wakes. Wired ONLY for the
	// main agent (human steering); nil for the goals decomposer and workers.
	resumeTask func()
	// notifyGoal, if set, wakes the planner AND records ONE "人が N 個のゴールを追加：…" trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "事実1 アセット25 脆弱性0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("事実%d アセット%d 脆弱性%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts { return t.writes }

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block
// and DropCoverageTools filter the two coverage-only tools out of the agent's tool
// list. It does NOT stop scope accumulation: insertAssets' auto-scope hook runs
// either way, because task_scope is the task's range boundary (the filter basis for
// asset queries), not merely a coverage denominator.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
// add_task_scope is deliberately NOT here: task_scope is the task's range boundary
// (the filter basis for asset queries), not merely a coverage denominator, so the
// agents that own scope definition keep it either way — in lockstep with insertAssets'
// auto-scope hook, which also runs regardless of the switch.
var coverageOnlyTools = map[string]bool{"list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ts != nil && t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// readExpTool / writeExpTool build a domain tool whose handler dereferences the
// task-bound ExplorationStore. Two ToolSets carry a nil store: the catalog's
// seed-only shell (never called) and the server-level one behind buildDomainReg,
// which the tools table can bind to ANY agent — including ones that never run
// inside a task (auto/pentest/reporter/custom agent/side-question). Refusing there
// keeps a mis-bound tool a bad tool call; without the guard it was a nil deref,
// and tool handlers run on the harness's own goroutine, so the panic is out of
// reach of every recover() in the server and kills the whole process.
func (t *ToolSet) readExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return readTool(name, desc, schema, t.needExploration(name, run))
}

func (t *ToolSet) writeExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return writeTool(name, desc, schema, t.needExploration(name, run))
}

// needExploration wraps a handler so it only runs with an exploration store.
// Tools that degrade more usefully than "unavailable" (report_finding points at
// add_task_hint, set_goals/set_constraints at the task itself) keep their own
// bespoke guard instead.
func (t *ToolSet) needExploration(name string, run func(context.Context, json.RawMessage) (actool.Result, error)) func(context.Context, json.RawMessage) (actool.Result, error) {
	return func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil {
			return actool.Errorf(name + " はタスクコンテキスト（探索グラフ）が必要です：現在の agent はタスク内で実行されていないため、タスクの探索グラフを取得できず、このツールは使用できません。タスク内で使用するか、task_id を伴うクロスタスク読み取りツール（get_task_node_detail / list_task_findings / get_task_graph など）を使用してください。"), nil
		}
		return run(ctx, in)
	}
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return t.readExpTool("graph_overview",
		"(探索チェーングラフ)探索状況の蒸留サマリ：アセット数、API のないサイト、frontier、ファインディング、hints（人間/メイン agent の戦略ヒント。意図の生成時に必ず織り込む）。計画時はまずこれを呼び出す。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return jsonResult(t.graphOverviewData())
		})
}

// provenFinding reports whether a finding's triage status lets it count as a
// confirmed breakthrough for the planner (C4: pending placeholder nodes count neither
// as prove_goal evidence nor as "confirmed vuln count", but still appear in
// finding_list for the "progress in the same direction" dedup check).
// "" means the node has no standalone findings row (synthetic/legacy) — keep
// legacy behavior and treat it as countable.
func provenFinding(status string) bool {
	return status == "" || status == db.FindingConfirmed
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, _ := t.ts.ListByKind(db.KindGoal, 100)
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: 人間/メイン agent が add_hint でグラフに付けた戦略ヒント。folded in so the
	// planner reads them every round when generating intents (さもないと書きっぱなしになる).
	hints, _ := t.ts.ListByKind(db.KindHint, 50)
	hsum := make([]map[string]any, 0, len(hints))
	for _, h := range hints {
		var p map[string]any
		_ = json.Unmarshal(h.Payload, &p)
		hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
		if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
			hint["traffic_refs"] = p["traffic_refs"]
		}
		hsum = append(hsum, hint)
	}
	out["hints"] = hsum
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, _ := t.ts.Edges(5000)
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	// cold-digest §6: members folded into an active digest are shown via cold_digests
	// (below), not the flat recent_* lists. `covered` maps member id → its digest id.
	// §6 render-time revival check: a covered member that has become hot again (a new
	// intent derived from it) must reappear this round — so `hidden` folds a member out
	// only when it is covered AND still cold. covered_members (built from hidden) lets a
	// parents/yields id pointing into a live fold stay resolvable (§6.5 dangling lineage).
	covered, _ := t.ts.CoveredMembers()
	// Hot set at render time serves two §6 needs: (1) a covered member that revived
	// (now hot) must reappear this round; (2) the 60-cap must never truncate hot
	// (active-context) nodes — only the cold-but-unfolded region is cappable (§6.3).
	// Computed every round (cheap for real graph sizes); nil map degrades safely.
	var hotAtRender map[int64]bool
	if cg, _, err := loadColdGraph(t.ts); err == nil {
		hotAtRender = cg.hotSet()
	}
	hidden := func(id int64) bool { _, c := covered[id]; return c && !hotAtRender[id] }
	fr, _ := t.ts.Frontier(100)
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, _ := t.ts.ListByKind(db.KindIntent, 300)
	var running, recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "running":
			running = append(running, n)
		case "done", "blocked", "exhausted":
			if hidden(n.ID) {
				continue // in a cold_digest and still cold — shown via cold_digests (§6.2)
			}
			recentDone = append(recentDone, n) // §6: no 15-cap; folded ones are gone, cap applied below
		}
	}
	out["running_intents"] = compactIntents(running, parentsOf, yieldsOf)
	// recent_done_intents is finalized further below (after facts are known) so the
	// 60-cap can balance the two lists and protect hot nodes (§6.3).
	// done_intents_total：終了済み意図（done/blocked/exhausted）の総数。recent_done_intents と
	// 対になる名前で、後者はその最新ウィンドウ（≤15）に切り詰めたビュー。2つのキーを並べれば
	// 「見えているのは N/総数」と自明になり、planner は「未表示」を「未投入」と誤解せずに済む。
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	out["frontier_open"] = len(fr)
	// findings (confirmed vulns) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, _ := t.ts.ListByKind(db.KindFinding, 1000)
	factNodes, _ := t.ts.ListByKind(db.KindFact, 1000) // newest first
	// out["findings"] は下方の finding_list 組み立て時に集計する。triage で confirmed の
	// 脆弱性のみ（C4）。pending のプレースホルダと誤検知は「突破済み」に数えないが、全件を finding_list に入れる。
	out["facts"] = len(factNodes) // 探索した事実/結論の数（否定的結論を含む）
	// findings はタスクで最も価値の高い産物で、単一タスクでは通常そう多くない → facts と違い
	// 最近のウィンドウではなく全量を概観に載せ、planner がゴール判定のたびに確認済み脆弱性を一目で把握できる（list_findings の再呼び出し不要）。
	// 各要素は {id, summary, evidence?, status?, from_intent?, assets?} のみ。evidence は report_finding の
	// PoC テキスト（payload.evidence.poc）、status は triage 状態（pending=未検証、C4）、from_intent はこの脆弱性を生んだ意図、assets は影響アセットの
	// 可読表現（url/ドメイン/ip:port など。裸の id ではない）。アンカー関係は exploration_anchors に保存され findings
	// テーブル経由で引き当てる。vulnclass/severity/state などは引き続き list_findings / node_detail(id) で取得できる。
	var findingMeta map[int64]db.FindingMeta // node_id -> アンカーされたアセットなど。タスクコンテキストでのみ参照可能
	assetByID := map[int64]*db.Asset{}
	if t.as != nil && t.taskID > 0 {
		findingMeta, _ = t.as.FindingMetaByNodeID(t.taskID)
		idSet := map[int64]struct{}{}
		for _, meta := range findingMeta {
			for _, aid := range meta.AssetIDs {
				idSet[aid] = struct{}{}
			}
		}
		if len(idSet) > 0 {
			ids := make([]int64, 0, len(idSet))
			for aid := range idSet {
				ids = append(ids, aid)
			}
			if assets, err := t.as.GetByIDs(ids); err == nil {
				for _, a := range assets {
					assetByID[a.ID] = a
				}
			}
		}
	}
	findingList := make([]map[string]any, 0, len(vulnNodes))
	confirmedFindings := 0
	for _, n := range vulnNodes {
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := map[string]any{"id": n.ID, "summary": fp["summary"]}
		// C4:triage 状態を付ける。pending には「未検証」と表示し、planner にエビデンスとして
		// 使えず「同方向に既に進展あり」の重複排除シグナルでしかないと伝える。meta なし（プラットフォーム対話コンテキスト）ならそのまま。
		status := ""
		if meta, ok := findingMeta[n.ID]; ok {
			status = meta.Status
			m["status"] = status
		}
		if findingMeta == nil || provenFinding(status) {
			confirmedFindings++
		}
		if ev, ok := fp["evidence"].(map[string]any); ok {
			if poc, ok := ev["poc"].(string); ok && poc != "" {
				m["evidence"] = poc
			}
		}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // この脆弱性を生んだ意図
		}
		if meta, ok := findingMeta[n.ID]; ok && len(meta.AssetIDs) > 0 {
			assets := make([]string, 0, len(meta.AssetIDs))
			for _, aid := range meta.AssetIDs {
				if a := assetByID[aid]; a != nil {
					if v := assetValue(a); v != "" {
						assets = append(assets, v)
						continue
					}
				}
				assets = append(assets, fmt.Sprintf("#%d", aid)) // アセットが削除済み/未検出 → id 表記に戻して情報を落とさない
			}
			m["assets"] = assets // 影響を受けるアセットの可読表現
		}
		findingList = append(findingList, m)
	}
	out["finding_list"] = findingList
	out["findings"] = confirmedFindings // 確認済み脆弱性の数（ゴール判定はこれを見る。C4: pending/誤検知を含まない）
	// バッチ6 L1：ハニーポット疑いのアセット一覧（静的シグネチャ一致、score>0、最大20件、スコア降順）。
	// 単一シグナルでは降格のみで遮断しない。対処の規律はシステムプロンプト固定末尾（plannerHoneypotRules）。
	if t.as != nil && t.taskID > 0 {
		if pots, err := t.as.HoneypotAssetsByTask(t.taskID, 20); err == nil && len(pots) > 0 {
			plist := make([]map[string]any, 0, len(pots))
			for _, p := range pots {
				plist = append(plist, map[string]any{
					"id": p.ID, "asset": p.Label, "score": p.Score, "signatures": p.Evidence,
				})
			}
			out["honeypot_assets"] = plist
		}
	}
	// Build facts, split hot (active context — a fact under a live intent) from cold
	// (not-yet-folded). Hidden (folded & still cold) ones are surfaced via cold_digests.
	var recentFactsHot, recentFactsCold []map[string]any
	for _, n := range factNodes {
		if hidden(n.ID) {
			continue // in a cold_digest and still cold — surfaced via cold_digests (§6.2)
		}
		m := compactNode(n)
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // この事実を生んだ意図
		}
		// confidence を概観に含める。どの結論が inferred だけなのかを一目で分かるようにし（特に否定的結論
		// を確定事実とみなさない）、inferred には「(要再確認)」を付けて confirm_fact で再確認・昇格できると示す。
		// evidence は長いので node_detail(id) に委ねる。
		var fp map[string]any
		if json.Unmarshal(n.Payload, &fp) == nil {
			if c, ok := fp["confidence"].(string); ok && c != "" {
				if c == FactConfidenceInferred {
					c += "(要再確認)"
				}
				m["confidence"] = c
			}
		}
		if hotAtRender[n.ID] {
			recentFactsHot = append(recentFactsHot, m)
		} else {
			recentFactsCold = append(recentFactsCold, m)
		}
	}
	// Split the settled intents the same way: hot = ancestor of a live intent (active
	// context); cold = not-yet-folded settled.
	var doneHot, doneCold []*db.Node
	for _, n := range recentDone {
		if hotAtRender[n.ID] {
			doneHot = append(doneHot, n)
		} else {
			doneCold = append(doneCold, n)
		}
	}
	// §6.3 最後の砦の切り詰め：hot（アクティブな探索コンテキスト。live フロンティアの祖先チェーン、live 意図下の新事実）は【決して】
	// 切り詰めない。60-cap は【冷めていて未折り畳み】の残余のみを制約する（圧縮が遅れたときだけ増える）。live(open/running) は別フィールドで
	// 同じく影響を受けない。冷えた領域は両側を均衡して保持し、少ない側は全保持・多い側が残り枠を埋め、各々 cap/2 を下限として 0 にはしない。
	const unfoldedCap = 60
	keepColdDone, keepColdFacts, truncated := balancedCap(len(doneCold), len(recentFactsCold), unfoldedCap)
	if truncated {
		out["unfolded_truncated"] = true // 通常は発火しない。発火したら圧縮の遅延を意味する
	}
	out["recent_done_intents"] = append(
		compactIntents(doneHot, parentsOf, yieldsOf),
		compactIntents(doneCold[:keepColdDone], parentsOf, yieldsOf)...)
	out["recent_facts"] = append(recentFactsHot, recentFactsCold[:keepColdFacts]...) // {id, summary, from_intent, confidence?}——inferred は「(要再確認)」付き。詳細は node_detail(id)
	// cold-digest §6.1/§6.2: the folded cold region + the per-asset directory that
	// collapses independent directions, plus the dangling-lineage resolver map.
	if cds, cidx := t.coldDigestOverview(); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}] —— body をそのまま読む (§6.1)
		out["cold_index"] = cidx  // [{asset, asset_id, digest_ids}] —— アセット単位で方向を集約 (§6.2)
	}
	if len(covered) > 0 {
		cm := make(map[string]int64, len(covered))
		for member, dig := range covered {
			if hotAtRender[member] {
				continue // revived → shown live this round, not a dangling folded id
			}
			cm[strconv.FormatInt(member, 10)] = dig
		}
		if len(cm) > 0 {
			out["covered_members"] = cm // 宙に浮いた血縁 id → どの digest に属するか。expand_digest で展開 (§6.5)
		}
	}
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage：アセットのテストカバレッジの目安。範囲(task_scope)内のアセットのうち fact に触れられた
	// 割合 + by_type(種類別の 総数/テスト済み)。未テストの具体アセットは agent が list_untested_assets を必要時に呼んで判断する。タスクコンテキストでのみ提供。
	// アセットカバレッジ機能がオフの場合(coverageDisabled)：scope/hosts(範囲境界と対象ホストの
	// 把握情報。company の関連は scope 経由でここに現れる)のみ残し、denominator/tested/pct/by_type/note
	// などのカバレッジ指標は捨てる。コンテキストの汚染を避け、隠した add_task_scope/list_untested_assets を誘導しないため。
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["note"] = "coverage:アセットのテストカバレッジ（API など各種関連アセットを含む）で、粗い推定値であり参考情報にすぎない。現在のタスクと直接関連タスクの scope・事実アンカーを含む。関連 scope は読み取り専用。コンテナ型アセットや大量の列挙があると低く出るので、これでテスト完了と判断しない。add_task_scope で本タスクの範囲を追加でき、list_untested_assets で未テストのアセットを確認できる【通常は list_untested_assets を呼ばず、タスクを進めればよい】。"
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "範囲未アンカー"
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			// scope：現在のテスト範囲のルートアセット（task_scope の元の行）。このタスクが実際に
			// どの対象を範囲に含めたか（全テスト対象アセットではなく範囲境界そのもの）を agent に示す。カバレッジのスイッチとは無関係に常に提供。
			if rows, err := t.as.ListTaskScopeWithSources(t.taskID); err == nil && len(rows) > 0 {
				scope := make([]map[string]any, 0, len(rows))
				for _, r := range rows {
					e := map[string]any{"kind": r.Kind, "source": r.Source, "task_id": r.TaskID}
					if r.TaskID != t.taskID {
						inheritedMap(e, r.TaskID)
					}
					switch {
					case r.Domain != "":
						e["value"] = r.Domain
					case r.Net != "":
						e["value"] = r.Net
					case r.Value != "":
						e["value"] = r.Value
					case r.CompanyID != nil:
						e["company_id"] = *r.CompanyID
						if t.cs != nil {
							if company, err := t.cs.GetCompany(*r.CompanyID); err == nil && company != nil {
								e["company_name"] = company.Name
							}
							if rules, err := t.cs.GetScope(*r.CompanyID); err == nil {
								keywords := make([]string, 0)
								companyScope := make([]map[string]any, 0, len(rules))
								for _, rule := range rules {
									value := rule.Raw
									if value == "" {
										switch rule.Kind {
										case "domain":
											value = rule.Domain
										case "ip", "cidr":
											value = rule.Net
										default:
											value = rule.Value
										}
									}
									entry := map[string]any{"kind": rule.Kind, "value": value}
									if rule.Reason != "" {
										entry["reason"] = rule.Reason
									}
									companyScope = append(companyScope, entry)
									if rule.Kind == "keyword" && rule.Raw != "" {
										keywords = append(keywords, rule.Raw)
									}
								}
								if len(companyScope) > 0 {
									e["company_scope"] = companyScope
								}
								if len(keywords) > 0 {
									e["company_keywords"] = keywords
								}
							}
						}
					}
					scope = append(scope, e)
				}
				m["scope"] = scope
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// ホストは総数のみを渡し、host の一覧は graph_overview に展開しない（広範囲のタスクでは毎ラウンド
				// 大量の文字列を繰り返し運ぶことになり、計画判断への価値は限られる）。具体的なホストは list_assets で必要時に調べる。
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	return out
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes     = 48_000
	relatedOverviewMaxTextPerSource   = 8_000
	relatedOverviewMaxGoalsPerSource  = 8
	relatedOverviewMaxHintsPerSource  = 6
	relatedOverviewMaxFactsPerSource  = 12
	relatedOverviewMaxFindingsPerTask = 6
	relatedOverviewMaxIntentsPerTask  = 8
	relatedOverviewMaxScopePerSource  = 12
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		// §2 cross-task: render the source task's OWN folded view — fold out the
		// members it has already folded, and surface its cold_digests read-only.
		hidden := hiddenMembersFor(ts)
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			if hidden(fact.ID) {
				continue // folded into this source's cold_digests — shown there (§2/§6.2)
			}
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDoneRaw := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		recentDone := recentDoneRaw[:0] // in-place filter: drop this source's folded intents (§2)
		for _, intent := range recentDoneRaw {
			if hidden(intent.ID) {
				continue
			}
			recentDone = append(recentDone, intent)
		}
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		// §2 cross-task: the source task's folded cold region, read-only. Members are
		// resolvable via expand_digest(id)/node_detail(id), which search source tasks.
		if cds := activeDigestBodies(ts); len(cds) > 0 {
			for _, cd := range cds {
				cd["inherited"] = true
				cd["source_task_id"] = source.Task.TaskID
			}
			item["cold_digests"] = cds
		}
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// assetValue distills an asset to its most identifying human-readable string
// (url / domain / ip[:port] / app / service name) so finding_list can show the
// affected asset's content inline instead of a bare id. Empty when nothing
// identifying is set (caller falls back to #id).
func assetValue(a *db.Asset) string {
	switch {
	case a.URL != "":
		if a.Method != "" {
			return a.Method + " " + a.URL // API：HTTP メソッドも添える
		}
		return a.URL
	case a.Domain != "":
		if a.Port != nil {
			return fmt.Sprintf("%s:%d", a.Domain, *a.Port)
		}
		return a.Domain
	case a.IP != "":
		if a.Port != nil {
			return fmt.Sprintf("%s:%d", a.IP, *a.Port)
		}
		return a.IP
	case a.AppName != "":
		return a.AppName
	case a.ServiceName != "":
		return a.ServiceName
	}
	return ""
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "本タスクと直接関連タスクの【確認済み脆弱性】を列挙する（コンパクト：id+task_id+intent_id+vulnclass+severity+要約+ステータス）。関連タスクの項目は source_task_id/inherited=true を持ち読み取り専用。ここには脆弱性のみ。通常の探索事実は list_facts、詳細は node_detail(id)。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> それを生んだ intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "本タスクと直接関連タスクの【探索事実/結論】をページングで列挙する（新しい順。コンパクト：id+要約+ステータス。要約が長すぎる場合は切り詰めるので全文は node_detail(id)）。パラメータはすべて任意：limit（既定 20、上限 100）、before（カーソル。前ページが返した next_before を渡すとより古いページを取る。省略/0=最新ページ）、q（要約のキーワードで絞り込み）。{facts, total, has_more, next_before} を返す。total は絞り込み後の総数、has_more=true のときは next_before でページを進める。関連タスクの項目は source_task_id/inherited=true を持ち読み取り専用。脆弱性は list_findings。",
		obj(map[string]any{
			"limit":  intp("返す件数。既定は 20、上限 100"),
			"before": intp("ページングカーソル。id がこの値より小さい、より古い事実のみ返す。省略または 0 = 最新ページ"),
			"q":      str("事実の要約のキーワードで絞り込み（大文字小文字を区別しない）。省略 = 絞り込みなし"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // これを返して次のページ（より古い）を取得する
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "id で本タスクまたは直接関連タスクの【探索グラフノード】の完全な内容を取得する。継承ノードは source_task_id/inherited=true を持ち読み取り専用。対象は list_facts/list_findings/graph_overview が返した探索ノード id のみ。アセットは list_assets/asset_neighbors を使う。",
		obj(map[string]any{"id": idp("探索グラフノード id（アセット id ではない）")}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id は必須です"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("探索ノード %d が見つかりません。アセットを調べたい場合は list_assets / asset_neighbors を使用してください（アセットと探索ノードは別の id 空間で、アセット id を node_detail に渡すことはできません）。", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// --- planner write tools ---

// intentItem は add_intent のバッチ/単条における1つの探索方向。
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
	ChainTags []string          `json:"chain_tags"`
}

// addOneIntent は意図ノードを1つ作成して上流の血縁を結び、id を返す。
// 制約：意図は確認済みの知識にしかアンカーできない。各 parent_id は既存の fact/finding
// ノードでなければならない（他の意図/ゴール/ヒントには付けられない）。最上位の新規方向は parent_ids を空にし、origin fact に繋ぐ。
// これにより「すべての意図が fact ノードに繋がり、見かけの計画ではなく発見駆動である」ことが作成経路で強制される。
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary を入力してください")
	}
	// chain_tags（L3 サイドクエスチョン骨格のルーティングタグ）:値 ∈ chainskel.Categories、最大 2 個。
	tags := chainskel.NormalizeChainTags(it.ChainTags)
	if err := chainskel.ValidateChainTags(tags); err != nil {
		return 0, err
	}
	// 先にアンカーを検証する（ノード作成前に、壊れたアンカーで孤立した意図を残さないため）。
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d は本タスクまたは直接関連タスクに存在しません：parent_ids には既存の【事実(fact)/ファインディング(finding)】ノード id を指定してください。最上位の新規方向は parent_ids を空にしてください", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d は %q ノードであり、意図のアンカーにはできません：意図は確認済みの【事実(fact)/ファインディング(finding)】にしかアンカーできず、意図/ゴール/ヒントには付けられません。最上位の新規方向は parent_ids を空にしてください", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	if len(tags) > 0 {
		payload["chain_tags"] = tags
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return t.writeExpTool("add_intent", "【探索方向】を生成して frontier に書き込み、探索チェーンに繋ぐ。意図は固定の型ではなく開かれた探索方向であり、summary で何を探索/検証/悪用するかを一文で自由に記述する。\n"+
		"★バッチを優先：1ラウンドで絞り出した複数の新方向を intents 配列に入れて一度に送信する（逐次呼び出しより往復を節約できる）。ids 配列を返し、intents と同じ長さ・同じ順序（失敗した項目は id=0、詳細は errors）。単条の場合は intents を省略しトップレベルの summary を渡す。",
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "【こちらを優先】追加する探索方向の配列で、順に処理される。各要素のフィールドは下のトップレベルと同じ（summary/asset_ids/parent_ids/priority/chain_tags）。ids はこの配列と同じ長さ・同じ順序で返る。", "items": map[string]any{"type": "object"}},
			"summary":    str("[単条] この探索方向を一文で記述する。何をするか+なぜか。方向が書けていれば十分で、アセット id には依存しない。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "この方向でテスト/攻撃する【対象アセット id】（**できるだけ渡す**。0/1/複数可。list_assets が返すアセット id で、探索ノード id ではない）：この探索方向が向かうアセット（サイト/API/パラメータ/ホストなど）。具体的なアセットを対象とする方向なら必ず渡す。これは「この探索がどの対象を狙うか」の構造化マークで、カバレッジの重複排除と意図のアセットチェーン接続に使う。純粋に全体偵察で対象アセットが本当に無い場合のみ空にする。"},
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "上流アンカー id（任意、0/1/複数）：この方向がどの【確認済みの事実(fact)/ファインディング(finding)】から総合されたか。**既存の fact/finding ノード id のみ指定でき、意図/ゴール/ヒントは指定できない**。意図は確認済みの知識にアンカーし、見かけの計画ではなく発見駆動でなければならない。複数の事実から新たな意図を生む場合は複数渡す。最上位の全く新しい偵察方向は空にする（タスク起点の origin fact に自動で繋がる）。"},
			"priority":   intp("優先度 0〜10、既定は 5"),
			"chain_tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "攻撃面チェーンのタグ（任意、1-2 個。値は web/ad/app/priv/pwn/recon/rev/pivot/meta のみ）：この方向の主要な攻撃面でタグ付けすると、エンジンが実行 worker にそのシナリオ向けのサイドクエスチョン意思決定骨格（シナリオ→判定基準→方向転換）を注入する。迷ったらキーワードで選ぶ（sql/インジェクション→web、ドメイン/kerberos→ad、権限昇格→priv、トンネル/プロキシ/横展開→pivot、リバース/アンパック→rev、ヒープ/スタック/ctf→pwn、偵察/フィンガープリント→recon、apk/frida/ミニアプリ→app）。どうしても迷うなら空にすれば、エンジンが summary のキーワードで自動マッチする。"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // 単条モード：トップレベルの summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// 人間がメイン agent 経由で意図を直接投入 → タスクが done 済み（open ゴールの無い goalless 分岐）なら
			// running に戻し、worker がこの意図を引き受けられるようにする。resumeTask はメイン agent の Chat のみが
			// 設定する(SetResumeTask)。planner の ToolSet では nil なので、planner 自身の add_intent ではこの部分が
			// no-op となり、通常の意図生成に影響しない。意図ノードは上で(open)作成済みなので、復活時に枯渇と誤判定されない。
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // 単条：従来の返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

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
	return writeTool("goal_met", "【タスク全体を即座に終了する】——タスクの【すべてのゴールが本当に達成され、全体を締めくくれる】と確認できたときだけ呼ぶ（あくまでタスク【全体】の完了。そのうちの1つのゴール/1つの flag/1つの脆弱性を達成しただけでは【該当しない】。その場合は prove_goal でそのゴールをマークすればよい）。⚠️これは「今ラウンドの計画を終える」ためのものではない。今ラウンドに出す新しい意図が無い、worker の成果を待っている、いずれも【そのまま今ラウンドを終えればよく、このツールは呼ばない】（意図が0件なのは完全に正常）。通常の判定は prove_goal でゴールを個別に証明するのを優先し、goal_met は個別証明を省いて全体を締める手段にすぎない。",
		obj(map[string]any{"reason": str("達成理由（ゴールが本当に達成されたエビデンスであること。「今ラウンドに新方向が無い」のようなラウンド終了の理由は不可）")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// --- worker write tools ---

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "確認済みの脆弱性を記録する。evidence にはコマンド出力やログなど検証可能なエビデンスを渡す。タスクコンテキストでは現在の intent_id を渡す。返る finding_id は独立した脆弱性レコード ID、finding_node_id は探索ノード ID（先頭行はそのノード番号を保つ）。", obj(map[string]any{
		"vulnclass": str("脆弱性の分類"), "name": str("脆弱性名"), "severity": str("critical|high|medium|low"), "summary": str("ファインディングの要約"),
		"intent_id": idp("現在のタスクの意図 id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "影響を受けるアセット id。可能な限り付ける（同一エントリのマージに影響する）。空の場合、システムが報告テキスト中の URL から自動でアンカーする"},
		"evidence":         str("エビデンス/PoC テキスト"),
		"evidence_hint_id": idp("任意：本タスクでこの脆弱性に対応するヒントノード ID。その構造化 traffic_refs を自動で引き継ぐ。継承ヒントや他の脆弱性のヒントは参照できない"),
		"traffic_refs": map[string]any{"type": "array", "description": "任意。HTTP/HTTPS の脆弱性では、まず検索してリクエスト/レスポンスが本当にその結論を裏付けるか1件ずつ確認し、再現順に実際の ID を記入する。TCP など非 HTTP の脆弱性、未取得、該当レコードが見つからない場合は省略するか [] を渡す（報告は妨げない）。evidence に理由と他の検証可能なエビデンスを書いてよい。ID を推測したり、ドメイン/時刻から関連を推定したり、パケットを補うためだけに再探索したりしない。用途は baseline 正常対照 / proof 脆弱性の証明 / verification 補足検証 / supporting 補助エビデンス。",
			"items": obj(map[string]any{"traffic_id": str("traffic_search が返した実在のトラフィック ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("そのトラフィックが脆弱性の結論をどう裏付けるか")}, "traffic_id")},
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding はタスクコンテキストが必要です。プラットフォーム対話では add_task_hint で該当タスクに脆弱性を引き継ぎ、ヒントに既存の traffic_refs を載せてタスクの Agent に登録させてください。登録済みの脆弱性は bind_finding_traffic で追加バインドできます。"), nil
		}
		// Auto-binding off: ignore the evidence params instead of rejecting the call.
		// stripTrafficParameters already removes them from the advertised schema, but
		// models routinely emit fields anyway — failing here would discard a confirmed
		// finding over a stray parameter. The success path below reports evidence_status
		// "not_bound" with the "オフ。ページで手動関連付け可能" note, which is what the caller needs.
		if !findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id は有効なヒントノード ID である必要があります。引き継ぎヒントが無い場合は省略してください"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: pid(a.IntentID), VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs)}
		anchorText := strings.Join([]string{input.Name, input.Summary, input.Evidence}, "\n")
		if strings.TrimSpace(input.Name) == "" {
			input.Name = fallbackFindingName(input.VulnClass, anchorText)
		}
		// asset_ids が空なら主アセットを自動でアンカーする。段階Aの同一エントリ併合(db/finding_merge.go)は
		// (task, 正規化した vulnclass, 主アセット=AssetIDs[0]) で同一判定するため、asset_ids が空だと
		// 併合キーが全体として無効になる。アセットグラフはグローバル共有で、アンカー候補は全庫のアセット
		// (QueryByHost が SQL 層で host により粗く絞る)。task_ids の制約は受けず、無理に新規作成もしない。
		var anchoredID int64
		if len(input.AssetIDs) == 0 && t.as != nil {
			if tgt := extractAnchorTarget(anchorText); tgt.host != "" {
				if assets, aerr := t.as.QueryByHost(tgt.host, 0); aerr == nil {
					if id, _ := anchorFindingAsset(anchorText, assets); id > 0 {
						input.AssetIDs = []int64{id}
						anchoredID = id
					}
				}
			}
		}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("トラフィックエビデンスの保存が利用できません。脆弱性は登録していません"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if t.notify != nil {
			t.notify()
		}
		t.writes.Findings++
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string `json:"evidence_status"`
			EvidenceNote   string `json:"evidence_note,omitempty"`
			AutoAnchored   bool   `json:"auto_anchored,omitempty"`
			AssetID        int64  `json:"asset_id,omitempty"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if recorded.Merged {
			// 段階Aの同一エントリ併合：エビデンス/血縁は既存の confirmed 集約に追記され、システムが自動で
			// 増分再検証(C2)を起動する。同じエントリで脆弱性を重複作成しないようモデルに明示する。
			result.EvidenceNote = "この報告は既存の確認済み脆弱性と同一エントリ（同一タスク、同一の正規化カテゴリ、同一の主アセット）のため併合した。エビデンスは追記、severity は最大値を採用、血縁は既存ノードを指し、merged_into が集約先。システムは自動で増分再検証する。このため脆弱性を重複作成しないこと。"
		}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "脆弱性は保存したが、トラフィックは未バインド。TCP/パケットが無い場合はそのまま続行してよい。検証済みの HTTP トラフィックがあるなら、利用可能な bind_finding_traffic または脆弱性ページで追加バインドしてからエビデンスの引き継ぎを完了する。脆弱性を重複作成しないこと。"
			if recorded.Merged {
				result.EvidenceNote = "この報告は既存の確認済み脆弱性に併合された(merged=true)。今回は新しいトラフィックをバインドしていない。検証済みの HTTP トラフィックがあれば bind_finding_traffic で merged_into が指す脆弱性に追加バインドする。重複作成しないこと。"
			}
			if !findingTrafficBindingEnabled() {
				result.EvidenceNote = "脆弱性は保存した。Agent による自動トラフィックバインドはオフのため、ページで手動で関連付けられる。"
			}
		}
		if anchoredID > 0 {
			result.AutoAnchored = true
			result.AssetID = anchoredID
			note := fmt.Sprintf("asset_ids が空だったため、システムが報告テキストの URL から主アセット %d を自動でアンカーした（同一エントリ併合は主アセットで同一判定する）。次回の報告では asset_ids を直接渡すこと。", anchoredID)
			if result.EvidenceNote != "" {
				result.EvidenceNote = note + " " + result.EvidenceNote
			} else {
				result.EvidenceNote = note
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem は record_fact のバッチ/単条における1つの事実。
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // 1行の要点エビデンス（コマンド+要点の出力行）。結論を裏付け、後から照合しやすくする
	Confidence string            `json:"confidence"` // observed（直接見た）| inferred（現象からの推測）| confirmed（再確認で確定）
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// fact の信頼度は3段階（紅日3 の振り返り R3：自作ツールの否定的結論「445 は塞がれている」が確定事実として
// 扱われ、planner が誤った事実の上で数ラウンド空転した）。inferred と confirmed は直接観察と区別する。
const (
	FactConfidenceObserved  = "observed"  // 出力に直接現れたエビデンス（既定）
	FactConfidenceInferred  = "inferred"  // 推測/間接エビデンス/自作ツールの否定的結論
	FactConfidenceConfirmed = "confirmed" // 再確認/成熟ツールの検証で確定
)

// normalizeFactConfidence はモデルが自己申告した confidence を3段階に正規化する。空/未認識 → observed（既定段階）。
func normalizeFactConfidence(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case FactConfidenceInferred:
		return FactConfidenceInferred
	case FactConfidenceConfirmed:
		return FactConfidenceConfirmed
	default:
		return FactConfidenceObserved
	}
}

// negativeConclusionPatterns は否定的結論の保守的なキーワード検出（中国語・英語）。誤って拾う（inferred を多めに付ける）
// のは許容し、見逃さないことを優先する。命中したら observed→inferred を強制し、成熟ツールで再確認してから confirm_fact で confirmed に上げる。
var negativeConclusionPatterns = []string{
	"不可用", "被封", "无响应", "不存在", "不支持",
	"not supported", "blocked", "unreachable", "no response", "filtered",
}

// isNegativeConclusion はテキスト（summary+detail）が「X 不可用/被封/不存在」のような否定的結論かを判定する。
func isNegativeConclusion(text string) bool {
	s := strings.ToLower(text)
	for _, p := range negativeConclusionPatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// factDowngradeNote は否定的結論が自動降格されたときにツールの返却でモデルへ伝える文言。
const factDowngradeNote = "否定的結論を inferred にした（確定事実としては扱わない）。成熟ツール/直接エビデンスで再確認したら confirm_fact で confirmed に上げられる。"

// recordOneFact は fact ノードを1つ書き、意図に繋ぐ（intent→yields→fact）。defaultIntent は
// バッチ時の既定意図（この項目に intent_id が無いときに使う）。note が非空なら、ツールの返却とともに
// モデルへ伝える注意書き（現状は否定的結論が inferred に自動降格された場合のみ）。
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, string, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, "", fmt.Errorf("summary を入力してください")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	// confidence は常に保存する（既定 observed）。紅日3 の振り返り：否定的結論は inferred へ強制降格。
	conf := normalizeFactConfidence(it.Confidence)
	var note string
	if conf == FactConfidenceObserved && isNegativeConclusion(it.Summary+"\n"+it.Detail) {
		conf = FactConfidenceInferred
		note = factDowngradeNote
	}
	payload["confidence"] = conf
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, "", fmt.Errorf("intent_id は本タスクの意図である必要があります（関連タスクの意図は読み取り専用）")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, "", err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact
	}
	t.writes.Facts++
	return id, note, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "探索の【事実/結論】を探索グラフに書き込み、それを生んだ意図（intent_id）に繋ぐ。フィンガープリント/列挙などの【肯定的結論】と、「ポート閉」/「パラメータはインジェクション不可」/「ログイン入口が見つからない」などの【否定的結論】の両方を記録するために使う。\n"+
		"⚠️1回の探索で得た複数の観察は【1つの事実に集約】し、分割しない。1つにまとめられるなら極力1つで表す：summary=今回の結論をまとめた一文、detail=関連する詳細（複数の具体項目を含めてよい）。例：フィンガープリントの意図→1つの事実 {summary:'X サイトの技術スタックと応答特性を識別', detail:'nginx 1.25 / Vue3 / 200 / title=.. / body_len=..'} とし、ステータスコード・フィンガープリント・タイトルを別々に記録しない。1つの意図から出る事実は通常1つで、細かく割るとグラフが無限に膨らむ。\n"+
		"★facts 配列は【互いに異なる】結論を一度に複数書くときに使う（各項目の intent_id は省略でき、省略時はトップレベルの intent_id を使う）。ids 配列を返し、facts と同じ長さ・同じ順序。\n"+
		"⚠️ツール出力で【実際に見た】結論だけを書き、推測で補わない。evidence と confidence は不正確な結論でグラフを汚さないためのもの：\n"+
		"  · evidence=この結論を支える【1行】の要点エビデンス（コマンド+最も証明力のある1〜2行の出力）。**必ず簡潔に**。詳細は detail にあるので、ここに長い出力を貼らない。\n"+
		"  · confidence=observed（出力に直接現れた）| inferred（現象からの推測/間接エビデンス/自作ツールの否定的結論）| confirmed（再確認/成熟ツールで確定。通常は confirm_fact による昇格で、自分では付けない）。\n"+
		"  · **否定的な結論**（インジェクション不可/ポート閉/入口が見つからないなど）は「観察 + 試行的な読み」だけを書く。実際に何を見たかを述べ、方向を捨てるかは planner が全体を見て決める。evidence は必ず付け、手段を尽くしていない、またはエビデンスが弱い（1回だけ試した、それらしいだけ）場合は inferred、本当に尽くして直接見た場合だけ observed にする。\n"+
		"  · システム規則：テキストに「不可用/被封/无响应/不存在/不支持/blocked/unreachable」などの否定的結論の特徴が含まれると、observed は【自動で inferred に降格】され返却時に通知される。その後に成熟ツール/直接エビデンスで再確認して確定したら、confirm_fact で confirmed に上げる。",
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "【異なる結論が複数あるときに使う】事実の配列。要素のフィールドは下のトップレベルと同じ（summary/detail/evidence/confidence/intent_id/asset_ids）。intent_id を省略するとトップレベルの intent_id を使う。ids はこの配列と同じ長さ・同じ順序で返る。", "items": map[string]any{"type": "object"}},
			"summary":    str("今回の探索結論の【まとめの一文】（detail の要約）"),
			"intent_id":  idp("この事実を生んだ意図 id（自分が引き受けた意図。バッチ時は各項目の既定値）"),
			"detail":     str("この事実の関連詳細：今回の探索で得た複数の観察事実をここにまとめて書く"),
			"evidence":   str("【1行】の要点エビデンス：コマンド + 結論を最もよく証明する1〜2行の出力。必ず簡潔にし、長い出力を貼らない（詳細は detail）。"),
			"confidence": str("observed（出力に直接現れた）| inferred（現象からの推測/間接エビデンス/自作ツールの否定的結論）| confirmed（通常は confirm_fact での再確認・昇格。自分では付けない）。否定的結論は必ず正直に付ける。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "関連アセット id（任意、0/1/複数）：この事実が関わるアセット"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 単条モード + バッチ既定 intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // トップレベル intent_id = バッチ既定

			ids := make([]int64, len(items))
			errs := map[string]string{}
			notes := map[string]string{}
			for i, it := range items {
				id, note, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				if note != "" {
					notes[strconv.Itoa(i)] = note
				}
			}

			if !batch { // 単条：従来の返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				msg := fmt.Sprintf("fact recorded: %d", ids[0])
				if n := notes["0"]; n != "" {
					msg += "\n⚠️" + n
				}
				return actool.Text(msg), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			if len(notes) > 0 {
				out["notes"] = notes
			}
			return jsonResult(out)
		})
}

// confirmFact は inferred の事実を confirmed に上げる（紅日3 の振り返り R3 の受け皿：否定的結論が
// 自動降格された後、worker/planner が成熟ツール/直接エビデンスで再確認して確定し、本ツールで承認する）。監査
// （confirmed_by/confirmed_at を payload に保存）を伴い、fact は本タスクに属する必要がある。
func (t *ToolSet) confirmFact() actool.CoreTool {
	return writeTool("confirm_fact",
		"inferred の事実を confirmed に上げる。【その結論を成熟ツール/直接エビデンスで再確認済みの場合のみ】呼べる。例えば record_fact の否定的結論がシステムに自動降格された後、nmap/nc などの成熟ツールで再測定して確定してから本ツールを呼ぶ。入力 fact_id（graph_overview/list_facts にある本タスクの fact ノード id）。",
		obj(map[string]any{
			"fact_id": idp("confirmed に上げる事実ノード id（本タスクに属すること）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("confirm_fact は有効になっていません: ExplorationStore が初期化されていません"), nil
			}
			var a struct {
				FactID json.RawMessage `json:"fact_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.FactID)
			if id <= 0 {
				return actool.Errorf("fact_id は必須です"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindFact {
				return actool.Errorf("fact_id は本タスクの事実ノードである必要があります（関連タスクのノードは読み取り専用）"), nil
			}
			var p map[string]any
			_ = json.Unmarshal(node.Payload, &p)
			if prev, _ := p["confidence"].(string); prev == FactConfidenceConfirmed {
				return actool.Text(fmt.Sprintf("fact %d はすでに confirmed です。再確認は不要", id)), nil
			}
			if err := t.ts.ConfirmFact(id, t.worker); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if t.notify != nil {
				t.notify() // planner を起こす：inferred の事実が確定したので、計画の方向に影響しうる
			}
			return actool.Text(fmt.Sprintf("fact %d を confirmed に昇格した", id)), nil
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}

// addOneHint は hint ノード(active/human)を探索グラフに1つ付け、アセットをアンカーでき、id を返す。
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("Agent の自動トラフィックバインドはオフのため、traffic_refs を含むヒントは保存していません。システム設定で有効にするか、テキストのみで引き継いでください")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text を入力してください")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	id, err := t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
	if err == nil && t.notify != nil {
		t.notify() // wake the planner so the new hint is read promptly (debounced)
	}
	return id, err
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal は goal ノード(open)を探索グラフに1つ付け、タスクのルート(origin fact, rel spawns)に繋ぐ。
// origin は t.worker(既定 system):goals 分解器が書くものは "goals"、メイン agent の実行時は
// "human" を記録する。planner の起動は setGoals がバッチ全体を書き終えてから一括で行う(下記)。ここは保存のみを担う。
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text を入力してください")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)
	}
	return id, nil
}

// setGoals は【本タスク】に探索ゴール(goal ノード)を追加する。ゴール分解器の送信ツールでもあり、メイン
// agent の実行中にゴールを補うツールでもある。同一の管理対象ツールで、web 側で説明/schema の変更や agent 単位のバインドができる。
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"【本タスク】に探索ゴール(goal)を追加する。ゴール=最終的に納品/検証できる結果であり、攻撃ステップや偵察行動ではない。\n"+
			"★バッチを優先:複数のゴールを goals 配列に入れて一度に送信し、ids を同じ長さ・同じ順序で返す(失敗した項目は id=0、詳細は errors)。単条の場合は goals を省略しトップレベルの text を渡す。\n"+
			"vulnclass は任意:対応する脆弱性クラス(SQLi/IDOR など)。ビジネスロジック系のゴールは空にする。ゴール達成の判定はシステムが行い met を立てる。本ツールは追加のみを担う。",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【こちらを優先】追加するゴールの配列で、順に処理される。各要素：text(必須、独立して検証できる最終ゴール)+ vulnclass(任意)。ids はこの配列と同じ長さ・同じ順序で返る。", "items": map[string]any{"type": "object"}},
			"text":      str("[単条] 独立して検証できる最終ゴール"),
			"vulnclass": str("[単条] 対応する脆弱性クラス(明確な場合)。SQLi/IDOR など。ビジネスロジック系のゴールは空でよい"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals は有効になっていません: ExplorationStore が初期化されていません"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 単条モード:トップレベルの text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner を起こす(バッチで1回)。notifyGoal を優先：1回の set_goals で「人が
				// N 個のゴールを追加:…」のトリガを1件だけ記録し、1件ずつの氾濫を避ける。分解器/worker にはこのコールバックが無く → 素の notify に戻る(分解器は
				// round-0 では notify も未接続＝何もしない。このとき planner はまだ起動していない)。
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// メイン agent の実行中にゴールが追加された → 完了/一時停止したタスクを running に戻して続行させる(終端ゲートが
				// 通常の notify を飲み込むため、明示的に復活させる必要がある)。このコールバックを設定するのは mainagent のみ。分解器/worker は nil。
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 単条:従来の返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint は操作制約を1件 task_constraints に保存する。origin は t.worker(既定 system):
// 分解器は "goals"、メイン agent は "human" を書く。
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text を入力してください")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // 既定は禁止扱い:種類が未指定ならより保守的に
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type は allow または deny である必要があります")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints は【本タスク】に操作制約を追加する(allow=何をしてよいか / deny=何をしてはいけないか)。ゴール
// 分解器が round-0 で制約を抽出する送信ツールであり、メイン agent の実行中に制約を補うツールでもある。同一の管理対象ツールで、web
// 側で説明/schema を変更し agent 単位でバインドできる。制約は planner/worker のシステムプロンプトに注入され、探索の境界を定める。
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"【本タスク】に操作制約を追加し、探索の境界を定める：type=allow(許可する操作)または deny(禁止する操作)。\n"+
			"制約=「どの操作をしてよく、何をしてはいけないか」の規定(例：「現在のポートのみ試験し、他のポートはスキャンしない」「本番 DB への書き込みは禁止」「受動的偵察のみ許可」)であり、ゴールでも攻撃ステップでもない。\n"+
			"★バッチを優先:複数は constraints 配列に入れて一度に送信し、ids を同じ長さ・同じ順序で返す(失敗した項目は id=0、詳細は errors)。単条の場合は constraints を省略しトップレベルの text/type を渡す。\n"+
			"タスクのゴール/説明に【明記されている】制約だけを登録し、作り出さない。種類に迷ったら deny(より保守的)を使う。",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【こちらを優先】追加する制約の配列で、順に処理される。各要素：text(必須、1つの制約)+ type(allow|deny)。ids はこの配列と同じ長さ・同じ順序で返る。", "items": map[string]any{"type": "object"}},
			"text":        str("[単条] 1つの操作制約の内容"),
			"type":        str("[単条] allow(許可)または deny(禁止)。省略時は deny 扱い"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints は有効になっていません: ExplorationStore が初期化されていません"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 単条モード:トップレベルの text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 単条:シンプルな返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "人間/メイン agent の戦略ヒントを探索グラフに付ける。planner が次に意図を生成するときに読む。\n"+
		"★バッチを優先：複数のヒントを hints 配列に入れて一度に送信する（逐次呼び出しより往復を節約）。ids 配列を返し、hints と同じ長さ・同じ順序（失敗した項目は id=0、詳細は errors）。単条の場合は hints を省略しトップレベルの text を渡す。",
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先】追加するヒントの配列で、順に処理される。各要素のフィールドは下のトップレベルと同じ（text/asset_ids/traffic_refs）。ids はこの配列と同じ長さ・同じ順序で返る。", "items": obj(map[string]any{"text": str("ヒント内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})},
			"text":         str("[単条] ヒント内容。例：「認証後の API を重点的に掘る」"),
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "アンカーするアセット id（任意、0/1/複数）"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 単条モード：トップレベルの text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // 単条：従来の返却を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "実行中の意図(work)を1つ終了する。逸脱した/意味のない探索を止めるのに使う。終了した意図は stopped と記録され、自動で再び引き受けられることはない。まず get_worker_output で何をしているか確認してから決める。",
		obj(map[string]any{"intent_id": idp("終了する意図 id（= work ハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work は現在利用できません"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須です"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id は本タスクの意図である必要があります（関連タスクの意図は読み取り専用）"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("意図 %d の work に終了シグナルを送信した", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("stop X, focus on Y"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "実行中の意図(work)に修正指示をリアルタイムで注入する。中断せず、既存の進展も失わない。worker は次の行動の前に指示を受け取り、それに従って調整する。「X はやめて Y に集中する」のような【意図内】の修正に使う。方向そのものが誤っている場合は kill_work で止めてから新しい意図を出す。まず get_worker_output で何をしているか確認するのが望ましい。",
		obj(map[string]any{
			"intent_id": idp("修正する意図 id（= work ハンドル）"),
			"message":   str("worker への修正指示。何をやめて何に向かうかを明確に書く"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work は現在利用できません"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須です"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id は本タスクの意図である必要があります（関連タスクの意図は読み取り専用）"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message は必須です"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("意図 %d の work に修正指示を注入した（次のステップから有効）", id)), nil
		})
}

// getWorkerOutput returns a work's final (or up-to-abort) conclusion text by intent id.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "本タスクまたは直接関連タスクの意図(work)1件の最終出力の結論を取得する。関連タスクの結果は source_task_id/inherited=true を持ち読み取り専用。正常終了ならその要約を、終了(stopped)/異常の work なら中止時点までの最後の出力を返す。",
		obj(map[string]any{"intent_id": idp("意図 id（= work ハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須です"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id は本タスクまたはその直接関連タスクに属していません"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "（この work はまだ出力がありません）",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("（この work はまだ出力がありません）"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace exposes a work's execution PROCESS (not just its final output):
// list step summaries, keyword-search within one work, or pull full detail of a
// few specific steps. Thinking steps are excluded everywhere.
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return t.readExpTool("get_worker_trace",
		"ある意図(work)の【実行過程】を見る（最終結論だけを返す get_worker_output とは異なる）。3通りの使い方：\n"+
			"① intent_id のみ → その work の各ステップの要約ストリームを返す（summary≤100文字、step_id を含む。動作の輪郭のみで完全な出力は含まない）。\n"+
			"② intent_id + q → キーワードに一致したステップの要約のみを返す（要約と完全な出力の両方を検索する。返るのは summary のみで、内容は③で見る）。\n"+
			"③ intent_id + step_ids → それらステップの完全な内容(detail)を返す。1回につき最大 5 個で、超えた分は先頭 5 個のみ返し、未取得分を notice/omitted_step_ids で知らせる。\n"+
			"典型的な流れ：まず①/②で怪しいステップの step_id を特定し、③でその完全な出力を取得する。思考(thinking)ステップは含まない。直接関連タスクの履歴 trace にも対応し、その結果は source_task_id/inherited=true を持ち読み取り専用。",
		obj(map[string]any{
			"intent_id": idp("意図 id（= work ハンドル）"),
			"q":         str("キーワード。要約/完全な出力が一致したステップのみ返す（任意。step_ids とは排他）"),
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "完全な内容を取得する step_id（①/②の返り値から指定。1回につき最大 5 個で、多く渡すと先頭 5 個のみ返し、残りは omitted_step_ids に列挙する）"},
			"limit":     intp("要約ストリーム/検索の返却上限（任意）"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage   `json:"intent_id"`
				Q        string            `json:"q"`
				StepIDs  []json.RawMessage `json:"step_ids"`
				Limit    int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須です"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id は本タスクまたはその直接関連タスクに属していません"), nil
			}
			// ③ detail drill-down by step ids, thinking excluded by the store.
			if len(a.StepIDs) > 0 {
				// Dedup + drop invalid ids first so garbage/duplicates don't eat into
				// the per-call cap. detail is returned in full (untruncated), so the
				// cap bounds one tool result; over the cap we serve the first N and
				// tell the model exactly which ids were deferred, instead of erroring
				// and forcing it to re-plan the call.
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					// returned_step_ids/omitted_step_ids let the model decide programmatically
					// whether another call is worth it; the notice states the same in prose.
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"1回につき最大 %d 個のステップの完全な内容を取得できる。今回は先頭 %d 個（%v）を返し、未取得の %d 個は %v。"+
							"この内容で特定できたなら残りのステップは不要。続きが本当に必要なら、これらの step_id でもう一度呼ぶ。",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ①/② summary stream, optionally keyword-filtered; 100-char summaries.
			var acts []db.Activity
			var err error
			if strings.TrimSpace(a.Q) != "" {
				acts, err = t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
			} else {
				acts, err = t.ts.ActivityTraceWithSources(id, a.Limit)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{"intent_id": id, "steps": traceSteps(acts)}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces keyword-searches EVERY work's process in this task — for
// finding what a worker saw but never wrote back as a fact. Returns only matching
// summaries (≤100 chars), each tagged with its intent_id for follow-up drill-down.
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return t.readExpTool("search_all_worker_traces",
		"【通常は非推奨。システムが必要な情報の大部分を既に渡している】本タスクの他の work の実行過程をキーワード(q)で検索し、ある worker が見たのに fact に書かれなかったもの（あるパス/token/エラーなど）を掘り起こす。"+
			"自分自身のこの意図のステップは自動で除外される（それらは既に自分のコンテキストにある）。"+
			"一致したステップの要約(summary≤100文字)のみを返し、各項目に intent_id を付ける。そこから get_worker_trace(intent_id, step_ids=[...]) で完全な内容を取得する。",
		obj(map[string]any{
			"q":     str("キーワード（すべての work ステップの要約+完全な出力を検索）"),
			"limit": intp("返す件数の上限。既定は 100（任意）"),
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q は必須です"), nil
			}
			// 呼び出し元自身のこの意図のステップを除外する（worker 自身の trace は既にそのコンテキストにある）。
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces gives a worker (which has no graph_overview and can't see the
// intent graph) a lightweight index of the works in this task — intent_id +
// one-line summary + state — so it can DISCOVER which works to inspect via
// get_worker_trace. Without this a worker only knows intent_ids that come back
// from search_all_worker_traces hits. Excludes still-open intents (not yet run →
// no process to inspect).
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return t.readExpTool("list_worker_traces",
		"【通常は非推奨。システムが必要な情報の大部分を既に渡している】本タスクで【実行済みの work（意図）】の索引を列挙する：intent_id + 一言の方向(summary) + ステータス。"+
			"あなた(worker)は探索グラフを見られないので、これをどの work を覗く価値があるかの発見に使う。続けて get_worker_trace(intent_id) でステップを、get_worker_trace(intent_id, step_ids=[...]) で詳細を取得する。"+
			"実行済み(running/done/exhausted/blocked/stopped)のみを列挙し、まだ走っていない open は含まない。注意：あなたのタスク境界は自分が引き受けた意図のままで、他の work を見るのは観察の再利用/重複作業の回避のため。",
		obj(map[string]any{
			"q":     str("summary のキーワードで絞り込み（任意）"),
			"limit": intp("返す件数の上限。既定は 50（任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools is the read + intent-generation + goal-judgement tool set.
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		// cold-digest §6.1: restore folded cold nodes (digest body → members → detail).
		t.expandDigest(), t.expandIndex(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(),
		// confirm_fact：再確認で確定した後、inferred の事実（システムが自動降格した否定的結論）を confirmed に上げる。
		t.confirmFact(),
		// report_finding：計画中の状況判断で自ら脆弱性を確認できた場合、その場で登録できる（worker と同じツール）。
		t.addFinding(),
		// list_companies：企業一覧 + scope + アセット数を確認する（company_id の取得／帰属範囲の理解）。
		t.listCompanies(),
		// list_assets：計画時に DSL で全アセットライブラリを検索する（list_untested_assets の「範囲内の未テスト」視点と合わせ、
		// 「ドメイン/フィンガープリント/ポート/ステータスコードなどの条件で全体を検索する」能力を補う）。
		t.listAssets(),
		// add_company_scope：計画時にドメイン/IP/CIDR/ICP/キーワードをある企業のアセット範囲に含める（一致したアセットを自動で取り込む）。
		t.addCompanyScope(),
		// add_task_scope：ルートドメイン全体/企業全体/サブドメイン/IP を本タスクのテスト範囲（カバレッジの分母）に加える。
		t.addTaskScope(),
		// list_untested_assets：必要時に本タスク範囲内の未テストアセットを確認する（種類+ページング）。追加テストは自分で判断する。
		t.listUntestedAssets(),
	}
}
