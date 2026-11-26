package testpg

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

func TestSchemasKeepSuiteDataSeparateAndCleanupOwnedSchema(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL is not configured")
	}
	a, dropA, err := Isolate(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Dropping the first schema explicitly must not remove the second one.
	defer func() {
		if dropA != nil {
			_ = dropA()
		}
	}()
	b, dropB, err := Isolate(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := dropB(); err != nil {
			t.Error(err)
		}
	}()
	first, err := sql.Open("pgx", a)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := sql.Open("pgx", b)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var schemaA, schemaB string
	if err := first.QueryRowContext(t.Context(), "SELECT current_schema()").Scan(&schemaA); err != nil {
		t.Fatal(err)
	}
	if err := second.QueryRowContext(t.Context(), "SELECT current_schema()").Scan(&schemaB); err != nil {
		t.Fatal(err)
	}
	if schemaA == schemaB {
		t.Fatal("test schemas are shared")
	}
	if _, err := first.Exec(`CREATE TABLE fixture(value int); INSERT INTO fixture VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	var table sql.NullString
	if err := second.QueryRow(`SELECT to_regclass('fixture')::text`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table.Valid {
		t.Fatal("first suite data leaked into second suite")
	}
	if _, err := second.Exec(`CREATE TABLE fixture(value int); INSERT INTO fixture VALUES(2)`); err != nil {
		t.Fatal(err)
	}
	first.Close()
	if err := dropA(); err != nil {
		t.Fatal(err)
	}
	dropA = nil
	var remaining int
	if err := second.QueryRow(`SELECT count(*) FROM pg_namespace WHERE nspname=$1`, schemaA).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("owned schema was not dropped")
	}
	if err := second.QueryRowContext(context.Background(), `SELECT value FROM fixture`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("unrelated schema was changed: %d %v", remaining, err)
	}
}
