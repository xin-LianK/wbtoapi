// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

package session

import (
	"runtime"
	"testing"
)

// skipPosixOnly 跳过依赖 POSIX 权限位/目录语义的用例（Windows 上 0500 目录
// 仍可写、权限位恒为 0666），由 Linux CI 覆盖。
func skipPosixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX 权限语义，Windows 上行为不同（Linux CI 覆盖）")
	}
}
