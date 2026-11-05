// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package server

import (
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// resolveModelDetail 解析模型名协议（PLAN D6），并区分「realm 是否由调用方显式写出」：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 第三个返回值 realmExplicit 供跨域回退与跨域负载均衡判定：只有裸名（false）才允许
// 在首选域选不到号时改用另一域；显式 cn:/global: 前缀永不被改写。
func resolveModelDetail(model string) (realm, bare string, realmExplicit bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model, false
	}
	return prefix, model[idx+1:], true
}

// resolveModel 是 resolveModelDetail 的两返回值形态（既有调用方语义零改动）。
func resolveModel(model string) (realm, bare string) {
	realm, bare, _ = resolveModelDetail(model)
	return realm, bare
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// otherRealm 返回另一个选号域（"cn" ↔ "global"），供跨域回退使用。
func otherRealm(realm string) string {
	if realm == "global" {
		return "cn"
	}
	return "global"
}

// crossRealmOn pool.cross_realm 开着没有（合池或国内版优先）：开着时不带前缀的模型名可以分到两个域。
func crossRealmOn(mode string) bool {
	return mode == livecfg.CrossRealmMerge || mode == livecfg.CrossRealmCNFirst
}

// crossRealmFor 跨域负载均衡（pool.cross_realm）下，不带前缀的模型名该在哪个域选号：
// 只在模型目录里确有这个模型的域里选。两域都有 → ""（合池，选号不加域过滤）；
// 只有一域有 → 该域；两边目录都没有（未拉到或模型不存在）→ "cn"，与关闭开关时一致。
func crossRealmFor(bare string, has func(realm, model string) bool) string {
	cn, global := has("cn", bare), has("global", bare)
	switch {
	case cn && global:
		return ""
	case global:
		return "global"
	default:
		return "cn"
	}
}

// cnFirstRealm 国内版优先（pool.cross_realm 为 cn_first，两个域都有这个模型）这次在哪个域选号：国内版此刻
// 有能接的号（tried 里的除外，口径见 pool.ServableForModel）就只在国内版选；没有而国际版有，就在国际版选；
// 两边都没有时为 ""，照合池走全冷却兜底和排队，两个域的号都算。
func (h *Handler) cnFirstRealm(tried map[string]bool, model string) string {
	switch {
	case h.cfg.Pool.ServableForModel(tried, model, "cn"):
		return "cn"
	case h.cfg.Pool.ServableForModel(tried, model, "global"):
		return "global"
	}
	return ""
}
