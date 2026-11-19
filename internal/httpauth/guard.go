// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package httpauth

// guard.go 鉴权失败限流：同一来源在窗口期内连续猜错密钥，就往回退一段时间。
//
// 为什么需要：网关生成的密钥是 192 位随机串，在线爆破物理上不可能；但这条防线的前提是
// 「原文只由网关生成」。闸门之外还有一层意义——把猜错这件事本身变成可观测信号：以前
// 401 一写了之，既不限速也不留痕，有人跑字典时面板和日志里什么都看不到。

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 判定参数。生产值见注释；var 供测试缩短（authFailWindow 太小会让正常用户误伤，别乱改）。
var (
	// authFailWindow 统计窗口：窗口内累计失败达到 authFailLimit 就封。
	authFailWindow = time.Minute
	// authFailLimit 一个来源在一个窗口内允许的失败次数。
	authFailLimit = 10
	// authBlockFor 触发后封多久。封禁期内连摘要求都不做，直接回 429。
	authBlockFor = 5 * time.Minute
	// authMaxEntries 表的内存上限：来源 IP 由请求头决定，不能让伪造来源把表撑爆。
	authMaxEntries = 4096
)

// failState 一个来源的失败计数。
type failState struct {
	windowStart time.Time // 当前统计窗口的起点
	fails       int       // 窗口内的失败次数
	blockedTill time.Time // 封到什么时候；零值 = 没封
}

// Guard 按来源 IP 统计鉴权失败。零值不可用，用 NewGuard。nil 的 Guard 全部方法都安全
// （当作没开限流），方便测试里不装配。
type Guard struct {
	mu  sync.Mutex
	m   map[string]*failState
	now func() time.Time
}

// NewGuard 建一个限流器。
func NewGuard() *Guard {
	return &Guard{m: map[string]*failState{}, now: time.Now}
}

// Blocked 来源是否还在封禁期；是则返回还要等多久。
func (g *Guard) Blocked(ip string) (time.Duration, bool) {
	if g == nil {
		return 0, false
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.m[ip]
	if st == nil || st.blockedTill.IsZero() || !now.Before(st.blockedTill) {
		return 0, false
	}
	return st.blockedTill.Sub(now), true
}

// Fail 记一次失败。blocked 为 true 表示这一次刚好触发封禁（调用方据此只打一条日志、
// 只推一条告警，不必每个失败都推）；fails 是触发时的失败次数。
func (g *Guard) Fail(ip string) (blocked bool, fails int) {
	if g == nil {
		return false, 0
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.m[ip]
	if st == nil {
		if len(g.m) >= authMaxEntries {
			g.pruneLocked(now)
		}
		st = &failState{windowStart: now}
		g.m[ip] = st
	}
	if now.Sub(st.windowStart) > authFailWindow {
		st.windowStart, st.fails = now, 0
	}
	st.fails++
	if st.fails < authFailLimit {
		return false, st.fails
	}
	// 已经封着就只刷新时间，不再重复报「刚好触发」。
	first := st.blockedTill.IsZero() || !now.Before(st.blockedTill)
	st.blockedTill = now.Add(authBlockFor)
	st.windowStart, st.fails = now, 0
	return first, authFailLimit
}

// Reset 鉴权成功时清掉这个来源的计数：正常用户偶尔敲错一两次，不该在窗口里攒着。
func (g *Guard) Reset(ip string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.m, ip)
}

// pruneLocked 清掉已经过期的条目；表还是满的话整体丢掉重来（极端情况下宁可放宽，
// 也不能让内存无限长）。
func (g *Guard) pruneLocked(now time.Time) {
	for ip, st := range g.m {
		expired := st.blockedTill.IsZero() || !now.Before(st.blockedTill)
		if expired && now.Sub(st.windowStart) > authFailWindow {
			delete(g.m, ip)
		}
	}
	if len(g.m) >= authMaxEntries {
		g.m = map[string]*failState{}
	}
}

// MaskPrefix 日志里展示密钥用：只留前 6 个字符，够认清「是哪一段形态」，又拼不出原文。
// 日志会被备份、转发、贴进 issue，原始密钥不该出现在里面。
func MaskPrefix(s string) string {
	if s == "" {
		return "<none>"
	}
	if len(s) <= 6 {
		return s[:len(s)/2] + "…"
	}
	return s[:6] + "…"
}

// ClientIP 取用于限流的来源地址。
//
// 直连时用 RemoteAddr；RemoteAddr 是回环或内网地址（说明前面有本机/内网反向代理）时，
// 才看 X-Forwarded-For 的第一跳。反过来做会出两个问题：直连公网时信任 XFF 等于让人随便
// 换来源绕过限流；而只认 RemoteAddr 又会让「Caddy → 网关」的部署把全部用户算成同一个
// 来源，一个人被拦就全员被拦。
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return "unknown"
	}
	if !behindProxy(host) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if net.ParseIP(first) != nil {
			return first
		}
	}
	return host
}

// behindProxy 判断 RemoteAddr 是不是本机/内网（即请求由反向代理转进来）。
func behindProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
