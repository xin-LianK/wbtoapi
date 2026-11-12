// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/notify"
)

// fixedSources 一组固定读数，nil 的项表示该项读不到。
func fixedSources(total, healthy int, credits int64, accounts int) Sources {
	return Sources{
		Accounts: func() (int, int) { return total, healthy },
		Credits:  func() (int64, int) { return credits, accounts },
	}
}

func findByTitle(msgs []notify.Message, sub string) (notify.Message, bool) {
	for _, m := range msgs {
		if strings.Contains(m.Title, sub) {
			return m, true
		}
	}
	return notify.Message{}, false
}

func TestCheckAccounts(t *testing.T) {
	src := fixedSources(5, 2, 0, 0)
	c := New(Config{Enabled: true, MinHealthy: 3}, src)
	msgs := c.Check(time.Now())
	m, ok := findByTitle(msgs, "可用账号")
	if !ok {
		t.Fatalf("应告警: %+v", msgs)
	}
	if m.Level != notify.LevelWarn {
		t.Errorf("还有 2 个可用时应为 warn，得到 %s", m.Level)
	}
	if !strings.Contains(m.Text, "2 个") || !strings.Contains(m.Text, "3 个") {
		t.Errorf("正文应含实际值与阈值: %s", m.Text)
	}
}

// 0 个可用按 error：此刻进来的请求大概率直接 503。
func TestCheckAccountsZeroIsError(t *testing.T) {
	c := New(Config{Enabled: true, MinHealthy: 1}, fixedSources(3, 0, 0, 0))
	m, ok := findByTitle(c.Check(time.Now()), "可用账号")
	if !ok {
		t.Fatal("应告警")
	}
	if m.Level != notify.LevelError {
		t.Errorf("0 个可用应为 error，得到 %s", m.Level)
	}
}

// 没有账号（新装实例）不告警：否则每 5 分钟一条，第一次配完就被烦到关掉告警。
func TestCheckAccountsEmptyPoolSilent(t *testing.T) {
	c := New(Config{Enabled: true, MinHealthy: 3}, fixedSources(0, 0, 0, 0))
	if msgs := c.Check(time.Now()); len(msgs) != 0 {
		t.Fatalf("空号池不该告警: %+v", msgs)
	}
}

// 阈值 0 = 不检查该项。
func TestCheckDisabledThresholds(t *testing.T) {
	c := New(Config{Enabled: true}, fixedSources(5, 0, 1, 1))
	if msgs := c.Check(time.Now()); len(msgs) != 0 {
		t.Fatalf("未配阈值不该告警: %+v", msgs)
	}
}

func TestCheckCredits(t *testing.T) {
	c := New(Config{Enabled: true, MinCredits: 1000}, fixedSources(3, 3, 400, 3))
	m, ok := findByTitle(c.Check(time.Now()), "积分")
	if !ok {
		t.Fatal("应告警")
	}
	if !strings.Contains(m.Text, "400") {
		t.Errorf("正文应含剩余积分: %s", m.Text)
	}
}

// 传进来的都是可用号，但一个都没计入时（全是禁用）不该报：读不到就别猜。
func TestCheckCreditsNoAccounts(t *testing.T) {
	c := New(Config{Enabled: true, MinCredits: 1000}, fixedSources(3, 3, 0, 0))
	if msgs := c.Check(time.Now()); len(msgs) != 0 {
		t.Fatalf("没有计入的账号时不该告警: %+v", msgs)
	}
}

func TestCheckKeys(t *testing.T) {
	src := fixedSources(3, 3, 1<<30, 3)
	src.Keys = func(time.Time) []KeyUsage {
		return []KeyUsage{
			{Name: "团队A", Percent: 0.5, Detail: "今日请求 50/100"},
			{Name: "脚本", Percent: 0.92, Detail: "今日 token 920/1000"},
		}
	}
	c := New(Config{Enabled: true, KeyPercent: 0.8}, src)
	m, ok := findByTitle(c.Check(time.Now()), "密钥")
	if !ok {
		t.Fatal("应告警")
	}
	if !strings.Contains(m.Text, "脚本") || strings.Contains(m.Text, "团队A") {
		t.Errorf("只该列出超阈值的密钥: %s", m.Text)
	}
	// 占比高的排前面
	if strings.Index(m.Text, "92%") < 0 {
		t.Errorf("正文应含占比: %s", m.Text)
	}
}

// 密钥很多时正文不无限长（推送通道会截断）。
func TestCheckKeysCapped(t *testing.T) {
	src := fixedSources(1, 1, 1, 1)
	src.Keys = func(time.Time) []KeyUsage {
		out := make([]KeyUsage, 9)
		for i := range out {
			out[i] = KeyUsage{Name: "k" + string(rune('a'+i)), Percent: 0.9, Detail: "x"}
		}
		return out
	}
	c := New(Config{Enabled: true, KeyPercent: 0.8}, src)
	m, _ := findByTitle(c.Check(time.Now()), "密钥")
	if n := strings.Count(m.Text, "· "); n != maxKeyNames {
		t.Errorf("最多列 %d 个，实际 %d: %s", maxKeyNames, n, m.Text)
	}
	if !strings.Contains(m.Text, "另有") {
		t.Errorf("应提示还有更多: %s", m.Text)
	}
}

func TestCheckProxies(t *testing.T) {
	src := fixedSources(1, 1, 1, 1)
	src.Proxies = func(now time.Time, days int) ([]string, int) {
		if days != 7 {
			t.Errorf("窗口=%d want 7", days)
		}
		return []string{"香港-1", "日本-2"}, 1
	}
	c := New(Config{Enabled: true, ProxyExpiryDays: 7}, src)
	m, ok := findByTitle(c.Check(time.Now()), "代理")
	if !ok {
		t.Fatal("应告警")
	}
	for _, want := range []string{"香港-1", "日本-2", "2 个", "1 个已到期"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("正文缺少 %q: %s", want, m.Text)
		}
	}
}

// 关掉总开关就不查（阈值留着也不生效）。
func TestCheckDisabled(t *testing.T) {
	c := New(Config{Enabled: false, MinHealthy: 9}, fixedSources(1, 0, 0, 0))
	if msgs := c.Check(time.Now()); len(msgs) != 0 {
		t.Fatalf("关闭后不该告警: %+v", msgs)
	}
}

// 面板填 80 而不是 0.8 也应认（normalize 里除 100）。
func TestNormalizePercent(t *testing.T) {
	src := fixedSources(1, 1, 1, 1)
	src.Keys = func(time.Time) []KeyUsage { return []KeyUsage{{Name: "k", Percent: 0.85, Detail: "d"}} }
	c := New(Config{Enabled: true, KeyPercent: 80}, src)
	if _, ok := findByTitle(c.Check(time.Now()), "密钥"); !ok {
		t.Fatal("80 应被当成 80%")
	}
}

// 间隔下限 1 分钟：配 5 秒也只按分钟巡检。
func TestNormalizeInterval(t *testing.T) {
	if got := normalize(Config{Interval: 5 * time.Second}).Interval; got != time.Minute {
		t.Errorf("interval=%v want 1m", got)
	}
	if got := normalize(Config{}).Interval; got != 5*time.Minute {
		t.Errorf("缺省 interval=%v want 5m", got)
	}
}

// SetConfig 热更新下一轮生效。
func TestSetConfigHotReload(t *testing.T) {
	c := New(Config{Enabled: true}, fixedSources(1, 0, 0, 0))
	if msgs := c.Check(time.Now()); len(msgs) != 0 {
		t.Fatalf("初始不告警: %+v", msgs)
	}
	c.SetConfig(Config{Enabled: true, MinHealthy: 2})
	if _, ok := findByTitle(c.Check(time.Now()), "可用账号"); !ok {
		t.Fatal("改完阈值应立即生效")
	}
}
