// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

//go:build !windows

package upstream

// platformInfraErrnos 非 Windows 平台不需要额外错误码：POSIX 的 ECONNRESET /
// ECONNABORTED / ECONNREFUSED / ETIMEDOUT 就是套接字层真实返回的 errno，
// failoverIsInfraError 已直接比对。
func platformInfraErrnos() []error { return nil }
