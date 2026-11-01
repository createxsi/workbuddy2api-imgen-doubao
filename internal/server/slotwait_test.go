package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func slotWaitPool() (*auth.Auth, *auth.Auth) {
	return &auth.Auth{UID: "a1", AccessToken: "at1", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999}
}

const slotWaitBody = `{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// 一号模型冷却、另一号在途占满：等到名额释放后应 200，而不是立刻 503。
func TestSlotWaitRescues(t *testing.T) {
	saved := busyWaitCap
	busyWaitCap = 3 * time.Second
	t.Cleanup(func() { busyWaitCap = saved })

	a1, a2 := slotWaitPool()
	p := testPoolWith(a1, a2)
	p.SetMaxInFlight(2)
	p.CooldownSoftForModel("a1", time.Minute, time.Now().Add(time.Hour), "deepseek-v4.1-flash", "6004 quota")
	for i := 0; i < 2; i++ {
		if !p.Acquire("a2") {
			t.Fatalf("acquire %d", i)
		}
	}
	if !p.SlotWaitForRealm(nil, "deepseek-v4.1-flash", "cn") {
		t.Fatal("应判定为在途占满")
	}
	if p.SlotWaitForRealm(map[string]bool{"a2": true}, "deepseek-v4.1-flash", "cn") {
		t.Fatal("已试过的号不应计入")
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		p.Release("a2")
	}()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(slotWaitBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%.200s", rec.Code, rec.Body.String())
	}
}

// 名额始终不释放：等待有上限，最终 503。
func TestSlotWaitBounded(t *testing.T) {
	saved := busyWaitCap
	busyWaitCap = 150 * time.Millisecond
	t.Cleanup(func() { busyWaitCap = saved })

	a1, a2 := slotWaitPool()
	p := testPoolWith(a1, a2)
	p.SetMaxInFlight(1)
	p.CooldownSoftForModel("a1", time.Minute, time.Now().Add(time.Hour), "deepseek-v4.1-flash", "6004 quota")
	p.Acquire("a2")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(slotWaitBody)))
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("等待未被上限收住：%v", d)
	}
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%.200s", rec.Code, rec.Body.String())
	}
}
