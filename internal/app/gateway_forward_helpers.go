package app

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
)

func upstreamURLFromResponse(resp *http.Response) string {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return ""
	}
	return resp.Request.URL.String()
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

func readStreamLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func bodyHasResponsesInput(body map[string]any) bool {
	_, ok := body["input"]
	return ok
}

// forwardStreamDiagnosticHeaders 在流式路径开始写 SSE 之前，把上游的诊断类响应头透传给客户端。
//
// 非流式路径已有 copyHeader 全量透传，但流式路径是自写 SSE，此前完全丢弃上游响应头，
// 导致客户端无法看到节点身份（x-served-by / server 等）。当上游为多副本集群时，
// 这是判断「同一会话是否被路由到同一后端」的唯一外部线索，缺失会让缓存命中问题无法定位。
//
// 只透传白名单：既避免把上游的 Content-Length / Content-Encoding（与 SSE 冲突）
// 或 Set-Cookie 等敏感头泄给客户端，又保留诊断所需的最小信息集。
func forwardStreamDiagnosticHeaders(w http.ResponseWriter, resp *http.Response) {
	if resp == nil {
		return
	}
	for _, key := range []string{
		"X-Served-By", "Server", "X-Request-Id", "X-Model", "X-Backend",
		"Cf-Ray", "X-Amzn-Requestid", "X-Cache", "Via", "X-Upstream-Host",
	} {
		if v := resp.Header.Get(key); v != "" {
			w.Header().Set(key, v)
		}
	}
}

// streamCacheUsage 累积流式响应中的缓存命中用量。
// 流式场景下缓存数字只在最终 usage 分片中出现（chat: 某条 chunk 的 usage；
// responses: response.completed 里的 response.usage），因此需要逐分片探测并保留最后一次有效值。
// 与 withCacheUsage 的区别：后者针对完整响应体一次性解析，此处面向流式增量。
type streamCacheUsage struct {
	cached   int
	creation int
}

// observe 从单个 SSE data 分片中提取缓存用量，仅在有值时更新。
func (s *streamCacheUsage) observe(data []byte) {
	if s == nil || len(data) == 0 {
		return
	}
	cached, creation := parseCacheUsage(data)
	if cached > 0 {
		s.cached = cached
	}
	if creation > 0 {
		s.creation = creation
	}
}

// fill 把累积结果写入 forwardResult。
func (s *streamCacheUsage) fill(res forwardResult) forwardResult {
	if s == nil {
		return res
	}
	res.CachedTokens = s.cached
	res.CacheCreationTokens = s.creation
	return res
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
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n)
		}
		return 0
	default:
		return 0
	}
}

// parseCacheUsage 从上游响应体中解析缓存命中相关用量。
// 上游协议在缓存字段上并不统一，需同时兼容三种形态：
//   - OpenAI:      usage.prompt_tokens_details.cached_tokens（部分实现平铺为 usage.cached_tokens）
//   - OpenAI 新版: usage.input_tokens_details.cached_tokens
//   - Anthropic:   usage.cache_read_input_tokens / usage.cache_creation_input_tokens
//
// 之所以独立于 extractUsage：后者被 6 处以上调用且已受既有测试断言保护，
// 直接改签名会波及流式与非流式全部路径；此处新增只读解析，互不影响。
func parseCacheUsage(body []byte) (cached int, creation int) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return 0, 0
	}
	usage, ok := m["usage"].(map[string]any)
	if !ok {
		// 兼容 responses 风格：部分实现把用量放在 response.usage
		if resp, ok2 := m["response"].(map[string]any); ok2 {
			usage, ok = resp["usage"].(map[string]any)
		}
		if !ok {
			return 0, 0
		}
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		cached = intFromAny(details["cached_tokens"])
	}
	if cached == 0 {
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			cached = intFromAny(details["cached_tokens"])
		}
	}
	if cached == 0 {
		cached = intFromAny(usage["cached_tokens"])
	}
	if cached == 0 {
		cached = intFromAny(usage["cache_read_input_tokens"])
	}
	creation = intFromAny(usage["cache_creation_input_tokens"])
	return cached, creation
}

// withCacheUsage 把缓存用量补充进 forwardResult，供落库与调试日志使用。
func withCacheUsage(res forwardResult, body []byte) forwardResult {
	cached, creation := parseCacheUsage(body)
	res.CachedTokens = cached
	res.CacheCreationTokens = creation
	return res
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
