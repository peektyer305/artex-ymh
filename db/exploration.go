package db

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrIntentStateConflict marks a failed compare-and-set transition. Callers use
// it to distinguish an expected control race (HTTP 409) from a storage failure.
var ErrIntentStateConflict = errors.New("intent state changed concurrently")

// utf8Clean makes a string safe for a PostgreSQL text column. It (1) replaces
// invalid UTF-8 byte sequences with U+FFFD and (2) strips NUL (0x00) bytes.
// Tool output (nmap/curl/… stdout) can carry raw/truncated bytes that PostgreSQL's
// UTF8 encoding rejects ("invalid byte sequence for encoding UTF8"); without this
// the INSERT fails and the activity record is silently lost. NUL is *valid* UTF-8
// (U+0000) so ToValidUTF8 leaves it in place, yet PostgreSQL text still rejects it
// (SQLSTATE 22021) — so it must be removed separately. JSONB columns need the
// separate jsonbClean below: json.Marshal encodes a NUL as the escape backslash-u-0000,
// which the text-type json accepts but jsonb rejects (SQLSTATE 22P05).
func utf8Clean(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return strings.ToValidUTF8(s, "�")
}

// jsonbClean makes marshaled JSON safe for a PostgreSQL jsonb column. json.Marshal
// faithfully encodes a NUL byte (U+0000) as the 6-byte escape sequence \u0000; the
// json type stores it, but jsonb rejects it with "unsupported Unicode escape
// sequence" (SQLSTATE 22P05). Captured HTTP/tool bytes can carry NULs, so drop the
// escape — this also covers NULs nested inside json.RawMessage fields, which are
// copied verbatim into the output. Only a *real* escape is stripped: a \u0000 is
// genuine when preceded by an even number of backslashes, so an escaped-backslash
// run such as \\u0000 (the literal text u0000) is left intact.
func jsonbClean(b []byte) []byte {
	if !bytes.Contains(b, []byte("\\u0000")) {
		return b
	}
	out := make([]byte, 0, len(b))
	bs := 0 // consecutive backslashes already emitted before position i
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && bs%2 == 0 && i+5 < len(b) &&
			b[i+1] == 'u' && b[i+2] == '0' && b[i+3] == '0' && b[i+4] == '0' && b[i+5] == '0' {
			i += 5 // skip the whole \u0000
			bs = 0
			continue
		}
		if b[i] == '\\' {
			bs++
		} else {
			bs = 0
		}
		out = append(out, b[i])
	}
	return out
}

// Node is a typed reasoning node (= old task_nodes). kind ∈ goal|intent|finding|hint.
type Node struct {
	FindingID     int64           `json:"finding_id,omitempty"` // populated by finding-aware reads; never inferred from ID
	FindingNodeID int64           `json:"finding_node_id,omitempty"`
	TrafficCount  int             `json:"traffic_count,omitempty"`
	ID            int64           `json:"id"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	Priority      int             `json:"priority"`
	State         string          `json:"state"`
	Origin        string          `json:"origin,omitempty"`
	Owner         string          `json:"owner,omitempty"`
	BlockedReason string          `json:"blocked_reason,omitempty"`
	DeleteReason  string          `json:"delete_reason,omitempty"` // 意図の仮削除(state='deleted')のときだけ非空
	Anchors       []int64         `json:"anchors,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SourceTaskID  int64           `json:"source_task_id,omitempty"`
	Inherited     bool            `json:"inherited,omitempty"`
}

// Activity is one worker execution step (= old task_activity).
type Activity struct {
	ID        int64           `json:"id"`
	NodeID    *int64          `json:"node_id,omitempty"`
	Worker    string          `json:"worker,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error"`
	Summary   string          `json:"summary,omitempty"`
	Detail    string          `json:"-"` // full blob; lazy-loaded, omitted from list payloads
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	// Token usage — set only on the terminal kind='result' record; nil elsewhere.
	InputTokens      *int  `json:"input_tokens,omitempty"`
	OutputTokens     *int  `json:"output_tokens,omitempty"`
	CacheReadTokens  *int  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int  `json:"cache_write_tokens,omitempty"`
	SourceTaskID     int64 `json:"source_task_id,omitempty"`
	Inherited        bool  `json:"inherited,omitempty"`
	// MainSeg is the main-agent conversation segment (nil for non-mainagent rows;
	// nil/0 == the original session). Lets the UI route a mainagent row to its segment.
	MainSeg *int `json:"main_seg,omitempty"`
}

// TokenUsage is a per-worker token aggregate (TokenStatsByWorker).
type TokenUsage struct {
	Worker           string `json:"worker"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
}

// SessionTokenUsage is the authoritative completed-run total for one task UI
// session. Worker sessions are keyed by intent rather than the reusable work#N
// executor name, so retries and reassignment remain attached to the same row.
type SessionTokenUsage struct {
	Session          string `json:"session"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
}

// DailyTokenBucket is one day's global token aggregate across all tasks.
type DailyTokenBucket struct {
	Day             string `json:"day"` // "YYYY-MM-DD"
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheReadTokens int    `json:"cache_read_tokens"`
}

// TokenDailyAll aggregates token consumption by calendar day (UTC) for the
// past `days` days across all explorations. Only kind='result' rows carry token
// counts (the terminal per-run summary), so there is no double-counting.
func (d *DB) TokenDailyAll(days int) ([]DailyTokenBucket, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.Query(`
		SELECT TO_CHAR(DATE_TRUNC('day', created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS day,
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0)
		FROM activity
		WHERE kind = 'result'
		  AND created_at >= NOW() - ($1 * INTERVAL '1 day')
		GROUP BY day
		ORDER BY day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyTokenBucket
	for rows.Next() {
		var b DailyTokenBucket
		if err := rows.Scan(&b.Day, &b.InputTokens, &b.OutputTokens, &b.CacheReadTokens); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ExplorationStore is a handle onto one exploration's reasoning graph + activity.
type ExplorationStore struct {
	db    *DB
	expID int64
}

// CreateExploration creates a new exploration root and returns its id.
func (d *DB) CreateExploration(description, goal string) (int64, error) {
	var id int64
	err := d.QueryRow(`INSERT INTO explorations(description, goal) VALUES ($1, $2) RETURNING id`, description, goal).Scan(&id)
	return id, err
}

// Exploration returns a handle bound to an exploration id.
func (d *DB) Exploration(id int64) *ExplorationStore { return &ExplorationStore{db: d, expID: id} }

func (s *ExplorationStore) ID() int64 { return s.expID }

// Root returns the exploration's description and goal.
func (s *ExplorationStore) Root() (description, goal string, err error) {
	var d sql.NullString
	err = s.db.QueryRow(`SELECT description, goal FROM explorations WHERE id=$1`, s.expID).Scan(&d, &goal)
	return d.String, goal, err
}

// AddNode writes a typed reasoning node with optional anchors to asset ids.
func (s *ExplorationStore) AddNode(kind string, payload map[string]any, priority int, state, origin string, anchors []int64) (int64, error) {
	raw, _ := json.Marshal(payload)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		s.expID, kind, string(raw), priority, state, origin).Scan(&id); err != nil {
		return 0, err
	}
	for _, a := range anchors {
		if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id, asset_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, a); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// AddIntent is a convenience: an open intent.
func (s *ExplorationStore) AddIntent(payload map[string]any, priority int, anchors []int64, origin string) (int64, error) {
	if origin == "" {
		origin = "planner"
	}
	return s.AddNode("intent", payload, priority, "open", origin, anchors)
}

// AddGoal writes a goal node (state open).
func (s *ExplorationStore) AddGoal(payload map[string]any, origin string) (int64, error) {
	return s.AddNode("goal", payload, 0, "open", origin, nil)
}

// OriginFactID returns this exploration's root fact id — the KindFact node with
// state='origin' seeded at task creation (the task root). Every intent traces
// back to it. Returns 0 (no error) when none exists (legacy explorations created
// under the old 'begin' root, or none yet).
func (s *ExplorationStore) OriginFactID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND state='origin' ORDER BY id LIMIT 1`, s.expID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// Anchor records a node→asset reference edge (many-to-many): it captures which
// global assets an exploration node touched, as lineage/provenance. The asset
// graph itself is global and shared — anchoring no longer gates visibility (any
// task may read any asset via search); it only preserves the reasoning trail.
// Idempotent.
func (s *ExplorationStore) Anchor(nodeID, assetID int64) error {
	if nodeID <= 0 || assetID <= 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO exploration_anchors(node_id, asset_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, nodeID, assetID)
	return err
}

// Link adds a typed exploration edge (idempotent).
func (s *ExplorationStore) Link(from int64, rel string, to int64) error {
	_, err := s.db.Exec(`
INSERT INTO exploration_edges(exploration_id, src_id, rel, dst_id) VALUES ($1,$2,$3,$4)
ON CONFLICT (exploration_id, src_id, rel, dst_id) DO NOTHING`, s.expID, from, rel, to)
	return err
}

// SetNodeState updates any node's state (never deletes). content_version bumps
// so a folded member's state flip invalidates its digest's cached body (§5.3).
func (s *ExplorationStore) SetNodeState(id int64, state string) error {
	_, err := s.db.Exec(`UPDATE exploration_nodes SET state=$1, blocked_reason=NULL, content_version=content_version+1 WHERE id=$2 AND exploration_id=$3`, state, id, s.expID)
	return err
}

// UpdateGoalPayload rewrites a goal node's payload text (and optional vulnclass);
// scoped to kind='goal' so it can never mutate an intent/fact by id. Returns an
// error if no such goal exists in this exploration.
func (s *ExplorationStore) UpdateGoalPayload(id int64, text, vulnclass string) error {
	payload := map[string]any{"text": text}
	if vulnclass != "" {
		payload["vulnclass"] = vulnclass
	}
	raw, _ := json.Marshal(payload)
	res, err := s.db.Exec(`UPDATE exploration_nodes SET payload=$1 WHERE id=$2 AND exploration_id=$3 AND kind='goal'`, string(raw), id, s.expID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ゴールが存在しません")
	}
	return nil
}

// ConfirmFact はこの exploration の fact ノード 1 件の confidence を confirmed へ昇格し、
// 監査フィールド(confirmed_by/confirmed_at)を書き込む。kind='fact' にスコープされ、
// intent/goal を誤って変更しない。該当するファクトがない場合はエラー。content_version は
// SetNodeState と同様に bump して、折り畳み済み digest のキャッシュされた本文を無効化する。
func (s *ExplorationStore) ConfirmFact(id int64, origin string) error {
	if origin == "" {
		origin = "system"
	}
	audit, _ := json.Marshal(map[string]any{
		"confidence":   "confirmed",
		"confirmed_by": origin,
		"confirmed_at": time.Now().UTC().Format(time.RFC3339),
	})
	res, err := s.db.Exec(`UPDATE exploration_nodes SET payload = payload || $1::jsonb, content_version=content_version+1 WHERE id=$2 AND exploration_id=$3 AND kind='fact'`, string(audit), id, s.expID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ファクトが存在しません")
	}
	return nil
}

// DeleteGoal hard-deletes a goal node; scoped to kind='goal' so it can never drop
// an intent/fact by id. The edges (spawns) and anchors reference it with ON DELETE
// CASCADE, so they go with it; activity.node_id is ON DELETE SET NULL. Returns an
// error if no such goal exists in this exploration.
func (s *ExplorationStore) DeleteGoal(id int64) error {
	res, err := s.db.Exec(`DELETE FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='goal'`, id, s.expID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ゴールが存在しません")
	}
	return nil
}

// SetIntentState updates an intent node's state.
// ResetRunningIntents returns any intent left in 'running' back to 'open' so it
// is re-claimed. Called on startup: a 'running' intent with no live worker (a
// backend restart or crashed worker goroutine left it stuck) would otherwise spin
// forever in the UI. Workers resume from their transcript, so reopening is safe.
func (s *ExplorationStore) ResetRunningIntents() (int64, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE exploration_id=$1 AND kind='intent' AND state='running'`, s.expID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReopenIntent flips ONE not-successfully-finished intent (blocked/exhausted/stopped)
// back to 'open' so a worker re-claims it (graph writes it already produced stay).
// The worker resumes from its prior LLM transcript instead of restarting from scratch.
// done/open/running are left untouched. Returns whether a row changed. Used by "再実行".
func (s *ExplorationStore) ReopenIntent(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes
SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE id=$1 AND exploration_id=$2 AND kind='intent'
  AND state IN ('blocked','exhausted','stopped')`, id, s.expID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReopenBlockedIntents flips EVERY 'blocked' intent in this exploration back to 'open'
// (batch rerun after e.g. an LLM/network outage that blocked many at once). Returns the
// number reopened. Workers resume from their transcripts; kept graph writes remain.
func (s *ExplorationStore) ReopenBlockedIntents() (int64, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE exploration_id=$1 AND kind='intent' AND state='blocked'`, s.expID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *ExplorationStore) SetIntentState(id int64, state string) error {
	// terminal states stamp completed_at; reopening (back to open/running) clears it.
	terminal := state == "done" || state == "blocked" || state == "exhausted" || state == "stopped"
	_, err := s.db.Exec(`UPDATE exploration_nodes
SET state=$1, blocked_reason=NULL, content_version=content_version+1, completed_at = CASE WHEN $4 THEN now() ELSE NULL END
WHERE id=$2 AND exploration_id=$3 AND kind='intent'`, state, id, s.expID, terminal)
	return err
}

// CompareAndSetIntentState transitions one local intent only when its current
// state still matches expected. It is the state boundary used by pause/resume and
// worker settlement, so a stale API read cannot overwrite a concurrent finish.
func (s *ExplorationStore) CompareAndSetIntentState(id int64, expected, state string) (bool, error) {
	terminal := state == "done" || state == "blocked" || state == "exhausted" || state == "stopped"
	res, err := s.db.Exec(`UPDATE exploration_nodes
SET state=$1, blocked_reason=NULL, content_version=content_version+1, completed_at = CASE WHEN $5 THEN now() ELSE NULL END
WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state=$4`, state, id, s.expID, expected, terminal)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// IntentCleanup summarizes the blackboard records removed when a user cancels
// one worker intent. Global assets and traffic are deliberately not part of this
// operation; exploration anchors disappear only because their owning nodes do.
type IntentCleanup struct {
	Intents    int64 `json:"intents"`
	Facts      int64 `json:"facts"`
	Findings   int64 `json:"findings"`
	Activities int64 `json:"activities"`
}

// SoftDeleteIntent は未割り当て/実行中/一時停止中の意図を仮削除する：state='deleted' にし、
// ユーザーが入力した削除理由を delete_reason フィールドへ記録して、意図ノードとその全成果物/
// 系譜を保持する(旧実装のようにグラフへ fact を別途ぶら下げない)。削除前の意図の summary を
// 返し、planner の通知に使う。サイドセッションは削除状態と合わせて片付ける。
// 呼び出し側は実行中の worker を先に止め、以降の書き込みを防ぐこと。
func (s *ExplorationStore) SoftDeleteIntent(id int64, reason string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var state string
	var rawPayload []byte
	if err := tx.QueryRow(`SELECT state, payload FROM exploration_nodes
		WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, id, s.expID).Scan(&state, &rawPayload); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("intent not found")
		}
		return "", err
	}
	if state != "running" && state != "paused" && state != "open" {
		return "", fmt.Errorf("%w: intent state %s cannot be deleted", ErrIntentStateConflict, state)
	}
	if _, err := tx.Exec(`UPDATE exploration_nodes
		SET state='deleted', delete_reason=$3, blocked_reason=NULL,
		    content_version=content_version+1, completed_at=now()
		WHERE id=$1 AND exploration_id=$2`, id, s.expID, reason); err != nil {
		return "", err
	}
	// 主意図の監査トレイルは残すが、サイドクエスチョンのセッションは削除状態と原子的に
	// 片付ける(遅延スナップショットはそれを拒否する)。
	if _, err := tx.Exec(`DELETE FROM side_question_sessions WHERE intent_id=$1`, id); err != nil {
		return "", err
	}
	var summary string
	var p map[string]any
	if json.Unmarshal(rawPayload, &p) == nil {
		if sm, ok := p["summary"].(string); ok {
			summary = sm
		}
	}
	return summary, tx.Commit()
}

// CancelIntent は意図 1 件と、「その意図だけに支えられている」排他的な子孫ノードを物理削除
// する。削除対象は、その意図から yields/derived_from を下って到達でき、かつ全親ノード
// (そのノードを指す辺の全ソース)が削除集合に入っているノード。goal と origin fact は決して
// 削除せず、削除集合の外にある意図/digest から参照される共有ノードも、他のブランチを壊して
// 断線を生まないよう残す。削除する各 intent はまず token rollup(不可逆な計量を保持)を行い、
// その activity とサイドセッションを片付ける。削除する finding は先に findings 表の行を削除する
// (node_id FK が ON DELETE SET NULL なので、そうしないと孤児が残る)。辺はノードの CASCADE で
// 片付き、クリーンアップ全体は 1 トランザクション内に保たれる。呼び出し側は実行中の worker を
// 先に止め、以降の書き込みを防ぐこと。
func (s *ExplorationStore) CancelIntent(id int64) (IntentCleanup, error) {
	var out IntentCleanup
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	// 意図行をロックして存在を確認する(冪等：削除済みなら not found)。状態は検証しない。
	// 真の削除はいずれの状態でも成立し、running の worker は呼び出し側が先に止める。
	if err := tx.QueryRow(`SELECT 1 FROM exploration_nodes
		WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, id, s.expID).Scan(new(int)); err != nil {
		if err == sql.ErrNoRows {
			return out, fmt.Errorf("intent not found")
		}
		return out, err
	}

	// グラフ全体のノード(protected 判定)と辺(下方向の到達可能性 + 親集合の算出)を読み込む。
	// 実タスクのグラフ規模は小さい。
	kind := map[int64]string{}
	protected := map[int64]bool{}
	nrows, err := tx.Query(`SELECT id, kind, state FROM exploration_nodes WHERE exploration_id=$1`, s.expID)
	if err != nil {
		return out, err
	}
	for nrows.Next() {
		var nid int64
		var k, st string
		if err := nrows.Scan(&nid, &k, &st); err != nil {
			nrows.Close()
			return out, err
		}
		kind[nid] = k
		if k == KindGoal || (k == KindFact && st == StateOrigin) {
			protected[nid] = true // ゴールとタスク根のファクトは意図の削除に巻き込まれない
		}
	}
	nrows.Close()
	if err := nrows.Err(); err != nil {
		return out, err
	}

	parentsOf := map[int64][]int64{} // dst -> それを指す全辺の src(任意の rel。covers を含む：digest に覆われたメンバーは digest 親を持つため保持される)
	downOf := map[int64][]int64{}    // src -> yields/derived_from を辿った下方向の隣接ノード
	erows, err := tx.Query(`SELECT src_id, rel, dst_id FROM exploration_edges WHERE exploration_id=$1`, s.expID)
	if err != nil {
		return out, err
	}
	for erows.Next() {
		var src, dst int64
		var rel string
		if err := erows.Scan(&src, &rel, &dst); err != nil {
			erows.Close()
			return out, err
		}
		parentsOf[dst] = append(parentsOf[dst], src)
		if rel == RelYields || rel == RelDerivedFrom {
			downOf[src] = append(downOf[src], dst)
		}
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return out, err
	}

	// 排他的カスケード：意図から下方向へ広げ、あるノードが削除集合に入るのは、保護されて
	// おらず、かつ全親が既に集合内にある場合だけ(削除ノード経由以外に来路がない)。
	// 不動点まで反復する。
	del := map[int64]bool{id: true}
	for changed := true; changed; {
		changed = false
		for src := range del {
			for _, dst := range downOf[src] {
				if del[dst] || protected[dst] {
					continue
				}
				exclusive := true
				for _, p := range parentsOf[dst] {
					if !del[p] {
						exclusive = false
						break
					}
				}
				if exclusive {
					del[dst] = true
					changed = true
				}
			}
		}
	}

	// 種別ごとにバケット分けする。
	var ids, intentIDs, findingIDs []int64
	for nid := range del {
		ids = append(ids, nid)
		switch kind[nid] {
		case KindIntent:
			intentIDs = append(intentIDs, nid)
			out.Intents++
		case KindFact:
			out.Facts++
		case KindFinding:
			findingIDs = append(findingIDs, nid)
			out.Findings++
		}
	}

	// 削除する各意図：token rollup(不可逆な計量を保持)の後に activity を片付ける。rollup 行の
	// node_id は NULL なので、後段の node_id 指定の削除には掛からない。
	for _, iid := range intentIDs {
		tokenBuckets, err := intentTokenRollup(tx, s.expID, iid)
		if err != nil {
			return out, err
		}
		res, err := tx.Exec(`DELETE FROM activity WHERE exploration_id=$1 AND node_id=$2`, s.expID, iid)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			out.Activities += n
		}
		for _, bucket := range tokenBuckets {
			metadata, _ := json.Marshal(map[string]any{
				"cancelled_intent_id": iid,
				"token_day":           bucket.Day.Format(time.DateOnly),
				"token_rollup":        true,
			})
			if _, err := tx.Exec(`INSERT INTO activity(
				exploration_id, worker, kind, summary, metadata,
				input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, created_at)
				VALUES ($1,'token-ledger','result',$2,$3,$4,$5,$6,$7,$8)`,
				s.expID, fmt.Sprintf("キャンセルした意図 #%d のトークン計量", iid), metadata,
				bucket.Usage.InputTokens, bucket.Usage.OutputTokens,
				bucket.Usage.CacheReadTokens, bucket.Usage.CacheWriteTokens, bucket.Day); err != nil {
				return out, err
			}
		}
		if _, err := tx.Exec(`DELETE FROM side_question_sessions WHERE intent_id=$1`, iid); err != nil {
			return out, err
		}
	}

	// finding ノードを削除する前に findings 表の行を削除する(そうしないと node_id=NULL の
	// 孤児が残る)。
	for _, fid := range findingIDs {
		if _, err := tx.Exec(`DELETE FROM findings WHERE node_id=$1`, fid); err != nil {
			return out, err
		}
	}

	// ノードを削除する(辺は CASCADE で片付く)。1 件ずつ削除して行数を照合し、並行変更を防ぐ。
	var removed int64
	for _, nid := range ids {
		res, err := tx.Exec(`DELETE FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, nid, s.expID)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += n
		}
	}
	if removed != int64(len(ids)) {
		return out, fmt.Errorf("intent cleanup changed concurrently")
	}
	return out, tx.Commit()
}
