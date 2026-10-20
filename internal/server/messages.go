// messages.go Anthropic Messages 兼容入口：POST /v1/messages（及 /messages）。
// 请求改写成 chat completions 交给 chatCompletions，输出翻译回 Messages 形态。
// 不保存会话；thinking 块没有可校验的 signature，历史里的 thinking 不回传上游。
// 上游没有对应物的字段（top_k / cache_control / betas 等）忽略。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// withAnthropicAuth 官方 SDK 用 x-api-key 头；没有 Authorization 时把它提升为 Bearer。
func (h *Handler) withAnthropicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if k := r.Header.Get("x-api-key"); k != "" {
				r.Header.Set("Authorization", "Bearer "+k)
			}
		}
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			writeAnthropicError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	chat, stream, model, err := messagesToChat(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	mw := &messagesWriter{w: w, stream: stream, model: model, tools: map[int]*anthTool{}}
	bw := &bridgeWriter{rw: w, onData: mw.onData}
	mw.bw = bw
	if spec := anthropicSearchSpec(body); spec != nil {
		final, replay, recs := h.searchLoop(r, chat, *spec)
		mw.prelude = anthropicSearchBlocks(recs)
		if replay != nil {
			replayChat(bw, replay, stream)
			mw.finish()
			return
		}
		chat = final
	}
	h.runChatBridge(bw, r, chat)
	mw.finish()
}

func anthropicErrType(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusServiceUnavailable || status == 529:
		return "overloaded_error"
	}
	return "api_error"
}

func anthropicErrorBody(status int, msg string) map[string]any {
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrType(status), "message": msg},
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, anthropicErrorBody(status, msg))
}

// ---------- 请求：Messages → chat ----------

func messagesToChat(src []byte) (chat []byte, stream bool, model string, err error) {
	var in map[string]any
	if err = json.Unmarshal(src, &in); err != nil {
		return nil, false, "", fmt.Errorf("invalid JSON: %v", err)
	}
	model = str(in["model"])
	if model == "" {
		return nil, false, "", fmt.Errorf("model is required")
	}
	stream, _ = in["stream"].(bool)
	out := map[string]any{"model": model, "stream": stream}
	for _, k := range []string{"max_tokens", "temperature", "top_p"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if v, ok := in["stop_sequences"]; ok {
		out["stop"] = v
	}
	if md, ok := in["metadata"].(map[string]any); ok && str(md["user_id"]) != "" {
		out["user"] = md["user_id"]
	}
	if e := anthropicEffort(in); e != "" {
		out["reasoning_effort"] = e
	}
	if tools := anthropicTools(in["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if tc, ok := in["tool_choice"].(map[string]any); ok {
			if c := anthropicToolChoice(tc); c != nil {
				out["tool_choice"] = c
			}
			if dis, _ := tc["disable_parallel_tool_use"].(bool); dis {
				out["parallel_tool_calls"] = false
			}
		}
	}
	var msgs []any
	if sys := anthropicSystem(in["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	raw, _ := in["messages"].([]any)
	for _, m := range raw {
		mm, _ := m.(map[string]any)
		msgs = append(msgs, anthropicMessage(str(mm["role"]), mm["content"])...)
	}
	out["messages"] = msgs
	chat, err = json.Marshal(out)
	return chat, stream, model, err
}

// anthropicEffort output_config.effort 优先；否则按 thinking.budget_tokens 折成档位。
func anthropicEffort(in map[string]any) string {
	if oc, ok := in["output_config"].(map[string]any); ok {
		if e := str(oc["effort"]); e != "" {
			return e
		}
	}
	th, _ := in["thinking"].(map[string]any)
	if th == nil || str(th["type"]) != "enabled" {
		return ""
	}
	switch n := num(th["budget_tokens"]); {
	case n <= 0:
		return ""
	case n < 4096:
		return "low"
	case n < 16384:
		return "medium"
	default:
		return "high"
	}
}

func anthropicSystem(v any) string { return textOf(v) }

// anthropicTools 只转客户端函数工具（有 input_schema 的）；服务端内置工具没有本地执行器，跳过。
func anthropicTools(v any) []any {
	raw, _ := v.([]any)
	var out []any
	for _, t := range raw {
		m, _ := t.(map[string]any)
		name := str(m["name"])
		typ := str(m["type"])
		if name == "" || (typ != "" && typ != "custom") {
			continue
		}
		fn := map[string]any{"name": name}
		if d := str(m["description"]); d != "" {
			fn["description"] = d
		}
		if s, ok := m["input_schema"]; ok {
			fn["parameters"] = s
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// anthropicSearchSpec 客户端声明了 web_search 服务端工具（type web_search_YYYYMMDD）时返回其配置。
func anthropicSearchSpec(body []byte) *searchSpec {
	var in struct {
		Tools []map[string]any `json:"tools"`
	}
	_ = json.Unmarshal(body, &in)
	for _, t := range in.Tools {
		if strings.HasPrefix(str(t["type"]), "web_search_") {
			spec := &searchSpec{name: str(t["name"]), maxUses: int(num(t["max_uses"]))}
			if spec.name == "" {
				spec.name = "web_search"
			}
			if spec.maxUses <= 0 || spec.maxUses > searchToolMaxUses {
				spec.maxUses = searchToolMaxUses
			}
			return spec
		}
	}
	return nil
}

// anthropicSearchBlocks 已执行的检索 → server_tool_use + web_search_tool_result 成对块。
func anthropicSearchBlocks(recs []searchRecord) []map[string]any {
	var out []map[string]any
	for _, rec := range recs {
		id := "srvtoolu_" + rec.id
		out = append(out, map[string]any{"type": "server_tool_use", "id": id, "name": "web_search",
			"input": map[string]any{"query": rec.query}})
		var content any
		if rec.err != "" {
			content = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
		} else {
			items := []any{}
			for _, it := range rec.results {
				items = append(items, map[string]any{"type": "web_search_result", "title": it.Title, "url": it.URL,
					"encrypted_content": "", "page_age": nil})
			}
			content = items
		}
		out = append(out, map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": content})
	}
	return out
}

func anthropicToolChoice(tc map[string]any) any {
	switch str(tc["type"]) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if n := str(tc["name"]); n != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": n}}
		}
	}
	return nil
}

// anthropicMessage 一条 Messages 消息 → 一条或多条 chat 消息。
// user 里的 tool_result 拆成 role=tool（排在同条消息的其余内容之前，保持与上一条
// assistant tool_calls 相邻）；tool_result 里的图片放进紧随其后的 user 消息。
func anthropicMessage(role string, content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": s}}
	}
	blocks, _ := content.([]any)
	var parts []any
	var toolMsgs, toolImgs []any
	var calls []any
	var text strings.Builder
	addText := func(t string) {
		if t == "" {
			return
		}
		parts = append(parts, map[string]any{"type": "text", "text": t})
		if text.Len() > 0 {
			text.WriteString("\n")
		}
		text.WriteString(t)
	}
	for _, b := range blocks {
		m, _ := b.(map[string]any)
		switch str(m["type"]) {
		case "text":
			addText(str(m["text"]))
		case "image":
			if p := anthropicImage(m); p != nil {
				parts = append(parts, p)
			}
		case "document":
			if src, ok := m["source"].(map[string]any); ok && str(src["type"]) == "text" {
				addText(str(src["data"]))
			}
		case "tool_use":
			args, _ := json.Marshal(m["input"])
			if m["input"] == nil {
				args = []byte("{}")
			}
			calls = append(calls, map[string]any{
				"id": str(m["id"]), "type": "function",
				"function": map[string]any{"name": str(m["name"]), "arguments": string(args)},
			})
		case "tool_result":
			txt, imgs := toolResultContent(m["content"])
			if isErr, _ := m["is_error"].(bool); isErr && txt != "" {
				txt = "[error] " + txt
			}
			toolMsgs = append(toolMsgs, map[string]any{"role": "tool", "tool_call_id": str(m["tool_use_id"]), "content": txt})
			toolImgs = append(toolImgs, imgs...)
		case "web_search_tool_result":
			// 历史里的检索结果（本网关此前回报的）：转成文本保留上下文
			if items, ok := m["content"].([]any); ok && len(items) > 0 {
				var sb strings.Builder
				sb.WriteString("Web search results:")
				for _, it := range items {
					im, _ := it.(map[string]any)
					sb.WriteString("\n- " + str(im["title"]) + " " + str(im["url"]))
				}
				addText(sb.String())
			}
		}
		// thinking / redacted_thinking：无 signature 可校验，不回传
	}
	if role == "assistant" {
		msg := map[string]any{"role": "assistant", "content": text.String()}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		if text.Len() == 0 && len(calls) == 0 {
			return nil
		}
		return []any{msg}
	}
	out := append([]any{}, toolMsgs...)
	if len(toolImgs) > 0 {
		out = append(out, map[string]any{"role": "user", "content": append([]any{
			map[string]any{"type": "text", "text": "(images returned by the tool call above)"}}, toolImgs...)})
	}
	if len(parts) > 0 {
		if onlyText(parts) {
			out = append(out, map[string]any{"role": role, "content": text.String()})
		} else {
			out = append(out, map[string]any{"role": role, "content": parts})
		}
	}
	return out
}

func onlyText(parts []any) bool {
	for _, p := range parts {
		if m, _ := p.(map[string]any); str(m["type"]) != "text" {
			return false
		}
	}
	return true
}

func anthropicImage(m map[string]any) map[string]any {
	src, _ := m["source"].(map[string]any)
	var u string
	switch str(src["type"]) {
	case "base64":
		u = "data:" + str(src["media_type"]) + ";base64," + str(src["data"])
	case "url":
		u = str(src["url"])
	}
	if u == "" {
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}
}

func toolResultContent(v any) (string, []any) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	blocks, _ := v.([]any)
	var sb strings.Builder
	var imgs []any
	for _, b := range blocks {
		m, _ := b.(map[string]any)
		switch str(m["type"]) {
		case "text":
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(str(m["text"]))
		case "image":
			if p := anthropicImage(m); p != nil {
				imgs = append(imgs, p)
			}
		}
	}
	return sb.String(), imgs
}

// ---------- 响应：chat → Messages ----------

func anthropicStop(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// anthropicUsage Messages 口径：input_tokens 不含缓存命中部分。
func anthropicUsage(u map[string]any) map[string]any {
	prompt, cached, completion, _ := usageCounts(u)
	in := prompt - cached
	if in < 0 {
		in = 0
	}
	return map[string]any{
		"input_tokens":                in,
		"output_tokens":               completion,
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
	}
}

func anthropicMsgID(id string) string {
	if id == "" {
		id = randHex()
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

func chatToAnthropicMessage(obj map[string]any, model string) map[string]any {
	var content []any
	stop := "end_turn"
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		ch, _ := chs[0].(map[string]any)
		msg, _ := ch["message"].(map[string]any)
		if rc := str(msg["reasoning_content"]); rc != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": rc, "signature": ""})
		}
		if t := textOf(msg["content"]); t != "" {
			content = append(content, map[string]any{"type": "text", "text": t})
		}
		tcs, _ := msg["tool_calls"].([]any)
		for _, tc := range tcs {
			m, _ := tc.(map[string]any)
			fn, _ := m["function"].(map[string]any)
			content = append(content, map[string]any{
				"type": "tool_use", "id": str(m["id"]), "name": str(fn["name"]),
				"input": parseArgs(str(fn["arguments"])),
			})
		}
		stop = anthropicStop(str(ch["finish_reason"]))
		if len(tcs) > 0 {
			stop = "tool_use"
		}
	}
	if content == nil {
		content = []any{}
	}
	u, _ := obj["usage"].(map[string]any)
	return map[string]any{
		"id": anthropicMsgID(str(obj["id"])), "type": "message", "role": "assistant",
		"model": model, "content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": anthropicUsage(u),
	}
}

func parseArgs(s string) any {
	var v any
	if strings.TrimSpace(s) == "" || json.Unmarshal([]byte(s), &v) != nil {
		return map[string]any{}
	}
	if _, ok := v.(map[string]any); !ok {
		return map[string]any{}
	}
	return v
}

// ---------- 流式翻译 ----------

type anthTool struct {
	block      int
	id, name   string
	opened     bool
	pendingArg strings.Builder
}

type messagesWriter struct {
	w      http.ResponseWriter
	bw     *bridgeWriter
	stream bool
	model  string

	started, done bool
	id            string
	open          string // 当前打开的块类型：thinking / text / tool
	openIdx       int
	next          int
	tools         map[int]*anthTool
	openTool      *anthTool
	finishReason  string
	usage         map[string]any
	// prelude 网关已执行的服务端检索块，排在正文之前
	prelude []map[string]any
}

func (m *messagesWriter) emit(event string, payload map[string]any) error {
	payload["type"] = event
	raw, _ := json.Marshal(payload)
	if _, err := fmt.Fprintf(m.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if f, ok := m.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (m *messagesWriter) start(id string) error {
	if m.started {
		return nil
	}
	m.started = true
	m.id = anthropicMsgID(id)
	h := m.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	m.w.WriteHeader(http.StatusOK)
	if err := m.emit("message_start", map[string]any{"message": map[string]any{
		"id": m.id, "type": "message", "role": "assistant", "model": m.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	}}); err != nil {
		return err
	}
	for _, b := range m.prelude {
		idx := m.next
		m.next++
		if err := m.emit("content_block_start", map[string]any{"index": idx, "content_block": b}); err != nil {
			return err
		}
		if err := m.emit("content_block_stop", map[string]any{"index": idx}); err != nil {
			return err
		}
	}
	return nil
}

func (m *messagesWriter) closeBlock() error {
	if m.open == "" {
		return nil
	}
	if m.open == "thinking" {
		// 上游没有 signature；发空 signature_delta 让严格客户端的块结构完整。
		if err := m.emit("content_block_delta", map[string]any{"index": m.openIdx,
			"delta": map[string]any{"type": "signature_delta", "signature": ""}}); err != nil {
			return err
		}
	}
	m.open, m.openTool = "", nil
	return m.emit("content_block_stop", map[string]any{"index": m.openIdx})
}

func (m *messagesWriter) openBlock(kind string, block map[string]any) error {
	if err := m.closeBlock(); err != nil {
		return err
	}
	m.open, m.openIdx = kind, m.next
	m.next++
	return m.emit("content_block_start", map[string]any{"index": m.openIdx, "content_block": block})
}

func (m *messagesWriter) onData(payload string) error {
	if payload == "[DONE]" {
		return m.stop()
	}
	if m.done {
		return nil
	}
	var c chatChunk
	if json.Unmarshal([]byte(payload), &c) != nil {
		return nil
	}
	if c.Error != nil {
		msg := str(c.Error["message"])
		if msg == "" {
			msg = "upstream error"
		}
		return m.fail(msg)
	}
	if err := m.start(c.ID); err != nil {
		return err
	}
	if c.Usage != nil {
		m.usage = c.Usage
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.ReasoningContent != "" {
			if m.open != "thinking" {
				if err := m.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""}); err != nil {
					return err
				}
			}
			if err := m.emit("content_block_delta", map[string]any{"index": m.openIdx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": d.ReasoningContent}}); err != nil {
				return err
			}
		}
		if d.Content != "" {
			if m.open != "text" {
				if err := m.openBlock("text", map[string]any{"type": "text", "text": ""}); err != nil {
					return err
				}
			}
			if err := m.emit("content_block_delta", map[string]any{"index": m.openIdx,
				"delta": map[string]any{"type": "text_delta", "text": d.Content}}); err != nil {
				return err
			}
		}
		for _, tc := range d.ToolCalls {
			if err := m.onTool(tc); err != nil {
				return err
			}
		}
		if ch.FinishReason != "" {
			m.finishReason = ch.FinishReason
		}
	}
	return nil
}

func (m *messagesWriter) onTool(tc toolCallDelta) error {
	t := m.tools[tc.Index]
	if t == nil {
		t = &anthTool{}
		m.tools[tc.Index] = t
	}
	if tc.ID != "" && t.id == "" {
		t.id = tc.ID
	}
	if tc.Function.Name != "" && t.name == "" {
		t.name = tc.Function.Name
	}
	t.pendingArg.WriteString(tc.Function.Arguments)
	if !t.opened {
		if t.name == "" {
			return nil // 等名字到达再宣告，参数先攒着
		}
		if t.id == "" {
			t.id = "toolu_" + randHex()
		}
		if err := m.openBlock("tool", map[string]any{"type": "tool_use", "id": t.id, "name": t.name, "input": map[string]any{}}); err != nil {
			return err
		}
		t.opened, t.block = true, m.openIdx
		m.openTool = t
	} else if m.openTool != t {
		// 并行调用交错到达：已关闭的块不能再追加，剩余参数丢进当前块会错位，只能忽略
		t.pendingArg.Reset()
		return nil
	}
	if t.pendingArg.Len() == 0 {
		return nil
	}
	arg := t.pendingArg.String()
	t.pendingArg.Reset()
	return m.emit("content_block_delta", map[string]any{"index": t.block,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": arg}})
}

func (m *messagesWriter) stop() error {
	if m.done {
		return nil
	}
	if err := m.start(""); err != nil {
		return err
	}
	if err := m.closeBlock(); err != nil {
		return err
	}
	m.done = true
	stop := anthropicStop(m.finishReason)
	for _, t := range m.tools {
		if t.opened {
			stop = "tool_use"
			break
		}
	}
	u := anthropicUsage(m.usage)
	if err := m.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": u,
	}); err != nil {
		return err
	}
	return m.emit("message_stop", map[string]any{})
}

func (m *messagesWriter) fail(msg string) error {
	if m.done {
		return nil
	}
	if !m.started {
		m.started = true
		m.w.Header().Set("Content-Type", "text/event-stream")
		m.w.WriteHeader(http.StatusOK)
	}
	m.done = true
	return m.emit("error", map[string]any{"error": map[string]any{"type": "api_error", "message": msg}})
}

// finish chatCompletions 返回后收尾：流式补齐收尾事件；非流式/错误体整段翻译。
func (m *messagesWriter) finish() {
	if m.bw.isSSE() && m.started {
		if !m.done {
			_ = m.stop()
		}
		return
	}
	status := m.bw.status
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		msg, _ := chatErrorMessage(m.bw.body)
		writeAnthropicError(m.w, status, msg)
		return
	}
	var obj map[string]any
	if json.Unmarshal(m.bw.body, &obj) != nil {
		writeAnthropicError(m.w, http.StatusBadGateway, "invalid upstream response")
		return
	}
	if _, isErr := obj["error"]; isErr {
		msg, _ := chatErrorMessage(m.bw.body)
		writeAnthropicError(m.w, http.StatusBadGateway, msg)
		return
	}
	msg := chatToAnthropicMessage(obj, m.model)
	if len(m.prelude) > 0 {
		content := make([]any, 0, len(m.prelude))
		for _, b := range m.prelude {
			content = append(content, b)
		}
		msg["content"] = append(content, msg["content"].([]any)...)
	}
	writeJSON(m.w, http.StatusOK, msg)
}
