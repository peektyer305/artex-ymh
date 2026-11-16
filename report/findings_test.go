package report

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// readCSV は BOM を剥がし、標準 CSV として全レコードを解析する。
func readCSV(t *testing.T, raw []byte) [][]string {
	t.Helper()
	if !bytes.HasPrefix(raw, []byte("\xEF\xBB\xBF")) {
		t.Fatalf("missing UTF-8 BOM")
	}
	recs, err := csv.NewReader(bytes.NewReader(raw[len("\xEF\xBB\xBF"):])).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	return recs
}

// 正常なエクスポート：ヘッダー + 深刻度の降順の行。フィールドはそのままセルに入る。
func TestFindingsCSVNormal(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	fs := []*db.DBFinding{
		{ID: 2, Name: "通常の脆弱性", VulnClass: "xss", Severity: "low", Status: "confirmed", TaskDescription: "タスクB", Summary: "要約", CreatedAt: now},
		{ID: 1, Name: "重大な脆弱性", VulnClass: "sqli", Severity: "critical", Status: "pending", TaskDescription: "タスクA", Summary: "概要", CreatedAt: now.Add(-time.Hour), TrafficBindings: []db.FindingTrafficBinding{{ID: 7}, {ID: 9}}},
	}
	recs := readCSV(t, FindingsCSV(fs))
	if len(recs) != 3 {
		t.Fatalf("want 3 records (header + 2 rows), got %d", len(recs))
	}
	if recs[0][0] != "ID" || recs[0][9] != "トラフィックエビデンスID" {
		t.Fatalf("unexpected header: %v", recs[0])
	}
	// critical は low より前に並ぶ。
	if recs[1][0] != "1" || recs[1][1] != "重大な脆弱性" || recs[1][3] != "critical" {
		t.Fatalf("unexpected first data row: %v", recs[1])
	}
	if recs[1][8] != "2" || recs[1][9] != "7,9" {
		t.Fatalf("unexpected traffic columns: %v", recs[1])
	}
	if recs[2][0] != "2" || recs[2][1] != "通常の脆弱性" {
		t.Fatalf("unexpected second data row: %v", recs[2])
	}
}

// 数式インジェクション：= + - @ で始まる文字列フィールドは単引用符を前置する。
func TestFindingsCSVFormulaEscape(t *testing.T) {
	fs := []*db.DBFinding{{
		ID: 1, Name: "=cmd|'/c calc'!A1", VulnClass: "+1+2", Severity: "-1+1",
		Status: "-2+3", TaskDescription: "@SUM(1)", Summary: "=HYPERLINK(\"http://evil\")",
		CreatedAt: time.Now(),
	}}
	recs := readCSV(t, FindingsCSV(fs))
	row := recs[1]
	for _, col := range []int{1, 2, 3, 4, 5, 7} {
		if !strings.HasPrefix(row[col], "'") {
			t.Fatalf("col %d not quote-prefixed: %q", col, row[col])
		}
	}
	if row[1] != "'=cmd|'/c calc'!A1" {
		t.Fatalf("unexpected name cell: %q", row[1])
	}
	// 通常の値には誤って接頭辞を付けない。
	fs[0].Name = "正常な名前"
	recs = readCSV(t, FindingsCSV(fs))
	if recs[1][1] != "正常な名前" {
		t.Fatalf("plain value got prefixed: %q", recs[1][1])
	}
}

// カンマ/引用符を含むフィールドは CSV の引用規則で往復する。フィールド内の
// \r \n は必ず空白に置き換え、1 レコードが複数行に割れてはならない。
func TestFindingsCSVCommaQuoteNewline(t *testing.T) {
	fs := []*db.DBFinding{{
		ID: 1, Name: "a,b\"c", Severity: "high",
		Summary:   "1 行目\r\n2 行目\n3 行目\r4 行目",
		CreatedAt: time.Now(),
	}}
	recs := readCSV(t, FindingsCSV(fs))
	if len(recs) != 2 {
		t.Fatalf("record broken by embedded newline: got %d records", len(recs))
	}
	if recs[1][1] != "a,b\"c" {
		t.Fatalf("comma/quote field round-trip failed: %q", recs[1][1])
	}
	if strings.ContainsAny(recs[1][7], "\r\n") {
		t.Fatalf("summary still contains CR/LF: %q", recs[1][7])
	}
	// \r\n は 2 文字なのでそれぞれ空白 1 個に置換され、1 行目と 2 行目の間は空白 2 個になる。
	if recs[1][7] != "1 行目  2 行目 3 行目 4 行目" {
		t.Fatalf("unexpected sanitized summary: %q", recs[1][7])
	}
}

// 空リスト：BOM + ヘッダーのみ。
func TestFindingsCSVEmpty(t *testing.T) {
	recs := readCSV(t, FindingsCSV(nil))
	if len(recs) != 1 || recs[0][0] != "ID" {
		t.Fatalf("unexpected records for empty input: %v", recs)
	}
}
