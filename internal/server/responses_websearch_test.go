// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestRewriteWebSearchTools 客户端宣告的服务端 web_search 换成网关自己的函数工具；
// 没有这类工具时一个字节都不动。
func TestRewriteWebSearchTools(t *testing.T) {
	in := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},
		{"type":"web_search"},{"type":"web_search_preview"}]}`
	out, enabled, err := rewriteWebSearchTools([]byte(in))
	if err != nil || !enabled {
		t.Fatalf("应认出服务端搜索工具: enabled=%v err=%v", enabled, err)
	}
	obj := mustObj(t, out)
	tools := asSlice(obj["tools"])
	if len(tools) != 2 {
		t.Fatalf("两个 web_search 应合并成一个函数工具，实际 tools=%+v", tools)
	}
	last := asMap(tools[1])
	if asString(last["type"]) != "function" || asString(last["name"]) != webSearchToolName {
		t.Fatalf("末尾应是网关的搜索函数: %+v", last)
	}
	if params := asMap(last["parameters"]); asSlice(params["required"])[0] != "query" {
		t.Errorf("参数应只要求 query: %+v", params)
	}

	// tool_choice 指到服务端工具上时改成 auto。
	out, _, _ = rewriteWebSearchTools([]byte(`{"model":"m","input":"hi","tools":[{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`))
	if got := mustObj(t, out)["tool_choice"]; got != "auto" {
		t.Errorf("tool_choice 应改成 auto: %v", got)
	}

	plain := `{"model":"m","input":"hi"}`
	if out, enabled, _ := rewriteWebSearchTools([]byte(plain)); enabled || string(out) != plain {
		t.Fatalf("没有搜索工具应原样返回: enabled=%v out=%s", enabled, out)
	}
	if _, _, err := rewriteWebSearchTools([]byte("{bad")); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

// TestResponsesWebSearch 模型点名要搜时网关自己搜：搜索结果作为工具结果回给上游，最后一轮的正文
// 翻成 Responses 交给客户端。
func TestResponsesWebSearch(t *testing.T) {
	var mu sync.Mutex
	chats := 0
	rig := &searchRig{other: func(string) *http.Response {
		mu.Lock()
		defer mu.Unlock()
		chats++
		if chats == 1 {
			return textResp(200, "text/event-stream", toolSSE(webSearchToolName, `{"query":"go 1.26 发布"}`))
		}
		return textResp(200, "text/event-stream", sseOK)
	}}
	h, _ := newSearchHandler(t, rig)
	body := `{"model":"glm-5.2","stream":false,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go 1.26 有什么新东西"}]}],
		"tools":[{"type":"web_search"}]}`
	rec := post(h, "/v1/responses", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := rig.searches(); len(got) != 1 {
		t.Fatalf("应只搜一次，实际 %v", got)
	}
	// 最后一轮聊天请求（带工具结果的那次）和这次搜索的请求体。
	var searchBody, second string
	rig.mu.Lock()
	for _, c := range rig.calls {
		if c.path == "/agenttool/v1/search" {
			searchBody = c.body
			continue
		}
		second = c.body
	}
	rig.mu.Unlock()
	if !strings.Contains(searchBody, `"query":"go 1.26 发布"`) || !strings.Contains(searchBody, `"max_results":5`) {
		t.Errorf("搜索请求体 = %s", searchBody)
	}
	mu.Lock()
	rounds := chats
	mu.Unlock()
	if rounds != 2 {
		t.Fatalf("应有两轮模型调用，实际 %d", rounds)
	}
	if !strings.Contains(second, `"role":"tool"`) || !strings.Contains(second, "Go 1.26 发布") {
		t.Fatalf("第二轮应带工具结果: %s", second)
	}
	obj := mustObj(t, rec.Body.Bytes())
	if asString(obj["object"]) != "response" || asString(obj["output_text"]) != "你好" {
		t.Fatalf("客户端应拿回翻好的 Responses: %+v", obj)
	}
}

// TestResponsesWebSearchClientTool 同一轮里模型还调了客户端的工具：不代跑，整轮交给客户端（工具调用
// 照旧翻回去），也不发搜索请求。
func TestResponsesWebSearchClientTool(t *testing.T) {
	rig := &searchRig{other: func(string) *http.Response {
		return textResp(200, "text/event-stream", toolSSE("shell", `{"command":["ls"]}`))
	}}
	h, _ := newSearchHandler(t, rig)
	body := `{"model":"glm-5.2","stream":false,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"看下目录"}]}],
		"tools":[{"type":"web_search"},{"type":"function","name":"shell","parameters":{"type":"object"}}]}`
	rec := post(h, "/v1/responses", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := rig.searches(); len(got) != 0 {
		t.Fatalf("要点名调客户端工具时不该代跑搜索: %v", got)
	}
	obj := mustObj(t, rec.Body.Bytes())
	output := asSlice(obj["output"])
	if len(output) == 0 || asString(asMap(output[0])["type"]) != "function_call" {
		t.Fatalf("应把 function_call 交给客户端: %+v", output)
	}
}

// TestResponsesWebSearchStream 流式客户端：先攒完再合成 SSE，事件仍是标准 Responses 流。
func TestResponsesWebSearchStream(t *testing.T) {
	rig := &searchRig{other: func(string) *http.Response {
		return textResp(200, "text/event-stream", sseOK)
	}}
	h, _ := newSearchHandler(t, rig)
	body := `{"model":"glm-5.2","stream":true,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]}],
		"tools":[{"type":"web_search"}]}`
	rec := post(h, "/v1/responses", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, "event: response.completed") || !strings.Contains(raw, "你好") {
		t.Fatalf("流式应给出完整的 Responses 事件: %s", raw)
	}
	// 每个 data 行都必须是合法 JSON（除了 [DONE]）。
	for _, line := range strings.Split(raw, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("非法 data 行: %s", line)
		}
	}
}
