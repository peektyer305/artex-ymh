package guard

import (
	"reflect"
	"testing"
)

func TestExtractHosts(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{"curl URL", `curl https://api.example.com:8443/v1/login`, []string{"api.example.com"}},
		{"curl URL パイプ", `curl -s http://a.example.com/ | grep x`, []string{"a.example.com"}},
		{"nmap IP", `nmap -sV -p 80,443 10.0.0.7`, []string{"10.0.0.7"}},
		{"nmap CIDR リテラルはホストとして扱う", `nmap 192.168.1.0/24`, nil}, // / を含むリテラルは抽出しない（保守的）
		{"ssh user@host", `ssh admin@jump.example.com`, []string{"jump.example.com"}},
		{"ssh user@ip", `ssh root@172.16.0.5`, []string{"172.16.0.5"}},
		{"nc host port", `nc 10.0.0.9 4444`, []string{"10.0.0.9"}},
		{"host:port パラメータ", `ncat example.com 443`, []string{"example.com"}},
		{"ping ホスト名", `ping db.internal.example.com`, []string{"db.internal.example.com"}},
		{"wget URL", `wget http://files.example.com/a.sh -O /tmp/a.sh`, []string{"files.example.com"}},
		{"複数のターゲット", `curl http://one.example.com/ && nmap 10.1.1.1 && ssh u@two.example.com`,
			[]string{"one.example.com", "10.1.1.1", "two.example.com"}},
		{"ターゲットなしのコマンド", `ls -la /tmp`, nil},
		{"ターゲットなしのコマンド2", `cat /etc/passwd | grep root`, nil},
		{"flag 値はターゲットにしない", `nmap -iL targets.txt`, nil},
		{"flag 値はターゲットにしない2", `curl -H example.com http://real.example.com/`, []string{"real.example.com"}},
		{"独立した IP リテラル", `echo 8.8.8.8`, []string{"8.8.8.8"}},
		{"ツールのフルパス", `/usr/bin/nmap 10.2.2.2`, []string{"10.2.2.2"}},
		{"sudo プレフィックス", `sudo nmap 10.3.3.3`, []string{"10.3.3.3"}},
		{"重複排除", `ping 10.0.0.1; nmap 10.0.0.1`, []string{"10.0.0.1"}},
		{"空のコマンド", ``, nil},
	}
	for _, c := range cases {
		got := ExtractHosts(c.cmd)
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ExtractHosts(%q) = %v, want %v", c.name, c.cmd, got, c.want)
		}
	}
}
