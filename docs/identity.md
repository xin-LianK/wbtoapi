# 出站身份

官方同一套账号背后有几条按**客户端形态**分的配额线：桌面端那条打满了，VSCode 扩展那条可能还能用。
网关可以指定某个号用哪套身份发对话，也可以交给它在被 429 时自己往下一条换。

代码：`internal/upstream/identity.go`（怎么发）、`internal/server/identity.go`（429 换线）、
`internal/panel/identity.go`（面板改值）。

## 三种形态

| 身份 | 凭证里的 `product` | 依据 | 可信度 |
| ---- | --------------- | ---- | ---- |
| 桌面端 | `desktop`（缺省） | WorkBuddy 桌面端安装包（5.9.3 / 5.9.4）内置的 CLI，就是 `headers.go` 一直在对齐的那套 | 2026-10 生产实测 |
| VSCode 扩展 | `vscode` | Open VSX 上 `Tencent-Cloud.coding-copilot` 4.12.39997873 解包源码里的 `UserAgentHttpInterceptor` / `buildModelRequestHeaders` | **未用真扩展抓包核对**，默认不启用 |
| CLI | `cli` | 与桌面端同源（网关本来发的就是桌面端内置 CLI 的形状） | 只差一个 `acp-connection-id` |
| 自动换线 | `auto` | 桌面端起步，429 时往下换 | 见下 |

### 三套头差在哪

只列有差异的；其余头（账号头、会话头族、`X-Agent-Intent/Type`、`X-Product`）三套一样。

| 头 | 桌面端 | VSCode 扩展 | CLI |
| -- | ----- | ---------- | --- |
| `User-Agent` | `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cli>` | `VSCode/<宿主版本> CodeBuddy/<扩展版本>` | 同桌面端 |
| `X-IDE-Type` / `X-IDE-Name` | `WorkBuddy` | `VSCode` | `WorkBuddy` |
| `X-IDE-Version` | 桌面端版本（5.9.3 / 5.9.4） | 宿主 VSCode 版本（`upstream.vscode_ide_version`） | 桌面端版本 |
| `X-Product-Version` | 不发 | 扩展版本（`upstream.vscode_product_version`） | 不发 |
| `X-Env-ID` | 不发 | `production` | 不发 |
| `X-Stainless-*`（8 个） | 发（对话经 OpenAI SDK） | **不发**（扩展不走 SDK） | 发 |
| `X-Agent-Runtime` / `X-Agent-Purpose` | 发 | **不发** | 发 |
| `acp-connection-id` | 发（按账号稳定派生） | **不发** | **不发**（纯 CLI 进程没有 ACP 连接） |
| 会话 id 形态 | 带连字符的 UUID v7 | 32 位无连字符 hex；`X-Request-ID` 与轮级 id 同值 | 同桌面端 |
| `X-Request-Trace-Id` | 不发 | 每个请求一个带连字符的 UUID | 不发 |
| `X-Model-ID` + 两个 `monitor_*` 时间戳 | 不发 | 发（模型请求才带） | 不发 |
| `X-Private-Data` | 按模型优化开关 | 只在国际站发（国内号删掉） | 同桌面端 |

## 怎么设

凭证文件顶层加一个键（`auths/workbuddy_xxx.json`），或者在面板「账号详情 → 出站身份」里选：

```json
{ "product": "vscode", "accessToken": "...", "refreshToken": "..." }
```

- 键不存在 = `desktop`。老凭证文件不用动。
- 改完**下一个请求就生效**，不需要重启、不需要重登（身份只影响请求头）。
- 写回走 `POST /panel/api/accounts/{uid}/product`，body `{"product":"vscode"}`；
  认不出的值归一成 `desktop`，并把归一后的值写回文件。
- 两个版本段是**实例级**配置（`upstream.vscode_ide_version` / `upstream.vscode_product_version`），
  改完要重启。

## 自动换线（`product: "auto"`）

只在一种情况下换：**429 限流**，同一个号、同一条请求，只换身份不换号。

- 顺序：桌面端 → VSCode → CLI，最多 3 次（三条线走完就没了）。
- 换之前照常过一次出站节奏（`internal/pace`），被丢掉的那一跳按一次失败尝试记账。
- 三条线都 429 → 回到原来的处置（账号级软冷却 + 换号）。
- 402 余额不足、11140 账号被限、11128 通道校验失败、5xx 都**不换**：这些是账号级或通道级的事实，
  换形态要么一样失败，要么把请求放大几倍。
- 钉死身份（`desktop` / `vscode` / `cli`）的号**永远不换**——用户指定了形态，网关不去猜。
  默认就是 `desktop`，所以这项能力默认不参与任何出站。

## 风险与边界

- **VSCode 身份没有真机核对过。** 指纹来自解包源码，可能缺/多某个头。真发出去万一被判成非官方客户端，
  后果是 `11128 request refused`（不罚号，见 `upstream.ErrChannelRefused`），而不是封号。
  先用一个号试：`product: "vscode"` 发几轮，看请求日志里有没有 11128。
- **免不免费是上游说了算。** 换身份只是换「按哪种客户端记账」，能不能用、扣多少积分仍由上游的
  配额线决定。网关不据此做任何承诺，选号权重也不看身份。
- 身份**不参与**选号：同一个号在两条线上是同一个号，池子里的冷却、熔断、积分保底照旧按 uid 记。
