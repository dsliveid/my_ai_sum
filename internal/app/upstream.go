package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// upstreamForwardHeaders 是允许从入站请求透传到上游的请求头白名单。
//
// 之所以必须透传：上游若为多副本集群，会话粘性（同一对话固定落到同一副本）依赖
// 客户端声明的会话标识或缓存键。此前网关只设置 Authorization，把客户端带来的
// 会话标识全部丢弃，导致上游即使支持粘性路由也收不到信号，KV Cache 无法命中。
//
// 之所以只做白名单而非全量透传：Host / Content-Length / Connection 等由网关接管，
// Authorization 必须换成 provider 密钥，Cookie 等属敏感信息，全量透传会引入新隐患。
var upstreamForwardHeaders = []string{
	"X-Session-Id",
	"X-Opencode-Session",
	"X-Request-Id",
	"Anthropic-Beta",
	"Openai-Beta",
	"Prompt-Cache-Key",
	"X-Stainless-Lang",
	"X-Stainless-Package-Version",
	"X-Stainless-Os",
	"X-Stainless-Arch",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Timeout",
	"Idempotency-Key",
	// CommandCode（仅 Go 套餐）协议要求的私有头。这些键只会由 commandcode.go 主动设置，
	// 其它客户端的同名头出现概率为零，因此并入统一白名单不会污染既有协议；
	// 之所以放进白名单而非新增 doUpstream 参数：本文件函数签名必须保持不变。
	"X-Command-Code-Version",
	"X-Cli-Environment",
	"X-Project-Slug",
	"X-Taste-Learning",
	"Traceparent",
	"X-Cmd-Zdr",
}

// forwardInboundHeaders 按白名单从入站请求中挑选需要透传给上游的头。
func forwardInboundHeaders(in http.Header) http.Header {
	if in == nil {
		return nil
	}
	out := http.Header{}
	for _, key := range upstreamForwardHeaders {
		if v := in.Get(key); v != "" {
			out.Set(key, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// upstreamError 携带上游调用的失败信息与语义化状态码，供调用方统一落库。
// 区分 500（本地构造失败）与 502（上游交互失败）与既有行为保持一致。
type upstreamError struct {
	StatusCode int
	URL        string
	Err        error
}

func (e *upstreamError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// doUpstream 统一完成「取密钥 → 构造请求 → 注入鉴权头 → 透传白名单请求头 → 执行」。
//
// 这是五条转发路径此前各自复制一份的逻辑（约 25 行 × 5 处）。收敛到一处后，
// 新增上游协议只需提供 path 与 payload，无需再复制网络层代码。
// 返回 response 与最终 URL；出错时返回 *upstreamError，调用方负责落库与回写。
// 返回值 resp 在非 nil 时由调用方负责关闭 Body。
func (a *App) doUpstream(ctx context.Context, provider ProviderKey, path string, payload []byte, stream bool, inHeaders http.Header) (*http.Response, string, *upstreamError) {
	apiKey, err := a.providerBearerToken(ctx, provider)
	if err != nil {
		return nil, "", &upstreamError{StatusCode: 500, Err: err}
	}
	client, err := a.httpClientForProvider(provider)
	if err != nil {
		return nil, "", &upstreamError{StatusCode: 500, Err: err}
	}
	url := normalizeBaseURL(provider.BaseURL) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, url, &upstreamError{StatusCode: 500, URL: url, Err: err}
	}
	a.applyProviderAuthHeaders(req, provider, apiKey)
	req.Header.Set("Content-Type", "application/json")
	// CommandCode 走 CLI 客户端身份校验，User-Agent 不能是 Go 默认值。
	// 只在该协议下覆盖，避免污染其它上游依赖的默认 UA 行为。
	if normalizeProtocolName(provider.RequestProtocol) == ProtocolCommandCode {
		req.Header.Set("User-Agent", ccUpstreamUserAgent)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for key, values := range forwardInboundHeaders(inHeaders) {
		for _, v := range values {
			req.Header.Set(key, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, url, &upstreamError{StatusCode: 502, URL: url, Err: err}
	}
	return resp, url, nil
}

// marshalJSONString 序列化单个字符串，且不启用 HTML 转义。
// 之所以不用 json.Marshal：它会把 < > & 改写成 \u003c 等转义序列，
// 而本函数的全部价值就在于「除了 model 值之外一个字节都不要变」。
func marshalJSONString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// replaceTopLevelJSONStringField 在 raw 顶层对象中定位 key 并只替换它的字符串值，
// 其余字节（键序、数字字面量、转义形式）原样保留。
//
// 为什么需要它：上游若为多副本推理集群，KV Cache 命中依赖请求字节前缀完全一致。
// 把 body 反序列化成 map[string]any 再 json.Marshal 会按字典序重排键、把 1.0 写成 1、
// 把 < 写成 \u003c，前缀随即错位，缓存无法命中。定点 splice 是唯一能同时做到
// 「改写 model」与「保持其余字节不动」的方式。
//
// 返回 ok=false 表示无法安全改写（非 JSON 对象、缺少该 key、key 重复、值区间异常等），
// 调用方必须回退到 json.Marshal 全量序列化——绝不返回半成品或损坏的 JSON。
func replaceTopLevelJSONStringField(raw []byte, key string, newValue string) ([]byte, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return raw, false
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return raw, false
	}
	found := false
	valueStart, valueEnd := 0, 0
	for {
		tok, err = dec.Token()
		if err != nil {
			return raw, false
		}
		if delim, ok := tok.(json.Delim); ok {
			// 顶层对象正常结束；其它分隔符说明结构不符合预期。
			if delim != '}' {
				return raw, false
			}
			break
		}
		field, ok := tok.(string)
		if !ok {
			return raw, false
		}
		// InputOffset 此时停在 key 的闭合引号之后（尚未越过冒号），
		// 因此先把整段「冒号 + 空白 + 值 + 空白」解出来，再自行收缩到值本体。
		segStart := int(dec.InputOffset())
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return raw, false
		}
		segEnd := int(dec.InputOffset())
		if segStart < 0 || segEnd > len(raw) || segStart >= segEnd {
			return raw, false
		}
		// Decode 只吃掉了值本身，key 与值之间的冒号仍留在 raw 中；
		// 因此先定位分隔冒号（key 字符串已结束，中间只可能有空白），值本体从其后开始。
		seg := raw[segStart:segEnd]
		colon := -1
		for i := 0; i < len(seg); i++ {
			if seg[i] == ':' {
				colon = i
				break
			}
			if !isJSONSpace(seg[i]) {
				return raw, false
			}
		}
		if colon < 0 {
			return raw, false
		}
		if field != key {
			continue
		}
		// 重复 key 时显式改写语义不确定（保留哪一个由解析器决定），一律回退。
		if found {
			return raw, false
		}
		lead := colon + 1
		for lead < len(seg) && isJSONSpace(seg[lead]) {
			lead++
		}
		tail := len(seg)
		for tail > lead && isJSONSpace(seg[tail-1]) {
			tail--
		}
		if lead >= tail {
			return raw, false
		}
		found = true
		valueStart = segStart + lead
		valueEnd = segStart + tail
	}
	if !found {
		return raw, false
	}
	var current string
	if err := json.Unmarshal(raw[valueStart:valueEnd], &current); err == nil && current == newValue {
		// 值本来就一致：原样返回，连一个字节都不重新生成。
		return raw, true
	}
	encoded := marshalJSONString(newValue)
	out := make([]byte, 0, len(raw)-(valueEnd-valueStart)+len(encoded))
	out = append(out, raw[:valueStart]...)
	out = append(out, encoded...)
	out = append(out, raw[valueEnd:]...)
	return out, true
}

// buildDirectPayload 构造直通路径（客户端协议 == 上游协议）发给上游的请求体。
//
// 优先基于客户端原始字节做 model 定点替换，让上游收到的字节前缀与客户端原始请求一致，
// 从而命中多副本集群的 KV Cache；只有在没有原始字节（如后台测试聊天）或定点替换
// 不安全时才回退到 json.Marshal 全量序列化，保证功能正确性优先于字节保真。
func buildDirectPayload(raw []byte, body map[string]any, upstreamModel string) []byte {
	if len(raw) > 0 {
		if out, ok := replaceTopLevelJSONStringField(raw, "model", upstreamModel); ok {
			return out
		}
	}
	payload, _ := json.Marshal(body)
	return payload
}

// readUpstreamBody 读取上游响应体，失败时返回语义化的 upstreamError。
// 独立成函数是为了让「读取失败也要落库」这条纪律在所有路径上一致执行
// （此处曾是统计缺口：失败分支只回 500 而不写 request_logs / usage_records）。
func readUpstreamBody(resp *http.Response) ([]byte, *upstreamError) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &upstreamError{StatusCode: 502, Err: err}
	}
	return body, nil
}

// forwardCtx 聚合一次转发的固定上下文，避免 saveRequestAndUsage / logForwardDebug
// 那 10 余个裸字符串参数在每条路径上重复书写，也避免漏传导致统计错位。
type forwardCtx struct {
	Source        string
	Provider      ProviderKey
	LocalKeyID    string
	LocalModel    string
	UpstreamModel string
	Stream        bool
	Req           *http.Request
	URL           string
	Payload       []byte
}

// withURL 返回带上游地址的副本（便于错误分支按实际进度填写 URL）。
func (c forwardCtx) withURL(url string) forwardCtx {
	c.URL = url
	return c
}

// withStream 返回带实际流式标记的副本。
func (c forwardCtx) withStream(stream bool) forwardCtx {
	c.Stream = stream
	return c
}

// finish 是全部转发路径的唯一收口：写入用量统计与调试日志，并返回结果。
// 任何分支（含全部 error 分支）都必须经此函数，杜绝漏记统计。
func (a *App) finish(c forwardCtx, res forwardResult, responseBody []byte) forwardResult {
	// 统计落库是旁路观测：失败只丢统计数据，绝不能改变已经/即将写给客户端的响应。
	// 因此这里显式忽略 error（内部已事务化，失败即整体回滚，不会留下半截数据）。
	_ = a.saveRequestAndUsage(res, c.Source, c.Provider, c.LocalKeyID, c.LocalModel, c.UpstreamModel, c.Stream, c.Req)
	a.logForwardDebug(c.Req, res, c.Source, c.URL, c.Provider, c.LocalKeyID, c.LocalModel, c.UpstreamModel, c.Stream, c.Payload, responseBody)
	return res
}

// failForward 构造失败结果、落库并回写错误响应，返回结果供调用方直接 return。
// 把「构造 res → 落库 → writeError」三步固定下来，任何新增分支照此调用即不会漏统计。
func (a *App) failForward(w http.ResponseWriter, c forwardCtx, statusCode int, errMsg string) forwardResult {
	res := forwardResult{
		RequestID:    randomID("req"),
		StatusCode:   statusCode,
		Success:      false,
		ErrorMessage: errMsg,
	}
	a.finish(c, res, nil)
	writeError(w, statusCode, errMsg)
	return res
}

// finishStream 是流式转发路径的统一收口。
//
// 流式路径此前每条都手写「saveRequestAndUsage(..., true, r) + logForwardDebug(..., true, requestBody, ...)」
// 两行，且需自行 json.Marshal 请求体。此处把固定语义（stream 恒 true、上游 URL 取自响应对象、
// 请求体由 body 重新序列化）固定下来，调用方只需给出结果与响应内容。
func (a *App) finishStream(r *http.Request, resp *http.Response, body map[string]any, res forwardResult, source string, provider ProviderKey, localKeyID, localModel, upstreamModel string, responseBody []byte) forwardResult {
	payload, _ := json.Marshal(body)
	c := forwardCtx{
		Source: source, Provider: provider, LocalKeyID: localKeyID,
		LocalModel: localModel, UpstreamModel: upstreamModel,
		Stream: true, Req: r, Payload: payload,
		URL: upstreamURLFromResponse(resp),
	}
	return a.finish(c, res, responseBody)
}

// resolveUsage 统一「真实 usage 优先、缺失才估算」的取值语义，并补充缓存命中数。
//
// 估算函数以闭包传入，确保只在真正缺失 usage 时才执行估算，
// 避免为每条成功响应都做一次无用的 token 估算。
func resolveUsage(respBody []byte, statusCode int, promptEstimate func() int, completionText func() string) (pt, ct, tt, cached, creation int, source string) {
	pt, ct, tt, source = extractUsage(respBody)
	if source == "missing" && statusCode < 400 {
		pt = promptEstimate()
		ct = estimateTokensFromText(completionText())
		tt = pt + ct
		source = "estimated"
	}
	cached, creation = parseCacheUsage(respBody)
	return pt, ct, tt, cached, creation, source
}
