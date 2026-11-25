// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestFormatExpiryDigestSingle 单号：最近到期、7 天窗口、额度与合计都要写清。
func TestFormatExpiryDigestSingle(t *testing.T) {
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, cstZone)
	list := []pool.Status{{
		UID: "u1", Nickname: "一号", Credits: 3240, CreditsTotal: 5000,
		CreditsExpiryBuckets: []pool.ExpiryBucket{
			{Date: "2026-11-28", ExpireAt: time.Date(2026, 11, 28, 23, 59, 0, 0, cstZone), Remain: 200},
			{Date: "2026-11-30", ExpireAt: time.Date(2026, 11, 30, 23, 59, 0, 0, cstZone), Remain: 1200},
			{Date: "2026-12-20", ExpireAt: time.Date(2026, 12, 20, 23, 59, 0, 0, cstZone), Remain: 600},
		},
	}}
	title, body := formatExpiryDigest(list, now)
	if !strings.Contains(title, "11-25") {
		t.Errorf("标题应含日期: %q", title)
	}
	for _, want := range []string{
		"共 1 个号",
		"一号 · 剩 3,240 分（额度 5,000）",
		"最近 11-28 到期 200 分（3 天后），共 3 批",
		"7 天内 1,400 分 · 11-28 200 / 11-30 1,200 / 12-20 600",
		"合计 3,240 分 · 7 天内到期 1,400 分 · 最近 11-28 到期 200 分",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("正文缺 %q:\n%s", want, body)
		}
	}
}

// TestFormatExpiryDigestOrderAndFlags 排序：能用的号按最近到期升序，禁用号沉底但仍列出；
// 没有到期批次的号明说「不会按日期失效」。
func TestFormatExpiryDigestOrderAndFlags(t *testing.T) {
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, cstZone)
	list := []pool.Status{
		{UID: "u-disabled", Nickname: "停用号", Credits: 900, Disabled: true,
			CreditsExpiryBuckets: []pool.ExpiryBucket{{Date: "2026-11-26", Remain: 900}}},
		{UID: "u-none", Nickname: "长期号", Credits: 5000},
		{UID: "u-late", Nickname: "晚到期", Credits: 800,
			CreditsExpiryBuckets: []pool.ExpiryBucket{{Date: "2026-12-31", Remain: 800}}},
		{UID: "u-early", Nickname: "早到期", Credits: 300,
			CreditsExpiryBuckets: []pool.ExpiryBucket{{Date: "2026-11-26", Remain: 300}}},
	}
	_, body := formatExpiryDigest(list, now)
	order := []string{"早到期", "晚到期", "长期号", "停用号"}
	last := -1
	for _, name := range order {
		at := strings.Index(body, name)
		if at < 0 {
			t.Fatalf("正文缺 %q:\n%s", name, body)
		}
		if at < last {
			t.Fatalf("顺序应为 %v:\n%s", order, body)
		}
		last = at
	}
	if !strings.Contains(body, "停用号 · 剩 900 分 · 已禁用") {
		t.Errorf("禁用号要标出来:\n%s", body)
	}
	if !strings.Contains(body, "没有待到期批次（积分不会按日期失效）") {
		t.Errorf("无批次号要说明:\n%s", body)
	}
	if !strings.Contains(body, "其中 1 个号已禁用") {
		t.Errorf("合计行要点出禁用号数:\n%s", body)
	}
}

// TestFormatExpiryDigestBucketPreview 批次多于预览上限时折叠成「等 N 批」，不把正文撑长。
func TestFormatExpiryDigestBucketPreview(t *testing.T) {
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, cstZone)
	var buckets []pool.ExpiryBucket
	for i := 0; i < 5; i++ {
		at := time.Date(2026, 11, 26+i, 0, 0, 0, 0, cstZone)
		buckets = append(buckets, pool.ExpiryBucket{Date: at.Format(time.DateOnly), ExpireAt: at, Remain: 100})
	}
	_, body := formatExpiryDigest([]pool.Status{{UID: "u1", Nickname: "五批", Credits: 500,
		CreditsExpiryBuckets: buckets}}, now)
	if !strings.Contains(body, "等5批") {
		t.Errorf("超过 3 批应折叠成「等5批」:\n%s", body)
	}
	if strings.Contains(body, "11-30 100") {
		t.Errorf("折叠后不该再列第 4 批:\n%s", body)
	}
}

// TestFormatExpiryDigestEmpty 空池也要发一条（用户要的是每天都有信）。
func TestFormatExpiryDigestEmpty(t *testing.T) {
	title, body := formatExpiryDigest(nil, time.Date(2026, 11, 25, 8, 0, 0, 0, cstZone))
	if !strings.Contains(title, "积分到期日报") {
		t.Errorf("标题=%q", title)
	}
	if !strings.Contains(body, "还没有账号") {
		t.Errorf("正文=%q", body)
	}
}

// TestThousands 千分位：四位数分隔、负数与非正按 0。
func TestThousands(t *testing.T) {
	cases := map[int64]string{0: "0", -5: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := thousands(in); got != want {
			t.Errorf("thousands(%d)=%q want %q", in, got, want)
		}
	}
}

// TestNextWakeReportSlot 日报时点进排程：只有日报打开时，nextWake 落在 8 点且只派日报。
func TestNextWakeReportSlot(t *testing.T) {
	s := New(Config{ReportHours: []int{8}})
	at, kinds := s.nextWake(time.Date(2026, 11, 25, 7, 30, 0, 0, time.Local))
	if at.Hour() != 8 || at.Minute() != 0 {
		t.Fatalf("应在 8:00 触发，got %s", at)
	}
	if len(kinds) != 1 || kinds[0] != taskReport {
		t.Fatalf("应只派日报，got %v", kinds)
	}
	// 关掉之后不再排日报（8 点这一档消失；其余任务的默认时点照旧，见 New 的默认填充）。
	s.SetReportSchedule(nil, true)
	at, kinds = s.nextWake(time.Date(2026, 11, 25, 7, 30, 0, 0, time.Local))
	if at.Hour() == 8 {
		t.Fatalf("关闭后不应在 8:00 触发，got %s", at)
	}
	for _, k := range kinds {
		if k == taskReport {
			t.Fatalf("关闭后仍派了日报: %v", kinds)
		}
	}
}
