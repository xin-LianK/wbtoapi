# 上游失效排查决策树

> 这份文档只解决一件事：**上游变了之后，怎么在 10 分钟内判断是「能修」还是「先放弃」。**
> 代码基线：`73a7560`（作者停更时的最后一版），对应 v1.11.7。
> 所有行号以该 commit 为准，改过代码后自行 ±。

---

## 0. 先分清两类失效

| 类型 | 特征 | 可修性 | 典型耗时 |
|---|---|---|---|
| **A. 协议漂移** | 端点 404、参数报 11101、模型名不存在、返回结构变了 | 能修，通常是单点改动 | 半小时～1 天 |
| **B. 通道校验收紧** | 11128 `request illegal`、403 空体、账号批量 disabled | 难修，且修完还会再变 | 取决于能否拿到新版客户端指纹 |

作者停更的理由是 B 类。**如果你的场景必须稳定，先接受「B 类无法长期对抗」这个前提，再决定投入。**

---

## 1. 三十秒分层体检

```bash
# ① 网关自己活着吗（不需要 key）
curl -s http://127.0.0.1:7863/healthz

# ② 账号池状态（需要 key）
KEY=$(./deploy.sh key)
curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:7863/status | python -m json.tool

# ③ 真正打一次上游（用面板里显示的模型名）
curl -s -N http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"<模型名>","stream":false,"messages":[{"role":"user","content":"hi"}]}'
```

第 ③ 步的返回体是全部线索：`error.code` / `error.message` 直接对应下面第 3 节的字典。
面板「请求记录」里有逐请求的上游状态码、错误分类（kind）、命中的 uid，比看容器日志快。

---

## 2. 决策树

### A. 所有账号同时变红 / `/status` 里 healthy = 0

```
A1  401 且 body 含 "Offline user session not found" 或 12153
    → ErrSessionDead：refreshToken 也废了。该号只能重新扫码登录。
    → 若**全部**号同时这样，先查 refresh 端点是否变了：
       internal/upstream/client.go:1113  POST {chatBase}/v2/plugin/auth/token/refresh
       用面板「测试」或手工 curl 打一次，看是 404（路径变了）还是 400（参数变了）。

A2  401/403 且 body 含 "request illegal"，code=11128
    → ErrChannelRefused：通道校验失败，上游认为你不是官方客户端。
    → **先排除节奏**（第 6.3 节）：把 pacing.per_minute 砍到 20-30 观察 24h，
      再决定动不动 header。短时间高频出站会触发同一类拒绝，改头是白费功。
    → 节奏排除后再进第 5 节（指纹）逐项比对请求头。

A3  body 含 "request illegal"，code=11140
    → ErrAccountFault：账号被上游判定异常行为/风控。换号，别重试。
    → 若批量出现，说明整批号的注册/使用行为已被标记，池子该重建了。

A4  body 含 "trial not activated" / code=14017
    → 账号未完成试用激活（register 没走完），不是代码问题。

A5  402 或 body 含积分不足类文案
    → 余额耗尽。跑签到/成长任务，或换号。
```

> ⚠️ 11128 和 11140 的 msg **都是 `request illegal`**，只能靠 code 分流
> （`client.go:167` 与 `:177`）。注意代码里 11128 是按字面量 `"11-128"` 匹配的，
> 若上游改了 code 的字符串形态，这里会静默失灵 → 表现是「通道拒绝被误判成普通 4xx，只换号不报警」。

### B. 单个模型全线失败，其他模型正常

```
B1  code=11102「该后端无此模型」
    → 模型下线/改名。改 internal/upstream/model_catalog.go 或 model.json。
    → 网关会对 (账号, 模型) 做负缓存，改完需重启或等冷却过期。

B2  code=11158
    → 该账号类型无此模型权限（个人号 vs 企业号）。属于账号-模型匹配问题。

B3  code=6004（429）
    → 模型级用量限流，等重置。冷却时长会从 Retry-After 头读。

B4  模型列表整个空了
    → /v3/config 或 /console 的模型目录端点变了：
       client.go:1583 fetchV3Models / client.go:1858 GET {chatBase}/v3/config
       /v1/models 的动态缓存来自这里。
```

### C. 所有请求 400，code=11101「Unmarshal chat params failed」

这是**体重写管线**没跟上。逐个字段二分，不要瞎改：

```
C1  先确认是不是客户端本身发了畸形 JSON（换官方 SDK 发最小请求对照）
C2  最小请求能过、复杂请求不过 → 逐项加回：tools / tool_choice / image_url /
    max_completion_tokens / stream_options / reasoning_effort / thinking
C3  落点：internal/upstream/payload.go:30 PrepareBodyOptWithEffortsAndDefault
    - tool_choice 只接受 string      → normalizeToolChoice
    - image_url 只接受对象            → normalizeImageURL
    - max_completion_tokens 要翻译     → translateMaxCompletionTokens
    - GPT 系 max_tokens < 16 要抬到 16 → clampGPTMinMaxTokens
C4  报错信息里带 "tools" 相关字样 → 走 tool_pairing.go（合并/重排/删孤儿三步）
```

### D. 全部 403，body 是 HTML 或空

```
→ APISIX WAF 拦截（client.go:411 IsWafBlocked：403 且无业务信封）
→ 不是协议问题，是出口 IP / 频率问题。换出口、降并发，改代码无效。
```

### E. 流式断开、空帧、内容截断

```
E1  完全没有 delta 但连接 200 → sse.go:476 Stream 的帧规范化没认出新的帧形状
E2  内容中途停 → 检查 idle_timeout_seconds（默认 300s）与 finish_reason
E3  非流式返回空 / 502 upstream_parse → sse.go:30 Aggregate 聚合失败
E4  末帧没有 usage → 检查 body 是否仍带 stream_options.include_usage
```

### F. 工具调用不执行，正文里出现一段怪文本

```
→ dsml.go 的标记格式变了。当前识别的是两个全角竖线 U+FF5C 分隔的 DSML 块。
→ 抓一段原始输出（面板「请求记录」能看 raw），把新标记形态补进 markupPipe/解析器。
→ 兜底原则：解析不确定时必须原样透出，不能吞字节。
```

### G. 定时任务全部 0 分，但聊天正常

```
G1  账号是 global（国际版）→ 成长体系只对 CN 个人号开放
G2  账号是企业号 → 读端点可用、/v2/report 静默丢弃（见 auth.go:230 IsEnterpriseSession）
G3  端点变了 → internal/upstream/{report,streak,travel,blackcat,tasks}.go
    签到：POST /v2/billing/meter/daily-checkin
    上报：POST /v2/report
G4  这类失效**不影响转发主链路**，优先级最低，可以先关掉 schedule 里的开关止损。
```

---

## 3. 错误码字典

| code / 状态 | ErrKind | 网关动作 | 含义 |
|---|---|---|---|
| 11101 | `bad_params` | 不罚号、**不轮转**、400 透传 | 请求体上游解析失败 |
| 11102 | `model_blocked` | (账号,模型) 负缓存 | 该后端无此模型 |
| 11158 | `model_blocked` | (账号,模型) 负缓存 | 账号类型无权用该模型 |
| 11115 | `prompt_too_long` | 不罚号、透传 | 上下文超限 |
| 11128 | `channel_refused` | **不罚号** | 通道校验失败（反代识别） |
| 11133 | `client` | 视字段而定 | 参数非法（含 GPT 系 max_tokens<16） |
| 11135 | `image_invalid` | 不罚号、透传 | 图片格式/数据错误 |
| 11140 | `account_fault` | 冷却/禁用 | 账号被限制 |
| 11148 | `client` | 换号 | tool_calls 报文形状问题（先靠 tool_pairing 修） |
| 12153 | `session_dead` | 禁用 | session 失效，需重登 |
| 14017 | `account_fault` | 冷却 | 试用未激活 |
| 14018 | `hard_credit` | 长冷却 | 积分耗尽 |
| 6004 | `soft_rate` | 短冷却，按 Retry-After | 模型级限流 |
| 6021 / queue.full | `upstream_busy` | 不罚号、503 | 渠道队列打满（换号无用） |
| 402 | `hard_credit` | 长冷却 | 余额耗尽 |
| 403 无信封 | `waf_block` | 软冷却 + 抖动 | WAF 拦截 |
| 400 + 审核文案 | `content_blocked` | 不罚号、降级重试 | 内容策略误报 |

分类入口唯一：`internal/upstream/client.go:565 Classify()`。**新错误码一律加在这里，不要在 handler 里打补丁。**

---

## 4. 改动落点速查

| 症状 | 文件 | 函数 / 位置 |
|---|---|---|
| 新错误码没被识别 | `internal/upstream/client.go` | `Classify` :565 |
| 403 / WAF 判断 | 同上 | `IsWafBlocked` :411 |
| 通道校验判定 | 同上 | `isChannelRefused` :177 |
| 请求体被拒 | `internal/upstream/payload.go` | `PrepareBodyOpt...` :30 |
| 工具调用配对 400 | `internal/upstream/tool_pairing.go` | 三步修复 |
| 工具调用不执行 | `internal/upstream/dsml.go` | 标记解析 |
| 流式帧异常 | `internal/upstream/sse.go` | `Stream` :476 / `Aggregate` :30 |
| 模型列表空了 | `internal/upstream/model_catalog.go`、`model.json` | 目录抓取 |
| 登录失败 | `internal/panel/login.go` | `loginEndpoints` :36 |
| token 刷不动 | `internal/upstream/client.go` | `RefreshToken` :1113 |
| 定时任务端点 | `internal/upstream/{report,streak,travel,blackcat,tasks}.go` | — |
| 提示词被误杀 | `internal/prompt` + `config.prompt.mode` | — |
| 出站太快被风控 | `internal/pace` + `config.pacing` | `Limiter.Wait`（handler 轮转循环内） |

---

## 5. 指纹对齐：请求头这一层

### 5.1 不要先抓包，先读客户端自己的代码

抓包要装 CA、解 TLS、还得挑对时机；而客户端把「发什么头」直接写在 JS 里。
**直接静态提取，五分钟拿到完整答案**：

```bash
# 1) 找到客户端（Windows 默认装这里）
ls "/c/Program Files/WorkBuddy/resources/app.asar"

# 2) 解包（本项目自带工具，只导出 js/json，跳过二进制）
node tools/asar-extract.js "/c/Program Files/WorkBuddy/resources/app.asar" ../_wb_client

# 3) 定位请求头构造函数（一行 3.6MB，用 python 切片，别用 grep -o）
python tools/slice.py ../_wb_client/main/server.js 'buildVlmGatewayHeaders|applyGatewayHeaders' 800 2

# 4) 看客户端版本 —— 注意是 package.json，不是 version 文件
python -c "import json;print(json.load(open(r'C:/Program Files/WorkBuddy/resources/app/package.json',encoding='utf-8'))['version'])"
```

> **版本号取错是这里最常见的坑**：`C:\Program Files\WorkBuddy\version` 里的
> `37.10.3-42` 是 **Electron 版本**，不是产品版本。产品版本在
> `resources/app/package.json` 的 `version`（当前 `5.8.3`），两者差了一个数量级，
> 拿 Electron 版本去填 `client_version` 会直接触发 11128。
> 也可以直接问官方更新接口，最省事：
> `curl "https://www.codebuddy.cn/v2/update?platform=workbuddy-win32-x64-user"`

关键函数就三个，全在 `main/server.js`：

| 函数 | 作用 |
|---|---|
| `buildVlmGatewayHeaders` | **身份类头**（X-User-Id / X-Product / X-IDE-* / X-Private-Data / UA），真实版本号在这里 |
| `applyGatewayHeaders` | **会话类头 + 兜底表**（Accept / 会话四件套 / X-Agent-Intent / `YN` 默认值） |
| `applyTraceHeaders` | 链路头（traceparent / b3 / X-B3-*） |

调用顺序在 `VlmUpstream.send`：

```
入站头过滤(qN 黑名单) → buildVlmGatewayHeaders → applyGatewayHeaders(YN 兜底) → Accept-Encoding: identity → Authorization
```

**这个顺序决定了「谁是权威值」**：`buildVlmGatewayHeaders` 先写入真实版本，
`applyGatewayHeaders` 的 `YN` 表是 `e.set(n, e.get(n) || r)` —— **只在缺值时才兜底**。
所以 `X-IDE-Version` 的最终值是 `appVersion`，`YN` 里的 `"0.0.0"` 永远不会生效。
把 `0.0.0` 当成「官方值」抄进网关，是这里最容易犯的错。

> 上游地址硬编码在 `GN = https://copilot.tencent.com/v2/chat/completions`。

### 5.2 实测差异（2026-10-21，WorkBuddy 5.8.3 + CodeBuddy IDE 4.12.2 双客户端交叉验证）

**先回答一个容易搞混的问题：该模仿谁？**

| 客户端 | 版本 | 内核 | chat 头形态 |
|---|---|---|---|
| WorkBuddy 桌面端 | 5.8.3.41334564 | `app.asar` 自带 `genie` 内核 | `X-IDE-*` 写死 `WorkBuddy`，版本用 appVersion |
| CodeBuddy IDE | 4.12.2（VS Code 1.106.1） | 扩展 `Tencent-Cloud.coding-copilot` 3.10.0 | `X-IDE-*` 取 IDE 真实平台（`VSCode` / platformVersion） |

两者共用同一套协议常量（`X-Conversation-*` / `X-B3-*` / `X-Product` 语义完全相同），
**但身份头的取值来源不同**。本项目的账号是 WorkBuddy 账号、上游是 copilot VLM 网关，
所以**必须对齐桌面端，不能照抄 IDE**——IDE 的 `X-IDE-Type: VSCode` 在这里反而是错值。

与旧实现（v1.11.7）的差异：

| 头 | WorkBuddy 5.8.3 实际 | 旧实现 | 性质 |
|---|---|---|---|
| `X-Product` | `deploymentType`，缺省 `SaaS` | `WorkBuddy` | ❌ **值错**，最容易被识别 |
| `X-IDE-Version` | **`5.8.3`（appVersion）** | `5.5.4` | ⚠️ 只是版本落后 |
| `X-Private-Data` | `true` | 无 | ❌ 缺 |
| `X-Agent-Intent` / `X-Agent-Purpose` | `craft` / `conversation` | 无 | ❌ 缺 |
| `acp-connection-id` | UUID（每连接） | 无 | ❌ 缺 |
| `X-Tenant-Id` | 企业号必发 | 仅 billing 域发 | ❌ 缺 |
| `Accept-Encoding` | `identity` | Go 默认 gzip | ❌ 值错 |
| `Accept` | `application/json`（流式也一样） | `application/json, text/event-stream` | ❌ 值错 |
| `traceparent` / `b3` / `X-B3-ParentSpanId` | 全发 | 无 | ❌ 缺 |
| `Origin` / `Referer` | **显式 strip，不发** | 发 | ❌ 多发 |
| `Accept-Language` | 不发 | 发 zh-CN/en-US | ⚠️ 多发 |
| `X-Machine-ID` | **两个客户端 chat 链路都没有** | 按 uid 派生后发 | ❌ 头名猜错 |
| `X-Session-ID` | **chat 不发**（IDE 只在代码补全链路发） | 按 uid 派生后发 | ❌ 头名猜错 |
| `X-Custom-Channel-Id` | 只喂 app-server 的 auth-state，**chat 不发** | 不发 | — |
| `X-Device-Id-List` | 同上，**chat 不发** | 不发 | — |
| `X-Device-Token` | 只用于签到（`getCheckinRequestHeaders`）与 auth-state，**chat 不发** | chat 会注入（配置了 device_token 时） | ⚠️ 条件多发，默认已关闭 |
| `X-Requested-With` | `XMLHttpRequest`（`applyGatewayHeaders` 里是 `x-requested-with`） | 发 | ✅ |
| `X-Root-Request-ID` | 不发（IDE 有常量定义但 chat 不注入） | 发 | ⚠️ 多发 |
| `X-Conversation-Message-ID` / `X-Request-ID` | `randomUUID()` 带连字符、两次调用值不同 | 32 hex 无连字符 / 同值 | ⚠️ 形态 |
| `X-Trace-ID` / `b3` / `traceparent` | 每次请求 `randomBytes(16)`，与会话 ID 无关 | 复用 `X-Conversation-Request-ID` | ⚠️ 恒等即暴露 |
| `User-Agent` | `WorkBuddy/<appVersion> WorkBuddy/<appVersion> CLI/<platformVersion>` | 同形状，版本落后 | ⚠️ 版本 |

#### 为什么旧实现会「编」出 X-Machine-ID / X-Session-ID

这一条值得写清楚，因为它决定你以后遇到同类问题该怎么判断——**这不是随手瞎写，是猜错了头名**。

作者的真实意图写在 `deriveAccountStableID` 的注释里，原文：

> 本对头是「每账号一台固定虚拟设备」的稳定指纹，**防多号被上游按设备指纹缺失/漂移关联风控**。

这个动机是对的，问题也确实存在：本项目的登录走 `platform=CLI` 的插件 OAuth，**拿不到
`device_token`**（那是 Turing SDK 由原生模块产出的，见 `native/turing-sdk/`）。于是「冒充桌面端
chat，却没有桌面端的设备身份」成了一个必须补的洞——多个账号如果都没设备标识，上游按 IP+UA
就能把它们串成同一台机器。

官方确实有一组设备身份头，由 `WorkbuddyAuthStateHeaderProvider.provide()` 一次性产出
（`main/module.app-server.js`）：

```js
let e = { "X-Custom-Channel-Id": "unknown" };            // 恒发，值就是 unknown
if (deviceToken) e["X-Device-Token"] = deviceToken;       // Turing 真令牌
if (qimei36)     e["X-Device-Id-List"] = JSON.stringify({ qimei36 });  // 腾讯设备指纹
```

**但这组头不进 chat 链路。** 决定性证据是 chat 的注入点：

```js
// main/server.js @3526839 —— VlmUpstream 的 deps
getRequestHeaders: () => buildVlmGatewayHeaders({...})   // ← 不含任何设备头
```

`X-Device-Token` 在桌面端只出现在 `DesktopAccountRepo.getCheckinRequestHeaders()`（签到），
`X-Custom-Channel-Id` / `X-Device-Id-List` 只由 app-server 的 auth-state 接口消费。
**provider 名字里带 "AuthState"，它服务的是 agent/CLI 的运行时，不是 VLM chat 代理。**

**三个头里也没有 `X-Machine-ID`，没有 `X-Session-ID`。** 这两个名字之所以被选中，最可能的来源是
协议常量表——`X-Session-ID` 在 CLI 与 IDE 的常量表里都真实存在（`SESSION_ID_HEADER`），但全量代码
里只有定义、没有任何赋值点；`X-Machine-Id`（小写 d）只出现在 feedback 上传。**「名字搜得到」不等于
「chat 会发」**，这是这类逆向最容易踩的坑。

补一个来源事实：这两个头也不是本仓作者原创。`git log -S` 显示它们由提交 `860ec53`
（`feat(sync): 手工吸收上游 b5077d5..76bb543 全量改动`）引入，源头是上游 fork
`Sliverkiss/workbuddy2api`，两仓无共同 git 祖先。所以「原作者不可能犯这种错」这个直觉是对的
——错在更早的那层推演。

**为什么当时加了不但没报错，反而像是有效？** 因为宽松期服务端只校验「必要头是否存在」，
多一个不认识的头的确会被忽略；而少了设备标识是真会被风控串号。所以「编两个头补上」在当期是
有效的止血。等到服务端开始做**指纹一致性比对**——这个 UA 的客户端从不发 `X-Machine-ID`，
你却每个请求都发——同一批头就从无害变成证据。这类改动永远是这个规律：**宽松期是收益，收紧期是负债**。

结论：旧实现真正的错误不是「多发了几个头」这一件事，而是**用猜的头名替代了拿不到的真实凭据**。
正确的补法是拿到真 `device_token`（且只在官方会发的域名上用），而不是再造一个「稳定派生」的替代品。

> **一条经验（本轮踩过）**：看到「某某 provider 恒发 X」时，必须再确认**这个 provider 被哪条链路消费**。
> `WorkbuddyAuthStateHeaderProvider` 名字就写着 AuthState，却一度被误当成 chat 注入源，
> 于是把 `X-Custom-Channel-Id` 加进了 chat——那是把「少发」改成了「多发」，错误性质与造
> `X-Machine-ID` 完全相同。判断依据只有一个：**顺着注入钩子往回找**，看 chat 拿的是谁。

### 5.3 已落地的机制

- `upstream.profile`：`desktop`（缺省）= 上表中间列的真实形态；`legacy` = 旧形态，用于回滚对照。
- `upstream.client_version`：默认 `5.8.3`，**同时驱动 UA 与 `X-IDE-Version`**。
- `upstream.cli_version`：默认 `2.161.0`（5.8.3 内嵌 CLI `@tencent-ai/codebuddy-code` 的版本）。
  官方取的是服务端下发的 `platformVersion`，不是常量——实测到新值优先改配置，不要改代码。
- `upstream.chat_device_token`：默认 `false`。打开后 chat 会带 `X-Device-Token`（官方 chat 不发），
  仅用于验证「上游是否按设备绑定账号」，平时别开。
- `upstream.product`：默认 `SaaS`（= deploymentType）。
- 回归由 `internal/upstream/headers_desktop_test.go` 锁定：哪个头多发/少发，测试直接红。
- **ID 形态规范化**：`X-Conversation-ID` / `X-Conversation-Request-ID` 出站前统一走 `uuidDashed()`
  （32 裸 hex → 36 字符 UUID v4 形态），内部聚合键不动；`X-Trace-ID` 在 desktop 形态下每请求新随机，
  不再透传入站值。详见 §5.6。
- 环境变量覆盖：`WB2A_UPSTREAM_PROFILE` / `WB2A_CLIENT_VERSION` / `WB2A_CLI_VERSION` / `WB2A_PRODUCT`。
- **改完配置先自查**（不连上游、不用账号）：

```bash
./deploy.sh headers                       # 看当前 desktop 形态
./deploy.sh headers -profile legacy       # 看回滚形态
./deploy.sh headers -client-version 5.8.4 # 试新版本号
./deploy.sh headers -enterprise ent-123   # 模拟企业号（会多 X-Enterprise-Id / X-Tenant-Id）
```

正常输出应该是 **27 个头**（非企业号），与官方 `buildVlmGatewayHeaders + applyGatewayHeaders
+ applyTraceHeaders + identity + Authorization` 一一对应。多一个或少一个都要查 §5.2 的表。

- **自动对账**（比人眼可靠，客户端升级后必跑）：

```bash
python tools/header-parity.py ../_wb_client/main/server.js
# 直接读官方 JS 里的 buildVlmGatewayHeaders / applyGatewayHeaders / applyTraceHeaders / YN，
# 与 `go run ./cmd/headers-dump` 的实际输出比对；退出码 1 = 有少发/多发。
# X-Enterprise-Id / X-Tenant-Id / X-Domain 视为条件头，非企业形态缺失不算错。
```

脚本按**头名集合**比对（大小写不敏感）。**取值对不对它管不了**，取值要人看 §5.2 的表；
**字节形态（大小写/顺序）它也管不了**，见 §5.6。

### 5.4 客户端升级后怎么复查

**一条命令（推荐）**——版本没变就毫秒级跳过，变了才做完整对比：

```bash
./deploy.sh watch          # 每日入口：读 app.asar 里的 version，没变直接退出
./deploy.sh shape          # 有变化时看完整 diff（退出码 1 = 有漂移）
./deploy.sh shape --ours   # 顺带核对本网关实际出站的头
./deploy.sh shape --selftest   # 先确认检测器本身还能抓出漂移（9 项）
```

`shape` 直接按偏移读 app.asar 里的 `main/server.js`，不解 96 MB 整包，0.8 秒出结果。
它对比的是**形态**（头名 + 值形态 + 条件 + 端点），不是头名集合——上一轮那三个
「36 字符 vs 32 字符」的 bug 就是头名集合一致、形态不一致。

**手工路径**（脚本挂了、或需要看原文时）：

```bash
# 1) 当前版本（两个来源互相印证）
curl -s "https://www.codebuddy.cn/v2/update?platform=workbuddy-win32-x64-user"
python -c "import json;print(json.load(open(r'C:/Program Files/WorkBuddy/resources/app/package.json',encoding='utf-8'))['version'])"

# 2) 重新解包（客户端升级后 asar 一定换了）
node tools/asar-extract.js "/c/Program Files/WorkBuddy/resources/app.asar" ../_wb_client

# 3) 重新提取三个函数
python tools/slice.py ../_wb_client/main/server.js 'buildVlmGatewayHeaders|applyGatewayHeaders|applyTraceHeaders' 900 2
```

**改配置能解决 80% 的失效**：版本号落后是最常见的 11128 触发原因，而版本号是配置项。
只有当**头的集合或语义**变了（新增头、删除头、某个头从写死变成动态）才需要动 `headers.go`。

要看 IDE 形态做对照时，装官方 IDE 即可（`www.codebuddy.cn/ide/`），
请求头在 `resources/app/extensions/genie/out/extension/index.js`，
搜 `buildDeviceTokenHeaders` / `X-IDE-Version` 两处就能定位。

### 5.5 device_token

只能从真实桌面端状态文件里取（Turing SDK 产物），**不要伪造**。
伪造值比不带更容易被判定：长度和字符集都对不上，等于自己举牌。

### 5.6 头对了，不等于字节一样（2026-10-21 复核结论）

做完 §5.3 之后可以问一句「是不是和官方一样了」。答案要分层看，**别把「集合一致」当成「字节一致」**：

| 维度 | 状态 | 怎么验 |
|---|---|---|
| 头的**集合** | ✅ 27 / 28（带 domain）/ 29（企业号）与官方一一对应 | `python tools/header-parity.py ../_wb_client/main/server.js` |
| 头的**语义与取值** | ✅ 版本号、`X-Product`、UID/企业/域、`Accept`/`Accept-Encoding` 全部对齐 | `./deploy.sh headers` |
| ID 的**形态** | ✅ 已修（下表） | `go test ./internal/upstream -run Desktop` |
| 头名的**大小写 / 顺序** | ❌ 不一致（见下） | 起一个回环监听抓原文，或看线上 `curl -v` |
| TLS **指纹** | ❌ 不一致（§6.4 第 2 条） | JA3 在线比对 |

**本轮修掉的两处（都是「值形态」而非「头集合」，所以 §5.2 的对照表看不出来）**

| 头 | 官方 | 修前 | 为什么能识别 |
|---|---|---|---|
| `X-Conversation-ID` | ACP sessionId = `randomUUID()`，36 字符带横线 | 网关会话键是 32 位裸 hex，原样透传 | 同一字段官方永远 36 字符，我们永远 32 字符 |
| `X-Conversation-Request-ID` | `randomUUID()`（`VlmSessionLedger.acquire` 一次生成、按 session 缓存） | `TurnRequestID()` 的 32 位裸 hex | 同上，一条正则即可分流 |
| `X-Trace-ID` | 每请求 `randomBytes(16)`，**不看入站值** | 入站值合法就透传 | 客户端若发常量，随机值就变成固定值 |

注意第三行的边界：内部聚合键（`TurnRequestID` / `RequestIDForKey`）**仍然是 32 hex**，只在出站时
规范成 UUID 形态，日志与面板的分组口径不变。`legacy` profile 保持原样透传，用于回滚对照。

**仍然不一致的一处：头名字大小写与发送顺序（这是真正「字节级」的差异）**

官方走 undici（Electron 主进程的全局 `fetch`），HTTP/1.1 写出时**头名全部小写、顺序 = 插入顺序**；
Go 的 `net/http` 写的是 **Title-Case 且按键名字典序**，还会把 `X-B3-TraceId` 规范成 `X-B3-Traceid`、
`b3` 规范成 `B3`。抓包对比一眼可见：

```
# 官方（undici）                              # 本网关（Go net/http）
x-user-id: u_xxx                              X-User-Id: u_xxx
x-product: SaaS                               X-Product: SaaS
x-ide-type: WorkBuddy                         X-Ide-Type: WorkBuddy
...（按代码里 set 的顺序）                      ...（按 ASCII 升序）
```

HTTP 头名大小写不敏感，所以**协议层完全合法**；但它是一个稳定的客户端指纹。要改只能自写 HTTP/1.1
写出器（自己 dial + TLS + 手写请求行/头，再 `http.ReadResponse` 读响应），代价是接管连接复用、
超时与 SSE 流式读取——目前判断**收益不抵风险**，先记在这里，等真的因为这条被拦了再做。

> 一个顺带纠正：这个客户端的 TLS 是 **Node 的 OpenSSL**，不是 Chrome/Chromium。之前文档里
> 写的「与 Chromium 差得远」是错的，`utls` 的 `HelloChrome_*` 预设也对不上 Node。
> 这类结论一定要顺着 `fetch` 的来源往上找（`serverFetch → bootstrapWorkbuddyAppServer → 全局 fetch`），
> 不能看到 Electron 就假定 Chromium 网络栈。

### 5.7 灰度

改完先在**单个号**上验（其余号设 `paused`），确认不再是 11128 再放量。
11128 是「不罚号」的，所以拿单号试错成本很低。

> 如果上游开始校验请求体的签名/时间戳，这条路基本走到头了。届时唯一可持续的方向是自己实现官方 CLI 协议
> （如果官方有开放 CLI），而不是继续对抗。

### 5.8 形态快照：解包为主、抓包为辅（`tools/upstream-shape.py`）

**分工先讲清**，这也是原作者留下的方法（`git show 19dd3a9` 的注释）：
解包给**全集**（能看到所有条件分支、默认值、事件码枚举），抓包给**实例**
（真实取值、时序、必填字段）。只有解包会写出「字段齐全但值瞎填」的请求，
只有抓包会漏掉没走到的分支。原作者两份方法论文档（`docs/api-reference.md`、
`data/desktop-task-protocol.md`）被 `.gitignore` 排除、从未入库，现已丢失——本工具是它的替代品。

两条路写进**同一个 shape 结构**，因此 diff 只有一份：

```jsonc
{
  "source": "static",                    // 或 "capture"
  "client_version": "5.8.3",
  "endpoints": ["/v2/chat/completions", …],   // 客户端代码里出现的全部路径
  "headers": {
    "X-Trace-ID":  {"from": "applyTraceHeaders", "when": "always",
                    "value": {"kind": "hex", "detail": 32}},
    "X-Tenant-Id": {"from": "buildVlmGatewayHeaders", "when": "if enterpriseId",
                    "value": {"kind": "param"}},
    "X-IDE-Version": {"from": "buildVlmGatewayHeaders", "when": "always",
                      "value": {"kind": "input", "default": "0.0.0"}}
  },
  "structure": {"buildVlmGatewayHeaders": true, …}   // 锚点探针
}
```

**值形态（`kind`）才是关键**：`uuid` / `hex`(带位数) / `const`(带值) / `input`(带默认) /
`template` / `param`。上一轮的三个 bug 全部属于「头名一致、`kind` 不一致」。

三个实现细节值得记住，都是踩过的坑：

1. **必须解析局部变量别名**。`X-IDE-Version: r` 里的 `r` 是
   `e.appVersion?.trim()||\`0.0.0\``。不解析就会把 `0.0.0` 当成真实值——
   上一轮「X-IDE-Version 就是 0.0.0」的判断错误正是漏了这一层。
   `X-Trace-ID: n` 同理，`n` 是 `randomBytes(16).toString('hex')`。
2. **条件组要按括号回溯**。企业号的 `X-Enterprise-Id` 与 `X-Tenant-Id` 共用一个
   `e.enterpriseId?.trim()&&( … )`，只看紧跟的逗号会把第二个记成「必发」。
3. **fetch 层还有两个头**。`VlmUpstream.send` 在 `applyGatewayHeaders` 之后补
   `Accept-Encoding: identity` 与 `Authorization: Bearer <token>`，漏了会数错
   （24 + 2 + X-User-Id = 27，加 domain 28、加企业 29）。

**抓包怎么进来**：不用常驻 MITM。chat 链路走 Electron 主进程全局 fetch（undici），
**不读系统代理、主进程也不同步 env**，常规 mitmproxy 方案对它无效，且抓到后出去那程的
TLS 会变成 mitmproxy 的指纹——正好是我们想去对齐的东西。所以抓包只在「解包给不出实例」
时手动做一次（Sunny / Charles 导出 HAR），然后：

```bash
./deploy.sh shape --har capture.har --save     # 导入并建基线
./deploy.sh shape --har next.har               # 与上一份抓包对比（含 body 键集）
```

**每日检查怎么做**：`./deploy.sh watch`。它只做一件事——读 asar 里的 version 与最新快照比，
相同就一行退出。**不要按日期对比抓包数据**：UUID/trace/时间戳每次都变，按值 diff 是纯噪声；
同版本的静态提取结果则是逐字节相同的，所以「版本变了才比」既省事又没有误报。

**先自检再信任**：`./deploy.sh shape --selftest` 会制造 9 类漂移（删头/加头/值形态变/值长度变/
常量变/条件变/端点新增/出站值长度不符/出站漏发），逐个验证能被抓到。一个抓不出漂移的检测器
比没有更糟——它会给你「一致」的假结论。

**能力边界**：这一层看得见头、端点、值形态；看不见请求体字段（静态提取在压缩后的 JS 里
不可靠，交给抓包）、看不见 TLS 指纹、看不见运行时走了哪条条件分支。

**每日自动检查**（2026-10-21 配好，自动化 id `3ce3db20`）：每天 09:00 在 `D:/demo/workspace/_wb2api`
跑 `bash ./deploy.sh watch`，模型 `deepseek-v4.1-flash`。

输出刻意压到两个问题、五行以内，**不写任何文件**：

1. **客户端有没有新版本** —— 没变就一句「客户端 5.8.3，无更新」。
2. **需不需要同步改 wb2api** —— 只允许三种结论：不用动 / 只改
   `config.json` 的 `upstream.client_version` / 要改 `headers.go`（哪个头的什么形态变了）。

不生成报告、不建 docs 文件：日报的价值在于「要不要动手」，写长篇报告反而让人不想看。
真要细节时跑 `./deploy.sh shape` 看完整 diff。漂移超过 3 处时只列关键 3 处并写明总数。

三条刻意设的约束：**不自动 `--save` 覆盖基线**（否则第一次误报就会把真基线冲掉、以后再也比不出来）、
**不自动改 `headers.go` / `config.json`**、**不跑无参数的 `./deploy.sh`**（那会重建容器，有副作用）。

**`./deploy.sh stats` 不在每日任务里**（原设计每天跑，实际用起来太重）：它是手动工具，
想看昨日失败率和 403/429 环比时自己跑。判据写死在文件头：失败率环比 **+2pp** 才报、
403/429 **翻倍且绝对值 ≥3** 才报；无前日基线时不编造百分比。

**数据源**：`data/request-logs/requests-YYYY-MM-DD.jsonl`，由 `internal/reqlog` 写，
字段见 `reqlog.Event`（`status` / `ok` / `outcome` / `attempts` / `duration_ms`，已脱敏不含正文与凭证）。
注意 Event 里**没有上游业务错误码**（如 11128）——那个是响应 body 里的 JSON 字段，没进归档，
所以这一层能看到 403/429/5xx 与 `http_error` / `stream_error` 分布，看不到「11128 占多少」。

### 5.9 上游新增成长任务了怎么加

三道工序：**定位码 → 定判据 → 落地验证**。前两步的顺序不能反。

**① 定位码：`./deploy.sh tasks`**（需要先 `./login.sh` 登录，读 `./auths`）

工具拉 `/v2/activity/growth/tasks` 与 mp 口径，与面板 `accountTasks` 同源，然后跟
`internal/panel/autotask.go` 的 `autoActions` 做差集，把 ❌ 的那几个列出来。

为什么必须先跑工具：**任务码由服务端下发，代码里搜不到**。实测在解包的 1351 个 js 文件里搜
`Sequential_Tasks` / `growth/tasks` 是 **0 命中**——客户端只负责渲染，任务清单从来不在包里。
上游仓库（`linguo2625469/workbuddy2api-panel`）停在停止维护声明，也不会有新任务的提交。

**② 定判据：要发什么事件才算完成**

| 判据能否静态看出 | 走哪条路 |
|---|---|
| 能（事件名/形状在包里或与既有任务同形） | 参考相邻任务直接写，像 `Buddy_App` 那样「已验证：纯 API 可点亮」 |
| 不能（依赖真实交互、真实对话、点了某个 UI） | 抓一次真实样本（Sunny），照 5.8 的边界：**解包给全集，抓包给实例** |

判据没验证过的，`Desc` 里必须写「尝试型」并把 `Attempt: true`，别和已验证的混在一起——
一条假「已验证」比没做更糟，后面所有排查都会从它开始。

**③ 落地**

- `autoActions` 加一条：`TaskCode` + `Desc` + `run`
- run 函数优先复用：`runSequentialEventTask`（主/备两条路径）、`runMPMiniChatTask`（mp 口径）
- mp 专属码要登记进 `mpTaskCodes`，否则 accept/claim 走错口径
- **表的顺序就是执行顺序**（`autoActionIndex` 按它排队），前置任务放前面

**④ 验证**

1. `./deploy.sh tasks` 复查 ❌ 是否消失
2. 面板点「自动完成」，看回读进度——**上报 200 ≠ 计分**，代码用 `taskByCodeWaiting`
   轮询等异步计时落定后才决定领奖，这一步不要跳过
3. 判据仍不确定的，把 `Attempt` 留着，等实测点亮再改注释

**还有一种正确的不实现**：需要客户端内交互才产生的任务（例如必须点某个 UI），
`accountTaskAuto` 返回 501「该任务需要客户端内交互」是**预期行为**，不是 bug。
这类任务就算拿到码也做不了，别硬凑事件——硬凑出来的上报会被上游静默丢弃，白跑还浪费配额。

---

## 6. 存活策略：你没有在「防检测」，你在降低被选中的概率

上一节解决的是**单次请求长得像不像官方客户端**。但那只回答了问题的一半。

### 6.1 先纠正目标

「把这套反代做得和官方一模一样」不是可达目标，因为这是**不对称对抗**：

- 对方加一个检测维度，成本接近零（改规则就行）；
- 你要跟上，成本是几十小时逆向 + 每次客户端升级重做一遍。

所以正确的目标是：**降低被选中概率 + 假设随时会失效**。前者靠运营，后者靠架构（`legacy` profile、跨域回退、能快速换账号）。任何把这件事当布尔问题（「过了就是过了」）的判断，最后都会在某个早上发现全线 11128 而毫无准备。

### 6.2 上游实际能用哪些信号判定「这不是官方客户端」

按「你能不能改」排列——越往下越接近无解：

| 层 | 信号 | 现状 | 备注 |
|---|---|---|---|
| 请求头 | UA / `X-IDE-*` / `X-Conversation-*` / `b3` / `traceparent` | ✅ 已逐项对齐官方 5.8.3 | 见 §5；客户端升级必须复查 |
| 请求体 | 指纹字段、system 提示词 | ✅ `sanitize` + `prompt` 双层 | 黑名单可关 |
| 连接层 | 是否 `h2` | ✅ 已禁（`TLSNextProto` 置空） | 顺带规避了 h2 帧指纹，though 这是为半死连接修的 |
| **节奏** | **QPS、请求间隔分布、24h 持续性** | ✅ **`pacing`（本节 6.3）** | 最容易忽略、也不需要逆向 |
| 并发 | 单号同时在途数 | ✅ `max_in_flight`（默认 3，global 域 2） | 租约，管数量不管速率 |
| 出口 IP | ASN / 地理位置 / IP 跳变 | ⚠️ 靠部署 | 机房 IP + 高频是最显眼的组合 |
| **TLS 指纹** | **JA3 / JA4（cipher 顺序、extension 集合）** | ❌ **未做，硬伤** | 目标是 **Node/OpenSSL**（官方这条链路走 Electron 主进程的全局 `fetch` = undici），不是 Chrome——见下方更正 |
| 请求头**字节形态** | 头名大小写、发送顺序 | ❌ 未对齐 | Go 按字典序写 Title-Case，undici 写小写 + 插入顺序；§5.7 |
| **设备令牌** | `X-Device-Token`（Turing Shield / qimei36） | ⚠️ 留了文件挂载口，默认关 | **伪造不了**，签名绑设备，只能借真机的 |
| 账号画像 | 注册渠道、登录设备一致性、积分获取模式 | ⚠️ 靠养号 | 单请求无关，池层面问题 |

两条判定路径是**独立**的，这一点有实案：`internal/server/wafip.go` 的注释记录了 3 个请求头完全合法的账号在 1 秒内全部 403——WAF 拦的是**出口 IP**，不是账号也不是 header。所以「头都对了」不等于「不会死」。

### 6.3 `pacing`：租约管并发，它管速率

`pool.max_in_flight = 3` 听起来很保守，但 3 个在途名额配 200ms 返回的短请求，一秒能打出去十几次——而任何真实客户端都不会有这种分布。风控模型看的是**节奏特征**，不是单次请求。

配置（面板 → 账号与调度 → 出站节奏，热生效）：

```json
"pacing": { "enabled": true, "min_interval_ms": 120, "jitter_ms": 180, "per_minute": 90 }
```

| 参数 | 作用 | 调它的信号 |
|---|---|---|
| `min_interval_ms` | 同账号两次出站的最小间隔；并发请求按到达顺序排队，第 N 个等 (N-1)×间隔 | 日志里同号出站时间戳密集 → 调大 |
| `jitter_ms` | 在间隔上叠加 `[0,N)` 随机量 | **等间距本身就是指纹**（机器时钟），这一项别设 0 |
| `per_minute` | 滑动 60s 窗口硬上限 | 批量跑任务 / 多客户端共享账号 → 按需调低 |

默认档（120+180ms、90 次/分）的代价是平均每请求多等约 210ms，单人对话无体感（正常 10-20 次/分）。

- **想完全回到改动前**：`"enabled": false`（或三个参数全 0）。
- **触发 11128 之后的第一件事**：先把 `per_minute` 砍到 20-30 观察 24 小时，别急着改 header。多数「疑似通道校验」其实是节奏触发。
- 关掉它不会让 `max_in_flight` 失效——两层独立。

实现见 `internal/pace`（纯逻辑，假时钟可测）；接线点在 `handler.go` 的轮转循环里、`ChatStreamContext` 之前；`attemptStarted` 刻意记在 `Wait` 之后，所以 **reqlog 的 TTFB 不包含这段排队**，客户端感知延迟会比日志多出这一截。

### 6.4 还没做的三件事（按性价比）

1. **出口 IP 稳定**——不要一台机器一会儿国内一会儿国外；IP 跳变比高频更容易触发风控。这是部署问题，不是代码问题。
2. **uTLS 伪装 ClientHello**（`refraction-networking/utls`）——TLS 层唯一的解。⚠️ 目标不是 Chrome：
   官方这条链路走 Electron 主进程的全局 `fetch`（undici），TLS 是 **Node 的 OpenSSL**，JA3 属于
   Node 类而非浏览器类，`utls` 现成的 `HelloChrome_*` 预设**对不上**，得自己按 Node 的 cipher 顺序
   与 extension 集合手搓 `ClientHelloSpec`。
3. **真机 `X-Device-Token`**——`upstream.chat_device_token` + `WB2A_DEVICE_TOKEN_FILE` 已留好接口，需要一台真机持续供 token。

---

## 7. 改完必须做的验证

```bash
go vet ./...
go test ./...                                  # 全量，约 60s
go test ./internal/upstream/ -run TestClassify -v   # 只验错误分类
go test ./internal/server/ -run TestChat -v         # 只验转发主链路
```

灰度顺序：**单号 → 单模型 → 单客户端 → 全量**。每一步都看面板「请求记录」里的 kind 分布，
出现新的 `client` 类占比上升就是改坏了。

---

## 8. 版本与回滚

```bash
# 只留一个远端（自己的 fork），把作者仓库加成只读快照
git remote rename origin upstream
git remote add origin git@github.com:<你>/workbuddy2api-panel.git
git tag baseline-73a7560            # 基线打 tag，任何改动都能一键对比
git diff baseline-73a7560 -- internal/upstream/   # 只关心上游适配层的改动
```

**纪律：业务改动和上游适配改动分两个分支。** 上游适配分支永远保持「小、可回滚、每处改动都有对应的
错误码或抓包证据」，否则下次上游再变时你没法判断哪次改动引入了问题。

---

## 9. 已知脆弱点（按失效概率排序）

1. **通道指纹**（11128）——已在收紧，最可能先死，且无法根治。
2. **登录/刷新端点**——`/v2/plugin/auth/*` 一旦改路径，全池无法新增/续期。
3. **模型目录**（`/v3/config`）——模型改名/下线会导致点名 11102。
4. **成长任务端点**——纯逆向产物，随活动周期变动，但不影响转发。
5. **DSML 工具标记**——上游换分隔符或换语法，agent 类客户端会静默失效（不报错，只是不干活）。
6. **请求体字段语义**——上游悄悄改默认值（如 max_tokens 默认上限），表现为「输出被截断」而不是报错。

---

## 附：出问题时先看的三个地方

- 面板 → 请求记录：按 kind 排序，一眼看出是账号问题还是协议问题
- `data/state.json`：每个号的积分、冷却截止时间、错误计数
- `docker logs workbuddy2api --tail 100`：启动期的配置告警、auth 目录加载失败
