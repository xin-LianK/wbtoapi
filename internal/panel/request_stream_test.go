// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package panel

import (
	"net/http"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// 请求记录的实时通道：没接记录器 501；首连只给游标；带游标取增量；wait 到期即返回。
func TestPanelRequestStream(t *testing.T) {
	t.Run("nil 记录器 501", func(t *testing.T) {
		p, _ := newPanelFor(t, Config{})
		autoWantCode(t, get(p, http.MethodGet, "/panel/api/request_stream"), http.StatusNotImplemented)
	})

	rec := reqlog.New(reqlog.Config{})
	rec.Record(reqlog.Event{RequestID: "a", Status: 200, OK: true})
	rec.Record(reqlog.Event{RequestID: "b", Status: 200, OK: true})
	p, _ := newPanelFor(t, Config{RequestLog: rec})

	t.Run("首连只给游标", func(t *testing.T) {
		r := get(p, http.MethodGet, "/panel/api/request_stream?wait=50")
		autoWantCode(t, r, http.StatusOK)
		d := autoDecode(t, r)
		if d["seq"] != float64(2) {
			t.Fatalf("seq = %v, want 2", d["seq"])
		}
		if evs, ok := d["events"].([]any); !ok || len(evs) != 0 {
			t.Fatalf("首连不该回事件: %v", d["events"])
		}
		if totals, _ := d["totals"].(map[string]any); totals["completed"] != float64(2) {
			t.Fatalf("totals 应带总数: %v", d["totals"])
		}
	})

	t.Run("按游标取增量", func(t *testing.T) {
		r := get(p, http.MethodGet, "/panel/api/request_stream?after=1&wait=50")
		autoWantCode(t, r, http.StatusOK)
		d := autoDecode(t, r)
		evs, _ := d["events"].([]any)
		if len(evs) != 1 {
			t.Fatalf("增量 = %d 条, want 1", len(evs))
		}
		first, _ := evs[0].(map[string]any)
		if first["request_id"] != "b" {
			t.Fatalf("增量应是 b，实际 %v", first["request_id"])
		}
	})

	t.Run("没有新事件时按 wait 到期返回", func(t *testing.T) {
		start := time.Now()
		r := get(p, http.MethodGet, "/panel/api/request_stream?after=2&wait=80")
		autoWantCode(t, r, http.StatusOK)
		if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
			t.Fatalf("wait=80 应该挂住，实际 %s 就返回", elapsed)
		}
		d := autoDecode(t, r)
		if evs, _ := d["events"].([]any); len(evs) != 0 {
			t.Fatalf("没有新事件应回空: %v", evs)
		}
	})
}
