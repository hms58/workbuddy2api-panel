package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/modelgroup"
	"github.com/misakano7545/workbuddy2api-panel/internal/reqlog"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestSplitGroupPrefix 前缀解析：只认精确的 "group:"，其余一律不命中。
func TestSplitGroupPrefix(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		wantOK bool
	}{
		{"group:longtask", "longtask", true},
		{"group:LongTask", "longtask", true}, // 组 id 归一化为小写
		{"group:", "", false},                // 空 id
		{"group", "", false},                 // 缺冒号
		{"Group:longtask", "", false},        // 大小写敏感（与 cn:/global: 同口径）
		{"cn:group:x", "", false},            // 不是首段
		{"deepseek", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		id, ok := splitGroupPrefix(c.in)
		if ok != c.wantOK || id != c.wantID {
			t.Errorf("splitGroupPrefix(%q) = (%q,%v), want (%q,%v)", c.in, id, ok, c.wantID, c.wantOK)
		}
	}
}

// TestGroupUnknownReturnsNotFound 未知组 id 必须能被上层识别为「无此组」，
// 而不是回落成裸模型名去打上游。
func TestGroupUnknownReturnsNotFound(t *testing.T) {
	h := newGroupTestHandler(t, nil)
	if _, ok := h.groupPlan("nosuch", time.Now()); ok {
		t.Errorf("未知组不应返回计划")
	}
}

// newGroupTestHandler 构造一个带组集合的 handler。
func newGroupTestHandler(t *testing.T, groups []modelgroup.GroupConfig) *Handler {
	t.Helper()
	compiled, opts := modelgroup.Compile(groups, modelgroup.Options{PreferLastOK: true})
	gs := modelgroup.NewGroupSet(compiled, opts)
	p := testPoolWith(cnAuthForTest("u1"))
	return NewHandler(Config{
		Pool:      p,
		MaxRotate: 3,
		Groups:    func() *modelgroup.GroupSet { return gs },
	})
}

// cnAuthForTest 国内域账号。域名必须用 codebuddy.cn：workbuddy.ai 与 codebuddy.ai
// 的任意子域都被 auth.isGlobalDomain 判为 global。
func cnAuthForTest(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, Domain: "www.codebuddy.cn", AccessToken: "at-" + uid, ExpiresAt: 9999999999}
}

// globalAuthForTest 国际域账号。
func globalAuthForTest(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, Domain: "www.workbuddy.ai", AccessToken: "at-" + uid, ExpiresAt: 9999999999}
}

// groupAwareHandler 构造带组与缓存的 handler。
func groupAwareHandler(t *testing.T, groups []modelgroup.GroupConfig, cache *modelgroup.Cache, up *upstream.Client, poolSize int) *Handler {
	t.Helper()
	compiled, opts := modelgroup.Compile(groups, modelgroup.Options{PreferLastOK: true})
	gs := modelgroup.NewGroupSet(compiled, opts)
	auths := make([]*auth.Auth, 0, poolSize)
	for i := 0; i < poolSize; i++ {
		auths = append(auths, cnAuthForTest("u"+string(rune('0'+i))))
	}
	p := testPoolWith(auths...)
	return NewHandler(Config{
		Pool:      p,
		Upstream:  up,
		MaxRotate: 3,
		Groups:    func() *modelgroup.GroupSet { return gs },
		Cache:     cache,
	})
}

// TestGroupAdvancesToNextMember 核心行为：成员 1 被 6004 → 成员 2 成功。
// 断言上游依次收到 [m1, m2]，证明真的换了模型。
func TestGroupAdvancesToNextMember(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply,
		struct {
			status int
			body   string
		}{429, rateLimit6004Body},
		struct {
			status int
			body   string
		}{200, okChatSSE},
	)
	h := groupAwareHandler(t, []modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{
			{Model: "cn:m1", Weight: 100},
			{Model: "cn:m2", Weight: 90},
		},
	}}, &modelgroup.Cache{}, up.client(), 1)

	rec := doFallbackChat(h, "group:g")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200（应降级到成员 2），body=%s", rec.Code, rec.Body)
	}
	got := up.seen()
	want := []string{"m1", "m2"}
	if len(got) != len(want) {
		t.Fatalf("上游收到 %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 次上游模型 = %q, want %q（全序列 %v）", i, got[i], want[i], got)
		}
	}
}

// TestGroupStaysOnMemberWhileAccountsRemain ⭐ 用户实测反馈的缺陷：
// 成员内**只要有一个号**被 6004 模型级限流，整组就跳到下一个模型 —— 正确语义是
// 「该域全部账号都服务不了这个模型」才推进成员，还剩号能服务时应在同成员内换号。
// 这条钉住：两个号、m1 上第一个号 6004，上游第二次仍应收到 m1（而不是 m2）。
func TestGroupStaysOnMemberWhileAccountsRemain(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply,
		struct {
			status int
			body   string
		}{429, rateLimit6004Body},
		struct {
			status int
			body   string
		}{200, okChatSSE},
	)
	h := groupAwareHandler(t, []modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{
			{Model: "cn:m1", Weight: 100},
			{Model: "cn:m2", Weight: 90},
		},
	}}, &modelgroup.Cache{}, up.client(), 2) // 两个号：一个被 6004，另一个仍能服务 m1

	rec := doFallbackChat(h, "group:g")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200（同成员换号后应成功），body=%s", rec.Code, rec.Body)
	}
	got := up.seen()
	want := []string{"m1", "m1"}
	if len(got) != len(want) {
		t.Fatalf("上游收到 %v, want %v（成员内还有号就不该推进成员）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 次上游模型 = %q, want %q（全序列 %v）", i, got[i], want[i], got)
		}
	}
}

// TestGroupStickyPretestUsesMemberModel ⭐ 组内 global 成员的会话粘性必须生效。
//
// 缺陷：粘性预检用的是 routeModel（组请求里是 "group:<id>"），SplitRealmPrefix 对
// 组名回落 realm=cn，于是预检集合只剩 cn 账号 —— global 成员的绑定号永远"不可用"，
// 每请求都被重新分配，粘性实际失效（直连 global:xxx 正常）。
//
// 断言：同一会话连发两次，必须落在同一个账号上。修复前第二次会因预检失败回落加权
// 轮换（首个账号刚用过、被 minPickGap 跳过）而换号。
func TestGroupStickyPretestUsesMemberModel(t *testing.T) {
	p := testPoolWith(globalAuthForTest("ga"), globalAuthForTest("gb"))
	// 与 main 的接线同口径：按 (realm, 裸名) 过滤可用账号。
	sess := session.New(session.Config{AvailableForModel: func(model string) []string {
		realm, bare := ResolveModel(model)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}})
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})

	compiled, opts := modelgroup.Compile([]modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{{Model: "global:gm", Weight: 100}},
	}}, modelgroup.Options{})
	h := NewHandler(Config{
		Pool:      p,
		Upstream:  up.client(),
		MaxRotate: 3,
		Groups:    func() *modelgroup.GroupSet { return modelgroup.NewGroupSet(compiled, opts) },
		Session:   sess,
	})

	const body = `{"model":"group:g","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`
	send := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
		}
		return rec.Header().Get("X-Wb-Account")
	}
	first := send()
	second := send()
	if first == "" || second == "" {
		t.Fatalf("X-Wb-Account 缺失：first=%q second=%q", first, second)
	}
	if first != second {
		t.Fatalf("同一会话应复用同一账号，实际 first=%s second=%s（粘性预检没按成员真实模型过滤）", first, second)
	}
}

// TestGroupRequestLogShowsUsedMember ⭐ 运行日志必须显示「组名/实际成员」。
//
// 用户实测反馈：日志里只有 group:auto-flash，看不出实际用了哪个模型。
// 这条走真实请求链路（含组内降级），断言最终落库的 group_model 是**降级后**的成员，
// 而不是首个成员 —— 后者正是最容易写错的地方（st.usedModel 忘同步）。
func TestGroupRequestLogShowsUsedMember(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply,
		struct {
			status int
			body   string
		}{429, rateLimit6004Body},
		struct {
			status int
			body   string
		}{200, okChatSSE},
	)
	reqLog := reqlog.New(reqlog.Config{})
	compiled, opts := modelgroup.Compile([]modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{
			{Model: "cn:m1", Weight: 100},
			{Model: "cn:m2", Weight: 90},
		},
	}}, modelgroup.Options{PreferLastOK: true})
	h := NewHandler(Config{
		Pool:       testPoolWith(cnAuthForTest("u1")),
		Upstream:   up.client(),
		MaxRotate:  3,
		Groups:     func() *modelgroup.GroupSet { return modelgroup.NewGroupSet(compiled, opts) },
		Cache:      &modelgroup.Cache{},
		RequestLog: reqLog,
	})

	rec := doFallbackChat(h, "group:g")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200（应降级到成员 2），body=%s", rec.Code, rec.Body)
	}

	s := reqLog.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("请求台账应记 1 条，实际 %d：%+v", len(s.Recent), s.Recent)
	}
	e := s.Recent[0]
	// 展示串必须是降级后的 m2：若写成 m1，说明推进时没同步 usedModel。
	if want := "group:g/cn:m2"; e.GroupModel != want {
		t.Errorf("GroupModel = %q, want %q（必须是降级后的成员）", e.GroupModel, want)
	}
	// Model 保持组名，筛选键不被污染
	if e.Model != "group:g" {
		t.Errorf("Model = %q, want group:g", e.Model)
	}
}

// TestGroupRequestLogFirstMemberSucceeds ⭐ 首个成员即成功时的日志展示。
//
// 与上一条互补：那条测「降级后」，这条测「没降级」。
// 二者是不同代码路径 —— 初始化时 usedModel 若错用客户端点名的 "group:<id>"
// （而非成员裸名），会拼出 "cn:group:g" 这种错串，而这条测试正好钉住它。
func TestGroupRequestLogFirstMemberSucceeds(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})
	reqLog := reqlog.New(reqlog.Config{})
	compiled, opts := modelgroup.Compile([]modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{{Model: "cn:m1", Weight: 100}},
	}}, modelgroup.Options{PreferLastOK: true})
	h := NewHandler(Config{
		Pool:       testPoolWith(cnAuthForTest("u1")),
		Upstream:   up.client(),
		MaxRotate:  3,
		Groups:     func() *modelgroup.GroupSet { return modelgroup.NewGroupSet(compiled, opts) },
		Cache:      &modelgroup.Cache{},
		RequestLog: reqLog,
	})

	if rec := doFallbackChat(h, "group:g"); rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	s := reqLog.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("应记 1 条，实际 %d", len(s.Recent))
	}
	e := s.Recent[0]
	// 必须是 "group:g/cn:m1"，不能是 "group:g/cn:group:g"
	if want := "group:g/cn:m1"; e.GroupModel != want {
		t.Errorf("GroupModel = %q, want %q（初始化不得用客户端点名的组名拼成员名）", e.GroupModel, want)
	}
}

// TestGroupDisabledMemberSkipped 停用成员直接跳过：上游只会收到 m2。
func TestGroupDisabledMemberSkipped(t *testing.T) {
	off := false
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})
	h := groupAwareHandler(t, []modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{
			{Model: "cn:m1", Weight: 100, Enabled: &off},
			{Model: "cn:m2", Weight: 90},
		},
	}}, &modelgroup.Cache{}, up.client(), 1)

	rec := doFallbackChat(h, "group:g")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	got := up.seen()
	if len(got) != 1 || got[0] != "m2" {
		t.Fatalf("上游收到 %v, want 仅 [m2]（m1 已停用）", got)
	}
}

// TestGroupAllMembersHiddenReturns400 全部成员停用/时段外 → 400 model_unavailable，
// 且文案要说明是「没有当前可用的成员」（配置问题，不是上游问题）。
func TestGroupAllMembersHiddenReturns400(t *testing.T) {
	off := false
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})
	h := groupAwareHandler(t, []modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{{Model: "cn:m1", Weight: 100, Enabled: &off}},
	}}, &modelgroup.Cache{}, up.client(), 1)

	rec := doFallbackChat(h, "group:g")
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(up.seen()) != 0 {
		t.Errorf("全部成员不可见时不应打上游，实际 %v", up.seen())
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Errorf("错误码应为 model_unavailable，实际 %s", rec.Body)
	}
}

// TestGroupUnknownReturns400 未知组 id → 400，不回落成裸模型名。
func TestGroupUnknownReturns400(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})
	h := groupAwareHandler(t, nil, &modelgroup.Cache{}, up.client(), 1)

	rec := doFallbackChat(h, "group:nosuch")
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400（未知组不该回落打上游）", rec.Code)
	}
	if len(up.seen()) != 0 {
		t.Errorf("未知组不应打上游，实际 %v", up.seen())
	}
}

// TestGroupCrossRealmReparsesRealm ⭐ Review Focus 第 5 条：
// 跨域成员必须切到对应域账号。组第二个成员是 global:，必须路由到 global 号。
func TestGroupCrossRealmReparsesRealm(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply,
		struct {
			status int
			body   string
		}{429, rateLimit6004Body},
		struct {
			status int
			body   string
		}{200, okChatSSE},
	)
	compiled, opts := modelgroup.Compile([]modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{
			{Model: "cn:m1", Weight: 100},
			{Model: "global:gm", Weight: 90},
		},
	}}, modelgroup.Options{PreferLastOK: true})
	gs := modelgroup.NewGroupSet(compiled, opts)
	p := testPoolWith(
		cnAuthForTest("cn1"),
		globalAuthForTest("g1"),
	)
	h := NewHandler(Config{
		Pool: p, Upstream: up.client(), MaxRotate: 3,
		Groups: func() *modelgroup.GroupSet { return gs },
		Cache:  &modelgroup.Cache{},
	})

	rec := doFallbackChat(h, "group:g")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	got := up.seen()
	want := []string{"m1", "gm"}
	if len(got) != len(want) {
		t.Fatalf("上游收到 %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 次上游模型 = %q, want %q（全序列 %v）", i, got[i], want[i], got)
		}
	}
}

// TestGroupAdvancesReusesAccountHealthyOnNextMember ⭐ Review Focus 第 1 条：
// 账号在成员 1 上失败（模型级 6004，只锁该模型），推进到成员 2 时必须重新
// 拿到该账号的机会 —— 这是组内推进的主要收益来源。
// 单账号池：若 tried 未清空，成员 2 会选不到号而误报 503。
func TestGroupAdvancesReusesAccountHealthyOnNextMember(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply,
		struct {
			status int
			body   string
		}{429, rateLimit6004Body},
		struct {
			status int
			body   string
		}{200, okChatSSE},
	)
	h := groupAwareHandler(t, []modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{
			{Model: "cn:m1", Weight: 100},
			{Model: "cn:m2", Weight: 90},
		},
	}}, &modelgroup.Cache{}, up.client(), 1) // 单账号池

	rec := doFallbackChat(h, "group:g")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200（单账号在 m1 被模型级锁后应能在 m2 上重试），body=%s", rec.Code, rec.Body)
	}
}

// TestGroupResponseEchoesGroupName 响应里的 model 回显客户端原名 group:g。
func TestGroupResponseEchoesGroupName(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})
	h := groupAwareHandler(t, []modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{{Model: "cn:m1", Weight: 100}},
	}}, &modelgroup.Cache{}, up.client(), 1)

	rec := doFallbackChat(h, "group:g")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "group:g") {
		t.Errorf("响应应回显 group:g，实际 %s", rec.Body)
	}
}

// TestGroupDoesNotBreakPlainModel 非组请求零回归：裸模型名照旧直连。
func TestGroupDoesNotBreakPlainModel(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})
	h := groupAwareHandler(t, []modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{{Model: "cn:m1", Weight: 100}},
	}}, &modelgroup.Cache{}, up.client(), 1)

	rec := doFallbackChat(h, "plain-model")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	got := up.seen()
	if len(got) != 1 || got[0] != "plain-model" {
		t.Fatalf("非组请求应原样透传模型名，上游收到 %v", got)
	}
}

// TestModelListIncludesEnabledGroups /v1/models 必须列出启用的组，
// 否则客户端在模型选择器里看不到 group:<id>，等于功能不可发现。
// 停用的组不列出；被 models.hidden 隐藏的组名也不列出。
func TestModelListIncludesEnabledGroups(t *testing.T) {
	compiled, opts := modelgroup.Compile([]modelgroup.GroupConfig{
		{ID: "on", Name: "启用组", Enabled: true, Members: []modelgroup.MemberConfig{{Model: "cn:m1"}}},
		{ID: "off", Name: "停用组", Enabled: false, Members: []modelgroup.MemberConfig{{Model: "cn:m2"}}},
	}, modelgroup.Options{})
	gs := modelgroup.NewGroupSet(compiled, opts)

	// modelList 会顺带拉上游目录：给一个空目录的假上游，避免 nil client panic。
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{"models":[]}`, false })
	h := NewHandler(Config{
		Pool:      testPoolWith(cnAuthForTest("u1")),
		Upstream:  up,
		MaxRotate: 3,
		Groups:    func() *modelgroup.GroupSet { return gs },
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析: %v（body=%s）", err, rec.Body)
	}
	var foundOn, foundOff bool
	for _, d := range resp.Data {
		if d.ID == "group:on" {
			foundOn = true
			if d.OwnedBy != "model-group" {
				t.Errorf("group:on owned_by = %q, want model-group", d.OwnedBy)
			}
		}
		if d.ID == "group:off" {
			foundOff = true
		}
	}
	if !foundOn {
		t.Error("/v1/models 应列出启用的组 group:on")
	}
	if foundOff {
		t.Error("/v1/models 不应列出停用的组 group:off")
	}
}

// TestModelListHidesGroupWhenHidden models.hidden 对组名同样生效：
// 否则「隐藏了某模型」但它仍在组里被用到，隐藏机制出现绕过口。
func TestModelListHidesGroupWhenHidden(t *testing.T) {
	compiled, opts := modelgroup.Compile([]modelgroup.GroupConfig{
		{ID: "secret", Enabled: true, Members: []modelgroup.MemberConfig{{Model: "cn:m1"}}},
	}, modelgroup.Options{})
	gs := modelgroup.NewGroupSet(compiled, opts)

	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{"models":[]}`, false })
	h := NewHandler(Config{
		Pool:         testPoolWith(cnAuthForTest("u1")),
		Upstream:     up,
		MaxRotate:    3,
		Groups:       func() *modelgroup.GroupSet { return gs },
		ModelsHidden: []string{"group:secret"},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if strings.Contains(rec.Body.String(), "group:secret") {
		t.Errorf("hidden 的组不应出现在 /v1/models，实际 %s", rec.Body)
	}
}
