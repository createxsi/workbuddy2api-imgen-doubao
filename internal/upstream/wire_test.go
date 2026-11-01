package upstream

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// rawTLSServer 记录每个请求的原始头块与 body，按连接计数；回 200 ok（keep-alive）。
type rawTLSServer struct {
	ln     net.Listener
	mu     sync.Mutex
	heads  []string
	bodies []string
	conns  int
}

func newRawTLSServer(t *testing.T) *rawTLSServer {
	t.Helper()
	cert := httptest.NewTLSServer(http.NotFoundHandler())
	cfg := cert.TLS.Clone()
	cert.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &rawTLSServer{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *rawTLSServer) serve(c net.Conn) {
	defer c.Close()
	var raw bytes.Buffer
	br := bufio.NewReader(io.TeeReader(c, &raw))
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		body, _ := io.ReadAll(req.Body)
		head := raw.String()
		if i := strings.Index(head, "\r\n\r\n"); i >= 0 {
			head = head[:i+4]
		}
		// raw 里可能已有下一请求的预读字节：只保留本请求之后的部分。
		consumed := raw.Len() - br.Buffered()
		rest := append([]byte(nil), raw.Bytes()[consumed:]...)
		raw.Reset()
		raw.Write(rest)
		s.mu.Lock()
		s.heads = append(s.heads, head)
		s.bodies = append(s.bodies, string(body))
		s.mu.Unlock()
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 11\r\n\r\n{\"code\":0}\n")
	}
}

func (s *rawTLSServer) url() string { return "https://" + s.ln.Addr().String() }

func testWireClient(shape, perAccount bool) *Client {
	d := newDialer()
	base := newTransportWith(d, TLSFingerprintNode, shape)
	base.DialTLSContext = newTLSDialer(d, &tls.Config{InsecureSkipVerify: true, ServerName: "www.workbuddy.cn"}, TLSFingerprintNode, shape)
	wt := newWireTransport(base, shape, perAccount)
	return &Client{HTTP: &http.Client{Transport: wt, Timeout: 10 * time.Second}, ChatHTTP: &http.Client{Transport: wt}, WireCompression: true}
}

func headerNames(head string) []string {
	var out []string
	for _, ln := range strings.Split(strings.TrimSuffix(head, "\r\n\r\n"), "\r\n")[1:] {
		if i := strings.IndexByte(ln, ':'); i > 0 {
			out = append(out, ln[:i])
		}
	}
	return out
}

// assertOrder names 中出现在 order 里的头必须按 order 的相对顺序出现。
func assertOrder(t *testing.T, names, order []string) {
	t.Helper()
	rank := map[string]int{}
	for i, k := range order {
		rank[strings.ToLower(k)] = i
	}
	last := -1
	for _, n := range names {
		r, ok := rank[strings.ToLower(n)]
		if !ok {
			continue
		}
		if r < last {
			t.Errorf("头序不符：%s 出现在前序头之后；实际 %v", n, names)
			return
		}
		last = r
	}
}

// TestWireHeadOrderE2E 同一连接上连发 chat(Content-Length) / v3 config / chunked report：
// 头序、头名拼写按官方，body 原样，连接复用后状态不错位。
func TestWireHeadOrderE2E(t *testing.T) {
	srv := newRawTLSServer(t)
	c := testWireClient(true, false)
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	chatBody := `{"model":"m","messages":[{"role":"user","content":"hi\r\n\r\nthere"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.url()+"/v2/chat/completions", strings.NewReader(chatBody))
	c.ChatHeaders(req, a, "", ChatMeta{})
	req.Header.Set("X-Enterprise-Id", "e1") // 表外头
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	req2, _ := http.NewRequest(http.MethodPost, srv.url()+"/v2/report", io.NopCloser(strings.NewReader(`[{"a":1}]`)))
	req2.ContentLength = -1 // 强制 chunked
	c.setDesktopReportHeaders(req2, a, "www.workbuddy.cn")
	resp, err = c.HTTP.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	req3, _ := http.NewRequest(http.MethodPost, srv.url()+"/v2/chat/completions", strings.NewReader(chatBody))
	c.ChatHeaders(req3, a, "", ChatMeta{})
	resp, err = c.HTTP.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.heads) != 3 || srv.conns != 1 {
		t.Fatalf("heads=%d conns=%d（应 3 个请求复用 1 条连接）", len(srv.heads), srv.conns)
	}
	if srv.bodies[0] != chatBody || srv.bodies[1] != `[{"a":1}]` || srv.bodies[2] != chatBody {
		t.Fatalf("body 被改动: %q", srv.bodies)
	}
	for i, order := range [][]string{chatHeadOrder, officialHeadOrder["/v2/report"], chatHeadOrder} {
		names := headerNames(srv.heads[i])
		assertOrder(t, names, order)
		if i != 1 && !strings.Contains(srv.heads[i], "\r\nx-stainless-lang: js\r\n") {
			t.Errorf("头名拼写未对齐：%s", srv.heads[i])
		}
		if i != 1 && !strings.Contains(srv.heads[i], "\r\nX-Conversation-ID: ") {
			t.Errorf("X-Conversation-ID 拼写未对齐")
		}
		if !strings.Contains(srv.heads[i], "\r\nConnection: keep-alive\r\n") {
			t.Errorf("缺 Connection: keep-alive: %s", srv.heads[i])
		}
	}
	// 表外头在传输层尾段之前。
	names := headerNames(srv.heads[0])
	pos := map[string]int{}
	for i, n := range names {
		pos[strings.ToLower(n)] = i
	}
	if pos["x-enterprise-id"] > pos["content-length"] {
		t.Errorf("表外头应在 Content-Length 之前: %v", names)
	}
}

// TestWireShapeOff header_shape=false：保持 Go 原生头序与拼写。
func TestWireShapeOff(t *testing.T) {
	srv := newRawTLSServer(t)
	c := testWireClient(false, false)
	req, _ := http.NewRequest(http.MethodPost, srv.url()+"/v2/chat/completions", strings.NewReader("{}"))
	c.ChatHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1"}, "", ChatMeta{})
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if strings.Contains(srv.heads[0], "x-stainless-lang") || strings.Contains(srv.heads[0], "Connection: keep-alive") {
		t.Errorf("关闭形态后不应改写: %s", srv.heads[0])
	}
}

// TestWireConnPerAccount 不同账号不共用连接，同账号复用。
func TestWireConnPerAccount(t *testing.T) {
	srv := newRawTLSServer(t)
	c := testWireClient(true, true)
	for _, uid := range []string{"u1", "u2", "u1", "u2"} {
		req, _ := http.NewRequest(http.MethodGet, srv.url()+"/v3/x", nil)
		req.Header.Set("X-User-Id", uid)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.conns != 2 {
		t.Errorf("conns=%d want 2（每账号一条，复用）", srv.conns)
	}
}

// TestWireProxyConnect https 目标经 HTTP CONNECT 代理，仍走自管 TLS 与头序。
func TestWireProxyConnect(t *testing.T) {
	srv := newRawTLSServer(t)
	var connected string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") == "" {
			http.Error(w, "bad", 400)
			return
		}
		connected = r.Host
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		hj, _ := w.(http.Hijacker)
		cc, bw, _ := hj.Hijack()
		bw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		bw.Flush()
		go io.Copy(up, cc)
		io.Copy(cc, up)
		cc.Close()
		up.Close()
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	pu.User = url.UserPassword("u", "p")
	old := envProxyFor
	envProxyFor = func(string) (*url.URL, error) { return pu, nil }
	defer func() { envProxyFor = old }()

	c := testWireClient(true, false)
	req, _ := http.NewRequest(http.MethodGet, srv.url()+"/v3/config", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if connected != srv.ln.Addr().String() {
		t.Errorf("未经代理 CONNECT: %q", connected)
	}
}

// TestWireSOCKS5 https 目标经 SOCKS5（用户名密码）代理。
func TestWireSOCKS5(t *testing.T) {
	srv := newRawTLSServer(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	var target string
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 512)
		io.ReadFull(c, buf[:2])
		io.ReadFull(c, buf[:buf[1]])
		c.Write([]byte{5, 2})
		io.ReadFull(c, buf[:2])
		io.ReadFull(c, buf[:buf[1]])
		io.ReadFull(c, buf[:1])
		io.ReadFull(c, buf[:buf[0]])
		c.Write([]byte{1, 0})
		io.ReadFull(c, buf[:5])
		n := int(buf[4])
		io.ReadFull(c, buf[:n+2])
		target = string(buf[:n])
		up, err := net.Dial("tcp", srv.ln.Addr().String())
		if err != nil {
			return
		}
		defer up.Close()
		c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		go io.Copy(up, c)
		io.Copy(c, up)
	}()
	pu, _ := url.Parse("socks5://u:p@" + ln.Addr().String())
	old := envProxyFor
	envProxyFor = func(string) (*url.URL, error) { return pu, nil }
	defer func() { envProxyFor = old }()

	c := testWireClient(true, false)
	req, _ := http.NewRequest(http.MethodGet, srv.url()+"/v3/config", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if target != "127.0.0.1" {
		t.Errorf("socks5 目标=%q", target)
	}
}

func TestReorderHeadBlockUnknownPathUntouched(t *testing.T) {
	blk := []byte("POST /v2/billing/x HTTP/1.1\r\nB: 1\r\nA: 2\r\n\r\n")
	if got := reorderHeadBlock(blk); !bytes.Equal(got, blk) {
		t.Errorf("无证据路径不应重排: %q", got)
	}
	img := []byte("POST /v2/images/generations HTTP/1.1\r\nHost: h\r\nUser-Agent: u\r\nAccept: a\r\n\r\n")
	if got := string(reorderHeadBlock(img)); !strings.HasPrefix(got, "POST /v2/images/generations HTTP/1.1\r\nAccept: a\r\nUser-Agent: u\r\nHost: h\r\n") {
		t.Errorf("生图路径应按 chat 序: %q", got)
	}
}

func TestChunkTrackerSplitWrites(t *testing.T) {
	stream := []byte("5\r\nhello\r\n3;x=1\r\nabc\r\n0\r\n\r\n")
	for split := 1; split < len(stream); split++ {
		var tr chunkTracker
		k1, done1, ok1 := tr.feed(stream[:split])
		if !ok1 || k1 != split {
			t.Fatalf("split=%d 第一段 k=%d ok=%v", split, k1, ok1)
		}
		if done1 {
			t.Fatalf("split=%d 过早结束", split)
		}
		k2, done2, ok2 := tr.feed(stream[split:])
		if !ok2 || !done2 || k2 != len(stream)-split {
			t.Fatalf("split=%d 第二段 k=%d done=%v ok=%v", split, k2, done2, ok2)
		}
	}
}
