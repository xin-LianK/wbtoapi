// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

// compaction.go Codex 的远程上下文压缩（Responses API 的 compaction）。
//
// 官方 OpenAI 的做法：客户端发整段保留的历史 + 一个 {"type":"compaction_trigger"} 控制项，服务端
// 自己把历史压成一条 compaction 输出项（encrypted_content 是不透明串），客户端把这条原样存下来、
// 下一轮原样回放，服务端再从里面恢复摘要当上下文。上游只认聊天格式，没有这个概念，所以网关自己做：
//
//  1. 压缩回合：把历史翻成聊天请求（普通消息照转，compaction_trigger 丢掉），末尾补一段固定的
//     交接摘要提示词，并且**不带 tools**（这一轮是要摘要，不是要继续干活）；拿回来的正文包成
//     恰好一条 compaction 项返回，encrypted_content 里放我们自己编码的摘要。
//  2. 回放：请求里出现 compaction 项时，把摘要解出来插成一条 user 消息，再接后面的历史。这样
//     压缩过的会话才能继续，否则旧的上下文就断了。
//
// 摘要为什么插成 user 而不是 system：网关默认的提示词模式会删掉客户端所有 system/developer 消息
// 再插自己的（见 internal/prompt.Rewrite），插成 system 会被删掉，摘要就丢了。
//
// 协议依据：CLIProxyAPI 的 codex_executor 压缩用例（compaction_trigger、response.compaction）
// 与本项目 docs/official-client.md 的记录；回放语义对齐同源 Python 实现。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// compactionPrompt 压缩回合的摘要提示词。客户端自己带了摘要要求时（旧版 /responses/compact 把
// 提示词放在最后一条 user 消息里）不再追加，否则会要求模型摘要两次。
const compactionPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.

Include:
- Current progress and key decisions made
- Important context, constraints, or user preferences
- What remains to be done (clear next steps)
- Any critical data, examples, or references needed to continue

Be concise, structured, and focused on helping the next LLM seamlessly continue the work. Return only the summary.`

// compactionPromptMarker 判「客户端自己要求摘要」用的标记（比整段逐字比对宽松，大小写不敏感）。
const compactionPromptMarker = "CONTEXT CHECKPOINT COMPACTION"

// compactionReplayPrefix 回放摘要时的引导句，让模型知道这是上一段会话的交接摘要，不是新的用户要求。
const compactionReplayPrefix = "Summary of the earlier conversation (context checkpoint, not a new request):\n\n"

// compactionItemTypes 各客户端给「上一轮压缩结果」用的 item 类型（Codex CLI / Desktop 都是
// compaction，另外两种是别的客户端的历史叫法）。
var compactionItemTypes = map[string]bool{
	"compaction":         true,
	"compaction_summary": true,
	"context_compaction": true,
}

// rewriteCompactionInput 改写 Responses 请求体里的压缩相关项，返回改写后的 body 和「这一轮是不是
// 压缩回合」。forceTurn 给 /v1/responses/compact 用：那个端点不管有没有 compaction_trigger 都是压缩。
//
//   - compaction 项 → 解出摘要插成一条 user 消息（回放）；
//   - compaction_trigger → 丢掉（它没有内容，只是要求压缩的信号）；
//   - 压缩回合：客户端没自己要求摘要时末尾补一段提示词，并且去掉 tools / tool_choice /
//     parallel_tool_calls，免得模型拿这一轮去调工具。
//
// body 不是合法 JSON 时返回 error，body 不含压缩相关项时原样返回。
func rewriteCompactionInput(body []byte, forceTurn bool) ([]byte, bool, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, forceTurn, err
	}
	turn, changed := forceTurn, false
	switch input := obj["input"].(type) {
	case string:
		if strings.TrimSpace(input) == "compaction_trigger" {
			turn, changed = true, true
			delete(obj, "input")
		}
	case []any:
		items := make([]any, 0, len(input)+1)
		for _, raw := range input {
			item, _ := raw.(map[string]any)
			typ := asString(item["type"])
			switch {
			case typ == "compaction_trigger":
				turn, changed = true, true
			case compactionItemTypes[typ]:
				changed = true
				if summary := decodeCompactionSummary(asString(item["encrypted_content"])); strings.TrimSpace(summary) != "" {
					items = append(items, replayCompactionMessage(summary))
				}
			default:
				items = append(items, raw)
			}
		}
		obj["input"] = items
	}
	if turn {
		if !hasCompactionPrompt(obj["input"]) {
			obj["input"] = append(asSlice(obj["input"]), summarizationMessage())
			changed = true
		}
		for _, k := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
			if _, ok := obj[k]; ok {
				delete(obj, k)
				changed = true
			}
		}
	}
	if !changed {
		return body, turn, nil
	}
	patched, err := json.Marshal(obj)
	if err != nil {
		return body, turn, nil
	}
	return patched, turn, nil
}

// replayCompactionMessage 把上一轮的摘要变成一条 user 消息（理由见文件头）。
func replayCompactionMessage(summary string) map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []any{map[string]any{"type": "input_text", "text": compactionReplayPrefix + strings.TrimSpace(summary)}},
	}
}

// summarizationMessage 压缩回合末尾那条「请写交接摘要」的消息。
func summarizationMessage() map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []any{map[string]any{"type": "input_text", "text": compactionPrompt}},
	}
}

// hasCompactionPrompt 客户端是不是自己已经要求摘要了：看最后一条 user 消息里有没有那个标记。
func hasCompactionPrompt(input any) bool {
	items := asSlice(input)
	for i := len(items) - 1; i >= 0; i-- {
		item := asMap(items[i])
		if item == nil || asString(item["role"]) != "user" {
			continue
		}
		text := strings.ToUpper(itemText(item))
		return strings.Contains(text, compactionPromptMarker)
	}
	return false
}

// itemText 取一条 input 项的正文（content 可能是字符串，也可能是 input_text 分片数组）。
func itemText(item map[string]any) string {
	switch c := item["content"].(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			if p := asMap(part); p != nil {
				sb.WriteString(asString(p["text"]))
				sb.WriteString("\n")
			}
		}
		return sb.String()
	}
	return ""
}

// encodeCompactionSummary 把摘要打成一个不透明串（Codex 原样存、原样回放，不解释内容）。
func encodeCompactionSummary(text string) string {
	raw, err := json.Marshal(map[string]any{"v": 1, "summary": strings.TrimSpace(text)})
	if err != nil {
		return text
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// decodeCompactionSummary 解开 encodeCompactionSummary 的结果。解不出来时把原文当摘要：宁可多一段
// 文本，也不要丢掉唯一一份上下文。
func decodeCompactionSummary(blob string) string {
	text := strings.TrimSpace(blob)
	if text == "" {
		return ""
	}
	if raw, err := base64.StdEncoding.DecodeString(text); err == nil {
		var obj struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(raw, &obj) == nil && strings.TrimSpace(obj.Summary) != "" {
			return obj.Summary
		}
	}
	if raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(text, "=")); err == nil {
		var obj struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(raw, &obj) == nil && strings.TrimSpace(obj.Summary) != "" {
			return obj.Summary
		}
	}
	return text
}

// responsesCompact POST /v1/responses/compact（Codex 旧版压缩入口）：这一轮固定是压缩，摘要提示词由
// 客户端自己放在最后一条 user 消息里。
func (h *Handler) responsesCompact(w http.ResponseWriter, r *http.Request) {
	format := sdktranslator.FormatOpenAIResponse
	body, err := readRequestBody(r)
	if err != nil {
		writeBridgeError(w, format, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	patched, _, err := rewriteCompactionInput(body, true)
	if err != nil {
		writeBridgeError(w, format, http.StatusBadRequest, "invalid_request", "request body is not valid JSON: "+err.Error())
		return
	}
	h.compactBody(w, r, patched, peekStream(body))
}

// compactBody 跑一次压缩回合：把改写好的请求交给完整的聊天链路（选号、粘性、换号、用量都照旧），
// 拿回正文当摘要，再按 Codex 要的形状写出一条 compaction 输出项。
func (h *Handler) compactBody(w http.ResponseWriter, r *http.Request, body []byte, stream bool) {
	format := sdktranslator.FormatOpenAIResponse
	model, routed, _, ok := h.bridgePrologue(w, r, format, body)
	if !ok {
		return
	}
	env := translators.TranslateRequestEnvelope(r.Context(), format, sdktranslator.FormatOpenAI, sdktranslator.RequestEnvelope{
		Format: format, Model: model, Stream: false, Body: body,
	})
	if env.Err != nil {
		writeBridgeError(w, format, http.StatusBadRequest, "invalid_request", env.Err.Error())
		return
	}
	// 压缩要拿完整正文：让 chatCompletions 按非流式把结果攒成一个 JSON 写回来（上游那边照样被
	// PrepareBody 改成流式，网关一律按聚合处理，这一点不由这里决定）。
	chat := forceNonStream(env.Body)
	ctx := r.Context()
	if routed {
		ctx = context.WithValue(ctx, routedKey{}, true)
	}
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))
	cw := &compactionCapture{}
	h.chatCompletions(cw, r2)
	if cw.status != http.StatusOK {
		code, msg := openAIErrorText(cw.buf.Bytes())
		writeBridgeError(w, format, cw.status, code, msg)
		return
	}
	summary, usage := chatCompletionSummary(cw.buf.Bytes())
	if summary == "" {
		writeBridgeError(w, format, http.StatusBadGateway, "compaction_failed",
			"compaction produced no summary (the model returned no text); retry")
		return
	}
	writeCompactionResponse(w, model, summary, stream, usage)
}

// forceNonStream 把聊天请求体改成非流式。上游那边一律被 PrepareBody 改成流式（网关总是按聚合读），
// 这里改的是 chatCompletions 的回写形态：压缩要拿完整正文，不要让它在 captureWriter 上吐 SSE。
func forceNonStream(chat []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(chat, &obj) != nil {
		return chat
	}
	obj["stream"] = false
	delete(obj, "stream_options")
	out, err := json.Marshal(obj)
	if err != nil {
		return chat
	}
	return out
}

// chatCompletionSummary 从聊天格式的非流式响应里取正文和用量。
func chatCompletionSummary(raw []byte) (string, any) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	if json.Unmarshal(raw, &resp) != nil || len(resp.Choices) == 0 {
		return "", nil
	}
	return strings.TrimSpace(itemText(map[string]any{"content": resp.Choices[0].Message.Content})), resp.Usage
}

// writeCompactionResponse 写压缩结果：非流式一个 response 对象，流式四帧 SSE。
// Codex 的压缩收集器只认 response.output_item.done 里的 item，所以这里不补 output_item.added
// （补了反而会拿到一条空摘要）。
func writeCompactionResponse(w http.ResponseWriter, model, summary string, stream bool, usage any) {
	item := map[string]any{
		"id":                compactionID("cmp_"),
		"type":              "compaction",
		"encrypted_content": encodeCompactionSummary(summary),
	}
	base := map[string]any{
		"id":          compactionID("resp_"),
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"model":       model,
		"output":      []any{item},
		"output_text": "",
		"metadata":    map[string]any{},
	}
	if !stream {
		obj := cloneJSONMap(base)
		obj["status"] = "completed"
		obj["usage"] = responsesUsage(usage)
		out, err := json.Marshal(obj)
		if err != nil {
			writeBridgeError(w, sdktranslator.FormatOpenAIResponse, http.StatusInternalServerError, "server_error", "encode response: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}

	running := cloneJSONMap(base)
	running["status"] = "in_progress"
	running["output"] = []any{}
	done := cloneJSONMap(base)
	done["status"] = "completed"
	done["usage"] = responsesUsage(usage)
	events := []struct {
		name    string
		payload map[string]any
	}{
		{"response.created", map[string]any{"response": running}},
		{"response.in_progress", map[string]any{"response": running}},
		{"response.output_item.done", map[string]any{"output_index": 0, "item": item}},
		{"response.completed", map[string]any{"response": done}},
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	var buf bytes.Buffer
	for i, ev := range events {
		data := map[string]any{"type": ev.name, "sequence_number": i + 1}
		for k, v := range ev.payload {
			data[k] = v
		}
		raw, err := json.Marshal(data)
		if err != nil {
			continue
		}
		buf.WriteString("event: " + ev.name + "\ndata: ")
		buf.Write(raw)
		buf.WriteString("\n\n")
	}
	_, _ = w.Write(buf.Bytes())
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// responsesUsage 把聊天格式的用量换成 Responses 的字段名（Codex 只用来显示花了多少）。
func responsesUsage(raw any) map[string]any {
	usage := asMap(raw)
	in, out := asInt(usage["prompt_tokens"]), asInt(usage["completion_tokens"])
	total := asInt(usage["total_tokens"])
	if total == 0 {
		total = in + out
	}
	return map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": total}
}

// compactionID 生成 resp_ / cmp_ 开头的随机 ID。
func compactionID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err == nil {
		return prefix + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}

// peekStream 客户端这次要不要流式（压缩回合按它决定回 JSON 还是回 SSE）。
func peekStream(body []byte) bool {
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)
	return peek.Stream
}

// cloneJSONMap 浅拷贝一层，改副本不串到原件（events 里 base 会以不同状态出现多次）。
func cloneJSONMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// compactionCapture 接住聊天链路的非流式响应，压缩回合要的是正文而不是翻好的 Responses 对象。
type compactionCapture struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (c *compactionCapture) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}

func (c *compactionCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *compactionCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(p)
}

func (c *compactionCapture) Flush() {}
