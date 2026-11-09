# testing.md — 质量与测试（第 11 章）

本文覆盖第 11 章：质量门、单元测试矩阵与注入缝、mock 适配器端到端联调、竞态与优雅关闭、本地联调宿主。完成定义：质量门全绿 + §11.2 矩阵全过 + §11.3 端到端可复现。

## 11.1 质量门

合并前必须全绿（本仓库质量门的唯一口径；[`../AGENTS.md`](../AGENTS.md) 只指向本节）：

```bash
gofmt -l .          # 必须无输出
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

命令在 `flake.nix` devShell 内执行（direnv `use flake` → `nix develop`；`go` 由 devShell 提供，`GOTOOLCHAIN=local`）。额外可选：`golangci-lint run`。

`go test -race ./...` 是**必过项**：本插件大量使用定时器与并发状态，竞态检测不可省。

**记忆子系统的数据库测试**：`internal/memory` 下需要数据库的测试读环境变量 `PERSONA_MEMORY_TEST_DSN`，未设置时 `t.Skip`（质量门在无数据库环境下仍可全绿，但合并记忆相关改动前须带数据库跑一遍）。该变量只是测试基础设施，不属于运行期配置。本地启动测试库：

```bash
docker run --rm -d --name persona-mem-test -p 127.0.0.1:5432:5432 \
  -e POSTGRES_PASSWORD=test postgres:17
export PERSONA_MEMORY_TEST_DSN='postgres://postgres:test@localhost:5432/postgres?sslmode=disable'
go test -race ./internal/memory/...
```

每个测试经 `memtest.NewDB` 建独立 schema（DSN 带 `search_path`），结束时删除，测试之间互不影响。

S3 真实宿主运行验收（只启动 mock，独立 schema；按真实分钟调度，约 1 分钟）：

```bash
go build -C cmd/devhost -o /tmp/persona-memory-devhost .
PERSONA_MEMORY_HOST_BINARY=/tmp/persona-memory-devhost go test ./internal/memory -run TestRuntimeHostCleanup -v -count=1
```

该测试同时需要 `PERSONA_MEMORY_TEST_DSN`；普通质量门不设置宿主变量时跳过这项分钟级验收。

S4 真实数据与真实 LLM 运行验收：设置 `PERSONA_MEMORY_TEST_DSN`（只读其中已采集消息；测试写入独立 schema）、`PERSONA_MEMORY_RUNTIME_LLM_BASE_URL`、`PERSONA_MEMORY_RUNTIME_LLM_API_KEY`、`PERSONA_MEMORY_RUNTIME_LLM_MODEL`，可选 `PERSONA_MEMORY_RUNTIME_LLM_HEADERS`（JSON 对象），运行：

```bash
go test ./internal/memory/summary -run TestRuntimeDailySummary -v -count=1
```

测试把一个真实会话日回放为待摘要日期，不改源库；输出生成结果及来源归属校验，供人工抽读。这是运行抽验，S7 的 20 日正式评测另行执行。

S5 真实宿主注入与故障验收（需已配置可用聊天 LLM；只发送合成聚餐数据）：

```bash
go build -C cmd/devhost -o /tmp/persona-memory-devhost .
PERSONA_MEMORY_CONTEXT_HOST_BINARY=/tmp/persona-memory-devhost go test ./internal/memory -run TestRuntimeHostContext -v -count=1
```

测试使用独立 schema，通过仅供该宿主使用的 TCP 转发切断数据库链路，确认 debug 提示词含记忆块、模型回复提及摘要，故障后仍回复；不会停止共享 PostgreSQL，也不连接 QQ。

S6 显式查询真实宿主验收（仅向已配置聊天模型发送固定合成摘要，不读取真实会话）：

```bash
go build -C cmd/devhost -o /tmp/persona-memory-devhost .
PERSONA_MEMORY_SUMMARY_HOST_BINARY=/tmp/persona-memory-devhost go test ./internal/memory -run TestRuntimeHostSummaryTool -v -count=1
```

同样需要测试 DSN 与宿主聊天模型配置。测试询问上周三（附明确日期），检查 `memory__summary` 调用成功且回复包含对应摘要。

S7 评测：case 与映射表含真实数据，只能放在仓库外或被忽略的目录（如 `bin/eval/`），不得提交；真实数据相关只提交指标记录 `internal/memory/summary/testdata/eval/RESULTS.md`（B 的合成问题集随代码提交）。评测测试只在对应环境变量齐全时运行，否则 `t.Skip`；真实 LLM 评测耗时长，命令显式放宽 `-timeout`（`go test` 默认 10 分钟）。

导出（源库只读；case 名由会话代号与日期拼成 `<会话代号>-<YYYY-MM-DD>`）：

```bash
mkdir -p bin/eval
export PERSONA_MEMORY_EVAL_CASES_DIR="$PWD/bin/eval/cases"
export PERSONA_MEMORY_EVAL_ID_MAP="$PWD/bin/eval/id_map.json"
PERSONA_MEMORY_EVAL_SOURCE_DSN='postgres://...' PERSONA_MEMORY_EVAL_CONV='group:onebot:...' \
PERSONA_MEMORY_EVAL_DATE=2026-11-07 PERSONA_MEMORY_EVAL_CODE=g1 \
go test ./internal/memory/summary -run TestEvalExport -v -count=1
```

A：摘要与话题归类。回放 case、调用真实流水线，每次运行新建隔离 schema；大小模型应与生产的 `memory_llm_model_small` / `memory_llm_model_large` 一致：

```bash
PERSONA_MEMORY_TEST_DSN='postgres://...' \
PERSONA_MEMORY_EVAL_CASES_DIR="$PWD/bin/eval/cases" \
PERSONA_MEMORY_EVAL_LLM_BASE_URL='https://.../v1' PERSONA_MEMORY_EVAL_LLM_API_KEY='...' \
PERSONA_MEMORY_EVAL_LLM_MODEL_SMALL='...' PERSONA_MEMORY_EVAL_LLM_MODEL_LARGE='...' \
PERSONA_MEMORY_EVAL_RUNS=3 \
go test ./internal/memory/summary -run TestEvalDaily -v -count=1 -timeout 3h > bin/eval/daily-report.txt
```

`PERSONA_MEMORY_EVAL_RUNS` 缺省为 3。报告含生成的摘要内容，写到被忽略的 `bin/`，不提交；指标与改动摘要追加到 [`internal/memory/summary/testdata/eval/RESULTS.md`](../internal/memory/summary/testdata/eval/RESULTS.md)。

B：`memory__summary` 的调用时机。需要 `PERSONA_MEMORY_TEST_DSN`（隔离 schema）与宿主聊天模型配置（同 S6）；使用 `cmd/devhost/config.yaml` 的生产人格、每个问题独立群号与合成摘要，不读取真实会话。问题集见 `internal/memory/testdata/eval/tool_queries.json`。须在上海时间 04:00–23:00 之间开始运行，否则摘要日与模型看到的日历日不一致，测试会跳过：

```bash
go build -C cmd/devhost -o /tmp/persona-memory-devhost .
PERSONA_MEMORY_TEST_DSN='postgres://...' PERSONA_MEMORY_EVAL_HOST_BINARY=/tmp/persona-memory-devhost \
go test ./internal/memory -run TestEvalSummaryTool -v -count=1 -timeout 1h > bin/eval/tool-report.txt
```

B 只记录、看趋势，不设门槛；结果与 A 记在同一个 `RESULTS.md`。

## 11.2 单元测试矩阵

```text
用例 → 断言可观察结果
```

| 用例 | 断言 |
| --- | --- |
| S4 LLM 客户端 | 凭据及 JSON 模式、宿主超时隔离、网络退避、结构修复与 token 统计 |
| S6 显式查询与工具 | 日期补全与非法范围、关键词排序与零相关回落、Limit 边界、会话隔离、工具 schema 与错误映射、人格继承及过滤、MCP 刷新保留记忆工具 |
| S6 滚动摘要 | 200 条触发与每日次数上限、增量合并与固定运行序号重放、日摘要并发替换、滚动候选与短期历史去重；`TestRuntimeRollingLifecycle` 以真实 Service / PostgreSQL、HTTP 桩与可控时钟覆盖凌晨归属至日摘要替换 |
| S5 读取与注入 | 最近三天日摘要、可信会话隔离、关键词及时间打分、话题去重、行预算、提示词位置、随机轮次读取、错误及超时降级 |
| S4 日摘要 | 切片与预过滤、来源及归属校验、确定性合并、跨天延续、失败日期重试、幂等覆盖与空结果清理 |
| 无发送者 | 决策结果 `reason=no_sender` |
| 非群聊（群 Handler 收到 `Kind=private`） | 决策结果 `reason=not_group` |
| 非私聊（私聊 Handler 收到 `Kind=group`） | 决策结果 `reason=not_private` |
| 群/私名单四模式（`off`/`open`/`whitelist`/`blacklist`）× 名单命中/未命中/空名单 | 放行或拒绝；拒绝 → `reason=not_allowed` |
| 被名单拒绝的群会话 / 私聊对端 | `reason=not_allowed`，且**不建会话状态、不进历史** |
| 私聊默认（`private_policy=off`） | `reason=not_allowed`，不发送 |
| 私聊 `private_policy=open` | 必回；`Target.Kind=private`、`Target.UserID=<发送者>`、`Target.ChannelID=""` |
| 私聊 `random_enabled=false` | 仍回复（私聊不走随机路径） |
| 私聊 `mention_reply_probability=0` | `reason=probability`，不发送 |
| 私聊 `mention_min_interval` 内第二条 | `reason=cooldown` |
| 私聊 `private_policy=whitelist` + 名单 | 名单内回复、名单外 `reason=not_allowed` |
| 私聊 `reply_mention_sender=true` | 回复只有文本段（私聊不加 At 段） |
| 两个私聊对端（`u1`/`u2`） | 会话键 `mock:bot1:user:<id>` 与覆盖键互不相同 |
| `persona:private` 规则注册 | 规则存在、`EventType=EventMessage`、`Kind=MessagePrivate` |
| 命令触发名 | 命令规则注册为 `agent`：`/agent ...` 命中，`/persona ...` 不再命中 |
| `/agent policy`、`/agent list`（含 `add`/`del`/缺参/非法 scope/mode） | 输出行字面量一致；`add` 已存在、`del` 不存在幂等；非法参数回用法文本 |
| 策略写穿透 | `Storage` 键 `policy` 值含新模式/名单 |
| `Start` 恢复策略（合法 / 非法模式 / `null` 列表 / `[]` 列表 / 坏 JSON / 无覆盖） | 合法值生效；非法模式与 `null` 列表回落配置默认值；`[]` 采用空名单；坏 JSON 不 panic 且回落默认值 |
| 机器人发送者且 `ignore_bots=true` | 决策结果 `reason=bot_sender` |
| 命令消息且 `respond_to_commands=false` | 决策结果 `reason=command` |
| `/agent` 命令 | 决策结果 `reason=command`，且**不进历史** |
| 文本为空 | 决策结果 `reason=empty_text` |
| 文本短于 `trigger_min_chars` | 决策结果 `reason=too_short` |
| 会话被 `/agent off` 后入站 | 决策结果 `reason=channel_off` |
| Storage 懒加载未完成时入站 | 决策结果 `reason=loading`，且本条被暂存：`Get` 放行后**补判一次**并回复（群聊回 `g1`、私聊回对端）；加载窗口内多条只补判最新一条 |
| 懒加载期间暂存的消息 × 加载到的覆盖 | `disabled=true` → 补判结果 `channel_off`、不发送；`persona` 覆盖在补判生成中生效 |
| 非寻址且 `random_enabled=false` | 决策结果 `reason=not_addressed` |
| `random_max_per_hour=0` | 决策结果 `reason=hour_quota`（随机插话关闭） |
| 寻址三种来源：`bot.SegAt` / `bot.SegReply` / `trigger_keywords` | 均判定为寻址（不给出 `not_addressed`） |
| `self_ids` 为空 vs 非空：非空时 At 到他人 ID | 空 → 寻址；非空且未命中 → 非寻址 |
| `self_ids` 非空且 At 命中 `Data[bot.KeyUserID]` | 判定为寻址 |
| `At` 的 `Data[bot.KeyUserID] == "all"`（`@全体成员`） | **任何情况下都不算寻址**：`self_ids` 为空时也不触发；同一消息另含 @他人（`self_ids` 为空）或 @本人（命中 `self_ids`）时仍判为寻址 |
| 列表键写成 YAML 裸数字（`self_ids`/`group_list`/`private_list`）或裸标量 / 逗号分隔字符串 | 元素经 `readStringList` 转字符串后生效，不落入空列表语义；`group_policy=whitelist` + `group_list: [群号]` 命中放行 |
| `random_probability=0` | 永不命中（`reason=probability`） |
| `random_probability=1`（其余条件满足） | 必定进入 `schedule()` |
| `random_min_participants` 不足 | 决策结果 `reason=min_participants` |
| 距 `lastReplyAt` < `random_cooldown`（注入 `p.now` 桩） | 决策结果 `reason=cooldown` |
| 寻址且距 `lastReplyAt` < `mention_min_interval` | 决策结果 `reason=cooldown` |
| 近 1 小时回复数达 `random_max_per_hour`（注入 `p.now`） | 决策结果 `reason=hour_quota` |
| `random_quiet_hours` 跨零点（如 `23:00-07:00`，注入 `p.now`） | 窗口内 `reason=quiet_hours`，窗口外放行 |
| `random_timezone` 影响 `{{now}}` 与静默判定 | 注入不同时区，静默判定边界随之移动 |
| 批处理窗口合并（`batch_window=50ms`，真实定时器） | 窗口内多条消息合并为**一次**生成；`batch_max_window` 上限生效 |
| `st.epoch` 变化（`/agent off\|persona\|reset` 后定时器回调） | 旧回调因 `timerSeq` 失效，不生成、不发送；生成中的在途结果被丢弃，`reason=stale` |
| `/agent persona` 或 `off` 发生在批处理等待中 | 批处理被失效，`awaiting` 清零；随后随机消息仍能正常排程（不永久 `inflight`） |
| 寻址消息在生成在途时到达 | 本轮被保留并按 100ms 重试；首轮完成后补一轮，且两轮分别 @ 各自发送者 |
| `Stop` 发生在批处理等待/已触发回调时 | 批处理失效、`timer` 清空；Stop 后旧 `onBatch` 不重新布防、不新增 `wg` 任务 |
| LRU 达到 `context_max_channels` | 淘汰最久未使用的旧会话；刚创建、`inflight`/`awaiting`、有 `pending` 或正在 Handler 内处理的会话保留 |
| 懒加载期间 `/agent off\|persona\|reset` | 旧快照逐字段合并、不覆盖命令改过的字段；`off` 的暂存消息补判为 `channel_off`，`persona` 的暂存消息用新人格回复，`reset` 清空暂存；合并后的覆盖写穿透落库 |
| 懒加载期间修改名单策略 | `finishLoad` 补判前重新检查策略；拒绝时 `reason=not_allowed`，不发送 |
| 全局信号量占满（`limits_max_concurrent=1` 且已有在途） | `reason=semaphore_full`，不排队 |
| LLM 返回 skip token（无论 `llm_skip_enabled`） | `reason=skipped_by_llm` |
| LLM 返回空串 | `reason=empty_reply` |
| LLM 返回与最近自己发言重复且 `reply_dedupe=true` | `reason=duplicate_reply` |
| 发送失败 | `reason=send_error`，不重排、不重发 |
| `Start` 阶段 ctx 在阶段返回后被 cancel（模拟 kei 的 `defer cancel()`） | 插件级 ctx 仍可用：私聊照常回复（`context.WithoutCancel` 派生） |
| 模板渲染 11 个占位符 | 每个占位符被替换为预期值（含 `{{chat_kind}}` = `群聊`/`私聊`） |
| 模板含未知占位符（如 `{{unknown}}`） | 原样保留 |
| 寻址轮次（@ / 私聊）的请求 `system` | 渲染结果末尾追加寻址版本轮要求块：含「你必须回一句；不要输出 `<skip_token>`」、不含「决定不插话时」 |
| 随机轮次的请求 `system` | 末尾追加随机版本轮要求块：含「决定不插话时，只输出 `<skip_token>`」、不含「你必须回一句」 |
| `llm_skip_enabled=false` 的随机轮次请求 `system` | 末尾追加必须回复块：含「无论对方在聊什么，你都必须回一句」、不含「决定不插话时」 |
| 历史渲染 6 种回落 | `[图片]` / `[表情]` / `[文件]` / `[卡片]` / `[引用]` / `[消息]` 分别命中；`[图片]` 亦为图文混排里每个图片段的内联占位（多图多占位） |
| 历史块头按会话类型 | 群聊 `[群聊记录]`、私聊 `[私聊记录]` |
| 历史裁剪 | 超 `context_max_messages` 或 `llm_history_max_chars`（rune）从最旧丢弃，保留最新一条 |
| 清洗管线 8 步（见 [`persona.md`](persona.md) §8.7） | 每条输入→输出样例一致 |
| 在途写穿透未完成时调用 `Stop`（`Set` 阻塞到 gate 关闭） | `Stop` 先拒绝新写入并等队列排空、再取消插件级 ctx：写入不被取消，`Stop` 在其结束后才返回（持久化后端不丢最后一次覆盖/策略） |
| 连续两次覆盖/策略变更（第一次 `Set` 阻塞） | 第二次 `Set` 在第一次完成前不启动；最终落库值为后提交的新状态，内存与存储一致 |
| `/agent status` | 输出固定字段顺序一行 |
| `/agent persona` | 输出 `persona: persona=<name> 来源=<override\|binding\|default> · 预设=[<按名称排序的全部预设>]` |
| `/agent on`、`/agent off` | 切换开关，触发 `Storage.Set`，关闭时递增 `st.epoch` |
| `/agent reset` | 清历史/覆盖/计数器，置为开启，递增 `st.epoch` |
| `llm.go` 用 `httptest.Server`：200 | 返回解析后的文本 content |
| `llm.go` 429 重试 / 500 重试 | 按 `llm_max_retries` 重试并最终成功 |
| `llm.go` 400 不重试 | 立即返回错误，不重试 |
| `llm.go` 非法 JSON | 返回错误 |
| `llm.go` 缺 choices | 返回错误 |
| `llm.go` 超时（`llm_timeout` 极短） | 返回错误，`reason=llm_error` |
| 多模态开关关闭（`llm_vision_enabled=false`） | 请求体 `messages[1].content` 为块数组：首块 `{"type":"text","text":"[群聊记录]"}`，其后每条历史一个 `text` 块（`<显示名>: ` 前缀）；图片槽位回落字面量 `[图片]`，任何块都不含 `image_url` |
| 多模态开关开启（`llm_vision_enabled=true`） | 图片由插件下载后内联为 `data:<mime>;base64,<数据>`，并在该条历史原本的消息位置产出 `{"type":"image_url","image_url":{"url":"data:..."}}` 块（前一块的文本到此截断）；原始 URL 不出现在请求体中；每条消息最多 1 张（取首个带 URL 的图片段）、总数受 `llm_vision_max_images` 限制（按时间序取最后 N 张）、被文本裁剪丢掉的条目里的图片不发 |
| 图片下载失败路径 | 非 2xx、非 `image/*`、格式不在白名单、超过 `llm_vision_max_image_bytes`、连接失败的图各自被丢弃（`reason` 分别 `status`/`not_image`/`format`/`too_large`/`fetch_error`），其余图片与文本照常发送，不回落到传 URL |
| 图片格式白名单（`llm_vision_allowed_formats=["png"]`，混合 png/jpeg） | 仅 png 进请求体；jpeg 被丢弃且 `reason=format`；未配置时不丢弃任何已识别格式 |
| 图片地址限制 | 回环/私网/链路本地/未指定/组播目标（含 DNS 解析结果）被拒；CGNAT（含 `100.100.100.200`）、IETF/文档/benchmark/保留段、NAT64/6to4/Teredo 同样被拒；拨号时才校验（重定向同样受限）；`data:`/非 http(s) scheme 直接丢弃；宿主自定义 `DialTLS`/`DialTLSContext` 被清空 |
| `llm_vision_max_images` / `llm_vision_max_image_bytes` 校验 | `-1` → `persona: 配置错误 llm_vision_max_images=-1: 必须 >= 0`；`0` → `persona: 配置错误 llm_vision_max_image_bytes=0: 必须 >= 1`；默认 `4` / `4194304`，`llm_vision_enabled` 默认 `false`；`llm_vision_max_images=0` 时不附加图片块；llm_vision_allowed_formats 无额外校验（缺省/空 = 不过滤） |
| `debug_prompts=true` 日志 | 请求日志中 data URL 为 `data:image/png;base64,<N bytes>`，不含完整 base64；响应日志不变 |
| `llm.go` 响应含 `tool_calls`（`content` 为 null / 缺失 / 字符串） | 返回 `ToolCalls`，`Content` 为空串或原文本；无 `tool_calls` 且 `content` 为 null 仍是错误 |
| `llm.go` 请求体（`Tools` 为空 / 非空 / `ToolChoice="none"`） | 空时不含 `tools` 与 `tool_choice`、只有 system + user 两条消息；非空时含 `tools` 且 `Followups` 按序追加在 user 之后；`none` 时含 `"tool_choice":"none"` |
| 无可用工具（`p.tools` 为空，或人格 `tools: []`） | 只调用一次 `Complete`，请求不带 `tools`，行为与引入工具前一致 |
| 人格 `tools` 三态（不写 / `[]` / 名单） | 不写 → 请求 `tools[]` 为当前 `p.tools` 全部且保持装配顺序；`[]` → 不带 `tools`；名单 → 与目录取交集；缺失的 MCP 名字不暴露 |
| MCP 客户端与动态目录 | 按 [`mcp.md`](mcp.md) §14.8 精简新增覆盖：P8 调用闭环、失败不重放、取消三组；P9 刷新、并发快照与关闭两组。集中在 `mcp_test.go` 并复用现有桩；配置、人格和简单命令只补现有表格，不重复工具循环与 mock 适配器测试。阶段验收见 [§14.10](mcp.md#1410-分阶段实现规划)，质量门不变 |
| MCP 目录校验与启动失败 | `mcp_test.go`：非法（含点/超长/控制字符）、重名、非 object schema、要求 tasks 的条目各自整体拒绝，错误细节带出错工具名（转义、按 rune 截断 64）；Start 加载失败返回错误、记 `Warn`、取消插件级 ctx、清理 session、不发布目录，随后 `Stop` 不重复 DELETE |
| MCP 累计读取预算 | `mcp_test.go` 的桩检验分页中途失败与空列表；累计 1 MiB 由 `mcpBudget` 在单次操作内共享（含初始化与分页），超限返回 `response_too_large` |
| 工具循环：模型先返回 `tool_calls` 再返回文本 | 工具被调用一次；第二次请求的 `Followups` 为 assistant(tool_calls) + tool(结果，`tool_call_id` 对应)；最终文本被发送 |
| 工具循环：模型每轮都返回 `tool_calls`（`tools_max_rounds=2`） | 共 3 次 `Complete`，第 3 次 `ToolChoice="none"`；第 3 次仍返回 `tool_calls` 时只取 `content`（空则 `reason=empty_reply`） |
| 工具循环：一轮返回 4 个调用 | 前 3 个执行；第 4 个回忽略文本、`status=ignored`；4 个 `tool_call_id` 都有对应 tool 消息 |
| 工具循环：未知工具名（含当前人格不可用的已装配工具） | 不执行；tool 消息为未知工具文本、`status=unknown_tool`，循环继续 |
| 工具循环：桩工具阻塞到 ctx 取消（`tools_call_timeout` 极短） | 桩收到带截止时间的 ctx；tool 消息为超时文本、`status=timeout`，循环继续 |
| 工具循环：桩工具返回超长内容 / 非法 status | 内容按 `tools_result_max_chars` 截断并加 `…`；非 `ok` 的 status 记为 `error` |
| 工具循环：执行工具前 `st.epoch` 已变化 | 不调用工具，`reason=stale`，不发送 |
| 工具循环：后续轮次 `Complete` 失败 | `reason=llm_error`，不发送 |
| 工具日志 | 工具参数 `args` 只在 `debug_prompts=true` 时出现；MCP 日志契约见 [`mcp.md`](mcp.md) §14.4-14.5，不再单独枚举日志测试 |
| 工具配置校验 | `tools_max_rounds=0` → `必须 >= 1`；`tools_result_max_chars=0` → `必须 >= 1`；MCP 名称与启用规则补现有配置表，见 [`mcp.md`](mcp.md) §14.10.1 |
| 记忆配置 | 默认 `memory_database_dsn=""`、`8`/`50`/`5s`/`2048`；DSN 为空时 `p.mem == nil`，非空时构造 `*memory.Service`；`memory_ingest_interval=0s` → `必须 > 0`；`memory_db_max_open_conns`/`memory_ingest_batch`/`memory_queue_size=0` → `必须 >= 1`；batch 大于队列 → `不能大于 memory_queue_size` |
| 记忆记录构造（`memory_test.go`） | 群聊 `Conv`/`EventID`/`MsgID` 正确；`KeyMessageID` 为数字 `9001` 时 `ReplyTo="9001"`；`Mentions` 排除 `all`；`At` 为 UTC、零值回落 `p.now()`；私聊带 `PeerUserID`、`Name` 回落昵称；`Text` 截断到 2000 rune；自身回复 `Conv.Name` 为空、无消息 ID 时 `EventID=local:<启动时刻>:<序号>` 递增 |
| 记忆入队（`fakeMemory`） | 懒加载中、`/agent off`、机器人发送者的消息照常入队；命令消息、名单拒绝的会话不入队；发送成功后自身回复入队（`Self=true`、人格名、`Sender.ID=botID`），发送失败不入队；`Enqueue` 返回 false 时回复与历史不变 |
| 记忆生命周期 | `Start` 启动记忆；`Stop` 调用记忆 `Stop` 时插件级 ctx 尚未取消 |
| `/agent memory status` | 未启用输出 `记忆：未启用`；启用时两行逐字一致；缺参或非法参数回用法文本，不建会话状态 |

P8 删除 `rag_test.go`、内置注册表测试和旧 RAG 配置用例；通用循环、人格三态与日志参数测试继续保留，桩与配置的迁移见 [`mcp.md`](mcp.md) §14.10.1。

reason 词表（24 个）单测覆盖：`not_group`、`not_private`、`no_sender`、`not_allowed`、`bot_sender`、`command`、`empty_text`、`too_short`、`channel_off`、`loading`、`not_addressed`、`min_participants`、`cooldown`、`hour_quota`、`quiet_hours`、`inflight`、`probability`、`semaphore_full`、`skipped_by_llm`、`empty_reply`、`duplicate_reply`、`llm_error`、`send_error`、`stale`。

### 注入缝

必须在 `Plugin` 上定义（默认指向真实实现，测试替换为桩）：

| 字段 | 默认 | 用途 |
| --- | --- | --- |
| `p.now` (`func() time.Time`) | `time.Now` | 冷却、配额、静默时段的确定性时间 |
| `p.randFloat` (`func() float64`) | `rand.Float64` | 概率边界的确定性 |
| `p.completer` | `*openaiClient` | 屏蔽真实 LLM（实现 completer 接口），返回固定文本、工具调用或错误 |
| `p.tools` (`[]tool`，受目录锁保护) | Setup 时为空，Start/reload 发布完整 MCP 目录；MCP 未启用时为空 | 注入桩工具，断言调用参数与返回结果（[`mcp.md`](mcp.md) §13.2） |
| `p.api` (`bot.BotAPI`) | 注入的 `PluginContext.Bot` | 捕获 `Send` 调用；命令测试用 `bot.NewNoopReply()` 断言 `PlainText()` |
| `p.mem` (`memoryBackend`) | DSN 非空时为 `*memory.Service`，否则 nil | 根包测试注入 `fakeMemory`（`newTestEnvMem` 在 Setup 后、Start 前替换），不连数据库 |

禁止为测试引入第三方 mock 库，全部用标准库 testing + 手写桩。

## 11.3 端到端联调（mock 适配器）

以 kei 仓库检出为宿主，构建一个最小宿主 main：空导入本插件 + 启用 mock 适配器 + `plugins.persona`。装配走 kei 公开门面 `pkg/kei`（`kei.Run` 与 `cmd/bot` 是同一份实现），无需复制 `cmd/bot`：

```go
package main

import (
	"context"
	"log"

	_ "github.com/RandomLemon/kei-plugin-persona" // 空导入即注册插件
	_ "github.com/RandomLemon/kei/adapters/mock"
	"github.com/RandomLemon/kei/pkg/kei"
)

func main() {
	if err := kei.Run(context.Background(), kei.Options{ConfigFile: "configs/config.yaml"}); err != nil {
		log.Fatal(err)
	}
}
```

为稳定复现，配置 `random_probability: 1.0`、`mention_min_interval: 0s`、`random_cooldown: 0s`。

「重启后仍保留」这类断言需要持久化后端：kei 的默认 `storage.type: memory` 随进程消失，把宿主的 `storage: {type: sqlite, dsn: <文件路径>}`（或 `mysql`）配上才成立（见 [`architecture.md`](architecture.md) §4.7 与 kei `docs/configuration.md` §12.5）。SQL 方言是显式 opt-in：宿主 main 必须加 `_ "github.com/RandomLemon/kei/pkg/kei/sql"`（`cmd/bot` 已加），否则装配期报未知存储类型。sqlite 驱动经 cgo 编译，宿主构建需 `CGO_ENABLED=1` 与可用的 C 编译器（kei 的 devShell 已含 `gcc`）。想在同一进程里重复 `kei.Run` 模拟重启时，用 `kei.Options.Plugins: []bot.Plugin{&persona.Plugin{}}` 注入干净实例（与配置里的 `plugins.persona` 同名时注入优先，见 [`architecture.md`](architecture.md) 第 6 章）。

LLM 侧用**本地桩服务**（`python3 -m http.server` 不够，它不会返回 JSON）。20 行以内的桩要点：

```bash
python3 - <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('content-length', 0)))
        body = json.dumps({"choices": [{"message": {"content": "打球可以啊"}}]})
        self.send_response(200)
        self.send_header('content-type', 'application/json')
        self.end_headers()
        self.wfile.write(body.encode())
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', 19090), H).serve_forever()
PY
```

命令序列：

1. 启动：`go run ./cmd/bot -config configs/config.yaml`（mock 适配器 + 本插件；`llm_base_url: http://127.0.0.1:19090/v1`）。
2. 注入第一条群消息：
   ```bash
   curl -XPOST 127.0.0.1:18080/inject -H 'content-type: application/json' \
     -d '{"text":"今晚谁去打球","user_id":"u1","user_name":"张三","channel_id":"g1"}'
   ```
   （`kind` 缺省即 `group`。）
3. 注入 `u2/李四` 的消息，使活跃人数达到 `random_min_participants`。
4. 观察发送：
   ```bash
   curl -sS 127.0.0.1:18080/sent | jq '.[-1].Request.Message.Segments[0].Data.text'
   ```
   期望得到一条 LLM 文本（桩服务返回 `打球可以啊`）。
5. 注入 `/agent status`（核心 `auth.admin_users: ["u1"]`）→ 期望输出含 `persona=` 与计数器。
6. 注入 `/agent off` 后再注入消息 → 期望 `/sent` 不再增长。
7. 私聊（配置 `private_policy: open`）：注入 `{"kind":"private","text":"在吗","user_id":"u1","user_name":"张三"}` → 期望 `/sent` 增长，且 `Target.Kind=private`、`Target.UserID=u1`、`Target.ChannelID` 为空（`curl -sS 127.0.0.1:18080/sent | jq '.[-1].Request.Target'`）。
8. 名单策略（配置 `group_policy: whitelist`、`group_list: ["g9"]`）：向 `g1` 注入消息 → `/sent` 不增长；向 `g9` 注入消息 → `/sent` 增长。再注入 `/agent policy`、`/agent list group add g9`（核心 `auth.admin_users: ["u1"]`）→ 比对 [`participation.md`](participation.md) §7.8 的字面量；配了持久化存储后端（见上）时，重启宿主后 `/agent policy` 应显示 `policy` 键（底层键 `plugin:persona:policy`）里的覆盖值而非配置默认值——默认 memory 后端重启即丢，此时该断言不成立。

说明：mock 适配器的 HTTP `/inject` 支持 `kind`（缺省 `group`；可选 `private`/`channel`，`channel` 构造 `Channel`，`private` 不构造）与任意文本，非法 `kind` 返回 400 且不投递事件；控制面只允许绑定回环地址（非回环需显式开 `allow_remote_control_plane`），插件文档里的 `127.0.0.1:0` 用法不受影响。但 `/inject` **无法构造 `bot.SegAt`**。因此「寻址（@/引用）」用例改用 Go 侧 `Adapter.Inject(ctx, ev)` 注入含任意 `Segments` 的事件（写在 `e2e_test.go`），HTTP 路径覆盖随机插话、命令与私聊（`e2e_test.go` 的 `TestE2EMockAdapterPrivate`）。`/sent` 的每条记录含完整 `Request.Target`，可断言 `Kind`/`ChannelID`/`UserID`（私聊发送 `ChannelID` 为空）。

## 11.4 竞态与优雅关闭

- **停止后不发送**：用假 `BotAPI` 计数，断言 `Stop` 之后不再有 `Send`。
- **及时返回**：在存在待生成协程时调用 `Stop`，断言在 15s 内返回（`wg.Wait` 配合插件级 ctx cancel）。
- **无定时器泄漏**：断言 `Stop` 失效全部批处理（`timer` 停止、`timerSeq++`、`awaiting` 清零），且已触发的旧回调不会重新布防或新增 `p.wg` 任务。
- **幂等**：重复调用 `Stop` 安全无 panic。

## 11.5 本地联调宿主（devhost）

`cmd/devhost/` 是接真实 bot 与记忆库做手工联调的宿主：与 kei 的 `cmd/bot` 同构（空导入 + `kei.Run`），空导入本插件与所需适配器。它是**独立 Go 模块**（自带 `go.mod`，replace 本仓库与同级 `../kei`），因此根模块的质量门不编译它，插件本体的依赖规则也不受影响。

连哪个 bot、用哪个数据库全部由配置决定，改完插件代码重新运行即可复用同一个 bot 与数据库（记忆库表结构由插件启动时自动迁移）：

| 文件 | 提交 | 内容 |
| --- | --- | --- |
| `config.yaml` | 是 | mock 与 `qq-main`（OneBot `forward_ws`，缺省关闭）两个 bot、persona 联调参数；不写密钥与本机地址 |
| `.env.example` | 是 | 需要填写的环境变量模板 |
| `.env` | 否（已忽略） | 本机值与密钥：LLM、SnowLuma 地址与 token、bot QQ 号、测试群、管理员、记忆库 DSN |

`.env` 里的 `KEI_*` 变量覆盖或新增 `config.yaml` 的键，空值不生效（kei `docs/configuration.md` §12.7，插件侧见 [`configuration.md`](configuration.md) §10.3）：`KEI_BOTS_QQ_MAIN_<KEY>` 写 `qq-main` 的设置，`KEI_PLUGINS_PERSONA_<KEY>` 写插件键，二者的列表值用 YAML 写法（如 `[123456]`）；`KEI_AUTH_ADMIN_USERS` 例外，用逗号分隔。常用开关：

- 只用 mock：`KEI_BOTS_QQ_MAIN_ENABLED` 留空或 `false`。
- 关闭记忆：`KEI_PLUGINS_PERSONA_MEMORY_DATABASE_DSN` 留空。
- 新增平台：在 `main.go` 补一行适配器空导入，在 `config.yaml` 的 `bots` 加一个缺省关闭的条目。

`main.go` 已空导入 kei 的 SQL 存储方言（`pkg/kei/sql`），编译需要 cgo（devShell 自带 gcc，`CGO_ENABLED` 默认开启；产物链接 nix store 的 glibc，只能在本机运行）。`config.yaml` 的 kei 自身存储仍是 `storage.type: memory`，`/agent` 的运行时覆盖重启即丢；需要保留时设 `KEI_STORAGE_TYPE=sqlite` 与 `KEI_STORAGE_DSN=<文件路径>`。

mock 适配器的发送 ID 按进程从 `mock-1` 计数，而机器人自身回复以发送 ID 作去重键（`m:<MsgID>`），因此重启后同一 mock 会话里的新回复会与上一轮的旧回复撞键、计为「重复」而不落库。mock 与真实 bot 的 `conv_key` 带平台名，二者互不冲突；用 mock 测记忆层时让 DSN 指向单独的 schema 并每轮重建，见 [`memory/deploy.md`](memory/deploy.md)「注意事项」。

### 运行

```bash
cp cmd/devhost/.env.example cmd/devhost/.env   # 首次：填写 .env
cd cmd/devhost
(set -a && . ./.env && exec go run .)           # devShell 内执行；子 shell 避免密钥留在当前 shell
```

根模块质量门不编译 devhost，改动 kei 或适配器后可单独检查：`(cd cmd/devhost && go build -o ../../bin/devhost . && go vet ./...)`。编译 devhost 一律用 `-o` 输出到已忽略的 `bin/`：在 `cmd/devhost` 里直接 `go build`（包括 `go build ./...`）会在当前目录生成未被忽略的 `devhost` 二进制。

VS Code：装 direnv 扩展（`mkhl.direnv`）让编辑器加载 devShell 的 Go 与 dlv，再在本地建 `.vscode/launch.json`（`.vscode/` 被忽略，不提交）：

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "devhost",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/cmd/devhost",
      "cwd": "${workspaceFolder}/cmd/devhost",
      "envFile": "${workspaceFolder}/cmd/devhost/.env",
      "output": "${workspaceFolder}/bin/__debug_devhost"
    },
    {
      "name": "devhost（仅 mock）",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/cmd/devhost",
      "cwd": "${workspaceFolder}/cmd/devhost",
      "envFile": "${workspaceFolder}/cmd/devhost/.env",
      "output": "${workspaceFolder}/bin/__debug_devhost",
      "env": {
        "KEI_BOTS_QQ_MAIN_ENABLED": "false",
        "KEI_PLUGINS_PERSONA_GROUP_LIST": "[g1]",
        "KEI_PLUGINS_PERSONA_TRIGGER_KEYWORDS": "[bot]"
      }
    }
  ]
}
```

「仅 mock」项沿用 `.env` 的 LLM 与记忆库，只关掉真实 bot、放行 mock 的 `g1`，并以关键词 `bot` 触发（`/inject` 构造不了 @），注入 `{"text":"bot 在吗","user_id":"u1","channel_id":"g1"}` 即可，其余见 §11.3。`output` 把调试二进制放进已忽略的 `bin/`。

### 接真实 QQ 与记忆库

依赖容器（SnowLuma + PostgreSQL）的搭建、开发机 `.env`、正式部署、注意事项与验收见 [`memory/deploy.md`](memory/deploy.md)。
