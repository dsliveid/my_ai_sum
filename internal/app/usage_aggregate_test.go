package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// saveUsageForTest 走真实落库入口 saveRequestAndUsage，构造一条用量记录。
// 用同一入口而非裸 SQL，是为了让测试覆盖的正是生产路径（含事务与聚合 upsert）。
func saveUsageForTest(t *testing.T, a *App, reqID, source, model string, success bool, pt, ct, tt int) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return a.saveRequestAndUsage(forwardResult{
		RequestID:        reqID,
		StatusCode:       200,
		Success:          success,
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		UsageSource:      "upstream",
	}, source, ProviderKey{ID: "pk-agg", ProviderType: "openai_compatible"}, "lk-agg", "local-"+model, model, false, req)
}

// TestUsageDailyAggregateUpsertAccumulates 验证 ON CONFLICT 原子 upsert 的累加语义：
// 同一 (date, provider_key_id, model, source) 写两次必须只有一行，且计数与 token 累加。
func TestUsageDailyAggregateUpsertAccumulates(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	if err := saveUsageForTest(t, a, "req-agg-1", "client_api", "up-model", true, 100, 10, 110); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := saveUsageForTest(t, a, "req-agg-2", "client_api", "up-model", false, 200, 20, 220); err != nil {
		t.Fatalf("second save: %v", err)
	}

	var count, success, pt, ct, tt int
	err := a.db.QueryRow(`SELECT request_count, success_count, prompt_tokens, completion_tokens, total_tokens FROM usage_daily_aggregates WHERE provider_key_id=? AND model=? AND source=? AND date=?`,
		"pk-agg", "up-model", "client_api", time.Now().Format("2006-01-02")).
		Scan(&count, &success, &pt, &ct, &tt)
	if err != nil {
		t.Fatalf("query usage_daily_aggregates: %v", err)
	}
	if count != 2 || success != 1 {
		t.Fatalf("aggregate counts = %d/%d, want request_count=2 success_count=1", count, success)
	}
	if pt != 300 || ct != 30 || tt != 330 {
		t.Fatalf("aggregate tokens = %d/%d/%d, want 300/30/330", pt, ct, tt)
	}

	var rows int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM usage_daily_aggregates`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("aggregate rows = %d, want 1 (upsert must not create duplicates)", rows)
	}
}

// TestUsageDailyAggregateSeparatesModelAndSource 验证聚合主键维度：
// 不同 model 或不同 source 必须生成独立行，否则会互相污染累计值。
func TestUsageDailyAggregateSeparatesModelAndSource(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	cases := []struct {
		reqID  string
		source string
		model  string
	}{
		{"req-sep-1", "client_api", "model-a"},
		{"req-sep-2", "client_api", "model-b"},
		{"req-sep-3", "api_chat_test", "model-a"},
	}
	for _, c := range cases {
		if err := saveUsageForTest(t, a, c.reqID, c.source, c.model, true, 10, 1, 11); err != nil {
			t.Fatalf("save %s: %v", c.reqID, err)
		}
	}

	var rows int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM usage_daily_aggregates`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 3 {
		t.Fatalf("aggregate rows = %d, want 3 (model 与 source 均参与主键)", rows)
	}

	var count int
	if err := a.db.QueryRow(`SELECT request_count FROM usage_daily_aggregates WHERE model=? AND source=?`, "model-a", "client_api").Scan(&count); err != nil {
		t.Fatalf("query model-a/client_api: %v", err)
	}
	if count != 1 {
		t.Fatalf("model-a/client_api request_count = %d, want 1", count)
	}
}

// TestSaveRequestAndUsageRollsBackOnFailure 验证事务原子性：
// 第二条记录复用同一 request_logs 主键（必然触发唯一约束失败），
// 此时整个事务必须回滚——request_logs / usage_records / 日聚合三者都不应留下这半笔数据。
func TestSaveRequestAndUsageRollsBackOnFailure(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	if err := saveUsageForTest(t, a, "req-dup", "client_api", "up-model", true, 10, 1, 11); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// 同一 RequestID 再次落库：request_logs.id 主键冲突，函数必须返回 error 而非静默吞掉。
	err := saveUsageForTest(t, a, "req-dup", "client_api", "up-model", true, 10, 1, 11)
	if err == nil {
		t.Fatalf("duplicate request id should return error, got nil")
	}

	var logRows, usageRows int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE id=?`, "req-dup").Scan(&logRows); err != nil {
		t.Fatalf("count request_logs: %v", err)
	}
	if logRows != 1 {
		t.Fatalf("request_logs rows = %d, want 1", logRows)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE request_id=?`, "req-dup").Scan(&usageRows); err != nil {
		t.Fatalf("count usage_records: %v", err)
	}
	if usageRows != 1 {
		t.Fatalf("usage_records rows = %d, want 1 (失败的插入必须随事务回滚)", usageRows)
	}
	var count int
	if err := a.db.QueryRow(`SELECT request_count FROM usage_daily_aggregates WHERE provider_key_id=? AND model=? AND source=?`, "pk-agg", "up-model", "client_api").Scan(&count); err != nil {
		t.Fatalf("query aggregate: %v", err)
	}
	if count != 1 {
		t.Fatalf("aggregate request_count = %d, want 1 (失败事务不得累加)", count)
	}
}

// TestSaveRequestAndUsageRollsBackWhenAggregateStepFails 覆盖「后一步失败、前两步必须回滚」：
// 把聚合表改名使其不存在，第三条 Exec 必然报错。若没有事务，request_logs / usage_records
// 会留下一条孤立的半截数据；有了事务则必须干干净净。
// 用 RENAME 而非 DROP 构造故障，避免在测试里出现破坏性语句。
func TestSaveRequestAndUsageRollsBackWhenAggregateStepFails(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	if _, err := a.db.Exec(`ALTER TABLE usage_daily_aggregates RENAME TO usage_daily_aggregates_bak`); err != nil {
		t.Fatalf("rename aggregate table: %v", err)
	}

	err := saveUsageForTest(t, a, "req-agg-fail", "client_api", "up-model", true, 10, 1, 11)
	if err == nil {
		t.Fatalf("aggregate insert failure should surface as error, got nil")
	}

	var logRows, usageRows int
	if qerr := a.db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE id=?`, "req-agg-fail").Scan(&logRows); qerr != nil {
		t.Fatalf("count request_logs: %v", qerr)
	}
	if logRows != 0 {
		t.Fatalf("request_logs rows = %d, want 0 (前序写入必须随事务回滚)", logRows)
	}
	if qerr := a.db.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE request_id=?`, "req-agg-fail").Scan(&usageRows); qerr != nil {
		t.Fatalf("count usage_records: %v", qerr)
	}
	if usageRows != 0 {
		t.Fatalf("usage_records rows = %d, want 0 (前序写入必须随事务回滚)", usageRows)
	}
}

// getUsageDaily 直接调用 handler 并解出响应，避免测试里再起一遍完整路由。
func getUsageDaily(t *testing.T, a *App, query string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage/daily"+query, nil)
	rec := httptest.NewRecorder()
	a.handleUsageDaily(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage daily status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode usage daily: %v (body=%s)", err, rec.Body.String())
	}
	return items
}

// TestHandleUsageDailyPrefersAggregatesAndFallsBack 同时验证两件事：
// 1) 聚合表有数据时优先读聚合表（明细被改脏也不影响结果）；
// 2) 聚合表无数据时回退到 usage_records 明细聚合，保证历史数据可查。
func TestHandleUsageDailyPrefersAggregatesAndFallsBack(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	if err := saveUsageForTest(t, a, "req-daily", "client_api", "up-model", true, 400, 40, 440); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 把明细改脏，若接口仍返回 400/40/440，说明数据来自聚合表。
	if _, err := a.db.Exec(`UPDATE usage_records SET prompt_tokens=1, completion_tokens=1, total_tokens=2`); err != nil {
		t.Fatalf("dirty usage_records: %v", err)
	}

	items := getUsageDaily(t, a, "")
	if len(items) != 1 {
		t.Fatalf("daily items = %d, want 1: %#v", len(items), items)
	}
	got := items[0]
	if got["model"] != "up-model" || got["provider_key_id"] != "pk-agg" || got["date"] != time.Now().Format("2006-01-02") {
		t.Fatalf("daily keys = %#v", got)
	}
	if int(got["request_count"].(float64)) != 1 ||
		int(got["prompt_tokens"].(float64)) != 400 ||
		int(got["completion_tokens"].(float64)) != 40 ||
		int(got["total_tokens"].(float64)) != 440 {
		t.Fatalf("daily values = %#v, want aggregate 1/400/40/440", got)
	}

	// 清空聚合表后必须回退到明细表（此时读到的是被改脏的 1/1/2）。
	if _, err := a.db.Exec(`DELETE FROM usage_daily_aggregates`); err != nil {
		t.Fatalf("clear aggregates: %v", err)
	}
	items = getUsageDaily(t, a, "")
	if len(items) != 1 {
		t.Fatalf("fallback daily items = %d, want 1: %#v", len(items), items)
	}
	got = items[0]
	if int(got["total_tokens"].(float64)) != 2 || int(got["request_count"].(float64)) != 1 {
		t.Fatalf("fallback values = %#v, want usage_records 1/2", got)
	}

	// 日期区间过滤对聚合表按 date 列生效：区间外应无数据。
	today := time.Now().Format("2006-01-02")
	if items := getUsageDaily(t, a, "?start_date="+today+"&end_date="+today); len(items) != 1 {
		t.Fatalf("today filtered items = %d, want 1", len(items))
	}
	if items := getUsageDaily(t, a, "?start_date=2000-01-01&end_date=2000-01-01"); len(items) != 0 {
		t.Fatalf("old range items = %d, want 0", len(items))
	}
}
