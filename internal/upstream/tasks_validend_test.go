package upstream

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
)

// TestParseTaskTimeRealUpstreamValues 用真实上游下发的值固化解析行为：
// 实测 2026-10-31 任务列表，valid_end 全部是 RFC3339 带 +08:00 偏移。
func TestParseTaskTimeRealUpstreamValues(t *testing.T) {
	// 真实样本（来自线上任务列表，多账号一致）。
	samples := []struct {
		raw  string
		want string // CST 下的期望值
	}{
		{"2026-11-13T23:59:00+08:00", "2026-11-13 23:59:00"},
		{"2026-11-02T23:59:00+08:00", "2026-11-02 23:59:00"},
		{"2026-11-15T23:59:00+08:00", "2026-11-15 23:59:00"},
		{"2026-11-20T23:59:00+08:00", "2026-11-20 23:59:00"},
		{"2026-08-14T15:13:00+08:00", "2026-08-14 15:13:00"},
		{"2026-09-07T00:00:00+08:00", "2026-09-07 00:00:00"},
		{"2026-10-19T11:46:00+08:00", "2026-10-19 11:46:00"},
	}
	for _, s := range samples {
		t.Run(s.raw, func(t *testing.T) {
			got, ok := parseTaskTime(s.raw)
			if !ok {
				t.Fatalf("真实值解析失败: %q", s.raw)
			}
			want, _ := time.ParseInLocation("2006-01-02 15:04:05", s.want, cst.Loc)
			if !got.Equal(want) {
				t.Fatalf("%q → %s want %s", s.raw, got.In(cst.Loc).Format("2006-01-02 15:04:05"), s.want)
			}
		})
	}
}

// TestParseGrowthTasksNullValidEnd 上游大部分任务的 valid_start/valid_end 是 JSON null，
// 解析不能报错，也不能把 null 当成 0 值时间。
func TestParseGrowthTasksNullValidEnd(t *testing.T) {
	raw := []byte(`{"tasks":[
		{"task_code":"chat_5","target":5,"current":0,"valid_start":null,"valid_end":null},
		{"task_code":"WorkBuddy_Valorant","target":1,"current":0,
		 "valid_start":"2026-10-19T11:46:00+08:00","valid_end":"2026-11-15T23:59:00+08:00"}
	]}`)
	tasks, err := parseGrowthTasks(raw)
	if err != nil {
		t.Fatalf("null 字段不应导致解析失败: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks want 2", len(tasks))
	}
	if tasks[0].ValidEnd != "" {
		t.Fatalf("null 应解为空串，实际 %q", tasks[0].ValidEnd)
	}
	if _, ok := tasks[0].ValidEndTime(); ok {
		t.Fatal("null 的 valid_end 必须报告取不到")
	}
	if end, ok := tasks[1].ValidEndTime(); !ok || end.In(cst.Loc).Format("2006-01-02 15:04:05") != "2026-11-15 23:59:00" {
		t.Fatalf("真实值解析: end=%v ok=%v", end, ok)
	}
}
