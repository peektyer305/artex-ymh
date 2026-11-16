// deploy_test.go はデプロイ補助の純関数の単体テスト（実際の chisel デプロイは VPS の e2e 検収で確認する）。
package tunnel

import "testing"

func TestRewriteURLHost(t *testing.T) {
	cases := []struct {
		name         string
		url          string
		platformAddr string
		want         string
	}{
		{"ループバックのプレースホルダを callback host に置換", "http://127.0.0.1:8787/s/tok/chisel", "10.0.0.5:20001", "http://10.0.0.5:8787/s/tok/chisel"},
		{"ポートなしの URL", "http://example.com/s/tok/chisel", "10.0.0.5:20001", "http://10.0.0.5/s/tok/chisel"},
		{"platformAddr が不正ならそのまま返す", "http://127.0.0.1:8787/s/tok/chisel", "bad", "http://127.0.0.1:8787/s/tok/chisel"},
		{"URL が不正ならそのまま返す", "://nope", "10.0.0.5:20001", "://nope"},
	}
	for _, c := range cases {
		if got := rewriteURLHost(c.url, c.platformAddr); got != c.want {
			t.Errorf("%s: rewriteURLHost = %q, want %q", c.name, got, c.want)
		}
	}
}
