package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// reportHeaderKeys 桌面端 /v2/report 真机头键集（Node 形态，不含传输层自动头）。
var reportHeaderKeys = []string{
	"Accept", "Authorization", "Content-Type", "User-Agent", "X-Domain",
	"X-Product", "X-Request-Id", "X-User-Id",
}

func captureReport(t *testing.T, call func(c *Client, base string) error) (http.Header, []map[string]any, string) {
	t.Helper()
	var hdr http.Header
	var body []map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body 必须是数组: %v", err)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, WebBaseCN: srv.URL, BillingBaseCN: "http://billing.invalid"}
	if err := call(c, srv.URL); err != nil {
		t.Fatalf("report: %v", err)
	}
	return hdr, body, path
}

func assertReportHeaders(t *testing.T, hdr http.Header, wantUA string) {
	t.Helper()
	var keys []string
	for k := range hdr {
		switch k {
		case "Accept-Encoding", "Content-Length":
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := append([]string(nil), reportHeaderKeys...)
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("头键集=%v want %v", keys, want)
	}
	if got := hdr.Get("User-Agent"); got != wantUA {
		t.Errorf("UA=%q want %q", got, wantUA)
	}
	if got := hdr.Get("Content-Type"); got != "application/json;charset=UTF-8" {
		t.Errorf("Content-Type=%q", got)
	}
	if got := hdr.Get("Accept"); got != "application/json, text/plain, */*" {
		t.Errorf("Accept=%q", got)
	}
	if d := hdr.Get("X-Domain"); strings.Contains(d, "://") || strings.Contains(d, ":") {
		t.Errorf("X-Domain 应为裸主机名: %q", d)
	}
	if id := hdr.Get("X-Request-Id"); len(id) != 32 {
		t.Errorf("X-Request-ID 应为 32 hex: %q", id)
	}
}

// TestReportChatActivityShape 对话活跃上报：走 chatBase、桌面端 report 头族、5.9.3 字段集。
func TestReportChatActivityShape(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", Nickname: "n"}
	hdr, body, path := captureReport(t, func(c *Client, _ string) error {
		return c.ReportChatActivity(a, "conv-1", "req-1")
	})
	if path != "/v2/report" {
		t.Fatalf("path=%s", path)
	}
	assertReportHeaders(t, hdr, "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0")
	ev := body[0]
	for _, k := range []string{"isMaxMode", "source", "inputAttemptId", "vcsType", "vcsRepo",
		"vcsBranchName", "vcsRevId", "qimei36", "cpuModel", "codebuddy.session_id"} {
		if _, ok := ev[k]; !ok {
			t.Errorf("缺字段 %s", k)
		}
	}
	for _, k := range []string{"maxToken", "maxSteps", "temperature", "maxRetries",
		"skillCount", "totalCount", "fileUri", "expertId"} {
		if _, ok := ev[k]; ok {
			t.Errorf("多字段 %s（5.9.3 无此键）", k)
		}
	}
	if ev["agentName"] != "cli" || ev["agentType"] != "main" {
		t.Errorf("agent=%v/%v want cli/main", ev["agentName"], ev["agentType"])
	}
	if ev["ideVersion"] != "5.9.3" || ev["commit"] != desktopCommitCN {
		t.Errorf("CN 三元组不对: %v %v", ev["ideVersion"], ev["commit"])
	}
	if tr, _ := ev["traceId"].(string); len(tr) != 32 {
		t.Errorf("traceId=%v", ev["traceId"])
	}
}

// TestReportWebEventNodeShape web 事件上报不再带浏览器头（Origin/Referer/x-client-platform）。
func TestReportWebEventNodeShape(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	hdr, body, _ := captureReport(t, func(c *Client, _ string) error {
		return c.ReportWebEvent(a, "web_element_click", "https://www.workbuddy.cn/docs", "e", "n")
	})
	assertReportHeaders(t, hdr, "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0")
	for _, k := range []string{"Origin", "Referer", "X-Client-Platform"} {
		if hdr.Get(k) != "" {
			t.Errorf("不应带 %s", k)
		}
	}
	if ua, _ := body[0]["userAgent"].(string); strings.Contains(ua, "Chrome/152") {
		t.Errorf("body userAgent 仍是浏览器 UA: %q", ua)
	}
}

// TestReportDesktopEventGlobalShape 国际号：UA 首两段 workbuddy-ai、5.9.4 三元组、无 qimei36。
func TestReportDesktopEventGlobalShape(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"}
	var hdr http.Header
	var body []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseGlobal: srv.URL, GlobalEnabled: true}
	if err := c.ReportDesktopEvent(a, DesktopEvent{"eventCode": "x"}); err != nil {
		t.Fatal(err)
	}
	assertReportHeaders(t, hdr, "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0")
	if body[0]["ideVersion"] != "5.9.4" || body[0]["commit"] != desktopCommitGlobal {
		t.Errorf("global 三元组不对: %v", body[0])
	}
	if _, ok := body[0]["qimei36"]; ok {
		t.Errorf("国际版不应带 qimei36")
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.workbuddy.cn":         "www.workbuddy.cn",
		"https://www.workbuddy.ai/":        "www.workbuddy.ai",
		"http://127.0.0.1:8080/x":          "127.0.0.1",
		"":                                 "fb",
		"https://copilot.tencent.com/v2?q": "copilot.tencent.com",
	} {
		if got := hostOf(in, "fb"); got != want {
			t.Errorf("hostOf(%q)=%q want %q", in, got, want)
		}
	}
}
