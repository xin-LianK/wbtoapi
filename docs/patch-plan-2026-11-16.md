# 整改方案 2026-11-16（每日巡检产出）

本文汇总 2026-11-16 巡检得到的四项改动：上游 `workspace root` 渠道指纹清洗（**要改代码**）、`config.json` 版本覆盖项清空（**改本机配置两行**）、#429 任务码缺口（**先补口径，判据实测后再决定是否写动作**）、hub 监测下线（**已执行**）。

> 本轮只出方案，未动任何代码与配置（仅第三节的监测下线已执行）。落地顺序建议「一 → 二 → 四」，每条都能单独编译、单独测试。

## 结论速览

| 项 | 结论 | 改代码 | 优先级 |
|---|---|---|---|
| 上游 604bb31 `workspace root` | 本地确实没有该规则，Codex / DSH 系 system prompt 仍会吃 400 11-128 | 是（`internal/upstream`） | 高：直接影响请求能否通过 |
| `config.json` 版本覆盖 | 配置把两域钉在 5.9.3 / 2.165.0，抵消今早的 5.11.3 对齐 | 是（仅本机配置两行） | 中：影响 UA 字面 |
| #429 `skill_share` | 真缺口，判据未知，需先实测 | 暂不改动作，先补「不可自动」口径 | 中：影响积分，不影响请求 |
| #429 `wb_wechat_oa_subscribe_task` | 不是缺口：需在微信里关注满 24h，已记在 `docs/tasks.md:73` | 只改提示口径 | 低 |
| hub 监测 | 远端连续三日 404 | 否（状态文件 + 自动化已改） | 已完成 |

---

## 一、补齐「workspace root」渠道指纹清洗（上游 604bb31）

### 上游做了什么

上游在 2026-11-13 的 `604bb31` 加了一条清洗规则：Codex / Claude Code 系客户端的 system prompt 里有一句

```
Relative paths resolve from the workspace root; absolute paths identify ...
```

`workspace root` 是这类客户端的特征用语，上游按它做渠道指纹累计判定，到阈值即回 400（业务码 11-128 `Illegal API invocation from an unapproved channel`）；替换成语序自然的 `root of the workspace` 即放行。

关键点是**作用域刻意收窄**：只改 `system` / `developer` 级内容，`user` / `assistant` / `tool` / 无 role 以及 `tool_calls.arguments` 里的同一短语原样出站。上游 review 里有一个反例——`{"command":"echo workspace root; cc_version=1"}` 被全通道清洗后会因为 `cc_version=1` 被键值剥离层删掉而变成无效 JSON——这条规则因此不进指纹预检，只在 system 级做定点替换。

### 本地现状

| 位置 | 现在的行为 | 与本改动的差距 |
|---|---|---|
| `internal/upstream/sanitize.go:95-99` | 注释写明「以下四条」渠道指纹，作用域「所有角色都改」 | 没有 workspace root 这一条，也没有「唯一例外只改 system」的口径 |
| `internal/upstream/sanitize.go:107-120` | 四条正则：身份句、DSH GUI 段、品牌句、OhMyOpenCode | 缺 `sanitizeWorkspaceRootRe` |
| `internal/upstream/sanitize.go:175-188` | `hasChannelFingerprint` 只探 harness / ohmyopencode / 身份动词 | 该短语不参与预检（与上游一致，属正确做法，无需改） |
| `internal/upstream/sanitize.go:191-216` | `sanitizeText` 不接收 role，命中任一指纹即跑完整链 | 缺「只替换该短语」的短路径 |
| `internal/upstream/sanitize.go:294-334` | `sanitizeMessages` 只遍历 content / reasoning / reasoning_content / tool_calls，**不读 role** | 缺角色判定，加规则后会误伤 user 原文与工具参数 |
| `internal/upstream/client.go:47` | 11128 归 `ErrChannelRefused`；`internal/server/handler.go:2150` 不罚号，但会换号继续尝试 | 换号不改变通道指纹，等于把池里的健康号挨个白占一遍（占在途名额），最后仍把 400 原文透传给客户端 |

结论：**问题真实存在**。本地已有 4 条同类指纹，唯独没有这一条；而 Codex / DSH / Claude Code 系客户端的 system prompt 里恰好都有这句话，属于「哪种请求会 400」的确定项。

### 改动设计

1. **新增正则**（`sanitize.go`，接在 `sanitizeBrandRe` 之后）

   ```go
   // sanitizeWorkspaceRootRe Codex / Claude Code 系 system prompt 的特征短语 workspace root：
   // 上游按它做渠道指纹，命中即 400 11-128。只作用于 system 级内容（见 sanitizeTextForSystem），
   // 其余角色与工具参数原样保留。
   sanitizeWorkspaceRootRe = regexp.MustCompile(`(?i)\bworkspace[ \t]+root\b`)
   ```

   两端带词边界，只动独立短语，不误伤 `workspace roots` 之类的更长子串；替换串 `root of the workspace`。

2. **新增 system 级入口**（`sanitize.go`，放在 `sanitizeText` 之后）

   - `sanitizeTextForSystem(text string) string`：只做 `sanitizeWorkspaceRootRe` 替换，**不**进入 `hasFingerprint` 的完整链。避免「只含该短语的文本」因为预检命中而被连带跑一遍键值剥离（正是上游那个无效 JSON 的成因）。
   - `sanitizeContentForSystem(v any) (any, bool)`：`sanitizeContent` 的 system 版，只对 `text` part 做上述替换，image 等 part 不动。

3. **`sanitizeMessages` 按角色分流**（`sanitize.go:294`）

   - 读取 `m["role"]`（字符串，`strings.TrimSpace` 后 `EqualFold` 比较 `system`）。
   - `system` 的 `content` 走 `sanitizeContentForSystem`，其余字段仍走原 `sanitizeText`（头/键值等指纹不分角色，保持现状）。
   - 其他角色的 `content`、所有角色的 `tool_calls.arguments`、`reasoning` / `reasoning_content` 维持原样，不碰 workspace root。
   - 顺序上有个现成便利：`payload.go:60` 先跑 `normalizeRoles`，`payload.go:92` 才跑 `sanitizeMessages`；而 `normalizeRoles`（`payload.go:244`，赋值在 `:259`）已经把 `developer` 改写成 `system`，所以这里**只需判断 `system`**，不必再认 `developer`。

4. **注释同步**：`sanitize.go:95-99` 的「以下四条」改「五条」，并写明「身份句、品牌句、OhMyOpenCode 三条所有角色都改；workspace root 是唯一例外，只改 system 级」。

5. **文档同步**：在 `docs/upstream.md` 的「功能类和后来补看的一个修复」表（`:114` 起）加一行 `604bb31 | 清洗 system 级 workspace root 渠道指纹 | 照搬，作用域只到 system：user 原文、工具参数、推理内容原样出站`；这是该文件记录「挑哪些上游修复进来」的固定位置。

### 测试清单（新增 `internal/upstream/sanitize_test.go` 用例）

| 用例 | 断言 |
|---|---|
| `system` 含 `workspace root` | 出站为 `root of the workspace` |
| `user` / `assistant` / `tool` / 无 role 含同一短语 | 原样，零改动 |
| `assistant.tool_calls[].arguments` 含 `{"command":"echo workspace root; cc_version=1"}` | 该字符串**一字节不变**（JSON 有效性回归） |
| 大小写混写 `Workspace Root`、制表符分隔 | 命中并替换 |
| 更长子串（`workspace roots`、`my-workspace root`） | 不命中 |
| 同一段文本跑两次 | 幂等（第二次零改动） |
| 只含该短语、不含其他指纹 | 不进入键值剥离，`cc_version=` 等不被删 |

### 验收

- 单测：`go test ./internal/upstream -run Sanitize`
- 全量：`gofmt -l internal cmd`（应无输出）、`go test ./...`
- 线上观测：Codex / Claude Code 客户端的 system prompt 请求不再出现 `11128` / `ErrChannelRefused`；同一请求重放 200。

### 风险与回滚

- 风险面很窄：只改 `system` 级且只替换一个短语，语义不变。
- 若出现「模型读不到 workspace root 这个说法」的个案，把规则改成只在 `sanitize_blacklist_fingerprints=true` 下生效（目前该开关 `config.json:75` 默认 true，已是既有闸门），或直接回滚本提交。
- 提交建议：`fix(upstream): 清洗 system 级 workspace root 渠道指纹`。

---

## 二、#429 任务码缺口：`skill_share` 与 `wb_wechat_oa_subscribe_task`

### 现状

本地自建动作表已覆盖 issue 里点到的绝大部分码：

| 任务码 | 状态 | 位置 |
|---|---|---|
| `create_canvas` | 已实现 | `internal/panel/autotask.go:120` |
| `skill_1` | 已实现 | `internal/panel/autotask.go:140` |
| `WorkBuddy_Valorant` | 已实现（按名字搜专家） | `internal/panel/autotask.go:152` / `:177` |
| `expert_use_maidehao` / `douyinyin` / `shangjia_xuanchuan` / `bowangwang` | 已实现 | `internal/panel/autotask.go:157-172` |
| `skill_share` | **未实现**，全仓库零命中 | — |
| `wb_wechat_oa_subscribe_task` | **未实现**，且按设计做不了 | `docs/tasks.md:73`、`internal/upstream/tasks.go:134` |

两个缺口性质不同：

- `wb_wechat_oa_subscribe_task`（关注公众号满 24 小时）需要真人去微信里关注，`docs/tasks.md:73` 已经写明「面板做不了」。**不是能力缺口，是已知限制。**
- `skill_share` 全仓库零命中，也从未实测过判据，属于**真缺口**：和当年 `skill_1` 一样，需要先抓包确认服务端认哪个事件，盲写动作会白跑一轮还占配额。

### 现在的问题在哪里

`internal/panel/taskdiscover.go:289` 用 `exactActionFor(code) != nil || isMPTaskCode(code)` 判「已实现」，未实现的一律在 `:302-304` 标「需人工实现：无事件链族规则」。这条文案对 `skill_share` 是对的，对 `wb_wechat_oa_subscribe_task` 有误导——那个任务再加一行映射也做不了，面板会一直显示「需人工实现」，看上去像待办而不是已知限制。这与 `docs/tasks.md:90` 承诺的「标出面板做不了、靠真实使用点亮的任务」也不一致。

### 方案 A（建议先做，低风险，不改动作表）

1. 新增一张 `unsupportedTaskCodes map[string]string`（code → 一句话原因），放在 `internal/panel/autotask.go` 的 `mpTaskCodes`（`:322`）附近，先登记：

   ```go
   var unsupportedTaskCodes = map[string]string{
       "wb_wechat_oa_subscribe_task": "需在微信里关注公众号满 24 小时，面板做不了",
       "skill_share":                 "判据未实测（疑似需真实技能分享行为），暂不自动",
   }
   ```

2. `internal/panel/taskdiscover.go:302` 的三分支前插一档：命中该表 → `Note = 原因`，并单独计入统计（不再并进「需人工」），任务中心按「面板做不了」渲染。
3. 顺手修一处过期注释：`autotask.go:22` 仍写「仍未破解：skill_1」，但 `:140` 已经实现并在 2026-09-12 点亮，应删掉这行以免下次误判。

验收：任务发现里 `wb_wechat_oa_subscribe_task` 显示为「面板做不了」而不是「需人工实现」；`skill_share` 显示为「判据未实测」；`Skill`/`Manual` 计数不再把两者算作待实现。

### 方案 B（判据实测后再定，不在本轮）

对 `skill_share` 走和 `skill_1` 一样的路子：真号抓包确认事件码与字段 → 在 `autoActions` 追加一条（位置参照 `:140` 的 `skill_1`）→ 用 `cmd/tasks` 对照上游下发列表确认已被识别 → 三账号实测点亮后再进主线。

不做盲写：动作表里加一条错判据的码，会让「一键完成」白跑一轮真实对话（占配额），比不做更糟。

---

## 三、hub 监测下线（已执行）

`ardeyouxipianyi/workbuddy2api-hub` 连续三日 API 与 `git ls-remote` 均返回 `Repository not found`（11-14 / 11-15 / 11-16），已无监测价值。

已改两处：

| 位置 | 改动 |
|---|---|
| `.workbuddy/memory/upstream-watch.md` | 删除整个 `## hub` 段（`hub_last_seen` / `hub_checked_at` / `hub_last_tag` / `### hub 历史`）；在 `## 历史` 末尾留一行归档：`2026-11-16 hub 监测下线（远端连续三日 404，基准停在 5a95e48 / v2.1.3，不再跟踪）` |
| 每日巡检自动化 prompt | 删除原「四、hub 项目更新：值不值得借鉴」，输出格式从四段改三段（去掉「hub 参照」），Issues 来源收窄到 manager / panel 两个仓库，并注明「该文件已无 hub 段，不要再添加」 |

影响：每日巡检少一段输出；hub 若未来恢复公开，需要手动在新的状态文件段里重建基准，再决定是否把这一段加回自动化。

---

## 四、config.json 的版本覆盖项要清空（今日的版本对齐被配置抵消）

今天上午的 `6bacd85` 把出站身份对齐到官方新版，代码里的两域默认值已经分开：

| 常量 | 值 | 位置 |
|---|---|---|
| `defaultClientVersionCN` / `defaultCliVersionCN` | `5.11.3` / `2.176.0` | `internal/upstream/headers.go:28` / `:33` |
| `defaultClientVersionGlobal` / `defaultCliVersionGlobal` | `5.9.4` / `2.165.0` | `internal/upstream/headers.go:29` / `:34` |

但本机运行时配置把这两项钉死了：`config.json:67` = `5.9.3`、`config.json:68` = `2.165.0`，而 `clientVersion` / `cliVersion`（`headers.go:58-77`）的逻辑是**配置非空即覆盖、两域共用一个值**。结果是国内版账号的出站 UA 又回到 `WorkBuddy/5.9.3 … CLI/2.165.0`，早上那条对齐只对国际版成立。

`config.example.json:64-65` 的默认是空串（空 = 各域取内置默认），所以正确的改法是**清空这两项**，而不是填 `5.11.3`（填了就轮到国际版错）。

- 影响面：只影响 UA 与 `X-IDE-Version` 两段字面，不动请求形状；属于「请求能不能通过」的边缘项，优先级低于第一节。
- `deploy.sh watch` 看不出这个问题：它比的是 app.asar 顶层的 `version` 与静态快照（`snapshots/upstream-shape/static-5.9.2.json` 的 `meta.asar_mtime` 还是 10 月），报「5.9.2 → 5.9.3 无漂移」。真值以代码常量 + 安装包 `package.json` 的 `@genie/workbuddy-desktop` 版本为准，建议落地时顺手人工确认本机装的是哪个版本。
- 改动只有两行 JSON，不进 git（`config.json` 未纳入版本库），也不影响 `config.example.json`。

---

## 五、本轮明确不改的东西

- **出站请求头形状**：`deploy.sh watch` 零漂移（30 个头），今日 `6bacd85` 也刚核对过 `X-Agent-Type` / `X-Root-Request-ID` 这批只在子会话与压缩场景发的头 → 不动 `headers.go`，只按上一节清空配置里的版本覆盖。
- **#443 / #444 / #405 / #440**：分别是 manager 的模型档位选择器、仪表板缓存、账号保护预警确认态、国内版 503 呈现，均非本仓库代码线。其中 #443 的结论值得记一笔：官方客户端的「快速 / 均衡 / 极致」（0.15x / 0.50x / 1.00x）走客户端自有的模型选择协议，不在 OpenAI `model` 字段；`auto` 是上游自动路由的单一条目，本地按 model id 透传、倍率读目录 `credits`（`client.go:2142`），**不需要为档位改代码**。
- **hub 代码**：对方 MIT、本仓库 AGPL-3.0，即使恢复公开也只借鉴做法与口径，不复制代码。

## 六、落地顺序与验证

```bash
# 一（改代码）
gofmt -l internal cmd            # 应无输出
go test ./internal/upstream
go test ./...

# 二（改口径，含前端提示文案时额外跑）
go test ./internal/panel
cd web && npm run build

# 三（已执行）
cat .workbuddy/memory/upstream-watch.md
```

提交拆分建议（scope 取自 `docs/commits.md`）：

1. `fix(upstream): 清洗 system 级 workspace root 渠道指纹`
2. `feat(tasks): 区分「面板做不了」与「需人工实现」的任务码`
3. 第四节只改本机 `config.json`（不在版本库），不改代码；状态文件与自动化同样不需要提交。
