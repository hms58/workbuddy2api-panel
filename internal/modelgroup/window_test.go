package modelgroup

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
)

// at 构造 CST 时区的指定时刻（日期无关，只关心钟点）。
func at(h, m int) time.Time {
	return time.Date(2026, 10, 25, h, m, 0, 0, cst.Loc)
}

// TestParseWindow 覆盖合法/非法/空三类输入。
// 空串与非法值都返回 (nil, nil)：调用方据此回落「全天生效」，不阻断配置保存。
func TestParseWindow(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantNil bool
		start   int
		end     int
	}{
		{"空串=未配置", "", true, 0, 0},
		{"白天区间", "09:00-23:00", false, 9 * 60, 23 * 60},
		{"跨天区间", "23:00-07:00", false, 23 * 60, 7 * 60},
		{"两端相同=全天", "00:00-00:00", false, 0, 0},
		{"单位数小时", "9:00-23:00", false, 9 * 60, 23 * 60},
		{"带空格", " 09:00 - 23:00 ", false, 9 * 60, 23 * 60},
		{"缺结束", "09:00", true, 0, 0},
		{"小时越界", "25:00-07:00", true, 0, 0},
		{"分钟越界", "09:60-23:00", true, 0, 0},
		{"非数字", "ab:cd-ef:gh", true, 0, 0},
		{"缺前导零但合法", "0:00-7:05", false, 0, 7*60 + 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, err := ParseWindow(c.in)
			if err != nil {
				t.Fatalf("ParseWindow(%q) unexpected err: %v", c.in, err)
			}
			if c.wantNil {
				if w != nil {
					t.Fatalf("ParseWindow(%q) = %+v, want nil", c.in, w)
				}
				return
			}
			if w == nil {
				t.Fatalf("ParseWindow(%q) = nil, want window", c.in)
			}
			if w.Start != c.start || w.End != c.end {
				t.Errorf("ParseWindow(%q) = {Start:%d End:%d}, want {Start:%d End:%d}",
					c.in, w.Start, w.End, c.start, c.end)
			}
		})
	}
}

// TestInWindowBoundaries 边界一律左闭右开：start <= now < end。
// 这条是「相邻时段不重叠」的唯一保证，错一小时就会在错误时段选中模型。
func TestInWindowBoundaries(t *testing.T) {
	cases := []struct {
		name string
		win  string
		h, m int
		want bool
	}{
		{"nil=全天", "", 13, 0, true},
		{"白天 起点前", "09:00-23:00", 8, 59, false},
		{"白天 起点含", "09:00-23:00", 9, 0, true},
		{"白天 区间内", "09:00-23:00", 15, 30, true},
		{"白天 终点前", "09:00-23:00", 22, 59, true},
		{"白天 终点不含", "09:00-23:00", 23, 0, false},
		{"跨天 起点含", "23:00-07:00", 23, 0, true},
		{"跨天 深夜", "23:00-07:00", 2, 0, true},
		{"跨天 终点前", "23:00-07:00", 6, 59, true},
		{"跨天 终点不含", "23:00-07:00", 7, 0, false},
		{"跨天 正午", "23:00-07:00", 12, 0, false},
		{"跨天 起点前", "23:00-07:00", 22, 59, false},
		{"两端相同=全天", "00:00-00:00", 13, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, err := ParseWindow(c.win)
			if err != nil {
				t.Fatalf("ParseWindow(%q): %v", c.win, err)
			}
			if got := w.In(at(c.h, c.m)); got != c.want {
				t.Errorf("window %q at %02d:%02d = %v, want %v", c.win, c.h, c.m, got, c.want)
			}
		})
	}
}

// TestInWindowUsesCSTNotHostZone 时段按 CST 判定，与宿主时区无关：
// 同一个绝对时刻，在 UTC 宿主上算出来的钟点也必须等于 CST 钟点。
func TestInWindowUsesCSTNotHostZone(t *testing.T) {
	w, err := ParseWindow("09:00-17:00")
	if err != nil {
		t.Fatal(err)
	}
	// CST 2026-10-25 10:00 == UTC 2026-10-25 02:00
	utcMoment := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)
	if !w.In(utcMoment) {
		t.Errorf("CST 10:00 应在 09:00-17:00 内（若按 UTC 02:00 判会误判为不在）")
	}
}
