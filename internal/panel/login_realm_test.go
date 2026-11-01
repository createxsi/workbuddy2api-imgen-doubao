package panel

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestLoginEndpoints realm→端点映射与桌面端 platform 口径：global 三端点在 workbuddy.ai，
// cn/空/非法值 → copilot.tencent.com。
func TestLoginEndpoints(t *testing.T) {
	cases := []struct {
		realm     string
		wantState string
	}{
		{"global", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=workbuddy-ai"},
		{"cn", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy"},
		{"", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy"},
		{"weird", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy"},
	}
	for _, c := range cases {
		st, tok, acct := loginEndpoints(c.realm)
		if st != c.wantState {
			t.Errorf("realm=%q state=%q want %q", c.realm, st, c.wantState)
		}
		base := c.wantState[:strings.Index(c.wantState, "/v2/")]
		if !strings.HasPrefix(tok, base) || !strings.HasPrefix(acct, base) {
			t.Errorf("realm=%q token/account 应与 state 同域: %q %q", c.realm, tok, acct)
		}
	}
}

// 三端点头族：桌面端 RPC UA、无浏览器头、差异头按端点。
func TestLoginHeadersDesktopFamily(t *testing.T) {
	st := upstream.LoginHeaders("cn", upstream.LoginState, "", "")
	if st["User-Agent"] != "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0" || st["X-Domain"] != "www.workbuddy.cn" ||
		st["X-No-Authorization"] != "true" || st["X-Custom-Channel-Id"] != "unknown" || st["Content-Type"] == "" {
		t.Errorf("state 头不符: %v", st)
	}
	if _, ok := st["X-Device-Token"]; ok {
		t.Error("无设备 token 时不应发 X-Device-Token")
	}
	tok := upstream.LoginHeaders("global", upstream.LoginToken, "", "")
	if tok["User-Agent"] != "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0" || tok["X-Domain"] != "" || tok["Content-Type"] != "" {
		t.Errorf("token 头不符: %v", tok)
	}
	acct := upstream.LoginHeaders("global", upstream.LoginAccount, "at", "")
	if acct["Authorization"] != "Bearer at" || acct["X-Domain"] != "www.workbuddy.ai" || acct["X-No-Authorization"] != "" || acct["X-No-User-Id"] != "true" {
		t.Errorf("account 头不符: %v", acct)
	}
	for _, h := range []map[string]string{st, tok, acct} {
		for _, k := range []string{"Origin", "Referer", "X-Requested-With"} {
			if h[k] != "" {
				t.Errorf("登录端点不应发 %s", k)
			}
		}
		if len(h["X-Request-ID"]) != 32 {
			t.Errorf("X-Request-ID 应为 32 hex: %q", h["X-Request-ID"])
		}
	}
}
