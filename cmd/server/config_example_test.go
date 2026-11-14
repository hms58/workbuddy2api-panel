package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/modelgroup"
)

// TestConfigExampleIsValid 守卫仓库根目录的 config.example.json 是合法 JSON、
// 且能被 Config 结构解析（键名拼错/漏逗号都不会被编译期发现）。
//
// 来由：PR #55 加 features.sanitize_brand_scope 时漏了上一行的逗号，
// 文件变成非法 JSON 而 CI 全绿——因为它只在运行时被 `cp config.example.json
// config.json` 的人读到，报错现场离提交很远。这个测试把现场挪回 CI。
func TestConfigExampleIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}
	if !json.Valid(raw) {
		t.Fatalf("%s 不是合法 JSON", path)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%s 无法解析为 Config: %v", path, err)
	}
	// 新增的对外配置项必须在示例里可见，否则用户只能从 README 反推。
	if c.Features.SanitizeBrandScope != "all" {
		t.Errorf("features.sanitize_brand_scope 示例值应为 all，得到 %q", c.Features.SanitizeBrandScope)
	}
	if !c.Features.SanitizeBlacklistFingerprints {
		t.Error("features.sanitize_blacklist_fingerprints 示例值应为 true")
	}
	if c.Pool.ModelFallback == nil {
		t.Error("pool.model_fallback 示例应存在（空对象即可），否则用户不知道有这张表")
	}
	// models.allowed（issue #68）：示例必须是存在的数组（仿上方各项的「示例可见」
	// 口径）。空数组解析为非 nil 空切片；nil = 键缺失，用户无从发现白名单配置。
	if c.Models.Allowed == nil {
		t.Error("models.allowed 示例应存在（空数组即可），否则用户不知道有白名单开关")
	}
	// 成长任务领奖模式：示例必须可见，否则用户不知道可以「完成不领」。
	if c.Schedule.GrowthClaimMode != "manual" {
		t.Errorf("schedule.growth_claim_mode 示例值应为 manual，得到 %q", c.Schedule.GrowthClaimMode)
	}
}

// TestConfigExampleHasModelGroups 示例配置必须带上 model_groups 段：
// 空数组即可，否则用户不知道有这个功能（与 pool.model_fallback 同口径）。
func TestConfigExampleHasModelGroups(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("示例配置解析失败: %v", err)
	}
	if c.Pool.ModelGroups == nil {
		t.Error("pool.model_groups 示例应存在（空数组即可）")
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.ModelGroupOpts.SniffInterval != 60*time.Second {
		t.Errorf("缺省 sniff_interval = %v, want 60s", c.ModelGroupOpts.SniffInterval)
	}
	if c.ModelGroupOpts.SniffMinGap != 10*time.Minute {
		t.Errorf("缺省 sniff_min_gap = %v, want 10m", c.ModelGroupOpts.SniffMinGap)
	}
}

// TestConfigModelGroupsNormalize 归一化：非法时段不阻断保存，非法阈值报错。
func TestConfigModelGroupsNormalize(t *testing.T) {
	c := Default()
	c.Pool.ModelGroups = []modelgroup.GroupConfig{{
		ID: "G1", Enabled: true,
		Members: []modelgroup.MemberConfig{
			{Model: "cn:a", Weight: 100},
			{Model: "cn:b", Weight: 90, Window: "不是时段"},
		},
	}}
	c.Pool.ModelGroup.SniffInterval = "30s"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.ModelGroupOpts.SniffInterval != 30*time.Second {
		t.Errorf("SniffInterval = %v, want 30s", c.ModelGroupOpts.SniffInterval)
	}
	groups, _ := modelgroup.Compile(c.Pool.ModelGroups, c.ModelGroupOpts)
	if len(groups) != 1 || groups[0].ID != "g1" {
		t.Fatalf("组 id 应小写，实际 %+v", groups)
	}
	if groups[0].Members[1].Window != nil {
		t.Errorf("非法时段应回落全天（nil），实际 %+v", groups[0].Members[1].Window)
	}

	// 非法阈值必须报错（拼写错误静默吞掉等于把探活周期变成 0）
	c2 := Default()
	c2.Pool.ModelGroup.SniffInterval = "abc"
	if err := c2.normalize(); err == nil {
		t.Error("非法 sniff_interval 应报错")
	}
}

// TestConfigModelGroupSwitchDefaults 两个开关缺省 true（装好就生效）。
func TestConfigModelGroupSwitchDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.ModelGroupOpts.PreferLastOK {
		t.Error("prefer_last_ok 缺省应为 true")
	}
	if !c.ModelGroupOpts.SniffEnabled {
		t.Error("sniff_enabled 缺省应为 true")
	}

	f := false
	c2 := Default()
	c2.Pool.ModelGroup.SniffEnabled = &f
	if err := c2.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c2.ModelGroupOpts.SniffEnabled {
		t.Error("显式 false 应生效")
	}
}
