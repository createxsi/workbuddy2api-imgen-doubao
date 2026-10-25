// busy.go POST /v2/chat/busy：官方客户端对话前的「模型忙」预检。
//
// 请求 {"models":["..."]}，响应 {"code":0,"msg":"OK","data":[{"model","busy_level"}]}。
// 本地按池台账算，不打上游（国际版上游无此路由，透传会让两域行为不一致）：
//   - busy：该域所有可选账号对该模型都处于模型级冷却；
//   - idle：仍有账号能服务；
//   - unknown：模型不在本网关对外目录（拼错/被隐藏/目录未拉到）或模型名为空。
//
// 模型名按 chat 同口径解析：域前缀、models.aliases 映射与黑名单都生效。
package server

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	busyLevelIdle    = "idle"
	busyLevelBusy    = "busy"
	busyLevelUnknown = "unknown"
)

func (h *Handler) chatBusy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	var known map[string]struct{}
	if len(req.Models) > 0 {
		known = h.servableModelSet()
	}
	out := make([]map[string]any, 0, len(req.Models))
	for _, m := range req.Models {
		out = append(out, map[string]any{"model": m, "busy_level": h.busyLevel(m, known)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":      0,
		"msg":       "OK",
		"requestId": session.NewMessageID(),
		"data":      out,
	})
}

func (h *Handler) busyLevel(model string, known map[string]struct{}) string {
	realm, bare := resolveModel(model)
	if target, ok := h.modelAlias(realm, bare); ok {
		if !hasRealmPrefix(target) {
			target = realm + ":" + target
		}
		realm, bare = resolveModel(target)
	}
	if bare == "" {
		return busyLevelUnknown
	}
	if _, ok := known[realm+":"+bare]; !ok {
		return busyLevelUnknown
	}
	if h.cfg.Pool != nil && h.cfg.Pool.ModelBlockedForRealm(bare, realm).Blocked {
		return busyLevelBusy
	}
	return busyLevelIdle
}

// servableModelSet 网关对外目录（与 /v1/models 同源，已按黑名单过滤），键为 "realm:bare"。
// 目录为空（未探测/失败）时回空集，调用方判 unknown，不编造「有这个模型」。
func (h *Handler) servableModelSet() map[string]struct{} {
	infos := h.modelList()
	set := make(map[string]struct{}, len(infos))
	for _, it := range infos {
		if id, _ := it["id"].(string); id != "" {
			realm, bare := resolveModel(id)
			set[realm+":"+bare] = struct{}{}
		}
	}
	return set
}
