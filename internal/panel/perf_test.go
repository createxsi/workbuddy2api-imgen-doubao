package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestRingSince(t *testing.T) {
	r := NewRing(3)
	r.Write([]byte("a\nb\n"))
	r.Write([]byte("c\nd\n")) // 容量 3：a 被淘汰
	all, last := r.Since(0)
	if last != 4 || len(all) != 3 || all[0].Seq != 2 || all[2].Seq != 4 {
		t.Fatalf("全量 last=%d entries=%+v", last, all)
	}
	inc, last := r.Since(3)
	if last != 4 || len(inc) != 1 || inc[0].Text != "d" {
		t.Fatalf("增量 last=%d entries=%+v", last, inc)
	}
	none, _ := r.Since(4)
	if len(none) != 0 {
		t.Fatalf("无新行应为空，got %+v", none)
	}
}

func TestLogsHandlerSince(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	p.Logs().Write([]byte("one\ntwo\nthree\n"))
	get := func(q string) (entries []LogEntry, last uint64, boot string) {
		req := httptest.NewRequest("GET", "/panel/api/logs"+q, nil)
		req.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
		}
		var out struct {
			Entries []LogEntry `json:"entries"`
			LastSeq uint64     `json:"last_seq"`
			Boot    string     `json:"boot"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Entries, out.LastSeq, out.Boot
	}
	all, last, boot := get("")
	if len(all) != 3 || last != 3 || boot == "" {
		t.Fatalf("全量 entries=%d last=%d boot=%q", len(all), last, boot)
	}
	inc, _, _ := get("?since=2")
	if len(inc) != 1 || inc[0].Text != "three" {
		t.Fatalf("增量 = %+v", inc)
	}
	bad, _, _ := get("?since=abc")
	if len(bad) != 3 {
		t.Fatalf("非法游标应回落全量，got %d", len(bad))
	}
}

// 前端：setHTML 跳过相同内容、mergeLogRows 增量去重与重启回落、后台暂停轮询。
func TestAppJSPanelPerf(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
function slice(a, b) {
  const s = src.indexOf(a), e = src.indexOf(b, s);
  if (s < 0 || e < 0) throw new Error('slice not found: ' + a);
  return src.slice(s, e);
}

// setHTML
{
  const ctx = { WeakMap };
  vm.createContext(ctx);
  vm.runInContext("'use strict';\n" + slice('const lastHTML', '/* ── 主题') + '\nthis.setHTML=setHTML;', ctx);
  let writes = 0;
  const el = {};
  Object.defineProperty(el, 'innerHTML', { set() { writes++; }, get() { return ''; } });
  ctx.setHTML(el, '<a>'); ctx.setHTML(el, '<a>'); ctx.setHTML(el, '<b>');
  assert.strictEqual(writes, 2, 'same html must not touch DOM');
  ctx.setHTML(null, 'x');
}

// mergeLogRows
{
  const ctx = { Number, String, Math };
  vm.createContext(ctx);
  vm.runInContext("'use strict';\n" + slice('const LOG_ROWS_MAX', "$('logChips')") + '\nthis.mergeLogRows=mergeLogRows;', ctx);
  const e = n => ({ seq: n, ch: 'sys', text: 't' + n });
  let st = { rows: [], cursor: 0, boot: '' };
  st = ctx.mergeLogRows(st, { entries: [e(1), e(2)], last_seq: 2, boot: 'b1' }, true);
  assert.deepStrictEqual([st.rows.length, st.cursor, st.boot], [2, 2, 'b1']);
  // 增量：重复行（并发两轮）被丢弃
  st = ctx.mergeLogRows(st, { entries: [e(2), e(3)], last_seq: 3, boot: 'b1' }, false);
  assert.deepStrictEqual(st.rows.map(r => r.seq), [1, 2, 3]);
  assert.strictEqual(st.cursor, 3);
  // 无新行：游标不动
  st = ctx.mergeLogRows(st, { entries: [], last_seq: 3, boot: 'b1' }, false);
  assert.strictEqual(st.cursor, 3);
  // 重启：boot 变化或 last_seq 回退 → reset
  assert.ok(ctx.mergeLogRows(st, { entries: [e(9)], last_seq: 9, boot: 'b2' }, false).reset);
  assert.ok(ctx.mergeLogRows(st, { entries: [], last_seq: 1, boot: 'b1' }, false).reset);
  // 窗口上限 500
  const many = Array.from({ length: 600 }, (_, i) => e(i + 1));
  const big = ctx.mergeLogRows({ rows: [], cursor: 0, boot: '' }, { entries: many, last_seq: 600, boot: 'b' }, true);
  assert.strictEqual(big.rows.length, 500);
  assert.strictEqual(big.rows[0].seq, 101);
}

// 后台暂停轮询
{
  let handler = null, timers = 0, active = 0, refreshed = 0;
  const document = { visibilityState: 'visible', addEventListener(ev, fn) { if (ev === 'visibilitychange') handler = fn; } };
  const ctx = {
    document,
    setInterval: () => { timers++; active++; return timers; },
    clearInterval: () => { active--; },
    loadOverview: () => { refreshed++; }, loadLogs() {}, reattachQueueView() {},
  };
  vm.createContext(ctx);
  vm.runInContext("'use strict';\nlet refTimer = null; let view = 'accounts';\n" + slice('const POLL_MS', 'function start()') + '\nthis.startPolling=startPolling;', ctx);
  ctx.startPolling();
  assert.strictEqual(active, 1);
  document.visibilityState = 'hidden'; handler();
  assert.strictEqual(active, 0, 'hidden must stop timer');
  const before = refreshed;
  document.visibilityState = 'visible'; handler();
  assert.strictEqual(active, 1, 'visible must restart timer');
  assert.strictEqual(refreshed, before + 1, 'visible must refresh immediately');
}
process.stdout.write('OK');`
	f, err := os.CreateTemp(t.TempDir(), "perf-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("panel perf node test failed: %v\n%s", err, out)
	}
}
