package server

import (
	"context"
	"log"

	"github.com/Autumn-27/artex/agent"
)

func New(ctx context.Context, m *Manager, skillDir string, dataDir string, keyDir string) *Server {
	key, err := loadOrCreateJWTKey(keyDir, dataDir)
	if err != nil {
		log.Fatalf("[auth] JWT key: %v", err)
	}
	s := &Server{agentDeps: &agent.Dependencies{}, m: m, engine: NewEngine(m), ctx: ctx, skillDir: skillDir, jwtKey: key, chatBusy: map[string]bool{},
		chatCancel: map[string]context.CancelCauseFunc{}, triggerQ: map[string][]triggeredRun{},
		triggerActive: map[string]int{}, triggerCfg: map[string]triggerBehavior{},
		profChatAgents: map[int64]*agent.ChatAgent{},
		provByProfile:  map[int64]*provEntry{}, llmHealth: newLLMHealthRegistry(m.pg),
		taskAgents: map[string]*taskAgentBundle{}, archiveWake: make(chan struct{}, 1)}
	s.initSideQuestions()
	s.initStage(dataDir)        // 管理下ファイル投入の一時保存(F13);失敗時は無効化に縮退し、起動には影響しない
	checkToolsManifest(dataDir) // フェーズ 6 外部ツール一覧の自己チェック(欠落は常態、サマリログのみ)
	// サーキットブレーカーのしきい値/クールダウンは失敗経路のホットパラメータ。起動時にグローバル再試行ポリシーを Registry へ 1 回渡し、
	// 以降はポリシー保存のたびに渡す(saveLLMRetryPolicy)。
	s.applyRetryPolicy()
	// Every task uses a stable task router. An empty explicit chain is resolved by
	// that router through Agent bindings and then the global provider, so adding a
	// first chain to a running task takes effect on its very next LLM call.
	s.engine.SetAuthoritativeAgentResolver(func(t *Task) (*agent.Planner, *agent.Worker) {
		if !s.taskRuntimeAvailable(t, "planner", "worker") {
			return nil, nil
		}
		b := s.agentsForTask(t)
		return b.pl, b.wk
	})
	// Global readiness (Ready()/llm_configured): a global active LLM provider is
	// installed. Task-level runnability is separate (ReadyFor → the resolver above).
	s.engine.SetReadiness(func() bool {
		s.cfgMu.Lock()
		defer s.cfgMu.Unlock()
		return s.llmOn
	})
	// Wire DB-stored prompt templates into the agents (新方式 §3.3 / §5a). With no
	// override row, agents keep their built-in defaults — behavior is unchanged.
	if m.pg != nil {
		s.agentDeps.PromptOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil {
				return "", false
			}
			t, err := m.pg.CurrentPrompt(a.ID)
			if err != nil || t == "" {
				return "", false
			}
			return t, true
		}
		// Wire DB-stored wrap-up (settlement) prompts. Empty column → built-in default.
		s.agentDeps.WrapupOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.WrapupPrompt == "" {
				return "", false
			}
			return a.WrapupPrompt, true
		}
		// Wire DB-stored wrap-up turn budgets. 0 / missing → built-in per-agent default.
		s.agentDeps.WrapupMaxTurnsOverride = func(key string) (int, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.WrapupMaxTurns <= 0 {
				return 0, false
			}
			return a.WrapupMaxTurns, true
		}
		// Wire DB-stored TASK-TIMEOUT wrap-up prompt/turns (worker/planner). Empty/0 → default.
		s.agentDeps.WrapupTaskTimeoutOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.TaskTimeoutWrapupPrompt == "" {
				return "", false
			}
			return a.TaskTimeoutWrapupPrompt, true
		}
		s.agentDeps.WrapupTaskTimeoutTurnsOverride = func(key string) (int, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.TaskTimeoutWrapupMaxTurns <= 0 {
				return 0, false
			}
			return a.TaskTimeoutWrapupMaxTurns, true
		}
		wireAgentAugment(m.pg, s.skillDir, s.hostTools, s.agentDeps) // 可視 skills/MCP + トラフィック/編成 host ツールを agent のツールセットへ組み込む
		domainReg := buildDomainReg(m.Assets(), s.agentDeps)
		wireTools(m.pg, domainReg, s.agentDeps) // 組み込みツール表: agent ごとの絞り込み + 説明/schema の上書き + 既定値の注入
		seedPrompts(m.pg)                       // 組み込み agent の既定プロンプト本文を agent_prompts へ投入(空のときのみ)
		s.seedOrchestrationTools()              // P2 タスク横断の編成ツールを tools 表へ投入(agent ごとに紐付け可)
		s.seedStageShareTool()                  // F13 管理下ファイル投入ツールを tools 表へ投入(既定で worker に紐付け)
		s.initSessions()                        // フェーズ 1a フットホールドセッションサブシステム: Registry + 起動時復元(DB 上の生存セッション)
		s.seedSessionTools()                    // セッションツールを tools 表へ投入(既定で worker に紐付け)
		s.initCredentials()                     // フェーズ 2 認証情報の一級エンティティ: 永続化アクセス層(secret AES-GCM)
		s.seedCredentialTools()                 // 認証情報ツールを tools 表へ投入(既定で worker に紐付け)
		s.initTunnels(dataDir)                  // フェーズ 3a 多層プロキシトンネル: Manager + 孤立分を error に + ヘルスチェック
		s.seedTunnelTools()                     // トンネルツールを tools 表へ投入(既定で worker に紐付け)
		s.initReverse(dataDir)                  // フェーズ 5 リバース handler: penelope 管理下プロセス + 前期の reverse セッションを dead と正直に記録
		s.seedReverseTools()                    // reverse_listen ツールを tools 表へ投入(既定で worker に紐付け)
		if err := s.seedFindingRetester(); err != nil {
			log.Printf("[retester] seed: %v", err)
		}
		if err := s.seedFindingVerifier(); err != nil { // 反証検証 agent(要件二 案甲)
			log.Printf("[verifier] seed: %v", err)
		}
		go s.evidenceStore().RunGC(s.ctx)
		s.seedPythonInterpreter()     // カスタムスクリプトツール: 起動時に python インタプリタを検出して登録(空のときのみ)
		go newScheduler(s).Run(s.ctx) // P3 トリガーのスケジューリング(定時/finding/ゴールイベント)、カスタム agent のみ
		// Fill the tool cache for any enabled MCP that has none yet (notably the
		// seeded browser MCP on first run). Async so it never blocks startup.
		go s.discoverEmptyMCPsOnStartup()
		logSink.SetDB(ctx, m.pg) // restore last 100 log rows and enable async persistence
	}
	// precedence: persisted DB config > env.
	if cfg, ok := s.loadLLMConfig(); ok {
		if err := s.applyLLM(cfg); err != nil {
			log.Printf("[engine] saved LLM config init failed — engine idle: %v", err)
		} else {
			log.Printf("[engine] LLM configured from DB: %s / %s", cfg.Provider(), cfg.Model)
		}
	} else if cfg, ok := agent.FromEnv(); ok {
		if err := s.applyLLM(cfg); err != nil {
			log.Printf("[engine] env provider init failed — engine idle: %v", err)
		} else {
			log.Printf("[engine] LLM configured from env: %s / %s", cfg.Provider(), cfg.Model)
		}
	} else {
		log.Printf("[engine] no LLM provider configured — engine idle until set via /api/llm or env")
	}
	s.restoreTaskRuntimes()
	go s.reconcileConcurrency()
	s.startTaskArchiveWorker()
	s.wireInterceptReviewer() // LLM 最終防衛の承認: インターセプトルールに該当しないコマンドはモデルに判定させる
	// バッチ 5 B2 出口審査: 起動時にプロセス共有の EgressGuard へプラットフォーム機密のフィンガープリントを登録
	// (LLM key/jwt.key/gate パス/PG パスワード/callback_addr)、全タスクの guard に即時反映。
	s.registerEgressFingerprints()
	return s
}

// Restored deadline and worker loops must inherit the same context as new tasks,
// including the side-question checkpoint publisher installed during startup.
func (s *Server) restoreTaskRuntimes() {
	m := s.m
	// reload tasks persisted on disk so the task list survives a restart, and
	// restore persisted paused state (so a task paused before restart stays paused).
	for _, t := range m.LoadExisting() {
		lifecycle := t.lifecycleSnapshot()
		// clear stale 'running' intents from a prior crash/restart (no live worker
		// owns them) so they re-claim instead of spinning forever in the UI.
		if n, _ := t.Store.ResetRunningIntents(); n > 0 {
			log.Printf("[engine] task %s 残留していた running 意図 %d 件を open に戻しました", t.ID, n)
		}
		if lifecycle.Paused {
			s.engine.Pause(t.ID, agent.AbortPausedOnReload)
		}
		// タスク単位タイムアウト: 未終端で timeout を持つタスクごとに deadline コーディネータを起動し、planner/worker の
		// loop とは独立させる。非アクティブなタスクも再起動後に期限到達で終了処理できる(期限超過なら即座に終了処理手順へ)。
		if !isTerminalStatus(lifecycle.Status) {
			s.engine.startDeadlineCoordinator(s.ctx, t)
		}
	}
	// Restore every task that had already been admitted before shutdown. Starting
	// only the active UI task left other non-queued tasks counted as concurrency
	// occupants without live loops, which could permanently block the persistent
	// FIFO. Paused loops remain idle; queued tasks are admitted below as slots allow.
	for _, t := range m.List() {
		lifecycle := t.lifecycleSnapshot()
		if !lifecycle.Queued && !isTerminalStatus(lifecycle.Status) {
			s.engine.Run(s.ctx, t)
		}
	}
}
