// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// alerts.go 阈值告警巡检的装配：把号池、客户端密钥、代理池的读数接成
// alerts.Sources，让 internal/alerts 不依赖这些包（也就不用为单测造代理池）。
//
// 四个读数与面板「总览」同口径：可用号数取 CountsDetailedWithPaused 的 healthy，
// 积分只算未禁用的号（禁用号的积分用不上，算进去会让人以为还够）。
package main

import (
	"os"
	"strconv"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alerts"
	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/notify"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/proxies"
)

// alertsConfigFrom 把 config.json 的 notify.alerts 段翻成巡检配置。
func alertsConfigFrom(c *Config) alerts.Config {
	a := c.Notify.Alerts
	return alerts.Config{
		Enabled:         c.Notify.Enabled && a.Enabled,
		Interval:        time.Duration(a.IntervalMinutes) * time.Minute,
		MinHealthy:      a.MinHealthyAccounts,
		MinCredits:      a.MinTotalCredits,
		KeyPercent:      a.KeyQuotaPercent / 100,
		ProxyExpiryDays: a.ProxyExpiryDays,
	}
}

// newAlertChecker 装配巡检器（不启动；由 run 在 ctx 就绪后 go Run）。
func newAlertChecker(cfg *Config, p *pool.Pool, keys *apikeys.Store, reg *proxies.Registry, n *notify.Notifier) *alerts.Checker {
	return alerts.New(alertsConfigFrom(cfg), alerts.Sources{
		Notify: n,
		Accounts: func() (total, healthy int) {
			total, healthy, _, _, _, _, _ = p.CountsDetailedWithPaused()
			return total, healthy
		},
		Credits: func() (int64, int) {
			var sum int64
			var n int
			for _, s := range p.List() {
				if s.Disabled {
					continue
				}
				sum += s.Credits
				n++
			}
			return sum, n
		},
		Keys: func(now time.Time) []alerts.KeyUsage {
			return keyUsages(keys.List(now))
		},
		Proxies: func(now time.Time, days int) (expiring []string, expired int) {
			if reg == nil {
				return nil, 0
			}
			deadline := now.AddDate(0, 0, days)
			for _, px := range reg.View().Proxies {
				if px.ExpiresAt == nil {
					continue
				}
				switch {
				case !now.Before(*px.ExpiresAt):
					expired++
				case !deadline.Before(*px.ExpiresAt):
					expiring = append(expiring, px.Name)
				}
			}
			return expiring, expired
		},
	})
}

// keyUsages 取每个密钥最紧的那项限额占比（日限与总量限 × 请求/token/积分）。
// 没配任何限额的密钥不参与（永远 0%）。
func keyUsages(list []apikeys.Key) []alerts.KeyUsage {
	out := make([]alerts.KeyUsage, 0, len(list))
	for _, k := range list {
		best := 0.0
		detail := ""
		consider := func(used, limit int64, label string) {
			if limit <= 0 {
				return
			}
			if p := float64(used) / float64(limit); p > best {
				best = p
				detail = label + " " + strconv.FormatInt(used, 10) + "/" + strconv.FormatInt(limit, 10)
			}
		}
		consider(k.Today.Requests, k.DailyLimit.Requests, "今日请求")
		consider(k.Today.Tokens, k.DailyLimit.Tokens, "今日 token")
		consider(int64(k.Today.Credits), int64(k.DailyLimit.Credits), "今日积分")
		consider(k.Used.Requests, k.TotalLimit.Requests, "累计请求")
		consider(k.Used.Tokens, k.TotalLimit.Tokens, "累计 token")
		consider(int64(k.Used.Credits), int64(k.TotalLimit.Credits), "累计积分")
		if best > 0 && detail != "" {
			out = append(out, alerts.KeyUsage{Name: k.Name, Percent: best, Detail: detail})
		}
	}
	return out
}

// reloadAlertConfig 配置保存后按磁盘上的新值热更新阈值。
// 解析失败就保持旧配置：配置已经落盘成功，巡检不该因为一次读失败而停摆。
func reloadAlertConfig(checker *alerts.Checker, path string) {
	if checker == nil {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	c, err := ParseConfig(raw)
	if err != nil {
		return
	}
	checker.SetConfig(alertsConfigFrom(c))
}
