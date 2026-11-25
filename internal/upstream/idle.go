// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// idle.go 聊天 SSE 流中空闲监控：有内容帧续命，只有保活/心跳帧时照样掐流（释放租约）。
package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ErrStreamIdle 上游流在空闲窗口内没有内容帧，网关的空闲看门狗掐断了它。
// 单独一个哨兵：调用方据此把「上游停滞」与「客户端断连导致的取消」分开
// （两者都是 context.Canceled，靠错误本身分不出来）。
var ErrStreamIdle = errors.New("upstream stream idle: no content frame within the idle window")

// maxIdleLineBytes 单行缓冲上限：超了就不再累积，避免一条畸形超长行把内存吃光。
const maxIdleLineBytes = 64 * 1024

// idleMonitoringBody 包在聊天 SSE body 外层：
// 读到**内容帧**（data: 里带正文的帧）才刷新 lastContent；后台 goroutine 周期检查，
// 静默超过 idle 就 cancel 请求 context，中断阻塞中的 Read。
//
// 为什么按内容帧而不是按字节：上游会周期发保活帧（SSE 注释行 `: keepalive` 或
// `data: {"type":"ping"}`），按字节计时的话这些帧会把计时器一直刷新，空闲超时等于没有
// （issue #485：客户端 600s 弃连、上游 999s 才结束，网关 300s 的空闲上限一次也没生效）。
type idleMonitoringBody struct {
	rc          io.ReadCloser
	mu          sync.Mutex
	lastContent time.Time
	filter      contentFilter
	stopOnce    sync.Once
	stopCh      chan struct{}
	cancel      context.CancelFunc
	fired       atomic.Bool
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && b.filter.feed(p[:n]) {
		b.mu.Lock()
		b.lastContent = time.Now()
		b.mu.Unlock()
	}
	if err != nil && b.fired.Load() {
		// 看门狗掐的流：换成哨兵错误，调用方才能与「客户端断连」区分。
		return n, ErrStreamIdle
	}
	return n, err
}

// Close 停掉后台 goroutine、取消请求 context、关闭底流，保证无泄漏。
func (b *idleMonitoringBody) Close() error {
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.cancel()
	return b.rc.Close()
}

func (b *idleMonitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastContent)
}

// monitorBody 若 idle<=0 直接返回原底流（禁用空闲监控）；
// 否则包上流中空闲监控。计时从返回 body 之后开始——首字节阶段由
// Transport.ResponseHeaderTimeout 管，这里不抢跑。
func monitorBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	b := &idleMonitoringBody{
		rc:          rc,
		lastContent: time.Now(),
		stopCh:      make(chan struct{}),
		cancel:      cancel,
	}
	go func() {
		t := time.NewTicker(idleTick(idle))
		defer t.Stop()
		for {
			select {
			case <-b.stopCh:
				return
			case <-t.C:
				if b.idleFor() > idle {
					b.fired.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return b
}

// idleTick 返回监控周期：idle/4，钳在 [10ms, 1s]。小 idle 也能快速发现，大小值避免空转。
func idleTick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}

// contentFilter 增量判断读到的字节里有没有内容帧：按行切，只认 `data:` 帧里带正文的。
// 注释行（保活）、空行（帧分隔）、event:/id: 字段与 ping/keepalive/heartbeat 帧都不算。
type contentFilter struct {
	tail []byte // 尚未读到换行的当前行
	long bool   // 当前行已超上限：内容已记过，等换行后再按行判断
}

// feed 喂一段字节，返回这段里有没有内容帧。
// 未闭合的行不判：一行没读完时把字节当内容，会让拆包发的心跳帧把计时器刷回去，
// 白白抵消这个看门狗（comment/心跳帧的换行总在一两帧之内到）。
func (f *contentFilter) feed(p []byte) bool {
	content := false
	buf := p
	if len(f.tail) > 0 {
		buf = append(f.tail, p...)
		f.tail = nil
	}
	for len(buf) > 0 {
		if f.long {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				return content
			}
			buf = buf[i+1:]
			f.long = false
			continue
		}
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			if len(buf) > maxIdleLineBytes {
				// 超长单行：显然在吐数据，记一次内容并停止累积。
				content = true
				f.long = true
				return content
			}
			f.tail = append(f.tail[:0], buf...)
			return content
		}
		if isContentLine(buf[:i]) {
			content = true
		}
		buf = buf[i+1:]
	}
	return content
}

// isContentLine 报告一行 SSE 是不是内容帧。
func isContentLine(line []byte) bool {
	l := bytes.TrimSpace(line)
	if len(l) == 0 || l[0] == ':' {
		return false // 空行/注释（保活）
	}
	if !bytes.HasPrefix(l, []byte("data:")) {
		return false // event:/id:/retry: 等字段行，正文在配对的 data 行里
	}
	payload := bytes.TrimSpace(l[len("data:"):])
	if len(payload) == 0 {
		return false
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return true
	}
	return !isHeartbeatPayload(payload)
}

// isHeartbeatPayload 明确的心跳帧：空对象/null，或没有 choices 但带 ping / keepalive / heartbeat 字样。
func isHeartbeatPayload(payload []byte) bool {
	if bytes.Contains(payload, []byte(`"choices"`)) {
		return false
	}
	switch string(payload) {
	case "{}", "[]", "null":
		return true
	}
	for _, k := range [][]byte{[]byte("ping"), []byte("keepalive"), []byte("heartbeat")} {
		if bytes.Contains(payload, k) {
			return true
		}
	}
	return false
}
