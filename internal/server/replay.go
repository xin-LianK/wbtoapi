// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// replay.go 面板「请求记录 → 复测」：拿一条历史的模型名，现在再发一条最小请求，
// 看这个模型此刻还能不能用、走的哪个号、花了多久。
//
// 为什么只能复测模型、不能复现原请求：请求记录**按设计**不存提示词与响应正文
// （见 reqlog 包注释），只存模型、状态、耗时、token。所以能做到的最有价值的事是
// 「同模型再打一枪」——失败记录里 429 / 冷却 / 无可用号这类问题，复测一下就知道
// 是偶发还是还在持续，不必去客户端重放。
//
// 与测活（liveness.go）的区别：测活**钉死一个号**并写回该号的「上次测活」；
// 复测不钉号，走正常选号，结果只进请求记录（密钥列标「复测」），不改号池状态。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

const (
	// replayPath 复测请求在请求日志里的入口路径。
	replayPath = "/panel/replay"
	// replayLabel 请求日志「密钥」一列里复测的名字，按它能筛出所有复测。
	replayLabel = "复测"
)

// ReplayOutcome 一次复测的结果：模型级结论 + 实际服务的账号 + 回复片段。
type ReplayOutcome struct {
	pool.LivenessResult
	// Reply 上游回复的文字片段（没有正文时取思考内容），最多 livenessReplyMax 字节。
	Reply string
	// Account 这次复测实际用的账号标签（昵称(uid8)），失败时为空。
	Account string
}

// ReplayModel 用 model 发一条最小真实请求（max_tokens 16，内容 "hi"），走和真实
// 请求同一条流水线：选号、冷却、换号、错误分类、用量与请求日志。返回结果与上游
// 回复的文字片段。模型名为空、号池没装好时返回 error；上游失败不算 error，
// 写在结果里（客户端要看的是"通/不通"，不是网关调用出错）。
func (h *Handler) ReplayModel(ctx context.Context, model string) (ReplayOutcome, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return ReplayOutcome{}, fmt.Errorf("model is required")
	}
	if h.cfg.Pool == nil || h.cfg.Upstream == nil {
		return ReplayOutcome{}, fmt.Errorf("gateway is not ready (no account pool)")
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": livenessMaxTokens,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	tr := &requestTrace{id: reqlog.NewRequestID(), start: time.Now(), path: replayPath, apiKey: replayLabel}
	ctx = context.WithValue(ctx, requestTraceKey{}, tr)
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	w := &livenessWriter{header: http.Header{}}
	obs := &responseObserver{ResponseWriter: w}
	if h.cfg.RequestLog != nil {
		h.cfg.RequestLog.Begin()
	}
	h.chatCompletions(obs, r)
	ev := tr.event(obs.status, obs.errBody)
	if h.cfg.RequestLog != nil {
		h.cfg.RequestLog.Record(ev)
	}
	res := pool.LivenessResult{
		At: tr.start, OK: ev.OK, Model: model, Status: ev.Status, Error: ev.Error,
		TTFBMs: ev.TTFBMs, DurationMs: ev.DurationMs, Credit: ev.Credit,
	}
	// 只写系统日志，不碰号池状态：复测成功不代表某个号该解冻（它用的是当时最优的那个号）。
	if res.OK {
		log.Printf("replay model=%s: ok account=%s ttfb=%dms total=%dms credit=%g",
			model, ev.Account, res.TTFBMs, res.DurationMs, res.Credit)
	} else {
		log.Printf("replay model=%s: WARN 失败 status=%d: %s", model, res.Status, res.Error)
	}
	return ReplayOutcome{LivenessResult: res, Reply: livenessReply(w.body.Bytes()), Account: ev.Account}, nil
}
