package server

import (
	"context"
	"sync"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/artex/session"
	stagepkg "github.com/Autumn-27/artex/stage"
	"github.com/Autumn-27/artex/tunnel"
	"github.com/Autumn-27/norma/llm"
)

// BuildVersion is the backend application version, injected from cmd/artex at
// startup (which in turn gets it from -ldflags "-X main.version=<tag>").
// Defaults to "dev" for local builds. Exposed to the frontend via GET /api/health.
var BuildVersion = "dev"

// Server exposes the ARTEX backend over a JSON HTTP API for the shadcn/ui
// frontend.
type Server struct {
	agentDeps *agent.Dependencies
	m         *Manager
	engine    *Engine
	ctx       context.Context

	skillDir  string              // root directory for skill subdirectories
	jwtKey    []byte              // HS256 signing key loaded from / generated into dataDir/jwt.key
	gate      *Gate               // 対偵察偽装ゲート(F14、gate.go 参照);nil = 無効
	stage     *stagepkg.Manager   // 管理下ファイル投入の一時保存(F13、stage.go 参照);nil = 縮退して無効
	sessReg   *session.Registry   // フットホールドセッションレジストリ(フェーズ 1a、session.go 参照);nil = DB 未準備
	sessStore *db.SessionStore    // セッション永続化アクセス層(secret AES-GCM、鍵は jwtKey から派生)
	credStore *db.CredentialStore // 認証情報永続化アクセス層(フェーズ 2、secret AES-GCM、sessions とは別のドメインタグ)
	tunnels   *tunnel.Manager     // 多層プロキシトンネル(フェーズ 3a、tunnel.go 参照);nil = DB 未準備
	reverse   *PenelopeHandler    // リバースシェル handler(フェーズ 5、reverse.go 参照);nil = DB 未準備
	tickets   sseTicketStore      // SSE ワンタイムチケット(F6、auth.go 参照)、ゼロ値で使用可
	authkv    authKV              // auth 永続層の注入(テスト用);nil = m.pg を使用

	// concMu serializes concurrency-cap decisions (admission + reconcile) so a
	// scheduler tick and an HTTP settings change / task creation can't both count
	// free slots off the same snapshot and over-promote past the limit.
	concMu sync.Mutex

	cfgMu     sync.Mutex
	chatAgent *agent.ChatAgent // conversational runner for the chat page; nil w/o LLM
	// llmProv is the fully-decorated global provider (recorder + failover chain)
	// installed by applyLLM. Task routers use it after an explicit chain is cleared.
	llmProv   llm.Provider
	llmDirect llm.Provider // concrete global provider, before any failover pool
	llmCfg    agent.Config // current LLM config (key not exposed)
	llmOn     bool
	llmProf   string // active LLM profile name (for llmrec tagging)

	// chatBusy guards the per-task main-agent run: the chat handler launches the
	// agent on the server's background ctx (not the request ctx) and returns
	// immediately, so a page reload / proxy timeout can't abort a live run. This
	// map serializes turns per task — one main-agent run at a time per task, so
	// concurrent messages don't corrupt the shared exp<id>-main transcript.
	chatMu   sync.Mutex
	chatBusy map[string]bool
	// chatCancel holds the cancel func for each in-flight conversation run (keyed by
	// convBusyKey), so a manual stop can abort JUST that session's agent run. Set/
	// cleared alongside chatBusy under chatMu. Aborting a run does NOT touch the P3
	// trigger queue — the drain goroutine simply proceeds to the next queued fire.
	chatCancel map[string]context.CancelCauseFunc

	// triggerQ buffers P3 trigger fires PER AGENT. A per-agent "pump" launches runs up
	// to a concurrency limit derived from the agent's 戦略: serial → limit 1 (+ optional
	// merge); parallel → limit = trigger_max_parallel (0=∞), no merge. triggerActive
	// counts in-flight runs per agent (replaces a boolean drain flag); a run's
	// completion decrements it and re-pumps to fill the freed slot. triggerCfg caches
	// the agent's last-read 戦略 so the pump never queries the DB while holding queueMu.
	// Distinct agents always run concurrently. Queue is in-memory (matches chatBusy); a
	// restart drops pending fires — the scheduler re-fires from watermarks next tick.
	queueMu       sync.Mutex
	triggerQ      map[string][]triggeredRun
	triggerActive map[string]int
	triggerCfg    map[string]triggerBehavior

	// profChatAgents caches a per-profile ChatAgent (chat page), keyed by profile id.
	// Built lazily on first use; invalidated when any profile is saved/activated/deleted
	// so edits take effect.
	profMu         sync.Mutex
	profChatAgents map[int64]*agent.ChatAgent // per-profile ChatAgent cache (chat page)

	// provByProfile caches ONE provider per LLM profile id so every agent bound or
	// pinned to the same profile shares a single provider instance — hence one rate
	// limiter. applyLLM also resolves the persisted global-active profile through this
	// cache, so global fallback and task chains do not accidentally double the
	// configured request rate. Cleared on profile edits, then repopulated by active reapply.
	provCacheMu   sync.Mutex
	provByProfile map[int64]*provEntry
	provCacheGen  uint64

	// llmHealth is the process-wide circuit-breaker state for LLM failover (ポーリング).
	// It deliberately lives OUTSIDE the provider caches: rebuilding the chain
	// (saving an unrelated profile, flipping a setting) must not erase what we
	// learned about which backends are out of credit / rate-limited.
	llmHealth *llmpool.Registry

	// Explicit task chains use one task-scoped dynamic provider shared by goals,
	// planner, workers, and the main agent. Bundles are stable; their runtime reads
	// the persisted current profile at every new LLM call.
	taskAgentMu sync.Mutex
	taskAgents  map[string]*taskAgentBundle

	// Cold task archives run through one persistent FIFO worker. The buffered wake
	// channel coalesces enqueue bursts; the database remains the source of truth.
	archiveWake chan struct{}
	archiveWG   sync.WaitGroup
	side        *sideQuestionState
}

// provEntry is a cached provider + its config for one LLM profile id.
type provEntry struct {
	prov llm.Provider
	cfg  agent.Config
}

// triggeredRun is one queued P3 trigger fire awaiting its turn for an agent.
// taskID + mergeable let the drainer coalesce several event triggers (finding/goal)
// from the SAME task into one conversation before it starts (interval fires don't merge).
type triggeredRun struct {
	agentKey  string
	title     string
	message   string // イベント本文(トリガー文 + ツール/入力/戻り値など);タスク説明/ゴールヘッダーは含まない
	taskID    int64  // source task for finding/goal triggers; 0 for interval/none
	taskDesc  string // タスク説明(タスク単位、同一タスクで共通);マージ時は 1 回だけ描画
	taskGoal  string // タスクゴール(タスク単位、同一タスクで共通);マージ時は 1 回だけ描画
	mergeable bool   // true for finding/goal event triggers (merge by taskID)
}

// SetGate は対偵察偽装ゲートをインストールする(gate.go の NewGate の戻り値;nil = 素通し)。
// ゲートは requireAuth の外層にあり、ゲートを通過すれば JWT の流れは従来どおり。
func (s *Server) SetGate(g *Gate) {
	s.gate = g
	// バッチ 5 B2: 入口パスはプラットフォームの機密情報。ゲートの設置/変更後に出口審査のフィンガープリントを再登録する。
	s.registerEgressFingerprints()
}
