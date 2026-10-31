package panel

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/modelgroup"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

func testGroupPanel(t *testing.T, groups []modelgroup.GroupConfig, cache *modelgroup.Cache) (*Panel, *[]modelgroup.GroupConfig) {
	t.Helper()
	compiled, opts := modelgroup.Compile(groups, modelgroup.Options{PreferLastOK: true})
	gs := modelgroup.NewGroupSet(compiled, opts)
	if cache == nil {
		cache = &modelgroup.Cache{}
	}
	var saved []modelgroup.GroupConfig
	p := New(Config{
		APIKey:     "",
		GroupSet:   func() *modelgroup.GroupSet { return gs },
		GroupCache: func() *modelgroup.Cache { return cache },
		SaveModelGroups: func(g []modelgroup.GroupConfig, _ modelgroup.Options) error {
			saved = g
			return nil
		},
	})
	return p, &saved
}

// TestModelGroupsGetReturnsGroupsAndState 只读接口：返回组配置 + 每成员运行态。
// 必须包含 enabled / window / in_window / state / servable / total / locked，
// 前端据此渲染「已停用」「时段外」「部分限流」「全池锁定」。
//
// ⭐ 状态字段来自 pool.ModelLockView()，不是 modelgroup.Cache（见设计文档 §13.5）。
// 这里刻意只往 Cache 写数据、不往 Pool 写模型级冷却：
// 若实现偷懒读了 Cache，下面断言会拿到「锁定」，而期望是「可用」。
func TestModelGroupsGetReturnsGroupsAndState(t *testing.T) {
	cache := &modelgroup.Cache{}
	now := time.Now()
	// 故意在 Cache 里写一条「锁定」记录，但它不该影响面板展示
	cache.MarkLocked("cn", "m1", modelgroup.State{
		Kind: modelgroup.KindModelRate, Reason: "6004 cache-only",
		Until: now.Add(time.Hour), Servable: 0, LockedAccts: 2,
	}, now)

	off := false
	p, _ := testGroupPanel(t, []modelgroup.GroupConfig{{
		ID: "g", Name: "组", Enabled: true,
		Members: []modelgroup.MemberConfig{
			{Model: "cn:m1", Weight: 100},
			{Model: "cn:m2", Weight: 90, Enabled: &off},
			{Model: "cn:m3", Weight: 80, Window: "23:00-07:00"},
		},
	}}, cache)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/model_groups", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		OK     bool `json:"ok"`
		Groups []struct {
			ID      string `json:"id"`
			Members []struct {
				Model    string `json:"model"`
				Enabled  bool   `json:"enabled"`
				Window   string `json:"window"`
				InWindow bool   `json:"in_window"`
				State    string `json:"state"`
				Servable int    `json:"servable"`
				Total    int    `json:"total"`
				Locked   int    `json:"locked"`
				Reason   string `json:"reason"`
			} `json:"members"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v（body=%s）", err, rec.Body)
	}
	if !resp.OK || len(resp.Groups) != 1 {
		t.Fatalf("ok=%v groups=%d, want ok=true groups=1", resp.OK, len(resp.Groups))
	}
	ms := resp.Groups[0].Members
	if len(ms) != 3 {
		t.Fatalf("members = %d, want 3", len(ms))
	}
	if !ms[0].Enabled {
		t.Errorf("m1 enabled 应为 true")
	}
	// ⭐ 关键断言：池里没有该模型的冷却 → state 必须是「可用」，不能是 Cache 里的「锁定」
	if ms[0].State != "" && ms[0].State != "partial" {
		t.Errorf("m1 state = %q，want 空或 partial（池内无冷却；若得到 locked 说明读了 Cache）", ms[0].State)
	}
	if ms[0].Reason == "6004 cache-only" {
		t.Errorf("m1 reason 透出了 Cache 内容，说明展示源接错（应读 ModelLockView）")
	}
	if ms[1].Enabled {
		t.Errorf("m2 enabled 应为 false")
	}
	if ms[2].Window != "23:00-07:00" {
		t.Errorf("m3 window = %q, want 23:00-07:00", ms[2].Window)
	}
}

// TestModelGroupsStateComesFromModelLockView ⭐ Review Focus 第 6 条：
// 成员状态必须与账号池「模型锁池」表逐字一致。
//
// 造一个真实池：3 个 cn 账号，对 glm 锁 1 个 → partial；锁 3 个 → locked。
// 两步都要求两处接口给出相同 state。
func TestModelGroupsStateComesFromModelLockView(t *testing.T) {
	p := pool.New("")
	// 域名必须用 codebuddy.cn：workbuddy.ai 的任意子域都被 auth.isGlobalDomain
	// 判为 global，用错会让「cn 域」测试整体错域。
	for _, u := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: u, Domain: "www.codebuddy.cn", AccessToken: "at-" + u, ExpiresAt: 9999999999})
		p.SetCredits(u, 1000, 0)
	}

	compiled, opts := modelgroup.Compile([]modelgroup.GroupConfig{{
		ID: "g", Enabled: true,
		Members: []modelgroup.MemberConfig{{Model: "cn:glm", Weight: 100}},
	}}, modelgroup.Options{})
	gs := modelgroup.NewGroupSet(compiled, opts)

	pn := New(Config{
		Pool:     p,
		GroupSet: func() *modelgroup.GroupSet { return gs },
	})

	// 两处接口各取一次 state
	readStates := func() (fromLocks, fromGroups string) {
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
		var ov struct {
			ModelLocks []pool.ModelLockRow `json:"model_locks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
			t.Fatalf("overview 解析: %v", err)
		}
		for _, r := range ov.ModelLocks {
			if r.Model == "glm" && r.Realm == "cn" {
				fromLocks = r.State
			}
		}

		rec = httptest.NewRecorder()
		pn.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/model_groups", nil))
		var mg struct {
			Groups []struct {
				Members []struct {
					Model string `json:"model"`
					State string `json:"state"`
				} `json:"members"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &mg); err != nil {
			t.Fatalf("model_groups 解析: %v", err)
		}
		for _, g := range mg.Groups {
			for _, m := range g.Members {
				if m.Model == "cn:glm" {
					fromGroups = m.State
				}
			}
		}
		return fromLocks, fromGroups
	}

	cases := []struct {
		name  string
		setup func()
		want  string
	}{
		{"锁 1 个号", func() {
			p.CooldownSoftForModel("u1", time.Hour, time.Now().Add(time.Hour), "glm", "6004 one")
		}, "partial"},
		{"锁 3 个号", func() {
			p.CooldownSoftForModel("u2", time.Hour, time.Now().Add(time.Hour), "glm", "6004 two")
			p.CooldownSoftForModel("u3", time.Hour, time.Now().Add(time.Hour), "glm", "6004 three")
		}, "locked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.setup()
			locks, groups := readStates()
			if locks != c.want {
				t.Fatalf("overview.model_locks state = %q, want %q", locks, c.want)
			}
			if groups != locks {
				t.Errorf("两处状态不一致：model_locks=%q, model_groups=%q（必须同源）", locks, groups)
			}
		})
	}
}

// TestModelGroupsPostSavesAndValidates 保存接口：合法 payload 落盘；
// 空 id / 重复 id / 无成员 返回 400。
func TestModelGroupsPostSavesAndValidates(t *testing.T) {
	p, saved := testGroupPanel(t, nil, nil)

	body := `{"groups":[{"id":"g1","name":"组","enabled":true,"members":[{"model":"cn:m1","weight":100,"enabled":true,"window":"23:00-07:00"}]}],"options":{"prefer_last_ok":true,"sniff_enabled":true,"sniff_interval":"60s","sniff_min_until":"5m","sniff_min_gap":"10m"}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/model_groups", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(*saved) != 1 || (*saved)[0].ID != "g1" {
		t.Fatalf("保存内容 = %+v", *saved)
	}
	if (*saved)[0].Members[0].Window != "23:00-07:00" {
		t.Errorf("window 应原样保存，实际 %q", (*saved)[0].Members[0].Window)
	}

	// 空 id → 400
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/model_groups",
		strings.NewReader(`{"groups":[{"id":"","members":[{"model":"cn:m1"}]}]}`))
	p.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("空 id 应 400，实际 %d", rec.Code)
	}

	// 无成员 → 400
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/model_groups",
		strings.NewReader(`{"groups":[{"id":"g","members":[]}]}`))
	p.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("无成员应 400，实际 %d", rec.Code)
	}

	// 重复 id → 400
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/model_groups",
		strings.NewReader(`{"groups":[{"id":"g","members":[{"model":"cn:a"}]},{"id":"g","members":[{"model":"cn:b"}]}]}`))
	p.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("重复 id 应 400，实际 %d", rec.Code)
	}
}

// TestModelGroupsPausedAccountsNotCounted ⭐ 暂停选号的账号不算「可服务」。
//
// 用户实测：把国际账号全部暂停选号后，global 成员仍显示「全部/全部」，
// 而实际是 0 个可用账号 —— 展示与实际相反，会让人误判该成员能顶上。
//
// 两种口径都必须对：
//   - 全部暂停 → 0/0（该域等同没有账号）
//   - 部分暂停 → 可服务数排除暂停的号，总数也排除
func TestModelGroupsPausedAccountsNotCounted(t *testing.T) {
	// cn 用国内域名、global 用国际域名：Auth.Realm() 按 Domain 判定域。
	newPoolWith := func() *pool.Pool {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "at", ExpiresAt: 9999999999})
		p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at", ExpiresAt: 9999999999})
		p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at", ExpiresAt: 9999999999})
		return p
	}
	get := func(t *testing.T, p *pool.Pool) map[string]map[string]any {
		t.Helper()
		compiled, opts := modelgroup.Compile([]modelgroup.GroupConfig{{
			ID: "g", Name: "组", Enabled: true,
			Members: []modelgroup.MemberConfig{
				{Model: "cn:m1", Weight: 100},
				{Model: "global:m1", Weight: 90},
			},
		}}, modelgroup.Options{PreferLastOK: true})
		gs := modelgroup.NewGroupSet(compiled, opts)
		pn := New(Config{
			APIKey:   "",
			Pool:     p,
			GroupSet: func() *modelgroup.GroupSet { return gs },
		})
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/model_groups", nil))
		if rec.Code != 200 {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
		}
		var resp struct {
			Groups []struct {
				Members []map[string]any `json:"members"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("解析: %v（body=%s）", err, rec.Body)
		}
		out := map[string]map[string]any{}
		for _, m := range resp.Groups[0].Members {
			out[m["model"].(string)] = m
		}
		return out
	}

	// ① 全部国际账号暂停 → global 成员 0/0，cn 成员不受影响
	p := newPoolWith()
	if !p.Pause("g1") || !p.Pause("g2") {
		t.Fatal("Pause 失败")
	}
	ms := get(t, p)
	if got := ms["global:m1"]["total"]; got != float64(0) {
		t.Errorf("国际账号全暂停：global total = %v, want 0（不该算进可路由账号）", got)
	}
	if got := ms["global:m1"]["servable"]; got != float64(0) {
		t.Errorf("国际账号全暂停：global servable = %v, want 0", got)
	}
	if got := ms["cn:m1"]["total"]; got != float64(1) {
		t.Errorf("cn 未受影响：total = %v, want 1", got)
	}

	// ② 只暂停一个国际账号 → 1/1（总数与可服务都排除暂停的号）
	p2 := newPoolWith()
	if !p2.Pause("g1") {
		t.Fatal("Pause 失败")
	}
	ms = get(t, p2)
	if got := ms["global:m1"]["total"]; got != float64(1) {
		t.Errorf("部分暂停：global total = %v, want 1（3 号里 1 个暂停 → 可路由 1 个）", got)
	}
	if got := ms["global:m1"]["servable"]; got != float64(1) {
		t.Errorf("部分暂停：global servable = %v, want 1", got)
	}

	// ③ 禁用账号与暂停同口径：不计入 total
	p3 := newPoolWith()
	p3.Disable("g1", "test")
	if got := get(t, p3)["global:m1"]["total"]; got != float64(1) {
		t.Errorf("禁用 1 个：global total = %v, want 1", got)
	}
}

// TestModelGroupsSniffEndpoint 面板「立即嗅探全部」接口。
//
// 关键约束：接口只是把 Sniffer.RunOnceNow 调一次，不自己重写「哪些成员该探」的判据
// —— 可见性过滤、锁因过滤、剩余冷却下限、探活频率上限都已在 RunOnce 里。
// 所以这里注入一个假回调，验证面板层确实把结果原样回报，不加工。
func TestModelGroupsSniffEndpoint(t *testing.T) {
	var gotCtx bool
	p := New(Config{
		APIKey:   "",
		GroupSet: func() *modelgroup.GroupSet { return nil },
		SniffModelGroups: func(ctx context.Context) (int, int, error) {
			gotCtx = ctx != nil
			return 3, 1, nil
		},
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/model_groups/sniff", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if !gotCtx {
		t.Error("回调未收到 ctx（无法超时/取消）")
	}
	var resp struct {
		OK        bool `json:"ok"`
		Probed    int  `json:"probed"`
		Recovered int  `json:"recovered"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析: %v（body=%s）", err, rec.Body)
	}
	if !resp.OK || resp.Probed != 3 || resp.Recovered != 1 {
		t.Errorf("回报 = %+v, want ok=true probed=3 recovered=1（不得加工真实结果）", resp)
	}

	// 未接线 → 501（与其它能力未装配同口径）
	rec = httptest.NewRecorder()
	New(Config{APIKey: ""}).ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/model_groups/sniff", nil))
	if rec.Code != 501 {
		t.Errorf("未接线时应 501，实际 %d", rec.Code)
	}
}

// TestModelGroupsMemberCanSniff 成员项必须透出 kind / can_sniff。
//
// 前端据此决定「嗅探」按钮是否可用：只有 model_rate 值得真探活，
// model_missing / hard_credit 探了也必然失败，等 TTL 即可。
// 若前端自己猜锁因，就会与后端 RunOnce 的过滤条件漂移。
func TestModelGroupsMemberCanSniff(t *testing.T) {
	cache := &modelgroup.Cache{}
	now := time.Now()
	// 一个 model_rate 锁定成员（可嗅探）
	cache.MarkLocked("cn", "m1", modelgroup.State{
		Kind: modelgroup.KindModelRate, Reason: "6004", Until: now.Add(time.Hour),
	}, now)
	// 一个 model_missing 成员（不可嗅探）
	cache.MarkLocked("cn", "m2", modelgroup.State{
		Kind: modelgroup.KindModelMissing, Reason: "11102", Until: now.Add(time.Hour),
	}, now)

	p, _ := testGroupPanel(t, []modelgroup.GroupConfig{{
		ID: "g", Name: "组", Enabled: true,
		Members: []modelgroup.MemberConfig{
			{Model: "cn:m1", Weight: 100},
			{Model: "cn:m2", Weight: 90},
		},
	}}, cache)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/model_groups", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Groups []struct {
			Members []struct {
				Model    string `json:"model"`
				Kind     string `json:"kind"`
				CanSniff bool   `json:"can_sniff"`
			} `json:"members"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析: %v（body=%s）", err, rec.Body)
	}
	ms := resp.Groups[0].Members
	if ms[0].Kind != "model_rate" || !ms[0].CanSniff {
		t.Errorf("m1 应 kind=model_rate can_sniff=true，实际 %+v", ms[0])
	}
	if ms[1].Kind != "model_missing" || ms[1].CanSniff {
		t.Errorf("m2 应 kind=model_missing can_sniff=false（探了也必然失败），实际 %+v", ms[1])
	}
}

// TestModelGroupsGetEmptyConfigReturns200 ⭐ 回归：空配置必须能进页面。
//
// 真实缺陷（用户实测发现）：compileModelGroups 在没有任何组时返回 nil，
// 而 modelGroupsGet 把 nil 当成「能力未装配」返回 501 —— 于是全新部署的用户
// 打开「模型组」页拿到 501，永远无法新建第一个组。空配置是合法初始状态，
// 只有 GroupSet 回调本身没注入时才该 501。
func TestModelGroupsGetEmptyConfigReturns200(t *testing.T) {
	var saved []modelgroup.GroupConfig
	p := New(Config{
		APIKey:   "",
		GroupSet: func() *modelgroup.GroupSet { return nil }, // 已接线，但编译后无组
		SaveModelGroups: func(g []modelgroup.GroupConfig, _ modelgroup.Options) error {
			saved = g
			return nil
		},
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/model_groups", nil))
	if rec.Code != 200 {
		t.Fatalf("空配置应 200（可新建第一个组），实际 %d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		OK      bool             `json:"ok"`
		Groups  []map[string]any `json:"groups"`
		Options map[string]any   `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析: %v（body=%s）", err, rec.Body)
	}
	if !resp.OK {
		t.Error("ok 应为 true")
	}
	if len(resp.Groups) != 0 {
		t.Errorf("组数 = %d, want 0", len(resp.Groups))
	}
	// options 必须给出缺省值，前端据此渲染嗅探参数文案
	if resp.Options == nil || resp.Options["sniff_interval"] == "" {
		t.Errorf("options 应含缺省 sniff_interval，实际 %+v", resp.Options)
	}
	// ⭐ 两个开关的设计默认都是 true。裸零值会让它们显示成关闭，
	// 用户一旦保存就把 false 写进配置，从此回不到默认。
	if v, ok := resp.Options["prefer_last_ok"].(bool); !ok || !v {
		t.Errorf("空配置下 prefer_last_ok 应为 true（设计默认），实际 %+v", resp.Options["prefer_last_ok"])
	}
	if v, ok := resp.Options["sniff_enabled"].(bool); !ok || !v {
		t.Errorf("空配置下 sniff_enabled 应为 true（设计默认），实际 %+v", resp.Options["sniff_enabled"])
	}

	// 空配置下也必须能保存第一个组
	body := `{"groups":[{"id":"first","name":"第一个组","enabled":true,"members":[{"model":"cn:m1","weight":100,"enabled":true}]}],"options":{"prefer_last_ok":true,"sniff_enabled":false,"sniff_interval":"60s","sniff_min_until":"5m","sniff_min_gap":"10m"}}`
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/model_groups", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("空配置下保存第一个组应 200，实际 %d body=%s", rec.Code, rec.Body)
	}
	if len(saved) != 1 || saved[0].ID != "first" {
		t.Errorf("保存内容 = %+v, want 1 个 id=first 的组", saved)
	}
}

// TestModelGroupsNilDepsReturns501 未装配时返回 501（与 usage/balance 同口径）。
func TestModelGroupsNilDepsReturns501(t *testing.T) {
	p := New(Config{APIKey: ""})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/model_groups", nil))
	if rec.Code != 501 {
		t.Errorf("未装配时应 501，实际 %d", rec.Code)
	}
}
