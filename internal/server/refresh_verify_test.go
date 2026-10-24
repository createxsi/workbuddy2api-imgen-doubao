package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeChatRefresh chat 恒回 chatStatus/chatBody，refresh 端点回 refStatus/refBody。
func fakeChatRefresh(chatStatus int, chatBody string, refStatus int, refBody string) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body, ct := chatStatus, chatBody, "application/json"
			if strings.Contains(r.URL.Path, "refresh") {
				status, body = refStatus, refBody
			}
			if !strings.HasPrefix(body, "{") {
				ct = "text/html"
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ct}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

const gatewayHTML = `<html><head><title>401 Authorization Required</title></head><body>openresty</body></html>`

func runChat(t *testing.T, up *upstream.Client) (*auth.Auth, func() (disabled, cooling bool)) {
	t.Helper()
	a := &auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999,
		FilePath: filepath.Join(t.TempDir(), "u1.json")}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	return a, func() (bool, bool) { st, _ := p.Status("u1"); return st.Disabled, st.Cooling }
}

// 12153 时 refresh 端点本身也被网关拦截：无判词，不得禁用。
func TestSessionDeadRefreshInconclusiveNotDisabled(t *testing.T) {
	_, st := runChat(t, fakeChatRefresh(401, `{"code":12153,"msg":"Offline user session not found"}`, 401, gatewayHTML))
	if d, c := st(); d || !c {
		t.Fatalf("disabled=%v cooling=%v, want soft cooldown only", d, c)
	}
}

// 12153 且 refresh 被应用层明确拒绝：禁用。
func TestSessionDeadRefreshRejectedDisables(t *testing.T) {
	_, st := runChat(t, fakeChatRefresh(401, `{"code":12153,"msg":"Offline user session not found"}`, 400, `{"code":11001,"msg":"refresh token is invalid"}`))
	if d, _ := st(); !d {
		t.Fatal("refresh rejected, account should be disabled")
	}
}

// 网关 401（HTML 页）但 refresh 成功：换新 token、软冷却，不禁用。
func TestGateway401RefreshOK(t *testing.T) {
	a, st := runChat(t, fakeChatRefresh(401, gatewayHTML, 200, `{"code":0,"data":{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}}`))
	if d, c := st(); d || !c {
		t.Fatalf("disabled=%v cooling=%v, want soft cooldown", d, c)
	}
	if a.AccessTokenValue() != "at2" {
		t.Errorf("access token = %q, want at2", a.AccessTokenValue())
	}
}

func TestRefreshRejected(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&upstream.Error{Status: 400, Msg: `code=11001 msg=invalid`}, true},
		{&upstream.Error{Status: 401, Msg: `{"code":12153,"msg":"x"}`}, true},
		{&upstream.Error{Status: 401, Msg: gatewayHTML}, false},
		{&upstream.Error{Status: 403, Msg: gatewayHTML}, false},
		{&upstream.Error{Status: 429, Msg: `{"code":1}`}, false},
		{&upstream.Error{Status: 502, Msg: `bad gateway`}, false},
		{io.ErrUnexpectedEOF, false},
	}
	for i, c := range cases {
		if got := upstream.RefreshRejected(c.err); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

// 6020 排队：503 + Retry-After，不罚号。
func TestChatUpstreamBusyNoPenalty(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":6020,"msg":"queue.waiting.title"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("code=%d retry-after=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if st, _ := p.Status("u1"); st.Cooling || st.Disabled {
		t.Fatalf("6020 must not penalize account: %+v", st)
	}
}
