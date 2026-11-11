// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// identity.go 出站身份自动换线：官方同一套账号背后按客户端形态分了几条配额线（见
// upstream/identity.go 的形态依据）。账号在凭证里设 product="auto" 时，桌面端那条被 429 限流
// 不代表这个号没额度了——换 VSCode 扩展那条可能还能用。
//
// 与远端的差异（有意保守）：
//   - 只在 **429** 上换，且只换身份、**不换号**：402 余额不足、11140 限制这类是账号级事实，
//     换形态也一样；5xx / 通道校验失败（11128）换形态只会把请求放大几倍。
//   - 只对 product=auto 的号生效。用户给某个号钉了 desktop / vscode / cli 就是明确表态，
//     网关不去猜。默认 product 就是桌面端，所以这项能力默认不参与任何出站。
//   - 换到下一跳仍是 429 才继续往下换，最多 3 次（三条线走完即止）。HTTP 状态码与错误
//     分类仍由 handler 里原来那段分支统一处置，这里只管「这一跳用哪套身份」。
package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxIdentitySwitches 一个请求最多在同一个号上换几次身份（三条线走完就没了，留 3 兜底）。
const maxIdentitySwitches = 3

// identityOrder 自动换线的顺序：桌面 → VSCode → CLI。
// 桌面端是默认形态、也是唯一 2026-10 生产实测过的；VSCode 扩展的指纹来自解包（未用真扩展
// 抓包核对），所以排在桌面端之后；CLI 与桌面端只差一个 ACP 头，放最后。
var identityOrder = []string{auth.ProductDesktop, auth.ProductVSCode, auth.ProductCLI}

// isRateLimited 这一跳是不是限流（429）。分类以 upstream.Error 的 Kind 为准：ErrSoftRate
// 覆盖 429 与「200 已开流 + 限流 error 帧」两种形态（后者走不到这里，但在分类口径上同源）。
// terr 为 nil 时按状态码兜底（Classify 的防御分支会漏 Kind）。
func isRateLimited(status int, terr error) bool {
	var ue *upstream.Error
	if errors.As(terr, &ue) {
		return ue.Kind == upstream.ErrSoftRate
	}
	return terr == nil && status == http.StatusTooManyRequests
}

// nextAutoIdentity 这一跳被 429 之后，同一个号该用哪套身份重发；返回空表示不换（直接交给
// 原来的错误处置）。meta.Identity 是已经生效的身份（空 = 按账号凭证，auto 落到桌面端）。
func nextAutoIdentity(acct *auth.Auth, meta upstream.ChatMeta, status int, terr error) string {
	if acct == nil || !isRateLimited(status, terr) {
		return ""
	}
	// 钉死身份的号不换：用户指定 desktop / vscode / cli 就按他说的发。
	if acct.ProductValue() != auth.ProductAuto {
		return ""
	}
	cur := auth.NormalizeProduct(meta.Identity)
	for i, id := range identityOrder {
		if id != cur {
			continue
		}
		if i+1 >= len(identityOrder) {
			return "" // 已经到最后一条线
		}
		return identityOrder[i+1]
	}
	return identityOrder[0]
}

// chatAttemptWith 发一次上游对话（含 6020 容量排队），把 meta 原样带进去——出站身份就在
// meta.Identity 里，所以换身份重发只要改 meta 再调一次。
func (h *Handler) chatAttemptWith(ctx context.Context, acct *auth.Auth, body []byte, clientIP string, meta upstream.ChatMeta, model string, started time.Time) chatAttempt {
	a := chatAttempt{started: started}
	a.rc, a.status, a.body, a.err = h.cfg.Upstream.ChatStreamContext(ctx, acct, body, clientIP, meta)
	if isQueueWaiting(a.err) {
		// 6020 排进容量队列了：在这个号上照官方排队，到号了原样重发（见 capqueue.go）。
		a = h.capacityQueue(ctx, acct, body, clientIP, meta, model, a)
	}
	return a
}
