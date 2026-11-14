package panel

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 任务频道容量与 taskaudit.go 的常量一致：改容量只改一处。
func TestPanelTaskLogCapacity(t *testing.T) {
	if got := New(Config{}).logs.cap; got != taskLogCapacity {
		t.Fatalf("ring cap = %d, want %d", got, taskLogCapacity)
	}
}

// 新增的审计行必须仍归任务频道，否则面板「任务」筛选与 task-logs.json 都收不到。
func TestTaskAuditLinesGoToTaskChannel(t *testing.T) {
	lines := []string{
		"panel: task auto uid=x code=chat_5 status=done action=run",
		"panel: task claim uid=x code=chat_5 via=auto mode=auto result=ok +100c+5e err=\"\"",
		"panel: task job start uid=x job=01234567 trigger=daily tasks=28",
		"panel: task job done uid=x job=01234567 status=finished done=3",
	}
	for _, line := range lines {
		if got := classifyLine(line); got != ChTask {
			t.Errorf("classifyLine(%q) = %q, want %q", line, got, ChTask)
		}
	}
}

// 逐项任务行：完成 / 只领奖 / 空转三种情况必须能在日志里分辨。
func TestAutoTaskLogLine(t *testing.T) {
	cases := []struct {
		name string
		out  autoTaskOutcome
		dur  time.Duration
		want string
	}{
		{
			name: "claim_only",
			out: autoTaskOutcome{
				Status: "done", Message: "任务已达标；已自动领奖 +100 分 +5 能",
				ProgressBefore: "5/5", ProgressAfter: "5/5", Claimed: true, Claimable: true,
				Credit: 100, Energy: 5,
			},
			dur:  1200 * time.Millisecond,
			want: `panel: task auto uid=u1 code=chat_5 status=done action=claim progress=5/5->5/5 claimed=true reward=+100c+5e dur=1.2s reason="任务已达标；已自动领奖 +100 分 +5 能" claim_error=""`,
		},
		{
			name: "already_claimed",
			out: autoTaskOutcome{
				Status: "done", Message: "该任务已领取过奖励", Claimed: true,
				ProgressBefore: "5/5", ProgressAfter: "5/5",
			},
			want: `panel: task auto uid=u1 code=chat_5 status=done action=none progress=5/5->5/5 claimed=true reward=+0c+0e dur=0s reason="该任务已领取过奖励" claim_error=""`,
		},
		{
			name: "claim_failed",
			out: autoTaskOutcome{
				Status: "claim_pending", Message: "任务已达标；任务已达标，但领奖失败: task not completed",
				ProgressBefore: "5/5", ProgressAfter: "5/5", Claimable: true, ClaimError: "task not completed",
			},
			want: `panel: task auto uid=u1 code=chat_5 status=claim_pending action=claim progress=5/5->5/5 claimed=false reward=+0c+0e dur=0s reason="任务已达标；任务已达标，但领奖失败: task not completed" claim_error="task not completed"`,
		},
	}
	for _, c := range cases {
		if got := autoTaskLogLine("u1", "chat_5", c.out, c.dur); got != c.want {
			t.Fatalf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// taskAction 只用既有字段推导，不给 autoTaskOutcome 加字段（减少与主干冲突）。
func TestTaskActionDerivation(t *testing.T) {
	cases := map[string]struct {
		out  autoTaskOutcome
		want string
	}{
		"run":   {autoTaskOutcome{ActionRun: true, Claimable: true}, "run"},
		"claim": {autoTaskOutcome{Claimable: true}, "claim"},
		"none":  {autoTaskOutcome{Status: "skipped"}, "none"},
	}
	for name, c := range cases {
		if got := taskAction(c.out); got != c.want {
			t.Errorf("%s: taskAction = %q, want %q", name, got, c.want)
		}
	}
}

// 领取行：成功与失败都要留下金额与原因（result=fail 由前端标红）。
func TestClaimLogLine(t *testing.T) {
	ok := claimLogLine("u1", "chat_5", "auto", "auto", 100, 5, nil)
	wantOK := `panel: task claim uid=u1 code=chat_5 via=auto mode=auto result=ok +100c+5e err=""`
	if ok != wantOK {
		t.Fatalf("ok line:\n got %s\nwant %s", ok, wantOK)
	}
	fail := claimLogLine("u1", "chat_5", "sweep", "manual", 0, 0, errors.New("task not completed"))
	wantFail := `panel: task claim uid=u1 code=chat_5 via=sweep mode=manual result=fail +0c+0e err="task not completed"`
	if fail != wantFail {
		t.Fatalf("fail line:\n got %s\nwant %s", fail, wantFail)
	}
}

// 作业边界行：结果分布按状态归类，金额兼容 int64（内存）与 float64（JSON 恢复）。
func TestTaskJobLogLines(t *testing.T) {
	start := time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC)
	job := TaskJob{
		ID:        "0123456789abcdef",
		Trigger:   "daily",
		Status:    "finished",
		StartedAt: start,
		Total:     28,
		Results: []map[string]any{
			{"status": "done", "credit": int64(100), "energy": int64(5)},
			{"status": "done", "credit": float64(50), "energy": float64(1)},
			{"status": "claim_pending", "claim_error": "task not completed"},
			{"status": "awaiting_progress"},
			{"status": "skipped"},
			{"status": "error", "message": "执行失败: boom"},
		},
	}
	if got, want := taskJobStartLine("u1", job.ID, job), "panel: task job start uid=u1 job=01234567 trigger=daily tasks=28"; got != want {
		t.Fatalf("start line:\n got %s\nwant %s", got, want)
	}
	fin := start.Add(90 * time.Second)
	job.FinishedAt = &fin
	got := taskJobDoneLine("u1", job.ID, job, "全部可自动任务已处理")
	want := `panel: task job done uid=u1 job=01234567 status=finished done=2 skipped=1 waiting=1 claim_fail=1 error=1 +150c+6e dur=1m30s msg="全部可自动任务已处理"`
	if got != want {
		t.Fatalf("done line:\n got %s\nwant %s", got, want)
	}
}

// 落盘去抖：写入后不会立刻重写文件，空闲窗口过去才落一次。
func TestRingTaskPersistDebounced(t *testing.T) {
	oldIdle, oldMax := taskFlushIdle, taskFlushMaxDelay
	taskFlushIdle, taskFlushMaxDelay = 30*time.Millisecond, time.Second
	t.Cleanup(func() { taskFlushIdle, taskFlushMaxDelay = oldIdle, oldMax })

	archive := filepath.Join(t.TempDir(), "task-logs.json")
	r := NewRing(10)
	if err := r.SetTaskArchive(archive); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	if _, err := r.Write([]byte("panel: task auto uid=x code=chat_5 status=done\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("archive written inside the debounce window: err=%v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(archive); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	restored := NewRing(10)
	if err := restored.SetTaskArchive(archive); err != nil {
		t.Fatal(err)
	}
	if got := len(restored.Snapshot()); got != 1 {
		t.Fatalf("restored entries = %d, want 1", got)
	}
}

// 关停必须把去抖窗口内的尾行写下去，否则「任务完成」这类关键行会丢。
func TestRingCloseFlushesPending(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "task-logs.json")
	r := NewRing(10)
	if err := r.SetTaskArchive(archive); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("panel: task claim uid=x code=chat_5 via=auto mode=auto result=ok\n")); err != nil {
		t.Fatal(err)
	}
	r.Close()
	restored := NewRing(10)
	if err := restored.SetTaskArchive(archive); err != nil {
		t.Fatal(err)
	}
	if got := len(restored.Snapshot()); got != 1 {
		t.Fatalf("restored entries = %d, want 1", got)
	}
}
