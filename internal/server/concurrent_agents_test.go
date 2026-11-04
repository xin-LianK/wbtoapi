// concurrent_agents_test.go 多 agent 并发的容量实证。
//
// 场景：5 个账号、max_in_flight=3、3 个 agent 同时发起请求。验证两点——
//  1. 三个请求**全部成功**（并发容量 5×3=15 远大于 3，不该有任何 503/排队失败）；
//  2. 请求被**分散到不同账号**（选号器的 lastUsed 防撞号生效），而不是三个都砸在
//     同一个号上把该号的 3 个名额占满、让第 4 个并发请求无处可去。
package server

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestFiveAccountsThreeConcurrentAgentsAllSucceed(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		calls[authz]++
		mu.Unlock()
		// 故意延迟，让三个请求的「在途窗口」真正重叠（否则第一个可能已返回，
		// 测试就退化成串行，测不到并发行为）。
		time.Sleep(60 * time.Millisecond)
		return 200, sseOK, true
	})

	auths := make([]*auth.Auth, 0, 5)
	for _, uid := range []string{"a1", "a2", "a3", "a4", "a5"} {
		auths = append(auths, &auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
	}
	p := testPoolWith(auths...)
	p.SetMaxInFlight(3) // 与生产 config.json 一致
	h := NewHandler(Config{Pool: p, Upstream: up, StopOnBan: true})

	const agents = 3
	codes := make([]int, agents)
	var wg sync.WaitGroup
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != 200 {
			t.Errorf("agent %d 返回 %d，want 200（5 号 × max_in_flight=3 = 15 在途容量，3 个并发不该失败）", i+1, c)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 {
		t.Errorf("3 个并发请求只用了 %d 个账号（%v），选号未分流——单号 3 名额被瞬间占满，第 4 个并发就会 503", len(calls), calls)
	}
	t.Logf("3 并发实际分布: %v（共 5 个号可用）", calls)
}

// TestFiveAccountsSurviveTwoBanned 冗余度：5 个号里有 2 个被封/禁用时，剩 3 个号
// 仍能承载 3 个 agent 并发（3×3=9 在途容量）。这是「封号保护」之外的第二道保险——
// 封号会减少号数，号数决定并发天花板，所以冗余量要按「最坏情况剩几个号」算。
func TestFiveAccountsSurviveTwoBanned(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		time.Sleep(40 * time.Millisecond)
		return 200, sseOK, true
	})
	auths := make([]*auth.Auth, 0, 5)
	for _, uid := range []string{"a1", "a2", "a3", "a4", "a5"} {
		auths = append(auths, &auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
	}
	p := testPoolWith(auths...)
	p.SetMaxInFlight(3)
	// 模拟两个号已被封禁移出池子（上一版的 stop_on_ban 会做这件事）。
	p.Disable("a1", "test: banned")
	p.Disable("a2", "test: banned")
	h := NewHandler(Config{Pool: p, Upstream: up, StopOnBan: true})

	const agents = 3
	codes := make([]int, agents)
	var wg sync.WaitGroup
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != 200 {
			t.Errorf("agent %d 返回 %d，want 200（剩 3 个号 × 3 名额 = 9，仍够 3 并发）", i+1, c)
		}
	}
}
