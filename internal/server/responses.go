// responses.go OpenAI Responses API 兼容层：POST /v1/responses。
//
// 存在理由：Codex CLI 与新版 OpenAI Agent SDK 走的是 Responses API（`input` /
// `output` / `response.*` 事件），而不是 chat/completions。网关只有 chat 形态时，
// 这类客户端要么接不进来，要么得改代码。
//
// 与 anthropic.go 同一取向：请求转成 chat 形态后交给既有流水线，响应经 sseTap
// 翻译回来；不复制选号/租约/记账逻辑。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// responsesRequest Responses 请求里本网关关心的字段。
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	Stream          bool            `json:"stream"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Tools           []responsesTool `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

// responsesTool Responses 的工具声明是扁平结构（name 在顶层）。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// responsesItem input 数组里的单项（message / function_call / function_call_output）。
type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
}

// responsesPart message content 数组里的单项。
type responsesPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// responsesImageURL 图片地址既可能是字符串，也可能是 {url: "..."}。
func responsesImageURL(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.URL
	}
	return ""
}

// responsesPartsToChat content 数组 → OpenAI content（字符串或 part 数组）。
func responsesPartsToChat(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []responsesPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "":
			if p.Text != "" {
				out = append(out, map[string]any{"type": "text", "text": p.Text})
			}
		case "input_image", "image_url":
			if u := responsesImageURL(p.ImageURL); u != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	return partsOrString(out)
}

// responsesInputToChat 把 input（字符串或 item 数组）转成 OpenAI messages。
//
// function_call / function_call_output 是 Responses 把「工具调用轮」拆成独立 item
// 的形态；chat 侧必须还原成 assistant.tool_calls + role:tool 消息对。连续多个
// function_call 会合并进同一条 assistant 消息（并行工具调用）。
func responsesInputToChat(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []any{map[string]any{"role": "user", "content": s}}
	}
	var items []responsesItem
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]any, 0, len(items))
	// pendingCalls 收集连续 function_call，直到遇到非调用 item 才落成一条 assistant 消息。
	var pendingCalls []any
	flushCalls := func() {
		if len(pendingCalls) == 0 {
			return
		}
		out = append(out, map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": pendingCalls,
		})
		pendingCalls = nil
	}
	for _, it := range items {
		switch it.Type {
		case "function_call":
			args := strings.TrimSpace(it.Arguments)
			if args == "" {
				args = "{}"
			}
			id := it.CallID
			if id == "" {
				id = it.Name
			}
			pendingCalls = append(pendingCalls, map[string]any{
				"id":       id,
				"type":     "function",
				"function": map[string]any{"name": it.Name, "arguments": args},
			})
			continue
		case "function_call_output":
			flushCalls()
			outText := ""
			if len(it.Output) > 0 {
				var str string
				if json.Unmarshal(it.Output, &str) == nil {
					outText = str
				} else {
					outText = string(it.Output)
				}
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": it.CallID,
				"content":      outText,
			})
			continue
		}
		// message item（type 可能为空的老形态）
		flushCalls()
		role := it.Role
		if role == "" {
			role = "user"
		}
		out = append(out, map[string]any{
			"role":    role,
			"content": responsesPartsToChat(it.Content),
		})
	}
	flushCalls()
	return out
}

// responsesToolChoice Responses 的 tool_choice → chat 形态。
func responsesToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none":
			return s
		case "required":
			return "required"
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// responsesToChat 组装 chat 请求体。
func responsesToChat(req *responsesRequest) map[string]any {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	msgs := make([]any, 0, 4)
	if strings.TrimSpace(req.Instructions) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.Instructions})
	}
	msgs = append(msgs, responsesInputToChat(req.Input)...)
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "function" {
				continue // web_search / file_search 等内置工具上游没有对应能力，跳过
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 && string(t.Parameters) != "null" {
				fn["parameters"] = t.Parameters
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if tc := responsesToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}
	return out
}

// responsesToolItem 流式期间累积的单个工具调用。
type responsesToolItem struct {
	index     int
	id        string
	callID    string
	name      string
	args      strings.Builder
	outputIdx int
	open      bool
}

// responsesTranslator OpenAI chat 流 / 整体响应 → Responses API 形态。
type responsesTranslator struct {
	model string
	id    string
	sink  func(string)

	started     bool
	seq         int
	msgItemID   string
	msgOpen     bool
	text        strings.Builder
	toolItems   map[int]*responsesToolItem
	toolOrder   []int
	nextOutput  int
	inputTokens int
	outTokens   int
	finished    bool
	errSent     bool
	// finishReason 上游 finish_reason 原值（用于判定是否需要补工具调用项）。
	finishReason string
}

func newResponsesTranslator(model string) *responsesTranslator {
	return &responsesTranslator{model: model, toolItems: map[int]*responsesToolItem{}}
}

// emit 写一个 SSE 事件（Responses 用 event: + data: 双行形态）。
func (r *responsesTranslator) emit(event string, payload map[string]any) {
	if r.sink == nil {
		return
	}
	payload["type"] = event
	payload["sequence_number"] = r.seq
	r.seq++
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	r.sink("event: " + event + "\ndata: " + string(b) + "\n\n")
}

// responseObject 组装响应对象（流式 created/completed 与非流式共用）。
func (r *responsesTranslator) responseObject(status string, output []any) map[string]any {
	if r.id == "" {
		r.id = "resp_" + randomHex(12)
	}
	if output == nil {
		output = []any{}
	}
	obj := map[string]any{
		"id":         r.id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      r.model,
		"output":     output,
		"parallel_tool_calls": true,
	}
	if status == "completed" {
		obj["usage"] = map[string]any{
			"input_tokens":  r.inputTokens,
			"output_tokens": r.outTokens,
			"total_tokens":  r.inputTokens + r.outTokens,
		}
	}
	return obj
}

func (r *responsesTranslator) start() {
	if r.started {
		return
	}
	r.emit("response.created", map[string]any{"response": r.responseObject("in_progress", nil)})
	r.emit("response.in_progress", map[string]any{"response": r.responseObject("in_progress", nil)})
	r.started = true
}

// textDelta 追加正文并发出 output_text.delta。
func (r *responsesTranslator) textDelta(txt string) {
	if txt == "" {
		return
	}
	if !r.msgOpen {
		r.msgItemID = "msg_" + randomHex(12)
		r.emit("response.output_item.added", map[string]any{
			"output_index": r.nextOutput,
			"item": map[string]any{
				"type":    "message",
				"id":      r.msgItemID,
				"role":    "assistant",
				"status":  "in_progress",
				"content": []any{},
			},
		})
		r.emit("response.content_part.added", map[string]any{
			"item_id":       r.msgItemID,
			"output_index":  r.nextOutput,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
		r.msgOpen = true
	}
	r.text.WriteString(txt)
	r.emit("response.output_text.delta", map[string]any{
		"item_id":       r.msgItemID,
		"output_index":  r.nextOutput,
		"content_index": 0,
		"delta":         txt,
	})
}

// toolDelta 处理工具调用分片。
func (r *responsesTranslator) toolDelta(tc map[string]any) {
	if tc == nil {
		return
	}
	idx := asInt(tc["index"])
	fn := asMap(tc["function"])
	item := r.toolItems[idx]
	if item == nil {
		callID := asString(tc["id"])
		if callID == "" {
			callID = "call_" + randomHex(12)
		}
		item = &responsesToolItem{
			index:     idx,
			id:        "fc_" + randomHex(12),
			callID:    callID,
			name:      asString(fn["name"]),
			outputIdx: r.nextOutput,
		}
		if r.msgOpen {
			// 正文项与工具项是两个不同的 output item，序号必须错开。
			item.outputIdx = r.nextOutput + 1
		}
		r.toolItems[idx] = item
		r.toolOrder = append(r.toolOrder, idx)
		item.open = true
		r.emit("response.output_item.added", map[string]any{
			"output_index": item.outputIdx,
			"item": map[string]any{
				"type":      "function_call",
				"id":        item.id,
				"call_id":   item.callID,
				"name":      item.name,
				"arguments": "",
				"status":    "in_progress",
			},
		})
	}
	if args := asString(fn["arguments"]); args != "" {
		item.args.WriteString(args)
		r.emit("response.function_call_arguments.delta", map[string]any{
			"item_id":      item.id,
			"output_index": item.outputIdx,
			"delta":        args,
		})
	}
}

// chunk 处理一帧 chat chunk。
func (r *responsesTranslator) chunk(obj map[string]any) {
	if r.finished || r.errSent || obj == nil {
		return
	}
	if asMap(obj["error"]) != nil {
		// Responses 的错误形态与 OpenAI 一致（顶层 error 对象），交给最后统一写。
		return
	}
	if id := asString(obj["id"]); id != "" && r.id == "" {
		r.id = "resp_" + strings.TrimPrefix(id, "chatcmpl-")
	}
	if m := asString(obj["model"]); m != "" {
		r.model = m
	}
	r.start()
	if u := asMap(obj["usage"]); u != nil {
		if v, ok := u["prompt_tokens"]; ok {
			r.inputTokens = asInt(v)
		}
		if v, ok := u["completion_tokens"]; ok {
			r.outTokens = asInt(v)
		}
	}
	choices := asSlice(obj["choices"])
	if len(choices) == 0 {
		return
	}
	c0 := asMap(choices[0])
	if c0 == nil {
		return
	}
	if d := asMap(c0["delta"]); d != nil {
		r.textDelta(asString(d["content"]))
		for _, tc := range asSlice(d["tool_calls"]) {
			r.toolDelta(asMap(tc))
		}
	}
	if fr := asString(c0["finish_reason"]); fr != "" {
		r.finishReason = fr
	}
}

// outputItems 组装最终的 output 数组。
func (r *responsesTranslator) outputItems() []any {
	out := make([]any, 0, 2)
	if r.msgOpen || r.text.Len() > 0 {
		out = append(out, map[string]any{
			"type":   "message",
			"id":     r.msgItemID,
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        r.text.String(),
				"annotations": []any{},
			}},
		})
	}
	for _, idx := range r.toolOrder {
		item := r.toolItems[idx]
		if item == nil {
			continue
		}
		out = append(out, map[string]any{
			"type":      "function_call",
			"id":        item.id,
			"call_id":   item.callID,
			"name":      item.name,
			"arguments": item.args.String(),
			"status":    "completed",
		})
	}
	return out
}

// done 收尾：补齐 done 事件并发出 response.completed。
func (r *responsesTranslator) done() {
	if r.finished || r.errSent {
		return
	}
	r.start()
	if r.msgOpen {
		r.emit("response.output_text.done", map[string]any{
			"item_id":       r.msgItemID,
			"output_index":  r.nextOutput,
			"content_index": 0,
			"text":          r.text.String(),
		})
		r.emit("response.content_part.done", map[string]any{
			"item_id":       r.msgItemID,
			"output_index":  r.nextOutput,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": r.text.String(), "annotations": []any{}},
		})
		r.emit("response.output_item.done", map[string]any{
			"output_index": r.nextOutput,
			"item": map[string]any{
				"type":   "message",
				"id":     r.msgItemID,
				"role":   "assistant",
				"status": "completed",
				"content": []any{map[string]any{
					"type":        "output_text",
					"text":        r.text.String(),
					"annotations": []any{},
				}},
			},
		})
		r.msgOpen = false
	}
	for _, idx := range r.toolOrder {
		item := r.toolItems[idx]
		if item == nil {
			continue
		}
		r.emit("response.function_call_arguments.done", map[string]any{
			"item_id":      item.id,
			"output_index": item.outputIdx,
			"arguments":    item.args.String(),
		})
		r.emit("response.output_item.done", map[string]any{
			"output_index": item.outputIdx,
			"item": map[string]any{
				"type":      "function_call",
				"id":        item.id,
				"call_id":   item.callID,
				"name":      item.name,
				"arguments": item.args.String(),
				"status":    "completed",
			},
		})
	}
	r.emit("response.completed", map[string]any{"response": r.responseObject("completed", r.outputItems())})
	r.finished = true
}

// jsonBody 非流式响应改写。
func (r *responsesTranslator) jsonBody(body []byte) []byte {
	var resp map[string]any
	if json.Unmarshal(body, &resp) != nil {
		return body
	}
	if asMap(resp["error"]) != nil {
		return body // 错误形态与 OpenAI 一致，原样透出
	}
	choices := asSlice(resp["choices"])
	output := make([]any, 0, 2)
	text := ""
	var calls []any
	finish := "stop"
	if len(choices) > 0 {
		c0 := asMap(choices[0])
		if m := asMap(c0["message"]); m != nil {
			text = asString(m["content"])
			for _, raw := range asSlice(m["tool_calls"]) {
				tc := asMap(raw)
				if tc == nil {
					continue
				}
				fn := asMap(tc["function"])
				calls = append(calls, map[string]any{
					"type":      "function_call",
					"id":        "fc_" + randomHex(12),
					"call_id":   asString(tc["id"]),
					"name":      asString(fn["name"]),
					"arguments": asString(fn["arguments"]),
					"status":    "completed",
				})
			}
		}
		if fr := asString(c0["finish_reason"]); fr != "" {
			finish = fr
		}
	}
	if text != "" || len(calls) == 0 {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     "msg_" + randomHex(12),
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		})
	}
	output = append(output, calls...)

	if u := asMap(resp["usage"]); u != nil {
		r.inputTokens = asInt(u["prompt_tokens"])
		r.outTokens = asInt(u["completion_tokens"])
	}
	if id := asString(resp["id"]); id != "" {
		r.id = "resp_" + strings.TrimPrefix(id, "chatcmpl-")
	}
	if m := asString(resp["model"]); m != "" {
		r.model = m
	}
	obj := r.responseObject("completed", output)
	obj["output_text"] = text
	if finish == "tool_calls" || finish == "function_call" {
		obj["status"] = "completed"
	}
	out, _ := json.Marshal(obj)
	return out
}

// responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	chatBody, err := json.Marshal(responsesToChat(&req))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "failed to build upstream request")
		return
	}

	tr := newResponsesTranslator(req.Model)
	tap := newSSETap(w, tr.chunk, tr.done, tr.jsonBody)
	tr.sink = tap.emit

	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chatBody))
	r2.ContentLength = int64(len(chatBody))
	h.chatCompletions(tap, r2)
	tap.Close()
}
