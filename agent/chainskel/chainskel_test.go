package chainskel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateChainTags(t *testing.T) {
	cases := []struct {
		name    string
		tags    []string
		wantErr bool
	}{
		{"nil は有効", nil, false},
		{"単一で有効", []string{"web"}, false},
		{"2 つで有効", []string{"web", "pivot"}, false},
		{"不正な値", []string{"web", "cloud"}, true},
		{"3 つ以上", []string{"web", "ad", "priv"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateChainTags(c.tags)
			if (err != nil) != c.wantErr {
				t.Fatalf("ValidateChainTags(%v) err=%v, wantErr=%v", c.tags, err, c.wantErr)
			}
		})
	}
}

func TestNormalizeChainTags(t *testing.T) {
	got := NormalizeChainTags([]string{" Web ", "web", "PIVOT", "", "  "})
	want := []string{"web", "pivot"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		text string
		want []string // nil はヒット無しを表す
	}{
		{"/api/user?id=1 に対して sql インジェクションのテスト", []string{"web"}},
		{"ドメイン内で kerberos 委任の悪用を調査", []string{"ad"}},
		{"webshell で足場を確保し、トンネルを張って内部ネットワークを横展開", []string{"pivot"}},
		{"Linux の権限昇格:まず suid とカーネル脆弱性を調べる", []string{"priv"}},
		{"ctf pwn の問題、スタックオーバーフロー + rop", []string{"pwn"}},
		{"サブドメイン収集とフィンガープリント識別などの偵察作業", []string{"recon"}},
		{"そのサンプルをリバースエンジニアリングし、まずアンパックしてから静的解析", []string{"rev"}},
		{"apk のパケットキャプチャが阻まれたので frida で hook", []string{"app"}},
		{"このインターフェースに sql インジェクション、取れた後に権限昇格", []string{"web", "priv"}}, // 複数ヒット。ルール表の順で先頭 2 つを取る
		{"通常の巡回、明確な攻撃面はなし", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := Match(c.text)
		if len(got) != len(c.want) {
			t.Fatalf("Match(%q)=%v, want %v", c.text, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("Match(%q)=%v, want %v", c.text, got, c.want)
			}
		}
	}
}

func TestSkeletonEmbedded(t *testing.T) {
	for _, c := range Categories {
		sk, ok := Skeleton(c)
		if !ok {
			t.Fatalf("カテゴリ %s のスケルトンが欠落(go:embed が 9 カテゴリ全てを保証すべき)", c)
		}
		if !strings.Contains(sk, "場面") || !strings.Contains(sk, "判定基準") {
			t.Fatalf("カテゴリ %s のスケルトンに 場面/判定基準 フィールドが無い。内容が誤っている疑い", c)
		}
	}
	if _, ok := Skeleton("nope"); ok {
		t.Fatal("不正なカテゴリは ok=false を返すべき")
	}
}

func payload(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestForIntentByTags(t *testing.T) {
	out := ForIntent(payload(t, map[string]any{"summary": "あるサイトを攻撃", "chain_tags": []string{"web"}}))
	if !strings.Contains(out, `<chains-skeleton source="chains-skeleton" category="web">`) {
		t.Fatalf("chain_tags による注入に失敗:\n%s", out)
	}
	if strings.Contains(out, "untrusted-data") {
		t.Fatal("スケルトン区画は untrusted の意味ラベルを再利用してはならない")
	}
}

func TestForIntentMultiTagCap2(t *testing.T) {
	out := ForIntent(payload(t, map[string]any{"summary": "x", "chain_tags": []string{"web", "ad", "pivot"}}))
	if !strings.Contains(out, `category="web"`) || !strings.Contains(out, `category="ad"`) {
		t.Fatalf("先頭 2 つの tag が注入されるべき:\n%s", out)
	}
	if strings.Contains(out, `category="pivot"`) {
		t.Fatal("3 つ目の tag は MaxTagsPerIntent を超えるため注入されない")
	}
}

func TestForIntentInvalidTagFallsBack(t *testing.T) {
	// 不正な tag は無視。残りに有効な tag が無ければ summary のキーワードマッチへフォールバック。
	out := ForIntent(payload(t, map[string]any{"summary": "sql インジェクションのテスト", "chain_tags": []string{"bogus"}}))
	if !strings.Contains(out, `category="web"`) {
		t.Fatalf("不正な tag はキーワードマッチへフォールバックして web を注入するべき:\n%s", out)
	}
}

func TestForIntentNoTagKeywordFallback(t *testing.T) {
	out := ForIntent(payload(t, map[string]any{"summary": "内部ネットワークのトンネル選定と横展開"}))
	if !strings.Contains(out, `category="pivot"`) {
		t.Fatalf("tags が無ければキーワードフォールバックで pivot を注入するべき:\n%s", out)
	}
}

func TestForIntentNoMatchSilent(t *testing.T) {
	if out := ForIntent(payload(t, map[string]any{"summary": "適当に見る"})); out != "" {
		t.Fatalf("マッチしなければ静かに非注入にすべき。got:\n%s", out)
	}
	if out := ForIntent(json.RawMessage(`{bad json`)); out != "" {
		t.Fatal("payload の解析失敗は静かに非注入にすべき")
	}
	if out := ForIntent(payload(t, map[string]any{"chain_tags": []string{"bogus"}})); out != "" {
		t.Fatal("tags が全て不正で summary にキーワードが無ければ静かに非注入にすべき")
	}
}

func TestSectionTruncation(t *testing.T) {
	defer func(orig int) { sectionCapRunes = orig }(sectionCapRunes)
	sectionCapRunes = 600 // 上限を小さくして切り詰めを強制
	out := Section([]string{"web", "ad"})
	if out == "" {
		t.Fatal("切り詰め後も内容があるべき")
	}
	if !strings.Contains(out, "切り詰め") {
		t.Fatalf("cap 超過で切り詰め注記があるべき:\n%s", out)
	}
	if n := len([]rune(out)); n > sectionCapRunes+80 { // 标注本身另算,给足余量
		t.Fatalf("切り詰め後の総長 %d 远超 cap %d", n, sectionCapRunes)
	}
}

func TestSectionMissingCategoryDegrades(t *testing.T) {
	// 不正なカテゴリ(スケルトンが必ず欠落)は静かにスキップ。有効なカテゴリはそのまま注入。
	out := Section([]string{"nope", "web"})
	if !strings.Contains(out, `category="web"`) {
		t.Fatalf("欠落カテゴリはスキップし、有効カテゴリは通常どおり注入:\n%s", out)
	}
	if strings.Contains(out, "nope") {
		t.Fatal("欠落カテゴリは出力に現れない")
	}
	if Section([]string{"nope"}) != "" {
		t.Fatal("全て欠落なら空文字列を返す(非注入)")
	}
}
