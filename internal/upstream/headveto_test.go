package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVetoHeadErrorFrame(t *testing.T) {
	_, err := VetoHead(strings.NewReader("data: {\"error\":{\"message\":\"limit\",\"code\":6004}}\n\ndata: [DONE]\n\n"))
	hv, ok := IsHeadVeto(err)
	if !ok || !strings.Contains(hv.Payload, `"code":6004`) {
		t.Fatalf("应否决 error 首帧：%v", err)
	}
}

func TestVetoHeadEmbeddedCodeFrame(t *testing.T) {
	_, err := VetoHead(strings.NewReader("data: {\"code\":6021,\"msg\":\"queue.full.title\",\"requestId\":\"r1\"}\n\n"))
	hv, ok := IsHeadVeto(err)
	if !ok {
		t.Fatalf("应否决 200 内嵌失败帧：%v", err)
	}
	if !strings.Contains(hv.Payload, `"error"`) || !strings.Contains(hv.Payload, `"requestId":"r1"`) {
		t.Fatalf("应改写成标准 error 帧并保留 requestId：%s", hv.Payload)
	}
	if FrameKind(hv.Payload) != ErrUpstreamBusy {
		t.Fatalf("6021 帧应归 upstream_busy，got %v", FrameKind(hv.Payload))
	}
}

func TestVetoHeadEmpty(t *testing.T) {
	for _, in := range []string{"", "\n\n", "data: [DONE]\n\n"} {
		_, err := VetoHead(strings.NewReader(in))
		hv, ok := IsHeadVeto(err)
		if !ok || hv.Payload != "" {
			t.Fatalf("%q 应按零帧否决：%v", in, err)
		}
	}
}

// 正常首帧 / 注释先到：不否决，回放字节与原文逐字节一致。
func TestVetoHeadReplayExact(t *testing.T) {
	for _, in := range []string{
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
		": ping\n\ndata: {\"error\":{\"message\":\"x\"}}\n\n",
		"\n\ndata: {\"choices\":[]}", // 无结尾换行
	} {
		r, err := VetoHead(strings.NewReader(in))
		if err != nil {
			t.Fatalf("%q 不应否决：%v", in, err)
		}
		got, _ := io.ReadAll(r)
		if string(got) != in {
			t.Fatalf("回放不一致：%q vs %q", got, in)
		}
	}
}

// 正文里带 choices 的帧即使含 code 字样也不是失败帧。
func TestStreamErrorFrameNarrow(t *testing.T) {
	if _, _, ok := StreamErrorFrame(map[string]any{"code": 11140.0, "choices": []any{}}); ok {
		t.Fatal("有 choices 不应判失败帧")
	}
	if _, _, ok := StreamErrorFrame(map[string]any{"code": 0.0, "msg": "ok"}); ok {
		t.Fatal("code=0 不应判失败帧")
	}
	if c, m, ok := StreamErrorFrame(map[string]any{"code": 11140.0, "displayMsg": "拦截"}); !ok || c != 11140 || m != "拦截" {
		t.Fatalf("got %d %q %v", c, m, ok)
	}
}

// 流内 11140「request illegal」不按账号封禁处理；11128 仍归通道校验失败。
func TestFrameKindByCode(t *testing.T) {
	if k := FrameKind(`{"error":{"message":"request illegal","code":11140}}`); k != ErrClient {
		t.Fatalf("流内 11140 应归 ErrClient，got %v", k)
	}
	if k := FrameKind(`{"error":{"message":"request illegal","code":11128}}`); k != ErrChannelRefused {
		t.Fatalf("流内 11128 应归通道校验失败，got %v", k)
	}
}

// 中途出现的 200 内嵌失败帧如实透出为 error 帧（不再被白名单剥成空 chunk），并通知观察者。
func TestStreamHintMidStreamEmbeddedError(t *testing.T) {
	in := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"code\":11140,\"msg\":\"request illegal\"}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	var seen string
	if err := StreamHint(rec, strings.NewReader(in), nil, WithErrorFrameObserver(func(p string) { seen = p })); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"error":{"code":11140,"message":"request illegal"}`) {
		t.Fatalf("应透出 error 帧：%s", out)
	}
	if !strings.Contains(seen, "11140") {
		t.Fatalf("观察者应收到改写后的帧：%q", seen)
	}
}
