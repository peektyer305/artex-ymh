// plan_test.go は計画組み立ての純関数単体テスト：ポートプール解析、ポート割り当て、
// コマンドライン組み立て（auth はコマンドラインに入れない）、ロールバック手順の順序。
package tunnel

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestParsePortRange(t *testing.T) {
	cases := []struct {
		in      string
		wantMin int
		wantMax int
		wantErr bool
	}{
		{"20000-21000", 20000, 21000, false},
		{"", 20000, 21000, false}, // 空 = デフォルトプール
		{" 8080 - 8085 ", 8080, 8085, false},
		{"20000", 0, 0, true},   // - がない
		{"0-100", 0, 0, true},   // min が不正
		{"100-99", 0, 0, true},  // min>max
		{"1-70000", 0, 0, true}, // max が範囲外
		{"abc-def", 0, 0, true},
	}
	for _, c := range cases {
		min, max, err := ParsePortRange(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParsePortRange(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && (min != c.wantMin || max != c.wantMax) {
			t.Errorf("ParsePortRange(%q) = %d-%d, want %d-%d", c.in, min, max, c.wantMin, c.wantMax)
		}
	}
}

func TestAllocatePort(t *testing.T) {
	used := map[int]bool{20000: true, 20001: true}
	closed := map[int]bool{20002: true} // 空きだが別プロセスが占有中
	canListen := func(p int) bool { return !closed[p] }
	p, err := AllocatePort(20000, 20010, used, canListen)
	if err != nil {
		t.Fatal(err)
	}
	if p != 20003 {
		t.Errorf("AllocatePort = %d, want 20003（台帳の占有とバインド失敗をスキップ）", p)
	}
	// すべて利用不可 → 明確なエラー
	if _, err := AllocatePort(20000, 20002, used, canListen); err == nil {
		t.Error("ポートプールが枯渇したらエラーになるべきです")
	}
	// nil used / nil canListen でも動作する
	p, err = AllocatePort(21000, 21001, nil, nil)
	if err != nil || p != 21000 {
		t.Errorf("AllocatePort(nil) = %d, %v", p, err)
	}
}

func testOpts() *PlanOpts {
	return &PlanOpts{
		TaskID: 7, ViaSessionID: 3, CallbackHost: "10.0.0.5",
		PortMin: 20000, PortMax: 20100, UsedPorts: map[int]bool{},
		CanListen: func(int) bool { return true },
		DataDir:   "/data/tunnels",
	}
}

func TestPlanSocksAssembly(t *testing.T) {
	p, err := PlanSocks(testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindSocks || p.Adapter != AdapterChisel {
		t.Errorf("kind/adapter = %s/%s", p.Kind, p.Adapter)
	}
	if p.ListenPort < 20000 || p.ListenPort > 20100 || p.SocksPort < 20000 || p.SocksPort > 20100 {
		t.Errorf("ポートがプール外です： listen=%d socks=%d", p.ListenPort, p.SocksPort)
	}
	if p.ListenPort == p.SocksPort {
		t.Error("listen と socks のポートは必ず異なる必要があります")
	}
	if p.PlatformAddr != "10.0.0.5:"+strconv.Itoa(p.ListenPort) {
		t.Errorf("PlatformAddr = %s（callback host + listen port であるべき）", p.PlatformAddr)
	}
	// server のコマンドライン組み立て
	joined := strings.Join(p.ServerArgs, " ")
	for _, want := range []string{"server", "--host 0.0.0.0", "-p " + strconv.Itoa(p.ListenPort), "--reverse", "--authfile"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ServerArgs に %q がありません: %s", want, joined)
		}
	}
	// auth はどのコマンドラインにも現れてはならない（server は --authfile、client は AUTH env）
	if strings.Contains(joined, p.AuthPass) {
		t.Error("auth が ServerArgs に漏えいしています")
	}
	if strings.Contains(strings.Join(p.ClientArgs, " "), p.AuthPass) {
		t.Error("auth が ClientArgs に漏えいしています")
	}
	if len(p.AuthPass) != 32 { // 16 バイト hex
		t.Errorf("AuthPass の長さ %d, want 32（crypto/rand 16 バイトの hex）", len(p.AuthPass))
	}
	// client の remote 形式：リバース socks をプラットフォームのループバックにバインド
	wantRemote := "R:127.0.0.1:" + strconv.Itoa(p.SocksPort) + ":socks"
	found := false
	for _, a := range p.ClientArgs {
		if a == wantRemote {
			found = true
		}
	}
	if !found {
		t.Errorf("ClientArgs に remote %q がありません: %v", wantRemote, p.ClientArgs)
	}
	// authfile は chisel が期待する JSON users 形式 {"user:pass": [".*"]} であること
	var users map[string][]string
	if err := json.Unmarshal([]byte(p.AuthfileContent()), &users); err != nil {
		t.Fatalf("AuthfileContent が正当な JSON ではありません: %v", err)
	}
	key := p.AuthUser + ":" + p.AuthPass
	if got, ok := users[key]; !ok || len(got) != 1 || got[0] != ".*" {
		t.Errorf("AuthfileContent users = %v, want {%q: [\".*\"]}", users, key)
	}
}

func TestPlanPortfwdAssembly(t *testing.T) {
	o := testOpts()
	o.TargetHost, o.TargetPort = "172.16.1.8", 3306
	p, err := PlanPortfwd(o)
	if err != nil {
		t.Fatal(err)
	}
	wantRemote := "R:127.0.0.1:" + strconv.Itoa(p.LocalPort) + ":172.16.1.8:3306"
	found := false
	for _, a := range p.ClientArgs {
		if a == wantRemote {
			found = true
		}
	}
	if !found {
		t.Errorf("ClientArgs に remote %q がありません: %v", wantRemote, p.ClientArgs)
	}
	// target がなければエラーになること
	if _, err := PlanPortfwd(testOpts()); err == nil {
		t.Error("portfwd で target がない場合はエラーになるべきです")
	}
	// callback host がなければエラーになること
	o2 := testOpts()
	o2.CallbackHost = ""
	if _, err := PlanSocks(o2); err == nil {
		t.Error("callback host がない場合はエラーになるべきです")
	}
}

func TestPlanSocksPortPoolExhausted(t *testing.T) {
	o := testOpts()
	o.PortMin, o.PortMax = 20000, 20000 // ポートが 1 つだけだが socks は 2 つ必要
	if _, err := PlanSocks(o); err == nil {
		t.Error("ポートプールが 2 つのポートを割り当てるのに足りない場合はエラーになるべきです")
	}
}

func TestRollbackStepsOrder(t *testing.T) {
	p, err := PlanSocks(testOpts())
	if err != nil {
		t.Fatal(err)
	}
	p.RemotePID, p.ServerPID, p.StageToken = 1234, 5678, "tok"
	steps := p.RollbackSteps()
	wantOrder := []string{"remote_process", "remote_files", "server_process", "stage_entry", "authfile", "ledger_row"}
	if len(steps) != len(wantOrder) {
		t.Fatalf("ロールバックのステップ数 %d, want %d", len(steps), len(wantOrder))
	}
	for i, layer := range wantOrder {
		if steps[i].Layer != layer {
			t.Errorf("ロールバックの %d 番目のステップ = %s, want %s（まず目標側、次にプラットフォーム側、台帳は最後に削除）", i, steps[i].Layer, layer)
		}
	}
}
