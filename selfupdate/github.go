package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo はリリース元。設定項目にせずハードコードする：更新元を設定可能にすることは、設定を変更できる
// 誰かにリモートコード実行の経路を与えることになり、ペネトレーションテスト基盤としては許されない。
const Repo = "Autumn-27/artex"

// latestURL は GitHub の「最新の正式版」API。prerelease と draft は自動的にスキップされる。
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts はアップグレード経路がアクセスできるドメインを限定する。後述の checkRedirect と組み合わせ、
// いずれかのリダイレクト先がリスト外のホストなら即座に失敗する——DNS ポイズニングや中間者攻撃で
// バイナリをすり替えられるのを防ぐ第一の関門で、第二は SHA256SUMS の照合。
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release アセットが実際に置かれるオブジェクトストレージ
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release は GitHub Release のうち本コードが扱うフィールド。
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset は Release に添付された 1 つのファイル。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient は GitHub のドメインだけを扱う HTTP クライアントを構築する。proxy が空なら直接接続する。
//
// あえてデフォルトの Transport を再利用しない：アップグレード経路は TLS を強制して証明書を検証する必要があり、
// 他所で設定された InsecureSkipVerify などの影響を受けてはならない。
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // パッケージ全体をダウンロードするため、リクエスト単位のタイムアウトで打ち切られない
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("リダイレクトが多すぎます")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL は https とドメイン許可リストを強制する。
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS 以外のアドレスを拒否しました: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub 以外のドメインを拒否しました: %s", u.Hostname())
	}
	return nil
}

// FetchLatest は最新の正式版を取得する。
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub へのアクセスに失敗しました（システム設定でグローバルプロキシを設定できます）: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 未認証の GitHub API は IP あたり 1 時間 60 回で、出口 IP を共有しているとすぐに上限に達する。
		return nil, fmt.Errorf("GitHub API のレート制限（1 時間 60 回）に達しました。しばらくしてから再試行してください")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("リポジトリ %s はまだ正式版を公開していません", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub が %d を返しました", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release の解析に失敗しました: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release に tag がありません")
	}
	return &rel, nil
}

// AssetName は現在のプラットフォームに対応するパッケージ名を返し、build.sh の package_binary と揃える：
// artex-<バージョン>-<os>-<arch>.zip（バージョン番号に v は付けない）。
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset は Release 内で名前からアセットを探す。
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
