// wire.go 非 OpenAI 协议的响应改写基座（Anthropic Messages / OpenAI Responses）。
//
// 为什么在 ResponseWriter 层做协议翻译，而不是给每种协议各写一套转发逻辑：
// chatCompletions 里糅了选号、会话粘性、在途租约、错误分类、账号处置、用量记账、
// 标记修复等十几处紧耦合逻辑。任何一份"另一条流水线"的拷贝都会在几周内漂移，
// 且漂移只在边缘协议上暴露（最不容易被测到）。这里只做「字节进、字节出」的
// 翻译，其余链路原样复用——协议差异被收敛到一个文件里。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// sseTap 拦在 chatCompletions 与真实 ResponseWriter 之间。
//
// 两种形态都覆盖：
//   - 流式：chatCompletions 经 upstream.StreamHint 写出 `data: {...}\n\n` 帧，
//     这里按空行切帧后逐帧交给 onChunk；
//   - 非流式：writeJSON 整体写出一个 JSON 对象，这里先整体缓冲，Close 时一次性
//     交给 onJSON 改写。
//
// 形态判定发生在 WriteHeader（或首次 Write 的隐式 200）时：此刻上游已把
// Content-Type 设好（StreamHint 设 text/event-stream，writeJSON 设 application/json），
// 据此分流即可，不需要额外约定。
type sseTap struct {
	w         http.ResponseWriter
	fl        http.Flusher
	committed bool
	sse       bool
	buf       []byte // SSE 半帧
	jsonBody  []byte // 非流式整体缓冲

	onChunk func(map[string]any)
	onDone  func()
	// onJSON 返回改写后的字节；返回 nil 表示不写（例如已在内部写出）。
	onJSON func([]byte) []byte
}

// newSSETap 构造拦截器。三个回调都可为 nil（nil = 该形态不转发，丢弃）。
func newSSETap(w http.ResponseWriter, onChunk func(map[string]any), onDone func(), onJSON func([]byte) []byte) *sseTap {
	t := &sseTap{w: w, onChunk: onChunk, onDone: onDone, onJSON: onJSON}
	if f, ok := w.(http.Flusher); ok {
		t.fl = f
	}
	return t
}

func (t *sseTap) Header() http.Header { return t.w.Header() }

// classify 依据已设好的 Content-Type 判定本次响应的形态，并把 200 提交给真实
// ResponseWriter。必须在任何写出之前调用一次。
func (t *sseTap) classify(status int) {
	if t.committed {
		return
	}
	t.sse = strings.Contains(t.w.Header().Get("Content-Type"), "text/event-stream")
	t.w.WriteHeader(status)
	t.committed = true
}

func (t *sseTap) WriteHeader(status int) {
	t.classify(status)
}

func (t *sseTap) Write(p []byte) (int, error) {
	if !t.committed {
		// StreamHint 只设头不显式 WriteHeader，走到这里由我们补一次隐式 200。
		t.classify(http.StatusOK)
	}
	if !t.sse {
		// 非流式：缓冲到 Close 再整体改写（状态码此时已提交，只改 body）。
		t.jsonBody = append(t.jsonBody, p...)
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	for {
		i := bytes.Index(t.buf, []byte("\n\n"))
		if i < 0 {
			break
		}
		block := t.buf[:i]
		t.buf = t.buf[i+2:]
		t.handleBlock(block)
	}
	return len(p), nil
}

// handleBlock 处理一个完整 SSE 事件块（可能含多行，只取 data: 行）。
func (t *sseTap) handleBlock(block []byte) {
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		if string(payload) == "[DONE]" {
			continue // [DONE] 由 Close 统一收尾，避免重复
		}
		var obj map[string]any
		if json.Unmarshal(payload, &obj) != nil {
			continue // 非 JSON 帧在目标协议里无对应形态，丢弃
		}
		if t.onChunk != nil {
			t.onChunk(obj)
		}
	}
}

func (t *sseTap) Flush() {
	if t.fl != nil {
		t.fl.Flush()
	}
}

// emit 直接向真实 ResponseWriter 写一段已翻译好的字节并 flush。
func (t *sseTap) emit(s string) {
	if s == "" {
		return
	}
	_, _ = io.WriteString(t.w, s)
	t.Flush()
}

// Close 在 chatCompletions 返回后调用：收尾流式事件或改写缓冲的非流式 body。
func (t *sseTap) Close() {
	if t.sse {
		t.buf = nil
		if t.onDone != nil {
			t.onDone()
		}
		return
	}
	if len(t.jsonBody) == 0 {
		return
	}
	body := t.jsonBody
	if t.onJSON != nil {
		body = t.onJSON(t.jsonBody)
	}
	if len(body) > 0 {
		_, _ = t.w.Write(body)
	}
}

// asInt 把 JSON 数字（float64 / json.Number）归一成 int。
func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	case int:
		return n
	}
	return 0
}

// asString 取字符串字段（非字符串一律空串）。
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// asMap 取对象字段（非对象一律 nil）。
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// asSlice 取数组字段（非数组一律 nil）。
func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}
