package panel

// 批量轮次任务列表缓存（growthclaim.go）的生命周期用例：
// 预取（ListALLTasks）→ 轮内 ListTasks/ListTasksMP/taskByCode 全部命中缓存
// （零上游请求）→ 计分回读走 *Live 实时口径 → 轮末清除恢复直连。

import (
	"context"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

func TestListALLTasksPopulatesBatchCache(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 5, 5, "claimed")

	merged, err := p.ListALLTasks(context.Background(), a)
	if err != nil {
		t.Fatalf("ListALLTasks: %v", err)
	}
	if len(merged) == 0 || merged[0].TaskCode != "chat_5" {
		t.Fatalf("merged = %+v, want chat_5", merged)
	}
	listCalls, _, _ := state.counters()
	if listCalls != 2 {
		t.Fatalf("预取应恰好发 web+mp 两次列表请求, got %d", listCalls)
	}

	// 轮内读取全部命中缓存：不再产生上游请求。
	if _, err := p.ListTasks(a); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if _, err := p.ListTasksMP(a); err != nil {
		t.Fatalf("ListTasksMP: %v", err)
	}
	if got, err := p.taskByCode(a, "chat_5"); err != nil || got == nil {
		t.Fatalf("taskByCode = %+v, %v; want cached hit", got, err)
	}
	if got, err := p.taskByCode(a, "not_in_list"); err != nil || got != nil {
		t.Fatalf("缓存内落空应返回 nil/nil, got %+v, %v", got, err)
	}
	if calls, _, _ := state.counters(); calls != 2 {
		t.Fatalf("轮内读取不应回源, list calls = %d, want 2", calls)
	}

	// 计分回读走实时口径：吃缓存会导致达标判定永远等不到。
	if _, err := p.taskByCodeLive(a, "chat_5"); err != nil {
		t.Fatalf("taskByCodeLive: %v", err)
	}
	if calls, _, _ := state.counters(); calls != 3 {
		t.Fatalf("Live 回读应直连上游, list calls = %d, want 3", calls)
	}

	// 轮末清除：恢复直连。
	p.clearAccountTaskList(a.UID)
	if _, ok := p.cachedTaskLists(a.UID); ok {
		t.Fatal("清除后不应再有缓存条目")
	}
	if _, err := p.ListTasks(a); err != nil {
		t.Fatalf("清除后 ListTasks 应直连上游: %v", err)
	}
	if calls, _, _ := state.counters(); calls != 4 {
		t.Fatalf("清除后 ListTasks 应回源, list calls = %d, want 4", calls)
	}
}

func TestRunAutoAllObservedReusesBatchCache(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 5, 5, "claimed")
	results := p.runAutoAllObserved(context.Background(), a, nil)

	// 全程仅阶段 0/0b 的两次列表拉取（兼作缓存预取）：25 个待办的初始读全部
	// 命中缓存——已领取的跳过、落空的按「没有此任务」跳过，都不再打列表接口。
	listCalls, _, _ := state.counters()
	if listCalls != 2 {
		t.Fatalf("list calls = %d, want 2（仅 web+mp 两次）", listCalls)
	}
	var chat5 map[string]any
	for _, r := range results {
		if code, _ := r["task_code"].(string); code == "chat_5" {
			chat5 = r
		}
	}
	if chat5 == nil || chat5["status"] != "done" {
		t.Fatalf("chat_5 result = %+v, want done", chat5)
	}
	// 轮末清除：缓存不残留到下一轮。
	if _, ok := p.cachedTaskLists(a.UID); ok {
		t.Fatal("批量结束后缓存应已清除")
	}
}

func TestTaskListCacheNotSharedAcrossAccounts(t *testing.T) {
	p, a, _ := newAutoTaskTestPanel(t, 5, 5, "claimed")
	if _, err := p.ListALLTasks(context.Background(), a); err != nil {
		t.Fatalf("ListALLTasks: %v", err)
	}
	other := &auth.Auth{UID: "another-account", AccessToken: "token"}
	if _, ok := p.cachedTaskLists(other.UID); ok {
		t.Fatal("缓存按账号隔离，他号不应命中")
	}
	// 他号读取回落实时拉取，不受本账号缓存影响。
	if _, err := p.ListTasks(other); err != nil {
		t.Fatalf("他号 ListTasks 应直连上游: %v", err)
	}
	var _ []upstream.Task // 保持 import（upstream 在 taskListWrap 定义处使用）
}
