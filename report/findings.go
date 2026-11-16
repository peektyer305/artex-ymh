package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ファインディング画面「エクスポート」用のレンダリング：findings テーブルの行を
// まとめ Markdown、1 件分の Markdown、CSV にレンダリングする。JSON は server 層が
// DTO を直接シリアライズするためここでは扱わない。

// sortFindingsForExport は深刻度の降順、次いで時刻の降順で並べ替える。まとめレポートのグループ分けと一致する。
func sortFindingsForExport(fs []*db.DBFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := sevRank[fs[i].Severity], sevRank[fs[j].Severity]
		if ri != rj {
			return ri < rj // sevRank は小さいほど深刻
		}
		return fs[i].CreatedAt.After(fs[j].CreatedAt)
	})
}

// findingTitle は脆弱性の可読タイトルを返す：名前 → カテゴリ → 「未分類」。
func findingTitle(f *db.DBFinding) string {
	return nz(f.Name, nz(f.VulnClass, "未分類"))
}

// FindingsMarkdown は findings をまとめレポートに整形する（要約 + 深刻度ごとのグループ、
// 各エントリにカテゴリ/ステータス/所属タスク/エビデンス/詳細レポート）。
func FindingsMarkdown(fs []*db.DBFinding, generatedAt time.Time) string {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var b strings.Builder
	b.WriteString("# ファインディングまとめレポート\n\n")
	fmt.Fprintf(&b, "- **生成日時**：%s\n", generatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **ファインディング総数**：%d 件\n\n", len(items))

	// 要約：各深刻度の件数。
	counts := map[string]int{}
	for _, f := range items {
		counts[f.Severity]++
	}
	b.WriteString("## 要約\n\n")
	b.WriteString("| 深刻度 | 件数 |\n| --- | --- |\n")
	for _, s := range []struct{ key, label string }{
		{"critical", "緊急"}, {"high", "高"}, {"medium", "中"}, {"low", "低"},
	} {
		fmt.Fprintf(&b, "| %s | %d |\n", s.label, counts[s.key])
	}
	b.WriteString("\n")

	if len(items) == 0 {
		b.WriteString("_該当する脆弱性はありません。_\n")
		return b.String()
	}

	b.WriteString("## 脆弱性の詳細\n\n")
	for i, f := range items {
		fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
		if f.VulnClass != "" {
			fmt.Fprintf(&b, "- **カテゴリ**：%s\n", f.VulnClass)
		}
		fmt.Fprintf(&b, "- **ステータス**：%s\n", nz(f.Status, "pending"))
		if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
			fmt.Fprintf(&b, "- **所属タスク**：%s\n", desc)
		}
		fmt.Fprintf(&b, "- **発見日時**：%s\n\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
		if s := strings.TrimSpace(f.Summary); s != "" {
			fmt.Fprintf(&b, "%s\n\n", s)
		}
		if e := strings.TrimSpace(f.Evidence); e != "" {
			fmt.Fprintf(&b, "**エビデンス：**\n\n```\n%s\n```\n\n", e)
		}
		if rep := strings.TrimSpace(f.Report); rep != "" {
			b.WriteString("**詳細レポート：**\n\n")
			b.WriteString(rep)
			b.WriteString("\n\n")
		}
		b.WriteString(findingTrafficMarkdown(f, false))
		b.WriteString("---\n\n")
	}
	return b.String()
}

// SingleFindingMarkdown は 1 件の脆弱性を独立した Markdown にレンダリングする（「1 脆弱性 1 ファイル」梱包用）。
func SingleFindingMarkdown(f *db.DBFinding, generatedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# [%s] %s\n\n", strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
	if f.VulnClass != "" {
		fmt.Fprintf(&b, "- **カテゴリ**：%s\n", f.VulnClass)
	}
	fmt.Fprintf(&b, "- **深刻度**：%s\n", nz(f.Severity, "info"))
	fmt.Fprintf(&b, "- **ステータス**：%s\n", nz(f.Status, "pending"))
	if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
		fmt.Fprintf(&b, "- **所属タスク**：%s\n", desc)
	}
	fmt.Fprintf(&b, "- **発見日時**：%s\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **生成日時**：%s\n\n", generatedAt.Format("2006-01-02 15:04:05"))
	if s := strings.TrimSpace(f.Summary); s != "" {
		fmt.Fprintf(&b, "## 概要\n\n%s\n\n", s)
	}
	if e := strings.TrimSpace(f.Evidence); e != "" {
		fmt.Fprintf(&b, "## エビデンス\n\n```\n%s\n```\n\n", e)
	}
	if rep := strings.TrimSpace(f.Report); rep != "" {
		b.WriteString("## 詳細レポート\n\n")
		b.WriteString(rep)
		b.WriteString("\n")
	}
	b.WriteString(findingTrafficMarkdown(f, true))
	return b.String()
}

var unsafeFilenameChars = regexp.MustCompile(`[^\p{Han}\p{L}\p{N}._-]+`)

// FindingFilename は「1 脆弱性 1 ファイル」用の安全な .md ファイル名を生成する。
// 例：`critical_SQLインジェクション_#123.md`。パス区切りと制御文字を除去し、
// zip 内の不正なパスを防ぐ。
func FindingFilename(f *db.DBFinding) string {
	sev := nz(f.Severity, "info")
	title := findingTitle(f)
	name := fmt.Sprintf("%s_%s_#%d", sev, title, f.ID)
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = fmt.Sprintf("finding_%d", f.ID)
	}
	// 防御的措置：パスをもう一段剥ぎ、zip slip を確実に防ぐ。
	name = path.Base(name)
	if len(name) > 120 {
		name = name[:120]
	}
	return name + ".md"
}

// csvSafe は CSV に書き出す文字列フィールドに 2 つの処理を行う：
//  1. 数式インジェクション対策：= + - @ で始まるセルは Excel/表計算ソフトが数式として
//     実行する（典型的な CSV injection の入口。脆弱性名/エビデンス要約はいずれも
//     対象側のデータ由来）。先頭に単引用符を付け、テキストとして解析させる;
//  2. フィールド内の \r \n を空白に置換し、セルの内容で 1 レコードが複数行に
//     割れないようにする（レコード分断インジェクション）。
func csvSafe(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@':
		return "'" + s
	}
	return s
}

// FindingsCSV は findings を CSV にレンダリングする（UTF-8 BOM 付き。Excel が
// 日本語を正しく認識できるようにするため）。report/エビデンスの全文は含めず、
// 要約系フィールドのみを出す。全文が必要なら Markdown/JSON でエクスポートする。
func FindingsCSV(fs []*db.DBFinding) []byte {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"ID", "名前", "カテゴリ", "深刻度", "ステータス", "所属タスク", "発見日時", "概要", "トラフィックエビデンス数", "トラフィックエビデンスID"})
	for _, f := range items {
		_ = w.Write([]string{
			fmt.Sprintf("%d", f.ID),
			csvSafe(findingTitle(f)),
			csvSafe(f.VulnClass),
			csvSafe(nz(f.Severity, "info")),
			csvSafe(nz(f.Status, "pending")),
			csvSafe(f.TaskDescription),
			f.CreatedAt.Format("2006-01-02 15:04:05"),
			csvSafe(strings.TrimSpace(f.Summary)),
			fmt.Sprint(len(f.TrafficBindings)), csvSafe(findingTrafficIDs(f)),
		})
	}
	w.Flush()
	return buf.Bytes()
}

func findingTrafficIDs(f *db.DBFinding) string {
	ids := make([]string, 0, len(f.TrafficBindings))
	for _, b := range f.TrafficBindings {
		ids = append(ids, fmt.Sprint(b.ID))
	}
	return strings.Join(ids, ",")
}

func findingTrafficMarkdown(f *db.DBFinding, attachments bool) string {
	stale := f.Report != "" && f.EvidenceVersion != f.ReportEvidenceVersion
	if len(f.TrafficBindings) == 0 && !stale {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n## 関連トラフィックエビデンス\n\n")
	fmt.Fprintf(&out, "エビデンスバージョン：%d；紐付け件数：%d。\n\n", f.EvidenceVersion, len(f.TrafficBindings))
	if stale {
		out.WriteString("エビデンスが変更されたため、詳細レポートは未更新です。\n\n")
	}
	for i, b := range f.TrafficBindings {
		fmt.Fprintf(&out, "%d. **エビデンス #%d · %s** — `%s %s`、ステータスコード %d\n", i+1, b.ID, b.Role, b.Snapshot.Method, strings.ReplaceAll(b.Snapshot.URL, "`", "%60"), b.Snapshot.Status)
		if b.Note != "" {
			fmt.Fprintf(&out, "   %s\n", strings.ReplaceAll(b.Note, "\n", "\n   "))
		}
		if attachments {
			fmt.Fprintf(&out, "   [リクエストメッセージ](evidence/%d/%d/request.http) · [レスポンスメッセージ](evidence/%d/%d/response.http)\n", f.ID, b.ID, f.ID, b.ID)
		}
	}
	out.WriteString("\n")
	return out.String()
}
