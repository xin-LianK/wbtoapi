// stop_on_ban_config_test.go pool.stop_on_ban 配置测试：缺省 true（账号被封禁即
// 终止本次请求的轮转），显式 false 才退回旧行为（禁用当事号后继续换号）。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStopOnBanDefault 键缺席 → 缺省 true：老配置无需补键即获得「封号即停轮转」保护。
func TestStopOnBanDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Pool.StopOnBan {
		t.Error("stop_on_ban 缺省必须为 true（防同一请求把整池号逐个打成封禁）")
	}
}

// TestStopOnBanExplicitOff 显式 false = 逃生门（保留「禁用当事号后继续换号」的旧行为）。
func TestStopOnBanExplicitOff(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"stop_on_ban":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.StopOnBan {
		t.Error("stop_on_ban=false 未生效")
	}
}
