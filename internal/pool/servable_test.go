package pool

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

func servableTestPool(auths ...*auth.Auth) *Pool {
	p := New("")
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	return p
}

// cnAuth 国内域账号。域名必须用 codebuddy.cn：workbuddy.ai / codebuddy.ai 的
// 任意子域都被 auth.isGlobalDomain 判为 global（见 auth.go:312），用错会把
// 「cn 域」测试整体错域。
func cnAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, Domain: "www.codebuddy.cn", AccessToken: "at-" + uid, ExpiresAt: 9999999999}
}

func globalAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, Domain: "www.workbuddy.ai", AccessToken: "at-" + uid, ExpiresAt: 9999999999}
}

// TestServableForCountsModelLevelLocks ⭐ Review Focus 第 2 条的另一半：
// 额度按账户独立，「同一模型额度用完」表现为多个账号各自被这个模型锁住。
// Count 必须是「还能服务该模型的账号数」，LockedAccts 是被该模型锁住的数量。
func TestServableForCountsModelLevelLocks(t *testing.T) {
	p := servableTestPool(cnAuth("a"), cnAuth("b"), cnAuth("c"))
	p.CooldownSoftForModel("a", 0, time.Now().Add(time.Hour), "glm", "6004 test")
	p.CooldownSoftForModel("b", 0, time.Now().Add(2*time.Hour), "glm", "6004 test")

	got := p.ServableFor("cn", "glm")
	if got.Total != 3 {
		t.Errorf("Total = %d, want 3", got.Total)
	}
	if got.Count != 1 {
		t.Errorf("Count = %d, want 1（只剩 c 能服务）", got.Count)
	}
	if got.LockedAccts != 2 {
		t.Errorf("LockedAccts = %d, want 2", got.LockedAccts)
	}
	if got.EarliestUnlock.IsZero() {
		t.Errorf("EarliestUnlock 不应为零值（两个号分别锁 1h/2h）")
	}
}

// TestServableForCountsAccountLevelHardCredit 402 余额耗尽锁的是**全部模型**：
// 该号对任何模型都不可用，Count 里不能算它。
func TestServableForCountsAccountLevelHardCredit(t *testing.T) {
	p := servableTestPool(cnAuth("a"), cnAuth("b"))
	p.CooldownUntilTomorrow4AM("a", "余额不足")

	got := p.ServableFor("cn", "glm")
	if got.Count != 1 {
		t.Errorf("Count = %d, want 1（a 整体余额耗尽）", got.Count)
	}
	// LockedAccts 的定义是「因**该模型自身冷却**被挡的账号数」。402 是账号级硬冷却
	// （写 entry.until，不写 modelCooldowns），所以不计入 LockedAccts —— 它对所有模型
	// 都不可用，这个维度与模型无关。诊断「全号余额耗尽」要看 Count==0 && LockedAccts==0。
	if got.LockedAccts != 0 {
		t.Errorf("LockedAccts = %d, want 0（402 是账号级冷却，不是模型级）", got.LockedAccts)
	}
	if got.Total != 2 {
		t.Errorf("Total = %d, want 2", got.Total)
	}
}

// TestServableForRealmIsolation 同名模型在两域各自计数。
func TestServableForRealmIsolation(t *testing.T) {
	p := servableTestPool(cnAuth("cn1"), cnAuth("cn2"), globalAuth("g1"))
	p.CooldownSoftForModel("cn1", 0, time.Now().Add(time.Hour), "glm", "6004")
	p.CooldownSoftForModel("cn2", 0, time.Now().Add(time.Hour), "glm", "6004")

	cn := p.ServableFor("cn", "glm")
	if cn.Count != 0 || cn.LockedAccts != 2 {
		t.Errorf("cn = {Count:%d Locked:%d}, want {0, 2}", cn.Count, cn.LockedAccts)
	}
	gl := p.ServableFor("global", "glm")
	if gl.Count != 1 {
		t.Errorf("global Count = %d, want 1（global 号不受 cn 锁影响）", gl.Count)
	}
}

// TestServableForExcludesDisabled 禁用/暂停号不计入 Total（与 ModelBlocked 同口径）。
func TestServableForExcludesDisabled(t *testing.T) {
	p := servableTestPool(cnAuth("a"), cnAuth("b"))
	p.Disable("b", "test")
	got := p.ServableFor("cn", "glm")
	if got.Total != 1 {
		t.Errorf("Total = %d, want 1（禁用号不计）", got.Total)
	}
	if got.Count != 1 {
		t.Errorf("Count = %d, want 1", got.Count)
	}
}

// TestServableForEmptyModelReturnsZero model 为空时不做判定（无模型上下文）。
func TestServableForEmptyModelReturnsZero(t *testing.T) {
	p := servableTestPool(cnAuth("a"))
	if got := p.ServableFor("cn", ""); got.Count != 0 || got.Total != 0 {
		t.Errorf("空模型应返回零值，实际 %+v", got)
	}
}

// TestServableForModelCooledButAccountHealthy 该模型被锁、但号本身健康：
// LockedAccts 记 1，Count 记 0 —— 这是「该模型额度用完」的典型形态。
func TestServableForModelCooledButAccountHealthy(t *testing.T) {
	p := servableTestPool(cnAuth("a"))
	p.CooldownSoftForModel("a", 0, time.Now().Add(time.Hour), "glm", "6004")
	got := p.ServableFor("cn", "glm")
	if got.Count != 0 {
		t.Errorf("Count = %d, want 0", got.Count)
	}
	if got.LockedAccts != 1 {
		t.Errorf("LockedAccts = %d, want 1", got.LockedAccts)
	}
	if got.Reason == "" {
		t.Errorf("Reason 应带上游原文")
	}
	// 同一账号对另一个模型仍然可服务（模型级锁只锁该模型）
	if other := p.ServableFor("cn", "other-model"); other.Count != 1 {
		t.Errorf("同号其它模型 Count = %d, want 1（模型级冷却只锁该模型）", other.Count)
	}
}

// TestServableForCountsReserveBlocked ⭐ Review Focus 第 6 条 / §11 的核心依据：
// ServableFor 比 ModelLockView 多算「积分保底」——被保底拦住的号确实不能服务该模型，
// 路由必须按不可用处理。本测试钉住这个**有意为之的口径差异**，防止后续有人"顺手合并"。
func TestServableForCountsReserveBlocked(t *testing.T) {
	p := servableTestPool(cnAuth("a"))
	p.SetReserveCredits(500) // 保底 500
	// 把 a 的余额压到保底线下。total 必须 > 0：reserveBlocked 只在
	// 「已知余额」（creditsTotal > 0）时生效，0 表示「从未查过」不受影响。
	p.SetCredits("a", 100, 1000)

	// 付费模型：被保底拦 → ServableFor 的 Count 必须是 0
	got := p.ServableFor("cn", "glm")
	if got.Count != 0 {
		t.Errorf("Count = %d, want 0（a 的余额 100 < 保底 500，不能服务付费模型）", got.Count)
	}
	if got.Total != 1 {
		t.Errorf("Total = %d, want 1（保底号仍参与选号，只是不出票）", got.Total)
	}

	// 同一时刻 ModelLockView 不认保底 → 它不给这个模型出行（没有模型级冷却）
	// 这正是两套口径的差异点：路由说「没号」，展示说「没被锁」。
	if rows := p.ModelLockView(); len(rows) != 0 {
		t.Errorf("ModelLockView 应无行（没有模型级冷却），实际 %+v", rows)
	}
}
