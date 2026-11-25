package main

import (
	"fmt"
	"os"
	"path/filepath"
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

	// 4. トンネルツール（suo5/chisel/payloads）：存在確認 +（一覧にハッシュが固定されている場合は）sha256
	{
		var parts []string
		worst := docOK
		for _, r := range results {
			if r.Entry.Category != "tunnel" {
				continue
			}
			parts = append(parts, doctorToolNote(r))
			if r.Status == tools.StatusMissing || r.Status == tools.StatusMismatch {
				worst = docWarn // トンネルツールの欠落はトンネル機能にのみ影響し、プラットフォームは停止しない
			}
		}
		check(worst, "トンネルツール", strings.Join(parts, ";"))
	}

	// 5. 武器庫ツールの存在状況を集計（存在 X/Y）
	{
		var present, missing []string
		for _, r := range results {
			if r.Entry.Category != "arsenal" {
				continue
			}
			if r.Status == tools.StatusMissing {
				missing = append(missing, r.Entry.Name)
			} else {
				present = append(present, r.Entry.Name)
			}
		}
		total := len(present) + len(missing)
		switch {
		case total == 0:
			check(docSkip, "武器庫ツール", "一覧が欠落しているため集計できません")
		case len(missing) == 0:
			check(docOK, "武器庫ツール", fmt.Sprintf("存在 %d/%d", len(present), total))
		default:
			check(docWarn, "武器庫ツール", fmt.Sprintf("存在 %d/%d（欠落: %s）。必要に応じて %s に揃えてください",
				len(present), total, strings.Join(missing, ", "), filepath.Join(*dataDir, "tools")))
		}
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
