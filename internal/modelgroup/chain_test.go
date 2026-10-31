package modelgroup

import (
	"testing"
	"time"
)

func mustCompile(t *testing.T, gcs []GroupConfig) *GroupSet {
	t.Helper()
	groups, opts := Compile(gcs, Options{PreferLastOK: true})
	return NewGroupSet(groups, opts)
}

// TestPlanPrefersLastOK 上次可用成员在本权重层内提到层首，其余保持权重序。
func TestPlanPrefersLastOK(t *testing.T) {
	s := mustCompile(t, []GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:a", Weight: 100},
		{Model: "cn:b", Weight: 90},
		{Model: "cn:c", Weight: 90},
	}}})
	now := time.Now()

	plan := s.Plan("g", now)
	if len(plan) != 3 || plan[0].Model != "cn:a" {
		t.Fatalf("初始顺序应权重序 [cn:a cn:b cn:c]，实际 %+v", plan)
	}

	s.NoteOK("g", "cn:c")
	plan = s.Plan("g", now)
	if len(plan) != 3 || plan[0].Model != "cn:a" {
		t.Fatalf("更高权重的 cn:a 必须仍在首位（优先级压过 lastOK），实际 %+v", plan)
	}
	if plan[1].Model != "cn:c" || plan[2].Model != "cn:b" {
		t.Errorf("lastOK 应提到同权重层首（cn:c 在 cn:b 之前），实际 %+v", plan)
	}
}

// TestPlanHigherWeightBeatsLastOK ⭐ 用户实测反馈：开启「上次可用成员优先」后，
// 高优成员（权重更高）恢复可用时仍应优先使用它 —— lastOK 不得跨权重层提升。
func TestPlanHigherWeightBeatsLastOK(t *testing.T) {
	s := mustCompile(t, []GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:hi", Weight: 100},
		{Model: "cn:mid", Weight: 90},
		{Model: "cn:lo", Weight: 80},
	}}})
	now := time.Now()
	// 模拟「高优成员失败、降级到低优成员并成功」：lastOK 落在权重最低的成员上。
	s.NoteOK("g", "cn:lo")

	plan := s.Plan("g", now)
	want := []string{"cn:hi", "cn:mid", "cn:lo"}
	for i := range want {
		if plan[i].Model != want[i] {
			t.Fatalf("plan[%d] = %s, want %s（lastOK 不得越过更高权重，全序列 %+v）",
				i, plan[i].Model, want[i], plan)
		}
	}
}

// TestPlanDropsLastOKWhenInvisible ⭐ lastOK 指向已停用/时段外成员时不得提到首位
// （否则每次请求都先撞一个不可能成功的成员）。
func TestPlanDropsLastOKWhenInvisible(t *testing.T) {
	off := false
	s := mustCompile(t, []GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:a", Weight: 100},
		{Model: "cn:b", Weight: 90, Enabled: &off},
	}}})
	now := time.Now()
	s.NoteOK("g", "cn:b")

	plan := s.Plan("g", now)
	if len(plan) != 1 || plan[0].Model != "cn:a" {
		t.Fatalf("停用成员不应出现在计划里，实际 %+v", plan)
	}
}

// TestPlanEmptyWhenAllHidden 全部停用或时段外 → 空计划（终态要单独报错）。
func TestPlanEmptyWhenAllHidden(t *testing.T) {
	off := false
	s := mustCompile(t, []GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:a", Weight: 100, Enabled: &off},
		{Model: "cn:b", Weight: 90, Window: "23:00-07:00"},
	}}})
	noon := time.Date(2026, 10, 25, 12, 0, 0, 0, cstLoc())
	if plan := s.Plan("g", noon); len(plan) != 0 {
		t.Fatalf("全部不可见时应返回空计划，实际 %+v", plan)
	}
}

// TestPlanOrderIsStableOnEqualWeight 同权重按列表顺序。
func TestPlanOrderIsStableOnEqualWeight(t *testing.T) {
	s := mustCompile(t, []GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:first", Weight: 50},
		{Model: "cn:second", Weight: 50},
		{Model: "cn:third", Weight: 50},
	}}})
	plan := s.Plan("g", time.Now())
	want := []string{"cn:first", "cn:second", "cn:third"}
	for i := range want {
		if plan[i].Model != want[i] {
			t.Fatalf("plan[%d] = %s, want %s（全序列 %+v）", i, plan[i].Model, want[i], plan)
		}
	}
}

// TestEnabledMembersForSniff ⭐ Review Focus 第 4 条：
// 嗅探候选集 = 所有启用组里当前可见的成员（去重）。
// 停用成员与时段外成员都不得出现 —— 否则「夜里才开」的成员会在白天被反复探活烧额度。
func TestEnabledMembersForSniff(t *testing.T) {
	off := false
	s := mustCompile(t, []GroupConfig{
		{ID: "g1", Enabled: true, Members: []MemberConfig{
			{Model: "cn:a", Weight: 100},
			{Model: "cn:dup", Weight: 90},
			{Model: "cn:off", Weight: 80, Enabled: &off},
		}},
		{ID: "g2", Enabled: true, Members: []MemberConfig{
			{Model: "cn:dup", Weight: 100},
			{Model: "cn:night", Weight: 90, Window: "23:00-07:00"},
		}},
		{ID: "g3", Enabled: false, Members: []MemberConfig{
			{Model: "cn:inactive-group", Weight: 100},
		}},
	})
	noon := time.Date(2026, 10, 25, 12, 0, 0, 0, cstLoc())
	got := s.EnabledMembers(noon)
	names := make([]string, 0, len(got))
	for _, m := range got {
		names = append(names, m.Model)
	}
	want := map[string]bool{"cn:a": true, "cn:dup": true}
	if len(names) != len(want) {
		t.Fatalf("嗅探候选 = %v, want 仅 %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("不应出现在嗅探候选里：%s（全序列 %v）", n, names)
		}
	}
}

// TestReferencedIncludesInvisibleMembers 缓存摘除判据用「引用」而非「可见」：
// 白天时段外的成员晚上还要用，不能因为此刻不可见就把缓存条目删掉。
func TestReferencedIncludesInvisibleMembers(t *testing.T) {
	s := mustCompile(t, []GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:night", Weight: 100, Window: "23:00-07:00"},
	}}})
	if !s.Referenced("cn", "night") {
		t.Errorf("时段外成员仍被引用，不应被摘除")
	}
	if s.Referenced("global", "night") {
		t.Errorf("不同域不应算作引用")
	}
}
