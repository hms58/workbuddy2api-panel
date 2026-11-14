// model_group.go 模型组在网关侧的路由辅助。
//
// 职责边界：本文件只负责「把 group:<id> 变成一个有序的成员计划」与「写缓存」，
// 不碰选号（那是 pool 的事）、不碰错误分类（那是 applyErrorPolicy 的事）。
package server

import (
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/modelgroup"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// groupPrefix 模型组前缀。与 cn:/global: 并列，大小写敏感（同 SplitRealmPrefix）。
const groupPrefix = "group:"

// splitGroupPrefix 解析 "group:<id>"。id 归一化为小写。
// 空 id / 缺前缀 → ok=false（调用方按普通模型名处理，零回归）。
func splitGroupPrefix(model string) (string, bool) {
	if !strings.HasPrefix(model, groupPrefix) {
		return "", false
	}
	id := strings.ToLower(strings.TrimSpace(model[len(groupPrefix):]))
	if id == "" {
		return "", false
	}
	return id, true
}

// groupSet 返回当前生效的组集合（nil 安全：未装配时返回 nil）。
func (h *Handler) groupSet() *modelgroup.GroupSet {
	if h.cfg.Groups == nil {
		return nil
	}
	return h.cfg.Groups()
}

// SetGroups 注入组集合读取函数（main 装配期调用，运行期只读）。
func (h *Handler) SetGroups(fn func() *modelgroup.GroupSet) { h.cfg.Groups = fn }

// SetCache 注入可用性缓存（main 装配期调用）。
func (h *Handler) SetCache(c *modelgroup.Cache) { h.cfg.Cache = c }

// groupPlan 返回某组的成员尝试顺序。
// 返回 ok=false 表示「无此组 / 组已停用 / 当前无可见成员」——
// 三种情形都应对客户端返回 400 model_unavailable，而不是回落成裸模型名。
func (h *Handler) groupPlan(id string, at time.Time) ([]modelgroup.Member, bool) {
	gs := h.groupSet()
	if gs == nil {
		return nil, false
	}
	plan := gs.Plan(id, at)
	if len(plan) == 0 {
		return nil, false
	}
	return plan, true
}

// chainState 一次组请求的推进状态。
//
// triedMembers 只记「已试过的成员」，单向推进，杜绝 A→B→A 死循环。
// 注意 tried（已试账号集）不在这里：它属于「当前成员内」的轮换状态，
// 由 handler 在成员推进时清空 —— 账号在 m1 上失败不代表它在 m2 上也不行。
type chainState struct {
	plan         []modelgroup.Member
	idx          int
	groupID      string
	triedMembers map[string]bool
}

// newChainState 以计划构建。plan 必须非空（调用方先校验）。
func newChainState(groupID string, plan []modelgroup.Member) *chainState {
	return &chainState{plan: plan, groupID: groupID, triedMembers: make(map[string]bool)}
}

// current 返回当前成员。全部试完 → ok=false。
func (c *chainState) current() (modelgroup.Member, bool) {
	if c == nil || c.idx >= len(c.plan) {
		return modelgroup.Member{}, false
	}
	return c.plan[c.idx], true
}

// advance 推进到下一个未试过、且此刻仍可见的成员。
//
// 跳过两类（都是「此刻不在组里」，不产生任何副作用）：
//   - 该成员已被本请求试过（防环）
//   - 该成员此刻不可见（停用/时段外：plan 构造后到现在可能已跨时段边界）
func (c *chainState) advance(at time.Time) bool {
	if c == nil {
		return false
	}
	for c.idx+1 < len(c.plan) {
		c.idx++
		m := c.plan[c.idx]
		if c.triedMembers[m.Model] {
			continue
		}
		if !m.IsVisible(at) {
			continue
		}
		return true
	}
	return false
}

// markCurrentTried 记下当前成员已试过。
func (c *chainState) markCurrentTried() {
	if c == nil || c.idx >= len(c.plan) {
		return
	}
	c.triedMembers[c.plan[c.idx].Model] = true
}

// chainGroupID 取组 id（非组请求返回空串）。
func chainGroupID(c *chainState) string {
	if c == nil {
		return ""
	}
	return c.groupID
}

// markMemberLocked 把「该域全部账号都服务不了这个模型」写进缓存。
//
// 只在 Pick 返回 nil（或 ServableFor 的 Count == 0）时调用 —— 单账号失败绝不能
// 落成员锁，否则高优成员剩下的额度会被白白闲置。
func (h *Handler) markMemberLocked(groupID, realm, bare string, info pool.ServableInfo, now time.Time) {
	if h.cfg.Cache == nil {
		return
	}
	kind := modelgroup.KindUnknown
	switch {
	case info.LockedAccts > 0 && info.Reason != "" && upstream.IsModelRateLimit(info.Reason):
		kind = modelgroup.KindModelRate
	case info.LockedAccts > 0:
		kind = modelgroup.KindModelMissing
	case info.Total > 0:
		kind = modelgroup.KindHardCredit
	default:
		kind = modelgroup.KindNoAccount
	}
	h.cfg.Cache.MarkLocked(realm, bare, modelgroup.State{
		Kind:        kind,
		Reason:      info.Reason,
		Until:       info.EarliestUnlock,
		Servable:    info.Count,
		LockedAccts: info.LockedAccts,
	}, now)
}

// noteMemberOK 成功服务后更新缓存与 lastOK。
// model 用 "realm:bare" 形态，与 Plan 返回的 Member.Model 同口径。
func (h *Handler) noteMemberOK(groupID, realm, bare string) {
	now := time.Now()
	if h.cfg.Cache != nil {
		h.cfg.Cache.MarkOK(realm, bare, now)
	}
	if groupID == "" {
		return
	}
	if gs := h.groupSet(); gs != nil {
		gs.NoteOK(groupID, realm+":"+bare)
	}
}
