// Package testpg isolates PostgreSQL-backed test suites from existing data and
// from other Go test packages. It is used only by test entrypoints.
package testpg

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Isolate creates an empty schema and returns a DSN whose search_path contains
// only that schema. Cleanup never drops the database or any pre-existing schema.
func Isolate(dsn string) (string, func() error, error) {
	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := pg.PingContext(ctx); err != nil {
		pg.Close()
		return "", nil, err
	}
	var random [12]byte
	_, _ = rand.Read(random[:])
	schema := "artex_test_" + hex.EncodeToString(random[:])
	quoted := `"` + schema + `"`
	if _, err := pg.ExecContext(ctx, `CREATE SCHEMA `+quoted); err != nil {
		pg.Close()
		return "", nil, err
	}
	cleanup := func() error {
		defer pg.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := pg.ExecContext(ctx, `DROP SCHEMA `+quoted+` CASCADE`)
		return err
	}
	isolated := dsn + " search_path=" + schema
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			_ = cleanup()
			return "", nil, err
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		isolated = u.String()
	}
	return isolated, cleanup, nil
}

// Run returns the test exit code after releasing the schema and restoring the
// environment. Call os.Exit here only from the caller, so cleanup actually runs.
func Run(m *testing.M, dsn string) (code int) {
	isolated, cleanup, err := Isolate(dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prepare isolated test schema:", err)
		return 1
	}
	defer func() {
		if err := cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, "remove isolated test schema:", err)
			code = 1
		}
	}()
	previous, wasSet := os.LookupEnv("ARTEX_PG_DSN")
	defer func() {
		if wasSet {
			_ = os.Setenv("ARTEX_PG_DSN", previous)
		} else {
			_ = os.Unsetenv("ARTEX_PG_DSN")
		}
	}()
	if err := os.Setenv("ARTEX_PG_DSN", isolated); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}
