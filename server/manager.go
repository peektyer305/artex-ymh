package server

import (
	"sync"

	pgdb "github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/enrich"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/traffic"
)

// Manager owns the PostgreSQL data source (asset graph + every task's exploration
// graph + config) and the in-memory set of task handles.
type Manager struct {
	dir         string
	pg          *pgdb.DB
	assets      *pgdb.AssetStore
	traffic     *traffic.Traffic       // process-wide recording proxy (may be nil)
	enrich      *enrich.Engine         // engine-side asset auto-completion (DNS/HTTP)
	interceptor *intercept.Interceptor // user-configured tool-call interception rules
	// egress はバッチ 5 B2 出口審査のプロセス単位の機密フィンガープリント集合(guard/egress.go を参照):各タスク
	// の Guard はすべてこの同じ集合を参照し(taskFromPG/agentGuard で組み立て)、server は起動時と LLM
	// profile 変更時にフィンガープリントを登録する。一度の登録が全タスクに効く。
	egress *guard.EgressGuard

	companyMu sync.Mutex // serializes task/company-scope commits with live handle registration
	// taskStateMu preserves commit order between PostgreSQL lifecycle writes and
	// their in-memory mirrors. lifecycleMu makes snapshots race-free, but without
	// this outer write lock an older request could commit first and publish last.
	taskStateMu sync.Mutex
	mu          sync.RWMutex
	tasks       map[string]*Task
	active      string
	trafficOn   bool // トラフィックキャプチャのスイッチ（デフォルト無効；settings.traffic_capture）
	llmRecOn    bool // LLM レコードのスイッチ（デフォルト無効；settings.llm_record）
	// ウェブ検索のスイッチとソース（デフォルト無効；settings.web_search_*）。brave-free は braveKey、tavily は tavilyKey が必要。
	// webSearchProxy は独立した出口プロキシ(http/https/socks5)で、トラフィックを記録する MITM プロキシとは無関係。
	webSearchOn      bool
	webSearchBackend string
	braveKey         string
	tavilyKey        string
	webSearchProxy   string
	// globalProxy is the egress proxy all target traffic routes through
	// (http/https/socks5, optional user:pass). Empty = direct. When traffic
	// capture is on it becomes the MITM's upstream; when capture is off it is
	// injected into agent bash env / WebFetch directly. See ProxyAddr.
	globalProxy string
	// taskProxy はフェーズ 3b のタスク別 MITM インスタンス管理(taskproxy.go を参照);nil = 無効
	// (ARTEX_TASK_PROXY=off またはポートプールが不正)。worker のトラフィックはタスクインスタンス → タスクトンネルを通る。
	taskProxy *taskProxyManager
}

// Settings keys the UI toggles at runtime.
const (
	settingTrafficCapture      = "traffic_capture"
	settingAgentTrafficBinding = "agent_traffic_binding"
	settingWebSearchOn         = "web_search_enabled"
	settingWebSearchBackend    = "web_search_backend"
	settingBraveKey            = "brave_search_api_key"
	settingTavilyKey           = "tavily_search_api_key"
	settingWebSearchProxy      = "web_search_proxy"
	// settingGlobalProxy is the global egress proxy for all target traffic
	// (http/https/socks5). Empty = direct. Distinct from web_search_proxy (which
	// only routes the search backend) and the per-profile LLM proxy.
	settingGlobalProxy = "global_proxy"
	settingWorkers     = "workers"
	settingLLMRecord   = "llm_record"
	// settingLLMRecordsRetentionDays は llm_records の日数単位の保持期間:デフォルト 30 日、
	// 0 = クリーンアップしない。日次クリーンアップは startLLMRecordsRetention を参照。
	settingLLMRecordsRetentionDays = "llm_records_retention_days"
	// defaultLLMRecordsRetentionDays is the retention when the setting is unset.
	defaultLLMRecordsRetentionDays = 30
	// LLM ポーリング(フェイルオーバー)。デフォルト無効。有効にすると「グローバル有効設定」を使う agent は現在の設定が
	// 使えない(残高不足/key 失効/レート制限/サービス障害)ときに自動で次の設定へ切り替える。
	// settingLLMPoolBindFallback はポーリング有効時のみ意味を持つ:デフォルト無効、つまり agent/タスクが明示的に
	// ある設定を紐付けたらそれだけを使い、失敗は失敗のまま;有効にすると紐付けた設定の失敗もポーリングチェーンに落ちる。
	settingLLMPoolOn           = "llm_pool_enabled"
	settingLLMPoolBindFallback = "llm_pool_bind_fallback"
	// タスク並列上限:スイッチ + 上限数。デフォルト無効;有効時のデフォルト上限は 5(defaultConcurrencyLimit を参照)。
	settingConcurrencyOn    = "task_concurrency_enabled"
	settingConcurrencyLimit = "task_concurrency_limit"
	// defaultWebSearchBackend is used when web search is on but no backend was picked.
	defaultWebSearchBackend = "ddgs"
	// deepSeekWebSearchBackend borrows the active LLM profile instead of its own
	// key, so it only works on an anthropic-format profile pointed at DeepSeek.
	deepSeekWebSearchBackend = "deepseek"
	// defaultWorkers is the concurrent work-agent count when the setting is unset.
	defaultWorkers = 3
	// defaultConcurrencyLimit is the simultaneous-running-task cap when the feature
	// is enabled but no explicit limit was saved.
	defaultConcurrencyLimit = 5
	// settingTarpitFetchBudget はバッチ 5 B3 tarpit 遮断の host ごとの取得予算(デフォルト 40、
	// guard.defaultTarpitFetchBudget を参照);settingWebUAPool はバッチ 5 B4 のクライアント UA プール
	// (JSON 文字列配列、空なら agent.DefaultUAPool を使う)。
	settingTarpitFetchBudget = "tarpit_fetch_budget"
	settingWebUAPool         = "web_ua_pool"
)
