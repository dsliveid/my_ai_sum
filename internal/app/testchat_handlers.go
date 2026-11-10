package app

import (
	"net/http"
)

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
	body["model"] = model
	requestProtocol := normalizeProtocolName(provider.RequestProtocol)
	if requestProtocol == "" {
		requestProtocol = protocolResponses
	}
	requestBody := body
	var res forwardResult
	switch requestProtocol {
	case protocolResponses:
		requestBody = chatBodyToResponsesBody(body)
		requestBody["model"] = model
		requestBody["stream"] = bodyBool(body, "stream")
		res = a.forwardResponses(w, r, "api_chat_test", "", model, model, provider, requestBody, nil)
	case protocolChatCompletions:
		res = a.forwardChat(w, r, "api_chat_test", "", model, model, provider, requestBody, nil)
	default:
		msg := "unsupported provider request protocol: " + requestProtocol
		writeError(w, 400, msg)
		res = forwardResult{StatusCode: 400, Success: false, ErrorMessage: msg, UsageSource: "missing"}
	}
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
	requestProtocol := normalizeProtocolName(localKey.ClientProtocol)
	if requestProtocol == "" {
		requestProtocol = protocolResponses
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
		// 后台测试聊天的请求体可能已被协议转换过，没有可信原始字节，传 nil 走 json.Marshal 回退。
		res = a.forwardChat(w, r, "api_chat_test", localKeyID, model, upstreamModel, provider, requestBody, nil)
	case requestProtocol == protocolChatCompletions && upstreamProtocol == protocolResponses:
		res = a.forwardChatViaResponses(w, r, "api_chat_test", localKeyID, model, upstreamModel, provider, requestBody)
	case requestProtocol == protocolResponses && upstreamProtocol == protocolResponses:
		res = a.forwardResponses(w, r, "api_chat_test", localKeyID, model, upstreamModel, provider, requestBody, nil)
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
