package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset は release.yml が生成するチェックサム一覧で、Release 内のすべての zip を対象とする。
const sumsAsset = "SHA256SUMS"

// maxBinarySize は解凍後のバイナリサイズを制限し、壊れた zip がディスクを埋め尽くすのを防ぐ。
const maxBinarySize = 512 << 20 // 512 MiB

// Phase はアップグレード過程の段階で、SSE イベントの phase フィールドとしてそのまま使う。
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress は呼び出し側が提供し、進捗をフロントエンドへ伝えるために使う。pct はダウンロード段階でのみ
// 意味を持ち（0-100）、それ以外の段階では -1 を渡す。
type Progress func(ph Phase, pct int, msg string)

// Stage は指定 Release の現行プラットフォーム向けパッケージをダウンロードし、検証後に新バイナリを artex.new として一時保存する。
//
// 裸のバイナリではなく完全な zip を使う理由は 2 つ：現行 Release の SHA256SUMS がもともと zip しか
// 対象にしておらず、zip 経由なら CI を変えずに、公開済みの過去バージョンとも互換を保てる；zip には
// skills/ も含まれ、将来組み込みスキルを同期する余地が残る。代償は skills の数百 KB を余分にダウンロードするだけ。
//
// 関数が返れば一時保存は完了しており、呼び出し側はその後グレースフルシャットダウンして ExitRestart で終了する。
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("このバージョンには %s/%s 向けのパッケージがありません（%s が欠落）", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "チェックサム一覧を取得中…")
	sums, sumsRaw, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}

	// 一覧を信じる前にまず署名を検証する：SHA256SUMS 自体も同じ Release からダウンロードするもので、
	// 署名の裏付けがなければ、zip を改ざんした攻撃者がダイジェストを 1 行書き換えるだけでハッシュ検証を回避できる。
	prog(PhaseVerify, -1, "リリース署名を検証中…")
	if err := verifySumsSignature(ctx, c, rel, sumsRaw, prog); err != nil {
		return err
	}

	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s に %s が含まれていないため、未検証のバイナリのインストールを拒否します", sumsAsset, name)
	}

	// 一時ファイルはすべて対象ディレクトリ内に置き、最後の rename が同一ファイルシステム内の
	// アトミック操作になるようにする（デバイスをまたぐ rename は失敗し、/tmp は独立したマウントポイントであることが多い）。
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("%s をダウンロード中（%s）…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 を検証中…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 が一致しません：期待 %s、実際 %s（ダウンロードの破損または改ざん）", short(want), short(got))
	}

	prog(PhaseExtract, -1, "解凍してスモークテスト中…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("新バージョンが現在のシステムで実行できません: %w", err)
	}

	// 一時保存ファイル自身の sha256 を別途保存する：次回起動の差し替え前にもう一度検証し、
	// 一時保存から再起動までの間にファイルが変更されたり壊れたりするのを防ぐ。
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("新しいバイナリのチェックサム計算に失敗しました: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("チェックサムの書き込みに失敗しました: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("新バージョンの一時保存に失敗しました: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// マーカーは自動ロールバック能力にしか影響せず、一時保存自体は完了しているため、これでアップグレードを中断しない。
		prog(PhaseStaged, -1, "警告：アップグレードマーカーの書き込みに失敗したため、今回のアップグレードには自動ロールバックの保護がありません")
	}

	prog(PhaseStaged, 100, "新バージョンの準備ができました。再起動中…")
	return nil
}

// fetchSums は SHA256SUMS をダウンロードして解析し、ファイル名 → 16 進ダイジェストと、ファイルの生
// バイト列を返す（署名検証は元のままハッシュする必要があり、解析後の map から組み直してはならない）。
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, []byte, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, nil, fmt.Errorf("この Release には %s がないため完全性を検証できず、アップグレードを拒否します", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("%s のダウンロードに失敗しました: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("%s の読み込みに失敗しました: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%s の内容が空か、形式を認識できません", sumsAsset)
	}
	return out, raw, nil
}

// parseSums は sha256sum 形式の一覧を解析し、ファイル名 → 16 進ダイジェストを返す。
//
// 最初のフィールドが 64 桁の 16 進数でなければ採用しない。「ちょうど 2 フィールド」という判定だけでは
// 不十分で、2 語からなる説明文の行まで正当な項目として扱われ、ゴミ値がダイジェスト表に入り、本来のアセットが
// 誤ったダイジェストに一致してしまうおそれがある。
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 形式は "<sha256>  <filename>"（sha256sum は半角スペース 2 つ；shasum のバイナリ
		// モードはファイル名の前に * を付ける）。
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download はアセットを dst に書き込み、同時に SHA256 を計算し、Content-Length に基づいて進捗を報告する。
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("%s のダウンロードに失敗しました: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("一時ファイルの作成に失敗しました: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("ダウンロードが中断されました: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("ディスクへの書き込みに失敗しました: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("ダウンロードが不完全です：期待 %d バイト、実際 %d バイト", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get は許可リストで制約された GET を送信し、レスポンスボディを返す。
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary はリリースパッケージから artex 実行ファイルを取り出す。
//
// パッケージ内の構造は artex-<バージョン>-<os>-<arch>/artex だが、ここでは完全なパスを組み立てずに
// **基底名**で照合する：バージョン番号はパッケージ名に 1 度現れるだけで、1 文字誤ればアップグレード全体が
// 失敗するため、基底名で探すほうが変更に強い。
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("リリースパッケージのオープンに失敗しました: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%s の読み込みに失敗しました: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("新しいバイナリの書き出しに失敗しました: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("%s の解凍に失敗しました: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("リリースパッケージ内の実行ファイルが %s を超えているため、解凍を拒否します", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("リリースパッケージ内の %s が空ファイルです", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("リリースパッケージ内に %s が見つかりません", want)
}

// checkWritable は事前にディレクトリの書き込み可否を確認する。これがないと、非 root で実行している場合や
// バイナリがシステムディレクトリにある場合に、数十 MB をダウンロードし終えてから差し替えの瞬間に失敗する。
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("プログラムディレクトリ %s に書き込めないため自動更新できません（権限を確認するか手動アップグレードに切り替えてください）: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter は書き込み済みバイト数を集計し、頻度を制限して報告する。32KiB のチャンクごとに SSE を送るのを避ける。
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("ダウンロード中 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
