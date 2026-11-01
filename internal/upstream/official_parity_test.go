// official_parity_test.go 官方桌面端真机头夹具对拍（CI 门禁，跟 misakano fbe8d3f / 01d4f1d）。
//
// 夹具 testdata/official_*_head.txt 是桌面端真机抓包的请求头原文（保序、保大小写；
// Authorization / X-Refresh-Token / X-User-Id 已脱敏）。本测试用**生产函数**经完整出站
// 链路（TLS 指纹 + 头序 + 头名拼写 + Connection）向本地 TLS 监听发请求，抓 wire 字节，
// 与夹具逐名比对：集合 / 大小写 / 静态值 / 顺序。差异非零即红；已知且有意的差异必须
// 写进 knownDiff 并给出理由。
//
// 本仓独有路径（/v2/images/*、/v3/config 目录探测三 UA、agenttool 搜索 / webfetch）
// 没有官方夹具：按「同族对拍」——与 chat 夹具共有的头必须同拼写、同静态值，头序服从
// chat 序，Connection 与官方同族一致。
//
// 跑法：默认随 go test 运行；WB2A_PARITY=report 只打印差异表不判失败（审计用）。
// 改夹具必须同时给出抓包依据，否则等于自己给自己放水。
package upstream

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// parityDynamic 值每次不同、只比存在性与拼写。
var parityDynamic = map[string]bool{
	"x-request-id": true, "x-conversation-message-id": true,
	"x-conversation-request-id": true, "x-root-request-id": true,
	"x-conversation-id": true, "acp-connection-id": true,
	"traceparent": true, "b3": true, "x-b3-traceid": true,
	"x-b3-parentspanid": true, "x-b3-spanid": true, "x-trace-id": true,
	"baggage": true, "authorization": true, "x-user-id": true,
	"content-length": true, "host": true, "x-refresh-token": true,
	// 测试监听在 127.0.0.1，X-Domain 由账号域推出；生产等于真实域。
	"x-domain": true,
	// 版本号随默认 clientVersion 演进，单独由头族测试守护。
	"user-agent": true, "x-ide-version": true,
	// 运行环境相关。
	"x-stainless-arch": true, "x-stainless-os": true,
}

// knownDiff 已知且有意保留的差异（小写头名 → 理由）。门禁只放过这里列出的项。
var knownDiff = map[string]map[string]string{
	// 报告 / refresh：真机夹具不带 Accept-Encoding；本仓与 misakano 一致全链路声明（出站
	// 压缩开关 upstream.wire_compression），待真机复核后再定是否按路径收窄。
	"official_report_cn_head.txt":  {"accept-encoding": "全链路声明（同 misakano），待真机复核"},
	"official_gl_report_head.txt":  {"accept-encoding": "同上"},
	"official_gl_refresh_head.txt": {"accept-encoding": "同上"},
	"official_v3config_head.txt": {
		"authorization":   "目录探测按账号拉取（含企业 / 试用模型），需带账号身份；真机该请求为匿名",
		"x-user-id":       "同上",
		"x-domain":        "同上",
		"accept-encoding": "出站压缩开启时统一声明四段编码；真机该路径不带",
	},
}

type rawHead struct {
	line   string
	order  []string
	values map[string]string
}

func parseHead(raw string) rawHead {
	raw = strings.TrimSuffix(strings.ReplaceAll(raw, "\r\n", "\n"), "\n\n")
	lines := strings.Split(raw, "\n")
	h := rawHead{values: map[string]string{}}
	for i, ln := range lines {
		if i == 0 {
			h.line = ln
			continue
		}
		c := strings.IndexByte(ln, ':')
		if c <= 0 {
			continue
		}
		k := ln[:c]
		h.order = append(h.order, k)
		h.values[k] = strings.TrimSpace(ln[c+1:])
	}
	return h
}

func loadOfficial(t *testing.T, name string) rawHead {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读夹具 %s: %v", name, err)
	}
	return parseHead(string(raw))
}

type parityDiff struct {
	missing, extra, caseDiff, valDiff, orderDiff []string
}

func (d parityDiff) total() int {
	return len(d.missing) + len(d.extra) + len(d.caseDiff) + len(d.valDiff) + len(d.orderDiff)
}

// compareHeads 夹具 vs 出站：allow 中的小写头名不计入差异。
func compareHeads(off, got rawHead, allow map[string]string) parityDiff {
	var d parityDiff
	offSet, gotSet := map[string]string{}, map[string]string{}
	for _, k := range off.order {
		offSet[strings.ToLower(k)] = k
	}
	for _, k := range got.order {
		gotSet[strings.ToLower(k)] = k
	}
	for lk, k := range offSet {
		if _, ok := allow[lk]; ok {
			continue
		}
		g, ok := gotSet[lk]
		switch {
		case !ok:
			d.missing = append(d.missing, k)
		case g != k:
			d.caseDiff = append(d.caseDiff, k+" ←出站 "+g)
		case !parityDynamic[lk] && off.values[k] != got.values[g]:
			d.valDiff = append(d.valDiff, k+": 官方="+strconv.Quote(off.values[k])+" 出站="+strconv.Quote(got.values[g]))
		}
	}
	for lk, g := range gotSet {
		if _, ok := allow[lk]; ok {
			continue
		}
		if _, ok := offSet[lk]; !ok {
			d.extra = append(d.extra, g)
		}
	}
	// 顺序：双方共有的头按夹具相对顺序出现。
	rank := map[string]int{}
	for i, k := range off.order {
		rank[strings.ToLower(k)] = i
	}
	last, lastName := -1, ""
	for _, g := range got.order {
		r, ok := rank[strings.ToLower(g)]
		if !ok {
			continue
		}
		if r < last {
			d.orderDiff = append(d.orderDiff, g+" 应在 "+lastName+" 之前")
		}
		if r > last {
			last, lastName = r, g
		}
	}
	return d
}

func reportParity(t *testing.T, name string, d parityDiff) {
	t.Helper()
	if d.total() == 0 {
		t.Logf("%s：完全一致（已知差异 %d 项见 knownDiff）", name, len(knownDiff[name]))
		return
	}
	msg := name + "：与真机夹具存在差异"
	for _, part := range []struct {
		tag  string
		list []string
	}{{"漏发", d.missing}, {"多发", d.extra}, {"大小写", d.caseDiff}, {"值", d.valDiff}, {"顺序", d.orderDiff}} {
		if len(part.list) > 0 {
			msg += "\n  【" + part.tag + "】" + strings.Join(part.list, "；")
		}
	}
	if os.Getenv("WB2A_PARITY") == "report" {
		t.Log(msg)
		return
	}
	t.Error(msg)
}

func parityClient(srvURL string) *Client {
	c := testWireClient(true, false)
	c.ChatBaseCN, c.BillingBaseCN, c.WebBaseCN = srvURL, srvURL, srvURL
	c.ChatBaseGlobal, c.BillingBaseGlobal = srvURL, srvURL
	return c
}

func cnAcct() *auth.Auth {
	return &auth.Auth{UID: "00000000-0000-4000-8000-000000000001", AccessToken: "AT", RefreshToken: "RT",
		Domain: "www.workbuddy.cn", ExpiresAt: time.Now().Add(time.Hour).Unix()}
}

func glAcct() *auth.Auth {
	return &auth.Auth{UID: "00000000-0000-4000-8000-000000000001", AccessToken: "AT", RefreshToken: "RT",
		Domain: "www.workbuddy.ai", ExpiresAt: time.Now().Add(time.Hour).Unix()}
}

// lastHead 取监听收到的最后一个请求头（可按路径前缀筛）。
func (s *rawTLSServer) lastHead(t *testing.T, pathPrefix string) rawHead {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.heads) - 1; i >= 0; i-- {
		h := parseHead(s.heads[i])
		if strings.Contains(h.line, " "+pathPrefix) {
			return h
		}
	}
	t.Fatalf("未抓到 %s 请求（共 %d 个）", pathPrefix, len(s.heads))
	return rawHead{}
}

// TestOfficialParity 有真机夹具的链路（本仓 /v2/chat/busy 本地应答、不出站，不在表内）：生产函数出站 vs 夹具。
func TestOfficialParity(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		fixture, path string
		send          func(c *Client)
	}{
		{"official_chat_head.txt", "/v2/chat/completions", func(c *Client) {
			rc, _, _, _ := c.ChatStreamContext(ctx, cnAcct(), []byte(`{"model":"hy3","messages":[{"role":"user","content":"`+strings.Repeat("hi ", 600)+`"}]}`), "",
				ChatMeta{ConversationID: "f7c6c392-e016-43bb-beca-ab918b436668", ConversationRequestID: "01a194ad31c27d81804ee9915cf954fb"})
			if rc != nil {
				rc.Close()
			}
		}},
		{"official_report_cn_head.txt", "/v2/report", func(c *Client) {
			_ = c.ReportDesktopEvent(cnAcct(), DesktopEvent{"eventCode": "x"})
		}},
		{"official_gl_report_head.txt", "/v2/report", func(c *Client) {
			_ = c.ReportDesktopEvent(glAcct(), DesktopEvent{"eventCode": "x"})
		}},
		{"official_gl_refresh_head.txt", "/v2/plugin/auth/token/refresh", func(c *Client) { _ = c.refreshTokenOnce(glAcct()) }},
		{"official_v3config_head.txt", "/v3/config", func(c *Client) { _, _ = c.fetchV3ConfigModelMap(cnAcct(), "") }},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSuffix(tc.fixture, "_head.txt"), func(t *testing.T) {
			srv := newRawTLSServer(t)
			c := parityClient(srv.url())
			tc.send(c)
			got := srv.lastHead(t, tc.path)
			reportParity(t, tc.fixture, compareHeads(loadOfficial(t, tc.fixture), got, knownDiff[tc.fixture]))
		})
	}
}

// TestOwnPathParity 本仓独有路径没有官方夹具：与 chat 夹具同族对拍。
//   - 与 chat 夹具共有的头：拼写一致、静态值一致；
//   - 共有头的相对顺序服从 chat 夹具；
//   - Connection 与官方同族一致（keep-alive）。
func TestOwnPathParity(t *testing.T) {
	ctx := context.Background()
	chat := loadOfficial(t, "official_chat_head.txt")
	cases := []struct {
		name, path string
		send       func(c *Client)
	}{
		{"images-generations", "/v2/images/generations", func(c *Client) {
			_, _, _ = c.GenerateImage(ctx, cnAcct(), []byte(`{"model":"m","prompt":"p"}`), "")
		}},
		{"images-edits", "/v2/images/edits", func(c *Client) {
			_, _, _ = c.GenerateImageEdit(ctx, cnAcct(), []byte(`{"model":"m","prompt":"p"}`), "")
		}},
		{"webfetch", webFetchPath, func(c *Client) { _, _ = c.WebFetch(ctx, cnAcct(), "https://example.com") }},
		{"search", "/agenttool/v1/", func(c *Client) { _, _ = c.Search(ctx, cnAcct(), "q", 3) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRawTLSServer(t)
			c := parityClient(srv.url())
			tc.send(c)
			got := srv.lastHead(t, tc.path)
			var d parityDiff
			gotSet := map[string]string{}
			for _, g := range got.order {
				gotSet[strings.ToLower(g)] = g
			}
			for _, k := range chat.order {
				lk := strings.ToLower(k)
				g, ok := gotSet[lk]
				if !ok {
					continue // 同族不同路径的头集合可以不同，只比共有头
				}
				if g != k {
					d.caseDiff = append(d.caseDiff, k+" ←出站 "+g)
				} else if !parityDynamic[lk] && lk != "accept" && lk != "content-type" && lk != "content-encoding" &&
					lk != "accept-encoding" && chat.values[k] != got.values[g] {
					d.valDiff = append(d.valDiff, k+": chat="+strconv.Quote(chat.values[k])+" 出站="+strconv.Quote(got.values[g]))
				}
			}
			sub := rawHead{values: chat.values}
			for _, k := range chat.order {
				if _, ok := gotSet[strings.ToLower(k)]; ok {
					sub.order = append(sub.order, k)
				}
			}
			d.orderDiff = compareHeads(sub, got, nil).orderDiff
			if v := got.values[gotSet["connection"]]; v != "keep-alive" {
				d.valDiff = append(d.valDiff, "Connection: 出站="+strconv.Quote(v)+" 应为 keep-alive")
			}
			reportParity(t, tc.name, d)
		})
	}
}

// TestV3ConfigProbeUAsParity 目录探测三 UA（桌面端 / IDE / CLI）：同一头形，只有 UA 不同。
func TestV3ConfigProbeUAsParity(t *testing.T) {
	var shapes []string
	for _, ua := range []string{"", codeBuddyIDEUA, codeBuddyCLIUA} {
		srv := newRawTLSServer(t)
		c := parityClient(srv.url())
		_, _ = c.fetchV3ConfigModelMap(glAcct(), ua)
		got := srv.lastHead(t, "/v3/config")
		d := compareHeads(loadOfficial(t, "official_v3config_head.txt"), got, knownDiff["official_v3config_head.txt"])
		reportParity(t, "v3config ua="+ua, d)
		shapes = append(shapes, strings.Join(got.order, ","))
	}
	for _, s := range shapes[1:] {
		if s != shapes[0] {
			t.Errorf("三 UA 探测头形不一致：%v", shapes)
		}
	}
}

// TestParityGateDetectsDrift 门禁有效性：篡改一个头名大小写必须被识别。
func TestParityGateDetectsDrift(t *testing.T) {
	off := loadOfficial(t, "official_v3config_head.txt")
	got := rawHead{values: map[string]string{}}
	for _, k := range off.order {
		g := k
		if k == "X-Request-ID" {
			g = "X-Request-Id"
		}
		got.order = append(got.order, g)
		got.values[g] = off.values[k]
	}
	if d := compareHeads(off, got, nil); len(d.caseDiff) != 1 {
		t.Fatalf("应识别出 1 处大小写差异：%+v", d)
	}
	got.order[0], got.order[1] = got.order[1], got.order[0]
	if d := compareHeads(off, got, nil); len(d.orderDiff) == 0 {
		t.Fatalf("应识别出顺序差异：%+v", d)
	}
}
