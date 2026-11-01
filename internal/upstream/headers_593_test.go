package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// RPC 族（非 chat REST）：无浏览器语义头、无 chat 专属头，UA 按域分族。
func TestCommonHeadersRPCFamily(t *testing.T) {
	c := &Client{}
	for _, tc := range []struct {
		a  *auth.Auth
		ua string
	}{
		{&auth.Auth{UID: "u", Domain: "www.codebuddy.cn"}, "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0"},
		{&auth.Auth{UID: "u", Domain: "www.workbuddy.ai"}, "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"},
	} {
		req, _ := http.NewRequest("GET", "https://x/v3/config", nil)
		c.CommonHeaders(req, tc.a)
		if got := req.Header.Get("User-Agent"); got != tc.ua {
			t.Errorf("UA=%q want %q", got, tc.ua)
		}
		for _, h := range []string{"Origin", "Referer", "X-Requested-With", "Accept-Language", "X-CodeBuddy-Request", "X-Machine-ID", "X-Session-ID"} {
			if req.Header.Get(h) != "" {
				t.Errorf("RPC 族不应发 %s", h)
			}
		}
		if req.Header.Get("X-Product") != "SaaS" || req.Header.Get("Accept") != "application/json, text/plain, */*" {
			t.Errorf("RPC 公共头不符: %v", req.Header)
		}
	}
}

// chat（SDK 族）：补回 XRW / X-CodeBuddy-Request，新增运行形态头，baggage 与会话头同值。
func TestChatHeadersAgentContext(t *testing.T) {
	c := &Client{}
	cases := []struct {
		a       *auth.Auth
		ua      string
		private string
	}{
		{&auth.Auth{UID: "u", AccessToken: "t", Domain: "www.codebuddy.cn"}, "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0", "false"},
		{&auth.Auth{UID: "u", AccessToken: "t", Domain: "www.codebuddy.cn", EnterpriseID: "e"}, "", "true"},
		{&auth.Auth{UID: "u", AccessToken: "t", Domain: "www.workbuddy.ai"}, "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0", "true"},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest("POST", "https://x/v2/chat/completions", nil)
		c.ChatHeaders(req, tc.a, "", ChatMeta{ConversationID: "conv-1", ConversationRequestID: "req-1"})
		if tc.ua != "" && req.Header.Get("User-Agent") != tc.ua {
			t.Errorf("chat UA=%q want %q", req.Header.Get("User-Agent"), tc.ua)
		}
		if req.Header.Get("X-Requested-With") != "XMLHttpRequest" || req.Header.Get("X-CodeBuddy-Request") != "1" || req.Header.Get("Accept") != "application/json" {
			t.Errorf("chat 族头缺失: %v", req.Header)
		}
		if req.Header.Get("X-Agent-Runtime") != "local-agent" {
			t.Error("缺 X-Agent-Runtime")
		}
		if got := req.Header.Get("X-Private-Data"); got != tc.private {
			t.Errorf("X-Private-Data=%q want %q", got, tc.private)
		}
		bg := req.Header.Get("baggage")
		if bg != "buddyId=,buddyName=,codebuddy.session_id=conv-1,codebuddy.conversation_request_id=req-1" {
			t.Errorf("baggage=%q", bg)
		}
		if strings.Contains(req.Header.Get("Origin")+req.Header.Get("Referer"), "http") {
			t.Error("chat 不应发 Origin/Referer")
		}
	}
}

func TestRefreshHeadersKeepsXRW(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest("POST", "https://x/v2/plugin/auth/token/refresh", nil)
	c.RefreshHeaders(req, &auth.Auth{UID: "u", RefreshToken: "r"})
	if req.Header.Get("X-Requested-With") != "XMLHttpRequest" || req.Header.Get("X-Refresh-Token") != "r" || req.Header.Get("Origin") != "" {
		t.Errorf("refresh 头不符: %v", req.Header)
	}
}
