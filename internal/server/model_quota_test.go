package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 全池该模型都是 6004 用量配额锁：429 + 数字业务码 + 上游原文，不发 Retry-After。
func TestChatModelQuotaEverywhereReturns429NoRetryAfter(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	body := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-10-26 10:00:00 UTC+8 重置"}`
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", body)

	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After=%q，配额锁不该让客户端按头空等", got)
	}
	var resp struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v %s", err, rec.Body)
	}
	if resp.Error.Code != 6004 || !strings.Contains(resp.Error.Message, "频率限制") {
		t.Errorf("error=%+v want code 6004 + 上游原文", resp.Error)
	}
}

// 预刷新时 refresh 回 12153 但 AT 仍有效：不禁用，直接用现有 AT 发请求。
func TestChatPreRefreshDeadButATUsable(t *testing.T) {
	var chatCalls int
	up := fakeChatRefresh(200, "", 401, `{"code":12153,"msg":"Offline user session not found"}`)
	base := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "refresh") {
			chatCalls++
		}
		return base.RoundTrip(r)
	})
	// 剩 1 分钟到期：落在预刷新窗口内，但 AT 尚未过期。
	a := &auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: time.Now().Add(time.Minute).Unix(),
		FilePath: filepath.Join(t.TempDir(), "u1.json")}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: up, RefreshSkew: 5 * time.Minute})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("AT still valid, must not disable: %+v", st)
	}
	if chatCalls == 0 {
		t.Fatal("expected chat request with existing AT")
	}
}
