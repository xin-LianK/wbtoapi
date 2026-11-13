// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package upstream

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestNormalizeRoles 验证出站请求体把 developer 角色归一为 system。
// 上游 role 白名单不含 developer（OpenAI 新规范的 system 别名），
// 命中即 HTTP 400 code=11128；此处走 PrepareBodyOptWithEfforts 全链路断言。
func TestNormalizeRoles(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRoles []string // 与输出 messages 逐条对应的期望 role；len 即消息数
	}{
		{"developer 改写为 system",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"Developer 首字母大写改写",
			`{"messages":[{"role":"Developer","content":"x"}]}`, []string{"system"}},
		{"DEVELOPER 全大写改写",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`, []string{"system"}},
		{"前后空白 TrimSpace 后改写",
			`{"messages":[{"role":" developer ","content":"x"}]}`, []string{"system"}},
		{"system 原样保留",
			`{"messages":[{"role":"system","content":"x"}]}`, []string{"system"}},
		{"user 原样保留",
			`{"messages":[{"role":"user","content":"x"}]}`, []string{"user"}},
		{"assistant 原样保留",
			`{"messages":[{"role":"assistant","content":"x"}]}`, []string{"assistant"}},
		{"tool 原样保留（不因未知而改写）",
			`{"messages":[{"role":"tool","content":"x"}]}`, []string{"tool"}},
		{"messages 缺失不 panic 且其余字段不变",
			`{"model":"glm-5.2"}`, []string{}},
		{"messages 为空数组不 panic",
			`{"messages":[]}`, []string{}},
		{"混合消息仅 developer 被改写",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"}},
		{"sanitize=false 时仍归一（与脱敏解耦）",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"非对象消息元素跳过、其余正常处理",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`, []string{"system"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 全程 sanitize=false：验证 role 归一与内容脱敏开关无关（D4）。
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			// 提取输出 messages 里的 role（非对象元素跳过，不 panic）。
			var got []string
			if msgs, ok := obj["messages"].([]any); ok {
				for _, m := range msgs {
					msg, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if role, ok := msg["role"].(string); ok {
						got = append(got, role)
					}
				}
			}

			if len(got) != len(c.wantRoles) {
				t.Fatalf("role 数量不符: got %v (%d) want %v (%d)", got, len(got), c.wantRoles, len(c.wantRoles))
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Errorf("role[%d] = %q want %q", i, got[i], c.wantRoles[i])
				}
			}
		})
	}

	// messages 缺失时，其余字段必须原样保留（除强制 stream）。
	t.Run("messages 缺失时其余字段不变", func(t *testing.T) {
		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("其余字段被改动: %v", obj)
		}
	})
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestPrepareBodyStreamOptions body 未显式带 stream_options 时注入
// {include_usage: true}（D7，官方 CLI 流式必发）；body 已带则不覆盖。
func TestPrepareBodyStreamOptions(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[]}`), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options not injected: %v", obj["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v want true", so["include_usage"])
	}

	out2 := PrepareBodyOptWithEffertsPreserve(t, `{"model":"glm-5.2","messages":[],"stream_options":{"include_usage":false}}`)
	obj2, err := decodeBody(out2)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so2, ok := obj2["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options lost: %v", obj2["stream_options"])
	}
	if so2["include_usage"] != false {
		t.Errorf("stream_options.include_usage = %v want false (not overwritten)", so2["include_usage"])
	}
}

// PrepareBodyOptWithEffertsPreserve helper：PrepareBodyOptWithEfforts 包装。
func PrepareBodyOptWithEffertsPreserve(t *testing.T, body string) []byte {
	t.Helper()
	return PrepareBodyOptWithEfforts([]byte(body), false, nil)
}

// decodeBody helper：解析 body JSON。
func decodeBody(b []byte) (map[string]any, error) {
	var obj map[string]any
	err := json.Unmarshal(b, &obj)
	return obj, err
}

// TestPrepareBodyDeterministic 序列化稳定性：同输入跑多遍出站字节级一致
// （prompt_cache_key 前缀命中的前提——链中不得注入时间/随机/ID 类不确定源）。
func TestPrepareBodyDeterministic(t *testing.T) {
	inputs := []string{
		`{"model":"glm-5.2","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}],"reasoning_effort":"high"}`,
		`{"model":"deepseek-v4","messages":[{"role":"user","content":"写个函数"}],"tool_choice":{"type":"auto"},"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"glm-5.3","messages":[{"role":"developer","content":"sys"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
	}
	for i, in := range inputs {
		var first []byte
		for round := 0; round < 5; round++ {
			out := PrepareBodyOptWithEfforts([]byte(in), true, map[string][]string{"glm-5.2": {"off", "low", "high"}})
			if round == 0 {
				first = out
				continue
			}
			if string(out) != string(first) {
				t.Fatalf("input #%d round %d differs from round 0:\n%s\n%s", i, round, first, out)
			}
		}
	}
}

// TestNormalizeImageURL 覆盖 OpenAI chat 多模态内容的 image_url 兼容：
// 字符串形态必须转为上游需要的对象形态；对象形态及其中字段必须原样保留；
// 无效输入不补默认值，继续交给上游返回真实错误。
func TestNormalizeImageURL(t *testing.T) {
	tests := []struct {
		name string
		body string
		want any
	}{
		{
			name: "data url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD"},
		},
		{
			name: "http url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/a.png"}]}]}`,
			want: map[string]any{"url": "https://example.test/a.png"},
		},
		{
			name: "object with detail preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"low","mime_type":"image/png"}}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD", "detail": "low", "mime_type": "image/png"},
		},
		{
			name: "invalid object url type preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: map[string]any{"url": float64(123)},
		},
		{
			name: "missing image url preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: nil,
		},
		{
			name: "empty string preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":""}]}]}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, sanitize := range []bool{false, true} {
				out := PrepareBodyOptWithEfforts([]byte(tc.body), sanitize, nil)
				obj, err := decodeBody(out)
				if err != nil {
					t.Fatalf("sanitize=%v unmarshal: %v (out=%s)", sanitize, err, out)
				}
				msgs := obj["messages"].([]any)
				content := msgs[0].(map[string]any)["content"].([]any)
				var part map[string]any
				for _, rawPart := range content {
					candidate, ok := rawPart.(map[string]any)
					if ok && candidate["type"] == "image_url" {
						part = candidate
						break
					}
				}
				if part == nil {
					t.Fatal("image_url part not found")
				}
				if tc.want == nil {
					if _, exists := part["image_url"]; exists {
						t.Fatalf("sanitize=%v: missing image_url should stay missing, got %#v", sanitize, part)
					}
					continue
				}
				if got := part["image_url"]; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("sanitize=%v: image_url=%#v want %#v", sanitize, got, tc.want)
				}
			}
		})
	}
}

// TestNormalizeToolPatterns 工具 schema pattern 的 `\_` 转义归一（11129 实案：
// exa agent_run 的 `^agent\_run\_` 被 deepseek 系确定性拒收，归一后上游 200）。
// 覆盖：function.parameters 与裸 parameters 两形态、嵌套 schema、patternProperties
// 键、消息正文不碰、无 tools no-op、sanitize 开关两态一致。
func TestNormalizeToolPatterns(t *testing.T) {
	lookupPattern := func(t *testing.T, body string, path ...any) string {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		cur := any(obj)
		for _, p := range path {
			switch k := p.(type) {
			case string:
				m, ok := cur.(map[string]any)
				if !ok {
					t.Fatalf("path %v: want object, got %T", path, cur)
				}
				cur = m[k]
			case int:
				l, ok := cur.([]any)
				if !ok || k >= len(l) {
					t.Fatalf("path %v: bad array at %T", path, cur)
				}
				cur = l[k]
			}
		}
		s, _ := cur.(string)
		return s
	}

	t.Run("exa agent_run pattern normalized", func(t *testing.T) {
		bs := string(byte(92))                                                                                                                                                                                                                                                                                                                // 反斜杠，测试体经工具链多层转义易被吞，运行时拼装保真
		body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"agent_run","parameters":{"type":"object","properties":{"runId":{"type":"string","pattern":"^agent` + bs + bs + `_run` + bs + bs + `_"}` + `,"query":{"type":"string"}},"required":["query"]}}}]}` //nolint:lll // 实案 body 原样
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "runId", "pattern"); got != `^agent_run_` {
			t.Fatalf("pattern = %q, want %q", got, `^agent_run_`)
		}
		// 同 schema 的兄弟字段不受影响
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "query", "type"); got != "string" {
			t.Fatalf("sibling field disturbed: %q", got)
		}
	})

	t.Run("nested anyOf items pattern normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"anyOf":[{"properties":{"code":{"pattern":"x` + bs + bs + `_y"}}},{"type":"string"}]}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "anyOf", 0, "properties", "code", "pattern"); got != "x_y" {
			t.Fatalf("nested pattern = %q, want %q", got, "x_y")
		}
	})

	t.Run("bare tool parameters form normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"name":"t","parameters":{"properties":{"id":{"pattern":"p` + bs + bs + `_q"}}}}]}`
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "parameters", "properties", "id", "pattern"); got != "p_q" {
			t.Fatalf("bare parameters pattern = %q, want %q", got, "p_q")
		}
	})

	t.Run("patternProperties key normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"patternProperties":{"^a` + bs + bs + `_b` + bs + bs + `_":{"type":"string"}}}}}]}` //nolint:lll
		var obj map[string]any
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		tool := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
		props := tool["patternProperties"].(map[string]any)
		if _, ok := props["^a_b_"]; !ok {
			t.Fatalf("normalized key missing: %v", props)
		}
		for k := range props {
			if strings.Contains(k, string(byte(92))) {
				t.Fatalf("patternProperties key not normalized: %q", k)
			}
		}
	})

	t.Run("message content backslash untouched", func(t *testing.T) {
		bs := string(byte(92))
		raw := `{"messages":[{"role":"user","content":"path C:` + bs + bs + `_dir and regex a` + bs + bs + `_b"}],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"q` + bs + bs + `_r"}}}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(raw), false, nil))
		var obj map[string]any
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		msg := obj["messages"].([]any)[0].(map[string]any)
		if got := msg["content"]; got != `path C:\_dir and regex a\_b` {
			t.Fatalf("message content mutated: %q", got)
		}
	})

	t.Run("clean pattern untouched", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"^[a-z]+` + bs + bs + `d_$"}}}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != `^[a-z]+\d_$` {
			t.Fatalf("clean pattern mutated: %q", got)
		}
	})

	t.Run("no tools is no-op", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"model":"m","messages":[{"role":"user","content":"a` + bs + bs + `_b"}]}`
		var inObj, outObj map[string]any
		if err := json.Unmarshal([]byte(body), &inObj); err != nil {
			t.Fatal(err)
		}
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &outObj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		inMsg, _ := json.Marshal(inObj["messages"])
		outMsg, _ := json.Marshal(outObj["messages"])
		if string(inMsg) != string(outMsg) {
			t.Fatalf("messages changed without tools: %s -> %s", inMsg, outMsg)
		}
		if _, has := outObj["tools"]; has {
			t.Fatal("tools appeared out of nowhere")
		}
	})

	t.Run("sanitize on and off behave the same", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"a` + bs + bs + `_b"}}}}}]}` //nolint:lll
		for _, sanitize := range []bool{false, true} {
			out := string(PrepareBodyOptWithEfforts([]byte(body), sanitize, nil))
			if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != "a_b" {
				t.Fatalf("sanitize=%v: pattern = %q, want %q", sanitize, got, "a_b")
			}
		}
	})
}

// TestClampGPTMinMaxTokens GPT 系上游要求 max_tokens ≥ 16（实测 gpt-6-sol/gpt-6-luna/
// gpt-5.6-sol：15 → 400 code=11133 model_param_invalid，16 → 200）。Claude Code 切
// 模型时的探针请求 max_tokens 极小，全号轮转同样被拒 → 客户端 503。非 GPT 模型不动。
func TestClampGPTMinMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want any // nil 表示字段不存在
	}{
		{"gpt below floor", `{"model":"gpt-6-sol","max_tokens":1,"messages":[]}`, float64(16)},
		{"gpt global prefix", `{"model":"global:gpt-5.6-sol","max_tokens":15,"messages":[]}`, float64(16)},
		{"gpt at floor", `{"model":"gpt-6-luna","max_tokens":16,"messages":[]}`, float64(16)},
		{"gpt above floor", `{"model":"gpt-6-sol","max_tokens":32000,"messages":[]}`, float64(32000)},
		{"gpt alias translated then clamped", `{"model":"gpt-6-sol","max_completion_tokens":1,"messages":[]}`, float64(16)},
		{"gpt absent untouched", `{"model":"gpt-6-sol","messages":[]}`, nil},
		{"non-gpt untouched", `{"model":"hy4-preview-f","max_tokens":1,"messages":[]}`, float64(1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, err := decodeBody(PrepareBodyOptWithEffertsPreserve(t, c.body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, has := obj["max_tokens"]
			if c.want == nil {
				if has {
					t.Fatalf("max_tokens = %v, want absent", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("max_tokens = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDropReasoningContentParts 覆盖思考片段剥离规则本身（直调，隔离其它归一化步骤）。
// 背景：Codex 只走 /v1/responses，new-api 的 responses→chat 转换器会把助手历史里的
// reasoning_text 原样透传进 /v1/chat/completions；上游 content 类型白名单不含它，
// 命中即 HTTP 400 code=11101 "unsupported content type at index 0: reasoning_text"。
func TestDropReasoningContentParts(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // 期望的 messages JSON（null 表示字段缺失）
	}{
		{
			"reasoning_text 剥离、text 保留",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"think"},{"type":"text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":[{"type":"text","text":"ans"}]}]`,
		},
		{
			"reasoning 别名同样剥离",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning","text":"think"},{"type":"output_text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":[{"type":"output_text","text":"ans"}]}]`,
		},
		{
			"type 前后空白同样命中",
			`{"messages":[{"role":"assistant","content":[{"type":" reasoning_text ","text":"think"},{"type":"text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":[{"type":"text","text":"ans"}]}]`,
		},
		{
			"剥离后 content 为空且无 tool_calls：整条消息删除",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"think"}]},{"role":"user","content":"hi"}]}`,
			`[{"role":"user","content":"hi"}]`,
		},
		{
			"剥离后 content 为空但有 tool_calls：消息保留（空数组 content）",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"think"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]}]}`,
			`[{"role":"assistant","content":[],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]}]`,
		},
		{
			"字符串 content 不受影响",
			`{"messages":[{"role":"assistant","content":"plain"},{"role":"user","content":"hi"}]}`,
			`[{"role":"assistant","content":"plain"},{"role":"user","content":"hi"}]`,
		},
		{
			"非对象片段原样保留",
			`{"messages":[{"role":"assistant","content":["raw",{"type":"text","text":"t"}]}]}`,
			`[{"role":"assistant","content":["raw",{"type":"text","text":"t"}]}]`,
		},
		{
			"messages 缺失不 panic",
			`{"model":"deepseek-v4.1-flash"}`,
			`null`,
		},
		{
			"messages 非数组不 panic",
			`{"messages":"weird"}`,
			`"weird"`,
		},
		{
			"全部消息均被剥离：messages 变空数组",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"t"}]}]}`,
			`[]`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, err := decodeBody([]byte(c.body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			dropReasoningContentParts(obj)
			var want any
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatalf("unmarshal want: %v", err)
			}
			if !reflect.DeepEqual(obj["messages"], want) {
				t.Fatalf("messages = %#v, want %#v", obj["messages"], want)
			}
		})
	}
}

// TestPrepareBodyDropsReasoningTextParts 全链路断言：PrepareBodyOptWithEfforts 管线已挂接
// 剥离步骤，Codex 历史报文里的 reasoning_text 不再出站（sanitize 两态一致）。
func TestPrepareBodyDropsReasoningTextParts(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"reasoning_text","text":"think"},{"type":"text","text":"ans"}]},
		{"role":"user","content":"again"}
	]}`
	for _, sanitize := range []bool{false, true} {
		out := PrepareBodyOptWithEfforts([]byte(body), sanitize, nil)
		if strings.Contains(string(out), "reasoning_text") {
			t.Fatalf("sanitize=%v: reasoning_text 未剥离: %s", sanitize, out)
		}
		obj, err := decodeBody(out)
		if err != nil {
			t.Fatalf("sanitize=%v unmarshal: %v", sanitize, err)
		}
		msgs := obj["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatalf("sanitize=%v: messages 数 = %d, want 3", sanitize, len(msgs))
		}
		content := msgs[1].(map[string]any)["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("sanitize=%v: 助手 content 片段数 = %d, want 1", sanitize, len(content))
		}
		if got := content[0].(map[string]any)["text"]; got != "ans" {
			t.Fatalf("sanitize=%v: 保留片段 text = %v, want ans", sanitize, got)
		}
	}
}

// TestHardenToolSchemaKeywords 工具 schema 约束关键字的类型回正（11129 实案：客户端把
// 数字/布尔序列化成字符串，上游 Go 校验器整条 400）。覆盖数字类、布尔类、字符串类、
// enum、additionalProperties、嵌套（properties/items/allOf/$defs）、属性名与 default
// 不被误改、幂等、与 sanitize 开关无关。
func TestHardenToolSchemaKeywords(t *testing.T) {
	// at 按 key/下标路径取值，取不到直接失败。
	at := func(t *testing.T, obj map[string]any, path ...any) any {
		t.Helper()
		var cur any = obj
		for _, p := range path {
			switch k := p.(type) {
			case string:
				m, ok := cur.(map[string]any)
				if !ok {
					t.Fatalf("path %v: want object, got %T", path, cur)
				}
				cur = m[k]
			case int:
				l, ok := cur.([]any)
				if !ok || k >= len(l) {
					t.Fatalf("path %v: bad array at %T", path, cur)
				}
				cur = l[k]
			}
		}
		return cur
	}

	// 每个参数都带一处「字符串化的类型」，并留一条正常字段做对照。
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"n": map[string]any{"type": "integer", "minimum": "1", "maximum": "10", "default": nil},
			"s": map[string]any{"type": "string", "minLength": "1", "maxLength": "9", "description": 42},
			"a": map[string]any{"type": "array", "minItems": "2", "uniqueItems": "true",
				"items": map[string]any{"type": "string", "pattern": "^a$"}},
			"e": map[string]any{"type": "string", "enum": "a,b"},
			"o": map[string]any{"type": "object", "additionalProperties": "false",
				"properties": map[string]any{"minimum": map[string]any{"type": "number", "multipleOf": "0.5"}}},
			"bad": map[string]any{"type": "number", "exclusiveMinimum": "not-a-number"},
		},
		"allOf":    []any{map[string]any{"properties": map[string]any{"x": map[string]any{"maximum": "3"}}}},
		"$defs":    map[string]any{"d": map[string]any{"type": "integer", "exclusiveMaximum": "8"}},
		"required": []any{"n"},
	}
	body, err := json.Marshal(map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": `{"minimum":"1"}`}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "t", "parameters": params}},
			map[string]any{"name": "bare", "parameters": map[string]any{"type": "object", "maxProperties": "3"}},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	props := []any{"tools", 0, "function", "parameters", "properties"}
	for _, sanitize := range []bool{false, true} {
		out := PrepareBodyOptWithEfforts(body, sanitize, nil)
		obj, err := decodeBody(out)
		if err != nil {
			t.Fatalf("sanitize=%v unmarshal: %v", sanitize, err)
		}
		cases := []struct {
			name string
			path []any
			want any
		}{
			{"minimum 字符串回正", append(props, "n", "minimum"), float64(1)},
			{"maximum 字符串回正", append(props, "n", "maximum"), float64(10)},
			{"minLength 字符串回正", append(props, "s", "minLength"), float64(1)},
			{"minItems 字符串回正", append(props, "a", "minItems"), float64(2)},
			{"uniqueItems 字符串回正", append(props, "a", "uniqueItems"), true},
			{"description 数字转字符串", append(props, "s", "description"), "42"},
			{"嵌套 multipleOf 回正", append(props, "o", "properties", "minimum", "multipleOf"), float64(0.5)},
			{"additionalProperties 字符串回正", append(props, "o", "additionalProperties"), false},
			{"allOf 内 maximum 回正", []any{"tools", 0, "function", "parameters", "allOf", 0, "properties", "x", "maximum"}, float64(3)},
			{"$defs 内 exclusiveMaximum 回正", []any{"tools", 0, "function", "parameters", "$defs", "d", "exclusiveMaximum"}, float64(8)},
			{"裸 parameters 也纠", []any{"tools", 1, "parameters", "maxProperties"}, float64(3)},
			// 属性名恰好叫 minimum 时不能被当成关键字改写（值仍是子 schema）
			{"属性名不当关键字", append(props, "o", "properties", "minimum", "type"), "number"},
			{"正常字段不动", append(props, "n", "type"), "integer"},
		}
		for _, tc := range cases {
			if got := at(t, obj, tc.path...); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sanitize=%v %s: got %#v (%T), want %#v", sanitize, tc.name, got, got, tc.want)
			}
		}
		// enum 非数组直接删（保留错误值只会继续 400）
		if _, has := at(t, obj, append(props, "e")...).(map[string]any)["enum"]; has {
			t.Errorf("sanitize=%v: 非数组 enum 未删除", sanitize)
		}
		// 解析不出来的数字关键字删除
		if _, has := at(t, obj, append(props, "bad")...).(map[string]any)["exclusiveMinimum"]; has {
			t.Errorf("sanitize=%v: 不可解析的 exclusiveMinimum 未删除", sanitize)
		}
		// default:null 是合法写法，不能顺手删
		if v, has := at(t, obj, append(props, "n")...).(map[string]any)["default"]; !has || v != nil {
			t.Errorf("sanitize=%v: default:null 被误改（has=%v v=%#v）", sanitize, has, v)
		}
		// 消息正文里的同形字符串不碰（只动 tools 子树）
		msg := at(t, obj, "messages", 0, "content")
		if msg != `{"minimum":"1"}` {
			t.Errorf("sanitize=%v: 消息正文被改写: %#v", sanitize, msg)
		}
		// 幂等：再跑一遍输出逐字节相同
		again := PrepareBodyOptWithEfforts(out, sanitize, nil)
		if string(again) != string(out) {
			t.Errorf("sanitize=%v: 不幂等\nfirst=%s\nagain=%s", sanitize, out, again)
		}
	}
}

// TestPrepareBodyKeepsValidToolSchema 已经合规的 schema 一个字节都不改：字符串化关键字
// 回正只该发生在违规节点上，不能顺手重排/丢字段。
func TestPrepareBodyKeepsValidToolSchema(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object",` +
		`"properties":{"n":{"type":"integer","minimum":1,"maximum":10,"enum":[1,2]}},` +
		`"required":["n"],"additionalProperties":false}}}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	obj, err := decodeBody(out)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fn := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	if _, has := params["additionalProperties"]; !has {
		t.Fatalf("合规的 additionalProperties:false 被删")
	}
	n := params["properties"].(map[string]any)["n"].(map[string]any)
	if n["minimum"] != float64(1) || n["maximum"] != float64(10) {
		t.Fatalf("合规数字被改写: %#v", n)
	}
	if _, has := n["enum"]; !has {
		t.Fatalf("合规 enum 被删")
	}
}

// TestHardenToolSchemaStructure 结构位置写错类型时也要能通过：type 给数字、properties
// 给数组、required 写成逗号串、items 写成类型名——上游反序列化 schema 直接 400/11129。
// 口径是「形态回正，认不出就删」，且必须幂等（第二次运行不再改动）。
func TestHardenToolSchemaStructure(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"t","parameters":{` +
		`"type":5,` +
		`"properties":[],` +
		`"required":"a, b ,a",` +
		`"items":"object",` +
		`"patternProperties":["x"],` +
		`"properties":[],` +
		`"allOf":[{"type":["string",5,"null","string"],"minLength":"2"},` +
		`{"items":[{"type":"string"},5],"required":[]}]}}}]}`

	run := func(in []byte) map[string]any {
		out := PrepareBodyOptWithEfforts(in, false, nil)
		obj, err := decodeBody(out)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return obj
	}
	params := func(obj map[string]any) map[string]any {
		fn := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
		return fn["parameters"].(map[string]any)
	}

	p := params(run([]byte(body)))
	if _, has := p["type"]; has {
		t.Errorf("type:5 应删掉，得到 %#v", p["type"])
	}
	if _, has := p["properties"]; has {
		t.Errorf("properties:[] 应删掉")
	}
	if _, has := p["patternProperties"]; has {
		t.Errorf("patternProperties:[] 应删掉")
	}
	if got := p["required"]; !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Errorf("required 逗号串应回正为 [a b]，得到 %#v", got)
	}
	if got, ok := p["items"].(map[string]any); !ok || got["type"] != "object" {
		t.Errorf(`items:"object" 应包成 {type:object}，得到 %#v`, p["items"])
	}

	branch := p["allOf"].([]any)
	first := branch[0].(map[string]any)
	if got := first["type"]; !reflect.DeepEqual(got, []any{"string", "null"}) {
		t.Errorf("type 数组应只留合法类型名并去重，得到 %#v", got)
	}
	if first["minLength"] != float64(2) {
		t.Errorf("同节点的标量回正不能丢: %#v", first["minLength"])
	}
	second := branch[1].(map[string]any)
	if got, ok := second["items"].([]any); !ok || len(got) != 1 {
		t.Errorf("items 数组应只留对象元素，得到 %#v", second["items"])
	}
	if _, has := second["required"]; has {
		t.Errorf("required:[] 应删掉")
	}

	// 幂等：把结果再跑一遍，序列化后逐字节相同。
	once := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	twice := PrepareBodyOptWithEfforts(once, false, nil)
	if string(once) != string(twice) {
		t.Errorf("不幂等:\n一次 %s\n二次 %s", once, twice)
	}

	// 反证：合规结构不能被改。type 数组、单 schema items、字符串 required 数组都算合规。
	okBody := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"t","parameters":{` +
		`"type":["object","null"],"properties":{"a":{"type":"string"}},` +
		`"required":["a"],"items":{"type":"string"},"patternProperties":{"^x":{"type":"number"}}}}}]}`
	okOut, err := decodeBody(PrepareBodyOptWithEfforts([]byte(okBody), false, nil))
	if err != nil {
		t.Fatalf("unmarshal 合规: %v", err)
	}
	kp := params(okOut)
	if got := kp["type"]; !reflect.DeepEqual(got, []any{"object", "null"}) {
		t.Errorf("合规 type 数组被改: %#v", got)
	}
	if got := kp["required"]; !reflect.DeepEqual(got, []any{"a"}) {
		t.Errorf("合规 required 被改: %#v", got)
	}
	if got, ok := kp["items"].(map[string]any); !ok || got["type"] != "string" {
		t.Errorf("合规 items 被改: %#v", kp["items"])
	}
	if got, ok := kp["patternProperties"].(map[string]any); !ok || len(got) != 1 {
		t.Errorf("合规 patternProperties 被改: %#v", kp["patternProperties"])
	}
}

// TestHardenToolSchemaPattern 非法 RE2 只能删不能改：上游按 Go 的 regexp 校验 schema，
// lookbehind / 负向断言这类 PCRE 写法会让整条请求 400/11129，换号无用。
func TestHardenToolSchemaPattern(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object",` +
		`"properties":{"ok":{"type":"string","pattern":"^[a-z]+$"},` +
		`"badLookbehind":{"type":"string","pattern":"(?<=x)y"},` +
		`"badLookahead":{"type":"string","pattern":"(?!x)."}},` +
		`"patternProperties":{"^ok_":{"type":"string"},"(?<!p)q":{"type":"number"}}}}}]}`

	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	obj, err := decodeBody(out)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fn := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	if got := props["ok"].(map[string]any)["pattern"]; got != "^[a-z]+$" {
		t.Errorf("合规 pattern 被改: %#v", got)
	}
	for _, k := range []string{"badLookbehind", "badLookahead"} {
		if _, has := props[k].(map[string]any)["pattern"]; has {
			t.Errorf("%s: 非法 RE2 应删掉 pattern", k)
		}
	}
	pp := params["patternProperties"].(map[string]any)
	if _, has := pp["^ok_"]; !has {
		t.Errorf("合规 patternProperties 键被删")
	}
	if _, has := pp["(?<!p)q"]; has {
		t.Errorf("非法 RE2 的 patternProperties 键应删掉")
	}
	if len(pp) != 1 {
		t.Errorf("patternProperties 应只剩 1 个键，得到 %d 个", len(pp))
	}
}
