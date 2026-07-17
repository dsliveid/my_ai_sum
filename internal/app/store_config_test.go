package app

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestResolveDataDirUsesExeBaseForRelativePath(t *testing.T) {
	base := filepath.Join("D:\\", "apps", "my_ai_sum")
	got := resolveDataDir("data", base)
	want := filepath.Join(base, "data")
	if !samePath(got, want) {
		t.Fatalf("resolveDataDir = %q, want %q", got, want)
	}
}

func TestPortableDataDirForConfigStoresDefaultAsRelative(t *testing.T) {
	base := filepath.Join("D:\\", "apps", "my_ai_sum")
	dataDir := filepath.Join(base, "data")
	if got := portableDataDirForConfig(dataDir, base); got != "data" {
		t.Fatalf("portableDataDirForConfig = %q, want data", got)
	}
}

func TestShouldUseCurrentDefaultDataDirForMovedGeneratedConfig(t *testing.T) {
	currentBase := filepath.Join("D:\\", "new-place", "my_ai_sum")
	defaultDataDir := filepath.Join(currentBase, "data")
	configPath := filepath.Join(defaultDataDir, "config.json")
	oldGeneratedDataDir := filepath.Join("D:\\", "old-place", "my_ai_sum", "data")

	if !shouldUseCurrentDefaultDataDir(oldGeneratedDataDir, oldGeneratedDataDir, defaultDataDir, configPath) {
		t.Fatal("expected moved generated data_dir to be replaced with current default data dir")
	}
}

func TestShouldKeepCustomAbsoluteDataDir(t *testing.T) {
	currentBase := filepath.Join("D:\\", "new-place", "my_ai_sum")
	defaultDataDir := filepath.Join(currentBase, "data")
	configPath := filepath.Join(defaultDataDir, "config.json")
	customDataDir := filepath.Join("D:\\", "my-ai-sum-data")

	if shouldUseCurrentDefaultDataDir(customDataDir, customDataDir, defaultDataDir, configPath) {
		t.Fatal("expected custom absolute data_dir to be kept")
	}
}

func TestMigrateModelMappingsUsesProviderScopedUniqueIndex(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE model_mappings (
		id TEXT PRIMARY KEY, local_model TEXT NOT NULL UNIQUE, upstream_model TEXT NOT NULL,
		provider_key_id TEXT NOT NULL, capability TEXT NOT NULL DEFAULT 'chat', enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO model_mappings(id,local_model,upstream_model,provider_key_id,capability,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		"mm1", "grok-4.5", "grok-4.5", "pk1", "chat", 1, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO model_mappings(id,local_model,upstream_model,provider_key_id,capability,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		"mm2", "grok-4.5", "grok-4.5", "pk2", "chat", 1, now(), now())
	if err != nil {
		t.Fatalf("expected duplicate local_model on different provider to be allowed: %v", err)
	}
	if _, err = db.Exec(`INSERT INTO model_mappings(id,local_model,upstream_model,provider_key_id,capability,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		"mm3", "grok-4.5", "grok-4.5-alt", "pk2", "chat", 1, now(), now()); err == nil {
		t.Fatal("expected duplicate local_model on same provider to fail")
	}
}
