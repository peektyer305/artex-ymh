package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
)

func TestParseLsLa(t *testing.T) {
	out := `total 48
drwxr-xr-x  5 root root 4096 Sep 12 10:00 .
drwxr-xr-x 22 root root 4096 Sep  1 08:00 ..
-rw-r--r--  1 root root  220 Sep 10  2023 .bash_logout
-rw-r--r--  1 root root 3771 Sep 10  2023 .bashrc
drwxr-xr-x  2 root root 4096 Aug 30 11:11 my dir
lrwxrwxrwx  1 root root   11 Sep 12 09:59 link -> /etc/passwd
-rw-------  1 www-data www-data 123456 Sep 12 12:00 dump.sql
not a valid line at all
`
	entries := parseLsLa(out)
	if len(entries) != 5 {
		t.Fatalf("5 件を期待したが %d 件だった: %+v", len(entries), entries)
	}
	byName := map[string]dirEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if e, ok := byName[".bashrc"]; !ok || e.IsDir || e.Size != 3771 {
		t.Errorf(".bashrc の解析が不正: %+v", e)
	}
	if e, ok := byName["my dir"]; !ok || !e.IsDir || e.Size != 4096 {
		t.Errorf("スペースを含むディレクトリの解析が不正: %+v", e)
	}
	if e, ok := byName["link"]; !ok || e.IsDir || e.Size != 11 {
		t.Errorf("symlink は -> target を除き、ディレクトリでないべき: %+v", e)
	}
	if e, ok := byName["dump.sql"]; !ok || e.IsDir || e.Size != 123456 {
		t.Errorf("dump.sql の解析が不正: %+v", e)
	}
	if _, ok := byName["."]; ok {
		t.Error(". はスキップされるべき")
	}
	if _, ok := byName[".."]; ok {
		t.Error(".. はスキップされるべき")
	}
}

func TestParseLsLaEmpty(t *testing.T) {
	if got := parseLsLa(""); len(got) != 0 {
		t.Fatalf("空の出力なら空のリストになるべき: %+v", got)
	}
	if got := parseLsLa("total 0\n"); len(got) != 0 {
		t.Fatalf("total 行だけなら空のリストになるべき: %+v", got)
	}
}

func TestSegmentOf(t *testing.T) {
	if got := segmentOf("10.0.0.5", "192.168.1.0/24"); got != "192.168.1.0/24" {
		t.Errorf("c_segment を優先すべき: %s", got)
	}
	if got := segmentOf("10.0.0.5", ""); got != "10.0.0.0/24" {
		t.Errorf("IPv4 は /24 を導出すべき: %s", got)
	}
	if got := segmentOf("not-an-ip", ""); got != "" {
		t.Errorf("不正な IP は空になるべき: %q", got)
	}
}

func TestBuildTopology(t *testing.T) {
	assets := []*db.Asset{
		{ID: 1, IP: "10.0.0.5", CSegment: "10.0.0.0/24",
			OpenPorts: []map[string]any{{"port": float64(22), "service": "ssh"}, {"port": float64(80)}}},
		{ID: 2, IP: "10.0.1.7"},
	}
	recs := []*db.SessionRecord{
		{ID: 11, HostAssetID: 1, URL: "http://10.0.0.5/shell.php", Status: db.SessionAlive, CreatedByTask: 3},
		{ID: 12, URL: "http://10.0.9.9:8080/s.jsp", Status: db.SessionAlive, CreatedByTask: 3}, // アセットなし → 合成ノード
		{ID: 13, HostAssetID: 2, URL: "http://10.0.1.7/x.php", Status: db.SessionDead, CreatedByTask: 3},
	}
	tuns := []*db.TunnelRecord{
		{ID: 21, TaskID: 3, Kind: "socks", ViaSessionID: 11, State: db.TunnelAlive},
		{ID: 22, TaskID: 3, Kind: "portfwd", ViaSessionID: 12, TargetHost: "10.0.0.8", TargetPort: 3306, State: db.TunnelAlive},
	}
	topo := buildTopology(assets, recs, tuns, "10.0.0.5")

	nodes := topo["nodes"].([]*topoNode)
	// アセット 2 + 合成 sess 1 + 合成 pf 1(portfwd のターゲットが台帳にない) + platform
	if len(nodes) != 5 {
		t.Fatalf("ノード数は 5 であるべき: %d", len(nodes))
	}
	byID := map[string]*topoNode{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	n5 := byID["asset-1"]
	if n5 == nil || !n5.HasSession || n5.AliveSessions != 1 || !n5.IsCallback {
		t.Errorf("10.0.0.5 のノードが不正: %+v", n5)
	}
	if n5.IP != "10.0.0.5" {
		t.Errorf("アセットノードの ip は 10.0.0.5 であるべき: %s", n5.IP)
	}
	if len(n5.Services) != 2 {
		t.Errorf("services は 2 件であるべき: %+v", n5.Services)
	}
	n7 := byID["asset-2"]
	if n7 == nil || !n7.HasSession || n7.AliveSessions != 0 {
		t.Errorf("dead セッションのホストは has_session=true だが alive=0 であるべき: %+v", n7)
	}
	n9 := byID["sess-12"]
	if n9 == nil || n9.IP != "10.0.9.9" || n9.Segment != "10.0.9.0/24" {
		t.Errorf("合成ノードが不正: %+v", n9)
	}
	// プラットフォーム自身の合成ノード:トンネルの辺はすべてここから出る;ラベルはコールバックアドレスを使い、星印は「プラットフォーム自身」の凡例を流用する。
	platform := byID["platform"]
	if platform == nil || !platform.IsCallback || platform.IP != "10.0.0.5" {
		t.Errorf("platform ノードが不正: %+v", platform)
	}
	// portfwd のターゲット 10.0.0.8 がアセット台帳にない → 合成ノード pf-<tunnel_id>、ラベルは host:port。
	pf := byID["pf-22"]
	if pf == nil || pf.IP != "10.0.0.8:3306" {
		t.Errorf("pf の合成ノードが不正: %+v", pf)
	}

	edges := topo["edges"].([]topoEdge)
	if len(edges) != 2 {
		t.Fatalf("辺の数は 2 であるべき: %d", len(edges))
	}
	// 辺の from/to は必ずノード id:socks の辺は platform → 経由セッションのホストノード。
	if edges[0].From != "platform" || edges[0].To != "asset-1" {
		t.Errorf("socks の辺は platform → セッションのホストノードであるべき: %+v", edges[0])
	}
	if edges[1].From != "platform" || edges[1].To != "pf-22" {
		t.Errorf("portfwd の辺は platform → pf 合成ノードであるべき: %+v", edges[1])
	}

	segs := topo["segments"].([]topoSegment)
	if len(segs) != 3 { // 10.0.0.0/24, 10.0.1.0/24, 10.0.9.0/24
		t.Fatalf("セグメント数は 3 であるべき: %+v", segs)
	}
	if segs[0].ColorKey == "" || segs[0].Name != "10.0.0.0/24" {
		t.Errorf("セグメントの色キー/並び順が不正: %+v", segs)
	}

	// JSON にシリアライズできる(ページが直接消費する)。
	if _, err := json.Marshal(topo); err != nil {
		t.Fatalf("トポロジの JSON シリアライズに失敗: %v", err)
	}
}

func TestBuildTopologyEmpty(t *testing.T) {
	topo := buildTopology(nil, nil, nil, "")
	if len(topo["nodes"].([]*topoNode)) != 0 ||
		len(topo["edges"].([]topoEdge)) != 0 ||
		len(topo["segments"].([]topoSegment)) != 0 {
		t.Fatalf("空の入力なら空のトポロジになるべき: %+v", topo)
	}
}

// TestBuildTopologyEdgeEndpoints はトポロジの不変条件:各辺の from/to は必ずノード
// id の集合に含まれる(G6:存在しないノードを指す辺は線を描けない);経由セッションがビュー内にないトンネルはスキップ。
func TestBuildTopologyEdgeEndpoints(t *testing.T) {
	assets := []*db.Asset{{ID: 1, IP: "10.0.0.5"}}
	recs := []*db.SessionRecord{
		{ID: 11, HostAssetID: 1, URL: "http://10.0.0.5/shell.php", Status: db.SessionAlive},
	}
	tuns := []*db.TunnelRecord{
		{ID: 21, Kind: "socks", ViaSessionID: 11, State: db.TunnelAlive},
		{ID: 22, Kind: "portfwd", ViaSessionID: 11, TargetHost: "10.0.0.5", TargetPort: 3306, State: db.TunnelAlive}, // アセットに一致
		{ID: 23, Kind: "portfwd", ViaSessionID: 11, TargetHost: "10.0.0.8", TargetPort: 445, State: db.TunnelAlive},  // 合成 pf ノード
		{ID: 24, Kind: "socks", ViaSessionID: 99, State: db.TunnelAlive},                                            // セッションがビュー内にない → スキップ
	}
	topo := buildTopology(assets, recs, tuns, "")

	ids := map[string]bool{}
	for _, n := range topo["nodes"].([]*topoNode) {
		ids[n.ID] = true
	}
	edges := topo["edges"].([]topoEdge)
	if len(edges) != 3 {
		t.Fatalf("辺の数は 3 であるべき(ビュー内にないセッションのトンネルはスキップ): %d", len(edges))
	}
	for _, e := range edges {
		if !ids[e.From] {
			t.Errorf("辺 %d の from=%q がノード集合にない", e.ID, e.From)
		}
		if !ids[e.To] {
			t.Errorf("辺 %d の to=%q がノード集合にない", e.ID, e.To)
		}
	}
	// portfwd がアセットの IP に一致した場合はそのアセットノードへ直接繋ぐ。
	if edges[1].To != "asset-1" {
		t.Errorf("portfwd がアセットに一致したら asset-1 に繋ぐべき: %+v", edges[1])
	}
}

// openIntranetTestDB はテスト用 DB に接続する;DB がない場合はスキップ(intranet_probe_test.go と同じ意味論)。
func openIntranetTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

// doListItems は台帳一覧を GET して items を取り出す;200 以外はそのまま fatal。
func doListItems(t *testing.T, h http.HandlerFunc, url string) []any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s のステータスコード = %d, body = %q", url, w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("レスポンス JSON の解析に失敗: %v (%q)", err, w.Body.String())
	}
	items, _ := body["items"].([]any)
	return items
}

// expectBadTaskID は不正な task_id を一律 400 とする。
func expectBadTaskID(t *testing.T, h http.HandlerFunc, url string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET %s で不正な task_id は 400 であるべき: %d", url, w.Code)
	}
}

// TestIntranetListSessionsTaskFilter は ?task_id= で本タスクが作成したセッションだけを残す;指定なしなら全体を保つ。
func TestIntranetListSessionsTaskFilter(t *testing.T) {
	pg := openIntranetTestDB(t)
	store := db.NewSessionStore(pg, []byte("intranet-test-master-key"))
	s := &Server{sessStore: store, sessReg: session.NewRegistry()}
	ctx := context.Background()
	mk := func(taskID int64) int64 {
		t.Helper()
		id, err := store.Create(ctx, &db.SessionRecord{
			Kind: "http_php", URL: "http://10.199.0.1/s.php", Lang: "php",
			Status: db.SessionAlive, CreatedByTask: taskID,
		})
		if err != nil {
			t.Fatalf("セッションの永続化に失敗: %v", err)
		}
		t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
		return id
	}
	id1 := mk(980001)
	mk(980002)

	items := doListItems(t, s.intranetListSessions, "/api/sessions?task_id=980001")
	if len(items) != 1 {
		t.Fatalf("task_id=980001 では 1 件だけ残るべき: %d", len(items))
	}
	it, _ := items[0].(map[string]any)
	if it["id"].(float64) != float64(id1) || it["created_by_task"].(float64) != 980001 {
		t.Errorf("セッションの絞り込み結果が不正: %+v", it)
	}

	// task_id を指定しなければ全体ビューを保つ(少なくとも今作った 2 件を含む)。旧フロントとの互換。
	if all := doListItems(t, s.intranetListSessions, "/api/sessions"); len(all) < 2 {
		t.Fatalf("全体ビューは少なくとも 2 件であるべき: %d", len(all))
	}
	expectBadTaskID(t, s.intranetListSessions, "/api/sessions?task_id=abc")
}

// TestIntranetListTunnelsTaskFilter は ?task_id= で ListByTask を通す;指定なしなら全体を保つ。
func TestIntranetListTunnelsTaskFilter(t *testing.T) {
	pg := openIntranetTestDB(t)
	s := &Server{m: &Manager{pg: pg}}
	store := db.NewTunnelStore(pg)
	ctx := context.Background()
	mk := func(taskID int64) int64 {
		t.Helper()
		id, err := store.Create(ctx, &db.TunnelRecord{
			TaskID: taskID, Kind: "socks", ListenHost: "127.0.0.1", ListenPort: 11080,
			State: db.TunnelAlive,
		})
		if err != nil {
			t.Fatalf("トンネルの永続化に失敗: %v", err)
		}
		t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
		return id
	}
	id1 := mk(980011)
	mk(980012)

	items := doListItems(t, s.intranetListTunnels, "/api/tunnels?task_id=980011")
	if len(items) != 1 {
		t.Fatalf("task_id=980011 では 1 件だけ残るべき: %d", len(items))
	}
	it, _ := items[0].(map[string]any)
	if it["id"].(float64) != float64(id1) || it["task_id"].(float64) != 980011 {
		t.Errorf("トンネルの絞り込み結果が不正: %+v", it)
	}

	if all := doListItems(t, s.intranetListTunnels, "/api/tunnels"); len(all) < 2 {
		t.Fatalf("全体ビューは少なくとも 2 件であるべき: %d", len(all))
	}
	expectBadTaskID(t, s.intranetListTunnels, "/api/tunnels?task_id=-1")
}

// TestIntranetListCredentialsTaskFilter は ?task_id= で ListByTask を通す;指定なしなら全体を保つ。
func TestIntranetListCredentialsTaskFilter(t *testing.T) {
	pg := openIntranetTestDB(t)
	cs := db.NewCredentialStore(pg, []byte("intranet-test-master-key"))
	s := &Server{credStore: cs}
	ctx := context.Background()
	mk := func(taskID int64) int64 {
		t.Helper()
		id, err := cs.Create(ctx, &db.CredentialRecord{
			TaskID: taskID, Username: "admin", CredType: "password",
			Secret: "s3cret-value", Source: "test",
		})
		if err != nil {
			t.Fatalf("認証情報の永続化に失敗: %v", err)
		}
		t.Cleanup(func() { _ = cs.Delete(context.Background(), id) })
		return id
	}
	id1 := mk(980021)
	mk(980022)

	items := doListItems(t, s.intranetListCredentials, "/api/credentials?task_id=980021")
	if len(items) != 1 {
		t.Fatalf("task_id=980021 では 1 件だけ残るべき: %d", len(items))
	}
	it, _ := items[0].(map[string]any)
	if it["id"].(float64) != float64(id1) || it["task_id"].(float64) != 980021 {
		t.Errorf("認証情報の絞り込み結果が不正: %+v", it)
	}

	if all := doListItems(t, s.intranetListCredentials, "/api/credentials"); len(all) < 2 {
		t.Fatalf("全体ビューは少なくとも 2 件であるべき: %d", len(all))
	}
	expectBadTaskID(t, s.intranetListCredentials, "/api/credentials?task_id=0")
}
