// Package db is the PostgreSQL data source for ARTEX (旧 graph 単一ファイル SQLite の置き換え)。
// 接続を開き、schema を適用し、組み込み agent と変数カタログを seed する。
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver ("pgx")
)

//go:embed schema.sql
var schemaSQL string

const schemaMigrationLockKey int64 = 7337741001

var schemaDeadlockRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func applySchemaWithRetry(ctx context.Context, execer schemaExecer, sleep func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		if _, err := execer.ExecContext(ctx, schemaSQL); err != nil {
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return err
			}
			sleep(schemaDeadlockRetryDelays[attempt])
			continue
		}
		return nil
	}
}

// withSchemaMigrationLock pins the session-level lock to one checked-out
// connection. Running pg_advisory_lock through *sql.DB is incorrect because a
// later schema or unlock call may use a different pooled PostgreSQL session.
func withSchemaMigrationLock(ctx context.Context, sqlDB *sql.DB, action func(*sql.Conn) error) (err error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaMigrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("advisory unlock: %w", unlockErr)
		}
	}()
	return action(conn)
}

// coordinateWithSchemaMigration makes long, multi-table archive transactions
// mutually exclusive with startup DDL while allowing ordinary runtime queries
// to continue normally.
func coordinateWithSchemaMigration(tx *sql.Tx) error {
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("coordinate with schema migration: %w", err)
	}
	return nil
}

// DSN resolves the PostgreSQL connection string and reports where it came from.
// Precedence: env ARTEX_PG_DSN > config file (config.json). There is no
// built-in default — it errors if neither source is configured.
func DSN() (dsn, source string, err error) {
	return config.PostgresDSN()
}

// Probe は軽量な読み取り専用の事前確認（artex doctor 用）：接続 + ping、通れば
// ついでに LLM profile 数を読み取り専用で数える。Open と違い、ここでは移行/seed/書き込みを一切しない。
// 戻り値 (llmProfiles, err)：err が非 nil なら接続失敗。llmProfiles < 0 は接続できたが
// 取得できない場合（DB 未初期化で llm_profiles テーブルが無い等）。
func Probe(dsn string) (int, error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return -1, err
	}
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return -1, err
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM llm_profiles`).Scan(&n); err != nil {
		return -1, nil
	}
	return n, nil
}

// DB wraps the shared *sql.DB. PG handles its own connection pool + concurrency
// (MVCC), so unlike the old SQLite store there is no process-wide write mutex.
type DB struct{ *sql.DB }

// ensureDatabase connects to the postgres system database and creates the target
// database if it does not exist. dsn must be a postgres:// URL.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil // unparseable DSN — let the normal Open fail with a clear error
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" || dbName == "postgres" {
		return nil
	}
	// connect to the postgres maintenance database instead
	adminDSN := *u
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("pgx", adminDSN.String())
	if err != nil {
		return nil // best-effort; let Open surface the real error
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return nil
	}
	var exists bool
	_ = admin.QueryRow(`SELECT true FROM pg_database WHERE datname=$1`, dbName).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(`CREATE DATABASE "` + dbName + `"`); err != nil {
			return fmt.Errorf("create database %q: %w", dbName, err)
		}
	}
	return nil
}

// Open connects, applies the schema (idempotent), and seeds builtin rows.
func Open(dsn string) (*DB, error) {
	if err := ensureDatabase(dsn); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping postgres (%s): %w", config.Redact(dsn), err)
	}
	d := &DB{sqlDB}
	// pgx runs multi-statement Exec via the simple protocol when there are no args.
	// Keep the dedicated lock connection checked out until both DDL and seeding
	// finish so concurrent application instances cannot initialize out of order.
	err = withSchemaMigrationLock(context.Background(), sqlDB, func(conn *sql.Conn) error {
		if err := applySchemaWithRetry(context.Background(), conn, time.Sleep); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := d.seedBuiltins(); err != nil {
			return fmt.Errorf("seed builtins: %w", err)
		}
		return nil
	})
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// builtinAgent describes one of the fixed agents and its prompt-variable catalog.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 行作成時のデフォルト対話型 shell スイッチ；ON CONFLICT はユーザーの後からの手動切り替えを上書きしない
	runSeconds            *int // 行作成時の 1 回の run のウォールクロック上限（秒）；nil=シード既定（1200）、0=無制限
}

type promptVar struct{ name, desc, example, source string }

// intp は v へのポインタを返し、builtinAgent の任意フィールド（runSeconds など）に明示的に値を設定するために使う。
func intp(v int) *int { return &v }

// builtinAgents mirrors docs §5(a). 組み込みツールは DB に入れず、ここでは agent + 変数カタログのみを seed する。
// 注：planner/worker/mainagent/auto の対話型 shell の既定値は下の interactive_shell_default_v1
// ブロックで一括 true にする（後からの toggle を尊重）。ここの interactiveShell は「行作成時から既定でオン」にする新 agent 専用。
var builtinAgents = []builtinAgent{
	{"goals", "ゴール分解", "goals", "ペネトレーションテストのタスク目標を、独立して検証可能な複数のサブゴールに分解する。", []promptVar{
		{"EngagementDescription", "タスク説明（テスト対象/背景）", "example.com サイトをテスト", "exploration"},
		// Now はグローバルな runtime 変数（server.globalPromptVars 参照）であり、各 agent のカタログで
		// 重複定義しない。さもないと withGlobalVars の追加時にグローバル項と名前が衝突する。
	}, false, nil},
	{"planner", "プランニング", "planner", "状況を読み、目標を判定し、未カバーの新方向が実際にあるときだけ探索意図を補う（タスクごとに 1 回のプランニングループ）。", []promptVar{
		{"Goal", "タスクの総目標", "example.com の管理者権限を取得する", "exploration"},
		{"AssetSummary", "アセット件数/種別分布の要約（任意）", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "メイン", "main", "人と機械のインターフェース：進捗を観察し、人の意図を hint または高優先度の意図として落とし込む。", []promptVar{
		{"Goal", "現在のタスク目標", "example.com の管理者権限を取得する", "exploration"},
		{"AssetSummary", "開始時の状況要約（任意）", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "確認済み脆弱性の要約（任意）", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "実行", "worker", "意図を 1 件受け取って実行し、判明した事実/脆弱性を知識グラフに書き戻してから停止する。", []promptVar{
		{"ProxyAddr", "記録用プロキシアドレス（if の 2 文言を切り替え）", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "worker の自己識別子（任意）", "worker-1", "runtime"},
		{"Intranet", "イントラネット期か（フットホールドセッション/稼働トンネルあり）", "true", "runtime"},
	}, false, nil},
	// worker.intranet:フェーズ 4 のイントラネット期 worker プロンプト変種——独立 agent ではなく、worker
	// 本文の DB 上書きスロット（フットホールド/トンネルのあるタスクの描画時に優先して使う。agent.renderSystemVariant 参照）。
	{"worker.intranet", "実行・イントラネット", "worker", "worker のイントラネット期プロンプト変種：タスクにフットホールドセッション/稼働トンネルがある場合に worker 本文の代わりに使う。", []promptVar{
		{"ProxyAddr", "記録用プロキシアドレス（if の 2 文言を切り替え）", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "worker の自己識別子（任意）", "worker-1", "runtime"},
		{"Intranet", "イントラネット期か（フットホールドセッション/稼働トンネルあり）", "true", "runtime"},
	}, false, nil},
	// Auto:組み込みの「プラットフォーム操作」agent。ペネトレーション編成ループには参加せず、チャットページから駆動され、ツールでプラットフォームを操作する。
	{"auto", "Auto", "assistant", "プラットフォーム操作アシスタント：ツールでタスク（作成/閲覧/一時停止/ヒント付与）とアセットを管理し、skill、カスタムツール、MCP の作成/変更もできる。", nil, false, nil},
	// ペネトレーションテスト：組み込みの「独立ペネトレーション」agent。チャットページから駆動され、1 人で偵察から終了処理までペネトレーションの全チェーンを歩み、自分で計画し、実行し、検証する。対話型 shell は既定でオン。
	{"pentest", "ペネトレーションテスト", "assistant", "独立ペネトレーション agent：1 人で偵察→攻撃面の探索→深掘り悪用→検証→終了処理まで全チェーンを歩み、自分で計画し、実行し、対抗的に検証する。", nil, true, intp(0)},
}

// seedBuiltins inserts the fixed built-in agents and their variable catalog (idempotent).
func (d *DB) seedBuiltins() error {
	for _, a := range builtinAgents {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// Drop catalog entries for variables that were renamed, so the white-list no
	// longer advertises a name templates can't resolve (EngagementTitle→Description).
	// 'Now' は各 agent のカタログからグローバルな runtime 変数へ昇格したため、旧 DB の goals に
	// 'Now' が残っているとグローバル項と名前が衝突する（フロントの変数一覧で key が重複）。まとめて消す。
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// Default-on interactive_shell for the runtime agents (planner/worker/mainagent/auto)
	// ONCE — respects a later user toggle-off (guarded by a settings flag). goals(one-shot
	// decomposer) stays off. Runs after the column exists (schema applied before seed).
	if v, _, _ := d.GetSetting("interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = d.SetSetting("interactive_shell_default_v1", "true")
	}
	// Seed the built-in browser (Playwright) MCP once — DISABLED by default (ユーザーが
	// 必要になった時点で自分で有効化する), no proxy by default. The traffic-capture toggle injects/strips
	// the recording proxy + CA at runtime (server.Manager.syncBrowserMCPProxy).
	// Insert only if absent so we never clobber user edits (args/env/enabled/
	// visibility) on restart.
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', 'npx', $1, '{}', false)
ON CONFLICT (name) DO NOTHING`,
		`["@playwright/mcp","--headless"]`); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}
	// NOTE: the placeholder ScopeSentry data-source MCP (empty URL + empty X-API-Key,
	// disabled) is seeded directly in schema.sql §F so a raw `psql < schema.sql` init
	// also gets it. schema.sql is Exec'd on every startup, so it stays idempotent.
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := d.seedDefaultInterceptRules(); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV2(); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV3(); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV4(); err != nil {
		return fmt.Errorf("seed intercept rules v4: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV5(); err != nil {
		return fmt.Errorf("seed intercept rules v5: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV6(); err != nil {
		return fmt.Errorf("seed intercept rules v6: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV7(); err != nil {
		return fmt.Errorf("seed intercept rules v7: %w", err)
	}
	return nil
}

// builtinSkillVisibility maps a shipped skill's directory name → the built-in
// agent keys that should see it by default. The skill FILES themselves live on the
// filesystem (SkillDir, loaded by norma at runtime); DB only carries this visibility
// binding. Skills omitted here (e.g. playwright-cli, scopesentry) ship invisible by
// default — the user turns them on per-agent when needed. scopesentry additionally
// declares `mcps: ScopeSentry`, which only takes effect once it's made visible and
// that MCP is enabled/configured.
var builtinSkillVisibility = map[string][]string{
	"api-recon": {"auto", "pentest", "worker"},
}

// seedBuiltinSkillVisibility binds the shipped built-in skills to their default
// agents. Insert-if-absent (ON CONFLICT DO NOTHING) so a user's later toggle-off is
// never resurrected on restart — matches the browser-MCP / intercept-rule seed policy.
func (d *DB) seedBuiltinSkillVisibility() error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules inserts built-in safety intercept rules once on
// first startup. The seed is gated by a settings flag so user edits (disable,
// delete, re-order) are never overwritten on subsequent restarts.
func (d *DB) seedDefaultInterceptRules() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v1"); v == "done" {
		return nil
	}
	type rule struct {
		name     string
		target   string // tool_name | tool_input
		typ      string // string | regex
		pattern  string
		action   string
		message  string
		priority int
	}
	rules := []rule{
		// ── システム破壊的コマンド (priority 100) ──────────────────────────
		{
			name:     "[組み込み] 再帰的な強制削除 rm -rf",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			action:   "deny",
			message:  "再帰的な強制削除（rm -rf / rm --recursive）を禁止します。システムやターゲット環境を恒久的に破壊するおそれがあります",
			priority: 100,
		},
		{
			name:     "[組み込み] システム重要ディレクトリの削除",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			action:   "deny",
			message:  "システムの重要パスを削除することを禁止します",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスクのフォーマット mkfs",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bmkfs\b`,
			action:   "deny",
			message:  "ディスクのフォーマット（mkfs）を禁止します",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスクデバイスの上書き dd",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "dd によるディスクデバイスの上書きを禁止します",
			priority: 100,
		},
		{
			name:     "[組み込み] Fork 爆弾",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `:\(\)\s*\{[^}]*:\|:`,
			action:   "deny",
			message:  "Fork 爆弾の実行を禁止します",
			priority: 100,
		},
		{
			name:     "[組み込み] シャットダウン / 再起動",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			action:   "deny",
			message:  "シャットダウンまたは再起動コマンドの実行を禁止します",
			priority: 100,
		},
		{
			name:     "[組み込み] 全プロセスの強制終了",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			action:   "deny",
			message:  "kill -9 -1 または killall -9（全プロセスの強制終了）を禁止します",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスク消去 shred / wipe",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "ディスクデバイスに対する shred/wipe の消去を禁止します",
			priority: 100,
		},
		{
			name:     "[組み込み] ファイアウォールルールの消去",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			action:   "deny",
			message:  "ファイアウォールルールの消去（iptables -F / nft flush）を禁止します",
			priority: 100,
		},
		// ── データベース破壊的操作 (priority 90) ─────────────────────────
		{
			name:     "[組み込み] SQL DROP DATABASE / TABLE / SCHEMA",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			action:   "deny",
			message:  "DROP 操作を禁止します。データベースオブジェクトが不可逆に破壊されるおそれがあります",
			priority: 90,
		},
		{
			name:     "[組み込み] SQL TRUNCATE",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			action:   "deny",
			message:  "TRUNCATE の実行を禁止します。テーブルの全データが消去されるおそれがあります",
			priority: 90,
		},
		{
			name:     "[組み込み] MongoDB drop / dropDatabase",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			action:   "deny",
			message:  "MongoDB の drop 操作を禁止します",
			priority: 90,
		},
		{
			name:     "[組み込み] Redis FLUSHALL / FLUSHDB",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			action:   "deny",
			message:  "Redis の FLUSHALL / FLUSHDB を禁止します。キャッシュデータがすべて消去されるおそれがあります",
			priority: 90,
		},
		// ── HTTP 破壊的リクエスト (priority 80) ───────────────────────────
		// Agent が DELETE リクエストを送る 3 つのよくある方法：
		//   1. curl -X DELETE / --request DELETE（Bash ツールで直接実行、またはスクリプトに記述）
		//   2. Python HTTP クライアントの .delete() メソッド
		//   3. JS/汎用スクリプト内の method: 'DELETE' / method="DELETE"
		{
			name:     "[組み込み] curl / wget による DELETE リクエスト",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			action:   "deny",
			message:  "curl/wget による HTTP DELETE リクエストの送信を禁止します。ターゲットシステムのデータが削除されるおそれがあります",
			priority: 80,
		},
		{
			name:     "[組み込み] Python HTTP クライアントの DELETE（requests/httpx/aiohttp）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			action:   "deny",
			message:  "Python HTTP クライアントによる DELETE リクエストの送信を禁止します",
			priority: 80,
		},
		{
			name:     "[組み込み] スクリプト内の HTTP DELETE メソッド宣言（JS/汎用）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			action:   "deny",
			message:  "スクリプト内で HTTP DELETE リクエストを宣言・送信することを禁止します",
			priority: 80,
		},
		{
			name:     "[組み込み] 一括消去 / クリア系の API パス",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			action:   "deny",
			message:  "一括消去や破棄系の API 呼び出し（/clear /wipe /flush /purge など）を禁止します",
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2 migrates the two safety patterns that used to be
// hard-coded in guard.go (destructive shell + data-exfil pipe) into ordinary
// intercept rules. Gated by its own flag so it also lands on DBs that already ran
// v1. Unlike the old guard.go floor, these are plain [組み込み] rules — the user can
// disable or delete them. The exfil rule ships DISABLED by default (its
// curl/wget/nc pipe pattern mis-fires on legitimate CTF/pentest reverse-shell and
// data-transfer pipes); enable it manually when exfil gating is actually wanted.
func (d *DB) seedDefaultInterceptRulesV2() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := []struct {
		name     string
		pattern  string
		action   string
		message  string
		enabled  bool
		priority int
	}{
		{
			name:     "[組み込み] 破壊的なシステムコマンド",
			pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			action:   "deny",
			message:  "破壊的コマンドを拒否しました（rm -rf / / mkfs / dd / fork 爆弾 / シャットダウン・再起動 / ディスクデバイスの上書き）",
			enabled:  true,
			priority: 100,
		},
		{
			name:     "[組み込み] データ外部送信パイプ",
			pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
			action:   "deny",
			message:  "データ外部送信の疑いがあるパイプを拒否しました（コマンド出力を curl/wget/nc で外部へ送信）",
			enabled:  false,
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v2", "done")
}

// stageShareHint は一時 HTTP サービスを対象とするインターセプトルールの統一的な許可ヒント（管理型の代替手段を指す）。
const stageShareHint = "一時 HTTP サービスは忘れたまま露出しがちです（TTL なし、タスク終了で孤児化、ディレクトリに戦利品や認証情報が混在しがち）。stage_share ツールでファイルを配信してください（管理型の一時領域：ランダム token パス、ハード TTL で自動破棄、ワンタイムで即時消去可）。"

// builtinHTTPServerRules は F13 のプラットフォーム下限ルール（pattern はすべて tool_input の正規表現、
// action=ask、builtin=true）。パッケージ変数に切り出して、seed と PG なしの正規表現単体テストの両方で使う。
var builtinHTTPServerRules = []struct {
	name    string
	pattern string
}{
	{
		name:    "[組み込み] 一時 HTTP サービス：python -m http.server",
		pattern: `(?i)\bpython[0-9.]*\s+-m\s+(?:http\.server|SimpleHTTPServer)\b`,
	},
	{
		name:    "[組み込み] 一時 HTTP サービス：php -S",
		pattern: `(?i)\bphp[0-9.]*\s+-S\b`,
	},
	{
		name:    "[組み込み] 一時 HTTP サービス：busybox httpd",
		pattern: `(?i)\bbusybox\s+httpd\b`,
	},
	{
		name:    "[組み込み] 一時 HTTP サービス：ruby -run -e httpd",
		pattern: `(?i)\bruby\b[^\n|;]{0,60}-e\s+httpd\b`,
	},
	{
		name:    "[組み込み] 一時 HTTP サービス：npx serve / http-server",
		pattern: `(?i)\bnpx\s+(?:-y\s+|--yes\s+)?(?:serve|http-server)\b`,
	},
}

// seedDefaultInterceptRulesV3 は「非正規の HTTP サービス配信」（F13）を塞ぐ：worker が Bash で
// python3 -m http.server / php -S / busybox httpd / npx serve を起動して一時サービスを立て、
// 武器を配信する。プラットフォームは感知できず TTL もなく、タスク終了時点で全ネットワークに開いた
// 孤児になる。これらは【プラットフォーム下限ルール】：builtin=true で、削除/無効化は禁止
// （DeleteInterceptRule/ToggleInterceptRule が拒否）。action=ask で deny ではない：正当な
// 場面（ターゲットが特定ポートへの戻り接続を必須とする等）では 1 回だけ手動許可できる。
func (d *DB) seedDefaultInterceptRulesV3() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v3"); v == "done" {
		return nil
	}
	for _, r := range builtinHTTPServerRules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action, builtin)
VALUES ($1, true, 85, 'tool_input', 'regex', $2, 'ask', $3, false, 60, 'deny', true)
ON CONFLICT DO NOTHING`,
			r.name, r.pattern, stageShareHint,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v3", "done")
}

// lateralHint は横展開/スプレー遮断ルールの統一ヒント（イントラネットの blast radius は外部ネットワークより
// はるかに大きい：スプレーでドメイン全体のアカウントがロックされ、SMB で端末が落ちる。docs/architecture/intranet-pivot.md §6.3 参照）。
const lateralHint = "横展開/パスワードスプレーは高ノイズな動作です（アカウントのロックやアラートを招く）。手動での確認が必要で、かつ目標が RoE の範囲内であることを確認してください（新しいセグメント≠承認済み。まずタスクスコープに登録）。"

// builtinLateralRules はイントラネット期 2 のプラットフォーム下限ルール（§4.4：スプレー/ブルート
// フォースの動作は必ず guard の ask を通す。F13 と同パターン：pattern はすべて tool_input の
// 正規表現、action=ask、builtin=true）。
//
// 判断の説明：hashcat は入れない——ローカルのパスワード解析ツールで、コマンドラインに対象ホストが
// 同時に現れる場合だけ遮断すべきだが、「任意の hashcat 引数に IP/ドメインが含まれる」を正規表現で
// 表すとローカル解析の場面を大量に誤検知する（辞書/マスクのパスにはドット付き数字がよく含まれる）。
// 半端な遮断をするより、載せずにツールとヒント側で制約する。
var builtinLateralRules = []struct {
	name    string
	pattern string
}{
	{
		name:    "[組み込み] パスワードスプレー/ブルートフォース：crackmapexec/nxc",
		pattern: `(?i)\b(?:crackmapexec|cme|nxc|netexec)\b`,
	},
	{
		name:    "[組み込み] パスワードスプレー/ブルートフォース：hydra",
		pattern: `(?i)\bhydra\b`,
	},
	{
		name:    "[組み込み] パスワードスプレー/ブルートフォース：medusa",
		pattern: `(?i)\bmedusa\b`,
	},
	{
		name:    "[組み込み] 横展開実行：psexec/wmiexec/smbexec",
		pattern: `(?i)\b(?:psexec|wmiexec|smbexec|atexec|dcomexec)(?:\.py)?\b`,
	},
	{
		name:    "[組み込み] ドメイン Kerberos 列挙/スプレー：kerbrute",
		pattern: `(?i)\bkerbrute\b`,
	},
}

// builtinTunnelBinaryRules は「トンネルバイナリの直接使用」を遮断する（イントラネット期 3 の実戦の教訓）：worker が
// tunnel_deploy を迂回して chisel/frp などでトンネルを張る——台帳もヘルス監視も自動回収もなく、
// 管理対象リソースの原則に反する（docs/architecture/intranet-pivot.md 4.3a）。プラットフォーム下限ルール、builtin=true。
var builtinTunnelBinaryRules = []struct {
	name    string
	pattern string
}{
	{
		name: "[組み込み] トンネルツールの直接使用（tunnel_deploy に変更）：chisel/frp/ligolo/suo5 など",
		// 「コマンドとして実行」された場合のみ遮断する（行頭/パイプ/セミコロン/sudo の後、パス付き可）。
		// テキスト中でこの語に言及するだけなら遮断しない（grep/cat/readme の診断場面、実戦での誤検知の教訓）。
		// suo5 も対象：プラットフォームの tunnel_deploy 自身は Go exec で CLI を起動するため tool_input の遮断を通らず、
		// 自然に免除される。worker が素の Bash で suo5 を起動するのは引き続き遮断（台帳/監視/回収なし）。
		pattern: `(?i)(?:^|[|;&]\s*|&&\s*|\|\|\s*|sudo\s+)(?:\S*/)?(?:chisel|frpc|frps|ligolo(?:-ng)?|nps|npc|stowaway|iodine|dnscat2?|suo5)\b`,
	},
}

// tunnelBinaryHint はトンネルツールを直接使用して ask で遮断されたときにモデルへ出す手動承認のヒント。
const tunnelBinaryHint = "トンネルバイナリの直接使用には台帳/ヘルス監視/自動回収がなく、管理対象リソースの原則に反します。" +
	"トンネルは tunnel_deploy（内蔵の配信/検証/監視）に変更し、一時ファイルの配信には stage_share を使ってください。" +
	"どうしても直接使用が必要な承認済みの場面では、1 回だけ手動で許可できます。"

// builtinProtocolClientRules は「最初からプロトコルクライアントを自作する」ことを遮断する（ツール規律の guard による下支え）：
// 红日3 の振り返りでは、worker が MySQL/SMB/TDS/Kerberos の生ソケットクライアントを自作し、誤った fact を生み出して
// プランニングを汚染した。導入済みツール（gogo/naabu/httpx/katana/impacket など）とプラットフォームツールを必ず先に使い、
// 手書きは最後の妥協とする。action=ask で deny ではない：承認済みの（ツールでカバーしきれない）場面では 1 回だけ手動許可できる。
var builtinProtocolClientRules = []struct {
	name    string
	pattern string
}{
	{
		name:    "[組み込み] 自作プロトコルクライアント（導入済みツールを先に使用）：socket + プロトコル特徴",
		pattern: `(?is)(import\s+socket|from\s+socket\s+import|socket\.create_connection)[\s\S]{0,600}(smb2?|mysql|postgres|mssql|tds|kerberos|krb5|ldap|ntlm|dcerpc|ncacn|drsuapi)`,
	},
}

// protocolClientHint は自作プロトコルクライアントが ask で遮断されたときにモデルへ出す手動承認のヒント。
const protocolClientHint = "自作プロトコルクライアントの兆候を検出しました。ツール規律：プロトコル対話/スキャンは必ず導入済みツール（gogo/naabu/httpx/katana/impacket）とプラットフォームツールを先に使ってください。" +
	"これらのツールでは完了できないと確認したうえで、手動で 1 回だけ許可できます。許可する際は fact に、どのツールが使えず、なぜかを明記してください。"

// seedDefaultInterceptRulesV6 は「最初からプロトコルクライアントを自作する」ことを遮断する（ツール規律の guard による下支え、プラットフォーム下限ルール）。
func (d *DB) seedDefaultInterceptRulesV6() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v6"); v == "done" {
		return nil
	}
	for _, r := range builtinProtocolClientRules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action, builtin)
VALUES ($1, true, 88, 'tool_input', 'regex', $2, 'ask', $3, false, 60, 'deny', true)
ON CONFLICT DO NOTHING`,
			r.name, r.pattern, protocolClientHint,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v6", "done")
}

// seedDefaultInterceptRulesV5 は「トンネルバイナリの直接使用」を遮断する（イントラネット期 3）：トンネルは長寿命の
// 管理対象リソースであり、必ず tunnel_deploy 経由で台帳に入れる。素の Bash で起動したトンネルプロセスは
// プラットフォームが監視も回収もできない。
func (d *DB) seedDefaultInterceptRulesV5() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v5"); v == "done" {
		return nil
	}
	for _, r := range builtinTunnelBinaryRules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action, builtin)
VALUES ($1, true, 87, 'tool_input', 'regex', $2, 'ask', $3, false, 60, 'deny', true)
ON CONFLICT DO NOTHING`,
			r.name, r.pattern, tunnelBinaryHint,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v5", "done")
}

// seedDefaultInterceptRulesV7 は suo5 を「トンネルツールの直接使用」下限ルールに統合する（suo5 アダプタの提供開始）：
// プラットフォームの tunnel_deploy 自身は suo5 CLI を Go exec で起動するため tool_input の遮断を通らず、自然に
// 免除される。worker が素の Bash で suo5 を起動するのは台帳/監視/回収がなく、引き続き遮断する。既に V5 を seed
// 済みの旧 DB ではここで名前+正規表現を差し替える。新規 DB は V5 で新しいルールを使っており、UPDATE が 0 行なら
// ON CONFLICT のフォールバックで何もしない。
func (d *DB) seedDefaultInterceptRulesV7() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v7"); v == "done" {
		return nil
	}
	r := builtinTunnelBinaryRules[0]
	res, err := d.Exec(`
UPDATE intercept_rules SET name=$2, pattern=$3, message=$4
WHERE name=$1 AND builtin=true`,
		"[組み込み] トンネルツールの直接使用（tunnel_deploy に変更）：chisel/frp/ligolo など", r.name, r.pattern, tunnelBinaryHint)
	if err != nil {
		return fmt.Errorf("rule %q: %w", r.name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action, builtin)
VALUES ($1, true, 87, 'tool_input', 'regex', $2, 'ask', $3, false, 60, 'deny', true)
ON CONFLICT DO NOTHING`,
			r.name, r.pattern, tunnelBinaryHint); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v7", "done")
}

// seedDefaultInterceptRulesV4 は「横展開/パスワードスプレー系ツール」を遮断する（イントラネット期 2）：worker が直接
// crackmapexec/nxc/hydra/medusa/psexec.py/kerbrute でイントラネットを攻撃すると、ノイズが大きくドメイン全体の
// アカウントをロックし得るため、必ず手動確認と RoE 範囲内であることが必要。これらは【プラットフォーム下限ルール】：
// builtin=true で削除/無効化は禁止、action=ask：承認済みの場面では 1 回だけ手動許可できる。
func (d *DB) seedDefaultInterceptRulesV4() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v4"); v == "done" {
		return nil
	}
	for _, r := range builtinLateralRules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action, builtin)
VALUES ($1, true, 86, 'tool_input', 'regex', $2, 'ask', $3, false, 60, 'deny', true)
ON CONFLICT DO NOTHING`,
			r.name, r.pattern, lateralHint,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v4", "done")
}
