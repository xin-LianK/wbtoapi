// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

// responses_websearch.go Responses 入口声明的服务端联网搜索工具。
//
// Codex / Responses SDK 可以把 web_search 当「服务端工具」宣告（{"type":"web_search"}），由服务端自己去搜。
// 上游只认聊天接口，没有这个能力；翻译器又会把非 function 的工具整条丢掉（以前的表现就是模型假装没法
// 搜索）。这里把它桥到网关现成的那条官方搜索（POST {入口}/agenttool/v1/search，见 search.go）：
//
//  1. 把宣告的 web_search 换成同名函数工具（参数只有 query），模型按普通工具调用它；
//  2. 模型点名要搜时，网关自己搜完把结果当工具结果喂回去，再让模型接着写；
//  3. 一旦模型还要调**客户端**的工具，或者搜够轮数，就把这一轮原样交给客户端（工具调用照旧翻回
//     Responses 的 function_call，客户端自己执行）。
//
// 为什么客户端要流式也先攒一轮再合成：搜索结果必须在下一次模型调用之前拿到，中间不能先给客户端吐一半
// 正文。所以内部一律按非流式聚合，最后把结果拼成聊天 SSE 交给同一个翻译器（bridgeWriter），客户端拿到
// 的仍是标准的 Responses 流，只是正文一次到而不是逐步到。
//
// 未做的：web_fetch / file_search 不是这次的范围，仍按原来的方式忽略（宣告被丢掉，模型不知道有这能力）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// errNoChoice 上游响应里没有 choices（拼不出下一轮的 assistant 消息）。
var errNoChoice = errors.New("upstream response has no choices")

// webSearchToolName 网关替客户端执行的那个函数名（和官方服务端工具同名，模型看着自然）。
const webSearchToolName = "web_search"

// responsesSearchTypes 客户端宣告服务端搜索工具时用的 type。
var responsesSearchTypes = map[string]bool{
	"web_search":         true,
	"web_search_preview": true,
}

// webSearchRounds 一次请求最多替客户端搜几轮（每轮是一次额外的模型调用，别让它无限循环）。
const webSearchRounds = 3

// rewriteWebSearchTools 把 requested 的 web_search 换成网关自己执行的函数工具；没有这类工具时原样返回。
func rewriteWebSearchTools(body []byte) (out []byte, enabled bool, err error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false, err
	}
	tools := asSlice(obj["tools"])
	if len(tools) == 0 {
		return body, false, nil
	}
	kept := make([]any, 0, len(tools)+1)
	for _, raw := range tools {
		tool := asMap(raw)
		if tool != nil && responsesSearchTypes[asString(tool["type"])] {
			enabled = true
			continue
		}
		kept = append(kept, raw)
	}
	if !enabled {
		return body, false, nil
	}
	kept = append(kept, webSearchToolDef())
	obj["tools"] = kept
	// 客户端可能把 tool_choice 指到那个服务端工具上；翻译器认不出，改成 auto。
	if tc := asMap(obj["tool_choice"]); tc != nil && responsesSearchTypes[asString(tc["type"])] {
		obj["tool_choice"] = "auto"
	}
	patched, err := json.Marshal(obj)
	if err != nil {
		return body, true, nil
	}
	return patched, true, nil
}

// webSearchToolDef 网关执行的搜索工具定义（参数只有 query，和官方 WebSearch 工具的必填项一致）。
func webSearchToolDef() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        webSearchToolName,
		"description": "Search the web for current information. Returns titles, URLs and snippets for the query.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "The search query."},
			},
			"required": []any{"query"},
		},
	}
}

// responsesWebSearch 带服务端搜索的 Responses 请求：代替客户端执行 web_search，直到模型写出正文或
// 要点名调客户端的工具。
func (h *Handler) responsesWebSearch(w http.ResponseWriter, r *http.Request, body []byte, stream bool) {
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
	ctx := r.Context()
	if routed {
		ctx = context.WithValue(ctx, routedKey{}, true)
	}
	chat := forceNonStream(env.Body)

	var final []byte
	for round := 0; ; round++ {
		cw := &compactionCapture{}
		r2 := r.Clone(ctx)
		r2.Body = io.NopCloser(bytes.NewReader(chat))
		r2.ContentLength = int64(len(chat))
		h.chatCompletions(cw, r2)
		if cw.status != http.StatusOK {
			code, msg := openAIErrorText(cw.buf.Bytes())
			writeBridgeError(w, format, cw.status, code, msg)
			return
		}
		final = cw.buf.Bytes()
		calls, others := webSearchCalls(final)
		if len(calls) == 0 || others || round+1 >= webSearchRounds {
			break
		}
		results := h.runWebSearchCalls(ctx, r, calls, model)
		next, err := appendSearchRound(chat, final, results)
		if err != nil {
			break // 拼不起来就按这一轮的结果交差，不吞掉模型已经写出来的东西
		}
		chat = next
	}
	h.writeResponsesResult(w, ctx, format, model, body, env.Body, final, stream)
}

// webSearchCall 模型要点名执行的一次搜索。
type webSearchCall struct {
	id    string
	query string
}

// webSearchCalls 从聊天响应里挑出网关要执行的搜索调用；others 表示还有客户端自己执行的工具调用
// （那种情况一律不代跑，整轮交给客户端，免得工具序列缺一块）。
func webSearchCalls(resp []byte) (calls []webSearchCall, others bool) {
	var obj struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(resp, &obj) != nil || len(obj.Choices) == 0 {
		return nil, false
	}
	for _, tc := range obj.Choices[0].Message.ToolCalls {
		if tc.Function.Name != webSearchToolName {
			others = true
			continue
		}
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		calls = append(calls, webSearchCall{id: tc.ID, query: strings.TrimSpace(args.Query)})
	}
	return calls, others
}

// runWebSearchCalls 依次执行搜索，返回「工具调用 id → 结果文本」。搜索失败也当结果回给模型（附原因），
// 让模型自己决定怎么说，而不是把整个请求打成错误。
func (h *Handler) runWebSearchCalls(ctx context.Context, r *http.Request, calls []webSearchCall, model string) map[string]string {
	out := make(map[string]string, len(calls))
	meta := searchMeta(r.Header)
	for _, c := range calls {
		q := upstream.SearchQuery{Query: c.query}
		if err := q.Check(); err != nil {
			out[c.id] = "web search failed: " + err.Error()
			continue
		}
		st := newSearchStat(nil, false)
		res, fail := h.runSearch(ctx, q, model, meta, st)
		switch {
		case fail != nil:
			out[c.id] = "web search failed: " + fail.message()
		case res == nil:
			out[c.id] = "web search failed: no result"
		default:
			out[c.id] = formatSearch(c.query, res, time.Now())
		}
		log.Printf("INFO: [server] responses web_search query=%q results=%d", truncateQuery(c.query), resultCount(res))
	}
	return out
}

// truncateQuery 日志里的搜索词截断（搜索词是用户内容，只留开头便于排障）。
func truncateQuery(q string) string {
	if len(q) > 60 {
		return q[:60] + "…"
	}
	return q
}

func resultCount(res *upstream.SearchOutcome) int {
	if res == nil {
		return 0
	}
	return len(res.Results)
}

// appendSearchRound 把这一轮的 assistant 工具调用 + 搜索结果接进聊天请求，供下一次模型调用用。
func appendSearchRound(chat, resp []byte, results map[string]string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(chat, &body); err != nil {
		return nil, err
	}
	var reply map[string]any
	if err := json.Unmarshal(resp, &reply); err != nil {
		return nil, err
	}
	choices := asSlice(reply["choices"])
	if len(choices) == 0 {
		return nil, errNoChoice
	}
	message := asMap(asMap(choices[0])["message"])
	if message == nil {
		return nil, errNoChoice
	}
	msgs := asSlice(body["messages"])
	// 整条 assistant 消息（含 tool_calls）原样接上：上游下一轮要看到是谁请求的这次工具调用。
	msgs = append(msgs, map[string]any{
		"role":       "assistant",
		"content":    message["content"],
		"tool_calls": message["tool_calls"],
	})
	for _, raw := range asSlice(message["tool_calls"]) {
		call := asMap(raw)
		if call == nil {
			continue
		}
		id := asString(call["id"])
		text, ok := results[id]
		if !ok {
			continue
		}
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": id, "content": text})
	}
	body["messages"] = msgs
	return json.Marshal(body)
}

// writeResponsesResult 把最后一轮的聊天响应交给客户端：非流式一次翻好，流式先拼成聊天 SSE 再让同一个
// 翻译器翻成 Responses 事件（客户端拿到的仍是标准 Responses 流）。
func (h *Handler) writeResponsesResult(w http.ResponseWriter, ctx context.Context, format sdktranslator.Format,
	model string, orig, chat, final []byte, stream bool) {
	bw := &bridgeWriter{ctx: ctx, w: w, format: format, model: model, orig: orig, chat: chat}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = bw.Write(final)
		bw.finish()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	bw.WriteHeader(http.StatusOK)
	for _, frame := range synthChatSSE(final) {
		if _, err := bw.Write(frame); err != nil {
			return
		}
	}
	_, _ = bw.Write([]byte("data: [DONE]\n\n"))
	bw.finish()
}

// synthChatSSE 把一次聚合好的聊天响应拼回 SSE（正文、工具调用、收尾与用量各一帧）。
// 客户端看到的是「整段正文一次到达」，不是逐字到达：搜索结果必须在下一次模型调用前拿到，没法边搜边流。
func synthChatSSE(resp []byte) [][]byte {
	var obj map[string]any
	if json.Unmarshal(resp, &obj) != nil {
		return nil
	}
	id := asString(obj["id"])
	if id == "" {
		id = compactionID("chatcmpl-")
	}
	created := asInt(obj["created"])
	if created == 0 {
		created = int(time.Now().Unix())
	}
	chunk := func(choices []any, extra map[string]any) []byte {
		frame := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model": asString(obj["model"]), "choices": choices,
		}
		for k, v := range extra {
			frame[k] = v
		}
		raw, err := json.Marshal(frame)
		if err != nil {
			return nil
		}
		return []byte("data: " + string(raw) + "\n\n")
	}
	choices := asSlice(obj["choices"])
	finish := any("stop")
	message := map[string]any{}
	if len(choices) > 0 {
		choice := asMap(choices[0])
		if fr := choice["finish_reason"]; fr != nil {
			finish = fr
		}
		if m := asMap(choice["message"]); m != nil {
			message = m
		}
	}
	delta := map[string]any{"role": "assistant", "content": asString(message["content"])}
	if rc := asString(message["reasoning_content"]); rc != "" {
		delta["reasoning_content"] = rc
	}
	frames := [][]byte{chunk([]any{map[string]any{"index": 0, "delta": delta}}, nil)}
	if calls := asSlice(message["tool_calls"]); len(calls) > 0 {
		frames = append(frames, chunk([]any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": calls}}}, nil))
	}
	tail := map[string]any{}
	if usage := asMap(obj["usage"]); usage != nil {
		tail["usage"] = usage
	}
	frames = append(frames, chunk([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, tail))
	return frames
}
