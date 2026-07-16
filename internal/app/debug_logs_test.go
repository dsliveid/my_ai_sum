package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceAPIDebugLogRangeKeepsAppendedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-debug-2026-07-16.log")
	if err := writeDebugLogFileForTest(path, []string{"one", "two", "three", "four"}); err != nil {
		t.Fatalf("write log: %v", err)
	}

	segment, err := tailAPIDebugLogSegment(dir, 2)
	if err != nil {
		t.Fatalf("tail segment: %v", err)
	}
	if segment.StartLine != 3 || segment.EndLine != 4 || segment.Content != "three\nfour" {
		t.Fatalf("segment = %#v", segment)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	_, _ = f.WriteString("five\n")
	_ = f.Close()

	if err := replaceAPIDebugLogRange(dir, segment.File, segment.StartLine, segment.EndLine, "THREE\nFOUR"); err != nil {
		t.Fatalf("replace range: %v", err)
	}
	got, err := readAllDebugLogForTest(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	want := strings.Join([]string{"one", "two", "THREE", "FOUR", "five"}, "\n") + "\n"
	if got != want {
		t.Fatalf("log content = %q, want %q", got, want)
	}
}

func TestReplaceAPIDebugLogRangeRejectsUnsafeFile(t *testing.T) {
	if err := replaceAPIDebugLogRange(t.TempDir(), "other.log", 1, 1, "x"); err == nil {
		t.Fatal("expected unsafe file to be rejected")
	}
}
