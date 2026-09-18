package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestLogAPIDebugLevelFallback(t *testing.T) {
	dir := t.TempDir()
	app := &App{
		cfg: Config{
			DataDir:              dir,
			APIDebugEnabled:      true,
			LogLevel:             "error",
			APIDebugLevel:        "", // empty, should fallback to LogLevel "error"
			APIDebugRequestBody:  true,
			APIDebugResponseBody: true,
			APIDebugMaxBodyChars: 4000,
		},
	}

	// 1. Info log should be filtered out
	app.logAPIDebug(apiDebugEntry{
		RequestID: "req_info",
		Level:     "info",
		Success:   true,
	}, []byte(`{"req":"info"}`), []byte(`{"resp":"info"}`))

	lines, err := tailAPIDebugLogs(filepath.Join(dir, "logs"), 10)
	if err != nil {
		t.Fatalf("tail logs: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected 0 lines for info log when level is error, got %d", len(lines))
	}

	// 2. Error log should be written
	app.logAPIDebug(apiDebugEntry{
		RequestID: "req_err",
		Level:     "error",
		Success:   false,
	}, []byte(`{"req":"err"}`), []byte(`{"error":"failed"}`))

	lines, err = tailAPIDebugLogs(filepath.Join(dir, "logs"), 10)
	if err != nil {
		t.Fatalf("tail logs: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line for error log, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "req_err") {
		t.Fatalf("expected log to contain req_err, got %s", lines[0])
	}
}

func TestForwardStreamConsolidatesLog(t *testing.T) {
	a := newTestApp(t)
	t.Cleanup(func() { _ = a.db.Close() })
	a.cfg.APIDebugEnabled = true
	a.cfg.APIDebugResponseBody = true
	a.cfg.APIDebugLevel = "debug"

	// Mock SSE upstream server
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"id":"chatcmpl-test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}`,
			`data: {"id":"chatcmpl-test","choices":[{"index":0,"delta":{"content":" World"}}]}`,
			`data: {"id":"chatcmpl-test","choices":[{"index":0,"delta":{"content":"!"}}]}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstreamServer.Close()

	resp, err := http.Get(upstreamServer.URL)
	if err != nil {
		t.Fatalf("get upstream: %v", err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	provider := ProviderKey{ID: "pk_test", Name: "TestProvider"}
	body := map[string]any{"model": "gpt-4", "stream": true}
	res := a.forwardStream(rec, req, resp, time.Now(), "req_stream_test", "api_chat_test", provider, "lk_test", "gpt-4", "gpt-4", body)
	if !res.Success {
		t.Fatalf("expected success, got error: %v", res.ErrorMessage)
	}

	// Verify debug logs in disk
	lines, err := tailAPIDebugLogs(a.apiDebugLogDir(), 10)
	if err != nil {
		t.Fatalf("tail logs: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 consolidated log line for stream, got %d", len(lines))
	}

	var entry apiDebugEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("parse log entry: %v", err)
	}

	// Verify that response_body_preview is a single valid JSON object containing the consolidated text
	var respObj map[string]any
	if err := json.Unmarshal([]byte(entry.ResponseBodyPreview), &respObj); err != nil {
		t.Fatalf("expected ResponseBodyPreview to be valid consolidated JSON, got: %s", entry.ResponseBodyPreview)
	}

	choices, ok := respObj["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("expected choices array in consolidated response, got %#v", respObj)
	}
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello World!" {
		t.Fatalf("expected aggregated content 'Hello World!', got %q", msg["content"])
	}
}

func TestLogAPIDebugDisabledOmitsBodiesButKeepsNormalLog(t *testing.T) {
	dir := t.TempDir()
	app := &App{
		cfg: Config{
			DataDir:         dir,
			APIDebugEnabled: false,
			LogLevel:        "info",
		},
	}

	app.logAPIDebug(apiDebugEntry{
		RequestID: "req_normal",
		Level:     "info",
		Success:   true,
		Path:      "/v1/chat/completions",
		LatencyMS: 123,
	}, []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`), []byte(`{"choices":[{"message":{"content":"hi"}}]}`))

	lines, err := tailAPIDebugLogs(filepath.Join(dir, "logs"), 10)
	if err != nil {
		t.Fatalf("tail logs: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line written even when APIDebugEnabled is false, got %d", len(lines))
	}

	var entry apiDebugEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if entry.RequestID != "req_normal" {
		t.Fatalf("expected RequestID req_normal, got %q", entry.RequestID)
	}
	if entry.LatencyMS != 123 {
		t.Fatalf("expected LatencyMS 123, got %d", entry.LatencyMS)
	}
	if entry.RequestBodyPreview != "" {
		t.Fatalf("expected empty RequestBodyPreview when APIDebugEnabled is false, got %q", entry.RequestBodyPreview)
	}
	if entry.ResponseBodyPreview != "" {
		t.Fatalf("expected empty ResponseBodyPreview when APIDebugEnabled is false, got %q", entry.ResponseBodyPreview)
	}
}
