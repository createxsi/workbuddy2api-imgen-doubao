package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/balance"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
)

func TestBalanceHistoryEndpoint(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "甲", AccessToken: "x", ExpiresAt: 9999999999})
	rec := balance.New("")
	scheduler.SampleBalance(rec, "u1", "甲", "cn", 120, 500)
	scheduler.SampleBalance(rec, "u2", "企业", "cn", 1<<40, -1) // 企业不限量不采样
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Balance: rec})
	req := httptest.NewRequest("GET", "/panel/api/balance_history?days=99", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var out struct {
		Days   int `json:"days"`
		Series []struct {
			UID, Name, Realm string
			Current          int64
		} `json:"series"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Days != 30 || len(out.Series) != 1 || out.Series[0].UID != "u1" || out.Series[0].Current != 120 || out.Series[0].Realm != "cn" {
		t.Fatalf("got %+v", out)
	}

	p2 := New(Config{Version: "test", APIKey: "k", Pool: pl})
	rr = httptest.NewRecorder()
	p2.ServeHTTP(rr, req)
	if rr.Code != 501 {
		t.Fatalf("无记录器应 501，got %d", rr.Code)
	}
}

// 余额趋势前端：本月流量与日键按北京时间；折线与日柱能在无 DOM 依赖下渲染。
func TestAppJSBalanceTrend(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const BH_REALMS');
const end = src.indexOf('async function loadBalance');
if (start < 0 || end < 0) throw new Error('balance region not found');
const els = {};
const ctx = { Number, String, Math, Date, Map, Array, Object, Infinity, esc: s => String(s), overviewData: { accounts: [{ uid: 'u2', disabled: true }] },
  disabledKind: () => ({ label: '上游封禁', tone: 'bad' }),
  localStorage: { getItem: () => null }, $: id => (els[id] = els[id] || { innerHTML: '' }) };
vm.createContext(ctx);
vm.runInContext("'use strict';\n" + src.slice(start, end) +
  '\nthis.bhDayKey=bhDayKey; this.bhMonthFlow=bhMonthFlow; this.renderBalanceChart=renderBalanceChart; this.renderBalanceDaily=renderBalanceDaily; this.bhPickRealm=bhPickRealm; this.bhAcctMark=bhAcctMark;', ctx);
const now = Date.parse('2026-10-25T17:30:00Z'); // 北京时间 10-26 01:30
const s = { uid: 'u1', current: 90, daily: [
  { day: '2026-09-30', consumed: 99, gained: 1 },
  { day: '2026-10-03', consumed: 10, gained: 5 },
  { day: '2026-10-26', consumed: 3, gained: 0 } ],
  points: [{ t: 1000, c: 100 }, { t: 2800, c: 90 }] };
const mo = ctx.bhMonthFlow(s, now);
ctx.renderBalanceChart([s], new Map([['u1', '#123456']]));
ctx.renderBalanceDaily([s]);
process.stdout.write(JSON.stringify({
  day: ctx.bhDayKey(now),
  month: mo.consumed === 13 && mo.gained === 5 && mo.days === 2 && !mo.covered,
  chart: els.bhChart.innerHTML.includes('<polyline') && els.bhChart.innerHTML.includes('#123456'),
  daily: (els.bhDaily.innerHTML.match(/<rect/g) || []).length === 6,
  realm: ctx.bhPickRealm([{ realm: 'global' }]),
  mark: ctx.bhAcctMark('u2').includes('上游封禁') && ctx.bhAcctMark('u1') === '',
}));`
	f, err := os.CreateTemp(t.TempDir(), "bh-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	const want = `{"day":"2026-10-26","month":true,"chart":true,"daily":true,"realm":"global","mark":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("got %s want %s", out, want)
	}
}

func TestAppJSDisabledKind(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function disabledKind');
const end = src.indexOf('function rateLimitRowsHtml');
const ctx = { String, Math, Date, Number };
vm.createContext(ctx);
vm.runInContext("'use strict';\n" + src.slice(start, end) + '\nthis.disabledKind=disabledKind; this.disabledAge=disabledAge;', ctx);
const k = r => ctx.disabledKind(r).label;
const now = Date.parse('2026-10-26T00:00:00Z');
process.stdout.write(JSON.stringify([
  k('account restricted by upstream (11140 request illegal); appeal'), k('12153 session dead'), k('refresh session dead'),
  k('gateway 401'), k('manual disable (panel)'), k('other'),
  ctx.disabledAge('2026-10-23T00:00:00Z', now), ctx.disabledAge('2026-10-25T23:30:00Z', now), ctx.disabledAge('', now),
]));`
	f, err := os.CreateTemp(t.TempDir(), "dk-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	const want = `["上游封禁","登录失效","登录失效","鉴权失败","手动禁用","已禁用","3 天前","30 分钟前",""]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("got %s want %s", out, want)
	}
}
