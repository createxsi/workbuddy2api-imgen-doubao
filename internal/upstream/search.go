// search.go 官方 agenttool 联网能力（国内版/国际版同构，chatBase 同域）：
// /agenttool/v1/search（snippet 检索）、/agenttool/v1/agentic_search（综述式检索）、
// /agenttool/v1/webfetch（网页正文）。
// 鉴权与请求头同 chat（ChatHeaders）。请求体只发 query / type / max_results：
// type 固定 text2text；provider 是响应字段，请求里带会被拒；max_results 是上限不是保证。
// 响应没有 {code,msg,data} 信封，原文返回。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	webSearchPath     = "/agenttool/v1/search"
	webFetchPath      = "/agenttool/v1/webfetch"
	agenticSearchPath = "/agenttool/v1/agentic_search"
)

// WebSearchDefaultMaxResults 官方客户端的取值。
const WebSearchDefaultMaxResults = 5

// SearchResult 单条检索结果。
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Site    string `json:"site,omitempty"`
}

// SearchResponse 检索响应中桥接用到的字段。
type SearchResponse struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
}

// Search 联网检索，返回上游原文 JSON。≥400 或 200 带非零 code 返回 *Error（Kind 经 Classify）。
func (c *Client) Search(ctx context.Context, a *auth.Auth, query string, maxResults int) (json.RawMessage, error) {
	if maxResults <= 0 {
		maxResults = WebSearchDefaultMaxResults
	}
	body, err := json.Marshal(map[string]any{"query": query, "type": "text2text", "max_results": maxResults})
	if err != nil {
		return nil, err
	}
	return c.agentTool(ctx, a, webSearchPath, body)
}

// WebFetch 抓取网页正文，返回上游原文 JSON（{url,title,content}，content 为 markdown）。
func (c *Client) WebFetch(ctx context.Context, a *auth.Auth, url string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"url": url})
	if err != nil {
		return nil, err
	}
	return c.agentTool(ctx, a, webFetchPath, body)
}

// agentTool POST 到 agenttool 族 JSON 端点（与 chat 同域同头）。
func (c *Client) agentTool(ctx context.Context, a *auth.Auth, path string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	c.ChatHeaders(req, a, "", ChatMeta{})
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return raw, &Error{Kind: searchKind(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
		return raw, &Error{Kind: searchKind(resp.StatusCode, env.Msg), Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return json.RawMessage(raw), nil
}

// AgenticSearch 综述式检索（POST /agenttool/v1/agentic_search，SSE）：服务端自行多轮检索、
// 抓页并综述，耗时数十秒、消耗较大。返回 done 帧 data 原文（含 synthesis / results /
// partial 等字段，不改写）。200 但没有 done 帧视为失败，不返回半截内容。
func (c *Client) AgenticSearch(ctx context.Context, a *auth.Auth, query string) (json.RawMessage, error) {
	bizVia := "public"
	if a.IsEnterprise() {
		bizVia = "internal"
	}
	body, err := json.Marshal(map[string]any{"query": query, "search_mode": 2, "stream": true, "biz_via": bizVia})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+agenticSearchPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	c.ChatHeaders(req, a, "", ChatMeta{})
	req.Header.Set("Accept", "text/event-stream")
	hc := c.chatHTTP()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	if resp.StatusCode >= 400 {
		return raw, &Error{Kind: searchKind(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	done, ok := sseEventData(raw, "done")
	if !ok {
		return nil, fmt.Errorf("agentic search: no done frame (%d bytes): %s", len(raw), truncate(string(raw), 160))
	}
	return done, nil
}

// sseEventData 取 SSE 流中指定 event 帧的 data（LF / CRLF 换行都认）。
func sseEventData(raw []byte, want string) (json.RawMessage, bool) {
	var event string
	var data []string
	flush := func() (json.RawMessage, bool) {
		if event == want && len(data) > 0 {
			return json.RawMessage(strings.Join(data, "\n")), true
		}
		event, data = "", nil
		return nil, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			if d, ok := flush(); ok {
				return d, true
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(line[len("data:"):]))
		}
	}
	return flush()
}

func searchKind(status int, body string) ErrKind {
	if k := Classify(status, body); k != ErrNone {
		return k
	}
	return ErrClient
}
