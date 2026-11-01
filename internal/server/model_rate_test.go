package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 普通 429 只停当前模型：同号换模型仍可用；全池该模型被停时回 429 + Retry-After。
func TestSoftRateIsolatedByModel(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":1,"msg":"rate limit"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	st, _ := p.Status("u1")
	if st.Cooling || modelCoolRemaining(st, "glm-5.2") <= 0 {
		t.Fatalf("应只冷却 glm-5.2: %+v", st)
	}
	if p.PickExcludingForRealm(nil, "glm-5.3", "") == nil {
		t.Fatal("同号其他模型应仍可选")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("code=%d ra=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
}
