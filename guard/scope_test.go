package guard

import "testing"

func scopeRules(kv ...string) []ScopeRule {
	var out []ScopeRule
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, ScopeRule{Kind: kv[i], Value: kv[i+1]})
	}
	return out
}

func TestScopeMatcherDomain(t *testing.T) {
	m := NewScopeMatcher(scopeRules("domain", "Example.COM"))
	cases := []struct {
		host string
		want ScopeVerdict
	}{
		{"example.com", ScopeIn},           // 完全一致、大文字小文字を区別しない
		{"EXAMPLE.COM", ScopeIn},           //
		{"www.example.com", ScopeIn},       // サブドメイン
		{"a.b.example.com", ScopeIn},       // 多段サブドメイン
		{"example.com.", ScopeIn},          // 末尾の点
		{"www.EXAMPLE.com.", ScopeIn},      // 大文字小文字 + 末尾の点の混在
		{"notexample.com", ScopeOut},       // サフィックスがサブドメインと等しくない
		{"example.com.evil.com", ScopeOut}, // ドメインをプレフィックスにしても該当しない
		{"other.org", ScopeOut},            //
		{"example.com:8443", ScopeIn},      // host:port はポートを除去
	}
	for _, c := range cases {
		if got := m.Check(c.host); got != c.want {
			t.Errorf("Check(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestScopeMatcherIP(t *testing.T) {
	m := NewScopeMatcher(scopeRules("ip", "10.0.0.5", "ip", "2001:db8::1"))
	for _, c := range []struct {
		host string
		want ScopeVerdict
	}{
		{"10.0.0.5", ScopeIn},
		{"10.0.0.5:8080", ScopeIn}, // ポート除去後に完全一致
		{"10.0.0.6", ScopeOut},
		{"2001:db8::1", ScopeIn},
		{"[2001:db8::1]:443", ScopeIn},
		{"2001:db8::2", ScopeOut},
	} {
		if got := m.Check(c.host); got != c.want {
			t.Errorf("Check(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestScopeMatcherCIDR(t *testing.T) {
	m := NewScopeMatcher(scopeRules("cidr", "192.168.1.0/24", "cidr", "2001:db8::/32"))
	for _, c := range []struct {
		host string
		want ScopeVerdict
	}{
		{"192.168.1.1", ScopeIn},
		{"192.168.1.254", ScopeIn},
		{"192.168.2.1", ScopeOut},
		{"2001:db8::dead:beef", ScopeIn}, // IPv6 の包含
		{"2001:db9::1", ScopeOut},
	} {
		if got := m.Check(c.host); got != c.want {
			t.Errorf("Check(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestScopeMatcherICPAndKeyword(t *testing.T) {
	// icp は domain のセマンティクス。keyword は死規則で、マッチングに参加しない。
	m := NewScopeMatcher(scopeRules("icp", "example.cn", "keyword", "某企業"))
	if got := m.Check("www.example.cn"); got != ScopeIn {
		t.Errorf("icp のドメインセマンティクスは命中すべきです, got %v", got)
	}
	if got := m.Check("evil.com"); got != ScopeOut {
		t.Errorf("keyword ルールはどの host にも命中すべきではありません, got %v", got)
	}
}

func TestScopeMatcherBadRules(t *testing.T) {
	// 不正なルールはスキップ。全部スキップ = 空ルール → Unknown（「登録範囲内」とはみなさない）。
	m := NewScopeMatcher(scopeRules("cidr", "not-a-cidr", "ip", "999.999.1.1", "domain", "", "keyword", "x"))
	if got := m.Check("anything.com"); got != ScopeUnknown {
		t.Errorf("すべて不正なルールなら Unknown になるべきです, got %v", got)
	}
	// 正常/不正の混在:正常なルールはそのまま有効。
	m2 := NewScopeMatcher(scopeRules("cidr", "bogus", "domain", "ok.com"))
	if got := m2.Check("ok.com"); got != ScopeIn {
		t.Errorf("正常なルールは有効になるべきです, got %v", got)
	}
}

func TestScopeMatcherEmptyRulesUnknown(t *testing.T) {
	m := NewScopeMatcher(nil)
	if got := m.Check("8.8.8.8"); got != ScopeUnknown {
		t.Errorf("空のルール = 未登録の範囲, Unknown になるべきです, got %v", got)
	}
	if m.InScope("8.8.8.8") {
		t.Error("Unknown を InScope とみなすべきではありません")
	}
}

func TestScopeMatcherLocalOption(t *testing.T) {
	m := NewScopeMatcher(scopeRules("domain", "target.com"))
	for _, h := range []string{"127.0.0.1", "localhost", "::1", "169.254.1.1"} {
		if got := m.Check(h); got != ScopeIn {
			t.Errorf("デフォルト AllowLocal:%s は通過させるべきです(worker の本機操作), got %v", h, got)
		}
	}
	// RFC1918 の内部ネットワークアドレスは本機通過の対象外。範囲に登録する必要がある。
	if got := m.Check("10.0.0.9"); got != ScopeOut {
		t.Errorf("未登録の内部ネットワークアドレスは Out になるべきです, got %v", got)
	}
	m.AllowLocal = false
	if got := m.Check("127.0.0.1"); got != ScopeOut {
		t.Errorf("AllowLocal=false のとき 127.0.0.1 は範囲マッチングを通すべきです, got %v", got)
	}
}
