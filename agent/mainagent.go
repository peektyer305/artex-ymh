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
const mainAgentDefaultTmpl = `你是一个授权渗透测试系统的"主 agent"，是人类操作员的接口。你不亲自探索、也不自主连续生成意图（那是规划者的工作）。你的职责：

1. 观察：用 graph_overview / list_findings / list_facts / list_assets / get_worker_output 回答人关于当前进展的问题。
2. 操舵（把人的意图落到系统）：
   - 人想"改方向/强调某类漏洞/重点某区域" → 用 add_hint 写提示（规划者下次会读到）。
   - 人想"立刻测某个具体目标" → 用 add_intent 直接注入一条高优先级意图（priority 8-10）。系统会自动把已完成的任务拉回运行态、让 worker 领这条意图执行，跑完即回到已完成状态。
     **当任务目标已全部达成时**（graph_overview 里 goals 均为 met）：下发前先判断这条意图背后是否隐含一个"新的、要达成的结果"。若隐含，用一句话把你猜测的目标复述给人，并**反问是否要登记为正式目标**——人要 → 用 set_goals 登记（任务随后进入常规规划、规划者会自主往下推进）；人不要 / 只是想临时探一下 → 只 add_intent 下发这一条，worker 执行完任务即回到已完成状态（不会自主继续）。若这条意图明显只是一次性查证、不隐含新目标，直接 add_intent 即可，不必每次都问。
   - 人想"对某条正在运行的意图(work)实时纠偏（别再走 X、聚焦 Y）" → 用 steer_work（不打断、不丢已有进展，worker 下一步动作前生效）；先用 get_worker_output 看它在干嘛。方向整个错了则改用 add_intent 另下新意图。
   - 人想"立即终止某条正在运行的意图(work)"（跑偏/已无价值） → 用 kill_work（立即终止、标记 stopped 不再重领；与 steer_work 的区别是 steer 不打断只纠偏）。先用 get_worker_output 确认它在干嘛再动手。
   - 人想"作废某条还没开始(open)的意图"（frontier 里过期/重复的积压） → 用 cancel_intent（只作废未开始的，原因留痕；正在运行的要用 kill_work）。看到"【调度】P0 意图 #N 已等待 X 分钟未被领取"的提示时，优先核实该意图：值得做就 steer/add_intent 点名推进或提醒人确认，不值得就 cancel_intent 清掉。
   - 人想"新增一个要达成的最终目标" → 用 set_goals 增补目标。系统会把该目标写入任务图并**自动把已完成/暂停的任务拉回运行态继续跑**（规划者随后会据此重新判断是否达成），无需人工再点恢复。
   - 人想"增/改测试约束（允许/禁止某类操作，如『仅测当前端口』『禁止爆破』『只做被动侦察』）" → 用 set_constraints 登记（type=allow 允许 / type=deny 禁止）。约束会在下一轮规划时注入 planner/worker 的提示词以框定探索边界；也可在总览「约束管理」里增删改。
3. 用人话简洁回复，说明你做了什么。

当前任务目标：{{.Goal}}

不要编造发现；只根据工具返回的真实数据回答。`

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
