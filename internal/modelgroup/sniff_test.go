package modelgroup

import (
	"context"
	"testing"
	"time"
)

type fakeProber struct {
	ok     bool
	until  time.Time
	reason string
	calls  []string
}

func (f *fakeProber) Probe(ctx context.Context, realm, bare string) (bool, time.Time, string) {
	f.calls = append(f.calls, realm+":"+bare)
	return f.ok, f.until, f.reason
}

// TestSnifferOnlyProbesModelRate 只有 model_rate 类值得真探活：
// model_missing（结构性缺失）与 hard_credit（余额耗尽）探了也必然失败。
func TestSnifferOnlyProbesModelRate(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkLocked("cn", "rate", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)
	c.MarkLocked("cn", "missing", State{Kind: KindModelMissing, Until: now.Add(time.Hour)}, now)
	c.MarkLocked("cn", "credit", State{Kind: KindHardCredit, Until: now.Add(time.Hour)}, now)

	compiled, opts := Compile([]GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:rate", Weight: 100},
		{Model: "cn:missing", Weight: 90},
		{Model: "cn:credit", Weight: 80},
	}}}, Options{SniffEnabled: true})
	gs := NewGroupSet(compiled, opts)
	pr := &fakeProber{ok: false}
	s := NewSniffer(&c, func() *GroupSet { return gs }, pr, opts)

	s.RunOnce(context.Background(), now)
	if len(pr.calls) != 1 || pr.calls[0] != "cn:rate" {
		t.Fatalf("只应探 model_rate 类，实际探了 %v", pr.calls)
	}
}

// TestSnifferSkipsDisabledAndOutOfWindow ⭐ Review Focus 第 4 条：
// 停用成员与时段外成员不得被探活 —— 夜里才免费开的模型，白天探活了也用不上，
// 纯消耗额度且增加风控暴露。
func TestSnifferSkipsDisabledAndOutOfWindow(t *testing.T) {
	var c Cache
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, cstLoc()) // 正午
	c.MarkLocked("cn", "off", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)
	c.MarkLocked("cn", "night", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)
	c.MarkLocked("cn", "day", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)

	off := false
	compiled, opts := Compile([]GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:off", Weight: 100, Enabled: &off},
		{Model: "cn:night", Weight: 90, Window: "23:00-07:00"},
		{Model: "cn:day", Weight: 80},
	}}}, Options{SniffEnabled: true})
	gs := NewGroupSet(compiled, opts)
	pr := &fakeProber{ok: false}
	s := NewSniffer(&c, func() *GroupSet { return gs }, pr, opts)

	s.RunOnce(context.Background(), now)
	if len(pr.calls) != 1 || pr.calls[0] != "cn:day" {
		t.Fatalf("正午只应探 cn:day，实际探了 %v", pr.calls)
	}
}

// TestSnifferRespectsMinGap 同一成员两次探活至少间隔 SniffMinGap。
func TestSnifferRespectsMinGap(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkLocked("cn", "m", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)

	compiled, opts := Compile([]GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:m", Weight: 100},
	}}}, Options{SniffEnabled: true, SniffMinGap: 10 * time.Minute})
	gs := NewGroupSet(compiled, opts)
	pr := &fakeProber{ok: false}
	s := NewSniffer(&c, func() *GroupSet { return gs }, pr, opts)

	s.RunOnce(context.Background(), now)
	if len(pr.calls) != 1 {
		t.Fatalf("首轮应探 1 次，实际 %v", pr.calls)
	}
	s.RunOnce(context.Background(), now.Add(5*time.Minute))
	if len(pr.calls) != 1 {
		t.Fatalf("未到 min_gap 不应再探，实际 %v", pr.calls)
	}
	s.RunOnce(context.Background(), now.Add(11*time.Minute))
	if len(pr.calls) != 2 {
		t.Fatalf("超过 min_gap 应再探，实际 %v", pr.calls)
	}
}

// TestSnifferSkipsShortRemainingCooldown 剩余冷却短于 SniffMinUntil 时不探：
// 等 TTL 到期就行，探活是浪费。
func TestSnifferSkipsShortRemainingCooldown(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkLocked("cn", "m", State{Kind: KindModelRate, Until: now.Add(2 * time.Minute)}, now)

	compiled, opts := Compile([]GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:m", Weight: 100},
	}}}, Options{SniffEnabled: true, SniffMinUntil: 5 * time.Minute})
	gs := NewGroupSet(compiled, opts)
	pr := &fakeProber{ok: false}
	s := NewSniffer(&c, func() *GroupSet { return gs }, pr, opts)

	s.RunOnce(context.Background(), now)
	if len(pr.calls) != 0 {
		t.Fatalf("剩余冷却 2min < min_until 5min，不该探活，实际 %v", pr.calls)
	}
}

// TestSnifferRecoversOnProbeSuccess 探活成功 → 缓存标记可用。
func TestSnifferRecoversOnProbeSuccess(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkLocked("cn", "m", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)

	compiled, opts := Compile([]GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:m", Weight: 100},
	}}}, Options{SniffEnabled: true})
	gs := NewGroupSet(compiled, opts)
	pr := &fakeProber{ok: true}
	s := NewSniffer(&c, func() *GroupSet { return gs }, pr, opts)

	probed, recovered := s.RunOnce(context.Background(), now)
	if probed != 1 || recovered != 1 {
		t.Fatalf("RunOnce = (%d,%d), want (1,1)", probed, recovered)
	}
	if c.Skip("cn", "m", now) {
		t.Fatalf("探活成功后不应再跳过")
	}
}

// TestSnifferExpiredUntilMarksUsableForFree until 已过 → 直接标记可用，不消耗额度。
func TestSnifferExpiredUntilMarksUsableForFree(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkLocked("cn", "m", State{Kind: KindModelRate, Until: now.Add(-time.Minute)}, now)

	compiled, opts := Compile([]GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:m", Weight: 100},
	}}}, Options{SniffEnabled: true})
	gs := NewGroupSet(compiled, opts)
	pr := &fakeProber{ok: false}
	s := NewSniffer(&c, func() *GroupSet { return gs }, pr, opts)

	s.RunOnce(context.Background(), now)
	if len(pr.calls) != 0 {
		t.Fatalf("until 已过应免费恢复，不该探活，实际 %v", pr.calls)
	}
	if c.Skip("cn", "m", now) {
		t.Fatalf("until 已过应标记可用")
	}
}

// TestSnifferDisabled 总开关关闭时不做任何事。
func TestSnifferDisabled(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkLocked("cn", "m", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)

	compiled, opts := Compile([]GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:m", Weight: 100},
	}}}, Options{SniffEnabled: false})
	gs := NewGroupSet(compiled, opts)
	pr := &fakeProber{ok: true}
	s := NewSniffer(&c, func() *GroupSet { return gs }, pr, opts)

	if probed, _ := s.RunOnce(context.Background(), now); probed != 0 {
		t.Fatalf("sniff_enabled=false 时不应探活，实际 %d 次", probed)
	}
}

// TestSnifferPrunesUnreferencedEntries 缓存条目摘除：不再被任何启用组引用的
// 条目在本轮结束时被删除（防「map 只增不减」）。
func TestSnifferPrunesUnreferencedEntries(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkLocked("cn", "stale", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)
	c.MarkLocked("cn", "live", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)

	compiled, opts := Compile([]GroupConfig{{ID: "g", Enabled: true, Members: []MemberConfig{
		{Model: "cn:live", Weight: 100},
	}}}, Options{SniffEnabled: true})
	gs := NewGroupSet(compiled, opts)
	s := NewSniffer(&c, func() *GroupSet { return gs }, &fakeProber{}, opts)

	s.RunOnce(context.Background(), now)
	if _, ok := c.Get("cn", "stale"); ok {
		t.Errorf("不再被引用的条目应被摘除")
	}
	if _, ok := c.Get("cn", "live"); !ok {
		t.Errorf("仍被引用的条目不应被摘除")
	}
}
