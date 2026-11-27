// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/notify"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
)

// TestNotifyReportNotConfigured 通道或调度器缺席时明确回 501，不静默成功。
func TestNotifyReportNotConfigured(t *testing.T) {
	p := New(Config{Version: "test"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/notify/report", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501（body=%s）", rec.Code, rec.Body)
	}
}

// TestNotifyReportSendsDigest 立即发一条日报：把池里的到期快照拼成通知，正文原样回给调用方。
func TestNotifyReportSendsDigest(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", Nickname: "一号"})
	pl.SetCreditsSnapshot("u1", pool.CreditsSnapshot{
		Remain: 1500, Total: 2000,
		ExpiryBuckets: []pool.ExpiryBucket{
			{Date: "2026-11-28", ExpireAt: time.Date(2026, 11, 28, 23, 59, 0, 0, time.FixedZone("CST", 8*3600)), Remain: 500},
		},
	})
	sch := scheduler.New(scheduler.Config{Pool: pl})
	// Bark 的 device key 在路径里（https://api.day.app/<key>），投递就是 POST 到该地址。
	n := notify.New(notify.Config{Enabled: true, URL: srv.URL + "/devkey", Format: notify.FormatBark, Timeout: 2 * time.Second})

	p := New(Config{Version: "test", Pool: pl, Scheduler: sch, Notify: n})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/notify/report", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if !resp.OK || !strings.Contains(resp.Title, "积分到期日报") {
		t.Fatalf("响应=%+v", resp)
	}
	if !strings.Contains(resp.Text, "一号 · 剩 1,500 分（额度 2,000）") {
		t.Errorf("正文缺号的余额: %s", resp.Text)
	}
	// Bark 载荷里 title/body 都要有，且正文是拼好的日报。
	if !strings.Contains(got, "积分到期日报") || !strings.Contains(got, `"body"`) {
		t.Errorf("推送载荷不对: %s", got)
	}
}
