// Package selfupdate は ARTEX の画面からのワンクリック更新を実装する：GitHub Release から新版の
// バイナリを取得し、検証、一時保存して、次回起動時にアトミックに差し替える。
//
// 全体の分担（start.sh / start.bat を参照）：
//
//	起動スクリプト = 単純な監視ループで、「プロセス終了後に終了コードで再起動するかを決める」だけを担う
//	本パッケージ   = 間違えやすい処理のすべて（ダウンロード / SHA256 検証 / スモークテスト / 差し替え / 失敗時のロールバック）
//
// 差し替えをスクリプトではなく Go に置いているのは、sha256 検証とスモークテストを sh と bat で
// 二通り（sha256sum / shasum / certutil）書く必要があり、しかもそこが最も誤りを許されない箇所だから。
// 動かないバイナリに差し替えると、監視プロセスは忠実にそれを起動し続け、ユーザーは手作業で復旧するしかない。
//
// 1 回の完全なアップグレードは 3 回のプロセス起動を経る：
//
//	① 旧版 server が /api/update/apply を受信 → ダウンロードと検証 → artex.new に一時保存 → exit 75
//	② スクリプトが旧版を再起動 → Bootstrap が artex.new を検出 → 検証+スモークテスト → 差し替え → exit 75
//	③ スクリプトが再起動、この時点で新版 → Bootstrap が試行を 1 回記録 → 起動成功後にマーカーを消す
//
// どの段階で失敗しても旧版に戻す：② 検証に通らなければ一時保存したものを削除して旧版を動かし続ける；
// ③ 3 回連続でマーカー消去まで至らない（起動できずに落ちる）なら artex.old を自動で戻す。
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart は「監視プロセスに自分を再起動してほしい」という終了コード（EX_TEMPFAIL）。起動スクリプトは
// これを見ると直ちに再実行し、クラッシュのバックオフには数えない。0 はユーザーの正常停止（スクリプトはループを抜ける）、それ以外はクラッシュとみなす。
const ExitRestart = 75

// maxAttempts は差し替え後に許される起動試行回数。新版は起動のたびにカウントを +1 し、
// settleDelay を生き延びればマーカーを消す；maxAttempts 回連続で落ちるなら新版は起動できず、自動ロールバックする。
const maxAttempts = 3

// Paths は 1 回のアップグレードに関わる全ファイルで、**実行ファイルのあるディレクトリ**の下にまとめて置く。
// あえて CWD を使わない：サービスとして動かすと作業ディレクトリが / や任意のパスになりうるため、
// CWD を使うと一時保存ファイルが別の場所に落ちて差し替えロジックが機能しなくなる。
type Paths struct {
	Dir     string // 実行ファイルのあるディレクトリ
	Current string // 現在実行中のバイナリ        artex      / artex.exe
	New     string // 一時保存した新バージョン    artex.new  / artex.new.exe
	Sum     string // 新バージョンの sha256（hex）  artex.new.sha256 / artex.new.exe.sha256
	Old     string // 差し替え前に退避した旧バージョン      artex.old  / artex.old.exe
	Marker  string // アップグレード状態のマーカー            artex.upgrade.json
}

// ResolvePaths は現在の実行ファイルから全アップグレードパスを導出する。
//
// Windows では .new/.old にも .exe が必要で、ないとスモークテストや差し替え後の実行が失敗する。
// そのため先にサフィックスを外してから連結し、両プラットフォームの命名を揃える。
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("実行ファイルの特定に失敗しました: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows では ".exe"、Unix では通常は空
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker は 1 回の差し替えの進捗を記録し、新版が起動しないときに自動ロールバックを起こすために使う。
type marker struct {
	From     string `json:"from"`     // アップグレード前のバージョン
	To       string `json:"to"`       // 対象バージョン
	Attempts int    `json:"attempts"` // 差し替え後に起動を試みた回数
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged は一時保存したファイルを消す。差し替え成功、検証失敗、ユーザーによるキャンセルのいずれも
// これを通し、残った artex.new が次回起動時に再試行されるのを防ぐ。
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions は 2 つのバージョン番号を比較し、-1/0/1（a<b / a==b / a>b）を返す。
// ok=false は少なくとも一方が比較可能なバージョン番号でないことを示す（例：ローカル開発ビルドの "dev" や
// git describe が出力する "0.3.7-2-gabc1234-dirty"）。この場合、呼び出し側はワンクリック更新を無効にすべきで、
// さもないと開発中のビルドを正式版へ「アップグレード」して未コミットの変更を上書きしてしまう。
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion は "v0.3.7" / "0.3.7" 形式のバージョン番号を [3]int に解析する。
//
// 受け付けるのは純粋な 3 段式のみ：build.sh は tag でないビルドで git describe を使い
// "0.3.7-2-gabc1234" のようなサフィックス付きのバージョンを出力するが、これらは 0.3.7 ではなく
// 比較不能と判定しなければならない。さもないと開発ビルドが「最新」と誤判定されたり、正式版に上書きされたりする。
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker はプロセスがコンテナ内で動いているかを報告する。Docker では差し替えはコンテナの書き込み可能
// レイヤに書かれ、`docker compose up -d` でコンテナを作り直すとイメージ同梱のバージョンに戻る。これは想定
// どおりの動作（そのときユーザーは新しいイメージを取得している）だが、フロントエンドがそれに基づいて説明できる必要がある。
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
