// toolscheck.go は起動時の外部ツール完全性セルフチェック(期 6 デプロイチェーン強化):server.New 時に
// 固定版マニフェスト(packaging/tools-manifest.json、または ARTEX_TOOLS_MANIFEST 指定)を読み、
// 存在するバイナリの sha256 を検証する(マニフェストで空でないもののみ)。マニフェストファイルがない場合(開発時)は黙ってスキップし、
// ツールの欠落は常態(最小構成では一部のみ導入)なので 1 行のサマリだけ出して画面を埋めない。ハッシュ不一致は警告のみで
// ブロックしない(ユーザーが自前ビルドした可能性がある)。詳細な項目別レポートは `artex doctor`。
package server

import (
	"log"
	"os"
	"strings"

	"github.com/Autumn-27/artex/tools"
)

// checkToolsManifest は起動時のセルフチェックを実行してログを出す。失敗してもログのみで、起動は止めない。
func checkToolsManifest(dataDir string) {
	m, err := tools.Load(tools.ManifestPath())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[tools] マニフェストの解析に失敗しました(セルフチェックをスキップ): %v", err)
		}
		return
	}
	var ok, unpinned, missing, mismatch []string
	for _, r := range tools.Check(m, dataDir) {
		switch r.Status {
		case tools.StatusOK:
			ok = append(ok, r.Entry.Name)
		case tools.StatusUnpinned:
			unpinned = append(unpinned, r.Entry.Name)
		case tools.StatusMismatch:
			mismatch = append(mismatch, r.Entry.Name)
			log.Printf("[tools] ⚠ %s のハッシュが一致しません(期待 %s、実際 %s)。自前ビルド/新版の可能性があります。警告のみでブロックしません",
				r.Entry.Name, r.Entry.SHA256, r.Actual)
		case tools.StatusMissing:
			missing = append(missing, r.Entry.Name)
		}
	}
	log.Printf("[tools] マニフェストセルフチェック：sha256 一致 %d、未固定 %d、欠落 %d、不一致 %d%s%s",
		len(ok), len(unpinned), len(missing), len(mismatch),
		toolsListNote("欠落", missing), toolsListNote("不一致", mismatch))
}

// toolsListNote はリストをサマリ行の接尾辞に組み立てる。空のリストは出力しない(画面を埋めない)。
func toolsListNote(label string, names []string) string {
	if len(names) == 0 {
		return ""
	}
	return ";" + label + ": " + strings.Join(names, ", ")
}
