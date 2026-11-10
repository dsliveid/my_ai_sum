package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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
	pendingToolCallIDs := map[string]bool{}
	pendingToolCallCount := 0
	var deferredMessages []map[string]any
	flushDeferredMessages := func() {
		if pendingToolCallCount > 0 || len(deferredMessages) == 0 {
			return
		}
		messages = append(messages, deferredMessages...)
		deferredMessages = nil
	}
	appendRegularMessage := func(msg map[string]any) {
		if pendingToolCallCount > 0 {
			deferredMessages = append(deferredMessages, msg)
			return
		}
		messages = append(messages, msg)
	}
	appendAssistantToolCall := func(call map[string]any) {
		if len(messages) == 0 || stringFromAny(messages[len(messages)-1]["role"], "") != "assistant" {
			messages = append(messages, map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{}})
		}
		last := messages[len(messages)-1]
		last["tool_calls"] = appendChatToolCall(last["tool_calls"], call)
		id := stringFromAny(call["id"], "")
		if id != "" && !pendingToolCallIDs[id] {
			pendingToolCallIDs[id] = true
			pendingToolCallCount++
		}
	}
	appendToolMessage := func(msg map[string]any) {
		callID := stringFromAny(msg["tool_call_id"], "")
		if callID != "" && !pendingToolCallIDs[callID] {
			appendAssistantToolCall(chatToolCall(callID, stringFromAny(msg["name"], "tool"), "{}"))
		}
		messages = append(messages, msg)
		if pendingToolCallIDs[callID] {
			delete(pendingToolCallIDs, callID)
			pendingToolCallCount--
		}
		flushDeferredMessages()
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			if text := strings.TrimSpace(textFromAny(item)); text != "" {
				appendRegularMessage(map[string]any{"role": "user", "content": text})
			}
			continue
		}
		itemType, _ := m["type"].(string)
		if itemType == "function_call_output" {
			msg := map[string]any{"role": "tool", "content": textFromAny(m["output"])}
			if callID, ok := m["call_id"].(string); ok && callID != "" {
				msg["tool_call_id"] = callID
			}
			appendToolMessage(msg)
			continue
		}
		if itemType == "function_call" {
			callID := stringFromAny(m["call_id"], stringFromAny(m["id"], randomID("call")))
			appendAssistantToolCall(chatToolCall(callID, stringFromAny(m["name"], ""), stringFromAny(m["arguments"], "{}")))
			continue
		}
		if itemType != "" && itemType != "message" && m["role"] == nil {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		if role == "developer" {
			role = "system"
		}
		appendRegularMessage(map[string]any{"role": role, "content": textFromAny(m["content"])})
	}
	flushDeferredMessages()
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

func chatToolCall(id, name, arguments string) map[string]any {
	return map[string]any{
		"id":   id,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
}

func appendChatToolCall(v any, call map[string]any) []map[string]any {
	var calls []map[string]any
	switch items := v.(type) {
	case []map[string]any:
		calls = append(calls, items...)
	case []any:
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				calls = append(calls, m)
			}
		}
	}
	return append(calls, call)
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

func chatStreamFinishReason(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	return stringFromAny(choice["finish_reason"], "")
}

func chatStreamToolCallDeltas(body []byte) []chatToolCallDelta {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	items, _ := delta["tool_calls"].([]any)
	var out []chatToolCallDelta
	for fallbackIndex, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		index := intFromAny(m["index"])
		if index == 0 && m["index"] == nil {
			index = fallbackIndex
		}
		fn, _ := m["function"].(map[string]any)
		out = append(out, chatToolCallDelta{
			Index:     index,
			ID:        stringFromAny(m["id"], ""),
			Name:      stringFromAny(fn["name"], ""),
			Arguments: textFromAny(fn["arguments"]),
		})
	}
	return out
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

func looksLikeSSE(contentType string, body []byte) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/event-stream") {
		return true
	}
	s := strings.TrimSpace(string(body))
	return strings.HasPrefix(s, "event:") || strings.HasPrefix(s, "data:")
}

func upstreamNonJSONError(url string, status int, contentType string, body []byte) (string, string) {
	trimmed := strings.TrimSpace(string(body))
	switch {
	case trimmed == "":
		return "upstream_empty_response", fmt.Sprintf("上游接口 %s 返回了空响应，状态码 %d，Content-Type 为 %q。请检查外部服务是否正常返回 OpenAI 兼容 JSON。", url, status, contentType)
	case looksLikeSSE(contentType, body):
		return "upstream_sse_response", fmt.Sprintf("上游接口 %s 返回了 SSE 流式响应，状态码 %d，Content-Type 为 %q；但当前请求按非流式 JSON 处理，系统未能从事件流中聚合出有效结果。请尝试在 API 对话测试中开启流式，或检查该外部服务的请求协议/流式配置。", url, status, contentType)
	case looksLikeHTML(body):
		return "upstream_html_response", fmt.Sprintf("上游接口 %s 返回了 HTML 页面，状态码 %d，Content-Type 为 %q。这通常表示 Base URL 指向了官网、控制台或普通网页，而不是 OpenAI 兼容 API 入口；请检查外部服务 Base URL 是否应使用 API 地址，例如带 /v1 的地址。", url, status, contentType)
	default:
		return "upstream_non_json_response", fmt.Sprintf("上游接口 %s 返回了非 JSON 响应，状态码 %d，Content-Type 为 %q。请查看 body_preview 判断上游实际返回内容，并检查请求协议、模型名称和外部服务配置。", url, status, contentType)
	}
}

func looksLikeHTML(body []byte) bool {
	s := strings.ToLower(strings.TrimSpace(string(body)))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html") || strings.Contains(s, "<body")
}

func responsesSSEToResponsesBody(body []byte, clientModel string) map[string]any {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var fullText strings.Builder
	var lastResponse map[string]any
	responseID := randomID("resp")
	model := clientModel
	createdAt := time.Now().Unix()
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}
		if response, ok := payload["response"].(map[string]any); ok {
			lastResponse = response
			responseID = stringFromAny(response["id"], responseID)
			if clientModel == "" {
				model = stringFromAny(response["model"], model)
			}
			if created := intFromAny(response["created_at"]); created > 0 {
				createdAt = int64(created)
			}
			if stringFromAny(payload["type"], "") == "response.completed" {
				response["model"] = stringFromAny(clientModel, stringFromAny(response["model"], model))
				if strings.TrimSpace(textFromAny(response["output_text"])) == "" {
					response["output_text"] = responseOutputText(response["output"])
				}
				return response
			}
		}
		switch stringFromAny(payload["type"], "") {
		case "response.output_text.delta":
			fullText.WriteString(textFromAny(payload["delta"]))
		case "response.output_text.done":
			if text := textFromAny(payload["text"]); text != "" {
				fullText.Reset()
				fullText.WriteString(text)
			}
		}
	}
	if lastResponse != nil {
		lastResponse["model"] = stringFromAny(clientModel, stringFromAny(lastResponse["model"], model))
		if strings.TrimSpace(textFromAny(lastResponse["output_text"])) == "" {
			text := fullText.String()
			if text == "" {
				text = responseOutputText(lastResponse["output"])
			}
			lastResponse["output_text"] = text
		}
		return lastResponse
	}
	text := fullText.String()
	response := responsesSkeleton(responseID, stringFromAny(clientModel, model), createdAt)
	item := responseMessageItem(randomID("msg"), text)
	response["output"] = []map[string]any{item}
	response["output_text"] = text
	return response
}
