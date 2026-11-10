package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type forwardResult struct {
	RequestID        string
	StatusCode       int
	LatencyMS        int64
	Success          bool
	ErrorMessage     string
	ResponseSummary  string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedTokens 记录上游返回的缓存命中 token 数（OpenAI: prompt_tokens_details.cached_tokens，
	// Anthropic: cache_read_input_tokens）。缺失时为 0。这是衡量上游 KV Cache 命中率的唯一依据。
	CachedTokens        int
	CacheCreationTokens int
	UsageSource         string
}

type chatToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

type streamedToolCall struct {
	Index     int
	ID        string
	CallID    string
	Name      string
	Arguments string
	Added     bool
}

func (a *App) serveGateway(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models"):
		secret := bearerToken(r)
		localKey, err := a.verifyLocalAPIKey(secret)
		if err != nil {
			writeError(w, 401, "invalid api key")
			return
		}
		models, err := a.listModelsForLocalKey(localKey)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": models})
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/chat/completions"):
		localKey, err := a.verifyLocalAPIKey(bearerToken(r))
		if err != nil {
			writeError(w, 401, "invalid api key")
			return
		}
		a.handleChatCompletion(w, r, "client_api", localKey)
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/responses" || r.URL.Path == "/responses"):
		localKey, err := a.verifyLocalAPIKey(bearerToken(r))
		if err != nil {
			writeError(w, 401, "invalid api key")
			return
		}
		a.handleResponses(w, r, "client_api", localKey)
	default:
		writeError(w, 404, "not found")
	}
}

func (a *App) listModelsForLocalKey(localKey LocalAPIKey) ([]map[string]any, error) {
	if localKey.ProviderKeyID == "" {
		return a.listModelsFromMappings()
	}
	provider, err := a.getProvider(localKey.ProviderKeyID, false)
	if err != nil {
		return nil, err
	}
	data := []map[string]any{}
	for _, model := range splitModels(provider.Models) {
		data = append(data, map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "my_ai_sum"})
	}
	// CommandCode（仅 Go 套餐）没有可用的模型发现接口：/provider/v1/models 对 Go 套餐返回 403，
	// 因此 provider 未配置 models 时必须回退到内置静态清单，绝不能去请求上游。
	if len(data) == 0 && normalizeProtocolName(provider.RequestProtocol) == ProtocolCommandCode {
		return ccGoPlanModelList(), nil
	}
	return data, nil
}

// ccProtocolFor 判断本次转发是否必须走 CommandCode 适配器。
//
// 两条判据：本地 Key 的协议转换目标为 commandcode；或 provider 本身登记为 commandcode
// （此时即使未开启协议转换，也不能按 chat/completions 直通到不存在的端点）。
func ccProtocolFor(upstreamProtocol string, provider ProviderKey) bool {
	if normalizeProtocolName(upstreamProtocol) == ProtocolCommandCode {
		return true
	}
	return normalizeProtocolName(provider.RequestProtocol) == ProtocolCommandCode
}

func (a *App) handleChatCompletion(w http.ResponseWriter, r *http.Request, source string, localKey LocalAPIKey) forwardResult {
	// raw 保留客户端原始字节，直通路径用它做 model 定点替换以保住 KV Cache 前缀。
	body, raw, err := decodeRawJSON(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: err.Error()}
	}
	localModel, _ := body["model"].(string)
	if strings.TrimSpace(localModel) == "" {
		writeError(w, 400, "model is required")
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: "model is required"}
	}
	var provider ProviderKey
	upstreamModel := localModel
	if localKey.ProviderKeyID != "" {
		provider, err = a.getProvider(localKey.ProviderKeyID, true)
		if err != nil {
			writeError(w, 404, "local api key provider not found")
			return forwardResult{StatusCode: 404, Success: false, ErrorMessage: err.Error()}
		}
		if mapping, ok := a.findMappingForProvider(localModel, provider.ID); ok {
			upstreamModel = mapping.UpstreamModel
		}
	} else {
		var mapping ModelMapping
		mapping, provider, err = a.findMapping(localModel)
		if err != nil {
			writeError(w, 404, "no enabled model mapping or provider for "+localModel)
			return forwardResult{StatusCode: 404, Success: false, ErrorMessage: err.Error()}
		}
		upstreamModel = mapping.UpstreamModel
	}
	body["model"] = upstreamModel
	upstreamProtocol, err := localKeyUpstreamProtocol(localKey, protocolChatCompletions)
	if err != nil {
		writeError(w, 400, err.Error())
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: err.Error()}
	}
	switch {
	case ccProtocolFor(upstreamProtocol, provider):
		return a.ccForward(w, r, source, localKey.ID, localModel, upstreamModel, provider, body, protocolChatCompletions)
	case upstreamProtocol == protocolChatCompletions:
		return a.forwardChat(w, r, source, localKey.ID, localModel, upstreamModel, provider, body, raw)
	case upstreamProtocol == protocolResponses:
		return a.forwardChatViaResponses(w, r, source, localKey.ID, localModel, upstreamModel, provider, body)
	default:
		msg := "unsupported upstream protocol: " + upstreamProtocol
		writeError(w, 400, msg)
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: msg}
	}
}

func (a *App) handleResponses(w http.ResponseWriter, r *http.Request, source string, localKey LocalAPIKey) forwardResult {
	// 同上：raw 供直通路径 model 定点替换使用。
	body, raw, err := decodeRawJSON(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: err.Error()}
	}
	localModel, _ := body["model"].(string)
	if strings.TrimSpace(localModel) == "" {
		writeError(w, 400, "model is required")
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: "model is required"}
	}
	var provider ProviderKey
	upstreamModel := localModel
	if localKey.ProviderKeyID != "" {
		provider, err = a.getProvider(localKey.ProviderKeyID, true)
		if err != nil {
			writeError(w, 404, "local api key provider not found")
			return forwardResult{StatusCode: 404, Success: false, ErrorMessage: err.Error()}
		}
		if mapping, ok := a.findMappingForProvider(localModel, provider.ID); ok {
			upstreamModel = mapping.UpstreamModel
		}
	} else {
		var mapping ModelMapping
		mapping, provider, err = a.findMapping(localModel)
		if err != nil {
			writeError(w, 404, "no enabled model mapping or provider for "+localModel)
			return forwardResult{StatusCode: 404, Success: false, ErrorMessage: err.Error()}
		}
		upstreamModel = mapping.UpstreamModel
	}
	body["model"] = upstreamModel
	upstreamProtocol, err := localKeyUpstreamProtocol(localKey, protocolResponses)
	if err != nil {
		writeError(w, 400, err.Error())
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: err.Error()}
	}
	switch {
	case ccProtocolFor(upstreamProtocol, provider):
		return a.ccForward(w, r, source, localKey.ID, localModel, upstreamModel, provider, body, protocolResponses)
	case upstreamProtocol == protocolResponses:
		return a.forwardResponses(w, r, source, localKey.ID, localModel, upstreamModel, provider, body, raw)
	case upstreamProtocol == protocolChatCompletions:
		return a.forwardResponsesViaChat(w, r, source, localKey.ID, localModel, upstreamModel, provider, body)
	default:
		msg := "unsupported upstream protocol: " + upstreamProtocol
		writeError(w, 400, msg)
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: msg}
	}
}

func (a *App) forwardChat(w http.ResponseWriter, r *http.Request, source, localKeyID, localModel, upstreamModel string, provider ProviderKey, body map[string]any, raw []byte) forwardResult {
	start := time.Now()
	stream, _ := body["stream"].(bool)
	reqID := randomID("req")
	// 直通路径优先透传客户端原始字节（仅定点替换 model），保住上游 KV Cache 的前缀对齐。
	payload := buildDirectPayload(raw, body, upstreamModel)
	fctx := forwardCtx{
		Source: source, Provider: provider, LocalKeyID: localKeyID,
		LocalModel: localModel, UpstreamModel: upstreamModel,
		Stream: stream, Req: r, Payload: payload,
	}
	resp, url, upErr := a.doUpstream(r.Context(), provider, "/chat/completions", payload, stream, r.Header)
	if upErr != nil {
		// 此前的统计缺口：取密钥/建 client/构请求失败只回 500 不落库，现统一收口。
		return a.failForward(w, fctx.withURL(upErr.URL), upErr.StatusCode, upErr.Error())
	}
	defer resp.Body.Close()
	if stream {
		return a.forwardStream(w, r, resp, start, reqID, source, provider, localKeyID, localModel, upstreamModel, body)
	}
	respBody, readErr := readUpstreamBody(resp)
	if readErr != nil {
		return a.failForward(w, fctx.withURL(url), readErr.StatusCode, readErr.Error())
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && looksLikeSSE(resp.Header.Get("Content-Type"), respBody) {
		if responseBody := responsesSSEToResponsesBody(respBody, localModel); responseBody != nil {
			respBody, _ = json.Marshal(responseBody)
		}
	}
	if source == "api_chat_test" && !json.Valid(respBody) {
		statusCode := resp.StatusCode
		if statusCode >= 200 && statusCode < 300 {
			statusCode = http.StatusBadGateway
		}
		contentType := resp.Header.Get("Content-Type")
		preview := summarizeText(string(respBody), 800)
		errType, errMsg := upstreamNonJSONError(url, resp.StatusCode, contentType, respBody)
		res := forwardResult{
			RequestID:       reqID,
			StatusCode:      statusCode,
			LatencyMS:       time.Since(start).Milliseconds(),
			Success:         false,
			ErrorMessage:    errMsg,
			ResponseSummary: preview,
			UsageSource:     "missing",
		}
		a.finish(fctx.withURL(url), res, respBody)
		writeJSON(w, statusCode, map[string]any{
			"error_type":      errType,
			"error":           errMsg,
			"upstream_status": resp.StatusCode,
			"content_type":    contentType,
			"body_preview":    preview,
		})
		return res
	}
	pt, ct, tt, cached, creation, usageSource := resolveUsage(respBody, resp.StatusCode,
		func() int { return estimateTokensFromAny(body["messages"]) },
		func() string { return string(respBody) })
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	errMsg := ""
	if !success {
		errMsg = summarizeText(string(respBody), 500)
	}
	res := forwardResult{
		RequestID:           reqID,
		StatusCode:          resp.StatusCode,
		LatencyMS:           time.Since(start).Milliseconds(),
		Success:             success,
		ErrorMessage:        errMsg,
		ResponseSummary:     summarizeText(string(respBody), 800),
		PromptTokens:        pt,
		CompletionTokens:    ct,
		TotalTokens:         tt,
		CachedTokens:        cached,
		CacheCreationTokens: creation,
		UsageSource:         usageSource,
	}
	a.finish(fctx.withURL(url), res, respBody)
	copyHeader(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	return res
}

func (a *App) forwardChatViaResponses(w http.ResponseWriter, r *http.Request, source, localKeyID, localModel, upstreamModel string, provider ProviderKey, body map[string]any) forwardResult {
	start := time.Now()
	stream, _ := body["stream"].(bool)
	reqID := randomID("req")
	responsesBody := chatBodyToResponsesBody(body)
	responsesBody["model"] = upstreamModel
	responsesBody["stream"] = stream
	payload, _ := json.Marshal(responsesBody)
	fctx := forwardCtx{
		Source: source, Provider: provider, LocalKeyID: localKeyID,
		LocalModel: localModel, UpstreamModel: upstreamModel,
		Stream: stream, Req: r, Payload: payload,
	}
	resp, url, upErr := a.doUpstream(r.Context(), provider, "/responses", payload, stream, r.Header)
	if upErr != nil {
		return a.failForward(w, fctx.withURL(upErr.URL), upErr.StatusCode, upErr.Error())
	}
	defer resp.Body.Close()
	if stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return a.forwardResponsesStreamAsChat(w, r, resp, start, reqID, source, provider, localKeyID, localModel, upstreamModel, body)
	}
	respBody, readErr := readUpstreamBody(resp)
	if readErr != nil {
		return a.failForward(w, fctx.withURL(url), readErr.StatusCode, readErr.Error())
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && looksLikeSSE(resp.Header.Get("Content-Type"), respBody) {
		if responseBody := responsesSSEToResponsesBody(respBody, localModel); responseBody != nil {
			respBody, _ = json.Marshal(responseBody)
		}
	}
	if source == "api_chat_test" && !json.Valid(respBody) {
		statusCode := resp.StatusCode
		if statusCode >= 200 && statusCode < 300 {
			statusCode = http.StatusBadGateway
		}
		contentType := resp.Header.Get("Content-Type")
		preview := summarizeText(string(respBody), 800)
		errType, errMsg := upstreamNonJSONError(url, resp.StatusCode, contentType, respBody)
		res := forwardResult{
			RequestID:       reqID,
			StatusCode:      statusCode,
			LatencyMS:       time.Since(start).Milliseconds(),
			Success:         false,
			ErrorMessage:    errMsg,
			ResponseSummary: preview,
			UsageSource:     "missing",
		}
		a.finish(fctx.withURL(url), res, respBody)
		writeJSON(w, statusCode, map[string]any{
			"error_type":      errType,
			"error":           errMsg,
			"upstream_status": resp.StatusCode,
			"content_type":    contentType,
			"body_preview":    preview,
		})
		return res
	}
	pt, ct, tt, cached, creation, usageSource := resolveUsage(respBody, resp.StatusCode,
		func() int { return estimateTokensFromAny(body["messages"]) },
		func() string { return string(respBody) })
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	errMsg := ""
	if !success {
		errMsg = summarizeText(string(respBody), 500)
	}
	chatBody := responsesBodyToChatCompletionBody(respBody, localModel)
	if chatBody == nil {
		chatBody = map[string]any{"error": "failed to convert responses body to chat completion", "upstream_body": summarizeText(string(respBody), 800)}
	}
	out, _ := json.Marshal(chatBody)
	res := forwardResult{
		RequestID:           reqID,
		StatusCode:          resp.StatusCode,
		LatencyMS:           time.Since(start).Milliseconds(),
		Success:             success,
		ErrorMessage:        errMsg,
		ResponseSummary:     summarizeText(string(out), 800),
		PromptTokens:        pt,
		CompletionTokens:    ct,
		TotalTokens:         tt,
		CachedTokens:        cached,
		CacheCreationTokens: creation,
		UsageSource:         usageSource,
	}
	a.finish(fctx.withURL(url), res, respBody)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	return res
}

func (a *App) forwardResponses(w http.ResponseWriter, r *http.Request, source, localKeyID, localModel, upstreamModel string, provider ProviderKey, body map[string]any, raw []byte) forwardResult {
	start := time.Now()
	stream, _ := body["stream"].(bool)
	reqID := randomID("req")
	// 与 forwardChat 同理：responses 直通路径同样按原始字节透传以保前缀一致。
	payload := buildDirectPayload(raw, body, upstreamModel)
	fctx := forwardCtx{
		Source: source, Provider: provider, LocalKeyID: localKeyID,
		LocalModel: localModel, UpstreamModel: upstreamModel,
		Stream: stream, Req: r, Payload: payload,
	}
	resp, url, upErr := a.doUpstream(r.Context(), provider, "/responses", payload, stream, r.Header)
	if upErr != nil {
		return a.failForward(w, fctx.withURL(upErr.URL), upErr.StatusCode, upErr.Error())
	}
	if stream {
		defer resp.Body.Close()
		return a.forwardStream(w, r, resp, start, reqID, source, provider, localKeyID, localModel, upstreamModel, body)
	}
	defer resp.Body.Close()
	respBody, readErr := readUpstreamBody(resp)
	if readErr != nil {
		return a.failForward(w, fctx.withURL(url), readErr.StatusCode, readErr.Error())
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && looksLikeSSE(resp.Header.Get("Content-Type"), respBody) {
		if responseBody := responsesSSEToResponsesBody(respBody, localModel); responseBody != nil {
			respBody, _ = json.Marshal(responseBody)
		}
	}
	if source == "api_chat_test" && !json.Valid(respBody) {
		statusCode := resp.StatusCode
		if statusCode >= 200 && statusCode < 300 {
			statusCode = http.StatusBadGateway
		}
		contentType := resp.Header.Get("Content-Type")
		preview := summarizeText(string(respBody), 800)
		errType, errMsg := upstreamNonJSONError(url, resp.StatusCode, contentType, respBody)
		res := forwardResult{
			RequestID:       reqID,
			StatusCode:      statusCode,
			LatencyMS:       time.Since(start).Milliseconds(),
			Success:         false,
			ErrorMessage:    errMsg,
			ResponseSummary: preview,
			UsageSource:     "missing",
		}
		a.finish(fctx.withURL(url), res, respBody)
		writeJSON(w, statusCode, map[string]any{
			"error_type":      errType,
			"error":           errMsg,
			"upstream_status": resp.StatusCode,
			"content_type":    contentType,
			"body_preview":    preview,
		})
		return res
	}
	pt, ct, tt, cached, creation, usageSource := resolveUsage(respBody, resp.StatusCode,
		func() int { return estimateResponsesPromptTokens(body) },
		func() string { return string(respBody) })
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	errMsg := ""
	if !success {
		errMsg = summarizeText(string(respBody), 500)
	}
	res := forwardResult{
		RequestID:           reqID,
		StatusCode:          resp.StatusCode,
		LatencyMS:           time.Since(start).Milliseconds(),
		Success:             success,
		ErrorMessage:        errMsg,
		ResponseSummary:     summarizeText(string(respBody), 800),
		PromptTokens:        pt,
		CompletionTokens:    ct,
		TotalTokens:         tt,
		CachedTokens:        cached,
		CacheCreationTokens: creation,
		UsageSource:         usageSource,
	}
	a.finish(fctx.withURL(url), res, respBody)
	copyHeader(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	return res
}

func (a *App) forwardResponsesViaChat(w http.ResponseWriter, r *http.Request, source, localKeyID, localModel, upstreamModel string, provider ProviderKey, body map[string]any) forwardResult {
	start := time.Now()
	reqID := randomID("req")
	wantStream, _ := body["stream"].(bool)
	chatBody := responsesBodyToChatBody(body)
	chatBody["model"] = upstreamModel
	chatBody["stream"] = wantStream
	payload, _ := json.Marshal(chatBody)
	fctx := forwardCtx{
		Source: source, Provider: provider, LocalKeyID: localKeyID,
		LocalModel: localModel, UpstreamModel: upstreamModel,
		Stream: wantStream, Req: r, Payload: payload,
	}
	resp, url, upErr := a.doUpstream(r.Context(), provider, "/chat/completions", payload, wantStream, r.Header)
	if upErr != nil {
		return a.failForward(w, fctx.withURL(upErr.URL), upErr.StatusCode, upErr.Error())
	}
	defer resp.Body.Close()
	if wantStream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return a.forwardChatStreamAsResponses(w, r, resp, start, reqID, source, provider, localKeyID, localModel, upstreamModel, body)
	}
	respBody, readErr := readUpstreamBody(resp)
	if readErr != nil {
		return a.failForward(w, fctx.withURL(url), readErr.StatusCode, readErr.Error())
	}
	pt, ct, tt, cached, creation, usageSource := resolveUsage(respBody, resp.StatusCode,
		func() int { return estimateResponsesPromptTokens(body) },
		func() string { return string(respBody) })
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	errMsg := ""
	if !success {
		errMsg = summarizeText(string(respBody), 500)
	}
	responseBody := chatCompletionToResponsesBody(respBody, localModel)
	if responseBody == nil {
		responseBody = map[string]any{"error": "failed to convert chat completion to responses body", "upstream_body": summarizeText(string(respBody), 800)}
	}
	out, _ := json.Marshal(responseBody)
	res := forwardResult{
		RequestID:           reqID,
		StatusCode:          resp.StatusCode,
		LatencyMS:           time.Since(start).Milliseconds(),
		Success:             success,
		ErrorMessage:        errMsg,
		ResponseSummary:     summarizeText(string(out), 800),
		PromptTokens:        pt,
		CompletionTokens:    ct,
		TotalTokens:         tt,
		CachedTokens:        cached,
		CacheCreationTokens: creation,
		UsageSource:         usageSource,
	}
	a.finish(fctx.withURL(url), res, respBody)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	return res
}

func (a *App) forwardChatStreamAsResponses(w http.ResponseWriter, r *http.Request, resp *http.Response, start time.Time, reqID, source string, provider ProviderKey, localKeyID, localModel, upstreamModel string, body map[string]any) forwardResult {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	forwardStreamDiagnosticHeaders(w, resp)
	w.WriteHeader(http.StatusOK)
	responseID := randomID("resp")
	itemID := randomID("msg")
	createdAt := time.Now().Unix()
	response := responsesSkeleton(responseID, localModel, createdAt)
	response["status"] = "in_progress"
	item := responseMessageItem(itemID, "")
	item["status"] = "in_progress"
	writeSSEEvent(w, "response.created", map[string]any{"type": "response.created", "response": response})
	writeSSEEvent(w, "response.in_progress", map[string]any{"type": "response.in_progress", "response": response})
	writeSSEEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	writeSSEEvent(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var fullText strings.Builder
	toolCalls := map[int]*streamedToolCall{}
	var toolCallOrder []int
	pt, ct, tt := 0, 0, 0
	usageSource := "missing"
	success := true
	errMsg := ""
	sawDone := false
	sawFinish := false
	var streamErr error
	var cacheUsage streamCacheUsage
	for {
		line, err := readStreamLine(reader)
		if err != nil && len(line) == 0 {
			if err != io.EOF {
				streamErr = err
			}
			break
		}
		if !strings.HasPrefix(line, "data:") {
			if err == io.EOF {
				break
			}
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			if err == io.EOF {
				break
			}
			continue
		}
		if data != "" {
			cacheUsage.observe([]byte(data))
			if p, c, t, src := extractUsage([]byte(data)); src != "missing" {
				pt, ct, tt, usageSource = p, c, t, "stream_final"
			}
			if chatStreamFinishReason([]byte(data)) != "" {
				sawFinish = true
			}
			delta := chatStreamContentDelta([]byte(data))
			if delta != "" {
				fullText.WriteString(delta)
				writeSSEEvent(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": delta})
			}
			for _, delta := range chatStreamToolCallDeltas([]byte(data)) {
				tc, ok := toolCalls[delta.Index]
				if !ok {
					id := stringFromAny(delta.ID, randomID("fc"))
					tc = &streamedToolCall{Index: delta.Index, ID: id, CallID: id}
					toolCalls[delta.Index] = tc
					toolCallOrder = append(toolCallOrder, delta.Index)
				}
				if delta.ID != "" {
					tc.ID = delta.ID
					tc.CallID = delta.ID
				}
				if delta.Name != "" {
					tc.Name = delta.Name
				}
				outputIndex := 1 + indexOfInt(toolCallOrder, delta.Index)
				if !tc.Added && tc.Name != "" {
					tc.Added = true
					writeSSEEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": responseFunctionCallItem(tc.ID, tc.CallID, tc.Name, "", "in_progress")})
				}
				if delta.Arguments != "" {
					tc.Arguments += delta.Arguments
					if !tc.Added {
						tc.Added = true
						writeSSEEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": responseFunctionCallItem(tc.ID, tc.CallID, tc.Name, "", "in_progress")})
					}
					writeSSEEvent(w, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": tc.ID, "output_index": outputIndex, "delta": delta.Arguments})
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if streamErr != nil {
		success = false
		errMsg = streamErr.Error()
	} else if !sawDone && !sawFinish {
		success = false
		errMsg = "upstream chat stream closed before finish_reason or [DONE]"
	}
	text := fullText.String()
	if usageSource == "missing" {
		pt = estimateResponsesPromptTokens(body)
		ct = estimateTokensFromText(text)
		tt = pt + ct
		usageSource = "estimated"
	}
	if !success {
		writeSSEErrorEvent(w, errMsg)
		res := forwardResult{
			RequestID:        reqID,
			StatusCode:       resp.StatusCode,
			LatencyMS:        time.Since(start).Milliseconds(),
			Success:          false,
			ErrorMessage:     errMsg,
			ResponseSummary:  summarizeText(text, 800),
			PromptTokens:     pt,
			CompletionTokens: ct,
			TotalTokens:      tt,
			UsageSource:      usageSource,
		}
		return a.finishStream(r, resp, body, res, source, provider, localKeyID, localModel, upstreamModel, []byte(text))
	}
	donePart := map[string]any{"type": "output_text", "text": text}
	doneItem := responseMessageItem(itemID, text)
	response["status"] = "completed"
	output := []map[string]any{doneItem}
	response["output_text"] = text
	response["usage"] = map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": tt}
	writeSSEEvent(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": 0, "content_index": 0, "text": text})
	writeSSEEvent(w, "response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": itemID, "output_index": 0, "content_index": 0, "part": donePart})
	writeSSEEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": doneItem})
	for i, index := range toolCallOrder {
		tc := toolCalls[index]
		outputIndex := 1 + i
		if !tc.Added {
			writeSSEEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": responseFunctionCallItem(tc.ID, tc.CallID, tc.Name, "", "in_progress")})
		}
		doneToolCall := responseFunctionCallItem(tc.ID, tc.CallID, tc.Name, tc.Arguments, "completed")
		writeSSEEvent(w, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": tc.ID, "output_index": outputIndex, "name": tc.Name, "arguments": tc.Arguments})
		writeSSEEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": doneToolCall})
		output = append(output, doneToolCall)
	}
	response["output"] = output
	writeSSEEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	res := forwardResult{
		RequestID:        reqID,
		StatusCode:       resp.StatusCode,
		LatencyMS:        time.Since(start).Milliseconds(),
		Success:          success,
		ErrorMessage:     errMsg,
		ResponseSummary:  summarizeText(text, 800),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		UsageSource:      usageSource,
	}
	res = cacheUsage.fill(res)
	responseObj := map[string]any{
		"id":          responseID,
		"object":      "response",
		"status":      "completed",
		"output_text": text,
		"output":      output,
		"usage":       map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": tt},
	}
	finalResponseBody, _ := json.Marshal(responseObj)
	return a.finishStream(r, resp, body, res, source, provider, localKeyID, localModel, upstreamModel, finalResponseBody)
}

func (a *App) forwardResponsesStreamAsChat(w http.ResponseWriter, r *http.Request, resp *http.Response, start time.Time, reqID, source string, provider ProviderKey, localKeyID, localModel, upstreamModel string, body map[string]any) forwardResult {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	forwardStreamDiagnosticHeaders(w, resp)
	w.WriteHeader(http.StatusOK)
	chatID := randomID("chatcmpl")
	createdAt := time.Now().Unix()
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var fullText strings.Builder
	pt, ct, tt := 0, 0, 0
	usageSource := "missing"
	success := true
	errMsg := ""
	sawCompleted := false
	sawDone := false
	sawTextDone := false
	var streamErr error
	var cacheUsage streamCacheUsage
	writeChatSSEChunk(w, chatID, localModel, createdAt, map[string]any{"role": "assistant"}, "")
	for {
		line, err := readStreamLine(reader)
		if err != nil && len(line) == 0 {
			if err != io.EOF {
				streamErr = err
			}
			break
		}
		if !strings.HasPrefix(line, "data:") {
			if err == io.EOF {
				break
			}
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
		} else if data != "" {
			cacheUsage.observe([]byte(data))
			if p, c, t, src := extractUsage([]byte(data)); src != "missing" {
				pt, ct, tt, usageSource = p, c, t, "stream_final"
			}
			var payload map[string]any
			if json.Unmarshal([]byte(data), &payload) == nil {
				switch stringFromAny(payload["type"], "") {
				case "response.completed":
					sawCompleted = true
				case "response.output_text.done":
					sawTextDone = true
				}
			}
			delta := responsesStreamTextDelta([]byte(data))
			if delta != "" {
				fullText.WriteString(delta)
				writeChatSSEChunk(w, chatID, localModel, createdAt, map[string]any{"content": delta}, "")
			}
		}
		if err == io.EOF {
			break
		}
	}
	if streamErr != nil {
		success = false
		errMsg = streamErr.Error()
	} else if !sawCompleted && !sawDone && !sawTextDone {
		success = false
		errMsg = "upstream stream closed before response.completed"
	}
	text := fullText.String()
	if usageSource == "missing" {
		pt = estimateTokensFromAny(body["messages"])
		ct = estimateTokensFromText(text)
		tt = pt + ct
		usageSource = "estimated"
	}
	if !success {
		writeSSEErrorEvent(w, errMsg)
		res := forwardResult{
			RequestID:        reqID,
			StatusCode:       resp.StatusCode,
			LatencyMS:        time.Since(start).Milliseconds(),
			Success:          false,
			ErrorMessage:     errMsg,
			ResponseSummary:  summarizeText(text, 800),
			PromptTokens:     pt,
			CompletionTokens: ct,
			TotalTokens:      tt,
			UsageSource:      usageSource,
		}
		return a.finishStream(r, resp, body, res, source, provider, localKeyID, localModel, upstreamModel, []byte(text))
	}
	writeChatSSEChunk(w, chatID, localModel, createdAt, map[string]any{}, "stop")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	res := forwardResult{
		RequestID:        reqID,
		StatusCode:       resp.StatusCode,
		LatencyMS:        time.Since(start).Milliseconds(),
		Success:          success,
		ErrorMessage:     errMsg,
		ResponseSummary:  summarizeText(text, 800),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		UsageSource:      usageSource,
	}
	res = cacheUsage.fill(res)
	completionObj := map[string]any{
		"id":      chatID,
		"object":  "chat.completion",
		"created": createdAt,
		"model":   localModel,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": tt},
	}
	finalResponseBody, _ := json.Marshal(completionObj)
	return a.finishStream(r, resp, body, res, source, provider, localKeyID, localModel, upstreamModel, finalResponseBody)
}

func (a *App) forwardStream(w http.ResponseWriter, r *http.Request, resp *http.Response, start time.Time, reqID, source string, provider ProviderKey, localKeyID, localModel, upstreamModel string, body map[string]any) forwardResult {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	forwardStreamDiagnosticHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var collected strings.Builder
	var chatContent strings.Builder
	var reasoningContent strings.Builder
	finishReason := ""
	chunkID := ""
	chunkModel := upstreamModel
	toolCallsMap := map[int]*streamedToolCall{}
	var toolCallOrder []int
	pt, ct, tt := 0, 0, 0
	usageSource := "missing"
	isResponsesStream := bodyHasResponsesInput(body)
	sawResponsesCompleted := false
	sawDone := false
	sawTextDone := false
	var responseText strings.Builder
	var lastResponse map[string]any
	var streamErr error
	dropNextBlankLine := false
	delayedDone := false
	var cacheUsage streamCacheUsage
	for {
		line, err := readStreamLine(reader)
		if err != nil && len(line) == 0 {
			if err != io.EOF {
				streamErr = err
			}
			break
		}
		if dropNextBlankLine && line == "" {
			dropNextBlankLine = false
			if err == io.EOF {
				break
			}
			continue
		}
		shouldForwardLine := true
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				sawDone = true
				if isResponsesStream && !sawResponsesCompleted {
					shouldForwardLine = false
					dropNextBlankLine = true
					delayedDone = true
				}
			} else if data != "" {
				collected.WriteString(data)
				collected.WriteByte('\n')
				cacheUsage.observe([]byte(data))
				if p, c, t, src := extractUsage([]byte(data)); src != "missing" {
					pt, ct, tt, usageSource = p, c, t, "stream_final"
				}
				var payload map[string]any
				if json.Unmarshal([]byte(data), &payload) == nil {
					if id := stringFromAny(payload["id"], ""); id != "" && chunkID == "" {
						chunkID = id
					}
					if m := stringFromAny(payload["model"], ""); m != "" {
						chunkModel = m
					}
					if response, ok := payload["response"].(map[string]any); ok {
						lastResponse = response
					}
					switch stringFromAny(payload["type"], "") {
					case "response.completed":
						sawResponsesCompleted = true
					case "response.output_text.delta":
						responseText.WriteString(textFromAny(payload["delta"]))
					case "response.output_text.done":
						sawTextDone = true
						if text := textFromAny(payload["text"]); text != "" {
							responseText.Reset()
							responseText.WriteString(text)
						}
					}
					if choices, ok := payload["choices"].([]any); ok && len(choices) > 0 {
						if choice, ok := choices[0].(map[string]any); ok {
							if fr := stringFromAny(choice["finish_reason"], ""); fr != "" {
								finishReason = fr
							}
							if delta, ok := choice["delta"].(map[string]any); ok {
								if c := textFromAny(delta["content"]); c != "" {
									chatContent.WriteString(c)
								}
								if rc := textFromAny(delta["reasoning_content"]); rc != "" {
									reasoningContent.WriteString(rc)
								}
								if items, ok := delta["tool_calls"].([]any); ok {
									for idx, itm := range items {
										if tm, ok := itm.(map[string]any); ok {
											tIndex := intFromAny(tm["index"])
											if tIndex == 0 && tm["index"] == nil {
												tIndex = idx
											}
											tc, exists := toolCallsMap[tIndex]
											if !exists {
												tc = &streamedToolCall{Index: tIndex, ID: stringFromAny(tm["id"], randomID("fc"))}
												toolCallsMap[tIndex] = tc
												toolCallOrder = append(toolCallOrder, tIndex)
											}
											if tm["id"] != nil && stringFromAny(tm["id"], "") != "" {
												tc.ID = stringFromAny(tm["id"], tc.ID)
											}
											if fn, ok := tm["function"].(map[string]any); ok {
												if n := stringFromAny(fn["name"], ""); n != "" {
													tc.Name = n
												}
												if args := textFromAny(fn["arguments"]); args != "" {
													tc.Arguments += args
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
		if shouldForwardLine {
			_, _ = fmt.Fprint(w, line+"\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			break
		}
	}
	if usageSource == "missing" && resp.StatusCode < 400 {
		if _, ok := body["input"]; ok {
			pt = estimateResponsesPromptTokens(body)
		} else {
			pt = estimateTokensFromAny(body["messages"])
		}
		summaryForTokens := chatContent.String()
		if summaryForTokens == "" {
			summaryForTokens = responseText.String()
		}
		if summaryForTokens == "" {
			summaryForTokens = collected.String()
		}
		ct = estimateTokensFromText(summaryForTokens)
		tt = pt + ct
		usageSource = "estimated"
	}
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	errMsg := ""
	if streamErr != nil {
		if isResponsesStream && sawDone {
			streamErr = nil
		} else {
			success = false
			errMsg = streamErr.Error()
		}
	} else if isResponsesStream && resp.StatusCode >= 200 && resp.StatusCode < 300 && !sawResponsesCompleted {
		if sawDone || sawTextDone {
			synthetic := syntheticResponsesCompletedEvent(lastResponse, localModel, responseText.String(), pt, ct, tt)
			writeSSEEvent(w, "response.completed", synthetic)
			if delayedDone || sawTextDone {
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
		} else {
			success = false
			errMsg = "upstream stream closed before response.completed"
		}
	}
	if !success && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		writeSSEErrorEvent(w, errMsg)
	}

	var finalResponseBody []byte
	var summaryText string

	if isResponsesStream || lastResponse != nil {
		respObj := lastResponse
		if respObj == nil {
			respObj = responsesSkeleton(randomID("resp"), localModel, start.Unix())
		}
		if responseText.Len() > 0 {
			respObj["output_text"] = responseText.String()
		}
		if _, hasOutput := respObj["output"]; !hasOutput && responseText.Len() > 0 {
			respObj["output"] = []map[string]any{responseMessageItem(randomID("msg"), responseText.String())}
		}
		respObj["status"] = "completed"
		if pt > 0 || ct > 0 || tt > 0 {
			respObj["usage"] = map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": tt}
		}
		finalResponseBody, _ = json.Marshal(respObj)
		summaryText = textFromAny(respObj["output_text"])
	} else if chatContent.Len() > 0 || len(toolCallsMap) > 0 || reasoningContent.Len() > 0 {
		if chunkID == "" {
			chunkID = reqID
		}
		if finishReason == "" {
			finishReason = "stop"
		}
		msg := map[string]any{
			"role":    "assistant",
			"content": chatContent.String(),
		}
		if reasoningContent.Len() > 0 {
			msg["reasoning_content"] = reasoningContent.String()
		}
		if len(toolCallOrder) > 0 {
			tcs := []map[string]any{}
			for _, idx := range toolCallOrder {
				tc := toolCallsMap[idx]
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": tc.Arguments,
					},
				})
			}
			msg["tool_calls"] = tcs
		}
		completionObj := map[string]any{
			"id":      chunkID,
			"object":  "chat.completion",
			"created": start.Unix(),
			"model":   chunkModel,
			"choices": []map[string]any{{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
			}},
		}
		if pt > 0 || ct > 0 || tt > 0 {
			completionObj["usage"] = map[string]any{
				"prompt_tokens":     pt,
				"completion_tokens": ct,
				"total_tokens":      tt,
			}
		}
		finalResponseBody, _ = json.Marshal(completionObj)
		summaryText = chatContent.String()
	} else {
		summaryText = responseText.String()
		if summaryText == "" {
			summaryText = collected.String()
		}
		finalResponseBody = []byte(summaryText)
	}

	if summaryText == "" {
		summaryText = collected.String()
	}

	res := forwardResult{
		RequestID:        reqID,
		StatusCode:       resp.StatusCode,
		LatencyMS:        time.Since(start).Milliseconds(),
		Success:          success,
		ErrorMessage:     errMsg,
		ResponseSummary:  summarizeText(summaryText, 800),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		UsageSource:      usageSource,
	}
	res = cacheUsage.fill(res)
	return a.finishStream(r, resp, body, res, source, provider, localKeyID, localModel, upstreamModel, finalResponseBody)
}

// saveRequestAndUsage 把一次转发的请求日志、用量明细与日聚合一并落库。
//
// 为什么必须事务化：此前 request_logs 与 usage_records 是两条独立的 Exec，且错误全被
// `_, _ =` 丢弃。任意一条失败都会留下「有请求日志无用量明细」或反之的漂移，统计与
// 日志对不上，而且完全没有痕迹可查。现在三条写入（含日聚合的原子 upsert）共享同一
// 事务，任意一步失败即整体回滚，杜绝半截数据。
//
// 为什么返回 error 却不影响 HTTP 响应：统计落库属于旁路观测，调用方一律以
// `_ =` 忽略返回值（或仅写调试日志）。统计失败绝不能改变客户端拿到的状态码与响应体。
func (a *App) saveRequestAndUsage(res forwardResult, source string, provider ProviderKey, localKeyID, localModel, upstreamModel string, stream bool, r *http.Request) error {
	if a == nil || a.db == nil {
		return nil
	}
	if res.RequestID == "" {
		res.RequestID = randomID("req")
	}
	method, path, ip := r.Method, r.URL.Path, clientIP(r)
	t := now()
	// 日聚合的 date 直接取自与 usage_records.created_at 同一次 now() 的前 10 个字符，
	// 保证两张表时区与日期口径完全一致，不会因跨秒/跨时区产生错位。
	day := t
	if len(day) > 10 {
		day = day[:10]
	}

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	// Commit 成功后 Rollback 会返回 sql.ErrTxDone，可安全忽略；这条 defer 的作用是
	// 保证任何提前 return 的异常路径都不会泄漏未结束的事务。
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`INSERT INTO request_logs(id,source,provider_key_id,local_api_key_id,local_model,upstream_model,method,path,client_ip,status_code,latency_ms,success,error_message,prompt_tokens,completion_tokens,total_tokens,cached_tokens,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		res.RequestID, source, provider.ID, localKeyID, localModel, upstreamModel, method, path, ip, res.StatusCode, res.LatencyMS, boolInt(res.Success), res.ErrorMessage, res.PromptTokens, res.CompletionTokens, res.TotalTokens, res.CachedTokens, t); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO usage_records(id,request_id,provider_key_id,provider_type,local_api_key_id,source,local_model,upstream_model,client_ip,prompt_tokens,completion_tokens,total_tokens,cached_tokens,cache_creation_tokens,usage_source,stream,success,status_code,latency_ms,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		randomID("ur"), res.RequestID, provider.ID, provider.ProviderType, localKeyID, source, localModel, upstreamModel, ip, res.PromptTokens, res.CompletionTokens, res.TotalTokens, res.CachedTokens, res.CacheCreationTokens, res.UsageSource, boolInt(stream), boolInt(res.Success), res.StatusCode, res.LatencyMS, t); err != nil {
		return err
	}
	// 日聚合用 ON CONFLICT 做原子累加：同一 (date, provider_key_id, model, source) 反复写入
	// 只会在原行上叠加计数与 token，既避免「先 SELECT 再 UPDATE」的读改写竞态，也让
	// 用量日查询可以直接读现成累计值而不必每次全表聚合。model 取 upstream_model，
	// 与 handleUsageDaily 明细路径的分组维度保持一致。
	// 注：本表没有 cached_tokens 列（本次不扩展表结构），故不累加缓存命中数。
	if _, err := tx.Exec(`INSERT INTO usage_daily_aggregates(date,provider_key_id,provider_type,model,source,request_count,success_count,prompt_tokens,completion_tokens,total_tokens)
		VALUES(?,?,?,?,?,1,?,?,?,?)
		ON CONFLICT(date, provider_key_id, model, source) DO UPDATE SET
			request_count = request_count + 1,
			success_count = success_count + excluded.success_count,
			prompt_tokens = prompt_tokens + excluded.prompt_tokens,
			completion_tokens = completion_tokens + excluded.completion_tokens,
			total_tokens = total_tokens + excluded.total_tokens,
			provider_type = excluded.provider_type`,
		day, provider.ID, provider.ProviderType, upstreamModel, source, boolInt(res.Success), res.PromptTokens, res.CompletionTokens, res.TotalTokens); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) logForwardDebug(r *http.Request, res forwardResult, source, upstreamURL string, provider ProviderKey, localKeyID, localModel, upstreamModel string, stream bool, requestBody, responseBody []byte) {
	a.logAPIDebug(apiDebugEntry{
		RequestID:        res.RequestID,
		Source:           source,
		ClientIP:         clientIP(r),
		Method:           r.Method,
		Path:             r.URL.Path,
		UpstreamURL:      upstreamURL,
		ProviderKeyID:    provider.ID,
		ProviderName:     provider.Name,
		LocalAPIKeyID:    localKeyID,
		LocalModel:       localModel,
		UpstreamModel:    upstreamModel,
		Stream:           stream,
		StatusCode:       res.StatusCode,
		LatencyMS:        res.LatencyMS,
		Success:          res.Success,
		ErrorMessage:     res.ErrorMessage,
		PromptTokens:     res.PromptTokens,
		CompletionTokens: res.CompletionTokens,
		TotalTokens:      res.TotalTokens,
		UsageSource:      res.UsageSource,
	}, requestBody, responseBody)
}
