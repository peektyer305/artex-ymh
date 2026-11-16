package db

import "testing"

func TestMergeHoneypotEvidence(t *testing.T) {
	// 旧値が空 + 新しいエビデンス
	got := MergeHoneypotEvidence("", []string{"a(X, 0.90)"})
	if got != `["a(X, 0.90)"]` {
		t.Fatalf("from empty: %q", got)
	}
	// マージして重複を除き、順序を保つ
	got = MergeHoneypotEvidence(`["a(X, 0.90)"]`, []string{"a(X, 0.90)", "b(Y, 0.85)"})
	if got != `["a(X, 0.90)","b(Y, 0.85)"]` {
		t.Fatalf("dedup merge: %q", got)
	}
	// 旧値が壊れている場合は旧値なしとして扱い、新しいエビデンスを失わない
	got = MergeHoneypotEvidence("garbage", []string{"a(X, 0.90)"})
	if got != `["a(X, 0.90)"]` {
		t.Fatalf("corrupt old: %q", got)
	}
	// 両方とも空
	if got := MergeHoneypotEvidence("", nil); got != "" {
		t.Fatalf("both empty: %q", got)
	}
}
