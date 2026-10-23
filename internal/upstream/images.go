// images.go 生图（text-to-image / image-to-image）出站实现。
//
// 上游形态（本机桌面版 WorkBuddyAI 实测日志 + 打包源码交叉验证）：
//
//	POST {base}/v2/images/generations   —— 文本生图
//	POST {base}/v2/images/edits         —— 图片编辑（image-to-image）
//
// base 按账号 realm 选：global = https://www.workbuddy.ai（与桌面版同域）；
// cn = copilot.tencent.com。二者与 chat 共用同一 chatBase()，故此处直接复用。
//
// 请求体按模型前缀三分支（对齐桌面端 ImageService.buildBaseRequestBody）：
//
//	openai 分支（默认，gpt-image-*）强制 response_format=b64_json —— 上游回 base64 而非 URL；
//	hunyuan 分支：n（默认 1）+ footnote + revise；
//	gemini 分支：仅 model/prompt/size。
//
// 响应外层是腾讯风格信封 {code,msg,request_id,data:{data:[...],usage:{credit}}}，
// 由 NormalizeImageResponse 归一化为 OpenAI Images API 形态。
package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// imagesGenerationsPath 文本生图路径。CN 与 global 共用 /v2 单路径（与 chat 同约定）。
	imagesGenerationsPath = "/v2/images/generations"
	// imagesEditsPath 图片编辑路径（图生图，与 generations 同域不同路径）。
	imagesEditsPath = "/v2/images/edits"

	// DefaultTextToImageModel 国际版文本生图默认模型。
	//
	// 原默认 gpt-image-2.5-sunburst 已下架（同类项目 2026-10-22 实测 400 code=14401
	// route config not found）；国际版当前可用的出图模型是 hy-image-3.5（只出现在
	// /v3/config 的 modelPromotions 里，实测 200，每张约 0.27 credit）。
	DefaultTextToImageModel = "hy-image-3.5"

	// DefaultImageToImageModel 国际版图生图默认模型。sunburst 下架后国际版没有实测
	// 可用的图生图模型，先与文生图同值；需要稳定图生图请用 cn:hunyuan-image-alpha-edit。
	DefaultImageToImageModel = "hy-image-3.5"

	// DefaultTextToImageModelCN / DefaultImageToImageModelCN CN 域默认生图模型。
	//
	// 实测修正（2026-10-09，桌面端 UA 探测 /v3/config，55 条目录）：
	//   - `hunyuan-image-alpha`      tags=[text-to-image]   → 文生图
	//   - `hunyuan-image-alpha-edit` tags=[image-to-image]  → 图生图
	//
	// **注意**：只有桌面端 UA（`WorkBuddy/<ver> ... CLI/<ver>`）能拿到这两个模型；
	// 旧 IDE UA 单路探测只返回 21 条且两个都不含，故曾误判「CN 无图生图能力」。
	// 目录探测已于 2026-10-09 还原为**单路 IDE UA**（见 fetchV3Models），因此这两个
	// 模型**不会出现在 /v1/models 列表里**——但生图端点不看列表，直接指定模型名即可调用。
	DefaultTextToImageModelCN  = "hunyuan-image-alpha"
	DefaultImageToImageModelCN = "hunyuan-image-alpha-edit"

	// DefaultSize 未传 size 时上游默认画布（桌面端代码内常量 "1024x1024"）。
	//
	// 实测（2026-10-09）：上游有**最小分辨率下限**。512x512 被拒：
	// code=14401 "Invalid size '512x512'. Requested resolution is below the current minimum"。
	// 需要小图请在网关侧生成后再缩放，不要直接传 <1024 的 size。
	DefaultSize = "1024x1024"

	// imageMaxBody 生图响应体读取上限。
	// 必须远大于 doJSON 的 1 MiB：openai 分支回 base64 PNG，1024x1536 一张常在
	// 2–5 MB 量级，用默认上限会把图片静默截断成半个 base64。
	imageMaxBody = 64 << 20
)

// ImageGenRequest 生图入站请求（OpenAI Images API 兼容 + 桌面端扩展字段）。
type ImageGenRequest struct {
	Model          string `json:"model,omitempty"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	Style          string `json:"style,omitempty"`
	Background     string `json:"background,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	// hunyuan 分支专用
	Footnote string `json:"footnote,omitempty"`
	Revise   *struct {
		Value bool `json:"value"`
	} `json:"revise,omitempty"`
}

// DefaultTextToImageModelFor 按 realm 返回文本生图的默认模型：
// global → hy-image-3.5；cn → hunyuan-image-alpha。
// 空/未知 realm 按 global 处理（与 imageDefaultRealm 缺省一致）。
func DefaultTextToImageModelFor(realm string) string {
	if realm == "cn" {
		return DefaultTextToImageModelCN
	}
	return DefaultTextToImageModel
}

// DefaultImageToImageModelFor 按 realm 返回图生图的默认模型（同上口径）。
func DefaultImageToImageModelFor(realm string) string {
	if realm == "cn" {
		return DefaultImageToImageModelCN
	}
	return DefaultImageToImageModel
}

// ImageBranch 返回模型名对应的上游分支（openai / hunyuan / gemini）。
func ImageBranch(model string) string {
	switch {
	case strings.HasPrefix(model, "hunyuan-"):
		return "hunyuan"
	case strings.HasPrefix(model, "gemini-"):
		return "gemini"
	default:
		return "openai"
	}
}

// BuildImageUpstreamBody 按模型前缀构造上游请求体。
// model 应为已剥离 realm 前缀的裸模型名（handler 侧 resolveModel 的 bare 段）。
func BuildImageUpstreamBody(req *ImageGenRequest, model string) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	size := req.Size
	if size == "" {
		size = DefaultSize
	}
	// 用 map 而非结构体：三分支字段集合互斥，且要保证「未传的字段不出现在体里」
	// （上游对多余字段的容忍度未知，桌面端也是按需 append）。
	body := map[string]any{
		"model":  model,
		"prompt": req.Prompt,
		"size":   size,
	}
	switch ImageBranch(model) {
	case "hunyuan":
		n := req.N
		if n <= 0 {
			n = 1 // 桌面端 hunyuan 分支兜底 n=1
		}
		body["n"] = n
		if req.Footnote != "" {
			body["footnote"] = req.Footnote
		}
		if req.Revise != nil {
			body["revise"] = map[string]any{"value": req.Revise.Value}
		}
	case "gemini":
		// gemini 分支只发基础三字段，不透传 n/quality/style/background。
	default: // openai
		// 强制 base64：上游据此回 b64_json 而不是外链 URL（网关统一落盘/回传语义）。
		body["response_format"] = "b64_json"
		if req.N > 0 {
			body["n"] = req.N
		}
		if req.Quality != "" {
			body["quality"] = req.Quality
		}
		if req.Style != "" {
			body["style"] = req.Style
		}
		if req.Background != "" {
			body["background"] = req.Background
		}
	}
	return json.Marshal(body)
}

// ImageHeaders 生图出站头：CommonHeaders 全量 + 账号鉴权 + 会话头族，
// 但 Accept 保持 application/json（生图是非流式；ChatHeaders 的 event-stream 不适用）。
func (c *Client) ImageHeaders(req *http.Request, a *auth.Auth, clientIP string) {
	c.CommonHeaders(req, a)
	// 与 ChatHeaders 同口径剥离客户端 chat 出站从不发的头（见 ChatHeaders 注释）。
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	req.Header.Del("Accept-Language")
	req.Header.Del("X-Machine-ID")
	req.Header.Del("X-Session-ID")
	req.Header.Set("Accept", "application/json")
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 企业与域头按 realm 分发（与 ChatHeaders 同口径）：
	// global 账号统一覆写为国际客户端形态（X-No-Enterprise-Id + X-Domain=www.workbuddy.ai）。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	c.injectAttribution(req)
	c.injectClientIP(req, clientIP)
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/B3）：上游后台按 X-Conversation-Request-ID 聚合，
	// 生图不带会被记成碎片。空 meta 时函数内部自补 32 hex。
	c.injectConversationHeaders(req, a.UID, ChatMeta{})
}

// GenerateImage 发一次文本生图请求（非流式 JSON），返回上游**原始信封**与 HTTP 状态。
// 归一化交给 NormalizeImageResponse，本方法只负责传输 + 错误分类。
//
// 返回值约定：
//   - err != nil 且为 *Error  → 上游已分类错误（status 有效，raw 为原文片段）；
//   - err != nil 且非 *Error  → 传输层故障（连接中断/超时），调用方换号但不应罚号。
func (c *Client) GenerateImage(ctx context.Context, a *auth.Auth, body []byte, clientIP string) (raw []byte, status int, err error) {
	return c.postImages(ctx, a, imagesGenerationsPath, body, clientIP)
}

// GenerateImageEdit 发一次图片编辑（image-to-image）请求，走 /v2/images/edits。
//
// 注意：上游 edits **也是非流式 JSON**（不是 multipart）。桌面端 editImage 源码里
// Content-Type 明确是 application/json，图片以 data URL 字符串数组放在 image 字段。
// 因此本方法与 GenerateImage 共用同一发送链路，只差路径。
func (c *Client) GenerateImageEdit(ctx context.Context, a *auth.Auth, body []byte, clientIP string) (raw []byte, status int, err error) {
	return c.postImages(ctx, a, imagesEditsPath, body, clientIP)
}

// postImages images 域统一出站：路径可变，头/超时/错误分类一致。
func (c *Client) postImages(ctx context.Context, a *auth.Auth, path string, body []byte, clientIP string) (raw []byte, status int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := c.chatBase(a) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	c.ImageHeaders(req, a, clientIP)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// 传输层失败 → 清空共享连接池空闲连接（与 chat 同加固策略）。
		roundTripCloseIdle(c.HTTP.Transport)
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, imageMaxBody))
	if rerr != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", rerr)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return raw, resp.StatusCode, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// 响应归一化：腾讯信封 → OpenAI Images API
// ---------------------------------------------------------------------------

// imageEnvelope 上游外层信封。
//
// **字段名以实测为准（2026-10-09 直连抓包）**：上游用**驼峰 `requestId`**，
// 不是下划线的 `request_id`——曾按桌面端源码推断写成下划线，导致错误对账
// 信息里的 request_id 恒为空（Go 的 tag 匹配要求除大小写外完全一致，
// `request_id` 与 `requestId` 差一个下划线，不匹配）。
//
// 实测原始响应（成功；requestId 已脱敏为占位）：
//
//	{"code":0,"msg":"OK","requestId":"<request-id>",
//	 "data":{"background":"opaque","created":<unix>,
//	         "data":[{"b64_json":"iVBORw0KGgo..."}],
//	         "output_format":"png","quality":"...","size":"1024x1024","usage":{...}}}
//
// 实测原始响应（失败）：
//
//	{"code":14401,"msg":"Create image failed with error: TokenHub returned HTTP 400: {...}",
//	 "requestId":"<request-id>"}
type imageEnvelope struct {
	Code      int             `json:"code"`
	Msg       string          `json:"msg"`
	RequestID string          `json:"requestId"`
	Data      json.RawMessage `json:"data"`
}

// imageItem 单张图：openai/hunyuan 分支给 b64_json，部分分支给 url。
type imageItem struct {
	B64JSON       string `json:"b64_json"`
	URL           string `json:"url"`
	RevisedPrompt string `json:"revised_prompt"`
}

// imageInner 信封 data 段。实测键：background / created / data / output_format /
// quality / size / usage（比最初按桌面端推断的多出若干元字段，一并解析以便透传）。
type imageInner struct {
	Created int64       `json:"created"` // 上游生成时间戳，优先于本地时钟
	Data    []imageItem `json:"data"`
	Usage   struct {
		Credit float64 `json:"credit"`
	} `json:"usage"`
	Size         string `json:"size"`
	Quality      string `json:"quality"`
	OutputFormat string `json:"output_format"`
	Background   string `json:"background"`
}

// ImageResult 归一化后的生图结果。
type ImageResult struct {
	// Body 已序列化的 OpenAI Images API 响应体（{created,data:[...]}）。
	Body []byte
	// Credit 上游 usage.credit（本次消耗积分）；HasCredit 为 false 时无意义。
	Credit    float64
	HasCredit bool
	// Count 返回的图片张数。
	Count int
}

// NormalizeImageResponse 把上游信封归一化为 OpenAI Images API 响应：
//
//	上游 {code,msg,request_id,data:{data:[{b64_json|url}],usage:{credit}}}
//	  → {created,data:[{b64_json|url}],usage:{credit}}
//
// code != 0 返回 *Error（Kind 走 Classify，Msg 带 msg+request_id 便于对账）。
// data.data 为空视为上游异常（桌面端同判："No image data returned from API"）。
func NormalizeImageResponse(raw []byte, created int64) (*ImageResult, error) {
	var env imageEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse envelope: %w (body: %s)", err, truncate(string(raw), 160))
	}
	if env.Code != 0 {
		kind := Classify(http.StatusOK, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{
			Kind:   kind,
			Status: http.StatusBadGateway,
			Msg:    fmt.Sprintf("code=%d msg=%s request_id=%s", env.Code, truncate(env.Msg, 160), env.RequestID),
		}
	}
	var inner imageInner
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &inner); err != nil {
			return nil, fmt.Errorf("parse data: %w (body: %s)", err, truncate(string(env.Data), 160))
		}
	}
	if len(inner.Data) == 0 {
		return nil, fmt.Errorf("no image data returned from upstream (request_id=%s)", env.RequestID)
	}

	out := make([]map[string]any, 0, len(inner.Data))
	for _, it := range inner.Data {
		m := map[string]any{}
		switch {
		case it.B64JSON != "":
			m["b64_json"] = it.B64JSON
		case it.URL != "":
			// url 分支（hunyuan 等）原样透传，由客户端自行下载。
			m["url"] = it.URL
		default:
			return nil, fmt.Errorf("image item contains neither base64 nor URL (request_id=%s)", env.RequestID)
		}
		if it.RevisedPrompt != "" {
			m["revised_prompt"] = it.RevisedPrompt
		}
		out = append(out, m)
	}

	// created 优先用上游值（实测 data.created 存在）；上游缺失才回落调用方传入的本地时钟。
	createdAt := created
	if inner.Created > 0 {
		createdAt = inner.Created
	}
	resp := map[string]any{"created": createdAt, "data": out}
	if inner.Usage.Credit > 0 {
		resp["usage"] = map[string]any{"credit": inner.Usage.Credit}
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &ImageResult{
		Body:      body,
		Credit:    inner.Usage.Credit,
		HasCredit: inner.Usage.Credit > 0,
		Count:     len(out),
	}, nil
}

// ---------------------------------------------------------------------------
// 图片编辑（image-to-image）
// ---------------------------------------------------------------------------

// ImageEditRequest 图片编辑入站请求。兼容两种入站形态：
//   - multipart/form-data（OpenAI 标准，server 侧解析后填充本结构）；
//   - application/json（网关原生，image 直接给字符串数组）。
//
// Image 每项可为：data URL / http(s) URL / 裸 base64 / 本地文件路径，
// 由 NormalizeImageInput 统一转成上游要的 data URL。
type ImageEditRequest struct {
	Model         string   `json:"model,omitempty"`
	Prompt        string   `json:"prompt"`
	Image         []string `json:"image"`
	Mask          string   `json:"mask,omitempty"` // 预留：上游 editImage 未见 mask 处理
	N             int      `json:"n,omitempty"`
	Size          string   `json:"size,omitempty"`
	Quality       string   `json:"quality,omitempty"`
	Style         string   `json:"style,omitempty"`
	Background    string   `json:"background,omitempty"`
	InputFidelity string   `json:"input_fidelity,omitempty"`
}

// BuildImageEditUpstreamBody 构造 /v2/images/edits 请求体。
// 基础字段复用 generations 的三分支构造器（同 model 前缀规则），再叠加 image 数组与
// input_fidelity —— 与桌面端 editImage 的实现逐字对应。
func BuildImageEditUpstreamBody(req *ImageEditRequest, model string, images []string) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	base, err := BuildImageUpstreamBody(&ImageGenRequest{
		Model:      req.Model,
		Prompt:     req.Prompt,
		N:          req.N,
		Size:       req.Size,
		Quality:    req.Quality,
		Style:      req.Style,
		Background: req.Background,
	}, model)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(base, &body); err != nil {
		return nil, err
	}
	if len(images) > 0 {
		body["image"] = images
	}
	if req.InputFidelity != "" {
		body["input_fidelity"] = req.InputFidelity
	}
	return json.Marshal(body)
}

// maxImageInputBytes 单张图片输入上限。桌面端无显式限制，此处给 32 MiB 防御值：
// data URL 会膨胀约 4/3，仍在 imageMaxBody(64 MiB) 的响应读取上限之内。
const maxImageInputBytes = 32 << 20

// NormalizeImageInput 把各种形态的图片输入统一转成上游要的 data URL。
// 复刻桌面端 ImageService.processImageInput 的四分支：
//
//	data:image/...;base64,...  → 原样返回
//	http(s)://...              → GET 下载 → 按 content-type（缺失时按 magic bytes）转 data URL
//	疑似裸 base64              → magic bytes 探测 mime → data URL
//	其它                        → 当作本地文件路径读取 → 按扩展名定 mime → data URL
//
// 已知取舍（与桌面端一致）：疑似 base64 的判定排除了含 `/` 的字符串，因此**标准
// base64（字符集含 /）会被优先当作文件路径**。需要传裸 base64 时请用 data URL 前缀，
// 或改用 URL-safe base64（`-`/`_`，不含 `/`）。
func (c *Client) NormalizeImageInput(ctx context.Context, in string) (string, error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", fmt.Errorf("empty image input")
	}
	switch {
	case strings.HasPrefix(s, "data:"):
		if !strings.HasPrefix(s, "data:image/") {
			return "", fmt.Errorf("unsupported data URL (only image/* accepted)")
		}
		return s, nil
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
		return c.fetchImageAsDataURL(ctx, s)
	case isLikelyBase64(s):
		return "data:" + detectImageMIMEFromBase64(s) + ";base64," + s, nil
	default:
		return readImageFileAsDataURL(s)
	}
}

// fetchImageAsDataURL 下载远端图片并转 data URL。
func (c *Client) fetchImageAsDataURL(ctx context.Context, url string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("download image: upstream %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxImageInputBytes))
	if err != nil {
		return "", fmt.Errorf("read image: %w", err)
	}
	mime := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.TrimSpace(mime)
	if !strings.HasPrefix(mime, "image/") {
		mime = detectImageMIME(raw)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// readImageFileAsDataURL 读取本地图片文件并转 data URL。
func readImageFileAsDataURL(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("image file not found: %s", path)
	}
	if st.Size() > maxImageInputBytes {
		return "", fmt.Errorf("image too large: %d bytes (limit %d)", st.Size(), maxImageInputBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read image file: %w", err)
	}
	mime := mimeByExt(path)
	if !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("unsupported file type: %s (only image files are supported)", mime)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// isLikelyBase64 与桌面端同判：不含路径分隔符、长度≥100、只由 base64 字符集构成。
func isLikelyBase64(s string) bool {
	if strings.ContainsAny(s, "/\\") {
		return false
	}
	if len(s) < 100 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '+', ch == '=', ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}

// detectImageMIME 按 magic bytes 判定图片类型（复刻桌面端 detectMimeTypeFromBase64）。
func detectImageMIME(b []byte) string {
	switch {
	case len(b) >= 4 && b[0] == 0x89 && b[1] == 0x50 && b[2] == 0x4E && b[3] == 0x47:
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 4 && b[0] == 0x47 && b[1] == 0x49 && b[2] == 0x46 && b[3] == 0x38:
		return "image/gif"
	case len(b) >= 12 && b[0] == 0x52 && b[1] == 0x49 && b[2] == 0x46 && b[3] == 0x46 &&
		b[8] == 0x57 && b[9] == 0x45 && b[10] == 0x42 && b[11] == 0x50:
		return "image/webp"
	case len(b) >= 2 && b[0] == 0x42 && b[1] == 0x4D:
		return "image/bmp"
	}
	return "image/png" // 桌面端同兜底
}

// detectImageMIMEFromBase64 解出前 32 字符的头部字节后判类型。
func detectImageMIMEFromBase64(s string) string {
	n := 32
	if len(s) < n {
		n = len(s)
	}
	n -= n % 4 // 对齐 base64 块
	if n <= 0 {
		return "image/png"
	}
	raw, err := base64.StdEncoding.DecodeString(s[:n])
	if err != nil {
		return "image/png"
	}
	return detectImageMIME(raw)
}

// mimeByExt 扩展名 → MIME（与桌面端 getMimeType 同表）。
func mimeByExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".tiff", ".tif":
		return "image/tiff"
	}
	return "application/octet-stream"
}

// DataURLFromBytes 把原始图片字节转成 data URL：MIME 优先按文件名扩展名判定，
// 扩展名缺失或非图片时按 magic bytes 判定（供 multipart 上传路径使用）。
func DataURLFromBytes(raw []byte, filename string) string {
	mime := ""
	if filename != "" {
		if m := mimeByExt(filename); strings.HasPrefix(m, "image/") {
			mime = m
		}
	}
	if mime == "" {
		mime = detectImageMIME(raw)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// ---------------------------------------------------------------------------
// 生成类模型探测（GET /v1/images/models 的数据源）
// ---------------------------------------------------------------------------

// generationTags 生成类标签：命中即视为"走专用端点、不能当对话模型"的模型。
var generationTags = [...]string{"text-to-image", "image-to-image", "text-to-video", "image-to-video"}

// IsGenerationModel 判定 tags 是否含生成类标签（图片/视频）。
//
// 与 nonChatModel 的关系：nonChatModel 用它来**剔除**这些模型（避免它们进聊天目录，
// 当对话模型发上去会被上游拒——实测 code=11103 "Backend [maas] is not supported"）；
// 本函数用于**反向筛选**，供独立端点 /v1/images/models 把它们单独列出来。
func IsGenerationModel(tags []string) bool {
	for _, t := range tags {
		for _, g := range generationTags {
			if t == g {
				return true
			}
		}
	}
	return false
}

// imageDeadModels 上游已下架、没有路由的出图模型：列出来只会让客户端选到一个必然
// 400 的模型。gpt-image-2.5-sunburst：400 code=14401 route config not found
// （同类项目 2026-10-22 实测）。模型恢复时删一行即可；未知模型照旧列出。
var imageDeadModels = map[string]bool{
	"gpt-image-2.5-sunburst": true,
}

// FetchGenerationModels 独立探测**生成类模型**目录（生图/生视频）。
//
// 为什么不能复用 FetchModels：后者按 nonChatModel 规则把生成类模型剔除，所以
// 生成类模型只能由本方法单独探测。二者互不影响——/v1/models 保持聊天目录整洁。
//
// UA 选择：v3/config 对 UA 敏感且各 UA 下发的模型集合不同。CN 域桌面端 UA 目录最全
// （2026-10-09 实测 55 条）；global 域桌面端 UA 现已不含生成类模型，只有 IDE UA 下发
// （2026-10-21 实测）。故按 桌面端 → IDE → CLI 依次探测、取并集，任一路成功即可。
//
// 无号/网络错/目录里没有生成类模型 → 返回错误（调用方降级为静态兜底清单）。
func (c *Client) FetchGenerationModels(a *auth.Auth) ([]ModelInfo, error) {
	byID := map[string]ModelInfo{}
	var errs []string
	for _, ua := range []string{c.defaultWorkBuddyUAFor(a), codeBuddyIDEUA, codeBuddyCLIUA} {
		m, err := c.fetchV3ConfigModelMap(a, ua)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for id, mi := range m {
			if _, seen := byID[id]; !seen && IsGenerationModel(mi.Tags) && !imageDeadModels[id] {
				byID[id] = mi
			}
		}
	}
	out := make([]ModelInfo, 0, len(byID))
	for _, mi := range byID {
		out = append(out, mi)
	}
	if len(out) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("no generation model in v3/config directory: %s", strings.Join(errs, "; "))
		}
		return nil, fmt.Errorf("no generation model in v3/config directory")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ImagesEditsPathForTest 暴露编辑路径（回归哨兵）。
func ImagesEditsPathForTest() string { return imagesEditsPath }
