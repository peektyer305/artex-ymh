package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

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
		if t.findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
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
	out["recent_facts"] = append(recentFactsHot, recentFactsCold[:keepColdFacts]...) // {id, summary, from_intent, confidence?}。inferred は「(要再確認)」付き。詳細は node_detail(id)
	// cold-digest §6.1/§6.2: the folded cold region + the per-asset directory that
	// collapses independent directions, plus the dangling-lineage resolver map.
	if cds, cidx := t.coldDigestOverview(); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}]。body をそのまま読む (§6.1)
		out["cold_index"] = cidx  // [{asset, asset_id, digest_ids}]。アセット単位で方向を集約 (§6.2)
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
