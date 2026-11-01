package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// 段1（空截断）：只有思考、正文零字节、finish_reason=length，带 usage——线上
// 「思考吃光预算」的客户端可见形态。
const testSeg1Starved = `data: {"id":"s1","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"先想想"}}]}` + "\n\n" +
	`data: {"id":"s1","choices":[{"index":0,"delta":{"reasoning_content":"再想想"}}]}` + "\n\n" +
	`data: {"id":"s1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":200,"total_tokens":210,"completion_thinking_tokens":200}}` + "\n\n" +
	"data: [DONE]\n\n"

// 段1 变体：正文有内容（正常截断），不在此救。
const testSeg1TextCut = `data: {"id":"t1","choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n" +
	`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
	"data: [DONE]\n\n"

func TestStarvedRetryBodyDisablesThinking(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","max_tokens":200,"thinking":{"type":"enabled"},` +
		`"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f"}}]}`
	out, ok := starvedRetryBody([]byte(body))
	if !ok {
		t.Fatalf("应可构造救援体")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("救援体非法 JSON: %v", err)
	}
	th, _ := obj["thinking"].(map[string]any)
	if got, _ := th["type"].(string); got != "disabled" {
		t.Errorf("thinking.type = %q want disabled", got)
	}
	if _, has := obj["reasoning_effort"]; has {
		t.Errorf("关思考后不应再带 reasoning_effort（实际: %v）", obj["reasoning_effort"])
	}
	// 额度与其余字段必须原样保留：救援不放大客户端要的上限。
	if got, _ := obj["max_tokens"].(float64); got != 200 {
		t.Errorf("max_tokens = %v want 200", got)
	}
	if _, has := obj["tools"]; !has {
		t.Errorf("tools 丢了")
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("messages 变了: %v", msgs)
	}
}

// TestStarvedRetryBodyInjectsSwitchForDeepSeek 客户端不带 thinking 时，按模型能力
// 注入 disabled（网关自己的注入发生在出站管线里，客户端 body 上没有这个字段）。
func TestStarvedRetryBodyInjectsSwitchForDeepSeek(t *testing.T) {
	for _, body := range []string{
		`{"model":"deepseek-v4.1-flash","max_tokens":200,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"cn:deepseek-v4.1-flash","max_tokens":200,"messages":[]}`,
	} {
		out, ok := starvedRetryBody([]byte(body))
		if !ok {
			t.Fatalf("deepseek 模型应可构造救援体: %s", body)
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("救援体非法 JSON: %v", err)
		}
		if th, _ := obj["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
			t.Errorf("应注入 thinking.type=disabled，实际 %v", obj["thinking"])
		}
		if got, _ := obj["max_tokens"].(float64); got != 200 {
			t.Errorf("max_tokens = %v want 200", got)
		}
	}
}

func TestStarvedRetryBodySkips(t *testing.T) {
	cases := map[string]string{
		"无 thinking 开关（非 deepseek）": `{"model":"gpt-x","max_tokens":200,"messages":[]}`,
		"已经关着":                      `{"model":"d","max_tokens":200,"thinking":{"type":"disabled"},"messages":[]}`,
		"额度太小（探活请求）":                `{"model":"d","max_tokens":1,"thinking":{"type":"enabled"},"messages":[]}`,
		"body 不是 JSON":              `not json`,
		"thinking 非法非对象":            `{"model":"d","max_tokens":200,"thinking":"enabled","messages":[]}`,
	}
	for name, body := range cases {
		if _, ok := starvedRetryBody([]byte(body)); ok {
			t.Errorf("%s：不应构造救援体", name)
		}
	}
}

func TestAggregateStarved(t *testing.T) {
	mk := func(finish string, content any, tcs bool) map[string]any {
		msg := map[string]any{"role": "assistant", "content": content}
		if tcs {
			msg["tool_calls"] = []any{map[string]any{"id": "c1"}}
		}
		return map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": msg}}}
	}
	cases := []struct {
		name string
		resp map[string]any
		want bool
	}{
		{"length+空正文", mk("length", "", false), true},
		{"length+空白正文", mk("length", "   ", false), true},
		{"length+有正文", mk("length", "半截", false), false},
		{"length+tool_calls", mk("length", "", true), false},
		{"stop+空正文", mk("stop", "", false), false},
		{"无 choices", map[string]any{}, false},
	}
	for _, c := range cases {
		if got := aggregateStarved(c.resp); got != c.want {
			t.Errorf("%s: aggregateStarved = %v want %v", c.name, got, c.want)
		}
	}
}

// TestContinueReaderStarvedEmptyCutRescues 空截断 + 客户端显式限额 → 同额度关思考
// 重发一次；客户端看到的是思考（第一段外泄的）+ 正文 + stop，不再有 length。
func TestContinueReaderStarvedEmptyCutRescues(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	body := `{"model":"deepseek-v4.1-flash","max_tokens":200,"thinking":{"type":"enabled"},"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	r := newTestContinueReader(t, up, body, io.NopCloser(strings.NewReader(testSeg1Starved)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("重发请求数=%d want 1", len(*reqs))
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte((*reqs)[0]), &sent); err != nil {
		t.Fatalf("重发请求体非法: %v", err)
	}
	if th, _ := sent["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
		t.Errorf("重发请求应关思考，实际 thinking=%v", sent["thinking"])
	}
	if _, has := sent["reasoning_effort"]; has {
		t.Errorf("重发请求不应带 reasoning_effort")
	}
	if got, _ := sent["max_tokens"].(float64); got != 200 {
		t.Errorf("重发请求 max_tokens = %v want 200（额度不放大）", got)
	}
	if !strings.Contains(s, "继续") {
		t.Errorf("客户端应看到救援段正文:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Errorf("终态应为 stop:\n%s", s)
	}
	if strings.Contains(s, `"finish_reason":"length"`) {
		t.Errorf("空截断的 length 终态不应外泄:\n%s", s)
	}
	if !strings.Contains(s, "先想想") {
		t.Errorf("第一段思考照常外泄（已发出的收不回）:\n%s", s)
	}
}

// TestContinueReaderStarvedRescueOnce 救援只做一次：重发仍空截断 → 按原路降级，
// 客户端看到 length，不会无限重发。
func TestContinueReaderStarvedRescueOnce(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg1Starved })
	body := `{"model":"deepseek-v4.1-flash","max_tokens":200,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`
	r := newTestContinueReader(t, up, body, io.NopCloser(strings.NewReader(testSeg1Starved)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("重发请求数=%d want 1（每请求只救一次）", len(*reqs))
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Errorf("救援失败应按原路降级为 length:\n%s", out)
	}
}

// TestContinueReaderStarvedTextCutUntouched 有正文的截断（客户端额度语义内）不救援。
func TestContinueReaderStarvedTextCutUntouched(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	body := `{"model":"deepseek-v4.1-flash","max_tokens":200,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`
	r := newTestContinueReader(t, up, body, io.NopCloser(strings.NewReader(testSeg1TextCut)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("重发请求数=%d want 0（有正文的截断不救）", len(*reqs))
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Errorf("原 length 终态应原样回放:\n%s", out)
	}
}
