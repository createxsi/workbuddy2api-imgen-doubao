// search.go 官方联网检索：POST {chatBase}/agenttool/v1/search（国内版/国际版同构）。
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

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const webSearchPath = "/agenttool/v1/search"

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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+webSearchPath, bytes.NewReader(body))
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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

func searchKind(status int, body string) ErrKind {
	if k := Classify(status, body); k != ErrNone {
		return k
	}
	return ErrClient
}
