// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档（requests-<日期>.jsonl，见 logarchive）
// 只写请求元数据，不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
//
// 写入前还会把失败原因与 User-Agent 过一遍 redact.Text：上游原文、代理地址、UA 里夹带的
// 凭证或邮箱不该因为「只是元数据」就落盘。脱敏之后再进内存环与归档，读侧不需再处理。
//
// 面板「请求记录」的实时更新走 Wait：每条事件带自增 Seq，前端拿游标 $after 做长轮询，
// 只取增量。这与定时拉整表不同——请求量一大，5 秒一次的「全量快照 + 读归档」既费磁盘
// 又会让正在翻看的列表整体跳动（同 magpie 的 route trace 做法）。
package reqlog

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logarchive"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redact"
)

const (
	recentLimit    = 100
	defaultReadMax = 1000
	archivePrefix  = "requests"
	// streamLimit 是增量游标的窗口：面板一次最多显示 300 条，游标落在这个窗口内即可只取增量；
	// 落后更多（页面挂了一夜）就回 reset，让前端整体重来，而不是漏掉中间一段。
	streamLimit = 300
	// DefaultStreamWait 长轮询最长挂多久；MaxStreamWait 是上限（防止有人传 1 小时）。
	DefaultStreamWait = 20 * time.Second
	MaxStreamWait     = 25 * time.Second
	// streamCoalesce 拿到首批事件后再等一小会儿，把同一波结束的请求并成一批返回。
	// agent 并发 8 个请求时，这样只需一次往返，而不是 8 次。
	streamCoalesce = 150 * time.Millisecond
)

// 测试可替换：随机数失败回退。
var randRead = rand.Read

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := randRead(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。RetentionDays=0 = 永久保留（仍受 MaxBytes 限制），
// MaxBytes<=0 回落 100 MiB。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	// Seq 进程内自增序号（从 1 起），面板长轮询用它做增量游标；归档里跨重启没有意义，
	// 所以只用于内存流，读到旧归档时为 0。
	Seq     uint64 `json:"seq,omitempty"`
	Path    string `json:"path"`
	Account string `json:"account,omitempty"`
	// Proxy 最后选的号走的代理（代理名，没绑代理为空）。
	Proxy            string  `json:"proxy,omitempty"`
	Model            string  `json:"model,omitempty"`
	Status           int     `json:"status"`
	OK               bool    `json:"ok"`
	Outcome          string  `json:"outcome"`
	DurationMs       int64   `json:"duration_ms"`
	TTFBMs           int64   `json:"ttfb_ms,omitempty"`
	Attempts         int     `json:"attempts,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	TotalTokens      int64   `json:"total_tokens,omitempty"`
	Credit           float64 `json:"credit,omitempty"`
	HasCredit        bool    `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64  `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64  `json:"cache_miss_tokens,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
	// APIKey 请求带的密钥：客户端密钥记名字，管理员密钥记「管理员」，没开鉴权为空。不记原文。
	APIKey string `json:"api_key,omitempty"`
	// Error 失败原因：回给客户端的错误消息（流中途出错时是上游的错误帧），最多 200 字；成功的请求为空。
	Error string `json:"error,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	Model     string
	ClientIP  string
	UserAgent string
	APIKey    string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats = logarchive.Stats

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Totals 进程级计数（不含最近事件）。长轮询每次都带回来，前端表头的「已完成 / 成功率 /
// 平均耗时 / 进行中」因此保持连续，不需要额外请求一次全量快照。
type Totals struct {
	Completed       int64   `json:"completed"`
	InFlight        int64   `json:"in_flight"`
	Succeeded       int64   `json:"succeeded"`
	Failed          int64   `json:"failed"`
	SuccessRate     float64 `json:"success_rate"`
	HTTPSuccessRate float64 `json:"http_success_rate"`
	AvgDurationMs   float64 `json:"avg_duration_ms"`
}

// Stream 一次长轮询的结果。Events 按时间倒序（新的在前）。
//
// Reset=true 表示游标已经对不上（网关重启，seq 从头来；或页面挂太久，缺口前的记录
// 已被淘汰）：此时 Events 是现有窗口的全部内容，前端应整体替换而不是追加。
type Stream struct {
	Seq    uint64  `json:"seq"`
	Reset  bool    `json:"reset,omitempty"`
	Events []Event `json:"events"`
	Totals Totals  `json:"totals"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	recent      []Event
	// stream 按 Seq 升序保存最近的 streamLimit 条，供游标增量读取；recent 是它的倒序视图。
	stream  []Event
	seq     uint64
	changed chan struct{} // 有新事件时 close 并换一个，等待方立即醒来
	archive *logarchive.Store
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	r := &Recorder{
		started: time.Now(),
		recent:  make([]Event, 0, recentLimit),
		stream:  make([]Event, 0, streamLimit),
		changed: make(chan struct{}),
	}
	r.archive = logarchive.New(logarchive.Config{
		Dir: cfg.Dir, Prefix: archivePrefix, Enabled: cfg.Enabled,
		RetentionDays: cfg.RetentionDays, MaxBytes: cfg.MaxBytes,
		FileMaxBytes: cfg.FileMaxBytes, QueueSize: cfg.QueueSize,
	})
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	// 失败原因是回给客户端的上游原文，UA 是调用方自报的串：两者都可能夹带凭证或邮箱，
	// 落盘前先盖掉。提示词与响应正文本来就不进这里。
	e.Error = redact.Text(e.Error)
	e.UserAgent = redact.Text(e.UserAgent)
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	r.seq++
	e.Seq = r.seq
	r.recent = append([]Event{e}, r.recent...)
	if len(r.recent) > recentLimit {
		r.recent = r.recent[:recentLimit]
	}
	r.stream = append(r.stream, e)
	if overflow := len(r.stream) - streamLimit; overflow > 0 {
		r.stream = r.stream[overflow:]
	}
	// 广播：等待中的长轮询各自重查一次，谁拿到增量谁返回。
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
	r.archive.Append(e.Time, e)
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	t := r.totalsLocked()
	s := Snapshot{
		StartedAt:       r.started,
		Completed:       t.Completed,
		InFlight:        t.InFlight,
		Succeeded:       t.Succeeded,
		Failed:          t.Failed,
		SuccessRate:     t.SuccessRate,
		HTTPSuccessRate: t.HTTPSuccessRate,
		AvgDurationMs:   t.AvgDurationMs,
		Recent:          append([]Event(nil), r.recent...),
	}
	r.mu.Unlock()
	s.Archive = r.archive.Stats()
	return s
}

// totalsLocked 取计数快照，调用方持锁。
func (r *Recorder) totalsLocked() Totals {
	t := Totals{
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
	}
	if r.completed > 0 {
		t.Failed = r.completed - r.succeeded
		t.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		t.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		t.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	return t
}

// Wait 是面板长轮询的取数入口：返回 Seq > after 的事件，没有新事件就挂着等，
// 直到有新事件、ctx 取消（浏览器关页面 / 切走）或 maxWait 到期。
//
// after=0 表示「只要游标」：首次连接时前端已经有一份 Snapshot（或归档）在手上，
// 不需要再把窗口里的 300 条重传一遍；返回的 Seq 就是后续增量的起点。
//
// 拿到首批事件后会再等 streamCoalesce 合并同一波，避免并发请求多时一秒里来回七八趟。
func (r *Recorder) Wait(ctx context.Context, after uint64, maxWait time.Duration) Stream {
	if r == nil {
		return Stream{Events: []Event{}}
	}
	if maxWait <= 0 {
		maxWait = DefaultStreamWait
	}
	if maxWait > MaxStreamWait {
		maxWait = MaxStreamWait
	}
	deadline := time.Now().Add(maxWait)
	for {
		r.mu.Lock()
		cur, ready := r.sinceLocked(after)
		ch := r.changed
		r.mu.Unlock()
		if ready {
			// 首次只问游标时不必合并，立刻就回。
			if len(cur.Events) > 0 {
				cur = r.coalesce(ctx, deadline, after)
			}
			return cur
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return cur
		}
		timer := time.NewTimer(remain)
		select {
		case <-ch:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return cur
		case <-timer.C:
			return cur
		}
	}
}

// coalesce 在首批事件之后再等一小会儿，把同一波完成的请求并进同一批。
func (r *Recorder) coalesce(ctx context.Context, deadline time.Time, after uint64) Stream {
	wait := min(streamCoalesce, time.Until(deadline))
	if wait <= 0 {
		return r.peek(after)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	return r.peek(after)
}

// peek 不等待，直接取当前增量。
func (r *Recorder) peek(after uint64) Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, _ := r.sinceLocked(after)
	return s
}

// sinceLocked 按游标取增量。ready=false 表示「现在没有新东西」，调用方可以放心去等；
// 游标对不上（重启 / 落后太多）时 ready=true 且 Reset=true，带回窗口内全部事件。
//
// Events 一律倒序（新的在前），与 Snapshot.Recent 的口径一致，前端直接拼表头即可。
func (r *Recorder) sinceLocked(after uint64) (Stream, bool) {
	s := Stream{Seq: r.seq, Totals: r.totalsLocked(), Events: []Event{}}
	if after == 0 {
		return s, true
	}
	// 重启（seq 归零后重新计数）或窗口已滚过游标：整段重来。
	stale := after > r.seq || (len(r.stream) > 0 && after+1 < r.stream[0].Seq)
	if stale {
		s.Reset = true
		for i := len(r.stream) - 1; i >= 0; i-- {
			s.Events = append(s.Events, r.stream[i])
		}
		return s, true
	}
	for i := len(r.stream) - 1; i >= 0; i-- {
		if r.stream[i].Seq <= after {
			break
		}
		s.Events = append(s.Events, r.stream[i])
	}
	return s, len(s.Events) > 0
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
// 归档关着时返回空（面板改用内存里的最近 100 条），即使磁盘上还留着关之前的文件。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil || !r.archive.Enabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	rows, _, err := logarchive.Read(r.archive, logarchive.Query[Event]{
		Limit: limit, From: filter.From, To: filter.To,
		Time: func(e Event) time.Time { return e.Time },
		Keep: filter.match,
	})
	return rows, err
}

// ArchiveStats 归档的存储状态（不带内存指标，面板「日志留存」用）。
func (r *Recorder) ArchiveStats() ArchiveStats {
	if r == nil {
		return ArchiveStats{}
	}
	return r.archive.Stats()
}

// SetArchive 运行中改归档的开关、保留天数（0 = 永久）和容量上限，面板保存配置即生效。
func (r *Recorder) SetArchive(enabled bool, days int, maxBytes int64) {
	if r == nil {
		return
	}
	r.archive.Reconfigure(enabled, days, maxBytes)
}

// ClearArchive 删掉全部请求归档文件（内存里的指标和最近 100 条不动）。
func (r *Recorder) ClearArchive() error {
	if r == nil {
		return nil
	}
	return r.archive.Clear()
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.archive.Close()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFold(e.Account, f.Account) {
		return false
	}
	if !containsFold(e.Model, f.Model) {
		return false
	}
	if !containsFold(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if !containsFold(e.APIKey, f.APIKey) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
