package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func encodeAs(t *testing.T, enc string, src []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	switch enc {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "br":
		w = brotli.NewWriter(&buf)
	case "deflate-zlib":
		w = zlib.NewWriter(&buf)
	case "deflate-raw":
		w, _ = flate.NewWriter(&buf, flate.DefaultCompression)
	}
	w.Write(src)
	w.Close()
	return buf.Bytes()
}

func TestDecodeResponseBody(t *testing.T) {
	src := []byte(`{"code":0,"data":{"ok":true}}`)
	for _, enc := range []string{"gzip", "br", "deflate-zlib", "deflate-raw"} {
		h := http.Header{}
		h.Set("Content-Encoding", strings.SplitN(enc, "-", 2)[0])
		got, err := decodeResponseBody(h, encodeAs(t, enc, src))
		if err != nil || !bytes.Equal(got, src) {
			t.Errorf("%s: got %q err %v", enc, got, err)
		}
	}
	if got, err := decodeResponseBody(http.Header{}, src); err != nil || !bytes.Equal(got, src) {
		t.Errorf("identity: %q %v", got, err)
	}
	h := http.Header{}
	h.Set("Content-Encoding", "compress")
	if _, err := decodeResponseBody(h, src); err == nil {
		t.Error("compress 应报不支持")
	}
}

// newWireClient 走生产 decodeTransport 的 client（压缩口径开）。
func newWireClient(base string) *Client {
	c := New()
	c.ChatBaseCN = base
	c.BillingBaseCN = base
	return c
}

// TestDoJSONDecodesBrotli400 400 + br：Classify 必须看到解压后的文本。
func TestDoJSONDecodesBrotli400(t *testing.T) {
	body := encodeAs(t, "br", []byte(`{"code":14018,"msg":"insufficient credit"}`))
	var gotAE []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = append(gotAE, r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(400)
		w.Write(body)
	}))
	defer srv.Close()
	for _, c := range []*Client{newWireClient(srv.URL), {HTTP: srv.Client(), BillingBaseCN: srv.URL}} {
		_, err := c.billingJSON(&auth.Auth{AccessToken: "at", UID: "u"}, http.MethodPost, "/x", nil)
		var ue *Error
		if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "insufficient credit") {
			t.Errorf("err=%v（压缩体未解压就进了 Classify）", err)
		}
	}
	if len(gotAE) != 2 || gotAE[0] != wireAcceptEncoding {
		t.Errorf("Accept-Encoding=%q", gotAE)
	}
}

// TestDoJSONBadGzipNotPenalized 声明 gzip 但体坏：返回传输层错误（非 *Error，不罚号）。
func TestDoJSONBadGzipNotPenalized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(400)
		w.Write([]byte("not gzip at all"))
	}))
	defer srv.Close()
	for _, c := range []*Client{newWireClient(srv.URL), {HTTP: srv.Client(), BillingBaseCN: srv.URL}} {
		_, err := c.billingJSON(&auth.Auth{AccessToken: "at", UID: "u"}, http.MethodPost, "/x", nil)
		var ue *Error
		if err == nil || errors.As(err, &ue) {
			t.Errorf("坏压缩体应返回非 *Error 的传输错误，got %v", err)
		}
	}
}

// TestChatStreamWireCompression chat：请求体 gzip + 四段 Accept-Encoding；
// 上游 br 压缩的 SSE 被流式解成明文。
func TestChatStreamWireCompression(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	var reqBody []byte
	var reqCE, reqAE string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCE, reqAE = r.Header.Get("Content-Encoding"), r.Header.Get("Accept-Encoding")
		zr, err := gzip.NewReader(r.Body)
		if err == nil {
			reqBody, _ = io.ReadAll(zr)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "br")
		bw := brotli.NewWriter(w)
		bw.Write([]byte(sse))
		bw.Close()
	}))
	defer srv.Close()
	c := newWireClient(srv.URL)
	big := `{"model":"m","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 2048) + `"}]}`
	rc, status, _, err := c.ChatStreamContext(nil, &auth.Auth{AccessToken: "at", UID: "u"}, []byte(big), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	defer rc.Close()
	out, _ := io.ReadAll(bufio.NewReader(rc))
	if string(out) != sse {
		t.Errorf("SSE 未解压: %q", out)
	}
	if reqCE != "gzip" || !bytes.Contains(reqBody, []byte(`"model"`)) {
		t.Errorf("请求体未 gzip: ce=%q body=%.40q", reqCE, reqBody)
	}
	if reqAE != wireAcceptEncoding {
		t.Errorf("Accept-Encoding=%q", reqAE)
	}
}

// TestWireCompressionOff 关闭后：小/大 body 都明文，不显式声明 Accept-Encoding。
func TestWireCompressionOff(t *testing.T) {
	var ce, ae string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ce, ae = r.Header.Get("Content-Encoding"), r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := newWireClient(srv.URL)
	c.WireCompression = false
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 2048) + `"}]}`
	rc, _, _, err := c.ChatStreamContext(nil, &auth.Auth{AccessToken: "at", UID: "u"}, []byte(big), "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if ce != "" || ae != "gzip" {
		t.Errorf("关闭后 ce=%q ae=%q（应为明文 + Go 默认 gzip）", ce, ae)
	}
}
