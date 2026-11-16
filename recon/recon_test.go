package recon

import (
	"strings"
	"testing"
)

// サンプルはすべて実際のコマンド出力（Linux が中心、Windows は中国語・英語の変種）で、境界を網羅:
// IPv6 なし、複数 NIC、Windows の中国語出力、ARP の不完全な項目、UDP の state 列なし。

const sampleIPAddr = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP group default qlen 1000
    link/ether 08:00:27:aa:bb:cc brd ff:ff:ff:ff:ff:ff
    inet 192.168.1.10/24 brd 192.168.1.255 scope global dynamic eth0
       valid_lft 86300sec preferred_lft 86300sec
    inet6 fe80::a00:27ff:feaa:bbcc/64 scope link
       valid_lft forever preferred_lft forever
3: eth1: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP group default qlen 1000
    link/ether 08:00:27:dd:ee:ff brd ff:ff:ff:ff:ff:ff
    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth1
       valid_lft forever preferred_lft forever
`

const sampleIPOneline = `1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
2: eth0    inet 192.168.1.10/24 brd 192.168.1.255 scope global dynamic eth0\       valid_lft 86300sec preferred_lft 86300sec
3: eth1    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth1\       valid_lft forever preferred_lft forever
`

const sampleIfconfig = `eth0: flags=4163<UP,BROADCAST,RUNNING,MULTICAST>  mtu 1500
        inet 192.168.1.10  netmask 255.255.255.0  broadcast 192.168.1.255
        ether 08:00:27:aa:bb:cc  txqueuelen 1000  (Ethernet)

lo: flags=73<UP,LOOPBACK,RUNNING>  mtu 65536
        inet 127.0.0.1  netmask 255.0.0.0
`

const sampleIfconfigOld = `eth0      Link encap:Ethernet  HWaddr 08:00:27:aa:bb:cc
          inet addr:10.0.0.5  Bcast:10.0.0.255  Mask:255.255.255.0
          UP BROADCAST RUNNING MULTICAST  MTU:1500  Metric:1

lo        Link encap:Local Loopback
          inet addr:127.0.0.1  Mask:255.0.0.0
`

func TestParseIPAddr(t *testing.T) {
	nics := ParseIPAddr(sampleIPAddr)
	if len(nics) != 3 {
		t.Fatalf("NIC は 3 件(lo を含む)を期待, got %d: %+v", len(nics), nics)
	}
	want := map[string]NIC{"eth0": {Iface: "eth0", IP: "192.168.1.10", Prefix: 24}, "eth1": {Iface: "eth1", IP: "10.0.0.5", Prefix: 24}}
	for _, n := range nics {
		if w, ok := want[n.Iface]; ok && (n.IP != w.IP || n.Prefix != w.Prefix) {
			t.Errorf("%s = %+v, want %+v", n.Iface, n, w)
		}
		if n.Iface == "lo" && (n.IP != "127.0.0.1" || n.Prefix != 8) {
			t.Errorf("lo = %+v", n)
		}
	}
	// IPv6 のない出力でも正常に動くこと（サンプル内の inet6 行を誤解析しない）。
	for _, n := range nics {
		if strings.Contains(n.IP, ":") {
			t.Errorf("IPv6 アドレスが混入しています: %+v", n)
		}
	}
}

func TestParseIPAddrOneline(t *testing.T) {
	// ip -o -4 addr show の単一行レイアウト（継続行のバックスラッシュ付き、webshell のエコーそのまま）。
	nics := ParseIPAddr(sampleIPOneline)
	if len(nics) != 3 || nics[1].Iface != "eth0" || nics[1].IP != "192.168.1.10" || nics[1].Prefix != 24 {
		t.Fatalf("ip -o の解析に失敗しました: %+v", nics)
	}
	if nics[2].Iface != "eth1" || nics[2].IP != "10.0.0.5" {
		t.Fatalf("eth1 の解析に失敗しました: %+v", nics[2])
	}
}

func TestParseIfconfig(t *testing.T) {
	nics := ParseIfconfig(sampleIfconfig)
	if len(nics) != 2 || nics[0].Iface != "eth0" || nics[0].IP != "192.168.1.10" || nics[0].Prefix != 24 {
		t.Fatalf("新形式 ifconfig の解析に失敗しました: %+v", nics)
	}
	old := ParseIfconfig(sampleIfconfigOld)
	if len(old) != 2 || old[0].Iface != "eth0" || old[0].IP != "10.0.0.5" || old[0].Prefix != 24 {
		t.Fatalf("旧形式 ifconfig(Link encap)の解析に失敗しました: %+v", old)
	}
}

const sampleIPConfigAllCN = `
Windows IP 配置

   主机名  . . . . . . . . . . . . . : WEB01

以太网适配器 以太网:

   连接特定的 DNS 后缀 . . . . . . . :
   IPv4 地址 . . . . . . . . . . . : 192.168.1.10(首选)
   子网掩码  . . . . . . . . . . . : 255.255.255.0
   默认网关. . . . . . . . . . . . : 192.168.1.1

Ethernet adapter Ethernet1:

   Connection-specific DNS Suffix  . :
   IPv4 Address. . . . . . . . . . . : 10.0.0.5
   Subnet Mask . . . . . . . . . . . : 255.255.255.0
   Default Gateway . . . . . . . . . : 10.0.0.1
`

func TestParseIPConfigAll(t *testing.T) {
	nics := ParseIPConfigAll(sampleIPConfigAllCN)
	if len(nics) != 2 {
		t.Fatalf("NIC は 2 件を期待, got %d: %+v", len(nics), nics)
	}
	if nics[0].IP != "192.168.1.10" || nics[0].Prefix != 24 || !strings.Contains(nics[0].Iface, "以太网") {
		t.Errorf("中国語アダプタの解析に失敗しました: %+v", nics[0])
	}
	if nics[1].IP != "10.0.0.5" || nics[1].Prefix != 24 {
		t.Errorf("英語アダプタの解析に失敗しました: %+v", nics[1])
	}
}

const sampleIPRoute = `default via 192.168.1.1 dev eth0 proto dhcp metric 100
10.0.0.0/8 via 10.0.0.1 dev eth1
10.0.0.0/24 dev eth1 proto kernel scope link src 10.0.0.5
192.168.1.0/24 dev eth0 proto kernel scope link src 192.168.1.10 metric 100
172.16.0.0/16 via 192.168.1.254 dev eth0
`

func TestParseIPRoute(t *testing.T) {
	routes := ParseIPRoute(sampleIPRoute)
	if len(routes) != 5 {
		t.Fatalf("ルートは 5 件を期待, got %d: %+v", len(routes), routes)
	}
	static := map[string]Route{}
	for _, r := range routes {
		if r.IsStatic {
			static[r.Dest] = r
		}
	}
	if len(static) != 2 || static["10.0.0.0/8"].Via != "10.0.0.1" || static["172.16.0.0/16"].Via != "192.168.1.254" {
		t.Fatalf("静的ルートのマーキングが誤っています: %+v", static)
	}
	for _, r := range routes {
		if r.Dest == "default" && r.IsStatic {
			t.Error("デフォルトルートを静的とマークしてはなりません")
		}
		if strings.Contains(r.Dest, "/24") && r.IsStatic {
			t.Errorf("直結ルートを静的とマークしてはなりません: %+v", r)
		}
	}
}

const sampleRouteN = `Kernel IP routing table
Destination     Gateway         Genmask         Flags Metric Ref    Use Iface
0.0.0.0         192.168.1.1     0.0.0.0         UG    100    0        0 eth0
10.0.0.0        10.0.0.1        255.0.0.0       UG    0      0        0 eth1
10.0.0.0        0.0.0.0         255.255.255.0   U     0      0        0 eth1
192.168.1.0     0.0.0.0         255.255.255.0   U     100    0        0 eth0
`

func TestParseRouteN(t *testing.T) {
	routes := ParseRouteN(sampleRouteN)
	if len(routes) != 4 {
		t.Fatalf("ルートは 4 件を期待, got %d: %+v", len(routes), routes)
	}
	var def, static *Route
	for i := range routes {
		r := &routes[i]
		if r.Dest == "default" {
			def = r
		}
		if r.IsStatic {
			static = r
		}
	}
	if def == nil || def.Via != "192.168.1.1" || def.Iface != "eth0" {
		t.Fatalf("デフォルトルートの解析に失敗しました: %+v", def)
	}
	if static == nil || static.Dest != "10.0.0.0/8" || static.Via != "10.0.0.1" {
		t.Fatalf("静的ルートのマーキングが誤っています: %+v", static)
	}
}

const sampleRoutePrintCN = `
===========================================================================
IPv4 路由表
===========================================================================
活动路由:
网络目标        网络掩码          网关              接口        跃点数
          0.0.0.0          0.0.0.0      192.168.1.1     192.168.1.10     25
       10.0.0.0        255.0.0.0         10.0.0.1         10.0.0.5     26
    192.168.1.0    255.255.255.0         在链路上      192.168.1.10    281
   192.168.1.10  255.255.255.255         在链路上      192.168.1.10    281
===========================================================================
持久路由:
  无
`

func TestParseRoutePrint(t *testing.T) {
	routes := ParseRoutePrint(sampleRoutePrintCN)
	if len(routes) != 4 {
		t.Fatalf("ルートは 4 件を期待, got %d: %+v", len(routes), routes)
	}
	var static *Route
	for i := range routes {
		if routes[i].IsStatic {
			static = &routes[i]
		}
	}
	if static == nil || static.Dest != "10.0.0.0/8" || static.Via != "10.0.0.1" {
		t.Fatalf("中国語 route print の静的ルート解析に失敗しました: %+v", static)
	}
	for _, r := range routes {
		if r.Dest == "192.168.1.0/24" && (r.IsStatic || r.Via != "") {
			t.Errorf("「在链路上」ルートはゲートウェイを持たず、静的とマークしてもいけません: %+v", r)
		}
	}
}

func TestParseNeighbors(t *testing.T) {
	// ip neigh:FAILED の項目はスキップ。
	neigh := ParseNeighbors(`192.168.1.1 dev eth0 lladdr 08:00:27:11:22:33 REACHABLE
10.0.0.9 dev eth1 FAILED
10.0.0.7 dev eth1 lladdr 08:00:27:44:55:66 STALE
`)
	if len(neigh) != 2 || neigh[0].IP != "192.168.1.1" || neigh[1].IP != "10.0.0.7" || neigh[1].Iface != "eth1" {
		t.Fatalf("ip neigh の解析に失敗しました: %+v", neigh)
	}
	// Linux arp -a:<incomplete> はスキップ。
	neigh = ParseNeighbors(`? (192.168.1.1) at 08:00:27:11:22:33 [ether] on eth0
? (10.0.0.9) at <incomplete> on eth1
`)
	if len(neigh) != 1 || neigh[0].IP != "192.168.1.1" || neigh[0].Iface != "eth0" {
		t.Fatalf("Linux arp -a の解析に失敗しました: %+v", neigh)
	}
	// Windows arp -a（中国語）:ブロードキャスト／マルチキャストはスキップ。
	neigh = ParseNeighbors(`
接口: 192.168.1.10 --- 0x6
  Internet 地址         物理地址              类型
  192.168.1.1           08-00-27-11-22-33     动态
  192.168.1.255         ff-ff-ff-ff-ff-ff     静态
  224.0.0.22            01-00-5e-00-00-16     静态
  255.255.255.255       ff-ff-ff-ff-ff-ff     静态
`)
	if len(neigh) != 1 || neigh[0].IP != "192.168.1.1" || neigh[0].MAC != "08-00-27-11-22-33" {
		t.Fatalf("Windows arp -a の解析に失敗しました: %+v", neigh)
	}
}

func TestParseHosts(t *testing.T) {
	entries := ParseHosts(`127.0.0.1	localhost
::1	localhost ip6-localhost
# コメント行
10.0.0.5	web01 web01.corp.local   # 行内コメント
192.168.1.1	gw
`)
	if len(entries) != 3 {
		t.Fatalf("hosts は 3 件を期待, got %d: %+v", len(entries), entries)
	}
	if entries[1].IP != "10.0.0.5" || len(entries[1].Names) != 2 || entries[1].Names[1] != "web01.corp.local" {
		t.Fatalf("hosts 行の解析に失敗しました: %+v", entries[1])
	}
}

const sampleNetstatLinux = `Active Internet connections (servers and established)
Proto Recv-Q Send-Q Local Address           Foreign Address         State
tcp        0      0 0.0.0.0:22              0.0.0.0:*               LISTEN
tcp        0      0 127.0.0.1:3306          0.0.0.0:*               LISTEN
tcp        0      0 192.168.1.10:45678      10.8.0.5:445            ESTABLISHED
tcp        0      0 192.168.1.10:80         192.168.1.20:51234      TIME_WAIT
tcp6       0      0 :::80                   :::*                    LISTEN
udp        0      0 0.0.0.0:68              0.0.0.0:*
`

func TestParseNetstatLinux(t *testing.T) {
	listening, peers := ParseConnTable(sampleNetstatLinux)
	ports := map[int]string{}
	for _, l := range listening {
		ports[l.Port] = l.Proto
	}
	if len(listening) != 4 || ports[22] != "tcp" || ports[3306] != "tcp" || ports[80] != "tcp" || ports[68] != "udp" {
		t.Fatalf("netstat の待ち受けポート解析に失敗しました: %+v", listening)
	}
	if len(peers) != 1 || peers[0].IP != "10.8.0.5" || peers[0].Port != 445 {
		t.Fatalf("netstat の対向先解析に失敗しました(TIME_WAIT は数えない): %+v", peers)
	}
}

const sampleSS = `Netid State  Recv-Q Send-Q Local Address:Port  Peer Address:Port
udp   UNCONN 0      0      0.0.0.0:68         0.0.0.0:*
tcp   LISTEN 0      128    0.0.0.0:22         0.0.0.0:*
tcp   LISTEN 0      100    127.0.0.1:3306     0.0.0.0:*
tcp   ESTAB  0      0      192.168.1.10:45678 10.8.0.5:445
`

func TestParseSS(t *testing.T) {
	listening, peers := ParseConnTable(sampleSS)
	if len(listening) != 3 || len(peers) != 1 || peers[0].IP != "10.8.0.5" || peers[0].Port != 445 {
		t.Fatalf("ss の解析に失敗しました: listening=%+v peers=%+v", listening, peers)
	}
}

const sampleNetstatWinCN = `
活动连接

  协议  本地地址          外部地址        状态           PID
  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1234
  TCP    0.0.0.0:445            0.0.0.0:0              LISTENING       4
  TCP    192.168.1.10:49678     10.8.0.5:445           ESTABLISHED     2345
  UDP    0.0.0.0:500            *:*                                    5678
`

func TestParseNetstatWindows(t *testing.T) {
	listening, peers := ParseConnTable(sampleNetstatWinCN)
	if len(listening) != 3 || len(peers) != 1 || peers[0].IP != "10.8.0.5" || peers[0].Port != 445 {
		t.Fatalf("Windows netstat -ano の解析に失敗しました: listening=%+v peers=%+v", listening, peers)
	}
}

func TestInfer(t *testing.T) {
	report := &ReconReport{
		NICs: []NIC{
			{Iface: "lo", IP: "127.0.0.1", Prefix: 8},
			{Iface: "eth0", IP: "192.168.1.10", Prefix: 24},
			{Iface: "eth1", IP: "10.0.0.5", Prefix: 24},
		},
		Routes: []Route{
			{Dest: "default", Via: "192.168.1.1", Iface: "eth0"},
			{Dest: "172.16.0.0/16", Via: "10.0.0.1", Iface: "eth1", IsStatic: true},
		},
		Neighbors: []Neighbor{{IP: "192.168.1.1", MAC: "08:00:27:11:22:33"}},
		Peers: []Peer{
			{IP: "10.8.0.5", Port: 445}, // 自ホストのセグメント外 = 疎通が証明済み
			{IP: "10.0.0.9", Port: 22},  // 自ホストのセグメント内なので数えない
		},
	}
	inf := Infer(report)
	kinds := map[string]Inference{}
	for _, i := range inf {
		kinds[i.Kind] = i
	}
	if _, ok := kinds["multi_nic"]; !ok {
		t.Error("マルチ NIC の踏み台推論が欠落しています")
	}
	if sr, ok := kinds["static_route"]; !ok || !strings.Contains(sr.Summary, "172.16.0.0/16") {
		t.Errorf("静的ルートの推論が誤っています: %+v", sr)
	}
	pp, ok := kinds["proven_peer"]
	if !ok || !strings.Contains(pp.Detail, "10.8.0.5:445") || strings.Contains(pp.Detail, "10.0.0.9") {
		t.Errorf("疎通が証明済みの対向先の推論が誤っています(自セグメントの対向先は数えない): %+v", pp)
	}
	if _, ok := kinds["arp_neighbors"]; !ok {
		t.Error("ARP ネイバーの推論が欠落しています")
	}

	// 単一 NIC で静的ルートも対向先もない:ネイバーの推論のみ。
	minimal := Infer(&ReconReport{
		NICs:      []NIC{{Iface: "eth0", IP: "192.168.1.10", Prefix: 24}},
		Neighbors: []Neighbor{{IP: "192.168.1.2", MAC: "08:00:27:99:88:77"}},
	})
	if len(minimal) != 1 || minimal[0].Kind != "arp_neighbors" {
		t.Fatalf("単一 NIC のシナリオの推論が誤っています: %+v", minimal)
	}
}

func TestAutoDetect(t *testing.T) {
	// Auto 系：ip 系の出力を優先し、空出力ならフォールバック。形式同士が混ざらないこと。
	if n := ParseNICsAuto(sampleIfconfig); len(n) != 2 || n[0].IP != "192.168.1.10" {
		t.Fatalf("ParseNICsAuto の ifconfig フォールバックに失敗しました: %+v", n)
	}
	if r := ParseRoutesAuto(sampleRouteN); len(r) != 4 {
		t.Fatalf("ParseRoutesAuto の route -n フォールバックに失敗しました: %+v", r)
	}
	if r := ParseRoutesAuto(sampleRoutePrintCN); len(r) != 4 {
		t.Fatalf("ParseRoutesAuto の route print フォールバックに失敗しました: %+v", r)
	}
}
