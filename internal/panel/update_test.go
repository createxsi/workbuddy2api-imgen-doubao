package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/updater"
)

func doReq(p *Panel, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

func TestUpdateEndpoints(t *testing.T) {
	// 未接入更新器：接口 501，overview.update 为 null。
	p := New(Config{Version: "1.13.4-panel", APIKey: "k", Pool: pool.New("")})
	if rr := doReq(p, "GET", "/panel/api/update"); rr.Code != 501 {
		t.Fatalf("nil updater code=%d", rr.Code)
	}
	var ov map[string]any
	json.Unmarshal(doReq(p, "GET", "/panel/api/overview").Body.Bytes(), &ov)
	if v, ok := ov["update"]; !ok || v != nil {
		t.Fatalf("overview.update 应为 null，got %v", v)
	}

	// 接入更新器：状态可读；未配置重启回调时安装被拒（不触网）。
	m := updater.New("1.13.4-panel", "", updater.Settings{AutoEnabled: true, AutoTime: "03:30"})
	p2 := New(Config{Version: "1.13.4-panel", APIKey: "k", Pool: pool.New(""), Updater: m})
	rr := doReq(p2, "GET", "/panel/api/update")
	var st updater.Status
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil || rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	if st.CurrentVersion != "1.13.4-panel" || !st.AutoEnabled || st.AutoTime != "03:30" {
		t.Fatalf("status=%+v", st)
	}
	json.Unmarshal(doReq(p2, "GET", "/panel/api/overview").Body.Bytes(), &ov)
	if u, _ := ov["update"].(map[string]any); u == nil || u["current_version"] != "1.13.4-panel" {
		t.Fatalf("overview.update=%v", ov["update"])
	}
	if rr := doReq(p2, "POST", "/panel/api/update/install"); rr.Code != 502 {
		t.Fatalf("install 应被拒，code=%d", rr.Code)
	}
	// 未鉴权不可访问。
	req := httptest.NewRequest("POST", "/panel/api/update/install", nil)
	rr2 := httptest.NewRecorder()
	p2.ServeHTTP(rr2, req)
	if rr2.Code != 401 {
		t.Fatalf("未鉴权 code=%d", rr2.Code)
	}
}

func TestAppJSUpdateNote(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('function displayVersion'), e = src.indexOf('function renderUpdateStatus');
if (s < 0 || e < 0) throw new Error('update helpers not found');
const ctx = { String };
vm.createContext(ctx);
vm.runInContext("'use strict';\n" + src.slice(s, e) + '\nthis.displayVersion=displayVersion;this.updateNoteText=updateNoteText;', ctx);
assert.strictEqual(ctx.displayVersion('1.13.4-panel'), 'v1.13.4-panel');
assert.strictEqual(ctx.displayVersion(''), 'dev');
assert.strictEqual(ctx.updateNoteText({ latest_version: '1.13.4' }), '已是最新版本 · v1.13.4');
assert.strictEqual(ctx.updateNoteText({ update_available: true, latest_version: '1.14.0', can_install: true }), '发现新版本 v1.14.0');
assert.strictEqual(ctx.updateNoteText({ update_available: true, latest_version: '1.14.0', install_blocked: '容器部署，请更新镜像' }), '发现新版本 v1.14.0 · 容器部署，请更新镜像');
assert.strictEqual(ctx.updateNoteText({ installing: true }), '正在准备更新…');
assert.ok(ctx.updateNoteText({ last_error: 'x' }).includes('x'));
assert.strictEqual(ctx.updateNoteText({}), '尚未检查');
process.stdout.write('OK');`
	f, err := os.CreateTemp(t.TempDir(), "update-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(script)
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("update note node test failed: %v\n%s", err, out)
	}
}
