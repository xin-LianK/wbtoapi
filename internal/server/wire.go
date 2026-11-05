// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// wire.go 协议层共用的取值辅助：把解出来的 any 安全地收窄成 int / string /
// map / slice。原先这里还有一套 ResponseWriter 层的翻译基座（sseTap），改成
// SDK 翻译器后整段不再被调用，已删除；这几个取值函数仍被 bridge.go 使用。
package server

import "encoding/json"

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
