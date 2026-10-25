// images.go 生图端点：
//
//	POST /v1/images/generations  文本生图（JSON 入站）
//	POST /v1/images/edits        图片编辑 / 图生图（multipart 或 JSON 入站）
//
// 设计要点：
//   - 两个端点复用同一套选号/轮转/错误处置链路（runImage），账号健康度口径与 chat 一致；
//   - realm 支持 "global:" / "cn:" 前缀（与 chat 同一 D6 协议）；**无前缀时默认
//     global**——桌面版实测生图走 www.workbuddy.ai，池内无 global 可服务号才回退 cn；
//   - 上游请求体由 upstream.BuildImageUpstreamBody / BuildImageEditUpstreamBody 构造；
//   - 上游腾讯信封由 upstream.NormalizeImageResponse 归一化为 OpenAI 响应。
//
// 上游 edits 是 application/json（非 multipart），图片以 data URL 数组放在 image 字段，
// 故出站链路与 generations 完全一致，只差路径。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// imageDefaultRealm 无显式 realm 前缀时的默认域。
// 依据：桌面版 WorkBuddyAI ImageGen 实测出站 https://www.workbuddy.ai/v2/images/generations，
// 即 global realm。池内无 global 可服务号时由 resolveImageRealm 回退 cn。
const imageDefaultRealm = "global"

// maxEditUploadBytes 图片编辑入站请求体上限（multipart 解析用）。
// OpenAI 侧图片是 base64 传输，1024×1536 的 PNG 约 2–5 MB，多图 + 膨胀留足余量。
const maxEditUploadBytes = 64 << 20

// resolveImageRealm 解析生图请求的 realm：
//   - 显式 "global:" / "cn:" 前缀 → 用前缀（与 chat 同协议）；
//   - 无前缀 → 默认 global；global 域不可服务时回退 cn（零配置可用性优先）。
func (h *Handler) resolveImageRealm(model string) (string, string) {
	realm, bare := resolveModel(model)
	// resolveModel 对无前缀恒返回 "cn"，无法区分「显式 cn:」与「未写」，故此处自判前缀。
	if strings.HasPrefix(model, "cn:") || strings.HasPrefix(model, "global:") {
		return realm, bare
	}
	if h.cfg.Pool != nil && h.cfg.Pool.ServableForRealm(imageDefaultRealm) {
		return imageDefaultRealm, bare
	}
	return "cn", bare
}

// imageCall 一次出站调用（generations 与 edits 各传各的方法）。
type imageCall func(ctx context.Context, a *auth.Auth, body []byte, clientIP string) ([]byte, int, error)

// runImage 生图/图生图共用的执行体：选号轮转 → token 刷新 → 出站 → 错误处置 →
// 归一化 → 写出 OpenAI 响应。两个端点只在「请求体构造」与「出站方法」上不同。
func (h *Handler) runImage(
	w http.ResponseWriter,
	r *http.Request,
	kind string,
	bareModel, realm string,
	upstreamBody []byte,
	call imageCall,
) {
	clientIP := upstream.ExtractClientIP(r)
	tried := map[string]bool{}
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()

	var lastErr error
	var lastStatus int
	var modelBlock pool.ModelBlockStatus

	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcludingForRealm(tried, bareModel, realm)
		if acct == nil {
			// 池内无可用号，或全部号对该模型处于模型级冷却（6004/11102）。
			modelBlock = h.cfg.Pool.ModelBlocked(bareModel)
			break
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		heldUID = acct.UID

		// token 临近过期先刷新（失败按既有策略处置并换号）。
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				h.cfg.Pool.Release(acct.UID)
				heldUID = ""
				if !rotateBackoff(i, r.Context()) {
					break
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("ERR: [server] %s refresh acct=%s: save auth failed: %v",
					kind, logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		raw, status, terr := call(r.Context(), acct, upstreamBody, clientIP)
		h.cfg.Pool.Release(acct.UID)
		heldUID = ""

		if terr != nil {
			lastErr = terr
			lastStatus = status
			var ue *upstream.Error
			if errors.As(terr, &ue) {
				lastStatus = ue.Status
				// 与 chat 同口径的账号处置（冷却/熔断/禁用/内容拦截零动作）。
				h.applyErrorPolicy(acct.UID, ue.Kind, string(raw), bareModel, ue)
			}
			// 传输层故障：只换号，不喂熔断（同 chat 语义）。
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}

		res, nerr := upstream.NormalizeImageResponse(raw, time.Now().Unix())
		if nerr != nil {
			lastErr = nerr
			lastStatus = http.StatusBadGateway
			var ue *upstream.Error
			if errors.As(nerr, &ue) {
				h.applyErrorPolicy(acct.UID, ue.Kind, string(raw), bareModel, ue)
			}
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}

		log.Printf("| %s | uid=%s model=%s realm=%s n=%d credit=%.2f |",
			kind, logfmt.Label(acct.UID, acct.Nickname), bareModel, realm, res.Count, res.Credit)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.Body)
		return
	}

	// 轮转耗尽：按既有错误语义出口。
	if lastErr == nil {
		code := "no_healthy_account"
		msg := "no available account for image generation"
		if modelBlock.Blocked {
			code = "model_unavailable"
			msg = fmt.Sprintf("all accounts are rate-limited for model %s (%d cooling, reason=%s)",
				bareModel, modelBlock.Count, strings.TrimSpace(modelBlock.Reason))
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, code, msg)
		return
	}
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	writeOpenAIError(w, lastStatus, "upstream_error", lastErr.Error())
}

// ---------------------------------------------------------------------------
// POST /v1/images/generations —— 文本生图
// ---------------------------------------------------------------------------

// imagesGenerations 入站（OpenAI 兼容）：
//
//	{"model":"hy-image-3.5","prompt":"...","size":"1024x1536","quality":"high"}
//
// 出站响应（OpenAI 兼容）：
//
//	{"created":1700000000,"data":[{"b64_json":"..."}],"usage":{"credit":5.0}}
func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var in upstream.ImageGenRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}

	realm, bareModel := h.resolveImageRealm(in.Model)
	if bareModel == "" {
		// 按 realm 选默认模型：global → hy-image-3.5；cn → hunyuan-image-alpha。
		bareModel = upstream.DefaultTextToImageModelFor(realm)
	}
	if h.rejectHiddenImageModel(w, realm, bareModel) {
		return
	}
	upstreamBody, err := upstream.BuildImageUpstreamBody(&in, bareModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	h.runImage(w, r, "image", bareModel, realm, upstreamBody, h.cfg.Upstream.GenerateImage)
}

// ---------------------------------------------------------------------------
// POST /v1/images/edits —— 图片编辑 / 图生图
// ---------------------------------------------------------------------------

// imagesEdits 支持两种入站形态：
//
//	① multipart/form-data（OpenAI 标准）
//	   image=<file>[,image=<file>...]  prompt=...  model=...  size=...  input_fidelity=...
//	   image 字段也可直接给文本（URL / data URL / 本地路径），与文件混用。
//
//	② application/json（网关原生，便于脚本化）
//	   {"model":"...","prompt":"...","image":["<URL|dataURL|path|base64>",...]}
//
// 两者最终都转成上游的 JSON：image = ["data:image/png;base64,...", ...]。
func (h *Handler) imagesEdits(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	var (
		in     upstream.ImageEditRequest
		images []string
	)

	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxEditUploadBytes); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse multipart: "+err.Error())
			return
		}
		in.Model = r.FormValue("model")
		in.Prompt = r.FormValue("prompt")
		in.Size = r.FormValue("size")
		in.Quality = r.FormValue("quality")
		in.Style = r.FormValue("style")
		in.Background = r.FormValue("background")
		in.InputFidelity = r.FormValue("input_fidelity")
		if v := strings.TrimSpace(r.FormValue("n")); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				in.N = n
			}
		}

		// ① 文本形态的 image（URL / data URL / 本地路径 / 裸 base64）
		if r.MultipartForm != nil {
			for _, v := range r.MultipartForm.Value["image"] {
				if strings.TrimSpace(v) == "" {
					continue
				}
				d, err := h.cfg.Upstream.NormalizeImageInput(r.Context(), v)
				if err != nil {
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image: "+err.Error())
					return
				}
				images = append(images, d)
			}
			// ② 文件形态的 image（OpenAI 标准用法）
			for _, fh := range r.MultipartForm.File["image"] {
				f, err := fh.Open()
				if err != nil {
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "open upload: "+err.Error())
					return
				}
				raw, rerr := io.ReadAll(io.LimitReader(f, maxImageInputBytesServer+1))
				f.Close()
				if rerr != nil {
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read upload: "+rerr.Error())
					return
				}
				if len(raw) > maxImageInputBytesServer {
					writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request",
						fmt.Sprintf("image too large: %d bytes (limit %d)", len(raw), maxImageInputBytesServer))
					return
				}
				images = append(images, upstream.DataURLFromBytes(raw, fh.Filename))
			}
		}
	} else {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &in); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
			return
		}
		for _, img := range in.Image {
			if strings.TrimSpace(img) == "" {
				continue
			}
			d, err := h.cfg.Upstream.NormalizeImageInput(r.Context(), img)
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image: "+err.Error())
				return
			}
			images = append(images, d)
		}
	}

	if strings.TrimSpace(in.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	if len(images) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"image is required (multipart field `image`, or JSON array `image`)")
		return
	}

	realm, bareModel := h.resolveImageRealm(in.Model)
	if bareModel == "" {
		// 按 realm 选默认模型：global → hy-image-3.5；cn → hunyuan-image-alpha-edit。
		bareModel = upstream.DefaultImageToImageModelFor(realm)
	}
	if h.rejectHiddenImageModel(w, realm, bareModel) {
		return
	}
	upstreamBody, err := upstream.BuildImageEditUpstreamBody(&in, bareModel, images)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	h.runImage(w, r, "image-edit", bareModel, realm, upstreamBody, h.cfg.Upstream.GenerateImageEdit)
}

// maxImageInputBytesServer 与 upstream.maxImageInputBytes 同值（server 侧上传校验用）。
const maxImageInputBytesServer = 32 << 20

// ---------------------------------------------------------------------------
// GET /v1/images/models —— 生成类模型清单（生图/生视频）
// ---------------------------------------------------------------------------

// generationModelsTTL / generationModelsFailCooldown 清单缓存时长。
// 独立于 dynamicModelsCache（10min）：本缓存**保留**生成类模型，且端点调用频率远低
// 于 /v1/models，故用更长 TTL 少打上游。
const (
	generationModelsTTL          = time.Hour
	generationModelsFailCooldown = 5 * time.Minute
)

// generationModelsCache 按 realm 缓存的生成类模型清单。
var generationModelsCache = struct {
	sync.RWMutex
	byRealm map[string]*generationModelsEntry
}{byRealm: map[string]*generationModelsEntry{}}

type generationModelsEntry struct {
	infos    []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// imagesModels 返回生成类模型清单（生图/生视频）。
//
// **为什么是独立端点，而不是给 /v1/models 加 ?include_non_chat=1**：
//   - 生成类模型被 nonChatModel 从聊天目录剔除是**正确行为**（它们当对话模型发到
//     /chat/completions 会被上游拒，实测 code=11103 "Backend [maas] is not supported"），
//     不该为了暴露它们去改聊天目录的代码路径；
//   - 独立端点零侵入：/v1/models 及其探测链路一行不改，本功能全在新文件里。
//
// 响应沿用 OpenAI /v1/models 形状（object/id/created/owned_by），另加：
//   - generation_model: true —— 客户端据此把它路由到 images 端点而非 chat；
//   - tags / name / credits —— 上游原值透出；
//   - endpoints —— 该模型可用的网关端点提示。
func (h *Handler) imagesModels(w http.ResponseWriter, r *http.Request) {
	data := make([]map[string]any, 0, 6)
	for _, realm := range [...]string{"cn", "global"} {
		if realm == "global" && !h.cfg.GlobalEnabled {
			continue
		}
		for _, mi := range h.generationModelsFor(realm) {
			if h.hiddenModel(realm, mi.ID) {
				continue
			}
			data = append(data, generationModelEntry(realm+":"+mi.ID, mi))
		}
	}
	if len(data) == 0 {
		// 探测全失败（无号/网络错/目录无生成类模型）：回落静态清单，
		// 保证客户端至少能拿到可用的模型名（生图端点不看目录，指定名即可调用）。
		for _, e := range generationModelStaticFallback() {
			if realm, bare := resolveModel(e["id"].(string)); !h.hiddenModel(realm, bare) {
				data = append(data, e)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// generationModelsFor 取某 realm 的生成类模型（带 1h 缓存 + 5min 负缓存）。
func (h *Handler) generationModelsFor(realm string) []upstream.ModelInfo {
	generationModelsCache.RLock()
	if e := generationModelsCache.byRealm[realm]; e != nil {
		if len(e.infos) > 0 && time.Since(e.fetched) < generationModelsTTL {
			out := e.infos
			generationModelsCache.RUnlock()
			return out
		}
		if !e.lastFail.IsZero() && time.Since(e.lastFail) < generationModelsFailCooldown {
			generationModelsCache.RUnlock()
			return nil
		}
	}
	generationModelsCache.RUnlock()

	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", realm)
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchGenerationModels(acct)
	if err != nil || len(infos) == 0 {
		log.Printf("[images] generation model probe failed realm=%s: %v", realm, err)
		generationModelsCache.Lock()
		generationModelsCache.byRealm[realm] = &generationModelsEntry{lastFail: time.Now()}
		generationModelsCache.Unlock()
		return nil
	}
	generationModelsCache.Lock()
	generationModelsCache.byRealm[realm] = &generationModelsEntry{infos: infos, fetched: time.Now()}
	generationModelsCache.Unlock()
	return infos
}

// generationModelEntry 把上游 ModelInfo 转成 /v1/images/models 的条目。
func generationModelEntry(id string, mi upstream.ModelInfo) map[string]any {
	entry := map[string]any{
		"id":               id,
		"object":           "model",
		"created":          1753600000,
		"owned_by":         "workbuddy",
		"generation_model": true,
	}
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits
	}
	if mi.Description != "" {
		entry["description"] = mi.Description
	}
	// 端点提示：客户端据此知道该模型该发到哪个网关端点。
	eps := make([]string, 0, 2)
	for _, t := range mi.Tags {
		switch t {
		case "text-to-image":
			eps = append(eps, "POST /v1/images/generations")
		case "image-to-image":
			eps = append(eps, "POST /v1/images/edits")
		}
	}
	if len(eps) > 0 {
		entry["endpoints"] = eps
	}
	return entry
}

// generationModelStaticFallback 探测全失败时的静态兜底清单。
// 只列模型名与能力标签——生图端点不读目录，指定模型名即可正常调用。
func generationModelStaticFallback() []map[string]any {
	return []map[string]any{
		{
			"id": "global:hy-image-3.5", "object": "model", "created": 1753600000,
			"owned_by": "workbuddy", "generation_model": true,
			"name":      "HY Image 3.5",
			"tags":      []string{"text-to-image"},
			"endpoints": []string{"POST /v1/images/generations"},
		},
		{
			"id": "cn:hunyuan-image-alpha-edit", "object": "model", "created": 1753600000,
			"owned_by": "workbuddy", "generation_model": true,
			"name":      "Hunyuan Image Alpha Edit",
			"tags":      []string{"image-to-image"},
			"endpoints": []string{"POST /v1/images/edits"},
		},
		{
			"id": "cn:hunyuan-image-alpha", "object": "model", "created": 1753600000,
			"owned_by": "workbuddy", "generation_model": true,
			"name":      "Hunyuan Image Alpha",
			"tags":      []string{"text-to-image"},
			"endpoints": []string{"POST /v1/images/generations"},
		},
	}
}

// rejectHiddenImageModel models.hidden 同样管出图入口：列表已隐藏的模型不能直接点名调用
// （出图真扣积分，漏掉这里黑名单就可绕过）。已写出 400 时返回 true。
func (h *Handler) rejectHiddenImageModel(w http.ResponseWriter, realm, bare string) bool {
	if !h.hiddenModel(realm, bare) {
		return false
	}
	writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
		"model is hidden by gateway configuration, try another model")
	return true
}
