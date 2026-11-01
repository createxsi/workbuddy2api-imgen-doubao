// headorder_conn.go 把官方头顺序落到连接层：包一层 net.Conn，在 Write 侧攒齐请求头块后
// 重排再写出。
//
// 请求边界只看**写侧自己发出的分帧**（Content-Length 计数 / chunked 解析 / 无 body），
// 不依赖读响应——早到的错误响应、连接复用都不会让状态错位。任何一步判断不出来就
// 永久降级为原样透传（最坏只是没重排），绝不改动数据本身，也不会扣住字节不发。
package upstream

import (
	"bytes"
	"net"
	"strconv"
	"strings"
)

// maxPendingHead 请求头累积上限：超过放弃重排、原样放行。
const maxPendingHead = 64 * 1024

type headOrderConn struct {
	net.Conn
	pending  []byte // 未完整的请求头
	inBody   bool
	remain   int64 // Content-Length 剩余
	chunked  bool
	chunk    chunkTracker
	disabled bool
}

func newHeadOrderConn(c net.Conn) net.Conn { return &headOrderConn{Conn: c} }

// Unwrap 供测试 / 诊断取底层连接。
func (c *headOrderConn) Unwrap() net.Conn { return c.Conn }

// Write 由 Transport 的写协程串行调用。
func (c *headOrderConn) Write(p []byte) (int, error) {
	if c.disabled {
		return c.Conn.Write(p)
	}
	n := len(p)
	for len(p) > 0 && !c.disabled {
		if c.inBody {
			k := c.consumeBody(p)
			if _, err := c.Conn.Write(p[:k]); err != nil {
				return 0, err
			}
			p = p[k:]
			continue
		}
		c.pending = append(c.pending, p...)
		p = nil
		i := bytes.Index(c.pending, []byte("\r\n\r\n"))
		if i < 0 {
			if len(c.pending) > maxPendingHead {
				return n, c.giveUp()
			}
			return n, nil
		}
		head := c.pending[:i+4]
		rest := append([]byte(nil), c.pending[i+4:]...)
		c.pending = nil
		if !c.startBody(head) {
			// 分帧判断不出：头照原样发出，之后整条连接透传。
			c.disabled = true
			if _, err := c.Conn.Write(head); err != nil {
				return 0, err
			}
			if _, err := c.Conn.Write(rest); err != nil {
				return 0, err
			}
			return n, nil
		}
		if _, err := c.Conn.Write(reorderHeadBlock(head)); err != nil {
			return 0, err
		}
		p = rest
	}
	if c.disabled && len(p) > 0 {
		if _, err := c.Conn.Write(p); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func (c *headOrderConn) giveUp() error {
	c.disabled = true
	out := c.pending
	c.pending = nil
	_, err := c.Conn.Write(out)
	return err
}

// startBody 解析请求头里的分帧；返回 false 表示无法判断。
func (c *headOrderConn) startBody(head []byte) bool {
	c.remain, c.chunked = 0, false
	for _, ln := range strings.Split(string(head), "\r\n")[1:] {
		colon := strings.IndexByte(ln, ':')
		if colon <= 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(ln[:colon]))
		val := strings.TrimSpace(ln[colon+1:])
		switch name {
		case "content-length":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil || n < 0 {
				return false
			}
			c.remain = n
		case "transfer-encoding":
			if !strings.EqualFold(val, "chunked") {
				return false
			}
			c.chunked = true
		}
	}
	c.chunk = chunkTracker{}
	c.inBody = c.chunked || c.remain > 0
	return true
}

// consumeBody 返回 p 中属于当前请求体的字节数，并在请求体结束时回到等头状态。
func (c *headOrderConn) consumeBody(p []byte) int {
	if c.chunked {
		k, done, ok := c.chunk.feed(p)
		if !ok {
			c.disabled = true
			return len(p)
		}
		if done {
			c.inBody = false
		}
		return k
	}
	if int64(len(p)) >= c.remain {
		k := int(c.remain)
		c.remain = 0
		c.inBody = false
		return k
	}
	c.remain -= int64(len(p))
	return len(p)
}

// chunkTracker 解析写侧 chunked 分帧（Go chunkedWriter 的规整输出），定位结束块。
type chunkTracker struct {
	state int    // 0 读大小行 1 读数据 2 读数据后 CRLF 3 读 trailer
	line  []byte // 跨 Write 的半行
	left  int64
}

// feed 返回消费字节数、请求体是否结束、是否可解析。
func (t *chunkTracker) feed(b []byte) (int, bool, bool) {
	i := 0
	for i < len(b) {
		switch t.state {
		case 0, 3:
			j := bytes.IndexByte(b[i:], '\n')
			if j < 0 {
				t.line = append(t.line, b[i:]...)
				if len(t.line) > 4096 {
					return len(b), false, false
				}
				return len(b), false, true
			}
			line := strings.TrimRight(string(append(t.line, b[i:i+j]...)), "\r")
			t.line = nil
			i += j + 1
			if t.state == 3 {
				if line == "" {
					t.state = 0
					return i, true, true
				}
				continue
			}
			if k := strings.IndexByte(line, ';'); k >= 0 {
				line = line[:k]
			}
			n, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
			if err != nil || n < 0 {
				return len(b), false, false
			}
			if n == 0 {
				t.state = 3
			} else {
				t.left, t.state = n, 1
			}
		case 1:
			if int64(len(b)-i) < t.left {
				t.left -= int64(len(b) - i)
				return len(b), false, true
			}
			i += int(t.left)
			t.left, t.state = 0, 2
		case 2:
			t.left = 0
			t.state = 4
			fallthrough
		case 4: // 跳过 CRLF（可能跨 Write）
			for i < len(b) && t.state == 4 {
				if b[i] == '\n' {
					t.state = 0
				} else if b[i] != '\r' {
					return len(b), false, false
				}
				i++
			}
		}
	}
	return i, false, true
}
