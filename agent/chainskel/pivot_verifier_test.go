package chainskel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifierChecklistGeneralAlways(t *testing.T) {
	for _, v := range []string{"", "見たことのない種類の脆弱性", "weird-vuln"} {
		out := VerifierChecklist(v)
		if !strings.Contains(out, "反証判定 checklist") {
			t.Fatalf("vulnclass=%q 缺固定段头:\n%s", v, out)
		}
		for _, want := range []string{"共通判定基準", "200 ≠ 突破", "想定する特徴へアンカーする", "陰性 ≠ 脆弱性なし"} {
			if !strings.Contains(out, want) {
				t.Fatalf("vulnclass=%q 通用层缺 %q:\n%s", v, want, out)
			}
		}
		if strings.Contains(out, "専用判定基準") {
			t.Fatalf("vulnclass=%q 未ヒットのカテゴリに専用判定基準があってはならない:\n%s", v, out)
		}
	}
}

func TestVerifierChecklistClasses(t *testing.T) {
	cases := []struct {
		vulnclass string
		class     string
		want      string // 専用判定基準里的锚点词
	}{
		{"SQL インジェクション", "sqli", "真偽の分岐"},
		{"sqli (time-based)", "sqli", "ベースライン"},
		{"XSS 反射型", "xss", "エコー ≠ 実行可能"},
		{"クロスサイトスクリプティング", "xss", "エコー"},
		{"SSRF", "ssrf", "送信元 IP"},
		{"コマンドインジェクション", "cmdi", "線形に対応"},
		{"リモートコマンド実行(RCE)", "cmdi", "線形"},
		{"ファイルアップロード", "upload", "設置+解析"},
		{"権限外アクセス", "auth-bypass", "2 アカウント"},
		{"IDOR", "auth-bypass", "2 アカウント"},
		{"機密情報の漏えい", "info-leak", "真実性"},
	}
	for _, c := range cases {
		out := VerifierChecklist(c.vulnclass)
		if !strings.Contains(out, "→ "+c.class) {
			t.Fatalf("vulnclass=%q は %s に帰一すべき:\n%s", c.vulnclass, c.class, out)
		}
		if !strings.Contains(out, c.want) {
			t.Fatalf("vulnclass=%q 専用判定基準缺 %q:\n%s", c.vulnclass, c.want, out)
		}
		if !strings.Contains(out, "共通判定基準") {
			t.Fatalf("vulnclass=%q 共通判定基準は常に存在すべき:\n%s", c.vulnclass, out)
		}
	}
	// 順序に敏感:コマンドインジェクションが sqli の「インジェクション」系アンカーに食われない。
	if out := VerifierChecklist("コマンドインジェクション"); !strings.Contains(out, "→ cmdi") {
		t.Fatalf("コマンドインジェクションは sqli ではなく cmdi に帰一すべき:\n%s", out)
	}
}

func TestPivotHintsByTags(t *testing.T) {
	out := PivotHints([]string{"web"}, "", 3)
	if out == "" {
		t.Fatal("web tag は方向転換エントリを抽出すべき")
	}
	if !strings.Contains(out, "[web] ") {
		t.Fatalf("方向転換エントリはカテゴリマークを伴うべき:\n%s", out)
	}
	if !strings.HasPrefix(out, "同種シナリオで選べる方向転換パス") {
		t.Fatalf("ガイド行が欠けている:\n%s", out)
	}
	if total := strings.Count(out, "\n- ["); total != 3 {
		t.Fatalf("max=3 はちょうど 3 つ取るべき, got %d:\n%s", total, out)
	}
	// 内容はスケルトンの「方向転換:」以降の原文(web.md の先頭)から来る。
	if !strings.Contains(out, "分岐なしなら時間ベースブラインド/OOB") {
		t.Fatalf("スケルトンの方向転換原文を抽出すべき:\n%s", out)
	}
}

func TestPivotHintsMaxCap(t *testing.T) {
	out := PivotHints([]string{"web"}, "", 1)
	if total := strings.Count(out, "\n- ["); total != 1 {
		t.Fatalf("max=1 は 1 つだけ取るべき, got %d:\n%s", total, out)
	}
	if PivotHints([]string{"web"}, "", 0) != "" {
		t.Fatal("max<=0 は空を返すべき")
	}
}

func TestPivotHintsFallbackSummary(t *testing.T) {
	// tags が全て不正 → summary のキーワードマッチへフォールバック。
	out := PivotHints([]string{"bogus"}, "内部ネットワークのトンネル選定と横展開", 3)
	if !strings.Contains(out, "[pivot] ") {
		t.Fatalf("不正な tag はキーワードマッチへフォールバックして pivot に:\n%s", out)
	}
}

func TestPivotHintsNoMatchDegrades(t *testing.T) {
	if out := PivotHints(nil, "適当に見る", 3); out != "" {
		t.Fatalf("マッチなしは空を返す(呼び出し側は汎用の文言を使う), got:\n%s", out)
	}
	if out := PivotHints(nil, "", 3); out != "" {
		t.Fatal("空入力は空を返す")
	}
}

func TestPivotHintsForIntent(t *testing.T) {
	out := PivotHintsForIntent(payload(t, map[string]any{"summary": "x", "chain_tags": []string{"web"}}), 3)
	if !strings.Contains(out, "[web] ") {
		t.Fatalf("payload エントリは chain_tags に従い web の方向転換を出すべき:\n%s", out)
	}
	if out := PivotHintsForIntent(json.RawMessage(`{bad json`), 3); out != "" {
		t.Fatal("payload の解析失敗は空を返すべき")
	}
}

func TestRefsGuideForIntent(t *testing.T) {
	out := RefsGuideForIntent(payload(t, map[string]any{"summary": "あるサイトを攻撃", "chain_tags": []string{"web"}}))
	if !strings.Contains(out, "skills/chains-web/refs/") || !strings.Contains(out, "正確な篇目は対応する SKILL.md を参照") {
		t.Fatalf("web tag は refs ガイドを出すべき:\n%s", out)
	}
	// 2 tag:両方のパスが 1 行に入る。
	out = RefsGuideForIntent(payload(t, map[string]any{"summary": "x", "chain_tags": []string{"web", "ad", "pivot"}}))
	if !strings.Contains(out, "skills/chains-web/refs/") || !strings.Contains(out, "skills/chains-ad/refs/") {
		t.Fatalf("先頭 2 つの tag のパスが両方あるべき:\n%s", out)
	}
	if strings.Contains(out, "chains-pivot") {
		t.Fatal("3 つ目の tag は上限超過で現れない")
	}
	// tag なしはキーワードへフォールバック。ヒットなしは静かに。
	out = RefsGuideForIntent(payload(t, map[string]any{"summary": "sql インジェクションのテスト"}))
	if !strings.Contains(out, "skills/chains-web/refs/") {
		t.Fatalf("キーワードフォールバックは web の refs ガイドを出すべき:\n%s", out)
	}
	if out := RefsGuideForIntent(payload(t, map[string]any{"summary": "適当に見る"})); out != "" {
		t.Fatalf("マッチしなければ静かに非注入にすべき。got:\n%s", out)
	}
	if out := RefsGuideForIntent(json.RawMessage(`{bad json`)); out != "" {
		t.Fatal("payload の解析失敗は静かに非注入にすべき")
	}
}
