package db

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// capLLMRawBody:上限内はそのまま返し、超過時は切り詰めて行内マーカーを付け、切り詰め位置で UTF-8 を壊してはならない。
func TestCapLLMRawBody(t *testing.T) {
	small := strings.Repeat("a", 1024)
	if got := capLLMRawBody(small); got != small {
		t.Fatalf("small body changed (len %d)", len(got))
	}

	over := strings.Repeat("b", llmRawBodyCap+100)
	got := capLLMRawBody(over)
	if !strings.Contains(got, "truncated") {
		t.Fatalf("oversized body missing truncation marker")
	}
	if !strings.HasPrefix(got, strings.Repeat("b", 100)) {
		t.Fatalf("truncated body lost its prefix")
	}

	// マルチバイト文字が切り詰め位置をまたぐ場合も、結果は有効な UTF-8 でなければならない。
	mixed := strings.Repeat("中", llmRawBodyCap) // 3 バイト/文字なので必ず境界をまたぐ
	got = capLLMRawBody(mixed)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation broke UTF-8 validity")
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("mixed body missing truncation marker")
	}
}
