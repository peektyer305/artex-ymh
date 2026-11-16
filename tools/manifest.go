// Package tools は外部ツールバイナリの固定版マニフェストと完全性セルフチェックを管理する（フェーズ6 デプロイチェーン強化）。
//
// プラットフォームは外部ツール群に依存する：トンネルツール（suo5/chisel、tunnel/ 参照）と武器庫の常用ツール
// （gogo/naabu/httpx/katana/fscan/impacket/…、通常 data/tools/ 下に置き、tools テーブルの
// shell 行でモデルに Bash から直接呼び出せると伝える）。マニフェスト（packaging/tools-manifest.json）は
// 各ツールの名前/用途/バージョン/プラットフォーム/sha256/相対パスを記録する。sha256 が空文字列なら「未固定」
// （正しいハッシュが不明で、空のままにする方が捏造よりまし）。起動時と `artex doctor` はここの
// Load/Check を共用する：存在するバイナリでハッシュが固定済みのものだけを検証し、不一致は警告のみでブロックしない
// （ユーザーが自前ビルドする場合がある）；マニフェストファイル自体の欠落（開発時）は呼び出し側が黙ってスキップする。
package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/config"
)

// Entry はマニフェスト内の外部ツール1件のレコード。
type Entry struct {
	Name     string `json:"name"`     // ツール名（一意）
	Purpose  string `json:"purpose"`  // 用途（1行）
	Version  string `json:"version"`  // 固定したバージョン；空 = 未固定
	Platform string `json:"platform"` // 対応プラットフォーム（例: linux/amd64;any = 制限なし）
	SHA256   string `json:"sha256"`   // 固定した sha256；空文字列 = 未固定（検証しない）
	Path     string `json:"path"`     // data ディレクトリからの相対パス（ファイルまたはディレクトリ）
	Env      string `json:"env"`      // 任意：パスを上書きする環境変数（例: ARTEX_SUO5_PATH）
	Category string `json:"category"` // tunnel（トンネル） | arsenal（武器庫）
}

// Manifest は tools-manifest.json の全体構造。
type Manifest struct {
	Tools []Entry `json:"tools"`
}

// ManifestPath はマニフェストファイルのパスを解決する：
//
//	env ARTEX_TOOLS_MANIFEST  >  BaseDir()/packaging/tools-manifest.json
//
// 返るパスは存在しない場合がある（開発時/簡易デプロイ）——呼び出し側は os.IsNotExist で判定して黙ってスキップする。
func ManifestPath() string {
	if v := strings.TrimSpace(os.Getenv("ARTEX_TOOLS_MANIFEST")); v != "" {
		return v
	}
	return filepath.Join(config.BaseDir(), "packaging", "tools-manifest.json")
}

// Load はマニフェストファイルを読み込んで解析する。ファイル欠落時の error は os.IsNotExist を満たす。
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Status は単一ツールのセルフチェック結果。
type Status int

const (
	StatusOK       Status = iota // 存在する；ハッシュが固定されていれば一致
	StatusUnpinned               // 存在するが、マニフェストにハッシュ未固定（存在のみ報告し、検証しない）
	StatusMissing                // 欠落（簡易環境では通常で、故障とは見なさない）
	StatusMismatch               // 存在するがハッシュがマニフェストと不一致（警告のみ、自前ビルドの可能性）
)

// CheckResult は単一ツールのセルフチェック結果。
type CheckResult struct {
	Entry  Entry
	Status Status
	Path   string // 実際に解決した絶対/相対パス（env による上書きを優先）
	Actual string // 実際の sha256（計算した場合のみ）
}

// Check はマニフェスト内の各ツールについて存在性と（ハッシュ固定済みなら）sha256 検証を行う。dataDir は data
// ディレクトリ；エントリに env があり環境変数が設定済みなら、環境変数で指定したパスを優先する（tunnel サブシステム
// の解決順と一致）。ディレクトリのエントリは存在性チェックのみ（ディレクトリに単一ハッシュは計算できない）。
func Check(m *Manifest, dataDir string) []CheckResult {
	if m == nil {
		return nil
	}
	out := make([]CheckResult, 0, len(m.Tools))
	for _, e := range m.Tools {
		out = append(out, CheckEntry(e, dataDir))
	}
	return out
}

// CheckEntry は単一のマニフェストエントリを検証する（Check 参照）。
func CheckEntry(e Entry, dataDir string) CheckResult {
	r := CheckResult{Entry: e, Status: StatusMissing}
	p := ""
	if e.Env != "" {
		p = strings.TrimSpace(os.Getenv(e.Env))
	}
	if p == "" {
		p = filepath.Join(dataDir, filepath.FromSlash(e.Path))
	}
	r.Path = p
	st, err := os.Stat(p)
	if err != nil {
		return r
	}
	want := strings.ToLower(strings.TrimSpace(e.SHA256))
	if st.IsDir() || want == "" {
		r.Status = StatusUnpinned
		return r
	}
	sum, err := fileSHA256(p)
	if err != nil {
		return r // 存在するが読めない場合は欠落扱い；実行時により明確なエラーが出る
	}
	r.Actual = sum
	if sum == want {
		r.Status = StatusOK
	} else {
		r.Status = StatusMismatch
	}
	return r
}

// fileSHA256 はファイルの sha256 を計算する（tunnel/deploy.go のサプライチェーンセルフチェックと同じ基準）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
