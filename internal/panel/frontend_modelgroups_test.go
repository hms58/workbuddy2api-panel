package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestModelGroupsViewWiring 「模型组」菜单与视图必须完整接线：
// 菜单项、视图容器、TITLES、懒加载分支，四者缺一都会让页面白屏或永远空表。
func TestModelGroupsViewWiring(t *testing.T) {
	p := newTestPanel()

	// ① index.html：菜单项 + 视图容器
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()

	if !strings.Contains(html, `data-view="modelgroups"`) {
		t.Fatal("index.html 缺少模型组菜单项 data-view=\"modelgroups\"")
	}
	if !strings.Contains(html, `id="view-modelgroups"`) {
		t.Fatal("index.html 缺少视图容器 id=\"view-modelgroups\"")
	}
	// 菜单项必须排在「模型与档位」之后（用户明确要求的菜单位置）
	iModels := strings.Index(html, `data-view="models"`)
	iGroups := strings.Index(html, `data-view="modelgroups"`)
	if iModels < 0 || iGroups < 0 || iGroups < iModels {
		t.Errorf("模型组菜单应位于「模型与档位」之后（models@%d groups@%d）", iModels, iGroups)
	}
	// 关键控件 id 必须存在（前端逻辑依赖它们，改名会静默白屏）
	for _, id := range []string{"mgList", "mgBody", "mgMembers", "mgAddQ", "mgAddSel", "mgAddBtn", "mgSaveBtn"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少控件 id=%q", id)
		}
	}

	// ② app.js：TITLES + 懒加载 + 两个函数
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()
	if !strings.Contains(js, "modelgroups:") {
		t.Error("app.js TITLES 缺少 modelgroups 标题")
	}
	if !strings.Contains(js, "loadModelGroups") {
		t.Error("app.js 缺少 loadModelGroups")
	}
	if !strings.Contains(js, "renderModelGroups") {
		t.Error("app.js 缺少 renderModelGroups")
	}
	if !strings.Contains(js, "v === 'modelgroups'") {
		t.Error("app.js go() 缺少 modelgroups 懒加载分支")
	}
}

// TestModelGroupsMobileLayout 移动端适配：760px 断点内必须有成员行重排规则。
// 只做桌面而不做窄屏重排是本项目既有页面的固定要求（index.html 的手机版段）。
func TestModelGroupsMobileLayout(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()

	// 找到 760px 媒体查询段，确认其中出现 .mgrow 的重排声明
	idx := strings.Index(html, "@media (max-width: 760px)")
	if idx < 0 {
		t.Fatal("index.html 缺少 760px 断点")
	}
	seg := html[idx:]
	end := strings.Index(seg, "@media")
	if end > 0 {
		seg = seg[:end]
	}
	if !strings.Contains(seg, ".mgrow") {
		t.Error("760px 断点内缺少 .mgrow 重排规则（移动端成员行会挤成一列）")
	}
}

// TestModelGroupsMobileToggleAndSearch ⭐ 用户实测反馈的两条移动端缺陷：
//  1. 成员行在窄屏把 .mglead（启停复选框）整块 display:none —— 手机上没有任何
//     控件能开启/关闭某个模型成员；
//  2. 「添加成员」的搜索框在手机上仍按桌面 1/3 宽渲染，输入时看不全自己打的字。
//
// 两条都只在 760px 断点内生效：桌面浏览器完全看不出来，只能在测试里钉住。
func TestModelGroupsMobileToggleAndSearch(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()

	seg := mobile760ModelGroupBlock(t, html)

	// ① 启停复选框必须可见
	if strings.Contains(seg, ".mglead { display: none;") {
		t.Error("760px 断点把 .mglead 整块隐藏：移动端将没有开启/关闭模型成员的开关")
	}
	if !strings.Contains(seg, ".mgrow .mglead { display: flex;") {
		t.Error("760px 断点未显式保留 .mglead（启停复选框会在窄屏消失）")
	}
	// ② 搜索框必顶满整行（flex 简写覆盖，而不是只改 flex-basis）
	if !strings.Contains(seg, ".mgadd input[type=search],") {
		t.Error("760px 断点缺少「添加成员」搜索框的整行规则")
	}
	if !strings.Contains(seg, "flex: 1 1 100%") {
		t.Error("760px 断点未用 flex 简写把搜索框拉满整行（只改 flex-basis 会被桌面 flex-grow/shrink 卡住）")
	}

	// ③ 搜索接线：除 input 外还要补 change/search/compositionend（手机输入法兜底）
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()
	for _, want := range []string{"function mgAddQSearch", "$('mgAddQ').oninput = mgAddQSearch",
		"$('mgAddQ').onchange = mgAddQSearch", "$('mgAddQ').onsearch = mgAddQSearch",
		"$('mgAddQ').oncompositionend = mgAddQSearch"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %q：手机上搜索框可能输入无反应", want)
		}
	}
}

// mobile760ModelGroupBlock 取出「模型组」那个 760px 媒体查询块（按花括号配平）。
//
// 不能直接用 strings.Index(seg, "@media") 截断：那个位置就是块自身开头，结果是
// 整份剩余文档，断言形同虚设。页面上有多处 760px 断点（导航抽屉另有一处），
// 这里按「块内含 .mgrow」挑出模型组那一块。
func mobile760ModelGroupBlock(t *testing.T, html string) string {
	t.Helper()
	const marker = "@media (max-width: 760px)"
	for off := 0; ; {
		i := strings.Index(html[off:], marker)
		if i < 0 {
			t.Fatal("index.html 缺少模型组的 760px 断点")
		}
		start := off + i
		if blk, ok := balancedBlock(html[start:]); ok {
			if strings.Contains(blk, ".mgrow") {
				return blk
			}
			off = start + len(marker)
			continue
		}
		t.Fatal("760px 断点花括号不配平（后续所有规则会失效）")
	}
}

// balancedBlock 从 s 的第一个 '{' 开始按花括号配平取出完整块。
func balancedBlock(s string) (string, bool) {
	open := strings.Index(s, "{")
	if open < 0 {
		return "", false
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
}

// TestModelGroupsReusesMLState ⭐ Review Focus 第 6 条的前端侧：
// 模型组页的状态标签必须复用账号池「模型锁池」表的 ML_STATE，不得新造一套文案，
// 否则同一状态在两块面板上会显示成不同中文（「全池锁定」vs「该模型额度用完」）。
func TestModelGroupsReusesMLState(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()

	// ML_STATE 只应有一处定义（app.js 里 const ML_STATE = ...）
	if n := strings.Count(js, "const ML_STATE"); n != 1 {
		t.Fatalf("ML_STATE 定义出现 %d 次，want 1（不得为模型组另造一份）", n)
	}
	// 模型组渲染函数必须引用 ML_STATE
	i := strings.Index(js, "function availOfMG")
	if i < 0 {
		t.Fatal("app.js 缺少 availOfMG")
	}
	seg := js[i:]
	if j := strings.Index(seg, "function renderMGMembers"); j > 0 {
		seg = seg[:j]
	}
	if !strings.Contains(seg, "ML_STATE") {
		t.Error("availOfMG 未引用 ML_STATE（状态文案会与账号池锁池表漂移）")
	}
	// 且不得再出现 kind/usable 这类 Cache 字段（展示源已改为 ModelLockView）
	for _, bad := range []string{"st.usable", "st.kind"} {
		if strings.Contains(seg, bad) {
			t.Errorf("availOfMG 仍引用 Cache 字段 %q，应改用 ModelLockView 的 state", bad)
		}
	}
}

// TestModelGroupsMemberPickerHasSource ⭐ 回归：「添加成员」下拉曾经恒为空。
//
// 两个真实缺陷（用户实测发现）：
//  1. mgAllModels 只在 loadModels()（访问「模型与档位」页）时填充。直接进模型组页
//     时它从未被赋值 → 下拉空白，无法添加任何成员。
//  2. /panel/api/models 返回的 id 已带域前缀（cn:xxx / global:xxx），旧代码又拼了一次
//     'cn:'+id → 得到 cn:cn:xxx，即便有目录也选不出合法成员。
//
// 这条测试钉住修法：必须有独立的按需加载入口，且不得再手工拼域前缀。
func TestModelGroupsMemberPickerHasSource(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()

	// ① 必须有独立的按需加载函数（不能只依赖「模型与档位」页先被访问过）
	if !strings.Contains(js, "function mgEnsureModels") {
		t.Error("缺少 mgEnsureModels：直接进模型组页时「添加成员」下拉会恒为空")
	}
	// ② 进入模型组页必须触发它（loadModelGroups 或 renderMGEditor 任一处即可）
	if !strings.Contains(js, "mgEnsureModels()") {
		t.Error("mgEnsureModels 从未被调用：目录不会被加载")
	}
	// ③ 填充分离成一个函数，避免「拼前缀」这类逻辑散落
	if !strings.Contains(js, "function mgFillModels") {
		t.Error("缺少 mgFillModels：目录填充逻辑应集中一处")
	}
	// ④ 绝不能再手工拼域前缀（id 已带 cn:/global:）
	for _, bad := range []string{"'cn:' + id", "'global:' + id"} {
		if strings.Contains(js, bad) {
			t.Errorf("仍手工拼域前缀 %q：/panel/api/models 的 id 已带 cn:/global:，会得到 cn:cn:xxx", bad)
		}
	}
}

// TestModelGroupsMemberSearch ⭐ 「添加成员」必须支持搜索过滤。
//
// 模型目录有 80+ 个模型，单行 <select> 只能盲翻 —— 用户实测反馈要求搜索。
// 这条钉住：搜索框存在、候选渲染走独立的 renderMGAddPicker、且真的按搜索词过滤。
func TestModelGroupsMemberSearch(t *testing.T) {
	p := newTestPanel()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()

	// ① 搜索框必须是 type=search（可点 ✕ 清空，与「模型与档位」页 mdQ 同口径）
	if !strings.Contains(html, `id="mgAddQ" type="search"`) {
		t.Error("index.html 缺少 id=\"mgAddQ\" type=\"search\" 的搜索框")
	}
	// ② 候选列表用 size 展开成多行，而不是单行盲选
	if !strings.Contains(html, `id="mgAddSel" size="`) {
		t.Error("mgAddSel 应为 size 多行列表（单行下拉在 80+ 项时无法浏览）")
	}

	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()

	// ③ 候选渲染必须抽成独立函数（搜索与目录加载都复用它，避免两份过滤逻辑漂移）
	if !strings.Contains(js, "function renderMGAddPicker") {
		t.Error("缺少 renderMGAddPicker：候选渲染应集中一处，供搜索与目录加载复用")
	}
	// ④ 过滤必须真的按搜索词做子串匹配
	if !strings.Contains(js, "includes(q)") {
		t.Error("renderMGAddPicker 未按搜索词子串过滤（q 未参与匹配）")
	}
	// ⑤ 搜索框必须接线（输入即筛）
	if !strings.Contains(js, "$('mgAddQ').oninput") {
		t.Error("mgAddQ 未绑定 oninput：输入不会过滤候选")
	}
	// ⑥ 目录加载完只能刷新候选列表，不能整体重渲染编辑器 ——
	//    后者会用 mgGroups 的值重写 mgID/mgName，冲掉用户正在编辑的内容。
	i := strings.Index(js, "async function mgEnsureModels")
	if i < 0 {
		t.Fatal("缺少 mgEnsureModels")
	}
	seg := js[i:]
	if j := strings.Index(seg, "async function loadModelGroups"); j > 0 {
		seg = seg[:j]
	}
	if strings.Contains(seg, "renderMGEditor()") {
		t.Error("mgEnsureModels 调了 renderMGEditor：会冲掉用户正在编辑的表单内容，应只调 renderMGAddPicker")
	}
}

// TestModelGroupsLockTable 模型组页必须有「模型锁状态」表与「立即嗅探全部」按钮。
//
// 原型与设计文档（§13.5 展示侧、§6.5 嗅探演示）都要求这块，
// 但它曾在实现时整块遗漏 —— 用户对比原型才发现。这条钉住它的存在与接线。
func TestModelGroupsLockTable(t *testing.T) {
	p := newTestPanel()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()

	// ① 表容器与按钮必须存在（id 是前端逻辑的挂载点，改名会静默失效）
	for _, id := range []string{"mgLockBody", "mgLockNote", "mgSniffAllBtn", "mgLockRefreshBtn"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少控件 id=%q（模型锁状态表）", id)
		}
	}
	// ② 表头必须是这 8 列（与原型逐字对齐；改列会让人对不上原型）
	for _, th := range []string{"成员", "可见性", "状态", "可服务 / 总数", "锁因", "最早解锁", "上次探活"} {
		if !strings.Contains(html, "<th>"+th+"</th>") {
			t.Errorf("模型锁状态表缺少表头 %q", th)
		}
	}
	// ③ 按钮文案必须是「立即嗅探全部」（用户按原型对照验收的锚点）
	if !strings.Contains(html, ">立即嗅探全部<") {
		t.Error("缺少「立即嗅探全部」按钮文案")
	}

	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()

	// ④ 渲染函数与事件接线
	for _, want := range []string{"function renderMGLocks", "mgSniffAll", "$('mgSniffAllBtn').onclick",
		"function mgRefreshLocks", "$('mgLockRefreshBtn').onclick"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %q", want)
		}
	}
	// 刷新按钮必须只重读状态，不得调嗅探接口 —— 否则「刷新」会变成隐式探活，
	// 消耗额度且受频率上限约束，与用户点「刷新」的预期完全不符。
	ri := strings.Index(js, "async function mgRefreshLocks")
	if ri < 0 {
		t.Fatal("缺少 mgRefreshLocks")
	}
	rseg := js[ri:]
	if j := strings.Index(rseg, "function parseDurMs"); j > 0 {
		rseg = rseg[:j]
	}
	if strings.Contains(rseg, "model_groups/sniff") {
		t.Error("mgRefreshLocks 调了 sniff 接口：刷新不应触发探活（会消耗额度）")
	}
	// 两个按钮必须在同一个动作组里（header 可换行，分散放置窄屏会被拆行）
	if !strings.Contains(html, `class="mglockbtns"`) {
		t.Error("两个动作按钮应包在 .mglockbtns 组内，保证并列且间距一致")
	}
	// ⑤ 必须调后端接口，而不是在前端假装嗅探
	if !strings.Contains(js, "model_groups/sniff") {
		t.Error("app.js 未调用 POST model_groups/sniff")
	}
	// ⑥ 锁表只列可见成员：停用与时段外的成员不参与嗅探，也不该出现在表里。
	//    过滤集中在 collectVisibleLocks（锁表与嗅探回执共用，避免两份判据漂移）。
	i := strings.Index(js, "function collectVisibleLocks")
	if i < 0 {
		t.Fatal("缺少 collectVisibleLocks：可见性过滤应集中一处")
	}
	seg := js[i:]
	if j := strings.Index(seg, "function renderMGLocks"); j > 0 {
		seg = seg[:j]
	}
	if !strings.Contains(seg, "g.enabled") {
		t.Error("collectVisibleLocks 未过滤停用组：停用组的成员不该进锁状态表")
	}
	if !strings.Contains(seg, "m.enabled && inWin(m.window)") {
		t.Error("collectVisibleLocks 未按「成员启用且时段内」过滤：停用/时段外成员不该进锁状态表")
	}
	// ⑦ 能否嗅探必须由后端判定（canSniff），不得前端自己猜锁因
	i = strings.Index(js, "function renderMGLocks")
	if i < 0 {
		t.Fatal("缺少 renderMGLocks")
	}
	seg = js[i:]
	if j := strings.Index(seg, "async function mgSniffOne"); j > 0 {
		seg = seg[:j]
	}
	if !strings.Contains(seg, "canSniff") {
		t.Error("renderMGLocks 未使用后端下发的 can_sniff：前端自猜锁因会与 RunOnce 过滤条件漂移")
	}
	// ⑧ 锁表必须复用 collectVisibleLocks（否则可见性判据会出现第二份）
	if !strings.Contains(seg, "collectVisibleLocks()") {
		t.Error("renderMGLocks 未复用 collectVisibleLocks：可见性过滤会与嗅探回执漂移")
	}
}

// TestModelGroupsCSSParses ⭐ 回归：CSS 花括号必须平衡，且模型组样式必须真的被解析。
//
// 真实缺陷（浏览器实测发现）：上游提交在 .bh-daylegend 的两条规则里漏了右大括号，
// 导致 CSS 解析器把后续规则全部当成该规则的延续 —— 模型组样式整块失效，
// 表现为「移动端仍按桌面 5 列渲染、成员行挤成一列」，而所有 Go 测试全绿。
//
// 这里只做机械校验：括号平衡 + 模型组关键规则存在 + 760px 断点排在基础样式之后。
func TestModelGroupsCSSParses(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()

	i := strings.Index(html, "<style>")
	j := strings.Index(html, "</style>")
	if i < 0 || j < 0 {
		t.Fatal("index.html 缺少 <style> 段")
	}
	css := html[i+7 : j]

	if o, c := strings.Count(css, "{"), strings.Count(css, "}"); o != c {
		t.Fatalf("CSS 花括号不平衡：{ = %d, } = %d（漏括号会让其后所有规则失效）", o, c)
	}

	// 模型组关键规则必须存在
	for _, sel := range []string{".mgrow {", ".mgwrap {", ".mgitem {", ".mgwin {"} {
		if !strings.Contains(css, sel) {
			t.Errorf("CSS 缺少模型组规则 %q", sel)
		}
	}

	// 760px 断点里的 .mgrow 重排必须排在基础 .mgrow 之后：
	// 同特异性规则靠源码顺序决胜负，写前面会被后面的桌面 5 列定义盖掉。
	base := strings.Index(css, ".mgrow {")         // 第一处 = 桌面基础样式
	lastMrow := strings.LastIndex(css, ".mgrow {") // 最后一处 = 断点内的重排
	if base < 0 {
		t.Fatal("CSS 缺少 .mgrow 基础样式")
	}
	if base == lastMrow {
		t.Fatal("CSS 只有一处 .mgrow：窄屏重排规则缺失")
	}
	// 断点内的那条 .mgrow 之前，必须能找到一个 760px 媒体查询把它包住
	mediaBefore := strings.LastIndex(css[:lastMrow], "@media (max-width: 760px)")
	if mediaBefore < 0 {
		t.Error("最后一处 .mgrow 不在 760px 断点内：窄屏重排规则没生效")
	}
	if lastMrow < base {
		t.Error("760px 断点写在 .mgrow 基础样式之前：移动端会被桌面 5 列定义覆盖")
	}
}
