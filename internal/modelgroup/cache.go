// cache.go 成员级可用性缓存。
//
// key 是 (域, 裸名)，不按组隔离：可用性是模型属性，多组共享同一份，
// 避免「A 组认为可用、B 组认为锁定」的分裂。
//
// 判定的是**成员级**不可用（该域全部账号都服务不了这个模型），不是单账号状态。
// 单账号的 6004/402 由 pool 在同成员内换号消化，不落这里 —— 落错会让高优成员的
// 剩余额度被白白闲置。
package modelgroup

import (
	"strings"
	"sync"
	"time"
)

// LockKind 锁因分类。决定嗅探行为：只有 KindModelRate 值得真探活。
type LockKind string

const (
	// KindModelRate 模型级额度窗口（6004/6000/14003）：上游可能提前恢复，值得探。
	KindModelRate LockKind = "model_rate"
	// KindModelMissing 结构性缺失/权限拒绝（11102/11158）：换号也不会好。
	KindModelMissing LockKind = "model_missing"
	// KindHardCredit 该域账号整体余额耗尽（402）：等次日 04:00 重置。
	KindHardCredit LockKind = "hard_credit"
	// KindNoAccount 账号级冷却/在途占满/积分保底：与模型无关，等一下就好。
	KindNoAccount LockKind = "no_account"
	// KindUnknown 未分类。
	KindUnknown LockKind = "unknown"
)

// State 单个 (域, 裸名) 的运行态。
type State struct {
	Usable      bool      // 成员级：该域是否还有账号能服务这个模型
	Until       time.Time // 预计恢复时刻；零值 = 未知（乐观尝试）
	Kind        LockKind
	Reason      string
	Servable    int // 最近一次聚合的可服务账号数
	LockedAccts int // 该域被这个模型锁住的账号数（诊断用）
	LastOK      time.Time
	LastFail    time.Time
	ProbedAt    time.Time
}

// Cache 可用性缓存。零值可直接使用。
type Cache struct {
	mu sync.RWMutex
	m  map[string]State
}

func cacheKey(realm, bare string) string { return realm + "\x1f" + bare }

// Skip 报告本请求是否应跳过该成员：仅当「标记不可用 且 until 未到」。
//
// 乐观语义：until 一过立即放行真实尝试。这样最坏只多打一次上游，
// 不会出现「上游已恢复、网关还在等嗅探」的黑障。
func (c *Cache) Skip(realm, bare string, now time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.m[cacheKey(realm, bare)]
	if !ok || st.Usable {
		return false
	}
	return st.Until.IsZero() || now.Before(st.Until)
}

// MarkOK 该成员成功服务了一次。
func (c *Cache) MarkOK(realm, bare string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]State)
	}
	k := cacheKey(realm, bare)
	st := c.m[k]
	st.Usable = true
	st.Until = time.Time{}
	st.Kind = KindUnknown
	st.Reason = ""
	st.LastOK = now
	c.m[k] = st
}

// MarkLocked 落成员级锁。只在「该域全部账号都不可用」时调用。
func (c *Cache) MarkLocked(realm, bare string, in State, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]State)
	}
	k := cacheKey(realm, bare)
	st := c.m[k]
	st.Usable = false
	st.Kind = in.Kind
	st.Reason = in.Reason
	st.Until = in.Until
	st.Servable = in.Servable
	st.LockedAccts = in.LockedAccts
	st.LastFail = now
	c.m[k] = st
}

// TouchUntil 只更新预计恢复时刻与原因，不动 Usable。
//
// 用途：收到 6004 预告墙钟时提前记下「何时值得重试」，但该成员此刻仍可能
// 通过同域其它账号成功 —— 若顺手改成不可用，会把还有额度的成员误判为耗尽。
func (c *Cache) TouchUntil(realm, bare string, until time.Time, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		return
	}
	k := cacheKey(realm, bare)
	st, ok := c.m[k]
	if !ok {
		return
	}
	st.Until = until
	if reason != "" {
		st.Reason = reason
	}
	c.m[k] = st
}

// NoteProbe 记录一次探活时刻（频率上限用）。
func (c *Cache) NoteProbe(realm, bare string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		return
	}
	k := cacheKey(realm, bare)
	st, ok := c.m[k]
	if !ok {
		return
	}
	st.ProbedAt = now
	c.m[k] = st
}

// Get 读一份拷贝（面板与嗅探用）。
func (c *Cache) Get(realm, bare string) (State, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.m[cacheKey(realm, bare)]
	return st, ok
}

// Snapshot 返回全量拷贝（面板渲染用，只读）。
func (c *Cache) Snapshot() map[string]State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]State, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// PruneAlive 摘除不再被任何启用组引用的条目，防「map 只增不减」。
func (c *Cache) PruneAlive(keep func(realm, bare string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		realm, bare, ok := strings.Cut(k, "\x1f")
		if !ok || !keep(realm, bare) {
			delete(c.m, k)
		}
	}
}
