// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

//go:build windows

package upstream

import "syscall"

// platformInfraErrnos Windows 上的套接字错误码。
//
// 坑：Windows 版 Go 里 syscall.ECONNREFUSED / ECONNRESET / ECONNABORTED / ETIMEDOUT
// 是应用层造的伪常量（0x20000000 段），真正从 connect/recv 冒出来的始终是 Winsock 的
// WSA* 码（连接被拒 = 10061）。所以只比对 syscall.ECONNREFUSED 会永远不匹配，表现为
// 「Windows 上连不上端口不算基础设施失败」——熔断器不计数、故障转移不换 IP。
// Windows 的 Winsock 错误码。syscall 只导出了 WSAECONNRESET，其余三个按 winsock2.h
// 的定义直接写数值（10053 ECONNABORTED / 10060 ETIMEDOUT / 10061 ECONNREFUSED）。
const (
	wsaECONNABORTED = syscall.Errno(10053)
	wsaETIMEDOUT    = syscall.Errno(10060)
	wsaECONNREFUSED = syscall.Errno(10061)
)

func platformInfraErrnos() []error {
	return []error{
		syscall.WSAECONNRESET,
		wsaECONNABORTED,
		wsaECONNREFUSED,
		wsaETIMEDOUT,
	}
}
