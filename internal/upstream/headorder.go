// headorder.go 出站请求头发送顺序对齐官方客户端（Node 按代码插入序写头，Go 按 key 排序）。
//
// 顺序表来自真机夹具（chat / report / chat busy / v3 config / refresh）。本仓独有但同属
// SDK 族的链路（/v2/images/*、搜索 / webfetch 等走 ChatHeaders 的请求）按 chat 序；
// 没有证据的路径保持 Go 原生排序，不臆造。表外头（企业号头、设备 token 等）保持
// 原相对顺序，放在传输层尾段（Content-Length / Accept-Encoding / Host / Connection）之前。
package upstream

import (
	"bytes"
	"strings"
)

var chatHeadOrder = []string{
	"Accept", "Content-Type", "X-Requested-With",
	"x-stainless-arch", "x-stainless-lang", "x-stainless-os", "x-stainless-package-version",
	"x-stainless-retry-count", "x-stainless-runtime", "x-stainless-runtime-version",
	"X-Conversation-ID", "X-Agent-Purpose", "X-IDE-Type", "X-IDE-Name", "X-IDE-Version",
	"X-Agent-Intent", "X-Agent-Runtime", "X-Private-Data", "acp-connection-id",
	"x-codebuddy-request", "X-Request-ID", "X-Conversation-Message-ID",
	"X-Conversation-Request-ID", "X-Root-Request-ID", "X-Agent-Type", "baggage",
	"Content-Encoding", "traceparent", "b3", "X-B3-TraceId", "X-B3-ParentSpanId",
	"X-B3-SpanId", "X-B3-Sampled", "X-Trace-ID", "X-Product", "User-Agent",
	"Authorization", "X-User-Id", "X-Domain",
	"Content-Length", "Accept-Encoding", "Host", "Connection",
}

var refreshHeadOrder = []string{
	"Accept", "Content-Type", "X-Domain", "X-Refresh-Token", "X-Auth-Refresh-Source",
	"User-Agent", "X-Request-ID", "X-Product", "Authorization", "X-User-Id",
	"Content-Length", "Accept-Encoding", "Host", "Connection",
}

var officialHeadOrder = map[string][]string{
	"/v2/chat/completions": chatHeadOrder,
	"/v2/report": {
		"Accept", "Content-Type", "X-Request-ID", "X-Product", "User-Agent",
		"Authorization", "X-User-Id", "X-Domain",
		"Content-Length", "Accept-Encoding", "Host", "Connection",
	},
	"/v2/chat/busy": {
		"Accept", "Content-Type", "User-Agent", "X-Request-ID", "X-Product",
		"Authorization", "X-User-Id", "X-Domain",
		"Content-Length", "Accept-Encoding", "Host", "Connection",
	},
	"/v3/config": {
		"Accept", "Connection", "User-Agent", "X-Request-ID", "X-Product", "Host",
	},
	"/v2/plugin/auth/token/refresh": refreshHeadOrder,
}

// tailHeads 传输层尾段：表外头插在它们之前。
var tailHeads = map[string]bool{"content-length": true, "accept-encoding": true, "host": true, "connection": true}

// headOrderFor 按路径选顺序表；/v2/images/* 与带 SDK 族头的请求按 chat 序。
func headOrderFor(path string, names []string) []string {
	if o, ok := officialHeadOrder[path]; ok {
		return o
	}
	if strings.HasPrefix(path, "/v2/images/") {
		return chatHeadOrder
	}
	for _, n := range names {
		if strings.EqualFold(n, "x-stainless-lang") {
			return chatHeadOrder
		}
	}
	return nil
}

// requestPathOf 从请求行取 path（去 query；absolute-form 只留 path）。
func requestPathOf(line string) string {
	sp1 := strings.IndexByte(line, ' ')
	if sp1 <= 0 {
		return ""
	}
	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		return ""
	}
	target := rest[:sp2]
	if i := strings.Index(target, "://"); i >= 0 {
		if j := strings.IndexByte(target[i+3:], '/'); j >= 0 {
			target = target[i+3+j:]
		} else {
			target = "/"
		}
	}
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	return target
}

// reorderHeadBlock 按官方顺序重排完整请求头块（含结尾空行）。纯函数；无法识别时原样返回。
func reorderHeadBlock(block []byte) []byte {
	if !bytes.HasSuffix(block, []byte("\r\n\r\n")) {
		return block
	}
	lines := strings.Split(string(block[:len(block)-4]), "\r\n")
	if len(lines) < 2 {
		return block
	}
	type entry struct{ name, raw string }
	entries := make([]entry, 0, len(lines)-1)
	names := make([]string, 0, len(lines)-1)
	for _, ln := range lines[1:] {
		colon := strings.IndexByte(ln, ':')
		if colon <= 0 {
			return block // 续行等异常形态：不碰
		}
		entries = append(entries, entry{ln[:colon], ln})
		names = append(names, ln[:colon])
	}
	order := headOrderFor(requestPathOf(lines[0]), names)
	if order == nil {
		return block
	}
	rank := make(map[string]int, len(order))
	for i, k := range order {
		rank[strings.ToLower(k)] = i
	}
	used := make([]bool, len(entries))
	var out bytes.Buffer
	out.Grow(len(block))
	out.WriteString(lines[0])
	out.WriteString("\r\n")
	emit := func(i int) {
		used[i] = true
		out.WriteString(entries[i].raw)
		out.WriteString("\r\n")
	}
	flushUnranked := func() {
		for i, e := range entries {
			if _, ok := rank[strings.ToLower(e.name)]; !ok && !used[i] {
				emit(i)
			}
		}
	}
	tailDone := false
	for _, k := range order {
		lk := strings.ToLower(k)
		if tailHeads[lk] && !tailDone {
			flushUnranked()
			tailDone = true
		}
		for i, e := range entries {
			if !used[i] && strings.ToLower(e.name) == lk {
				emit(i)
			}
		}
	}
	flushUnranked()
	out.WriteString("\r\n")
	return out.Bytes()
}
