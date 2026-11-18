// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestBucketBoostBurnFeasibility 用线上真实数据验证紧迫度公式：
// 498 分 / 9.6 小时后到期的号必须明显高于 123 分 / 6 天后到期的号，
// 而后者的余额更高——这正是旧实现把最急的号压成最低权重的根因。
func TestBucketBoostBurnFeasibility(t *testing.T) {
	now := time.Now()
	p := New("")
	p.SetExpiringWindow(168 * time.Hour)

	cases := []struct {
		name      string
		remain    int64
		until     time.Duration
		wantMin   float64
		wantMax   float64
		wantSlots int // 0 = 不校验槽位
	}{
		// need = 51.9/h → risk ≈ 0.945 → boost ≈ 5.73
		{"498 分 / 9.6 小时", 498, 9*time.Hour + 36*time.Minute, 5.6, 5.9, 6},
		// need = 0.85/h → risk ≈ 0.22 → boost ≈ 2.11
		{"123 分 / 6 天", 123, 144 * time.Hour, 2.0, 2.2, 2},
		// need = 3/h → risk = 0.5 → boost = 3.5（正好压在 baseline 上）
		{"300 分 / 100 小时", 300, 100 * time.Hour, 3.4, 3.6, 4},
		// 额度低于 min_credits(30)：尾款不值得调流量
		{"9 分 / 30 分钟", 9, 30 * time.Minute, 1, 1, 1},
		// 超出窗口：大额度远期批次不能被算成「急」
		{"5000 分 / 30 天（窗口外）", 5000, 30 * 24 * time.Hour, 1, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.bucketBoost(c.remain, now.Add(c.until), now)
			if got < c.wantMin || got > c.wantMax {
				t.Fatalf("boost=%v want [%v, %v]", got, c.wantMin, c.wantMax)
			}
			if c.wantSlots > 0 {
				e := &entry{a: &auth.Auth{UID: "x"}, expiryBuckets: []ExpiryBucket{
					{Date: now.Format("2006-01-02"), ExpireAt: now.Add(c.until), Remain: c.remain},
				}}
				if slots := p.urgencySlotsLocked(e, now); slots != c.wantSlots {
					t.Errorf("slots=%d want %d (boost=%v)", slots, c.wantSlots, got)
				}
			}
		})
	}
}

// TestUrgencyBoostWindowGates 验证窗口是唯一判据：窗口从 168h 收到 24h 后，
// 6 天后到期的号立刻退出加权（这也是 expiring_soon 调小带来的直接收益）。
func TestUrgencyBoostWindowGates(t *testing.T) {
	now := time.Now()
	p := New("")
	e := &entry{a: &auth.Auth{UID: "x"}, expiryBuckets: []ExpiryBucket{
		{Date: now.Format("2006-01-02"), ExpireAt: now.Add(144 * time.Hour), Remain: 123},
	}}

	p.SetExpiringWindow(168 * time.Hour)
	if got := p.urgencyBoostOf(e, now); got <= 1.5 {
		t.Fatalf("168h 窗口内 123 分 / 6 天应加权，boost=%v", got)
	}
	p.SetExpiringWindow(24 * time.Hour)
	if got := p.urgencyBoostOf(e, now); got != 1 {
		t.Fatalf("24h 窗口外应退化为不加权，boost=%v", got)
	}
	p.SetExpiringWindow(0)
	if got := p.urgencyBoostOf(e, now); got != 1 {
		t.Fatalf("窗口为 0（禁用到期信息）应不加权，boost=%v", got)
	}
}

// TestUrgencyBoostNeedsValidBucket 无批次、批次已过期、批次零剩余都不加权。
func TestUrgencyBoostNeedsValidBucket(t *testing.T) {
	now := time.Now()
	p := New("")
	p.SetExpiringWindow(24 * time.Hour)

	if got := p.urgencyBoostOf(&entry{a: &auth.Auth{UID: "x"}}, now); got != 1 {
		t.Fatalf("无批次 boost=%v want 1", got)
	}
	expired := &entry{a: &auth.Auth{UID: "x"}, expiryBuckets: []ExpiryBucket{
		{Date: now.Format("2006-01-02"), ExpireAt: now.Add(-time.Minute), Remain: 500},
	}}
	if got := p.urgencyBoostOf(expired, now); got != 1 {
		t.Fatalf("已过期批次 boost=%v want 1", got)
	}
	zero := &entry{a: &auth.Auth{UID: "x"}, expiryBuckets: []ExpiryBucket{
		{Date: now.Format("2006-01-02"), ExpireAt: now.Add(time.Hour), Remain: 0},
	}}
	if got := p.urgencyBoostOf(zero, now); got != 1 {
		t.Fatalf("零剩余批次 boost=%v want 1", got)
	}
}

// TestUrgencyBoostTakesMaxBucket 多批次取最急的一个，而不是最早的那个：
// 最早一批只剩 5 分（尾款），真正的大头在 40 分钟后才是该被推的。
func TestUrgencyBoostTakesMaxBucket(t *testing.T) {
	now := time.Now()
	p := New("")
	p.SetExpiringWindow(24 * time.Hour)
	e := &entry{a: &auth.Auth{UID: "x"}, expiryBuckets: []ExpiryBucket{
		{Date: now.Format("2006-01-02"), ExpireAt: now.Add(10 * time.Minute), Remain: 5},
		{Date: now.Format("2006-01-02"), ExpireAt: now.Add(40 * time.Minute), Remain: 400},
	}}
	// 400 分 / 0.67h → need 600/h → risk≈0.995 → boost≈5.98（5 分那批被 min_credits 跳过）
	if got := p.urgencyBoostOf(e, now); got < 5.5 {
		t.Fatalf("应取更急的大额批次，boost=%v", got)
	}
}

// TestUrgencySlotsCap 倍数换算槽位时钳到上限，避免极端配置撑爆 UID 列表。
func TestUrgencySlotsCap(t *testing.T) {
	now := time.Now()
	p := New("")
	p.SetExpiringWindow(24 * time.Hour)
	p.SetUrgency(3, 1000, 30) // 人为把上限拉满
	e := &entry{a: &auth.Auth{UID: "x"}, expiryBuckets: []ExpiryBucket{
		{Date: now.Format("2006-01-02"), ExpireAt: now.Add(time.Minute), Remain: 100000},
	}}
	if got := p.urgencySlotsLocked(e, now); got != maxUrgencySlots {
		t.Fatalf("slots=%d want %d", got, maxUrgencySlots)
	}
}

// TestUrgencySetIgnoresNonPositive 非正值保留原值（与 SetWeights 同风格）。
func TestUrgencySetIgnoresNonPositive(t *testing.T) {
	p := New("")
	p.SetUrgency(0, 0, 0)
	if p.urgencyBaseline != defaultUrgencyBaseline || p.urgencyMaxBoost != defaultUrgencyMaxBoost ||
		p.urgencyMinCredits != defaultUrgencyMinCredits {
		t.Fatalf("非正值应保留默认：%v/%v/%v", p.urgencyBaseline, p.urgencyMaxBoost, p.urgencyMinCredits)
	}
	p.SetUrgency(1.5, 2, 10)
	if p.urgencyBaseline != 1.5 || p.urgencyMaxBoost != 2 || p.urgencyMinCredits != 10 {
		t.Fatalf("setter 未生效：%v/%v/%v", p.urgencyBaseline, p.urgencyMaxBoost, p.urgencyMinCredits)
	}
}

// TestUrgentNowIgnoresTinyBatch 额度低于下限的尾款批次不再触发硬分层。
// 旧实现用 expiringNow 判定，只剩 9 分的号会把候选集独占——自己拿不到加权，
// 又把其他号挡在门外。现在硬分层与加权共用 urgentNow，口径不可能不一致。
func TestUrgentNowIgnoresTinyBatch(t *testing.T) {
	now := time.Now()
	p := New("")
	p.SetExpiringWindow(24 * time.Hour)
	today := now.Format("2006-01-02")

	tiny := &entry{a: &auth.Auth{UID: "tiny"}, expiryBuckets: []ExpiryBucket{
		{Date: today, ExpireAt: now.Add(30 * time.Minute), Remain: 9},
	}}
	if p.urgentNow(tiny, now) {
		t.Fatal("9 分尾款不该触发硬分层")
	}
	big := &entry{a: &auth.Auth{UID: "big"}, expiryBuckets: []ExpiryBucket{
		{Date: today, ExpireAt: now.Add(30 * time.Minute), Remain: 500},
	}}
	if !p.urgentNow(big, now) {
		t.Fatal("500 分 / 30 分钟应触发硬分层")
	}
}

// TestPickUrgencyBeatsHigherBalance 复现线上那一幕：余额高但 6 天后到期的号，
// 不该赢过余额低但 9.6 小时后到期的号。硬分层开着时两者都在候选里，
// 旧实现按余额排（7262 > 4394）由前者通吃，现在由紧迫度决定。
func TestPickUrgencyBeatsHigherBalance(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	p := New("")
	p.Add(&auth.Auth{UID: "soon"})  // 余额低、最急
	p.Add(&auth.Auth{UID: "later"}) // 余额高、不急

	p.SetExpiringWindow(168 * time.Hour)
	p.SetCreditsSnapshot("soon", CreditsSnapshot{
		Remain: 4394, Total: 4394, Expiring: 1406,
		EarliestAt: now.Add(9*time.Hour + 36*time.Minute), EarliestRemaining: 498,
		ExpiryBuckets: []ExpiryBucket{
			{Date: today, ExpireAt: now.Add(9*time.Hour + 36*time.Minute), Remain: 498},
			{Date: now.Add(6 * 24 * time.Hour).Format("2006-01-02"), ExpireAt: now.Add(6 * 24 * time.Hour), Remain: 908},
		},
	})
	p.SetCreditsSnapshot("later", CreditsSnapshot{
		Remain: 7262, Total: 7262, Expiring: 123,
		EarliestAt: now.Add(144 * time.Hour), EarliestRemaining: 123,
		ExpiryBuckets: []ExpiryBucket{
			{Date: now.Add(144 * time.Hour).Format("2006-01-02"), ExpireAt: now.Add(144 * time.Hour), Remain: 123},
		},
	})

	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatal("pick returned nil")
		}
		counts[got.UID]++
	}
	// soon 权重 ≈ (1+6.05+idle)×5.7，later ≈ (1+10+idle)×2.1，前者应明显占优。
	if counts["soon"] <= counts["later"] {
		t.Fatalf("最急的号应拿到更多流量，counts=%v", counts)
	}
	if counts["later"] == 0 {
		t.Fatalf("不急的号也不该完全没有流量，counts=%v", counts)
	}
}
