// window.go 成员生效时间段的解析与判定。
//
// 时段语义按 CST 固定（复用 internal/cst），与宿主时区无关：容器跑在 UTC 时
// 「23:00-07:00」不能漂成 15:00-23:00。判定是纯整数比较；解析只在归一化阶段做一次，
// 请求路径不再碰 time.Parse。
package modelgroup

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
)

// Window 生效时间段。Start/End 是当日分钟数 [0,1440)。
// Start > End 表示跨天（如 23:00-07:00）；Start == End 表示全天。
type Window struct {
	Start int
	End   int
}

// ParseWindow 解析 "HH:MM-HH:MM"。
//
// 空串返回 (nil, nil) —— 语义是「未配置 = 全天生效」，不是错误。
// 格式非法同样返回 (nil, nil)：调用方按全天处理，不阻断配置保存
// （运维写错一个时段不该让整份配置存不进去）。
func ParseWindow(s string) (*Window, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	left, right, ok := strings.Cut(s, "-")
	if !ok {
		return nil, nil
	}
	start, ok := parseHHMM(left)
	if !ok {
		return nil, nil
	}
	end, ok := parseHHMM(right)
	if !ok {
		return nil, nil
	}
	return &Window{Start: start, End: end}, nil
}

// parseHHMM 把 "H:MM" / "HH:MM" 解析成当日分钟数。越界或非数字返回 ok=false。
func parseHHMM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	hh, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || hh < 0 || hh > 23 {
		return 0, false
	}
	mm, err := strconv.Atoi(strings.TrimSpace(m))
	if err != nil || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// In 报告 at 是否落在窗口内。nil receiver（未配置）恒为 true。
//
// 边界左闭右开（start <= now < end）：这是「相邻时段不重叠」的唯一保证。
// 跨天窗口（Start > End）拆成两段或条件：now >= Start || now < End。
func (w *Window) In(at time.Time) bool {
	if w == nil {
		return true
	}
	if w.Start == w.End {
		return true
	}
	local := at.In(cst.Loc)
	t := local.Hour()*60 + local.Minute()
	if w.Start > w.End {
		return t >= w.Start || t < w.End
	}
	return t >= w.Start && t < w.End
}

// String 便于日志与面板展示。跨天窗口加 " (跨天)" 后缀，
// 避免运维把它读成「07:00 到 23:00」。
func (w *Window) String() string {
	if w == nil {
		return "全天"
	}
	f := func(v int) string { return fmt.Sprintf("%02d:%02d", v/60, v%60) }
	if w.Start > w.End {
		return f(w.Start) + "-" + f(w.End) + " (跨天)"
	}
	return f(w.Start) + "-" + f(w.End)
}
