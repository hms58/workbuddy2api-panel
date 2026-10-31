// sniff.go 定时嗅探已锁成员的恢复情况。
//
// 两段式（先免费判定，必要时才真探活）：
//  1. until 已过 → 直接标记可用，零上游调用
//  2. 只有 model_rate 类、剩余冷却够长、且距上次探活够久 → 才真探活
//
// 时段外的成员不探：探活了也用不上，纯消耗额度。
package modelgroup

import (
	"context"
	"log"
	"time"
)

// Prober 成员级探活。实现方（internal/server）内部按该域账号逐个试，
// 任一个成功即返回 ok=true（成员级恢复）。
type Prober interface {
	Probe(ctx context.Context, realm, bare string) (ok bool, until time.Time, reason string)
}

// Sniffer 嗅探调度器。
type Sniffer struct {
	cache  *Cache
	groups func() *GroupSet
	prober Prober
	opts   Options
}

// NewSniffer 构建。groups 用函数而非实例：热更后自动拿到新集合。
func NewSniffer(cache *Cache, groups func() *GroupSet, prober Prober, opts Options) *Sniffer {
	return &Sniffer{cache: cache, groups: groups, prober: prober, opts: opts.WithDefaults()}
}

// RunOnce 执行一轮。返回 (探活次数, 恢复数)。可被测试与定时循环直接调用。
func (s *Sniffer) RunOnce(ctx context.Context, now time.Time) (probed, recovered int) {
	if s == nil || s.cache == nil || !s.opts.SniffEnabled {
		return 0, 0
	}
	return s.runOnce(ctx, now)
}

// RunOnceNow 手动触发一轮，忽略 sniff_enabled 总开关。
//
// 为什么允许绕过：sniff_enabled=false 的语义是「不要为探活自动消耗额度」，
// 而运维在面板上显式点「立即嗅探」是明确的一次性意图 —— 用总开关把它拦掉，
// 用户只能改配置 + 重启才能探一次，代价远大于收益。
// 但频率保护（sniff_min_until / sniff_min_gap）**不**绕过：那是防止打爆上游的护栏，
// 与「自动还是手动」无关。
func (s *Sniffer) RunOnceNow(ctx context.Context, now time.Time) (probed, recovered int) {
	if s == nil || s.cache == nil {
		return 0, 0
	}
	return s.runOnce(ctx, now)
}

// runOnce 是两段式嗅探的实际实现，被定时循环与手动触发共用。
func (s *Sniffer) runOnce(ctx context.Context, now time.Time) (probed, recovered int) {
	gs := s.groups()
	if gs == nil {
		return 0, 0
	}

	// 候选集：启用组里当前可见的成员（去重）。停用与时段外都不在里面。
	visible := gs.EnabledMembers(now)
	if len(visible) == 0 {
		s.cache.PruneAlive(gs.Referenced)
		return 0, 0
	}

	for _, m := range visible {
		st, ok := s.cache.Get(m.Realm, m.Bare)
		if !ok || st.Usable {
			continue
		}
		// ① 免费判定：until 已过 → 直接恢复
		if !st.Until.IsZero() && !now.Before(st.Until) {
			s.cache.MarkOK(m.Realm, m.Bare, now)
			recovered++
			continue
		}
		// ② 只探 model_rate
		if st.Kind != KindModelRate {
			continue
		}
		// ③ 剩余冷却太短：等 TTL 就行
		if !st.Until.IsZero() && st.Until.Sub(now) < s.opts.SniffMinUntil {
			continue
		}
		// ④ 频率上限
		if !st.ProbedAt.IsZero() && now.Sub(st.ProbedAt) < s.opts.SniffMinGap {
			continue
		}

		probed++
		okProbe, until, reason := s.prober.Probe(ctx, m.Realm, m.Bare)
		if okProbe {
			s.cache.MarkOK(m.Realm, m.Bare, now)
			s.cache.NoteProbe(m.Realm, m.Bare, now)
			recovered++
			log.Printf("[modelgroup] 嗅探成功：%s:%s 已提前恢复", m.Realm, m.Bare)
			continue
		}
		next := until
		if next.IsZero() {
			next = st.Until // 上游没给新墙钟 → 沿用旧的
		}
		s.cache.MarkLocked(m.Realm, m.Bare, State{
			Kind:        KindModelRate,
			Reason:      reason,
			Until:       next,
			Servable:    0,
			LockedAccts: st.LockedAccts,
		}, now)
		s.cache.NoteProbe(m.Realm, m.Bare, now)
	}
	// 摘除不再被任何启用组引用的条目（含本轮未探到的）。
	s.cache.PruneAlive(gs.Referenced)
	return probed, recovered
}

// Start 后台循环（ctx 取消即停）。interval <= 0 时不启动。
func (s *Sniffer) Start(ctx context.Context) {
	if s == nil || !s.opts.SniffEnabled || s.opts.SniffInterval <= 0 {
		return
	}
	interval := s.opts.SniffInterval
	log.Printf("[modelgroup] 嗅探已启用：每 %s 一轮（min_until=%s, min_gap=%s）",
		interval, s.opts.SniffMinUntil, s.opts.SniffMinGap)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.RunOnce(ctx, time.Now())
			}
		}
	}()
}
