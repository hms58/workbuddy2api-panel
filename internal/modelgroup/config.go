// config.go 组配置的归一化：用户写的 JSON 形态 → 运行时只读的 Group/Member。
//
// 归一化在启动与每次面板保存时各跑一次，请求路径只消费编译产物：
// 成员已排序、realm/bare 已解析、window 已转成分钟数。热路径不做任何字符串处理。
package modelgroup

import (
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// cstLoc 暴露 CST 时区（测试构造时刻用）。
func cstLoc() *time.Location { return cst.Loc }

// MemberConfig 配置文件的成员形态（`pool.model_groups[].members[]`）。
type MemberConfig struct {
	Model   string `json:"model"`
	Weight  int    `json:"weight"`
	Enabled *bool  `json:"enabled,omitempty"` // nil = 未配置 = true
	Window  string `json:"window,omitempty"`  // "" = 全天
}

// GroupConfig 配置文件的组形态。
type GroupConfig struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Enabled bool           `json:"enabled"`
	Members []MemberConfig `json:"members"`
}

// Member 编译后的成员。Realm/Bare 已解析，Window 已转分钟数。
type Member struct {
	Model   string  // 原始形态（含前缀），面板与日志展示用
	Realm   string  // cn | global
	Bare    string  // 上游模型裸名
	Weight  int
	Enabled bool
	Window  *Window // nil = 全天
}

// IsVisible 报告成员此刻是否可见：enabled 且在当前生效时段内。
// 停用与时段外都返回 false —— 两者语义都是「这个成员此刻不在组里」。
func (m *Member) IsVisible(at time.Time) bool {
	return m.Enabled && m.Window.In(at)
}

// Group 编译后的组。Members 已按 (weight 降序, 原列表顺序) 稳定排序。
type Group struct {
	ID      string
	Name    string
	Enabled bool
	Members []Member
}

// Visible 返回此刻可见的成员（保持已排序的顺序）。
// 每次请求调用一次：成员数 ≤ 5，纯 bool 判定 + 整数比较，纳秒级。
func (g *Group) Visible(at time.Time) []Member {
	out := make([]Member, 0, len(g.Members))
	for i := range g.Members {
		if g.Members[i].IsVisible(at) {
			out = append(out, g.Members[i])
		}
	}
	return out
}

// Options 全局参数（`pool.model_group`）。
type Options struct {
	PreferLastOK  bool
	SniffEnabled  bool
	SniffInterval time.Duration
	SniffMinUntil time.Duration
	SniffMinGap   time.Duration
}

// WithDefaults 补齐阈值缺省值（见设计文档 §7.2）。
// 两个 bool 开关不在此处理：它们的「开/关」由配置解析层显式给出，
// 这里只保证阈值有值，避免 0 被当成「每 0 秒探活」。
func (o Options) WithDefaults() Options {
	if o.SniffInterval <= 0 {
		o.SniffInterval = 60 * time.Second
	}
	if o.SniffMinUntil <= 0 {
		o.SniffMinUntil = 5 * time.Minute
	}
	if o.SniffMinGap <= 0 {
		o.SniffMinGap = 10 * time.Minute
	}
	return o
}

// Compile 把配置编译成运行时形态。丢弃非法条目不返回错误：
// 一份配置里写错一个成员，不该让整个组不可用。
//
// 规则（顺序即优先级）：
//  1. id Trim + 小写；空 id / 无有效成员的组丢弃；重复 id 后写覆盖先写
//  2. 成员 model Trim；空 model 丢弃；组内重复 (realm, bare) 保留先出现的那条
//  3. weight <= 0 归一化为 1
//  4. enabled 缺省 true
//  5. window 解析；空/非法 → nil（全天）
//  6. 按 (weight 降序, 原列表顺序) 稳定排序
func Compile(groups []GroupConfig, opts Options) ([]Group, Options) {
	out := make([]Group, 0, len(groups))
	index := make(map[string]int, len(groups))
	for _, gc := range groups {
		id := strings.ToLower(strings.TrimSpace(gc.ID))
		if id == "" {
			continue
		}
		members := compileMembers(gc.Members)
		if len(members) == 0 {
			continue
		}
		g := Group{
			ID:      id,
			Name:    strings.TrimSpace(gc.Name),
			Enabled: gc.Enabled,
			Members: members,
		}
		if i, ok := index[id]; ok {
			out[i] = g // 后写覆盖先写
			continue
		}
		index[id] = len(out)
		out = append(out, g)
	}
	return out, opts.WithDefaults()
}

// compileMembers 逐成员归一化 + 去重 + 排序。
func compileMembers(in []MemberConfig) []Member {
	seen := make(map[string]struct{}, len(in))
	out := make([]Member, 0, len(in))
	for _, mc := range in {
		raw := strings.TrimSpace(mc.Model)
		if raw == "" {
			continue
		}
		realm, bare := upstream.SplitRealmPrefix(raw)
		if bare == "" {
			continue
		}
		key := realm + "\x1f" + bare
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		w := mc.Weight
		if w <= 0 {
			w = 1
		}
		enabled := true
		if mc.Enabled != nil {
			enabled = *mc.Enabled
		}
		win, _ := ParseWindow(mc.Window) // 非法 → nil（全天），不阻断保存
		out = append(out, Member{
			Model:   raw,
			Realm:   realm,
			Bare:    bare,
			Weight:  w,
			Enabled: enabled,
			Window:  win,
		})
	}
	sortMembersByWeight(out)
	return out
}

// sortMembersByWeight 按 weight 降序稳定排序。
// 用插入排序而非 sort.SliceStable：成员数 ≤ 5，省掉一次闭包分配与反射开销。
func sortMembersByWeight(ms []Member) {
	for i := 1; i < len(ms); i++ {
		cur := ms[i]
		j := i - 1
		for j >= 0 && ms[j].Weight < cur.Weight {
			ms[j+1] = ms[j]
			j--
		}
		ms[j+1] = cur
	}
}
