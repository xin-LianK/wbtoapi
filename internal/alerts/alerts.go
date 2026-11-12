// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// Package alerts 状态类阈值告警的巡检。
//
// 与 notify 的分工：notify 只负责"把消息发出去"，判定逻辑在各调用点。签到失败、
// 账号被禁用这类**事件**在 scheduler / server 的失败分支里当场就能报；但"可用号只剩
// 一个""全池积分快见底""某个密钥今天快用完了""代理下周到期"是**状态**——没有哪个
// 请求会触发它，不巡检就永远没人知道，直到某天全部 503。
//
// 设计取舍：
//   - **只读、不补救**：发现可用号不足只发消息，不自动改池子参数。自动扩容/自动
//     降阈值这类动作会把"运维判断"藏起来，出问题时更难追。
//   - **数据源用函数注入**：本包不认识 pool/apikeys/proxies，只认四个读数函数
//     （见 Sources）。这样单测不需要造代理池和密钥库，装配也集中在一处。
//   - **去重交给 notify**：同一条消息在 min_interval 内只发一次；数值变化会改正文，
//     所以"从 2 个掉到 1 个"仍会再报一次，这是想要的。
package alerts

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/notify"
)

// Config 阈值配置。各项 <=0 表示**该项不检查**（而不是"阈值为 0"），
// 这样默认配置不会产生任何告警，加字段也不会改变既有部署的行为。
type Config struct {
	Enabled bool
	// Interval 巡检间隔。
	Interval time.Duration
	// MinHealthy 可用账号数下限：低于它告警。
	MinHealthy int
	// MinCredits 可用账号剩余积分合计下限。
	MinCredits int64
	// KeyPercent 客户端密钥用量占比阈值（0~1），任一限额（日/总量 × 请求/token/积分）
	// 达到这个占比就告警。
	KeyPercent float64
	// ProxyExpiryDays 代理到期提醒窗口（天）。
	ProxyExpiryDays int
}

// KeyUsage 一个密钥最紧的那项限额。
type KeyUsage struct {
	Name    string
	Percent float64 // 0~1
	Detail  string  // 人看的说明，例如「今日请求 820/1000」
}

// Sources 巡检要读的实时数据，全部由装配方接上（见 cmd/server/alerts.go）。
// 返回零值表示"读不到"，对应告警项自动跳过，不编造 0。
type Sources struct {
	// Accounts 返回账号总数与可用数（与面板概况同口径）。
	Accounts func() (total, healthy int)
	// Credits 返回可用账号剩余积分合计与计入的账号数。
	Credits func() (remaining int64, accounts int)
	// Keys 返回各客户端密钥的用量占比。
	Keys func(now time.Time) []KeyUsage
	// Proxies 返回窗口内到期的代理名与已到期数量。
	Proxies func(now time.Time, days int) (expiring []string, expired int)
	// Notify 告警出口（nil = 巡检照跑并记录，只是发不出去）。
	Notify *notify.Notifier
}

// maxKeyNames 一条密钥告警里最多点几个名字：再多正文会长到被推送通道截断。
const maxKeyNames = 5

// Checker 阈值巡检器。零值不可用，必须经 New 构造。
type Checker struct {
	mu   sync.Mutex
	cfg  Config
	deps Sources
	last time.Time

	wake chan struct{}
}

// New 构造巡检器（不启动；Run 由 main 用独立 goroutine 拉起）。
func New(cfg Config, deps Sources) *Checker {
	return &Checker{cfg: normalize(cfg), deps: deps, wake: make(chan struct{}, 1)}
}

// SetConfig 运行中改阈值与开关（面板保存配置后调用），立即按新参数重算下一轮。
func (c *Checker) SetConfig(cfg Config) {
	c.mu.Lock()
	c.cfg = normalize(cfg)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func normalize(cfg Config) Config {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Interval < time.Minute {
		cfg.Interval = time.Minute // 巡检是兜底，不必秒级
	}
	if cfg.KeyPercent > 1 {
		cfg.KeyPercent = cfg.KeyPercent / 100
	}
	return cfg
}

func (c *Checker) snapshot() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// Run 按配置间隔巡检，直到 ctx 取消。装配方用 `go checker.Run(ctx)`。
//
// 用一个分钟级 ticker 唤醒、再按当前间隔判断要不要真查：这样运行中改间隔
// （面板保存配置）不用重建 ticker，也不会因为间隔配成 1 分钟就建出高频定时器。
func (c *Checker) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		cfg := c.snapshot()
		now := time.Now()
		if cfg.Enabled && now.Sub(c.last) >= cfg.Interval {
			c.last = now
			for _, m := range c.Check(now) {
				c.deps.Notify.Send(m.Level, m.Title, m.Text)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-tick.C:
		}
	}
}

// Check 立刻按当前配置体检一次，返回要发的消息（不发出去，由调用方决定）。
// 顺序固定：账号 → 积分 → 密钥 → 代理，同一轮里先看最要命的。
func (c *Checker) Check(now time.Time) []notify.Message {
	cfg := c.snapshot()
	if !cfg.Enabled {
		return nil
	}
	var out []notify.Message
	if m, ok := c.checkAccounts(cfg); ok {
		out = append(out, m)
	}
	if m, ok := c.checkCredits(cfg); ok {
		out = append(out, m)
	}
	if m, ok := c.checkKeys(cfg, now); ok {
		out = append(out, m)
	}
	if m, ok := c.checkProxies(cfg, now); ok {
		out = append(out, m)
	}
	return out
}

// checkAccounts 可用号低于下限。0 个可用按 error（此刻进来的请求大概率直接 503）。
func (c *Checker) checkAccounts(cfg Config) (notify.Message, bool) {
	if cfg.MinHealthy <= 0 || c.deps.Accounts == nil {
		return notify.Message{}, false
	}
	total, healthy := c.deps.Accounts()
	// total=0 是"还没导入账号"：新装实例的常态，不该每 5 分钟报一次。
	if total == 0 || healthy >= cfg.MinHealthy {
		return notify.Message{}, false
	}
	level := notify.LevelWarn
	if healthy == 0 {
		level = notify.LevelError
	}
	return notify.Message{
		Level: level,
		Title: "可用账号不足",
		Text: fmt.Sprintf("可用账号 %d 个，低于阈值 %d 个（共 %d 个）。到面板「账号池」看冷却与禁用原因。",
			healthy, cfg.MinHealthy, total),
	}, true
}

// checkCredits 可用账号的剩余积分合计低于下限。
func (c *Checker) checkCredits(cfg Config) (notify.Message, bool) {
	if cfg.MinCredits <= 0 || c.deps.Credits == nil {
		return notify.Message{}, false
	}
	remaining, n := c.deps.Credits()
	if n == 0 || remaining >= cfg.MinCredits {
		return notify.Message{}, false
	}
	return notify.Message{
		Level: notify.LevelWarn,
		Title: "全池积分不足",
		Text: fmt.Sprintf("可用账号剩余积分合计 %d，低于阈值 %d（计入 %d 个号，已禁用的不计）。跑完现有额度后请求会大面积失败。",
			remaining, cfg.MinCredits, n),
	}, true
}

// checkKeys 客户端密钥用量达到阈值。多个密钥触线时合并成一条，按占比从高到低。
func (c *Checker) checkKeys(cfg Config, now time.Time) (notify.Message, bool) {
	if cfg.KeyPercent <= 0 || c.deps.Keys == nil {
		return notify.Message{}, false
	}
	var hit []KeyUsage
	for _, k := range c.deps.Keys(now) {
		if k.Percent >= cfg.KeyPercent {
			hit = append(hit, k)
		}
	}
	if len(hit) == 0 {
		return notify.Message{}, false
	}
	sort.SliceStable(hit, func(i, j int) bool { return hit[i].Percent > hit[j].Percent })
	var lines []string
	for i, k := range hit {
		if i == maxKeyNames {
			lines = append(lines, fmt.Sprintf("…另有 %d 个", len(hit)-maxKeyNames))
			break
		}
		lines = append(lines, fmt.Sprintf("· %s：%.0f%%（%s）", k.Name, k.Percent*100, k.Detail))
	}
	return notify.Message{
		Level: notify.LevelWarn,
		Title: "客户端密钥接近用量上限",
		Text:  "以下密钥已达到 " + fmt.Sprintf("%.0f%%", cfg.KeyPercent*100) + " 用量：\n" + strings.Join(lines, "\n"),
	}, true
}

// checkProxies 代理在窗口内到期（已到期的单独点名）。
func (c *Checker) checkProxies(cfg Config, now time.Time) (notify.Message, bool) {
	if cfg.ProxyExpiryDays <= 0 || c.deps.Proxies == nil {
		return notify.Message{}, false
	}
	expiring, expired := c.deps.Proxies(now, cfg.ProxyExpiryDays)
	if len(expiring) == 0 && expired == 0 {
		return notify.Message{}, false
	}
	var b strings.Builder
	if len(expiring) > 0 {
		sort.Strings(expiring)
		names := expiring
		if len(names) > maxKeyNames {
			names = names[:maxKeyNames]
		}
		fmt.Fprintf(&b, "%d 个代理将在 %d 天内到期：%s", len(expiring), cfg.ProxyExpiryDays, strings.Join(names, "、"))
		if len(expiring) > len(names) {
			fmt.Fprintf(&b, " 等")
		}
		b.WriteString("。续费后在面板改到期日即可恢复。")
	}
	if expired > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "另有 %d 个已到期，绑在上面的号现在出不去。", expired)
	}
	return notify.Message{
		Level: notify.LevelWarn,
		Title: "代理即将到期",
		Text:  b.String(),
	}, true
}
