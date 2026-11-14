package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 「账号任务」表格的结束时间列：上游 valid_end 有文本（空格分隔）与 RFC3339 两种
// 常见形态，都要截到分钟；空值显示「—」而不是空白（空白会被读成"没有这一列"）。
func TestAppJSTaskEndText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; task end time test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function taskEndText');
// 只取到 AUTO_TASKS 表之前：再往后就是 openTasks/事件绑定等顶层语句，vm 里没有 $。
const end = src.indexOf('const AUTO_TASKS = {');
if (start < 0 || end < 0) throw new Error('taskEndText not found');
const ctx = { String, Number };
vm.createContext(ctx);
vm.runInContext("'use strict';\n" + src.slice(start, end) + '\nthis.taskEndText=taskEndText;', ctx);
process.stdout.write(JSON.stringify({
  text: ctx.taskEndText('2026-11-20 23:59:59'),
  // 不带时区的 ISO 串按本地时区解析，断言与时区无关。
  iso: ctx.taskEndText('2026-11-20T23:59:00'),
  empty: ctx.taskEndText(''),
  nil: ctx.taskEndText(null),
}));`
	f, err := os.CreateTemp(t.TempDir(), "task-end-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("task end time node test failed: %v\n%s", err, out)
	}
	const want = `{"text":"2026-11-20 23:59","iso":"2026-11-20 23:59","empty":"—","nil":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("task end time=%s want %s", out, want)
	}
}

// 作业状态行：跑完的作业要带「结束于 …」，排队/执行中（无 finished_at）不能凭空出现。
func TestAppJSTaskJobFinishedAt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; task job finished time test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let taskUID = null'), end = src.indexOf('async function loadTasks()');
if (start < 0 || end < 0) throw new Error('task job block not found');
const ctx = {
  console, Promise, Object, Array, String, Number, Map, encodeURIComponent,
  $: () => ({ textContent: '', innerHTML: '', hidden: false, disabled: false, classList: { add(){}, remove(){} }, addEventListener(){} }),
  api: () => Promise.resolve({}), toast(){}, esc: s => String(s || ''), qrowHTML: () => '',
  ago: () => '刚刚', view: 'accounts', loadTasks(){}, setInterval: () => 1, clearInterval(){},
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end), ctx);
process.stdout.write(JSON.stringify({
  finished: ctx.taskJobPresentation({ status: 'finished', trigger: 'daily', completed: 29, total: 29, finished_at: '2026-11-14T23:00:00', results: [] }).message,
  running: ctx.taskJobPresentation({ status: 'running', trigger: 'daily', completed: 3, total: 29, current_task: 'chat_5', results: [] }).message,
}));`
	f, err := os.CreateTemp(t.TempDir(), "task-job-time-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("task job finished time node test failed: %v\n%s", err, out)
	}
	var got struct {
		Finished string `json:"finished"`
		Running  string `json:"running"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if !strings.Contains(got.Finished, "结束于 2026-11-14 23:00") {
		t.Fatalf("finished job message = %q, want 结束于 2026-11-14 23:00", got.Finished)
	}
	if strings.Contains(got.Running, "结束于") {
		t.Fatalf("running job must not show finish time: %q", got.Running)
	}
}

// 表头与行内单元格数必须一致，否则整张表错位（与事件日志页同一条硬约束）。
func TestTaskTableColumnCountMatches(t *testing.T) {
	html, err := os.ReadFile(filepath.Join("index.html"))
	if err != nil {
		t.Fatal(err)
	}
	const head = `<th class="mark" aria-hidden="true"></th><th>任务</th><th>进度</th><th>奖励</th><th class="num">结束时间</th><th>状态</th><th class="c-acts"></th>`
	if !strings.Contains(string(html), head) {
		t.Fatal("账号任务表头缺少「结束时间」列（或列顺序变了）")
	}
	js, err := os.ReadFile(filepath.Join("app.js"))
	if err != nil {
		t.Fatal(err)
	}
	// 行模板：mark + who + 进度 + 奖励 + 结束时间 + 状态 + 动作 = 7 个 <td>。
	// 工作区检出是 CRLF，断言前统一成 LF，否则跨平台会假失败。
	const row = `'<td class="num">' + esc(end) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>'`
	if !strings.Contains(strings.ReplaceAll(string(js), "\r\n", "\n"), row) {
		t.Fatal("账号任务行模板缺少结束时间单元格（表头与行内列数会错位）")
	}
}
