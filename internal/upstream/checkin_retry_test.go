// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// checkin_retry_test.go 钉住签到/余额维护类计费调用的瞬时错误有界重试：
// 上游 5xx（实测偶发 code 10000 / http 500）与网络抖动重试，业务错误（已签到/
// 参数错）不重试——重试只会原样再失败一次。
package upstream

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// shortBillingRetry 测试用重试间隔（生产 2s/20s 会让单测秒级膨胀）。
func shortBillingRetry(t *testing.T) {
	t.Helper()
	old, oldSoft := billingRetryDelay, softRateRetryDelay
	billingRetryDelay = time.Millisecond
	softRateRetryDelay = time.Millisecond
	t.Cleanup(func() { billingRetryDelay, softRateRetryDelay = old, oldSoft })
}

// checkinClient 签到状态固定回「活动进行中、今天未签」，其余请求交给 fn（只数签到本身）。
func checkinClient(fn func(r *http.Request) (*http.Response, error)) *Client {
	return testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == checkinStatusPath {
			return jsonResp(200, `{"code":0,"data":{"active":true,"today_checked_in":false}}`), nil
		}
		return fn(r)
	})
}

func TestDailyCheckinRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := checkinClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != dailyCheckinPath {
			return nil, errors.New("wrong path")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("首次 500 应重试成功，err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}

func TestDailyCheckinRetriesExhausted(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := checkinClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("持续 500 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls=%d want 3（1 次 + 2 次重试封顶）", n)
	}
}

func TestDailyCheckinNoRetryOnBusinessError(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := checkinClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		// 「今天已签到」是业务幂等拒绝（code!=0），重试无意义。
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !IsAlreadyCheckin(err) {
		t.Fatalf("err=%v want already-checkin business error", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（业务错误不重试）", n)
	}
}

// 429 软限流等长间隔后补打一次：整池号在整点连发时排前面的号常吃 429，
// 但隔几十秒单发是成功的（实测同一出口 6 个号 7 秒内连打，第 2 个被拒）。
func TestDailyCheckinRetriesSoftRate(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := checkinClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != dailyCheckinPath {
			return nil, errors.New("wrong path")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(429, `{"code":14003,"msg":"too many requests"}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("首次 429 应补打成功，err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次 429 + 1 次补打）", n)
	}
}

// 429 只补打一次：限流窗口没退就继续等下去只会把整轮拖长（签到后面还有一串号），
// 失败留给 21:00 那轮，不在一个号上死磕。
func TestDailyCheckinSoftRateRetriedOnlyOnce(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := checkinClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != dailyCheckinPath {
			return nil, errors.New("wrong path")
		}
		atomic.AddInt32(&calls, 1)
		return jsonResp(429, `{"code":14003,"msg":"too many requests"}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("持续 429 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次 + 1 次补打，不再继续）", n)
	}
}

func TestDailyCheckinNoRetryOn4xx(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := checkinClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(403, `{"code":11140,"msg":"request illegal"}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("403 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（4xx 非瞬时，不重试）", n)
	}
}

func TestUserResourceDetailedRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case resourceSummaryPath:
			return jsonResp(200, `{"code":0,"data":{"Packages":[{"CycleTotalCapacity":"100","CycleRemainCapacity":"60"}]}}`), nil
		case resourcePaidPath:
			return jsonResp(200, resourceList()), nil
		case resourceFreePath:
		default:
			return nil, errors.New("wrong path")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}
		return jsonResp(200, resourceList(`{"PackageName":"p","CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"60"}`)), nil
	})
	remain, _, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil || remain != 60 {
		t.Fatalf("remain=%d err=%v, want 60 nil", remain, err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}
