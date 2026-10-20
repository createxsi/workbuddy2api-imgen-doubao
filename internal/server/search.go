// search.go 官方 agenttool 联网能力的出口：
//   - POST /v1/search：检索原文透传（deep=true 走综述式检索）；POST /v1/webfetch：网页正文；
//   - /v1/messages 的 web_search 服务端工具、/v1/responses 的托管 web_search 工具：
//     由网关执行检索，模型只负责写检索词与据结果作答。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// searchRealm 显式 realm 优先；否则取第一个可服务的域（cn → global）。
func (h *Handler) searchRealm(realm string) string {
	if realm == "cn" || realm == "global" {
		return realm
	}
	if h.cfg.Pool != nil && !h.cfg.Pool.ServableForRealm("cn") && h.cfg.Pool.ServableForRealm("global") {
		return "global"
	}
	return "cn"
}

// agentToolCall 一次 agenttool 族出站调用。
type agentToolCall func(ctx context.Context, a *auth.Auth) (json.RawMessage, error)

func (h *Handler) searchOnce(ctx context.Context, realm, query string, maxResults int) (json.RawMessage, int, error) {
	return h.agentToolOnce(ctx, realm, func(ctx context.Context, a *auth.Auth) (json.RawMessage, error) {
		return h.cfg.Upstream.Search(ctx, a, query, maxResults)
	})
}

// agentToolOnce 选号执行一次 agenttool 调用。请求级错误（4xx 业务错误）不罚号、不换号直接返回；
// 账号类错误按 chat 同口径处置后换号；传输错误只换号。
func (h *Handler) agentToolOnce(ctx context.Context, realm string, call agentToolCall) (json.RawMessage, int, error) {
	tried := map[string]bool{}
	var lastErr error
	lastStatus := http.StatusServiceUnavailable
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			if !rotateBackoff(i, ctx) {
				break
			}
			continue
		}
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				h.cfg.Pool.Release(acct.UID)
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("ERR: [search] refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}
		raw, err := call(ctx, acct)
		h.cfg.Pool.Release(acct.UID)
		if err == nil {
			return raw, http.StatusOK, nil
		}
		lastErr = err
		var ue *upstream.Error
		if errors.As(err, &ue) {
			lastStatus = ue.Status
			switch ue.Kind {
			case upstream.ErrClient, upstream.ErrBadParams:
				return raw, ue.Status, err
			}
			h.applyErrorPolicy(acct.UID, ue.Kind, string(raw), "", ue)
		} else {
			lastStatus = http.StatusBadGateway
		}
		if !rotateBackoff(i, ctx) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no healthy account for realm %s", realm)
	}
	return nil, lastStatus, lastErr
}

// agenticSearchTimeout 综述式检索单次上限（实测数十秒）。
const agenticSearchTimeout = 3 * time.Minute

// webSearch POST /v1/search  {"query":"...","max_results":5,"realm":"cn|global","deep":false}
// deep=true 走综述式检索（耗时数十秒、消耗较大），返回 done 帧原文。
func (h *Handler) webSearch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var in struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
		Realm      string `json:"realm"`
		Deep       bool   `json:"deep"`
	}
	if json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.Query) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "query is required")
		return
	}
	var raw json.RawMessage
	var status int
	if in.Deep {
		ctx, cancel := context.WithTimeout(r.Context(), agenticSearchTimeout)
		defer cancel()
		raw, status, err = h.agentToolOnce(ctx, h.searchRealm(in.Realm), func(ctx context.Context, a *auth.Auth) (json.RawMessage, error) {
			return h.cfg.Upstream.AgenticSearch(ctx, a, in.Query)
		})
	} else {
		raw, status, err = h.searchOnce(r.Context(), h.searchRealm(in.Realm), in.Query, in.MaxResults)
	}
	writeAgentToolResult(w, raw, status, err)
}

// webFetch POST /v1/webfetch  {"url":"https://...","realm":"cn|global"}：网页正文（markdown）。
func (h *Handler) webFetch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var in struct {
		URL   string `json:"url"`
		Realm string `json:"realm"`
	}
	if json.Unmarshal(body, &in) != nil || !(strings.HasPrefix(in.URL, "http://") || strings.HasPrefix(in.URL, "https://")) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "url (http/https) is required")
		return
	}
	raw, status, err := h.agentToolOnce(r.Context(), h.searchRealm(in.Realm), func(ctx context.Context, a *auth.Auth) (json.RawMessage, error) {
		return h.cfg.Upstream.WebFetch(ctx, a, in.URL)
	})
	writeAgentToolResult(w, raw, status, err)
}

func writeAgentToolResult(w http.ResponseWriter, raw json.RawMessage, status int, err error) {
	if err != nil {
		if status < 400 {
			status = http.StatusBadGateway
		}
		writeOpenAIError(w, status, "agenttool_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// ---------- 服务端检索工具 ----------

// searchToolMaxUses 客户端未给 max_uses 时的检索轮数上限。
const searchToolMaxUses = 3

// searchSpec 客户端声明的服务端检索工具。
type searchSpec struct {
	name    string
	maxUses int
}

// searchRecord 一次已执行的检索（回报给客户端）。
type searchRecord struct {
	id, query string
	results   []upstream.SearchResult
	err       string
}

func searchFunction(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name,
		"description": "Search the public web for up-to-date information. Returns pages with title, url and snippet. " +
			"Use it when the answer depends on recent or external facts.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string", "description": "Search query"}},
			"required":   []any{"query"},
		},
	}}
}

// bufWriter 收集一次内部 chat 调用的完整输出。
type bufWriter struct {
	h      http.Header
	status int
	body   []byte
}

func (b *bufWriter) Header() http.Header         { return b.h }
func (b *bufWriter) WriteHeader(c int)           { b.status = c }
func (b *bufWriter) Write(p []byte) (int, error) { b.body = append(b.body, p...); return len(p), nil }

func (h *Handler) chatSync(r *http.Request, chat map[string]any) (map[string]any, bool) {
	body, _ := json.Marshal(chat)
	bw := &bufWriter{h: http.Header{}}
	h.runChatBridge(bw, r, body)
	if bw.status != 0 && bw.status != http.StatusOK {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal(bw.body, &obj) != nil {
		return nil, false
	}
	if _, isErr := obj["error"]; isErr {
		return nil, false
	}
	return obj, true
}

// searchLoop 带检索工具做非流式调用：模型调检索就由网关执行并把结果作为 tool 消息续上，
// 直到模型不再检索（返回该轮结果 replay，由调用方按原协议回放）或用满次数
// （返回不含检索工具的 final 请求体，由调用方正常走 chat 链路作答）。
// 首轮内部调用失败时返回原请求体（final），按普通对话处理。
func (h *Handler) searchLoop(r *http.Request, chatBody []byte, spec searchSpec) (final []byte, replay map[string]any, recs []searchRecord) {
	var chat map[string]any
	if json.Unmarshal(chatBody, &chat) != nil {
		return chatBody, nil, nil
	}
	realm, _ := resolveModel(str(chat["model"]))
	stream := chat["stream"]
	baseTools, _ := chat["tools"].([]any)
	msgs, _ := chat["messages"].([]any)
	for use := 0; use < spec.maxUses; use++ {
		probe := make(map[string]any, len(chat))
		for k, v := range chat {
			probe[k] = v
		}
		delete(probe, "stream_options")
		probe["stream"] = false
		probe["messages"] = msgs
		probe["tools"] = append(append([]any{}, baseTools...), searchFunction(spec.name))
		resp, ok := h.chatSync(r, probe)
		if !ok {
			if use == 0 {
				return chatBody, nil, nil
			}
			break
		}
		msg := firstMessage(resp)
		var calls []any
		tcs, _ := msg["tool_calls"].([]any)
		for _, tc := range tcs {
			tm, _ := tc.(map[string]any)
			if fn, _ := tm["function"].(map[string]any); str(fn["name"]) == spec.name {
				calls = append(calls, tc)
			}
		}
		if len(calls) == 0 {
			return nil, resp, recs
		}
		msgs = append(append([]any{}, msgs...), map[string]any{"role": "assistant", "content": str(msg["content"]), "tool_calls": calls})
		for _, tc := range calls {
			tm, _ := tc.(map[string]any)
			fn, _ := tm["function"].(map[string]any)
			var args struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal([]byte(str(fn["arguments"])), &args)
			rec := h.runSearchTool(r.Context(), realm, args.Query)
			recs = append(recs, rec)
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": str(tm["id"]), "content": searchResultText(rec)})
		}
	}
	chat["messages"] = msgs
	chat["stream"] = stream
	if len(baseTools) == 0 {
		delete(chat, "tools")
		delete(chat, "tool_choice")
	}
	final, _ = json.Marshal(chat)
	return final, nil, recs
}

func (h *Handler) runSearchTool(ctx context.Context, realm, query string) searchRecord {
	rec := searchRecord{id: randHex(), query: query}
	if strings.TrimSpace(query) == "" {
		rec.err = "empty query"
		return rec
	}
	raw, _, err := h.searchOnce(ctx, realm, query, 0)
	if err != nil {
		log.Printf("WARN: [search] tool query failed: %v", err)
		rec.err = err.Error()
		return rec
	}
	var sr upstream.SearchResponse
	_ = json.Unmarshal(raw, &sr)
	rec.results = sr.Results
	return rec
}

// searchResultText 给模型的检索结果；失败如实告知，不伪装成「没有结果」。
func searchResultText(rec searchRecord) string {
	if rec.err != "" {
		return "Web search failed: " + rec.err
	}
	if len(rec.results) == 0 {
		return fmt.Sprintf("No web results for %q.", rec.query)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Web search results for %q:\n", rec.query)
	for i, it := range rec.results {
		fmt.Fprintf(&sb, "\n[%d] %s\nURL: %s\n%s\n", i+1, it.Title, it.URL, strings.TrimSpace(it.Snippet))
	}
	return sb.String()
}

func firstMessage(obj map[string]any) map[string]any {
	chs, _ := obj["choices"].([]any)
	if len(chs) == 0 {
		return map[string]any{}
	}
	ch, _ := chs[0].(map[string]any)
	m, _ := ch["message"].(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// replayChat 把一次非流式 chat 结果交给协议桥：流式时拆成 chunk 逐个回放，否则作为整段 JSON。
func replayChat(bw *bridgeWriter, obj map[string]any, stream bool) {
	if !stream {
		bw.status = http.StatusOK
		bw.body, _ = json.Marshal(obj)
		return
	}
	bw.rw.Header().Set("Content-Type", "text/event-stream")
	msg := firstMessage(obj)
	id := str(obj["id"])
	send := func(delta map[string]any, finish any, usage any) {
		ch := map[string]any{"id": id, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			ch["usage"] = usage
		}
		raw, _ := json.Marshal(ch)
		_ = bw.onData(string(raw))
	}
	if rc := str(msg["reasoning_content"]); rc != "" {
		send(map[string]any{"reasoning_content": rc}, nil, nil)
	}
	if t := textOf(msg["content"]); t != "" {
		send(map[string]any{"content": t}, nil, nil)
	}
	if tcs, ok := msg["tool_calls"].([]any); ok {
		for i, tc := range tcs {
			tm, _ := tc.(map[string]any)
			send(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tm["id"], "type": "function", "function": tm["function"]}}}, nil, nil)
		}
	}
	fr := "stop"
	if chs, _ := obj["choices"].([]any); len(chs) > 0 {
		if ch, _ := chs[0].(map[string]any); str(ch["finish_reason"]) != "" {
			fr = str(ch["finish_reason"])
		}
	}
	send(map[string]any{}, fr, obj["usage"])
	_ = bw.onData("[DONE]")
}
