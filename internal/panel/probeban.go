package panel

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// accountProbeBan 只读探测禁用账号在上游的现状：用余额查询（最轻的只读端点）试一次，
// 不解冻、不参与选号、不改池状态——反复解冻会让号重新接流量再撞封禁，可能加重风控。
//
// 结论：lifted（已可用，可解冻）/ still_banned（11140 类账号故障）/ blocked（401 但
// refresh 成功：会话仍活，请求被拦截，可能仍是封禁）/ session_dead（refresh 被拒，需重登）/
// inconclusive（网络/限流等无判词）。401 的响应体对封禁与登录失效一样，故以 refresh 为判词。
func (p *Panel) accountProbeBan(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	st, ok := p.cfg.Pool.Status(uid)
	a := p.cfg.Pool.AuthByUID(uid)
	if !ok || a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream not available")
		return
	}
	verdict, detail := p.probeBan(a)
	log.Printf("panel: probe_ban uid=%s → %s", uid, verdict)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid, "verdict": verdict, "detail": detail,
		"was_disabled": st.Disabled, "probed_at": time.Now().Format(time.RFC3339),
	})
}

func (p *Panel) probeBan(a *auth.Auth) (verdict, detail string) {
	_, _, err := p.cfg.Upstream.UserResource(a)
	if err == nil {
		return "lifted", ""
	}
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return "inconclusive", err.Error()
	}
	switch {
	case ue.Kind == upstream.ErrAccountFault:
		return "still_banned", err.Error()
	case ue.Kind == upstream.ErrSessionDead || upstream.IsGatewayUnauthorized(ue.Status, ue.Msg):
		if a.RefreshTokenValue() == "" {
			return "session_dead", err.Error()
		}
		rerr := p.cfg.Upstream.RefreshToken(a)
		if rerr == nil {
			if serr := a.SaveAtomic(); serr != nil {
				log.Printf("ERR: panel: probe_ban uid=%s: save auth failed: %v", a.UID, serr)
			}
			return "blocked", err.Error()
		}
		if upstream.RefreshRejected(rerr) {
			return "session_dead", rerr.Error()
		}
		return "inconclusive", rerr.Error()
	}
	return "inconclusive", err.Error()
}
