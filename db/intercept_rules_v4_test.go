package db

import (
	"regexp"
	"testing"
)

// 純粋な単体テスト（PG 不要）:イントラネット期 2 の横展開/パスワードスプレー用レッドラインの正規表現が一般的な変種に命中し、
// 正常なコマンドを誤検知しないこと。hashcat によるローカル解読を止めない判断は db.go の builtinLateralRules のコメントを参照。
func TestBuiltinLateralRulePatterns(t *testing.T) {
	hits := map[string][]string{
		"crackmapexec/nxc": {
			"crackmapexec smb 10.0.0.0/24 -u administrator -p P@ssw0rd",
			"cme smb 10.0.0.5 -u admin -H aad3b435b51404ee",
			"nxc smb 10.0.0.0/24 -u users.txt -p pass.txt",
			"netexec winrm 10.0.0.9 -u admin -p pass",
		},
		"hydra": {
			"hydra -l root -P rockyou.txt ssh://10.0.0.5",
			"hydra -L users.txt -p Summer2026 10.0.0.0/24 smb",
		},
		"medusa": {
			"medusa -h 10.0.0.5 -u root -P pass.txt -M ssh",
			"medusa -H hosts.txt -U users.txt -P pass.txt -M rdp",
		},
		"psexec 系": {
			"python3 psexec.py administrator@10.0.0.5",
			"psexec.py -hashes aad3b435:31d6cfe0 administrator@10.0.0.5",
			"impacket-wmiexec CORP/administrator@10.0.0.5",
			"wmiexec.py administrator@10.0.0.5 'whoami'",
			"smbexec.py administrator@10.0.0.5",
		},
		"kerbrute": {
			"kerbrute passwordspray -d corp.local users.txt 'Summer2026'",
			"kerbrute userenum --dc 10.0.0.1 -d corp.local users.txt",
		},
	}
	misses := []string{
		"hashcat -m 1000 ntlm.txt rockyou.txt",        // ローカル解読のためブロックしない（判断は db.go のコメント参照）
		"john --wordlist=rockyou.txt shadow.txt",      // ローカル解読
		"ssh administrator@10.0.0.5",                  // 正常なログイン
		"smbclient //10.0.0.5/share -U administrator", // 正常な SMB アクセス
		"nmap -p 445,3389 10.0.0.0/24",                // 通常のスキャン
		"impacket-secretsdump administrator@10.0.0.5", // secretsdump は今回の対象外
		"python3 exploit.py 10.0.0.5",                 // 通常のスクリプト
	}
	// 既知のトレードオフ:"cat medusa.jpg" のような同じ語を含むファイル名は hydra/medusa の語境界マッチに命中する。
	// ask（deny ではない）では誤検知のコストは手動確認 1 回で許容範囲;それを消すために正規表現を複雑にはしない。
	var rules []*regexp.Regexp
	for _, r := range builtinLateralRules {
		re, err := regexp.Compile(r.pattern)
		if err != nil {
			t.Fatalf("ルール %q の正規表現コンパイルに失敗しました: %v", r.name, err)
		}
		rules = append(rules, re)
	}
	matchAny := func(s string) bool {
		for _, re := range rules {
			if re.MatchString(s) {
				return true
			}
		}
		return false
	}
	for group, cmds := range hits {
		for _, c := range cmds {
			if !matchAny(c) {
				t.Errorf("[%s] は命中すべきですが未命中です: %s", group, c)
			}
		}
	}
	for _, c := range misses {
		if matchAny(c) {
			t.Errorf("正常なコマンドを誤検知しました: %s", c)
		}
	}
}
