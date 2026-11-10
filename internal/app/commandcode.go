package app

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CommandCode（仅 Go 套餐）协议适配器。
//
// 上游唯一端点 POST /alpha/generate，信封嵌套 params，响应为 NDJSON 逐行事件流
// （不是 SSE event: 帧），且上游不发 [DONE] —— 全部由本层补齐。
//
// 四条对外路径（客户端 chat 流 / 客户端 responses 流 / 客户端 chat 非流式 /
// 客户端 responses 非流式）共用同一次 NDJSON 扫描，统计收口统一走
// finish / failForward / finishStream，本文件不直接写库或写调试日志。

const (
	// ccUpstreamPath 是 Go 套餐唯一可用端点。
	ccUpstreamPath = "/alpha/generate"
	// ccClientVersion 必须与真实 CLI 版本一致，否则上游可能拒绝请求。
	ccClientVersion = "1.53.1"
	// ccDefaultMaxTokens / ccMaxTokensLimit 决定 params.max_tokens 的默认值与上限。
	ccDefaultMaxTokens = 64000
	ccMaxTokensLimit   = 200000
	// ccSessionTTL 是 x-session-id 的复用窗口（12h + 抖动）：
	// 复用可命中上游会话级缓存，抖动避免同批实例同时失效。
	ccSessionTTL    = 12 * time.Hour
	ccSessionJitter = 30 * time.Minute
	// ccPlaceholderSystem 是无法从请求中提取 system 时必须发送的占位符。
	// 实测：不发 system 时上游会注入自身约 7.5K token 默认提示词（prompt_tokens 7653→85）。
	ccPlaceholderSystem = " "
)

// ccUpstreamUserAgent 是发给上游的 CLI 身份标识（upstream.go 在 commandcode 协议下会采用它）。
const ccUpstreamUserAgent = "command-code/" + ccClientVersion

// ccRequestError 表示请求本身不被 CommandCode 上游支持，应回 400。
type ccRequestError string

func (e ccRequestError) Error() string { return string(e) }

// ccUpstreamHeaders 复制入站请求头并注入 CommandCode 私有头。
// 之所以能生效：这些键已登记进 upstream.go 的 upstreamForwardHeaders 白名单，
// doUpstream 的透传逻辑才会把它们带到上游（函数签名未做任何改动）。
func ccUpstreamHeaders(in http.Header, provider ProviderKey) http.Header {
	out := http.Header{}
	if in != nil {
		out = in.Clone()
	}
	out.Set("X-Command-Code-Version", ccClientVersion)
	out.Set("X-Cli-Environment", "production")
	out.Set("X-Project-Slug", slugifyProjectPath(provider.Name))
	out.Set("X-Taste-Learning", "false")
	out.Set("X-Session-Id", ccSessionFor(provider.ID))
	out.Set("Traceparent", generateTraceparent())
	return out
}

// generateTraceparent 生成 W3C traceparent：00-<32hex>-<16hex>-01。
func generateTraceparent() string {
	trace := make([]byte, 16)
	span := make([]byte, 8)
	_, _ = rand.Read(trace)
	_, _ = rand.Read(span)
	return "00-" + hex.EncodeToString(trace) + "-" + hex.EncodeToString(span) + "-01"
}

// slugifyProjectPath 把项目路径/名称转成上游 x-project-slug 可接受的短标识。
func slugifyProjectPath(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	var b strings.Builder
	lastDash := false
	for _, r := range p {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "my-ai-sum"
	}
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-")
	}
	return out
}

// ccSessionEntry 是 provider 维度的 x-session-id 缓存条目。
type ccSessionEntry struct {
	id      string
	expires time.Time
}

var ccSessionCache = struct {
	sync.Mutex
	m map[string]ccSessionEntry
}{m: map[string]ccSessionEntry{}}

// ccSessionFor 返回 provider 维度的 x-session-id，12h + 抖动内复用同一个值。
func ccSessionFor(providerID string) string {
	key := providerID
	if strings.TrimSpace(key) == "" {
		key = "default"
	}
	now := time.Now()
	ccSessionCache.Lock()
	defer ccSessionCache.Unlock()
	if e, ok := ccSessionCache.m[key]; ok && now.Before(e.expires) && e.id != "" {
		return e.id
	}
	// 抖动由纳秒低位派生，避免引入额外随机源，同时让各 provider 的失效时刻错开。
	jitter := time.Duration(now.UnixNano()%int64(2*ccSessionJitter)) - ccSessionJitter
	id := ccUUID()
	ccSessionCache.m[key] = ccSessionEntry{id: id, expires: now.Add(ccSessionTTL + jitter)}
	return id
}

// ccUUID 生成标准 UUID v4 字符串（上游对 x-session-id / threadId 格式有要求）。
func ccUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ---------------------------------------------------------------------------
// 请求信封构造
// ---------------------------------------------------------------------------

// ccBodyFromChat 把 OpenAI chat/completions 请求体折叠进 CommandCode 信封。
func ccBodyFromChat(body map[string]any, upstreamModel string) (map[string]any, error) {
	if v, ok := body["response_format"]; ok && v != nil && !ccIsTextFormat(v) {
		return nil, ccRequestError("response_format other than text is not supported by commandcode upstream")
	}
	messages := ccAsMapSlice(body["messages"])
	params := map[string]any{
		"model":       upstreamModel,
		"messages":    ccMessagesFromChat(messages),
		"system":      ccSystemBlocks(messages, ""),
		"tools":       ccToolsFromChat(body["tools"]),
		"tool_choice": ccToolChoice(body["tool_choice"]),
		"max_tokens":  ccMaxTokens(body["max_tokens"]),
		// 上游没有「非流式」开关，恒发 stream:true，非流式由本层聚合 NDJSON 得出。
		"stream": true,
	}
	ccCopyParams(params, body, "temperature", "top_p", "reasoning_effort", "parallel_tool_calls")
	return ccEnvelope(params, body), nil
}

// ccBodyFromResponses 把 OpenAI responses 请求体折叠进 CommandCode 信封。
func ccBodyFromResponses(body map[string]any, upstreamModel string) (map[string]any, error) {
	if v := strings.TrimSpace(stringFromAny(body["previous_response_id"], "")); v != "" {
		return nil, ccRequestError("previous_response_id is not supported by commandcode upstream")
	}
	if text, ok := body["text"].(map[string]any); ok {
		if f, ok2 := text["format"]; ok2 && f != nil && !ccIsTextFormat(f) {
			return nil, ccRequestError("text.format other than text is not supported by commandcode upstream")
		}
	}
	instructions := strings.TrimSpace(textFromAny(body["instructions"]))
	// 复用既有 responses→chat messages 转换，function_call / function_call_output 语义已在此收敛。
	messages := responsesInputToChatMessages(body["input"])
	params := map[string]any{
		"model":       upstreamModel,
		"messages":    ccMessagesFromChat(messages),
		"system":      ccSystemBlocks(messages, instructions),
		"tools":       ccToolsFromChat(body["tools"]),
		"tool_choice": ccToolChoice(body["tool_choice"]),
		"max_tokens":  ccMaxTokens(firstNonEmptyInt(body["max_output_tokens"], body["max_tokens"])),
		"stream":      true,
	}
	ccCopyParams(params, body, "temperature", "top_p", "parallel_tool_calls")
	if v, ok := body["reasoning"].(map[string]any); ok {
		if effort := strings.TrimSpace(stringFromAny(v["effort"], "")); effort != "" {
			params["reasoning_effort"] = effort
		}
	} else if effort := strings.TrimSpace(stringFromAny(body["reasoning_effort"], "")); effort != "" {
		params["reasoning_effort"] = effort
	}
	return ccEnvelope(params, body), nil
}

// ccEnvelope 组装完整信封骨架。params 之外的部分对上游是固定协议字段。
func ccEnvelope(params map[string]any, src map[string]any) map[string]any {
	slug := "my-ai-sum"
	env := map[string]any{
		"config": map[string]any{
			"workingDir":  "/workspace/" + slug,
			"date":        time.Now().UTC().Format(time.RFC3339),
			"environment": "production",
			"structure":   []any{},
			"isGitRepo":   false,
		},
		"memory":         nil,
		"taste":          nil,
		"skills":         nil,
		"permissionMode": "standard",
		"mode":           "agent",
		"params":         params,
	}
	if tid := strings.TrimSpace(stringFromAny(src["thread_id"], "")); tid != "" {
		env["threadId"] = tid
	}
	return env
}

// ccCopyParams 把客户端请求里的可选参数原样搬进 params。
func ccCopyParams(params map[string]any, body map[string]any, keys ...string) {
	for _, key := range keys {
		if v, ok := body[key]; ok && v != nil {
			params[key] = v
		}
	}
}

// ccMaxTokens 归一 max_tokens：缺省 64000，上限 200000。
func ccMaxTokens(v any) int {
	n := intFromAny(v)
	if n <= 0 {
		n = ccDefaultMaxTokens
	}
	if n > ccMaxTokensLimit {
		n = ccMaxTokensLimit
	}
	return n
}

func firstNonEmptyInt(values ...any) any {
	for _, v := range values {
		if intFromAny(v) > 0 {
			return v
		}
	}
	return nil
}

// ccIsTextFormat 判断 response_format / text.format 是否为纯文本输出。
func ccIsTextFormat(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "text")
	case map[string]any:
		t := strings.ToLower(strings.TrimSpace(stringFromAny(x["type"], "")))
		return t == "" || t == "text"
	}
	return false
}

// ccSystemBlocks 汇总 system 提示词为块数组；为空时注入空格占位符防止上游注入自身默认提示词。
func ccSystemBlocks(messages []map[string]any, instructions string) []map[string]any {
	var parts []string
	if s := strings.TrimSpace(instructions); s != "" {
		parts = append(parts, s)
	}
	for _, m := range messages {
		role := strings.ToLower(stringFromAny(m["role"], ""))
		if role != "system" && role != "developer" {
			continue
		}
		if t := strings.TrimSpace(textFromAny(m["content"])); t != "" {
			parts = append(parts, t)
		}
	}
	text := strings.Join(parts, "\n\n")
	if strings.TrimSpace(text) == "" {
		text = ccPlaceholderSystem
	}
	return []map[string]any{{"type": "text", "text": text}}
}

// ccMessagesFromChat 把 chat messages 转成 CommandCode 的块数组形态。
// 关键点：assistant 块的顺序固定为 reasoning → text → tool-call；
// tool 消息的 toolName 必须由 tool_call_id 反查回填（上游不接受缺失 toolName 的 tool-result）。
func ccMessagesFromChat(messages []map[string]any) []map[string]any {
	toolNames := map[string]string{}
	for _, m := range messages {
		for _, tc := range ccAsMapSlice(m["tool_calls"]) {
			callID := stringFromAny(tc["id"], "")
			if callID == "" {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			toolNames[callID] = ccNormalizeToolName(stringFromAny(fn["name"], ""))
		}
	}
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(stringFromAny(m["role"], "user")))
		switch role {
		case "system", "developer":
			// 已折叠进 params.system，正文不再重复。
			continue
		case "assistant":
			blocks := make([]map[string]any, 0, 3)
			if rc := textFromAny(m["reasoning_content"]); strings.TrimSpace(rc) != "" {
				blocks = append(blocks, map[string]any{"type": "reasoning", "text": rc})
			}
			if c := textFromAny(m["content"]); strings.TrimSpace(c) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": c})
			}
			for _, tc := range ccAsMapSlice(m["tool_calls"]) {
				fn, _ := tc["function"].(map[string]any)
				blocks = append(blocks, map[string]any{
					"type":       "tool-call",
					"toolCallId": stringFromAny(tc["id"], ""),
					"toolName":   ccNormalizeToolName(stringFromAny(fn["name"], "")),
					"input":      ccToolInputFromArguments(stringFromAny(fn["arguments"], "")),
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": ""})
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			callID := stringFromAny(m["tool_call_id"], "")
			name := ccNormalizeToolName(firstNonEmpty(stringFromAny(m["name"], ""), toolNames[callID]))
			out = append(out, map[string]any{"role": "tool", "content": []map[string]any{{
				"type":       "tool-result",
				"toolCallId": callID,
				"toolName":   name,
				"output":     map[string]any{"type": "text", "value": textFromAny(m["content"])},
			}}})
		default:
			out = append(out, map[string]any{"role": "user", "content": ccUserBlocks(m["content"])})
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": ""}}})
	}
	return out
}

// ccUserBlocks 把 user content 转成 text / image 块数组。
func ccUserBlocks(v any) []map[string]any {
	switch x := v.(type) {
	case string:
		return []map[string]any{{"type": "text", "text": x}}
	case []map[string]any:
		return ccUserBlocksFrom(x)
	case []any:
		return ccUserBlocksFrom(ccAsMapSlice(x))
	}
	text := textFromAny(v)
	return []map[string]any{{"type": "text", "text": text}}
}

func ccUserBlocksFrom(items []map[string]any) []map[string]any {
	if len(items) == 0 {
		return []map[string]any{{"type": "text", "text": ""}}
	}
	blocks := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch strings.ToLower(strings.TrimSpace(stringFromAny(item["type"], ""))) {
		case "image", "image_url", "input_image":
			url, mime := "", ""
			switch img := item["image_url"].(type) {
			case string:
				url = img
			case map[string]any:
				url = stringFromAny(img["url"], "")
			}
			if url == "" {
				url = firstNonEmpty(stringFromAny(item["url"], ""), stringFromAny(item["image"], ""))
			}
			if raw, ok := item["image_url"].(map[string]any); ok {
				mime = stringFromAny(raw["mime_type"], "")
			}
			if mime == "" {
				mime = firstNonEmpty(stringFromAny(item["mime_type"], ""), ccMimeFromDataURL(url))
			}
			blocks = append(blocks, map[string]any{"type": "image", "image": url, "mimeType": mime})
		default:
			blocks = append(blocks, map[string]any{"type": "text", "text": textFromAny(item["text"])})
		}
	}
	return blocks
}

// ccMimeFromDataURL 从 data URL 前缀解析 mimeType（data:image/png;base64,...）。
func ccMimeFromDataURL(url string) string {
	if !strings.HasPrefix(url, "data:") {
		return ""
	}
	rest := strings.TrimPrefix(url, "data:")
	if i := strings.IndexByte(rest, ';'); i > 0 {
		return rest[:i]
	}
	if i := strings.IndexByte(rest, ','); i > 0 {
		return rest[:i]
	}
	return ""
}

// ccToolInputFromArguments 把 OpenAI 的 arguments JSON 字符串转成对象。
func ccToolInputFromArguments(args string) any {
	args = strings.TrimSpace(args)
	if args == "" {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal([]byte(args), &out); err == nil {
		return out
	}
	return map[string]any{"raw": args}
}

// ccToolsFromChat 把 chat / responses 两种 tools 形态统一成 CommandCode 工具声明。
// 注意：上游工具元素没有 type:"function" 外壳，schema 字段名为 input_schema。
func ccToolsFromChat(v any) []map[string]any {
	out := []map[string]any{}
	for _, t := range ccAsMapSlice(v) {
		name := stringFromAny(t["name"], "")
		desc := stringFromAny(t["description"], "")
		schema := t["parameters"]
		if fn, ok := t["function"].(map[string]any); ok {
			if ttype := strings.ToLower(strings.TrimSpace(stringFromAny(t["type"], "function"))); ttype != "function" {
				continue
			}
			name = stringFromAny(fn["name"], name)
			desc = stringFromAny(fn["description"], desc)
			schema = fn["parameters"]
		}
		name = ccNormalizeToolName(name)
		if name == "" {
			continue
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"name": name, "description": desc, "input_schema": schema})
	}
	return out
}

// ccNormalizeToolName 归一到 CommandCode CLI 的工具名别名表。
func ccNormalizeToolName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash_output", "task_output":
		return "shell_output"
	case "tool_search":
		return "search_tools"
	case "read_multiple_files":
		return "read_file"
	}
	return strings.TrimSpace(name)
}

// ccToolChoice 映射 tool_choice：auto→auto、none→none、required→any、function→tool+name。
func ccToolChoice(v any) map[string]any {
	switch x := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "none":
			return map[string]any{"type": "none"}
		case "required", "any":
			return map[string]any{"type": "any"}
		}
		return map[string]any{"type": "auto"}
	case map[string]any:
		name := stringFromAny(x["name"], "")
		if fn, ok := x["function"].(map[string]any); ok {
			name = stringFromAny(fn["name"], name)
		}
		if name != "" {
			return map[string]any{"type": "tool", "name": ccNormalizeToolName(name)}
		}
	}
	return map[string]any{"type": "auto"}
}

// ccAsMapSlice 宽容地把 []any / []map[string]any 统一成 []map[string]any。
func ccAsMapSlice(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------------------
// 错误与状态码映射
// ---------------------------------------------------------------------------

// ccStatusToHTTP 把上游状态码映射为对客户端的状态码，并给出建议 retry_after 秒数。
func ccStatusToHTTP(upstreamStatus int) (int, int) {
	switch upstreamStatus {
	case 400:
		return http.StatusBadRequest, 0
	case 401:
		return http.StatusUnauthorized, 0
	case 402:
		// 402 = 套餐额度耗尽 / 需要付费，对客户端表现为限流。
		return http.StatusTooManyRequests, 30
	case 403:
		return http.StatusUnauthorized, 0
	case 404:
		return http.StatusNotFound, 0
	case 422:
		return http.StatusBadRequest, 0
	case 429:
		return http.StatusTooManyRequests, 0
	case 500, 502:
		return http.StatusBadGateway, 0
	case 503:
		return http.StatusServiceUnavailable, 0
	default:
		return http.StatusBadGateway, 0
	}
}

// mapCCError 解析上游错误体并给出 (客户端状态码, 错误消息, retry_after 秒数)。
func mapCCError(body []byte, upstreamStatus int) (int, string, int) {
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	msg := ""
	if payload != nil {
		for _, key := range []string{"errorText", "message", "detail", "error"} {
			if s := strings.TrimSpace(textFromAny(payload[key])); s != "" {
				msg = s
				break
			}
		}
	}
	if msg == "" {
		msg = summarizeText(string(body), 500)
	}
	if msg == "" {
		msg = http.StatusText(upstreamStatus)
	}
	status, retryAfter := ccStatusToHTTP(upstreamStatus)
	if retryAfter == 0 && payload != nil {
		for _, key := range []string{"retry_after", "retryAfter"} {
			if n := intFromAny(payload[key]); n > 0 {
				retryAfter = n
				break
			}
		}
	}
	if retryAfter == 0 && upstreamStatus == http.StatusTooManyRequests {
		retryAfter = 10
	}
	return status, msg, retryAfter
}

// ---------------------------------------------------------------------------
// NDJSON 事件聚合
// ---------------------------------------------------------------------------

// ccUsage 汇总上游事件中的用量字段（OpenAI 语义：prompt 不减缓存）。
type ccUsage struct {
	Input      int
	Output     int
	Cached     int
	CacheRead  int
	CacheWrite int
	NoCache    int
	Seen       bool
}

// promptTokens 返回对外的 prompt_tokens：outputTokens==0 时一并清零，防止误计费。
func (u ccUsage) promptTokens() int {
	if u.Output == 0 {
		return 0
	}
	return u.Input
}

// cachedTokens 返回对外的 cached_tokens，同样受 outputTokens==0 清零规则约束。
func (u ccUsage) cachedTokens() int {
	if u.Output == 0 {
		return 0
	}
	if u.Cached > 0 {
		return u.Cached
	}
	return u.CacheRead
}

// ccUsageFromAny 从任意 usage / totalUsage 对象解析用量。
func ccUsageFromAny(v any) ccUsage {
	m, ok := v.(map[string]any)
	if !ok {
		return ccUsage{}
	}
	u := ccUsage{Seen: true}
	u.Input = firstPositiveInt(m["inputTokens"], m["promptTokens"])
	u.Output = firstPositiveInt(m["outputTokens"], m["completionTokens"])
	u.Cached = intFromAny(m["cachedInputTokens"])
	if d, ok := m["inputTokenDetails"].(map[string]any); ok {
		u.NoCache = intFromAny(d["noCacheTokens"])
		u.CacheRead = intFromAny(d["cacheReadTokens"])
		u.CacheWrite = intFromAny(d["cacheWriteTokens"])
	}
	if u.Cached == 0 {
		u.Cached = u.CacheRead
	}
	if u.Input == 0 && (u.NoCache > 0 || u.CacheRead > 0) {
		u.Input = u.NoCache + u.CacheRead
	}
	return u
}

func firstPositiveInt(values ...any) int {
	for _, v := range values {
		if n := intFromAny(v); n > 0 {
			return n
		}
	}
	return 0
}

// mergeCCUsage 合并两次用量快照，仅在有值时覆盖（保留最后一次有效值）。
func mergeCCUsage(base, next ccUsage) ccUsage {
	if !next.Seen {
		return base
	}
	base.Seen = true
	if next.Input > 0 {
		base.Input = next.Input
	}
	if next.Output > 0 {
		base.Output = next.Output
	}
	if next.Cached > 0 {
		base.Cached = next.Cached
	}
	if next.CacheRead > 0 {
		base.CacheRead = next.CacheRead
	}
	if next.CacheWrite > 0 {
		base.CacheWrite = next.CacheWrite
	}
	if next.NoCache > 0 {
		base.NoCache = next.NoCache
	}
	return base
}

// ccToolCallAcc 累积单个工具调用（含流式分片）。
type ccToolCallAcc struct {
	ID     string
	CallID string
	Name   string
	args   strings.Builder
}

func (t *ccToolCallAcc) arguments() string {
	s := strings.TrimSpace(t.args.String())
	if s == "" {
		return "{}"
	}
	return s
}

// ccAggregate 累积一次上游 NDJSON 流的全部语义结果。
type ccAggregate struct {
	textBuf      strings.Builder
	reasonBuf    strings.Builder
	tools        []*ccToolCallAcc
	toolByID     map[string]*ccToolCallAcc
	finishReason string
	truncated    bool
	sawFinish    bool
	usage        ccUsage
	errMsg       string
}

func newCCAggregate() *ccAggregate {
	return &ccAggregate{toolByID: map[string]*ccToolCallAcc{}}
}

func (s *ccAggregate) text() string      { return s.textBuf.String() }
func (s *ccAggregate) reasoning() string { return s.reasonBuf.String() }

func (s *ccAggregate) tool(id, name string) *ccToolCallAcc {
	key := id
	if key == "" {
		key = name
	}
	if key == "" {
		key = fmt.Sprintf("tool-%d", len(s.tools))
	}
	if tc, ok := s.toolByID[key]; ok {
		if name != "" {
			tc.Name = ccNormalizeToolName(name)
		}
		if id != "" {
			tc.ID = id
			tc.CallID = id
		}
		return tc
	}
	tc := &ccToolCallAcc{ID: id, CallID: id, Name: ccNormalizeToolName(name)}
	if tc.ID == "" {
		tc.ID = randomID("fc")
		tc.CallID = tc.ID
	}
	s.toolByID[key] = tc
	s.tools = append(s.tools, tc)
	return tc
}

// observe 消费单个 NDJSON 事件。
func (s *ccAggregate) observe(ev map[string]any) {
	kind := strings.ToLower(strings.TrimSpace(stringFromAny(ev["type"], "")))
	switch kind {
	case "text-delta":
		s.textBuf.WriteString(ccEventText(ev))
	case "text":
		// 部分实现只在 text-end / text 里给完整文本，此处仅在尚未累积时补齐。
		if s.textBuf.Len() == 0 {
			s.textBuf.WriteString(ccEventText(ev))
		}
	case "reasoning-delta":
		s.reasonBuf.WriteString(ccEventText(ev))
	case "tool-input-start":
		s.tool(ccToolCallID(ev), ccEventToolName(ev))
	case "tool-input-delta":
		if tc := s.tool(ccToolCallID(ev), ccEventToolName(ev)); tc != nil {
			tc.args.WriteString(ccEventText(ev))
		}
	case "tool-call":
		tc := s.tool(ccToolCallID(ev), ccEventToolName(ev))
		if in, ok := ev["input"]; ok && in != nil && strings.TrimSpace(tc.args.String()) == "" {
			if b, err := json.Marshal(in); err == nil {
				tc.args.Write(b)
			}
		}
	case "tool-error":
		if s.errMsg == "" {
			s.errMsg = firstNonEmpty(
				strings.TrimSpace(textFromAny(ev["errorText"])),
				strings.TrimSpace(textFromAny(ev["error"])),
				"upstream tool error",
			)
		}
	case "error":
		s.errMsg = ccEventError(ev)
	case "finish":
		s.sawFinish = true
		finishReason := strings.TrimSpace(stringFromAny(ev["finishReason"], ""))
		rawFinishReason := strings.TrimSpace(stringFromAny(ev["rawFinishReason"], ""))
		s.finishReason, s.truncated = ccNormalizeFinishReason(finishReason, rawFinishReason)
	}
	for _, key := range []string{"totalUsage", "usage"} {
		if u, ok := ev[key].(map[string]any); ok {
			s.usage = mergeCCUsage(s.usage, ccUsageFromAny(u))
		}
	}
}

// ccEventText 取事件里的增量文本（上游不同事件族字段名不一致）。
func ccEventText(ev map[string]any) string {
	if s, ok := ev["delta"].(string); ok && s != "" {
		return s
	}
	if s, ok := ev["textDelta"].(string); ok && s != "" {
		return s
	}
	if s, ok := ev["text"].(string); ok {
		return s
	}
	return ""
}

func ccToolCallID(ev map[string]any) string {
	for _, key := range []string{"toolCallId", "toolCallID", "tool_call_id", "id"} {
		if v := stringFromAny(ev[key], ""); v != "" {
			return v
		}
	}
	return ""
}

func ccEventToolName(ev map[string]any) string {
	for _, key := range []string{"toolName", "tool_name", "name"} {
		if v := stringFromAny(ev[key], ""); v != "" {
			return v
		}
	}
	return ""
}

func ccEventError(ev map[string]any) string {
	for _, key := range []string{"errorText", "message", "error"} {
		if s := strings.TrimSpace(textFromAny(ev[key])); s != "" {
			return s
		}
	}
	b, _ := json.Marshal(ev)
	return summarizeText(string(b), 300)
}

// ccNormalizeFinishReason 把上游结束原因归一成对外语义，并识别未声明原因的截断。
//
// 铁律：有效原因取 rawFinishReason ?? finishReason；若 finishReason=="other" 且无 raw，
// 视为截断，必须对外报 502 upstream_error（附带 retry_after）。
func ccNormalizeFinishReason(finishReason, rawFinishReason string) (string, bool) {
	raw := strings.TrimSpace(rawFinishReason)
	finish := strings.TrimSpace(finishReason)
	if raw == "" && strings.EqualFold(finish, "other") {
		return "upstream_error", true
	}
	eff := raw
	if eff == "" {
		eff = finish
	}
	switch strings.ToLower(eff) {
	case "tool_use", "tool-calls", "tool_calls", "tool-call", "toolcalls":
		return "tool_calls", false
	case "length", "max_tokens", "max_output_tokens", "model_context_window_exceeded", "pause_turn":
		return "length", false
	case "network", "connection", "upstream_error", "upstream-error", "upstream error":
		return "upstream_error", false
	case "stop", "end_turn", "stop_sequence", "complete", "completed", "finished":
		return "stop", false
	case "":
		return "stop", false
	default:
		return strings.ToLower(eff), false
	}
}

// ---------------------------------------------------------------------------
// 对外响应体构造（非流式）
// ---------------------------------------------------------------------------

// ccChatCompletionBody 把聚合结果构造为 OpenAI chat.completion 响应。
func ccChatCompletionBody(agg *ccAggregate, id, model string, createdAt int64, pt, ct, tt, cached int) map[string]any {
	message := map[string]any{"role": "assistant", "content": agg.text()}
	if r := agg.reasoning(); r != "" {
		message["reasoning_content"] = r
	}
	if len(agg.tools) > 0 {
		calls := make([]map[string]any, 0, len(agg.tools))
		for i, tc := range agg.tools {
			calls = append(calls, map[string]any{
				"index": i,
				"id":    tc.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.arguments(),
				},
			})
		}
		message["tool_calls"] = calls
	}
	finishReason := agg.finishReason
	if finishReason == "" {
		finishReason = "stop"
	}
	if finishReason == "upstream_error" {
		finishReason = "stop"
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": createdAt,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": ccUsageBody(pt, ct, tt, cached),
	}
}

// ccResponsesBody 把聚合结果构造为 OpenAI responses 响应。
func ccResponsesBody(agg *ccAggregate, id, model string, createdAt int64, pt, ct, tt, cached int) map[string]any {
	text := agg.text()
	response := responsesSkeleton(id, model, createdAt)
	response["status"] = "completed"
	response["output_text"] = text
	output := []map[string]any{}
	if text != "" {
		output = append(output, responseMessageItem(randomID("msg"), text))
	}
	for _, tc := range agg.tools {
		output = append(output, responseFunctionCallItem(tc.ID, tc.CallID, tc.Name, tc.arguments(), "completed"))
	}
	response["output"] = output
	response["usage"] = ccResponsesUsageBody(pt, ct, tt, cached)
	return response
}

func ccUsageBody(pt, ct, tt, cached int) map[string]any {
	usage := map[string]any{
		"prompt_tokens":     pt,
		"completion_tokens": ct,
		"total_tokens":      tt,
	}
	if cached > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return usage
}

func ccResponsesUsageBody(pt, ct, tt, cached int) map[string]any {
	usage := map[string]any{
		"input_tokens":  pt,
		"output_tokens": ct,
		"total_tokens":  tt,
	}
	if cached > 0 {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return usage
}

// ccEstimatePromptTokens 在上游未返回 usage 时按客户端请求体估算 prompt token。
func ccEstimatePromptTokens(body map[string]any, clientProtocol string) int {
	if clientProtocol == protocolResponses {
		return estimateResponsesPromptTokens(body)
	}
	if body == nil {
		return 0
	}
	return estimateTokensFromAny(body["messages"])
}

// ccResolveUsage 统一「上游真实用量优先、缺失才估算」的取值语义。
func ccResolveUsage(agg *ccAggregate, body map[string]any, clientProtocol string) (pt, ct, tt, cached int, source string) {
	pt = agg.usage.promptTokens()
	ct = agg.usage.Output
	cached = agg.usage.cachedTokens()
	source = "upstream"
	if pt == 0 && ct == 0 {
		pt = ccEstimatePromptTokens(body, clientProtocol)
		ct = estimateTokensFromText(agg.text())
		cached = 0
		source = "estimated"
	}
	tt = pt + ct
	return pt, ct, tt, cached, source
}

// ---------------------------------------------------------------------------
// 流式事件发射
// ---------------------------------------------------------------------------

// ccStreamEmitter 把上游 NDJSON 事件转写成对客 SSE。
type ccStreamEmitter struct {
	w         http.ResponseWriter
	protocol  string
	id        string
	model     string
	createdAt int64
	itemID    string
	indexByID map[string]int
	toolDone  map[string]bool
}

func newCCStreamEmitter(w http.ResponseWriter, protocol, id, model string, createdAt int64) *ccStreamEmitter {
	return &ccStreamEmitter{
		w:         w,
		protocol:  protocol,
		id:        id,
		model:     model,
		createdAt: createdAt,
		itemID:    randomID("msg"),
		indexByID: map[string]int{},
		toolDone:  map[string]bool{},
	}
}

// start 发送客户端流首帧（chat: role 首块；responses: created/in_progress/item.added）。
func (e *ccStreamEmitter) start() {
	if e.protocol == protocolResponses {
		response := responsesSkeleton(e.id, e.model, e.createdAt)
		response["status"] = "in_progress"
		writeSSEEvent(e.w, "response.created", map[string]any{"type": "response.created", "response": response})
		writeSSEEvent(e.w, "response.in_progress", map[string]any{"type": "response.in_progress", "response": response})
		item := responseMessageItem(e.itemID, "")
		item["status"] = "in_progress"
		writeSSEEvent(e.w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		writeSSEEvent(e.w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": e.itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
		return
	}
	writeChatSSEChunk(e.w, e.id, e.model, e.createdAt, map[string]any{"role": "assistant", "content": ""}, "")
}

// emit 转写单个上游事件（仅处理对客有意义的事件族）。
func (e *ccStreamEmitter) emit(ev map[string]any, agg *ccAggregate) {
	kind := strings.ToLower(strings.TrimSpace(stringFromAny(ev["type"], "")))
	switch kind {
	case "text-delta":
		delta := ccEventText(ev)
		if delta == "" {
			return
		}
		if e.protocol == protocolResponses {
			writeSSEEvent(e.w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": e.itemID, "output_index": 0, "content_index": 0, "delta": delta})
			return
		}
		writeChatSSEChunk(e.w, e.id, e.model, e.createdAt, map[string]any{"content": delta}, "")
	case "reasoning-delta":
		if e.protocol == protocolResponses {
			return
		}
		if delta := ccEventText(ev); delta != "" {
			writeChatSSEChunk(e.w, e.id, e.model, e.createdAt, map[string]any{"reasoning_content": delta}, "")
		}
	case "tool-call", "tool-input-start", "tool-input-delta":
		e.emitTool(ev, agg, kind)
	}
}

// emitTool 转写工具调用事件。chat 用 tool_calls 增量；responses 用 output_item + arguments 增量。
func (e *ccStreamEmitter) emitTool(ev map[string]any, agg *ccAggregate, kind string) {
	id := ccToolCallID(ev)
	if id == "" {
		id = ccEventToolName(ev)
	}
	if id == "" {
		return
	}
	index, ok := e.indexByID[id]
	if !ok {
		index = len(e.indexByID)
		e.indexByID[id] = index
	}
	tc := agg.tool(ccToolCallID(ev), ccEventToolName(ev))
	if tc == nil {
		return
	}
	if kind == "tool-call" {
		// 完整工具调用：一次性下发 id/name/arguments（chat）或 added + done（responses）。
		if e.protocol == protocolResponses {
			e.emitResponsesToolItem(tc, index, "", "in_progress")
			e.emitResponsesToolItem(tc, index, tc.arguments(), "completed")
			e.toolDone[id] = true
			return
		}
		writeChatSSEChunk(e.w, e.id, e.model, e.createdAt, map[string]any{
			"tool_calls": []map[string]any{{
				"index":    index,
				"id":       tc.ID,
				"type":     "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.arguments()},
			}},
		}, "")
		return
	}
	if kind == "tool-input-start" {
		if e.protocol == protocolResponses {
			e.emitResponsesToolItem(tc, index, "", "in_progress")
			return
		}
		writeChatSSEChunk(e.w, e.id, e.model, e.createdAt, map[string]any{
			"tool_calls": []map[string]any{{
				"index":    index,
				"id":       tc.ID,
				"type":     "function",
				"function": map[string]any{"name": tc.Name, "arguments": ""},
			}},
		}, "")
		return
	}
	delta := ccEventText(ev)
	if delta == "" {
		return
	}
	if e.protocol == protocolResponses {
		writeSSEEvent(e.w, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": tc.ID, "output_index": 1 + index, "delta": delta})
		return
	}
	writeChatSSEChunk(e.w, e.id, e.model, e.createdAt, map[string]any{
		"tool_calls": []map[string]any{{
			"index":    index,
			"function": map[string]any{"arguments": delta},
		}},
	}, "")
}

// emitResponsesToolItem 发送 responses 的 output_item 事件（added 或 done）。
func (e *ccStreamEmitter) emitResponsesToolItem(tc *ccToolCallAcc, index int, arguments, status string) {
	outputIndex := 1 + index
	item := responseFunctionCallItem(tc.ID, tc.CallID, tc.Name, arguments, status)
	event := "response.output_item.added"
	if status == "completed" {
		event = "response.output_item.done"
	}
	writeSSEEvent(e.w, event, map[string]any{"type": event, "output_index": outputIndex, "item": item})
}

// finish 发送流式收尾帧并补上上游缺失的 [DONE]。
func (e *ccStreamEmitter) finish(agg *ccAggregate, pt, ct, tt, cached int) {
	if e.protocol == protocolResponses {
		text := agg.text()
		writeSSEEvent(e.w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": e.itemID, "output_index": 0, "content_index": 0, "text": text})
		writeSSEEvent(e.w, "response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": e.itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text}})
		writeSSEEvent(e.w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": responseMessageItem(e.itemID, text)})
		output := []map[string]any{}
		if text != "" {
			output = append(output, responseMessageItem(e.itemID, text))
		}
		for i, tc := range agg.tools {
			if !e.toolDone[tc.ID] {
				writeSSEEvent(e.w, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": tc.ID, "output_index": 1 + i, "name": tc.Name, "arguments": tc.arguments()})
				e.emitResponsesToolItem(tc, i, tc.arguments(), "completed")
			}
			output = append(output, responseFunctionCallItem(tc.ID, tc.CallID, tc.Name, tc.arguments(), "completed"))
		}
		response := responsesSkeleton(e.id, e.model, e.createdAt)
		response["status"] = "completed"
		response["output_text"] = text
		response["output"] = output
		response["usage"] = ccResponsesUsageBody(pt, ct, tt, cached)
		writeSSEEvent(e.w, "response.completed", map[string]any{"type": "response.completed", "response": response})
		_, _ = fmt.Fprint(e.w, "data: [DONE]\n\n")
		if flusher, ok := e.w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	finishReason := agg.finishReason
	if finishReason == "" || finishReason == "upstream_error" {
		finishReason = "stop"
	}
	writeChatSSEChunk(e.w, e.id, e.model, e.createdAt, map[string]any{}, finishReason)
	_, _ = fmt.Fprint(e.w, "data: [DONE]\n\n")
	if flusher, ok := e.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// ccHeadersForStream 设置对客 SSE 响应头。
func ccHeadersForStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

// ---------------------------------------------------------------------------
// 主转发流程
// ---------------------------------------------------------------------------

// ccForward 是 CommandCode 上游的唯一转发入口，clientProtocol 表示客户端使用的协议
// （protocolChatCompletions / protocolResponses），上游协议恒为 NDJSON 流。
func (a *App) ccForward(w http.ResponseWriter, r *http.Request, source, localKeyID, localModel, upstreamModel string, provider ProviderKey, body map[string]any, clientProtocol string) forwardResult {
	start := time.Now()
	reqID := randomID("req")
	wantStream, _ := body["stream"].(bool)
	fctx := forwardCtx{
		Source: source, Provider: provider, LocalKeyID: localKeyID,
		LocalModel: localModel, UpstreamModel: upstreamModel,
		Stream: wantStream, Req: r,
	}

	var (
		envelope map[string]any
		err      error
	)
	if clientProtocol == protocolResponses {
		envelope, err = ccBodyFromResponses(body, upstreamModel)
	} else {
		envelope, err = ccBodyFromChat(body, upstreamModel)
	}
	if err != nil {
		// 请求形态不被上游支持（response_format / previous_response_id 等）→ 400，且必须落库。
		return a.failForward(w, fctx, http.StatusBadRequest, err.Error())
	}
	payload, _ := json.Marshal(envelope)
	fctx.Payload = payload

	// 上游无计流参数，恒以 stream=true 请求，非流式由本层聚合 NDJSON 得出。
	resp, url, upErr := a.doUpstream(r.Context(), provider, ccUpstreamPath, payload, true, ccUpstreamHeaders(r.Header, provider))
	if upErr != nil {
		return a.failForward(w, fctx.withURL(upErr.URL), upErr.StatusCode, upErr.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := readUpstreamBody(resp)
		if readErr != nil {
			return a.failForward(w, fctx.withURL(url), readErr.StatusCode, readErr.Error())
		}
		statusCode, msg, retryAfter := mapCCError(respBody, resp.StatusCode)
		res := forwardResult{
			RequestID:       reqID,
			StatusCode:      statusCode,
			LatencyMS:       time.Since(start).Milliseconds(),
			Success:         false,
			ErrorMessage:    msg,
			ResponseSummary: summarizeText(string(respBody), 800),
			UsageSource:     "missing",
		}
		a.finish(fctx.withURL(url), res, respBody)
		if retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		}
		writeError(w, statusCode, msg)
		return res
	}

	agg := newCCAggregate()
	var emitter *ccStreamEmitter
	if wantStream {
		ccHeadersForStream(w)
		forwardStreamDiagnosticHeaders(w, resp)
		w.WriteHeader(http.StatusOK)
		emitter = newCCStreamEmitter(w, clientProtocol, randomID("chatcmpl"), localModel, start.Unix())
		emitter.start()
	}

	var scanErr error
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, readErr := readStreamLine(reader)
		if trimmed := strings.TrimSpace(line); trimmed != "" && trimmed != "[DONE]" && !strings.HasPrefix(trimmed, ":") {
			var ev map[string]any
			if json.Unmarshal([]byte(trimmed), &ev) == nil {
				agg.observe(ev)
				if emitter != nil {
					emitter.emit(ev, agg)
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				scanErr = readErr
			}
			break
		}
	}

	pt, ct, tt, cached, usageSource := ccResolveUsage(agg, body, clientProtocol)
	errMsg := ""
	success := true
	switch {
	case scanErr != nil:
		success = false
		errMsg = scanErr.Error()
	case agg.truncated:
		// 铁律：finishReason=="other" 且无 rawFinishReason → 上游截断，必须报 502 upstream_error。
		success = false
		errMsg = firstNonEmpty(agg.errMsg, "commandcode upstream truncated the response without a finish reason")
	case agg.finishReason == "upstream_error":
		success = false
		errMsg = firstNonEmpty(agg.errMsg, "commandcode upstream reported a network/upstream error")
	case agg.errMsg != "":
		success = false
		errMsg = agg.errMsg
	case !agg.sawFinish && agg.text() == "" && len(agg.tools) == 0:
		success = false
		errMsg = "commandcode upstream closed the stream without events"
	}

	if emitter != nil {
		if !success {
			writeSSEErrorEvent(w, errMsg)
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			res := forwardResult{
				RequestID:        reqID,
				StatusCode:       http.StatusBadGateway,
				LatencyMS:        time.Since(start).Milliseconds(),
				Success:          false,
				ErrorMessage:     errMsg,
				ResponseSummary:  summarizeText(agg.text(), 800),
				PromptTokens:     pt,
				CompletionTokens: ct,
				TotalTokens:      tt,
				CachedTokens:     cached,
				UsageSource:      usageSource,
			}
			return a.finishStream(r, resp, body, res, source, provider, localKeyID, localModel, upstreamModel, []byte(agg.text()))
		}
		emitter.finish(agg, pt, ct, tt, cached)
		res := forwardResult{
			RequestID:        reqID,
			StatusCode:       http.StatusOK,
			LatencyMS:        time.Since(start).Milliseconds(),
			Success:          true,
			ResponseSummary:  summarizeText(agg.text(), 800),
			PromptTokens:     pt,
			CompletionTokens: ct,
			TotalTokens:      tt,
			CachedTokens:     cached,
			UsageSource:      usageSource,
		}
		finalBody := ccStreamResponseSummary(agg, clientProtocol, localModel, pt, ct, tt, cached)
		return a.finishStream(r, resp, body, res, source, provider, localKeyID, localModel, upstreamModel, finalBody)
	}

	if !success {
		statusCode := http.StatusBadGateway
		if agg.truncated {
			w.Header().Set("Retry-After", "10")
		}
		res := forwardResult{
			RequestID:        reqID,
			StatusCode:       statusCode,
			LatencyMS:        time.Since(start).Milliseconds(),
			Success:          false,
			ErrorMessage:     errMsg,
			ResponseSummary:  summarizeText(agg.text(), 800),
			PromptTokens:     pt,
			CompletionTokens: ct,
			TotalTokens:      tt,
			CachedTokens:     cached,
			UsageSource:      usageSource,
		}
		a.finish(fctx.withURL(url), res, []byte(agg.text()))
		writeError(w, statusCode, errMsg)
		return res
	}

	var out map[string]any
	if clientProtocol == protocolResponses {
		out = ccResponsesBody(agg, randomID("resp"), localModel, start.Unix(), pt, ct, tt, cached)
	} else {
		out = ccChatCompletionBody(agg, randomID("chatcmpl"), localModel, start.Unix(), pt, ct, tt, cached)
	}
	responseBody, _ := json.Marshal(out)
	res := forwardResult{
		RequestID:        reqID,
		StatusCode:       http.StatusOK,
		LatencyMS:        time.Since(start).Milliseconds(),
		Success:          true,
		ResponseSummary:  summarizeText(string(responseBody), 800),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		CachedTokens:     cached,
		UsageSource:      usageSource,
	}
	a.finish(fctx.withURL(url), res, responseBody)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBody)
	return res
}

// ccStreamResponseSummary 生成流式路径落库/调试日志用的响应摘要体。
func ccStreamResponseSummary(agg *ccAggregate, clientProtocol, model string, pt, ct, tt, cached int) []byte {
	if clientProtocol == protocolResponses {
		b, _ := json.Marshal(ccResponsesBody(agg, randomID("resp"), model, time.Now().Unix(), pt, ct, tt, cached))
		return b
	}
	b, _ := json.Marshal(ccChatCompletionBody(agg, randomID("chatcmpl"), model, time.Now().Unix(), pt, ct, tt, cached))
	return b
}
