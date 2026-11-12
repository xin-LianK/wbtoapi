// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package main

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
)

// TestKeyUsagesPicksTightest 每个密钥取最紧的一项限额：日限比的是 Today，
// 总量限比的是累计 Used（不是 Today）——两者混用会让"总量快用完了"永远报不出来。
func TestKeyUsagesPicksTightest(t *testing.T) {
	list := []apikeys.Key{
		{Name: "a", Today: apikeys.Usage{Requests: 80}, DailyLimit: apikeys.Limits{Requests: 100}},
		// 日限只用 1%（10/1000），但累计已 18/20：最紧的是总量限 90%。
		{Name: "b", Today: apikeys.Usage{Tokens: 10}, Used: apikeys.Usage{Tokens: 18},
			DailyLimit: apikeys.Limits{Tokens: 1000}, TotalLimit: apikeys.Limits{Tokens: 20}},
		{Name: "无限额"},
	}
	got := keyUsages(list)
	if len(got) != 2 {
		t.Fatalf("应只有 2 个参与: %+v", got)
	}
	// 顺序由 alerts 侧统一排序，这里只校验取值。
	for _, u := range got {
		switch u.Name {
		case "a":
			if u.Percent < 0.79 || u.Percent > 0.81 {
				t.Errorf("a 应≈80%%（今日请求 80/100）: %+v", u)
			}
		case "b":
			if u.Percent < 0.89 || u.Percent > 0.91 {
				t.Errorf("b 应≈90%%（累计 token 18/20，而不是日限的 1%%）: %+v", u)
			}
		default:
			t.Fatalf("不该出现 %q", u.Name)
		}
	}
}

// TestAlertsConfigFrom 配置换算：百分数除 100、间隔按分钟、notify 关闭时整体关闭。
func TestAlertsConfigFrom(t *testing.T) {
	var c Config
	c.Notify.Enabled = true
	c.Notify.Alerts.Enabled = true
	c.Notify.Alerts.IntervalMinutes = 7
	c.Notify.Alerts.MinHealthyAccounts = 2
	c.Notify.Alerts.MinTotalCredits = 5000
	c.Notify.Alerts.KeyQuotaPercent = 80
	c.Notify.Alerts.ProxyExpiryDays = 10

	got := alertsConfigFrom(&c)
	if !got.Enabled {
		t.Error("应启用")
	}
	if got.Interval != 7*time.Minute {
		t.Errorf("interval=%v", got.Interval)
	}
	if got.KeyPercent != 0.8 {
		t.Errorf("KeyPercent=%v want 0.8", got.KeyPercent)
	}
	if got.MinHealthy != 2 || got.MinCredits != 5000 || got.ProxyExpiryDays != 10 {
		t.Errorf("阈值换算错误: %+v", got)
	}

	// notify 总开关关掉时巡检也停：没有出口，巡检只剩无用功。
	c.Notify.Enabled = false
	if alertsConfigFrom(&c).Enabled {
		t.Error("notify 关闭时不该巡检")
	}
}
