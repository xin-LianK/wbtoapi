// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// Package testutil 放各包共用的测试辅助。只被 _test.go 引用，不进二进制。
package testutil

import (
	"runtime"
	"testing"
)

// SkipPosixOnly 跳过依赖 POSIX 权限位/目录语义的用例。
//
// Windows 上这些前提不成立（本机实测）：
//   - 0500 目录对属主仍可写，写盘不会失败；
//   - 权限位恒为 0666，0600 的收敛断言永远不成立；
//   - os.ReadDir(常规文件) 被映射成 fs.ErrNotExist，「目录被文件占住」变成
//     「目录不存在」，读/清空/统计都会假装成功。
//
// 这些分支由 Linux CI 覆盖；本地 Windows 跳过，避免把平台差异当成回归。
func SkipPosixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX 权限/目录语义，Windows 上行为不同（Linux CI 覆盖）")
	}
}
