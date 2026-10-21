// busywait_test.go 钉住「模型繁忙等一次」的判据：只有「纯 14003 模型繁忙、且剩余冷却
// 在等待上限内」才值得在请求内等一等，其余一律立刻上报（等下去只是让客户端干耗）。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestBusyWaitFor(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		blk  pool.ModelBlockStatus
		want bool
	}{
		{"繁忙且在上限内（上游原文）", pool.ModelBlockStatus{Blocked: true, Reason: `{"code":14003,"msg":"too many requests"}`, Until: now.Add(3 * time.Second)}, true},
		{"繁忙且在上限内（本地标记）", pool.ModelBlockStatus{Blocked: true, Reason: "14003 model busy", Until: now.Add(3 * time.Second)}, true},
		{"繁忙但超出上限", pool.ModelBlockStatus{Blocked: true, Reason: `{"code":14003,"msg":"too many requests"}`, Until: now.Add(30 * time.Second)}, false},
		{"繁忙但无解封时刻", pool.ModelBlockStatus{Blocked: true, Reason: `{"code":14003,"msg":"too many requests"}`}, false},
		{"模型不存在（换号必然同样失败）", pool.ModelBlockStatus{Blocked: true, Reason: "11102 model not found", Until: now.Add(3 * time.Second)}, false},
		{"未被阻塞", pool.ModelBlockStatus{}, false},
	}
	for _, c := range cases {
		if got := busyWaitFor(c.blk, now, 10*time.Second) > 0; got != c.want {
			t.Errorf("%s: got wait=%v want %v", c.name, got, c.want)
		}
	}
}

// 上限为 0（测试/关闭）时恒不等：轮转测试不被这条路径拖慢。
func TestBusyWaitForDisabled(t *testing.T) {
	blk := pool.ModelBlockStatus{Blocked: true, Reason: `{"code":14003,"msg":"too many requests"}`, Until: time.Now().Add(time.Second)}
	if d := busyWaitFor(blk, time.Now(), 0); d != 0 {
		t.Fatalf("cap=0 时不应等待，got %v", d)
	}
}

// 等待必须等满：双向抖动会在冷却没解除时醒来，「只等一次」的额度白花、客户端
// 多等十秒照样拿 503（实测 8 次里 4 次）。
func TestWaitPastNeverEarly(t *testing.T) {
	for d := time.Millisecond; d <= 10*time.Second; d *= 10 {
		for n := 0; n < 200; n++ {
			if got := waitPast(d); got < d {
				t.Fatalf("waitPast(%v)=%v 提前醒来", d, got)
			} else if got > d+d/4+time.Millisecond {
				t.Fatalf("waitPast(%v)=%v 超出正向抖动上限", d, got)
			}
		}
	}
}

// 轮转耗尽（MaxRotate=1 时必然）也要给可重试语义：此前落到 429 rate_limit_exceeded
// 且没有 Retry-After，与选号失败分支的 503 model_busy 自相矛盾。
func TestChatModelBusyRotationExhaustedReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":14003,"msg":"too many requests","requestId":"rid"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "a", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[]}`)))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "model_busy") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("缺 Retry-After：客户端无从知道等多久")
	}
}

// 全池短冷却 + 上游其实可用：请求内等完再试一次，客户端应看到 200 而不是 503。
func TestChatModelBusyWaitRescues(t *testing.T) {
	saved := busyWaitCap
	busyWaitCap = 2 * time.Second
	defer func() { busyWaitCap = saved }()

	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "a", ExpiresAt: 9999999999})
	// 80ms 的「14003 冷却」：语义与 CooldownModelBusy 同形，只是短到测试可等。
	p.CooldownSoftForModel("g1", time.Second, time.Now().Add(80*time.Millisecond), "deepseek-v4.1-flash", "14003 model busy")
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%.120s（等待后应该救回来）", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1（等待不该额外打上游）", calls)
	}
}
