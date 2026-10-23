// images_test.go 生图出站层单测：请求体三分支构造 + 腾讯信封归一化。
// 纯函数测试，不打上游、不消耗积分。
package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// decodeMap 把构造出的请求体解成 map 便于按字段断言。
func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal body: %v (raw=%s)", err, raw)
	}
	return m
}

func TestImageBranch(t *testing.T) {
	cases := []struct{ model, want string }{
		{"gpt-image-2.5-sunburst", "openai"},
		{"gpt-image-1", "openai"},
		{"hunyuan-image-v3.0", "hunyuan"},
		{"gemini-3.0-pro-image", "gemini"},
		{"", "openai"},
	}
	for _, c := range cases {
		if got := ImageBranch(c.model); got != c.want {
			t.Errorf("ImageBranch(%q)=%q want %q", c.model, got, c.want)
		}
	}
}

// openai 分支必须强制 response_format=b64_json（上游据此回 base64 而非外链）。
func TestBuildImageUpstreamBodyOpenAIForcesB64(t *testing.T) {
	req := &ImageGenRequest{Prompt: "a cat", Size: "1024x1536", Quality: "high", N: 2, Style: "vivid", Background: "opaque"}
	m := decodeMap(t, mustBuild(t, req, "gpt-image-2.5-sunburst"))

	if m["response_format"] != "b64_json" {
		t.Errorf("response_format=%v want b64_json", m["response_format"])
	}
	if m["model"] != "gpt-image-2.5-sunburst" {
		t.Errorf("model=%v", m["model"])
	}
	if m["prompt"] != "a cat" {
		t.Errorf("prompt=%v", m["prompt"])
	}
	if m["size"] != "1024x1536" {
		t.Errorf("size=%v", m["size"])
	}
	if m["quality"] != "high" || m["style"] != "vivid" || m["background"] != "opaque" {
		t.Errorf("openai 扩展字段缺失: %v", m)
	}
	if m["n"] != float64(2) {
		t.Errorf("n=%v want 2", m["n"])
	}
}

// openai 分支：未传的可选字段不得出现在请求体里。
func TestBuildImageUpstreamBodyOpenAIOmitsAbsent(t *testing.T) {
	m := decodeMap(t, mustBuild(t, &ImageGenRequest{Prompt: "x"}, "gpt-image-1"))
	for _, k := range []string{"n", "quality", "style", "background", "footnote", "revise"} {
		if _, ok := m[k]; ok {
			t.Errorf("字段 %s 不应出现（未传）", k)
		}
	}
}

// size 缺省回落 1024x1024（桌面端代码内常量）。
func TestBuildImageUpstreamBodyDefaultSize(t *testing.T) {
	m := decodeMap(t, mustBuild(t, &ImageGenRequest{Prompt: "x"}, "gpt-image-1"))
	if m["size"] != DefaultSize {
		t.Errorf("size=%v want %q", m["size"], DefaultSize)
	}
}

// hunyuan 分支：n 兜底 1，带 footnote/revise，不带 response_format。
func TestBuildImageUpstreamBodyHunyuan(t *testing.T) {
	req := &ImageGenRequest{Prompt: "x", Footnote: "fn", Revise: &struct {
		Value bool `json:"value"`
	}{Value: true}}
	m := decodeMap(t, mustBuild(t, req, "hunyuan-image-v3.0"))

	if m["n"] != float64(1) {
		t.Errorf("hunyuan n 兜底=%v want 1", m["n"])
	}
	if m["footnote"] != "fn" {
		t.Errorf("footnote=%v", m["footnote"])
	}
	if _, ok := m["revise"]; !ok {
		t.Error("revise 缺失")
	}
	if _, ok := m["response_format"]; ok {
		t.Error("hunyuan 分支不应带 response_format")
	}
}

// gemini 分支：只发基础三字段。
func TestBuildImageUpstreamBodyGemini(t *testing.T) {
	req := &ImageGenRequest{Prompt: "x", N: 3, Quality: "high", Style: "vivid"}
	m := decodeMap(t, mustBuild(t, req, "gemini-3.0-pro-image"))

	if len(m) != 3 {
		t.Errorf("gemini 分支字段数=%d want 3 (%v)", len(m), m)
	}
	for _, k := range []string{"model", "prompt", "size"} {
		if _, ok := m[k]; !ok {
			t.Errorf("gemini 缺基础字段 %s", k)
		}
	}
}

func mustBuild(t *testing.T, req *ImageGenRequest, model string) []byte {
	t.Helper()
	raw, err := BuildImageUpstreamBody(req, model)
	if err != nil {
		t.Fatalf("BuildImageUpstreamBody: %v", err)
	}
	return raw
}

// ---------------------------------------------------------------------------
// 响应归一化
// ---------------------------------------------------------------------------

// 正常信封 → OpenAI Images API 形态（b64 分支）。
func TestNormalizeImageResponseB64(t *testing.T) {
	raw := []byte(`{"code":0,"msg":"ok","request_id":"req-1","data":{"data":[{"b64_json":"QUJD"}],"usage":{"credit":5.5}}}`)
	res, err := NormalizeImageResponse(raw, 1700000000)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Count != 1 {
		t.Errorf("count=%d want 1", res.Count)
	}
	if !res.HasCredit || res.Credit != 5.5 {
		t.Errorf("credit=%v hasCredit=%v want 5.5/true", res.Credit, res.HasCredit)
	}
	var out struct {
		Created int64 `json:"created"`
		Data    []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Usage struct {
			Credit float64 `json:"credit"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	if out.Created != 1700000000 {
		t.Errorf("created=%d", out.Created)
	}
	if len(out.Data) != 1 || out.Data[0].B64JSON != "QUJD" {
		t.Errorf("data=%+v", out.Data)
	}
	if out.Usage.Credit != 5.5 {
		t.Errorf("usage.credit=%v", out.Usage.Credit)
	}
}

// url 分支原样透传。
func TestNormalizeImageResponseURL(t *testing.T) {
	raw := []byte(`{"code":0,"msg":"ok","data":{"data":[{"url":"https://example.com/a.png"}]}}`)
	res, err := NormalizeImageResponse(raw, 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(string(res.Body), `"url":"https://example.com/a.png"`) {
		t.Errorf("body=%s", res.Body)
	}
	if res.HasCredit {
		t.Error("无 usage.credit 时 HasCredit 应为 false")
	}
}

// code != 0 → *Error（带 request_id 便于对账）。
func TestNormalizeImageResponseBizError(t *testing.T) {
	// 字段名对齐上游实测：驼峰 requestId（不是下划线 request_id）。
	raw := []byte(`{"code":10001,"msg":"no permission","requestId":"req-9"}`)
	_, err := NormalizeImageResponse(raw, 1)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *upstream.Error, got %T", err)
	}
	if !strings.Contains(ue.Msg, "10001") || !strings.Contains(ue.Msg, "req-9") {
		t.Errorf("msg=%q 应含 code 与 requestId", ue.Msg)
	}
}

// 上游真实响应形状回归（2026-10-09 直连 https://www.workbuddy.ai/v2/images/generations 抓包）：
// 外层腾讯信封 {code,msg,requestId,data}；图片在 data.data[]；created 在 data.created；
// 另含 background/output_format/quality/size 等元字段。本地传入的 created 应被上游值覆盖。
func TestNormalizeImageResponseRealUpstreamShape(t *testing.T) {
	// requestId 用占位值（真实抓包值已脱敏），仅保留信封结构用于回归。
	raw := []byte(`{"code":0,"msg":"OK","requestId":"<request-id>",
		"data":{"background":"opaque","created":1791555584,
		"data":[{"b64_json":"iVBORw0KGgo="}],
		"output_format":"png","quality":"high","size":"1024x1024",
		"usage":{"credit":0.6}}}`)
	res, err := NormalizeImageResponse(raw, 999) // 999 = 本地时钟，应被上游 created 覆盖
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(res.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got["created"] != float64(1791555584) {
		t.Errorf("created=%v want 1791555584（应取上游 data.created）", got["created"])
	}
	arr, _ := got["data"].([]any)
	if len(arr) != 1 {
		t.Fatalf("data 长度=%d want 1", len(arr))
	}
	item, _ := arr[0].(map[string]any)
	if item["b64_json"] != "iVBORw0KGgo=" {
		t.Errorf("b64_json=%v", item["b64_json"])
	}
	u, _ := got["usage"].(map[string]any)
	if u["credit"] != 0.6 {
		t.Errorf("credit=%v want 0.6", u["credit"])
	}
	if res.Count != 1 || res.Credit != 0.6 {
		t.Errorf("Count=%d Credit=%v want 1 / 0.6", res.Count, res.Credit)
	}
}

// data.data 为空 → 上游异常（桌面端同判）。
func TestNormalizeImageResponseEmptyData(t *testing.T) {
	raw := []byte(`{"code":0,"msg":"ok","data":{"data":[]}}`)
	if _, err := NormalizeImageResponse(raw, 1); err == nil {
		t.Fatal("空 data 应报错")
	}
}

// 单张图既无 b64 也无 url → 报错。
func TestNormalizeImageResponseNoPayload(t *testing.T) {
	raw := []byte(`{"code":0,"msg":"ok","data":{"data":[{}]}}`)
	if _, err := NormalizeImageResponse(raw, 1); err == nil {
		t.Fatal("既无 base64 也无 URL 应报错")
	}
}

// 非 JSON → 解析错误（不 panic）。
func TestNormalizeImageResponseMalformed(t *testing.T) {
	if _, err := NormalizeImageResponse([]byte(`not json`), 1); err == nil {
		t.Fatal("畸形 body 应报错")
	}
}

// 生图路径常量锚定（上游形态变更时的回归哨兵）。
func TestImagesPaths(t *testing.T) {
	if imagesGenerationsPath != "/v2/images/generations" {
		t.Errorf("generations path=%q", imagesGenerationsPath)
	}
	if ImagesEditsPathForTest() != "/v2/images/edits" {
		t.Errorf("edits path=%q", ImagesEditsPathForTest())
	}
}

// ---------------------------------------------------------------------------
// 生成类模型识别（供 GET /v1/images/models 独立端点用）
//
// 注意：这里**不改** nonChatModel 的语义——聊天目录依旧按 nonChatModel 规则剔除
// 生成类模型（它们当对话模型发到 /chat/completions 会被上游拒，实测 code=11103）。
// 生成类模型通过独立端点暴露，不在 /v1/models 里出现。
// ---------------------------------------------------------------------------

// IsGenerationModel 只认生成类标签。
func TestIsGenerationModel(t *testing.T) {
	if !IsGenerationModel([]string{"text-to-image"}) {
		t.Error("text-to-image 应判生成类")
	}
	if !IsGenerationModel([]string{"image-to-video"}) {
		t.Error("image-to-video 应判生成类")
	}
	if IsGenerationModel([]string{"craft"}) {
		t.Error("craft 不应判生成类")
	}
	if IsGenerationModel(nil) {
		t.Error("nil 不应判生成类")
	}
}

// 生成类标签与对话标签的边界：生成类模型确实会被 nonChatModel 剔除（既有行为，
// 本功能不改动它），这里用包内 nonChatModel 交叉验证两者口径一致。
func TestGenerationTagsAlsoNonChat(t *testing.T) {
	cases := []struct {
		name string
		id   string
		out  int64
		tags []string
	}{
		{"生图模型", "gpt-image-2.5-sunburst", 0, []string{"text-to-image", "image-to-image"}},
		{"生视频模型", "seedance-2.5", 0, []string{"text-to-video", "image-to-video"}},
		{"图生图 tag", "some-model", 0, []string{"image-to-image"}},
	}
	for _, c := range cases {
		if !IsGenerationModel(c.tags) {
			t.Errorf("%s: IsGenerationModel 应为 true", c.name)
		}
		if !nonChatModel(c.id, c.out, c.tags) {
			t.Errorf("%s: nonChatModel 应为 true（生成类不该进聊天目录）", c.name)
		}
	}
	// 对话模型：既非生成类，也不该被 nonChatModel 剔除。
	if IsGenerationModel([]string{"craft"}) || nonChatModel("glm-5.3", 131072, []string{"craft"}) {
		t.Error("craft 标签的对话模型不应被当作生成类/非对话模型")
	}
}

// 按 realm 的默认生图模型（CN 用 hunyuan 系列，global 用 hy-image-3.5）。
func TestDefaultModelsForRealm(t *testing.T) {
	if got := DefaultTextToImageModelFor("global"); got != "hy-image-3.5" {
		t.Errorf("global text2img=%q", got)
	}
	if got := DefaultTextToImageModelFor("cn"); got != "hunyuan-image-alpha" {
		t.Errorf("cn text2img=%q", got)
	}
	if got := DefaultImageToImageModelFor("cn"); got != "hunyuan-image-alpha-edit" {
		t.Errorf("cn img2img=%q", got)
	}
	if got := DefaultImageToImageModelFor("global"); got != "hy-image-3.5" {
		t.Errorf("global img2img=%q", got)
	}
	// 空/未知 realm 按 global 处理。
	if got := DefaultTextToImageModelFor(""); got != "hy-image-3.5" {
		t.Errorf("空 realm=%q", got)
	}
}

// ---------------------------------------------------------------------------
// 图片编辑（image-to-image）
// ---------------------------------------------------------------------------

// 编辑请求体：基础字段沿用三分支，叠加 image 数组与 input_fidelity。
func TestBuildImageEditUpstreamBody(t *testing.T) {
	req := &ImageEditRequest{Prompt: "make it blue", Size: "1024x1024", InputFidelity: "high"}
	imgs := []string{"data:image/png;base64,QUJD", "data:image/jpeg;base64,REVG"}
	m := decodeMap(t, mustBuildEdit(t, req, "gpt-image-2.5-sunburst", imgs))

	if m["response_format"] != "b64_json" {
		t.Errorf("response_format=%v want b64_json", m["response_format"])
	}
	if m["input_fidelity"] != "high" {
		t.Errorf("input_fidelity=%v", m["input_fidelity"])
	}
	arr, ok := m["image"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("image=%v want 2 项数组", m["image"])
	}
	if arr[0] != "data:image/png;base64,QUJD" {
		t.Errorf("image[0]=%v", arr[0])
	}
	if m["prompt"] != "make it blue" || m["model"] != "gpt-image-2.5-sunburst" {
		t.Errorf("基础字段错误: %v", m)
	}
}

// 未传 input_fidelity 时不得出现该键。
func TestBuildImageEditUpstreamBodyOmitsFidelity(t *testing.T) {
	m := decodeMap(t, mustBuildEdit(t, &ImageEditRequest{Prompt: "x"}, "gpt-image-1", []string{"data:image/png;base64,QUJD"}))
	if _, ok := m["input_fidelity"]; ok {
		t.Error("未传 input_fidelity 不应出现该键")
	}
}

func mustBuildEdit(t *testing.T, req *ImageEditRequest, model string, images []string) []byte {
	t.Helper()
	raw, err := BuildImageEditUpstreamBody(req, model, images)
	if err != nil {
		t.Fatalf("BuildImageEditUpstreamBody: %v", err)
	}
	return raw
}

// data URL 原样透传；非 image/* 的 data URL 被拒。
func TestNormalizeImageInputDataURL(t *testing.T) {
	c := &Client{}
	got, err := c.NormalizeImageInput(context.Background(), "data:image/png;base64,QUJD")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "data:image/png;base64,QUJD" {
		t.Errorf("got %q", got)
	}
	if _, err := c.NormalizeImageInput(context.Background(), "data:text/plain;base64,QUJD"); err == nil {
		t.Error("非 image data URL 应报错")
	}
}

// 本地文件路径 → data URL（按扩展名定 mime）。
func TestNormalizeImageInputLocalFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	if err := os.WriteFile(p, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	got, err := c.NormalizeImageInput(context.Background(), p)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Errorf("got %q", got)
	}
}

// 不存在的路径报错（不 panic）。
func TestNormalizeImageInputMissingFile(t *testing.T) {
	c := &Client{}
	if _, err := c.NormalizeImageInput(context.Background(), filepath.Join(t.TempDir(), "nope.png")); err == nil {
		t.Error("不存在的文件应报错")
	}
}

// 空输入报错。
func TestNormalizeImageInputEmpty(t *testing.T) {
	c := &Client{}
	if _, err := c.NormalizeImageInput(context.Background(), "   "); err == nil {
		t.Error("空输入应报错")
	}
}

// 裸 base64（URL-safe，不含 /）→ 按 magic bytes 判 mime 后包成 data URL。
func TestNormalizeImageInputRawBase64(t *testing.T) {
	// 最小 PNG 头 8 字节的 URL-safe base64（把 + / 替换掉后仍为合法 base64 字母表）
	raw := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	b64 := base64URLSafe(raw)
	if strings.ContainsAny(b64, "/+") {
		t.Fatalf("测试数据应不含 /+ : %q", b64)
	}
	// 补足长度 ≥100 以命中 isLikelyBase64
	padded := b64 + strings.Repeat("A", 100-len(b64))
	c := &Client{}
	got, err := c.NormalizeImageInput(context.Background(), padded)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Errorf("got 前缀错误: %.40s", got)
	}
}

// base64URLSafe 把标准 base64 转成不含 / 与 + 的形态（仅用于测试构造）。
func base64URLSafe(raw []byte) string {
	s := base64.StdEncoding.EncodeToString(raw)
	s = strings.ReplaceAll(s, "+", "-")
	s = strings.ReplaceAll(s, "/", "_")
	return s
}

// isLikelyBase64 的边界：含 / 或 \ 一律 false（与桌面端同取舍）。
func TestIsLikelyBase64(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{strings.Repeat("A", 100), true},
		{strings.Repeat("A", 99), false},
		{"abc/def" + strings.Repeat("A", 100), false},
		{`C:\path\img.png`, false},
		{strings.Repeat("A", 100) + "!", false},
	}
	for _, c := range cases {
		if got := isLikelyBase64(c.in); got != c.want {
			t.Errorf("isLikelyBase64(%.20s...)=%v want %v", c.in, got, c.want)
		}
	}
}

// magic bytes 判定（复刻桌面端 detectMimeTypeFromBase64）。
func TestDetectImageMIME(t *testing.T) {
	cases := []struct {
		in   []byte
		want string
	}{
		{[]byte{0x89, 0x50, 0x4E, 0x47}, "image/png"},
		{[]byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"},
		{[]byte{0x47, 0x49, 0x46, 0x38}, "image/gif"},
		{[]byte{0x52, 0x49, 0x46, 0x46, 0, 0, 0, 0, 0x57, 0x45, 0x42, 0x50}, "image/webp"},
		{[]byte{0x42, 0x4D, 0, 0}, "image/bmp"},
		{[]byte{0x00, 0x01}, "image/png"}, // 未知 → 兜底 png
	}
	for _, c := range cases {
		if got := detectImageMIME(c.in); got != c.want {
			t.Errorf("detectImageMIME(%v)=%q want %q", c.in, got, c.want)
		}
	}
}

// 扩展名映射表。
func TestMimeByExt(t *testing.T) {
	cases := map[string]string{
		"a.jpg": "image/jpeg", "a.JPEG": "image/jpeg", "a.png": "image/png",
		"a.gif": "image/gif", "a.webp": "image/webp", "a.bmp": "image/bmp",
		"a.svg": "image/svg+xml", "a.tif": "image/tiff", "a.txt": "application/octet-stream",
	}
	for in, want := range cases {
		if got := mimeByExt(in); got != want {
			t.Errorf("mimeByExt(%q)=%q want %q", in, got, want)
		}
	}
}

// multipart 上传路径用的字节转 data URL：优先扩展名，缺失时按 magic。
func TestDataURLFromBytes(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	if got := DataURLFromBytes(png, "x.png"); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Errorf("按扩展名: %s", got)
	}
	if got := DataURLFromBytes(png, ""); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Errorf("按 magic: %s", got)
	}
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if got := DataURLFromBytes(jpg, ""); !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Errorf("jpeg magic: %s", got)
	}
	// 扩展名不认识 → 回落 magic
	if got := DataURLFromBytes(jpg, "weird.dat"); !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Errorf("未知扩展名应回落 magic: %s", got)
	}
}

// global 桌面端 UA 的 v3/config 不再下发生成类模型，只有 IDE UA 下发：探测须取多 UA 并集。
func TestFetchGenerationModelsFallsBackAcrossUAs(t *testing.T) {
	var uas []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		ua := r.Header.Get("User-Agent")
		uas = append(uas, ua)
		if ua == codeBuddyIDEUA {
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"seedance-2.5","name":"Seedance","tags":["text-to-video"]},
				{"id":"hy3","name":"HY3"}
			]}}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{"models":[{"id":"hy3","name":"HY3"}]}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"}
	infos, err := c.FetchGenerationModels(a)
	if err != nil {
		t.Fatalf("FetchGenerationModels: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "seedance-2.5" {
		t.Fatalf("infos=%+v", infos)
	}
	if len(uas) < 2 {
		t.Fatalf("expected multi-UA probe, got %v", uas)
	}
}

// 国际版出图模型只在 modelPromotions 里以纯模型定义出现：须补入；已下架的 sunburst 须剔除。
func TestFetchGenerationModelsFromPromotionsAndDropsDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"gpt-image-2.5-sunburst","name":"GPT-Image-2.5-Sunburst","tags":["text-to-image","image-to-image"]},
			{"id":"hy3","name":"HY3"}
		],"modelPromotions":[
			{"id":"hy-image-3.5","name":"HY Image 3.5","tags":["text-to-image"]},
			{"id":"offpeak","enabled":true,"modelIds":["hy3"]}
		]}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"}
	infos, err := c.FetchGenerationModels(a)
	if err != nil {
		t.Fatalf("FetchGenerationModels: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "hy-image-3.5" {
		t.Fatalf("infos=%+v", infos)
	}
}
