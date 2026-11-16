package server

import "testing"

func TestFindingVerifyAutoEnabled(t *testing.T) {
	// デフォルト（未設定/未知の値）は auto。明示的な off のみ無効化する（イントラネット段階）。
	for _, mode := range []string{"", "auto", "AUTO", "whatever"} {
		if !findingVerifyAutoEnabled(mode) {
			t.Errorf("mode %q should keep auto-verify enabled", mode)
		}
	}
	if findingVerifyAutoEnabled("off") {
		t.Error("mode off should disable auto-verify")
	}
}
