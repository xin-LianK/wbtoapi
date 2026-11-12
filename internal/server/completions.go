// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// completions.go OpenAI legacy 文本补全入口：POST /v1/completions。
//
// 存在理由：一批老客户端（早期 SDK、部分编辑器插件、自己写的脚本）只会调
// /v1/completions，不认 /v1/chat/completions。上游只有对话端点，所以这里把
// prompt 包成一条 user 消息，交给既有 chat 流水线（选号、换号、冷却、记账、
// 请求日志全都照旧），再把聊天格式的响应翻回 text_completion 形状。
//
// 与 anthropic.go / responses.go 同一取向：不复制流水线，只做协议适配；
// 差异（只支持字符串 prompt、没有 tool_calls 概念、没有 logprobs 数组）在
// 入口处**明确拒绝**，不静默丢参数——老客户端最怕的就是"参数被吃了还回 200"。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// legacyCompletionMaxTokens 老接口的 max_tokens 缺省值：OpenAI 文档写的就是 16。
// 不跟随 chat 的"不传则由模型决定"，否则一个没写 max_tokens 的脚本可能一次跑出
// 几千 token 的账单——老接口的用户预期就是短补全。
const legacyCompletionMaxTokens = 16

// completionsRequest /v1/completions 里本网关关心的字段。
type completionsRequest struct {
	Model            string          `json:"model"`
	Prompt           json.RawMessage `json:"prompt"`
	MaxTokens        *int            `json:"max_tokens"`
	Temperature      *float64        `json:"temperature"`
	TopP             *float64        `json:"top_p"`
	N                *int            `json:"n"`
	Stream           bool            `json:"stream"`
	Stop             json.RawMessage `json:"stop"`
	PresencePenalty  *float64        `json:"presence_penalty"`
	FrequencyPenalty *float64        `json:"frequency_penalty"`
	Seed             *int            `json:"seed"`
	User             string          `json:"user"`
	// Echo 老接口独有：把 prompt 原文拼在补全结果前面。chat 侧没有对应能力，这里在
	// 响应侧补（非流式拼在正文前，流式作为第一帧发出），语义与官方一致。
	Echo bool `json:"echo"`
	// 下面几个 chat 侧没有等价物，显式拒绝而不是静默忽略。
	Suffix   string `json:"suffix"`
	BestOf   *int   `json:"best_of"`
	Logprobs *int   `json:"logprobs"`
}

// completionsPrompt 取出 prompt 文本。只支持字符串与「单元素字符串数组」两种形态：
// 多 prompt（官方会跑 N×len 次）与 token id 数组需要网关侧拆成多次上游调用，
// 成本与语义都变味，直接告诉用户改用 chat 端点。
func completionsPrompt(raw json.RawMessage) (string, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", "prompt is required"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, ""
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		switch len(list) {
		case 0:
			return "", "prompt is required"
		case 1:
			return list[0], ""
		default:
			return "", "prompt arrays with more than one entry are not supported; send one prompt per request, or use /v1/chat/completions"
		}
	}
	var ids []int
	if json.Unmarshal(raw, &ids) == nil {
		return "", "token-id prompts are not supported; send the text prompt instead, or use /v1/chat/completions"
	}
	return "", "prompt must be a string"
}

// completionsToChat 组装 chat 请求体，返回 prompt 文本（echo 用）。
// 第二个返回值非空表示请求里有本网关不支持、且不能静默丢弃的参数。
func completionsToChat(req *completionsRequest) (map[string]any, string, string) {
	prompt, perr := completionsPrompt(req.Prompt)
	if perr != "" {
		return nil, "", perr
	}
	if req.Suffix != "" {
		return nil, "", "suffix is not supported (the upstream has no infill endpoint); use /v1/chat/completions"
	}
	if req.BestOf != nil && *req.BestOf > 1 {
		return nil, "", "best_of is not supported (it would multiply upstream cost); use n instead"
	}
	if req.Logprobs != nil && *req.Logprobs > 0 {
		return nil, "", "logprobs is not supported by the upstream; use /v1/chat/completions if your client can"
	}
	maxTokens := legacyCompletionMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	out := map[string]any{
		"model":      req.Model,
		"stream":     req.Stream,
		"max_tokens": maxTokens,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.N != nil {
		out["n"] = *req.N
	}
	if req.PresencePenalty != nil {
		out["presence_penalty"] = *req.PresencePenalty
	}
	if req.FrequencyPenalty != nil {
		out["frequency_penalty"] = *req.FrequencyPenalty
	}
	if req.Seed != nil {
		out["seed"] = *req.Seed
	}
	if req.User != "" {
		out["user"] = req.User
	}
	// stop 在 chat 侧同样接受字符串或数组，原样带过去。
	if len(req.Stop) > 0 && string(req.Stop) != "null" {
		out["stop"] = req.Stop
	}
	return out, prompt, ""
}

// completions POST /v1/completions（legacy 文本补全）。
func (h *Handler) completions(w http.ResponseWriter, r *http.Request) {
	body, err := readRequestBody(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if !json.Valid(body) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return
	}
	var req completionsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	chatReq, prompt, bad := completionsToChat(&req)
	if bad != "" {
		writeOpenAIError(w, http.StatusBadRequest, "unsupported_parameter", bad)
		return
	}
	raw, err := json.Marshal(chatReq)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cw := &completionsWriter{
		w:       w,
		model:   req.Model,
		prompt:  prompt,
		echo:    req.Echo,
		created: time.Now().Unix(),
		id:      "cmpl-" + strings.TrimPrefix(reqlog.NewRequestID(), "req-"),
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(raw))
	r2.ContentLength = int64(len(raw))
	h.chatCompletions(cw, r2)
	cw.finish()
}

// completionsWriter 接住 chatCompletions 写出的聊天格式响应，翻成 text_completion。
//   - 200 + text/event-stream：逐帧翻译，边收边写；
//   - 其余（非流式成功、各种错误）：先攒齐，finish 时一次翻译写出。
//
// 错误响应本身就是 OpenAI 错误形状，两个接口一致，原样透传即可。
type completionsWriter struct {
	w       http.ResponseWriter
	model   string
	prompt  string
	echo    bool
	created int64
	id      string

	status int
	stream bool
	line   []byte
	buf    bytes.Buffer
	// echoed 流式下 echo 只发一次（第一帧补 prompt 原文）。
	echoed bool
}

func (c *completionsWriter) Header() http.Header { return c.w.Header() }

func (c *completionsWriter) WriteHeader(code int) {
	if c.status != 0 {
		return
	}
	c.status = code
	if code == http.StatusOK && strings.HasPrefix(c.w.Header().Get("Content-Type"), "text/event-stream") {
		c.stream = true
		c.w.WriteHeader(code)
	}
}

func (c *completionsWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if !c.stream {
		return c.buf.Write(p)
	}
	c.line = append(c.line, p...)
	for {
		i := bytes.IndexByte(c.line, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimRight(c.line[:i], "\r")
		if err := c.streamLine(line); err != nil {
			return 0, err
		}
		c.line = c.line[i+1:]
	}
	return len(p), nil
}

// Flush 直接下推到客户端，保持"边收边写"。
func (c *completionsWriter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// streamLine 处理一行 SSE：只关心 data: 行，注释与空行丢弃（这里重排帧，不留原始注释）。
func (c *completionsWriter) streamLine(line []byte) error {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 {
		return nil
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		_, err := io.WriteString(c.w, "data: [DONE]\n\n")
		if err == nil {
			c.Flush()
		}
		return err
	}
	// 上游错误帧：形状与 chat 相同（{"error":{...}}），原样转出，流到此结束。
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(payload, &probe) == nil && len(probe.Error) > 0 && !bytes.Equal(probe.Error, []byte("null")) {
		if _, err := io.WriteString(c.w, "data: "+string(payload)+"\n\n"); err != nil {
			return err
		}
		c.Flush()
		return nil
	}
	out, ok := completionChunk(payload, c.id, c.created)
	if !ok {
		return nil
	}
	if !c.echoed && c.echo && c.prompt != "" {
		// echo：prompt 原文作为第一帧的 text 前置（与官方"补全 = prompt + 续写"一致）。
		if ch, ok := out["choices"].([]any); ok {
			for _, it := range ch {
				m := asMap(it)
				if m != nil {
					m["text"] = c.prompt + asString(m["text"])
				}
			}
		}
		c.echoed = true
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	if _, err := io.WriteString(c.w, "data: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	c.Flush()
	return nil
}

// finish 在 chatCompletions 返回后调用：流式补完残行，非流式一次翻译写出。
func (c *completionsWriter) finish() {
	if c.stream {
		if len(c.line) > 0 {
			_ = c.streamLine(bytes.TrimRight(c.line, "\r"))
			c.line = nil
		}
		c.Flush()
		return
	}
	if c.status == 0 {
		return
	}
	c.w.Header().Del("Content-Length")
	if c.status != http.StatusOK {
		// 错误体已经是 OpenAI 错误形状，原样写回（状态码照旧）。
		c.w.WriteHeader(c.status)
		_, _ = c.w.Write(c.buf.Bytes())
		return
	}
	out := completionResponse(c.buf.Bytes(), c.id, c.created, c.model)
	out["model"] = c.model
	if c.echo && c.prompt != "" {
		if ch, ok := out["choices"].([]any); ok {
			for _, it := range ch {
				m := asMap(it)
				if m != nil {
					m["text"] = c.prompt + asString(m["text"])
				}
			}
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		writeOpenAIError(c.w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(raw)
}

// completionResponse 把一条 chat 非流式响应翻成 text_completion。
// usage / created / 每个 choice 的 index 与 finish_reason 都保留，客户端据此记账。
func completionResponse(chatRaw []byte, id string, created int64, model string) map[string]any {
	var chat struct {
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	_ = json.Unmarshal(chatRaw, &chat)
	if chat.Created == 0 {
		chat.Created = created
	}
	if chat.Model == "" {
		chat.Model = model
	}
	choices := make([]any, 0, len(chat.Choices))
	for _, ch := range chat.Choices {
		choices = append(choices, map[string]any{
			"text":          ch.Message.Content,
			"index":         ch.Index,
			"logprobs":      nil,
			"finish_reason": ch.FinishReason,
		})
	}
	out := map[string]any{
		"id":      id,
		"object":  "text_completion",
		"created": chat.Created,
		"model":   chat.Model,
		"choices": choices,
	}
	if chat.Usage != nil {
		out["usage"] = chat.Usage
	}
	return out
}

// completionChunk 把一帧 chat SSE 翻成 text_completion 帧；没有 choices 的帧（例如
// 只有 usage 的尾帧）翻成 choices:[]，保持"每帧都有一个 choices 数组"的形状。
func completionChunk(payload []byte, id string, created int64) (map[string]any, bool) {
	var chunk struct {
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, false
	}
	if chunk.Created == 0 {
		chunk.Created = created
	}
	choices := make([]any, 0, len(chunk.Choices))
	for _, ch := range chunk.Choices {
		choices = append(choices, map[string]any{
			"text":          ch.Delta.Content,
			"index":         ch.Index,
			"logprobs":      nil,
			"finish_reason": ch.FinishReason,
		})
	}
	out := map[string]any{
		"id":      id,
		"object":  "text_completion",
		"created": chunk.Created,
		"model":   chunk.Model,
		"choices": choices,
	}
	if chunk.Usage != nil {
		out["usage"] = chunk.Usage
	}
	return out, true
}
