// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package reqlog

import (
	"context"
	"strings"
	"testing"
	"time"
)

func rec(n int) []Event {
	out := make([]Event, 0, n)
	for i := range n {
		out = append(out, Event{RequestID: "r" + string(rune('a'+i)), Status: 200, OK: true})
	}
	return out
}

func TestWaitCursorZeroReturnsOnlySeq(t *testing.T) {
	r := New(Config{})
	for _, e := range rec(3) {
		r.Record(e)
	}
	s := r.Wait(context.Background(), 0, time.Second)
	if len(s.Events) != 0 {
		t.Fatalf("after=0 不该回事件，实际 %d 条", len(s.Events))
	}
	if s.Seq != 3 {
		t.Fatalf("Seq = %d, want 3", s.Seq)
	}
	if s.Totals.Completed != 3 {
		t.Fatalf("totals.completed = %d, want 3", s.Totals.Completed)
	}
}

func TestWaitReturnsIncrementNewestFirst(t *testing.T) {
	r := New(Config{})
	for _, e := range rec(4) {
		r.Record(e)
	}
	s := r.Wait(context.Background(), 2, time.Second)
	if s.Reset {
		t.Fatal("游标在窗口内，不该 reset")
	}
	if len(s.Events) != 2 {
		t.Fatalf("增量 = %d 条, want 2", len(s.Events))
	}
	if s.Events[0].RequestID != "rd" || s.Events[1].RequestID != "rc" {
		t.Fatalf("应新的在前，实际 %q, %q", s.Events[0].RequestID, s.Events[1].RequestID)
	}
	if s.Events[0].Seq != 4 {
		t.Fatalf("事件应带 seq，实际 %d", s.Events[0].Seq)
	}
}

func TestWaitBlocksUntilNextRecord(t *testing.T) {
	r := New(Config{})
	r.Record(Event{RequestID: "first", Status: 200, OK: true})
	done := make(chan Stream, 1)
	go func() { done <- r.Wait(context.Background(), 1, 3*time.Second) }()

	select {
	case s := <-done:
		t.Fatalf("没有新事件时应挂住，却立即返回 %+v", s)
	case <-time.After(80 * time.Millisecond):
	}
	r.Record(Event{RequestID: "second", Status: 200, OK: true})
	select {
	case s := <-done:
		if len(s.Events) != 1 || s.Events[0].RequestID != "second" {
			t.Fatalf("醒来后应拿到 second，实际 %+v", s.Events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Record 后没有唤醒等待方")
	}
}

func TestWaitContextCancelDoesNotHang(t *testing.T) {
	r := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	s := r.Wait(ctx, 0, 10*time.Second)
	if len(s.Events) != 0 {
		t.Fatalf("取消时应回空增量，实际 %+v", s.Events)
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Fatalf("ctx 取消后应立即返回，实际等了 %s", elapsed)
	}
}

func TestWaitResetsWhenCursorFallsBehindWindow(t *testing.T) {
	r := New(Config{})
	for _, e := range rec(streamLimit + 10) {
		r.Record(e)
	}
	s := r.Wait(context.Background(), 1, time.Second)
	if !s.Reset {
		t.Fatal("游标已滚出窗口，应 reset")
	}
	if len(s.Events) != streamLimit {
		t.Fatalf("reset 应带回整窗 %d 条，实际 %d", streamLimit, len(s.Events))
	}
	if s.Seq != uint64(streamLimit+10) {
		t.Fatalf("Seq = %d", s.Seq)
	}
}

func TestWaitResetsAfterRestart(t *testing.T) {
	r := New(Config{})
	r.Record(Event{RequestID: "a", Status: 200, OK: true})
	// 前端记着上一次进程的游标：比当前 seq 大。
	s := r.Wait(context.Background(), 99, time.Second)
	if !s.Reset {
		t.Fatal("游标大于当前 seq 说明网关重启过，应 reset")
	}
	if len(s.Events) != 1 || s.Seq != 1 {
		t.Fatalf("reset 应带回现有窗口: %+v seq=%d", s.Events, s.Seq)
	}
}

func TestRecordMasksErrorAndUserAgent(t *testing.T) {
	r := New(Config{})
	r.Record(Event{
		RequestID: "x", Status: 502,
		Error:     "upstream 401: Bearer abcdefghijklmnopqrstuvwxyz failed",
		UserAgent: "curl/8.0 (zhang.san@example.com)",
	})
	s := r.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("recent = %d", len(s.Recent))
	}
	got := s.Recent[0]
	if strings.Contains(got.Error, "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("失败原因里的凭证没脱敏: %q", got.Error)
	}
	if !strings.Contains(got.Error, "Bearer abcd***wxyz") {
		t.Errorf("失败原因应保留可辨识前缀: %q", got.Error)
	}
	if strings.Contains(got.UserAgent, "zhang.san@") {
		t.Errorf("UA 里的邮箱没脱敏: %q", got.UserAgent)
	}
}
