package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const nodeCols = `id, kind, payload, priority, state, COALESCE(origin,''), COALESCE(owner,''), COALESCE(blocked_reason,''), COALESCE(delete_reason,''), created_at`

func scanNode(sc interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var payload []byte
	if err := sc.Scan(&n.ID, &n.Kind, &payload, &n.Priority, &n.State, &n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason, &n.CreatedAt); err != nil {
		return nil, err
	}
	n.Payload = json.RawMessage(payload)
	return &n, nil
}

func scanNodes(rows *sql.Rows) ([]*Node, error) {
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListByKind lists nodes of a kind (newest first).
func (s *ExplorationStore) ListByKind(kind string, limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind=$2 ORDER BY id DESC LIMIT $3`, s.expID, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListByKindPage returns one newest-first page of nodes of a kind for reverse
// pagination: up to `limit` nodes with id < `before` (before<=0 = the newest page).
// hasMore reports whether still-older nodes exist, so a client (e.g. the worker
// session list) can page past the old fixed cap instead of losing older intents.
func (s *ExplorationStore) ListByKindPage(kind string, before int64, limit int) ([]*Node, bool, error) {
	if limit <= 0 {
		limit = 300
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind=$2 AND ($3 <= 0 OR id < $3)
ORDER BY id DESC LIMIT $4`, s.expID, kind, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(nodes) > limit
	if hasMore {
		nodes = nodes[:limit]
	}
	return nodes, hasMore, nil
}

// listByKindPageFiltered is ListByKindPage with an optional summary keyword
// filter (payload->>'summary' ILIKE %q%). Fetches one extra row so the caller can
// probe hasMore. Empty q = no filter. Newest-first by id.
func (s *ExplorationStore) listByKindPageFiltered(kind string, before int64, limit int, q string) ([]*Node, error) {
	where := `exploration_id=$1 AND kind=$2 AND ($3 <= 0 OR id < $3)`
	args := []any{s.expID, kind, before}
	if q != "" {
		args = append(args, "%"+q+"%")
		where += fmt.Sprintf(` AND payload->>'summary' ILIKE $%d`, len(args))
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE `+where+` ORDER BY id DESC LIMIT $`+fmt.Sprintf("%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// countByKindFiltered counts nodes of a kind under the same summary filter, for a
// page's total. Empty q = count all of that kind.
func (s *ExplorationStore) countByKindFiltered(kind, q string) (int, error) {
	where := `exploration_id=$1 AND kind=$2`
	args := []any{s.expID, kind}
	if q != "" {
		args = append(args, "%"+q+"%")
		where += fmt.Sprintf(` AND payload->>'summary' ILIKE $%d`, len(args))
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes WHERE `+where, args...).Scan(&n)
	return n, err
}

// CountFinishedIntents counts this exploration's intents in a terminal explored
// state (done/blocked/exhausted) — graph_overview's done_intents_total, so the planner
// knows recent_done_intents (capped at a recent window) is a truncated view and
// stays cautious about "already tried" dedup. Excludes 'stopped' (killed/deleted),
// matching exactly what recent_done_intents surfaces.
func (s *ExplorationStore) CountFinishedIntents() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state IN ('done','blocked','exhausted')`, s.expID).Scan(&n)
	return n, err
}

// GetNode returns one node of this exploration by id (nil, nil if not found).
func (s *ExplorationStore) GetNode(id int64) (*Node, error) {
	n, err := scanNode(s.db.QueryRow(`SELECT `+nodeCols+` FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, id, s.expID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return n, err
}

// Nodes lists all nodes (for graph viz).
func (s *ExplorationStore) Nodes(limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 2000
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes WHERE exploration_id=$1 ORDER BY id LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// Edge is one row from exploration_edges.
type Edge struct {
	From int64
	Rel  string
	To   int64
}

// Edges lists exploration edges (for graph viz).
func (s *ExplorationStore) Edges(limit int) ([]Edge, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.db.Query(`SELECT src_id, rel, dst_id FROM exploration_edges WHERE exploration_id=$1 LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.Rel, &e.To); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FactsYielded returns the ids of fact nodes an intent produced (intent --yields-->
// fact), oldest first. Used to spell out the round's incremental facts to the planner
// alongside the finished worker's output. Best-effort: returns nil for an intent with
// no facts (or a missing one).
func (s *ExplorationStore) FactsYielded(intentID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT n.id
		FROM exploration_edges e
		JOIN exploration_nodes n ON n.id=e.dst_id AND n.exploration_id=e.exploration_id
		WHERE e.exploration_id=$1 AND e.src_id=$2 AND e.rel=$3 AND n.kind=$4
		ORDER BY n.id`, s.expID, intentID, RelYields, KindFact)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FindingLineage returns the sub-DAG that leads FROM the exploration root TO the
// given node: the node itself plus all its ancestors (nodes reverse-reachable by
// following edges backward), and every edge whose both endpoints are in that set.
// Used to render "how this finding was reached" — origin → … → finding. Returns
// empty (not error) for a nonexistent node.
func (s *ExplorationStore) FindingLineage(nodeID int64) ([]*Node, []Edge, error) {
	// anc = the node + everything that can reach it via edges (walk src<-dst).
	nodeRows, err := s.db.Query(`
WITH RECURSIVE anc(id) AS (
    SELECT $2::bigint
  UNION
    SELECT e.src_id
    FROM exploration_edges e
    JOIN anc ON e.dst_id = anc.id
    WHERE e.exploration_id = $1
)
SELECT `+nodeCols+`
FROM exploration_nodes
WHERE exploration_id = $1 AND id IN (SELECT id FROM anc)
ORDER BY id`, s.expID, nodeID)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := scanNodes(nodeRows)
	if err != nil {
		return nil, nil, err
	}
	if len(nodes) == 0 {
		return nil, nil, nil
	}
	// edges among the ancestor set only (the lineage's internal relations).
	edgeRows, err := s.db.Query(`
WITH RECURSIVE anc(id) AS (
    SELECT $2::bigint
  UNION
    SELECT e.src_id
    FROM exploration_edges e
    JOIN anc ON e.dst_id = anc.id
    WHERE e.exploration_id = $1
)
SELECT src_id, rel, dst_id
FROM exploration_edges
WHERE exploration_id = $1
  AND src_id IN (SELECT id FROM anc)
  AND dst_id IN (SELECT id FROM anc)`, s.expID, nodeID)
	if err != nil {
		return nil, nil, err
	}
	defer edgeRows.Close()
	var edges []Edge
	for edgeRows.Next() {
		var e Edge
		if err := edgeRows.Scan(&e.From, &e.Rel, &e.To); err != nil {
			return nil, nil, err
		}
		edges = append(edges, e)
	}
	return nodes, edges, edgeRows.Err()
}

// FindingIntents maps each finding id to the intent that produced it (the
// intent --yields--> finding edge; report_finding links it). Findings with no
// producing intent are absent. Precise (JOIN, no edge-scan limit).
func (s *ExplorationStore) FindingIntents() (map[int64]int64, error) {
	rows, err := s.db.Query(`SELECT e.dst_id, e.src_id
FROM exploration_edges e
JOIN exploration_nodes n ON n.id = e.dst_id AND n.exploration_id = e.exploration_id
WHERE e.exploration_id=$1 AND e.rel=$2 AND n.kind='finding'`, s.expID, RelYields)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var findingID, intentID int64
		if err := rows.Scan(&findingID, &intentID); err != nil {
			return nil, err
		}
		out[findingID] = intentID
	}
	return out, rows.Err()
}

// FindingIntentsTerminal is the inherited-history variant: it returns finding
// lineage only when the producing intent has reached an immutable terminal
// state. This keeps a source task's live worker topology private.
func (s *ExplorationStore) FindingIntentsTerminal() (map[int64]int64, error) {
	rows, err := s.db.Query(`SELECT e.dst_id, e.src_id
	FROM exploration_edges e
	JOIN exploration_nodes finding ON finding.id=e.dst_id AND finding.exploration_id=e.exploration_id
	JOIN exploration_nodes intent ON intent.id=e.src_id AND intent.exploration_id=e.exploration_id
	WHERE e.exploration_id=$1 AND e.rel=$2 AND finding.kind='finding'
	  AND intent.kind='intent' AND intent.state IN ('done','blocked','exhausted','stopped')`, s.expID, RelYields)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var findingID, intentID int64
		if err := rows.Scan(&findingID, &intentID); err != nil {
			return nil, err
		}
		out[findingID] = intentID
	}
	return out, rows.Err()
}

// Frontier returns open intents ordered by priority desc then id (FIFO tiebreak).
func (s *ExplorationStore) Frontier(limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='open'
ORDER BY priority DESC, id ASC LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// HasActiveIntent reports whether this exploration has any intent still in play
// (state open OR running). Used to decide whether to kick the FIRST planner round:
// a seeded task's intent may already have been claimed (open→running) by a worker
// before the check, so counting only 'open' (Frontier) races with worker claim and
// wrongly kicks a redundant first round. Counting open+running is race-proof.
func (s *ExplorationStore) HasActiveIntent() (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state IN ('open','running'))`, s.expID).Scan(&exists)
	return exists, err
}

// HasOpenGoal reports whether this exploration still has any goal in state 'open'.
// false ⇒ 全ゴールが met/abandoned(または本タスクにゴールがない)⇒ goalless(手動直接投入)
// の分岐へ入る：planner は停止し、タスクを終えるかは frontier が枯れたかで決まる。met と
// abandoned はいずれも「決着済み」とみなす。
func (s *ExplorationStore) HasOpenGoal() (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes
WHERE exploration_id=$1 AND kind='goal' AND state='open')`, s.expID).Scan(&exists)
	return exists, err
}

// ClaimIntent atomically moves an open intent to running. Returns true if claimed.
func (s *ExplorationStore) ClaimIntent(id int64, owner string) (bool, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes SET state='running', owner=$1
WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state='open'`, owner, id, s.expID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Stats returns node counts grouped by kind (for dashboard).
func (s *ExplorationStore) Stats() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT kind, count(*) FROM exploration_nodes WHERE exploration_id=$1 GROUP BY kind`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var c int
		if err := rows.Scan(&k, &c); err != nil {
			return nil, err
		}
		out[k] = c
	}
	return out, rows.Err()
}

// AssetRef is a compact exploration node (intent/fact/finding) that references an
// asset via an anchor — for the coverage graph's node drawer.
type AssetRef struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Summary      string `json:"summary"`
	SourceTaskID int64  `json:"source_task_id,omitempty"`
	Inherited    bool   `json:"inherited,omitempty"`
}

// AssetRefs returns the intents / facts / findings in THIS exploration anchored to
// the given asset id (newest first) — what this task tested / concluded about it.
func (s *ExplorationStore) AssetRefs(assetID int64) ([]AssetRef, error) {
	if assetID <= 0 {
		return []AssetRef{}, nil
	}
	rows, err := s.db.Query(`
SELECT en.id, en.kind, en.state, en.payload
FROM exploration_anchors ea
JOIN exploration_nodes en ON en.id = ea.node_id
WHERE ea.asset_id = $1 AND en.exploration_id = $2 AND en.kind IN ('intent','fact','finding')
ORDER BY en.id DESC`, assetID, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssetRef{}
	for rows.Next() {
		var r AssetRef
		var payload []byte
		if err := rows.Scan(&r.ID, &r.Kind, &r.State, &payload); err != nil {
			return nil, err
		}
		var p map[string]any
		_ = json.Unmarshal(payload, &p)
		for _, k := range []string{"summary", "text"} {
			if v, ok := p[k].(string); ok && strings.TrimSpace(v) != "" {
				r.Summary = v
				break
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
