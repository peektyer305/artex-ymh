package server

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/tunnel"
)

// fakeTunnelLister は注入式のトンネル台帳(taskProxyManager の ListByTask データソース)。
type fakeTunnelLister struct {
	recs []*db.TunnelRecord
	err  error
}

func (f *fakeTunnelLister) ListByTask(context.Context, int64) ([]*db.TunnelRecord, error) {
	return f.recs, f.err
}

func socksRec(id int64, state string, socksPort int) *db.TunnelRecord {
	raw, _ := json.Marshal(tunnel.Plan{Kind: tunnel.KindSocks, SocksPort: socksPort})
	return &db.TunnelRecord{ID: id, Kind: tunnel.KindSocks, State: state, DeployParams: raw}
}

// suo5Rec は --auth 付きの suo5 トンネル台帳行を作る（上流 URL には user:pass を埋め込む必要がある）。
func suo5Rec(id int64, state string, socksPort int) *db.TunnelRecord {
	raw, _ := json.Marshal(tunnel.Plan{Kind: tunnel.KindSocks, SocksPort: socksPort, AuthUser: "artex", AuthPass: "deadbeef"})
	return &db.TunnelRecord{ID: id, Kind: tunnel.KindSocks, Adapter: tunnel.AdapterSuo5, State: state, DeployParams: raw}
}

// pickUpstream:最新の alive な socks トンネルが勝つ;トンネルがなければグローバルプロキシに戻る;portfwd/非 alive は対象外。
func TestPickUpstream(t *testing.T) {
	const global = "http://127.0.0.1:9000"
	cases := []struct {
		name string
		recs []*db.TunnelRecord
		want string
	}{
		{"トンネルなしはグローバルへ回落", nil, global},
		{"alive な socks が選ばれる", []*db.TunnelRecord{socksRec(1, db.TunnelAlive, 20801)}, "socks5://127.0.0.1:20801"},
		{"複数なら id 最大を取得", []*db.TunnelRecord{
			socksRec(1, db.TunnelAlive, 20801), socksRec(3, db.TunnelAlive, 20803), socksRec(2, db.TunnelAlive, 20802),
		}, "socks5://127.0.0.1:20803"},
		{"error/stopped は対象外", []*db.TunnelRecord{
			socksRec(1, db.TunnelError, 20801), socksRec(2, db.TunnelStopped, 20802),
		}, global},
		{"portfwd は上流ではない", []*db.TunnelRecord{{
			ID: 1, Kind: tunnel.KindPortfwd, State: db.TunnelAlive,
			DeployParams: json.RawMessage(`{"kind":"portfwd","local_port":20810}`),
		}}, global},
		{"deploy_params に socks_port がなければスキップ", []*db.TunnelRecord{
			{ID: 1, Kind: tunnel.KindSocks, State: db.TunnelAlive, DeployParams: json.RawMessage(`{}`)},
		}, global},
		{"suo5 上流は auth を埋め込む", []*db.TunnelRecord{suo5Rec(9, db.TunnelAlive, 20809)}, "socks5://artex:deadbeef@127.0.0.1:20809"},
	}
	for _, c := range cases {
		if got := pickUpstream(c.recs, global); got != c.want {
			t.Errorf("%s: pickUpstream = %q, want %q", c.name, got, c.want)
		}
	}
	// グローバルが空(直接接続)のときは、トンネル上流もないので空になる。
	if got := pickUpstream(nil, ""); got != "" {
		t.Errorf("トンネルもグローバルプロキシもなければ直接接続(空)になるべき, got %q", got)
	}
}

// freePort は現在バインド可能なポートを 1 つ取得する(システムに返却する;トンネルの CanListen と
// 同じ微小な競合があり、テストでは許容する)。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("空きポートの取得: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func managedPortRegistered(port int) bool {
	_, ok := dynManagedPorts.Load(port)
	return ok
}

// 遅延生成 → ポート登録 → 上流のホットスイッチ(トンネルの出現/消滅)→ CloseTask のクリーンアップ → 再度の遅延生成。
func TestTaskProxyLifecycle(t *testing.T) {
	port := freePort(t)
	lister := &fakeTunnelLister{}
	m := newTaskProxyManager(t.TempDir(), lister, func() string { return "http://127.0.0.1:9000" }, port, port)
	ctx := context.Background()

	addr, ca := m.ForTask(ctx, 42)
	if addr == "" || ca == "" {
		t.Fatalf("ForTask はインスタンスのアドレスと CA を返すべき, got (%q, %q)", addr, ca)
	}
	if !strings.Contains(addr, ":"+strconv.Itoa(port)) {
		t.Errorf("インスタンスのアドレスは割り当てポート %d を含むべき: %s", port, addr)
	}
	if _, err := os.Stat(ca); err != nil {
		t.Errorf("インスタンスの CA 証明書は保存済みであるべき %s: %v", ca, err)
	}
	if !managedPortRegistered(port) {
		t.Error("インスタンスのポートはポート監査台帳に登録されるべき(registerManagedPort)")
	}
	inst := m.proxies[42]
	if inst == nil {
		t.Fatal("インスタンスは表に登録済みであるべき")
	}
	if got := inst.tr.UpstreamProxy(); got != "http://127.0.0.1:9000" {
		t.Errorf("トンネルがないときの上流はグローバルプロキシへ回落すべき, got %q", got)
	}

	// 冪等:再度 ForTask しても同じインスタンスを再利用する(ポートは変わらない)。
	addr2, _ := m.ForTask(ctx, 42)
	if addr2 != addr {
		t.Errorf("ForTask の繰り返しでインスタンスを再作成しない: %q → %q", addr, addr2)
	}

	// トンネル出現 → Refresh で socks 上流へホットスイッチ。
	lister.recs = []*db.TunnelRecord{socksRec(7, db.TunnelAlive, 20807)}
	m.Refresh(ctx, 42)
	if got := inst.tr.UpstreamProxy(); got != "socks5://127.0.0.1:20807" {
		t.Errorf("トンネルが alive になったら上流をホットスイッチすべき, got %q", got)
	}

	// トンネル死亡 → グローバルプロキシに戻る。
	lister.recs = []*db.TunnelRecord{socksRec(7, db.TunnelError, 20807)}
	m.Refresh(ctx, 42)
	if got := inst.tr.UpstreamProxy(); got != "http://127.0.0.1:9000" {
		t.Errorf("トンネル死亡後の上流はグローバルプロキシへ回落すべき, got %q", got)
	}

	// インスタンスのないタスクの Refresh は no-op で、遅延生成を起こさない。
	m.Refresh(ctx, 99)
	if m.proxies[99] != nil {
		t.Error("Refresh はインスタンスのないタスクにインスタンスを作らない(遅延生成の意味)")
	}

	// CloseTask:ポートの登録解除、インスタンスの表外し、ディレクトリ(CA/記録ツリー)は保持。
	m.CloseTask(42)
	if managedPortRegistered(port) {
		t.Error("CloseTask 後はポートを登録解除すべき")
	}
	if m.proxies[42] != nil {
		t.Error("CloseTask 後はインスタンスを表から外すべき")
	}
	if _, err := os.Stat(ca); err != nil {
		t.Errorf("CloseTask 後も CA ディレクトリは残すべき(証跡), stat %s: %v", ca, err)
	}
	m.CloseTask(42) // 冪等で、panic しない

	// 再度の遅延生成:新しいインスタンスを取得する(同じポートプールに 1 つしか残っていないので同じポートを再利用)。
	addr3, _ := m.ForTask(ctx, 42)
	if addr3 == "" {
		t.Fatal("CloseTask 後は再び遅延生成できるべき")
	}
	m.Close()
	if managedPortRegistered(port) {
		t.Error("Close 後はポートをすべて登録解除すべき")
	}
}
