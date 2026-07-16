package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *App) handleProviderKeys(w http.ResponseWriter, r *http.Request, sub string) {
	id, tail := splitID(sub)
	if id == "" {
		switch r.Method {
		case http.MethodGet:
			rows, err := a.db.Query(`SELECT id FROM provider_keys ORDER BY created_at DESC`)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			defer rows.Close()
			items := []ProviderKey{}
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				p, _ := a.getProvider(id, false)
				items = append(items, p)
			}
			writeJSON(w, 200, items)
		case http.MethodPost:
			a.saveProviderKey(w, r, "")
		default:
			methodNotAllowed(w)
		}
		return
	}
	if tail == "/test" && r.Method == http.MethodPost {
		a.testProviderKey(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := a.getProvider(id, r.URL.Query().Get("reveal") == "1")
		if err != nil {
			writeError(w, 404, "provider key not found")
			return
		}
		writeJSON(w, 200, p)
	case http.MethodPut:
		a.saveProviderKey(w, r, id)
	case http.MethodDelete:
		_, err := a.db.Exec(`DELETE FROM provider_keys WHERE id=?`, id)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (a *App) saveProviderKey(w http.ResponseWriter, r *http.Request, id string) {
	var req ProviderKey
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := firstErr(required(req.Name, "name"), required(req.ProviderType, "provider_type")); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	req.BaseURL = defaultBaseURL(req.ProviderType, req.BaseURL)
	req.RequestProtocol = normalizeProtocolName(req.RequestProtocol)
	if req.RequestProtocol == "" {
		req.RequestProtocol = protocolResponses
	}
	if req.ProxyMode == "" {
		req.ProxyMode = "none"
	}
	if req.ProxyMode != "custom" {
		req.ProxyID = ""
	}
	t := now()
	if id == "" {
		if req.APIKey == "" {
			writeError(w, 400, "api_key is required")
			return
		}
		enc, err := a.encryptText(req.APIKey)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		req.ID = randomID("pk")
		_, err = a.db.Exec(`INSERT INTO provider_keys(id,name,provider_type,base_url,request_protocol,api_key_enc,proxy_mode,proxy_id,enabled,models,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			req.ID, req.Name, req.ProviderType, req.BaseURL, req.RequestProtocol, enc, req.ProxyMode, req.ProxyID, boolInt(req.Enabled), req.Models, t, t)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
	} else {
		req.ID = id
		if req.APIKey != "" && !strings.Contains(req.APIKey, "...") {
			enc, err := a.encryptText(req.APIKey)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			_, err = a.db.Exec(`UPDATE provider_keys SET name=?,provider_type=?,base_url=?,request_protocol=?,api_key_enc=?,proxy_mode=?,proxy_id=?,enabled=?,models=?,updated_at=? WHERE id=?`,
				req.Name, req.ProviderType, req.BaseURL, req.RequestProtocol, enc, req.ProxyMode, req.ProxyID, boolInt(req.Enabled), req.Models, t, id)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
		} else {
			_, err := a.db.Exec(`UPDATE provider_keys SET name=?,provider_type=?,base_url=?,request_protocol=?,proxy_mode=?,proxy_id=?,enabled=?,models=?,updated_at=? WHERE id=?`,
				req.Name, req.ProviderType, req.BaseURL, req.RequestProtocol, req.ProxyMode, req.ProxyID, boolInt(req.Enabled), req.Models, t, id)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
		}
	}
	p, _ := a.getProvider(req.ID, false)
	writeJSON(w, 200, p)
}

func (a *App) handleProxies(w http.ResponseWriter, r *http.Request, sub string) {
	id, tail := splitID(sub)
	if id == "" {
		switch r.Method {
		case http.MethodGet:
			rows, err := a.db.Query(`SELECT id FROM proxies ORDER BY is_default DESC, created_at DESC`)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			defer rows.Close()
			items := []ProxyConfig{}
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				p, _ := a.getProxy(id, false)
				p.Password = ""
				items = append(items, p)
			}
			writeJSON(w, 200, items)
		case http.MethodPost:
			a.saveProxy(w, r, "")
		default:
			methodNotAllowed(w)
		}
		return
	}
	if tail == "/test" && r.Method == http.MethodPost {
		a.testProxy(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := a.getProxy(id, false)
		if err != nil {
			writeError(w, 404, "proxy not found")
			return
		}
		writeJSON(w, 200, p)
	case http.MethodPut:
		a.saveProxy(w, r, id)
	case http.MethodDelete:
		_, err := a.db.Exec(`DELETE FROM proxies WHERE id=?`, id)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (a *App) saveProxy(w http.ResponseWriter, r *http.Request, id string) {
	var req ProxyConfig
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := firstErr(required(req.Name, "name"), required(req.Type, "type"), required(req.Host, "host")); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if req.Port <= 0 {
		writeError(w, 400, "port is required")
		return
	}
	t := now()
	if req.IsDefault {
		_, _ = a.db.Exec(`UPDATE proxies SET is_default=0`)
	}
	if id == "" {
		enc, _ := a.encryptText(req.Password)
		req.ID = randomID("px")
		_, err := a.db.Exec(`INSERT INTO proxies(id,name,type,host,port,username,password_enc,enabled,is_default,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			req.ID, req.Name, req.Type, req.Host, req.Port, req.Username, enc, boolInt(req.Enabled), boolInt(req.IsDefault), t, t)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
	} else {
		req.ID = id
		if req.Password != "" && req.Password != "****" {
			enc, _ := a.encryptText(req.Password)
			_, err := a.db.Exec(`UPDATE proxies SET name=?,type=?,host=?,port=?,username=?,password_enc=?,enabled=?,is_default=?,updated_at=? WHERE id=?`,
				req.Name, req.Type, req.Host, req.Port, req.Username, enc, boolInt(req.Enabled), boolInt(req.IsDefault), t, id)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
		} else {
			_, err := a.db.Exec(`UPDATE proxies SET name=?,type=?,host=?,port=?,username=?,enabled=?,is_default=?,updated_at=? WHERE id=?`,
				req.Name, req.Type, req.Host, req.Port, req.Username, boolInt(req.Enabled), boolInt(req.IsDefault), t, id)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
		}
	}
	p, _ := a.getProxy(req.ID, false)
	writeJSON(w, 200, p)
}

func (a *App) handleModelMappings(w http.ResponseWriter, r *http.Request, sub string) {
	id, _ := splitID(sub)
	if id == "" {
		if r.Method == http.MethodGet {
			rows, err := a.db.Query(`SELECT id,local_model,upstream_model,provider_key_id,capability,enabled,created_at,updated_at FROM model_mappings ORDER BY local_model`)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			defer rows.Close()
			items := []ModelMapping{}
			for rows.Next() {
				var m ModelMapping
				var en int
				_ = rows.Scan(&m.ID, &m.LocalModel, &m.UpstreamModel, &m.ProviderKeyID, &m.Capability, &en, &m.CreatedAt, &m.UpdatedAt)
				m.Enabled = intBool(en)
				items = append(items, m)
			}
			writeJSON(w, 200, items)
			return
		}
		if r.Method == http.MethodPost {
			a.saveModelMapping(w, r, "")
			return
		}
		methodNotAllowed(w)
		return
	}
	switch r.Method {
	case http.MethodPut:
		a.saveModelMapping(w, r, id)
	case http.MethodDelete:
		_, err := a.db.Exec(`DELETE FROM model_mappings WHERE id=?`, id)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (a *App) saveModelMapping(w http.ResponseWriter, r *http.Request, id string) {
	var req ModelMapping
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if req.Capability == "" {
		req.Capability = "chat"
	}
	if err := firstErr(required(req.LocalModel, "local_model"), required(req.UpstreamModel, "upstream_model"), required(req.ProviderKeyID, "provider_key_id")); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	t := now()
	if id == "" {
		req.ID = randomID("mm")
		_, err := a.db.Exec(`INSERT INTO model_mappings(id,local_model,upstream_model,provider_key_id,capability,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			req.ID, req.LocalModel, req.UpstreamModel, req.ProviderKeyID, req.Capability, boolInt(req.Enabled), t, t)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
	} else {
		req.ID = id
		_, err := a.db.Exec(`UPDATE model_mappings SET local_model=?,upstream_model=?,provider_key_id=?,capability=?,enabled=?,updated_at=? WHERE id=?`,
			req.LocalModel, req.UpstreamModel, req.ProviderKeyID, req.Capability, boolInt(req.Enabled), t, id)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, req)
}

func (a *App) handleLocalAPIKeys(w http.ResponseWriter, r *http.Request, sub string) {
	id, _ := splitID(sub)
	if id == "" {
		if r.Method == http.MethodGet {
			rows, err := a.db.Query(`SELECT id,name,key_enc,provider_key_id,protocol_conversion_enabled,client_protocol,upstream_protocol,enabled,created_at,last_used_at FROM local_api_keys ORDER BY created_at DESC`)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			defer rows.Close()
			items := []LocalAPIKey{}
			for rows.Next() {
				var k LocalAPIKey
				var en, protocolConversionEnabled int
				var enc string
				_ = rows.Scan(&k.ID, &k.Name, &enc, &k.ProviderKeyID, &protocolConversionEnabled, &k.ClientProtocol, &k.UpstreamProtocol, &en, &k.CreatedAt, &k.LastUsedAt)
				k.Enabled = intBool(en)
				k.ProtocolConversionEnabled = intBool(protocolConversionEnabled)
				if enc != "" {
					key, _ := a.decryptText(enc)
					k.KeyMasked = maskSecret(key)
				} else {
					k.KeyMasked = "不可恢复"
				}
				items = append(items, k)
			}
			writeJSON(w, 200, items)
			return
		}
		if r.Method == http.MethodPost {
			var req LocalAPIKey
			_ = readJSON(r, &req)
			if req.Name == "" {
				req.Name = "default"
			}
			a.normalizeLocalKeyProtocolConfig(&req)
			secret := randomSecret("myas")
			salt := newSalt()
			enc, err := a.encryptText(secret)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			k := LocalAPIKey{ID: randomID("lk"), Name: req.Name, Key: secret, ProviderKeyID: req.ProviderKeyID, ProtocolConversionEnabled: req.ProtocolConversionEnabled, ClientProtocol: req.ClientProtocol, UpstreamProtocol: req.UpstreamProtocol, Enabled: true, CreatedAt: now()}
			k.KeyMasked = maskSecret(secret)
			_, err = a.db.Exec(`INSERT INTO local_api_keys(id,name,key_hash,salt,key_enc,provider_key_id,protocol_conversion_enabled,client_protocol,upstream_protocol,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, k.ID, k.Name, hashSecret(secret, salt), salt, enc, k.ProviderKeyID, boolInt(k.ProtocolConversionEnabled), k.ClientProtocol, k.UpstreamProtocol, 1, k.CreatedAt)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, k)
			return
		}
		methodNotAllowed(w)
		return
	}
	if r.Method == http.MethodGet {
		var k LocalAPIKey
		var en, protocolConversionEnabled int
		var enc string
		err := a.db.QueryRow(`SELECT id,name,key_enc,provider_key_id,protocol_conversion_enabled,client_protocol,upstream_protocol,enabled,created_at,last_used_at FROM local_api_keys WHERE id=?`, id).Scan(&k.ID, &k.Name, &enc, &k.ProviderKeyID, &protocolConversionEnabled, &k.ClientProtocol, &k.UpstreamProtocol, &en, &k.CreatedAt, &k.LastUsedAt)
		if err != nil {
			writeError(w, 404, "local api key not found")
			return
		}
		k.Enabled = intBool(en)
		k.ProtocolConversionEnabled = intBool(protocolConversionEnabled)
		if r.URL.Query().Get("reveal") == "1" && enc != "" {
			k.Key, _ = a.decryptText(enc)
		}
		if enc != "" {
			key, _ := a.decryptText(enc)
			k.KeyMasked = maskSecret(key)
		} else {
			k.KeyMasked = "不可恢复"
		}
		writeJSON(w, 200, k)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var req LocalAPIKey
		if err := readJSON(r, &req); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		a.normalizeLocalKeyProtocolConfig(&req)
		_, err := a.db.Exec(`UPDATE local_api_keys SET name=?,provider_key_id=?,protocol_conversion_enabled=?,client_protocol=?,upstream_protocol=?,enabled=? WHERE id=?`, req.Name, req.ProviderKeyID, boolInt(req.ProtocolConversionEnabled), req.ClientProtocol, req.UpstreamProtocol, boolInt(req.Enabled), id)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodDelete:
		_, err := a.db.Exec(`DELETE FROM local_api_keys WHERE id=?`, id)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (a *App) normalizeLocalKeyProtocolConfig(k *LocalAPIKey) {
	k.ClientProtocol = normalizeProtocolName(k.ClientProtocol)
	k.UpstreamProtocol = normalizeProtocolName(k.UpstreamProtocol)
	if k.ClientProtocol == "" {
		k.ClientProtocol = protocolResponses
	}
	if k.UpstreamProtocol == "" {
		k.UpstreamProtocol = protocolResponses
		if k.ProviderKeyID != "" {
			if p, err := a.getProvider(k.ProviderKeyID, false); err == nil && p.RequestProtocol != "" {
				k.UpstreamProtocol = p.RequestProtocol
			}
		}
	}
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, a.cfg)
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w)
		return
	}
	var req Config
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(req.Host) != "" {
		a.cfg.Host = strings.TrimSpace(req.Host)
	}
	if req.Port > 0 {
		if req.Port > 65535 {
			writeError(w, 400, "port must be between 1 and 65535")
			return
		}
		a.cfg.Port = req.Port
	}
	a.cfg.AutoOpen = req.AutoOpen
	if req.LogLevel != "" {
		a.cfg.LogLevel = req.LogLevel
	}
	a.cfg.APIDebugEnabled = req.APIDebugEnabled
	a.cfg.APIDebugLevel = req.APIDebugLevel
	a.cfg.APIDebugRequestBody = req.APIDebugRequestBody
	a.cfg.APIDebugResponseBody = req.APIDebugResponseBody
	a.cfg.APIDebugMaxBodyChars = req.APIDebugMaxBodyChars
	normalizeAPIDebugConfig(&a.cfg)
	if err := saveConfig(a.cfg); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, a.cfg)
}

func (a *App) handleSettingsReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	url, old, err := a.switchHTTPServer(a.cfg.Host, a.cfg.Port)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "url": url, "host": a.cfg.Host, "port": a.cfg.Port})
	go shutdownOldServer(old)
}

func (a *App) testProviderKey(w http.ResponseWriter, r *http.Request, id string) {
	p, err := a.getProvider(id, true)
	if err != nil {
		writeError(w, 404, "provider not found")
		return
	}
	client, err := a.httpClientForProvider(p)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	start := now()
	result := a.runProviderHealthCheck(client, p)
	_, _ = a.db.Exec(`UPDATE provider_keys SET last_check_status=?,last_error=?,updated_at=? WHERE id=?`, result["status"], result["error"], start, id)
	writeJSON(w, 200, result)
}

func looksLikeModelsJSON(contentType string, body []byte) bool {
	if !strings.Contains(strings.ToLower(contentType), "json") {
		return false
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return true
}

func (a *App) runProviderHealthCheck(client *http.Client, p ProviderKey) map[string]any {
	attempts := []map[string]any{}
	for _, testedURL := range modelTestURLs(p.BaseURL) {
		req, _ := http.NewRequest(http.MethodGet, testedURL, nil)
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		resp, err := client.Do(req)
		attempt := map[string]any{"method": "GET", "url": testedURL}
		if err != nil {
			attempt["error"] = err.Error()
			attempts = append(attempts, attempt)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		attempt["status_code"] = resp.StatusCode
		attempt["content_type"] = resp.Header.Get("Content-Type")
		attempt["body"] = summarizeText(string(body), 240)
		attempts = append(attempts, attempt)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && looksLikeModelsJSON(resp.Header.Get("Content-Type"), body) {
			return map[string]any{"status": "ok", "error": "", "tested_url": testedURL, "status_code": resp.StatusCode, "content_type": resp.Header.Get("Content-Type"), "stage": "models", "attempts": attempts}
		}
	}
	if model := firstConfiguredModel(p.Models); model != "" {
		for _, tc := range chatTestCandidates(p.BaseURL, model, p.RequestProtocol) {
			resp, err := doJSONRequest(client, tc.Method, tc.URL, p.APIKey, tc.Body)
			attempt := map[string]any{"method": tc.Method, "url": tc.URL, "model": model, "api": tc.API}
			if err != nil {
				attempt["error"] = err.Error()
				attempts = append(attempts, attempt)
				continue
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			attempt["status_code"] = resp.StatusCode
			attempt["content_type"] = resp.Header.Get("Content-Type")
			attempt["body"] = summarizeText(string(body), 240)
			attempts = append(attempts, attempt)
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
				return map[string]any{"status": "ok", "error": "", "tested_url": tc.URL, "status_code": resp.StatusCode, "content_type": resp.Header.Get("Content-Type"), "stage": tc.API, "attempts": attempts}
			}
		}
	}
	msg := "provider test failed; Base URL may need /v1 or this provider may not expose /models"
	if len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		if v, ok := last["status_code"]; ok {
			msg = "last attempt failed with status " + fmt.Sprint(v)
			if body, ok := last["body"].(string); ok && body != "" {
				msg += ": " + body
			}
		} else if v, ok := last["error"].(string); ok {
			msg = v
		}
	}
	return map[string]any{"status": "failed", "error": msg, "attempts": attempts}
}

type providerTestCandidate struct {
	Method string
	URL    string
	API    string
	Body   map[string]any
}

func modelTestURLs(baseURL string) []string {
	base := normalizeBaseURL(baseURL)
	urls := []string{base + "/models"}
	if !strings.HasSuffix(base, "/v1") {
		urls = append(urls, base+"/v1/models")
	}
	return urls
}

func chatTestCandidates(baseURL, model, requestProtocol string) []providerTestCandidate {
	base := normalizeBaseURL(baseURL)
	chatBody := map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "ping"}}, "max_tokens": 1, "stream": false}
	responsesBody := map[string]any{"model": model, "input": "ping", "max_output_tokens": 1}
	var out []providerTestCandidate
	requestProtocol = normalizeProtocolName(requestProtocol)
	if requestProtocol == "" {
		requestProtocol = protocolResponses
	}
	if requestProtocol == protocolChatCompletions {
		out = append(out, providerTestCandidate{Method: http.MethodPost, URL: base + "/chat/completions", API: "chat_completions", Body: chatBody})
	} else {
		out = append(out, providerTestCandidate{Method: http.MethodPost, URL: base + "/responses", API: "responses", Body: responsesBody})
	}
	if !strings.HasSuffix(base, "/v1") {
		if requestProtocol == protocolChatCompletions {
			out = append(out, providerTestCandidate{Method: http.MethodPost, URL: base + "/v1/chat/completions", API: "chat_completions", Body: chatBody})
		} else {
			out = append(out, providerTestCandidate{Method: http.MethodPost, URL: base + "/v1/responses", API: "responses", Body: responsesBody})
		}
	}
	return out
}

func firstConfiguredModel(models string) string {
	for _, item := range strings.Split(models, ",") {
		if m := strings.TrimSpace(item); m != "" {
			return m
		}
	}
	return ""
}

func doJSONRequest(client *http.Client, method, url, apiKey string, body map[string]any) (*http.Response, error) {
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

func (a *App) testProxy(w http.ResponseWriter, r *http.Request, id string) {
	p, err := a.getProxy(id, true)
	if err != nil {
		writeError(w, 404, "proxy not found")
		return
	}
	start := time.Now()
	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		_, _ = a.db.Exec(`UPDATE proxies SET last_check_status=?,last_error=?,updated_at=? WHERE id=?`, "failed", err.Error(), now(), id)
		writeJSON(w, 200, map[string]any{"status": "failed", "error": err.Error(), "stage": "tcp", "latency_ms": time.Since(start).Milliseconds()})
		return
	}
	_ = conn.Close()
	target := r.URL.Query().Get("target")
	if target == "" {
		_, _ = a.db.Exec(`UPDATE proxies SET last_check_status=?,last_error=?,updated_at=? WHERE id=?`, "ok", "", now(), id)
		writeJSON(w, 200, map[string]any{
			"status":     "ok",
			"error":      "",
			"stage":      "tcp",
			"latency_ms": time.Since(start).Milliseconds(),
			"message":    "proxy port is reachable; no target URL tested",
		})
		return
	}
	fake := ProviderKey{ProxyMode: "custom", ProxyID: p.ID}
	client, err := a.httpClientForProvider(fake)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	resp, err := client.Get(target)
	status := "ok"
	msg := ""
	if err != nil {
		status = "failed"
		msg = err.Error()
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			status = "failed"
			msg = resp.Status
		}
	}
	_, _ = a.db.Exec(`UPDATE proxies SET last_check_status=?,last_error=?,updated_at=? WHERE id=?`, status, msg, now(), id)
	writeJSON(w, 200, map[string]any{"status": status, "error": msg, "stage": "target", "target": target, "latency_ms": time.Since(start).Milliseconds()})
}

func (a *App) listModelsFromMappings() ([]map[string]any, error) {
	rows, err := a.db.Query(`SELECT local_model FROM model_mappings WHERE enabled=1 ORDER BY local_model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var m string
		_ = rows.Scan(&m)
		data = append(data, map[string]any{"id": m, "object": "model", "created": 0, "owned_by": "my_ai_sum"})
	}
	if len(data) == 0 {
		prows, err := a.db.Query(`SELECT models FROM provider_keys WHERE enabled=1`)
		if err != nil {
			return nil, err
		}
		defer prows.Close()
		seen := map[string]bool{}
		for prows.Next() {
			var models string
			_ = prows.Scan(&models)
			for _, item := range strings.Split(models, ",") {
				name := strings.TrimSpace(item)
				if name != "" && !seen[name] {
					seen[name] = true
					data = append(data, map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "my_ai_sum"})
				}
			}
		}
	}
	return data, nil
}

func scanNullString(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

func atoiDefault(s string, d int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return v
}
