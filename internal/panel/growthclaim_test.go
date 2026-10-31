package panel

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// taskWithValidEnd 造一个已达标的任务；raw 非空时填入上游原样下发的 valid_end。
func taskWithValidEnd(raw string) *upstream.Task {
	return &upstream.Task{
		TaskCode: "chat_5", Target: 1, Current: 1, Claimable: true, ValidEnd: raw,
	}
}

// TestAutoClaimAllowed 自动领奖闸门：auto 恒放行；manual 只在活动 24 小时内截止时放行，
// 截止时间取不到一律保留人工领取（不猜、不误领）。
func TestAutoClaimAllowed(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, cst.Loc)
	within := now.Add(3 * time.Hour).In(cst.Loc).Format("2006-01-02 15:04:05")
	exact := now.Add(claimExpiringWindow).In(cst.Loc).Format("2006-01-02 15:04:05")
	beyond := now.Add(48 * time.Hour).In(cst.Loc).Format("2006-01-02 15:04:05")
	expired := now.Add(-time.Hour).In(cst.Loc).Format("2006-01-02 15:04:05")

	cases := []struct {
		name string
		mode string
		end  string
		want bool
	}{
		{"auto 模式无截止字段也领", "auto", "", true},
		{"auto 模式远期活动也领", "auto", beyond, true},
		{"manual 模式无截止字段不领", "manual", "", false},
		{"manual 模式剩余 3 小时自动领", "manual", within, true},
		{"manual 模式恰好 24 小时自动领", "manual", exact, true},
		{"manual 模式剩余 2 天不领", "manual", beyond, false},
		{"manual 模式已过期不领", "manual", expired, false},
		{"未设置模式回落 manual", "", within, true},
		{"未知模式值回落 manual", "whatever", beyond, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(Config{})
			if c.mode != "" {
				p.SetGrowthClaimMode(c.mode)
			}
			got, reason := p.autoClaimAllowedAt(taskWithValidEnd(c.end), now)
			if got != c.want {
				t.Fatalf("autoClaimAllowed=%v want %v（reason=%q）", got, c.want, reason)
			}
			if !got && reason == "" {
				t.Fatal("拒绝自动领取必须给出原因，否则面板无从解释")
			}
		})
	}
}

// TestGrowthClaimModeDefaultIsManual 缺省即手动领取：默认 auto 会让本次优化对所有人无效。
func TestGrowthClaimModeDefaultIsManual(t *testing.T) {
	if got := New(Config{}).growthClaimModeValue(); got != claimModeManual {
		t.Fatalf("默认模式=%q want %q", got, claimModeManual)
	}
	// 非法/空值一律回落 manual，不允许静默变成 auto。
	for _, raw := range []string{"", "whatever", "AUTO ", "1"} {
		p := New(Config{})
		p.SetGrowthClaimMode(raw)
		if got := p.growthClaimModeValue(); got != claimModeManual {
			t.Fatalf("SetGrowthClaimMode(%q) → %q，want manual", raw, got)
		}
	}
	p := New(Config{})
	p.SetGrowthClaimMode("auto")
	if got := p.growthClaimModeValue(); got != claimModeAuto {
		t.Fatalf("auto 未被接受，got %q", got)
	}
}
