// taskaudit.go 任务中心关键动作的审计日志（逐项完成 / 积分领取 / 后台作业轮次），
// 以及任务频道落盘去抖。
//
// 为什么单独成文件：这些是本仓定制（上游没有对应实现），集中在这里可以让合并主干
// 时只需审一个新文件；对既有文件的改动压到每处 1 行（调用点见各函数注释）。
//
// 日志格式约定（三条都服务于「能被既有设施接住」）：
//   - 行首一律 "panel: task "：命中 ring.taskPrefixes → 归 task 频道，自动进
//     /panel/api/logs 与 task-logs.json，与对话流量互不挤占；
//   - 字段顺序固定，新字段只追加在 status 之后，既有 grep 与前端着色不受影响；
//   - 空值字段照打（claim_error=""、err=""）：前端 logLevel 会先抹掉「空赋值」
//     再判级别，所以成功行不会被 error 字样误标红。
package panel

import (
	"fmt"
	"log"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
)

// taskLogCapacity 任务频道保留条数（内存环与落盘文件是同一份）。
//
// 取值依据：每账号每天约 37 行（一轮成长 28 项 + 签到/旅行/夜猫子/连登等），
// 500 条只够约 13 个账号·天——20 个号时往往只剩当天最后一段；3000 条约合
// 81 账号·天（20 个号约 4 天）。再大只会让「整份重写」的单文件线性变大。
const taskLogCapacity = 3000

// ---------------------------------------------------------------------------
// 任务频道落盘去抖（Ring 只持有 taskPersist 指针，逻辑全在本文件）
// ---------------------------------------------------------------------------

// 落盘节奏：空闲 taskFlushIdle 才写盘（每次任务行写入都重置计时），持续写入时
// 最多拖 taskFlushMaxDelay 就强制写一次。
//
// 为什么要去抖：persistTasksLocked 是「整份重写」，一轮成长任务 28 项（项间还有
// 1.05s 节流）会触发 28 次全量 marshal+fsync+rename；合并后一轮只写 1~3 次，
// 落盘字节数同级但系统调用降一个数量级。代价：进程被 kill 时最多丢 2s 的任务
// 日志行（任务状态本身走 task-jobs.json，同步写，不受影响）。
//
// 用 var 而不是 const：单测可以把窗口缩到毫秒级验证去抖行为。
var (
	taskFlushIdle     = 2 * time.Second
	taskFlushMaxDelay = 10 * time.Second
)

// taskPersist 任务频道落盘去抖状态（由 Ring.persist 持有，访问需持 Ring.mu）。
type taskPersist struct {
	dirty  bool      // 有未落盘的行
	since  time.Time // 这批脏数据的起点（用于 taskFlushMaxDelay 兜底）
	timer  *time.Timer
	closed bool // 已 Close，此后不再落盘
}

// scheduleTaskPersist 标记有未落盘的任务行并重置空闲合并计时器。
// 调用方必须持有 r.mu（由 Ring.Write 调用）。
func (r *Ring) scheduleTaskPersist() {
	if r.taskArchive == "" {
		return // 未配置归档（测试/裸用）：保持旧语义，不攒状态
	}
	p := r.persist
	if p == nil {
		p = &taskPersist{}
		r.persist = p
	}
	if p.closed {
		return
	}
	now := time.Now()
	if p.since.IsZero() {
		p.since = now
	}
	p.dirty = true
	if now.Sub(p.since) >= taskFlushMaxDelay {
		// 持续写入导致计时器一直被重置时的兜底：攒够上限先落一次。
		r.persistTasksLocked()
		p.dirty = false
		p.since = time.Time{}
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(taskFlushIdle, r.flushTaskPersist)
}

// flushTaskPersist 空闲合并到期：把攒下的任务行整份落盘。
func (r *Ring) flushTaskPersist() {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.persist
	if p == nil {
		return
	}
	p.timer = nil
	if p.closed || !p.dirty {
		return
	}
	r.persistTasksLocked()
	p.dirty = false
	p.since = time.Time{}
}

// Flush 立即落盘（关停与单测用）。没有待写内容时是空操作。
func (r *Ring) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushTaskLocked()
}

// Close 停掉合并计时器并落盘；之后不再落盘（进程正在退出）。
func (r *Ring) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.persist; p != nil {
		p.closed = true
	}
	r.flushTaskLocked()
}

// flushTaskLocked 落盘待写内容并清状态（调用方持 r.mu）。
func (r *Ring) flushTaskLocked() {
	p := r.persist
	if p == nil || !p.dirty {
		return
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	r.persistTasksLocked()
	p.dirty = false
	p.since = time.Time{}
}

// ---------------------------------------------------------------------------
// 逐项任务（完成 / 跳过 / 领奖）
// ---------------------------------------------------------------------------

// taskAction 本轮实际做的事：run=跑了动作链路、claim=只领奖（动作早已达标）、
// none=没做任何事（早退 / 跳过 / 读错）。
//
// 只用 outcome 既有字段推导，不给 autoTaskOutcome 加字段（减少与主干的冲突面）：
//   - ActionRun 只在 act.run 真被调用后为 true；
//   - Claimable 只在 claimAutoTask 里置 true，而那条路径必然发起过一次领取。
func taskAction(out autoTaskOutcome) string {
	switch {
	case out.ActionRun:
		return "run"
	case out.Claimable:
		return "claim"
	default:
		return "none"
	}
}

// autoTaskLogLine 逐项任务日志的唯一格式。字段顺序固定，新字段只追加在 status 之后。
func autoTaskLogLine(uid, code string, out autoTaskOutcome, dur time.Duration) string {
	return fmt.Sprintf(
		"panel: task auto uid=%s code=%s status=%s action=%s progress=%s->%s claimed=%v reward=+%dc+%de dur=%s reason=%q claim_error=%q",
		uid, code, out.Status, taskAction(out),
		logfmt.Truncate(out.ProgressBefore, 64), logfmt.Truncate(out.ProgressAfter, 64),
		out.Claimed, out.Credit, out.Energy, dur.Round(100*time.Millisecond),
		logfmt.Truncate(out.Message, 160), logfmt.Truncate(out.ClaimError, 180))
}

// logTaskOutcome 打印一项任务的执行结果（executeAutoTask 的唯一出口调用）。
func (p *Panel) logTaskOutcome(uid, code string, out autoTaskOutcome, dur time.Duration) {
	log.Printf("%s", autoTaskLogLine(uid, code, out, dur))
}

// ---------------------------------------------------------------------------
// 积分领取（自动领奖 / 人工全部领取 / 补领清扫）
// ---------------------------------------------------------------------------

// claimLogLine 一次领取事实的日志：via 说明谁发起的，result=fail 时 err 非空。
func claimLogLine(uid, code, via, mode string, credit, energy int64, err error) string {
	result := "ok"
	errText := ""
	if err != nil {
		result = "fail"
		errText = err.Error()
	}
	return fmt.Sprintf("panel: task claim uid=%s code=%s via=%s mode=%s result=%s +%dc+%de err=%q",
		uid, code, via, mode, result, credit, energy, logfmt.Truncate(errText, 180))
}

// claimTaskLogged 领取并记审计日志；口径按任务码自动判定（与 claimTask 同口径）。
func (p *Panel) claimTaskLogged(a *auth.Auth, code, via string) (credit, energy int64, err error) {
	credit, energy, err = p.claimTask(a, code)
	p.logTaskClaim(a, code, via, credit, energy, err)
	return credit, energy, err
}

// claimTaskLoggedMP 同上，但口径由调用方指定——补领清扫对「只出现在 mp 列表」
// 的码也必须走 mp（Web 域会 400），claimTask 的自动判定覆盖不到这种情况。
func (p *Panel) claimTaskLoggedMP(a *auth.Auth, code, via string, mp bool) (credit, energy int64, err error) {
	if mp {
		credit, energy, err = p.cfg.Upstream.ClaimRewardMP(a, code)
	} else {
		credit, energy, err = p.cfg.Upstream.ClaimReward(a, code)
	}
	p.logTaskClaim(a, code, via, credit, energy, err)
	return credit, energy, err
}

// logTaskClaim 领取结果的唯一打印点（三条领取路径共用）。
func (p *Panel) logTaskClaim(a *auth.Auth, code, via string, credit, energy int64, err error) {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	log.Printf("%s", claimLogLine(uid, code, via, p.growthClaimModeValue(), credit, energy, err))
}

// ---------------------------------------------------------------------------
// 后台作业轮次（定时成长 / 一键完成全部）
// ---------------------------------------------------------------------------

// shortJobID 作业 ID 取前 8 位：日志里足够定位，不铺满一行。
func shortJobID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// taskJobStartLine 一轮后台作业开始（调用点在 taskjobs.go 的 run）。
func taskJobStartLine(uid, jobID string, job TaskJob) string {
	return fmt.Sprintf("panel: task job start uid=%s job=%s trigger=%s tasks=%d",
		uid, shortJobID(jobID), job.Trigger, job.Total)
}

func (p *Panel) logTaskJobStart(uid, jobID string, job TaskJob) {
	log.Printf("%s", taskJobStartLine(uid, jobID, job))
}

// taskJobDoneLine 一轮后台作业结束：结果分布 + 累计奖励 + 耗时（调用点在 finish）。
//
// 结果里的 credit/energy 在内存中是 int64，重启从 JSON 恢复后是 float64，两者都收。
func taskJobDoneLine(uid, jobID string, job TaskJob, message string) string {
	var done, skipped, waiting, claimFail, errored int
	var credit, energy int64
	for _, result := range job.Results {
		status, _ := result["status"].(string)
		switch status {
		case "done":
			done++
		case "skipped":
			skipped++
		case "awaiting_progress", "claim_manual":
			waiting++
		case "claim_pending":
			claimFail++
		case "error":
			errored++
		}
		credit += resultInt(result, "credit")
		energy += resultInt(result, "energy")
	}
	dur := time.Duration(0)
	if job.FinishedAt != nil {
		dur = job.FinishedAt.Sub(job.StartedAt)
	}
	return fmt.Sprintf("panel: task job done uid=%s job=%s status=%s done=%d skipped=%d waiting=%d claim_fail=%d error=%d +%dc+%de dur=%s msg=%q",
		uid, shortJobID(jobID), job.Status, done, skipped, waiting, claimFail, errored,
		credit, energy, dur.Round(time.Second), logfmt.Truncate(message, 160))
}

func (p *Panel) logTaskJobDone(uid, jobID string, job TaskJob, message string) {
	log.Printf("%s", taskJobDoneLine(uid, jobID, job, message))
}

// resultInt 从任务结果 map 里读整数字段（int64 内存值 / float64 JSON 值）。
func resultInt(result map[string]any, key string) int64 {
	switch v := result[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}
