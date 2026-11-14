// servable.go 按 (域, 模型) 聚合「还有几个号能服务」。
//
// 与 ModelBlockedForRealm 的分工：那个只回答「是不是全池都被**模型级**冷却挡住」；
// 本方法同时统计账号级不可用（402 余额耗尽/冷却/在途占满/积分保底），
// 因为额度按账户独立，「同一模型额度用完」既可能是 N 个号各自 6004，
// 也可能是整域号都 402 —— 两者都要能判出来。
package pool

import "time"

// ServableInfo (域, 模型) 的可服务性聚合。
type ServableInfo struct {
	// Count 此刻能服务该模型的账号数（selectable：健康 + 在途未满 + 未被保底拦）。
	Count int
	// LockedAccts 因该模型自身冷却被挡的账号数（诊断用，面板显示「3 个号都被这个模型锁」）。
	LockedAccts int
	// Total 参与选号的账号数（禁用/暂停不计）。
	Total int
	// EarliestUnlock 该域最早的解封时刻（零值 = 没有已知墙钟）。
	EarliestUnlock time.Time
	// Reason 取最早解封那条的上游原文。
	Reason string
}

// ServableFor 聚合 (realm, model) 的可服务性。只读，持 RLock。
//
// 口径与 selectable 严格一致：禁用/暂停号跳过（它们的不可用与模型无关），
// 在途占满与积分保底也计入不可服务 —— 否则「号在但都占满」会被误报成还能服务。
//
// 与 ModelLockView 的唯一口径差异：本方法计入 reserveBlocked（积分保底）。
// 展示层不需要这个维度，路由必须按不可用处理 —— 这是有意为之，不要合并。
func (p *Pool) ServableFor(realm, model string) ServableInfo {
	if model == "" {
		return ServableInfo{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()

	var info ServableInfo
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if e.disabled || e.paused {
			continue
		}
		info.Total++
		if e.modelCooled(now, model) {
			info.LockedAccts++
			if mc, ok := e.modelCooldowns[model]; ok && !mc.AuditOnly && !mc.Until.IsZero() {
				if info.EarliestUnlock.IsZero() || mc.Until.Before(info.EarliestUnlock) {
					info.EarliestUnlock = mc.Until
					info.Reason = mc.Reason
				}
			}
		}
		if p.selectable(e, now, model) {
			info.Count++
		}
	}
	if info.Reason == "" && info.LockedAccts > 0 {
		// 全都没有解封时刻（如 14003 无墙钟的有界退避）：退回第一条可读原因。
		for _, e := range p.byUID {
			if realm != "" && e.a.Realm() != realm {
				continue
			}
			if mc, ok := e.modelCooldowns[model]; ok && !mc.AuditOnly && mc.Reason != "" {
				info.Reason = mc.Reason
				break
			}
		}
	}
	return info
}
