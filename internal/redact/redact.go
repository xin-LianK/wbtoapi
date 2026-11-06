// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// Package redact 对**要落到面板、日志或归档里的文本**做就地脱敏，让凭证和身份信息在
// 排查问题时仍然可辨识（保留前后几位），但不再构成可直接使用的秘密。
//
// 设计口径与 magpie 的脱敏中间件一致：只处理已经离开运行时的副本（日志行、请求记录的
// 失败原因），不碰真正发给上游的请求，也不做可逆映射——日志不需要还原成原文，
// 需要还原的那一套在 internal/upstream 的请求链路上，与此无关。
//
// 覆盖：URL 里的 user:password、Bearer 凭证、sk- 开头的密钥、JWT、query / JSON 里的
// token= / password= 一类字段、Cookie 头、邮箱、中国大陆手机号。长文本里没有命中任何
// 规则时直接返回原串（不分配）。
package redact

import (
	"regexp"
	"strings"
)

// maskKeep 是掩码时保留的位数：短值多留一位辨识度，长值固定 4 位，
// 免得一段 40 位 token 露掉大半。前 4 + 后 4 足以让同一个人认出「是同一个值」。
const (
	maskShort = 2
	maskLong  = 4
	shortLen  = 24
)

var (
	// URL 里的 user:password@：代理地址、上游地址都可能带。
	reUserinfo = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.\-]*://)([^\s/:@]+):([^\s/@]+)@`)
	// Authorization: Bearer xxx
	reBearer = regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=\-]{12,})`)
	// 裸的 sk- 密钥（含厂商前缀变体）。
	reSK = regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{12,}\b`)
	// JWT：三段 base64url。
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{4,}\b`)
	// query / 表单里的敏感字段：?access_token=...&password=...
	reKV = regexp.MustCompile(`(?i)\b((?:access_token|refresh_token|id_token|api_?key|secret|password|passwd|client_secret|token)=)([^&\s"',;]{8,})`)
	// JSON 里的同名字段："access_token":"..."
	reJSONKV = regexp.MustCompile(`(?i)("(?:access_token|refresh_token|id_token|api_?key|secret|password|passwd|client_secret|token)"\s*:\s*")([^"]{8,})(")`)
	// Cookie / Set-Cookie 头的整段值。
	reCookie = regexp.MustCompile(`(?i)\b((?:set-)?cookie\s*:\s*)([^\r\n]+)`)
	// 邮箱：本地部分只留开头。
	reEmail = regexp.MustCompile(`\b([A-Za-z0-9._%+\-]{1,64})@([A-Za-z0-9.\-]+\.[A-Za-z]{2,})\b`)
	// 中国大陆手机号：138****8000。
	rePhone = regexp.MustCompile(`\b(1[3-9]\d)\d{4}(\d{4})\b`)
	// 私钥块（多行，Text 支持整段传入时一并盖掉）。
	rePrivateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
)

// Text 返回脱敏后的文本。没有命中任何规则时返回原串本身。
func Text(s string) string {
	if s == "" {
		return s
	}
	if rePrivateKey.MatchString(s) {
		s = rePrivateKey.ReplaceAllString(s, "[私钥已脱敏]")
	}
	if strings.Contains(s, "://") {
		s = reUserinfo.ReplaceAllStringFunc(s, func(m string) string {
			// 保留 scheme 与用户名（用户名本身不是秘密），密码盖掉。
			sub := reUserinfo.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			return sub[1] + sub[2] + ":***@"
		})
	}
	if strings.Contains(strings.ToLower(s), "bearer") {
		s = reBearer.ReplaceAllStringFunc(s, func(m string) string {
			sub := reBearer.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			return sub[1] + Mask(sub[2])
		})
	}
	if strings.Contains(s, "sk-") {
		s = reSK.ReplaceAllStringFunc(s, Mask)
	}
	if strings.Contains(s, "eyJ") {
		s = reJWT.ReplaceAllStringFunc(s, Mask)
	}
	if strings.Contains(s, "\"") {
		s = reJSONKV.ReplaceAllStringFunc(s, func(m string) string {
			sub := reJSONKV.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			return sub[1] + Mask(sub[2]) + sub[3]
		})
	}
	if strings.Contains(s, "=") {
		s = reKV.ReplaceAllStringFunc(s, func(m string) string {
			sub := reKV.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			return sub[1] + Mask(sub[2])
		})
	}
	if low := strings.ToLower(s); strings.Contains(low, "cookie") {
		s = reCookie.ReplaceAllStringFunc(s, func(m string) string {
			sub := reCookie.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			return sub[1] + maskCookieValue(sub[2])
		})
	}
	if strings.Contains(s, "@") {
		s = reEmail.ReplaceAllStringFunc(s, func(m string) string {
			sub := reEmail.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			local := sub[1]
			keep := 2
			if len(local) < keep {
				keep = len(local)
			}
			return local[:keep] + "***@" + sub[2]
		})
	}
	if rePhone.MatchString(s) {
		s = rePhone.ReplaceAllString(s, "${1}****${2}")
	}
	return s
}

// Mask 把一个凭证缩成「前 4 + *** + 后 4」，短值收到 2 位。
// 原文不足 8 位时全部盖掉——太短的值保留任何一位都可能被猜出来。
func Mask(v string) string {
	if v == "" {
		return v
	}
	if len(v) < 8 {
		return "***"
	}
	keep := maskLong
	if len(v) < shortLen {
		keep = maskShort
	}
	return v[:keep] + "***" + v[len(v)-keep:]
}

// Hint 给面板展示用：只留前缀，不暴露长度（长度本身也是信息）。
func Hint(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 6 {
		return "***"
	}
	return v[:3] + "***"
}

// maskCookieValue 把 Cookie 头里的每个 k=v 的值盖掉，保留 cookie 名。
// 一条 cookie 头里通常有好几个键，整条盖掉会让排查「带了没有」变得不可能。
func maskCookieValue(v string) string {
	parts := strings.Split(v, ";")
	for i, p := range parts {
		k, val, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		if len(strings.TrimSpace(val)) > 0 {
			parts[i] = k + "=***"
		}
	}
	return strings.Join(parts, ";")
}
