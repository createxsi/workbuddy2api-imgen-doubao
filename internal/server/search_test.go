package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const searchResultJSON = `{"query":"q","provider":"x","results":[{"title":"标题A","url":"https://a.example/1","snippet":"摘要A"}],"total_results":1}`

// searchFake 按路径分流：检索端点回固定结果；chat 按调用次序回 replies[i]。
func searchFake(t *testing.T, replies []string, chatBodies *[]string, searches *int) *Handler {
	t.Helper()
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			if strings.HasSuffix(r.URL.Path, "/agenttool/v1/search") {
				*searches++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(searchResultJSON))}, nil
			}
			*chatBodies = append(*chatBodies, string(b))
			body := replies[len(replies)-1]
			if calls < len(replies) {
				body = replies[calls]
			}
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

const sseSearchCall = "data: {\"id\":\"p1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_s\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"query\\\":\\\"今日新闻\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
const sseAnswer = "data: {\"id\":\"p2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"据标题A\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func TestSearchEndpoint(t *testing.T) {
	var bodies []string
	n := 0
	h := searchFake(t, []string{sseAnswer}, &bodies, &n)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"q"}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "标题A") || n != 1 {
		t.Fatalf("code=%d n=%d body=%s", rec.Code, n, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{}`)))
	if rec.Code != 400 {
		t.Fatalf("empty query code=%d", rec.Code)
	}
}

func TestMessagesWebSearchTool(t *testing.T) {
	var bodies []string
	n := 0
	h := searchFake(t, []string{sseSearchCall, sseAnswer}, &bodies, &n)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"hy3","stream":true,"max_tokens":50,
	 "tools":[{"type":"web_search_20250305","name":"web_search","max_uses":2}],"messages":[{"role":"user","content":"新闻"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || n != 1 || len(bodies) != 2 {
		t.Fatalf("code=%d searches=%d chats=%d body=%s", rec.Code, n, len(bodies), rec.Body.String())
	}
	if !strings.Contains(bodies[1], "标题A") || !strings.Contains(bodies[1], `"role":"tool"`) {
		t.Errorf("second probe lacks search result: %s", bodies[1])
	}
	evs := parseSSE(t, rec.Body.String())
	var types []string
	for _, e := range evs {
		if cb, ok := e.data["content_block"].(map[string]any); ok {
			types = append(types, str(cb["type"]))
		}
	}
	if strings.Join(types, ",") != "server_tool_use,web_search_tool_result,text" {
		t.Fatalf("blocks=%v", types)
	}
	if strings.Contains(rec.Body.String(), `"tool_use"`) {
		t.Error("server tool leaked as client tool_use")
	}
}

func TestResponsesHostedWebSearch(t *testing.T) {
	var bodies []string
	n := 0
	h := searchFake(t, []string{sseSearchCall, sseAnswer}, &bodies, &n)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"hy3","tools":[{"type":"web_search"}],"input":"新闻"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	out, _ := resp["output"].([]any)
	if rec.Code != 200 || n != 1 || len(out) != 2 {
		t.Fatalf("code=%d searches=%d body=%s", rec.Code, n, rec.Body.String())
	}
	if out[0].(map[string]any)["type"] != "web_search_call" || out[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output=%v", out)
	}
}

func TestMessagesWithoutSearchToolNoProbe(t *testing.T) {
	var bodies []string
	n := 0
	h := searchFake(t, []string{sseAnswer}, &bodies, &n)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"hy3","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || len(bodies) != 1 || strings.Contains(bodies[0], "web_search") {
		t.Fatalf("code=%d chats=%d", rec.Code, len(bodies))
	}
}

func agentToolFake(t *testing.T, path, ct, body string, gotPath *string) *Handler {
	t.Helper()
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			*gotPath = r.URL.Path
			if !strings.HasSuffix(r.URL.Path, path) {
				return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

func TestSearchDeepUsesAgenticDoneFrame(t *testing.T) {
	var got string
	sse := "event: progress\r\ndata: {\"step\":1}\r\n\r\nevent: done\r\ndata: {\"synthesis\":{\"content\":\"综述\"},\"partial\":false}\r\n\r\n"
	h := agentToolFake(t, "/agenttool/v1/agentic_search", "text/event-stream", sse, &got)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"q","deep":true}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "综述") || strings.Contains(rec.Body.String(), "step") {
		t.Fatalf("code=%d path=%s body=%s", rec.Code, got, rec.Body.String())
	}
}

func TestWebFetchEndpoint(t *testing.T) {
	var got string
	h := agentToolFake(t, "/agenttool/v1/webfetch", "application/json", `{"url":"https://a.example","title":"T","content":"# hi"}`, &got)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/webfetch", strings.NewReader(`{"url":"https://a.example"}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "# hi") {
		t.Fatalf("code=%d path=%s body=%s", rec.Code, got, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/webfetch", strings.NewReader(`{"url":"file:///etc/passwd"}`)))
	if rec.Code != 400 {
		t.Fatalf("non-http url code=%d", rec.Code)
	}
}

func TestResponsesOrphanOutputBecomesUserMessage(t *testing.T) {
	chat, _, err := responsesToChat([]byte(`{"model":"hy3","input":[
	 {"role":"user","content":"hi"},
	 {"type":"function_call_output","call_id":"lost","output":"结果X"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(chat, &got)
	last := got.Messages[len(got.Messages)-1]
	if last["role"] != "user" || !strings.Contains(string(chat), "结果X") || strings.Contains(string(chat), `"role":"tool"`) {
		t.Fatalf("messages=%v", got.Messages)
	}
}
