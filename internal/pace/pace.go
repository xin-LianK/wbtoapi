// Package pace 控制出站到上游的请求节奏（账号维度）。
//
// 为什么需要这一层：pool 的在途租约（max_in_flight）限制的是「同时有几个请求
// 在路上」，但它对**速率**完全不设防——3 个在途名额配短请求（如 200ms 就返回
// 的 fast-model），一秒能打出去十几次。官方客户端不存在这种节奏：人打字再快
// 也不会 24 小时每秒十次。上游的风控模型（IP 级 403、账号级 11128）看的正是
// 这种「不像人」的分布，而不是单次请求长什么样。
//
// 本包只做三件事，全部为账号维度、进程内状态：
//  1. MinInterval：同一账号两次出站的最小间隔（串行化，排队自然形成节奏）；
//  2. Jitter：在间隔上叠加 [0, Jitter) 随机量——固定间隔本身也是指纹
//     （等间距的机器时钟），抖动把它打散成泊松样分布；
//  3. PerMinute：滑动 60s 窗口内的硬上限，防「间隔很小但持续打满」。
//
// 与 rotateBackoff 的分工：那个是**失败后换号**的退避，本包是**成功请求之间**
// 的节奏。两者语义不重叠，失败重试会叠加等待（可接受：重试本就该更慢）。
//
// 全部关闭（Config 零值）时 Wait 直接返回 nil，零开销零回归。
package pace

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Config 出站节奏参数（零值 = 全关）。
type Config struct {
	// MinInterval 同账号两次出站的最小间隔；<=0 不限制。
	MinInterval time.Duration
	// Jitter 在 MinInterval 上叠加的随机上界 [0, Jitter)；<=0 不加抖动。
	Jitter time.Duration
	// PerMinute 单账号滑动 60s 窗口内的请求数上限；<=0 不限制。
	PerMinute int
}

// Disabled 报告是否全关（Wait 走快路径）。
func (c Config) Disabled() bool {
	return c.MinInterval <= 0 && c.Jitter <= 0 && c.PerMinute <= 0
}

// slot 单账号的节奏状态。
type slot struct {
	// nextAt 该账号下一次允许出站的时刻（由锁内串行推进）。
	nextAt time.Time
	// stamps 最近 60s 内已占位的出站时刻，升序（滑动窗口配额用）。
	stamps []time.Time
}

// Limiter 账号维度的出站节奏限制器。零值不可用，须经 New 构造。
type Limiter struct {
	cfg atomic.Value // 存 Config，支持热改

	mu    sync.Mutex
	slots map[string]*slot

	// 以下字段供测试注入确定性行为；生产用默认实现。
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	rnd   func(n int64) int64
}

// New 构造限制器。cfg 可后续经 SetConfig 热改。
func New(cfg Config) *Limiter {
	l := &Limiter{
		slots: make(map[string]*slot),
		now:   time.Now,
		sleep: sleepCtx,
		rnd:   rand.Int63n,
	}
	l.SetConfig(cfg)
	return l
}

// SetConfig 热更新参数（原子替换；在途 Wait 已取到的快照不受影响）。
func (l *Limiter) SetConfig(cfg Config) {
	if l == nil {
		return
	}
	l.cfg.Store(cfg)
}

// Config 返回当前生效参数。
func (l *Limiter) Config() Config {
	if l == nil {
		return Config{}
	}
	v := l.cfg.Load()
	if v == nil {
		return Config{}
	}
	return v.(Config)
}

// Wait 在 key（账号 UID）维度申请一次出站许可，必要时阻塞到允许时刻。
//
// 语义要点：
//   - 占位是**锁内串行推进**的：并发请求按到达顺序排成队列，第 N 个请求的
//     nextAt 是前一个的 nextAt 再加一个间隔，因此节奏不会被并发绕过；
//   - ctx 取消/超时立即返回错误，不把等待拖过客户端耐心；
//   - 取消后不回收已推进的 nextAt 与已记的 stamps——偏差方向是「多等」而非
//     「洪泛」，风控场景下宁可保守。
func (l *Limiter) Wait(ctx context.Context, key string) error {
	if l == nil {
		return nil
	}
	cfg := l.Config()
	if cfg.Disabled() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	now := l.now()

	l.mu.Lock()
	s := l.slots[key]
	if s == nil {
		s = &slot{}
		l.slots[key] = s
	}

	at := now
	if s.nextAt.After(at) {
		at = s.nextAt
	}

	if cfg.PerMinute > 0 {
		s.stamps = pruneStamps(s.stamps, now)
		if len(s.stamps) >= cfg.PerMinute {
			// 配额已满：要等到「窗口内第 (len-PerMinute) 个」滑出一分钟，
			// 那时窗口里才空出一个位置。
			if t := s.stamps[len(s.stamps)-cfg.PerMinute].Add(time.Minute); t.After(at) {
				at = t
			}
		}
	}

	// 占位：本次出站时刻 = at；下一次最早 = at + MinInterval + 抖动。
	gap := cfg.MinInterval
	if cfg.Jitter > 0 {
		gap += time.Duration(l.rnd(int64(cfg.Jitter)))
	}
	if gap < 0 {
		gap = 0
	}
	s.nextAt = at.Add(gap)
	if cfg.PerMinute > 0 {
		s.stamps = append(s.stamps, at)
	}
	l.mu.Unlock()

	d := at.Sub(now)
	if d <= 0 {
		return nil
	}
	if err := l.sleep(ctx, d); err != nil {
		return err
	}
	return nil
}

// pruneStamps 剪掉 60s 窗口外的记录（stamps 升序，前缀删除即可）。
func pruneStamps(stamps []time.Time, now time.Time) []time.Time {
	cut := now.Add(-time.Minute)
	i := 0
	for i < len(stamps) && !stamps[i].After(cut) {
		i++
	}
	if i == 0 {
		return stamps
	}
	return stamps[i:]
}

// sleepCtx 可被 ctx 打断的等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
