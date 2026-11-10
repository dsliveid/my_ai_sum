package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProviderHTTPClientReusesConnections 是缓存命中率修复的核心回归测试。
//
// 背景：上游若为多副本集群，负载均衡普遍按「连接」分发。若网关每个请求都新建
// http.Client/Transport，则每个请求都会重新建连，等于每次向负载均衡重新抽签，
// 同一会话的请求可能落到不同副本，导致上游 KV Cache 无法命中、命中率断崖式下跌。
//
// 本测试通过记录服务端看到的客户端源端口来判定连接是否被复用：
// 同一底层 TCP 连接上的多次请求，RemoteAddr（含源端口）必须完全一致。
func TestProviderHTTPClientReusesConnections(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	remoteAddrs := make([]string, 0, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteAddrs = append(remoteAddrs, r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	provider := ProviderKey{ID: "pk-reuse", ProxyMode: "none"}
	client, err := a.httpClientForProvider(provider)
	if err != nil {
		t.Fatalf("httpClientForProvider: %v", err)
	}
	// 第二次调用必须返回同一个 client 实例（缓存生效）。
	client2, err := a.httpClientForProvider(provider)
	if err != nil {
		t.Fatalf("httpClientForProvider second call: %v", err)
	}
	if client != client2 {
		t.Fatalf("httpClientForProvider returned different clients for same provider; connection pool would be split")
	}

	for i := 0; i < 4; i++ {
		resp, err := client.Get(srv.URL + "/ping")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		// 必须读完并关闭 body，连接才会被放回空闲池复用。
		_, _ = resp.Body.Read(make([]byte, 64))
		resp.Body.Close()
	}

	if len(remoteAddrs) != 4 {
		t.Fatalf("server saw %d requests, want 4", len(remoteAddrs))
	}
	for i, addr := range remoteAddrs {
		if addr != remoteAddrs[0] {
			t.Fatalf("connection not reused: request %d came from %s, request 0 from %s; upstream cache would miss", i, addr, remoteAddrs[0])
		}
	}
}

// TestProviderHTTPClientCacheKeyedByProxyConfig 确保代理配置变化时会得到不同 client，
// 避免「改了代理配置却继续复用旧连接」而走错出口。
func TestProviderHTTPClientCacheKeyedByProxyConfig(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	base, err := a.httpClientForProvider(ProviderKey{ID: "pk-key", ProxyMode: "none"})
	if err != nil {
		t.Fatalf("httpClientForProvider none: %v", err)
	}
	custom, err := a.httpClientForProvider(ProviderKey{ID: "pk-key", ProxyMode: "custom", ProxyID: "px-1"})
	if err != nil {
		t.Fatalf("httpClientForProvider custom: %v", err)
	}
	if base == custom {
		t.Fatalf("clients for different proxy modes must not be shared")
	}

	// 失效后应重新构建 client。
	a.invalidateProviderClients()
	after, err := a.httpClientForProvider(ProviderKey{ID: "pk-key", ProxyMode: "none"})
	if err != nil {
		t.Fatalf("httpClientForProvider after invalidate: %v", err)
	}
	if after == base {
		t.Fatalf("client cache was not invalidated")
	}
}

// TestParseCacheUsage 覆盖上游三种缓存用量字段形态。
func TestParseCacheUsage(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantCached   int
		wantCreation int
	}{
		{
			name:       "openai prompt_tokens_details",
			body:       `{"usage":{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":768}}}`,
			wantCached: 768,
		},
		{
			name:       "openai input_tokens_details",
			body:       `{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":512}}}`,
			wantCached: 512,
		},
		{
			name:       "flat cached_tokens",
			body:       `{"usage":{"prompt_tokens":1000,"cached_tokens":640}}`,
			wantCached: 640,
		},
		{
			name:         "anthropic cache fields",
			body:         `{"usage":{"input_tokens":1000,"cache_read_input_tokens":900,"cache_creation_input_tokens":128}}`,
			wantCached:   900,
			wantCreation: 128,
		},
		{
			name:       "no cache info",
			body:       `{"usage":{"prompt_tokens":1000,"completion_tokens":20}}`,
			wantCached: 0,
		},
		{
			name:       "invalid json tolerated",
			body:       `not-json`,
			wantCached: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cached, creation := parseCacheUsage([]byte(tc.body))
			if cached != tc.wantCached || creation != tc.wantCreation {
				t.Fatalf("parseCacheUsage = (%d,%d), want (%d,%d)", cached, creation, tc.wantCached, tc.wantCreation)
			}
		})
	}
}

// TestSaveRequestAndUsagePersistsCachedTokens 验证缓存用量确实落库，
// 否则命中率仍不可观测，修复效果无法度量。
func TestSaveRequestAndUsagePersistsCachedTokens(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	res := forwardResult{
		RequestID:           "req-cache-1",
		StatusCode:          200,
		Success:             true,
		PromptTokens:        1000,
		CompletionTokens:    50,
		TotalTokens:         1050,
		CachedTokens:        768,
		CacheCreationTokens: 64,
		UsageSource:         "upstream",
	}
	a.saveRequestAndUsage(res, "client_api", ProviderKey{ID: "pk-1", ProviderType: "openai"}, "lk-1", "local-model", "upstream-model", false, req)

	var cached, creation, prompt int
	if err := a.db.QueryRow(`SELECT cached_tokens, cache_creation_tokens, prompt_tokens FROM usage_records WHERE request_id=?`, "req-cache-1").
		Scan(&cached, &creation, &prompt); err != nil {
		t.Fatalf("query usage_records: %v", err)
	}
	if cached != 768 || creation != 64 || prompt != 1000 {
		t.Fatalf("usage_records cached=%d creation=%d prompt=%d, want 768/64/1000", cached, creation, prompt)
	}

	var logCached int
	if err := a.db.QueryRow(`SELECT cached_tokens FROM request_logs WHERE id=?`, "req-cache-1").Scan(&logCached); err != nil {
		t.Fatalf("query request_logs: %v", err)
	}
	if logCached != 768 {
		t.Fatalf("request_logs cached_tokens=%d, want 768", logCached)
	}
}

// TestWithCacheUsageFillsForwardResult 验证 withCacheUsage 正确补充字段。
func TestWithCacheUsageFillsForwardResult(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"usage": map[string]any{
			"prompt_tokens":         2000,
			"prompt_tokens_details": map[string]any{"cached_tokens": 1536},
		},
	})
	res := withCacheUsage(forwardResult{PromptTokens: 2000}, body)
	if res.CachedTokens != 1536 {
		t.Fatalf("CachedTokens = %d, want 1536", res.CachedTokens)
	}
	if res.PromptTokens != 2000 {
		t.Fatalf("PromptTokens = %d, want 2000 (must not be altered)", res.PromptTokens)
	}
}

// TestForwardInboundHeadersWhitelist 验证入站请求头按白名单透传：
// 会话标识等缓存相关头必须透传（否则上游粘性路由收不到信号），
// 而 Authorization / Host / Cookie 等必须由网关接管，不得透传。
func TestForwardInboundHeadersWhitelist(t *testing.T) {
	in := http.Header{}
	in.Set("X-Session-Id", "sess-abc")
	in.Set("X-Opencode-Session", "oc-1")
	in.Set("Prompt-Cache-Key", "cache-key-1")
	in.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
	in.Set("Authorization", "Bearer client-secret")
	in.Set("Cookie", "session=secret")
	in.Set("Content-Length", "999")
	in.Set("Host", "evil.example.com")

	out := forwardInboundHeaders(in)
	for _, key := range []string{"X-Session-Id", "X-Opencode-Session", "Prompt-Cache-Key", "Anthropic-Beta"} {
		if out.Get(key) == "" {
			t.Fatalf("header %s must be forwarded to upstream", key)
		}
	}
	for _, key := range []string{"Authorization", "Cookie", "Content-Length", "Host"} {
		if out.Get(key) != "" {
			t.Fatalf("header %s must NOT be forwarded, got %q", key, out.Get(key))
		}
	}
	// 无任何命中时返回 nil，避免无谓分配。
	if got := forwardInboundHeaders(http.Header{}); got != nil {
		t.Fatalf("empty headers should return nil, got %v", got)
	}
	if got := forwardInboundHeaders(nil); got != nil {
		t.Fatalf("nil headers should return nil, got %v", got)
	}
}

// TestDoUpstreamForwardsSessionHeaderAndKeepsProviderAuth 端到端验证：
// 上游必须收到 provider 密钥（而非客户端密钥）以及透传的会话标识。
func TestDoUpstreamForwardsSessionHeaderAndKeepsProviderAuth(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	var gotAuth, gotSession, gotContentType, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("X-Session-Id")
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	provider := ProviderKey{ID: "pk-up", BaseURL: srv.URL, ProxyMode: "none", APIKey: "provider-secret"}
	in := http.Header{}
	in.Set("X-Session-Id", "sess-xyz")
	in.Set("Authorization", "Bearer client-should-be-replaced")

	resp, url, upErr := a.doUpstream(context.Background(), provider, "/chat/completions", []byte(`{"model":"m"}`), true, in)
	if upErr != nil {
		t.Fatalf("doUpstream: %v", upErr)
	}
	defer resp.Body.Close()

	if url != srv.URL+"/chat/completions" {
		t.Fatalf("url = %q, want %q", url, srv.URL+"/chat/completions")
	}
	if gotAuth != "Bearer provider-secret" {
		t.Fatalf("upstream Authorization = %q, want provider secret", gotAuth)
	}
	if gotSession != "sess-xyz" {
		t.Fatalf("session header not forwarded: %q", gotSession)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q", gotContentType)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream for streaming", gotAccept)
	}
}

// TestDoUpstreamReportsBadBaseURL 验证构造失败时返回语义化错误码，
// 使调用方能落库而不是静默丢统计。
func TestDoUpstreamReportsBadBaseURL(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	provider := ProviderKey{ID: "pk-bad", BaseURL: "://bad-url", ProxyMode: "none", APIKey: "k"}
	_, _, upErr := a.doUpstream(context.Background(), provider, "/chat/completions", []byte(`{}`), false, nil)
	if upErr == nil {
		t.Fatalf("expected error for bad base url")
	}
	if upErr.StatusCode != 500 {
		t.Fatalf("StatusCode = %d, want 500", upErr.StatusCode)
	}
}

// TestResolveUsagePrefersUpstreamAndFallsBackToEstimate 验证 usage 取值语义：
// 上游有真实 usage 时不做估算；缺失时才估算并标记 estimated；缓存字段始终被解析。
func TestResolveUsagePrefersUpstreamAndFallsBackToEstimate(t *testing.T) {
	estimateCalled := false
	promptEstimate := func() int { estimateCalled = true; return 111 }
	completionText := func() string { return "hello world" }

	body := []byte(`{"usage":{"prompt_tokens":500,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":400}}}`)
	pt, ct, tt, cached, _, source := resolveUsage(body, 200, promptEstimate, completionText)
	if estimateCalled {
		t.Fatalf("estimate must not run when upstream usage present")
	}
	if pt != 500 || ct != 20 || tt != 520 || cached != 400 || source != "upstream" {
		t.Fatalf("got pt=%d ct=%d tt=%d cached=%d source=%s", pt, ct, tt, cached, source)
	}

	// 无 usage 时回落到估算。
	pt, ct, tt, cached, _, source = resolveUsage([]byte(`{"choices":[]}`), 200, promptEstimate, completionText)
	if !estimateCalled {
		t.Fatalf("estimate should run when usage missing")
	}
	if source != "estimated" || pt != 111 || cached != 0 || tt != pt+ct {
		t.Fatalf("fallback got pt=%d ct=%d tt=%d cached=%d source=%s", pt, ct, tt, cached, source)
	}

	// 错误响应不估算。
	estimateCalled = false
	_, _, _, _, _, source = resolveUsage([]byte(`{"error":"boom"}`), 500, promptEstimate, completionText)
	if estimateCalled || source != "missing" {
		t.Fatalf("must not estimate for error status, source=%s", source)
	}
}

// TestFinishForwardAlwaysRecords 验证统一收口确实写入了统计与调试日志，
// 这是「任何分支都不漏统计」纪律的回归保障。
func TestFinishForwardAlwaysRecords(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	fctx := forwardCtx{
		Source: "client_api", Provider: ProviderKey{ID: "pk-f", ProviderType: "openai"},
		LocalKeyID: "lk-f", LocalModel: "lm", UpstreamModel: "um",
		Stream: false, Req: req, Payload: []byte(`{"model":"lm"}`), URL: "http://up/v1/chat/completions",
	}
	res := a.finish(fctx, forwardResult{RequestID: "req-finish-1", StatusCode: 502, Success: false, ErrorMessage: "boom"}, nil)

	if res.RequestID != "req-finish-1" {
		t.Fatalf("finish must return the same result")
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE id=?`, "req-finish-1").Scan(&count); err != nil {
		t.Fatalf("query request_logs: %v", err)
	}
	if count != 1 {
		t.Fatalf("request_logs rows = %d, want 1 (error branch must still be recorded)", count)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE request_id=?`, "req-finish-1").Scan(&count); err != nil {
		t.Fatalf("query usage_records: %v", err)
	}
	if count != 1 {
		t.Fatalf("usage_records rows = %d, want 1", count)
	}
}

// TestStreamCacheUsageAccumulatesAcrossChunks 验证流式场景下缓存用量能被逐分片累积。
// 流式响应中缓存数字通常只在最后一个 usage 分片出现，必须跨分片观察才不会丢失。
func TestStreamCacheUsageAccumulatesAcrossChunks(t *testing.T) {
	var acc streamCacheUsage
	chunks := []string{
		`{"id":"chatcmpl-1","choices":[{"delta":{"content":"he"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"delta":{"content":"llo"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":1792}}}`,
	}
	for _, c := range chunks {
		acc.observe([]byte(c))
	}
	res := acc.fill(forwardResult{PromptTokens: 2000})
	if res.CachedTokens != 1792 {
		t.Fatalf("CachedTokens = %d, want 1792", res.CachedTokens)
	}
	// 未出现缓存数字的分片不得把已有值清零。
	acc.observe([]byte(`{"id":"chatcmpl-1","choices":[{"delta":{"content":"!"}}]}`))
	res = acc.fill(forwardResult{PromptTokens: 2000})
	if res.CachedTokens != 1792 {
		t.Fatalf("CachedTokens cleared by later chunk: got %d, want 1792", res.CachedTokens)
	}
}

// TestStreamCacheUsageHandlesResponsesCompletedEvent 验证 responses 风格流式
// （缓存数字嵌套在 response.completed 的 response.usage 内）也能被识别。
func TestStreamCacheUsageHandlesResponsesCompletedEvent(t *testing.T) {
	var acc streamCacheUsage
	acc.observe([]byte(`{"type":"response.output_text.delta","delta":"hi"}`))
	acc.observe([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":3000,"output_tokens":20,"input_tokens_details":{"cached_tokens":2560}}}}`))
	res := acc.fill(forwardResult{PromptTokens: 3000})
	if res.CachedTokens != 2560 {
		t.Fatalf("CachedTokens = %d, want 2560", res.CachedTokens)
	}
}

// TestStreamCacheUsageNilSafe 确认空累积器不会 panic（诊断/容错要求）。
func TestStreamCacheUsageNilSafe(t *testing.T) {
	var acc *streamCacheUsage
	acc.observe([]byte(`{"usage":{"cached_tokens":10}}`))
	res := acc.fill(forwardResult{PromptTokens: 10})
	if res.CachedTokens != 0 {
		t.Fatalf("nil accumulator should not fill, got %d", res.CachedTokens)
	}
}

// TestForwardStreamDiagnosticHeadersWhitelist 验证流式诊断头按白名单透传，
// 且不会把与 SSE 冲突或敏感的响应头带给客户端。
func TestForwardStreamDiagnosticHeadersWhitelist(t *testing.T) {
	upstream := &http.Response{Header: http.Header{}}
	upstream.Header.Set("X-Served-By", "node-a")
	upstream.Header.Set("Server", "nginx")
	upstream.Header.Set("X-Request-Id", "abc-123")
	upstream.Header.Set("X-Model", "gpt-x")
	upstream.Header.Set("Content-Length", "12345")
	upstream.Header.Set("Content-Encoding", "gzip")
	upstream.Header.Set("Set-Cookie", "session=secret")

	rec := httptest.NewRecorder()
	forwardStreamDiagnosticHeaders(rec, upstream)

	for _, key := range []string{"X-Served-By", "Server", "X-Request-Id", "X-Model"} {
		if rec.Header().Get(key) == "" {
			t.Fatalf("diagnostic header %s not forwarded", key)
		}
	}
	for _, key := range []string{"Content-Length", "Content-Encoding", "Set-Cookie"} {
		if rec.Header().Get(key) != "" {
			t.Fatalf("header %s must not be forwarded, got %q", key, rec.Header().Get(key))
		}
	}
	// nil resp 必须安全。
	forwardStreamDiagnosticHeaders(rec, nil)
}
