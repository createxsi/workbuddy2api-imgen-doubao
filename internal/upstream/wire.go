// wire.go 出站传输出口：所有经 Client.HTTP / ChatHTTP 的请求都过 wireTransport。
//   - shape：头名拼写（headcase.go）与 Connection 头按官方真机补齐；头序在连接层
//     （headorder_conn.go）重排；
//   - perAccount：连接池按账号隔离（X-User-Id），同出口两个账号不共用 TCP/TLS 连接；
//   - 响应按 Content-Encoding 流式解压（encoding.go）。
package upstream

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

var timeZero time.Time

// 出站形态开关默认值（config upstream.tls_fingerprint / header_shape / conn_per_account）。
const (
	defaultHeaderShape    = true
	defaultConnPerAccount = true
)

type wireTransport struct {
	base       *http.Transport
	shape      bool
	perAccount bool

	mu    sync.Mutex
	pools map[string]*http.Transport
}

func newWireTransport(base *http.Transport, shape, perAccount bool) *wireTransport {
	return &wireTransport{base: base, shape: shape, perAccount: perAccount}
}

func (t *wireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.shape {
		req = normalizeHeaderSpelling(req)
		applyConnectionHeader(req)
	}
	resp, err := t.pick(req).RoundTrip(req)
	if err != nil {
		return nil, err
	}
	decodeResponse(req, resp)
	return resp, nil
}

// pick 按账号取连接池：带 X-User-Id 的请求各用一份 base 的克隆（独立空闲池），
// 其余（登录、无账号请求）共用 base。
func (t *wireTransport) pick(req *http.Request) *http.Transport {
	if !t.perAccount {
		return t.base
	}
	uid := req.Header.Get("X-User-Id")
	if uid == "" {
		return t.base
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr, ok := t.pools[uid]; ok {
		return tr
	}
	if t.pools == nil {
		t.pools = map[string]*http.Transport{}
	}
	tr := t.base.Clone()
	tr.MaxIdleConnsPerHost = perAccountIdleConns
	t.pools[uid] = tr
	return tr
}

// perAccountIdleConns 每账号每主机空闲连接上限（单号并发有限，不需要 base 那么大）。
const perAccountIdleConns = 4

// CloseIdleConnections 清空 base 与所有账号池的空闲连接（roundTripCloseIdle 依赖）。
func (t *wireTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.pools {
		tr.CloseIdleConnections()
	}
}

// setResponseHeaderTimeout 同步改 base 与已建账号池。
func (t *wireTransport) setResponseHeaderTimeout(d time.Duration) {
	t.base.ResponseHeaderTimeout = d
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.pools {
		tr.ResponseHeaderTimeout = d
	}
}

// HTTPTransport 取出 RoundTripper 底层的 *http.Transport（含 wireTransport 包装）。
func HTTPTransport(rt http.RoundTripper) (*http.Transport, bool) {
	switch v := rt.(type) {
	case *http.Transport:
		return v, true
	case *wireTransport:
		return v.base, true
	}
	return nil, false
}

// SetHeaderTimeout 设置聊天 SSE 首字节前上限（作用于 ChatHTTP 的全部连接池）。
func (c *Client) SetHeaderTimeout(d time.Duration) {
	c.HeaderTimeout = d
	switch v := c.chatHTTP().Transport.(type) {
	case *wireTransport:
		v.setResponseHeaderTimeout(d)
	case *http.Transport:
		v.ResponseHeaderTimeout = d
	}
}

// officialConnectionValue 官方在该路径显式发送的 Connection 值（真机夹具）；
// 没抓到过的路径返回空，保持 Go 默认。
func officialConnectionValue(path string) string {
	switch path {
	case "/v3/config":
		return "close"
	case "/v2/chat/completions", "/v2/chat/busy", "/v2/report", "/v2/plugin/auth/token/refresh":
		return "keep-alive"
	}
	// 本仓独有、与 chat 同族（同域同头族）的链路：生图与 agenttool 检索 / 抓取，
	// 与 chat 同发 keep-alive（由逐路径对拍门禁守护，见 official_parity_test.go）。
	if strings.HasPrefix(path, "/v2/images/") || strings.HasPrefix(path, "/agenttool/") {
		return "keep-alive"
	}
	return ""
}

// applyConnectionHeader close 走 req.Close（Go 自己写，手写会重复）；keep-alive 是
// HTTP/1.1 默认值 Go 从不发，必须手写。
func applyConnectionHeader(req *http.Request) {
	switch officialConnectionValue(req.URL.Path) {
	case "close":
		req.Close = true
	case "keep-alive":
		req.Header.Set("Connection", "keep-alive")
	}
}

// WireOptions 出站形态配置。
type WireOptions struct {
	TLSFingerprint string // node（缺省）/ go
	HeaderShape    bool   // 头名拼写 / 头序 / Connection 对齐官方
	ConnPerAccount bool   // 连接池按账号隔离
}

// ConfigureWire 按配置重建出站传输（装配期调用，需在发出任何请求之前）。
func (c *Client) ConfigureWire(o WireOptions) error {
	fp, err := validateTLSFingerprint(o.TLSFingerprint)
	if err != nil {
		return err
	}
	c.TLSFingerprint = fp
	c.headerShape = o.HeaderShape
	c.connPerAccount = o.ConnPerAccount
	c.rebuildTransport()
	return nil
}

// rebuildTransport 用当前拨号器与形态开关重建 HTTP / ChatHTTP 共用的 wireTransport，
// 保留既有超时设置。
func (c *Client) rebuildTransport() {
	base := newTransportWith(c.dialer, c.TLSFingerprint, c.headerShape)
	if old, ok := HTTPTransport(c.chatHTTP().Transport); ok {
		base.ResponseHeaderTimeout = old.ResponseHeaderTimeout
	}
	wt := newWireTransport(base, c.headerShape, c.connPerAccount)
	if c.HTTP != nil {
		c.HTTP.Transport = wt
	}
	if c.ChatHTTP != nil {
		c.ChatHTTP.Transport = wt
	}
}

// LoginTransport 设备码登录专用传输：与出站同一 ClientHello / 头形态（含环境变量代理），
// 但用更短的分段超时，且不按账号分池。
func (c *Client) LoginTransport() http.RoundTripper {
	base := newTransportWith(c.dialer, c.TLSFingerprint, c.headerShape)
	base.TLSHandshakeTimeout = 8 * time.Second
	base.ResponseHeaderTimeout = 15 * time.Second
	base.MaxIdleConns = 16
	base.MaxIdleConnsPerHost = 8
	base.IdleConnTimeout = 60 * time.Second
	return newWireTransport(base, c.headerShape, false)
}
