// anthropic.go Anthropic Messages API 兼容层：POST /v1/messages。
//
// 为什么需要它：Claude Code、cc-switch、以及任何用 @anthropic-ai/sdk 的工具，
// 说的是 Anthropic Messages 协议（`/v1/messages` + 具名 SSE 事件 + `x-api-key`），
// 而本网关此前只有 OpenAI 形态。缺这一层，这些客户端一个都接不进来。
//
// 实现取向：**不做第二条转发流水线**。请求转成 OpenAI chat 形态后交给既有
// chatCompletions（选号、粘性、租约、错误分类、账号处置、用量记账全部复用），
// 响应再经 wire.go 的 sseTap 翻译回 Anthropic 形态。协议差异全部收敛在本文件。
//
// 有意不做的映射（宁可缺字段也不伪造协议语义）：
//   - `thinking` 请求块不转成 reasoning_effort：上游的思考档位有自己的枚举，
//     硬映射会把 Claude 的 budget_tokens 猜成错误档位。上游仍会按自己的
//     默认策略返回 reasoning_content，只是这里不回吐成 thinking 块。
//   - 响应里的 `reasoning_content` **不回吐**：Anthropic 的 thinking 块要求
//     带签名的完整块，网关没有签名可给；塞半块会让严格客户端直接报协议错误。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// anthropicBodyLimit /v1/messages 请求体上限。对话请求可能带图片（base64），
// 但 Anthropic 官方对单请求也有量级限制；4MiB 足够覆盖长上下文 + 少量图片，
// 又不至于让一次误发的巨型 body 把内存吃满（chat 路径本身无上限）。
const anthropicBodyLimit = 4 << 20

// anthropicRequest Anthropic Messages 请求里本网关关心的字段。
type anthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	Stream        bool                 `json:"stream"`
	System        json.RawMessage      `json:"system"`
	Messages      []anthropicMessage   `json:"messages"`
	Temperature   *float64             `json:"temperature"`
	TopP          *float64             `json:"top_p"`
	StopSequences []string             `json:"stop_sequences"`
	Tools         []anthropicTool      `json:"tools"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice"`
}

// anthropicMessage 单条消息。content 既可能是字符串也可能是 block 数组。
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicTool 工具声明。input_schema 即 JSON Schema，直接作为 OpenAI 的 parameters。
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice `{type:"auto"|"any"|"tool", name?}`。
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// anthropicSource 图片源（base64 或 url 两形态）。
type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// anthropicBlock content 数组里的单个块。只声明本网关会转换的字段。
type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// image
	Source *anthropicSource `json:"source"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// parseAnthropicBlocks 把 content（字符串或 block 数组）归一成 block 列表。
func parseAnthropicBlocks(raw json.RawMessage) []anthropicBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return blocks
}

// anthropicSystemText 把 system（字符串或 text block 数组）拼成一段文本。
func anthropicSystemText(raw json.RawMessage) string {
	blocks := parseAnthropicBlocks(raw)
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "" || b.Type == "text" {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicImagePart 图片块 → OpenAI image_url part。
func anthropicImagePart(b anthropicBlock) any {
	if b.Source == nil {
		return nil
	}
	var url string
	switch b.Source.Type {
	case "base64":
		mt := b.Source.MediaType
		if mt == "" {
			mt = "image/png"
		}
		url = "data:" + mt + ";base64," + b.Source.Data
	case "url":
		url = b.Source.URL
	}
	if url == "" {
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// anthropicToolResultText tool_result 的 content（字符串 / text block 数组 /
// 任意 JSON）→ 单段文本。role:"tool" 的 content 在 OpenAI 侧只能是字符串。
func anthropicToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if (b.Type == "" || b.Type == "text") && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	// 结构化 JSON：原样序列化比丢内容好（工具可能返回 JSON 结果）。
	return string(raw)
}

// partsOrString 单文本 part 退回字符串形态：OpenAI 侧 content 为字符串时兼容性最好，
// 数组形态留给真正带图的消息。
func partsOrString(parts []any) any {
	if len(parts) == 0 {
		return ""
	}
	allText := true
	var texts []string
	for _, p := range parts {
		m := asMap(p)
		if m == nil || asString(m["type"]) != "text" {
			allText = false
			break
		}
		texts = append(texts, asString(m["text"]))
	}
	if allText {
		if len(texts) == 1 {
			return texts[0]
		}
		return strings.Join(texts, "\n")
	}
	return parts
}

// anthropicMessagesToChat 把 Anthropic 的 messages 数组转成 OpenAI messages。
//
// 一条 Anthropic user 消息可能同时含 tool_result 与文本块，OpenAI 侧必须拆成
// 「若干 role:tool 消息 + 一条 role:user 消息」，且顺序不能反——tool 消息是
// 对上一轮 assistant tool_calls 的应答，必须紧跟在它后面。
func anthropicMessagesToChat(items []anthropicMessage) []any {
	out := make([]any, 0, len(items)+2)
	for _, m := range items {
		blocks := parseAnthropicBlocks(m.Content)
		if m.Role == "assistant" {
			var texts []string
			var calls []any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						texts = append(texts, b.Text)
					}
				case "tool_use":
					args := strings.TrimSpace(string(b.Input))
					if args == "" {
						args = "{}"
					}
					calls = append(calls, map[string]any{
						"id":   b.ID,
						"type": "function",
						"function": map[string]any{
							"name":      b.Name,
							"arguments": args,
						},
					})
				}
			}
			msg := map[string]any{"role": "assistant"}
			if len(texts) > 0 {
				msg["content"] = strings.Join(texts, "\n")
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
				if len(texts) == 0 {
					msg["content"] = nil // 对齐 OpenAI：纯工具调用轮 content 为 null
				}
			}
			if _, ok := msg["content"]; !ok {
				msg["content"] = ""
			}
			out = append(out, msg)
			continue
		}

		var toolMsgs []any
		parts := make([]any, 0, len(blocks))
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				}
			case "image":
				if p := anthropicImagePart(b); p != nil {
					parts = append(parts, p)
				}
			case "tool_result":
				toolMsgs = append(toolMsgs, map[string]any{
					"role":         "tool",
					"tool_call_id": b.ToolUseID,
					"content":      anthropicToolResultText(b.Content),
				})
			}
		}
		out = append(out, toolMsgs...)
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": partsOrString(parts)})
		} else if len(toolMsgs) == 0 {
			// 空 user 消息：保留占位，避免上游看到「只有 system」的畸形请求。
			out = append(out, map[string]any{"role": "user", "content": ""})
		}
	}
	return out
}

// anthropicToChat 组装 OpenAI chat 请求体。
func anthropicToChat(req *anthropicRequest) map[string]any {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}

	msgs := make([]any, 0, len(req.Messages)+1)
	if sys := anthropicSystemText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	msgs = append(msgs, anthropicMessagesToChat(req.Messages)...)
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
				fn["parameters"] = t.InputSchema
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			if tc.Name != "" {
				out["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": tc.Name},
				}
			}
		}
	}
	return out
}

// anthropicStop OpenAI finish_reason → Anthropic stop_reason。
func anthropicStop(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "stop":
		return "end_turn"
	}
	return "end_turn"
}

// anthropicErrType 把 OpenAI 错误码映射成 Anthropic 错误类型词表。
func anthropicErrType(code string) string {
	switch code {
	case "rate_limit_exceeded", "rate_limit":
		return "rate_limit_error"
	case "invalid_request", "bad_params", "prompt_too_long", "model_unavailable", "content_blocked":
		return "invalid_request_error"
	case "authentication_error", "invalid_api_key":
		return "authentication_error"
	case "overloaded_error", "upstream_busy":
		return "overloaded_error"
	}
	return "api_error"
}

// anthropicTranslator 把 OpenAI 流式帧 / 整体响应翻译成 Anthropic 形态。
//
// 状态机基于 Anthropic 的「顺序内容块」模型：任何时刻至多一个块处于打开状态，
// 切到另一种类型（text ↔ tool_use）必须先关旧块再开新块；块下标单调递增。
type anthropicTranslator struct {
	model string
	id    string
	sink  func(string)

	started    bool
	blockOpen  bool
	blockKind  string // "text" / "tool"
	blockIndex int
	toolBlocks map[int]int

	stop         string
	inputTokens  int
	outputTokens int
	finished     bool
	errSent      bool
}

func newAnthropicTranslator(model string) *anthropicTranslator {
	return &anthropicTranslator{model: model, toolBlocks: map[int]int{}}
}

// emit 写一个具名 SSE 事件。
func (a *anthropicTranslator) emit(event string, payload any) {
	if a.sink == nil {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	a.sink("event: " + event + "\ndata: " + string(b) + "\n\n")
}

// start 发 message_start（每个流恰好一次）。
func (a *anthropicTranslator) start() {
	if a.started {
		return
	}
	if a.id == "" {
		a.id = "msg_" + randomHex(12)
	}
	a.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            a.id,
			"type":          "message",
			"role":          "assistant",
			"model":         a.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	a.started = true
}

func (a *anthropicTranslator) closeBlock() {
	if !a.blockOpen {
		return
	}
	a.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": a.blockIndex})
	a.blockOpen = false
	a.blockKind = ""
	a.blockIndex++
}

func (a *anthropicTranslator) textDelta(txt string) {
	if !a.blockOpen || a.blockKind != "text" {
		a.closeBlock()
		a.blockOpen = true
		a.blockKind = "text"
		a.emit("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         a.blockIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	a.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": a.blockIndex,
		"delta": map[string]any{"type": "text_delta", "text": txt},
	})
}

// toolDelta 处理 OpenAI 的增量 tool_calls 分片。
func (a *anthropicTranslator) toolDelta(tc map[string]any) {
	if tc == nil {
		return
	}
	idx := asInt(tc["index"])
	fn := asMap(tc["function"])
	name := asString(fn["name"])
	args := asString(fn["arguments"])

	bIdx, seen := a.toolBlocks[idx]
	if !seen {
		a.closeBlock()
		id := asString(tc["id"])
		if id == "" {
			id = "toolu_" + randomHex(12)
		}
		a.blockOpen = true
		a.blockKind = "tool"
		bIdx = a.blockIndex
		a.toolBlocks[idx] = bIdx
		a.emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": bIdx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  name,
				"input": map[string]any{},
			},
		})
	}
	if args != "" {
		a.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": bIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
}

// chunk 处理一帧 OpenAI chunk。
func (a *anthropicTranslator) chunk(obj map[string]any) {
	if a.finished || a.errSent || obj == nil {
		return
	}
	if e := asMap(obj["error"]); e != nil {
		msg := asString(e["message"])
		if msg == "" {
			msg = "upstream error"
		}
		a.emit("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrType(asString(e["code"])), "message": msg},
		})
		a.errSent = true
		return
	}
	if id := asString(obj["id"]); id != "" {
		a.id = id
	}
	if m := asString(obj["model"]); m != "" {
		a.model = m
	}
	a.start()
	if u := asMap(obj["usage"]); u != nil {
		if v, ok := u["prompt_tokens"]; ok {
			a.inputTokens = asInt(v)
		}
		if v, ok := u["completion_tokens"]; ok {
			a.outputTokens = asInt(v)
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
		if txt := asString(d["content"]); txt != "" {
			a.textDelta(txt)
		}
		for _, tc := range asSlice(d["tool_calls"]) {
			a.toolDelta(asMap(tc))
		}
	}
	if fr := asString(c0["finish_reason"]); fr != "" {
		a.stop = anthropicStop(fr)
	}
}

// done 收尾：关块 → message_delta → message_stop。
func (a *anthropicTranslator) done() {
	if a.finished || a.errSent {
		return
	}
	a.start()
	a.closeBlock()
	stop := a.stop
	if stop == "" {
		stop = "end_turn"
	}
	a.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": a.inputTokens, "output_tokens": a.outputTokens},
	})
	a.emit("message_stop", map[string]any{"type": "message_stop"})
	a.finished = true
}

// jsonBody 非流式响应改写。
func (a *anthropicTranslator) jsonBody(body []byte) []byte {
	var resp map[string]any
	if json.Unmarshal(body, &resp) != nil {
		return body // 解析不了就原样透出（宁可露出原始错误也不要吞掉）
	}
	if e := asMap(resp["error"]); e != nil {
		msg := asString(e["message"])
		if msg == "" {
			msg = "upstream error"
		}
		out, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrType(asString(e["code"])), "message": msg},
		})
		return out
	}

	content := make([]any, 0, 4)
	stopReason := "end_turn"
	if choices := asSlice(resp["choices"]); len(choices) > 0 {
		c0 := asMap(choices[0])
		if m := asMap(c0["message"]); m != nil {
			if txt := asString(m["content"]); txt != "" {
				content = append(content, map[string]any{"type": "text", "text": txt})
			}
			for _, raw := range asSlice(m["tool_calls"]) {
				tc := asMap(raw)
				if tc == nil {
					continue
				}
				fn := asMap(tc["function"])
				args := asString(fn["arguments"])
				var input any = map[string]any{}
				if args != "" {
					var parsed any
					if json.Unmarshal([]byte(args), &parsed) == nil {
						input = parsed
					}
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    asString(tc["id"]),
					"name":  asString(fn["name"]),
					"input": input,
				})
			}
		}
		if fr := asString(c0["finish_reason"]); fr != "" {
			stopReason = anthropicStop(fr)
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if u := asMap(resp["usage"]); u != nil {
		usage["input_tokens"] = asInt(u["prompt_tokens"])
		usage["output_tokens"] = asInt(u["completion_tokens"])
	}
	id := asString(resp["id"])
	if id == "" {
		id = a.id
	}
	if id == "" {
		id = "msg_" + randomHex(12)
	}
	model := asString(resp["model"])
	if model == "" {
		model = a.model
	}
	out, _ := json.Marshal(map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	})
	return out
}

// withAuthAnthropic 同 withAuth，但额外接受 `x-api-key`（Anthropic SDK 的默认形态）。
func (h *Handler) withAuthAnthropic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyAPIKey(r, h.loadLive().APIKey) {
			writeMessagesError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// anthropicMessages 处理 POST /v1/messages。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, anthropicBodyLimit))
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	chatBody, err := json.Marshal(anthropicToChat(&req))
	if err != nil {
		writeMessagesError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
		return
	}

	tr := newAnthropicTranslator(req.Model)
	tap := newSSETap(w, tr.chunk, tr.done, tr.jsonBody)
	tr.sink = tap.emit

	// 交给既有 chat 流水线：换 body、保留原 context 与请求头。
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chatBody))
	r2.ContentLength = int64(len(chatBody))
	h.chatCompletions(tap, r2)
	tap.Close()
}

// anthropicCountTokens 处理 POST /v1/messages/count_tokens。
//
// 只做量级估算（与同类网关同口径）：Anthropic 没有本地 tokenizer，而这条接口
// 的消费者（Claude Code）只用它做上下文预算提示，不参与计费。估算规则：
// CJK 每字 1 token，其余按 4 字节/字符一组，图片按 1500 token 计。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, anthropicBodyLimit))
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateAnthropicTokens(&req)})
}

// estimateAnthropicTokens 粗估输入 token。
func estimateAnthropicTokens(req *anthropicRequest) int {
	total := estimateTokensText(anthropicSystemText(req.System))
	for _, m := range req.Messages {
		for _, b := range parseAnthropicBlocks(m.Content) {
			switch b.Type {
			case "text", "":
				total += estimateTokensText(b.Text)
			case "image":
				total += 1500
			case "tool_use":
				total += estimateTokensText(b.Name) + estimateTokensText(string(b.Input))
			case "tool_result":
				total += estimateTokensText(anthropicToolResultText(b.Content))
			}
		}
	}
	for _, t := range req.Tools {
		total += estimateTokensText(t.Name) + estimateTokensText(t.Description) + estimateTokensText(string(t.InputSchema))
	}
	return total
}

// estimateTokensText CJK 按字计、其余按 4 字符 1 token。
func estimateTokensText(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	n := cjk + other/4
	if n == 0 && other > 0 {
		n = 1
	}
	return n
}
