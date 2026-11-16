package db

import (
	"regexp"
	"testing"
)

// 純粋な単体テスト(PG 不要):F13 下限ルールの正規表現はよくある変種に一致し、通常のコマンドを誤検知してはならない。
func TestBuiltinHTTPServerRulePatterns(t *testing.T) {
	hits := map[string][]string{
		"python": {
			"python -m http.server 8000",
			"python3 -m http.server 8080 --bind 0.0.0.0",
			"python3.11 -m http.server",
			"sudo python -m SimpleHTTPServer 80",
			"cd /tmp && python3 -m http.server 8000 &",
		},
		"php": {
			"php -S 0.0.0.0:8000",
			"php -S 127.0.0.1:8080 -t /var/www",
			"php8.2 -S 0.0.0.0:80",
		},
		"busybox": {
			"busybox httpd -p 8080 -h /tmp",
			"busybox httpd -f -p 80",
		},
		"ruby": {
			"ruby -run -e httpd . -p 8000",
			"ruby -run -e httpd -- --port 8080",
		},
		"npx": {
			"npx serve -l 8080 .",
			"npx http-server -p 8000",
			"npx --yes serve .",
			"npx -y http-server .",
		},
	}
	misses := []string{
		"curl http://10.0.0.2:8787/s/abc/tool.sh -o /tmp/tool.sh", // stage_share の正常な取得
		"python3 exploit.py",                    // 通常のスクリプト
		"php -r 'echo 1;'",                      // php ワンライナー
		"php artisan serve",                     // フレームワークのサブコマンドで、php -S ではない
		"gem install httpd-tools",               // httpd の文字列を含む
		"ruby script.rb",                        // 通常の ruby
		"npm install",                           // npm は npx serve ではない
		"cat httpd.conf",                        // 設定ファイル
		"nmap -p 80,8000-9000 10.0.0.2",         // 通常のスキャン
	}
	var rules []*regexp.Regexp
	for _, r := range builtinHTTPServerRules {
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
				t.Errorf("[%s] 一致すべきだが一致しません: %s", group, c)
			}
		}
	}
	for _, c := range misses {
		if matchAny(c) {
			t.Errorf("正常なコマンドを誤検知しました: %s", c)
		}
	}
}
