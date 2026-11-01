package panel

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// 一个响应都没拿到（连接被掐断）时重试；拿到响应（含 4xx）不重试。
func TestLoginDoJSONRetriesTransportOnly(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"ok":true}}`))
	}))
	defer srv.Close()
	data, status, err := doJSON(http.MethodPost, srv.URL, "", []byte("{}"), "https://example.invalid")
	if err != nil || status != 200 || string(data) != `{"ok":true}` {
		t.Fatalf("want retry success, got %s %d %v", data, status, err)
	}
	if atomic.LoadInt32(&n) != 2 {
		t.Fatalf("attempts=%d want 2", n)
	}

	var m int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv2.Close()
	if _, status, err := doJSON(http.MethodGet, srv2.URL, "", nil, "https://example.invalid"); err == nil || status != 502 {
		t.Fatalf("want 502 error, got %d %v", status, err)
	}
	if atomic.LoadInt32(&m) != 1 {
		t.Fatalf("4xx/5xx must not retry, attempts=%d", m)
	}
}
