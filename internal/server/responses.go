// responses.go OpenAI Responses 兼容入口：POST /v1/responses（Codex 等客户端）。
// 请求改写成 chat completions 交给 chatCompletions，输出翻译回 Responses 事件流。
// 不保存会话（store / previous_response_id 不生效）；客户端需每轮带全量 input。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chat, meta, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	rw := &responsesWriter{w: w, meta: meta, tools: map[int]*respTool{}}
	bw := &bridgeWriter{rw: w, onData: rw.onData}
	rw.bw = bw
	if spec := responsesSearchSpec(body); spec != nil {
		final, replay, recs := h.searchLoop(r, chat, *spec)
		rw.prelude = responsesSearchItems(recs)
		if replay != nil {
			replayChat(bw, replay, meta.stream)
			rw.finish()
			return
		}
		chat = final
	}
	h.runChatBridge(bw, r, chat)
	rw.finish()
}

// responsesWebsocket Codex 会先尝试 WebSocket 传输；明确回 426 让它回落到 HTTP。
func (h *Handler) responsesWebsocket(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, http.StatusUpgradeRequired, "websocket_unsupported", "use POST /v1/responses")
}

// responsesSearchSpec 声明了托管 web_search 工具（无 name 的 web_search / web_search_preview）时返回配置。
func responsesSearchSpec(body []byte) *searchSpec {
	var in struct {
		Tools []map[string]any `json:"tools"`
	}
	_ = json.Unmarshal(body, &in)
	for _, t := range in.Tools {
		if typ := str(t["type"]); strings.HasPrefix(typ, "web_search") && str(t["name"]) == "" {
			return &searchSpec{name: "web_search", maxUses: searchToolMaxUses}
		}
	}
	return nil
}

// responsesSearchItems 已执行的检索 → web_search_call 输出项。
func responsesSearchItems(recs []searchRecord) []map[string]any {
	var out []map[string]any
	for _, rec := range recs {
		status := "completed"
		if rec.err != "" {
			status = "failed"
		}
		out = append(out, map[string]any{"type": "web_search_call", "id": "ws_" + rec.id, "status": status,
			"action": map[string]any{"type": "search", "query": rec.query}})
	}
	return out
}

// customToolHint 追加进 custom（freeform）工具描述：chat 只有 JSON 函数工具，
// 降级为单个 string 参数 input。
const customToolHint = "This is a freeform tool: put the complete raw payload verbatim into the `input` string parameter."

// respToolInfo 出站函数名 → 客户端声明的形态，用于回程还原。
type respToolInfo struct {
	name, namespace string
	custom          bool
}

type responsesMeta struct {
	model  string
	stream bool
	tools  map[string]respToolInfo // 出站名 → 原始信息
	flat   map[string]string       // namespace+"\x00"+原名 → 出站名
}

func (m *responsesMeta) outName(name, ns string) string {
	if f, ok := m.flat[ns+"\x00"+name]; ok {
		return f
	}
	return name
}

func (m *responsesMeta) addTool(fn map[string]any, ns string, custom bool) map[string]any {
	name := str(fn["name"])
	out := name
	if _, taken := m.tools[out]; taken && ns != "" {
		out = ns + "__" + name
	}
	for i := 2; ; i++ {
		if _, taken := m.tools[out]; !taken {
			break
		}
		out = fmt.Sprintf("%s_%d", name, i)
	}
	m.tools[out] = respToolInfo{name: name, namespace: ns, custom: custom}
	m.flat[ns+"\x00"+name] = out
	f := map[string]any{"name": out}
	desc := str(fn["description"])
	if custom {
		if desc != "" {
			desc += "\n\n"
		}
		desc += customToolHint
		f["parameters"] = map[string]any{
			"type":       "object",
			"properties": map[string]any{"input": map[string]any{"type": "string"}},
			"required":   []any{"input"},
		}
	} else if p, ok := fn["parameters"]; ok && p != nil {
		f["parameters"] = p
	} else {
		f["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if desc != "" {
		f["description"] = desc
	}
	return map[string]any{"type": "function", "function": f}
}

// convertTools function / custom / namespace 组转成 chat 函数工具；其余托管工具没有执行器，跳过。
func (m *responsesMeta) convertTools(raw []any, ns string, depth int) []any {
	var out []any
	for _, t := range raw {
		tm, _ := t.(map[string]any)
		switch typ := str(tm["type"]); {
		case typ == "namespace" && depth < 4:
			sub, _ := tm["tools"].([]any)
			out = append(out, m.convertTools(sub, str(tm["name"]), depth+1)...)
		case typ == "custom" && str(tm["name"]) != "":
			out = append(out, m.addTool(tm, ns, true))
		case (typ == "function" || typ == "") && str(tm["name"]) != "":
			out = append(out, m.addTool(tm, ns, false))
		}
	}
	return out
}

func responsesToChat(src []byte) ([]byte, *responsesMeta, error) {
	var in map[string]any
	if err := json.Unmarshal(src, &in); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON: %v", err)
	}
	meta := &responsesMeta{model: str(in["model"]), tools: map[string]respToolInfo{}, flat: map[string]string{}}
	if meta.model == "" {
		return nil, nil, fmt.Errorf("model is required")
	}
	meta.stream, _ = in["stream"].(bool)
	out := map[string]any{"model": meta.model, "stream": meta.stream}
	for _, k := range []string{"temperature", "top_p", "parallel_tool_calls", "user"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if v, ok := in["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if rs, ok := in["reasoning"].(map[string]any); ok && str(rs["effort"]) != "" {
		out["reasoning_effort"] = rs["effort"]
	}
	if raw, ok := in["tools"].([]any); ok {
		if tools := meta.convertTools(raw, "", 0); len(tools) > 0 {
			out["tools"] = tools
			if tc := meta.toolChoice(in["tool_choice"]); tc != nil {
				out["tool_choice"] = tc
			}
		}
	}
	var msgs []any
	if s := str(in["instructions"]); s != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": s})
	}
	switch v := in["input"].(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		msgs = append(msgs, meta.convertInput(v)...)
	}
	out["messages"] = msgs
	chat, err := json.Marshal(out)
	return chat, meta, err
}

func (m *responsesMeta) toolChoice(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case map[string]any:
		if n := str(c["name"]); n != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": m.outName(n, str(c["namespace"]))}}
		}
	}
	return nil
}

// convertInput Responses input 项 → chat messages。
// 连续的 function_call 合并进同一条 assistant 消息（紧跟在它前面的 assistant 文本之后）；
// 工具输出里的图片另起 user 消息（chat 的 tool 消息不能带图）。
func (m *responsesMeta) convertInput(items []any) []any {
	var msgs []any
	var pendingImgs []any
	flushImgs := func() {
		if len(pendingImgs) > 0 {
			msgs = append(msgs, map[string]any{"role": "user", "content": append([]any{
				map[string]any{"type": "text", "text": "(images returned by the tool call above)"}}, pendingImgs...)})
			pendingImgs = nil
		}
	}
	lastAssistant := func() map[string]any {
		if len(msgs) == 0 {
			return nil
		}
		a, _ := msgs[len(msgs)-1].(map[string]any)
		if str(a["role"]) != "assistant" {
			return nil
		}
		return a
	}
	addCall := func(id, name, args string) {
		call := map[string]any{"id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": args}}
		if a := lastAssistant(); a != nil {
			calls, _ := a["tool_calls"].([]any)
			a["tool_calls"] = append(calls, call)
			return
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call}})
	}
	for _, it := range items {
		im, _ := it.(map[string]any)
		typ := str(im["type"])
		if typ != "function_call_output" && typ != "custom_tool_call_output" {
			flushImgs()
		}
		switch {
		case typ == "message" || (typ == "" && str(im["role"]) != ""):
			role := str(im["role"])
			if role == "developer" {
				role = "system"
			}
			c := responsesContent(im["content"])
			if role == "assistant" {
				if a := lastAssistant(); a != nil && a["tool_calls"] == nil {
					a["content"] = str(a["content"]) + textOf(im["content"])
					continue
				}
				c = textOf(im["content"])
			}
			msgs = append(msgs, map[string]any{"role": role, "content": c})
		case typ == "function_call":
			args := str(im["arguments"])
			if args == "" {
				args = "{}"
			}
			addCall(str(im["call_id"]), m.outName(str(im["name"]), str(im["namespace"])), args)
		case typ == "custom_tool_call":
			args, _ := json.Marshal(map[string]any{"input": str(im["input"])})
			addCall(str(im["call_id"]), m.outName(str(im["name"]), str(im["namespace"])), string(args))
		case typ == "function_call_output" || typ == "custom_tool_call_output":
			txt, imgs := toolOutput(im["output"])
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": str(im["call_id"]), "content": txt})
			pendingImgs = append(pendingImgs, imgs...)
		}
		// reasoning 项：上游无法复用加密推理，跳过
	}
	flushImgs()
	return msgs
}

func responsesContent(v any) any {
	parts, ok := v.([]any)
	if !ok {
		return v
	}
	var out []any
	hasImg := false
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		switch str(pm["type"]) {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": str(pm["text"])})
		case "input_image":
			if u := imageURLOf(pm["image_url"]); u != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
				hasImg = true
			}
		}
	}
	if !hasImg {
		return textOf(v)
	}
	return out
}

func imageURLOf(v any) string {
	switch u := v.(type) {
	case string:
		return u
	case map[string]any:
		return str(u["url"])
	}
	return ""
}

func toolOutput(v any) (string, []any) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	parts, _ := v.([]any)
	var sb strings.Builder
	var imgs []any
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		switch str(pm["type"]) {
		case "input_text", "output_text", "text":
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(str(pm["text"]))
		case "input_image":
			if u := imageURLOf(pm["image_url"]); u != "" {
				imgs = append(imgs, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		}
	}
	return sb.String(), imgs
}

// ---------- 输出 ----------

// toolItem 按出站名还原客户端形态的工具调用项。
func (m *responsesMeta) toolItem(id, callID, name, args, status string) map[string]any {
	info, ok := m.tools[name]
	if !ok {
		info = respToolInfo{name: name}
	}
	var item map[string]any
	if info.custom {
		var a struct {
			Input string `json:"input"`
		}
		input := args
		if json.Unmarshal([]byte(args), &a) == nil {
			input = a.Input
		}
		item = map[string]any{"type": "custom_tool_call", "id": id, "call_id": callID, "name": info.name, "input": input, "status": status}
	} else {
		item = map[string]any{"type": "function_call", "id": id, "call_id": callID, "name": info.name, "arguments": args, "status": status}
	}
	if info.namespace != "" {
		item["namespace"] = info.namespace
	}
	return item
}

func responsesUsage(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	prompt, cached, completion, reasoning := usageCounts(u)
	return map[string]any{
		"input_tokens":          prompt,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         completion,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          prompt + completion,
	}
}

// responsesStatus finish_reason → Responses 状态；截断如实报 incomplete。
func responsesStatus(fr string) (string, map[string]any) {
	switch fr {
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	}
	return "completed", nil
}

func respID(id string) string {
	if id == "" {
		id = randHex()
	}
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + id
}

func (m *responsesMeta) envelope(id string, created int64, status string, output []any, usage map[string]any, incomplete any) map[string]any {
	if output == nil {
		output = []any{}
	}
	resp := map[string]any{
		"id": id, "object": "response", "created_at": created, "status": status,
		"model": m.model, "output": output, "parallel_tool_calls": true,
		"incomplete_details": incomplete, "error": nil, "usage": usage,
	}
	return resp
}

func chatToResponse(obj map[string]any, meta *responsesMeta) map[string]any {
	id := respID(str(obj["id"]))
	var output []any
	status, incomplete := "completed", map[string]any(nil)
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		ch, _ := chs[0].(map[string]any)
		msg, _ := ch["message"].(map[string]any)
		if rc := str(msg["reasoning_content"]); rc != "" {
			output = append(output, map[string]any{"type": "reasoning", "id": "rs_" + randHex(),
				"summary": []any{map[string]any{"type": "summary_text", "text": rc}}})
		}
		if t := textOf(msg["content"]); t != "" {
			output = append(output, map[string]any{"type": "message", "id": "msg_" + randHex(), "status": "completed",
				"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": t, "annotations": []any{}}}})
		}
		tcs, _ := msg["tool_calls"].([]any)
		for _, tc := range tcs {
			tm, _ := tc.(map[string]any)
			fn, _ := tm["function"].(map[string]any)
			output = append(output, meta.toolItem("fc_"+randHex(), str(tm["id"]), str(fn["name"]), str(fn["arguments"]), "completed"))
		}
		status, incomplete = responsesStatus(str(ch["finish_reason"]))
	}
	u, _ := obj["usage"].(map[string]any)
	var inc any
	if incomplete != nil {
		inc = incomplete
	}
	return meta.envelope(id, time.Now().Unix(), status, output, responsesUsage(u), inc)
}

type respTool struct {
	outIdx         int
	id, callID     string
	name           string
	args           strings.Builder
	opened, closed bool
}

type responsesWriter struct {
	w    http.ResponseWriter
	bw   *bridgeWriter
	meta *responsesMeta

	started, done bool
	id            string
	created       int64
	seq           int
	output        []any // 已完成的输出项（按 output_index）
	nextOut       int

	// 当前打开的推理 / 正文项
	rsOpen  bool
	rsIdx   int
	rsID    string
	rsText  strings.Builder
	msgOpen bool
	msgIdx  int
	msgID   string
	msgText strings.Builder

	tools        map[int]*respTool
	toolOrder    []int
	finishReason string
	usage        map[string]any
	// prelude 网关已执行的托管检索（web_search_call），排在其余输出之前
	prelude []map[string]any
}

func (rw *responsesWriter) emit(typ string, payload map[string]any) error {
	payload["type"] = typ
	payload["sequence_number"] = rw.seq
	rw.seq++
	raw, _ := json.Marshal(payload)
	if _, err := fmt.Fprintf(rw.w, "event: %s\ndata: %s\n\n", typ, raw); err != nil {
		return err
	}
	if f, ok := rw.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (rw *responsesWriter) start(id string) error {
	if rw.started {
		return nil
	}
	rw.started = true
	rw.id = respID(id)
	rw.created = time.Now().Unix()
	h := rw.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	rw.w.WriteHeader(http.StatusOK)
	env := rw.meta.envelope(rw.id, rw.created, "in_progress", nil, nil, nil)
	if err := rw.emit("response.created", map[string]any{"response": env}); err != nil {
		return err
	}
	if err := rw.emit("response.in_progress", map[string]any{"response": env}); err != nil {
		return err
	}
	for _, item := range rw.prelude {
		idx := rw.nextOut
		rw.nextOut++
		rw.setOutput(idx, item)
		if err := rw.emit("response.output_item.added", map[string]any{"output_index": idx, "item": item}); err != nil {
			return err
		}
		if err := rw.emit("response.output_item.done", map[string]any{"output_index": idx, "item": item}); err != nil {
			return err
		}
	}
	return nil
}

func (rw *responsesWriter) setOutput(idx int, item map[string]any) {
	for len(rw.output) <= idx {
		rw.output = append(rw.output, nil)
	}
	rw.output[idx] = item
}

func (rw *responsesWriter) closeReasoning() error {
	if !rw.rsOpen {
		return nil
	}
	rw.rsOpen = false
	text := rw.rsText.String()
	part := map[string]any{"type": "summary_text", "text": text}
	base := map[string]any{"item_id": rw.rsID, "output_index": rw.rsIdx, "summary_index": 0}
	if err := rw.emit("response.reasoning_summary_text.done", merge(base, map[string]any{"text": text})); err != nil {
		return err
	}
	if err := rw.emit("response.reasoning_summary_part.done", merge(base, map[string]any{"part": part})); err != nil {
		return err
	}
	item := map[string]any{"type": "reasoning", "id": rw.rsID, "summary": []any{part}}
	rw.setOutput(rw.rsIdx, item)
	return rw.emit("response.output_item.done", map[string]any{"output_index": rw.rsIdx, "item": item})
}

func (rw *responsesWriter) closeMessage() error {
	if !rw.msgOpen {
		return nil
	}
	rw.msgOpen = false
	text := rw.msgText.String()
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	base := map[string]any{"item_id": rw.msgID, "output_index": rw.msgIdx, "content_index": 0}
	if err := rw.emit("response.output_text.done", merge(base, map[string]any{"text": text})); err != nil {
		return err
	}
	if err := rw.emit("response.content_part.done", merge(base, map[string]any{"part": part})); err != nil {
		return err
	}
	item := map[string]any{"type": "message", "id": rw.msgID, "status": "completed", "role": "assistant", "content": []any{part}}
	rw.setOutput(rw.msgIdx, item)
	return rw.emit("response.output_item.done", map[string]any{"output_index": rw.msgIdx, "item": item})
}

func merge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func (rw *responsesWriter) onData(payload string) error {
	if payload == "[DONE]" {
		return rw.complete()
	}
	if rw.done {
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
		return rw.fail(msg)
	}
	if err := rw.start(c.ID); err != nil {
		return err
	}
	if c.Usage != nil {
		rw.usage = c.Usage
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.ReasoningContent != "" {
			if err := rw.onReasoning(d.ReasoningContent); err != nil {
				return err
			}
		}
		if d.Content != "" {
			if err := rw.onText(d.Content); err != nil {
				return err
			}
		}
		for _, tc := range d.ToolCalls {
			if err := rw.onTool(tc); err != nil {
				return err
			}
		}
		if ch.FinishReason != "" {
			rw.finishReason = ch.FinishReason
		}
	}
	return nil
}

func (rw *responsesWriter) onReasoning(delta string) error {
	if !rw.rsOpen {
		if err := rw.closeMessage(); err != nil {
			return err
		}
		rw.rsOpen, rw.rsIdx, rw.rsID = true, rw.nextOut, "rs_"+randHex()
		rw.nextOut++
		rw.rsText.Reset()
		if err := rw.emit("response.output_item.added", map[string]any{"output_index": rw.rsIdx,
			"item": map[string]any{"type": "reasoning", "id": rw.rsID, "summary": []any{}}}); err != nil {
			return err
		}
		if err := rw.emit("response.reasoning_summary_part.added", map[string]any{"item_id": rw.rsID,
			"output_index": rw.rsIdx, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}); err != nil {
			return err
		}
	}
	rw.rsText.WriteString(delta)
	return rw.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": rw.rsID,
		"output_index": rw.rsIdx, "summary_index": 0, "delta": delta})
}

func (rw *responsesWriter) onText(delta string) error {
	if !rw.msgOpen {
		if err := rw.closeReasoning(); err != nil {
			return err
		}
		rw.msgOpen, rw.msgIdx, rw.msgID = true, rw.nextOut, "msg_"+randHex()
		rw.nextOut++
		rw.msgText.Reset()
		if err := rw.emit("response.output_item.added", map[string]any{"output_index": rw.msgIdx,
			"item": map[string]any{"type": "message", "id": rw.msgID, "status": "in_progress", "role": "assistant", "content": []any{}}}); err != nil {
			return err
		}
		if err := rw.emit("response.content_part.added", map[string]any{"item_id": rw.msgID, "output_index": rw.msgIdx,
			"content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
	}
	rw.msgText.WriteString(delta)
	return rw.emit("response.output_text.delta", map[string]any{"item_id": rw.msgID, "output_index": rw.msgIdx,
		"content_index": 0, "delta": delta})
}

// onTool 工具调用：名字到达才宣告 output_item.added（客户端按 added 派发执行器）；
// custom 工具的 input 需要从 JSON 包裹里解出，参数攒齐后在 done 一次给全。
func (rw *responsesWriter) onTool(tc toolCallDelta) error {
	t := rw.tools[tc.Index]
	if t == nil {
		t = &respTool{}
		rw.tools[tc.Index] = t
		rw.toolOrder = append(rw.toolOrder, tc.Index)
	}
	if tc.ID != "" && t.callID == "" {
		t.callID = tc.ID
	}
	if tc.Function.Name != "" && t.name == "" {
		t.name = tc.Function.Name
	}
	before := t.args.Len()
	t.args.WriteString(tc.Function.Arguments)
	if !t.opened {
		if t.name == "" {
			return nil
		}
		if err := rw.closeReasoning(); err != nil {
			return err
		}
		if err := rw.closeMessage(); err != nil {
			return err
		}
		if t.callID == "" {
			t.callID = "call_" + randHex()
		}
		t.opened, t.outIdx, t.id = true, rw.nextOut, "fc_"+randHex()
		rw.nextOut++
		before = 0
		item := rw.meta.toolItem(t.id, t.callID, t.name, "", "in_progress")
		if err := rw.emit("response.output_item.added", map[string]any{"output_index": t.outIdx, "item": item}); err != nil {
			return err
		}
	}
	if rw.meta.tools[t.name].custom || t.args.Len() == before {
		return nil
	}
	return rw.emit("response.function_call_arguments.delta", map[string]any{"item_id": t.id,
		"output_index": t.outIdx, "delta": t.args.String()[before:]})
}

func (rw *responsesWriter) closeTools() error {
	for _, i := range rw.toolOrder {
		t := rw.tools[i]
		if !t.opened || t.closed {
			continue
		}
		t.closed = true
		args := t.args.String()
		if args == "" {
			args = "{}"
		}
		item := rw.meta.toolItem(t.id, t.callID, t.name, args, "completed")
		if item["type"] == "custom_tool_call" {
			if err := rw.emit("response.custom_tool_call_input.done", map[string]any{"item_id": t.id,
				"output_index": t.outIdx, "input": item["input"]}); err != nil {
				return err
			}
		} else if err := rw.emit("response.function_call_arguments.done", map[string]any{"item_id": t.id,
			"output_index": t.outIdx, "arguments": args}); err != nil {
			return err
		}
		rw.setOutput(t.outIdx, item)
		if err := rw.emit("response.output_item.done", map[string]any{"output_index": t.outIdx, "item": item}); err != nil {
			return err
		}
	}
	return nil
}

func (rw *responsesWriter) complete() error {
	if rw.done {
		return nil
	}
	if err := rw.start(""); err != nil {
		return err
	}
	if err := rw.closeReasoning(); err != nil {
		return err
	}
	if err := rw.closeMessage(); err != nil {
		return err
	}
	if err := rw.closeTools(); err != nil {
		return err
	}
	rw.done = true
	status, incomplete := responsesStatus(rw.finishReason)
	var inc any
	if incomplete != nil {
		inc = incomplete
	}
	var out []any
	for _, it := range rw.output {
		if it != nil {
			out = append(out, it)
		}
	}
	env := rw.meta.envelope(rw.id, rw.created, status, out, responsesUsage(rw.usage), inc)
	return rw.emit("response."+status, map[string]any{"response": env})
}

func (rw *responsesWriter) fail(msg string) error {
	if rw.done {
		return nil
	}
	if err := rw.start(""); err != nil {
		return err
	}
	rw.done = true
	env := rw.meta.envelope(rw.id, rw.created, "failed", nil, nil, nil)
	env["error"] = map[string]any{"code": "server_error", "message": msg}
	return rw.emit("response.failed", map[string]any{"response": env})
}

func (rw *responsesWriter) finish() {
	if rw.bw.isSSE() && rw.started {
		if !rw.done {
			_ = rw.complete()
		}
		return
	}
	status := rw.bw.status
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		// 错误体本就是 OpenAI 形态，原样透出
		rw.w.Header().Set("Content-Type", "application/json")
		rw.w.WriteHeader(status)
		_, _ = rw.w.Write(rw.bw.body)
		return
	}
	var obj map[string]any
	if json.Unmarshal(rw.bw.body, &obj) != nil {
		writeOpenAIError(rw.w, http.StatusBadGateway, "upstream_parse", "invalid upstream response")
		return
	}
	if _, isErr := obj["error"]; isErr {
		rw.w.Header().Set("Content-Type", "application/json")
		rw.w.WriteHeader(http.StatusBadGateway)
		_, _ = rw.w.Write(rw.bw.body)
		return
	}
	resp := chatToResponse(obj, rw.meta)
	if len(rw.prelude) > 0 {
		out := make([]any, 0, len(rw.prelude))
		for _, it := range rw.prelude {
			out = append(out, it)
		}
		resp["output"] = append(out, resp["output"].([]any)...)
	}
	writeJSON(rw.w, http.StatusOK, resp)
}
