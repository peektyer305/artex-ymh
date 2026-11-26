package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadErrorsAndMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ARTEX_CONFIG", path)
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"database":{"host":"valid"},"skill_dir":3}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err == nil || !strings.Contains(err.Error(), path) || got.Database.Host != "" {
		t.Fatalf("%#v %v", got, err)
	}
	t.Setenv("ARTEX_PG_DSN", "")
	if _, _, err := PostgresDSN(); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("lost parse error: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("directory accepted as configuration")
	}
}
