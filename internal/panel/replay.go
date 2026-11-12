// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// replay.go 面板「请求记录 → 复测」接口。
//
// 请求记录按设计只存元数据，不存提示词与响应正文（见 reqlog 包注释），所以这里
// 能做的不是"原样重放"而是"同模型再打一枪"：拿这条记录的模型名发一条最小请求，
// 告诉运维此刻这个模型通不通、走哪个号、多久回来。失败记录里最常见的三种情况
// ——偶发 429、号在冷却、模型全池不可用——复测一下就能分清，不必回客户端重放。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ReplayResult 一次复测的结果（字段与前端 types.ReplayResult 对齐）。
type ReplayResult struct {
	OK         bool    `json:"ok"`
	Status     int     `json:"status"`
	Model      string  `json:"model"`
	Reply      string  `json:"reply,omitempty"`
	Error      string  `json:"error,omitempty"`
	Account    string  `json:"account,omitempty"`
	TTFBMs     int64   `json:"ttfb_ms,omitempty"`
	DurationMs int64   `json:"duration_ms"`
	Credit     float64 `json:"credit,omitempty"`
	At         string  `json:"at,omitempty"`
}

// SetReplay 注入复测入口（main 在 handler 构造完成后调用）。
//
// 与 SetChatEndpoint 同一个缘由：面板要先于 server.Handler 构造，那时 chat 流水线
// 还不存在，只能用 setter 补这一步，避免 server ↔ panel 的循环依赖。
func (p *Panel) SetReplay(fn func(ctx context.Context, model string) (ReplayResult, error)) {
	p.cfg.Replay = fn
}

// requestReplay 复测一个模型（请求记录每行的「复测」按钮）。
// 与单号测活一样等到结果再返回；关掉面板也不影响（context 去取消，测完照样记账）。
func (p *Panel) requestReplay(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Replay == nil {
		writeErr(w, http.StatusNotImplemented, "replay not available")
		return
	}
	var in struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	res, err := p.cfg.Replay(context.WithoutCancel(r.Context()), model)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}
