package app

import "strings"

const (
	protocolResponses       = "responses"
	protocolChatCompletions = "chat_completions"
	// ProtocolCommandCode 是 CommandCode（仅 Go 套餐）私有协议：
	// 上游唯一端点为 POST /alpha/generate，信封嵌套 params，响应为 NDJSON 事件流。
	// Pro / Max / GOAT 等其他套餐不走本适配器，继续用 chat_completions / responses。
	ProtocolCommandCode = "commandcode"
)

func normalizeProtocolName(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "responses", "response":
		return protocolResponses
	case "chat_completions", "chat-completions", "chat/completions", "chat":
		return protocolChatCompletions
	case "commandcode", "command-code", "command_code", "cc":
		return ProtocolCommandCode
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}

func localKeyUpstreamProtocol(localKey LocalAPIKey, requestProtocol string) (string, error) {
	requestProtocol = normalizeProtocolName(requestProtocol)
	if !localKey.ProtocolConversionEnabled {
		return requestProtocol, nil
	}
	clientProtocol := normalizeProtocolName(localKey.ClientProtocol)
	upstreamProtocol := normalizeProtocolName(localKey.UpstreamProtocol)
	if clientProtocol == "" {
		clientProtocol = requestProtocol
	}
	if upstreamProtocol == "" {
		upstreamProtocol = requestProtocol
	}
	if clientProtocol != requestProtocol {
		return "", protocolError("local key expects client protocol " + clientProtocol + ", got " + requestProtocol)
	}
	return upstreamProtocol, nil
}

type protocolError string

func (e protocolError) Error() string { return string(e) }
