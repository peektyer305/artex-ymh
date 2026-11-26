package session

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// phpMock handles only the PHP fragments used by the HTTP shell protocol tests.
// Commands and files stay in fakeFS; it never invokes PHP, a shell, or a loader.
type phpMock struct {
	mu                            sync.Mutex
	fs                            *fakeFS
	variant                       string
	disabled                      map[string]bool
	mailOn, putenvOn, bypassWorks bool
}

func newPHPMock(variant string) *phpMock {
	return &phpMock{fs: newFakeFS(), variant: variant, disabled: map[string]bool{}}
}

func (m *phpMock) disable(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disabled[name] = true
}

func probeURL(t *testing.T, handler http.Handler) string {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s.URL
}

func (m *phpMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Powered-By", "PHP/8.3")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, _ = fmt.Fprint(w, m.eval(r.PostForm.Get("cmd")))
}

var (
	phpMockEcho    = regexp.MustCompile(`echo\s+("(?:\\.|[^"\\])*")`)
	phpMockLiteral = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)
	phpMockB64     = regexp.MustCompile(`base64_decode\("([A-Za-z0-9+/=]*)"\)`)
	phpMockExec    = regexp.MustCompile(`if\(function_exists\("([a-z_]+)"\)\)`)
	phpMockSeek    = regexp.MustCompile(`fseek\(\$h,(\d+)\)`)
	phpMockRead    = regexp.MustCompile(`fread\(\$h,(\d+)\)`)
)

func (m *phpMock) eval(code string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Contains(code, `.(20+22).`) {
		literals := phpMockLiteral.FindAllString(code, -1)
		if len(literals) != 2 {
			return "unsupported PHP probe"
		}
		start, _ := strconv.Unquote(literals[0])
		end, _ := strconv.Unquote(literals[1])
		return start + "42" + end
	}
	echoes := phpMockEcho.FindAllStringSubmatch(code, -1)
	if len(echoes) < 2 {
		return "unsupported PHP fragment"
	}
	start, _ := strconv.Unquote(echoes[0][1])
	end, _ := strconv.Unquote(echoes[len(echoes)-1][1])
	wrap := func(out string) string { return start + out + end }
	var decoded [][]byte
	for _, match := range phpMockB64.FindAllStringSubmatch(code, -1) {
		value, err := base64.StdEncoding.DecodeString(match[1])
		if err != nil {
			return wrap("invalid base64")
		}
		decoded = append(decoded, value)
	}
	switch {
	case strings.Contains(code, "debug_backtrace"):
		return wrap(m.variant)
	case strings.Contains(code, "foreach(array("):
		var available []string
		for _, name := range phpExecFuncs {
			if !m.disabled[name] {
				available = append(available, name)
			}
		}
		if m.mailOn {
			available = append(available, "mail")
		}
		if m.putenvOn {
			available = append(available, "putenv")
		}
		return wrap(strings.Join(available, ","))
	case strings.Contains(code, "file_put_contents") && len(decoded) == 2:
		m.fs.writeFile(string(decoded[0]), decoded[1], strings.Contains(code, "FILE_APPEND"))
		return wrap("WRITEOK")
	case strings.Contains(code, "BYPASSOK") && len(decoded) == 2:
		if !m.mailOn || !m.putenvOn {
			return wrap("NOBYPASS")
		}
		blob, ok := m.fs.readFile(string(decoded[0]))
		i := bytes.Index(blob, []byte(ldpreloadMagic)) + len(ldpreloadMagic)
		if !m.bypassWorks || !ok || i < len(ldpreloadMagic) || len(blob) < i+ldpreloadPathLen {
			return wrap("BYPASSFAIL")
		}
		marker := strings.TrimRight(string(blob[i:i+ldpreloadPathLen]), "\x00")
		if marker != string(decoded[1]) {
			return wrap("BYPASSFAIL")
		}
		return wrap("BYPASSOK")
	case strings.Contains(code, "fopen") && len(decoded) == 1:
		data, ok := m.fs.readFile(string(decoded[0]))
		seek, read := phpMockSeek.FindStringSubmatch(code), phpMockRead.FindStringSubmatch(code)
		if !ok || len(seek) != 2 || len(read) != 2 {
			return wrap("READFAIL")
		}
		offset, _ := strconv.Atoi(seek[1])
		size, _ := strconv.Atoi(read[1])
		offset = min(offset, len(data))
		return wrap(base64.StdEncoding.EncodeToString(data[offset:min(offset+size, len(data))]))
	case strings.Contains(code, "@unlink") && len(decoded) == 1:
		m.fs.remove(string(decoded[0]))
		return wrap("")
	case strings.Contains(code, "$c=base64_decode") && len(decoded) == 1:
		fn := phpMockExec.FindStringSubmatch(code)
		if len(fn) != 2 || m.disabled[fn[1]] {
			return wrap("NOFUNC")
		}
		out, _ := m.fs.run(string(decoded[0]))
		return wrap(out)
	default:
		return wrap("unsupported PHP fragment")
	}
}
