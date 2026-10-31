// chain.go 组集合与「本请求会尝试的成员顺序」。
//
// 可见性过滤必须发生在排序之前：lastOK 与权重序都依赖候选集，若在选号循环里
// 才逐个跳过，lastOK 可能指向一个已停用成员，prefer_last_ok 就会把它提到首位，
// 导致每次请求都先撞一个不可能成功的成员。
package modelgroup

import (
	"sync"
	"time"
)

// GroupSet 一份不可变的组集合 + 可变的 lastOK 表。
// 组配置热更时整体替换 GroupSet（新对象），读方拿到的一致视图天然安全。
type GroupSet struct {
	groups []Group
	opts   Options

	mu     sync.RWMutex
	lastOK map[string]string // groupID → 上次成功的成员 Model
}

// NewGroupSet 以编译产物构建。groups 只读共享，调用方不得再修改。
func NewGroupSet(groups []Group, opts Options) *GroupSet {
	return &GroupSet{groups: groups, opts: opts, lastOK: make(map[string]string)}
}

// Options 返回当前参数。
func (s *GroupSet) Options() Options { return s.opts }

// Lookup 按 id 查组（id 已小写）。
func (s *GroupSet) Lookup(id string) (*Group, bool) {
	for i := range s.groups {
		if s.groups[i].ID == id {
			return &s.groups[i], true
		}
	}
	return nil, false
}

// All 返回全部组（面板渲染用）。
func (s *GroupSet) All() []Group { return s.groups }

// Plan 返回本请求的成员尝试顺序：可见集 → 权重序 → lastOK 提到**本权重层首位**。
//
// lastOK 只在它当前可见时才被提升；不可见时整条忽略（回落纯权重序）。
//
// ⚠️ 提升边界是「同权重层」，不是整条计划的第 0 位：权重是用户显式配的优先级，
// 高优成员恢复可用后必须重新压过上次成功的低优成员。历史实现把 lastOK 直接挪到
// 首位，导致「高优模型恢复后仍一直用低优模型」（用户实测反馈，issue：上次可用成员
// 优先压过优先级）。同权重成员之间仍由 lastOK 决定先后 —— 那正是本开关的作用域。
func (s *GroupSet) Plan(id string, at time.Time) []Member {
	g, ok := s.Lookup(id)
	if !ok || !g.Enabled {
		return nil
	}
	vis := g.Visible(at)
	if len(vis) == 0 {
		return nil
	}
	if !s.opts.PreferLastOK {
		return vis
	}
	s.mu.RLock()
	last := s.lastOK[g.ID]
	s.mu.RUnlock()
	if last == "" {
		return vis
	}
	for i := range vis {
		if vis[i].Model != last {
			continue
		}
		// 回溯到本权重层的层首（vis 已按 weight 降序稳定排序，同层必然连续）。
		j := i
		for j > 0 && vis[j-1].Weight == vis[i].Weight {
			j--
		}
		if j == i {
			return vis // 已在层首，无需搬动
		}
		hit := vis[i]
		copy(vis[j+1:i+1], vis[j:i])
		vis[j] = hit
		return vis
	}
	// lastOK 指向的成员已不可见 → 忽略它（不清表：它可能只是暂时在时段外）。
	return vis
}

// NoteOK 记录某组上次成功使用的成员。
func (s *GroupSet) NoteOK(groupID, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastOK[groupID] = model
}

// EnabledMembers 返回所有启用组里当前可见的成员（按 (域, 裸名) 去重）。
// 嗅探候选集：停用成员与时段外成员都不在里面。
func (s *GroupSet) EnabledMembers(at time.Time) []Member {
	seen := make(map[string]struct{})
	out := make([]Member, 0, len(s.groups))
	for i := range s.groups {
		if !s.groups[i].Enabled {
			continue
		}
		for _, m := range s.groups[i].Visible(at) {
			k := m.Realm + "\x1f" + m.Bare
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}

// Referenced 报告 (域, 裸名) 是否仍被某个启用组引用（含当前不可见的成员）。
// 缓存摘除用：不可见 ≠ 不再需要，白天时段外的成员晚上还要用。
func (s *GroupSet) Referenced(realm, bare string) bool {
	for i := range s.groups {
		if !s.groups[i].Enabled {
			continue
		}
		for _, m := range s.groups[i].Members {
			if m.Realm == realm && m.Bare == bare {
				return true
			}
		}
	}
	return false
}
