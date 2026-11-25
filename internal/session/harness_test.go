// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package session

import (
	"testing"
	"time"
)

// TestHarnessTrackerSteps 同轮第 1 次是 stepIndex=0 无 previous；提交后下一步递增并指向上一步消息 id；
// 同一 messageID 重复提交幂等；不同轮互不影响；零值/nil 接收者退化为首步。
func TestHarnessTrackerSteps(t *testing.T) {
	h := NewHarnessTracker()
	now := time.Now()
	h.now = func() time.Time { return now }

	if step := h.Peek("r1"); step.Index != 0 || step.PreviousMessageID != "" {
		t.Fatalf("首步应为 0/空，实际 %+v", step)
	}
	h.Commit("r1", "m1")
	if step := h.Peek("r1"); step.Index != 1 || step.PreviousMessageID != "m1" {
		t.Fatalf("提交一次后应 1/m1，实际 %+v", step)
	}
	// 重试复用同一 messageID：幂等，不推进步数。
	h.Commit("r1", "m1")
	if step := h.Peek("r1"); step.Index != 1 {
		t.Fatalf("同一 messageID 重复提交不应推进，实际 %+v", step)
	}
	h.Commit("r1", "m2")
	if step := h.Peek("r1"); step.Index != 2 || step.PreviousMessageID != "m2" {
		t.Fatalf("第二次提交后应 2/m2，实际 %+v", step)
	}
	if step := h.Peek("r2"); step.Index != 0 {
		t.Fatalf("另一轮应各自从 0 开始，实际 %+v", step)
	}

	// TTL 过期：回到首步。
	now = now.Add(defaultHarnessTTL + time.Second)
	if step := h.Peek("r1"); step.Index != 0 || step.PreviousMessageID != "" {
		t.Fatalf("过期后应回到首步，实际 %+v", step)
	}

	// nil / 零值接收者安全：Peek 首步、Commit 不 panic。
	var nilH *HarnessTracker
	nilH.Commit("r1", "m1")
	if step := nilH.Peek("r1"); step.Index != 0 {
		t.Fatalf("nil 接收者应退化首步，实际 %+v", step)
	}
	var zero HarnessTracker
	zero.Commit("r1", "m1")
	if step := zero.Peek("r1"); step.Index != 1 || step.PreviousMessageID != "m1" {
		t.Fatalf("零值接收者应可用，实际 %+v", step)
	}
}
