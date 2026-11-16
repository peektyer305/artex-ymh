package agent

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// anchorTestAssets は endpoint/service/ip/root_domain の 4 種類をカバーする固定のアセット集合。
func anchorTestAssets() []*db.Asset {
	return []*db.Asset{
		{ID: 1, Type: "endpoint", URL: "http://example.com/api"},
		{ID: 2, Type: "endpoint", URL: "http://example.com/api/users"},
		{ID: 3, Type: "endpoint", URL: "https://example.com/api"},
		{ID: 4, Type: "service", URL: "http://example.com"},
		{ID: 5, Type: "service", URL: "http://10.0.0.1:8080"},
		{ID: 6, Type: "ip", IP: "10.0.0.2"},
		{ID: 7, Type: "ip", IP: "10.0.0.1"},
		{ID: 8, Type: "root_domain", Domain: "example.com"},
	}
}

func TestAnchorFindingAsset(t *testing.T) {
	assets := anchorTestAssets()
	cases := []struct {
		name string
		text string
		want int64
	}{
		{
			name: "endpoint は最長プレフィックスを優先",
			text: "GET http://example.com/api/users/1?q=<script> による反射型 XSS",
			want: 2, // /api/users は /api より接頭辞が長い
		},
		{
			name: "endpoint は次に長いプレフィックス",
			text: "http://example.com/api/x にインジェクションあり",
			want: 1,
		},
		{
			name: "パスセグメント境界ではプレフィックス一致しない",
			// text の /api は /apisix に一致しない
			text: "http://example.com/apisix が未認証でアクセス可能",
			want: 4, // service にフォールバック
		},
		{
			name: "service へフォールバック",
			text: "http://example.com/other/path RCE",
			want: 4,
		},
		{
			name: "https/http の区別：https は https endpoint に一致",
			text: "https://example.com/api で機密情報が漏えい",
			want: 3,
		},
		{
			name: "https/http の区別：http service は https URL に一致しない",
			// https endpoint の /api は /login に前方一致しない
			text: "https://example.com/login の弱いパスワード",
			want: 8, // http service はポート/scheme が合わない → root_domain
		},
		{
			name: "明示ポートは既定ポートの service と一致しない",
			text: "http://example.com:8080/x で任意ファイルアップロード",
			want: 8, // service は 80 番ポート → root_domain
		},
		{
			name: "ip へフォールバック",
			text: "http://10.0.0.2:9000/admin に弱いパスワードあり",
			want: 6,
		},
		{
			name: "service は同じ host の ip より優先",
			text: "http://10.0.0.1:8080/actuator が未認証",
			want: 5,
		},
		{
			name: "root_domain へフォールバック:サブドメインの接尾辞一致",
			text: "https://sub.example.com/ で sqli",
			want: 8,
		},
		{
			name: "複数 URL は最初のものを取る",
			text: "先に http://example.com/api/x を要求し、次に http://10.0.0.2/y と比較",
			want: 1,
		},
		{
			name: "URL なしでは host:port で service に一致",
			text: "10.0.0.1:8080 に未認証でアクセス可能",
			want: 5,
		},
		{
			name: "URL なしの裸の host は ip へフォールバック",
			text: "10.0.0.2 は 22 番ポートが開放され、弱いパスワードでログイン可能",
			want: 6,
		},
		{
			name: "URL なしで一致なしなら空のまま",
			text: "内部ネットワークのサービスに弱いパスワードがあるが、利用できる識別子がない",
			want: 0,
		},
		{
			name: "空のテキスト",
			text: "",
			want: 0,
		},
		{
			name: "URL 末尾の句読点は除去する",
			text: "http://example.com/api/x. を参照、応答に入力が反射された",
			want: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := anchorFindingAsset(c.text, assets)
			if got != c.want {
				t.Errorf("anchorFindingAsset(%q) = %d (%s), want %d", c.text, got, reason, c.want)
			}
			if (got > 0) != (reason != "") {
				t.Errorf("id=%d と理由 %q が一致しません", got, reason)
			}
		})
	}
}

func TestAnchorFindingAssetEmptyCandidates(t *testing.T) {
	if got, _ := anchorFindingAsset("http://example.com/x", nil); got != 0 {
		t.Errorf("空のアセット集合では 0 を返すべきです, got %d", got)
	}
}

func TestAnchorFindingAssetDeterministicTie(t *testing.T) {
	// 同じパスの endpoint が 2 件：id の最小を取り、マージキーを安定させる。
	assets := []*db.Asset{
		{ID: 9, Type: "endpoint", URL: "http://example.com/api"},
		{ID: 4, Type: "endpoint", URL: "http://example.com/api"},
	}
	if got, _ := anchorFindingAsset("http://example.com/api/x", assets); got != 4 {
		t.Errorf("同点なら最小の id 4 を取るべきです, got %d", got)
	}
}

func TestFallbackFindingName(t *testing.T) {
	cases := []struct {
		vulnClass, text, want string
	}{
		{"xss", "http://example.com/api/users/1 で反射を確認", "xss http://example.com/api/users/1"},
		{"sqli", "URL も host もない", "sqli"},
		{"", "", "名称未設定の脆弱性"},
		{"  xss  ", "http://a.com/1", "xss http://a.com/1"},
	}
	for _, c := range cases {
		if got := fallbackFindingName(c.vulnClass, c.text); got != c.want {
			t.Errorf("fallbackFindingName(%q, %q) = %q, want %q", c.vulnClass, c.text, got, c.want)
		}
	}
}
