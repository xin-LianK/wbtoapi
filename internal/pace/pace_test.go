package pace

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// newFake 构造使用假时钟与假 sleep 的限制器：sleep 只记录请求时长、不真的等，
// 时钟也不前进——因此可以直接断言「第 N 次调用要求等多久」。
func newFake(cfg Config) (*Limiter, *fakeClock) {
	fc := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := New(cfg)
	l.now = func() time.Time { return fc.now }
	l.sleep = func(ctx context.Context, d time.Duration) error {
		fc.waits = append(fc.waits, d)
		return nil
	}
	l.rnd = func(int64) int64 { return 0 }
	return l, fc
}

type fakeClock struct {
	now   time.Time
	waits []time.Duration
}

func (f *fakeClock) total() (sum time.Duration) {
	for _, w := range f.waits {
		sum += w
	}
	return sum
}

func TestDisabledIsNoop(t *testing.T) {
	l, fc := newFake(Config{})
	for i := 0; i < 5; i++ {
		if err := l.Wait(context.Background(), "u1"); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if len(fc.waits) != 0 {
		t.Fatalf("全关时不应有任何等待，got %v", fc.waits)
	}
}

func TestMinIntervalSerializesSameAccount(t *testing.T) {
	l, fc := newFake(Config{MinInterval: 200 * time.Millisecond})
	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background(), "u1"); err != nil {
			t.Fatalf("Wait #%d: %v", i, err)
		}
	}
	// 第一次立即，第二、三次各等 200ms。
	if len(fc.waits) != 2 {
		t.Fatalf("期望 2 次等待（首次不等待），got %v", fc.waits)
	}
	if fc.waits[0] != 200*time.Millisecond || fc.waits[1] != 400*time.Millisecond {
		t.Fatalf("串行推进不符合预期: %v", fc.waits)
	}
}

func TestDifferentAccountsIndependent(t *testing.T) {
	l, fc := newFake(Config{MinInterval: time.Second})
	if err := l.Wait(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(context.Background(), "u2"); err != nil {
		t.Fatal(err)
	}
	if len(fc.waits) != 0 {
		t.Fatalf("不同账号不应互相排队，got %v", fc.waits)
	}
}

func TestJitterAddsToGap(t *testing.T) {
	l, fc := newFake(Config{MinInterval: 100 * time.Millisecond, Jitter: 50 * time.Millisecond})
	l.rnd = func(n int64) int64 {
		if n != int64(50*time.Millisecond) {
			t.Fatalf("rnd 上界应为 Jitter（%d），got %d", int64(50*time.Millisecond), n)
		}
		return n - 1 // 抖动取上界-1，便于断言
	}
	_ = l.Wait(context.Background(), "u1")
	_ = l.Wait(context.Background(), "u1")
	if len(fc.waits) != 1 {
		t.Fatalf("期望 1 次等待，got %v", fc.waits)
	}
	want := 100*time.Millisecond + 50*time.Millisecond - 1
	if fc.waits[0] != want {
		t.Fatalf("间隔应为 MinInterval+抖动=%v，got %v", want, fc.waits[0])
	}
}

func TestPerMinuteQuota(t *testing.T) {
	l, fc := newFake(Config{PerMinute: 2})
	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background(), "u1"); err != nil {
			t.Fatalf("Wait #%d: %v", i, err)
		}
	}
	if len(fc.waits) != 1 {
		t.Fatalf("前两个应放行、第三个应等待，got %v", fc.waits)
	}
	if fc.waits[0] != time.Minute {
		t.Fatalf("第三个应等到首条记录滑出 60s 窗口，want 1m got %v", fc.waits[0])
	}
}

func TestPerMinutePrunesOldStamps(t *testing.T) {
	l, fc := newFake(Config{PerMinute: 2})
	_ = l.Wait(context.Background(), "u1")
	_ = l.Wait(context.Background(), "u1")
	// 时钟推进 61s：两条旧记录都应被剪掉，配额恢复。
	fc.now = fc.now.Add(61 * time.Second)
	if err := l.Wait(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if len(fc.waits) != 0 {
		t.Fatalf("窗口已滑过不应等待，got %v", fc.waits)
	}
}

func TestContextCancelInterruptsWait(t *testing.T) {
	l := New(Config{MinInterval: 5 * time.Second})
	_ = l.Wait(context.Background(), "u1") // 占位，令下一次必须等
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := l.Wait(ctx, "u1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("期望 context.Canceled，got %v", err)
	}
}

func TestContextDeadlineInterruptsWait(t *testing.T) {
	l := New(Config{MinInterval: 10 * time.Second})
	_ = l.Wait(context.Background(), "u1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := l.Wait(ctx, "u1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期望 DeadlineExceeded，got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("不应真的等满 10s，实际 %v", elapsed)
	}
}

func TestSetConfigHotReload(t *testing.T) {
	l, fc := newFake(Config{MinInterval: time.Second})
	if got := l.Config().MinInterval; got != time.Second {
		t.Fatalf("初始配置错误: %v", got)
	}
	l.SetConfig(Config{MinInterval: 50 * time.Millisecond})
	_ = l.Wait(context.Background(), "u1")
	_ = l.Wait(context.Background(), "u1")
	if len(fc.waits) != 1 || fc.waits[0] != 50*time.Millisecond {
		t.Fatalf("热改后应按新间隔等待，got %v", fc.waits)
	}
	l.SetConfig(Config{})
	_ = l.Wait(context.Background(), "u1")
	if len(fc.waits) != 1 {
		t.Fatalf("关掉后不应再等待，got %v", fc.waits)
	}
}

// TestConcurrentDoesNotBypassPacing 并发压测：20 个 goroutine 同时申请同一账号的
// 许可，节奏必须由锁内串行推进守住——累计等待应等于 0+1+…+19 个间隔。
func TestConcurrentDoesNotBypassPacing(t *testing.T) {
	l, fc := newFake(Config{MinInterval: 10 * time.Millisecond})
	var mu sync.Mutex
	l.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		fc.waits = append(fc.waits, d)
		mu.Unlock()
		return nil
	}
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Wait(context.Background(), "u1"); err != nil {
				t.Errorf("Wait: %v", err)
			}
		}()
	}
	wg.Wait()

	want := time.Duration(n*(n-1)/2) * 10 * time.Millisecond // 0+10+…+190 = 1900ms
	if got := fc.total(); got != want {
		t.Fatalf("并发排队总等待应为 %v，got %v（waits=%d）", want, got, len(fc.waits))
	}
}

func TestNilLimiterSafe(t *testing.T) {
	var l *Limiter
	if err := l.Wait(context.Background(), "u1"); err != nil {
		t.Fatalf("nil Limiter 应直接放行: %v", err)
	}
	if (Config{}).Disabled() != true {
		t.Fatal("零值 Config 应为全关")
	}
}
