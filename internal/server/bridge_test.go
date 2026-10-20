package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// sseToolTurn 上游流：推理 → 正文 → 一个分两片到达的工具调用 → usage。
const sseToolTurn = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"想一下\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好的\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"北京\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120,\"prompt_tokens_details\":{\"cached_tokens\":60}}}\n\n" +
	"data: [DONE]\n\n"

func bridgeHandler(t *testing.T, status int, body string, stream bool) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return status, body, stream })
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

type sseEvent struct {
	name string
	data map[string]any
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, fr := range strings.Split(body, "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(fr, "\n") {
			if strings.HasPrefix(line, "event: ") {
				ev.name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data); err != nil {
					t.Fatalf("bad data %q: %v", line, err)
				}
			}
		}
		if ev.name != "" {
			out = append(out, ev)
		}
	}
	return out
}

func TestMessagesToChatConversion(t *testing.T) {
	in := `{"model":"global:hy3","max_tokens":100,"system":[{"type":"text","text":"sys"}],
	"thinking":{"type":"enabled","budget_tokens":8000},
	"tools":[{"name":"get_weather","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}],
	"tool_choice":{"type":"any"},
	"messages":[
	 {"role":"user","content":"天气？"},
	 {"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"s"},{"type":"text","text":"查一下"},{"type":"tool_use","id":"t1","name":"get_weather","input":{"city":"北京"}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"晴"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},{"type":"text","text":"谢谢"}]}
	]}`
	chat, stream, model, err := messagesToChat([]byte(in))
	if err != nil || stream || model != "global:hy3" {
		t.Fatalf("err=%v stream=%v model=%q", err, stream, model)
	}
	var got struct {
		ReasoningEffort string           `json:"reasoning_effort"`
		ToolChoice      any              `json:"tool_choice"`
		Tools           []map[string]any `json:"tools"`
		Messages        []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(chat, &got)
	if got.ReasoningEffort != "medium" || got.ToolChoice != "required" {
		t.Errorf("effort=%q choice=%v", got.ReasoningEffort, got.ToolChoice)
	}
	if len(got.Tools) != 1 {
		t.Errorf("server tool should be skipped: %v", got.Tools)
	}
	roles := []string{}
	for _, m := range got.Messages {
		roles = append(roles, str(m["role"]))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user,user" {
		t.Fatalf("roles=%v", roles)
	}
	a := got.Messages[2]
	if a["content"] != "查一下" || len(a["tool_calls"].([]any)) != 1 {
		t.Errorf("assistant=%v", a)
	}
	if got.Messages[3]["tool_call_id"] != "t1" || got.Messages[3]["content"] != "晴" {
		t.Errorf("tool=%v", got.Messages[3])
	}
	if _, ok := got.Messages[4]["content"].([]any); !ok {
		t.Errorf("tool image should become user parts: %v", got.Messages[4])
	}
	if got.Messages[5]["content"] != "谢谢" {
		t.Errorf("trailing user=%v", got.Messages[5])
	}
}

func TestMessagesStream(t *testing.T) {
	h := bridgeHandler(t, 200, sseToolTurn, true)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"hy3","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	evs := parseSSE(t, rec.Body.String())
	var names []string
	var partial strings.Builder
	for _, e := range evs {
		names = append(names, e.name)
		if d, ok := e.data["delta"].(map[string]any); ok && d["type"] == "input_json_delta" {
			partial.WriteString(str(d["partial_json"]))
		}
	}
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop," +
		"content_block_start,content_block_delta,content_block_stop," +
		"content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if strings.Join(names, ",") != want {
		t.Fatalf("events=%v", names)
	}
	if partial.String() != `{"city":"北京"}` {
		t.Errorf("tool args=%q", partial.String())
	}
	md := evs[len(evs)-2].data
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop=%v", md)
	}
	u := md["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 40 || u["cache_read_input_tokens"].(float64) != 60 || u["output_tokens"].(float64) != 20 {
		t.Errorf("usage=%v", u)
	}
}

func TestMessagesNonStream(t *testing.T) {
	h := bridgeHandler(t, 200, sseToolTurn, true)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"hy3","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil || rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	content := msg["content"].([]any)
	if len(content) != 3 || msg["stop_reason"] != "tool_use" || msg["type"] != "message" {
		t.Fatalf("msg=%v", msg)
	}
	tu := content[2].(map[string]any)
	if tu["type"] != "tool_use" || tu["input"].(map[string]any)["city"] != "北京" {
		t.Errorf("tool_use=%v", tu)
	}
}

func TestMessagesErrorShape(t *testing.T) {
	h := bridgeHandler(t, 200, sseToolTurn, true)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != 400 || e["type"] != "error" || e["error"].(map[string]any)["type"] != "invalid_request_error" {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResponsesToChatConversion(t *testing.T) {
	in := `{"model":"hy3","instructions":"sys","reasoning":{"effort":"high"},"max_output_tokens":50,
	"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},
	         {"type":"custom","name":"apply_patch","description":"patch"},
	         {"type":"namespace","name":"mcp","tools":[{"type":"function","name":"shell"}]},
	         {"type":"web_search"}],
	"input":[
	 {"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]},
	 {"type":"message","role":"user","content":[{"type":"input_text","text":"做事"}]},
	 {"type":"reasoning","summary":[]},
	 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"好"}]},
	 {"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
	 {"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Begin Patch"},
	 {"type":"function_call","call_id":"c3","name":"shell","namespace":"mcp","arguments":"{}"},
	 {"type":"function_call_output","call_id":"c1","output":"a.txt"},
	 {"type":"custom_tool_call_output","call_id":"c2","output":"ok"},
	 {"type":"function_call_output","call_id":"c3","output":[{"type":"input_text","text":"x"},{"type":"input_image","image_url":"data:image/png;base64,AAA"}]}
	]}`
	chat, meta, err := responsesToChat([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MaxTokens       int              `json:"max_tokens"`
		ReasoningEffort string           `json:"reasoning_effort"`
		Tools           []map[string]any `json:"tools"`
		Messages        []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(chat, &got)
	if got.MaxTokens != 50 || got.ReasoningEffort != "high" || len(got.Tools) != 3 {
		t.Fatalf("max=%d effort=%q tools=%v", got.MaxTokens, got.ReasoningEffort, got.Tools)
	}
	nsName := got.Tools[2]["function"].(map[string]any)["name"]
	if nsName != "mcp__shell" {
		t.Errorf("namespaced dup name=%v", nsName)
	}
	roles := []string{}
	for _, m := range got.Messages {
		roles = append(roles, str(m["role"]))
	}
	if strings.Join(roles, ",") != "system,system,user,assistant,tool,tool,tool,user" {
		t.Fatalf("roles=%v", roles)
	}
	a := got.Messages[3]
	calls := a["tool_calls"].([]any)
	if a["content"] != "好" || len(calls) != 3 {
		t.Fatalf("assistant=%v", a)
	}
	c2 := calls[1].(map[string]any)["function"].(map[string]any)
	if c2["arguments"] != `{"input":"*** Begin Patch"}` {
		t.Errorf("custom args=%v", c2["arguments"])
	}
	if calls[2].(map[string]any)["function"].(map[string]any)["name"] != "mcp__shell" {
		t.Errorf("namespace history call not mapped: %v", calls[2])
	}
	item := meta.toolItem("fc", "c", "mcp__shell", "{}", "completed")
	if item["name"] != "shell" || item["namespace"] != "mcp" {
		t.Errorf("toolItem=%v", item)
	}
	ci := meta.toolItem("fc", "c", "apply_patch", `{"input":"P"}`, "completed")
	if ci["type"] != "custom_tool_call" || ci["input"] != "P" {
		t.Errorf("custom item=%v", ci)
	}
}

func TestResponsesStream(t *testing.T) {
	h := bridgeHandler(t, 200, sseToolTurn, true)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"hy3","stream":true,"input":"hi","tools":[{"type":"function","name":"get_weather"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	evs := parseSSE(t, rec.Body.String())
	var names []string
	for i, e := range evs {
		names = append(names, e.name)
		if int(e.data["sequence_number"].(float64)) != i {
			t.Fatalf("sequence_number at %d = %v", i, e.data["sequence_number"])
		}
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{
		"response.created,response.in_progress,response.output_item.added,response.reasoning_summary_part.added,response.reasoning_summary_text.delta",
		"response.output_text.delta",
		"response.function_call_arguments.delta,response.function_call_arguments.delta,response.function_call_arguments.done,response.output_item.done,response.completed",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("events=%v missing %s", names, want)
		}
	}
	resp := evs[len(evs)-1].data["response"].(map[string]any)
	out := resp["output"].([]any)
	if len(out) != 3 || out[2].(map[string]any)["arguments"] != `{"city":"北京"}` {
		t.Fatalf("output=%v", out)
	}
	if resp["usage"].(map[string]any)["input_tokens_details"].(map[string]any)["cached_tokens"].(float64) != 60 {
		t.Errorf("usage=%v", resp["usage"])
	}
}

func TestResponsesNonStreamIncomplete(t *testing.T) {
	sse := "data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"截\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
	h := bridgeHandler(t, 200, sse, true)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"hy3","input":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 200 || resp["status"] != "incomplete" || resp["object"] != "response" {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResponsesWebsocketRejected(t *testing.T) {
	h := bridgeHandler(t, 200, sseToolTurn, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/responses", nil))
	if rec.Code != 426 {
		t.Fatalf("code=%d", rec.Code)
	}
}

// commitWriter 模拟 net/http：Write/Flush 隐式提交 200，提交后再 WriteHeader 计为重复。
type commitWriter struct {
	*httptest.ResponseRecorder
	committed   bool
	superfluous int
}

func (c *commitWriter) WriteHeader(code int) {
	if c.committed {
		c.superfluous++
		return
	}
	c.committed = true
	c.ResponseRecorder.WriteHeader(code)
}
func (c *commitWriter) Write(p []byte) (int, error) { c.committed = true; return c.ResponseRecorder.Write(p) }
func (c *commitWriter) Flush()                      { c.committed = true; c.ResponseRecorder.Flush() }

func TestBridgeStreamNoSuperfluousWriteHeader(t *testing.T) {
	roleOnly := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	for _, up := range []string{roleOnly + sseToolTurn, "data: [DONE]\n\n", roleOnly + "data: [DONE]\n\n"} {
		for _, tc := range []struct{ path, body string }{
			{"/v1/messages", `{"model":"hy3","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
			{"/v1/responses", `{"model":"hy3","stream":true,"input":"hi"}`},
		} {
			h := bridgeHandler(t, 200, up, true)
			cw := &commitWriter{ResponseRecorder: httptest.NewRecorder()}
			h.ServeHTTP(cw, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
			if cw.superfluous != 0 {
				t.Errorf("%s upstream=%q: superfluous WriteHeader=%d code=%d", tc.path, up[:20], cw.superfluous, cw.Code)
			}
		}
	}
}
