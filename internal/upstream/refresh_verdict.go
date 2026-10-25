package upstream

import (
	"errors"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"net/http"
)

// IsGatewayUnauthorized 报告 401 是否为网关拦截形态（无业务信封：HTML 页、空体、纯文本）。
// token 真失效与边缘瞬时拦截的响应体完全一样，单看 401 分不出来，需 refresh 求证。
func IsGatewayUnauthorized(status int, body string) bool {
	return status == http.StatusUnauthorized && !hasBusinessEnvelope(body)
}

// RefreshRejected 报告 refresh 失败是否为上游应用层的明确拒绝（token 确已失效）。
// 传输错误、5xx、429 以及 refresh 端点自身被网关拦截（401/403 无业务信封）都不是
// 判词：refresh 同样挂在 /v2/* 上，边缘拦截时它也回 HTML 页，按死号处理会误杀。
func RefreshRejected(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	if ue.Status >= 500 || ue.Status == http.StatusTooManyRequests {
		return false
	}
	if IsGatewayUnauthorized(ue.Status, ue.Msg) || IsWafBlocked(ue.Status, ue.Msg) {
		return false
	}
	return ue.Status >= 400
}

// SessionDeadButUsable 报告「refresh 判死，但 access token 仍未过期」。refresh token
// 失效与账号不可用是两件事：AT 有效期内 chat 照常。只适用于 AT 尚未被上游拒绝的路径
// （保活 / 发请求前的预刷新）；chat 已拿到 12153/401 时 AT 已被拒，不适用。
func SessionDeadButUsable(a *auth.Auth) bool {
	return a != nil && !a.NeedsRefresh(0)
}
