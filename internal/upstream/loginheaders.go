package upstream

import (
	"os"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// 设备码登录三端点（auth/state、auth/token、login/account）的桌面端口径
// （CN 5.9.3 / global 5.9.4 真机）：
//   - ?platform= 取分发包 authentication.attributes.platform：CN workbuddy、
//     global workbuddy-ai（不是顶层 platform 字段的 CLI）；
//   - UA 为桌面端 RPC 族，node fetch 发出，不带 Origin / Referer / X-Requested-With；
//   - 三端点差异头：state 带 X-Domain + 四个 X-No-* + X-Custom-Channel-Id；token 只有
//     四个 X-No-*（唯一不带 X-Domain）；account 带 X-Domain + Bearer + 三个 X-No-*。
const (
	LoginState   = "state"
	LoginToken   = "token"
	LoginAccount = "account"
)

// LoginPlatform 授权 state 的 platform 参数。
func LoginPlatform(realm string) string {
	if realmKey(realm) == "global" {
		return "workbuddy-ai"
	}
	return "workbuddy"
}

// loginDomain X-Domain：声明调用方前端主机名，与请求落到哪台 API 主机无关。
func loginDomain(realm string) string {
	if realmKey(realm) == "global" {
		return "www.workbuddy.ai"
	}
	return "www.workbuddy.cn"
}

// LoginHeaders 返回指定端点的完整出站头。deviceToken 为空不发（伪造比缺失更可疑）。
func LoginHeaders(realm, endpoint, accessToken, deviceToken string) map[string]string {
	h := map[string]string{
		"Accept":       "application/json, text/plain, */*",
		"User-Agent":   DesktopRPCUserAgent(realm, "", ""),
		"X-Product":    "SaaS",
		"X-Request-ID": session.NewTraceID(),
	}
	sentinel := func(noAuth bool) {
		h["X-No-User-Id"] = "true"
		h["X-No-Enterprise-Id"] = "true"
		h["X-No-Department-Info"] = "true"
		if noAuth {
			h["X-No-Authorization"] = "true"
		}
	}
	switch endpoint {
	case LoginState:
		h["Content-Type"] = "application/json"
		h["X-Domain"] = loginDomain(realm)
		h["X-Custom-Channel-Id"] = "unknown"
		sentinel(true)
		if deviceToken != "" {
			h["X-Device-Token"] = deviceToken
		}
	case LoginToken:
		sentinel(true)
	case LoginAccount:
		h["X-Domain"] = loginDomain(realm)
		sentinel(false)
		if accessToken != "" {
			h["Authorization"] = "Bearer " + accessToken
		}
	}
	return h
}

// LoginDeviceToken 登录用设备风控 token：CODEBUDDY_LOGIN_DEVICE_TOKEN 或
// CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE；取不到返回空。
func LoginDeviceToken() string {
	if v := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN")); v != "" {
		return v
	}
	if p := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE")); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}
