// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersion 出站 WorkBuddy 客户端版本段（UA 的 `WorkBuddy/<ver>` 与
	// X-IDE-Version）。对齐官方 WorkBuddy Desktop 当前分发包版本（5.8.3，
	// 2026-10-21 由 www.codebuddy.cn/v2/update?platform=workbuddy-win32-x64-user 取得，
	// 与本机 app.asar 的 package.json version 一致）。
	// config upstream.client_version 可覆盖（空 = 内置默认）。
	defaultClientVersion = "5.8.3"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。官方取值来自
	// productManager 下发的 platformVersion（buildVlmGatewayHeaders 的 cliVersion）。
	// 5.8.3 安装包内嵌的 CLI 为 @tencent-ai/codebuddy-code 2.161.0
	// （resources/app.asar.unpacked/cli/dist/codebuddy-lite-wb.mjs 内 customPackage.version）。
	// config upstream.cli_version 可覆盖（空 = 内置默认），实测到新值优先改配置。
	defaultCliVersion = "2.161.0"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之，
// 否则内置默认 defaultClientVersion。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方桌面端 RestOperations 层形状）：
// `WorkBuddy/<clientVersion> <platform>/<clientVersion> CLI/<cliVersion>`。
// 平台段（第二段）品牌按 realm 切换——CN 用 applicationName 同值 `WorkBuddy`，
// global 用官方国际版 productName `WorkBuddy AI`（intl 项目逆向证据：
// `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2`）。
// global 账号送错平台段（`WorkBuddy` 非 `WorkBuddy AI`）可能触发上游 403 code 11140
// "request illegal" 风控。官方无任何 UA 随机化，故默认确定性。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	platform := "WorkBuddy"
	if a != nil && a.IsGlobal() {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + c.clientVersion() + " " + platform + "/" + c.clientVersion() + " CLI/" + c.cliVersion()
}

// defaultWorkBuddyUA 返回 CN 形态的默认 UA（默认账号形态即 CN，零回归兼容既有调用/测试）。
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态（global 换 `WorkBuddy AI` 平台段）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner 显式覆写形态，不带 CLI 段）。默认生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 才不设 UA（还原旧行为，Go 默认 UA）。
func (c *Client) billingUA() string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion()
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包派生盐的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。用 sha256 与项目
// 既有派生（session/ids.go、cache_key.go）保持一致；截 36 hex 提供更长熵。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
//
// 与 injectDeviceToken 的 X-Device-Token 并存不冲突：那是登录时上游签发的
// 真实设备令牌（有则发，权威）；本对头是「每账号一台固定虚拟设备」的稳定指纹，
// 防多号被上游按设备指纹缺失/漂移关联风控。两者是不同头族，官方桌面端都发。
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// injectAccountStableHeaders 在 req 注入 X-Machine-ID / X-Session-ID：按 uid 稳定
// 派生，跨重启固定、账号间互异。uid 为空时不注入（匿名请求无设备标识，上游不要求）。
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// desktopProfile 报告是否使用「WorkBuddy 桌面端 37.x」出站头形态。
// 缺省（Profile 为空）即 desktop：这是从本机 app.asar 实测得到的当前真实形态。
// 显式 "legacy" 回到停更前的 CodeBuddy IDE 5.x 形态（仅用于回滚对比）。
func (c *Client) desktopProfile() bool {
	if c == nil {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(c.Profile), "legacy")
}

// ideVersion X-IDE-Version 取值。
//
// 权威来源是 WorkBuddy 桌面端 buildVlmGatewayHeaders：
//
//	n["X-IDE-Version"] = appVersion?.trim() || "0.0.0"
//
// 即真实产品版本（如 5.8.3）。applyGatewayHeaders 里 YN 常量表的
// ["X-IDE-Version","0.0.0"] 只是 `e.set(n, e.get(n) || r)` 的**兜底**——真实值先设好
// 就不会落到 0.0.0。所以这里必须跟随 clientVersion，写成 0.0.0 反而是错的。
func (c *Client) ideVersion() string {
	if c != nil && strings.TrimSpace(c.IDEVersion) != "" {
		return c.IDEVersion
	}
	return c.clientVersion()
}

// product X-Product 取值。桌面端取 deploymentType，缺省 "SaaS"。
// 写成 "WorkBuddy" 是最常见也最致命的错误——客户端从不发这个值。
func (c *Client) product() string {
	if c != nil && strings.TrimSpace(c.Product) != "" {
		return c.Product
	}
	return "SaaS"
}

// uuidDashed 把 32 位 hex 转成 UUID v4 文本形态（8-4-4-4-12）。
//
// 只插横线不够：官方所有 ID 都出自 crypto.randomUUID()，第 13 位恒为 "4"
// （版本位）、第 17 位恒为 8/9/a/b（变体位）。裸 hex 加横线会产出
// "…-cd98-4a67-…" 这种版本位乱跳的值，一条正则就能和官方区分开。
func uuidDashed(h string) string {
	if len(h) != 32 {
		return h
	}
	b := []byte(h)
	b[12] = '4'
	// 变体位：保留低 2 bit 熵，高位固定为 10b（即 8/9/a/b）。
	i := strings.IndexByte("0123456789abcdef", b[16])
	if i < 0 {
		i = 0
	}
	b[16] = "89ab"[i&0x3]
	return string(b[0:8]) + "-" + string(b[8:12]) + "-" + string(b[12:16]) + "-" + string(b[16:20]) + "-" + string(b[20:32])
}

// applyDesktopChatHeaders 把 ChatHeaders 的结果修正为桌面端真实形态。
// 只作用于 chat 路径：refresh/billing 走的是插件域，形态本就不同，不在此列。
//
// 依据（2026-10-21 从 WorkBuddy 5.8.3 app.asar 的 buildVlmGatewayHeaders /
// applyGatewayHeaders / applyTraceHeaders 提取，并用本机安装的 CodeBuddy IDE 4.12.2
// 的 genie 扩展交叉验证）：
//   - Origin / Referer 被客户端显式 strip（qN 集合含 origin），不发；
//   - Accept-Language 不在构造里，不发；
//   - X-Machine-ID 在 chat 链路零出现（WorkBuddy 与 CodeBuddy IDE 均无；
//     IDE 侧只有 feedback 接口的 X-Machine-Id），纯造头，删；
//   - X-Session-ID 只在 IDE 的代码补全链路发（来自 sessionId），chat 不发，删；
//   - Accept 固定 application/json（流式也如此，响应侧才是 text/event-stream）；
//   - Accept-Encoding 显式 identity（禁用压缩）；
//   - 额外发 acp-connection-id / X-Private-Data / X-Agent-Intent（见各注入点）。
func (c *Client) applyDesktopChatHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	req.Header.Del("Accept-Language")
	req.Header.Del("X-Machine-ID")
	req.Header.Del("X-Session-ID")
	// X-No-* 是插件域（旧实现）的「显式声明缺省」约定，官方桌面端 chat 从不发。
	// 官方是「有值才设」，没有对应的 X-No-* 头（buildVlmGatewayHeaders 全是
	// `e.userId?.trim() && (...)` 这种短路写法）。留着就是可识别特征。
	req.Header.Del("X-No-User-Id")
	req.Header.Del("X-No-Enterprise-Id")
	req.Header.Del("X-No-Department-Info")
	req.Header.Set("Accept-Encoding", "identity")
	// 设备身份三件套（X-Device-Token / X-Custom-Channel-Id / X-Device-Id-List）
	// 由 WorkbuddyAuthStateHeaderProvider 产出，但**只喂给 app-server 的
	// auth-state 与签到接口**，不在 chat 链路里：
	//   - chat 的注入钩子 getRequestHeaders 接的是 buildVlmGatewayHeaders，
	//     不是这个 provider（main/server.js @3526839 可证）；
	//   - X-Device-Token 在桌面端只出现在 getCheckinRequestHeaders()。
	// 所以 chat 一个都不能带。给 chat 补这三个头，等于把「反代特征」从
	// 少发变成多发——和旧实现造 X-Machine-ID 是同一类错误。
	req.Header.Del("X-Custom-Channel-Id")
	req.Header.Del("X-Device-Id-List")
	// X-Device-Token 例外：可用 upstream.chat_device_token 显式打开，
	// 用于验证「上游是否按设备绑定账号」。默认关闭 = 与官方一致。
	if !c.ChatDeviceToken {
		req.Header.Del("X-Device-Token")
	}
	// 官方 chat 请求头 Accept 恒为 application/json（applyGatewayHeaders 写死），
	// 流式与否只影响 body 的 stream 字段与响应类型。发 event-stream 属可识别偏差。
	req.Header.Set("Accept", "application/json")
	// 企业号在 chat 域也发 X-Tenant-Id（旧实现只在 billing 域发）。
	if a != nil && a.EnterpriseID != "" {
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	// ACP 连接标识：官方是每连接一个 randomUUID()（带连字符）。这里按账号稳定派生，
	// 形态必须对齐（uuidDashed）——裸 36 hex 与官方 UUID 一眼可辨。
	if a != nil && a.UID != "" {
		req.Header.Set("acp-connection-id", uuidDashed(deriveAccountStableID(a.UID, "acp")[:32]))
	}
}

// CommonHeaders 设置所有 API 共享的请求头。

func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 ChatHeaders 覆盖为 event-stream。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders——chat 经 ChatHeaders
	// 叠加 CommonHeaders 天然继承；billing 域另行注入，全出站覆盖。
	c.injectAccountStableHeaders(req, a)
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：body 提取的入站值，空则不发（透传优先，不伪造）
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// chat 流式 Accept 覆盖 CommonHeaders 的非流式默认（D6）。
	req.Header.Set("Accept", "application/json, text/event-stream")
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 走既有分支（EnterpriseID/Domain 原样透传，缺省 X-No-*）；
	// global 账号由 injectGlobalChatHeaders 统一覆写为国际客户端形态
	// （X-No-Enterprise-Id=1 声明无企业 + X-Domain=www.workbuddy.ai 声明国际版域），
	// 且不回退 X-Domain 到登录会话原值——对齐 intl 项目出站头。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：默认伪造 WorkBuddy 桌面端指纹（client_name="SaaS" 还原旧行为）。
	c.injectAttribution(req)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
	// 桌面端形态修正放在最后：以「覆盖/删除」的方式收敛，不改动 common 与选号链路。
	if c.desktopProfile() {
		c.applyDesktopChatHeaders(req, a)
	}
}

// injectGlobalChatHeaders global 账号（无企业 ID）的 chat 专属声明头，对齐 intl 项目
// （ANALYSIS-global-chat-solutions.md）：
//   - X-No-Enterprise-Id: 1  个人账号无企业 ID，显式声明（避免上游按缺省/可疑判定）
//   - X-Domain: www.workbuddy.ai  显式声明国际版域（与 Origin/Referer 同域）
//
// 仅 global realm 注入；CN 账号走既有 X-No-Department-Info 等分支，零回归。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定（body 的 conversationId）。空则不发——
//     透传客户端原值优先，客户端没给就不伪造，避免误导后台建错会话。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID：入站透传或 = conversationRequestID。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：链路族。B3 规范只认 16/32 hex
//     TraceId 与 16 hex SpanId；入站 conversationRequestID 非法时 TraceId 回落
//     messageID（恒 32 hex），SpanId 取 messageID[:16]（每消息新）。
func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	if c.desktopProfile() {
		c.injectDesktopConversationHeaders(req, meta)
		return
	}
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	// 会话头三元组必须齐全：国际端点收紧通道校验后，缺 X-Conversation-ID 的
	// 请求会直接 400 / 11128（conversation id missing）。入站没带会话 ID 时
	// 本级补一个 32 hex（与 X-Conversation-Request-ID 的零值兜底同口径），
	// 不再因为「透传优先」而漏发该头（issue #190）。
	conversationID := meta.ConversationID
	if conversationID == "" {
		conversationID = session.NewMessageID()
	}
	req.Header.Set("X-Conversation-ID", conversationID)
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}
	req.Header.Set("X-Trace-ID", traceID)
	b3Trace := convReqID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // 非法 B3 TraceId → 回落恒 32 hex 的消息级 ID
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

// injectDesktopConversationHeaders 桌面端 37.x 的会话/链路头族。
// 与 legacy 的差异：
//   - 消息级 ID 用带连字符 UUID（客户端用 randomUUID()）
//   - 不发 X-Root-Request-ID（网关路径没有这个头，多发即特征）
//   - 补全 W3C traceparent + 单头 b3 + X-B3-ParentSpanId
func (c *Client) injectDesktopConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		convReqID = uuidDashed(session.NewMessageID())
	}
	messageID := uuidDashed(session.NewMessageID())
	// 官方 applyGatewayHeaders 是两次独立的 randomUUID()：
	//   X-Conversation-Message-ID = randomUUID()
	//   X-Request-ID              = randomUUID()
	// 不能复用同一个值——「消息 ID 与请求 ID 恒等」是真实客户端不会出现的形态。
	requestID := uuidDashed(session.NewMessageID())
	conversationID := meta.ConversationID
	if conversationID == "" {
		conversationID = uuidDashed(session.NewMessageID())
	}
	// 链路 trace：官方 applyTraceHeaders **每次请求都重新 randomBytes(16)**，
	// 与 conversationRequestId 无任何关系。早期实现拿 convReqID 当兜底，
	// 结果 X-Trace-ID 恒等于 X-Conversation-Request-ID（仅去掉横线）——
	// 两个本应独立的随机值永远相等，一条比对就能和真实客户端区分开。
	traceID := strings.ReplaceAll(meta.TraceID, "-", "")
	if len(traceID) != 32 || !validTraceID(traceID) {
		traceID = strings.ReplaceAll(session.NewMessageID(), "-", "")
	}
	if len(traceID) != 32 || !validTraceID(traceID) {
		traceID = strings.ReplaceAll(messageID, "-", "")
	}
	spanID := strings.ReplaceAll(session.NewMessageID(), "-", "")[:16]
	parentID := strings.ReplaceAll(session.NewMessageID(), "-", "")[:16]

	req.Header.Set("X-Conversation-ID", conversationID)
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("X-Trace-ID", traceID)
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("b3", traceID+"-"+spanID+"-1-"+parentID)
	req.Header.Set("X-B3-TraceId", traceID)
	req.Header.Set("X-B3-ParentSpanId", parentID)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-Sampled", "1")
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// 默认（ClientName 空）即对齐官方 WorkBuddy 桌面端指纹：X-Agent-Purpose="conversation"
// + X-IDE-Name/Type/Product="WorkBuddy" + X-IDE-Version=client_version，上游用量归因
// 不再出现 client/agentPurpose 为空的「网关特征」。显式 ClientName="SaaS" 还原旧行为
// （仅 X-Product="SaaS"，不设 X-IDE-*）；配其他值则四头跟随该值。
func (c *Client) injectAttribution(req *http.Request) {
	// 桌面端 5.8.3：X-IDE-* 恒为 WorkBuddy，X-IDE-Version 取 appVersion（真实版本），
	// X-Product 取 deploymentType（缺省 SaaS），并额外带 X-Private-Data / X-Agent-Intent。
	// 这四个头官方**每个 chat 请求都发**，缺任何一个都比版本号写错更显眼。
	if c.desktopProfile() {
		req.Header.Set("X-Agent-Purpose", "conversation")
		req.Header.Set("X-Agent-Intent", "craft")
		req.Header.Set("X-IDE-Type", "WorkBuddy")
		req.Header.Set("X-IDE-Name", "WorkBuddy")
		req.Header.Set("X-IDE-Version", c.ideVersion())
		req.Header.Set("X-Product", c.product())
		req.Header.Set("X-Private-Data", "true")
		return
	}
	name := c.attributionClientName()
	if name == "SaaS" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersion())
	req.Header.Set("X-Product", name)
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
