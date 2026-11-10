package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestReplaceTopLevelJSONStringFieldPreservesKeyOrder 验证键序保持：
// 这是本改动存在的根本原因，一旦排序就会破坏上游 KV Cache 的前缀对齐。
func TestReplaceTopLevelJSONStringFieldPreservesKeyOrder(t *testing.T) {
	raw := []byte(`{"zeta":1,"model":"old-model","alpha":2}`)
	out, ok := replaceTopLevelJSONStringField(raw, "model", "new-model")
	if !ok {
		t.Fatalf("期望替换成功，实际 ok=false")
	}
	got := string(out)
	want := `{"zeta":1,"model":"new-model","alpha":2}`
	if got != want {
		t.Fatalf("字节结果不符\n got=%s\nwant=%s", got, want)
	}
	// 用索引位置断言键序，避免反序列化把顺序抹平导致误判。
	iZeta := strings.Index(got, `"zeta"`)
	iModel := strings.Index(got, `"model"`)
	iAlpha := strings.Index(got, `"alpha"`)
	if iZeta < 0 || iModel < 0 || iAlpha < 0 {
		t.Fatalf("缺少期望的键: %s", got)
	}
	if !(iZeta < iModel && iModel < iAlpha) {
		t.Fatalf("键序未保持，位置 zeta=%d model=%d alpha=%d，原文=%s", iZeta, iModel, iAlpha, got)
	}
}

// TestReplaceTopLevelJSONStringFieldSameValueUntouched 验证值一致时零改写：
// 必须返回完全相同的字节（连重新编码都不做），否则前缀仍会漂移。
func TestReplaceTopLevelJSONStringFieldSameValueUntouched(t *testing.T) {
	raw := []byte(`{"model":"same","n":1.0}`)
	out, ok := replaceTopLevelJSONStringField(raw, "model", "same")
	if !ok {
		t.Fatalf("期望 ok=true")
	}
	if string(out) != string(raw) {
		t.Fatalf("期望字节完全相同\n got=%s\nwant=%s", out, raw)
	}
}

// TestReplaceTopLevelJSONStringFieldKeepsNumberLiterals 验证数字字面量逐字符保真：
// 1.0 / 1e10 / 超大整数经 float64 中转会被改写，这是 json.Marshal 的隐性破坏。
func TestReplaceTopLevelJSONStringFieldKeepsNumberLiterals(t *testing.T) {
	raw := []byte(`{"model":"m","a":1.0,"b":1e10,"c":12345678901234567890}`)
	out, ok := replaceTopLevelJSONStringField(raw, "model", "m2")
	if !ok {
		t.Fatalf("期望 ok=true")
	}
	got := string(out)
	for _, literal := range []string{"1.0", "1e10", "12345678901234567890"} {
		if !strings.Contains(got, literal) {
			t.Fatalf("数字字面量 %q 被改写: %s", literal, got)
		}
	}
}

// TestReplaceTopLevelJSONStringFieldKeepsHTMLLiterals 验证 HTML 相关字符不被转义：
// json.Marshal 默认把 < > & 写成 \u003c 等，会让前缀字节完全不同。
func TestReplaceTopLevelJSONStringFieldKeepsHTMLLiterals(t *testing.T) {
	raw := []byte(`{"model":"m","p":"<tag>","q":"a&b"}`)
	out, ok := replaceTopLevelJSONStringField(raw, "model", "m2")
	if !ok {
		t.Fatalf("期望 ok=true")
	}
	got := string(out)
	if !strings.Contains(got, `"<tag>"`) || !strings.Contains(got, `"a&b"`) {
		t.Fatalf("HTML 字符被转义: %s", got)
	}
	if strings.Contains(got, `\u003c`) || strings.Contains(got, `\u0026`) {
		t.Fatalf("出现不该有的转义序列: %s", got)
	}
}

// TestReplaceTopLevelJSONStringFieldFallback 验证异常一律回退（ok=false）：
// 宁可走 json.Marshal 全量序列化，也绝不返回损坏的 JSON。
func TestReplaceTopLevelJSONStringFieldFallback(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		key  string
	}{
		{"空字节", nil, "model"},
		{"非 JSON", []byte("not json at all"), "model"},
		{"JSON 数组", []byte(`[{"model":"m"}]`), "model"},
		{"缺少 model 键", []byte(`{"a":1}`), "model"},
		{"重复 model 键", []byte(`{"model":"a","model":"b"}`), "model"},
	}
	for _, c := range cases {
		out, ok := replaceTopLevelJSONStringField(c.raw, c.key, "new")
		if ok {
			t.Fatalf("%s: 期望 ok=false，实际 ok=true，输出=%s", c.name, out)
		}
		if string(out) != string(c.raw) {
			t.Fatalf("%s: 回退时应原样返回 raw\n got=%s\nwant=%s", c.name, out, c.raw)
		}
	}
}

// TestBuildDirectPayloadDiffersFromMarshal 是本次改动的「真实性」金丝雀：
// 键序非字典序的请求体，buildDirectPayload 必须与 json.Marshal 输出不同，
// 且保留原键序；否则说明改写是空操作、缓存前缀依旧错位。
func TestBuildDirectPayloadDiffersFromMarshal(t *testing.T) {
	raw := []byte(`{"zeta":"z","model":"local","alpha":"a"}`)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	body["model"] = "upstream"

	got := string(buildDirectPayload(raw, body, "upstream"))
	marshaled, _ := json.Marshal(body)
	wantMarshal := string(marshaled)

	if got == wantMarshal {
		t.Fatalf("buildDirectPayload 输出与 json.Marshal 相同，改动未生效: %s", got)
	}
	if got != `{"zeta":"z","model":"upstream","alpha":"a"}` {
		t.Fatalf("未保留原键序或 model 未替换: %s", got)
	}
	// json.Marshal 走字典序，alpha 一定在最前——用于证明两条路径确实不同。
	if strings.Index(wantMarshal, `"alpha"`) != 1 {
		t.Fatalf("预期 json.Marshal 输出为字典序，实际=%s", wantMarshal)
	}
}

// TestBuildDirectPayloadFallbackWithoutRaw 验证没有原始字节时回退到 json.Marshal，
// 保证后台测试聊天等调用点行为不变。
func TestBuildDirectPayloadFallbackWithoutRaw(t *testing.T) {
	body := map[string]any{"model": "m", "stream": false}
	got := string(buildDirectPayload(nil, body, "m"))
	want, _ := json.Marshal(body)
	if got != string(want) {
		t.Fatalf("raw 为空时应回退 json.Marshal\n got=%s\nwant=%s", got, want)
	}
	// raw 非空但缺少 model 键同样必须回退。
	got2 := string(buildDirectPayload([]byte(`{"stream":false}`), body, "m"))
	if got2 != string(want) {
		t.Fatalf("缺少 model 键时应回退 json.Marshal\n got=%s\nwant=%s", got2, want)
	}
}

// TestBuildDirectPayloadKeepsWhitespace 验证值前后的空白与整体排版完全保留，
// 因为推理集群比对的是字节前缀，任何空格差异都可能导致缓存未命中。
func TestBuildDirectPayloadKeepsWhitespace(t *testing.T) {
	raw := []byte("{\n  \"model\" : \"local\" ,\n  \"x\" : [1, 2]\n}")
	got := string(buildDirectPayload(raw, map[string]any{"model": "up"}, "up"))
	want := "{\n  \"model\" : \"up\" ,\n  \"x\" : [1, 2]\n}"
	if got != want {
		t.Fatalf("空白/排版未保真\n got=%q\nwant=%q", got, want)
	}
}
