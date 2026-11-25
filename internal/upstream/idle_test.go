// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package upstream

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// TestMonitorBodyIdleCutoff 静默超过 idle（配小值）→ Read 返回错误（context canceled）。
func TestMonitorBodyIdleCutoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 模拟真实 resp.Body：阻塞直到请求 context 被 cancel 才返回 error。
	body := monitorBody(&ctxBoundReader{ctx: ctx}, 50*time.Millisecond, cancel)
	defer body.Close()
	if _, err := body.Read(make([]byte, 16)); err == nil {
		t.Fatal("expect read error after idle cutoff")
	}
}

// TestMonitorBodyRenewsOnActivity 持续有内容帧（总时长 > idle）→ 流不被掐。
func TestMonitorBodyRenewsOnActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 每 30ms 一帧正文；idle=80ms。总读 10 帧耗时 300ms > idle：
	// 若内容续命失效（绝对 deadline 语义）早在 80ms 就被掐断。
	frame := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
	body := monitorBody(&sseReader{ctx: ctx, frame: frame, period: 30 * time.Millisecond, left: 10}, 80*time.Millisecond, cancel)
	defer body.Close()
	buf := make([]byte, len(frame))
	for i := 0; i < 10; i++ {
		if _, err := io.ReadFull(body, buf); err != nil {
			t.Fatalf("read %d: %v (active stream must not be cut)", i, err)
		}
	}
}

// TestMonitorBodyCutsKeepaliveOnly 只有保活/心跳帧时照样掐流（issue #485），且错误是哨兵。
func TestMonitorBodyCutsKeepaliveOnly(t *testing.T) {
	for name, frame := range map[string]string{
		"注释行":  ": keepalive\n\n",
		"心跳帧":  "data: {\"type\":\"ping\"}\n\n",
		"心跳字段": "event: heartbeat\ndata: {}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := monitorBody(&sseReader{ctx: ctx, frame: frame, period: 15 * time.Millisecond, left: 1000}, 60*time.Millisecond, cancel)
			defer body.Close()
			buf := make([]byte, 64)
			var err error
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if _, err = body.Read(buf); err != nil {
					break
				}
			}
			if !errors.Is(err, ErrStreamIdle) {
				t.Fatalf("只发保活帧的流应在 idle 后被掐断并返回 ErrStreamIdle，got %v", err)
			}
		})
	}
}

// TestContentFilter 内容帧判定：正文算、保活/心跳/字段行不算，跨读的拆包帧照常认。
func TestContentFilter(t *testing.T) {
	cases := []struct {
		name  string
		parts []string
		want  bool
	}{
		{"正文帧", []string{"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n"}, true},
		{"结束帧", []string{"data: [DONE]\n"}, true},
		{"注释行", []string{": keepalive\n"}, false},
		{"空行", []string{"\n\n"}, false},
		{"ping 帧", []string{"data: {\"type\":\"ping\"}\n"}, false},
		{"字段行", []string{"event: message\n"}, false},
		{"拆包的正文帧", []string{"data: {\"cho", "ices\":[{}]}\n"}, true},
		{"拆包的注释行", []string{": keep", "alive\n"}, false},
	}
	for _, tc := range cases {
		var f contentFilter
		got := false
		for _, p := range tc.parts {
			if f.feed([]byte(p)) {
				got = true
			}
		}
		if got != tc.want {
			t.Errorf("%s: content=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMonitorBodyDisabledWhenIdleZero idle<=0 直接返回原底流。
func TestMonitorBodyDisabledWhenIdleZero(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := nopCloserBody{Reader: &oneByteReader{}}
	out := monitorBody(rc, 0, cancel)
	if _, ok := out.(nopCloserBody); !ok {
		t.Fatalf("idle<=0 should return underlying body verbatim, got %T", out)
	}
	buf := make([]byte, 1)
	if _, err := out.Read(buf); err != nil || buf[0] != 'a' {
		t.Fatalf("read=%q err=%v", buf, err)
	}
}

// TestMonitorBodyCloseStopsGoroutine Close 停掉 goroutine，无泄漏（等效监控循环 + 短周期）。
func TestMonitorBodyCloseStopsGoroutine(t *testing.T) {
	m := &idleMonitoringBody{
		rc:          &ctxBoundReader{ctx: context.Background()},
		lastContent: time.Now(),
		stopCh:      make(chan struct{}),
		cancel:      func() {},
	}
	stopped := make(chan struct{})
	go func() {
		tk := time.NewTicker(idleTick(40 * time.Millisecond))
		defer tk.Stop()
		for {
			select {
			case <-m.stopCh:
				close(stopped)
				return
			case <-tk.C:
			}
		}
	}()
	m.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("goroutine not stopped after Close")
	}
}

// —— helpers ——

// ctxBoundReader 模拟 net/http resp.Body：Read 阻塞在 ctx.Done() 上，ctx cancel 后返回错误。
type ctxBoundReader struct{ ctx context.Context }

func (r *ctxBoundReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *ctxBoundReader) Close() error { return nil }

// sseReader 每次读吐一帧（frame），吐够 left 次后阻塞到 ctx 取消——模拟「连接还活着但没正文」。
type sseReader struct {
	ctx    context.Context
	frame  string
	period time.Duration
	left   int
}

func (r *sseReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
	}
	if r.left > 0 {
		time.Sleep(r.period)
		r.left--
		return copy(p, r.frame), nil
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *sseReader) Close() error { return nil }

type oneByteReader struct{ done bool }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	p[0] = 'a'
	return 1, nil
}

type nopCloserBody struct{ io.Reader }

func (nopCloserBody) Close() error { return nil }
