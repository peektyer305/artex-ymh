package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string when there are no constraints (or ts is nil). The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【操作制約（最高優先度。下記のあらゆる探索・攻撃面拡大のヒューリスティックに優先する。意図を1つ生成する前、動作を1つ実行する前に、必ず違反していないか自己点検し、違反するものは実施してはならない）】：")
	if len(allow) > 0 {
		b.WriteString("\n許可される操作：\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\n禁止される操作：\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n（制約の外にある新たな対象/新たなポート/新たなホストを発見しても、それは権限を取得したことにはなりません。上記の許可範囲に含まれない限り、out-of-scope の事実として記録してスキップし、それに対して意図を派生させたり動作を実行したりしてはなりません。）")
	return b.String()
}
