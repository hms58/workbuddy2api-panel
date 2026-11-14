// growthclaim.go 成长任务的「完成」与「领取积分」拆分：领奖闸门 + 人工领取入口。
//
// 为什么拆：官方奖励积分只有一个月有效期，任务做完立刻领等于提前开始倒计时；
// 用户当前积分充足时没必要马上领。定时轮次因此只完成动作，积分留到需要时人工领。
// 例外是活动任务剩余不足 24 小时——不领就会过期，此时自动领。
package panel

import (
	"context"
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

	tasks, err := p.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	all := make([]upstream.Task, 0, len(tasks)*2)
	all = append(all, tasks...)
	if mp, mpErr := p.ListTasksMP(a); mpErr == nil {
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
		// 用与执行器同口径的达标判定：mp 任务 accept 前上游 target 下发 0，
		// 只看 t.Claimable 会把它们整批漏掉（runner 已不自己领奖，就再没人领）。
		if t.Claimed || t.Locked || !taskReachedForAuto(&t) {
			continue
		}
		c, e, cerr := p.claimTaskLogged(a, t.TaskCode, "manual")
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

// ---------------------------------------------------------------------------
// 批量轮次任务列表缓存
//
// 批量任务（定时成长队列 / 任务中心队列 / 补领清扫）对同一账号要反复「按 code
// 查任务」，逐任务重拉全量列表是上游 429 的主要来源。约定：
//   - 批量开始：ListALLTasks 预取该账号两口径列表写入 accountTaskListMap；
//   - 批量期间：ListTasks / ListTasksMP（及 taskByCode / taskByCodeMP）命中
//     缓存直接返回，不再打上游；
//   - 批量结束：clearAccountTaskList 清除该账号条目，不影响下一轮取新。
//   - 动作后的异步计分回读必须走 taskByCodeLive / taskByCodeMPLive（不吃缓存），
//     否则永远读到动作前的旧进度。
// ---------------------------------------------------------------------------

// taskListWrap 单账号一轮批量任务的两口径任务列表缓存值。
type taskListWrap struct {
	cnTaskList []upstream.Task // web 默认口径；nil = 本轮该口径拉取失败
	mpTaskList []upstream.Task // mp 小程序口径；nil = 拉取失败
}

// cachedTaskLists 读账号的批量缓存条目；未建立返回 ok=false（调用方走直连）。
func (p *Panel) cachedTaskLists(uid string) (taskListWrap, bool) {
	p.accountTaskListMu.Lock()
	defer p.accountTaskListMu.Unlock()
	lists, ok := p.accountTaskListMap[uid]
	return lists, ok
}

// storeTaskLists 写入/覆盖账号的批量缓存条目（map 惰性创建）。
func (p *Panel) storeTaskLists(uid string, lists taskListWrap) {
	p.accountTaskListMu.Lock()
	defer p.accountTaskListMu.Unlock()
	if p.accountTaskListMap == nil {
		p.accountTaskListMap = make(map[string]taskListWrap)
	}
	p.accountTaskListMap[uid] = lists
}

// mergeTaskLists 把单口径列表并入账号的批量缓存条目（nil 侧不动另一口径）。
// 供批量轮次的分阶段预取使用：阶段 0 先存 web 口径、阶段 0b 再补 mp 口径。
func (p *Panel) mergeTaskLists(uid string, cn, mp []upstream.Task) {
	if cn == nil && mp == nil {
		return
	}
	p.accountTaskListMu.Lock()
	defer p.accountTaskListMu.Unlock()
	if p.accountTaskListMap == nil {
		p.accountTaskListMap = make(map[string]taskListWrap)
	}
	lists := p.accountTaskListMap[uid]
	if cn != nil {
		lists.cnTaskList = cn
	}
	if mp != nil {
		lists.mpTaskList = mp
	}
	p.accountTaskListMap[uid] = lists
}

// clearAccountTaskList 批量轮次结束（defer 调用）清除该账号的缓存条目。
// 按账号清而不是整表置 nil：多个账号的后台任务并发跑（job 并发 2），
// 整表置 nil 会抹掉其他账号还没用完的条目。
func (p *Panel) clearAccountTaskList(uid string) {
	p.accountTaskListMu.Lock()
	defer p.accountTaskListMu.Unlock()
	delete(p.accountTaskListMap, uid)
}

// ListALLTasks 一次拉齐账号两口径任务列表并写入批量缓存（批量轮次的预取入口）。
// 两次拉取之间检查 ctx：取消后不再发起新请求，保持「只等在途请求」的服务
// 关闭契约。返回合并去重后的列表；单口径失败不阻塞另一口径（失败侧不入缓存，
// 批量期间该口径读回落实时拉取），两口径都失败才返回错误。
func (p *Panel) ListALLTasks(ctx context.Context, a *auth.Auth) ([]upstream.Task, error) {
	lists := taskListWrap{}
	var cnErr, mpErr error
	if ctx.Err() == nil {
		var cn []upstream.Task
		if cn, cnErr = p.cfg.Upstream.ListTasks(a); cnErr == nil {
			lists.cnTaskList = cn
		}
	}
	if ctx.Err() == nil {
		var mp []upstream.Task
		if mp, mpErr = p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
			lists.mpTaskList = mp
		}
	}
	if lists.cnTaskList == nil && lists.mpTaskList == nil {
		if cnErr == nil {
			cnErr = ctx.Err() // ctx 已取消跳过了拉取
		}
		return nil, fmt.Errorf("list tasks: cn=%v, mp=%v", cnErr, mpErr)
	}
	p.storeTaskLists(a.UID, lists)
	merged := make([]upstream.Task, 0, len(lists.cnTaskList)+len(lists.mpTaskList))
	seen := make(map[string]bool, cap(merged))
	for _, t := range lists.cnTaskList {
		merged = append(merged, t)
		seen[t.TaskCode] = true
	}
	// mp 列表是默认口径的超集，同码只保留一份。
	for _, t := range lists.mpTaskList {
		if !seen[t.TaskCode] {
			merged = append(merged, t)
		}
	}
	return merged, nil
}

// ListTasks 带批量缓存的 web 口径任务列表：批量轮次（缓存已建立）直接返回
// 预取结果；缓存未建立时透传上游（单任务手动入口语义不变）。
// 输入输出与 p.cfg.Upstream.ListTasks 一致，错误由调用方按原口径处理。
func (p *Panel) ListTasks(a *auth.Auth) ([]upstream.Task, error) {
	if lists, ok := p.cachedTaskLists(a.UID); ok && lists.cnTaskList != nil {
		return lists.cnTaskList, nil
	}
	return p.cfg.Upstream.ListTasks(a)
}

// ListTasksMP 带批量缓存的 mp 口径任务列表，语义同 ListTasks。
func (p *Panel) ListTasksMP(a *auth.Auth) ([]upstream.Task, error) {
	if lists, ok := p.cachedTaskLists(a.UID); ok && lists.mpTaskList != nil {
		return lists.mpTaskList, nil
	}
	return p.cfg.Upstream.ListTasksMP(a)
}

// taskByCodeLive 同 taskByCode 但始终实时拉取，不吃批量缓存——动作后的
// 异步计分回读专用（上游计分数秒后才刷新，缓存里永远是动作前的旧进度）。
func (p *Panel) taskByCodeLive(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	if isMPTaskCode(code) {
		return p.taskByCodeMPLive(a, code)
	}
	return nil, nil
}

// taskByCodeMPLive 同 taskByCodeMP 但始终实时拉取，不吃批量缓存——accept 登记
// 验证与动作后的异步计分回读专用（缓存里是动作前的旧状态）。
func (p *Panel) taskByCodeMPLive(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasksMP(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}
