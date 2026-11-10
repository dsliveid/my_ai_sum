package app

import (
	"net/http"
)

func (a *App) handleRequestLogs(w http.ResponseWriter, r *http.Request, sub string) {
	if sub != "" && sub != "/" {
		writeError(w, 404, "not found")
		return
	}
	rows, err := a.db.Query(`SELECT id,source,provider_key_id,local_api_key_id,local_model,upstream_model,method,path,client_ip,status_code,latency_ms,success,error_message,prompt_tokens,completion_tokens,total_tokens,created_at FROM request_logs ORDER BY datetime(created_at) DESC LIMIT ?`, queryLimit(r, 200))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []RequestLog{}
	for rows.Next() {
		var x RequestLog
		var success int
		_ = rows.Scan(&x.ID, &x.Source, &x.ProviderKeyID, &x.LocalAPIKeyID, &x.LocalModel, &x.UpstreamModel, &x.Method, &x.Path, &x.ClientIP, &x.StatusCode, &x.LatencyMS, &success, &x.ErrorMessage, &x.PromptTokens, &x.CompletionTokens, &x.TotalTokens, &x.CreatedAt)
		x.Success = intBool(success)
		items = append(items, x)
	}
	writeJSON(w, 200, items)
}
