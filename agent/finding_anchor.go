package agent

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// report_finding のアセット自動アンカリング（純粋なロジック層、単体テスト可能）。
//
// 背景:方針 A の同一入口マージ（db/finding_merge.go）は (task_id、正規化した vulnclass、
// 主アセット=AssetIDs[0]) の 3 つが一致することを要求する。負荷試験では worker が
// asset_ids を付けずに報告することが多く、マージキーが全体として機能しなくなった。
// そこで asset_ids が空のときは報告テキスト（name+summary+evidence）から最初の URL を
// 抽出し（URL が無ければ host[:port]/素の host）、DB 全体のアセット（呼び出し側が既に SQL 層で
// host により粗く絞り込む。db.AssetStore.QueryByHost を参照）の中から
//
//	endpoint（同一 scheme://host:port、path の前方一致で最長の接頭辞）
//	→ service（同一 scheme://host:port）
//	→ ip / root_domain
//
// の優先順で主アセットを 1 件アンカリングする。どれにも一致しなければ空のままとし、アセットを
// 無理に新規作成しない。

var (
	// findingURLRe はテキスト中で最初の http(s) URL を取る。
	findingURLRe = regexp.MustCompile(`https?://[^\s)\]"']+`)
	// findingHostRe は URL が無いときに最初の host[:port]（IPv4 またはドメイン）を取る。
	findingHostRe = regexp.MustCompile(`\b(?:\d{1,3}(?:\.\d{1,3}){3}|(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,})(?::\d{1,5})?\b`)
)

// anchorTarget は報告テキストから抽出した位置特定の対象。
type anchorTarget struct {
	raw    string // 一致した元テキスト（URL または host[:port]）。name が空のときのフォールバック用
	hasURL bool
	scheme string // hasURL のときのみ有意。http/https の小文字
	host   string // 小文字の hostname
	port   int    // 0 = 未指定。URL のときは scheme に応じて既定ポートを補う
	path   string // hasURL のときのみ有意。最低 "/"
}

// extractAnchorTarget はテキスト中で最初の URL を取る。URL が無ければ最初の host[:port]/素の host を取る。
func extractAnchorTarget(text string) anchorTarget {
	if m := findingURLRe.FindString(text); m != "" {
		m = strings.TrimRight(m, ".,;:!?")
		if u, err := url.Parse(m); err == nil && u.Hostname() != "" {
			scheme := strings.ToLower(u.Scheme)
			port := 0
			if p := u.Port(); p != "" {
				port, _ = strconv.Atoi(p)
			}
			if port == 0 {
				switch scheme {
				case "http":
					port = 80
				case "https":
					port = 443
				}
			}
			path := u.Path
			if path == "" {
				path = "/"
			}
			return anchorTarget{raw: m, hasURL: true, scheme: scheme, host: strings.ToLower(u.Hostname()), port: port, path: path}
		}
	}
	if m := findingHostRe.FindString(text); m != "" {
		host, port := m, 0
		if i := strings.LastIndex(m, ":"); i >= 0 {
			if p, err := strconv.Atoi(m[i+1:]); err == nil {
				host, port = m[:i], p
			}
		}
		return anchorTarget{raw: m, host: strings.ToLower(host), port: port, path: "/"}
	}
	return anchorTarget{}
}

// urlKey はアセット URL を (scheme, host, port, path) に分解する;host は小文字、ポートの
// 省略時は scheme に応じて 80/443 を補い、path の省略時は "/" とする。解析不能なら ok=false。
func urlKey(raw string) (scheme, host string, port int, path string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", "", 0, "", false
	}
	scheme = strings.ToLower(u.Scheme)
	host = strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	if port == 0 {
		switch scheme {
		case "http":
			port = 80
		case "https":
			port = 443
		}
	}
	path = u.Path
	if path == "" {
		path = "/"
	}
	return scheme, host, port, path, true
}

// pathPrefix は endpoint アセットのパス prefix が finding のパス path の接頭辞かを判定する
// （パスセグメントの境界で判定:"/api" は "/api/users" に一致するが "/apisix" には一致しない）。
func pathPrefix(prefix, path string) bool {
	if prefix == "" || prefix == "/" {
		return true
	}
	if path == prefix {
		return true
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return strings.HasPrefix(path, prefix+"/")
}

// anchorFindingAsset は candidates（DB 全体のアセット、既に host で粗く絞り込み済み）から報告
// テキストの主アセットを 1 件アンカリングし、アセット id と一致理由を返す。一致しなければ 0。
// 優先順位はファイル冒頭のコメントを参照。同点なら id の最小を取り、同じテキストと同じ
// アセット集合に対して結果が確定するようにする（マージキーの安定化）。
func anchorFindingAsset(text string, candidates []*db.Asset) (int64, string) {
	tgt := extractAnchorTarget(text)
	if tgt.host == "" {
		return 0, ""
	}
	// 1) endpoint:同一 scheme://host:port かつ path が前方一致するものから最長の接頭辞を取る。
	if tgt.hasURL {
		var best *db.Asset
		bestLen := -1
		for _, a := range candidates {
			if a == nil || a.Type != "endpoint" {
				continue
			}
			scheme, host, port, path, ok := urlKey(a.URL)
			if !ok || scheme != tgt.scheme || host != tgt.host || port != tgt.port {
				continue
			}
			if !pathPrefix(path, tgt.path) {
				continue
			}
			if len(path) > bestLen || (len(path) == bestLen && best != nil && a.ID < best.ID) {
				best, bestLen = a, len(path)
			}
		}
		if best != nil {
			return best.ID, "endpoint の最長接頭辞に一致"
		}
	}
	// 2) service:同一 scheme://host:port。URL が無い場合（素の host[:port]）は host（+port）で一致を取る。
	var svc *db.Asset
	for _, a := range candidates {
		if a == nil || a.Type != "service" {
			continue
		}
		scheme, host, port, _, ok := urlKey(a.URL)
		if !ok || host != tgt.host {
			continue
		}
		if tgt.hasURL {
			if scheme != tgt.scheme || port != tgt.port {
				continue
			}
		} else if tgt.port > 0 && port != tgt.port {
			continue
		}
		if svc == nil || a.ID < svc.ID {
			svc = a
		}
	}
	if svc != nil {
		return svc.ID, "service が同一 scheme://host:port に一致"
	}
	// 3) ip / root_domain のフォールバック。
	var ip *db.Asset
	for _, a := range candidates {
		if a == nil || a.Type != "ip" || a.IP != tgt.host {
			continue
		}
		if ip == nil || a.ID < ip.ID {
			ip = a
		}
	}
	if ip != nil {
		return ip.ID, "ip アセットに一致"
	}
	var root *db.Asset
	for _, a := range candidates {
		if a == nil || a.Type != "root_domain" {
			continue
		}
		d := strings.ToLower(strings.TrimSpace(a.Domain))
		if d == "" || (tgt.host != d && !strings.HasSuffix(tgt.host, "."+d)) {
			continue
		}
		if root == nil || a.ID < root.ID {
			root = a
		}
	}
	if root != nil {
		return root.ID, "root_domain アセットに一致"
	}
	return 0, ""
}

// fallbackFindingName は name が空のときのフォールバック：vulnclass + テキスト中の最初の
// URL/host。どちらも空なら固定のプレースホルダ名を返し、空文字の name を書き込まない。
func fallbackFindingName(vulnClass, text string) string {
	vc := strings.TrimSpace(vulnClass)
	if tgt := extractAnchorTarget(text); tgt.raw != "" {
		return strings.TrimSpace(vc + " " + tgt.raw)
	}
	if vc != "" {
		return vc
	}
	return "名称未設定の脆弱性"
}
