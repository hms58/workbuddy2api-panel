// growthclaim.go 成长任务的「完成」与「领取积分」拆分：领奖闸门 + 人工领取入口。
//
// 为什么拆：官方奖励积分只有一个月有效期，任务做完立刻领等于提前开始倒计时；
// 用户当前积分充足时没必要马上领。定时轮次因此只完成动作，积分留到需要时人工领。
// 例外是活动任务剩余不足 24 小时——不领就会过期，此时自动领。
package panel

import (
	"fmt"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

const (
	claimModeAuto   = "auto"
	claimModeManual = "manual"
	// claimExpiringWindow 活动剩余不足该时长时，手动模式也自动领奖（避免忘记导致过期）。
	claimExpiringWindow = 24 * time.Hour
)

// SetGrowthClaimMode 设置领奖模式；空值与非法值一律回落 manual（安全侧：宁可少领不误领）。
func (p *Panel) SetGrowthClaimMode(mode string) {
	v := int64(0)
	if strings.TrimSpace(mode) == claimModeAuto {
		v = 1
	}
	p.growthClaimMode.Store(v)
}

// growthClaimModeValue 当前领奖模式（未设置 = manual）。
func (p *Panel) growthClaimModeValue() string {
	if p.growthClaimMode.Load() == 1 {
		return claimModeAuto
	}
	return claimModeManual
}

// autoClaimAllowed 自动领奖的唯一闸门。返回 (是否允许, 原因)。
func (p *Panel) autoClaimAllowed(t *upstream.Task) (bool, string) {
	return p.autoClaimAllowedAt(t, time.Now())
}

// autoClaimAllowedAt 同上，时间可注入（测试与回放用）。
func (p *Panel) autoClaimAllowedAt(t *upstream.Task, now time.Time) (bool, string) {
	if p.growthClaimModeValue() == claimModeAuto {
		return true, "auto 模式：达标即领"
	}
	if t == nil {
		return false, "任务状态不可用"
	}
	end, ok := t.ValidEndTime()
	if !ok {
		return false, "上游未下发活动截止时间，保留人工领取"
	}
	remain := end.Sub(now)
	if remain <= 0 {
		return false, "活动已结束"
	}
	if remain <= claimExpiringWindow {
		return true, fmt.Sprintf("活动剩余 %.1f 小时，自动领取避免过期", remain.Hours())
	}
	return false, fmt.Sprintf("活动剩余 %.1f 天，保留人工领取", remain.Hours()/24)
}
