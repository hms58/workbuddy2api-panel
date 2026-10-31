package modelgroup

import (
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

// TestCompileSortsAndParses 归一化的核心契约：
//   - 成员按 weight 降序稳定排序（同权重保持列表原顺序）
//   - 裸名按 cn 处理（与 upstream.SplitRealmPrefix 同口径）
//   - enabled 缺省视为 true
//   - window 一次性解析成 *Window
func TestCompileSortsAndParses(t *testing.T) {
	in := []GroupConfig{{
		ID: "LongTask", Name: "长任务", Enabled: true,
		Members: []MemberConfig{
			{Model: "cn:deepseek-v4.1-flash", Weight: 80},
			{Model: "cn:glm-5.3-flash", Weight: 100},
			{Model: "global:glm-5.3-flash", Weight: 80, Window: "23:00-07:00"},
		},
	}}
	got, _ := Compile(in, Options{})
	if len(got) != 1 {
		t.Fatalf("groups = %d, want 1", len(got))
	}
	g := got[0]
	if g.ID != "longtask" {
		t.Errorf("ID = %q, want 小写 %q", g.ID, "longtask")
	}
	wantOrder := []string{"cn:glm-5.3-flash", "cn:deepseek-v4.1-flash", "global:glm-5.3-flash"}
	if len(g.Members) != len(wantOrder) {
		t.Fatalf("members = %d, want %d", len(g.Members), len(wantOrder))
	}
	for i, w := range wantOrder {
		if g.Members[i].Model != w {
			t.Errorf("member[%d] = %q, want %q（同权重须保持列表顺序）", i, g.Members[i].Model, w)
		}
	}
	if !g.Members[0].Enabled {
		t.Errorf("enabled 缺省应为 true")
	}
	if g.Members[0].Realm != "cn" || g.Members[0].Bare != "glm-5.3-flash" {
		t.Errorf("裸名应解析成 (cn, glm-5.3-flash)，实际 (%q,%q)", g.Members[0].Realm, g.Members[0].Bare)
	}
	if g.Members[2].Realm != "global" {
		t.Errorf("global: 前缀应解析成 global 域，实际 %q", g.Members[2].Realm)
	}
	if g.Members[2].Window == nil || g.Members[2].Window.Start != 23*60 || g.Members[2].Window.End != 7*60 {
		t.Errorf("window 应解析成 23:00-07:00，实际 %+v", g.Members[2].Window)
	}
	if g.Members[0].Window != nil {
		t.Errorf("未配置 window 应为 nil（全天），实际 %+v", g.Members[0].Window)
	}
}

// TestCompileDropsInvalid 非法条目丢弃而非报错：一份配置里写错一个成员，
// 不该让整个组不可用。
func TestCompileDropsInvalid(t *testing.T) {
	in := []GroupConfig{
		{ID: "", Members: []MemberConfig{{Model: "cn:a", Weight: 1}}},
		{ID: "ok", Members: nil},
		{ID: "dup", Members: []MemberConfig{{Model: "cn:a", Weight: 1}}},
		{ID: "dup", Members: []MemberConfig{{Model: "cn:b", Weight: 1}}},
		{ID: "selfdup", Members: []MemberConfig{
			{Model: "cn:x", Weight: 10}, {Model: "cn:x", Weight: 5},
		}},
		{ID: "blank", Members: []MemberConfig{{Model: "  ", Weight: 10}, {Model: "cn:y", Weight: 3}}},
	}
	got, _ := Compile(in, Options{})
	byID := map[string]Group{}
	for _, g := range got {
		byID[g.ID] = g
	}
	if _, ok := byID[""]; ok {
		t.Errorf("空 id 的组应被丢弃")
	}
	if _, ok := byID["ok"]; ok {
		t.Errorf("无成员的组应被丢弃")
	}
	if g, ok := byID["dup"]; !ok || g.Members[0].Model != "cn:b" {
		t.Errorf("重复 id 应后写覆盖，实际 %+v", byID["dup"])
	}
	if g := byID["selfdup"]; len(g.Members) != 1 {
		t.Errorf("组内重复 model 应去重成 1 个，实际 %d 个", len(g.Members))
	}
	if g := byID["blank"]; len(g.Members) != 1 || g.Members[0].Model != "cn:y" {
		t.Errorf("空 model 应被丢弃，实际 %+v", g.Members)
	}
}

// TestCompileNormalizesWeight 权重 <=0 视为 1（不是 0：0 会让排序退化成列表序，
// 与「显式写 0 想排最后」的直觉冲突）。
func TestCompileNormalizesWeight(t *testing.T) {
	in := []GroupConfig{{ID: "g", Members: []MemberConfig{
		{Model: "cn:a", Weight: 0}, {Model: "cn:b", Weight: -5}, {Model: "cn:c", Weight: 1},
	}}}
	got, _ := Compile(in, Options{})
	for _, m := range got[0].Members {
		if m.Weight < 1 {
			t.Errorf("model %s weight = %d, want >= 1", m.Model, m.Weight)
		}
	}
}

// TestVisibleMembers 可见性 = enabled && inWindow，与设计文档 §6.2.1 一致。
func TestVisibleMembers(t *testing.T) {
	noon := time.Date(2026, 10, 25, 12, 0, 0, 0, cstLoc())
	in := []GroupConfig{{ID: "g", Members: []MemberConfig{
		{Model: "cn:always", Weight: 100},
		{Model: "cn:disabled", Weight: 90, Enabled: boolPtr(false)},
		{Model: "cn:night", Weight: 80, Window: "23:00-07:00"},
		{Model: "cn:day", Weight: 70, Window: "09:00-23:00"},
	}}}
	got, _ := Compile(in, Options{})
	g := got[0]

	vis := g.Visible(noon)
	if len(vis) != 2 {
		t.Fatalf("正午可见成员 = %d 个，want 2（always + day），实际 %+v", len(vis), vis)
	}
	if vis[0].Model != "cn:always" || vis[1].Model != "cn:day" {
		t.Errorf("正午可见成员 = [%s %s], want [cn:always cn:day]", vis[0].Model, vis[1].Model)
	}

	night := time.Date(2026, 10, 25, 2, 0, 0, 0, cstLoc())
	vis = g.Visible(night)
	if len(vis) != 2 {
		t.Fatalf("凌晨可见成员 = %d 个，want 2（always + night），实际 %+v", len(vis), vis)
	}
	if vis[1].Model != "cn:night" {
		t.Errorf("凌晨可见成员[1] = %s, want cn:night", vis[1].Model)
	}

	for _, m := range vis {
		if m.Model == "cn:disabled" {
			t.Errorf("停用成员不应出现在可见集里")
		}
	}
}

// TestOptionsDefaults 阈值缺省值必须与设计文档 §7.2 一致。
func TestOptionsDefaults(t *testing.T) {
	o := Options{}.WithDefaults()
	if o.SniffInterval != 60*time.Second {
		t.Errorf("SniffInterval = %v, want 60s", o.SniffInterval)
	}
	if o.SniffMinUntil != 5*time.Minute {
		t.Errorf("SniffMinUntil = %v, want 5m", o.SniffMinUntil)
	}
	if o.SniffMinGap != 10*time.Minute {
		t.Errorf("SniffMinGap = %v, want 10m", o.SniffMinGap)
	}
	o2 := Options{PreferLastOK: true, SniffEnabled: true}.WithDefaults()
	if !o2.PreferLastOK || !o2.SniffEnabled {
		t.Errorf("显式开启的开关不应被 WithDefaults 覆盖，实际 %+v", o2)
	}
}
