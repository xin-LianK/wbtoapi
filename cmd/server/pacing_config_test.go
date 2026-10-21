package main

import (
	"strings"
	"testing"
	"time"
)

// TestPacingDefaults 缺省开启温和档：间隔 120ms / 抖动 180ms / 90 次每分钟。
// 锁定默认值是因为它是「升级后行为变化」的唯一可见处——改它等于改所有部署的
// 出站节奏，必须显式。
func TestPacingDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Pacing.Enabled {
		t.Fatal("pacing 缺省应开启")
	}
	if c.Pacing.MinIntervalMS != 120 || c.Pacing.JitterMS != 180 || c.Pacing.PerMinute != 90 {
		t.Fatalf("默认参数漂移: interval=%d jitter=%d per_minute=%d",
			c.Pacing.MinIntervalMS, c.Pacing.JitterMS, c.Pacing.PerMinute)
	}
	if c.PacingCfg.MinInterval != 120*time.Millisecond {
		t.Fatalf("解析后 MinInterval=%v want 120ms", c.PacingCfg.MinInterval)
	}
	if c.PacingCfg.Jitter != 180*time.Millisecond {
		t.Fatalf("解析后 Jitter=%v want 180ms", c.PacingCfg.Jitter)
	}
	if c.PacingCfg.PerMinute != 90 {
		t.Fatalf("解析后 PerMinute=%d want 90", c.PacingCfg.PerMinute)
	}
}

// TestLegacyConfigKeepsPacingDefaults 老配置文件（完全没有 pacing 键）升级后
// 必须拿到默认档——键缺席不覆盖 Default()，这是「安全默认」的前提。
func TestLegacyConfigKeepsPacingDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(`{"listen":":7863","api_key":"k"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.PacingCfg.Disabled() {
		t.Fatal("老配置升级后 pacing 不应是关闭态")
	}
	if c.PacingCfg.MinInterval != 120*time.Millisecond {
		t.Fatalf("老配置应沿用默认间隔，got %v", c.PacingCfg.MinInterval)
	}
}

// TestPacingDisabledBySwitch 总开关关闭时三档参数一律不生效（即使填了值）。
func TestPacingDisabledBySwitch(t *testing.T) {
	c, err := ParseConfig([]byte(`{"pacing":{"enabled":false,"min_interval_ms":500,"jitter_ms":500,"per_minute":10}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !c.PacingCfg.Disabled() {
		t.Fatalf("enabled=false 时应为全关，got %+v", c.PacingCfg)
	}
}

// TestPacingCustom 显式配置按值解析；0 表示该档不限制。
func TestPacingCustom(t *testing.T) {
	c, err := ParseConfig([]byte(`{"pacing":{"enabled":true,"min_interval_ms":50,"jitter_ms":0,"per_minute":0}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.PacingCfg.MinInterval != 50*time.Millisecond {
		t.Fatalf("MinInterval=%v want 50ms", c.PacingCfg.MinInterval)
	}
	if c.PacingCfg.Jitter != 0 {
		t.Fatalf("Jitter 应为 0（不加抖动），got %v", c.PacingCfg.Jitter)
	}
	if c.PacingCfg.PerMinute != 0 {
		t.Fatalf("PerMinute 应为 0（不限），got %d", c.PacingCfg.PerMinute)
	}
	if c.PacingCfg.Disabled() {
		t.Fatal("MinInterval=50ms 不应被判定为全关")
	}
}

// TestPacingNegativeRejected 负值无语义且静默钳 0 会把保护悄悄关掉 → fail fast。
func TestPacingNegativeRejected(t *testing.T) {
	cases := map[string]string{
		"min_interval_ms": `{"pacing":{"min_interval_ms":-1}}`,
		"jitter_ms":       `{"pacing":{"jitter_ms":-1}}`,
		"per_minute":      `{"pacing":{"per_minute":-1}}`,
	}
	for field, raw := range cases {
		_, err := ParseConfig([]byte(raw))
		if err == nil {
			t.Fatalf("%s 负值应报错", field)
		}
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("错误信息应含字段名 %s，got %v", field, err)
		}
	}
}

// TestPacingEnvOverride 环境变量可覆盖（部署时免改配置文件）。
func TestPacingEnvOverride(t *testing.T) {
	t.Setenv("WB2A_PACE_ENABLED", "false")
	t.Setenv("WB2A_PACE_MIN_INTERVAL_MS", "250")
	t.Setenv("WB2A_PACE_JITTER_MS", "750")
	t.Setenv("WB2A_PACE_PER_MINUTE", "30")

	c := Default()
	applyEnv(c)
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.PacingCfg.Disabled() {
		t.Fatalf("WB2A_PACE_ENABLED=false 应关停，got %+v", c.PacingCfg)
	}

	// 再开回来验证数值覆盖生效。
	c2 := Default()
	t.Setenv("WB2A_PACE_ENABLED", "true")
	applyEnv(c2)
	if err := c2.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c2.PacingCfg.MinInterval != 250*time.Millisecond ||
		c2.PacingCfg.Jitter != 750*time.Millisecond ||
		c2.PacingCfg.PerMinute != 30 {
		t.Fatalf("环境变量未生效: %+v", c2.PacingCfg)
	}
}
