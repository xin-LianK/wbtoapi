// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// 到期紧迫度加权：把「快过期」从布尔升级成「烧得完吗」的连续信号。
package pool

// 背景：expiring_first 硬分层只回答「某个号算不算快过期」，收窄之后的层内仍按
// weightOf 的余额项分配——余额高的号权重高、接更多请求。而快过期号之间余额高低
// 与到期紧迫度无关，甚至相反：余额 4394 的号最早一批 9.6 小时后到期，余额 7262 的
// 号还有 6 天，前者权重最低、几乎接不到请求（issue #101 遗留问题）。
//
// 这里对每个到期批次算「要按时烧完所需的最低消耗速率」：
//
//	need  = Remain / 剩余小时           （分/小时）
//	risk  = need / (need + baseline)    （0~1，越接近 1 越烧不完）
//	boost = 1 + maxBoost × risk         （1 ~ 1+maxBoost）
//
// 账号取所有批次里 boost 最大者。三个约束保证它只表达「急」，不表达「额度大」：
//
//  1. 只有到期时间落在 expiringWindow（pool.expiring_soon）内的批次参与——否则
//     30 天后到期的 5000 分会算出很高的 need，把大额度远期号顶成流量中心；
//  2. 额度小于 minCredits 的批次跳过——剩几分的尾款不值得为它调流量；
//  3. 已过期 / 零剩余的批次跳过（批次数据靠余额刷新更新，两次刷新之间会自然过期）。
//
// 只改权重与硬分层的判据、不动其他候选规则：优先级分层、成本分层全部照旧。因此
// prefer_expiring=false、expiring_soon=0 或没有任何窗口内有效批次时，倍数恒为 1，
// 结果与旧行为完全一致。

import (
	"math"
	"time"
)

// maxUrgencySlots 紧迫度倍数换算成新会话虚拟实例数时的硬上限（防 1+maxBoost 被
// 配成极大值时把 UID 列表撑爆）。与 pick 侧的浮点倍数口径不同，仅用于槽位展开。
const maxUrgencySlots = 16

// urgentNow 报告账号是否存在「值得为它调流量」的窗口内到期批次——即紧迫度倍数 > 1。
//
// 硬分层（restrictToExpiringLocked / 新会话分配）与加权共用这一个判据，而不是各用
// 一套：否则一个只剩 9 分的尾款批次能把候选集独占（expiringNow 为真、倍数为 1），
// 既拿不到加权，又把其他号挡在门外。用倍数当判据还顺带保证了「收窄的一定是加了权
// 的」，两个阶段不可能口径不一致。expiring_soon 为 0（禁用到期信息）时恒为 false。
func (p *Pool) urgentNow(e *entry, now time.Time) bool {
	return p.urgencyBoostOf(e, now) > 1
}

// urgencyBoostOf 返回账号的到期紧迫度权重倍数（≥1；1 = 不加权）。调用方持 p.mu。
//
// expiringWindow <= 0（pool.expiring_soon 为空/0）时整体关闭：窗口是「什么算急」的
// 唯一判据，没有它就无法区分「1 小时后到期」和「30 天后到期」，而按 need 排序会让
// 大额度远期批次垄断流量。这与 expiring_soon=0 时「禁用到期信息」的既有语义一致。
func (p *Pool) urgencyBoostOf(e *entry, now time.Time) float64 {
	if !p.preferExpiring || p.urgencyMaxBoost <= 0 || p.expiringWindow <= 0 {
		return 1
	}
	boost := 1.0
	// 有按日批次就逐批算（能识别「最早一批只剩几分、真正的大头在后面」）；没有批次
	// 的快照（旧 state / 只拿到聚合值的调用方）退回最早一批，语义相同。
	if len(e.expiryBuckets) > 0 {
		for _, b := range e.expiryBuckets {
			boost = math.Max(boost, p.bucketBoost(b.Remain, b.ExpireAt, now))
		}
		return boost
	}
	if expiringNow(e, now) {
		boost = p.bucketBoost(e.creditsEarliestRemaining, e.creditsEarliestExpiry, now)
	}
	return boost
}

// bucketBoost 单个到期批次的紧迫度倍数。不满足条件时返回 1（不加权）。
func (p *Pool) bucketBoost(remain int64, expireAt, now time.Time) float64 {
	if remain <= 0 || remain < p.urgencyMinCredits {
		return 1
	}
	if expireAt.IsZero() || !expireAt.After(now) {
		return 1
	}
	// 窗口闸门：窗口外的批次不算急（大额度 + 远期 = 高 need，但不该抢流量）。
	if expireAt.Sub(now) > p.expiringWindow {
		return 1
	}
	hours := expireAt.Sub(now).Hours()
	if hours <= 0 {
		return 1
	}
	need := float64(remain) / hours
	base := p.urgencyBaseline
	if base <= 0 {
		base = defaultUrgencyBaseline
	}
	// need ≥ 0、base > 0 → risk ∈ [0,1)。need 远大于 baseline 时趋近 1（烧不完）。
	risk := need / (need + base)
	return 1 + p.urgencyMaxBoost*risk
}

// urgencySlotsLocked 把紧迫度倍数换算成新会话候选集的虚拟实例数（1 ~ maxUrgencySlots）。
// 旧行为是常数 expiringVirtualSlots=3（所有快过期号同权），现在按号自己的紧迫度取整，
// 让「9.6 小时后到期 498 分」比「6 天后到期 123 分」多拿到槽位。调用方持 p.mu。
func (p *Pool) urgencySlotsLocked(e *entry, now time.Time) int {
	boost := p.urgencyBoostOf(e, now)
	if boost <= 1 {
		return 1
	}
	slots := int(math.Round(boost))
	if slots < 1 {
		slots = 1
	}
	if slots > maxUrgencySlots {
		slots = maxUrgencySlots
	}
	return slots
}
