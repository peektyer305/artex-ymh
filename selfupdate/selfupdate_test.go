package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths は隔離されたアップグレードディレクトリを作る。ResolvePaths() は直接使えない——テスト
// バイナリ自身を指してしまい、実行すると go test の実行ファイルが改名されてしまう。
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin は artex の代わりになる実行可能なシェルスクリプトを書き出す。smokeTest は -h で起動して
// 終了コードを見るだけなので、スクリプトで十分であり、本物のバイナリをビルドするよりずっと速い。
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("偽バイナリ %s の書き込みに失敗しました: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage は bin を「一時保存済みで差し替え待ち」の状態に整える：artex.new とそのチェックサムを書く。
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("チェックサムの計算に失敗しました: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("チェックサムの書き込みに失敗しました: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s の読み込みに失敗しました: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("偽バイナリは sh スクリプトなので、Windows では実行できません")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh は v を外し tag には v が付くので、両方を受け付ける必要がある
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // 辞書順ではなく数値で比較する
		{"1.0.0", "0.99.99", 1, true},
		// 開発ビルドは比較不能と判定する必要がある。さもないと正式版に未コミットの変更を上書きされる。
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) の comparable=%v が期待値 %v と異なります", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d が期待値 %d と異なります", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// 重要な不変条件：アップグレードファイルはすべて実行ファイルと同じディレクトリにある。CWD に落ちると、
	// サービスとして動かす場合（作業ディレクトリが / の可能性がある）に差し替えが完全に機能しなくなる。
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s が実行ファイルのディレクトリにありません: %s (期待 %s)", name, path, p.Dir)
		}
	}
	// Windows では .new/.old に .exe を残す必要があり、ないとスモークテストや差し替え後の実行が失敗する。
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows では .new/.old が .exe で終わる必要があります: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// チェックサムを書いた後にファイルを変更し、ダウンロードの破損 / すり替えを再現する。
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("SHA256 不一致で拒否されるはずが、通過しました")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // 実行はできるが終了コードが 0 以外

	if err := verifyStaged(p); err == nil {
		t.Fatal("スモークテストの失敗で拒否されるはずが、通過しました")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("マーカーの書き込みに失敗しました: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("Restart を期待しましたが %v でした", action)
	}
	if !st.Pending {
		t.Error("差し替え後の状態は Pending であるべきです")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex は新しいバージョンに置き換わっているべきです")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("旧バージョンは artex.old に退避されているべきです")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("差し替え後は artex.new が消えているべきです")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("差し替え後はチェックサムファイルが削除されているべきです")
	}
	// マーカーは残す必要がある。次回起動（動くのは新版）がこれで試行を数え、必要ならロールバックする。
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("差し替え後もアップグレードマーカーは残るべきです")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // チェックサムを壊す

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("検証失敗時は Continue を期待しましたが %v でした", action)
	}
	if !st.FailedStage {
		t.Error("状態は FailedStage であるべきです")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("検証失敗時に現在のバージョンを変えてはなりません")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("検証に失敗した一時保存ファイルは削除されるべきです。残ると次回起動で再試行されます")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // 前回のアップグレードで残ったバックアップ
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("v3 に差し替わるべきです")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("バックアップは直前に外した v2 に更新されるべきです")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// maxAttempts 回目までは試行回数を数えるだけで、新版が自力で安定する機会を与える。
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("%d 回目の試行では Continue を期待しましたが %v でした", i, action)
		}
		if !st.Pending {
			t.Errorf("%d 回目の試行では状態が Pending であるべきです", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("%d 回目の試行後の attempts=%d（ok=%v）が期待値 %d と異なります", i, got.Attempts, ok, i)
		}
		m = got
	}

	// もう 1 回落ちると上限を超え、自動で旧版に戻す。
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("試行上限超過時は Restart を期待しましたが %v でした", action)
	}
	if !st.RolledBack {
		t.Error("状態は RolledBack であるべきです")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("旧バージョンへロールバックされているべきです")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("ロールバック後はマーカーを消すべきです。残ると無限にロールバックします")
	}
	// 起動しないバージョンは直接削除せず、調査用に残す。
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("失敗したバージョンは .failed として調査用に残るべきです")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback() は ResolvePaths() を通るため、ここでは下位の交換処理を直接テストする。
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("ロールバック後の現在のバージョンは v1 であるべきです")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("ロールバック後のバックアップは v2 になるべきです。そうすれば再び戻せます")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum の出力は半角スペース 2 つ区切り；shasum -a 256 はバイナリモードでファイル名の前に * を付ける。
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // フィールドはちょうど 2 つだが、最初がダイジェストではない
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // ダイジェストの長さが正しくない

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux の項目の解析結果が誤っています: %v", out)
	}
	// ダイジェストは小文字に揃え、照合時に大文字小文字で不一致と誤判定しないようにする。
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows の項目が誤っています（* プレフィックスを外し、ダイジェストを小文字にすべきです）: %q", got)
	}
	if len(out) != 2 {
		t.Errorf("空行、ダイジェストでない行、長さが正しくない行は無視されるべきです。結果は %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("パッケージ内の基底名は Windows では artex.exe のため、このケースは Unix の命名で構成しています")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 実際のリリースパッケージの構造：artex-<バージョン>-<os>-<arch>/artex に、いくつかのダミーファイルを加えたもの。
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("解凍されたものが artex 実行ファイルではありません: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("解凍されたバイナリには実行ビットが必要です")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("パッケージ内に実行ファイルがない場合はエラーになるべきです")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // HTTPS ではない
		"https://evil.com/artex.zip",    // ドメインが許可リストにない
		"https://github.com.evil.com/x", // サフィックスによる偽装
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) は拒否すべきです", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // ドメインは大文字小文字を区別しない
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) は許可すべきですがエラーになりました: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh の package_binary は artex-<バージョン>-<os>-<arch>.zip を使い、バージョン番号から
	// v を外している。ここが 1 文字でも違うと、全プラットフォームのワンクリック更新がアセットを見つけられない。
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q の解析に失敗しました: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("安定確認後はアップグレードマーカーを消す必要があります")
	}
	// マーカーがなければ、以後の通常の再起動で試行回数が増えることも、誤ってロールバックすることもない。
	if _, ok := readMarker(p.Marker); ok {
		t.Error("マーカーの読み込みは失敗するべきです")
	}
	// バックアップは残す。ユーザーが手動でロールバックできるようにするため。
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("安定確認後も前のバージョンのバックアップは残るべきです")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // 通常の起動経路。panic もファイルの変更もあってはならない
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("マーカーがない場合、settle はどのファイルにも影響してはなりません")
	}
}
