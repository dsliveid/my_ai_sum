package app

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	UsageSource      string
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
	return data, nil
}

func (a *App) handleChatCompletion(w http.ResponseWriter, r *http.Request, source string, localKey LocalAPIKey) forwardResult {
	body, _, err := decodeRawJSON(r)
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
	switch upstreamProtocol {
	case protocolChatCompletions:
		return a.forwardChat(w, r, source, localKey.ID, localModel, upstreamModel, provider, body)
	case protocolResponses:
		return a.forwardChatViaResponses(w, r, source, localKey.ID, localModel, upstreamModel, provider, body)
	default:
		msg := "unsupported upstream protocol: " + upstreamProtocol
		writeError(w, 400, msg)
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: msg}
	}
}

func (a *App) handleResponses(w http.ResponseWriter, r *http.Request, source string, localKey LocalAPIKey) forwardResult {
	body, _, err := decodeRawJSON(r)
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
	switch upstreamProtocol {
	case protocolResponses:
		return a.forwardResponses(w, r, source, localKey.ID, localModel, upstreamModel, provider, body)
	case protocolChatCompletions:
		return a.forwardResponsesViaChat(w, r, source, localKey.ID, localModel, upstreamModel, provider, body)
	default:
		msg := "unsupported upstream protocol: " + upstreamProtocol
		writeError(w, 400, msg)
		return forwardResult{StatusCode: 400, Success: false, ErrorMessage: msg}
	}
}

func (a *App) forwardChat(w http.ResponseWriter, r *http.Request, source, localKeyID, localModel, upstreamModel string, provider ProviderKey, body map[string]any) forwardResult {
	start := time.Now()
	stream, _ := body["stream"].(bool)
	reqID := randomID("req")
	apiKey := provider.APIKey
	if apiKey == "" || strings.Contains(apiKey, "...") {
		apiKey, _ = a.decryptProviderKey(provider)
	}
	payload, _ := json.Marshal(body)
	client, err := a.httpClientForProvider(provider)
	if err != nil {
		res := forwardResult{RequestID: reqID, StatusCode: 500, Success: false, ErrorMessage: err.Error(), LatencyMS: time.Since(start).Milliseconds(), UsageSource: "missing"}
		a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
		writeError(w, 500, err.Error())
		return res
	}
	url := normalizeBaseURL(provider.BaseURL) + "/chat/completions"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		writeError(w, 500, err.Error())
		return forwardResult{RequestID: reqID, StatusCode: 500, Success: false, ErrorMessage: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		res := forwardResult{RequestID: reqID, StatusCode: 502, Success: false, ErrorMessage: err.Error(), LatencyMS: time.Since(start).Milliseconds(), UsageSource: "missing"}
		a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
		writeError(w, 502, err.Error())
		return res
	}
	defer resp.Body.Close()
	if stream {
		return a.forwardStream(w, r, resp, start, reqID, source, provider, localKeyID, localModel, upstreamModel, body)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, 502, err.Error())
		return forwardResult{RequestID: reqID, StatusCode: 502, Success: false, ErrorMessage: err.Error()}
	}
	if source == "api_chat_test" && !json.Valid(respBody) {
		statusCode := resp.StatusCode
		if statusCode >= 200 && statusCode < 300 {
			statusCode = http.StatusBadGateway
		}
		contentType := resp.Header.Get("Content-Type")
		preview := summarizeText(string(respBody), 800)
		errMsg := fmt.Sprintf("upstream returned a non-JSON response from %s (upstream status %d, content-type %q). Check whether the external service Base URL points to an OpenAI-compatible API endpoint, for example it may need /v1.", url, resp.StatusCode, contentType)
		res := forwardResult{
			RequestID:       reqID,
			StatusCode:      statusCode,
			LatencyMS:       time.Since(start).Milliseconds(),
			Success:         false,
			ErrorMessage:    errMsg,
			ResponseSummary: preview,
			UsageSource:     "missing",
		}
		a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
		writeJSON(w, statusCode, map[string]any{
			"error":           errMsg,
			"upstream_status": resp.StatusCode,
			"content_type":    contentType,
			"body_preview":    preview,
		})
		return res
	}
	pt, ct, tt, usageSource := extractUsage(respBody)
	if usageSource == "missing" && resp.StatusCode < 400 {
		pt = estimateTokensFromAny(body["messages"])
		ct = estimateTokensFromText(string(respBody))
		tt = pt + ct
		usageSource = "estimated"
	}
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	errMsg := ""
	if !success {
		errMsg = summarizeText(string(respBody), 500)
	}
	res := forwardResult{
		RequestID:        reqID,
		StatusCode:       resp.StatusCode,
		LatencyMS:        time.Since(start).Milliseconds(),
		Success:          success,
		ErrorMessage:     errMsg,
		ResponseSummary:  summarizeText(string(respBody), 800),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		UsageSource:      usageSource,
	}
	a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
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
	apiKey := provider.APIKey
	if apiKey == "" || strings.Contains(apiKey, "...") {
		apiKey, _ = a.decryptProviderKey(provider)
	}
	payload, _ := json.Marshal(responsesBody)
	client, err := a.httpClientForProvider(provider)
	if err != nil {
		res := forwardResult{RequestID: reqID, StatusCode: 500, Success: false, ErrorMessage: err.Error(), LatencyMS: time.Since(start).Milliseconds(), UsageSource: "missing"}
		a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
		writeError(w, 500, err.Error())
		return res
	}
	url := normalizeBaseURL(provider.BaseURL) + "/responses"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		writeError(w, 500, err.Error())
		return forwardResult{RequestID: reqID, StatusCode: 500, Success: false, ErrorMessage: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		res := forwardResult{RequestID: reqID, StatusCode: 502, Success: false, ErrorMessage: err.Error(), LatencyMS: time.Since(start).Milliseconds(), UsageSource: "missing"}
		a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
		writeError(w, 502, err.Error())
		return res
	}
	defer resp.Body.Close()
	if stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return a.forwardResponsesStreamAsChat(w, r, resp, start, reqID, source, provider, localKeyID, localModel, upstreamModel, body)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, 502, err.Error())
		return forwardResult{RequestID: reqID, StatusCode: 502, Success: false, ErrorMessage: err.Error()}
	}
	pt, ct, tt, usageSource := extractUsage(respBody)
	if usageSource == "missing" && resp.StatusCode < 400 {
		pt = estimateTokensFromAny(body["messages"])
		ct = estimateTokensFromText(string(respBody))
		tt = pt + ct
		usageSource = "estimated"
	}
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
		RequestID:        reqID,
		StatusCode:       resp.StatusCode,
		LatencyMS:        time.Since(start).Milliseconds(),
		Success:          success,
		ErrorMessage:     errMsg,
		ResponseSummary:  summarizeText(string(out), 800),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		UsageSource:      usageSource,
	}
	a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	return res
}

func (a *App) forwardResponses(w http.ResponseWriter, r *http.Request, source, localKeyID, localModel, upstreamModel string, provider ProviderKey, body map[string]any) forwardResult {
	start := time.Now()
	stream, _ := body["stream"].(bool)
	reqID := randomID("req")
	apiKey := provider.APIKey
	if apiKey == "" || strings.Contains(apiKey, "...") {
		apiKey, _ = a.decryptProviderKey(provider)
	}
	payload, _ := json.Marshal(body)
	client, err := a.httpClientForProvider(provider)
	if err != nil {
		res := forwardResult{RequestID: reqID, StatusCode: 500, Success: false, ErrorMessage: err.Error(), LatencyMS: time.Since(start).Milliseconds(), UsageSource: "missing"}
		a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
		writeError(w, 500, err.Error())
		return res
	}
	url := normalizeBaseURL(provider.BaseURL) + "/responses"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		writeError(w, 500, err.Error())
		return forwardResult{RequestID: reqID, StatusCode: 500, Success: false, ErrorMessage: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		res := forwardResult{RequestID: reqID, StatusCode: 502, Success: false, ErrorMessage: err.Error(), LatencyMS: time.Since(start).Milliseconds(), UsageSource: "missing"}
		a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
		writeError(w, 502, err.Error())
		return res
	}
	if stream {
		defer resp.Body.Close()
		return a.forwardStream(w, r, resp, start, reqID, source, provider, localKeyID, localModel, upstreamModel, body)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, 502, err.Error())
		return forwardResult{RequestID: reqID, StatusCode: 502, Success: false, ErrorMessage: err.Error()}
	}
	pt, ct, tt, usageSource := extractUsage(respBody)
	if usageSource == "missing" && resp.StatusCode < 400 {
		pt = estimateResponsesPromptTokens(body)
		ct = estimateTokensFromText(string(respBody))
		tt = pt + ct
		usageSource = "estimated"
	}
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	errMsg := ""
	if !success {
		errMsg = summarizeText(string(respBody), 500)
	}
	res := forwardResult{
		RequestID:        reqID,
		StatusCode:       resp.StatusCode,
		LatencyMS:        time.Since(start).Milliseconds(),
		Success:          success,
		ErrorMessage:     errMsg,
		ResponseSummary:  summarizeText(string(respBody), 800),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		UsageSource:      usageSource,
	}
	a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, stream, r.Method, r.URL.Path)
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
	apiKey := provider.APIKey
	if apiKey == "" || strings.Contains(apiKey, "...") {
		apiKey, _ = a.decryptProviderKey(provider)
	}
	payload, _ := json.Marshal(chatBody)
	client, err := a.httpClientForProvider(provider)
	if err != nil {
		res := forwardResult{RequestID: reqID, StatusCode: 500, Success: false, ErrorMessage: err.Error(), LatencyMS: time.Since(start).Milliseconds(), UsageSource: "missing"}
		a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, wantStream, r.Method, r.URL.Path)
		writeError(w, 500, err.Error())
		return res
	}
	url := normalizeBaseURL(provider.BaseURL) + "/chat/completions"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		writeError(w, 500, err.Error())
		return forwardResult{RequestID: reqID, StatusCode: 500, Success: false, ErrorMessage: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if wantStream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		res := forwardResult{RequestID: reqID, StatusCode: 502, Success: false, ErrorMessage: err.Error(), LatencyMS: time.Since(start).Milliseconds(), UsageSource: "missing"}
		a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, wantStream, r.Method, r.URL.Path)
		writeError(w, 502, err.Error())
		return res
	}
	defer resp.Body.Close()
	if wantStream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return a.forwardChatStreamAsResponses(w, r, resp, start, reqID, source, provider, localKeyID, localModel, upstreamModel, body)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, 502, err.Error())
		return forwardResult{RequestID: reqID, StatusCode: 502, Success: false, ErrorMessage: err.Error()}
	}
	pt, ct, tt, usageSource := extractUsage(respBody)
	if usageSource == "missing" && resp.StatusCode < 400 {
		pt = estimateResponsesPromptTokens(body)
		ct = estimateTokensFromText(string(respBody))
		tt = pt + ct
		usageSource = "estimated"
	}
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
		RequestID:        reqID,
		StatusCode:       resp.StatusCode,
		LatencyMS:        time.Since(start).Milliseconds(),
		Success:          success,
		ErrorMessage:     errMsg,
		ResponseSummary:  summarizeText(string(out), 800),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		UsageSource:      usageSource,
	}
	a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, wantStream, r.Method, r.URL.Path)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	return res
}

func (a *App) forwardChatStreamAsResponses(w http.ResponseWriter, r *http.Request, resp *http.Response, start time.Time, reqID, source string, provider ProviderKey, localKeyID, localModel, upstreamModel string, body map[string]any) forwardResult {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	responseID := randomID("resp")
	itemID := randomID("msg")
	createdAt := time.Now().Unix()
	response := responsesSkeleton(responseID, localModel, createdAt)
	item := responseMessageItem(itemID, "")
	writeSSEEvent(w, "response.created", map[string]any{"type": "response.created", "response": response})
	writeSSEEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	writeSSEEvent(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var fullText strings.Builder
	pt, ct, tt := 0, 0, 0
	usageSource := "missing"
	success := true
	errMsg := ""
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if p, c, t, src := extractUsage([]byte(data)); src != "missing" {
			pt, ct, tt, usageSource = p, c, t, "stream_final"
		}
		delta := chatStreamContentDelta([]byte(data))
		if delta == "" {
			continue
		}
		fullText.WriteString(delta)
		writeSSEEvent(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": delta})
	}
	if err := scanner.Err(); err != nil {
		success = false
		errMsg = err.Error()
	}
	text := fullText.String()
	if usageSource == "missing" {
		pt = estimateResponsesPromptTokens(body)
		ct = estimateTokensFromText(text)
		tt = pt + ct
		usageSource = "estimated"
	}
	donePart := map[string]any{"type": "output_text", "text": text}
	doneItem := responseMessageItem(itemID, text)
	response["output"] = []map[string]any{doneItem}
	response["output_text"] = text
	response["usage"] = map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": tt}
	writeSSEEvent(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": 0, "content_index": 0, "text": text})
	writeSSEEvent(w, "response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": itemID, "output_index": 0, "content_index": 0, "part": donePart})
	writeSSEEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": doneItem})
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
	a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, true, r.Method, r.URL.Path)
	return res
}

func (a *App) forwardResponsesStreamAsChat(w http.ResponseWriter, r *http.Request, resp *http.Response, start time.Time, reqID, source string, provider ProviderKey, localKeyID, localModel, upstreamModel string, body map[string]any) forwardResult {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	chatID := randomID("chatcmpl")
	createdAt := time.Now().Unix()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var fullText strings.Builder
	pt, ct, tt := 0, 0, 0
	usageSource := "missing"
	success := true
	errMsg := ""
	writeChatSSEChunk(w, chatID, localModel, createdAt, map[string]any{"role": "assistant"}, "")
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if p, c, t, src := extractUsage([]byte(data)); src != "missing" {
			pt, ct, tt, usageSource = p, c, t, "stream_final"
		}
		delta := responsesStreamTextDelta([]byte(data))
		if delta == "" {
			continue
		}
		fullText.WriteString(delta)
		writeChatSSEChunk(w, chatID, localModel, createdAt, map[string]any{"content": delta}, "")
	}
	if err := scanner.Err(); err != nil {
		success = false
		errMsg = err.Error()
	}
	text := fullText.String()
	if usageSource == "missing" {
		pt = estimateTokensFromAny(body["messages"])
		ct = estimateTokensFromText(text)
		tt = pt + ct
		usageSource = "estimated"
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
	a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, true, r.Method, r.URL.Path)
	return res
}

func (a *App) forwardStream(w http.ResponseWriter, r *http.Request, resp *http.Response, start time.Time, reqID, source string, provider ProviderKey, localKeyID, localModel, upstreamModel string, body map[string]any) forwardResult {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var collected strings.Builder
	pt, ct, tt := 0, 0, 0
	usageSource := "missing"
	for scanner.Scan() {
		line := scanner.Text()
		_, _ = fmt.Fprint(w, line+"\n")
		if flusher != nil {
			flusher.Flush()
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data != "[DONE]" && data != "" {
				collected.WriteString(data)
				collected.WriteByte('\n')
				if p, c, t, src := extractUsage([]byte(data)); src != "missing" {
					pt, ct, tt, usageSource = p, c, t, "stream_final"
				}
			}
		}
	}
	if usageSource == "missing" && resp.StatusCode < 400 {
		if _, ok := body["input"]; ok {
			pt = estimateResponsesPromptTokens(body)
		} else {
			pt = estimateTokensFromAny(body["messages"])
		}
		ct = estimateTokensFromText(collected.String())
		tt = pt + ct
		usageSource = "estimated"
	}
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	errMsg := ""
	if err := scanner.Err(); err != nil {
		success = false
		errMsg = err.Error()
	}
	res := forwardResult{
		RequestID:        reqID,
		StatusCode:       resp.StatusCode,
		LatencyMS:        time.Since(start).Milliseconds(),
		Success:          success,
		ErrorMessage:     errMsg,
		ResponseSummary:  summarizeText(collected.String(), 800),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		UsageSource:      usageSource,
	}
	a.saveRequestAndUsage(res, source, provider, localKeyID, localModel, upstreamModel, true, r.Method, r.URL.Path)
	return res
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func extractUsage(body []byte) (int, int, int, string) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return 0, 0, 0, "missing"
	}
	u, ok := m["usage"].(map[string]any)
	if !ok {
		return 0, 0, 0, "missing"
	}
	pt := intFromAny(u["prompt_tokens"])
	if pt == 0 {
		pt = intFromAny(u["input_tokens"])
	}
	ct := intFromAny(u["completion_tokens"])
	if ct == 0 {
		ct = intFromAny(u["output_tokens"])
	}
	tt := intFromAny(u["total_tokens"])
	if tt == 0 {
		tt = pt + ct
	}
	return pt, ct, tt, "upstream"
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	default:
		return 0
	}
}

func splitModels(models string) []string {
	var out []string
	for _, item := range strings.Split(models, ",") {
		if model := strings.TrimSpace(item); model != "" {
			out = append(out, model)
		}
	}
	return out
}

func estimateTokensFromAny(v any) int {
	b, _ := json.Marshal(v)
	return estimateTokensFromText(string(b))
}

func estimateResponsesPromptTokens(body map[string]any) int {
	total := estimateTokensFromAny(body["input"])
	if instructions, ok := body["instructions"]; ok {
		total += estimateTokensFromAny(instructions)
	}
	return total
}

func responsesBodyToChatBody(body map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty", "stop", "seed", "user"} {
		if v, ok := body[key]; ok {
			out[key] = v
		}
	}
	if v, ok := body["max_tokens"]; ok {
		out["max_tokens"] = v
	} else if v, ok := body["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if tools := responsesToolsToChatTools(body["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if v, ok := body["tool_choice"]; ok {
			out["tool_choice"] = v
		}
	}
	messages := responsesInputToChatMessages(body["input"])
	if instructions := strings.TrimSpace(textFromAny(body["instructions"])); instructions != "" {
		messages = append([]map[string]any{{"role": "system", "content": instructions}}, messages...)
	}
	if len(messages) == 0 {
		messages = []map[string]any{{"role": "user", "content": ""}}
	}
	out["messages"] = messages
	return out
}

func chatBodyToResponsesBody(body map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"temperature", "top_p", "user", "tool_choice"} {
		if v, ok := body[key]; ok {
			out[key] = v
		}
	}
	if v, ok := body["max_output_tokens"]; ok {
		out["max_output_tokens"] = v
	} else if v, ok := body["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if tools := chatToolsToResponsesTools(body["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	out["input"] = chatMessagesToResponsesInput(body["messages"])
	return out
}

func chatMessagesToResponsesInput(v any) []map[string]any {
	items, ok := v.([]any)
	if !ok {
		if typed, ok := v.([]map[string]any); ok {
			for _, item := range typed {
				items = append(items, item)
			}
		}
	}
	var out []map[string]any
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := stringFromAny(m["role"], "user")
		if role == "system" {
			role = "developer"
		}
		if role == "tool" {
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": stringFromAny(m["tool_call_id"], ""),
				"output":  textFromAny(m["content"]),
			})
			continue
		}
		out = append(out, map[string]any{
			"role": role,
			"content": []map[string]any{{
				"type": "input_text",
				"text": textFromAny(m["content"]),
			}},
		})
	}
	return out
}

func chatToolsToResponsesTools(v any) []map[string]any {
	items, ok := v.([]any)
	if !ok {
		if typed, ok := v.([]map[string]any); ok {
			for _, item := range typed {
				items = append(items, item)
			}
		}
	}
	if len(items) == 0 {
		return nil
	}
	var out []map[string]any
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok || stringFromAny(m["type"], "") != "function" {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		out = append(out, map[string]any{
			"type":        "function",
			"name":        stringFromAny(fn["name"], ""),
			"description": stringFromAny(fn["description"], ""),
			"parameters":  fn["parameters"],
		})
	}
	return out
}

func responsesInputToChatMessages(input any) []map[string]any {
	if input == nil {
		return nil
	}
	if s, ok := input.(string); ok {
		return []map[string]any{{"role": "user", "content": s}}
	}
	items, ok := input.([]any)
	if !ok {
		if typed, ok := input.([]map[string]any); ok {
			for _, item := range typed {
				items = append(items, item)
			}
		}
	}
	if len(items) == 0 {
		return []map[string]any{{"role": "user", "content": textFromAny(input)}}
	}
	var messages []map[string]any
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			if text := strings.TrimSpace(textFromAny(item)); text != "" {
				messages = append(messages, map[string]any{"role": "user", "content": text})
			}
			continue
		}
		itemType, _ := m["type"].(string)
		if itemType == "function_call_output" {
			msg := map[string]any{"role": "tool", "content": textFromAny(m["output"])}
			if callID, ok := m["call_id"].(string); ok && callID != "" {
				msg["tool_call_id"] = callID
			}
			messages = append(messages, msg)
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		if role == "developer" {
			role = "system"
		}
		msg := map[string]any{"role": role, "content": textFromAny(m["content"])}
		if itemType == "function_call" {
			msg = map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []map[string]any{{
					"id":   stringFromAny(m["call_id"], stringFromAny(m["id"], randomID("call"))),
					"type": "function",
					"function": map[string]any{
						"name":      stringFromAny(m["name"], ""),
						"arguments": stringFromAny(m["arguments"], "{}"),
					},
				}},
			}
		}
		messages = append(messages, msg)
	}
	return messages
}

func responsesToolsToChatTools(v any) []map[string]any {
	items, ok := v.([]any)
	if !ok {
		if typed, ok := v.([]map[string]any); ok {
			for _, item := range typed {
				items = append(items, item)
			}
		}
	}
	if len(items) == 0 {
		return nil
	}
	var out []map[string]any
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok || stringFromAny(m["type"], "") != "function" {
			continue
		}
		fn := map[string]any{
			"name":        stringFromAny(m["name"], ""),
			"description": stringFromAny(m["description"], ""),
			"parameters":  m["parameters"],
		}
		if fn["parameters"] == nil {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func chatCompletionToResponsesBody(body []byte, clientModel string) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	content := textFromAny(message["content"])
	output := []map[string]any{}
	if strings.TrimSpace(content) != "" {
		output = append(output, map[string]any{
			"id":     randomID("msg"),
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]any{{
				"type": "output_text",
				"text": content,
			}},
		})
	}
	if toolCalls, ok := message["tool_calls"].([]any); ok {
		for _, tc := range toolCalls {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tcm["function"].(map[string]any)
			output = append(output, map[string]any{
				"id":        stringFromAny(tcm["id"], randomID("fc")),
				"type":      "function_call",
				"call_id":   stringFromAny(tcm["id"], randomID("call")),
				"name":      stringFromAny(fn["name"], ""),
				"arguments": stringFromAny(fn["arguments"], "{}"),
				"status":    "completed",
			})
		}
	}
	pt, ct, tt, _ := extractUsage(body)
	response := responsesSkeleton(stringFromAny(payload["id"], randomID("resp")), stringFromAny(clientModel, stringFromAny(payload["model"], "")), time.Now().Unix())
	response["output"] = output
	response["output_text"] = content
	response["usage"] = map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": tt}
	return response
}

func responsesBodyToChatCompletionBody(body []byte, clientModel string) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	text := textFromAny(payload["output_text"])
	if text == "" {
		text = responseOutputText(payload["output"])
	}
	pt, ct, tt, _ := extractUsage(body)
	return map[string]any{
		"id":      stringFromAny(payload["id"], randomID("chatcmpl")),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   stringFromAny(clientModel, stringFromAny(payload["model"], "")),
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     pt,
			"completion_tokens": ct,
			"total_tokens":      tt,
		},
	}
}

func responseOutputText(v any) string {
	items, ok := v.([]any)
	if !ok {
		if typed, ok := v.([]map[string]any); ok {
			for _, item := range typed {
				items = append(items, item)
			}
		}
	}
	var parts []string
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok || stringFromAny(m["type"], "") != "message" {
			continue
		}
		if text := strings.TrimSpace(textFromAny(m["content"])); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func chatStreamContentDelta(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	return textFromAny(delta["content"])
}

func responsesStreamTextDelta(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if stringFromAny(payload["type"], "") == "response.output_text.delta" {
		return textFromAny(payload["delta"])
	}
	return ""
}

func writeChatSSEChunk(w http.ResponseWriter, id, model string, createdAt int64, delta map[string]any, finishReason string) {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": createdAt,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}
	if finishReason != "" {
		chunk["choices"] = []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}}
	}
	b, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func responsesSkeleton(id, model string, createdAt int64) map[string]any {
	return map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          createdAt,
		"status":              "completed",
		"model":               model,
		"output":              []map[string]any{},
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
	}
}

func responseMessageItem(id, text string) map[string]any {
	return map[string]any{
		"id":     id,
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{{
			"type": "output_text",
			"text": text,
		}},
	}
}

func writeResponsesSSE(w http.ResponseWriter, response map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	itemID := randomID("msg")
	if output, ok := response["output"].([]map[string]any); ok && len(output) > 0 {
		itemID = stringFromAny(output[0]["id"], itemID)
	}
	writeSSEEvent(w, "response.created", map[string]any{"type": "response.created", "response": response})
	if text := strings.TrimSpace(textFromAny(response["output_text"])); text != "" {
		item := responseMessageItem(itemID, "")
		writeSSEEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		writeSSEEvent(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
		writeSSEEvent(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": text})
		writeSSEEvent(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": 0, "content_index": 0, "text": text})
		donePart := map[string]any{"type": "output_text", "text": text}
		doneItem := responseMessageItem(itemID, text)
		writeSSEEvent(w, "response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": itemID, "output_index": 0, "content_index": 0, "part": donePart})
		writeSSEEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": doneItem})
	}
	writeSSEEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeSSEEvent(w http.ResponseWriter, event string, payload map[string]any) {
	b, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func textFromAny(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		var parts []string
		for _, item := range x {
			if text := strings.TrimSpace(textFromAny(item)); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"text", "content", "output"} {
			if text := strings.TrimSpace(textFromAny(x[key])); text != "" {
				return text
			}
		}
		b, _ := json.Marshal(x)
		return string(b)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func stringFromAny(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

func estimateTokensFromText(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n := len([]rune(s)) / 4
	if n < 1 {
		return 1
	}
	return n
}

func summarizeText(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:limit])
}

func (a *App) saveRequestAndUsage(res forwardResult, source string, provider ProviderKey, localKeyID, localModel, upstreamModel string, stream bool, method, path string) {
	if res.RequestID == "" {
		res.RequestID = randomID("req")
	}
	t := now()
	_, _ = a.db.Exec(`INSERT INTO request_logs(id,source,provider_key_id,local_api_key_id,local_model,upstream_model,method,path,status_code,latency_ms,success,error_message,prompt_tokens,completion_tokens,total_tokens,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		res.RequestID, source, provider.ID, localKeyID, localModel, upstreamModel, method, path, res.StatusCode, res.LatencyMS, boolInt(res.Success), res.ErrorMessage, res.PromptTokens, res.CompletionTokens, res.TotalTokens, t)
	_, _ = a.db.Exec(`INSERT INTO usage_records(id,request_id,provider_key_id,provider_type,local_api_key_id,source,local_model,upstream_model,prompt_tokens,completion_tokens,total_tokens,usage_source,stream,success,status_code,latency_ms,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		randomID("ur"), res.RequestID, provider.ID, provider.ProviderType, localKeyID, source, localModel, upstreamModel, res.PromptTokens, res.CompletionTokens, res.TotalTokens, res.UsageSource, boolInt(stream), boolInt(res.Success), res.StatusCode, res.LatencyMS, t)
}

func (a *App) handleTestChat(w http.ResponseWriter, r *http.Request, sub string) {
	switch {
	case sub == "/upstream" && r.Method == http.MethodPost:
		a.handleUpstreamTestChat(w, r)
	case sub == "/local" && r.Method == http.MethodPost:
		a.handleLocalTestChat(w, r)
	case sub == "/sessions" && r.Method == http.MethodGet:
		rows, err := a.db.Query(`SELECT id,target_type,provider_key_id,local_api_key_id,model,stream,request_summary,response_summary,status_code,latency_ms,success,error_message,created_at FROM test_chat_sessions ORDER BY created_at DESC LIMIT ?`, queryLimit(r, 100))
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		defer rows.Close()
		items := []TestChatSession{}
		for rows.Next() {
			var s TestChatSession
			var stream, success int
			_ = rows.Scan(&s.ID, &s.TargetType, &s.ProviderKeyID, &s.LocalAPIKeyID, &s.Model, &stream, &s.RequestSummary, &s.ResponseSummary, &s.StatusCode, &s.LatencyMS, &success, &s.ErrorMessage, &s.CreatedAt)
			s.Stream = intBool(stream)
			s.Success = intBool(success)
			items = append(items, s)
		}
		writeJSON(w, 200, items)
	default:
		writeError(w, 404, "not found")
	}
}

func (a *App) handleUpstreamTestChat(w http.ResponseWriter, r *http.Request) {
	body, raw, err := decodeRawJSON(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	providerID, _ := body["provider_key_id"].(string)
	model, _ := body["model"].(string)
	if providerID == "" || model == "" {
		writeError(w, 400, "provider_key_id and model are required")
		return
	}
	delete(body, "provider_key_id")
	provider, err := a.getProvider(providerID, true)
	if err != nil {
		writeError(w, 404, "provider not found")
		return
	}
	res := a.forwardChat(w, r, "api_chat_test", "", model, model, provider, body)
	a.saveTestSession("upstream", providerID, "", model, bodyBool(body, "stream"), summarizeText(string(raw), 500), res)
}

func (a *App) handleLocalTestChat(w http.ResponseWriter, r *http.Request) {
	body, raw, err := decodeRawJSON(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	localKeyID, _ := body["local_api_key_id"].(string)
	delete(body, "local_api_key_id")
	model, _ := body["model"].(string)
	if model == "" {
		writeError(w, 400, "model is required")
		return
	}
	localKey := LocalAPIKey{ID: localKeyID}
	if localKeyID != "" {
		if lk, err := a.getLocalAPIKeyByID(localKeyID); err == nil {
			localKey = lk
		}
	}
	var provider ProviderKey
	upstreamModel := model
	if localKey.ProviderKeyID != "" {
		provider, err = a.getProvider(localKey.ProviderKeyID, true)
		if err != nil {
			writeError(w, 404, "local api key provider not found")
			return
		}
		if mapping, ok := a.findMappingForProvider(model, provider.ID); ok {
			upstreamModel = mapping.UpstreamModel
		}
	} else {
		var m ModelMapping
		m, provider, err = a.findMapping(model)
		if err != nil {
			writeError(w, 404, "no model mapping")
			return
		}
		upstreamModel = m.UpstreamModel
	}
	body["model"] = upstreamModel
	requestProtocol := protocolChatCompletions
	if localKey.ProtocolConversionEnabled {
		if clientProtocol := normalizeProtocolName(localKey.ClientProtocol); clientProtocol != "" {
			requestProtocol = clientProtocol
		}
	}
	requestBody := body
	if requestProtocol == protocolResponses {
		requestBody = chatBodyToResponsesBody(body)
		requestBody["model"] = upstreamModel
		requestBody["stream"] = bodyBool(body, "stream")
	}
	upstreamProtocol, err := localKeyUpstreamProtocol(localKey, requestProtocol)
	if err != nil {
		writeError(w, 400, err.Error())
		a.saveTestSession("local", provider.ID, localKeyID, model, bodyBool(body, "stream"), summarizeText(string(raw), 500), forwardResult{
			StatusCode:   400,
			Success:      false,
			ErrorMessage: err.Error(),
			UsageSource:  "missing",
		})
		return
	}
	var res forwardResult
	switch {
	case requestProtocol == protocolChatCompletions && upstreamProtocol == protocolChatCompletions:
		res = a.forwardChat(w, r, "api_chat_test", localKeyID, model, upstreamModel, provider, requestBody)
	case requestProtocol == protocolChatCompletions && upstreamProtocol == protocolResponses:
		res = a.forwardChatViaResponses(w, r, "api_chat_test", localKeyID, model, upstreamModel, provider, requestBody)
	case requestProtocol == protocolResponses && upstreamProtocol == protocolResponses:
		res = a.forwardResponses(w, r, "api_chat_test", localKeyID, model, upstreamModel, provider, requestBody)
	case requestProtocol == protocolResponses && upstreamProtocol == protocolChatCompletions:
		res = a.forwardResponsesViaChat(w, r, "api_chat_test", localKeyID, model, upstreamModel, provider, requestBody)
	default:
		msg := "unsupported protocol conversion: " + requestProtocol + " -> " + upstreamProtocol
		writeError(w, 400, msg)
		res = forwardResult{StatusCode: 400, Success: false, ErrorMessage: msg, UsageSource: "missing"}
	}
	a.saveTestSession("local", provider.ID, localKeyID, model, bodyBool(requestBody, "stream"), summarizeText(string(raw), 500), res)
}

func bodyBool(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func (a *App) saveTestSession(target, providerID, localKeyID, model string, stream bool, reqSummary string, res forwardResult) {
	_, _ = a.db.Exec(`INSERT INTO test_chat_sessions(id,target_type,provider_key_id,local_api_key_id,model,stream,request_summary,response_summary,status_code,latency_ms,success,error_message,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		randomID("tc"), target, providerID, localKeyID, model, boolInt(stream), reqSummary, res.ResponseSummary, res.StatusCode, res.LatencyMS, boolInt(res.Success), res.ErrorMessage, now())
}

func (a *App) handleUsage(w http.ResponseWriter, r *http.Request, sub string) {
	switch {
	case sub == "/records" && r.Method == http.MethodGet:
		a.handleUsageRecords(w, r)
	case sub == "/provider-keys" && r.Method == http.MethodGet:
		a.handleUsageAggregate(w, r, "provider_key_id")
	case sub == "/models" && r.Method == http.MethodGet:
		a.handleUsageAggregate(w, r, "upstream_model")
	case sub == "/daily" && r.Method == http.MethodGet:
		a.handleUsageDaily(w, r)
	case sub == "/export" && r.Method == http.MethodGet:
		a.handleUsageExport(w, r)
	default:
		writeError(w, 404, "not found")
	}
}

func (a *App) handleUsageRecords(w http.ResponseWriter, r *http.Request) {
	where, args, err := usageDateWhere(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	args = append(args, queryLimit(r, 200))
	rows, err := a.db.Query(`SELECT id,request_id,provider_key_id,provider_type,local_api_key_id,source,local_model,upstream_model,prompt_tokens,completion_tokens,total_tokens,usage_source,stream,success,status_code,latency_ms,created_at FROM usage_records`+where+` ORDER BY datetime(created_at) DESC LIMIT ?`, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []UsageRecord{}
	for rows.Next() {
		var u UsageRecord
		var stream, success int
		_ = rows.Scan(&u.ID, &u.RequestID, &u.ProviderKeyID, &u.ProviderType, &u.LocalAPIKeyID, &u.Source, &u.LocalModel, &u.UpstreamModel, &u.PromptTokens, &u.CompletionTokens, &u.TotalTokens, &u.UsageSource, &stream, &success, &u.StatusCode, &u.LatencyMS, &u.CreatedAt)
		u.Stream = intBool(stream)
		u.Success = intBool(success)
		items = append(items, u)
	}
	writeJSON(w, 200, items)
}

func (a *App) handleUsageAggregate(w http.ResponseWriter, r *http.Request, field string) {
	where, args, err := usageDateWhere(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	rows, err := a.db.Query(fmt.Sprintf(`SELECT %s, COUNT(*), SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens) FROM usage_records%s GROUP BY %s`, field, where, field), args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var key string
		var count, pt, ct, tt int
		_ = rows.Scan(&key, &count, &pt, &ct, &tt)
		items = append(items, map[string]any{"key": key, "request_count": count, "prompt_tokens": pt, "completion_tokens": ct, "total_tokens": tt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["total_tokens"].(int) > items[j]["total_tokens"].(int) })
	writeJSON(w, 200, items)
}

func (a *App) handleUsageDaily(w http.ResponseWriter, r *http.Request) {
	where, args, err := usageDateWhere(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	rows, err := a.db.Query(`SELECT substr(created_at,1,10) AS day, provider_key_id, upstream_model, COUNT(*), SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens) FROM usage_records`+where+` GROUP BY day,provider_key_id,upstream_model ORDER BY day DESC`, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var day, provider, model string
		var count, pt, ct, tt int
		_ = rows.Scan(&day, &provider, &model, &count, &pt, &ct, &tt)
		items = append(items, map[string]any{"date": day, "provider_key_id": provider, "model": model, "request_count": count, "prompt_tokens": pt, "completion_tokens": ct, "total_tokens": tt})
	}
	writeJSON(w, 200, items)
}

func (a *App) handleUsageExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=usage.csv")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "request_id", "provider_key_id", "provider_type", "source", "local_model", "upstream_model", "prompt_tokens", "completion_tokens", "total_tokens", "usage_source", "created_at"})
	where, args, err := usageDateWhere(r)
	if err != nil {
		_ = cw.Write([]string{"error", err.Error()})
		cw.Flush()
		return
	}
	rows, err := a.db.Query(`SELECT id,request_id,provider_key_id,provider_type,source,local_model,upstream_model,prompt_tokens,completion_tokens,total_tokens,usage_source,created_at FROM usage_records`+where+` ORDER BY datetime(created_at) DESC`, args...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, req, pk, ptp, source, lm, um, us, created string
			var p, c, t int
			_ = rows.Scan(&id, &req, &pk, &ptp, &source, &lm, &um, &p, &c, &t, &us, &created)
			_ = cw.Write([]string{id, req, pk, ptp, source, lm, um, fmt.Sprint(p), fmt.Sprint(c), fmt.Sprint(t), us, created})
		}
	}
	cw.Flush()
}

func usageDateWhere(r *http.Request) (string, []any, error) {
	q := r.URL.Query()
	start := strings.TrimSpace(q.Get("start_date"))
	end := strings.TrimSpace(q.Get("end_date"))
	clauses := []string{}
	args := []any{}
	if start != "" {
		if err := validateDateParam(start, "start_date"); err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "substr(created_at,1,10) >= ?")
		args = append(args, start)
	}
	if end != "" {
		if err := validateDateParam(end, "end_date"); err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "substr(created_at,1,10) <= ?")
		args = append(args, end)
	}
	if len(clauses) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

func validateDateParam(v, name string) error {
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return fmt.Errorf("%s must be YYYY-MM-DD", name)
	}
	return nil
}

func (a *App) handleRequestLogs(w http.ResponseWriter, r *http.Request, sub string) {
	if sub != "" && sub != "/" {
		writeError(w, 404, "not found")
		return
	}
	rows, err := a.db.Query(`SELECT id,source,provider_key_id,local_api_key_id,local_model,upstream_model,method,path,status_code,latency_ms,success,error_message,prompt_tokens,completion_tokens,total_tokens,created_at FROM request_logs ORDER BY datetime(created_at) DESC LIMIT ?`, queryLimit(r, 200))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []RequestLog{}
	for rows.Next() {
		var x RequestLog
		var success int
		_ = rows.Scan(&x.ID, &x.Source, &x.ProviderKeyID, &x.LocalAPIKeyID, &x.LocalModel, &x.UpstreamModel, &x.Method, &x.Path, &x.StatusCode, &x.LatencyMS, &success, &x.ErrorMessage, &x.PromptTokens, &x.CompletionTokens, &x.TotalTokens, &x.CreatedAt)
		x.Success = intBool(success)
		items = append(items, x)
	}
	writeJSON(w, 200, items)
}
