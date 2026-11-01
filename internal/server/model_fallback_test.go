package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	fbFrom     = "deepseek-v4.1-flash"
	fbTo       = "deepseek-v4-flash"
	body6004   = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2099-01-01 10:00:00 UTC+8 重置"}`
	body14003  = `{"code":14003,"msg":"模型繁忙"}`
	body6000Sw = `{"code":6000,"msg":"该模型用量已达上限，您也可以切换其他模型继续使用"}`
)

// modelAwareUpstream 按出站 body 的 model 决定响应，并按顺序记下上游实际收到的模型。
func modelAwareUpstream(t *testing.T, behavior func(model string) (int, string)) (*upstream.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	c := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var peek struct {
				Model string `json:"model"`
			}
			if r.Body != nil {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &peek)
			}
			mu.Lock()
			seen = append(seen, peek.Model)
			mu.Unlock()
			status, body := behavior(peek.Model)
			ct := "application/json"
			if status == 200 {
				ct = "text/event-stream"
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ct}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return c, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func fbChat(h *Handler, model string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	return rec
}

func fbAcct() *auth.Auth { return &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999} }

// 6004 → 同账号改道等价模型；冷却记在原请求模型上，等价模型不受影响；不占换号次数。
func TestModelFallbackOn6004(t *testing.T) {
	up, seen := modelAwareUpstream(t, func(m string) (int, string) {
		if m == fbFrom {
			return 429, body6004
		}
		return 200, sseOK
	})
	p := testPoolWith(fbAcct())
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1, ModelsFallback: []string{fbFrom + "=" + fbTo}})
	rec := fbChat(h, fbFrom)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := seen(); len(got) != 2 || got[0] != fbFrom || got[1] != fbTo {
		t.Fatalf("upstream models=%v want [%s %s]", got, fbFrom, fbTo)
	}
	if !p.ModelBlocked(fbFrom).Blocked {
		t.Error("原模型应被记冷却")
	}
	if p.ModelBlocked(fbTo).Blocked {
		t.Error("等价模型不该被冷却")
	}
}

// 带「切换其他模型」的 6000 同样触发。
func TestModelFallbackOn6000SwitchHint(t *testing.T) {
	up, seen := modelAwareUpstream(t, func(m string) (int, string) {
		if m == fbFrom {
			return 429, body6000Sw
		}
		return 200, sseOK
	})
	h := NewHandler(Config{Pool: testPoolWith(fbAcct()), Upstream: up, MaxRotate: 1, ModelsFallback: []string{fbFrom + "=" + fbTo}})
	if rec := fbChat(h, fbFrom); rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := seen(); len(got) != 2 || got[1] != fbTo {
		t.Fatalf("upstream models=%v", got)
	}
}

// 14003 繁忙、未配置改道表时都不改道。
func TestModelFallbackNotTriggered(t *testing.T) {
	cases := []struct {
		name string
		body string
		fb   []string
	}{
		{"14003", body14003, []string{fbFrom + "=" + fbTo}},
		{"no-config", body6004, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, seen := modelAwareUpstream(t, func(m string) (int, string) {
				if m == fbFrom {
					return 429, tc.body
				}
				return 200, sseOK
			})
			h := NewHandler(Config{Pool: testPoolWith(fbAcct()), Upstream: up, MaxRotate: 1, ModelsFallback: tc.fb})
			if rec := fbChat(h, fbFrom); rec.Code == 200 {
				t.Fatalf("不该改道成功：%s", rec.Body)
			}
			for _, m := range seen() {
				if m == fbTo {
					t.Fatalf("不该请求等价模型：%v", seen())
				}
			}
		})
	}
}

// A↔B 互为等价且都 6004：各试一次不循环，冷却分别记在各自实际请求的模型上。
func TestModelFallbackNoLoop(t *testing.T) {
	up, seen := modelAwareUpstream(t, func(m string) (int, string) { return 429, body6004 })
	p := testPoolWith(fbAcct())
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1,
		ModelsFallback: []string{fbFrom + "=" + fbTo, fbTo + "=" + fbFrom}})
	rec := fbChat(h, fbFrom)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := seen(); len(got) != 2 {
		t.Fatalf("upstream models=%v want 2 attempts", got)
	}
	st, _ := p.Status("u1")
	limited := map[string]bool{}
	for _, m := range st.RateLimitedModels {
		limited[m.Model] = true
	}
	if !limited[fbFrom] || !limited[fbTo] {
		t.Errorf("两个模型都应各自记冷却：%+v", st.RateLimitedModels)
	}
}

// 全池原模型被配额锁：不打原模型，直接改道；等价模型也锁住时回配额 429。
func TestModelFallbackWhenPoolQuotaLocked(t *testing.T) {
	up, seen := modelAwareUpstream(t, func(m string) (int, string) { return 200, sseOK })
	p := testPoolWith(fbAcct())
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), fbFrom, body6004)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1, ModelsFallback: []string{fbFrom + "=" + fbTo}})
	if rec := fbChat(h, fbFrom); rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := seen(); len(got) != 1 || got[0] != fbTo {
		t.Fatalf("upstream models=%v want [%s]", got, fbTo)
	}

	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), fbTo, body6004)
	rec := fbChat(h, fbFrom)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("code=%d body=%s want quota 429", rec.Code, rec.Body)
	}
}

// 跨域、跨模型族的条目不生效。
func TestModelFallbackScope(t *testing.T) {
	up, seen := modelAwareUpstream(t, func(m string) (int, string) {
		if m == fbFrom {
			return 429, body6004
		}
		return 200, sseOK
	})
	h := NewHandler(Config{Pool: testPoolWith(fbAcct()), Upstream: up, MaxRotate: 1,
		ModelsFallback: []string{fbFrom + "=global:" + fbTo, fbFrom + "=glm-5.2"}})
	if rec := fbChat(h, fbFrom); rec.Code == 200 {
		t.Fatalf("跨域/跨族不该改道：%s", rec.Body)
	}
	if got := seen(); len(got) != 1 {
		t.Fatalf("upstream models=%v", got)
	}
}
