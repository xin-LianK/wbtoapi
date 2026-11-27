// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// notify_test.go 钉住 Bark 的投递路径：device key 在路径里，POST 到 {base} 本身。
package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBarkDeliverPostsToKeyPath(t *testing.T) {
	var gotPath string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Config{Enabled: true, URL: srv.URL + "/devkey/", Format: FormatBark})
	if n == nil {
		t.Fatal("bark 通道应可用")
	}
	if err := n.SendSync(LevelInfo, "标题", "正文"); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	// 拼成 /devkey/push 会落到服务端的 /:key/:body 路由，正文被路径段覆盖。
	if gotPath != "/devkey" {
		t.Fatalf("应 POST 到 /devkey，实际 %q", gotPath)
	}
	if got["title"] != "标题" || got["body"] != "正文" || got["group"] != "WorkBuddy" {
		t.Fatalf("推送体 = %+v", got)
	}
}

// URL 里没有 device key 时必须明确报错，而不是把正文推成字面量 "push"。
func TestBarkWithoutDeviceKeyFails(t *testing.T) {
	n := New(Config{Enabled: true, URL: "https://api.day.app", Format: FormatBark})
	if err := n.SendSync(LevelInfo, "t", "b"); err == nil {
		t.Fatal("缺 device key 应报错")
	}
}
