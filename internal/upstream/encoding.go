// encoding.go 出站压缩口径（桌面端真机）：
//   - 全链路声明 Accept-Encoding: gzip, compress, deflate, br；
//   - chat 请求体 ≥1KB 时 gzip（Content-Encoding: gzip）。
//
// 显式声明 Accept-Encoding 会关掉 net/http 的透明解压，所以必须自己解：
// decodeTransport 在传输层按 Content-Encoding 流式解压（SSE 长流同样适用），并
// 剥掉 Content-Encoding / Content-Length，调用方看到的永远是明文；doJSON 另有
// decodeResponseBody 兜底（注入的测试 client 不经 decodeTransport）。
// 解压失败表现为读错误（非 *Error），不进 Classify，不罚号。
//
// 开关：Client.WireCompression（config upstream.wire_compression，缺省开）；关闭后
// 回到 Go 默认（自动 gzip 声明 + 明文请求体）。
package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

// wireAcceptEncoding 真机全链路 Accept-Encoding 取值。
const wireAcceptEncoding = "gzip, compress, deflate, br"

// gzipMinChatBody chat 请求体触发 gzip 的最小字节数（极小 body 压了反而更长）。
const gzipMinChatBody = 1024

// setAcceptEncoding 按开关声明真机 Accept-Encoding；关闭时不设（交回 net/http）。
func (c *Client) setAcceptEncoding(req *http.Request) {
	if c != nil && c.WireCompression {
		req.Header.Set("Accept-Encoding", wireAcceptEncoding)
	} else {
		req.Header.Del("Accept-Encoding")
	}
}

// gzipBody BestSpeed 压缩出站 body。
func gzipBody(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(src); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodingChain 解析 Content-Encoding；全部可解时返回逆序（后施加的先解）编码表，
// 含不支持的编码（如 compress）时 ok=false。
func encodingChain(h http.Header) (chain []string, ok bool) {
	enc := strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return nil, true
	}
	parts := strings.Split(enc, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		switch e := strings.TrimSpace(parts[i]); e {
		case "", "identity":
		case "gzip", "x-gzip", "deflate", "br":
			chain = append(chain, e)
		default:
			return nil, false
		}
	}
	return chain, true
}

// decoderFor 为单个编码构造流式解码器。deflate 兼容 zlib 包装与裸 deflate。
func decoderFor(enc string, r io.Reader) (io.Reader, error) {
	switch enc {
	case "gzip", "x-gzip":
		return gzip.NewReader(r)
	case "br":
		return brotli.NewReader(r), nil
	case "deflate":
		br := bufio.NewReader(r)
		head, err := br.Peek(2)
		if err == nil && len(head) == 2 && head[0]&0x0f == 8 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
			return zlib.NewReader(br)
		}
		return flate.NewReader(br), nil
	}
	return nil, fmt.Errorf("不支持的内容编码 %q", enc)
}

// decodeResponseBody 按 Content-Encoding 解压已读入的响应体（doJSON 兜底用）。
func decodeResponseBody(h http.Header, raw []byte) ([]byte, error) {
	chain, ok := encodingChain(h)
	if !ok {
		return nil, fmt.Errorf("不支持的内容编码 %q", h.Get("Content-Encoding"))
	}
	for _, e := range chain {
		r, err := decoderFor(e, bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("解压 %s 失败: %w", e, err)
		}
		if raw, err = io.ReadAll(r); err != nil {
			return nil, fmt.Errorf("解压 %s 失败: %w", e, err)
		}
	}
	return raw, nil
}

// decodeTransport 包一层 *http.Transport：请求显式声明了 Accept-Encoding 时，按响应
// Content-Encoding 流式解压并剥掉编码头。未声明（Go 自己加 gzip）时 net/http 已
// 透明解压，原样返回。
type decodeTransport struct {
	base *http.Transport
}

func (t *decodeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || req.Header.Get("Accept-Encoding") == "" || req.Method == http.MethodHead {
		return resp, err
	}
	chain, ok := encodingChain(resp.Header)
	if !ok || len(chain) == 0 {
		return resp, nil
	}
	resp.Body = &lazyDecodeBody{src: resp.Body, chain: chain}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

// CloseIdleConnections 透传给底层 Transport（roundTripCloseIdle 依赖）。
func (t *decodeTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

// HTTPTransport 取出 RoundTripper 底层的 *http.Transport（含 decodeTransport 包装）。
func HTTPTransport(rt http.RoundTripper) (*http.Transport, bool) {
	switch v := rt.(type) {
	case *http.Transport:
		return v, true
	case *decodeTransport:
		return v.base, true
	}
	return nil, false
}

// lazyDecodeBody 首次 Read 才建解码器（gzip.NewReader 会读头，构造期不应阻塞）。
type lazyDecodeBody struct {
	src   io.ReadCloser
	chain []string
	r     io.Reader
	err   error
}

func (b *lazyDecodeBody) Read(p []byte) (int, error) {
	if b.r == nil && b.err == nil {
		var r io.Reader = b.src
		for _, e := range b.chain {
			d, err := decoderFor(e, r)
			if err != nil {
				b.err = fmt.Errorf("解压 %s 失败: %w", e, err)
				break
			}
			r = d
		}
		b.r = r
	}
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		err = fmt.Errorf("解压响应失败: %w", err)
	}
	return n, err
}

func (b *lazyDecodeBody) Close() error { return b.src.Close() }
