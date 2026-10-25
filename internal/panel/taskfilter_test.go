package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 任务中心筛选：已完成/不适用默认收起，切换开关原地重绘；不适用行（队列里同为 done）
// 只受「隐藏不适用」控制。
func TestAppJSTaskCenterFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const LS_TC_HIDE_DONE');
const end = src.indexOf('function updateProgress(q)');
if (start < 0 || end < 0) throw new Error('task filter region not found');
const els = {};
const el = id => (els[id] = els[id] || { innerHTML: '', textContent: '', style: {}, hidden: false, checked: false,
  querySelector: sel => (el(id + sel)) });
const store = {};
const ctx = { String, Object, esc: s => String(s), qrowHTML: r => '[' + r.code + ']', updateProgress: () => {},
  localStorage: { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = v; } }, $: el };
vm.createContext(ctx);
vm.runInContext("'use strict';\n" + src.slice(start, end) + '\nthis.renderQueue=renderQueue;', ctx);
const groups = [{ uid: 'u1', nick: 'n', rows: [
  { code: 'A', status: 'done', message: '已完成（已领取）' },
  { code: 'B', status: 'done', message: '该账号无此任务' },
  { code: 'C', status: 'running' },
]}];
ctx.renderQueue(groups, null);
const def = els.qcList.innerHTML;
els.tcHideDone.onchange({ target: { checked: false } });
const showDone = els.qcList.innerHTML;
els.tcHideDone.onchange({ target: { checked: true } });
els.tcHideNa.onchange({ target: { checked: false } });
const showNa = els.qcList.innerHTML;
ctx.renderQueue([{ uid: 'u1', rows: [{ code: 'A', status: 'done' }] }], null, 'T', 'D');
process.stdout.write(JSON.stringify({
  def: def.includes('[C]') && !def.includes('[A]') && !def.includes('[B]'),
  showDone: showDone.includes('[A]') && !showDone.includes('[B]'),
  showNa: showNa.includes('[B]') && !showNa.includes('[A]'),
  stored: store['wb2api.tc.hideNa'] === '0' && store['wb2api.tc.hideDone'] === '1',
  allHidden: els['tcEmpty.t'].textContent === '当前筛选下没有任务',
}));`
	f, err := os.CreateTemp(t.TempDir(), "tc-filter-*.cjs")
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
	const want = `{"def":true,"showDone":true,"showNa":true,"stored":true,"allHidden":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("got %s want %s", out, want)
	}
}
