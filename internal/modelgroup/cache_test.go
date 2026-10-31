package modelgroup

import (
	"testing"
	"time"
)

// TestCacheSkipIsOptimistic 乐观语义：只有「标记不可用 且 until 未到」才跳过。
// until 一过立即当可用去真实尝试 —— 最坏多打一次上游，好过「明明恢复了还在等嗅探」。
func TestCacheSkipIsOptimistic(t *testing.T) {
	var c Cache
	now := time.Now()

	if c.Skip("cn", "m", now) {
		t.Fatalf("无记录时应尝试，不该跳过")
	}

	c.MarkLocked("cn", "m", State{
		Kind: KindModelRate, Reason: "6004", Until: now.Add(10 * time.Minute),
	}, now)
	if !c.Skip("cn", "m", now) {
		t.Fatalf("锁到未来时应跳过")
	}
	if c.Skip("cn", "m", now.Add(11*time.Minute)) {
		t.Fatalf("until 已过应放行（乐观重试）")
	}

	c.MarkOK("cn", "m", now)
	if c.Skip("cn", "m", now) {
		t.Fatalf("标记可用后不该跳过")
	}
}

// TestCacheKeyIsPerRealm 同名模型在两个域是两份独立状态。
func TestCacheKeyIsPerRealm(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkLocked("cn", "glm", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)
	if c.Skip("global", "glm", now) {
		t.Errorf("cn 域锁定不应影响 global 域")
	}
	if !c.Skip("cn", "glm", now) {
		t.Errorf("cn 域应保持锁定")
	}
}

// TestCacheTouchUntilKeepsUsable ⭐ Review Focus 第 2 条的一部分：
// TouchUntil 只更新预计恢复时刻，不得把可用改成不可用 ——
// 同成员内换号仍可能成功，改成不可用会让高优成员的剩余额度被白白闲置。
func TestCacheTouchUntilKeepsUsable(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkOK("cn", "m", now)
	c.TouchUntil("cn", "m", now.Add(30*time.Minute), "6004 预告墙钟")
	if c.Skip("cn", "m", now) {
		t.Fatalf("TouchUntil 不得把可用改成不可用")
	}
	st, ok := c.Get("cn", "m")
	if !ok {
		t.Fatal("Get 应命中")
	}
	if !st.Until.After(now) {
		t.Errorf("Until 应被更新到未来，实际 %v", st.Until)
	}
	if !st.Usable {
		t.Errorf("Usable 应保持 true，实际 false")
	}
}

// TestCacheSnapshotAndPrune 面板快照与条目摘除。
func TestCacheSnapshotAndPrune(t *testing.T) {
	var c Cache
	now := time.Now()
	c.MarkLocked("cn", "a", State{Kind: KindModelRate, Until: now.Add(time.Hour)}, now)
	c.MarkOK("global", "b", now)

	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot 条目 = %d, want 2", len(snap))
	}
	if _, ok := snap["cn\x1fa"]; !ok {
		t.Errorf("snapshot key 应为 realm\\x1fbare，实际 %v", snap)
	}

	c.PruneAlive(func(realm, bare string) bool { return realm == "cn" && bare == "a" })
	snap = c.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("prune 后条目 = %d, want 1（%v）", len(snap), snap)
	}
}
