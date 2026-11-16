// Package recon はイントラネット受動偵察の出力を解析するレイヤー（イントラネットペネトレーションの
// フェーズ 2、docs/architecture/intranet-pivot.md §5 フェーズ 2。意味は PivotHub の pivothub/service/recon.py
// から移植し、Go で書き直したもので丸写しではない）。
// パーサーと推論ルールはすべて純粋関数で、DB にもネットワークにも触れず、実際のコマンド出力の
// サンプルでそのまま単体テストする。
//
// 対象コマンド（Linux が中心、Windows は各 1 変種）:
//   - ip addr / ifconfig / ipconfig /all      → NIC + IP + マスク
//   - ip route / route -n / route print       → ルート（非デフォルトの静的ルートを明示）
//   - arp -a / ip neigh                       → ネイバー
//   - cat /etc/hosts（Windows の hosts を含む） → 静的なホストマッピング
//   - netstat -an / ss -an / netstat -ano     → リスニングポート + 確立済み接続の対向先
//
// 価値の高い推論は chains の pivot 骨格「受動六表」の判定基準に従う（agent/chainskel/pivot.md）:
// 非デフォルトの静的ルートは疎通確実な価値あるセグメント、established の対向先は疎通が証明済みの
// ホスト、複数 NIC = 潜在的な踏み台。
package recon

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// NIC は 1 枚の NIC の IPv4 アドレス 1 件。
type NIC struct {
	Iface  string `json:"iface"`
	IP     string `json:"ip"`
	Prefix int    `json:"prefix"` // CIDR プレフィックス長（例: 24）
}

// Route は 1 本のルート。IsStatic は非デフォルトの静的ルートを示す（価値が高い: 静的ルートの
// 指すセグメントは管理者が明示的に設定したもので、疎通が確実で通常は業務上の価値がある）。
type Route struct {
	Dest     string `json:"dest"` // CIDR（"default" はデフォルトルート）
	Via      string `json:"via,omitempty"`
	Iface    string `json:"iface,omitempty"`
	IsStatic bool   `json:"is_static"`
}

// Neighbor は ARP／ネイバーテーブルの 1 項目（同一セグメントの稼働ホストの受動的なエビデンス）。
type Neighbor struct {
	IP    string `json:"ip"`
	MAC   string `json:"mac,omitempty"`
	Iface string `json:"iface,omitempty"`
}

// HostEntry は /etc/hosts の 1 行。
type HostEntry struct {
	IP    string   `json:"ip"`
	Names []string `json:"names"`
}

// Listener は 1 つのリスニングポート。
type Listener struct {
	Port  int    `json:"port"`
	Proto string `json:"proto"` // tcp | udp
}

// Peer は確立済み接続の対向先 1 件。
type Peer struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// ReconReport は受動偵察 1 回分の構造化された結果。
type ReconReport struct {
	NICs         []NIC       `json:"nics"`
	Routes       []Route     `json:"routes"`
	Neighbors    []Neighbor  `json:"neighbors"`
	HostsEntries []HostEntry `json:"hosts_entries"`
	Listening    []Listener  `json:"listening"`
	Peers        []Peer      `json:"peers"`
}

// Inference は価値の高い推論 1 件（pivot 骨格の判定基準に従う）。
type Inference struct {
	Kind    string `json:"kind"` // multi_nic | static_route | proven_peer | arp_neighbors
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
}

var ipRe = regexp.MustCompile(`^\d{1,3}(?:\.\d{1,3}){3}$`)

func isIPv4(s string) bool { return ipRe.MatchString(s) && net.ParseIP(s) != nil }

// SegmentOf は ip/prefix のセグメント CIDR（例 "10.0.0.0/24"）を計算する。不正な入力は "" を返す。
func SegmentOf(ip string, prefix int) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return ""
	}
	if prefix < 0 || prefix > 32 {
		return ""
	}
	return netip.PrefixFrom(addr, prefix).Masked().String()
}

func isLoopbackOrLinkLocal(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return true
	}
	return addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsMulticast()
}

func maskToPrefix(mask string) int {
	ip := net.ParseIP(mask)
	if ip == nil {
		return 24
	}
	ones, _ := net.IPMask(ip.To4()).Size()
	if ones == 0 {
		return 24
	}
	return ones
}

// ---------------------------------------------------------------------------
// NIC:ip addr（Linux の新旧 2 レイアウト）/ ifconfig / ipconfig /all
// ---------------------------------------------------------------------------

// ParseIPAddr は `ip addr` / `ip -o -4 addr show` の出力を解析する。
func ParseIPAddr(text string) []NIC {
	out := []NIC{}
	iface := ""
	for _, line := range strings.Split(text, "\n") {
		// "2: eth0: <BROADCAST,...>" / "5: eth0@if2: <...>" / ip -o の "2: eth0 inet ..."
		if m := regexp.MustCompile(`^\d+:\s+([^:@\s]+)[:@ ]`).FindStringSubmatch(line); m != nil {
			iface = m[1]
		}
		// inet 10.0.0.5/24（brd/scope の接尾辞が付く場合あり。ip -o と複数行レイアウトで共通）
		if m := regexp.MustCompile(`\binet\s+(\d{1,3}(?:\.\d{1,3}){3})/(\d{1,2})\b`).FindStringSubmatch(line); m != nil {
			prefix, _ := strconv.Atoi(m[2])
			out = append(out, NIC{Iface: iface, IP: m[1], Prefix: prefix})
		}
	}
	return dedupNICs(out)
}

// ParseIfconfig は `ifconfig` / `ifconfig -a` の出力を解析する（新方式の flags レイアウトと
// 旧方式の Link encap レイアウトの両方に対応）。
func ParseIfconfig(text string) []NIC {
	out := []NIC{}
	iface := ""
	for _, line := range strings.Split(text, "\n") {
		if m := regexp.MustCompile(`^(\S+?):\s+flags=`).FindStringSubmatch(line); m != nil {
			iface = m[1]
			continue
		}
		if m := regexp.MustCompile(`^(\S+)\s+Link encap`).FindStringSubmatch(line); m != nil {
			iface = m[1]
			continue
		}
		// 新方式: "inet 192.168.1.10  netmask 255.255.255.0  broadcast ..."
		// 旧方式: "inet addr:10.0.0.5  Bcast:10.0.0.255  Mask:255.255.255.0"
		m := regexp.MustCompile(`inet\s+(?:addr:)?(\d{1,3}(?:\.\d{1,3}){3})(?:\s+(?:Bcast|broadcast)[:=]?\s*\d{1,3}(?:\.\d{1,3}){3})?\s+(?:netmask\s*[:=]?\s*|Mask:\s*)(\d{1,3}(?:\.\d{1,3}){3})`).FindStringSubmatch(line)
		if m != nil && iface != "" {
			out = append(out, NIC{Iface: iface, IP: m[1], Prefix: maskToPrefix(m[2])})
		}
	}
	return dedupNICs(out)
}

// ParseIPConfigAll は Windows の `ipconfig /all` を解析する（中国語・英語どちらの出力にも対応:
// IPv4 アドレスとサブネットマスクの各表記。アドレスの後ろに優先の接尾辞が付く場合がある）。
func ParseIPConfigAll(text string) []NIC {
	out := []NIC{}
	iface := ""
	pendingIP := ""
	for _, line := range strings.Split(text, "\n") {
		if m := regexp.MustCompile(`(?:adapter|适配器)\s+(.+?):\s*$`).FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			iface = strings.TrimSpace(m[1])
			pendingIP = ""
			continue
		}
		if m := regexp.MustCompile(`IPv4[^:：]*[:：]\s*(\d{1,3}(?:\.\d{1,3}){3})`).FindStringSubmatch(line); m != nil {
			pendingIP = m[1]
			continue
		}
		if m := regexp.MustCompile(`(?:Subnet Mask|子网掩码)[^:：]*[:：]\s*(\d{1,3}(?:\.\d{1,3}){3})`).FindStringSubmatch(line); m != nil {
			if pendingIP != "" {
				if iface == "" {
					iface = "iface"
				}
				out = append(out, NIC{Iface: iface, IP: pendingIP, Prefix: maskToPrefix(m[1])})
				pendingIP = ""
			}
		}
	}
	return dedupNICs(out)
}

// ParseNICsAuto はまず ip addr として解析し、空なら ifconfig、さらに空なら ipconfig /all で解析する。
func ParseNICsAuto(text string) []NIC {
	if n := ParseIPAddr(text); len(n) > 0 {
		return n
	}
	if n := ParseIfconfig(text); len(n) > 0 {
		return n
	}
	return ParseIPConfigAll(text)
}

func dedupNICs(in []NIC) []NIC {
	seen := map[string]bool{}
	out := []NIC{}
	for _, n := range in {
		key := n.Iface + "|" + n.IP
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}

// ---------------------------------------------------------------------------
// ルート：ip route / route -n / route print
// ---------------------------------------------------------------------------

// ParseIPRoute は Linux の `ip route` を解析する。非デフォルトでゲートウェイ（via）を経由し、
// 直結（scope link）でないルートを静的ルートとして示す。
func ParseIPRoute(text string) []Route {
	out := []Route{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		dest := fields[0]
		isDefault := dest == "default"
		if !isDefault && !strings.Contains(dest, "/") {
			continue // ルート行ではない
		}
		r := Route{Dest: dest}
		if m := regexp.MustCompile(`\bvia\s+(\S+)`).FindStringSubmatch(line); m != nil {
			r.Via = m[1]
		}
		if m := regexp.MustCompile(`\bdev\s+(\S+)`).FindStringSubmatch(line); m != nil {
			r.Iface = m[1]
		}
		connected := strings.Contains(line, "scope link") || strings.Contains(line, "proto kernel")
		r.IsStatic = !isDefault && r.Via != "" && !connected
		out = append(out, r)
	}
	return dedupRoutes(out)
}

// ParseRouteN は Linux の `route -n`（Destination Gateway Genmask Flags ... Iface）を解析する。
func ParseRouteN(text string) []Route {
	out := []Route{}
	for _, raw := range strings.Split(text, "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 8 || !isIPv4(fields[0]) || !isIPv4(fields[1]) {
			continue
		}
		dest, gateway, mask, flags := fields[0], fields[1], fields[2], fields[3]
		r := Route{
			Dest:  SegmentOf(dest, maskToPrefix(mask)),
			Iface: fields[len(fields)-1],
		}
		isDefault := dest == "0.0.0.0"
		if isDefault {
			r.Dest = "default"
		}
		hasGW := strings.Contains(flags, "G")
		if hasGW {
			r.Via = gateway
		}
		r.IsStatic = !isDefault && hasGW
		out = append(out, r)
	}
	return dedupRoutes(out)
}

// ParseRoutePrint は Windows の `route print` の IPv4 有効ルート（Network Destination
// Netmask Gateway Interface Metric。中国語の見出しでも同じレイアウト）を解析する。
// On-link（リンク上）のゲートウェイは静的ルートに数えない。
func ParseRoutePrint(text string) []Route {
	out := []Route{}
	for _, raw := range strings.Split(text, "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 5 || !isIPv4(fields[0]) || !isIPv4(fields[1]) {
			continue
		}
		dest, mask, gateway, iface := fields[0], fields[1], fields[2], fields[3]
		r := Route{Iface: iface}
		if dest == "0.0.0.0" && mask == "0.0.0.0" {
			r.Dest = "default"
		} else {
			r.Dest = SegmentOf(dest, maskToPrefix(mask))
		}
		onLink := !isIPv4(gateway) // "On-link" / リンク上（中国語出力）
		if !onLink {
			r.Via = gateway
		}
		r.IsStatic = r.Dest != "default" && !onLink
		out = append(out, r)
	}
	return dedupRoutes(out)
}

// ParseRoutesAuto は ip route / route -n / route print の順に解析する。
func ParseRoutesAuto(text string) []Route {
	if r := ParseIPRoute(text); len(r) > 0 {
		return r
	}
	if r := ParseRouteN(text); len(r) > 0 {
		return r
	}
	return ParseRoutePrint(text)
}

func dedupRoutes(in []Route) []Route {
	seen := map[string]bool{}
	out := []Route{}
	for _, r := range in {
		key := r.Dest + "|" + r.Via
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// ネイバー：ip neigh / arp -a（Linux）/ arp -a（Windows）
// ---------------------------------------------------------------------------

// ParseNeighbors は `ip neigh`、`arp -a`（Linux と Windows の 2 形式）を解析する。
// FAILED／INCOMPLETE の項目（MAC なし）は稼働のエビデンスにならないためスキップする。
func ParseNeighbors(text string) []Neighbor {
	out := []Neighbor{}
	seen := map[string]bool{}
	add := func(ip, mac, iface string) {
		if !isIPv4(ip) || isLoopbackOrLinkLocal(ip) || strings.HasPrefix(ip, "224.") || strings.HasPrefix(ip, "239.") {
			return
		}
		mac = strings.ToLower(strings.TrimSpace(mac))
		if mac == "" || mac == "<incomplete>" || mac == "ff-ff-ff-ff-ff-ff" || ip == "255.255.255.255" {
			return
		}
		if seen[ip] {
			return
		}
		seen[ip] = true
		out = append(out, Neighbor{IP: ip, MAC: mac, Iface: iface})
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		// ip neigh: "10.0.0.1 dev eth0 lladdr 00:11:22:33:44:55 REACHABLE"
		if m := regexp.MustCompile(`^(\d{1,3}(?:\.\d{1,3}){3})\s+dev\s+(\S+)\s+lladdr\s+(\S+)`).FindStringSubmatch(line); m != nil {
			add(m[1], m[3], m[2])
			continue
		}
		// Linux arp -a: "? (10.0.0.1) at 00:11:22:33:44:55 [ether] on eth0"
		if m := regexp.MustCompile(`\((\d{1,3}(?:\.\d{1,3}){3})\)\s+at\s+(\S+)`).FindStringSubmatch(line); m != nil {
			iface := ""
			if im := regexp.MustCompile(`\bon\s+(\S+)`).FindStringSubmatch(line); im != nil {
				iface = im[1]
			}
			add(m[1], m[2], iface)
			continue
		}
		// Windows arp -a: "  10.0.0.1           00-11-22-33-44-55     動的/dynamic"
		if m := regexp.MustCompile(`^(\d{1,3}(?:\.\d{1,3}){3})\s+([0-9a-fA-F]{2}(?:-[0-9a-fA-F]{2}){5})\s+\S+`).FindStringSubmatch(line); m != nil {
			add(m[1], m[2], "")
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// /etc/hosts（Windows の hosts を含む）
// ---------------------------------------------------------------------------

// ParseHosts は hosts ファイルを解析する（コメント行と空行は無視）。
func ParseHosts(text string) []HostEntry {
	out := []HostEntry{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if !isIPv4(fields[0]) || len(fields) < 2 {
			continue
		}
		out = append(out, HostEntry{IP: fields[0], Names: fields[1:]})
	}
	return out
}

// ---------------------------------------------------------------------------
// 接続テーブル：netstat -an / ss -an / netstat -ano
// ---------------------------------------------------------------------------

var knownStates = map[string]bool{
	"listen": true, "listening": true, "unconn": true,
	"established": true, "estab": true, "time_wait": true, "close_wait": true,
	"syn_sent": true, "syn_recv": true, "fin_wait1": true, "fin_wait2": true,
	"closing": true, "last_ack": true,
}

// splitHostPort は "10.0.0.5:22" / ":::8080" / "[::1]:3306" / "*:*" を host/port に分割する。
// port が数字でない（"*"）場合は ok=false。
func splitHostPort(s string) (host string, port int, ok bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, false
	}
	host = strings.Trim(s[:i], "[]")
	p := s[i+1:]
	if p == "*" || p == "" {
		return host, 0, false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return "", 0, false
	}
	return host, n, true
}

// ParseConnTable は netstat／ss の接続テーブルを解析し、リスニングポートと確立済み接続の
// 対向先を返す。3 種類のレイアウトを同じ解析経路で扱う: proto は先頭列。state とアドレス対の
// 位置はツールによって異なるため、「既知の state 語彙 + addr:port の形」で識別し、固定の列位置に
// 依存しない。
func ParseConnTable(text string) (listening []Listener, peers []Peer) {
	seenL := map[string]bool{}
	seenP := map[string]bool{}
	for _, raw := range strings.Split(text, "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 3 {
			continue
		}
		proto := strings.ToLower(fields[0])
		base := strings.TrimSuffix(proto, "6")
		if base != "tcp" && base != "udp" {
			continue
		}
		// state とアドレス対：netstat レイアウトでは state がアドレスの後、ss では proto の後。
		state := ""
		addrs := []string{}
		for _, f := range fields[1:] {
			lf := strings.ToLower(f)
			if knownStates[lf] && state == "" {
				state = lf
				continue
			}
			if strings.Contains(f, ":") {
				addrs = append(addrs, f)
			}
		}
		if len(addrs) < 2 {
			continue
		}
		local, foreign := addrs[0], addrs[1]
		_, localPort, localOK := splitHostPort(local)
		foreignHost, foreignPort, foreignOK := splitHostPort(foreign)

		isListen := state == "listen" || state == "listening" || state == "unconn" ||
			(state == "" && base == "udp") // udp の netstat は state 列がないことが多い
		if isListen && localOK {
			key := base + "|" + strconv.Itoa(localPort)
			if !seenL[key] {
				seenL[key] = true
				listening = append(listening, Listener{Port: localPort, Proto: base})
			}
			continue
		}
		if (state == "established" || state == "estab") && foreignOK && isIPv4(foreignHost) &&
			!isLoopbackOrLinkLocal(foreignHost) {
			key := foreignHost + "|" + strconv.Itoa(foreignPort)
			if !seenP[key] {
				seenP[key] = true
				peers = append(peers, Peer{IP: foreignHost, Port: foreignPort})
			}
		}
	}
	sort.Slice(listening, func(i, j int) bool {
		if listening[i].Port != listening[j].Port {
			return listening[i].Port < listening[j].Port
		}
		return listening[i].Proto < listening[j].Proto
	})
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].IP != peers[j].IP {
			return peers[i].IP < peers[j].IP
		}
		return peers[i].Port < peers[j].Port
	})
	return listening, peers
}

// ---------------------------------------------------------------------------
// 価値の高い推論（pivot 骨格「受動六表」の判定基準に従う）
// ---------------------------------------------------------------------------

// Infer は構造化レポートから価値の高い情報を導出する:
//   - 複数 NIC でセグメントをまたぐ = 潜在的な踏み台（multi_nic）
//   - 非デフォルトの静的ルートが指すセグメント = 疎通確実な価値あるセグメント（static_route）
//   - established の対向先で自ホストのセグメント外にある IP = 疎通が証明済みの稼働ホスト（proven_peer）
//   - ARP ネイバー = 同一セグメントで受動的に得られる稼働ホスト（arp_neighbors）
func Infer(r *ReconReport) []Inference {
	out := []Inference{}

	// 自ホストのセグメント集合（ループバック／リンクローカルを除外）。
	segments := map[string][]NIC{}
	for _, n := range r.NICs {
		if isLoopbackOrLinkLocal(n.IP) {
			continue
		}
		seg := SegmentOf(n.IP, n.Prefix)
		if seg == "" {
			continue
		}
		segments[seg] = append(segments[seg], n)
	}

	if len(segments) >= 2 {
		parts := []string{}
		for seg, nics := range segments {
			for _, n := range nics {
				parts = append(parts, fmt.Sprintf("%s %s/%d（セグメント %s）", n.Iface, n.IP, n.Prefix, seg))
			}
		}
		sort.Strings(parts)
		out = append(out, Inference{
			Kind:    "multi_nic",
			Summary: fmt.Sprintf("ホストは非ループバックのセグメントを %d 個持ち（NIC %d 枚）、潜在的な踏み台です", len(segments), len(parts)),
			Detail:  strings.Join(parts, ";"),
		})
	}

	for _, rt := range r.Routes {
		if !rt.IsStatic {
			continue
		}
		out = append(out, Inference{
			Kind:    "static_route",
			Summary: fmt.Sprintf("非デフォルトの静的ルート %s via %s（%s）— 管理者が明示的に設定したもので疎通は確実、価値の高い偵察対象です", rt.Dest, rt.Via, rt.Iface),
		})
	}

	// 確立済み接続の対向先: 自ホストのどのセグメントにもない = 疎通が証明済みの外部セグメントの稼働ホスト。
	inLocalSegment := func(ip string) bool {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			return false
		}
		for seg := range segments {
			p, err := netip.ParsePrefix(seg)
			if err == nil && p.Contains(addr) {
				return true
			}
		}
		return false
	}
	proven := []string{}
	for _, p := range r.Peers {
		if !inLocalSegment(p.IP) {
			proven = append(proven, fmt.Sprintf("%s:%d", p.IP, p.Port))
		}
	}
	if len(proven) > 0 {
		out = append(out, Inference{
			Kind:    "proven_peer",
			Summary: fmt.Sprintf("確立済み接続の対向先のうち %d 件が自ホストのセグメント外にあります — 疎通が証明済みの稼働ホスト（横展開の成功率が高い対象）", len(proven)),
			Detail:  strings.Join(proven, ";"),
		})
	}

	if len(r.Neighbors) > 0 {
		ips := []string{}
		for _, n := range r.Neighbors {
			ips = append(ips, n.IP)
		}
		detail := strings.Join(ips, ";")
		if len(ips) > 20 {
			detail = strings.Join(ips[:20], ";") + fmt.Sprintf("…（全 %d 件）", len(ips))
		}
		out = append(out, Inference{
			Kind:    "arp_neighbors",
			Summary: fmt.Sprintf("ARP ネイバーテーブルに同一セグメントの稼働ホストが %d 件（受動的に取得でき、パケット送信は不要）", len(r.Neighbors)),
			Detail:  detail,
		})
	}
	return out
}
