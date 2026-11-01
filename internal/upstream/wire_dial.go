// wire_dial.go 接管出站 TLS 拨号：TCP（直连 / HTTP(S) 代理 CONNECT / SOCKS5）→
// ClientHello（node 复刻官方 Node/OpenSSL，或 go 原生）→ 头序重排包装。
//
// 为什么接管整条 TLS 拨号：http.Transport 在 DialContext 之上自己做 TLS，写进 conn 的
// 是密文，改不了头序；只有 DialTLSContext 返回已握手的 conn，上层写入的才是明文。
// 代价：Transport 走 HTTP(S)_PROXY 时会绕开 DialTLSContext（自己 CONNECT 后用
// crypto/tls 握手），所以代理也在这里自己做，Transport.Proxy 只对明文 http 目标生效。
package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	utls "github.com/refraction-networking/utls"
)

// envProxyFor 目标 addr（host:port，https）对应的环境变量代理；nil = 直连。测试可替换。
var envProxyFor = func(addr string) (*url.URL, error) {
	return http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
}

// plainHTTPProxy Transport.Proxy：只给明文 http 目标走环境变量代理；https 目标的代理
// 由 newTLSDialer 自己处理（否则 Transport 会绕开 DialTLSContext）。
func plainHTTPProxy(req *http.Request) (*url.URL, error) {
	if req.URL != nil && req.URL.Scheme == "https" {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}

// newTLSDialer 构造 DialTLSContext。cfg 仅测试注入（InsecureSkipVerify / ServerName），
// 生产恒 nil。fp 见 tlsfingerprint.go；shape=true 时包 headOrderConn 重排头序。
func newTLSDialer(d *net.Dialer, cfg *tls.Config, fp string, shape bool) func(context.Context, string, string) (net.Conn, error) {
	insecure := cfg != nil && cfg.InsecureSkipVerify
	sniOverride := ""
	if cfg != nil {
		sniOverride = cfg.ServerName
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		hctx, cancel := context.WithTimeout(ctx, tlsHandshakeTimeout)
		defer cancel()
		raw, err := dialViaProxy(hctx, d, network, addr)
		if err != nil {
			return nil, err
		}
		host, _, serr := net.SplitHostPort(addr)
		if serr != nil {
			host = addr
		}
		if sniOverride != "" {
			host = sniOverride
		}
		var conn net.Conn
		if useNodeFingerprint(fp) {
			// ALPN 留空：官方 Node 栈不带 ALPN，也与禁 h2 口径一致。
			uc := utls.UClient(raw, &utls.Config{
				ServerName:         host,
				InsecureSkipVerify: insecure,
				MinVersion:         tls.VersionTLS12,
			}, utls.HelloCustom)
			if perr := uc.ApplyPreset(officialHelloSpec()); perr != nil {
				raw.Close()
				return nil, fmt.Errorf("tls fingerprint preset: %w", perr)
			}
			if herr := uc.HandshakeContext(hctx); herr != nil {
				raw.Close()
				return nil, herr
			}
			conn = uc
		} else {
			tc0 := &tls.Config{MinVersion: tls.VersionTLS12}
			if cfg != nil {
				tc0 = cfg.Clone()
			}
			tc0.ServerName = host
			tc0.NextProtos = nil
			tc := tls.Client(raw, tc0)
			if herr := tc.HandshakeContext(hctx); herr != nil {
				raw.Close()
				return nil, herr
			}
			conn = tc
		}
		if shape {
			return newHeadOrderConn(conn), nil
		}
		return conn, nil
	}
}

// dialViaProxy 按环境变量代理建到 addr 的 TCP 隧道：无代理直连；http/https 代理走
// CONNECT；socks5/socks5h 走 SOCKS5 CONNECT（域名交给代理解析）。
func dialViaProxy(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
	pu, err := envProxyFor(addr)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	if pu == nil {
		return d.DialContext(ctx, network, addr)
	}
	switch pu.Scheme {
	case "http", "https":
		return dialConnect(ctx, d, pu, addr)
	case "socks5", "socks5h":
		return dialSOCKS5(ctx, d, pu, addr)
	}
	return nil, fmt.Errorf("proxy: 不支持的代理协议 %q", pu.Scheme)
}

func proxyAddr(pu *url.URL, defPort string) string {
	if pu.Port() != "" {
		return pu.Host
	}
	return net.JoinHostPort(pu.Hostname(), defPort)
}

// dialConnect HTTP CONNECT 隧道（https 代理先与代理本身做标准 TLS）。
func dialConnect(ctx context.Context, d *net.Dialer, pu *url.URL, addr string) (net.Conn, error) {
	def := "80"
	if pu.Scheme == "https" {
		def = "443"
	}
	c, err := d.DialContext(ctx, "tcp", proxyAddr(pu, def))
	if err != nil {
		return nil, err
	}
	if pu.Scheme == "https" {
		tc := tls.Client(c, &tls.Config{ServerName: pu.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, err
		}
		c = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
		defer c.SetDeadline(timeZero)
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if pu.User != nil {
		pw, _ := pu.User.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(pu.User.Username()+":"+pw)) + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("proxy CONNECT: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, fmt.Errorf("proxy CONNECT: %s", resp.Status)
	}
	if br.Buffered() > 0 {
		c.Close()
		return nil, errors.New("proxy CONNECT: 代理在隧道建立前多发了数据")
	}
	return c, nil
}

// dialSOCKS5 最小 SOCKS5 CONNECT（无认证 / 用户名密码），目标以域名交给代理。
func dialSOCKS5(ctx context.Context, d *net.Dialer, pu *url.URL, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || len(host) > 255 {
		return nil, fmt.Errorf("socks5: 目标地址无效 %q", addr)
	}
	c, err := d.DialContext(ctx, "tcp", proxyAddr(pu, "1080"))
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
		defer c.SetDeadline(timeZero)
	}
	fail := func(e error) (net.Conn, error) { c.Close(); return nil, fmt.Errorf("socks5: %w", e) }
	methods := []byte{0x00}
	if pu.User != nil {
		methods = []byte{0x00, 0x02}
	}
	if _, err := c.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return fail(err)
	}
	var rep [2]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		return fail(err)
	}
	switch rep[1] {
	case 0x00:
	case 0x02:
		if pu.User == nil {
			return fail(errors.New("代理要求认证"))
		}
		u := pu.User.Username()
		pw, _ := pu.User.Password()
		if len(u) > 255 || len(pw) > 255 {
			return fail(errors.New("用户名或密码过长"))
		}
		msg := append([]byte{0x01, byte(len(u))}, u...)
		msg = append(append(msg, byte(len(pw))), pw...)
		if _, err := c.Write(msg); err != nil {
			return fail(err)
		}
		if _, err := io.ReadFull(c, rep[:]); err != nil {
			return fail(err)
		}
		if rep[1] != 0x00 {
			return fail(errors.New("认证失败"))
		}
	default:
		return fail(errors.New("代理不接受可用的认证方式"))
	}
	msg := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	msg = binary.BigEndian.AppendUint16(msg, uint16(port))
	if _, err := c.Write(msg); err != nil {
		return fail(err)
	}
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return fail(err)
	}
	if head[1] != 0x00 {
		return fail(fmt.Errorf("CONNECT 被拒（rep=%d）", head[1]))
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return fail(err)
		}
		skip = int(l[0])
	default:
		return fail(errors.New("回包地址类型无效"))
	}
	if _, err := io.CopyN(io.Discard, c, int64(skip+2)); err != nil {
		return fail(err)
	}
	return c, nil
}
