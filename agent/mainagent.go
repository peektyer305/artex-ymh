package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	killWork        func(intentID int64) error             // engine callback: kill a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	guard           *guard.Guard                           // optional; nil disables intercept hooks for main-agent tool calls
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// SetKillWork wires the engine callback that lets the main agent's kill_work
// tool terminate a running work (nil = tool off). The callback carries the
// killed_by_mainagent named cause (server wires engine.KillWorkAs).
func (m *MainAgent) SetKillWork(fn func(intentID int64) error) { m.killWork = fn }

// SetGuard attaches a guard (with user-configured intercept rules) to the main
// agent, so its tool calls pass the same PreToolUse approval gate as the
// worker's. Must be called before Chat; safe to call multiple times.
func (m *MainAgent) SetGuard(g *guard.Guard) { m.guard = g }

// mainAgentDefaultTmpl is the built-in EDITABLE body (section [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the
// intermediate-artifact output spec tail is code-owned (artifactSpec), appended
// after rendering.
const mainAgentDefaultTmpl = `あなたは認可されたペネトレーションテストシステムの「メイン Agent」で、人間のオペレーターとのインターフェースである。あなた自身は探索を行わず、意図を自律的に連続生成もしない（それはプランナーの仕事である）。あなたの役割：

1. 観察：graph_overview / list_findings / list_facts / list_assets / get_worker_output を使って、現在の進捗に関する人の質問に回答する。
2. 操舵（人の意図をシステムへ反映する）：
   - 「方向を変える/特定の脆弱性を強調する/特定の領域を重点的に」と望む場合 → add_hint でヒントを書く（プランナーが次回読む）。
   - 「今すぐ特定の対象をテストする」と望む場合 → add_intent で高優先度の意図（priority 8-10）を直接注入する。システムは完了済みのタスクを自動的に実行中状態へ戻し、worker にこの意図を受け取らせて実行させ、完了したら完了済み状態へ戻す。
     **タスクの目標がすべて達成済みの場合**（graph_overview の goals がすべて met）：配信前に、この意図の背後に「新たに達成すべき結果」が含意されていないか判断する。含意されている場合は、推測した目標を一文で人に伝え直し、**正式な目標として登録するかを確認する**。人が望むなら → set_goals で登録する（タスクはその後通常の計画に進み、プランナーが自律的に先へ進める）。人が望まない場合 / 一時的に調べたいだけの場合 → add_intent でこの 1 件だけ配信し、worker が実行を終えたらタスクは完了済み状態へ戻る（自律的には継続しない）。この意図が明らかに一回限りの確認で新たな目標を含意しない場合は、直接 add_intent するだけでよい。毎回尋ねる必要はない。
   - 「実行中の意図 (work) をリアルタイムで軌道修正する（X はやめ、Y に集中）」と望む場合 → steer_work を使う（中断せず、既存の進捗を失わず、worker の次のアクション前に有効になる）。まず get_worker_output で何をしているか確認する。方向が完全に誤っている場合は add_intent で新しい意図を別途配信する。
   - 「実行中の意図 (work) を直ちに終了する」（脱線した/価値がなくなった）と望む場合 → kill_work を使う（直ちに終了し、stopped とマークして再取得しない。steer_work との違いは、steer は中断せず軌道修正のみである点）。まず get_worker_output で何をしているか確認してから実行する。
   - 「まだ開始していない (open) 意図を取り消す」（frontier 内の期限切れ/重複した滞留）と望む場合 → cancel_intent を使う（未開始のものだけを取り消し、理由を残す。実行中のものは kill_work を使う）。「【スケジュール】P0 意図 #N が X 分間取得されず待機中」というプロンプトを見たときは、その意図を優先して裏付けを確認する。実施する価値があれば steer/add_intent で指名して進めるか人に確認を促し、価値がなければ cancel_intent で削除する。
   - 「達成すべき最終目標を新しく追加する」と望む場合 → set_goals で目標を追加する。システムはその目標をタスクグラフへ書き込み、**完了済み/一時停止中のタスクを自動的に実行中状態へ戻して続行する**（プランナーはその後、これに基づいて達成済みかを再判定する）。人手で再開を押す必要はない。
   - 「テスト制約を追加/変更する（特定の操作を許可/禁止。例：『現在のポートのみテスト』『ブルートフォース禁止』『受動偵察のみ』）」と望む場合 → set_constraints で登録する（type=allow 許可 / type=deny 禁止）。制約は次の計画ラウンドで planner/worker のプロンプトに注入され、探索の境界を定める。総覧の「制約管理」でも追加・削除・変更できる。
3. 平易な言葉で簡潔に回答し、何をしたかを説明する。

現在のタスク目標：{{.Goal}}

発見をでっち上げない。ツールが返す実際のデータだけに基づいて回答する。`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // add_hint wakes this task's planner (debounced)
	tsx.SetResumeTask(resume)     // set_goals が目標を追加 → 完了/一時停止中のタスクを running に戻す
	tsx.SetNotifyGoal(notifyGoal) // set_goals が目標を追加 → planner に「人が目標を追加:…」を記録して起動
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	tsx.killWork = m.killWork     // enable kill_work tool (nil = unavailable)
	// ドメインツール + 基本の既定ツールセット（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// アセットカバレッジ機能が無効なときは add_task_scope/list_untested_assets を除外する（prompt に入れない）。
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// 本タスクの作業ディレクトリ <workDir>/tasks/<taskID> を先に作成する。
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 記録プロキシ経由で痕跡を残す;プロキシの CA を読み込み、MITM で再署名された HTTPS 証明書を検証する
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// ウェブ検索（任意）。ddgs はキー不要、brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ（http/https/socks5）で、記録用の MITM プロキシとは
		// 関係しない。空なら直接接続する。
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash の子コマンドは既定でプロキシ経由 + CA を信頼
		WorkingDir:            mainDir,                              // 本タスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // セッション単位の一時 ToDo（TodoWrite）。計画用のみで、終了時に破棄
		// 予算（ステップ数）に達したら SDK が終了処理を実行:ユーザーへ進捗の一言まとめを出力する。
		// Prompt と終了処理のターン数は管理画面で編集できる（既定 10 ターン）。
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // この profile が非ストリーミングなら Provider.Complete を通る
		MaxTokens:    m.maxTokens(),    // 0 = 上限を送らず、サーバ側の既定値に任せる
	}
	if m.guard != nil { // 承認ゲート：PreToolUse のインターセプトは hooks が実行し、PermissionMode に依存しない（ModeBypass のまま）
		opts.Hooks = m.guard.Hooks()
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
