package chainskel

import (
	"strconv"
	"strings"
)

// verifier の反証 checklist(chains L4 → verifier 判定層):脆弱性カテゴリごとに
// スケルトンライブラリの「判定基準」列から蒸留したコード定数。server 側が verifier の
// コンテキストをスナップショットする際に丸ごと注入し、verifier に印象ではなく
// 1 条ずつ反証させて verified/false_positive を下させる。
//
// 正規化の考え方は db.NormalizeVulnClass と一致する(折り畳み後の別名・キーワードヒット)。
// ただし本パッケージは独自にマッピングを保持する:chainskel は下位パッケージで、
// 逆に db へ依存するとレイヤー逆転・依存循環になるため。

// verifierClassRule は折り畳み後の vulnclass テキストを専用判定基準グループへマップする。
// 表順で最初にヒットしたものが有効になる。そのため、より具体的なキーワード(sql/xss/ssrf)を
// 広義の語(インジェクション/漏えい)より前に置き、かつ sqli は「インジェクション」ではなく
// "sql" をアンカーに使い、「コマンドインジェクション」を食い潰さないようにする。
var verifierClassRules = []struct {
	class    string
	keywords []string
}{
	{"cmdi", []string{"コマンドインジェクション", "コマンド実行", "コード実行", "commandinjection", "cmdi", "rce"}}, // sqli より先:「コマンドインジェクション」が「インジェクション」系の語に食われないようにする
	{"sqli", []string{"sql"}},
	{"xss", []string{"xss", "クロスサイト"}},
	{"ssrf", []string{"ssrf"}},
	{"upload", []string{"ファイルアップロード", "アップロード", "upload", "webshell"}},
	{"auth-bypass", []string{"権限外アクセス", "未認可", "認証バイパス", "認証・認可", "idor", "bypass", "ロジック脆弱性"}},
	{"info-leak", []string{"情報漏えい", "機密情報", "漏えい", "leak", "disclosure"}},
}

// verifierGeneralChecks は共通の反証層:どのカテゴリ(未識別カテゴリを含む)でも 1 条ずつ確認する。
var verifierGeneralChecks = []string{
	"200 ≠ 突破:WAF/ブロックページは 200 を偽装する。まず良性/悪性の対照を行い、差分のあるレスポンスだけをシグナルとする。対照のない 200 は証拠にならない。",
	"想定する特徴へアンカーする:結論は、攻撃が成功したときにしか現れない特徴(独自データ/線形の遅延/コールバック/ファイルの設置)に必ず結びつける。レスポンスが「正常に見える」「エラーが出ない」はいずれも証拠にならない。",
	"陰性 ≠ 脆弱性なし:再現に失敗したら、環境・防御・タイミング要因を除外してから false_positive とする。再現できず除外もできないときは inconclusive にして、無理に判定しない。",
}

// verifierClassChecks はカテゴリ別の専用反証判定基準(対応するスケルトンの「判定基準」列から蒸留)。
var verifierClassChecks = map[string][]string{
	"sqli": {
		"真偽の分岐が核心の判定基準:ブールブラインドは、同一リクエストが真/偽の条件下で再現可能な安定した差異を示すことが条件。単発のエラーは構文が parser に入ったことを示すだけで、制御可能である証明にはならない。",
		"時間ベースのブラインドはベースライン対照が必要:まず payload なしの応答時間ベースラインを測り、遅延はベースラインを有意に超え、かつ 2 回以上安定して再現すること。",
		"エラーのエコーは制御可能な出力に落とし込むこと:自分で構成した値(version()、リテラルなど)がレスポンスに現れて初めて成立する。",
	},
	"xss": {
		"エコー ≠ 実行可能:payload がエコーされるコンテキスト(HTML body/属性/JS 文字列/URL)を確認し、かつ重要な文字がエンコード・エスケープされていない場合に成立する。",
		"格納型は読み取り側のトリガーを検証する:書き込み後、独立したセッション/ページで再読み込みし、設置され想定どおりに描画されることを確認する。",
		"反射型はエコーがなく、スキャナの特徴だけでヒットした場合は verified と判定してはならない。",
	},
	"ssrf": {
		"コールバックは、送信元 IP が対象サーバー側の出口であることを確認する(対象の公開 IP/出口セグメントと対照)。CDN ノードやブラウザ側レンダリングによる偽 SSRF を除外する。",
		"外向き通信を証明しただけでは SSRF の影響は成立しない:内部ネットワーク/メタデータ/状態を変更するインターフェースへ到達できることをさらに検証するか、影響範囲を明示してレベルを定める。",
	},
	"cmdi": {
		"sleep の遅延は設定値と線形に対応すること(3s→3s、7s→7s)。それで初めてネットワークジッタによる誤検知を除外できる。単発の遅延や非線形の遅延では判定しない。",
		"エコー/帯域外の証拠を時間ベースより優先する:コールバックやコマンド出力のエコーがある場合はそれを採用し、時間ベースは格下げの手段に留める。",
	},
	"upload": {
		"設置+解析の二重検証:ファイルは、ディスク上のパスへ直接アクセスできること、かつアクセス時にスクリプトとして解析・実行されることを確認する。アクセスでソースが返る・直接ダウンロードされる場合は未解析であり、成立しない。",
		"ランダム名・パスでエコーがない場合、設置場所と解析挙動を確認する前は verified と判定してはならない。",
	},
	"auth-bypass": {
		"2 アカウントの対照が確固たる判定基準:B のセッションが、A があらかじめ埋め込んだ一意マーカーのある非公開リソースへアクセスできて初めて権限外アクセスが成立する。公開データ・公開インターフェースへの「権限外アクセス」は誤検知。",
		"読み取りと書き込みを別々に検証する:読めるからといって変更できるとは限らない。公開・共有データを読めただけでは verified と判定しない。",
	},
	"info-leak": {
		"データの真実性を優先する:サンプルは対象独自の実データ(対象のアセット/ユーザーに関連付けられるもの)を含むこと。デフォルトページ・テストデータ・公開情報は漏えいにならない。",
		"機密度でレベルを定める:パス・バージョン番号・banner のような低機密情報だけで高リスクと判定してはならない。",
	},
}

// normalizeVerifierClass は vulnclass を折り畳み(小文字化、空白・ハイフン・アンダースコア除去)、
// verifierClassRules で最初にヒットした専用判定基準グループ名を返す。ヒットしなければ "" を返す。
func normalizeVerifierClass(v string) string {
	key := strings.ToLower(v)
	for _, r := range []string{" ", "\t", "　", "-", "_", "/", "、", ":", ":"} {
		key = strings.ReplaceAll(key, r, "")
	}
	if key == "" {
		return ""
	}
	for _, rule := range verifierClassRules {
		for _, kw := range rule.keywords {
			if strings.Contains(key, kw) {
				return rule.class
			}
		}
	}
	return ""
}

// VerifierChecklist は vulnclass に応じて verifier の反証 checklist を生成する:共通層は常に含め、
// カテゴリにヒットしたらそのカテゴリの専用判定基準を追記する。コンテキストへ直接注入できる
// 固定テキストを返す。
func VerifierChecklist(vulnclass string) string {
	var b strings.Builder
	b.WriteString("【反証判定 checklist(プラットフォーム内蔵。以下の判定基準は反証に用い、1 条ずつ確認する)】\n共通判定基準(どのカテゴリでも確認):\n")
	for i, c := range verifierGeneralChecks {
		b.WriteString(strconv.Itoa(i+1) + ". " + c + "\n")
	}
	if class := normalizeVerifierClass(vulnclass); class != "" {
		b.WriteString("専用判定基準(vulnclass=\"" + strings.TrimSpace(vulnclass) + "\" → " + class + "):\n")
		for i, c := range verifierClassChecks[class] {
			b.WriteString(strconv.Itoa(i+1) + ". " + c + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
