package upstream

import (
	"errors"
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
