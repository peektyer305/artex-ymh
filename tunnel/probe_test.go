// probe_test.go はプローブ解析とエビデンス格付けの純関数単体テスト。
package tunnel

import (
	"strings"
	"testing"
)

func TestParseProbeOutput(t *testing.T) {
	cases := []struct {
		name      string
		tag, out  string
		wantOK    bool
		wantFound bool
	}{
		{"ok マーカー", "TCP_1", "ARTEX_TCP_1_OK\n", true, true},
		{"fail マーカー", "TCP_1", "ARTEX_TCP_1_FAIL\n", false, true},
		{"マーカーがエコーに混在", "DNS", ";; reply\nARTEX_DNS_OK\n;; done", true, true},
		{"マーカーなし（チャネル異常）", "TCP_1", "bash: connect: 拒否\n", false, false},
		{"空の出力", "TCP_1", "", false, false},
		{"OK の部分文字列が FAIL を誤検知しない", "HTTP_2", "ARTEX_HTTP_2_OK_200", true, true},
		{"他のプローブマーカーを誤検知しない", "TCP_1", "ARTEX_TCP_2_OK\n", false, false},
	}
	for _, c := range cases {
		ok, found := ParseProbeOutput(c.tag, c.out)
		if ok != c.wantOK || found != c.wantFound {
			t.Errorf("%s: ParseProbeOutput(%q,%q) = (%v,%v), want (%v,%v)",
				c.name, c.tag, c.out, ok, found, c.wantOK, c.wantFound)
		}
	}
}

func rep(results ...ProbeResult) *ProbeReport {
	return &ProbeReport{SessionID: 1, Results: results}
}

func TestProbeCmdTCPThreeState(t *testing.T) {
	cmd := probeCmd("tcp", "TCP_1", "10.0.0.5:20000")
	for _, want := range []string{
		"timeout 6", "/dev/tcp/10.0.0.5/20000", "rc=$?",
		"ARTEX_TCP_1_OK_FILTERED", "ARTEX_TCP_1_OK_REFUSED", "ARTEX_TCP_1_OK_FAIL",
		"refused", // 拒否キーワードのマッチ（RST は疎通とみなす）
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("tcp プローブコマンドに %q がありません:\n%s", want, cmd)
		}
	}
}

func TestParseTCPOutput(t *testing.T) {
	cases := []struct {
		name      string
		tag, out  string
		wantState TCPState
		wantFound bool
	}{
		{"接続成功", "TCP_1", "ARTEX_TCP_1_OK\n", TCPConnected, true},
		{"refused 接続拒否（経路は通るがリスナーなし）", "TCP_1", "ARTEX_TCP_1_OK_REFUSED\n", TCPRefused, true},
		{"refused を connected と誤判定しない", "TCP_1", "bash: connect: Connection refused\nARTEX_TCP_1_OK_REFUSED\n", TCPRefused, true},
		{"タイムアウト filtered", "TCP_1", "ARTEX_TCP_1_OK_FILTERED\n", TCPFiltered, true},
		{"filtered を connected と誤判定しない", "TCP_1", "ARTEX_TCP_1_OK_FILTERED\n", TCPFiltered, true},
		{"その他の失敗", "TCP_1", "ARTEX_TCP_1_OK_FAIL\n", TCPFail, true},
		{"マーカーなし（チャネル異常）", "TCP_1", "bash: command not found\n", TCPFail, false},
		{"空の出力", "TCP_1", "", TCPFail, false},
		{"他のプローブマーカーを誤検知しない", "TCP_1", "ARTEX_TCP_2_OK\n", TCPFail, false},
	}
	for _, c := range cases {
		state, found := ParseTCPOutput(c.tag, c.out)
		if state != c.wantState || found != c.wantFound {
			t.Errorf("%s: ParseTCPOutput(%q,%q) = (%v,%v), want (%v,%v)",
				c.name, c.tag, c.out, state, found, c.wantState, c.wantFound)
		}
	}
}

func TestProbeAddrs(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		poolRange string
		want      []string
	}{
		{
			"常用ポート + ポートプールの先頭と末尾をサンプリング",
			"10.0.0.5", "20000-21000",
			[]string{"10.0.0.5:443", "10.0.0.5:80", "10.0.0.5:53", "10.0.0.5:20000", "10.0.0.5:21000"},
		},
		{
			"空のプールはデフォルトポートプールを使用",
			"10.0.0.5", "",
			[]string{"10.0.0.5:443", "10.0.0.5:80", "10.0.0.5:53", "10.0.0.5:20000", "10.0.0.5:21000"},
		},
		{
			"不正なプールは常用ポートのみ残す",
			"10.0.0.5", "abc",
			[]string{"10.0.0.5:443", "10.0.0.5:80", "10.0.0.5:53"},
		},
		{
			"プールのポートが常用ポートと重複する場合は重複排除",
			"10.0.0.5", "80-90",
			[]string{"10.0.0.5:443", "10.0.0.5:80", "10.0.0.5:53", "10.0.0.5:90"},
		},
		{
			"単一ポートのプールは重複サンプリングしない",
			"10.0.0.5", "20000-20000",
			[]string{"10.0.0.5:443", "10.0.0.5:80", "10.0.0.5:53", "10.0.0.5:20000"},
		},
	}
	for _, c := range cases {
		got := ProbeAddrs(c.host, c.poolRange)
		if len(got) != len(c.want) {
			t.Errorf("%s: ProbeAddrs(%q,%q) = %v, want %v", c.name, c.host, c.poolRange, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: ProbeAddrs(%q,%q)[%d] = %q, want %q", c.name, c.host, c.poolRange, i, got[i], c.want[i])
			}
		}
	}
}

func TestConcludeGrades(t *testing.T) {
	tcpOK := ProbeResult{Name: "tcp_1", Kind: "tcp", Target: "10.0.0.5:443", OK: true, State: string(TCPConnected)}
	tcpRefused := ProbeResult{Name: "tcp_4", Kind: "tcp", Target: "10.0.0.5:20000", OK: true, State: string(TCPRefused)}
	tcpFiltered := ProbeResult{Name: "tcp_1", Kind: "tcp", Target: "10.0.0.5:443", State: string(TCPFiltered)}
	tcpFail := ProbeResult{Name: "tcp_1", Kind: "tcp", Target: "10.0.0.5:443"}
	httpOK := ProbeResult{Name: "http_1", Kind: "http", Target: "10.0.0.5:443", OK: true}
	dnsOK := ProbeResult{Name: "dns", Kind: "dns", Target: "www.baidu.com", OK: true}
	icmpOK := ProbeResult{Name: "icmp", Kind: "icmp", Target: "223.5.5.5", OK: true}

	cases := []struct {
		name         string
		report       *ProbeReport
		wantGrade    string
		wantFeasible bool
		wantPort     int
	}{
		{"TCP 疎通→リバース socks", rep(tcpOK, dnsOK), GradeTCPEgress, true, 443},
		{"TCP 疎通時は最初の疎通ポートを推奨", rep(ProbeResult{Kind: "tcp", Target: "10.0.0.5:80", OK: true}, tcpOK), GradeTCPEgress, true, 80},
		{"refused も疎通扱い（RST で経路到達を証明）", rep(tcpFiltered, tcpRefused, dnsOK), GradeTCPEgress, true, 20000},
		{"常用ポートは filtered だがプールのポートが refused→tcp_egress", rep(
			ProbeResult{Kind: "tcp", Target: "10.0.0.5:443", State: string(TCPFiltered)},
			ProbeResult{Kind: "tcp", Target: "10.0.0.5:80", State: string(TCPFiltered)},
			ProbeResult{Kind: "tcp", Target: "10.0.0.5:53", State: string(TCPFiltered)},
			tcpRefused, dnsOK), GradeTCPEgress, true, 20000},
		{"HTTP のみ→http_only", rep(tcpFiltered, httpOK, dnsOK), GradeHTTPOnly, true, 0},
		{"全て filtered だが DNS は疎通→dns_only でトンネル非推奨", rep(tcpFiltered, dnsOK), GradeDNSOnly, false, 0},
		{"DNS のみ→dns_only でトンネル非推奨", rep(tcpFail, dnsOK), GradeDNSOnly, false, 0},
		{"ICMP のみ→icmp_only", rep(tcpFail, icmpOK), GradeICMPOnly, false, 0},
		{"全断→isolated でフォワード/WebShell を推奨", rep(tcpFail), GradeIsolated, false, 0},
		{"空のレポート→isolated", rep(), GradeIsolated, false, 0},
	}
	for _, c := range cases {
		got := Conclude(c.report)
		if got.Grade != c.wantGrade || got.Feasible != c.wantFeasible || got.SuggestedPort != c.wantPort {
			t.Errorf("%s: Conclude = %+v, want grade=%s feasible=%v port=%d",
				c.name, got, c.wantGrade, c.wantFeasible, c.wantPort)
		}
		if got.Recommendation == "" {
			t.Errorf("%s: Recommendation が空です", c.name)
		}
	}
}

func TestProbeReportHelpers(t *testing.T) {
	r := rep(
		ProbeResult{Kind: "tcp", Target: "10.0.0.5:443"},
		ProbeResult{Kind: "tcp", Target: "10.0.0.5:53", OK: true},
		ProbeResult{Kind: "dns", Target: "x", OK: true},
	)
	if !r.OKKind("tcp") || !r.OKKind("dns") || r.OKKind("http") {
		t.Error("OKKind の判定が誤っています")
	}
	if p := r.FirstOKPort(); p != 53 {
		t.Errorf("FirstOKPort = %d, want 53", p)
	}
}
