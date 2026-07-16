package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type apiDebugEntry struct {
	Time                string         `json:"time"`
	Level               string         `json:"level"`
	RequestID           string         `json:"request_id"`
	Source              string         `json:"source"`
	Method              string         `json:"method"`
	Path                string         `json:"path"`
	UpstreamURL         string         `json:"upstream_url"`
	ProviderKeyID       string         `json:"provider_key_id"`
	ProviderName        string         `json:"provider_name"`
	LocalAPIKeyID       string         `json:"local_api_key_id"`
	LocalModel          string         `json:"local_model"`
	UpstreamModel       string         `json:"upstream_model"`
	Stream              bool           `json:"stream"`
	StatusCode          int            `json:"status_code"`
	LatencyMS           int64          `json:"latency_ms"`
	Success             bool           `json:"success"`
	ErrorMessage        string         `json:"error_message,omitempty"`
	PromptTokens        int            `json:"prompt_tokens,omitempty"`
	CompletionTokens    int            `json:"completion_tokens,omitempty"`
	TotalTokens         int            `json:"total_tokens,omitempty"`
	UsageSource         string         `json:"usage_source,omitempty"`
	RequestBodyPreview  string         `json:"request_body_preview,omitempty"`
	ResponseBodyPreview string         `json:"response_body_preview,omitempty"`
	Extra               map[string]any `json:"extra,omitempty"`
}

type apiDebugLogSegment struct {
	Lines      []string `json:"lines"`
	Content    string   `json:"content"`
	File       string   `json:"file"`
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	TotalLines int      `json:"total_lines"`
	Limit      int      `json:"limit"`
	Editable   bool     `json:"editable"`
}

func (a *App) logAPIDebug(entry apiDebugEntry, requestBody, responseBody []byte) {
	if !a.cfg.APIDebugEnabled {
		return
	}
	normalizeAPIDebugConfig(&a.cfg)
	entry.Time = now()
	if entry.Level == "" {
		if entry.Success {
			entry.Level = "info"
		} else {
			entry.Level = "error"
		}
	}
	if !apiDebugLevelEnabled(a.cfg.APIDebugLevel, entry.Level) {
		return
	}
	if a.cfg.APIDebugRequestBody && len(requestBody) > 0 {
		entry.RequestBodyPreview = previewDebugBody(requestBody, a.cfg.APIDebugMaxBodyChars)
	}
	if a.cfg.APIDebugResponseBody && len(responseBody) > 0 {
		entry.ResponseBodyPreview = previewDebugBody(responseBody, a.cfg.APIDebugMaxBodyChars)
	}
	if err := os.MkdirAll(a.apiDebugLogDir(), 0o700); err != nil {
		return
	}
	path := filepath.Join(a.apiDebugLogDir(), "api-debug-"+time.Now().Format("2006-01-02")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(entry)
	_, _ = f.Write(append(b, '\n'))
}

func (a *App) apiDebugLogDir() string {
	return filepath.Join(a.cfg.DataDir, "logs")
}

func apiDebugLevelEnabled(configured, level string) bool {
	ranks := map[string]int{"error": 0, "info": 1, "debug": 2, "trace": 3}
	c, ok := ranks[strings.ToLower(strings.TrimSpace(configured))]
	if !ok {
		c = ranks["info"]
	}
	l, ok := ranks[strings.ToLower(strings.TrimSpace(level))]
	if !ok {
		l = ranks["info"]
	}
	return l <= c
}

func previewDebugBody(body []byte, limit int) string {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(body, &v) == nil {
		v = redactDebugValue(v)
		if b, err := json.Marshal(v); err == nil {
			return summarizeText(string(b), limit)
		}
	}
	return summarizeText(string(body), limit)
}

func redactDebugValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if isSensitiveDebugKey(k) {
				out[k] = maskSecret(textFromAny(val))
				continue
			}
			out[k] = redactDebugValue(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, item := range x {
			out = append(out, redactDebugValue(item))
		}
		return out
	default:
		return v
	}
}

func isSensitiveDebugKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(k, "api_key") ||
		strings.Contains(k, "apikey") ||
		strings.Contains(k, "authorization") ||
		strings.Contains(k, "password") ||
		strings.Contains(k, "secret") ||
		strings.Contains(k, "token")
}

func (a *App) handleAPIDebugLogs(w http.ResponseWriter, r *http.Request, sub string) {
	switch {
	case sub == "" && r.Method == http.MethodGet:
		limit := queryLimit(r, 100)
		segment, err := tailAPIDebugLogSegment(a.apiDebugLogDir(), limit)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, segment)
	case sub == "/save" && r.Method == http.MethodPost:
		var req struct {
			File      string `json:"file"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
			Content   string `json:"content"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := replaceAPIDebugLogRange(a.apiDebugLogDir(), req.File, req.StartLine, req.EndLine, req.Content); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func tailAPIDebugLogs(dir string, limit int) ([]string, error) {
	segment, err := tailAPIDebugLogSegment(dir, limit)
	if err != nil {
		return nil, err
	}
	return segment.Lines, nil
}

func tailAPIDebugLogSegment(dir string, limit int) (apiDebugLogSegment, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 5000 {
		limit = 5000
	}
	empty := apiDebugLogSegment{Lines: []string{}, Limit: limit}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, nil
		}
		return empty, err
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "api-debug-") && strings.HasSuffix(name, ".log") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return empty, nil
	}
	path := paths[len(paths)-1]
	fileLines, err := readDebugLogLines(path)
	if err != nil {
		return empty, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	total := len(fileLines)
	start := total - limit
	if start < 0 {
		start = 0
	}
	lines := append([]string{}, fileLines[start:]...)
	return apiDebugLogSegment{
		Lines:      lines,
		Content:    strings.Join(lines, "\n"),
		File:       filepath.Base(path),
		StartLine:  start + 1,
		EndLine:    total,
		TotalLines: total,
		Limit:      limit,
		Editable:   true,
	}, nil
}

func readDebugLogLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func replaceAPIDebugLogRange(dir, file string, startLine, endLine int, content string) error {
	file = filepath.Base(strings.TrimSpace(file))
	if file == "." || file == "" || !strings.HasPrefix(file, "api-debug-") || !strings.HasSuffix(file, ".log") {
		return fmt.Errorf("invalid log file")
	}
	if startLine <= 0 || endLine < startLine {
		return fmt.Errorf("invalid line range")
	}
	path := filepath.Join(dir, file)
	if err := ensurePathInsideDir(path, dir); err != nil {
		return err
	}
	current, err := readDebugLogLinesPreserveEmpty(path)
	if err != nil {
		return err
	}
	if endLine > len(current) {
		return fmt.Errorf("log file changed: end line %d is beyond current total %d", endLine, len(current))
	}
	replacement := splitEditableLogContent(content)
	next := make([]string, 0, len(current)-(endLine-startLine+1)+len(replacement))
	next = append(next, current[:startLine-1]...)
	next = append(next, replacement...)
	next = append(next, current[endLine:]...)
	return os.WriteFile(path, []byte(strings.Join(next, "\n")+"\n"), 0o600)
}

func ensurePathInsideDir(path, dir string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return err
	}
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("log file is outside logs directory")
	}
	return nil
}

func readDebugLogLinesPreserveEmpty(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return []string{}, nil
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, nil
}

func splitEditableLogContent(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return []string{}
	}
	return strings.Split(content, "\n")
}

func writeDebugLogFileForTest(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func readAllDebugLogForTest(path string) (string, error) {
	b, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer b.Close()
	data, err := io.ReadAll(b)
	return string(data), err
}
