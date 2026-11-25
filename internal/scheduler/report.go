// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// report.go 每日积分到期日报：把池里每个号的剩余积分与按日到期批次拼成一条通知。
//
// 与 alerts 的分工：alerts 是**阈值**巡检（超了才报），日报是**固定时点**的摘要——
// 没超阈值也想每天看一眼「哪几个号的钱快到期了」。数据取余额刷新留下的池内快照
// （internal/pool 的 credits 与 expiryBuckets），**不发任何上游请求**：日报零成本，
// 号再多也不会因为多一条消息多一次登录。
package scheduler

import (
	"log"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/notify"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

const (
	// digestPreviewBuckets 每个号最多列几档到期批次：再多 Bark 正文会长到被推送通道截断。
	digestPreviewBuckets = 3
	// digestSoonDays 「N 天内到期」的窗口（天）。
	digestSoonDays = 7
)

// SendExpiryDigest 拼一条积分到期日报并投递（通知通道未配置时只记日志）。
// 由排程在 report_hours 时点调用，也可由面板手动触发。
func (s *Scheduler) SendExpiryDigest() {
	title, body := s.ExpiryDigest()
	if title == "" {
		return
	}
	if s.notifier == nil {
		log.Printf("积分到期日报：通知通道未配置，跳过（%s）", title)
		return
	}
	// 换行在日志里会拆成多行，压成一行便于 grep。
	log.Printf("积分到期日报：%s", strings.ReplaceAll(body, "\n", " | "))
	s.notifier.Send(notify.LevelInfo, title, body)
}

// ExpiryDigest 生成当前快照的日报标题与正文（面板「立刻发一条」用；池未装配时返回空）。
func (s *Scheduler) ExpiryDigest() (string, string) {
	if s == nil || s.cfg.Pool == nil {
		return "", ""
	}
	// 数据来自余额刷新留下的快照，不是「现在」的实时值：把快照时刻写进正文，
	// 免得关了后台刷新（或刚重启）时把几小时前的数字当成此刻的余额。
	snapAt := time.Now()
	if v := s.lastBalanceRefresh.Load(); v != 0 {
		snapAt = time.Unix(0, v)
	}
	return formatExpiryDigest(s.cfg.Pool.List(), snapAt)
}

// formatExpiryDigest 把账号状态拼成日报标题与正文（纯函数，便于单测）。
// at 是余额快照时刻，既用于标题日期，也作「N 天后 / 7 天内」的相对基准。
func formatExpiryDigest(list []pool.Status, at time.Time) (string, string) {
	now := at.In(cstZone)
	title := "积分到期日报 · " + now.Format("01-02")
	if len(list) == 0 {
		return title, "池里还没有账号。"
	}
	// 排序：能用的号在前，各按最近到期日升序；没有待到期批次的排最后。
	// 禁用的号沉底——它们不接流量，但余额照样会过期，不能省掉。
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Disabled != b.Disabled {
			return !a.Disabled
		}
		if at, bt := firstExpiry(a), firstExpiry(b); !at.Equal(bt) {
			return at.Before(bt)
		}
		return a.UID < b.UID
	})

	var total, soonTotal int64
	var blocked int
	lines := make([]string, 0, 2+3*len(list))
	lines = append(lines, now.Format("01-02 15:04")+" 快照，共 "+itoa(len(list))+" 个号")
	for i, st := range list {
		total += st.Credits
		head := itoa(i+1) + ". " + accountLabel(st) + " · 剩 " + thousands(st.Credits) + " 分"
		if st.CreditsTotal > 0 {
			head += "（额度 " + thousands(st.CreditsTotal) + "）"
		}
		switch {
		case st.Disabled:
			head += " · 已禁用"
			blocked++
		case st.Paused:
			head += " · 已暂停选号"
		}
		lines = append(lines, head)
		if b, ok := firstBucket(st); ok {
			soon := soonRemain(st, now)
			detail := "   最近 " + bucketAt(b).Format("01-02") + " 到期 " + thousands(b.Remain) + " 分" + whenLabel(bucketAt(b), now)
			if len(st.CreditsExpiryBuckets) > 1 {
				detail += "，共 " + itoa(len(st.CreditsExpiryBuckets)) + " 批"
			}
			lines = append(lines, detail)
			lines = append(lines, "   "+itoa(digestSoonDays)+" 天内 "+thousands(soon)+" 分 · "+bucketList(st))
			soonTotal += soon
		} else {
			lines = append(lines, "   没有待到期批次（积分不会按日期失效）")
		}
	}
	summary := "合计 " + thousands(total) + " 分 · " + itoa(digestSoonDays) + " 天内到期 " + thousands(soonTotal) + " 分"
	if b, ok := firstOfAll(list); ok {
		summary += " · 最近 " + bucketAt(b).Format("01-02") + " 到期 " + thousands(b.Remain) + " 分"
	}
	if blocked > 0 {
		summary += " · 其中 " + itoa(blocked) + " 个号已禁用"
	}
	lines = append(lines, summary)
	return title, strings.Join(lines, "\n")
}

// accountLabel 号的显示名：昵称优先，没昵称用 UID 前 8 位（通知书里要能对上人）。
func accountLabel(st pool.Status) string {
	if n := strings.TrimSpace(st.Nickname); n != "" {
		return n
	}
	if st.Note != "" {
		return st.Note
	}
	if len(st.UID) > 8 {
		return st.UID[:8]
	}
	return st.UID
}

// firstBucket 最近一批到期（buckets 已按日期升序，且清洗掉了过期批次）。
func firstBucket(st pool.Status) (pool.ExpiryBucket, bool) {
	if len(st.CreditsExpiryBuckets) == 0 {
		return pool.ExpiryBucket{}, false
	}
	return st.CreditsExpiryBuckets[0], true
}

// firstExpiry 最近到期时刻，无批次时返回远未来（排序用）。
func firstExpiry(st pool.Status) time.Time {
	if b, ok := firstBucket(st); ok {
		return bucketAt(b)
	}
	return digestNoExpiry
}

// firstOfAll 全池最近一批到期，用于合计行。
func firstOfAll(list []pool.Status) (pool.ExpiryBucket, bool) {
	var best pool.ExpiryBucket
	var at time.Time
	for _, st := range list {
		b, ok := firstBucket(st)
		if !ok {
			continue
		}
		if bt := bucketAt(b); at.IsZero() || bt.Before(at) {
			best, at = b, bt
		}
	}
	return best, !at.IsZero()
}

// digestNoExpiry 没有到期批次时的排序占位（远未来，排在所有真实到期之后）。
var digestNoExpiry = time.Date(9999, 12, 31, 0, 0, 0, 0, cstZone)

// bucketAt 批次的到期时刻：优先真实时间，缺失时按日期（UTC+8）解析。
func bucketAt(b pool.ExpiryBucket) time.Time {
	if !b.ExpireAt.IsZero() {
		return b.ExpireAt.In(cstZone)
	}
	if t, err := time.ParseInLocation(time.DateOnly, b.Date, cstZone); err == nil {
		return t
	}
	return digestNoExpiry
}

// soonRemain 窗口（digestSoonDays）内到期的积分合计。
func soonRemain(st pool.Status, now time.Time) int64 {
	cut := now.AddDate(0, 0, digestSoonDays)
	var sum int64
	for _, b := range st.CreditsExpiryBuckets {
		if at := bucketAt(b); !at.After(cut) {
			sum += b.Remain
		}
	}
	return sum
}

// bucketList 批次明细：「11-28 200 / 11-30 400 / 12-15 600 等 5 批」。
func bucketList(st pool.Status) string {
	var parts []string
	for i, b := range st.CreditsExpiryBuckets {
		if i >= digestPreviewBuckets {
			return strings.Join(parts, " / ") + " 等" + itoa(len(st.CreditsExpiryBuckets)) + "批"
		}
		parts = append(parts, bucketAt(b).Format("01-02")+" "+thousands(b.Remain))
	}
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, " / ")
}

// whenLabel 相对时间（「今天」「明天」「N 天后」）：手机上一眼知道还剩几天。
func whenLabel(at, now time.Time) string {
	y, m, d := at.Date()
	ny, nm, nd := now.Date()
	days := int(time.Date(y, m, d, 0, 0, 0, 0, cstZone).Sub(time.Date(ny, nm, nd, 0, 0, 0, 0, cstZone)).Hours() / 24)
	switch {
	case days <= 0:
		return "（今天）"
	case days == 1:
		return "（明天）"
	default:
		return "（" + itoa(days) + " 天后）"
	}
}

// thousands 千分位整数（积分在手机上读，五位数不带分隔容易看错）。
func thousands(n int64) string {
	if n <= 0 {
		return "0"
	}
	s := itoa(int(n))
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	return b.String()
}
