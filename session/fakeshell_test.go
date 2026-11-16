package session

// fakeshell_test.go は受け入れテストの目標側シミュレーション：メモリ内ファイルシステム + ミニ sh/cmd インタプリタで、
// ドライバが発行するあらゆるコマンド形態（echo/base64/dd/sh/rm/cat/tr/id/whoami/
// certutil など）を実行できる。PG にもホスト側 shell にも依存せず、すべてプロセス内で完結する。
// 注意：これは「プロトコルファミリ」レベルのシミュレーション——PHP コード実行フィールド、JSP sh エコー、ASPX cmd
// エコーの 3 種類のプロトコル形態を模擬し、特定の検証環境の値は一切含まない。

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// fakeFS は目標側ファイルシステムのメモリ内シミュレーション + sh インタプリタ。
type fakeFS struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newFakeFS() *fakeFS { return &fakeFS{files: map[string][]byte{}} }

func (fs *fakeFS) readFile(p string) ([]byte, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	b, ok := fs.files[p]
	return append([]byte(nil), b...), ok
}

func (fs *fakeFS) writeFile(p string, data []byte, appendMode bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if appendMode {
		fs.files[p] = append(fs.files[p], data...)
	} else {
		fs.files[p] = append([]byte(nil), data...)
	}
}

func (fs *fakeFS) remove(p string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.files, p)
}

func (fs *fakeFS) hasPrefixMatch(prefix string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for p := range fs.files {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// tokenize は sh の字句規則で切り分ける：単一引用符リテラル、二重引用符リテラル（変数展開はしない）、バックスラッシュエスケープ。
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	inS, inD, has := false, false, false
	flush := func() {
		if cur.Len() > 0 || has {
			out = append(out, cur.String())
			cur.Reset()
			has = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inS:
			if c == '\'' {
				inS = false
			} else {
				cur.WriteByte(c)
			}
		case inD:
			if c == '"' {
				inD = false
			} else {
				cur.WriteByte(c)
			}
		case c == '\'':
			inS, has = true, true
		case c == '"':
			inD, has = true, true
		case c == ' ' || c == '\t':
			flush()
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// splitQuoted はトップレベルの区切り文字で分割する（引用符内では分割しない）。
func splitQuoted(s string, seps ...string) []string {
	var out []string
	inS, inD := false, false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inS:
			if c == '\'' {
				inS = false
			}
		case inD:
			if c == '"' {
				inD = false
			}
		case c == '\'':
			inS = true
		case c == '"':
			inD = true
		default:
			for _, sep := range seps {
				if strings.HasPrefix(s[i:], sep) {
					out = append(out, s[start:i])
					i += len(sep) - 1
					start = i + 1
					goto next
				}
			}
		}
	next:
	}
	out = append(out, s[start:])
	return out
}

var arithRe = regexp.MustCompile(`\$\(\((\d+)\+(\d+)\)\)`)

// expandSh は POSIX sh の算術展開を模擬する（プローブ用）。
func expandSh(s string) string {
	return arithRe.ReplaceAllStringFunc(s, func(m string) string {
		g := arithRe.FindStringSubmatch(m)
		a, _ := strconv.Atoi(g[1])
		b, _ := strconv.Atoi(g[2])
		return strconv.Itoa(a + b)
	})
}

// run は複合 sh コマンドを 1 つ実行する（;、&&、|、>、>>、2>&1、2>/dev/null）。
// 統合出力と最後のセグメントの成功フラグを返す。
func (fs *fakeFS) run(cmd string) (string, bool) {
	var out strings.Builder
	ok := true
	// まず && で、次に ; で分割する（&& と ; は同レベルで左結合、ドライバの形態を覆うには十分）。
	segs := splitQuoted(cmd, "&&", ";")
	skip := false
	for i, seg := range segs {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		// セグメント前の連結子を判定する：本セグメントの元の位置を探すのは複雑すぎるため、分割順に再走査する。
		_ = i
		if skip {
			skip = false
			continue
		}
		o, k := fs.runSegment(expandSh(seg))
		out.WriteString(o)
		ok = k
		if !ok && strings.Contains(cmd, "&&") {
			// && の短絡の近似：失敗したらセグメントを 1 つ飛ばす（ドライバの形態では && は
			// `base64 -d > f && sh f` にのみ現れ、失敗時に停止するのは意味に合う）。
			skip = true
		}
	}
	return out.String(), ok
}

// runSegment は 1 つのパイプラインセグメントを実行する（| とリダイレクトを含みうる）。
func (fs *fakeFS) runSegment(seg string) (string, bool) {
	stages := splitQuoted(seg, "|")
	input := ""
	out := ""
	ok := true
	for si, st := range stages {
		args, redirect, appendMode := parseRedirect(tokenize(strings.TrimSpace(st)))
		if len(args) == 0 {
			continue
		}
		out, ok = fs.runBuiltin(args, input)
		input = out
		if si == len(stages)-1 && redirect != "" {
			fs.writeFile(redirect, []byte(out), appendMode)
			out = ""
		}
		if !ok {
			break
		}
	}
	return out, ok
}

// parseRedirect は 2>&1 / 2>/dev/null を取り除き、> >> のターゲットを抽出する。
func parseRedirect(args []string) (clean []string, target string, appendMode bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "2>&1" || a == "2>/dev/null" || strings.HasSuffix(a, "2>&1"):
			continue
		case a == ">" || a == ">>":
			if i+1 < len(args) {
				target = args[i+1]
				appendMode = a == ">>"
				i++
			}
		case strings.HasPrefix(a, ">>") && len(a) > 2:
			target, appendMode = a[2:], true
		case strings.HasPrefix(a, ">") && len(a) > 1:
			target = a[1:]
		default:
			clean = append(clean, a)
		}
	}
	return clean, target, appendMode
}

// tolerantB64Decode は空白を許容する（実際の base64 -d は改行を無視する）。
func tolerantB64Decode(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	return base64.StdEncoding.DecodeString(s)
}

func (fs *fakeFS) runBuiltin(args []string, stdin string) (string, bool) {
	switch args[0] {
	case "echo":
		// sh echo：デフォルトで改行を出力し、\n は解釈しない（ドライバは依存しない）。
		return strings.Join(args[1:], " ") + "\n", true
	case "printf":
		if len(args) < 2 {
			return "", true
		}
		format := strings.ReplaceAll(args[1], `\n`, "\n")
		var rest []any
		for _, a := range args[2:] {
			rest = append(rest, a)
		}
		return fmt.Sprintf(format, rest...), true
	case "cat":
		var sb strings.Builder
		for _, f := range args[1:] {
			b, ok := fs.readFile(f)
			if !ok {
				return sb.String() + "cat: " + f + ": No such file or directory\n", false
			}
			sb.Write(b)
		}
		return sb.String(), true
	case "base64":
		decode := false
		for _, a := range args[1:] {
			if a == "-d" {
				decode = true
			}
		}
		if decode {
			b, err := tolerantB64Decode(stdin)
			if err != nil {
				return "base64: invalid input\n", false
			}
			return string(b), true
		}
		return base64.StdEncoding.EncodeToString([]byte(stdin)) + "\n", true
	case "dd":
		var ifile string
		var bs, skip int
		for _, a := range args[1:] {
			kv := strings.SplitN(a, "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch kv[0] {
			case "if":
				ifile = kv[1]
			case "bs":
				bs, _ = strconv.Atoi(kv[1])
			case "skip":
				skip, _ = strconv.Atoi(kv[1])
			}
		}
		b, ok := fs.readFile(ifile)
		if !ok {
			return "dd: failed to open '" + ifile + "': No such file or directory\n", false
		}
		off := bs * skip
		if off >= len(b) {
			return "", true
		}
		end := off + bs
		if end > len(b) || bs == 0 {
			end = len(b)
		}
		return string(b[off:end]), true
	case "sh":
		if len(args) < 2 {
			return "sh: missing script\n", false
		}
		b, ok := fs.readFile(args[1])
		if !ok {
			return "sh: 0: cannot open " + args[1] + ": No such file\n", false
		}
		return fs.run(string(b))
	case "rm":
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			fs.remove(a)
		}
		return "", true
	case "touch":
		for _, a := range args[1:] {
			if _, ok := fs.readFile(a); !ok {
				fs.writeFile(a, nil, false)
			}
		}
		return "", true
	case "id":
		return "uid=33(www-data) gid=33(www-data) groups=33(www-data)\n", true
	case "whoami":
		return "www-data\n", true
	case "uname":
		return "Linux\n", true
	case "tr":
		if len(args) >= 3 && args[1] == "a-z" && args[2] == "A-Z" {
			return strings.ToUpper(stdin), true
		}
		return stdin, true
	case "true":
		return "", true
	}
	return "sh: " + args[0] + ": not found\n", false
}

// ---------------- Windows cmd シミュレーション（ASPX 用） ----------------

// fakeCmd は cmd.exe /c の最小限の意味論を模擬する：& 連結、未定義の %VAR% は空に展開、
// echo/type/certutil/del。
type fakeCmd struct{ fs *fakeFS }

var cmdVarRe = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)

func (c *fakeCmd) run(cmdline string) string {
	cmdline = cmdVarRe.ReplaceAllString(cmdline, "") // バッチ処理：未定義の変数は空に展開
	var out strings.Builder
	for _, seg := range strings.Split(cmdline, "&") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		out.WriteString(c.runOne(seg))
	}
	return out.String()
}

func (c *fakeCmd) runOne(seg string) string {
	args := tokenize(seg)
	var clean []string
	for _, a := range args {
		if a == "2>&1" {
			continue
		}
		clean = append(clean, a)
	}
	if len(clean) == 0 {
		return ""
	}
	switch strings.ToLower(clean[0]) {
	case "echo":
		return strings.Join(clean[1:], " ") + "\r\n"
	case "type":
		if len(clean) < 2 {
			return ""
		}
		b, ok := c.fs.readFile(clean[1])
		if !ok {
			return "The system cannot find the file specified.\r\n"
		}
		return string(b)
	case "certutil":
		// certutil -decode -f <in> <out>
		var in, out string
		for i := 1; i < len(clean); i++ {
			if strings.HasPrefix(clean[i], "-") {
				continue
			}
			if in == "" {
				in = clean[i]
			} else {
				out = clean[i]
			}
		}
		b, ok := c.fs.readFile(in)
		if !ok {
			return "CertUtil: The system cannot find the file specified.\r\n"
		}
		dec, err := tolerantB64Decode(string(b))
		if err != nil {
			return "CertUtil: -decode command failed.\r\n"
		}
		c.fs.writeFile(out, dec, false)
		return "CertUtil: -decode command completed successfully.\r\n"
	case "del":
		for _, a := range clean[1:] {
			if strings.HasPrefix(a, "/") {
				continue
			}
			c.fs.remove(a)
		}
		return ""
	case "whoami":
		return "nt authority\\network service\r\n"
	case "ver":
		return "Microsoft Windows [Version 10.0.20348.1]\r\n"
	}
	return "'" + clean[0] + "' is not recognized as an internal or external command\r\n"
}
