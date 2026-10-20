// bridge.go 协议桥共用件：/v1/messages 与 /v1/responses 都把入站请求改写成
// chat completions 请求体，交给 chatCompletions 走既有选号/轮转/错误处置链路，
// 再由 bridgeWriter 把 chat 的输出（SSE 帧或整段 JSON）翻译成目标协议。
package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// bridgeWriter 截获 chatCompletions 的输出。
//   - SSE（Content-Type 为 event-stream）：按空行切帧，逐个 data 载荷交给 onData；
//   - 其它（非流式成功体或任意错误体）：整段缓存，由调用方在 finish 时翻译。
type bridgeWriter struct {
	rw     http.ResponseWriter
	status int
	rest   []byte
	body   []byte
	onData func(payload string) error
	err    error
}

func (b *bridgeWriter) Header() http.Header  { return b.rw.Header() }
func (b *bridgeWriter) WriteHeader(code int) { b.status = code }

func (b *bridgeWriter) Flush() {
	if f, ok := b.rw.(http.Flusher); ok {
		f.Flush()
	}
}

func (b *bridgeWriter) isSSE() bool {
	return strings.Contains(b.rw.Header().Get("Content-Type"), "event-stream")
}

func (b *bridgeWriter) Write(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if !b.isSSE() {
		b.body = append(b.body, p...)
		return len(p), nil
	}
	b.rest = append(b.rest, p...)
	for {
		i := bytes.Index(b.rest, []byte("\n\n"))
		if i < 0 {
			break
		}
		frame := string(b.rest[:i])
		b.rest = b.rest[i+2:]
		if payload, ok := sseData(frame); ok {
			if err := b.onData(payload); err != nil {
				b.err = err
				return len(p), err
			}
		}
	}
	return len(p), nil
}

// sseData 取一帧里的 data 行（多行 data 按规范以换行拼接）；注释帧/空帧返回 false。
func sseData(frame string) (string, bool) {
	var parts []string
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			parts = append(parts, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// runChatBridge 用改写后的 chat 请求体调用 chatCompletions。
func (h *Handler) runChatBridge(w http.ResponseWriter, r *http.Request, chat []byte) {
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))
	r2.Header.Set("Content-Type", "application/json")
	h.chatCompletions(w, r2)
}

// chatErrorMessage 从 chat 侧错误体（OpenAI 形态）取出 message 与 code。
func chatErrorMessage(body []byte) (msg, code string) {
	var obj struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &obj) == nil && obj.Error.Message != "" {
		if s, ok := obj.Error.Code.(string); ok {
			code = s
		}
		return obj.Error.Message, code
	}
	msg = strings.TrimSpace(string(body))
	if msg == "" {
		msg = "upstream error"
	}
	return msg, ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// textOf 把 string 或 [{type:text|input_text|output_text,text}] 收成纯文本。
func textOf(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, it := range c {
			m, _ := it.(map[string]any)
			if t := str(m["text"]); t != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

// usageCounts 从 chat usage 取 输入（含缓存）/ 缓存命中 / 输出 / 推理 token。
func usageCounts(u map[string]any) (prompt, cached, completion, reasoning int64) {
	if u == nil {
		return
	}
	prompt = num(u["prompt_tokens"])
	completion = num(u["completion_tokens"])
	if hit, _, ok := cacheTokensFromUsage(u); ok {
		cached = hit
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		reasoning = num(d["reasoning_tokens"])
	}
	return
}

// toolCallDelta 流式 chunk 里 delta.tool_calls 的一项。
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatChunk 流式 chunk 中桥接关心的字段。
type chatChunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Usage   map[string]any `json:"usage"`
	Error   map[string]any `json:"error"`
	Choices []struct {
		Delta struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// randHex 24 位随机十六进制，用于本地合成的消息/调用 ID。
func randHex() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
