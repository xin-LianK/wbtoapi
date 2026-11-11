// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// identity.go 出站身份（产品形态）。官方同一套账号背后有几条按客户端形态分的配额线：桌面端打满了，
// VSCode 扩展那条可能还能用。账号可以指定自己用哪套身份发对话（凭证里的 product），也可以交给网关在
// 被 429 时自动往下一条换（server/identity.go）。
//
// 依据：
//   - 桌面端：WorkBuddy 桌面端安装包（5.9.3 / 5.9.4）内置的 CLI，就是 headers.go 一直在对齐的那套，
//     2026-10 生产实测过。
//   - VSCode：Open VSX 上的 Tencent-Cloud.coding-copilot 4.12.39997873 解包源码里的
//     UserAgentHttpInterceptor / CustomHeadersHttpProxyInterceptor / buildModelRequestHeaders。
//     **网关没有用真扩展抓包核对过**（标「未确认」），因此默认不启用，只有账号显式设成 vscode 才发。
//   - CLI：本仓库默认的出站形态本来就是桌面端内置 CLI 的形状（UA 里的 `CLI/<ver>` 段、X-Stainless-*、
//     对话经 SDK 发出），所以 product=cli 与桌面端只有一处可测差别：它是纯 CLI 进程，没有 ACP 连接，
//     不发 acp-connection-id。保留这个值是为了让「按身份分池」的配置写得清楚，不是造第三套指纹。
package upstream

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// VSCode 扩展身份的内置取值（解包 4.12.39997873 的 product.json 与拦截器源码）。
const (
	vscodeIDEName = "VSCode"
	// defaultVSCodeProductVersion 扩展自己的版本（X-Product-Version 与 UA 的第二段）。
	defaultVSCodeProductVersion = "4.12.39997873"
	// defaultVSCodeIDEVersion 宿主 VSCode 版本（扩展从 vscode.env 取）。网关没有宿主，取一个常见版本；
	// 使用者应该按自己真实的 VSCode 版本改，见 config upstream.vscode_ide_version。
	defaultVSCodeIDEVersion = "1.119.0"
	// vscodeEnvID 官方 HttpEnvRouteInterceptor 在 developerMode 关闭时固定为 production。
	vscodeEnvID = "production"
)

// effectiveIdentity 这次出站用哪套身份：ChatMeta.Identity（服务端按 429 换身份时显式指定）优先，
// 其次是账号凭证里的 product；auto 落到桌面端。
func (c *Client) effectiveIdentity(a *auth.Auth, meta ChatMeta) string {
	if meta.Identity != "" {
		return auth.NormalizeProduct(meta.Identity)
	}
	if a == nil {
		return auth.ProductDesktop
	}
	if p := a.ProductValue(); p != auth.ProductAuto {
		return p
	}
	return auth.ProductDesktop
}

// vscodeIDEVersion 生效的宿主 VSCode 版本。
func (c *Client) vscodeIDEVersion() string {
	if c != nil && strings.TrimSpace(c.VSCodeIDEVersion) != "" {
		return strings.TrimSpace(c.VSCodeIDEVersion)
	}
	return defaultVSCodeIDEVersion
}

// vscodeProductVersion 生效的扩展版本。
func (c *Client) vscodeProductVersion() string {
	if c != nil && strings.TrimSpace(c.VSCodeProductVersion) != "" {
		return strings.TrimSpace(c.VSCodeProductVersion)
	}
	return defaultVSCodeProductVersion
}

// vscodeUA VSCode 扩展的 UA：`<platform>/<宿主版本> <productName>/<扩展版本>`
// （UserAgentHttpInterceptor.buildUserAgent，platform=vscode、productName=CodeBuddy）。
func (c *Client) vscodeUA() string {
	return vscodeIDEName + "/" + c.vscodeIDEVersion() + " CodeBuddy/" + c.vscodeProductVersion()
}

// applyVSCodeChatHeaders 把 chat 的请求头换成 VSCode 扩展的形态。
//
// 和桌面端的差别（解包源码，未用真扩展抓包核对）：
//   - UA 是扩展自己的两段，X-IDE-Type / X-IDE-Name 是 VSCode、X-IDE-Version 是宿主版本，另带
//     X-Product-Version 与 X-Env-ID；
//   - X-Request-Trace-Id：每个请求一个带连字符的 UUID；X-Request-ID 与 X-Conversation-Request-ID
//     是同值，X-Conversation-* 全用 32 位无连字符 hex；
//   - 模型请求另带 X-Model-ID 与两个毫秒时间戳（BaseAgent 塞进 customHeaders 的）；
//   - 没有 X-Agent-Runtime / X-Agent-Purpose / acp-connection-id，也不经 OpenAI SDK，没有 X-Stainless-*；
//   - X-Private-Data 只在国际站发（国内站不发）。
func (c *Client) applyVSCodeChatHeaders(req *http.Request, a *auth.Auth, meta ChatMeta) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	for _, h := range []string{
		"X-Stainless-Retry-Count", "X-Stainless-Timeout", "X-Stainless-Lang", "X-Stainless-Package-Version",
		"X-Stainless-OS", "X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version",
		"X-Agent-Runtime", "X-Agent-Purpose", "acp-connection-id",
	} {
		req.Header.Del(h)
	}
	req.Header.Set("User-Agent", c.vscodeUA())
	req.Header.Set("X-IDE-Type", vscodeIDEName)
	req.Header.Set("X-IDE-Name", vscodeIDEName)
	req.Header.Set("X-IDE-Version", c.vscodeIDEVersion())
	req.Header.Set("X-Product-Version", c.vscodeProductVersion())
	req.Header.Set("X-Env-ID", vscodeEnvID)
	req.Header.Set("X-Request-Trace-Id", uuidDashed(session.NewMessageID()))
	// 会话头族改 32 位无连字符形态（BaseAgent 的 uuidv4().replace(/-/g,"")），
	// 并且 X-Request-ID 与轮级聚合 id 同值（官方 model-provider 就是这么发的）。
	convID := strings.ReplaceAll(req.Header.Get("X-Conversation-ID"), "-", "")
	convReqID := strings.ReplaceAll(req.Header.Get("X-Conversation-Request-ID"), "-", "")
	if convID != "" {
		req.Header.Set("X-Conversation-ID", convID)
	}
	if convReqID != "" {
		req.Header.Set("X-Conversation-Request-ID", convReqID)
		req.Header.Set("X-Request-ID", convReqID)
	}
	if a == nil || !a.IsGlobal() {
		req.Header.Del("X-Private-Data") // 国内站不发（CraftInvokableAgent 的拦截器只在非国内站加）
	}
	if meta.Model != "" {
		now := strconv.FormatInt(time.Now().UnixMilli(), 10)
		req.Header.Set("X-Model-ID", meta.Model)
		req.Header.Set("monitor_promptPrepareStartTime", now)
		req.Header.Set("monitor_httpSendTime", now)
	}
}

// applyCLIChatIdentity CLI 身份的差别只有一处（见文件头）：纯 CLI 进程没有 ACP 连接。
func (c *Client) applyCLIChatIdentity(req *http.Request) {
	req.Header.Del("acp-connection-id")
}
