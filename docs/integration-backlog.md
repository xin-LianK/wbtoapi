# 功能集成清单：对标 workbuddy2api-hub v2.1.1

对标对象：<https://github.com/ardeyouxipianyi/workbuddy2api-hub>，快照 commit `f5f15d3`（release v2.1.1），  
核对日期 2026-11-11。该仓库是 Python 单体实现（`wb_proxy.py` 28k 行 + 40 个 `wb_*.py`，共 6.6 万行，MIT），  
与本仓库同源分叉：多个模块的注释直接写明「移植自 panel `xxx.go`」。

本文只统计**本仓库没有、且值得判断是否吸收**的能力，不包含两边已等价的功能（会话头族、系统提示词  
三模式、sanitize、封号保护、签到/成长任务、代理绑定、用量统计、请求日志等）。

## 一、总览

统计口径：按能力单元计数，共 **18 项**（14 项功能缺口 + 1 项待决策 + 3 项明确不做）。
下表为 2026-11-11 二次代码核实后的裁决，与初稿相比有 4 项改判，改动都写在对应条目的「核实」小段里。

| 优先级    | 数量 | 含义                            |
| ------ | -- | ----------------------------- |
| **P0** | 2  | 直接影响可用性，建议先做                  |
| **P1** | 3  | 风控与运营价值明确，需要设计取舍              |
| **P2** | 8  | 锦上添花，或依赖部署形态/产品决策，可延后         |
| 非缺口    | 1  | 本地已有等价能力，只是入口不同（#8）           |
| 待决策    | 1  | CodeArts，体积等于半个项目             |
| 明确不做   | 3  | 与本地技术栈或部署形态不匹配                |

代码量一列是远端实现的规模，仅作复杂度参照，不等于本地工作量（Go 侧还要加测试与面板接口）。

## 二、P0：可用性缺口

### 1. Codex 上下文压缩（`/v1/responses/compact`）

- **远端出处**：`wb_proxy.py` `COMPACTION_PROMPT:19189`、`_has_compaction_trigger:19204`、  
  `_encode_compaction_summary:19217`、`_decode_compaction_summary:19224`、`responses_to_chat(compaction=):19540`、  
  `_responses_compaction_response:26326`；路由 `26941`、`27043`。
- **本地现状**：`internal/server/responses.go` 只做 Responses→chat 的常规转换，全仓 `grep` 无  
  `compaction` / `encrypted_content`。Codex 长会话触发压缩时没有端点，会话无法继续。
- **规模**：约 600 行（分散在转换层与路由层）。
- **要点**：压缩回合里客户端会带 `{"type":"compaction_trigger"}` 项，期望恰好返回一条 `compaction` 项，  
  其 `encrypted_content` 是**自家编码的摘要**；下一轮客户端把这条项原样回放，网关必须解出摘要文本当作  
  上下文消息。`/v1/responses/compact` 是更老的形态，摘要提示词由客户端以 user 消息给出，  
  不能再自己加一份。
- **验收**：构造「正常轮 → compaction_trigger → 回放 compaction 项 → 继续提问」四步链路，  
  断言第二步只返回一条 compaction、第四步上下文没丢。

### 2. `cache_retention=long`（按 CLI 版本分支）

- **远端出处**：`wb_proxy.py:17440`、`18196`，常量在 `wb_identity.CLI_CACHE_RETENTION`。
- **本地现状**：`internal/upstream/official_body.go` 只补 `reasoning_summary:"auto"`，并明确注释  
  「不补 `cache_retention`」，依据是内置 CLI 2.165.0 不发该字段、npm 2.169.0 才对非自定义模型补 `"long"`。
- **差异原因**：两边对标的 CLI 版本不同（本地 2.165.0，远端 2.168.2）。
- **风险**：1 小时缓存在部分模型上**写入更贵**，本地注释里已有实测结论（只覆盖过 deepseek）。  
  所以不能无条件打开，需要按「模型 × 是否自定义模型」放行，并保留配置开关。
- **验收**：同一提示词两轮请求，对比 `usage` 里 cache 相关字段与 credit 消耗。

## 三、P1：风控与运营

### 4. 三套出站身份可切换（桌面 / VSCode / CLI）

- **远端出处**：`wb_identity.py`（696 行），`build_identity_headers` + 三份产品常量。
- **本地现状**：只有一套桌面端身份（`internal/upstream/headers.go`），带 SDK 的 8 个 `X-Stainless-*`，  
  另有 `client_name="SaaS"` 的旧行为回滚开关，**没有身份切换维度**。
- **远端做法摘要**：
  - VSCode：`X-IDE-Type/Name=VSCode`，UA `VSCode/<宿主版本> CodeBuddy/<扩展版本>`，  
    国内版端点 `www.workbuddy.cn`。
  - CLI：`X-IDE-Type/Name=CLI`，UA `CLI/<ver> CodeBuddy/<ver>`，**没有** `Origin` / `Referer` /  
    `X-CodeBuddy-Request` / `Accept-Language`，`Accept` 只 `application/json`，不发 `X-Device-Token`，  
    请求体 gzip + 紧凑 JSON + `cache_retention=long`，不带 `metadata` / `conversation_id` /  
    `previous_response_id`，`X-Root-Request-ID` 等于本轮 `X-Conversation-Request-ID`。
- **远端怎么决定用哪个**（这是本节的关键，不是随机选）：
  1. **身份是按账号存的字段**，不是全局开关：账号档案里带 `product`，缺省即 `workbuddy`（桌面端），
     加载时经 `normalize_product` 收敛（`wb_accounts.py:1011`）。
  2. **手动切**：面板调 `POST /accounts/product`（`_route_accounts_product`），改完立刻落盘；
     支持按 uid 或按 realm 批量。
  3. **自动切**：只在**收到 429** 时触发。`_try_switch_product` 按
     桌面 → VSCode → CLI 的环形顺序找下一个"面板允许"的身份，同一请求最多切
     `MAX_PRODUCT_SWITCHES = 4` 次，60 秒内的切换算同一轮（防来回弹跳）；切完仍 429 就如实报错。
  4. **动机**是配额：远端注释写「官方有两套身分、两条配额线，某条满了可以切到另一条继续用」。
     所以它不是"伪装三选一"，而是拿第二个配额池。
- **风险**：这是风控对抗面，身份越多错配面越大；建议先只做「CLI 身份 + 按账号/按模型灰度」，
  保留整套头集合的抓包夹具再上。
- **验收**：每种身份各一条 raw socket 头顺序测试（本地已有 `headers_test.go` 模式可复用）。

### 5. 号池作用域分流（文本池 / 媒体池）

- **远端出处**：`wb_pool_scope.py`（232 行）+ 入口处判断。
- **本地现状**：单一账号池 + 分组 + 优先级，没有「这个实例只服务文本」的概念。
- **价值**：媒体号单独一池后，文本入口被打满不会把生图能力拖死；反之媒体入口的 `/v1/models`
  必须拒绝，否则客户端会误判实例也能对话。
- **规模**：小（配置 + 两个入口判断 + 面板开关）。

### 6. `web_search` / `web_fetch` 本地代跑

- **远端出处**：`wb_webtools.py`（1099 行），多引擎轮换 duckduckgo → bing → wikipedia →  
  stackoverflow → google news，含失败分类、冷却、同名工具去重。
- **本地现状**：走**官方** `/agenttool/v1/search`（`internal/upstream/search.go`），并暴露为  
  `/v1/search/messages` 与 `/mcp` 的 `web_search` 工具。
- **判断**：本地这条路更「官方」，不应被替换。真正缺的场景是**客户端自己宣告 Responses 服务端  
  `web_search` 工具**时——本地 `responses.go:293` 是直接跳过该工具定义，模型拿不到搜索能力。  
  远端是自己代跑，本地更合适的做法是先尝试桥到官方搜索，再考虑兜底引擎。
- **优先级说明**：价值明确但只在特定客户端（Codex App）暴露，故列 P1 末位。

## 四、P2：锦上添花

编号沿用初稿（本节从 #3 开始、编号不连续），便于和上面的裁决表对照。

### 3. 媒体出口：生图 / 生视频（初稿列 P0，核实后下调）

- **核实结论：模型确实存在，是本地主动过滤掉的。** `internal/upstream/client.go` 的 `nonChatModel`
  注释写明（2026-10-02 桌面端目录实测）：除 `text-to-image` 外还有 `text-to-video` /
  `image-to-video`（**seedance 系列**）与 `image-to-image`（**gpt-image 系列**），并注明
  「生成模型走各自专用端点，作为对话模型选上去只会报 11102」。远端给出的国内区例子是
  `hunyuan-image-alpha`、`hunyuan-image-alpha-edit`、`kling-video-v3`、`minimax-video-h3`。
- **为什么远端要加**：这些条目就混在同一个 `/v3/config` 的 `data.models` 里，只是 tag 不同；
  官方把真实调用放在 `/v2/images/*`、`/v2/videos/*`，于是它把「目录里看得见、chat 端点调不动」
  的这一类单独做了协议层 + 媒体号池。它仍然只服务 WorkBuddy（外加 CodeArts）一个产品线，
  之所以看起来像"不只 WorkBuddy"，是因为上游聚合了第三方模型（字节 seedance、OpenAI gpt-image、
  快手 kling）。
- **为什么下调**：目标用户是 Codex / Claude Code 这类编码客户端，用不到图/视频；可用性还取决于
  **账号所在区与档位的目录**——目录里没有带这四个 tag 的模型时，远端自己也只能报
  "该区账号的产品配置里没有任何带 xxx 标签的模型"。而且它是新品类（异步任务、产物下载、
  并发排队），不是补一个字段。
- **触发条件**：只有明确要「一个入口同时提供文本 + 图/视频」时才做，否则不做。
- **若做，协议可直接照搬**：`POST {chat_base}/v2/images/generations`、`/v2/images/edits`
  （body 追加 `image:[dataURL,...]`）、`/v2/videos/generations`
  （`prompt/model/seconds/negative_prompt/watermark:false/image_url?/extra_parameters{...}`）、
  `/v2/videos/tasks`（body 只有 `{task_id}`）。信封一律 `{code,msg,request_id,data}`，
  **HTTP 200 也可能 `code != 0`**。媒体端点专属差异：`Accept` 用列表形态、补
  `X-Requested-With`、补企业标识，且刻意**不带** `Origin` / `Referer`。

### 7. 上游静态 API Key 当账号（初稿列 P1，核实后下调：属企业线）

- **远端出处**：`wb_accounts.Account.headers` 的 `is_api_key` 分支：`X-API-Key` 与
  `Authorization: Bearer` **两个鉴权头都发**；入口是 `POST /accounts/add-api-key`。
- **核实**：这是 CodeBuddy 平台签发的 `ck_` / `pt_` 前缀 Key，只在 `realm=cn`、
  `platform=CLI`、`tasksEnabled=False`（不做签到）下使用，并且**用企业额度端点
  （`/v2/enterprises/.../usage`）探测有效性**才允许落盘。远端注释写明了动机：
  "CodeBuddy 的登录页不支持企业微信扫码，只能手工粘贴"——也就是给**企业/API 控制台**用户
  一条不用扫码的接入方式，不是个人 OAuth 号的替代品。
- **本地现状**：`internal/auth` 只认 OAuth 账号；企业号的目录、`IsEnterprise` 已有支持。
- **裁决**：本地若有企业 API Key 用户才有价值，否则不做。牵涉账号模型、面板导入、
  选号与配额语义（这类账号没有签到/积分）。


### 9. 等效成本计价

- **远端**：`wb_pricing.py`（2772 行）。按 OpenRouter 快照把每行 usage 折算成 USD/CNY，  
  支持按输入长度分档、按时段分档，价格策略按内容哈希存时间线，无引用策略定期清扫。
- **本地**：`internal/usage` 只记 credit 与 token，无折算。
- **取舍**：口径是「同样的 token 在 OpenRouter 要多少钱」，**不是实际计费**，容易被误读成账单。  
  若做，必须和 credit 列分开呈现并标"估算"。

### 10. 桌面端 at-rest 加密凭据解密导入

- **远端**：`wb_atrest.py`（915 行）。解 `{"$wbEncrypted":suite,"envelope":base64}` 信封  
  （AES-256-GCM + 自定义 AAD），密钥只能从**运行中的** `WorkBuddy.exe` 内存里回收  
  （判据：`sha256(窗口)[:8]` 等于 keyblob 里 static-v1 槽的 `protectorKeyId`）。
- **本地**：导入只支持登录流程与 auth JSON 文件。
- **核实：只在「网关与桌面客户端同机」时成立。** 入口是 `POST /accounts/import/desktop`，
  它扫的是**本机**桌面客户端的凭据目录；密钥还必须从**正在运行**的 `WorkBuddy.exe` 进程内存里
  回收（`recoverKey`）。网关部署在远端服务器/容器里时：没有 `.info` 文件、没有进程可读，
  即使把文件拷过去，密钥也取不到——这条对本项目当前的部署形态**没有价值**。
- **取舍**：仅当用户把网关跑在自己装了桌面客户端的机器上（本仓库也支持本地跑）才有意义。
  纯 Python 手写 AES-GCM 不引依赖的做法在 Go 侧不适用（`crypto/aes` 直接可用，反而更简单）。
  风险在于**读别人进程内存**这件事本身，建议只在面板显式点击时执行并写清提示。

### 11. 订阅桥（Clash/Mihomo → 每节点本地 HTTP 监听）

- **远端**：`wb_subbridge.py`（584 行）。解析订阅与 V2Ray/SS/SSR/Trojan 链接，渲染 mihomo 配置，  
  为每个节点开一个本地 HTTP 监听，端口映射粘性（`portmap.json`），槽位 URL 重渲染后不变。
- **本地**：`internal/proxies` 支持导入/测试/绑定/出口探测，但没有「多节点在本机落地成可绑定槽位」  
  这一层。

### 12. 网页活跃：300 条提示词池 + 云 agent 驱动

- **远端**：`wb_webprompts.py`（316 行，单条 ≤30 汉字，只问不需要联网、不引长答案的小问题）、  
  `wb_webagent.py`（282 行，ACP over streamable HTTP：`initialize → session/load → session/prompt`）。
- **本地**：网页活跃上报有（`internal/upstream/web.go` + `autotask.go`），但只发一句 `Hi`，  
  也没有驱动网页端云 agent 会话的链路。
- **取舍**：提示词池是纯增量、低风险；ACP 那条是为「只建会话不接沙箱会永远停在 CREATING」补的，  
  本地若不做网页云 agent 打卡就不需要。

### 13. 出口情报补 `hosting` 判定（住宅 / 机房）

- **远端**：`wb_ipintel.py`（107 行），取 `ip-api.com` 的 `hosting`/`as`/`proxy`/`mobile`。
- **本地**：`internal/proxies/exit.go` 已在查 `ip-api.com`，已解析 `query/country/regionName/city/isp`，  
  **缺 `hosting` 与 `as`**。
- **规模**：极小，加字段 + 面板标签即可。注意保留「查不到就报 unknown」而不是报错。

### 14. `/v1/completions`（legacy 文本补全）



- **远端**：路由 `1050` 行附近，与 chat 同一流水线。
- **本地**：没有。少量老客户端会用，成本极低（转成 chat 的 `prompt` 即可），价值也低。

## 五、非缺口：本地已有等价能力

### 8. `/v1/usage`、`/v1/balance-summary`（初稿误判为缺口）

- **远端实现**：`_get_v1_balance_summary` → `credit_balance_summary(realm)`，聚合**全部启用账号**
  的缓存积分快照；`_get_v1_usage` → `usage_snapshot(...)`，同样是全池统计（带 ETag 缓存）。
  鉴权用的是 `_authorized()`，也就是**面板/API Key 那一把**，不是按调用方的 Key 过滤。
- **改判理由**：初稿以为这是「客户端自助查自己的额度」，读实现后确认不是——它是把面板已有的
  统计接口**换个 `/v1` 前缀再暴露一遍**，返回的是**全池**数据。本地 `internal/usage` +
  面板 `/panel/api/usage`、`/panel/api/credit_*` 已经有等价能力，只是不在 `/v1` 下。
- **若仍要做**，动机只能是"给脚本/第三方一个 REST 口"，且必须明确这是**全池**视图——
  任何有效 Key 都能看到所有账号的余额汇总。

## 六、待决策：CodeArts 第三出口

- **远端**：`wb_codearts.py`（3059 行）+ `wb_codearts_crypto.py`（253 行），含华为云 OAuth
  三个入口（`/codearts/accounts/oauth/start`、`/oauth/callback`、`poll`）、SSE 协议转换、自动签到、
  独立模型目录（`/v1/models?realm=codearts`，模型 ID **精确匹配**，不做别名或静默回退）、
  独立的代理绑定与冷却。
- **本地**：完全没有。`realm` 只有 `cn` / `global`。
- **体量判断**：这不是「加一个功能」，而是**加一条新上游**——账号体系、鉴权、目录、计费口径、  
  面板视图、故障隔离全都要另开一套，规模约等于现有 `cn/global` 双域逻辑的三分之一。
- **建议**：先明确「有没有 CodeArts 账号可得、值不值得为它维护第二套协议」再谈，不要混进 P0/P1 的排期。

## 七、不建议吸收

| 项                               | 原因                                          |
| ------------------------------- | ------------------------------------------- |
| Windows 便携版自带精简 Python 运行时      | 本地是 Go 单二进制，天然没有这个问题                        |
| OpenWrt 包（`wrt/`）与 `-m` 启动省内存优化 | 同上，属于 Python 运行时特有的部署问题                     |
| 单文件 `dashboard.html` 看板         | 本地 React 19 + HeroUI 面板在功能和可维护性上已经领先，没有回退理由 |

## 八、要做的事（最终清单）

14 个功能缺口 + 1 个非缺口，逐个过完后的结论：**真正"必做"的只有 3 项**。

| 分档      | 数量 | 项                                          |
| ------- | -- | ------------------------------------------ |
| **必做**  | 3  | #1 Codex 压缩、#2 `cache_retention`、#13 `hosting` 字段 |
| 有条件     | 2  | #4 身份切换（被 429 打爆时）、#6 搜索兜底（有 Codex App 用户时）  |
| 依赖他项    | 1  | #5 作用域分流（只有随 #3 媒体一起做才有意义）                |
| 按需      | 7  | #3 媒体、#7 静态 Key、#9 计价、#10 at-rest、#11 订阅桥、#12 网页活跃、#14 legacy |

### 实施状态（2026-11-11）

五项全部落地，含单测；下面「改动要点」保留为设计记录，实际落点以此表为准。

| 项 | 落点 | 与本清单的偏差 |
| -- | ---- | ------------ |
| #1 Codex 压缩 | `internal/server/compaction.go`、`bridge.go`（`rewriteCompactionInput` 前置拦截 + `POST /v1/responses/compact`） | 无 |
| #2 `cache_retention` | `internal/upstream/official_body.go`、`client.go`、`cmd/server/config.go` | 白名单按**模型名前缀**匹配，比「非自定义模型」更可控；默认关 |
| #13 `hosting` | `internal/proxies/exit.go`（`fields=` 列全）、`proxies.go`、面板 `ProxiesPage` | 额外补了 `as`（ASN），`hosting: null` 单独挡掉（否则会被判成住宅） |
| #4 身份切换 | `internal/upstream/identity.go`、`internal/server/identity.go`、`internal/panel/identity.go` | **上限 3 次**（桌面→VSCode→CLI 就三条线，第 4 次没有下一跳）；只在 **429** 换，且只换身份不换号；只有 `product=auto` 的号参与（钉死身份的不猜） |
| #6 搜索兜底 | `internal/server/responses_websearch.go`（`web_search` 工具桥到官方 `/agenttool/v1/search`，最多 3 轮） | 客户端另有工具要调时整轮交回客户端，不代跑；流式一次性合成（正文不边生成边推） |

### 必做 1 / 3：Codex 上下文压缩

- **落点**：`internal/server/bridge.go`（`openaiResponses:77` 加前置判断、路由表 `:42` 加
  `POST /v1/responses/compact`）、新建 `internal/server/compaction.go`。
- **关键约束**：本地请求/响应翻译**不在本仓库**——`bridge.go:28` 的 `translators = builtin.Registry()`
  来自外部模块 `CLIProxyAPI/v8/sdk/translator`，走 `TranslateRequestEnvelope` / `TranslateStream`。
  而 `compaction` 项不是标准 chat 消息，**必须在交给翻译层之前拦截**，否则会被当成未知项丢掉，
  表现为"压缩请求发上去、回来一条普通回答"。这条决定了实现不能靠改 SDK。
- **改动要点**：
  1. 请求侧识别 `input[]` 里的 `{"type":"compaction_trigger"}`，以及回放的
     `{"type":"compaction","encrypted_content":...}`；后者要先解成上下文文本再进 chat 形态。
  2. 压缩回合用固定摘要提示词走既有 chat 管线取摘要文本，再把结果包成**恰好一条**
     `compaction` 项返回（`encrypted_content` 是自己编码的，不需要和上游格式一致）。
  3. `/v1/responses/compact` 是旧形态，摘要提示词由客户端放在 user 消息里——**不要**再加一份，
     否则模型会把指令当内容摘要。
- **验收**：`httptest` 假上游跑四步链路（正常轮 → `compaction_trigger` → 回放 compaction 项 →
  继续提问），断言第二步只输出一条 compaction、第四步上下文没丢；流式/非流式各一条。
- **规模**：中，约 300-500 行新代码 + 测试，是三项里唯一需要新文件的。

### 必做 2 / 3：`cache_retention=long`

- **落点**：`internal/upstream/official_body.go`（注入逻辑）、`internal/upstream/client.go:1166`
  （调用点需要能拿到模型名与「是否自定义模型」）、`cmd/server/config.go` 的 `upstream` 节新增开关。
- **改动要点**：只在特定条件下补 `cache_retention:"long"`，条件至少包含「CLI/桌面身份」
  与「非自定义模型」；默认不无条件开。
- **风险（必须写进注释）**：1 小时缓存在部分模型上**写入更贵**，本地已有实测结论（只覆盖过
  deepseek）。所以宁可默认关、按模型白名单开，也不要图省事全开。
- **验收**：单测断言 body 字段的有/无；真机对比留给运维，单测不连外网。

### 必做 3 / 3：`hosting` 判定

- **落点**：`internal/proxies/proxies.go` 的 `Exit` 结构（加 `Hosting bool` / `AS string`）、
  `internal/proxies/exit.go` 的 `builtinExitURLs`（当前是 `http://ip-api.com/json/?lang=zh-CN`，
  **没带 `fields=`**，所以默认返回里没有 `hosting`/`as`）与 `parseExit`、`internal/proxies/view.go`、
  面板对应类型与展示。
- **改动要点**：查询串补 `fields=...,hosting,as`；`hosting=true` 显示「机房」，false 显示「住宅」；
  查不到仍按现有口径记 `unknown`，不要因为多要两个字段就把整个查询判失败。
- **规模**：极小，三项里最适合先做（热身 + 验证发布流程）。

### 有条件的 2 项

- **#4 身份切换**：触发条件只有一个——**账号被 429 打爆、且第二条配额线确实存在**。做的话按远端
  已验证的形态落地：账号级 `product` 字段（`internal/auth` 加字段 + 面板选择器），默认桌面端；
  自动切换只在 429 时按 桌面→VSCode→CLI 找"允许的"下一个、单请求上限 4 次、60 秒内算同一轮。
  成本主要在头集合夹具（每个身份一套 raw socket 断言），不是逻辑。
- **#6 搜索兜底**：触发条件是**有 Codex App 用户**。本地现状是 `responses.go:293` 直接跳过
  服务端 `web_search` 工具定义。最小改动不是自建多引擎，而是把该工具桥到已有的官方
  `/agenttool/v1/search`（`internal/upstream/search.go` 现成）；只有在官方接口也不可用时，
  才考虑多引擎兜底。

### 依赖项

- **#5 作用域分流**：单独做没有意义——它存在的唯一理由是「媒体号池不被文本请求打满」。
  所以它跟随 #3 媒体走：真要做媒体时，用现有 `Groups` 表达媒体池 + 实例级入口开关，
  比照远端新加一套 scope 更省。

## 九、许可证与署名

远端是 **MIT**，把它的代码搬进本仓库（AGPL-3.0-only）在许可方向上没有障碍。反过来不成立：  
本仓库的代码不能以非 AGPL 的形式流回远端。新增源文件仍按 `AGENTS.md` 的要求写三行 SPDX 文件头，  
`license_test.go` 会校验。

参考其**实现思路**（如媒体端点的头差异、压缩项的回放语义）而不复制代码时，许可不约束，  
但建议在 `docs/official-client.md` 里注明线索来源。
