package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"
)

func TestMigrationFilenames(t *testing.T) {
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("%v %v", files, err)
	}
	for _, name := range files {
		if _, err := migrationVersion(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"baseline.sql", "0000_invalid.sql", "x001_invalid.sql"} {
		if _, err := migrationVersion(name); err == nil {
			t.Fatal(name)
		}
	}
}

func TestMigrationsFreshExistingAndRepeated(t *testing.T) {
	dsn, _, err := DSN()
	if err != nil {
		t.Skip("PostgreSQL is not configured")
	}
	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	ctx := context.Background()
	conn, err := pg.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, existing := range []bool{false, true} {
		schema := fmt.Sprintf("migration_test_%d", time.Now().UnixNano())
		if _, err := conn.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pg.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`) })
		if _, err := conn.ExecContext(ctx, `SET search_path TO "`+schema+`"`); err != nil {
			t.Fatal(err)
		}
		if existing {
			if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
				t.Fatal(err)
			}
		}
		if err := applyMigrations(ctx, conn); err != nil {
			t.Fatal(err)
		}
		if err := applyMigrations(ctx, conn); err != nil {
			t.Fatal("repeat", err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE artex_schema_migrations SET sha256='changed'`); err != nil {
			t.Fatal(err)
		}
		if err := applyMigrations(ctx, conn); err == nil || !strings.Contains(err.Error(), "changed after") {
			t.Fatalf("checksum mismatch: %v", err)
		}
		if _, err := conn.ExecContext(ctx, `SET search_path TO public`); err != nil {
			t.Fatal(err)
		}
	}
}
