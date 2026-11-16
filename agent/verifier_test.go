package agent

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// C4: confirmed の脆弱性だけが「突破済み」とみなされる。pending のプレースホルダ/誤検知は prove_goal の
// 証拠にできず、確認済み脆弱性数にも数えない。独立した findings 行がない場合（""）は従来の挙動を保つ。
func TestProvenFinding(t *testing.T) {
	if !provenFinding("") {
		t.Error("empty status (no standalone row) should stay countable")
	}
	if !provenFinding(db.FindingConfirmed) {
		t.Error("confirmed should be countable")
	}
	for _, st := range []string{db.FindingPending, db.FindingFalsePositive, db.FindingFixed, db.FindingIgnored, db.FindingDuplicate} {
		if provenFinding(st) {
			t.Errorf("status %q should not count as a proven finding", st)
		}
	}
}
