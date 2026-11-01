package upstream

import (
	"net/http"
	"regexp"
	"sort"
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

// TestRefreshHeadersRPCFamily refresh 真机键集：RPC 族，AT/RT 双发，无 XRW。
func TestRefreshHeadersRPCFamily(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest("POST", "https://www.workbuddy.cn/v2/plugin/auth/token/refresh", nil)
	c.RefreshHeaders(req, &auth.Auth{UID: "u", RefreshToken: "r", AccessToken: "at"})
	assertHeaderKeys(t, req.Header, "Accept", "Content-Type", "User-Agent", "X-Product", "X-Domain",
		"X-Refresh-Token", "X-Auth-Refresh-Source", "X-Request-Id", "Authorization", "X-User-Id")
	if req.Header.Get("X-Domain") != "www.workbuddy.cn" || req.Header.Get("Authorization") != "Bearer at" {
		t.Errorf("refresh 头不符: %v", req.Header)
	}
}

// TestBillingHeadersShape billing 族：带 X-Request-ID，不带 X-CodeBuddy-Request /
// Accept-Language，X-Domain 为裸主机名。
func TestBillingHeadersShape(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest("POST", "https://www.workbuddy.cn/v2/billing/meter/daily-checkin", nil)
	c.BillingHeaders(req, &auth.Auth{UID: "u", AccessToken: "at", Domain: "https://www.codebuddy.cn/"})
	assertHeaderKeys(t, req.Header, "Authorization", "Accept", "Content-Type", "X-Request-Id",
		"User-Agent", "X-User-Id", "X-Domain")
	if got := req.Header.Get("X-Domain"); got != "www.codebuddy.cn" {
		t.Errorf("X-Domain=%q", got)
	}
	if id := req.Header.Get("X-Request-ID"); len(id) != 32 {
		t.Errorf("X-Request-ID=%q", id)
	}
}

// TestChatHeadersDerivedConversationID 客户端未声明会话：头上放 (uid, 会话键) 派生 v7，
// 不放原始会话键；同号同键稳定，异号不同。
func TestChatHeadersDerivedConversationID(t *testing.T) {
	c := &Client{}
	get := func(uid string) (string, string) {
		req, _ := http.NewRequest("POST", "https://www.workbuddy.cn/v2/chat/completions", nil)
		c.ChatHeaders(req, &auth.Auth{UID: uid, AccessToken: "at"}, "", ChatMeta{CacheKeyID: "sess-raw-key"})
		return req.Header.Get("X-Conversation-ID"), req.Header.Get("acp-connection-id")
	}
	c1, a1 := get("u1")
	c2, _ := get("u1")
	c3, _ := get("u2")
	if !uuidV7Re.MatchString(c1) || !uuidV7Re.MatchString(a1) || c1 == a1 {
		t.Fatalf("conv=%q acp=%q", c1, a1)
	}
	if c1 != c2 || c1 == c3 || strings.Contains(c1, "sess-raw-key") {
		t.Errorf("派生不稳定或未隔离: %q %q %q", c1, c2, c3)
	}
	req, _ := http.NewRequest("POST", "https://www.workbuddy.cn/v2/chat/completions", nil)
	c.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}, "", ChatMeta{})
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.cn" {
		t.Errorf("chat X-Domain 应取对话域主机名，got %q", got)
	}
}

var uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func assertHeaderKeys(t *testing.T, h http.Header, want ...string) {
	t.Helper()
	var got []string
	for k := range h {
		got = append(got, k)
	}
	for i := range want {
		want[i] = http.CanonicalHeaderKey(want[i])
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("头键集=%v want %v", got, want)
	}
}
