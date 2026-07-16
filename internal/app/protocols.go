package app

import "strings"

const (
	protocolResponses       = "responses"
	protocolChatCompletions = "chat_completions"
)

func normalizeProtocolName(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "responses", "response":
		return protocolResponses
	case "chat_completions", "chat-completions", "chat/completions", "chat":
		return protocolChatCompletions
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
