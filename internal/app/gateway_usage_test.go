package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalKeyMappedModelUsageUsesUpstreamModel(t *testing.T) {
	var upstreamRequest map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer provider-secret" {
			t.Fatalf("unexpected upstream authorization: %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamRequest); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl_test",
			"object":  "chat.completion",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage": map[string]any{
				"prompt_tokens":     11,
				"completion_tokens": 7,
				"total_tokens":      18,
			},
		})
	}))
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	providerID := insertTestProvider(t, a, upstream.URL+"/v1")
	localSecret := insertTestLocalKey(t, a, providerID)
	insertTestMapping(t, a, providerID, "local-alias-model", "actual-upstream-model")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", a.serveAPI)
	mux.HandleFunc("/v1/", a.serveGateway)
	server := httptest.NewServer(withRecover(mux))
	defer server.Close()

	body := map[string]any{
		"model":    "local-alias-model",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	}
	resp := postJSON(t, server.URL+"/v1/chat/completions", "Bearer "+localSecret, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	if got := upstreamRequest["model"]; got != "actual-upstream-model" {
		t.Fatalf("upstream request model = %v, want actual-upstream-model", got)
	}

	var localModel, upstreamModel string
	var promptTokens, completionTokens, totalTokens int
	err := a.db.QueryRow(`SELECT local_model,upstream_model,prompt_tokens,completion_tokens,total_tokens FROM usage_records LIMIT 1`).
		Scan(&localModel, &upstreamModel, &promptTokens, &completionTokens, &totalTokens)
	if err != nil {
		t.Fatalf("read usage_records: %v", err)
	}
	if localModel != "local-alias-model" || upstreamModel != "actual-upstream-model" {
		t.Fatalf("usage model fields = local %q upstream %q", localModel, upstreamModel)
	}
	if promptTokens != 11 || completionTokens != 7 || totalTokens != 18 {
		t.Fatalf("usage tokens = %d/%d/%d", promptTokens, completionTokens, totalTokens)
	}

	adminToken := "admin-token"
	a.sessions[adminToken] = time.Now().Add(time.Hour)

	var byModel []map[string]any
	getJSON(t, server.URL+"/api/v1/usage/models", "Bearer "+adminToken, &byModel)
	if len(byModel) != 1 || byModel[0]["key"] != "actual-upstream-model" {
		t.Fatalf("usage models = %#v, want upstream model", byModel)
	}

	var daily []map[string]any
	getJSON(t, server.URL+"/api/v1/usage/daily", "Bearer "+adminToken, &daily)
	if len(daily) != 1 || daily[0]["model"] != "actual-upstream-model" {
		t.Fatalf("usage daily = %#v, want upstream model", daily)
	}

	today := time.Now().Format("2006-01-02")
	var filtered []map[string]any
	getJSON(t, server.URL+"/api/v1/usage/models?start_date="+today+"&end_date="+today, "Bearer "+adminToken, &filtered)
	if len(filtered) != 1 || filtered[0]["key"] != "actual-upstream-model" {
		t.Fatalf("filtered usage models = %#v, want current record", filtered)
	}

	var empty []map[string]any
	getJSON(t, server.URL+"/api/v1/usage/models?start_date=2000-01-01&end_date=2000-01-01", "Bearer "+adminToken, &empty)
	if len(empty) != 0 {
		t.Fatalf("old date usage models = %#v, want empty", empty)
	}
}

func TestResponsesGatewayUsesLocalKeyAndTracksResponsesUsage(t *testing.T) {
	var upstreamRequest map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer provider-secret" {
			t.Fatalf("unexpected upstream authorization: %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamRequest); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":     "resp_test",
			"object": "response",
			"output": []map[string]any{{"type": "message", "content": []map[string]any{{"type": "output_text", "text": "ok"}}}},
			"usage": map[string]any{
				"input_tokens":  13,
				"output_tokens": 5,
				"total_tokens":  18,
			},
		})
	}))
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	providerID := insertTestProvider(t, a, upstream.URL+"/v1")
	localSecret := insertTestLocalKey(t, a, providerID)
	insertTestMapping(t, a, providerID, "local-alias-model", "actual-upstream-model")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", a.serveAPI)
	mux.HandleFunc("/v1/", a.serveGateway)
	mux.HandleFunc("/responses", a.serveGateway)
	server := httptest.NewServer(withRecover(mux))
	defer server.Close()

	body := map[string]any{
		"model": "local-alias-model",
		"input": "hello",
	}
	resp := postJSON(t, server.URL+"/responses", "Bearer "+localSecret, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d", resp.StatusCode)
	}
	if got := upstreamRequest["model"]; got != "actual-upstream-model" {
		t.Fatalf("upstream responses model = %v, want actual-upstream-model", got)
	}

	var localModel, upstreamModel string
	var promptTokens, completionTokens, totalTokens int
	err := a.db.QueryRow(`SELECT local_model,upstream_model,prompt_tokens,completion_tokens,total_tokens FROM usage_records LIMIT 1`).
		Scan(&localModel, &upstreamModel, &promptTokens, &completionTokens, &totalTokens)
	if err != nil {
		t.Fatalf("read usage_records: %v", err)
	}
	if localModel != "local-alias-model" || upstreamModel != "actual-upstream-model" {
		t.Fatalf("usage model fields = local %q upstream %q", localModel, upstreamModel)
	}
	if promptTokens != 13 || completionTokens != 5 || totalTokens != 18 {
		t.Fatalf("responses usage tokens = %d/%d/%d", promptTokens, completionTokens, totalTokens)
	}
}

func TestResponsesGatewayConvertsResponsesToChatCompletion(t *testing.T) {
	var upstreamRequest map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamRequest); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl_deepseek",
			"object":  "chat.completion",
			"model":   "actual-upstream-model",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "deepseek ok"}}},
			"usage": map[string]any{
				"prompt_tokens":     17,
				"completion_tokens": 4,
				"total_tokens":      21,
			},
		})
	}))
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	providerID := insertTestProviderWithType(t, a, "deepseek", upstream.URL+"/v1")
	localSecret := insertTestLocalKeyWithProtocol(t, a, providerID, true, protocolResponses, protocolChatCompletions)
	insertTestMapping(t, a, providerID, "local-alias-model", "actual-upstream-model")

	mux := http.NewServeMux()
	mux.HandleFunc("/responses", a.serveGateway)
	server := httptest.NewServer(withRecover(mux))
	defer server.Close()

	resp := postJSON(t, server.URL+"/responses", "Bearer "+localSecret, map[string]any{
		"model":        "local-alias-model",
		"instructions": "be concise",
		"input":        "hello",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responses conversion status = %d", resp.StatusCode)
	}
	if got := upstreamRequest["model"]; got != "actual-upstream-model" {
		t.Fatalf("upstream chat model = %v, want actual-upstream-model", got)
	}
	messages, _ := upstreamRequest["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("upstream messages = %#v, want system + user", upstreamRequest["messages"])
	}
	var responseBody map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&responseBody); err != nil {
		t.Fatalf("decode responses conversion body: %v", err)
	}
	if responseBody["object"] != "response" || responseBody["output_text"] != "deepseek ok" {
		t.Fatalf("responses conversion body = %#v", responseBody)
	}
	if responseBody["model"] != "local-alias-model" {
		t.Fatalf("client response model = %v, want local-alias-model", responseBody["model"])
	}

	var localModel, upstreamModel string
	var promptTokens, completionTokens, totalTokens int
	err := a.db.QueryRow(`SELECT local_model,upstream_model,prompt_tokens,completion_tokens,total_tokens FROM usage_records LIMIT 1`).
		Scan(&localModel, &upstreamModel, &promptTokens, &completionTokens, &totalTokens)
	if err != nil {
		t.Fatalf("read usage_records: %v", err)
	}
	if localModel != "local-alias-model" || upstreamModel != "actual-upstream-model" {
		t.Fatalf("usage model fields = local %q upstream %q", localModel, upstreamModel)
	}
	if promptTokens != 17 || completionTokens != 4 || totalTokens != 21 {
		t.Fatalf("conversion usage tokens = %d/%d/%d", promptTokens, completionTokens, totalTokens)
	}
}

func TestLocalChatTestUsesLocalKeyClientProtocol(t *testing.T) {
	var upstreamRequest map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamRequest); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl_deepseek",
			"object":  "chat.completion",
			"model":   "actual-upstream-model",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "local test ok"}}},
			"usage": map[string]any{
				"prompt_tokens":     8,
				"completion_tokens": 3,
				"total_tokens":      11,
			},
		})
	}))
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	adminToken := "admin-token"
	a.sessions[adminToken] = time.Now().Add(time.Hour)
	providerID := insertTestProviderWithType(t, a, "deepseek", upstream.URL+"/v1")
	insertTestLocalKeyWithProtocol(t, a, providerID, true, protocolResponses, protocolChatCompletions)
	insertTestMapping(t, a, providerID, "local-alias-model", "actual-upstream-model")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", a.serveAPI)
	server := httptest.NewServer(withRecover(mux))
	defer server.Close()

	resp := postJSON(t, server.URL+"/api/v1/test-chat/local", "Bearer "+adminToken, map[string]any{
		"local_api_key_id": "lk_test",
		"model":            "local-alias-model",
		"messages":         []map[string]any{{"role": "system", "content": "be concise"}, {"role": "user", "content": "hello"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("local chat test status = %d body=%s", resp.StatusCode, body)
	}
	if got := upstreamRequest["model"]; got != "actual-upstream-model" {
		t.Fatalf("upstream chat model = %v, want actual-upstream-model", got)
	}
	messages, _ := upstreamRequest["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("upstream messages = %#v, want system + user", upstreamRequest["messages"])
	}
	var responseBody map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&responseBody); err != nil {
		t.Fatalf("decode local chat test response: %v", err)
	}
	if responseBody["object"] != "response" || responseBody["output_text"] != "local test ok" {
		t.Fatalf("local chat test response = %#v", responseBody)
	}
}

func TestUpstreamChatTestReportsNonJSONResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body>not an api endpoint</body></html>"))
	}))
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	adminToken := "admin-token"
	a.sessions[adminToken] = time.Now().Add(time.Hour)
	providerID := insertTestProvider(t, a, upstream.URL+"/v1")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", a.serveAPI)
	server := httptest.NewServer(withRecover(mux))
	defer server.Close()

	resp := postJSON(t, server.URL+"/api/v1/test-chat/upstream", "Bearer "+adminToken, map[string]any{
		"provider_key_id": providerID,
		"model":           "local-alias-model",
		"messages":        []map[string]any{{"role": "user", "content": "hello"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s, want 502", resp.StatusCode, body)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(stringFromAny(body["error"], ""), "non-JSON") {
		t.Fatalf("error response = %#v, want non-JSON message", body)
	}
	if !strings.Contains(stringFromAny(body["body_preview"], ""), "<!doctype html>") {
		t.Fatalf("body preview = %#v, want html preview", body["body_preview"])
	}
}

func TestResponsesGatewayConvertsChatStreamToResponsesStream(t *testing.T) {
	var upstreamRequest map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamRequest); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamRequest["stream"] != true {
			t.Fatalf("upstream stream = %#v, want true", upstreamRequest["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []map[string]any{
			{"id": "chatcmpl_stream", "object": "chat.completion.chunk", "model": "actual-upstream-model", "choices": []map[string]any{{"delta": map[string]any{"content": "hello "}}}},
			{"id": "chatcmpl_stream", "object": "chat.completion.chunk", "model": "actual-upstream-model", "choices": []map[string]any{{"delta": map[string]any{"content": "stream"}}}},
			{"id": "chatcmpl_stream", "object": "chat.completion.chunk", "model": "actual-upstream-model", "choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 9, "completion_tokens": 3, "total_tokens": 12}},
		}
		for _, chunk := range chunks {
			b, _ := json.Marshal(chunk)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	providerID := insertTestProviderWithType(t, a, "deepseek", upstream.URL+"/v1")
	localSecret := insertTestLocalKeyWithProtocol(t, a, providerID, true, protocolResponses, protocolChatCompletions)
	insertTestMapping(t, a, providerID, "local-alias-model", "actual-upstream-model")

	mux := http.NewServeMux()
	mux.HandleFunc("/responses", a.serveGateway)
	server := httptest.NewServer(withRecover(mux))
	defer server.Close()

	resp := postJSON(t, server.URL+"/responses", "Bearer "+localSecret, map[string]any{
		"model":  "local-alias-model",
		"input":  "hello",
		"stream": true,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responses stream conversion status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream response: %v", err)
	}
	text := string(body)
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", `"model":"local-alias-model"`, "hello ", "stream", "event: response.completed", "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream response missing %q in:\n%s", want, text)
		}
	}

	var localModel, upstreamModel, usageSource string
	var promptTokens, completionTokens, totalTokens, stream int
	err = a.db.QueryRow(`SELECT local_model,upstream_model,prompt_tokens,completion_tokens,total_tokens,usage_source,stream FROM usage_records LIMIT 1`).
		Scan(&localModel, &upstreamModel, &promptTokens, &completionTokens, &totalTokens, &usageSource, &stream)
	if err != nil {
		t.Fatalf("read usage_records: %v", err)
	}
	if localModel != "local-alias-model" || upstreamModel != "actual-upstream-model" {
		t.Fatalf("usage model fields = local %q upstream %q", localModel, upstreamModel)
	}
	if promptTokens != 9 || completionTokens != 3 || totalTokens != 12 || usageSource != "stream_final" || stream != 1 {
		t.Fatalf("stream usage = %d/%d/%d source=%s stream=%d", promptTokens, completionTokens, totalTokens, usageSource, stream)
	}
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	db, err := openStore(cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	key, err := loadMasterKey(cfg.DataDir)
	if err != nil {
		t.Fatalf("load master key: %v", err)
	}
	return &App{cfg: cfg, db: db, masterKey: key, sessions: map[string]time.Time{}, serveErr: make(chan error, 4)}
}

func insertTestProvider(t *testing.T, a *App, baseURL string) string {
	return insertTestProviderWithType(t, a, "openai_compatible", baseURL)
}

func insertTestProviderWithType(t *testing.T, a *App, providerType, baseURL string) string {
	t.Helper()
	enc, err := a.encryptText("provider-secret")
	if err != nil {
		t.Fatalf("encrypt provider key: %v", err)
	}
	id := "pk_test"
	_, err = a.db.Exec(`INSERT INTO provider_keys(id,name,provider_type,base_url,api_key_enc,proxy_mode,priority,enabled,models,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, "Test Provider", providerType, baseURL, enc, "none", 100, 1, "local-alias-model", now(), now())
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	return id
}

func insertTestLocalKey(t *testing.T, a *App, providerID string) string {
	return insertTestLocalKeyWithProtocol(t, a, providerID, false, "", "")
}

func insertTestLocalKeyWithProtocol(t *testing.T, a *App, providerID string, conversionEnabled bool, clientProtocol, upstreamProtocol string) string {
	t.Helper()
	secret := "myas_test_secret"
	salt := newSalt()
	enc, err := a.encryptText(secret)
	if err != nil {
		t.Fatalf("encrypt local key: %v", err)
	}
	_, err = a.db.Exec(`INSERT INTO local_api_keys(id,name,key_hash,salt,key_enc,provider_key_id,protocol_conversion_enabled,client_protocol,upstream_protocol,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		"lk_test", "Test Local Key", hashSecret(secret, salt), salt, enc, providerID, boolInt(conversionEnabled), clientProtocol, upstreamProtocol, 1, now())
	if err != nil {
		t.Fatalf("insert local key: %v", err)
	}
	return secret
}

func insertTestMapping(t *testing.T, a *App, providerID, localModel, upstreamModel string) {
	t.Helper()
	_, err := a.db.Exec(`INSERT INTO model_mappings(id,local_model,upstream_model,provider_key_id,capability,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		"mm_test", localModel, upstreamModel, providerID, "chat", 1, now(), now())
	if err != nil {
		t.Fatalf("insert mapping: %v", err)
	}
}

func postJSON(t *testing.T, url, auth string, body any) *http.Response {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post json: %v", err)
	}
	return resp
}

func getJSON(t *testing.T, url, auth string, dst any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}
