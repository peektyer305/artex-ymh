package server

import (
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// バッチ 5 B2:dsnPassword が 2 種類の DSN 形態(URL / keyword)からパスワードを抽出し、パスワードがなければ空を返す。
func TestDSNPassword(t *testing.T) {
	cases := []struct {
		dsn, want string
	}{
		{"postgres://artex:s3cr3t@127.0.0.1:5432/artex?sslmode=disable", "s3cr3t"},
		{"postgres://artex@127.0.0.1:5432/artex", ""},
		{"host=127.0.0.1 user=artex password=pw123456 dbname=artex", "pw123456"},
		{"host=127.0.0.1 password='pw with space' dbname=artex", "pw with space"},
		{"host=127.0.0.1 dbname=artex", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := dsnPassword(c.dsn); got != c.want {
			t.Errorf("dsnPassword(%q) = %q, want %q", c.dsn, got, c.want)
		}
	}
}

// バッチ 5 B4:uaForTask は DB がない場合に既定のプールを使い、同じタスクでは安定する(DB がある経路は PG に依存し、
// 統合環境でカバーするため、単体テストでは DSN を叩かない)。
func TestUAForTaskDefaultPool(t *testing.T) {
	s := &Server{} // m == nil → 既定のプール
	ua := s.uaForTask(42)
	if ua == "" {
		t.Fatal("uaForTask should return a default-pool UA without a DB")
	}
	if got := s.uaForTask(42); got != ua {
		t.Fatalf("same task must keep the same UA: %q vs %q", ua, got)
	}
	pool := agent.DefaultUAPool()
	found := false
	for _, p := range pool {
		if p == ua {
			found = true
		}
	}
	if !found {
		t.Fatalf("uaForTask without settings should pick from the default pool, got %q", ua)
	}
}
