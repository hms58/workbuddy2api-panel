// growthclaim.go 成长任务的「完成」与「领取积分」拆分：领奖闸门 + 人工领取入口。
//
// 为什么拆：官方奖励积分只有一个月有效期，任务做完立刻领等于提前开始倒计时；
// 用户当前积分充足时没必要马上领。定时轮次因此只完成动作，积分留到需要时人工领。
// 例外是活动任务剩余不足 24 小时——不领就会过期，此时自动领。
package panel

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
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

// claimTask 领取单个任务奖励的唯一出口：小程序口径走 chat 域 mp 头，其余走 Web 域。
// 人工领取与自动领取共用它，避免 mp/web 判断散落在多个调用点。
func (p *Panel) claimTask(a *auth.Auth, code string) (credit, energy int64, err error) {
	if isMPTaskCode(code) {
		return p.cfg.Upstream.ClaimRewardMP(a, code)
	}
	return p.cfg.Upstream.ClaimReward(a, code)
}

// growthClaimRoutes 注册成长任务人工领取相关路由（panel.routes 末尾调用一次）。
func (p *Panel) growthClaimRoutes() {
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim_all", p.withAuth(p.accountTaskClaimAll))
}

// accountTaskClaimAll 一次性领取该账号全部「已达标未领」的奖励。
// 人工领取不过模式闸门：用户点按钮就是明确要求领奖。
func (p *Panel) accountTaskClaimAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	if !a.SupportsPersonalEndpoints() {
		writeErr(w, http.StatusUnprocessableEntity, noPersonalTaskMessage)
		return
	}
	if a.IsGlobal() {
		writeErr(w, http.StatusNotImplemented, globalTaskWriteMessage)
		return
	}
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	defer p.unlockAccount(uid)

	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	all := make([]upstream.Task, 0, len(tasks)*2)
	all = append(all, tasks...)
	if mp, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
		all = append(all, mp...)
	} else {
		log.Printf("panel: 全部领取 uid=%s mp 列表: %v", uid, mpErr)
	}

	seen := make(map[string]bool, len(all))
	var claimed int
	var credit, energy int64
	failed := make([]map[string]any, 0)
	for _, t := range all {
		if t.TaskCode == "" || seen[t.TaskCode] {
			continue // mp 列表是默认口径的超集，同码只能领一次
		}
		seen[t.TaskCode] = true
		if !t.Claimable || t.Claimed || t.Locked {
			continue
		}
		c, e, cerr := p.claimTask(a, t.TaskCode)
		if cerr != nil {
			log.Printf("panel: 全部领取 uid=%s %s: %v", uid, t.TaskCode, cerr)
			failed = append(failed, map[string]any{"task_code": t.TaskCode, "error": cerr.Error()})
			continue
		}
		claimed++
		credit += c
		energy += e
	}
	log.Printf("panel: 全部领取 uid=%s 领奖 %d 项 +%d分 +%d能（失败 %d）", uid, claimed, credit, energy, len(failed))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "claimed": claimed, "credit": credit, "energy": energy, "failed": failed,
	})
}
