package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// aliasHandler 上游按 stream 回 SSE；记录出站 body。
func aliasHandler(t *testing.T, cfg Config, bodies *[]string) *Handler {
	t.Helper()
	cfg.Upstream = &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			*bodies = append(*bodies, string(b))
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	return NewHandler(cfg)
}

func postChat(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body))))
	return rec
}

func TestModelAliasRewritesAndEchoes(t *testing.T) {
	var bodies []string
	h := aliasHandler(t, Config{ModelsAliases: []string{"Claude-Sonnet-4=glm-5.2"}}, &bodies)

	rec := postChat(h, `{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || len(bodies) != 1 {
		t.Fatalf("code=%d calls=%d body=%s", rec.Code, len(bodies), rec.Body.String())
	}
	if !strings.Contains(bodies[0], `"model":"glm-5.2"`) || strings.Contains(bodies[0], "claude-sonnet-4") {
		t.Fatalf("出站应改写为真实名：%s", bodies[0])
	}
	if !strings.Contains(rec.Body.String(), `"model":"claude-sonnet-4"`) || strings.Contains(rec.Body.String(), `"model":"glm-5.2"`) {
		t.Fatalf("流式响应应回显客户端原发名：%s", rec.Body.String())
	}

	rec = postChat(h, `{"model":"cn:claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"model":"cn:claude-sonnet-4"`) {
		t.Fatalf("非流式应回显：code=%d %s", rec.Code, rec.Body.String())
	}

	// 未映射：body 不动、不回显改写
	bodies = nil
	rec = postChat(h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(bodies[0], `"model":"glm-5.2"`) {
		t.Fatalf("未映射回归：code=%d %v", rec.Code, bodies)
	}
}

// 别名不能绕过黑名单；域前缀键只在该域生效。
func TestModelAliasHiddenAndRealmScope(t *testing.T) {
	var bodies []string
	h := aliasHandler(t, Config{
		ModelsAliases: []string{"a=secret", "global:b=glm-5.2"},
		ModelsHidden:  []string{"secret"},
	}, &bodies)
	if rec := postChat(h, `{"model":"a","messages":[]}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("映射到隐藏模型应拒绝：code=%d %s", rec.Code, rec.Body.String())
	}
	if len(bodies) != 0 {
		t.Fatalf("隐藏模型不应出站：%v", bodies)
	}
	// cn 域请求 b：前缀键只在 global 生效，按原名透传。
	postChat(h, `{"model":"b","messages":[{"role":"user","content":"hi"}]}`)
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"model":"b"`) {
		t.Fatalf("cn 域不应命中 global 前缀键：%v", bodies)
	}
}

func TestImagesHiddenRejectedAndFiltered(t *testing.T) {
	var bodies []string
	h := aliasHandler(t, Config{ModelsHidden: []string{"hunyuan-image-alpha", "global:hy-image-3.5"}}, &bodies)
	for _, body := range []string{`{"model":"cn:hunyuan-image-alpha","prompt":"猫"}`, `{"model":"global:hy-image-3.5","prompt":"猫"}`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body)))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "model_unavailable") {
			t.Fatalf("%s → code=%d %s", body, rec.Code, rec.Body.String())
		}
	}
	if len(bodies) != 0 {
		t.Fatalf("隐藏出图模型不应出站：%v", bodies)
	}
	// 探测走负缓存 → 回落静态清单，静态清单同样过滤。
	generationModelsCache.Lock()
	saved := generationModelsCache.byRealm
	generationModelsCache.byRealm = map[string]*generationModelsEntry{"cn": {lastFail: time.Now()}}
	generationModelsCache.Unlock()
	defer func() {
		generationModelsCache.Lock()
		generationModelsCache.byRealm = saved
		generationModelsCache.Unlock()
	}()
	h2 := aliasHandler(t, Config{ModelsHidden: []string{"hunyuan-image-alpha"}}, &bodies)
	rec := httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/images/models", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"cn:hunyuan-image-alpha"`) || !strings.Contains(rec.Body.String(), "cn:hunyuan-image-alpha-edit") {
		t.Fatalf("出图列表应过滤隐藏模型：%s", rec.Body.String())
	}
}
