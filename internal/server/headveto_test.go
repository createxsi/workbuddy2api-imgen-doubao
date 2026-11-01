package server

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

const (
	vetoBusyFrame    = "data: {\"code\":6021,\"msg\":\"queue.full.title\"}\n\n"
	vetoIllegalFrame = "data: {\"code\":11140,\"msg\":\"request illegal\"}\n\n"
)

// sequenceUpstream 第 n 次上游调用（从 0 起）的响应由 seq[n] 决定，超出取最后一个。
func sequenceUpstream(t *testing.T, seq ...string) (*Handler, *int32) {
	t.Helper()
	var n int32
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(seq) {
			i = len(seq) - 1
		}
		return 200, seq[i], true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	return NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3, RequestLog: reqlog.New(reqlog.Config{})}), &n
}

func TestHeadVetoRotatesStream(t *testing.T) {
	for name, first := range map[string]string{"6021": vetoBusyFrame, "11140": vetoIllegalFrame, "empty": "data: [DONE]\n\n"} {
		t.Run(name, func(t *testing.T) {
			h, n := sequenceUpstream(t, first, sseOK)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
			if rec.Code != 200 || strings.Contains(rec.Body.String(), `"error"`) || !strings.Contains(rec.Body.String(), "你好") {
				t.Fatalf("应换号成功且无 error 帧：code=%d body=%s", rec.Code, rec.Body)
			}
			if atomic.LoadInt32(n) != 2 {
				t.Fatalf("上游调用次数=%d want 2", *n)
			}
		})
	}
}

func TestHeadVetoRotatesNonStream(t *testing.T) {
	h, n := sequenceUpstream(t, vetoBusyFrame, sseOK)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "你好") || atomic.LoadInt32(n) != 2 {
		t.Fatalf("非流式也应换号：code=%d n=%d body=%s", rec.Code, *n, rec.Body)
	}
}

// 协议桥：首帧否决发生在 bridgeWriter 写头之前，/v1/messages 拿到的是换号后的成功流。
func TestHeadVetoBeforeBridge(t *testing.T) {
	h, _ := sequenceUpstream(t, vetoIllegalFrame, sseOK)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	if rec.Code != 200 || strings.Contains(body, "event: error") || !strings.Contains(body, "你好") {
		t.Fatalf("messages 桥应透明换号：code=%d body=%s", rec.Code, body)
	}
}

// 全池首帧都失败：客户端拿到正常 HTTP 错误（JSON、非 200），而不是 200 + error 帧；
// 流内 11140 不硬禁用账号。
func TestHeadVetoExhausted(t *testing.T) {
	h, _ := sequenceUpstream(t, vetoIllegalFrame)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("应回 JSON 错误：code=%d ct=%s body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "request illegal") {
		t.Fatalf("应透传上游原文：%s", rec.Body)
	}
	for _, uid := range []string{"u1", "u2"} {
		if st, ok := h.cfg.Pool.Status(uid); !ok || st.Disabled {
			t.Fatalf("%s 不应被禁用", uid)
		}
	}
	s := h.cfg.RequestLog.Snapshot()
	if len(s.Recent) != 1 || s.Recent[0].Outcome != reqlog.OutcomeStreamError {
		t.Fatalf("观测应为 stream_error：%+v", s.Recent)
	}
}
