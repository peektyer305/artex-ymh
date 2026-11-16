package server

// session_message_test.go は register_session が返すシェル種別ラベルと登録時の明示文言を
// カバーする(振り返り R1:コマンドシェルの登録成功時は登録済みであることを必ず明示すること、disable_functions
// のフィンガープリントとバイパス可否はありのまま報告すること)。純関数のテストで PG に依存しない。

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/session"
)

func TestShellTypeLabel(t *testing.T) {
	cases := []struct {
		info session.ProbeInfo
		want string
	}{
		{session.ProbeInfo{Lang: "php", Variant: "eval"}, "eval"},
		{session.ProbeInfo{Lang: "php", Variant: "assert"}, "eval"},
		{session.ProbeInfo{Lang: "phpcmd", Variant: "cmd"}, "cmd"},
		{session.ProbeInfo{Lang: "jsp"}, "jsp"},
		{session.ProbeInfo{Lang: "aspx"}, "aspx"},
		{session.ProbeInfo{Lang: "phpenc", Variant: "eval"}, "enc"},
		{session.ProbeInfo{Lang: "jspenc"}, "enc"},
	}
	for _, c := range cases {
		if got := shellTypeLabel(c.info); got != c.want {
			t.Errorf("shellTypeLabel(%+v) = %q, want %q", c.info, got, c.want)
		}
	}
}

func TestRegisterMessageCmdShellExplicit(t *testing.T) {
	msg := registerMessage(session.ProbeInfo{Lang: "phpcmd", Variant: "cmd"})
	if !strings.Contains(msg, "コマンド実行 webshell") {
		t.Fatalf("コマンド実行 webshell の登録は明示すべき, got %q", msg)
	}
}

func TestRegisterMessageEvalWithBypass(t *testing.T) {
	msg := registerMessage(session.ProbeInfo{
		Lang: "php", Variant: "eval", DisableFunctions: true, Bypass: "ld_preload",
	})
	if !strings.Contains(msg, "disable_functions") || !strings.Contains(msg, "LD_PRELOAD") {
		t.Fatalf("disable_functions とバイパス可否は明示すべき, got %q", msg)
	}
	msg = registerMessage(session.ProbeInfo{Lang: "php", Variant: "eval", DisableFunctions: true})
	if !strings.Contains(msg, "ファイル読み書きのみ") {
		t.Fatalf("バイパスがない場合は退化した境界を明示すべき, got %q", msg)
	}
	msg = registerMessage(session.ProbeInfo{Lang: "php", Variant: "eval"})
	if !strings.Contains(msg, "eval webshell") {
		t.Fatalf("正常な eval webshell の文言が異常, got %q", msg)
	}
}
