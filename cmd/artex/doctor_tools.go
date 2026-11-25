package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Autumn-27/artex/tools"
)

// checkDoctorTools reports the tool checks in their original order.
func checkDoctorTools(dataDir *string, check func(doctorStatus, string, string)) {
	// 4/5. 外部ツール一覧：トンネルツールは個別に、武器庫は存在状況をまとめて確認
	manifest, mErr := tools.Load(tools.ManifestPath())
	if mErr != nil && !os.IsNotExist(mErr) {
		check(docWarn, "ツール一覧", "解析に失敗しました（ツールチェックをスキップ）: "+mErr.Error())
	}
	var results []tools.CheckResult
	if manifest != nil {
		results = tools.Check(manifest, *dataDir)
	} else if os.IsNotExist(mErr) {
		// 一覧が欠落している場合（開発時/最小構成デプロイ）:トンネルツールは実行時と同じ解決順（env > data/tools）でフォールバックして存在確認する。
		for _, name := range []string{"suo5", "chisel"} {
			env := "ARTEX_" + strings.ToUpper(name) + "_PATH"
			results = append(results, tools.CheckEntry(tools.Entry{
				Name: name, Path: "tools/" + name, Env: env, Category: "tunnel",
			}, *dataDir))
		}
		results = append(results, tools.CheckEntry(tools.Entry{
			Name: "suo5-payloads", Path: "tools/suo5-payloads", Env: "ARTEX_SUO5_PAYLOADS_DIR", Category: "tunnel",
		}, *dataDir))
		check(docWarn, "ツール一覧", "packaging/tools-manifest.json が欠落しています。トンネルツールは既定パスで確認し、武器庫は集計できません")
	}

	// Tool status classification is shared with the startup check.
	{
		summary := tools.Summarize(results, "tunnel")
		var parts []string
		for _, result := range results {
			if result.Entry.Category == "tunnel" {
				parts = append(parts, doctorToolNote(result))
			}
		}
		switch {
		case summary.Total() == 0:
			check(docSkip, "トンネルツール", skippedToolGroupNote(mErr))
		case summary.HasWarnings():
			check(docWarn, "トンネルツール", strings.Join(parts, ";"))
		default:
			check(docOK, "トンネルツール", strings.Join(parts, ";"))
		}
	}

	{
		summary := tools.Summarize(results, "arsenal")
		if summary.Total() == 0 {
			check(docSkip, "武器庫ツール", skippedToolGroupNote(mErr))
		} else {
			status := docOK
			note := fmt.Sprintf("存在 %d/%d", summary.Present(), summary.Total())
			if len(summary.Missing) > 0 {
				note += fmt.Sprintf("（欠落: %s）", strings.Join(summary.Missing, ", "))
			}
			if len(summary.Mismatch) > 0 {
				note += fmt.Sprintf("（ハッシュ不一致: %s）", strings.Join(summary.Mismatch, ", "))
			}
			if len(summary.Unknown) > 0 {
				note += fmt.Sprintf("（不明な状態: %s）", strings.Join(summary.Unknown, ", "))
			}
			if summary.HasWarnings() {
				status = docWarn
			}
			check(status, "武器庫ツール", note)
		}
	}

}

func skippedToolGroupNote(err error) string {
	switch {
	case os.IsNotExist(err):
		return "一覧ファイルが見つからず、この項目は確認できませんでした"
	case err != nil:
		return "一覧ファイルを読み込めず、この項目は確認できませんでした"
	default:
		return "一覧ファイルにこの分類の項目がありません"
	}
}

// doctorToolNote は 1 つのツールのチェック結果を描画する（doctor レポート用）。
func doctorToolNote(r tools.CheckResult) string {
	switch r.Status {
	case tools.StatusOK:
		return fmt.Sprintf("%s OK（sha256 一致、%s）", r.Entry.Name, r.Path)
	case tools.StatusUnpinned:
		return fmt.Sprintf("%s は存在します（sha256 未固定、%s）", r.Entry.Name, r.Path)
	case tools.StatusMismatch:
		return fmt.Sprintf("%s のハッシュが一致しません（期待値 %s、実際 %s。自己ビルドの可能性があるため警告のみ）", r.Entry.Name, r.Entry.SHA256, r.Actual)
	default:
		return fmt.Sprintf("%s が見つかりません（期待値 %s）", r.Entry.Name, r.Path)
	}
}
