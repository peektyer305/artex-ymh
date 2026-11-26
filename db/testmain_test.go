package db

import (
	"github.com/Autumn-27/artex/internal/testpg"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dsn, _, err := DSN()
	if err != nil {
		// Tests that need PostgreSQL skip themselves when it is not configured.
		os.Exit(m.Run())
	}
	os.Exit(testpg.Run(m, dsn))
}

// Use a child schema when a test relies on exact row counts or task id values.
func isolatedTestDB(t *testing.T) *DB {
	t.Helper()
	dsn, cleanup, err := testpg.Isolate(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}
