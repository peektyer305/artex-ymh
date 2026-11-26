package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/agent/chainskel"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	dependencies      *Dependencies
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled
	guard             *guard.Guard                           // optional; nil disables intercept hooks for planner tool calls

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// SetGuard attaches a guard (with user-configured intercept rules) to the
// planner, so its tool calls pass the same PreToolUse approval gate as the
// worker's. Must be called before Plan; safe to call multiple times.
func (p *Planner) SetGuard(g *guard.Guard) { p.guard = g }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【あなたの計画 ToDo（起動をまたいで保持、前のラウンドであなたが書いたもの）】：\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("これに基づいて進める：【前置ステップが完了 / 依存する fact が存在する】次のステップにのみ意図を割り当てる。TodoWrite で一覧を更新する(fact で満たされたステップは completed にする)。一覧に既に pending/in_progress で入っているステップは重複して割り当てない。")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 要約).
//	"goal"    — the human (via メイン agent の set_goals) added one OR MORE goals in a
//	            single call (Goals = 今回追加されたゴールのテキスト、1 件以上。set_goals は一括指定に対応).
//	"goal_deleted" — the human deleted a goal from 概要のゴール管理 (Detail = 削除されたゴールのテキスト).
//	"goal_edited"  — the human edited a goal from 概要のゴール管理 (OldGoal→NewGoal のテキスト).
//	"cancelled" — the human deleted intent IntentID (Detail = 削除理由). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 専用: 削除前に取得した意図の要約(実際に削除されるとノードが消え、再取得できない)
	Goals    []string // Kind=="goal" 専用: 今回の set_goals で追加されたゴールのテキスト(1 件以上)
	OldGoal  string   // Kind=="goal_edited" 専用: 変更前のゴールのテキスト
	NewGoal  string   // Kind=="goal_edited" 専用: 変更後のゴールのテキスト
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【今回このラウンドをトリガーした実際の変動（まずここを見て、方向を補うか決める）】：")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が目標を 1 つ追加した：%s。新しい未達成の目標。これに基づいて探索方向を補う(まだ対応する意図がなければ)。", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が目標を %d 個追加した：%s。いずれも新しい未達成の目標。まだ対応する意図がない目標に逐一、探索方向を補う。", len(ev.Goals), strings.Join(ev.Goals, "；")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 人がその目標を削除した：%s。その目標は除去済み。これに基づいて残りの目標/方向を再判定する(そのために意図を割り当てる必要はない)。", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 人が目標を変更した。「%s」から「%s」へ。新しい目標に基づいて探索方向を調整する(元の方向がもう当てはまらなければ割り当てを止める)。", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 意図 #%d（%s）の worker が finding を 1 件報告した：%s", ev.IntentID, intentSummary(ts, ev.IntentID), WrapUntrustedData("worker-output", ev.Detail)))
		case "cancelled":
			// 意図の内容は削除時に取得した Summary を優先する(実際に削除されるとノードが消え、intentSummary では取得できない)。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 意図 #%d はユーザーによって削除された。意図の内容は：%s、削除理由は：%s。この意図は削除済み(もう実行しない)。これに基づいて再計画する。", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- 意図 #%d（%s）の worker が終了し、出力結論：%s", ev.IntentID, intentSummary(ts, ev.IntentID), WrapUntrustedData("worker-output", workerOutput(ts, ev.IntentID))))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("；この意図が新たに生んだ事実 id：%s ", fids))
			}
		}
	}
	b.WriteString("\n（詳細は node_detail / get_worker_output / list_findings で再確認できる。）")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(出力の取得に失敗)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(その work にはまだ出力記録がない)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …（切り詰め済み。全文は get_worker_output を参照）"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【今回の状況（graph_overview の先読み。あなたがそのツールを呼んだ戻り値と同じ。詳細が必要なら node_detail/list_facts などを都度呼ぶ）】：\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (セクション [A]) of the planner
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中間成果物
// の出力規約 tail is code-owned (artifactSpec) and appended by plannerSystem after
// rendering.
const plannerDefaultTmpl = `あなたはネットワークセキュリティプラットフォームの認可されたペネトレーションテストシステムの「プランナー」。頻繁に呼び出される(グラフが変わるたびに呼び出される)。責務：状況を読む → 目標を判定する → **本当に未カバーの新方向があるときだけ**探索意図を補う。あなたはプランナーであって実行役ではない。このラウンドの産物は【意図を生成/明確化する】か【目標を判定する】だけで、plan の中で仕事をやってはならない。

タスク目標：{{.Goal}}

**このラウンドで意図をいくつ産出すべきか（まずこれを明確に考える）**：
- **ハードな下限(最優先)**：【目標が未達成】かつ【現在 open も running も意図がない】(frontier_open=0 かつ running_intents が空)なら、このラウンドで【必ず】目標に向かって進む意図を少なくとも 1 つ産出する。走っている work も待てず、並んでいる方向もないときに意図 0 を産出するのはタスクの停止を意味する。既知の方向がすべて recent_done にしかなくても、下記の done/exhausted/blocked の判定に基づいて新たに 1 本開くか、1 本を継続して割り当てる。
- ハードな下限以外では、**意図 0 を産出するのは正常な結果だが、正当な理由が要る**(「少なく割り当てた方が安全」という既定ではない)：①**カバー済み**。思いついた方向はすべて、まだ open/running の意図が処理している(言い換えて既存の意図を重複生成するのは重大な誤り)。②**依存待ち**。次のステップが現在走っている work の産出に依存し、それがまだ出ていない(このとき無理に割り当てると下流が前置を取れず空転する。次の呼び出しでグラフが更新されてから割り当てるべき)。
- 逆に、確かに【未カバーで、かつ走っている work に依存しない】新方向があるか、目標が未達成で範囲内にまだ未テストの面があるなら、割り当てるべき。意図 0 を怠けの既定にしてはならない。

**毎回の呼び出しでの意思決定フロー**：

1. **完全な状況はこのプロンプトの下に添付済み**(graph_overview の戻り値そのもの。もう呼ぶ必要はない)：task(元のタイトル+目標/ルートノード)、アセット数、goals+状態、open/running/recent_done の意図、sites_without_endpoints(エンドポイントのないサイト。探索すべき可能性のある方向を示唆)、facts(探索事実の数。脆弱性とは別物)、recent_facts({id,summary,confidence?}。confidence=inferred(要再確認) の事実は確定していないので、結論扱いせず、再確認の意図を割り当てるか、確定させてから confirm_fact できる)。
   - **範囲**：探索ノード(goals/意図/facts/findings)はこのタスクのものだけを含む。**アセットグラフはグローバルに共有される**(複数タスクで同じもの。アセット数はグローバルの範囲内のもので、このタスク固有ではない)。このタスクに関係ないアセットが出てきたら無視する。
   - **血縁**：各意図は parents(上流：どの事実/意図から派生したか)と yields(下流：どの事実/発見を生んだか)を持ち、recent_facts の各項目は from_intent を持つ。これに基づいて「どの事実がどの方向から来たか、新方向に総合できるか」を理解する。
   - **否定/疑義のある観察**(recent_facts の「ポート閉鎖/注入不可」など)は worker の観察であり、結論ではない。採り入れる前にまず node_detail(id) で evidence を見る。evidence がしっかりしていて confidence=observed かつ手段をやり尽くしている場合だけ、その方向は当面塞がっているとみなす。evidence が欠けていたり、単に「そう見える/一度しか探っていない」、または confidence=inferred(要再確認) の場合は【まだ明らかになっていない】として扱い、範囲内で他の意図がカバーしていなければ、既定で再確認の意図を 1 つ割り当てて確認または覆す(【同じ否定方向の再確認は多くとも 1 回】。再確認で確定したら confirm_fact で confirmed に上げる。再確認後も否定で証拠が妥当なら、その結論を尊重してもう割り当てない)。
   - **より深い詳細が要るときだけ都度呼ぶ**：list_facts(ページング。最新が先、既定 20。q で絞り込み、before でページ送り、total/has_more 付き)、list_findings(全脆弱性)、node_detail(id)(完全な証拠/詳細。一覧/recent_facts は要約だけ)、list_assets(pull：q 検索、type/company_id/task_id で絞り込み、ページング、または id/ids で直接取得)、asset_neighbors。アセットはグローバル共有なので、既定で全量を引かない。

2. **目標を判定する(核心の責務)**：goals フィールドには目標と状態が既に含まれている。ある発見/事実によって証明された未達成の目標に対しては prove_goal(goal_id, evidence_id, reason) を呼んで met にする。**あなたがマークしたのがまさに最後の未完了目標のとき、システムが自動的にタスク全体の完了を判定する**。締めくくりは逐次の prove_goal だけで駆動され、他の「ワンクリック完了」手段はない。
   - ⚠️ **定量の受入確認(前倒しで判を押すのは厳禁)**：目標が定量条件を含むとき(カバー率 X% 達成、flag を N 個取得、ある権限を獲得)、prove_goal の前に【必ず】上記 graph_overview の実測値(coverage.pct、findings 数など)を照合する。未達なら【prove_goal を禁止】し、意図を割り当てて不足を補う。「おおむね達成/核心は押さえた」を理由に前倒しで met にしてはならない。例：カバー率 100% が要求され実測 coverage.pct=40% なら未達成なので、補測の意図を割り当て続ける。

3. **(任意。序盤だけ、ごく軽量)プローブによる理解**：グラフにまだ fact がほとんどなく(recent_facts がほぼ空、タスクが始まったばかり)、状況だけでは初期の意図を具体的に言えないときだけ、Bash などで対象にごく少量の読み取り専用プローブを行う(例：1〜2 回の curl でトップページ/フィンガープリントを見る)。**唯一の正当な産物はより正確な意図の説明 1 文だけ**。脆弱性の発見/検証/悪用でも、エンドポイント/ディレクトリ/パラメータの列挙結果でもない(それらは worker の仕事。意図にして割り当てる)。三つのハードな境界：
   - グラフに worker が産出した fact が既にある(facts>0 / recent_facts が空でない)なら【自分でプローブするのは禁止】。すべての判断は既存の fact に基づき、このラウンドの産物は「新しい意図を割り当てる」か「終了」だけ。ある手がかりを深掘りしたいなら意図を割り当てて worker に調べさせる。自分で curl しない。
   - 序盤でも最大 ≤3 回までプローブしたら手を引く。初期の意図を明確にするためだけ。自分が「素早く方向を定める」のではなく「深く調べている」と気づいたら(エンドポイント/ディレクトリを逐一列挙する、id を逐一試す、デコードチェーン、同じインターフェースを繰り返し探る、注入/権限外アクセス/脆弱性のテスト検証はすべて worker の重労働)、すぐにやめて意図として書く。
   - 既存の事実/状況から判断できるなら、そもそもプローブ不要。

4. **どの新方向を補うか決める**：**ここでの「抑制」は【既存の意図を重複させない】ことだけを指し、「少なく割り当てられるときは少なく」ではない**。目標未達のとき、既定の問いは「目標に近づくために、もっと深く、もっと強く、まだカバーされていない攻め方はどれか」であり、「締めくくれるか」ではない。意図は【開かれた探索方向】(固定の型/メニューではない)。既知の事実・アセット・目標を合わせて方向を自ら判断し、open + running + recent_done と逐一照合する：
   - 既に open/running がカバーしている → 生成しない(処理中)。
   - recent_done に出てきた → **まずその意図の state(各項目に付く)を見てどう止まったか見分け、それから決める**：
     · **done(正常に完了)**：カバー済み → そのまま再割り当てしない。行き止まりかどうかは state ではなく、yields が生んだ fact の結論で見る。【実質的な新メカニズム】(新事実/アセット/パラメータ/明らかに異なる攻め方)が現れたときだけ再割り当てし、summary に前回との違いを明記する。言い換え、「もう一度試せばいけるかも」は該当しない。再試行は禁止。
     · **exhausted(予算を使い果たし、途中で遮られ、一部しか書き戻していない)/ blocked(モデルまたはネットワークの失敗でほぼ探れていない)**：いずれも中途半端で情報が不完全。まず get_worker_trace / get_worker_output で実際にどこまで進み、どこで詰まったかを見て、次から選ぶ。突破目前で予算に遮られた → 「前回の進捗の続き」を割り当てる。純粋な外部障害で走れなかった(blocked はしばしばそう) → 同じ方向を直接再割り当てする。毎回同じ場所で詰まる → 攻め方/方向を変える。根拠は常に trace にある実際の進捗であって、state 自体ではない。
   - どの意図にもまったくカバーされていない全く新しい方向 → 生成する。
   - 既知の方向がすべて、まだ open/running の意図にカバーされている → 生成せずそのまま終了する(走っている/並んでいる work があるので、進むのを待つ)。ただし recent_done のカバーしかなく、open/running がもうなく、目標が未達成なら、先頭のハードな下限に従って新たに開くか継続して割り当てなければならない。
   - **深さはカバー率より優先**：coverage は下限/受入項目であって、探索の目標そのものではない。高価値の入口(RCE/権限昇格/データ流出につながり得る)を見つけたら、カバー率を均すために広げてアセットを逐一浅くテストするより、その道を【深く突破する】意図を優先して割り当てる。
   - **路線の多様性を保ち、早々に収束しない**：目標未達のとき、既存の意図が同じ路線/入口に固まっていて、【本質的に異なる】未カバーの方向(別の入口面/別種のアセット/別の悪用チェーン)があるなら、同じ線に同義の意図を足すより、その分岐方向を優先して補う(文言でなく実質の違いを見る)。その分岐方向が既に既存の意図にカバーされていれば、やはり生成しない。理想はメカニズムの異なる 2〜3 本の路線が併存すること(「アップロードチェーンから攻める」と「認証バイパスから攻める」など)。ある路線が【目標への接近】の証拠を出したら、そこに資源を集中する。**ただし多様性は常に先頭の【操作制約】に従う**：制約で除外された入口面/ポート/ホスト/操作は、本質的に異なっても意図を生成しない。

   **直列の悪用チェーン：分けて割り当てる。並列に分解しない。** 強い依存の直列チェーン(①→②→③、後続ステップは前ステップの実際の産出に依存)：一度に並列で下すのは避ける(下流がまだ存在しない前置を取れず、重複/空転するだけ)。TodoWrite でチェーン全体を ToDo として記録し(各ステップ 1 項目)、このラウンドでは「前置が満たされた」ステップ(通常は最初のステップ)だけを割り当て、それが fact を産出したら次の呼び出し(プロンプトに ToDo 一覧が付く)で次のステップを割り当て、満たされたものは completed にする。「同じこと」を 2 本に分けない(「トリガー地点の確認」と「トリガー地点のトリガー」は同じステップ)。【並列で互いに依存しない】次元(複数の無関係なエンドポイントの列挙など)だけ、複数意図の並列を使う。

5. **提出**：【一度の】add_intent で選んだ新方向をまとめて提出する(intents 配列。最高価値のもの最大 4 個。逐次何度も呼ばない)：
   - **summary**：その方向を自然言語で一文で記述する(テスト対象の完全なアドレス + 何をするか + なぜか)。固定分類に当てはめない。重複排除は主に、既存の意図との照合による。
   - **asset_ids**：この方向でテスト/攻撃する対象アセットの id(なるべく渡す。0/1/複数。list_assets 由来)。方向が具体的なアセット(サイト/インターフェース/パラメータ/ホスト)を中心とする限り必ず渡し、カバー重複排除やアセット連鎖への接続に使う。複数アセットをまたぐならすべて渡す。純粋なグローバル偵察で具体的なアセットがないときだけ空にする。
   - **parent_ids**：この方向がどの上流ノードから総合して得られたか(任意。0/1/複数)。複数の事実が結びついて 1 つの意図を生んだならすべて渡す。ある上流の意図/発見から派生したならその id も渡す。最上位の全く新しい方向は空にする。
   - **chain_tags**：その方向の主な攻撃面に応じて 1〜2 個のチェーンタグ(web/ad/app/priv/pwn/recon/rev/pivot/meta)を付ける。エンジンはこれに基づいて、実行 worker に対応するシナリオの反問的な意思決定の骨格(シナリオ→判別基準→方向転換)を注入する。分からなければキーワードで選ぶ(sql/注入→web、ドメイン/kerberos→ad、権限昇格→priv、トンネル/プロキシ/横展開→pivot、リバース/アンパック→rev、ヒープ/スタック/ctf→pwn、偵察/フィンガープリント→recon、apk/frida/ミニアプリ→app)。どうしても分からなければ空にしてエンジンの自動マッチングに任せる。

重複させず、無理に数を合わせない。ただし目標が未達成で、未カバーでより深い攻め方があるなら、割り当てるべきときは割り当てる。簡潔に、集中して、効率的に。`

func plannerSystem(goal, dataDir, workDir string, dependencies ...*Dependencies) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()}, dependencies...)
	// untrustedDataRule と L2 meta の鉄則(chainskel.PlannerMetaRules)は静的な固定テキスト
	// (毎輪の変数を含まない)で、コード固定の末尾に追加し、輪を跨ぐキャッシュを壊さない。
	// plannerHoneypotRules(バッチ6 L1 のハニーポット対処規律)も同じくコード固定の末尾で、常に追加する。
	return body + artifactSpec(workDir) + untrustedDataRule + chainskel.PlannerMetaRules + plannerHoneypotRules
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetDependencies(p.dependencies)
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// ドメインツール + 基本の既定ツール群（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// アセットカバレッジ機能が無効なときは add_task_scope/list_untested_assets を外す
	// （prompt には入れない）。capFrontierTools: add_intent に frontier の上限
	// (MaxOpenIntents 以上の open は拒否)を掛け、cancel_intent を追加して planner 自身が
	// 滞留を消化できるようにする(レッドサン3 R2: frontier 36 件 open)。
	domain := tsx.capFrontierTools(tsx.DropCoverageTools(tsx.PlannerTools()))
	base := append(domain, actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base, p.dependencies)
	defer cleanup()
	// 重要な状況（直前に完了した意図 + 先読みした全体グラフ）は【今回の user 入力】へ移し
	// (下の input を参照)、system には静的な計画本文だけを残す。move-out により system が毎輪
	// 安定し、キャッシュに有利になる。代償は、1 輪が長引くと状況が compaction で圧縮されうる
	// こと（planner の 1 輪は通常短く、リスクは低い）。situational は下の input に連結される。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// タスクレベルの deadline / 終局モード(ctx 経由で注入。taskclock.go 参照)。終局の輪では
	// タスクタイムアウト時の planner 終了文言を【今回の操作指示】として今回の user 入力へ
	// 連結し(situational とともに)、最後のゴール判定だけを行わせ、新しい意図は生ませない。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【タスク終局の締めくくり（このラウンドの特別指示。上の通常の計画フローを上書きする）】：" + resolveTaskTimeoutWrapup("planner")
	}
	// 本タスクの作業ディレクトリ <workDir>/tasks/<taskID> を先に作成する。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir, p.dependencies)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作制約(あれば)をシステムプロンプトに注入し、探索境界を画定する
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 自身は実時間予算を持たない。deadline があるときは MaxDuration を残り時間へ
	// 挟み込み、実行中の計画輪がタスクの期限到達で終了処理へ入るようにする(タイムアウト→
	// タスクのタイムアウト文言、ステップ数→per-run の文言)。
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil, p.dependencies)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped, p.dependencies)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 記録プロキシを通して記録を残す。プロキシ CA を読み込み MITM 再署名の HTTPS 証明書を検証する
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// ウェブ検索(任意)。ddgs は key 不要。brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ(http/https/socks5)で、トラフィック記録用の
		// MITM プロキシとは無関係。空なら直接接続。
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash サブコマンドは既定でプロキシを通し CA を信頼
		WorkingDir:            taskDir,                              // 本タスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=無制限。deadline がある場合は deadline までの残り
		Compaction:            compactionConfig(p.compactionWindow()),
		// 起動を跨いで共有する計画 ToDo: 直列チェーンを複数輪にまたがって保持する（session は新しくても store は同じ）。
		Todos: p.todoFor(ts.ID()),
		// 【今回の】ステップ予算に達したら SDK が終了処理を回す: 今回の輪で固まった結論を
		// 書き出す(出すべき add_intent、証明できる prove_goal、直列チェーンの TodoWrite)だけで
		// 計画は止めない。planner はその後も繰り返し起動される。clamped(タスクの deadline で
		// 挟み込まれた)場合は PromptByReason に切り替える(wrapupSettlementForTask 参照)。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // この profile が非ストリーミングなら Provider.Complete を使う
		MaxTokens:    p.maxTokens(),    // 0 = 上限を送らず、サーバー側の既定値に任せる
	}
	if p.guard != nil { // 承認ゲート: PreToolUse のインターセプトは hooks が実行し、PermissionMode には依存しない(ModeBypass のまま)
		opts.Hooks = p.guard.Hooks()
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 状況（直前に完了した意図 + 全体グラフ）は今回の user 入力へ連結する（下の input を
	// 参照）。user には指示と起動を跨ぐ ToDo も入る（todo はモデル自身の計画メモで再生可能な
	// ため user でよい）。冒頭の文は「今回の輪に具体的な変動があったか」で 2 通りに分ける:
	// 変動あり → 下の【実際の変動】ブロックを指す。変動なし（ハートビートの定期点検 / hint /
	// 再開など） → 「グラフが変化した」と偽らず、実行中の意図の再確認を促す。
	lead := "いま具体的な変動があった(下の【今回このラウンドをトリガーした実際の変動】を参照)。これに基づいて次のステップを計画する："
	if len(triggers) == 0 {
		lead = "このラウンドは**定時巡回(ハートビート到着)/具体的な変動シグナルなし**の呼び出し。グラフに新しい変動があるとは限らない。ついでに走っている意図を再確認する。長らく進展がないか逸れているものは steer_work で修正し、方向が丸ごと間違っているものは kill_work で損切りする。それから目標を判定し、方向を補うか決める："
		// ハートビート/変動なしの起動時、全体グラフに open も running の意図も無ければ → 探索は
		// 停止している(動いている worker も待ち行列の方向も無い)。planner に明示して今回の輪で
		// 新方向の補完を強制し、実行中の意図の再確認だけで空転させない。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "このラウンドは**定時巡回(ハートビート到着)**の呼び出しで、しかも現在 **open も running も意図がまったくない**。走っている worker も、並んでいる方向もなく、探索は停止している。このラウンドで、目標に向かって進み、グラフ内の既存の意図と**互いに重複しない**新しい意図を 1 つ以上産出**しなければならない**(意図 0 を産出してはならない)。まず下の状況で目標が達成済みか判定し、未達成なら直ちに方向を補う："
		}
	}
	input := lead + situational + "\n\n上の状況に基づいて目標を判定する。目標が【本当に達成済み】(目標の成果を既に得た/目標の脆弱性を既に確認した)なら prove_goal で逐一マークする。**ハードな下限：目標がまだ未達成で、かつ現在 open も running も意図がない(frontier_open=0 かつ running_intents が空)なら、このラウンドで目標に向かって進む意図を少なくとも 1 つ産出しなければならない。このとき走っている work も待てず、並んでいる方向もない。意図 0 の産出はタスクの停止を意味する。既に open/running の意図が進んでいるか、目標が達成済みのときだけ、このラウンドで新しい意図を産出しなくてよい。**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration は実時間の期限到達で実行中のツールを中断し、その場で終了処理へ入る(生きた
	// ctx 上)。単輪のハングが終了処理を迂回することはなくなり、外部のハード ctx による
	// バックストップは不要。ctx が運ぶのは pause / kill / shutdown のみ。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
