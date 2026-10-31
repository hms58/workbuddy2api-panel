package upstream

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
)

// TestParseTaskTime 上游任务活动时间的三种已知形态都要能解析；
// 取不到一律返回 false（调用方据此放弃自动决策，安全侧）。
func TestParseTaskTime(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // CST 格式化后的期望值，空 = 期望解析失败
	}{
		{"空格分隔本地时间", "2026-11-20 23:59:59", "2026-11-20 23:59:59"},
		{"RFC3339 带偏移", "2026-11-20T23:59:59+08:00", "2026-11-20 23:59:59"},
		{"epoch 秒", "1795000000", "2026-11-18 19:06:40"},
		{"epoch 毫秒", "1795000000000", "2026-11-18 19:06:40"},
		{"空串", "", ""},
		{"只有空白", "   ", ""},
		{"非法文本", "not-a-time", ""},
		{"日期缺时间", "2026-11-20", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseTaskTime(c.raw)
			if c.want == "" {
				if ok {
					t.Fatalf("parseTaskTime(%q) 应失败，实际得到 %v", c.raw, got)
				}
				return
			}
			if !ok {
				t.Fatalf("parseTaskTime(%q) 解析失败", c.raw)
			}
			if want, _ := time.ParseInLocation("2006-01-02 15:04:05", c.want, cst.Loc); !got.Equal(want) {
				t.Fatalf("parseTaskTime(%q)=%v want %v", c.raw, got.In(cst.Loc), want)
			}
		})
	}
}

// TestParseGrowthTasksValidEnd 任务列表解析必须带出 valid_start/valid_end：
// 它们是「活动快结束才自动领」的唯一判据来源。
func TestParseGrowthTasksValidEnd(t *testing.T) {
	tasks, err := parseGrowthTasks(json.RawMessage(`{"tasks":[
		{"task_code":"WorkBuddy_Valorant","target":1,"current":0,
		 "valid_start":"2026-11-01 00:00:00","valid_end":"2026-11-15 23:59:59"},
		{"task_code":"chat_5","target":5,"current":0}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].ValidStart != "2026-11-01 00:00:00" || tasks[0].ValidEnd != "2026-11-15 23:59:59" {
		t.Fatalf("valid_start/valid_end 未带出: %+v", tasks[0])
	}
	if end, ok := tasks[0].ValidEndTime(); !ok || end.In(cst.Loc).Format("2006-01-02 15:04:05") != "2026-11-15 23:59:59" {
		t.Fatalf("ValidEndTime=%v ok=%v", end, ok)
	}
	// 字段缺失：不得当成 0 值时间，必须明确报告取不到。
	if tasks[1].ValidEnd != "" {
		t.Fatalf("chat_5 不该有 valid_end，实际 %q", tasks[1].ValidEnd)
	}
	if _, ok := tasks[1].ValidEndTime(); ok {
		t.Fatal("字段缺失时 ValidEndTime 必须返回 false")
	}
}
