package app

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

func writeSSEErrorEvent(w http.ResponseWriter, message string) {
	writeSSEEvent(w, "error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "stream_incomplete",
			"message": message,
		},
	})
}

func syntheticResponsesCompletedEvent(response map[string]any, model, text string, inputTokens, outputTokens, totalTokens int) map[string]any {
	if response == nil {
		response = responsesSkeleton(randomID("resp"), model, time.Now().Unix())
	}
	response["status"] = "completed"
	response["model"] = stringFromAny(model, stringFromAny(response["model"], ""))
	if strings.TrimSpace(textFromAny(response["output_text"])) == "" {
		response["output_text"] = text
	}
	if _, ok := response["output"]; !ok {
		response["output"] = []map[string]any{responseMessageItem(randomID("msg"), text)}
	}
	if _, ok := response["usage"]; !ok && totalTokens > 0 {
		response["usage"] = map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": totalTokens}
	}
	return map[string]any{"type": "response.completed", "response": response}
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

func responseFunctionCallItem(id, callID, name, arguments, status string) map[string]any {
	if id == "" {
		id = randomID("fc")
	}
	if callID == "" {
		callID = id
	}
	return map[string]any{
		"id":        id,
		"type":      "function_call",
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
		"status":    status,
	}
}

func indexOfInt(items []int, target int) int {
	for i, item := range items {
		if item == target {
			return i
		}
	}
	return len(items)
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
	startedResponse := cloneMap(response)
	startedResponse["status"] = "in_progress"
	writeSSEEvent(w, "response.created", map[string]any{"type": "response.created", "response": startedResponse})
	writeSSEEvent(w, "response.in_progress", map[string]any{"type": "response.in_progress", "response": startedResponse})
	if text := strings.TrimSpace(textFromAny(response["output_text"])); text != "" {
		item := responseMessageItem(itemID, "")
		item["status"] = "in_progress"
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

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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

// clientIP 提取客户端真实 IP：优先 X-Forwarded-For 首段（代理链中最早的原始客户端 IP），
// 其次 X-Real-IP，最后回退 RemoteAddr（去端口）。纯读取请求头，不产生 IO 与阻塞。
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
