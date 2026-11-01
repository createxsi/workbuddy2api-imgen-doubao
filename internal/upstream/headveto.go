// headveto.go 首帧否决：上游「HTTP 200 已开流 + 首帧即失败」时，在网关向客户端写出
// 任何字节之前识别出来，交给调用方按普通上游错误处理（罚号 / 换号 / 改道 / 透传）。
//
// 为什么放在流处理之前：SSE 一旦写出字节就改不了状态码，此前 6004 / 11140 / 6021
// 这类首帧失败只能以「200 + error 帧」透给客户端，可换号解决的失败变成用户可见的失败。
// 探针在协议桥（/v1/messages、/v1/responses 的 bridgeWriter）与原生工具调用标记还原
// 之前运行，被否决的尝试对客户端完全无痕。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// HeadVetoError 首帧否决：上游第一个 data 帧就是失败帧，或整个流没有任何有效帧。
// Payload 是标准 error 帧原文（{"error":{...}}；200 内嵌 {code,msg} 已改写成同形态），
// 空串表示零有效帧。
type HeadVetoError struct{ Payload string }

func (e *HeadVetoError) Error() string {
	if e.Payload == "" {
		return "upstream produced no frame before head commit"
	}
	return "upstream error frame before head commit: " + e.Payload
}

// IsHeadVeto 报告 err 是否为首帧否决，并取出否决详情。
func IsHeadVeto(err error) (*HeadVetoError, bool) {
	var hv *HeadVetoError
	if errors.As(err, &hv) {
		return hv, true
	}
	return nil, false
}

// StreamErrorFrame 识别「HTTP 200 流里内嵌的失败帧」，返回 (code, message, ok)。
//
// 上游把部分失败（11140 / 6021 等）塞在 SSE 里，报文是**顶层** {code,msg,displayMsg}，
// 既没有 choices 也没有 error；normalizeFrame 的白名单会把 code/msg 剥掉，客户端只看到
// 一个空 chunk。判据必须窄：只认没有 choices、没有 error、code>0 的帧——正文里出现
// 这些数字或文案是常态，按文本判必误伤。
func StreamErrorFrame(obj map[string]any) (int, string, bool) {
	if _, has := obj["choices"]; has {
		return 0, "", false
	}
	if _, has := obj["error"]; has {
		return 0, "", false
	}
	code, _ := obj["code"].(float64)
	if code <= 0 {
		return 0, "", false
	}
	msg, _ := obj["msg"].(string)
	if msg == "" {
		msg, _ = obj["displayMsg"].(string)
	}
	return int(code), msg, true
}

// streamErrorPayload 把内嵌失败帧改写成标准 error 帧：code/message 原样，requestId 有则带上，
// 其余字段不编造。
func streamErrorPayload(code int, msg, payload string) string {
	e := map[string]any{"message": msg, "code": code}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) == nil {
		if rid, ok := obj["requestId"].(string); ok && rid != "" {
			e["requestId"] = rid
		}
	}
	raw, err := json.Marshal(map[string]any{"error": e})
	if err != nil {
		return payload
	}
	return string(raw)
}

// headVetoMaxBytes 首帧探测最多缓冲的字节数（防御异常上游一直不给换行）。
const headVetoMaxBytes = 1 << 20

// VetoHead 读到上游第一个 data 帧为止并判定是否否决：
//   - 首帧带 error 键，或为 200 内嵌 {code,msg} 失败帧 → *HeadVetoError（Payload 为标准 error 帧）；
//   - 第一个非空行就是 [DONE]，或 EOF 前没有任何行 → *HeadVetoError（Payload 为空）；
//   - 其余（正常首帧、注释 / 心跳行先到、读错误）→ 返回一个回放已读字节的 reader，
//     调用方照常交给 StreamHint / Aggregate，字节级与未探测时一致。
//
// 注释 / 心跳行先到时不再继续缓冲（与既有透传语义一致：它们本来就会立刻提交响应头）。
func VetoHead(r io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var buf bytes.Buffer
	replay := func(tailErr error) io.Reader {
		if tailErr != nil && tailErr != io.EOF {
			return io.MultiReader(bytes.NewReader(buf.Bytes()), errReader{tailErr})
		}
		return io.MultiReader(bytes.NewReader(buf.Bytes()), br)
	}
	for {
		line, err := br.ReadString('\n')
		buf.WriteString(line)
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			// 帧分隔空行：继续读。
		case strings.HasPrefix(trimmed, "data: [DONE]"):
			return nil, &HeadVetoError{}
		case strings.HasPrefix(trimmed, "data: "):
			payload := strings.TrimPrefix(trimmed, "data: ")
			var probe map[string]any
			if json.Unmarshal([]byte(payload), &probe) == nil {
				if _, hasErr := probe["error"]; hasErr {
					return nil, &HeadVetoError{Payload: payload}
				}
				if code, msg, ok := StreamErrorFrame(probe); ok {
					return nil, &HeadVetoError{Payload: streamErrorPayload(code, msg, payload)}
				}
			}
			return replay(err), nil
		default:
			return replay(err), nil
		}
		if err != nil {
			if err == io.EOF {
				return nil, &HeadVetoError{}
			}
			return replay(err), nil
		}
		if buf.Len() > headVetoMaxBytes {
			return replay(nil), nil
		}
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
