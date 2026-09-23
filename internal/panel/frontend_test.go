package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// jsAppJSHarness 是 node 侧 DOM 桩（不直接求值 app.js，由调用方决定断言）：
// 读入 argv[2] 的 app.js 源码、搭一套 inert DOM/网络桩、createContext 成沙箱。
// 顶层冒烟与 rateCell 形态两个测试共用，避免桩代码两份漂移。
//
// 设计要点：fetch 返回永不 settle 的 Promise —— 所有 api() 调用悬停，测试只覆盖
// **同步顶层求值**（TDZ/ReferenceError 正是发生在这里，异步路径不是本闸门的范围）。
const jsAppJSHarness = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, URLSearchParams, TextEncoder, Intl, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
`

// runJSHarness 把 harness 落盘、以 node 执行（工作目录 = 本包目录，argv[2] = app.js）。
// 无 node 环境跳过；非零退出即失败（调用方仍需自查输出标记）。
//
// 带 60s 硬超时：app.js 顶层 start() 会 setInterval，harness 漏写 process.exit 时
// node 事件循环不退出——超时把「测试套件整体挂死」降级成一条明确失败。
func runJSHarness(t *testing.T, label, harness string, extraEnv ...string) []byte {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS test skipped")
	}
	hf, err := os.CreateTemp(t.TempDir(), "js-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, hf.Name(), "app.js")
	cmd.Dir = "." // 测试工作目录 = internal/panel
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: node 退出 %v\n%s", label, err, out)
	}
	return out
}

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	harness := jsAppJSHarness + `try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages"} {
		out := runJSHarness(t, "app.js 顶层求值 "+hash, harness, "SMOKE_HASH="+hash)
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}

// TestRateCellPromoShapes rateCell 倍率列五形态 + 转义（2b0eedd 展示侧）：
// 牌价/生效价/标签/时段说明四种输入组合的 HTML 必须逐字符稳定，且 promo_label /
// promo_note 来自上游目录、一律经 esc——否则拼进 title 属性的引号可闭合属性。
func TestRateCellPromoShapes(t *testing.T) {
	harness := jsAppJSHarness + `vm.runInContext(src, sandbox, { filename: 'app.js' });
const cases = [
  { credits: '0.29x', promo_factor: 0, promo_credits: '0x', promo_label: '限时免费', promo_note: '活动期间免费' },
  { credits: '0.29x', promo_factor: 0.5, promo_credits: '0.15x', promo_label: '夜间折扣', promo_note: '23:00-08:00' },
  { credits: '1x', promo_label: '错峰使用', promo_note: '10:00-18:00' },
  { credits: '1x' },
  {},
  { credits: '1x', promo_label: '<b>x</b>', promo_note: 'a"b & c' },
];
console.log('RATECELL ' + JSON.stringify(cases.map(c => sandbox.rateCell(c))));
process.exit(0); // app.js 顶层 start() 的 setInterval 会让事件循环不退出（同冒烟测试）
`
	out := runJSHarness(t, "rateCell 五形态", harness)
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "RATECELL ") {
			line = strings.TrimPrefix(l, "RATECELL ")
		}
	}
	if line == "" {
		t.Fatalf("rateCell 用例未输出结果:\n%s", out)
	}
	var got []string
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("解析 rateCell 输出: %v\n%s", err, line)
	}
	want := []string{
		// 有折扣：生效价大字 + 标签 + 划线牌价 + 悬停时段说明
		`<span title="活动期间免费" style="cursor:help"><b>0x</b> <span class="tag ok">限时免费</span> <s style="color:var(--ink-3);font-size:11.5px">0.29x</s></span>`,
		`<span title="23:00-08:00" style="cursor:help"><b>0.15x</b> <span class="tag ok">夜间折扣</span> <s style="color:var(--ink-3);font-size:11.5px">0.29x</s></span>`,
		// 仅标签（错峰类，无 discount）：牌价 + 标签
		`<span title="10:00-18:00" style="cursor:help">1x <span class="tag warn">错峰使用</span></span>`,
		// 无优惠：裸牌价
		`1x`,
		// 无牌价无优惠：占位
		`—`,
		// 上游文案必须转义（属性引号 + HTML）
		`<span title="a&quot;b &amp; c" style="cursor:help">1x <span class="tag warn">&lt;b&gt;x&lt;/b&gt;</span></span>`,
	}
	if len(got) != len(want) {
		t.Fatalf("rateCell 输出条数 %d != %d:\n%s", len(got), len(want), line)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rateCell 第 %d 例不符\n got: %s\nwant: %s", i+1, got[i], want[i])
		}
	}
}

// TestScanAllStopsQueuePolling 「扫描待办」必须停掉队列轮询（1d7c97b 语义闸门）：
// 队列在途时点扫描 → 新结果渲染；若轮询没停，下一个 tick 会把队列 items 回写、
// 冲掉扫描结果（用户视角「点了扫描还是上次的执行结果」）。
//
// 用假 setInterval 捕获 tick 回调手动驱动（不真等 3 秒）；DOM 用记录型桩，只断言
// qcList.innerHTML 的归属。断言分两层：扫描后不再有存活 tick；即便存在，其再次
// 执行也**不得**覆盖扫描结果——后者是 1d7c97b 修复前的真实表现（反事实必红）。
func TestScanAllStopsQueuePolling(t *testing.T) {
	harness := jsAppJSHarness + `const timers = [];
sandbox.setInterval = fn => { timers.push(fn); return timers.length; };
sandbox.clearInterval = id => { if (id > 0) timers[id - 1] = null; };
const els = {};
function mkEl(id) {
  return { id, innerHTML: '', textContent: '', hidden: false, style: {}, dataset: {}, title: '',
    classList: { add() {}, remove() {}, toggle() {}, contains: () => false },
    querySelector: () => mkEl(id + ':q'), appendChild() {}, remove() {}, addEventListener() {}, onclick: null,
    children: { length: 0 }, setAttribute() {}, getAttribute: () => null };
}
sandbox.document.getElementById = id => (els[id] || (els[id] = mkEl(id)));
const queueBody = { started: true, running: true, seq: 7, items: [
  { uid: 'u1', nickname: 'nick1', kind: 'growth', code: 'chat_5', status: 'running', message: '执行中' }] };
const scanBody = { accounts: [{ uid: 'u1', nickname: 'nick1', growth: [
  { task_code: 'template_5', title: '使用模板创建任务', current: 0, target: 5 }] }] };
sandbox.fetch = url => {
  const body = String(url).includes('scan_all') ? scanBody : queueBody;
  return Promise.resolve({ status: 200, ok: true, json: async () => body });
};
vm.runInContext(src, sandbox, { filename: 'app.js' });
(async () => {
  // app.js 顶层 start() 已注册一个 5s 刷新定时器 → 队列 tick 是本轮 push 的那一个。
  const qIdx = timers.length;
  sandbox.startQueuePolling();
  const tick = timers[qIdx];
  if (typeof tick !== 'function') { console.log('SCANFAIL 未捕获队列轮询回调'); process.exit(1); }
  await tick();
  const afterTick = els['qcList'].innerHTML;
  await els['btnScanAll'].onclick();          // 用户点「扫描待办」
  const afterScan = els['qcList'].innerHTML;
  let clobbered = null;
  if (timers[qIdx]) { await timers[qIdx](); clobbered = els['qcList'].innerHTML; } // 残留 tick 再跑一次
  console.log('SCANRESULT ' + JSON.stringify({ afterTick, afterScan, live: timers[qIdx] ? 1 : 0, clobbered }));
  process.exit(0);
})().catch(e => { console.log('SCANFAIL ' + (e && e.stack ? e.stack : e)); process.exit(1); });
`
	out := runJSHarness(t, "扫描待办停轮询", harness)
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "SCANRESULT ") {
			line = strings.TrimPrefix(l, "SCANRESULT ")
		}
	}
	if line == "" {
		t.Fatalf("未取得 SCANRESULT:\n%s", out)
	}
	var got struct {
		AfterTick string  `json:"afterTick"`
		AfterScan string  `json:"afterScan"`
		Live      int     `json:"live"`
		Clobbered *string `json:"clobbered"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("解析 SCANRESULT: %v\n%s", err, line)
	}
	if !strings.Contains(got.AfterTick, "chat_5") {
		t.Errorf("队列轮询未渲染队列条目:\n%s", got.AfterTick)
	}
	if !strings.Contains(got.AfterScan, "template_5") {
		t.Errorf("扫描结果未渲染:\n%s", got.AfterScan)
	}
	if got.Live != 0 {
		t.Errorf("点「扫描待办」后仍有 %d 个存活轮询 tick（残留轮询会冲掉扫描结果）", got.Live)
	}
	if got.Clobbered != nil {
		t.Errorf("残留 tick 冲掉了扫描结果，qcList 被回写成:\n%s", *got.Clobbered)
	}
}

// TestModelEntriesPromoContract rateCell 的数据契约（2b0eedd 展示侧的 Go 半边）：
// modelEntries 只在真有优惠时透出 promo_*，promo_factor/promo_credits 成对出现
// （前端 rateCell 以 `promo_factor != null && promo_credits` 为「有折扣」判据），
// 且 credits 牌价原样保留（前端划线对照用）。顺带守住双域前缀没被改坏。
func TestModelEntriesPromoContract(t *testing.T) {
	// newTestPanel 的 Upstream 为 nil（只覆盖鉴权路径）；本测试要读 cfg.Upstream.HTTP，
	// 给一个零值 client——ContextWindow/MaxTokens 都置正，查找链第 1 级即返回，不发请求。
	p := New(Config{Version: "test", APIKey: "test-key", Upstream: &upstream.Client{}})
	half := 0.5
	// ContextWindow/MaxTokens 置正值：四级查找链在第 1 级返回，测试不触发任何网络。
	base := func(id, credits string) upstream.ModelInfo {
		return upstream.ModelInfo{ID: id, Credits: credits, ContextWindow: 200000, MaxTokens: 8192}
	}
	disc := base("glm-5.2", "1x")
	disc.PromoFactor, disc.PromoCredits = &half, "0.50x"
	disc.PromoLabel, disc.PromoNote = "夜间折扣", "23:00-08:00 五折"
	badgeOnly := base("deepseek-v4.1-flash", "0.29x")
	badgeOnly.PromoLabel, badgeOnly.PromoNote = "错峰使用", "10:00-18:00"
	plain := base("plain-model", "1x")

	got := p.modelEntries("cn", "", []upstream.ModelInfo{disc, badgeOnly, plain})
	if len(got) != 3 {
		t.Fatalf("modelEntries 返回 %d 条，期望 3", len(got))
	}
	if got[0]["promo_factor"] != 0.5 || got[0]["promo_credits"] != "0.50x" {
		t.Errorf("折扣形态透出错误: factor=%v credits=%v", got[0]["promo_factor"], got[0]["promo_credits"])
	}
	if got[0]["promo_label"] != "夜间折扣" || got[0]["promo_note"] != "23:00-08:00 五折" {
		t.Errorf("折扣形态标签/说明透出错误: %v / %v", got[0]["promo_label"], got[0]["promo_note"])
	}
	if got[0]["credits"] != "1x" {
		t.Errorf("牌价必须原样透出（前端划线对照）: %v", got[0]["credits"])
	}
	if _, ok := got[1]["promo_factor"]; ok {
		t.Errorf("无 discount 的错峰形态不应透出 promo_factor")
	}
	if _, ok := got[1]["promo_credits"]; ok {
		t.Errorf("无 discount 的错峰形态不应透出 promo_credits")
	}
	if got[1]["promo_label"] != "错峰使用" || got[1]["promo_note"] != "10:00-18:00" {
		t.Errorf("错峰形态标签/说明透出错误: %v / %v", got[1]["promo_label"], got[1]["promo_note"])
	}
	for _, k := range []string{"promo_factor", "promo_credits", "promo_label", "promo_note"} {
		if _, ok := got[2][k]; ok {
			t.Errorf("无优惠模型不应透出 %s", k)
		}
	}
	// 双域分流：global 域条目 id 带前缀，promo 字段同样透出。
	gs := p.modelEntries("global", "global:", []upstream.ModelInfo{disc})
	if gs[0]["id"] != "global:glm-5.2" {
		t.Errorf("global 域 id 前缀错误: %v", gs[0]["id"])
	}
	if gs[0]["promo_factor"] != 0.5 {
		t.Errorf("global 域 promo_factor 未透出: %v", gs[0]["promo_factor"])
	}
}
