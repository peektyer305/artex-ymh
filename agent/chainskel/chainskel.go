// Package chainskel は反問思考チェーン(chains)の L2/L3 蒸留産物とルーティングロジックを保持する
// (CHAINS-INTEGRATION-DESIGN.md §3):
//
//   - L3 シナリオスケルトン:9 カテゴリ(web/ad/app/priv/pwn/recon/rev/pivot/meta)の
//     「シナリオ→判定基準→方向転換」意思決定スケルトン。オフラインで人手により <category>.md へ
//     蒸留し、go:embed でバイナリへ埋め込み、意図の chain_tags(またはフォールバックの
//     キーワードマッチ)で worker system へ注入する。コードによるルーティングで、モデルには依存しない。
//   - L2 meta 鉄則:planner/worker の 2 つの定数テキスト(meta_rules.go)。それぞれの
//     system prompt のコード固定末尾に追加する(静的で、キャッシュに有利)。
//
// フォールバック原則:スケルトン欠落・読み取り失敗、tag 不正、マッチ不中はいずれも
// 静かに非注入へ降格し、worker の起動を絶対にブロックしない。
package chainskel

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed *.md
var skeletonFS embed.FS

// Categories は有効な chain_tags の値(9 カテゴリ。コーパスのプレフィックスと一致)。
var Categories = []string{"web", "ad", "app", "priv", "pwn", "recon", "rev", "pivot", "meta"}

// MaxTagsPerIntent は各意図ごとに最大で注入するスケルトンカテゴリ数(L3 ルーティング上限)。
const MaxTagsPerIntent = 2

// sectionCapRunes は worker system へ注入するスケルトンブロックの総文字数(rune)上限。
// 超過時は切り詰めて注記する。テストで小さくして切り詰め経路を検証できるよう、
// パッケージレベルの変数にしている。
var sectionCapRunes = 8000

// Valid は tag が有効な chain_tags の値かを報告する。
func Valid(tag string) bool {
	for _, c := range Categories {
		if tag == c {
			return true
		}
	}
	return false
}

// NormalizeChainTags はタグを正規化する(小文字化、空白除去、重複除去、順序保持)。
func NormalizeChainTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		dup := false
		for _, o := range out {
			if o == t {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, t)
		}
	}
	return out
}

// ValidateChainTags はタグを検証する:値は Categories に属し、かつ最大 MaxTagsPerIntent 個で、
// (注入側は先頭 2 個のみを取る。多く渡すのは planner の誤用なので、直接エラーにして修正させる)。
func ValidateChainTags(tags []string) error {
	for _, t := range tags {
		if !Valid(t) {
			return fmt.Errorf("chain_tags に不正な値 %q を含む:有効な値は %s", t, strings.Join(Categories, "/"))
		}
	}
	if len(tags) > MaxTagsPerIntent {
		return fmt.Errorf("chain_tags は最大 %d 個(与えられたのは %d 個):主要攻撃面に絞って 1-2 個にすること", MaxTagsPerIntent, len(tags))
	}
	return nil
}

// Skeleton はあるカテゴリのスケルトンテキストを返す。ファイル欠落・読み取り失敗時は
// ok=false を返す(呼び出し側は静かに降格)。
func Skeleton(category string) (string, bool) {
	if !Valid(category) {
		return "", false
	}
	b, err := skeletonFS.ReadFile(category + ".md")
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", false
	}
	return s, true
}

// keywordRules はフォールバックのキーワードマッチ(意図に chain_tags がない場合に使用):
// 意図テキスト/vulnclass のキーワード → カテゴリ。純粋関数で決定的。
// 表の順序で先頭から MaxTagsPerIntent 個のヒットカテゴリを取る。
// キーワードはなるべく複数文字の語にして誤ヒットを減らす(例:「スタック」を単独で列挙せず
// 「スタックオーバーフロー」を使う)。
var keywordRules = []struct {
	cat      string
	keywords []string
}{
	{"web", []string{"sql", "インジェクション", "xss", "ssrf", "ssti", "xxe", "デシリアライズ", "ファイルアップロード", "アップロードポイント", "コマンドインジェクション", "コマンド実行", "コード実行", "rce", "waf", "ロジック脆弱性", "権限外アクセス", "jwt", "ファイルインクルード", "ディレクトリトラバーサル", "cms"}},
	{"ad", []string{"kerberos", "ドメインコントローラ", "ADドメイン", "アクティブディレクトリ", "ntlm", "dcsync", "kerberoast", "委任", "adcs", "ゴールデンチケット", "シルバーチケット", "as-rep", "ドメインペネトレーション", "ドメイン内", "パスワードスプレー"}},
	{"app", []string{"apk", "frida", "ミニプログラム", "アンドロイド", "android", "ios", "パケットキャプチャ", "app"}},
	{"priv", []string{"権限昇格", "権限昇格", "privesc", "suid", "カーネル脆弱性", "disable_functions", "コンテナエスケープ", "エスケープ"}},
	{"pwn", []string{"スタックオーバーフロー", "ヒープオーバーフロー", "ヒープスプレー", "書式文字列", "ctf", "pwn", "shellcode", "rop", "uaf", "double free", "tcache", "スタックピボット"}},
	{"recon", []string{"偵察", "フィンガープリント", "サブドメイン", "情報収集", "osint", "ポートスキャン", "ディレクトリスキャン", "アセット調査", "フットプリンティング", "機密情報", "漏えい"}},
	{"rev", []string{"リバースエンジニアリング", "アンパック", "デコンパイル", "パッキング", "アンチデバッグ", "難読化", "静的解析", "動的デバッグ", "アルゴリズム復元"}},
	{"pivot", []string{"トンネル", "プロキシ", "横展開", "内部ネットワーク", "ピボット", "socks", "chisel", "frp", "ポートフォワーディング", "踏み台", "足場確保", "webshell"}},
}

// Match はフォールバックのキーワードマッチ:自由テキスト(意図 summary / vulnclass)から
// 最大 MaxTagsPerIntent 個のカテゴリを導出し、ヒットなしなら nil を返す(呼び出し側は静かに非注入)。
func Match(text string) []string {
	text = strings.ToLower(text)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	for _, r := range keywordRules {
		for _, kw := range r.keywords {
			if strings.Contains(text, kw) {
				out = append(out, r.cat)
				break
			}
		}
		if len(out) >= MaxTagsPerIntent {
			break
		}
	}
	return out
}

// wrapSection は L3 スケルトンの軽量 section マーカー(WrapUntrustedData と同じ
// セクション分割の考え方だが、意味は逆:スケルトンはプラットフォーム内蔵の
// 【信頼できる】内容で、マーカーはモデルに「これはプラットフォームが注入した意思決定の規律」と
// 対象/worker が生成する信頼できないデータを区別させるためだけのもの。
// untrusted のタグや表現は再利用しない)。
func wrapSection(category, body string) string {
	safe := strings.ReplaceAll(body, "</chains-skeleton", "＜/chains-skeleton")
	return `<chains-skeleton source="chains-skeleton" category="` + category + `">` + "\n" + safe + "\n</chains-skeleton>"
}

// Section は複数カテゴリのスケルトンを worker system へ注入するブロックへ組み立てる:
// カテゴリごとに 1 つの <chains-skeleton> セクションにし、総文字数は sectionCapRunes(8K)を
// 上限として超過時は切り詰めて注記する。スケルトンが欠落したカテゴリは静かにスキップ。
// 最終的に空なら "" を返す。
func Section(cats []string) string {
	header := "\n\n【反問意思決定チェーンスケルトン(プラットフォーム内蔵の信頼できる内容。対象データではない):本意図の攻撃面に応じて注入する「シナリオ→判定基準→方向転換」の意思決定リフレクション。探索が阻まれたとき・結論を下すとき・真偽を判定するときに照らし合わせて自問する。完全な Q&A 原文は対応する skills/chains-<カテゴリ>/refs/ を参照】"
	var b strings.Builder
	b.WriteString(header)
	used := len([]rune(header))
	truncated := false
	injected := 0
	for _, c := range cats {
		if injected >= MaxTagsPerIntent {
			break
		}
		sk, ok := Skeleton(c)
		if !ok {
			continue // スケルトン欠落・読み取り失敗:静かに降格
		}
		sec := wrapSection(c, sk)
		secRunes := len([]rune(sec))
		if used+secRunes > sectionCapRunes {
			// 残り領域に切り詰めたスケルトン + 注記を入れる。領域が足りなければ全体を切り詰める。
			remain := sectionCapRunes - used
			if remain > 0 {
				r := []rune(sec)
				if remain > len(r) {
					remain = len(r)
				}
				b.WriteString(string(r[:remain]))
			}
			truncated = true
			break
		}
		b.WriteString("\n")
		used++ // 上記の改行
		b.WriteString(sec)
		used += secRunes
		injected++
	}
	if injected == 0 && !truncated {
		return ""
	}
	if truncated {
		mark := "\n…(スケルトンの総長が 8K を超えたため切り詰め済み。完全な原文は skills/chains-<カテゴリ>/refs/ を参照)"
		b.WriteString(mark)
	}
	return b.String()
}

// intentCats は ForIntent/RefsGuideForIntent が共用するカテゴリ解決:chain_tags を優先し
// (正規化後に有効な先頭 MaxTagsPerIntent 個を取る。不正値は無視)。有効な tags がなければ
// summary のキーワードマッチへフォールバック。マッチしなければ nil を返す。
func intentCats(payload json.RawMessage) []string {
	var p struct {
		Summary   string   `json:"summary"`
		ChainTags []string `json:"chain_tags"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	var cats []string
	for _, t := range NormalizeChainTags(p.ChainTags) {
		if Valid(t) {
			cats = append(cats, t)
		}
		if len(cats) >= MaxTagsPerIntent {
			break
		}
	}
	if len(cats) == 0 {
		cats = Match(p.Summary)
	}
	return cats
}

// ForIntent は意図ノードの payload から L3 注入ブロックを構築する:chain_tags を優先し
// (正規化後に有効な先頭 2 個を取る。不正値は無視)。tags がなければ summary のキーワードマッチへ
// フォールバック。マッチしないかスケルトンが欠落すれば "" を返す(注入せず、worker をブロックしない)。
func ForIntent(payload json.RawMessage) string {
	cats := intentCats(payload)
	if len(cats) == 0 {
		return ""
	}
	return Section(cats)
}
