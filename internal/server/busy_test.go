package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// /v2/chat/busy：模型级冷却 → busy；可服务 → idle；目录外/隐藏 → unknown；别名按目标判。
func TestChatBusyLevels(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "deepseek-v4.1-flash"}, {ID: "hidden-one"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.Unlock()
	}()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	}), ModelsHidden: []string{"hidden-one"}, ModelsAliases: []string{"my-fast=deepseek-v4.1-flash"}})

	levels := func(body string) map[string]string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var resp struct {
			Code int `json:"code"`
			Data []struct {
				Model     string `json:"model"`
				BusyLevel string `json:"busy_level"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Code != 0 {
			t.Fatalf("bad resp: %v %s", err, rec.Body)
		}
		out := map[string]string{}
		for _, d := range resp.Data {
			out[d.Model] = d.BusyLevel
		}
		return out
	}

	got := levels(`{"models":["deepseek-v4.1-flash","cn:deepseek-v4.1-flash","no-such","hidden-one","my-fast","global:deepseek-v4.1-flash"]}`)
	want := map[string]string{"deepseek-v4.1-flash": "idle", "cn:deepseek-v4.1-flash": "idle", "no-such": "unknown",
		"hidden-one": "unknown", "my-fast": "idle", "global:deepseek-v4.1-flash": "unknown"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%q want %q", k, got[k], v)
		}
	}

	p.CooldownModelBusy("u1", "deepseek-v4.1-flash", "14003 model busy")
	got = levels(`{"models":["deepseek-v4.1-flash","my-fast"]}`)
	if got["deepseek-v4.1-flash"] != "busy" || got["my-fast"] != "busy" {
		t.Errorf("模型级冷却应为 busy：%v", got)
	}
	if got := levels(`{"models":[]}`); len(got) != 0 {
		t.Errorf("空 models 应返回空 data：%v", got)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy", strings.NewReader("{bad")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400：%d", rec.Code)
	}
}
