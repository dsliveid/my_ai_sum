package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ccTestNDJSON 把事件列表编码成 NDJSON 响应体（每行一个 JSON，末行不含分隔符）。
func ccTestNDJSON(events ...map[string]any) string {
	var b strings.Builder
	for _, ev := range events {
		line, _ := json.Marshal(ev)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// ccNewUpstream 启动一个伪装 CommandCode NDJSON 上游，并把出站请求信封回传给 handler。
func ccNewUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, envelope map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ccUpstreamPath {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var envelope map[string]any
		_ = json.NewDecoder(r.Body).Decode(&envelope)
		handler(w, r, envelope)
	}))
}

// ccGateway 用给定上游地址搭建 provider + 本地 Key + mux，返回 localSecret 与网关 URL。
// 不能复用 insertTestProvider* 系列：它们使用固定主键 pk_test / lk_test，
// 而本文件多个用例需要在同一个 App 内共存多个 provider（例如 chat 与 responses 两条客户端协议）。
func ccGateway(t *testing.T, a *App, upstreamURL, providerProtocol, clientProtocol, localModel, upstreamModel string) (string, *httptest.Server) {
	t.Helper()
	providerID := "pk_cc_" + randomID("p")
	enc, err := a.encryptText("provider-secret")
	if err != nil {
		t.Fatalf("encrypt provider key: %v", err)
	}
	if _, err := a.db.Exec(`INSERT INTO provider_keys(id,name,provider_type,base_url,request_protocol,api_key_enc,proxy_mode,enabled,models,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		providerID, "CommandCode Go", "commandcode", upstreamURL, providerProtocol, enc, "none", 1, "", now(), now()); err != nil {
		t.Fatalf("insert cc provider: %v", err)
	}

	secret := "myas_cc_" + randomID("s")
	salt := newSalt()
	keyEnc, err := a.encryptText(secret)
	if err != nil {
		t.Fatalf("encrypt local key: %v", err)
	}
	if clientProtocol == "" {
		clientProtocol = protocolChatCompletions
	}
	if _, err := a.db.Exec(`INSERT INTO local_api_keys(id,name,key_hash,salt,key_enc,provider_key_id,protocol_conversion_enabled,client_protocol,upstream_protocol,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		"lk_cc_"+randomID("k"), "CC Local Key", hashSecret(secret, salt), salt, keyEnc, providerID, 1, clientProtocol, ProtocolCommandCode, 1, now()); err != nil {
		t.Fatalf("insert cc local key: %v", err)
	}
	if localModel != upstreamModel {
		insertTestMapping(t, a, providerID, localModel, upstreamModel)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", a.serveGateway)
	srv := httptest.NewServer(withRecover(mux))
	return secret, srv
}

// TestCCNonStreamAggregatesTextAndEnvelope 覆盖：非流式聚合 text、信封结构、私有请求头、
// 以及 tool 恒为数组 / system 占位符 / params.stream 恒 true 三条铁律。
func TestCCNonStreamAggregatesTextAndEnvelope(t *testing.T) {
	var gotEnvelope map[string]any
	gotHeaders := http.Header{}
	upstream := ccNewUpstream(t, func(w http.ResponseWriter, r *http.Request, envelope map[string]any) {
		gotEnvelope = envelope
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(ccTestNDJSON(
			map[string]any{"type": "start"},
			map[string]any{"type": "start-step"},
			map[string]any{"type": "text-start"},
			map[string]any{"type": "text-delta", "delta": "Hello "},
			map[string]any{"type": "text-delta", "delta": "world"},
			map[string]any{"type": "text-end"},
			map[string]any{"type": "finish-step"},
			map[string]any{"type": "finish", "finishReason": "stop", "totalUsage": map[string]any{
				"inputTokens": 12, "outputTokens": 5,
			}},
		)))
	})
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	secret, srv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolChatCompletions, "go-text-model", "go-text-model")
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/chat/completions", "Bearer "+secret, map[string]any{
		"model":    "go-text-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := make([]byte, 512)
		n, _ := resp.Body.Read(body)
		t.Fatalf("chat status = %d, body = %s", resp.StatusCode, body[:n])
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %#v", out["choices"])
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if got := textFromAny(message["content"]); got != "Hello world" {
		t.Fatalf("aggregated content = %q, want %q", got, "Hello world")
	}

	// 私有请求头必须真实抵达上游。
	for key, want := range map[string]string{
		"X-Command-Code-Version": ccClientVersion,
		"X-Cli-Environment":      "production",
		"X-Taste-Learning":       "false",
	} {
		if got := gotHeaders.Get(key); got != want {
			t.Fatalf("header %s = %q, want %q", key, got, want)
		}
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer provider-secret" {
		t.Fatalf("authorization = %q, want provider secret", got)
	}
	if tp := gotHeaders.Get("Traceparent"); !strings.HasPrefix(tp, "00-") || len(tp) != 55 {
		t.Fatalf("traceparent = %q, want 00-<32hex>-<16hex>-01 (len 55)", tp)
	}
	if sid := gotHeaders.Get("X-Session-Id"); len(sid) != 36 {
		t.Fatalf("x-session-id = %q, want uuid", sid)
	}

	// 信封铁律：params.stream 恒 true、tools 恒为数组、无 system 时注入占位符。
	params, _ := gotEnvelope["params"].(map[string]any)
	if params == nil {
		t.Fatalf("envelope params missing: %#v", gotEnvelope)
	}
	if params["stream"] != true {
		t.Fatalf("params.stream = %v, want true", params["stream"])
	}
	if _, ok := params["tools"].([]any); !ok {
		t.Fatalf("params.tools must be an array, got %#v", params["tools"])
	}
	system, _ := params["system"].([]any)
	if len(system) != 1 {
		t.Fatalf("params.system = %#v, want single placeholder block", params["system"])
	}
	if first, _ := system[0].(map[string]any); textFromAny(first["text"]) != ccPlaceholderSystem {
		t.Fatalf("system placeholder = %#v, want %q", first["text"], ccPlaceholderSystem)
	}
	if got := intFromAny(params["max_tokens"]); got != ccDefaultMaxTokens {
		t.Fatalf("params.max_tokens = %d, want %d", got, ccDefaultMaxTokens)
	}
	if gotEnvelope["mode"] != "agent" || gotEnvelope["permissionMode"] != "standard" {
		t.Fatalf("envelope fixed fields wrong: %#v", gotEnvelope)
	}

	// 用量必须落库（prompt 不减缓存；此处无缓存）。
	var pt, ct, tt int
	if err := a.db.QueryRow(`SELECT prompt_tokens, completion_tokens, total_tokens FROM usage_records LIMIT 1`).Scan(&pt, &ct, &tt); err != nil {
		t.Fatalf("read usage_records: %v", err)
	}
	if pt != 12 || ct != 5 || tt != 17 {
		t.Fatalf("usage = %d/%d/%d, want 12/5/17", pt, ct, tt)
	}
}

// TestCCStreamTextDeltaMapsToSSE 覆盖：流式 text-delta → 客户端 chat SSE，且本层补齐 [DONE]。
func TestCCStreamTextDeltaMapsToSSE(t *testing.T) {
	upstream := ccNewUpstream(t, func(w http.ResponseWriter, r *http.Request, envelope map[string]any) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(ccTestNDJSON(
			map[string]any{"type": "text-delta", "delta": "abc"},
			map[string]any{"type": "text-delta", "delta": "def"},
			map[string]any{"type": "finish", "finishReason": "stop", "totalUsage": map[string]any{"inputTokens": 3, "outputTokens": 2}},
		)))
	})
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	secret, srv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolChatCompletions, "go-stream-model", "go-stream-model")
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/chat/completions", "Bearer "+secret, map[string]any{
		"model":    "go-stream-model",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want event-stream", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	stream := string(raw)
	if !strings.Contains(stream, `"content":"abc"`) || !strings.Contains(stream, `"content":"def"`) {
		t.Fatalf("stream must carry both text deltas:\n%s", stream)
	}
	if !strings.Contains(stream, `"finish_reason":"stop"`) {
		t.Fatalf("stream must carry finish_reason:\n%s", stream)
	}
	if !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("gateway must append missing [DONE]:\n%s", stream)
	}
	// 不允许透传上游原始 NDJSON 事件名。
	if strings.Contains(stream, "text-delta") {
		t.Fatalf("upstream NDJSON event leaked to client:\n%s", stream)
	}
}

// TestCCStreamResponsesPath 覆盖第 4 条路径：客户端 responses 协议的流式输出。
func TestCCStreamResponsesPath(t *testing.T) {
	upstream := ccNewUpstream(t, func(w http.ResponseWriter, r *http.Request, envelope map[string]any) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(ccTestNDJSON(
			map[string]any{"type": "text-delta", "delta": "resp text"},
			map[string]any{"type": "finish", "finishReason": "stop", "totalUsage": map[string]any{"inputTokens": 4, "outputTokens": 2}},
		)))
	})
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	secret, srv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolResponses, "go-resp-model", "go-resp-model")
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/responses", "Bearer "+secret, map[string]any{
		"model":  "go-resp-model",
		"stream": true,
		"input":  "hi",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responses stream status = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	stream := string(raw)
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed", "data: [DONE]"} {
		if !strings.Contains(stream, want) {
			t.Fatalf("responses stream missing %q:\n%s", want, stream)
		}
	}
	if !strings.Contains(stream, "resp text") {
		t.Fatalf("responses stream missing text delta:\n%s", stream)
	}
}

// TestCCResponsesNonStreamBody 覆盖 responses 非流式输出形态。
func TestCCResponsesNonStreamBody(t *testing.T) {
	upstream := ccNewUpstream(t, func(w http.ResponseWriter, r *http.Request, envelope map[string]any) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(ccTestNDJSON(
			map[string]any{"type": "text-delta", "delta": "plain answer"},
			map[string]any{"type": "finish", "finishReason": "stop", "totalUsage": map[string]any{"inputTokens": 6, "outputTokens": 3}},
		)))
	})
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	secret, srv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolResponses, "go-resp-plain", "go-resp-plain")
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/responses", "Bearer "+secret, map[string]any{
		"model": "go-resp-plain",
		"input": "hi",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := textFromAny(out["output_text"]); got != "plain answer" {
		t.Fatalf("output_text = %q", got)
	}
	usage, _ := out["usage"].(map[string]any)
	if intFromAny(usage["input_tokens"]) != 6 || intFromAny(usage["output_tokens"]) != 3 {
		t.Fatalf("responses usage = %#v", usage)
	}
}

// TestCCToolCallAggregation 覆盖工具调用聚合（含 tool-input-delta 分片拼接与别名归一）。
func TestCCToolCallAggregation(t *testing.T) {
	upstream := ccNewUpstream(t, func(w http.ResponseWriter, r *http.Request, envelope map[string]any) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(ccTestNDJSON(
			map[string]any{"type": "tool-input-start", "toolCallId": "call_1", "toolName": "bash_output"},
			map[string]any{"type": "tool-input-delta", "toolCallId": "call_1", "delta": `{"cmd":"`},
			map[string]any{"type": "tool-input-delta", "toolCallId": "call_1", "delta": `ls"}`},
			map[string]any{"type": "tool-input-end", "toolCallId": "call_1"},
			map[string]any{"type": "tool-call", "toolCallId": "call_1", "toolName": "bash_output"},
			map[string]any{"type": "finish", "finishReason": "tool_use", "totalUsage": map[string]any{"inputTokens": 9, "outputTokens": 4}},
		)))
	})
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	secret, srv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolChatCompletions, "go-tool-model", "go-tool-model")
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/chat/completions", "Bearer "+secret, map[string]any{
		"model":    "go-tool-model",
		"messages": []map[string]any{{"role": "user", "content": "list files"}},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":       "bash_output",
				"parameters": map[string]any{"type": "object"},
			},
		}},
		"tool_choice": "required",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tool status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choices, _ := out["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v, want one call", message["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	// bash_output 必须被归一成 shell_output。
	if got := stringFromAny(fn["name"], ""); got != "shell_output" {
		t.Fatalf("tool name = %q, want shell_output", got)
	}
	if got := stringFromAny(fn["arguments"], ""); !strings.Contains(got, `"cmd"`) {
		t.Fatalf("tool arguments = %q, want accumulated JSON", got)
	}
	if got := stringFromAny(choice["finish_reason"], ""); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got)
	}
}

// TestCCErrorStatusMapping 覆盖错误码映射：402→429(Retry-After)、403→401、422→400。
func TestCCErrorStatusMapping(t *testing.T) {
	cases := []struct {
		name          string
		upstream      int
		wantStatus    int
		wantRetryHdr  string
		wantAuthError bool
	}{
		{name: "quota_exhausted_402_to_429", upstream: 402, wantStatus: http.StatusTooManyRequests, wantRetryHdr: "30"},
		{name: "forbidden_403_to_401", upstream: 403, wantStatus: http.StatusUnauthorized},
		{name: "unprocessable_422_to_400", upstream: 422, wantStatus: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := ccNewUpstream(t, func(w http.ResponseWriter, r *http.Request, envelope map[string]any) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstream)
				_, _ = w.Write([]byte(`{"errorText":"upstream rejected: quota"}`))
			})
			defer upstream.Close()

			a := newTestApp(t)
			defer a.db.Close()
			secret, srv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolChatCompletions, "go-err-model", "go-err-model")
			defer srv.Close()

			resp := postJSON(t, srv.URL+"/v1/chat/completions", "Bearer "+secret, map[string]any{
				"model":    "go-err-model",
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			})
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantRetryHdr != "" {
				if got := resp.Header.Get("Retry-After"); got != tc.wantRetryHdr {
					t.Fatalf("retry-after = %q, want %q", got, tc.wantRetryHdr)
				}
			}
			// 失败分支必须落库（此前是统计缺口）。
			var status, success int
			if err := a.db.QueryRow(`SELECT status_code, success FROM request_logs LIMIT 1`).Scan(&status, &success); err != nil {
				t.Fatalf("read request_logs: %v", err)
			}
			if status != tc.wantStatus || success != 0 {
				t.Fatalf("request_logs = status %d success %d", status, success)
			}
		})
	}
}

// TestCCFinishReasonNormalization 覆盖 finishReason 归一表与「other 且无 raw」判截断。
func TestCCFinishReasonNormalization(t *testing.T) {
	cases := []struct {
		finish   string
		raw      string
		want     string
		truncate bool
	}{
		{finish: "tool_use", want: "tool_calls"},
		{finish: "tool-calls", want: "tool_calls"},
		{finish: "max_tokens", want: "length"},
		{finish: "model_context_window_exceeded", want: "length"},
		{finish: "pause_turn", want: "length"},
		{finish: "network", want: "upstream_error"},
		{finish: "upstream-error", want: "upstream_error"},
		{finish: "stop", want: "stop"},
		{finish: "other", want: "upstream_error", truncate: true},
		{finish: "other", raw: "end_turn", want: "stop"},
		{finish: "other", raw: "max_tokens", want: "length"},
	}
	for _, tc := range cases {
		got, truncated := ccNormalizeFinishReason(tc.finish, tc.raw)
		if got != tc.want || truncated != tc.truncate {
			t.Fatalf("ccNormalizeFinishReason(%q,%q) = (%q,%v), want (%q,%v)", tc.finish, tc.raw, got, truncated, tc.want, tc.truncate)
		}
	}
}

// TestCCTruncatedWithoutRawReasonReturns502 覆盖「finishReason=other 且无 raw → 502 upstream_error」。
func TestCCTruncatedWithoutRawReasonReturns502(t *testing.T) {
	upstream := ccNewUpstream(t, func(w http.ResponseWriter, r *http.Request, envelope map[string]any) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(ccTestNDJSON(
			map[string]any{"type": "text-delta", "delta": "partial"},
			map[string]any{"type": "finish", "finishReason": "other"},
		)))
	})
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	secret, srv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolChatCompletions, "go-trunc-model", "go-trunc-model")
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/chat/completions", "Bearer "+secret, map[string]any{
		"model":    "go-trunc-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for truncated response", resp.StatusCode)
	}
	var status, success int
	var errMsg string
	if err := a.db.QueryRow(`SELECT status_code, success, error_message FROM request_logs LIMIT 1`).Scan(&status, &success, &errMsg); err != nil {
		t.Fatalf("read request_logs: %v", err)
	}
	if status != http.StatusBadGateway || success != 0 || !strings.Contains(errMsg, "truncated") {
		t.Fatalf("request_logs = %d/%d/%q", status, success, errMsg)
	}
}

// TestCCUsageAndCachedTokens 覆盖 usage / cached_tokens 落位与 outputTokens==0 清零规则。
func TestCCUsageAndCachedTokens(t *testing.T) {
	upstream := ccNewUpstream(t, func(w http.ResponseWriter, r *http.Request, envelope map[string]any) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(ccTestNDJSON(
			map[string]any{"type": "text-delta", "delta": "cached answer"},
			map[string]any{"type": "finish", "finishReason": "stop", "totalUsage": map[string]any{
				"inputTokens":       1000,
				"outputTokens":      20,
				"cachedInputTokens": 768,
				"inputTokenDetails": map[string]any{"noCacheTokens": 232, "cacheReadTokens": 768, "cacheWriteTokens": 0},
			}},
		)))
	})
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	secret, srv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolChatCompletions, "go-cache-model", "go-cache-model")
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/chat/completions", "Bearer "+secret, map[string]any{
		"model":    "go-cache-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var pt, ct, tt, cached int
	if err := a.db.QueryRow(`SELECT prompt_tokens, completion_tokens, total_tokens, cached_tokens FROM usage_records LIMIT 1`).
		Scan(&pt, &ct, &tt, &cached); err != nil {
		t.Fatalf("read usage_records: %v", err)
	}
	// OpenAI 语义：prompt_tokens 不减缓存（1000），cached_tokens 单独记 768。
	if pt != 1000 || ct != 20 || tt != 1020 || cached != 768 {
		t.Fatalf("usage = pt %d ct %d tt %d cached %d, want 1000/20/1020/768", pt, ct, tt, cached)
	}

	// outputTokens==0 时 input/cached 必须一并清零，防止误计费。
	zero := ccUsageFromAny(map[string]any{"inputTokens": 500, "outputTokens": 0, "cachedInputTokens": 400})
	if got := zero.promptTokens(); got != 0 {
		t.Fatalf("promptTokens with zero output = %d, want 0", got)
	}
	if got := zero.cachedTokens(); got != 0 {
		t.Fatalf("cachedTokens with zero output = %d, want 0", got)
	}
}

// TestCCRejectsUnsupportedResponseFormat 覆盖 response_format / text.format 非 text 与
// previous_response_id 的 400 拒绝逻辑（且必须落库、不得发出上游请求）。
func TestCCRejectsUnsupportedResponseFormat(t *testing.T) {
	upstreamCalled := false
	upstream := ccNewUpstream(t, func(w http.ResponseWriter, r *http.Request, envelope map[string]any) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	})
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	chatSecret, chatSrv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolChatCompletions, "go-reject-model", "go-reject-model")
	defer chatSrv.Close()
	respSecret, respSrv := ccGateway(t, a, upstream.URL, ProtocolCommandCode, protocolResponses, "go-reject-resp", "go-reject-resp")
	defer respSrv.Close()

	t.Run("chat_response_format_json_object", func(t *testing.T) {
		resp := postJSON(t, chatSrv.URL+"/v1/chat/completions", "Bearer "+chatSecret, map[string]any{
			"model":           "go-reject-model",
			"messages":        []map[string]any{{"role": "user", "content": "hi"}},
			"response_format": map[string]any{"type": "json_object"},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("chat_response_format_text_allowed", func(t *testing.T) {
		resp := postJSON(t, chatSrv.URL+"/v1/chat/completions", "Bearer "+chatSecret, map[string]any{
			"model":           "go-reject-model",
			"messages":        []map[string]any{{"role": "user", "content": "hi"}},
			"response_format": map[string]any{"type": "text"},
		})
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusBadRequest {
			t.Fatalf("text response_format must be accepted, got 400")
		}
	})
	t.Run("responses_text_format_json_schema", func(t *testing.T) {
		resp := postJSON(t, respSrv.URL+"/v1/responses", "Bearer "+respSecret, map[string]any{
			"model": "go-reject-resp",
			"input": "hi",
			"text":  map[string]any{"format": map[string]any{"type": "json_schema", "name": "x"}},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("responses_previous_response_id", func(t *testing.T) {
		resp := postJSON(t, respSrv.URL+"/v1/responses", "Bearer "+respSecret, map[string]any{
			"model":                "go-reject-resp",
			"input":                "hi",
			"previous_response_id": "resp_123",
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
	_ = upstreamCalled
}

// TestCCListModelsFallbackToStaticGoPlan 验证 provider 未配置 models 时回退静态 Go 套餐清单，
// 且绝不能调用上游 /provider/v1/models。
func TestCCListModelsFallbackToStaticGoPlan(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()

	a := newTestApp(t)
	defer a.db.Close()
	providerID := insertTestProviderWithProtocol(t, a, "commandcode", upstream.URL, ProtocolCommandCode)
	// 清空 models，触发静态回退。
	if _, err := a.db.Exec(`UPDATE provider_keys SET models='' WHERE id=?`, providerID); err != nil {
		t.Fatalf("clear provider models: %v", err)
	}
	provider, err := a.getProvider(providerID, true)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	models, err := a.listModelsForLocalKey(LocalAPIKey{ProviderKeyID: provider.ID})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) != len(ccGoPlanModelIDs()) {
		t.Fatalf("static model count = %d, want %d", len(models), len(ccGoPlanModelIDs()))
	}
	if len(models) != 46 {
		t.Fatalf("Go plan static model count = %d, want 46", len(models))
	}
	if upstreamCalls != 0 {
		t.Fatalf("gateway must not call upstream /provider/v1/models, got %d calls", upstreamCalls)
	}
	// provider 已配置 models 时必须优先使用其配置值。
	if _, err := a.db.Exec(`UPDATE provider_keys SET models='custom-a,custom-b' WHERE id=?`, providerID); err != nil {
		t.Fatalf("set provider models: %v", err)
	}
	models, err = a.listModelsForLocalKey(LocalAPIKey{ProviderKeyID: provider.ID})
	if err != nil {
		t.Fatalf("list models configured: %v", err)
	}
	if len(models) != 2 || models[0]["id"] != "custom-a" {
		t.Fatalf("configured models = %#v, want provider models", models)
	}
}

// TestCCEnvelopeMessagesAndTools 覆盖信封内 messages / tools / tool_choice 的结构铁律。
func TestCCEnvelopeMessagesAndTools(t *testing.T) {
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "run it"},
			map[string]any{
				"role":    "assistant",
				"content": "ok",
				"tool_calls": []any{map[string]any{
					"id":       "call_9",
					"type":     "function",
					"function": map[string]any{"name": "tool_search", "arguments": `{"q":"x"}`},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_9", "content": "result"},
		},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "tool_search", "description": "d", "parameters": map[string]any{"type": "object"}},
		}},
		"tool_choice": "required",
		"max_tokens":  300000,
	}
	env, err := ccBodyFromChat(body, "m")
	if err != nil {
		t.Fatalf("ccBodyFromChat: %v", err)
	}
	params, _ := env["params"].(map[string]any)
	if got := intFromAny(params["max_tokens"]); got != ccMaxTokensLimit {
		t.Fatalf("max_tokens = %d, want clamped %d", got, ccMaxTokensLimit)
	}
	// tool_choice required → {type:any}
	tc, _ := params["tool_choice"].(map[string]any)
	if stringFromAny(tc["type"], "") != "any" {
		t.Fatalf("tool_choice = %#v, want type any", tc)
	}
	// tools 元素无 type:"function" 外壳，schema 字段名必须是 input_schema。
	tools, _ := params["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["input_schema"] == nil {
		t.Fatalf("tools = %#v", params["tools"])
	}
	if _, has := tools[0]["type"]; has {
		t.Fatalf("tool element must not carry type field: %#v", tools[0])
	}
	if got := stringFromAny(tools[0]["name"], ""); got != "search_tools" {
		t.Fatalf("tool name alias = %q, want search_tools", got)
	}
	// system 必须从 messages 中移除并折叠进 params.system；tool 消息 toolName 由 call id 反查回填。
	messages, _ := params["messages"].([]map[string]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v, want 3 (system folded)", messages)
	}
	for _, m := range messages {
		if stringFromAny(m["role"], "") == "system" {
			t.Fatalf("system message must be folded into params.system")
		}
	}
	toolMsg := messages[2]
	content, _ := toolMsg["content"].([]map[string]any)
	if len(content) != 1 {
		t.Fatalf("tool content = %#v", toolMsg["content"])
	}
	if got := stringFromAny(content[0]["toolName"], ""); got != "search_tools" {
		t.Fatalf("tool-result toolName = %q, want search_tools (reverse resolved from tool_call_id)", got)
	}
	if got := stringFromAny(content[0]["toolCallId"], ""); got != "call_9" {
		t.Fatalf("tool-result toolCallId = %q", got)
	}
	assistantBlocks, _ := messages[1]["content"].([]map[string]any)
	if len(assistantBlocks) < 2 || stringFromAny(assistantBlocks[0]["type"], "") != "text" {
		t.Fatalf("assistant blocks = %#v, want text then tool-call", assistantBlocks)
	}
	last := assistantBlocks[len(assistantBlocks)-1]
	if stringFromAny(last["type"], "") != "tool-call" || last["input"] == nil {
		t.Fatalf("assistant tool-call block = %#v", last)
	}
	systemBlocks, _ := params["system"].([]map[string]any)
	if len(systemBlocks) != 1 || textFromAny(systemBlocks[0]["text"]) != "be brief" {
		t.Fatalf("params.system = %#v", params["system"])
	}
}

// TestCCSlugifyAndSessionReuse 覆盖 x-project-slug 生成与 session 在 TTL 内复用。
func TestCCSlugifyAndSessionReuse(t *testing.T) {
	if got := slugifyProjectPath("My AI Sum / Project"); got != "my-ai-sum-project" {
		t.Fatalf("slug = %q", got)
	}
	if got := slugifyProjectPath("!!!"); got != "my-ai-sum" {
		t.Fatalf("empty slug fallback = %q", got)
	}
	first := ccSessionFor("pk_session_test")
	second := ccSessionFor("pk_session_test")
	if first != second {
		t.Fatalf("session must be reused within TTL: %q vs %q", first, second)
	}
	if len(first) != 36 {
		t.Fatalf("session id = %q, want uuid", first)
	}
	if other := ccSessionFor("pk_other"); other == first {
		t.Fatalf("different providers must get different session ids")
	}
	// 人为让缓存条目过期，必须换新值。
	ccSessionCache.Lock()
	ccSessionCache.m["pk_session_test"] = ccSessionEntry{id: first, expires: time.Now().Add(-time.Second)}
	ccSessionCache.Unlock()
	if got := ccSessionFor("pk_session_test"); got == first {
		t.Fatalf("expired session must be regenerated")
	}
}
