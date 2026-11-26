package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Called only while the existing dedicated schema advisory lock is held.
func applyMigrations(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS artex_schema_migrations (
    version BIGINT PRIMARY KEY, name TEXT NOT NULL, sha256 TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	var latest int64
	seen := map[int64]bool{}
	for _, name := range files {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if seen[version] {
			return fmt.Errorf("duplicate migration version %d", version)
		}
		seen[version] = true
		if version > latest {
			latest = version
		}
	}
	var installed int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(max(version),0) FROM artex_schema_migrations`).Scan(&installed); err != nil {
		return err
	}
	if installed > latest {
		return fmt.Errorf("database schema version %d is newer than this binary (%d)", installed, latest)
	}
	for _, name := range files {
		version, _ := migrationVersion(name)
		body, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(body))
		var stored string
		err = conn.QueryRowContext(ctx, `SELECT sha256 FROM artex_schema_migrations WHERE version=$1`, version).Scan(&stored)
		if err == nil {
			if stored != digest {
				return fmt.Errorf("migration %s was changed after application; add a new migration", name)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for attempt := 0; ; attempt++ {
			err = applyMigration(ctx, conn, version, name, digest, string(body))
			if err == nil {
				break
			}
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return err
			}
			timer := time.NewTimer(schemaDeadlockRetryDelays[attempt])
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil
}

func migrationVersion(name string) (int64, error) {
	prefix, _, ok := strings.Cut(path.Base(name), "_")
	if !ok || len(prefix) != 4 {
		return 0, fmt.Errorf("invalid migration filename %s", name)
	}
	value, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid migration version %s", name)
	}
	return value, nil
}

func applyMigration(ctx context.Context, conn *sql.Conn, version int64, name, digest, body string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("apply %s: %w", name, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO artex_schema_migrations(version,name,sha256) VALUES($1,$2,$3)`, version, name, digest); err != nil {
		return err
	}
	return tx.Commit()
}

// Seed updates have a separate version from DDL. A failed seed never records success.
func applySeedVersion(ctx context.Context, conn *sql.Conn, version int64, apply func() error) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS artex_seed_versions(version BIGINT PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artex_seed_versions WHERE version=$1)`, version).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := apply(); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO artex_seed_versions(version) VALUES($1)`, version)
	return err
}
