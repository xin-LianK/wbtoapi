// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// compensation.go 官方补偿活动的状态查询与领取（/billing/meter/compensation-status、claim-compensation）。
//
// 背景（issue #189，2026-10-16）：官方在「套餐与积分」页挂限时补偿横幅
// （例：「DeepSeek 模型服务异常额外补偿 500 积分」，带领取截止日），账号侧看得见，
// 网关这边却是**盲领**——ClaimCompensation 只在每日签到后顺带调一次，且写法是
//
//	if credit, err := ...; err == nil { log.Printf(...) }
//
// 于是三种完全不同的情形在日志里同构：① 没有补偿 ② 已经领过 ③ **真的领取失败**
// （401/5xx/网络）。补偿带截止日，签到被禁用 / 机器关机 / 账号被禁用时就会静默
// 过期，用户全程无感知。本文件补上「先查状态再决定是否领」所缺的那一半。
//
// 上游口径（实测 2026-10-16，cn 域 CLI 侧与 Web 侧均返回一致）：
//
//	{"code":0,"msg":"OK","data":{
//	   "claimed":false,          // 是否已领
//	   "active":true,            // 当前是否有生效中的补偿活动
//	   "credit_num":500,         // 可领额度
//	   "validity_period":1,      // 发放积分的有效期（单位上游未明示，前端也不渲染）
//	   "end_time":"2026-10-22 23:59:59"}}  // 领取截止
//
// active=false 是「当前无补偿活动」的正常 200 返回，不是错误——故此接口只在
// 传输失败 / 非 2xx / 业务 code 非 0 时返回 error。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// CompensationStatus 单账号的补偿活动状态。
type CompensationStatus struct {
	// Active 当前是否有生效中的补偿活动（无活动时为 false，属正常返回）。
	Active bool `json:"active"`
	// Claimed 本次活动是否已领取。
	Claimed bool `json:"claimed"`
	// CreditNum 待领额度（积分）。
	CreditNum int64 `json:"credit_num"`
	// ValidityPeriod 发放积分的有效期倍数。上游未明示单位，且前端 UI 从不渲染
	// 该字段（仅后端返回），故此处只透传不做换算，避免臆造语义。
	ValidityPeriod int64 `json:"validity_period"`
	// EndTime 领取截止时间（上游为 "2006-01-02 15:04:05" 无时区字符串）。
	EndTime string `json:"end_time"`
}

// CompensationClaim 一次「查 + 按需领」的结果：区分本次新领和此前已领，
// 避免调用方把已到账的额度重复计入增量。
type CompensationClaim struct {
	CompensationStatus
	NewlyClaimed bool  `json:"newly_claimed"`
	Credit       int64 `json:"credit"`
}

// Pending 是否存在「可领且未领」的补偿——即该调用 ClaimCompensation 的前置判据。
//
// 三者与门：active 为 false 是没有活动；claimed 为 true 是已领完（再调只会拿到
// 业务错误）；credit_num <= 0 是活动存在但无额度。三种都归为「无需领取」。
func (s CompensationStatus) Pending() bool {
	return s.Active && !s.Claimed && s.CreditNum > 0
}

// compensationAccount 补偿与积分是 cn 个人版口径，国际版 / 企业号无此活动。
func compensationAccount(a *auth.Auth) error {
	if a == nil || a.IsGlobal() || a.IsEnterprise() {
		return fmt.Errorf("积分补偿仅支持国内版个人账号")
	}
	return nil
}

// GetCompensationStatus 查补偿活动状态。
//
// 走 billingJSON（billingBase），与 ClaimCompensation / ClaimGift 同域同头；
// 实测 CLI 域与 Web 域对该端点返回一致，故不必像 growth 领奖那样切 webBase。
func (c *Client) GetCompensationStatus(a *auth.Auth) (*CompensationStatus, error) {
	if err := compensationAccount(a); err != nil {
		return nil, err
	}
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/compensation-status", nil)
	if err != nil {
		return nil, err
	}
	var st *CompensationStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("解析补偿状态: %w", err)
	}
	if st == nil {
		return nil, fmt.Errorf("补偿状态为空")
	}
	return st, nil
}

// ClaimCompensation 领取活动补偿（有则领，无则业务错误）；业务提示保留上游原文。
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	if err := compensationAccount(a); err != nil {
		return 0, err
	}
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-compensation", nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &resp); err != nil {
			return 0, fmt.Errorf("解析补偿领取结果: %w", err)
		}
	}
	return resp.Credit, nil
}

// ClaimAvailableCompensation 供面板和签到排程共用：先查状态，已领 / 活动未开放时
// 跳过领奖，只在新到账时回报 NewlyClaimed。
func (c *Client) ClaimAvailableCompensation(a *auth.Auth) (*CompensationClaim, error) {
	status, err := c.GetCompensationStatus(a)
	if err != nil {
		return nil, err
	}
	result := &CompensationClaim{CompensationStatus: *status}
	if !status.Active || status.Claimed {
		return result, nil
	}
	credit, err := c.ClaimCompensation(a)
	if err != nil {
		// 其他入口可能刚领完，或响应丢失：回读确认，避免把已到账误报成失败。
		if after, readErr := c.GetCompensationStatus(a); readErr == nil && after.Claimed {
			return &CompensationClaim{CompensationStatus: *after}, nil
		}
		return nil, err
	}
	result.Claimed, result.NewlyClaimed = true, true
	result.Credit = credit
	if result.Credit == 0 {
		// 官网只依据 code=0 判断成功，部分响应不含 credit；沿用刚查询的官方额度。
		result.Credit = status.CreditNum
	}
	c.noteEarning(a, EarnCompensation, "", result.Credit)
	return result, nil
}
