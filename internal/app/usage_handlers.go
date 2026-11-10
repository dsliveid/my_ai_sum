package app

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

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
	rows, err := a.db.Query(`SELECT id,request_id,provider_key_id,provider_type,local_api_key_id,source,local_model,upstream_model,client_ip,prompt_tokens,completion_tokens,total_tokens,usage_source,stream,success,status_code,latency_ms,created_at FROM usage_records`+where+` ORDER BY datetime(created_at) DESC LIMIT ?`, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []UsageRecord{}
	for rows.Next() {
		var u UsageRecord
		var stream, success int
		_ = rows.Scan(&u.ID, &u.RequestID, &u.ProviderKeyID, &u.ProviderType, &u.LocalAPIKeyID, &u.Source, &u.LocalModel, &u.UpstreamModel, &u.ClientIP, &u.PromptTokens, &u.CompletionTokens, &u.TotalTokens, &u.UsageSource, &stream, &success, &u.StatusCode, &u.LatencyMS, &u.CreatedAt)
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

// handleUsageDaily 按 (日期, provider, 模型) 汇总用量。
//
// 数据来源改为「聚合表优先、明细表回退」：聚合表在写入时已用 ON CONFLICT 原子累加，
// 这里直接读现成累计值即可，无需每次全表扫描 usage_records。但历史数据（聚合表上线前
// 的明细）不会回溯进聚合表，因此当聚合表在请求区间内查询不到任何行时，必须回退到
// 明细表的原聚合逻辑，保证旧数据依然可查。两路返回的 JSON 字段名与结构完全一致。
func (a *App) handleUsageDaily(w http.ResponseWriter, r *http.Request) {
	if _, _, err := usageDateWhere(r); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	items, err := a.usageDailyFromAggregates(r)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if len(items) == 0 {
		items, err = a.usageDailyFromRecords(r)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, items)
}

// usageDailyFromAggregates 从日聚合表读取累计值。
//
// 注意必须再按 (date, provider_key_id, model) 做一次 SUM：聚合表主键含 source，
// 同一模型在不同渠道（client_api / api_chat_test）会各占一行，而本接口的返回维度
// 不含 source，只有合并求和才能与明细表 GROUP BY 的结果口径一致。
func (a *App) usageDailyFromAggregates(r *http.Request) ([]map[string]any, error) {
	where, args, err := usageAggregateDateWhere(r)
	if err != nil {
		return nil, err
	}
	rows, err := a.db.Query(`SELECT date, provider_key_id, model, SUM(request_count), SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens) FROM usage_daily_aggregates`+where+` GROUP BY date,provider_key_id,model ORDER BY date DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var day, provider, model string
		var count, pt, ct, tt int
		_ = rows.Scan(&day, &provider, &model, &count, &pt, &ct, &tt)
		items = append(items, map[string]any{"date": day, "provider_key_id": provider, "model": model, "request_count": count, "prompt_tokens": pt, "completion_tokens": ct, "total_tokens": tt})
	}
	return items, rows.Err()
}

// usageDailyFromRecords 是聚合表无数据时的回退分支：保留改动前的明细聚合逻辑原样，
// 确保聚合表上线前的历史数据、以及未走统一收口写入的旧记录仍可查询。
func (a *App) usageDailyFromRecords(r *http.Request) ([]map[string]any, error) {
	where, args, err := usageDateWhere(r)
	if err != nil {
		return nil, err
	}
	rows, err := a.db.Query(`SELECT substr(created_at,1,10) AS day, provider_key_id, upstream_model, COUNT(*), SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens) FROM usage_records`+where+` GROUP BY day,provider_key_id,upstream_model ORDER BY day DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var day, provider, model string
		var count, pt, ct, tt int
		_ = rows.Scan(&day, &provider, &model, &count, &pt, &ct, &tt)
		items = append(items, map[string]any{"date": day, "provider_key_id": provider, "model": model, "request_count": count, "prompt_tokens": pt, "completion_tokens": ct, "total_tokens": tt})
	}
	return items, rows.Err()
}

// usageAggregateDateWhere 与 usageDateWhere 语义一致，但直接作用于聚合表的 date 列。
//
// 之所以不能复用 usageDateWhere：那条拼的是 substr(created_at,1,10) >= ?，聚合表没有
// created_at 列，且它把日期独立成列正是为了让范围比较直接命中主键前缀。
func usageAggregateDateWhere(r *http.Request) (string, []any, error) {
	q := r.URL.Query()
	clauses := []string{}
	args := []any{}
	if start := strings.TrimSpace(q.Get("start_date")); start != "" {
		if err := validateDateParam(start, "start_date"); err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "date >= ?")
		args = append(args, start)
	}
	if end := strings.TrimSpace(q.Get("end_date")); end != "" {
		if err := validateDateParam(end, "end_date"); err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "date <= ?")
		args = append(args, end)
	}
	if len(clauses) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

func (a *App) handleUsageExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=usage.csv")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "request_id", "provider_key_id", "provider_type", "source", "local_model", "upstream_model", "client_ip", "prompt_tokens", "completion_tokens", "total_tokens", "usage_source", "created_at"})
	where, args, err := usageDateWhere(r)
	if err != nil {
		_ = cw.Write([]string{"error", err.Error()})
		cw.Flush()
		return
	}
	rows, err := a.db.Query(`SELECT id,request_id,provider_key_id,provider_type,source,local_model,upstream_model,client_ip,prompt_tokens,completion_tokens,total_tokens,usage_source,created_at FROM usage_records`+where+` ORDER BY datetime(created_at) DESC`, args...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, req, pk, ptp, source, lm, um, cip, us, created string
			var p, c, t int
			_ = rows.Scan(&id, &req, &pk, &ptp, &source, &lm, &um, &cip, &p, &c, &t, &us, &created)
			_ = cw.Write([]string{id, req, pk, ptp, source, lm, um, cip, fmt.Sprint(p), fmt.Sprint(c), fmt.Sprint(t), us, created})
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
