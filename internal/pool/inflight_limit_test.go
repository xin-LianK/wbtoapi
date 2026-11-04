// inflight_limit_test.go 在途并发上限的行为契约（用户问「3 个 agent 同步跑会不会
// 被限」的权威答案）。
//
// 要点：max_in_flight 限的是**单账号**同时在途数，不是全池/全局总量。因此实际能否
// 并行取决于「池里有多少个健康号」而不是「有几个 agent」。
package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func newInFlightPool(t *testing.T, limit int) (*Pool, *auth.Auth) {
	t.Helper()
	p := New("")
	p.SetMaxInFlight(limit)
	a := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	p.Add(a)
	return p, a
}

// TestInFlightLimitIsPerAccount 单号上限是硬上限：连续占满后必须拒绝，释放后立刻可再占。
// 这条是「3 个 agent 不会互相踩踏」的基础——只要池里号数 ≥ 并发数就各占各的。
func TestInFlightLimitIsPerAccount(t *testing.T) {
	p, _ := newInFlightPool(t, 3)

	for i := 0; i < 3; i++ {
		if !p.Acquire("u1") {
			t.Fatalf("第 %d 个在途名额被拒（limit=3 应允许 3 个并发）", i+1)
		}
	}
	// 第 4 个：单号上限 3，必须拒绝（handler 据此换号，全池满则 503）。
	if p.Acquire("u1") {
		t.Fatal("第 4 个在途名额不应被允许（limit=3）")
	}
	// 释放一个后立刻可以再占：说明它是在途租约，不是速率限制。
	p.Release("u1")
	if !p.Acquire("u1") {
		t.Fatal("释放后应能立即再占（在途租约不应变成限速器）")
	}
}

// TestInFlightLimitZeroMeansUnlimited 0 = 不限（老配置兼容）：永远放行，但仍计数供观测。
func TestInFlightLimitZeroMeansUnlimited(t *testing.T) {
	p, _ := newInFlightPool(t, 0)
	for i := 0; i < 50; i++ {
		if !p.Acquire("u1") {
			t.Fatalf("limit=0 应不限，第 %d 次却被拒", i+1)
		}
	}
}

// TestInFlightFullSkipsAccountInPick 名额满的号会被选号器跳过——这是并发安全的
// 关键一环：并发请求不会全部砸在同一个号上撞 Acquire 失败，而是自动分流到别的号。
func TestInFlightFullSkipsAccountInPick(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1) // 便于构造：占 1 个就满
	a1 := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	a2 := &auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999}
	p.Add(a1)
	p.Add(a2)

	if !p.Acquire("u1") {
		t.Fatal("u1 首次占名额应成功")
	}
	// u1 已满 → 选号必须给 u2（若仍返回 u1，并发请求会持续撞 Acquire 失败）。
	for i := 0; i < 5; i++ {
		got := p.PickExcluding(nil)
		if got == nil {
			t.Fatal("还有健康号 u2，不应选不到号")
		}
		if got.UID == "u1" {
			t.Fatalf("u1 在途已满（limit=1），选号不应再返回它")
		}
	}
}

// TestAllAccountsInFlightYieldsNil 全池在途占满 → 选不到号（handler 转 503，
// 不排队）。3 个 agent 配 1 个号且已用满 3 个名额时，第 4 个并发请求就是这条路径。
func TestAllAccountsInFlightYieldsNil(t *testing.T) {
	p, _ := newInFlightPool(t, 3)
	for i := 0; i < 3; i++ {
		if !p.Acquire("u1") {
			t.Fatal("占满前 3 个名额应成功")
		}
	}
	if got := p.PickExcluding(nil); got != nil {
		t.Fatalf("全池在途占满时应选不到号，实际返回 %s", got.UID)
	}
	// 释放后恢复可服务。
	p.Release("u1")
	if got := p.PickExcluding(nil); got == nil {
		t.Fatal("释放名额后应恢复可选中")
	}
}
