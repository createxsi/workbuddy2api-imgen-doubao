package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestDeepSeekFetchShape(t *testing.T) {
	var got string
	h := agentToolFake(t, "/agenttool/v1/webfetch", "application/json", `{"title":"T","content":"# hi"}`, &got)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch", strings.NewReader(`{"url":" https://a.example/x "}`)))
	if rec.Code != 200 || !strings.HasSuffix(got, "/agenttool/v1/webfetch") {
		t.Fatalf("code=%d path=%s body=%s", rec.Code, got, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["content"] != "# hi" || out["url"] != "https://a.example/x" || out["title"] != "T" {
		t.Fatalf("shape=%v", out)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch", strings.NewReader(`{"url":"ftp://a.example"}`)))
	if rec.Code != 400 {
		t.Fatalf("non-http url code=%d", rec.Code)
	}
}

func TestDeepSeekFetchMissingContentIs502(t *testing.T) {
	var got string
	h := agentToolFake(t, "/agenttool/v1/webfetch", "application/json", `{"url":"https://a.example","content":null}`, &got)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch", strings.NewReader(`{"url":"https://a.example"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// /v1/webfetch 保持原文透传，不做形状校验。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/webfetch", strings.NewReader(`{"url":"https://a.example"}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":null`) {
		t.Fatalf("webfetch code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func searchCaptureHandler(t *testing.T, upBody *string) *Handler {
	t.Helper()
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			*upBody = string(b)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"query":"q","results":[{"url":"https://a.example","title":null,"snippet":"s"}],"total_results":1}`))}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

func TestSearchDeepSeekCompat(t *testing.T) {
	var upBody string
	h := searchCaptureHandler(t, &upBody)
	req := httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"q","count":3}`))
	req.Header.Set("User-Agent", "deepseek-harness/0.2 (+https://example.invalid)")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(upBody, `"max_results":3`) {
		t.Fatalf("code=%d up=%s", rec.Code, upBody)
	}
	var out struct {
		Results []map[string]any `json:"results"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	it := out.Results[0]
	if d, has := it["date"]; !has || d != nil || it["title"] != "" || it["snippet"] != "s" {
		t.Fatalf("compat item=%v", it)
	}

	// 非 DSH 客户端：原文透传，不补 date。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"q","max_results":2,"count":7}`)))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"date"`) || !strings.Contains(upBody, `"max_results":2`) {
		t.Fatalf("code=%d up=%s body=%s", rec.Code, upBody, rec.Body.String())
	}
}
