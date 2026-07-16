package app

import (
	"path/filepath"
	"testing"
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
