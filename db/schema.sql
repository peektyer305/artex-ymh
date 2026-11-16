-- ARTEX PostgreSQL schema (単一データソース)
-- 冪等：繰り返し実行可能（IF NOT EXISTS / OR REPLACE / DROP TRIGGER IF EXISTS）。

-- =====================================================================
-- 0. 共通：updated_at トリガー
-- =====================================================================
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

-- 安全な text→inet 変換：不正な値は 22P02 を投げず NULL を返す。assets.ip は自由テキスト
-- (Agent / アセット API がホスト名を書き込むことがある)なので、裸の a.ip::inet 変換は
-- 1 行の汚れたデータで企業帰属の再計算文ごと落とす。呼び出し側は try_inet(...) IS NULL で
-- 該当行を見つけて警告する。pg_input_is_valid は PG16+ 必須なので使わず、より古い既存 DB にも合わせる。
CREATE OR REPLACE FUNCTION try_inet(value text) RETURNS inet AS $$
BEGIN
    RETURN value::inet;
EXCEPTION WHEN others THEN
    RETURN NULL;
END;
$$ LANGUAGE plpgsql IMMUTABLE STRICT;

-- =====================================================================
-- A. アセット層：companies / assets / company_scope
-- =====================================================================

CREATE TABLE IF NOT EXISTS companies (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    nkey       TEXT NOT NULL UNIQUE,
    logo       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_companies_nkey ON companies(nkey);
DROP TRIGGER IF EXISTS trg_companies_upd ON companies;
CREATE TRIGGER trg_companies_upd BEFORE UPDATE ON companies
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

CREATE TABLE IF NOT EXISTS assets (
    id              BIGSERIAL PRIMARY KEY,
    type            TEXT NOT NULL CHECK (type IN (
                        'root_domain','ip','subdomain','app','service','endpoint'
                    )),
    company_id      BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    -- explicit: caller/user selected the company; scope: derived from company_scope.
    -- Existing installations are conservatively migrated as explicit so a scope
    -- rebuild can never erase a historical manual association.
    company_source  TEXT NOT NULL DEFAULT 'explicit'
                    CHECK (company_source IN ('explicit','scope')),
    task_ids        BIGINT[] NOT NULL DEFAULT '{}',
    domain          TEXT,
    root_domain     TEXT,
    ip              TEXT,
    c_segment       CIDR,
    port            INTEGER CHECK (port BETWEEN 1 AND 65535),
    icp             TEXT,
    bound_domains   TEXT[]  NOT NULL DEFAULT '{}',
    open_ports      JSONB[] NOT NULL DEFAULT '{}',
    record_type     TEXT,
    record_value    TEXT[],
    bundle_id       TEXT,
    app_name        TEXT,
    category        TEXT,
    app_description TEXT,
    app_icp         TEXT,
    url             TEXT,
    service_type    TEXT CHECK (service_type IN ('http','other')),
    service_name    TEXT,
    favicon_mmh3    TEXT,
    status_code     INTEGER,
    content_length  BIGINT,
    page_title      TEXT,
    technologies    TEXT[]  NOT NULL DEFAULT '{}',
    auth            JSONB[] NOT NULL DEFAULT '{}',
    method          TEXT,
    params          JSONB[] NOT NULL DEFAULT '{}',
    extra           JSONB   NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_root_domain  ON assets(domain) WHERE type = 'root_domain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_ip           ON assets(ip)     WHERE type = 'ip';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_subdomain    ON assets(domain, COALESCE(record_type,'')) WHERE type = 'subdomain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_bundle   ON assets(bundle_id) WHERE type = 'app' AND bundle_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_name     ON assets(app_name)  WHERE type = 'app' AND bundle_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_http ON assets(url) WHERE type = 'service' AND service_type = 'http';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_other
    ON assets(COALESCE(domain,''), COALESCE(ip,''), port, service_name) WHERE type = 'service' AND service_type = 'other';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_endpoint     ON assets(url, method) WHERE type = 'endpoint';
CREATE INDEX IF NOT EXISTS idx_av2_company      ON assets(company_id)       WHERE company_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_company_type ON assets(company_id, type) WHERE company_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_task_ids     ON assets USING GIN(task_ids);
CREATE INDEX IF NOT EXISTS idx_av2_domain       ON assets(domain)      WHERE domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_root_domain  ON assets(root_domain) WHERE root_domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_ip           ON assets(ip)          WHERE ip IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_c_segment    ON assets USING GIST(c_segment inet_ops) WHERE c_segment IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_technologies ON assets USING GIN(technologies) WHERE type = 'service';
CREATE INDEX IF NOT EXISTS idx_av2_bound_domains ON assets USING GIN(bound_domains) WHERE type = 'ip';
CREATE INDEX IF NOT EXISTS idx_av2_open_ports   ON assets USING GIN(open_ports)    WHERE type = 'ip';
CREATE INDEX IF NOT EXISTS idx_av2_last_seen    ON assets(last_seen DESC);
CREATE INDEX IF NOT EXISTS idx_av2_type_seen    ON assets(type, last_seen DESC);
ALTER TABLE assets ADD COLUMN IF NOT EXISTS company_source TEXT;
UPDATE assets SET company_source = 'explicit' WHERE company_source IS NULL;
ALTER TABLE assets ALTER COLUMN company_source SET DEFAULT 'explicit';
ALTER TABLE assets ALTER COLUMN company_source SET NOT NULL;
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_company_source_check;
ALTER TABLE assets ADD CONSTRAINT assets_company_source_check
    CHECK (company_source IN ('explicit','scope'));
DROP TRIGGER IF EXISTS trg_av2_upd ON assets;
CREATE TRIGGER trg_av2_upd BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

CREATE TABLE IF NOT EXISTS company_scope (
    id         BIGSERIAL PRIMARY KEY,
    company_id BIGINT NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('domain','ip','cidr','icp','keyword')),
    domain     TEXT,
    net        CIDR,
    value      TEXT,
    raw        TEXT NOT NULL,
    reason     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_sv2_domain UNIQUE (company_id, domain),
    CONSTRAINT uq_sv2_net    UNIQUE (company_id, net),
    CONSTRAINT ck_company_scope_payload CHECK (
        (kind = 'domain' AND domain IS NOT NULL AND net IS NULL AND value IS NULL)
        OR (kind IN ('ip','cidr') AND domain IS NULL AND net IS NOT NULL AND value IS NULL)
        OR (kind IN ('icp','keyword') AND domain IS NULL AND net IS NULL AND value IS NOT NULL)
    )
);
-- Existing installations need the new text payload and expanded kind check.
ALTER TABLE company_scope ADD COLUMN IF NOT EXISTS value TEXT;
ALTER TABLE company_scope DROP CONSTRAINT IF EXISTS company_scope_kind_check;
ALTER TABLE company_scope ADD CONSTRAINT company_scope_kind_check
    CHECK (kind IN ('domain','ip','cidr','icp','keyword'));
ALTER TABLE company_scope DROP CONSTRAINT IF EXISTS ck_company_scope_payload;
ALTER TABLE company_scope ADD CONSTRAINT ck_company_scope_payload CHECK (
    (kind = 'domain' AND domain IS NOT NULL AND net IS NULL AND value IS NULL)
    OR (kind IN ('ip','cidr') AND domain IS NULL AND net IS NOT NULL AND value IS NULL)
    OR (kind IN ('icp','keyword') AND domain IS NULL AND net IS NULL AND value IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_sv2_domain  ON company_scope(domain)   WHERE kind = 'domain';
CREATE INDEX IF NOT EXISTS idx_sv2_net     ON company_scope USING GIST(net inet_ops) WHERE kind IN ('ip','cidr');
CREATE UNIQUE INDEX IF NOT EXISTS uq_sv2_value ON company_scope(company_id, kind, value) WHERE kind IN ('icp','keyword');
CREATE INDEX IF NOT EXISTS idx_sv2_icp ON company_scope(value) WHERE kind = 'icp';
CREATE INDEX IF NOT EXISTS idx_sv2_company ON company_scope(company_id);

-- =====================================================================
-- B. 推論探索層
-- =====================================================================
CREATE TABLE IF NOT EXISTS explorations (
    id          BIGSERIAL PRIMARY KEY,
    description TEXT,
    goal        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','achieved','failed')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- cold-digest (§2.3): per-task planner round counter — bumped once each time the
-- planner wakes and processes a round. Drives the ≥R cold-node debounce (measured in
-- this exploration's own rounds, not global node ids or wall-clock).
ALTER TABLE explorations ADD COLUMN IF NOT EXISTS round_no BIGINT NOT NULL DEFAULT 0;
DROP TRIGGER IF EXISTS trg_exp_upd ON explorations;
CREATE TRIGGER trg_exp_upd BEFORE UPDATE ON explorations
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

CREATE TABLE IF NOT EXISTS exploration_nodes (
    id             BIGSERIAL PRIMARY KEY,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}',
    priority       INT  NOT NULL DEFAULT 0,
    state          TEXT NOT NULL DEFAULT 'open',
    origin         TEXT,
    owner          TEXT,
    blocked_reason TEXT,
    delete_reason  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ,
    CONSTRAINT ck_node_kind CHECK (kind IN ('begin','goal','intent','fact','finding','hint','digest')),
    CONSTRAINT ck_node_state CHECK (
        (kind='begin'   AND state IN ('open')) OR
        (kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
        (kind='goal'    AND state IN ('open','met','abandoned')) OR
        (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
        (kind='finding' AND state IN ('confirmed','dismissed')) OR
        (kind='hint'    AND state IN ('active','consumed')) OR
        (kind='digest'  AND state IN ('active','superseded'))
    )
);
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS blocked_reason TEXT;
-- 意図の論理削除(soft delete):state='deleted' のとき、delete_reason にユーザーが入力した削除理由を記録する。
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS delete_reason TEXT;
-- cold-digest (§2.3/§5.3): content_version bumps on any change that could alter a
-- digest body (summary/state/confidence); cold_since_round stamps the planner round
-- a node most recently went from "has a live downstream branch" to none (NULL = hot).
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS content_version  INT    NOT NULL DEFAULT 0;
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS cold_since_round BIGINT;
-- ck_node_kind: existing installs predate the 'digest' kind — recreate to allow it.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid='exploration_nodes'::regclass
          AND conname='ck_node_kind'
          AND pg_get_constraintdef(oid) NOT LIKE '%digest%'
    ) THEN
        ALTER TABLE exploration_nodes DROP CONSTRAINT ck_node_kind;
        ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_kind
            CHECK (kind IN ('begin','goal','intent','fact','finding','hint','digest'));
    END IF;
END $$;
-- ck_node_state: recreate when it lacks the 'paused' (older), 'superseded' (digest rev),
-- or 'deleted' (intent soft-delete rev) branches.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid='exploration_nodes'::regclass
          AND conname='ck_node_state'
          AND (pg_get_constraintdef(oid) NOT LIKE '%paused%'
               OR pg_get_constraintdef(oid) NOT LIKE '%superseded%'
               OR pg_get_constraintdef(oid) NOT LIKE '%deleted%')
    ) THEN
        ALTER TABLE exploration_nodes DROP CONSTRAINT ck_node_state;
        ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_state CHECK (
            (kind='begin'   AND state IN ('open')) OR
            (kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
            (kind='goal'    AND state IN ('open','met','abandoned')) OR
            (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
            (kind='finding' AND state IN ('confirmed','dismissed')) OR
            (kind='hint'    AND state IN ('active','consumed')) OR
            (kind='digest'  AND state IN ('active','superseded'))
        );
    END IF;
END $$;
CREATE INDEX IF NOT EXISTS idx_expnodes_part     ON exploration_nodes(exploration_id, kind);
CREATE INDEX IF NOT EXISTS idx_expnodes_frontier ON exploration_nodes(exploration_id, priority DESC)
    WHERE kind='intent' AND state='open';
DROP TRIGGER IF EXISTS trg_expnodes_upd ON exploration_nodes;
CREATE TRIGGER trg_expnodes_upd BEFORE UPDATE ON exploration_nodes
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

CREATE TABLE IF NOT EXISTS exploration_edges (
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    src_id         BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    dst_id         BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    rel            TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (exploration_id, src_id, rel, dst_id),
    CONSTRAINT ck_edge_noself CHECK (src_id <> dst_id),
    CONSTRAINT ck_edge_rel CHECK (rel IN ('spawns','derived_from','yields','proves','covers'))
);
CREATE INDEX IF NOT EXISTS idx_expedges_src ON exploration_edges(src_id, rel);
CREATE INDEX IF NOT EXISTS idx_expedges_dst ON exploration_edges(dst_id, rel);
-- cold-digest (§1): the 'covers' relation (digest→member) postdates shipped installs,
-- whose rel CHECK is an inline auto-named constraint. Find and recreate it as ck_edge_rel.
DO $$
DECLARE cname text;
BEGIN
    SELECT conname INTO cname FROM pg_constraint
     WHERE conrelid='exploration_edges'::regclass AND contype='c'
       AND pg_get_constraintdef(oid) LIKE '%rel%'
       AND pg_get_constraintdef(oid) NOT LIKE '%covers%';
    IF cname IS NOT NULL THEN
        EXECUTE 'ALTER TABLE exploration_edges DROP CONSTRAINT '||quote_ident(cname);
        ALTER TABLE exploration_edges ADD CONSTRAINT ck_edge_rel
            CHECK (rel IN ('spawns','derived_from','yields','proves','covers'));
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS exploration_anchors (
    node_id   BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    asset_id  BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, asset_id)
);
CREATE INDEX IF NOT EXISTS idx_anchor_asset ON exploration_anchors(asset_id);

-- task_constraints: operator-authored operation constraints (allow/deny) for a task.
-- Extracted by the goals decomposer at round 0 (from goal/description), editable at
-- runtime by the main agent + 概要「制約管理」. Injected into the planner/worker system
-- prompt each round (config-gated) to keep exploration within the operator's boundary.
CREATE TABLE IF NOT EXISTS task_constraints (
    id             BIGSERIAL PRIMARY KEY,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('allow','deny')),
    text           TEXT NOT NULL,
    origin         TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_constraints_exp ON task_constraints(exploration_id);

CREATE TABLE IF NOT EXISTS activity (
    id                 BIGSERIAL PRIMARY KEY,
    exploration_id     BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    node_id            BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    worker             TEXT,
    kind               TEXT,
    tool               TEXT,
    tool_use_id        TEXT,
    is_error           BOOLEAN NOT NULL DEFAULT false,
    summary            TEXT,
    detail             TEXT,
    metadata           JSONB NOT NULL DEFAULT '{}',
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    cache_read_tokens  INTEGER,
    cache_write_tokens INTEGER,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE activity ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
-- main_seg segments the main-agent session into resettable conversations: a new
-- main session bumps the segment so its transcript + activity start clean while the
-- task's graph/assets/goal are untouched. NULL == legacy rows == segment 0 (the
-- original session). Only worker='mainagent' rows carry it.
ALTER TABLE activity ADD COLUMN IF NOT EXISTS main_seg INTEGER;
CREATE INDEX IF NOT EXISTS idx_act_node  ON activity(exploration_id, node_id, id);
CREATE INDEX IF NOT EXISTS idx_act_since ON activity(exploration_id, id);
CREATE INDEX IF NOT EXISTS idx_act_tool_call ON activity(exploration_id, tool_use_id, id)
  WHERE kind IN ('tool_use', 'tool_result');
-- Main/Plan history pages filter by worker (both carry NULL node_id, so idx_act_node
-- can't distinguish them); this covers reverse pagination of those sessions.
CREATE INDEX IF NOT EXISTS idx_act_worker ON activity(exploration_id, worker, id);
-- Main-session pages filter by segment on top of worker='mainagent'; this partial
-- index covers reverse pagination within one segment.
CREATE INDEX IF NOT EXISTS idx_act_main_seg ON activity(exploration_id, main_seg, id)
    WHERE worker='mainagent';
-- Task-list polls aggregate result usage and find the latest event repeatedly.
-- Cover the token columns for index-only aggregation and the timestamp order for
-- per-exploration latest-activity lookups.
-- 互換メモ:PG11+ の INCLUDE カバリングインデックスではなく複数列インデックスで書く。効果は
-- 同等で PG10 にも対応(Ubuntu 18.04 同梱の PG10、EXECUTE PROCEDURE も同様)。
CREATE INDEX IF NOT EXISTS idx_act_result_usage ON activity(exploration_id, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
    WHERE kind='result';
CREATE INDEX IF NOT EXISTS idx_act_latest ON activity(exploration_id, created_at DESC);

-- main_sessions records the resettable main-agent conversation segments of a task.
-- Segment 0 (the original session) is implicit and never stored; this table holds
-- only the extra segments created by 「新しいセッション」 (seq >= 1). The current segment is
-- MAX(seq) or 0. Each segment gets its own transcript file + activity slice; the
-- task's exploration graph/assets/goal are shared and never reset.
CREATE TABLE IF NOT EXISTS main_sessions (
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (exploration_id, seq)
);

-- =====================================================================
-- C. LLM profiles
-- =====================================================================
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS llm_profiles (
    id               BIGSERIAL PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    format           TEXT NOT NULL CHECK (format IN ('openai','anthropic','openai-responses')),
    base_url         TEXT,
    proxy            TEXT,
    model            TEXT NOT NULL,
    api_key          TEXT,
    api_key_hint     TEXT,
    rate_per_second  DOUBLE PRECISION NOT NULL DEFAULT 0,
    rate_per_minute  DOUBLE PRECISION NOT NULL DEFAULT 0,
    context_window_k INTEGER NOT NULL DEFAULT 0,
    -- 思考パラメータは 2 つの独立フィールドに分ける：thinking_type=思考スイッチ(''/disabled/enabled)、
    -- reasoning_effort=思考強度(''/low/medium/high/xhigh/max)で、互いに影響しない。
    reasoning_effort TEXT NOT NULL DEFAULT '',
    thinking_type    TEXT NOT NULL DEFAULT '',
    is_default       BOOLEAN NOT NULL DEFAULT false,
    -- ポーリング(フェイルオーバー)パラメータ。docs/LLM轮询设计.md を参照：
    --   priority     順位。大きいほど先に選ばれる。アクティブ設定(is_default)は本値に関係なく常にチェーン先頭。
    --   pool_exclude true=フェイルオーバー先にしない(agent/タスクから明示的にバインドして使うことは可能)。
    priority         INTEGER NOT NULL DEFAULT 0,
    pool_exclude     BOOLEAN NOT NULL DEFAULT false,
    -- streaming=true(既定)はストリーミング SSE。false は真の非ストリーミング(stream:false、一括 JSON)。
    streaming        BOOLEAN NOT NULL DEFAULT true,
    -- 1 回の応答の出力上限(token)。0=このフィールドを送信せず、サーバー側の既定値に従う——既存動作を維持。
    -- context_window_k(モデルの総容量で、ローカルの圧縮しきい値にのみ使用)とは別物：本値はリクエストとともに送信される。
    max_tokens       INTEGER NOT NULL DEFAULT 0,
    -- 出力上限にどのリクエストフィールド名を使うか。format='openai' のときだけ有効：
    --   ''                      = max_tokens(既定。大半のゲートウェイと互換)
    --   'max_completion_tokens' = 新しいフィールド。OpenAI 推論モデル(o シリーズ/GPT-5)はこれのみ受け付け、
    --                             max_tokens を送ると unsupported_parameter で拒否される。
    -- anthropic(max_tokens 必須)と openai-responses(max_output_tokens)は固有のフィールド名を持ち、この値の影響を受けない。
    max_tokens_field TEXT NOT NULL DEFAULT '',
    -- カスタムセッションヘッダー：空でないとき、毎回のリクエストにその名前の HTTP ヘッダーを付け、値=現在実行中の session id
    -- (chat セッション/worker 意図)。session-id ヘッダーでプロンプトキャッシュ/スティッキールーティングを行うゲートウェイ向け。''=送信しない。
    session_header_key TEXT NOT NULL DEFAULT '',
    -- 再試行の上書き：回数 0=グローバル既定/-1=無効/>0=その値、間隔 0=既定の指数バックオフ/>0=固定ミリ秒。
    -- 3 組はそれぞれ接続時の再試行、空レスポンスの再試行、同一 provider の安全ウィンドウ再試行に対応。詳細は下の ALTER のコメントを参照。
    retry_connect_attempts    INTEGER NOT NULL DEFAULT 0,
    retry_connect_interval_ms INTEGER NOT NULL DEFAULT 0,
    retry_empty_attempts      INTEGER NOT NULL DEFAULT 0,
    retry_empty_interval_ms   INTEGER NOT NULL DEFAULT 0,
    retry_stream_attempts     INTEGER NOT NULL DEFAULT 0,
    retry_stream_interval_ms  INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_llm_one_default ON llm_profiles(is_default) WHERE is_default;
DROP TRIGGER IF EXISTS trg_llm_upd ON llm_profiles;
CREATE TRIGGER trg_llm_upd BEFORE UPDATE ON llm_profiles
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();
-- ポーリング順位/除外フラグ。旧 DB への補填用。既定 0 / false = すべての設定がポーリングに参加。
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS priority     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS pool_exclude BOOLEAN NOT NULL DEFAULT false;
-- ストリーミングスイッチ。旧 DB への補填用。既定 true = 既存のストリーミング動作を維持し、旧設定も違和感なく移行。
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS streaming    BOOLEAN NOT NULL DEFAULT true;
-- format 制約を緩めて openai-responses(OpenAI Responses API)を許容する。旧 DB への補填用。
-- 起動のたびに実行され冪等:旧 CHECK を削除してから 3 値を含む新 CHECK を作成する。
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_format_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_format_check
    CHECK (format IN ('openai','anthropic','openai-responses'));

-- 出力上限とそのフィールド名。旧 DB への補填用。既定 0 / '' = 上限を送信せず、max_tokens のフィールド名を
-- 使い続ける。旧設定の動作はまったく変わらない。
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS max_tokens       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS max_tokens_field TEXT    NOT NULL DEFAULT '';
-- format と同様：先に削除してから作成し、起動のたびの冪等性を保証する。
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_max_tokens_field_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_max_tokens_field_check
    CHECK (max_tokens_field IN ('','max_completion_tokens'));
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_max_tokens_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_max_tokens_check
    CHECK (max_tokens >= 0);
-- カスタムセッションヘッダー名。旧 DB への補填用。既定 '' = 送信せず、旧設定の動作は変わらない。
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS session_header_key TEXT NOT NULL DEFAULT '';

-- 設定単位の再試行上書き（docs/LLM重试设计.md 参照）。3 組それぞれが「回数 + 固定間隔」の組を持ち、
-- 意味は統一：回数 0=グローバル既定を踏襲、-1=その層の再試行を無効、>0=その値。間隔 0=その層の
-- 既定の指数バックオフを踏襲、>0=この固定ミリ秒に変更。すべて既定 0 なので、旧 DB/旧設定の動作は変わらない。
--   connect = 接続時の再試行（SDK doStream：接続リセット/タイムアウト/429/5xx、ストリーム開始前）
--   empty   = 空レスポンスの再試行（SDK：完了したが content block が 1 つもない場合。openai 形式のみ）
--   stream  = 同一 provider の安全ウィンドウ再試行（本プロジェクト task_llm：出力未達のまま切れたストリームの再生）
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_connect_attempts    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_connect_interval_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_empty_attempts      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_empty_interval_ms   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_stream_attempts     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_stream_interval_ms  INTEGER NOT NULL DEFAULT 0;
-- format と同様：先に削除してから作成し、起動のたびの冪等性を保証する。回数の下限は -1(無効)、間隔は負にできない。
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_retry_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_retry_check CHECK (
    retry_connect_attempts >= -1 AND retry_empty_attempts >= -1 AND retry_stream_attempts >= -1
    AND retry_connect_interval_ms >= 0 AND retry_empty_interval_ms >= 0 AND retry_stream_interval_ms >= 0);

-- 思考スイッチのフィールド thinking_type は、旧来の単一 reasoning_effort の意味から一度に分割したもの。
-- schema.sql は起動のたびに実行されるため、移行は 1 回だけ走らせる必要がある：その列がまだ存在
-- しないときだけ埋め戻す。そうしないと起動のたびに、ユーザーが後から手動設定した組み合わせを上書きする。旧 reasoning_effort の意味：
--   'off'                    → 明示的にオフ  → thinking_type='disabled'、強度は空
--   'low/medium/high/max'    → オン+強度 → thinking_type='enabled'、強度は保持
--   ''                       → 送信しない    → 両方とも空(既定)
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'llm_profiles' AND column_name = 'thinking_type'
    ) THEN
        ALTER TABLE llm_profiles ADD COLUMN thinking_type TEXT NOT NULL DEFAULT '';
        UPDATE llm_profiles SET thinking_type = 'enabled'
            WHERE reasoning_effort IN ('low','medium','high','max');
        UPDATE llm_profiles SET thinking_type = 'disabled', reasoning_effort = ''
            WHERE reasoning_effort = 'off';
    END IF;
END $$;

-- LLM ポーリングのサーキットブレーカー状態：ある設定が連続で失敗(残高不足/key 失効/レート制限)すると
-- 冷却に入り、冷却期間中はポーリングで飛ばされる。メモリ上の状態が正で、ここへ落とすのは再起動後にも
-- 冷却ウィンドウを残すため。読み込み時は未到来の行(open_until > now)だけを取り、到来済みは"正常"に戻って次の半開試行を待つ。
CREATE TABLE IF NOT EXISTS llm_profile_health (
    profile_id  BIGINT PRIMARY KEY REFERENCES llm_profiles(id) ON DELETE CASCADE,
    fails       INTEGER NOT NULL DEFAULT 0,  -- 現在の連続失敗回数(成功でクリア)
    trips       INTEGER NOT NULL DEFAULT 0,  -- 累計のブレーカー作動回数。冷却時間の指数バックオフに使う
    open_until  TIMESTAMPTZ,                 -- 冷却の期限。NULL/期限切れ = 未作動
    last_error  TEXT NOT NULL DEFAULT '',
    last_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =====================================================================
-- D. タスク層
-- =====================================================================
-- Global task categories are intentionally independent from task templates.
-- Deleting a category only moves its tasks back to the uncategorized bucket.
CREATE TABLE IF NOT EXISTS task_categories (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    nkey       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_categories_name ON task_categories(name, id);
DROP TRIGGER IF EXISTS trg_task_categories_upd ON task_categories;
CREATE TRIGGER trg_task_categories_upd BEFORE UPDATE ON task_categories
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

CREATE TABLE IF NOT EXISTS tasks (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL DEFAULT '',
    category_id    BIGINT REFERENCES task_categories(id) ON DELETE SET NULL,
    description    TEXT NOT NULL,
    goal           TEXT NOT NULL,
    exploration_id BIGINT NOT NULL UNIQUE
                     REFERENCES explorations(id) ON DELETE RESTRICT,
    status         TEXT NOT NULL DEFAULT 'created'
                     CHECK (status IN ('created','running','paused','done','failed','timeout')),
    paused         BOOLEAN NOT NULL DEFAULT false,
    queued         BOOLEAN NOT NULL DEFAULT false,
    queued_at      TIMESTAMPTZ,
    queue_mode     TEXT NOT NULL DEFAULT '',
    llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    active_llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    llm_chain_revision BIGINT NOT NULL DEFAULT 0,
    company_id     BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    parent_ref     TEXT,
    timeout_seconds INTEGER NOT NULL DEFAULT 0,
    plan_heartbeat_seconds INTEGER NOT NULL DEFAULT 300,
    coverage_enabled BOOLEAN NOT NULL DEFAULT true,
    pinned_at      TIMESTAMPTZ,
    first_run_at   TIMESTAMPTZ,
    deadline_at    TIMESTAMPTZ,
    archived_at    TIMESTAMPTZ,
    deleted_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tasks_alive  ON tasks(created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status)          WHERE deleted_at IS NULL;
DROP TRIGGER IF EXISTS trg_tasks_upd ON tasks;
CREATE TRIGGER trg_tasks_upd BEFORE UPDATE ON tasks
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();
-- planner のハートビート間隔(秒);旧 DB への補填用。既定 300s(5min)。docs/planner-trigger-impl-plan.md 参照
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS plan_heartbeat_seconds INTEGER NOT NULL DEFAULT 300;
-- 同時実行上限による保留状態;旧 DB への補填用。true=同時実行上限でキューに入り、空きを待って自動開始。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS queued BOOLEAN NOT NULL DEFAULT false;
-- アセットカバレッジ機能のスイッチ;旧 DB への補填用。true(既定)=テストカバレッジを計算/表示し、
-- テスト範囲を自動で蓄積して agent に add_task_scope/list_untested_assets を開放する;false=すべて無効(task_scope.go 参照)。
-- 既存タスクの既定 true は元の動作を維持;company 関連(task_scope kind=company)はこのスイッチの影響を受けない。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS coverage_enabled BOOLEAN NOT NULL DEFAULT true;
-- queued_at makes admission FIFO reflect the actual enqueue order rather than the
-- task creation order. queue_mode distinguishes first bootstrap from resuming an
-- exploration that already owns goals/history.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS queued_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS queue_mode TEXT NOT NULL DEFAULT '';
-- 任意のタスク名;旧 DB への補填用。空文字=未命名で、フロント表示時は説明にフォールバックする。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS category_id BIGINT REFERENCES task_categories(id) ON DELETE SET NULL;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pinned_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS active_llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS llm_chain_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_tasks_category ON tasks(category_id, created_at DESC)
    WHERE deleted_at IS NULL AND category_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_pinned ON tasks(pinned_at DESC)
    WHERE deleted_at IS NULL AND pinned_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_archived ON tasks(archived_at DESC)
    WHERE archived_at IS NOT NULL;

-- Cold task archives retain only compact metadata in PostgreSQL. The complete
-- task payload lives in a versioned .tar.zst package under data/archives/tasks.
-- task_id stays unique so an operation can be retried safely after a restart.
CREATE TABLE IF NOT EXISTS task_archives (
    id                         BIGSERIAL PRIMARY KEY,
    task_id                    BIGINT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,
    state                      TEXT NOT NULL DEFAULT 'archive_queued' CHECK (state IN (
                                   'archive_queued','archiving','archive_failed','ready',
                                   'restore_queued','restoring','restore_failed',
                                   'delete_queued','deleting','delete_failed'
                               )),
    phase                      TEXT NOT NULL DEFAULT 'queued',
    progress                   INTEGER NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    error                      TEXT NOT NULL DEFAULT '',
    warnings                   JSONB NOT NULL DEFAULT '[]',
    format_version             INTEGER NOT NULL DEFAULT 2,
    archive_path               TEXT NOT NULL DEFAULT '',
    sha256                     TEXT NOT NULL DEFAULT '',
    original_size              BIGINT NOT NULL DEFAULT 0,
    compressed_size            BIGINT NOT NULL DEFAULT 0,
    task_name                  TEXT NOT NULL DEFAULT '',
    task_description           TEXT NOT NULL DEFAULT '',
    task_goal                  TEXT NOT NULL DEFAULT '',
    original_status            TEXT NOT NULL DEFAULT '',
    category_id_snapshot       BIGINT,
    category_name_snapshot     TEXT NOT NULL DEFAULT '',
    source_task_ids            BIGINT[] NOT NULL DEFAULT '{}',
    remaining_timeout_seconds  BIGINT NOT NULL DEFAULT 0,
    data_counts                JSONB NOT NULL DEFAULT '{}',
    aggregate_stats            JSONB NOT NULL DEFAULT '{}',
    archived_at                TIMESTAMPTZ,
    requested_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE task_archives ALTER COLUMN format_version SET DEFAULT 2;
CREATE INDEX IF NOT EXISTS idx_task_archives_state ON task_archives(state, requested_at, id);
CREATE INDEX IF NOT EXISTS idx_task_archives_archived ON task_archives(archived_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_task_archives_sources ON task_archives USING GIN(source_task_ids);
DROP TRIGGER IF EXISTS trg_task_archives_upd ON task_archives;
CREATE TRIGGER trg_task_archives_upd BEFORE UPDATE ON task_archives
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

-- Reusable task description/goal presets. nkey is the normalized, case-insensitive
-- identity used to reject visually equivalent duplicate names.
CREATE TABLE IF NOT EXISTS task_templates (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    nkey        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL,
    goal        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_templates_updated ON task_templates(updated_at DESC, id DESC);
DROP TRIGGER IF EXISTS trg_task_templates_upd ON task_templates;
CREATE TRIGGER trg_task_templates_upd BEFORE UPDATE ON task_templates
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

-- Direct, read-only task context inheritance. Relations are intentionally not
-- recursive: a task sees only the source tasks explicitly chosen at creation.
CREATE TABLE IF NOT EXISTS task_relations (
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    source_task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, source_task_id),
    CONSTRAINT ck_task_relation_not_self CHECK (task_id <> source_task_id)
);
CREATE INDEX IF NOT EXISTS idx_task_relations_source ON task_relations(source_task_id);

-- Task/asset provenance supplements the legacy assets.task_ids association. The
-- array remains the compatibility source for existing query and cleanup paths;
-- this relation records how each association was obtained for operator review.
CREATE TABLE IF NOT EXISTS task_asset_links (
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    asset_id       BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    source         TEXT NOT NULL DEFAULT 'system',
    source_summary TEXT NOT NULL DEFAULT '',
    source_node_id BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, asset_id)
);
CREATE INDEX IF NOT EXISTS idx_task_asset_links_asset ON task_asset_links(asset_id, task_id);
CREATE INDEX IF NOT EXISTS idx_task_asset_links_node ON task_asset_links(source_node_id)
    WHERE source_node_id IS NOT NULL;
DROP TRIGGER IF EXISTS trg_task_asset_links_upd ON task_asset_links;
CREATE TRIGGER trg_task_asset_links_upd BEFORE UPDATE ON task_asset_links
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

-- Keep provenance rows synchronized when existing asset upsert paths append or
-- remove task ids. Detailed callers overwrite the generic source after upsert.
CREATE OR REPLACE FUNCTION sync_task_asset_links() RETURNS trigger AS $$
BEGIN
    INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
    SELECT task.id, NEW.id, 'system', 'タスク実行中に自動関連付け'
    FROM unnest(NEW.task_ids) AS requested(task_id)
    JOIN tasks task ON task.id=requested.task_id AND task.deleted_at IS NULL
    ON CONFLICT (task_id, asset_id) DO NOTHING;

    DELETE FROM task_asset_links link
    WHERE link.asset_id=NEW.id
      AND NOT (link.task_id=ANY(NEW.task_ids));
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_assets_task_links ON assets;
CREATE TRIGGER trg_assets_task_links AFTER INSERT OR UPDATE OF task_ids ON assets
    FOR EACH ROW EXECUTE PROCEDURE sync_task_asset_links();

-- Existing installations receive an auditable legacy source without rewriting
-- task_ids. Ignore stale array ids that no longer resolve to a live task.
INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
SELECT task.id, asset.id, 'legacy', '過去のタスク資産関連から移行'
FROM assets asset
CROSS JOIN LATERAL unnest(asset.task_ids) AS requested(task_id)
JOIN tasks task ON task.id=requested.task_id AND task.deleted_at IS NULL
ON CONFLICT (task_id, asset_id) DO NOTHING;

-- Ordered task-level LLM failover chain. A quota-exhausted entry is skipped
-- until the user saves/resets the chain, which clears all failure state.
CREATE TABLE IF NOT EXISTS task_llm_profiles (
    task_id          BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    profile_id       BIGINT NOT NULL REFERENCES llm_profiles(id) ON DELETE CASCADE,
    position         INTEGER NOT NULL CHECK (position >= 0),
    status           TEXT NOT NULL DEFAULT 'ready'
                       CHECK (status IN ('ready','quota_exhausted')),
    last_error       TEXT,
    exhausted_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, profile_id),
    UNIQUE (task_id, position)
);
CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_order ON task_llm_profiles(task_id, position);
CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_profile ON task_llm_profiles(profile_id, task_id);
CREATE INDEX IF NOT EXISTS idx_tasks_llm_profile ON tasks(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_active_llm_profile ON tasks(active_llm_profile_id) WHERE active_llm_profile_id IS NOT NULL;
DROP TRIGGER IF EXISTS trg_task_llm_profiles_upd ON task_llm_profiles;
CREATE TRIGGER trg_task_llm_profiles_upd BEFORE UPDATE ON task_llm_profiles
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

-- One-time-compatible backfill: old pinned tasks become one-entry chains. A user
-- can still clear the chain later because the update path also clears the legacy
-- llm_profile_id column, preventing this block from re-adding it on restart.
INSERT INTO task_llm_profiles(task_id, profile_id, position)
SELECT t.id, t.llm_profile_id, 0
FROM tasks t
WHERE t.llm_profile_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM task_llm_profiles x WHERE x.task_id=t.id)
ON CONFLICT DO NOTHING;
UPDATE tasks t
SET active_llm_profile_id = t.llm_profile_id
WHERE t.active_llm_profile_id IS NULL
  AND t.llm_profile_id IS NOT NULL
  AND EXISTS (SELECT 1 FROM task_llm_profiles x WHERE x.task_id=t.id AND x.profile_id=t.llm_profile_id);

-- タスクのテスト範囲（アセットカバレッジの分母 + 認可境界）。
--   自動入力(source='auto')：insertAssets の最上位で、worker が明示的に挿入したアセット種別に応じた保守的な範囲を追加
--     （root_domain→root_domain、subdomain/service/endpoint→subdomain(host)、ip→ip）。
--     side-effect で派生したアセットは範囲に入らない（フックは handler の最上位にあり、派生は db 層の内部で起きる）。
--   agent が入力(source='agent')：add_task_scope で company/root_domain/subdomain/ip を追加。
-- カバレッジ = active な行に一致する assets（分母）のうち、fact ノードでアンカーされたものの割合（分子）。
CREATE TABLE IF NOT EXISTS task_scope (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('company','root_domain','subdomain','ip','cidr','icp','keyword')),
    company_id  BIGINT REFERENCES companies(id) ON DELETE CASCADE,  -- kind='company'
    domain      TEXT,          -- root_domain / subdomain
    net         CIDR,          -- ip / cidr
    value       TEXT,          -- icp / keyword
    source      TEXT NOT NULL DEFAULT 'auto' CHECK (source IN ('auto','agent','manual')),
    reason      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 旧 DB のアップグレード：企業範囲の単一テキスト欄と同じ識別能力を持つよう、タスク範囲を拡張する。
ALTER TABLE task_scope ADD COLUMN IF NOT EXISTS value TEXT;
ALTER TABLE task_scope DROP CONSTRAINT IF EXISTS task_scope_kind_check;
ALTER TABLE task_scope ADD CONSTRAINT task_scope_kind_check
    CHECK (kind IN ('company','root_domain','subdomain','ip','cidr','icp','keyword'));
-- 重複排除：同じ task の同じ範囲は 1 度だけ保存する（自動入力の一括挿入はこれで冪等）。
DROP INDEX IF EXISTS uq_task_scope;
CREATE UNIQUE INDEX IF NOT EXISTS uq_task_scope_v2 ON task_scope(
    task_id, kind, COALESCE(domain,''), COALESCE(net::text,''), COALESCE(company_id,0), COALESCE(value,''));
CREATE INDEX IF NOT EXISTS idx_ts_domain  ON task_scope(domain) WHERE kind IN ('root_domain','subdomain');
CREATE INDEX IF NOT EXISTS idx_ts_net     ON task_scope USING GIST(net inet_ops) WHERE kind IN ('ip','cidr');
CREATE INDEX IF NOT EXISTS idx_ts_company ON task_scope(company_id) WHERE kind = 'company';

-- フェーズ 4:イントラネットのパッシブ偵察で見つかった【未認証セグメント】を登録する(/24 単位で集約)。手動承認(approved)後に
-- task_scope へ書き込んで範囲を広げる;拒否(dismissed)後は同じ項目で hits を増やさず、繰り返しの通知を防ぐ。
CREATE TABLE IF NOT EXISTS pending_scope (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT NOT NULL,
    kind        TEXT NOT NULL,
    value       TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','dismissed')),
    hits        INT NOT NULL DEFAULT 1,
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at  TIMESTAMPTZ,
    UNIQUE (task_id, kind, value)
);

-- =====================================================================
-- E. Agents / プロンプトテンプレート / 変数カタログ
-- =====================================================================
CREATE TABLE IF NOT EXISTS agents (
    id                BIGSERIAL PRIMARY KEY,
    key               TEXT NOT NULL UNIQUE CHECK (key ~ '^[a-z][a-z0-9_.]*$'),
    name              TEXT NOT NULL,
    description       TEXT,
    role              TEXT NOT NULL,
    builtin           BOOLEAN NOT NULL DEFAULT true,
    enabled           BOOLEAN NOT NULL DEFAULT true,
    llm_profile_id    BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    current_prompt_id BIGINT,
    max_turns         INTEGER NOT NULL DEFAULT 0,
    run_seconds       INTEGER NOT NULL DEFAULT 1200,
    web_search        BOOLEAN NOT NULL DEFAULT false,
    interactive_shell BOOLEAN NOT NULL DEFAULT false,
    wrapup_prompt     TEXT NOT NULL DEFAULT '',
    wrapup_max_turns  INTEGER NOT NULL DEFAULT 0,
    task_timeout_wrapup_prompt    TEXT NOT NULL DEFAULT '',
    task_timeout_wrapup_max_turns INTEGER NOT NULL DEFAULT 0,
    trigger_run_mode     TEXT    NOT NULL DEFAULT 'serial'  CHECK (trigger_run_mode IN ('serial','parallel')),
    trigger_merge_mode   TEXT    NOT NULL DEFAULT 'all' CHECK (trigger_merge_mode IN ('by_task','all','none')),
    trigger_max_parallel INTEGER NOT NULL DEFAULT 5,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agents_role_ck CHECK (role IN ('goals','main','planner','worker','assistant'))
);
-- 列追加の移行(リリース済み。旧 DB のアップグレードでは列を補填し、新規 DB の CREATE には既に含まれる。移行に CHECK は付けない:旧 DB の既存データが安全 + バックエンドの書き込みホワイトリストで担保)。
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_run_mode     TEXT    NOT NULL DEFAULT 'serial';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_merge_mode   TEXT    NOT NULL DEFAULT 'all';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_max_parallel INTEGER NOT NULL DEFAULT 5;
-- per-agent の LLM バインド(agent 単位の既定モデル):列は初版から上の CREATE にあり、この ALTER は極めて古い DB 向けの保険(冪等)。
ALTER TABLE agents ADD COLUMN IF NOT EXISTS llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL;
-- run_seconds の 1 回の run の実時間既定を 600→1200 に変更:列の既定値だけを変え(今後挿入される行に影響)、旧 DB の既存行は変更しない。
ALTER TABLE agents ALTER COLUMN run_seconds SET DEFAULT 1200;
-- フェーズ 4:agent key でドットを許可(worker.intranet のようなプロンプト変種 key);旧 DB の制約を再作成(冪等)。
ALTER TABLE agents DROP CONSTRAINT IF EXISTS agents_key_check;
ALTER TABLE agents ADD CONSTRAINT agents_key_check CHECK (key ~ '^[a-z][a-z0-9_.]*$');
CREATE INDEX IF NOT EXISTS idx_agents_llm_profile ON agents(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
DROP TRIGGER IF EXISTS trg_agents_upd ON agents;
CREATE TRIGGER trg_agents_upd BEFORE UPDATE ON agents
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

CREATE TABLE IF NOT EXISTS agent_prompts (
    id            BIGSERIAL PRIMARY KEY,
    agent_id      BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    version       INT NOT NULL,
    template_text TEXT NOT NULL,
    note          TEXT,
    updated_by    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, version)
);
-- 循環外部キー：agents.current_prompt_id → agent_prompts.id（両テーブルの作成後に追加する必要がある）
DO $$ BEGIN
    ALTER TABLE agents ADD CONSTRAINT fk_agents_curprompt
        FOREIGN KEY (current_prompt_id) REFERENCES agent_prompts(id) ON DELETE SET NULL;
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS agent_prompt_vars (
    id          BIGSERIAL PRIMARY KEY,
    agent_id    BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    var_name    TEXT NOT NULL,
    description TEXT,
    example     TEXT,
    source      TEXT NOT NULL CHECK (source IN ('exploration','runtime','distilled')),
    UNIQUE (agent_id, var_name)
);

-- =====================================================================
-- F. MCP サービス
-- =====================================================================
CREATE TABLE IF NOT EXISTS mcp_servers (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    transport   TEXT NOT NULL CHECK (transport IN ('stdio','http','sse')),
    command     TEXT,
    args        JSONB NOT NULL DEFAULT '[]',
    env         JSONB NOT NULL DEFAULT '{}',
    url         TEXT,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    insecure    BOOLEAN NOT NULL DEFAULT false,  -- http: TLS 証明書の検証をスキップ(自己署名証明書のケース, issue #108)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Allow legacy MCP SSE servers on databases created before SSE support.
ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS mcp_servers_transport_check;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_transport_check
    CHECK (transport IN ('stdio','http','sse'));
-- 旧 DB への列の補填(schema.sql は起動のたびに Exec される)。
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS insecure BOOLEAN NOT NULL DEFAULT false;
DROP TRIGGER IF EXISTS trg_mcp_upd ON mcp_servers;
CREATE TRIGGER trg_mcp_upd BEFORE UPDATE ON mcp_servers
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

-- 既定データソースのプレースホルダー：ScopeSentry アセット同期 MCP（アドレスと認証は空のまま、無効）。
-- 「アセット同期」ページがデータソースの設定済み判定に使う。ユーザーがページで url と X-API-Key を入力してから有効にする。
-- 存在しないときだけ挿入し、ユーザーが設定/有効化済みのサーバーは絶対に上書きしない（schema.sql は起動のたびに Exec される）。
INSERT INTO mcp_servers (name, transport, url, env, enabled)
VALUES ('ScopeSentry', 'http', NULL, '{"X-API-Key":""}', false)
ON CONFLICT (name) DO NOTHING;

CREATE TABLE IF NOT EXISTS mcp_tools_cache (
    id            BIGSERIAL PRIMARY KEY,
    server_id     BIGINT NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    tool_name     TEXT NOT NULL,
    description   TEXT,
    schema        JSONB,
    discovered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (server_id, tool_name)
);

-- =====================================================================
-- G. 可視性：agent × mcp / skill
-- =====================================================================
CREATE TABLE IF NOT EXISTS agent_visibility (
    agent_id      BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    resource_kind TEXT   NOT NULL CHECK (resource_kind IN ('mcp')),
    resource_id   BIGINT NOT NULL,
    mcp_tool_name TEXT   NOT NULL DEFAULT '',
    enabled       BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, resource_kind, resource_id, mcp_tool_name)
);
CREATE INDEX IF NOT EXISTS idx_vis_resource ON agent_visibility(resource_kind, resource_id);

CREATE TABLE IF NOT EXISTS agent_skill_visibility (
    agent_id   BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    skill_name TEXT   NOT NULL,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, skill_name)
);
CREATE INDEX IF NOT EXISTS idx_askv_skill ON agent_skill_visibility(skill_name);

-- Skill 呼び出しの台帳（db/skill_usage.go 参照）。1 回の Skill() 呼び出しにつき 1 行で、次元だけを記録し本文は記録しない。
-- 外部キーは意図的に設けない：タスク/セッションを削除しても統計は残す必要があり（llm_usage と同様）、skill 自体も
-- ファイルシステム上のディレクトリ名にすぎず、対応するテーブルがない。
CREATE TABLE IF NOT EXISTS skill_usage (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    skill          TEXT NOT NULL,
    agent_key      TEXT,
    task_id        BIGINT,
    exploration_id BIGINT,
    intent_id      BIGINT,
    session_id     TEXT,
    args_len       INTEGER NOT NULL DEFAULT 0,
    -- false = モデルが存在しない skill を指名した(未ヒット)。この種の行も残す：「使いたいが無い」
    -- という空白を表し、skill を補う根拠になる。
    found          BOOLEAN NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS idx_skill_usage_skill ON skill_usage(skill, ts DESC);
CREATE INDEX IF NOT EXISTS idx_skill_usage_task  ON skill_usage(task_id);

-- ツール呼び出しの台帳（db/tool_usage.go 参照）。実際の CoreTool.Call 1 回につき 1 行で、帰属の次元だけを記録し、
-- ツールの引数や戻り値は保存しない。外部キーは意図的に設けず、タスク、セッション、カスタムツールを削除しても統計は残す。
CREATE TABLE IF NOT EXISTS tool_usage (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    tool_key       TEXT NOT NULL,
    agent_key      TEXT,
    task_id        BIGINT,
    exploration_id BIGINT,
    intent_id      BIGINT,
    session_id     TEXT
);
CREATE INDEX IF NOT EXISTS idx_tool_usage_tool ON tool_usage(tool_key, ts DESC);
CREATE INDEX IF NOT EXISTS idx_tool_usage_task ON tool_usage(task_id);

-- =====================================================================
-- H. 組み込みツールカタログ
-- =====================================================================
CREATE TABLE IF NOT EXISTS tools (
    key         TEXT PRIMARY KEY,
    system      BOOLEAN NOT NULL DEFAULT true,
    description TEXT    NOT NULL DEFAULT '',
    schema      JSONB   NOT NULL DEFAULT '{}',
    agents      JSONB   NOT NULL DEFAULT '[]',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    kind        TEXT    NOT NULL DEFAULT 'builtin',
    exec        JSONB   NOT NULL DEFAULT '{}',
    deferred    BOOLEAN NOT NULL DEFAULT false,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_tools_upd ON tools;
CREATE TRIGGER trg_tools_upd BEFORE UPDATE ON tools
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

-- =====================================================================
-- I. セッション（チャットページ）
-- =====================================================================
CREATE TABLE IF NOT EXISTS conversations (
    id             BIGSERIAL PRIMARY KEY,
    agent_key      TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    pinned_at      TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS pinned_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_conversations_llm_profile ON conversations(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_conversations_pinned ON conversations(pinned_at DESC) WHERE pinned_at IS NOT NULL;
DROP TRIGGER IF EXISTS trg_conversations_upd ON conversations;
CREATE TRIGGER trg_conversations_upd BEFORE UPDATE ON conversations
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

CREATE TABLE IF NOT EXISTS conversation_activities (
    id                 BIGSERIAL PRIMARY KEY,
    conversation_id    BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    worker             TEXT,
    kind               TEXT,
    tool               TEXT,
    tool_use_id        TEXT,
    is_error           BOOLEAN NOT NULL DEFAULT false,
    summary            TEXT,
    detail             TEXT,
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    cache_read_tokens  INTEGER,
    cache_write_tokens INTEGER,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_conv_act ON conversation_activities(conversation_id, id);
CREATE INDEX IF NOT EXISTS idx_conv_act_tool_call ON conversation_activities(conversation_id, tool_use_id, id)
  WHERE kind IN ('tool_use', 'tool_result');

-- =====================================================================
-- J. Agent トリガー
-- =====================================================================
CREATE TABLE IF NOT EXISTS agent_triggers (
    id                          BIGSERIAL PRIMARY KEY,
    agent_key                   TEXT NOT NULL,
    enabled                     BOOLEAN NOT NULL DEFAULT true,
    interval_sec                INTEGER NOT NULL DEFAULT 0,
    on_finding                  BOOLEAN NOT NULL DEFAULT false,
    on_goal_met                 BOOLEAN NOT NULL DEFAULT false,
    on_task_timeout             BOOLEAN NOT NULL DEFAULT false,
    on_tool_call                BOOLEAN NOT NULL DEFAULT false,
    on_task_create              BOOLEAN NOT NULL DEFAULT false,
    interval_message            TEXT NOT NULL DEFAULT '',
    finding_message             TEXT NOT NULL DEFAULT '',
    goal_message                TEXT NOT NULL DEFAULT '',
    task_timeout_message        TEXT NOT NULL DEFAULT '',
    tool_call_message           TEXT NOT NULL DEFAULT '',
    task_create_message         TEXT NOT NULL DEFAULT '',
    tool_names                  TEXT NOT NULL DEFAULT '',
    last_fire                   TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_triggers_agent ON agent_triggers(agent_key);
-- 列追加の移行(リリース済み。旧 DB のアップグレードでは列を補填し、新規 DB の CREATE には既に含まれるため ALTER は no-op)。冪等で、起動のたびに繰り返し実行できる。
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS on_tool_call        BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS tool_call_message   TEXT    NOT NULL DEFAULT '';
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS tool_names          TEXT    NOT NULL DEFAULT '';
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS on_task_create      BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS task_create_message TEXT    NOT NULL DEFAULT '';
DROP TRIGGER IF EXISTS trg_agent_triggers_upd ON agent_triggers;
CREATE TRIGGER trg_agent_triggers_upd BEFORE UPDATE ON agent_triggers
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

CREATE TABLE IF NOT EXISTS scheduler_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

-- =====================================================================
-- K. インターセプトルール
-- =====================================================================
CREATE TABLE IF NOT EXISTS intercept_rules (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT true,
    priority        INTEGER NOT NULL DEFAULT 0,
    match_target    TEXT NOT NULL CHECK (match_target IN ('tool_name', 'tool_input')),
    match_type      TEXT NOT NULL CHECK (match_type IN ('string', 'regex')),
    pattern         TEXT NOT NULL,
    action          TEXT NOT NULL CHECK (action IN ('allow', 'deny', 'ask')),
    message         TEXT NOT NULL DEFAULT '',
    timeout_enabled BOOLEAN NOT NULL DEFAULT true,
    timeout_seconds INTEGER NOT NULL DEFAULT 60,
    timeout_action  TEXT    NOT NULL DEFAULT 'deny',
    -- builtin=true のルールはプラットフォームの最低ライン(例: F13 の一時 HTTP サービス遮断)で、削除/無効化は禁止。
    builtin         BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE intercept_rules ADD COLUMN IF NOT EXISTS builtin BOOLEAN NOT NULL DEFAULT false;
DROP TRIGGER IF EXISTS trg_intercept_rules_upd ON intercept_rules;
CREATE TRIGGER trg_intercept_rules_upd BEFORE UPDATE ON intercept_rules
    FOR EACH ROW EXECUTE PROCEDURE set_updated_at();

CREATE TABLE IF NOT EXISTS intercept_pending (
    id              BIGSERIAL PRIMARY KEY,
    rule_id         BIGINT REFERENCES intercept_rules(id) ON DELETE SET NULL,
    conversation_id BIGINT REFERENCES conversations(id) ON DELETE CASCADE,
    task_id         TEXT,
    agent_name      TEXT NOT NULL DEFAULT '',
    tool_name       TEXT NOT NULL,
    tool_input      JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'allowed', 'denied', 'timeout')),
    -- 判定理由:ルールにヒットしたときはルールの message;LLM のフォールバック判定のときはモデルが返した短い理由([模型] を前置き)。
    reason          TEXT NOT NULL DEFAULT '',
    decided_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_status ON intercept_pending(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_task   ON intercept_pending(task_id, created_at DESC);
-- 旧 DB への補填:reason 列(リリース済み。列追加には IF NOT EXISTS が必要)。
ALTER TABLE intercept_pending ADD COLUMN IF NOT EXISTS reason TEXT NOT NULL DEFAULT '';
-- Detail payloads are lazy-loaded; NULL preserves the meaning of legacy history.
ALTER TABLE intercept_pending ADD COLUMN IF NOT EXISTS audit JSONB;
ALTER TABLE intercept_pending ADD COLUMN IF NOT EXISTS decision_source TEXT NOT NULL DEFAULT '';
UPDATE intercept_pending SET decision_source=CASE WHEN rule_id IS NOT NULL THEN 'rule'
 WHEN reason LIKE '[模型]%' THEN 'model' ELSE 'unknown' END WHERE decision_source='';

-- =====================================================================
-- L. 脆弱性ファインディングの永続化
-- =====================================================================
CREATE TABLE IF NOT EXISTS findings (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    node_id     BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    vulnclass   TEXT NOT NULL DEFAULT '',
    -- 脆弱性名(可読タイトル)。空のときはフロントが vulnclass を代わりに表示する。severity の値：
    -- critical 緊急 / high 高 / medium 中 / low 低（CHECK は付けず、status と同様に server 側のホワイトリストで検証）。
    name        TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL DEFAULT '',
    summary     TEXT NOT NULL DEFAULT '',
    evidence    TEXT NOT NULL DEFAULT '',
    worker      TEXT NOT NULL DEFAULT '',
    asset_ids   JSONB NOT NULL DEFAULT '[]',
    -- 処置ステータス：pending 未処理 / in_progress 処理中 / confirmed 確認済み / resolved 処置済み / fixed 修正済み /
    -- false_positive 誤検知 / ignored 無視 / duplicate 重複 / risk_accepted リスク受容。
    -- 値に CHECK は付けない：旧 DB は下の ALTER で列を補填し、CHECK では埋め戻せないため、server 側のホワイトリストで統一的に検証する。
    status      TEXT NOT NULL DEFAULT 'pending',
    -- 脆弱性の詳細レポート(Markdown)。既定は空で、詳細ページだけが読み込み/表示し、payload の肥大化を避けるため一覧 API には含めない。
    report      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE findings ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE findings ADD COLUMN IF NOT EXISTS name   TEXT NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN IF NOT EXISTS report TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_findings_task ON findings(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_findings_time ON findings(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_findings_status ON findings(status, created_at DESC);
-- 「アセット別」ビューは asset_ids @> '[<id>]' でファインディングを逆引きするため、この GIN インデックスがないと全表走査になる。
CREATE INDEX IF NOT EXISTS idx_findings_asset_ids ON findings USING GIN(asset_ids jsonb_path_ops);

-- 手動再テストは独立したセッションで行い、結論は元の脆弱性の処置ステータスとは別に保存する。
CREATE TABLE IF NOT EXISTS finding_retests (
    id BIGSERIAL PRIMARY KEY,
    finding_id BIGINT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    conversation_id BIGINT UNIQUE REFERENCES conversations(id) ON DELETE SET NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','completed','failed','stopped')),
    verdict TEXT NOT NULL DEFAULT '' CHECK (verdict IN ('','reproduced','fixed','inconclusive')),
    notes TEXT NOT NULL DEFAULT '',
    snapshot JSONB NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    evidence TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_finding_retests_history ON finding_retests(finding_id, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_finding_retests_active ON finding_retests(finding_id)
    WHERE status IN ('pending','running');

-- セッションを削除しても再テスト記録は残し、未終了の再テストの占有を解除する。
CREATE OR REPLACE FUNCTION stop_deleted_conversation_retest() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE finding_retests SET status='stopped', error='再テストのセッションが削除されました', finished_at=now()
    WHERE conversation_id=OLD.id AND status IN ('pending','running');
    RETURN OLD;
END;
$$;
DROP TRIGGER IF EXISTS trg_conversation_retest_delete ON conversations;
CREATE TRIGGER trg_conversation_retest_delete BEFORE DELETE ON conversations
    FOR EACH ROW EXECUTE PROCEDURE stop_deleted_conversation_retest();

-- 反証検証(要件 2 のプラン甲):finding_checks は「検証(出生証明)/再テスト(車検)」の統一パイプライン。
-- 既存の finding_retests は変更せず移行もしない;kind='retest' は今後のパイプライン統合のために予約。
-- verdict は 3 択:verified 真の脆弱性 / false_positive 誤検知 / inconclusive 確認できず(人に委ねる)。
CREATE TABLE IF NOT EXISTS finding_checks (
    id BIGSERIAL PRIMARY KEY,
    finding_id BIGINT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    kind TEXT NOT NULL DEFAULT 'verify' CHECK (kind IN ('verify','retest')),
    conversation_id BIGINT UNIQUE REFERENCES conversations(id) ON DELETE SET NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','completed','failed','stopped')),
    verdict TEXT NOT NULL DEFAULT '' CHECK (verdict IN ('','verified','false_positive','inconclusive')),
    -- 作成時に凍結した finding+アセット+制約のスナップショット(finding_retests.snapshot と同じ)。agent はツール経由で読む。
    snapshot JSONB NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    evidence TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_finding_checks_history ON finding_checks(finding_id, id DESC);
-- 1 つの finding に同時に 1 つだけアクティブな check を許す(finding_retests のアクティブ占有と同じ)。
CREATE UNIQUE INDEX IF NOT EXISTS idx_finding_checks_active ON finding_checks(finding_id)
    WHERE status IN ('pending','running');

-- セッションを削除しても検証記録は残し、未終了の検証の占有を解除する。
CREATE OR REPLACE FUNCTION stop_deleted_conversation_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE finding_checks SET status='stopped', error='検証のセッションが削除されました', finished_at=now()
    WHERE conversation_id=OLD.id AND status IN ('pending','running');
    RETURN OLD;
END;
$$;
DROP TRIGGER IF EXISTS trg_conversation_check_delete ON conversations;
CREATE TRIGGER trg_conversation_check_delete BEFORE DELETE ON conversations
    FOR EACH ROW EXECUTE PROCEDURE stop_deleted_conversation_check();

ALTER TABLE findings ADD COLUMN IF NOT EXISTS evidence_version BIGINT NOT NULL DEFAULT 0;
ALTER TABLE findings ADD COLUMN IF NOT EXISTS report_evidence_version BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS traffic_evidence_snapshots (
    id TEXT PRIMARY KEY,
    source_traffic_id TEXT NOT NULL,
    captured_at BIGINT NOT NULL,
    url TEXT NOT NULL,
    method TEXT NOT NULL,
    status INTEGER NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    req_head TEXT NOT NULL,
    resp_head TEXT NOT NULL,
    req_hash TEXT NOT NULL,
    resp_hash TEXT NOT NULL,
    req_len BIGINT NOT NULL,
    resp_len BIGINT NOT NULL,
    unreferenced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS finding_traffic_bindings (
    id BIGSERIAL PRIMARY KEY,
    finding_id BIGINT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    snapshot_id TEXT NOT NULL REFERENCES traffic_evidence_snapshots(id),
    role TEXT NOT NULL DEFAULT 'supporting',
    note TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(finding_id, snapshot_id)
);
CREATE INDEX IF NOT EXISTS idx_finding_traffic_order ON finding_traffic_bindings(finding_id, position, id);
CREATE INDEX IF NOT EXISTS idx_finding_traffic_snapshot ON finding_traffic_bindings(snapshot_id);

-- =====================================================================
-- M. バックエンドログの永続化
-- =====================================================================
CREATE TABLE IF NOT EXISTS server_logs (
    id         BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    level      TEXT NOT NULL DEFAULT 'info',
    tag        TEXT NOT NULL DEFAULT '',
    text       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_server_logs_id ON server_logs(id DESC);

-- Independent /btw history and the latest provider-ready main checkpoint.
CREATE TABLE IF NOT EXISTS side_question_sessions (
    session_key TEXT PRIMARY KEY,
    conversation_id BIGINT REFERENCES conversations(id) ON DELETE CASCADE,
    task_id BIGINT REFERENCES tasks(id) ON DELETE CASCADE,
    exploration_id BIGINT REFERENCES explorations(id) ON DELETE CASCADE,
    intent_id BIGINT REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    run_id BIGINT NOT NULL,
    version BIGINT NOT NULL,
    snapshot JSONB NOT NULL,
    generation BIGINT NOT NULL DEFAULT 0,
    CHECK ((conversation_id IS NOT NULL AND task_id IS NULL AND exploration_id IS NULL AND intent_id IS NULL)
        OR (conversation_id IS NULL AND task_id IS NOT NULL AND exploration_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_side_sessions_conv ON side_question_sessions(conversation_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_task ON side_question_sessions(task_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_exp ON side_question_sessions(exploration_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_intent ON side_question_sessions(intent_id);

CREATE TABLE IF NOT EXISTS side_question_requests (
    id TEXT PRIMARY KEY,
    ordinal BIGSERIAL UNIQUE,
    session_key TEXT NOT NULL REFERENCES side_question_sessions(session_key) ON DELETE CASCADE,
    generation BIGINT NOT NULL,
    client_id TEXT NOT NULL,
    question TEXT NOT NULL,
    answer TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN ('running','completed','failed','cancelled','interrupted')),
    error TEXT NOT NULL DEFAULT '',
    model JSONB NOT NULL,
    snapshot_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sequence BIGINT NOT NULL DEFAULT 0,
    usage JSONB NOT NULL DEFAULT '{}',
    UNIQUE(session_key,generation,client_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_side_request_running ON side_question_requests(session_key) WHERE status='running';
CREATE INDEX IF NOT EXISTS idx_side_requests_history ON side_question_requests(session_key,ordinal DESC);

-- Additive v3 archive fields; old archives restore these as empty objects.
ALTER TABLE side_question_sessions ADD COLUMN IF NOT EXISTS memory JSONB NOT NULL DEFAULT '{}';
ALTER TABLE side_question_requests ADD COLUMN IF NOT EXISTS context_info JSONB NOT NULL DEFAULT '{}';

-- =====================================================================
-- J. フットホールドセッション(イントラネット侵攻フェーズ 1a,INTRANET-PIVOT-DESIGN.md §4.2 参照)
-- =====================================================================
-- webshell/ssh/reverse フットホールドの一級管理。secret は接続パラメータの JSON を AES-GCM で
-- 暗号化して保存する(鍵は jwt.key から派生。db/sessions.go 参照)。文字列は "gcm1:<base64>" の形。
CREATE TABLE IF NOT EXISTS sessions (
    id                BIGSERIAL PRIMARY KEY,
    kind              TEXT NOT NULL,           -- http_php/http_jsp/http_aspx/ssh/reverse(予約)
    host_asset_id     BIGINT,                  -- webshell を endpoint アセットとして登録した後のアセット id(NULL 可)
    url               TEXT NOT NULL DEFAULT '',
    secret            TEXT NOT NULL DEFAULT '',
    lang              TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'alive' CHECK(status IN ('alive','dead')),
    created_by_task   BIGINT,
    created_by_intent BIGINT,
    last_beat         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_task ON sessions(created_by_task);

-- =====================================================================
-- K. 認証情報の一級エンティティ(イントラネット侵攻フェーズ 2,INTRANET-PIVOT-DESIGN.md §4.4 参照)
-- =====================================================================
-- 標的から回収したパスワード/hash/鍵/ticket をすべて登録する。secret は AES-GCM で暗号化して保存し、
-- 鍵は jwt.key から派生する(sessions とは別のドメイン分離ラベル。db/credentials.go 参照)。
CREATE TABLE IF NOT EXISTS credentials (
    id            BIGSERIAL PRIMARY KEY,
    task_id       BIGINT NOT NULL,
    host_asset_id BIGINT,                  -- 認証情報の取得元ホストのアセット id(NULL 可)
    username      TEXT NOT NULL DEFAULT '',
    cred_type     TEXT NOT NULL CHECK(cred_type IN
        ('password','hash_nt','hash_lm','hash_sha1','ticket','ssh_key','token')),
    secret        TEXT NOT NULL DEFAULT '', -- gcm1:<base64> の暗号文
    domain        TEXT NOT NULL DEFAULT '', -- ドメイン(例 CORP / corp.local)。空=ローカルアカウント
    source        TEXT NOT NULL DEFAULT '', -- 取得元の説明(例 "sqli dump users テーブル" / "/etc/shadow")
    verified      BOOLEAN NOT NULL DEFAULT false, -- 実際にログインできることを確認済み
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_credentials_task ON credentials(task_id);

-- =====================================================================
-- L. 多段プロキシトンネルの台帳(イントラネット侵攻フェーズ 3a,INTRANET-PIVOT-DESIGN.md §4.3/4.3a 参照)
-- =====================================================================
-- トンネルは長寿命の管理対象リソース:プラットフォーム側の chisel server プロセス、標的側の chisel client プロセス、
-- stage 配信の項目をすべて台帳に記録する(deploy_params に完全なデプロイパラメータを保存し、切断時の自動再起動に使う)。
-- タスクに帰属し、使い終わったら tunnel_teardown で回収する。
CREATE TABLE IF NOT EXISTS tunnels (
    id             BIGSERIAL PRIMARY KEY,
    task_id        BIGINT NOT NULL DEFAULT 0,   -- 帰属タスク(管理対象リソースの原則)
    kind           TEXT NOT NULL CHECK(kind IN ('socks','portfwd')),
    adapter        TEXT NOT NULL DEFAULT 'chisel',
    listen_host    TEXT NOT NULL DEFAULT '',    -- プラットフォーム側 chisel server のバインドアドレス
    listen_port    INT  NOT NULL DEFAULT 0,     -- プラットフォーム側 chisel server のポート(標的の逆接続口)
    target_host    TEXT NOT NULL DEFAULT '',    -- portfwd:トンネル経由でアクセスするイントラネットの標的
    target_port    INT  NOT NULL DEFAULT 0,
    via_session_id BIGINT NOT NULL DEFAULT 0,   -- どのフットホールドセッション経由でデプロイしたか
    remote_pid     INT  NOT NULL DEFAULT 0,     -- 標的側 chisel client の pid
    deploy_params  JSONB NOT NULL DEFAULT '{}', -- 完全なデプロイパラメータ(auth/server_pid/stage_token/リモートパスを含む)。再起動はこれに依存
    state          TEXT NOT NULL DEFAULT 'deploying'
                   CHECK(state IN ('deploying','alive','error','stopped')),
    last_check     TIMESTAMPTZ NOT NULL DEFAULT now(),
    error          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tunnels_task ON tunnels(task_id);
CREATE INDEX IF NOT EXISTS idx_tunnels_state ON tunnels(state);

-- =====================================================================
-- M. バッチ 6 L1 ハニーポット静的シグネチャ識別(HONEYPOT-DETECTION-DESIGN.md 二·L1)
-- =====================================================================
-- honeypot_score: 0-1。静的シグネチャにヒットした最高信頼度(0=シグナルなし)。再評価時は高い方を取る。
-- honeypot_evidence: ヒットしたシグネチャのエビデンス。JSON 配列テキスト(例 ["cowrie_default_ssh_banner(Cowrie, 0.99)"])で、マージして重複を除く。
ALTER TABLE assets ADD COLUMN IF NOT EXISTS honeypot_score REAL NOT NULL DEFAULT 0;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS honeypot_evidence TEXT NOT NULL DEFAULT '';
