// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// compactionRecorder 假上游：记下每次发给官方的请求体，固定回一段 SSE（网关对上游一律按流式聚合，
// 非流式请求也是，见 upstream.Aggregate）。
type compactionRecorder struct {
	mu     sync.Mutex
	bodies []string
	// reply 是 SSE 里的 data 行；空则回「这是交接摘要」+ usage。
	reply string
}

func (r *compactionRecorder) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			r.mu.Lock()
			r.bodies = append(r.bodies, string(raw))
			reply := r.reply
			r.mu.Unlock()
			if reply == "" {
				reply = compactionSSE(`{"content":"这是交接摘要"}`, compactionUsage)
			}
			return &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:   io.NopCloser(strings.NewReader(reply))}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// compactionUsage 末帧 usage（压缩测试用它核对 Responses 的字段换算）。
const compactionUsage = `data: {"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}` + "\n\ndata: [DONE]\n\n"

// compactionSSE 拼一段最小 SSE：一帧正文 + 收尾块。
func compactionSSE(delta, tail string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":` + delta + `}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + tail
}

func (r *compactionRecorder) last(t *testing.T) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		t.Fatal("没有请求打到上游")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(r.bodies[len(r.bodies)-1]), &obj); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v（%s）", err, r.bodies[len(r.bodies)-1])
	}
	return obj
}

func compactionHandler(t *testing.T, rec *compactionRecorder) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: rec.client(),
	})
}

// TestRewriteCompactionInput 压缩相关项的改写：控制项丢掉、摘要回放成 user 消息、压缩回合补提示词
// 并去掉 tools、客户端自己要求过摘要就不重复补、没碰到压缩项时一个字节都不动。
func TestRewriteCompactionInput(t *testing.T) {
	summary := "之前在做接口对齐"
	blob := encodeCompactionSummary(summary)
	if got := decodeCompactionSummary(blob); got != summary {
		t.Fatalf("摘要编解码 = %q，应为 %q", got, summary)
	}
	if got := decodeCompactionSummary("plain foreign text"); got != "plain foreign text" {
		t.Fatalf("解不出来应当原样保留，得到 %q", got)
	}

	// 压缩回合：控制项丢掉、补摘要提示词、去掉 tools。
	in := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"compaction_trigger"}],"tools":[{"type":"function","name":"shell"}],"tool_choice":"auto"}`
	out, turn, err := rewriteCompactionInput([]byte(in), false)
	if err != nil || !turn {
		t.Fatalf("应识别为压缩回合: turn=%v err=%v", turn, err)
	}
	obj := mustObj(t, out)
	items := asSlice(obj["input"])
	if len(items) != 2 {
		t.Fatalf("压缩回合 input 应为「原消息 + 摘要提示词」两条: %+v", items)
	}
	if got := itemText(asMap(items[1])); !strings.Contains(got, compactionPromptMarker) {
		t.Errorf("末尾应有摘要提示词: %q", got)
	}
	for _, raw := range items {
		if asString(asMap(raw)["type"]) == "compaction_trigger" {
			t.Error("compaction_trigger 应被丢掉")
		}
	}
	if obj["tools"] != nil || obj["tool_choice"] != nil {
		t.Errorf("压缩回合不该带 tools / tool_choice: %+v", obj)
	}

	// 回放：摘要插成 user 消息，且不算压缩回合。
	in = `{"model":"m","input":[{"type":"compaction","encrypted_content":"` + blob + `"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}]}`
	out, turn, err = rewriteCompactionInput([]byte(in), false)
	if err != nil || turn {
		t.Fatalf("回放不是压缩回合: turn=%v err=%v", turn, err)
	}
	items = asSlice(mustObj(t, out)["input"])
	if len(items) != 2 || asString(asMap(items[0])["role"]) != "user" {
		t.Fatalf("回放应把 compaction 换成 user 消息: %+v", items)
	}
	if got := itemText(asMap(items[0])); !strings.Contains(got, summary) {
		t.Errorf("回放消息应含摘要: %q", got)
	}

	// 客户端自己带着摘要要求（旧版 /compact）时不再补一条。
	in = `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + compactionPromptMarker + `: please"}]}]}`
	out, _, _ = rewriteCompactionInput([]byte(in), true)
	if n := strings.Count(string(out), compactionPromptMarker); n != 1 {
		t.Fatalf("不该重复补摘要提示词，出现 %d 次: %s", n, out)
	}

	// input 直接是字符串的压缩信号。
	out, turn, _ = rewriteCompactionInput([]byte(`{"model":"m","input":"compaction_trigger"}`), false)
	if !turn || mustObj(t, out)["input"] == nil {
		t.Fatalf("字符串形态的压缩信号: turn=%v out=%s", turn, out)
	}

	// 没碰到压缩项：原样返回（前缀缓存要靠字节不变）。
	plain := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out, turn, err = rewriteCompactionInput([]byte(plain), false)
	if err != nil || turn || string(out) != plain {
		t.Fatalf("不含压缩项应原样返回: turn=%v out=%s", turn, out)
	}

	if _, _, err := rewriteCompactionInput([]byte("{bad"), false); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

// TestCompactBodyResponses 压缩回合端到端：上游拿到的是不带 tools 的聊天请求、末尾带摘要提示词，
// 客户端拿回恰好一条 compaction 输出项，摘要能解出来。
func TestCompactBodyResponses(t *testing.T) {
	rec := &compactionRecorder{}
	h := compactionHandler(t, rec)
	body := `{"model":"glm-5.2","stream":false,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"改一下登录页"}]},
		{"type":"compaction_trigger"}],"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`
	res := post(h, "/v1/responses", body)
	if res.Code != http.StatusOK {
		t.Fatalf("压缩回合 status=%d body=%s", res.Code, res.Body)
	}
	obj := mustObj(t, res.Body.Bytes())
	if asString(obj["object"]) != "response" || asString(obj["status"]) != "completed" {
		t.Fatalf("压缩响应外壳不对: %+v", obj)
	}
	output := asSlice(obj["output"])
	if len(output) != 1 || asString(asMap(output[0])["type"]) != "compaction" {
		t.Fatalf("压缩回合应只输出一条 compaction 项: %+v", output)
	}
	if got := decodeCompactionSummary(asString(asMap(output[0])["encrypted_content"])); got != "这是交接摘要" {
		t.Fatalf("摘要 = %q", got)
	}
	if usage := asMap(obj["usage"]); asInt(usage["input_tokens"]) != 11 || asInt(usage["output_tokens"]) != 5 {
		t.Errorf("用量应换成 Responses 字段名: %+v", obj["usage"])
	}

	sent := rec.last(t)
	if sent["tools"] != nil {
		t.Errorf("压缩回合不该带 tools 上去: %+v", sent["tools"])
	}
	if sent["tool_choice"] != nil {
		t.Errorf("压缩回合不该带 tool_choice: %v", sent["tool_choice"])
	}
	msgs := asSlice(sent["messages"])
	if len(msgs) == 0 {
		t.Fatalf("上游没收到消息: %+v", sent)
	}
	if last := itemText(asMap(msgs[len(msgs)-1])); !strings.Contains(last, compactionPromptMarker) {
		t.Errorf("最后一条应是摘要提示词: %q", last)
	}
}

// TestResponsesCompactEndpoint 旧版 /v1/responses/compact：摘要提示词由客户端给，网关不再追加。
func TestResponsesCompactEndpoint(t *testing.T) {
	rec := &compactionRecorder{}
	h := compactionHandler(t, rec)
	prompt, _ := json.Marshal(compactionPrompt)
	body := `{"model":"glm-5.2","stream":true,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"history"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(prompt) + `}]}]}`
	res := post(h, "/v1/responses/compact", body)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body)
	}
	// stream:true → 四帧 SSE，压缩项在 output_item.done 里。
	raw := res.Body.String()
	if !strings.Contains(raw, "event: response.output_item.done") || !strings.Contains(raw, `"type":"compaction"`) ||
		!strings.Contains(raw, "event: response.completed") {
		t.Fatalf("压缩流式帧不对: %s", raw)
	}
	if strings.Contains(raw, "response.output_item.added") {
		t.Error("不该补 output_item.added（Codex 的收集器会拿到空摘要）")
	}
	sent := rec.last(t)
	if n := strings.Count(rec.bodies[len(rec.bodies)-1], compactionPromptMarker); n != 1 {
		t.Fatalf("客户端自己带了摘要要求，不该再补：出现 %d 次", n)
	}
	if len(asSlice(sent["messages"])) == 0 {
		t.Errorf("上游应收到聊天消息: %+v", sent)
	}
}

// TestResponsesReplayCompaction 回放压缩项：摘要要作为上下文进上游，普通轮不算压缩回合。
func TestResponsesReplayCompaction(t *testing.T) {
	rec := &compactionRecorder{reply: compactionSSE(`{"content":"继续干活"}`, compactionUsage)}
	h := compactionHandler(t, rec)
	blob := encodeCompactionSummary("上次做到第 3 步")
	body := `{"model":"glm-5.2","stream":false,"input":[
		{"type":"compaction","encrypted_content":"` + blob + `"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"接着上一步"}]}]}`
	res := post(h, "/v1/responses", body)
	if res.Code != http.StatusOK {
		t.Fatalf("回放 status=%d body=%s", res.Code, res.Body)
	}
	msgs := asSlice(rec.last(t)["messages"])
	if len(msgs) < 2 {
		t.Fatalf("回放消息没进上游: %+v", msgs)
	}
	var joined strings.Builder
	for _, m := range msgs {
		joined.WriteString(itemText(asMap(m)))
	}
	if !strings.Contains(joined.String(), "上次做到第 3 步") {
		t.Fatalf("摘要应作为上下文发给上游: %s", joined.String())
	}
	// 回放这一轮不是压缩回合，模型回答照常翻回 Responses。
	obj := mustObj(t, res.Body.Bytes())
	if asString(obj["object"]) != "response" {
		t.Fatalf("普通轮应回普通 Responses 对象: %+v", obj)
	}
}

// TestCompactionNoSummary 模型没吐正文时如实报错，不要返回一条空摘要（客户端会把空摘要当上下文）。
func TestCompactionNoSummary(t *testing.T) {
	rec := &compactionRecorder{reply: compactionSSE(`{}`, compactionUsage)}
	h := compactionHandler(t, rec)
	res := post(h, "/v1/responses", `{"model":"glm-5.2","input":[{"type":"compaction_trigger"}]}`)
	if res.Code != http.StatusBadGateway || !strings.Contains(res.Body.String(), "compaction") {
		t.Fatalf("空摘要应 502 并说明原因: %d %s", res.Code, res.Body)
	}
}

func mustObj(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("不是合法 JSON: %v（%s）", err, raw)
	}
	return obj
}
