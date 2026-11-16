package db

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Finding asset tree — the「アセット別」view of the global findings list.
//
// 階層は BuildCoverageGraph と同じ構造（company → root_domain/ip/app → subdomain →
// service → endpoint）だが、あちらは「あるタスクのスコープ内資産」の力学グラフで、
// こちらは「DB 全体でファインディングのある資産」のツリー：ファインディングのある
// 資産とその祖先チェーンだけを集め、ノードは部分ツリーの集計値を持つ。
// 両者の親子優先度規則は一致させること。片方を変える際は task_scope.go の
// 該当箇所も直す。
// ---------------------------------------------------------------------------

// FindingUnassignedAsset は「未紐づけアセット」のノード key であり、一覧 API の
// 絞り込み用センチネルでもある：asset_ids が空、または指す資産が削除済みの
// ファインディングにヒットする。
const FindingUnassignedAsset = "__none__"

// findingAssetTreeMaxNodes はフロントエンドへ返すノード上限。超過時は下から層ごと
// 捨てる（endpoint が先、次に service）：件数は既に親ノードへ累加済みで、ノードを
// 捨てても数字は失われない。
const findingAssetTreeMaxNodes = 3000

// FindingAssetNode は資産ツリーのノード。Key はカバレッジグラフと同構：資産行は
// "a:<id>"、企業は "c:<id>"、資産行のないルートドメインは合成の "r:<domain>"、
// 未紐づけバケットは "__none__"。
type FindingAssetNode struct {
	Key       string `json:"key"`
	Parent    string `json:"parent,omitempty"`
	Kind      string `json:"kind"` // company|root_domain|subdomain|ip|service|app|endpoint|none
	Label     string `json:"label"`
	AssetID   int64  `json:"asset_id,omitempty"`
	CompanyID int64  `json:"company_id,omitempty"`
	// Self はこの資産に直接紐づくファインディング数。Total は全子孫を含み、
	// finding 単位で重複排除する（1 件が複数資産に紐づく場合、共通祖先で 1 回だけ数える）。
	Self        int       `json:"self"`
	Total       int       `json:"total"`
	Critical    int       `json:"critical"`
	High        int       `json:"high"`
	Medium      int       `json:"medium"`
	Low         int       `json:"low"`
	LastFoundAt time.Time `json:"last_found_at"`
}

// FindingAssetTree はツリー全体のスナップショット。Nodes は整列済み：同一親の下では
// ファインディング数の降順、ラベルの昇順、「未紐づけアセット」は常に最後。
type FindingAssetTree struct {
	Nodes        []FindingAssetNode `json:"nodes"`
	FindingTotal int                `json:"finding_total"`
	// Truncated=true はサイズ制御のため DroppedKinds の階層を捨てたことを表す。
	Truncated    bool     `json:"truncated"`
	DroppedKinds []string `json:"dropped_kinds,omitempty"`
}

// assetRow はツリー構築に必要な資産フィールドの部分集合。
type assetRow struct {
	id          int64
	kind        string
	companyID   int64
	domain      string
	rootDomain  string
	ip          string
	url         string
	port        int
	serviceType string
	appName     string
}

func (a *assetRow) coverageNode() CoverageGraphNode {
	return CoverageGraphNode{
		Kind: a.kind, Domain: a.domain, RootDomain: a.rootDomain, IP: a.ip,
		URL: a.url, Port: a.port, ServiceType: a.serviceType, AppName: a.appName,
	}
}

// label はカバレッジグラフのラベル規則（URL > domain > ip > app_name > root_domain）
// を再利用するが、URL のないサービス（SMB、非 HTTP ポートなど）はポートを補う。
// そうしないとラベルが宿主の IP/ドメイン行と同一になり、親子 2 行が完全に重複して見える。
func (a *assetRow) label() string {
	if a.kind == "service" && a.url == "" {
		if host, port := a.hostPort(); host != "" && port > 0 {
			return host + ":" + strconv.Itoa(port)
		}
	}
	n := a.coverageNode()
	n.Key = assetKey(a.id)
	return coverageNodeLabel(&n)
}

// hostPort はカバレッジグラフと同一：domain を優先し、次に URL の host、最後に ip。
func (a *assetRow) hostPort() (string, int) {
	n := a.coverageNode()
	return hostPortOf(&n)
}

const findingAssetSelectCols = `a.id, a.type, COALESCE(a.company_id,0),
       COALESCE(a.domain,''), COALESCE(a.root_domain,''), COALESCE(a.ip,''),
       COALESCE(a.url,''), COALESCE(a.port,0), COALESCE(a.service_type,''),
       COALESCE(a.app_name,'')`

func scanAssetRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}) ([]*assetRow, error) {
	defer rows.Close()
	var out []*assetRow
	for rows.Next() {
		a := &assetRow{}
		if err := rows.Scan(&a.id, &a.kind, &a.companyID, &a.domain, &a.rootDomain,
			&a.ip, &a.url, &a.port, &a.serviceType, &a.appName); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// findingAssetHit はツリー構築段階でファインディング 1 件から必要な最小情報。
type findingAssetHit struct {
	severity string
	ts       time.Time
	assetIDs []int64
}

// BuildFindingAssetTree は現在の絞り込み条件で資産ツリーを構築する。AssetScope 自体は
// 反映しない（反映するとツリーが選択ノードへ縮退して 1 本の鎖になる）。
func (d *DB) BuildFindingAssetTree(f FindingFilter) (*FindingAssetTree, error) {
	return d.buildFindingAssetTree(f, findingAssetTreeMaxNodes)
}

// buildFindingAssetTree はノード上限付きの内部実装。maxNodes<=0 は非打ち切りを意味し、
// AssetScope を解決する際は必ずこのモードを使う。捨てた endpoint があると部分ツリーの
// id 集合が不完全になるため。
func (d *DB) buildFindingAssetTree(f FindingFilter, maxNodes int) (*FindingAssetTree, error) {
	f.AssetScope = ""
	f.assetIDs, f.assetNone, f.assetMiss = nil, false, false
	where, args := f.where()

	rows, err := d.Query(`SELECT COALESCE(f.severity,''), f.created_at,
       COALESCE(f.asset_ids::text,'[]')
FROM findings f LEFT JOIN tasks t ON f.task_id = t.id`+where, args...)
	if err != nil {
		return nil, err
	}
	hits := []findingAssetHit{}
	assetIDs := map[int64]bool{}
	for rows.Next() {
		var h findingAssetHit
		var aidsJSON string
		if err := rows.Scan(&h.severity, &h.ts, &aidsJSON); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(aidsJSON), &h.assetIDs)
		for _, id := range h.assetIDs {
			if id > 0 {
				assetIDs[id] = true
			}
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	tree := &FindingAssetTree{Nodes: []FindingAssetNode{}, FindingTotal: len(hits)}
	byID, err := d.loadFindingAssetRows(assetIDs)
	if err != nil {
		return nil, err
	}

	nodes, parentOf := d.assembleFindingAssetNodes(byID)
	if err := d.attachCompanyNodes(nodes, parentOf); err != nil {
		return nil, err
	}

	// 集計：1 件のファインディングについて各資産の祖先チェーンを上り、重複排除した
	// key 集合を集めてから 1 つずつ +1 するので、1 件が複数の子資産に紐づいても
	// 親ノードを二重計上しない。
	unassigned := &FindingAssetNode{Key: FindingUnassignedAsset, Kind: "none", Label: "未紐づけアセット"}
	touched := map[string]bool{}
	for _, h := range hits {
		clear(touched)
		var direct []*FindingAssetNode
		for _, id := range h.assetIDs {
			node := nodes[assetKey(id)]
			if node == nil {
				continue
			}
			direct = append(direct, node)
			for key := node.Key; key != ""; key = parentOf[key] {
				touched[key] = true
			}
		}
		if len(direct) == 0 {
			countFinding(unassigned, h)
			unassigned.Self++
			continue
		}
		for _, node := range direct {
			node.Self++
		}
		for key := range touched {
			countFinding(nodes[key], h)
		}
	}

	for _, node := range nodes {
		if node.Total > 0 {
			tree.Nodes = append(tree.Nodes, *node)
		}
	}
	if unassigned.Total > 0 {
		tree.Nodes = append(tree.Nodes, *unassigned)
	}
	sortFindingAssetNodes(tree.Nodes)
	truncateFindingAssetTree(tree, maxNodes)
	return tree, nil
}

// countFinding はファインディング 1 件をノードへ加算する（合計 / 深刻度別バケット / 最終発見時刻）。
func countFinding(n *FindingAssetNode, h findingAssetHit) {
	if n == nil {
		return
	}
	n.Total++
	switch h.severity {
	case "critical":
		n.Critical++
	case "high":
		n.High++
	case "medium":
		n.Medium++
	case "low":
		n.Low++
	}
	if h.ts.After(n.LastFoundAt) {
		n.LastFoundAt = h.ts
	}
}

// loadFindingAssetRows はヒットした資産行を読み、ラウンドごとに祖先を補う（service の
// 宿主ドメイン/IP、サブドメインのルートドメイン）。祖先自体にファインディングがなくても、
// ツリーの形を作るには必要。
func (d *DB) loadFindingAssetRows(ids map[int64]bool) (map[int64]*assetRow, error) {
	byID := map[int64]*assetRow{}
	if len(ids) == 0 {
		return byID, nil
	}
	idList := make([]int64, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	rows, err := d.Query(`SELECT `+findingAssetSelectCols+` FROM assets a WHERE a.id = ANY($1::bigint[])`, idList)
	if err != nil {
		return nil, err
	}
	found, err := scanAssetRows(rows)
	if err != nil {
		return nil, err
	}
	for _, a := range found {
		byID[a.id] = a
	}

	// 各ラウンドで親ノードを欠く宿主識別子を求め、1 層ずつまとめて補う。階層は固定
	// （endpoint→service→subdomain/ip→root_domain）なので 4 ラウンドで十分収束する。
	for range 4 {
		want := missingParents(byID)
		if want.empty() {
			break
		}
		added, err := d.loadAssetsByHost(want, byID)
		if err != nil {
			return nil, err
		}
		if added == 0 {
			break
		}
	}
	return byID, nil
}

// missingHosts は 1 ラウンドの補完で DB から探す宿主識別子。目的の資産種別ごとに分ける。
type missingHosts struct {
	services []string // endpoint の宿主（service 行を探す）
	domains  []string // service/endpoint の宿主ドメイン（subdomain 行を探す）
	ips      []string // service/endpoint の宿主 IP（ip 行を探す）
	roots    []string // サブドメインのルートドメイン（root_domain 行を探す）
}

func (m missingHosts) empty() bool {
	return len(m.services) == 0 && len(m.domains) == 0 && len(m.ips) == 0 && len(m.roots) == 0
}

// missingParents は未読み込みの宿主を集計する：service（endpoint の紐づけ先）、
// サブドメイン/IP（service と endpoint の紐づけ先）、ルートドメイン（サブドメインの
// 紐づけ先）。
func missingParents(byID map[int64]*assetRow) missingHosts {
	haveService := map[string]bool{}
	haveDomain := map[string]bool{}
	haveIP := map[string]bool{}
	haveRoot := map[string]bool{}
	for _, a := range byID {
		switch a.kind {
		case "service":
			if host, _ := a.hostPort(); host != "" {
				haveService[host] = true
			}
		case "subdomain":
			haveDomain[a.domain] = true
		case "ip":
			haveIP[a.ip] = true
		case "root_domain":
			haveRoot[a.domain] = true
		}
	}
	wantService := map[string]bool{}
	wantDomain := map[string]bool{}
	wantIP := map[string]bool{}
	wantRoot := map[string]bool{}
	for _, a := range byID {
		switch a.kind {
		case "service", "endpoint":
			host, _ := a.hostPort()
			// endpoint はまず同一宿主の service を探す。ポートが合わない service は
			// Total=0 として最終的に除外され、ツリーを汚さない。
			if a.kind == "endpoint" && host != "" && !haveService[host] {
				wantService[host] = true
			}
			if host != "" && !haveDomain[host] && !haveIP[host] && !haveRoot[host] {
				if isIPLiteral(host) {
					wantIP[host] = true
				} else {
					wantDomain[host] = true
				}
			}
			if a.ip != "" && !haveIP[a.ip] {
				wantIP[a.ip] = true
			}
		case "subdomain":
			if a.rootDomain != "" && !haveRoot[a.rootDomain] {
				wantRoot[a.rootDomain] = true
			}
		}
	}
	return missingHosts{
		services: keysOf(wantService),
		domains:  keysOf(wantDomain),
		ips:      keysOf(wantIP),
		roots:    keysOf(wantRoot),
	}
}

func keysOf(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// isIPLiteral は host が IP リテラルかを粗く判定する（宿主を ip 表と subdomain 表の
// どちらで探すかの判断に使う）。
func isIPLiteral(host string) bool {
	if strings.Contains(host, ":") {
		return true // IPv6
	}
	if host == "" {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return strings.Count(host, ".") == 3
}

// loadAssetsByHost は宿主識別子ごとに資産行をまとめて補い、今回追加した行数を返す。
func (d *DB) loadAssetsByHost(want missingHosts, byID map[int64]*assetRow) (int, error) {
	added := 0
	load := func(q string, arg []string) error {
		if len(arg) == 0 {
			return nil
		}
		rows, err := d.Query(q, arg)
		if err != nil {
			return err
		}
		found, err := scanAssetRows(rows)
		if err != nil {
			return err
		}
		for _, a := range found {
			if _, ok := byID[a.id]; ok {
				continue
			}
			byID[a.id] = a
			added++
		}
		return nil
	}
	if err := load(`SELECT `+findingAssetSelectCols+` FROM assets a
WHERE a.type='service' AND (a.domain = ANY($1::text[]) OR a.ip = ANY($1::text[]))`, want.services); err != nil {
		return 0, err
	}
	if err := load(`SELECT `+findingAssetSelectCols+` FROM assets a
WHERE a.type='subdomain' AND a.domain = ANY($1::text[])`, want.domains); err != nil {
		return 0, err
	}
	if err := load(`SELECT `+findingAssetSelectCols+` FROM assets a
WHERE a.type='ip' AND a.ip = ANY($1::text[])`, want.ips); err != nil {
		return 0, err
	}
	if err := load(`SELECT `+findingAssetSelectCols+` FROM assets a
WHERE a.type='root_domain' AND a.domain = ANY($1::text[])`, want.roots); err != nil {
		return 0, err
	}
	return added, nil
}

// assembleFindingAssetNodes は資産行をノード化して親子関係を結ぶ。親ノードがない
// 場合（そのルートドメイン資産が DB に存在しない）は "r:<domain>" のプレースホルダを
// 合成する。カバレッジグラフの扱いと同一。
func (d *DB) assembleFindingAssetNodes(byID map[int64]*assetRow) (map[string]*FindingAssetNode, map[string]string) {
	nodes := map[string]*FindingAssetNode{}
	parentOf := map[string]string{}
	rootByDomain := map[string]string{}
	subByDomain := map[string]string{}
	ipByAddr := map[string]string{}
	svcByHost := map[string]string{}
	svcByHostPort := map[string]string{}

	for _, a := range byID {
		key := assetKey(a.id)
		nodes[key] = &FindingAssetNode{
			Key: key, Kind: a.kind, Label: a.label(),
			AssetID: a.id, CompanyID: a.companyID,
		}
		switch a.kind {
		case "root_domain":
			if a.domain != "" {
				rootByDomain[a.domain] = key
			}
		case "subdomain":
			if a.domain != "" {
				subByDomain[a.domain] = key
			}
		case "ip":
			if a.ip != "" {
				ipByAddr[a.ip] = key
			}
		case "service":
			if host, port := a.hostPort(); host != "" {
				svcByHost[host] = key
				svcByHostPort[host+"|"+strconv.Itoa(port)] = key
			}
		}
	}

	// サブドメインのルートドメインに資産行がない場合はプレースホルダのルートを合成し、
	// サブドメインが最上位へ散らばるのを防ぐ。
	for _, a := range byID {
		if a.kind != "subdomain" || a.rootDomain == "" {
			continue
		}
		if _, ok := rootByDomain[a.rootDomain]; ok {
			continue
		}
		key := "r:" + a.rootDomain
		nodes[key] = &FindingAssetNode{Key: key, Kind: "root_domain", Label: a.rootDomain}
		rootByDomain[a.rootDomain] = key
	}

	firstOf := func(keys ...string) string {
		for _, k := range keys {
			if k != "" {
				if _, ok := nodes[k]; ok {
					return k
				}
			}
		}
		return ""
	}
	for _, a := range byID {
		key := assetKey(a.id)
		var parent string
		switch a.kind {
		case "subdomain":
			parent = firstOf(rootByDomain[a.rootDomain])
		case "service":
			host, _ := a.hostPort()
			parent = firstOf(subByDomain[a.domain], subByDomain[host],
				ipByAddr[a.ip], ipByAddr[host], rootByDomain[a.rootDomain], rootByDomain[host])
		case "endpoint":
			host, port := a.hostPort()
			parent = firstOf(svcByHostPort[host+"|"+strconv.Itoa(port)], svcByHost[host],
				subByDomain[host], subByDomain[a.domain], ipByAddr[host], ipByAddr[a.ip],
				rootByDomain[a.rootDomain], rootByDomain[host])
		}
		if parent != "" && parent != key {
			parentOf[key] = parent
			nodes[key].Parent = parent
		}
	}
	return nodes, parentOf
}

// attachCompanyNodes は最上位の資産（ルートドメイン / IP / アプリケーション）へ企業の
// 親ノードを補う。資産が実際に企業へ属する場合だけ企業層が現れ、属さない資産はそのまま
// 最上位に残る。
func (d *DB) attachCompanyNodes(nodes map[string]*FindingAssetNode, parentOf map[string]string) error {
	want := map[int64]bool{}
	for _, n := range nodes {
		if n.Parent != "" || n.CompanyID <= 0 {
			continue
		}
		switch n.Kind {
		case "root_domain", "ip", "app":
			want[n.CompanyID] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	rows, err := d.Query(`SELECT id, COALESCE(name,'') FROM companies WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	names := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		names[id] = name
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, name := range names {
		key := companyKey(id)
		if _, ok := nodes[key]; ok {
			continue
		}
		if name == "" {
			name = "企業 #" + strconv.FormatInt(id, 10)
		}
		nodes[key] = &FindingAssetNode{Key: key, Kind: "company", Label: name, CompanyID: id}
	}
	for _, n := range nodes {
		if n.Parent != "" || n.CompanyID <= 0 || n.Kind == "company" {
			continue
		}
		switch n.Kind {
		case "root_domain", "ip", "app":
			key := companyKey(n.CompanyID)
			if _, ok := nodes[key]; !ok {
				continue
			}
			n.Parent = key
			parentOf[n.Key] = key
		}
	}
	return nil
}

// sortFindingAssetNodes は並べ替え：ファインディングが多い順、同数ならラベル順、
// 「未紐づけアセット」は常に最後。フロントエンドは配列順に子ノードを付けるので、
// 同一親の下での相対順序が正しければよい。
func sortFindingAssetNodes(nodes []FindingAssetNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if (a.Kind == "none") != (b.Kind == "none") {
			return b.Kind == "none"
		}
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Label < b.Label
	})
}

// truncateFindingAssetTree はノード過多時に層ごと捨てる（endpoint が先、次に service）。
// 件数は既に親ノードへ累加済みで、失うのは展開可能な詳細階層だけ。
func truncateFindingAssetTree(tree *FindingAssetTree, maxNodes int) {
	if maxNodes <= 0 || len(tree.Nodes) <= maxNodes {
		return
	}
	for _, kind := range []string{"endpoint", "service"} {
		kept := tree.Nodes[:0]
		for _, n := range tree.Nodes {
			if n.Kind == kind {
				continue
			}
			kept = append(kept, n)
		}
		tree.Nodes = kept
		tree.Truncated = true
		tree.DroppedKinds = append(tree.DroppedKinds, kind)
		if len(tree.Nodes) <= maxNodes {
			return
		}
	}
}

// applyAssetScope は AssetScope（ノード key）を SQL で使える資産 id 集合へ解決する。
// 1 つのノードの選択はその部分ツリー全体の選択に等しいので、まずツリーを構築してから
// 子孫を集める。
func (d *DB) applyAssetScope(f FindingFilter) (FindingFilter, error) {
	scope := strings.TrimSpace(f.AssetScope)
	f.assetIDs, f.assetNone, f.assetMiss = nil, false, false
	if scope == "" {
		return f, nil
	}
	if scope == FindingUnassignedAsset {
		f.assetNone = true
		return f, nil
	}
	// 非打ち切り：捨てられた endpoint も id 収集に参加させる必要があり、そうしないと
	// 一覧のデータが欠ける。
	tree, err := d.buildFindingAssetTree(f, 0)
	if err != nil {
		return f, err
	}
	children := map[string][]FindingAssetNode{}
	byKey := map[string]FindingAssetNode{}
	for _, n := range tree.Nodes {
		byKey[n.Key] = n
		children[n.Parent] = append(children[n.Parent], n)
	}
	if _, ok := byKey[scope]; !ok {
		// 選択ノードが現在の絞り込み条件で既に存在しない場合は、絞り込みなしへ退化させず、
		// 結果を空にする。
		f.assetMiss = true
		return f, nil
	}
	seen := map[string]bool{scope: true}
	queue := []string{scope}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if id := byKey[key].AssetID; id > 0 {
			f.assetIDs = append(f.assetIDs, id)
		}
		for _, child := range children[key] {
			if seen[child.Key] {
				continue
			}
			seen[child.Key] = true
			queue = append(queue, child.Key)
		}
	}
	if len(f.assetIDs) == 0 {
		f.assetMiss = true
	}
	return f, nil
}

// assetIDContainments は資産 id を jsonb 包含判定の右オペランド集合へ変換する。
// idx_findings_asset_ids（GIN jsonb_path_ops）と組み合わせて使う。
func assetIDContainments(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "["+strconv.FormatInt(id, 10)+"]")
	}
	return out
}
