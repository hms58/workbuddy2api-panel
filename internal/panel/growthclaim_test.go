package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// taskWithValidEnd 造一个已达标的任务；raw 非空时填入上游原样下发的 valid_end。
func taskWithValidEnd(raw string) *upstream.Task {
	return &upstream.Task{
		TaskCode: "chat_5", Target: 1, Current: 1, Claimable: true, ValidEnd: raw,
	}
}

// TestAutoClaimAllowed 自动领奖闸门：auto 恒放行；manual 只在活动 24 小时内截止时放行，
// 截止时间取不到一律保留人工领取（不猜、不误领）。
func TestAutoClaimAllowed(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, cst.Loc)
	within := now.Add(3 * time.Hour).In(cst.Loc).Format("2006-01-02 15:04:05")
	exact := now.Add(claimExpiringWindow).In(cst.Loc).Format("2006-01-02 15:04:05")
	beyond := now.Add(48 * time.Hour).In(cst.Loc).Format("2006-01-02 15:04:05")
	expired := now.Add(-time.Hour).In(cst.Loc).Format("2006-01-02 15:04:05")

	cases := []struct {
		name string
		mode string
		end  string
		want bool
	}{
		{"auto 模式无截止字段也领", "auto", "", true},
		{"auto 模式远期活动也领", "auto", beyond, true},
		{"manual 模式无截止字段不领", "manual", "", false},
		{"manual 模式剩余 3 小时自动领", "manual", within, true},
		{"manual 模式恰好 24 小时自动领", "manual", exact, true},
		{"manual 模式剩余 2 天不领", "manual", beyond, false},
		{"manual 模式已过期不领", "manual", expired, false},
		{"未设置模式回落 manual", "", within, true},
		{"未知模式值回落 manual", "whatever", beyond, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(Config{})
			if c.mode != "" {
				p.SetGrowthClaimMode(c.mode)
			}
			got, reason := p.autoClaimAllowedAt(taskWithValidEnd(c.end), now)
			if got != c.want {
				t.Fatalf("autoClaimAllowed=%v want %v（reason=%q）", got, c.want, reason)
			}
			if !got && reason == "" {
				t.Fatal("拒绝自动领取必须给出原因，否则面板无从解释")
			}
		})
	}
}

// TestGrowthClaimModeDefaultIsManual 缺省即手动领取：默认 auto 会让本次优化对所有人无效。
func TestGrowthClaimModeDefaultIsManual(t *testing.T) {
	if got := New(Config{}).growthClaimModeValue(); got != claimModeManual {
		t.Fatalf("默认模式=%q want %q", got, claimModeManual)
	}
	// 非法/空值一律回落 manual，不允许静默变成 auto。
	for _, raw := range []string{"", "whatever", "AUTO ", "1"} {
		p := New(Config{})
		p.SetGrowthClaimMode(raw)
		if got := p.growthClaimModeValue(); got != claimModeManual {
			t.Fatalf("SetGrowthClaimMode(%q) → %q，want manual", raw, got)
		}
	}
	p := New(Config{})
	p.SetGrowthClaimMode("auto")
	if got := p.growthClaimModeValue(); got != claimModeAuto {
		t.Fatalf("auto 未被接受，got %q", got)
	}
}

// TestClaimAutoTaskKeepsRewardWhenManual 手动模式下「已达标未领」既不是完成也不是领奖失败：
// 状态必须能与 claim_pending（领奖失败待重试）区分开，且不发领奖请求。
func TestClaimAutoTaskKeepsRewardWhenManual(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 5, 5, "completed")
	p.SetGrowthClaimMode(claimModeManual)
	act := &autoAction{TaskCode: "chat_5"}
	before := &upstream.Task{TaskCode: "chat_5", Target: 5, Current: 5, Claimable: true}

	out := p.claimAutoTask(a, act, "任务已达标", before, before)
	if out.Status != "claim_manual" {
		t.Fatalf("status=%q want claim_manual（%s）", out.Status, out.Message)
	}
	if out.Claimed {
		t.Fatal("手动模式不得标记为已领取")
	}
	if out.ClaimError != "" {
		t.Fatalf("claim_error 非空会被前端当成失败: %q", out.ClaimError)
	}
	_, claims, _ := state.counters()
	if claims != 0 {
		t.Fatalf("手动模式发起了 %d 次领奖请求", claims)
	}
}

// TestClaimAutoTaskFirstBuddyIgnoresManualMode first_buddy 的奖励在 buddy/first 动作链里
// 到账，没有独立 claim 步骤：被闸门拦住会报「待领取」却永远领不到。
func TestClaimAutoTaskFirstBuddyIgnoresManualMode(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 1, 1, "completed")
	p.SetGrowthClaimMode(claimModeManual)
	state.taskCode = "first_buddy"
	act := &autoAction{TaskCode: "first_buddy"}
	before := &upstream.Task{TaskCode: "first_buddy", Target: 1, Current: 1, Claimable: true}

	out := p.claimAutoTask(a, act, "已领取 Buddy", before, before)
	if out.Status != "done" {
		t.Fatalf("status=%q want done（first_buddy 不参与手动领取）", out.Status)
	}
	_, claims, _ := state.counters()
	if claims != 1 {
		t.Fatalf("claims=%d want 1", claims)
	}
}

// TestClaimAutoTaskAutoModeStillClaims 回归：auto 模式下行为与改造前一致（达标即领）。
func TestClaimAutoTaskAutoModeStillClaims(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 5, 5, "completed")
	p.SetGrowthClaimMode(claimModeAuto)
	act := &autoAction{TaskCode: "chat_5"}
	before := &upstream.Task{TaskCode: "chat_5", Target: 5, Current: 5, Claimable: true}

	out := p.claimAutoTask(a, act, "任务已达标", before, before)
	if out.Status != "done" || !out.Claimed || out.Credit != 100 {
		t.Fatalf("outcome=%#v want done/claimed/+100", out)
	}
	if _, claims, _ := state.counters(); claims != 1 {
		t.Fatalf("claims=%d want 1", claims)
	}
}

// TestMPRunnerNoLongerClaimsInline mp 任务 runner 只负责完成动作：领奖由
// executeAutoTask 统一走闸门，runner 自己再领一次会在手动模式下绕过闸门。
func TestMPRunnerNoLongerClaimsInline(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 5, 5, "completed")
	p.SetGrowthClaimMode(claimModeManual)

	msg, err := p.runMPMiniChatTask(a, "chat_5")
	if err != nil {
		t.Fatalf("runner 报错: %v", err)
	}
	if !strings.Contains(msg, "达标") {
		t.Fatalf("runner 消息=%q，应说明进度已达标", msg)
	}
	_, claims, _ := state.counters()
	if claims != 0 {
		t.Fatalf("runner 自己发起了 %d 次领奖（应交给 executeAutoTask）", claims)
	}
}

// claimAllFake 全部领取的假上游：默认口径与 mp 口径各下发一份任务，记录领奖分流。
type claimAllFake struct {
	mu     sync.Mutex
	tasks  []map[string]any
	claims []string // "<code>|<x-client-platform>"
}

func (f *claimAllFake) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			f.mu.Lock()
			body, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"tasks": f.tasks}})
			f.mu.Unlock()
			_, _ = w.Write(body)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") &&
			strings.HasSuffix(r.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			f.mu.Lock()
			f.claims = append(f.claims, code+"|"+r.Header.Get("x-client-platform"))
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
		default:
			t.Errorf("未预期的上游请求: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func claimAllPanel(t *testing.T, f *claimAllFake, a *auth.Auth) *Panel {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	p := pool.New("")
	p.Add(a)
	return New(Config{Pool: p, Upstream: up})
}

// TestClaimAllOnlyClaimable 全部领取只领「达标 + 未领 + 未锁定」，mp 码走 mp 头；
// 手动领取不过闸门（人工操作永远允许）。
func TestClaimAllOnlyClaimable(t *testing.T) {
	f := &claimAllFake{tasks: []map[string]any{
		{"task_code": "chat_5", "target": 5, "current": 5, "accept_status": "completed"},
		{"task_code": "Library_read", "target": 1, "current": 1},
		{"task_code": "template_5", "target": 5, "current": 2},
		{"task_code": "Hp_Appearance", "target": 1, "current": 1, "locked": true},
		{"task_code": "skill_1", "target": 1, "current": 1, "accept_status": "claimed"},
		{"task_code": "Sequential_Tasks_1", "target": 1, "current": 1},
	}}
	p := claimAllPanel(t, f, &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"})
	p.SetGrowthClaimMode(claimModeManual)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/cn-1/tasks/claim_all", nil)
	req.SetPathValue("uid", "cn-1")
	p.accountTaskClaimAll(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool  `json:"ok"`
		Claimed int   `json:"claimed"`
		Credit  int64 `json:"credit"`
		Energy  int64 `json:"energy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Claimed != 3 {
		t.Fatalf("resp=%+v want ok 且 claimed=3（chat_5 / Library_read / Sequential_Tasks_1）", resp)
	}
	if resp.Credit != 300 || resp.Energy != 15 {
		t.Fatalf("credit/energy=%d/%d want 300/15", resp.Credit, resp.Energy)
	}
	f.mu.Lock()
	got := strings.Join(f.claims, ",")
	f.mu.Unlock()
	if got != "chat_5|web,Library_read|web,Sequential_Tasks_1|miniprogram" {
		t.Fatalf("claim 分流=%q", got)
	}
}

// TestClaimAllRespectsAccountLock 该账号有任务动作在跑时直接 409，不并发领奖。
func TestClaimAllRespectsAccountLock(t *testing.T) {
	f := &claimAllFake{tasks: []map[string]any{
		{"task_code": "chat_5", "target": 5, "current": 5},
	}}
	p := claimAllPanel(t, f, &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"})
	if !p.tryLockAccount("cn-1") {
		t.Fatal("测试前置：应能拿到账号锁")
	}
	defer p.unlockAccount("cn-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/cn-1/tasks/claim_all", nil)
	req.SetPathValue("uid", "cn-1")
	p.accountTaskClaimAll(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409；body=%s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.claims) != 0 {
		t.Fatalf("持锁期间不得领奖，claims=%v", f.claims)
	}
}
