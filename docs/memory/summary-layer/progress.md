# memory/summary-layer/progress.md — 摘要层实现进度

本文跟踪摘要层 S1–S7（[`summary-layer.md`](summary-layer.md) §16.3）的实现进度。

**更新规则**

- 阶段状态只用三个词：`未开始`、`进行中`、`已完成`。
- `已完成` 必须满足 [`summary-layer.md`](summary-layer.md) §16.3.2 的完成条件：本阶段验收表全过、运行验收走通、质量门全绿、§16.3.10 的文档已同步。
- 阶段完成后勾选下方清单、填写完成日期与提交，并同步 [`../progress.md`](../progress.md) 的 M1 / M2 行：M1 = S1–S2，M2 = S3–S7。
- 实现与设计不一致时，先改设计文档，再在「偏差记录」中记一条。

## 阶段进度

| 阶段 | 所属 | 状态 | 开始日期 | 完成日期 | 提交 / PR | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| S1 存储与写入 | M1 | 已完成 | 2026-11-05 | 2026-11-05 | `feat/memory` e965e96 … 171d7bf | 未接入根包；golangci-lint 0 issues |
| S2 根包接入采集 | M1 | 已完成 | 2026-11-05 | 2026-11-05 | `feat/memory` 8568c53 … d14dddf | 运行验收用最小 kei 宿主 + mock 适配器 + 本地 PostgreSQL 走通；golangci-lint 0 issues；2026-11-08 在 SnowLuma + PostgreSQL 测试部署（[`../deploy.md`](../deploy.md)）上复验通过：部署验收、运行中断库、启动时库不可达与恢复、DB 单测 |
| S3 任务框架与清理 | M2 | 已完成 | 2026-11-08 | 2026-11-08 | `feat(memory): add scheduled jobs and retention cleanup` | 真实宿主分钟调度与 status 验收通过；质量门全绿，review 2 轮通过 |
| S4 日摘要流水线 | M2 | 已完成 | 2026-11-08 | 2026-11-08 | `4058e6d` + `fix(memory): bound summary message and overlap budgets` | 预算规则已确认；真实会话日与真实模型抽验通过；本轮质量门全绿、review 1 轮通过 |
| S5 读取与注入 | M2 | 已完成 | 2026-11-08 | 2026-11-08 | `feat(memory): inject recent daily summaries into replies` | 读取与注入、隔离与降级测试、真实宿主验收通过；review 2 轮通过 |
| S6 滚动摘要与显式查询 | M2 | 已完成 | 2026-11-08 | 2026-11-08 | `65e3e58`、`3a93cdb` + `feat(memory): add explicit summary query tool` | 日界、滚动与读取、显式查询工具分开提交；运行验收及质量门通过，两部分 review 均 2 轮通过 |
| S7 评测与调参 | M2 | 进行中 | 2026-11-09 | — | 导出、A、B 三次提交（见「迭代验证记录」） | 持续阶段：评测框架已实现并通过冒烟；case、基线与 20-case 达标待真实数据，首次达标即完成（§16.3.9） |

## 阶段清单

### S1 存储与写入（§16.3.3）

- [x] `go.mod` 引入 `gorm.io/gorm`、`gorm.io/driver/postgres`
- [x] 类型、`conv_key`、模型与迁移、写协程、连库状态机与 `Stop`
- [x] `memtest.NewDB` 与测试库启动说明
- [x] 验收表全部通过
- [x] 质量门全绿
- [x] 文档同步：`architecture.md` 第 5 章、`testing.md` §11.1

### S2 根包接入采集（§16.3.4）

- [x] 记录构造、入队点、生命周期接入
- [x] 配置键与校验、`/agent memory status` 两行
- [x] 验收表全部通过
- [x] 运行验收：落库、重复投递、数据库宕机与恢复、启动时数据库不可达
- [x] 质量门全绿
- [x] 文档同步：`configuration.md`、`architecture.md` 第 1 章 / §3.1 / §3.2 / 第 5 章、`persona.md` §8.4、`testing.md` §11.2

### S3 任务框架与清理（§16.3.5）

- [x] `mem_job`、领取与重试、启动时重置执行中任务、worker 池、调度 tick、`cleanup`
- [x] 配置键、status「任务」行
- [x] 验收表全部通过
- [x] 运行验收：到点后 `cleanup` 任务完成
- [x] 质量门全绿
- [x] 文档同步：`configuration.md`、`persona.md` §8.4、`architecture.md` §3.2

### S4 日摘要流水线（§16.3.6）

- [x] 摘要与话题表、LLM 客户端、按会话建日摘要任务并逐天处理
- [x] 切片、预过滤、切片小结、合并与兜底、写入
- [x] 提示词全文完成并评审
- [x] 验收表全部通过
- [x] 运行验收：真实数据生成昨天的日摘要，人工抽读无明显编造或归属错误
- [x] 质量门全绿
- [x] 文档同步：`configuration.md` 的 `memory_llm_*`、`testing.md` §11.2

验收证据：

- `TestRuntimeDailySummary`：经用户明确批准，将一个已采集会话日回放至隔离 schema，调用现有配置的 `api.deepseek.com`；25 条消息生成 3 个话题，25.83s 通过，prompt / completion tokens = 1476 / 4168。引用归属校验通过；对照来源抽读，主体与核心话题来自原文。单日运行抽验不代表 S7 正式质量指标达标。
- `TestScheduledDailySummary`：真实 Service 调度、worker、HTTP 桩、摘要与 `mem_job.result` 集成验证通过；与上述真实模型 Handler 回放共同提供运行证据，不将二者混称一次完整宿主验收。
- 用户确认预算优先；单条正文 2000 rune（含省略号），切片正文 8000 rune，重叠最多 10 条且按需缩减，文档与边界测试已同步。

### S5 读取与注入（§16.3.7）

- [x] `Context`、关键词相关度与打分、选取与行渲染
- [x] 根包取上下文、记忆块渲染、`renderSystemPrompt` 迁移
- [x] 配置键
- [x] 验收表全部通过
- [x] 运行验收：能接上前几天的话题；数据库宕机时照常回复
- [x] 质量门全绿
- [x] 文档同步：`configuration.md`、`persona.md` §8.5、`architecture.md` 第 2 章

### S6 滚动摘要与显式查询（§16.3.8）

- [x] 摘要日日界改为 `memory_daily_at`（§16.2.3），迁移 `store.LocalDate` 全部调用点
- [x] 滚动摘要触发与流水线、读取侧滚动候选与短期窗口去重
- [x] `QuerySummaries`、`memory__summary`、`toolsFor` 迁移
- [x] 配置键与人格 `tools` 校验
- [x] 验收表全部通过
- [x] 运行验收：当天出现滚动摘要、次日被替换；凌晨消息归入前一天；模型调用 `memory__summary`
- [x] 质量门全绿
- [x] 文档同步：`configuration.md`（含 `memory_daily_at` 日界）、`overview.md` 每日任务链、`architecture.md` 第 1 / 5 章，核对 `mcp.md`

### S7 评测与调参（§16.3.9）

- [ ] 评测数据：`TestEvalExport` 导出与脱敏（会话内一致的 ID 映射）、标注流程走通；case 存放在仓库外（`PERSONA_MEMORY_EVAL_CASES_DIR`），持续积累至 20 个（含连续日、安静日与繁忙日）
- [x] A：`TestEvalDaily` 回放隔离 schema、逐 case 推进 `Now` 调用真实 `Pipeline.Daily`，输出自动指标（覆盖率、过碎、过粗、延续正确率、token）并按 `PERSONA_MEMORY_EVAL_RUNS` 取平均
- [x] B：`TestEvalSummaryTool`（生产人格、每问独立群号）、与 S6 运行测试共用的宿主辅助函数、合成问题集 `tool_queries.json`
- [ ] `testdata/eval/RESULTS.md` 建立基线，按调优循环追加记录
- [ ] 最新记录在至少 20 个 case 上达标：覆盖率 ≥ 80%、归属正确率 ≥ 95%、无编造
- [x] 调参后 S4–S6 测试仍通过；`testing.md` §11.1 补评测运行命令

评测框架已实现：`TestEvalExport`、`TestEvalDaily`、`TestEvalSummaryTool` 与问题集已提交；导出已在本地库验证，case 积累、标注、基线与 20-case 达标项待真实数据。

后续步骤（命令与环境变量见 [`../../testing.md`](../../testing.md) §11.1，规则见 [`summary-layer.md`](summary-layer.md) §16.3.9）：

1. 选样与导出：按 §16.3.9 的选样要求挑会话日，用 `TestEvalExport` 从生产库导出到 `bin/eval/cases/`；同一会话共用 `bin/eval/id_map.json`。
2. 标注：每个 case 先写 `labels.json`，再看模型输出。
3. 基线：用真实 LLM 跑 A（`TestEvalDaily`）与 B（`TestEvalSummaryTool`，需在上海时间 04:00–23:00 之间开始），须先经用户同意；指标与改动摘要写入 `RESULTS.md`，并更新下方「评测结果摘要」。
   - B 基线已完成（2026-11-09，25 题，见 `RESULTS.md`）。
   - 待办：初步基线。2026-11-10 04:00 之后生产库（目前只有 1 个群）有 11-08（287 条，繁忙日）与 11-09 两个完整会话日，可导出为 2 个连续 case，标注后跑一次 A，记作初步基线（case 数不足 20，延续只有 1 组）。
4. 调参：按 §16.3.9 的调优循环每次只改一项，复跑 A / B 并追加记录，直到最新记录在至少 20 个 case 上达标；之后勾选上方清单、把 S7 与 M2 标为已完成。

## 评测结果摘要

详细记录在 `internal/memory/summary/testdata/eval/RESULTS.md`（S7 创建），这里只登记最新一次的结论。

| 日期 | case 数 | 覆盖率 | 归属正确率 | 编造 | 平均 token / 会话日 | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| — | — | — | — | — | — | 尚未评测 |

## 偏差记录

| 日期 | 阶段 | 偏差 | 处理 |
| --- | --- | --- | --- |
| 2026-11-05 | S1 | 实现时补全的细节：`conv_key` 也要求 `Platform` 非空；群会话行不记 `bot_id`；`At` 为零值时用写入时刻；DSN 无法解析时记 `Error` 并退出、状态保持「连接中」；新增常量 `migrateTimeout = 30s`；连库、迁移、写入由同一个后台协程依次执行；被 `Stop` 打断的批次交给 flush 重写；gorm 日志适配器命名为 `store.NewLogger` | 已同步到 [`summary-layer.md`](summary-layer.md) §16.3.3 |
| 2026-11-05 | S1 | 按「不留占位」，`New` 在 S1 不带 `httpClient`（S4 引入 LLM 客户端时再加）；`prepare` 清洗 `Text` 与名字中的非法 UTF-8 和 `\x00`，`Enqueue` 复制 `Mentions`；数据库从未就绪时 `Stop` 对队列残留记 `Warn` | 已同步到 [`summary-layer.md`](summary-layer.md) §16.2.4、§16.3.3、§16.3.4、§16.3.6 |
| 2026-11-05 | S2 | 实现时补全的细节：`p.mem.Start` 放在 MCP 目录加载成功之后；`memEpoch` 在 `Start` 记录；`/agent memory` 不建会话状态，缺参或参数非法回用法文本；自身回复的 `Conv` 取自发送目标（与 `channelState` 同源） | 已同步到 [`summary-layer.md`](summary-layer.md) §16.3.4 |
| 2026-11-08 | S1 | 远程 PostgreSQL 上 `TestConcurrentEnqueueAndStop` 稳定失败（`Written` 少一批、`Duplicates` 多一批）：`Stop` 取消 ctx 时正在 COMMIT 的批次服务端已提交、客户端报错，flush 重写后整批记成重复。改为 `Stop` 后正在写的批次与 flush 共用 `flushTimeout` 收尾时限，不再立即打断 | 已同步到 [`summary-layer.md`](summary-layer.md) §16.3.3 |
| 2026-11-08 | S4 | 评审后调整：切片小结的网络、超时或 HTTP 错误让当天失败重试，只有输出修复后仍不合规的切片可被跳过；合并输入的要点只给 `type` 与 `text`；脱敏改在截断之前；切片输入首行补 `群聊「…」` / `私聊「…」` 标签；`JobResult` 的 `days` / `failed_days` 明确为跨尝试累计天次；LLM 客户端去掉对 `topics` 的硬编码检查，结构校验只由 `Validate` 负责；正则改为包级预编译 | 已同步到 [`summary-layer.md`](summary-layer.md) §16.3.6 |
| 2026-11-08 | S5 | `windowDays` 导出为 `WindowDays`，让日摘要延续与读取共用三日窗口；`memoryContext` 不保留本阶段不使用的轮次参数，随机与寻址均走同一路径 | 已同步到设计文档 §16.3.6–16.3.7 与全部调用点 |
| 2026-11-08 | S1–S6 | 开发前评审决定：摘要日以 `memory_daily_at` 为日界（默认当天 04:00 至次日 04:00 算同一天，`00:00` 即自然日），由 `store.LocalDate(t, loc, dayStart)` 统一换算，作为 S6 第一步单独实现；已入库数据不迁移 | 已同步到 [`summary-layer.md`](summary-layer.md) §16.2.3、§16.3.3、§16.3.5、§16.3.7、§16.3.8；`configuration.md`、`overview.md` 随实现同步 |
| 2026-11-08 | S6 | 开发前评审补定：滚动任务开始前与写入加锁后两次检查 `summarized_upto_id`，已有值即退出，D 不是今天不作为退出条件；更新今日滚动话题时保留 topic_key 与 `first_*`、参与者与要点取并集、标题摘要用新输出，已被今日滚动话题延续的 `k` 不再延续；新增 `RollingMergeSystem` 全文，日摘要提示词不变；新键序号为 `rolling_runs+1`，进度按开始时读到的 `msg_count` / `last_msg_id` 推进；读取侧滚动行排最前、不受 θ、无概况，「最近一天」只按日摘要计算，先按 `HistorySince` 跳过再去重；显式查询的单端日期补全、同日日摘要优先、零相关时退回日期排序，工具描述补充「按日期问时不要填 query」；触发测试移到 `ingest_test.go`，「写事务第 5 步」改为「第 4 步之后」 | 已同步到 [`summary-layer.md`](summary-layer.md) §16.3.8 |
| 2026-11-08 | S6 | 评审补充：滚动任务持久化固定 `JobPayload.Run`，开始及写入加锁后跳过已完成运行，避免摘要已提交但任务状态未保存时重放重复推进；最近日摘要概况独立于话题去重保留；`RollingEvery` / `RollingMaxPerDay`、`KeywordRelevance` / `OpenItems` 导出供写入和查询共用 | 设计与全部调用点同步，均有回归验证 |
| 2026-11-08 | S6 | 完成后评审修正：延续今日滚动话题时，合并输出的标题或摘要为空则沿用旧话题而非本次切片；`memory__summary` 参数为空串时按 `{}` 处理；验收表测试位置改为实际文件（`rolling_test.go`、`summary/rolling_test.go`、根包 `memory_tools_test.go`），偏差表去掉断行空行 | 已同步到 [`summary-layer.md`](summary-layer.md) §16.3.8 与 [`architecture.md`](../../architecture.md) 第 5 章，均有回归测试 |
| 2026-11-09 | S7 | 导出只替换映射表中已知的 ID（来自 sender / mentions），正文里从未作为 sender/mention 出现的号码无法识别；正文先 `Redact` 再替换；`msg_id` / `reply_to` 不重映射。A 固定 `Asia/Shanghai` + 04:00 日界，延续按匹配话题 `topic_key` 集合求交，过粗率分母用生成话题数，分母为 0 的指标输出 N/A，多次运行按分子分母累加求比例。B 的问题集字段为 `message` / `expected` / `query` / `date_from_n` / `date_to_n` / `out_date_n`，日期占位符 `{date:N}` 渲染为 `YYYY-MM-DD`；日期正确率按期望字段（from/to）计数，窗口外召回只统计 `out_date_n` 非零的问题，冗余调用率分母为 `any` 问题，误调用分母为 `no_call` 问题，分母为 0 输出 N/A；问题集未知字段报错。合成摘要每个日期一个不同话题：-1 天在窗口内，-4～-6 天供按话题问（窗口外、但在不带日期调用的默认范围 `[今天-6, 今天]` 内），-7 / -10 / -20 / -21 天供按日期问；相对日期问法用「N 天前」而非「上周三」，避免期望日期有歧义；上海时间 04:00 前或 23:00 后开始时跳过。A、B 运行命令显式放宽 `-timeout` | §16.3.9 与 `testing.md` §11.1 已同步；不改变门槛与流程 |
| 2026-11-09 | S7 | 导出的 `PERSONA_MEMORY_EVAL_CASE`（完整 case 名）改为 `PERSONA_MEMORY_EVAL_CODE`（会话代号），case 名由代号与 `PERSONA_MEMORY_EVAL_DATE` 拼成，日期只填一次，去掉两处日期的一致性校验；case 目录与文件格式不变 | 已同步到 [`summary-layer.md`](summary-layer.md) §16.3.9 与 `testing.md` §11.1 |
| 2026-11-09 | S7 | B 的问题集去掉 `date_from` / `date_to` 字符串字段与 `out_date_n`，日期期望只用成对的 `date_from_n` / `date_to_n`，给出即为窗口外问题；模型空回复被跳过（`reason=empty_reply`）也视为该题结束，不再等到超时判失败。测试精简：早期运行验收改用共用宿主辅助函数，`memtest.Open` 取代各包重复的连库代码。就绪探测改发到专用频道 `ready`，迟到的 status 回复不会算作测试频道的回复，`resetSent` 随之删除；S5 运行验收的种子日期改按宿主的 UTC 计算，此前在 00:00–04:00 UTC 运行会因种子落在宿主的「今天」而读不到。 |

## 迭代验证记录

- S3（2026-11-08）：Nix devShell / `GOTOOLCHAIN=local` 下 `gofmt`、build、vet、带 PostgreSQL 独立 schema 的 test / race、lint 全通过（0 issues）。`TestRuntimeHostCleanup` 用真实 devhost、mock 控制面与分钟时钟，60.10s 通过：cleanup 完成且 status 显示任务行。review 2 轮，最终 `NO_BLOCKING_ISSUES`；修复状态落库失败重试、未启用记忆时的新增配置校验，补齐清理与停止验证；无未复评修复。

- S4（2026-11-08，代码提交）：Nix devShell 下 build、vet、带 PostgreSQL 独立 schema 的 test / race、lint 全通过，0 issues，gofmt / diff check 干净。review 2 轮，代码最终 `NO_BLOCKING_ISSUES`；修复 JSON 修复响应的旧字段残留和任务超时时已完成调用的用量漏记，均有回归测试且经复评。未完成真实数据验收，阶段保持进行中；S5 尚未开始。

- S4 收尾（2026-11-08）：按用户补充要求实现切片预算与单条截断规则，build / vet / 全仓 PostgreSQL race / lint 全过，0 issues。真实数据验收与来源抽读完成；本轮 review 1 轮，最终 `NO_BLOCKING_ISSUES`，无未复评修复。

- S5（2026-11-08）：可信 Scene 与当前会话最近三日日摘要读取、关键词与时间打分、去重、预算和记忆块注入已完成；随机轮次同样读取，未启用时提示词逐字保持不变。Nix devShell 中 build / vet / 带 PostgreSQL 独立 schema 的全仓 race / lint 全过，0 issues；gofmt / diff check 干净。`TestRuntimeHostContext` 使用真实 devhost、现有聊天模型与固定合成聚餐摘要，debug 提示词含记忆块且回复提到聚餐；仅切断该测试宿主的数据库链路后仍正常回复并记录读取告警（未停止共享 PostgreSQL）。review 共 2 轮，最终 `NO_BLOCKING_ISSUES`；首轮功能评审通过，修正测试矩阵表格格式后通过收尾复核。最终源码的真实宿主验收复跑 2.28s 通过。无第三轮后未复评修复。

- S6 滚动摘要（2026-11-08）：写事务触发、增量合并、日摘要并发替换及读取去重完成。Nix devShell 中 build / vet / 全仓 PostgreSQL test 与 race / lint 全通过，0 issues；gofmt / diff check 干净。`TestRuntimeRollingLifecycle` 使用真实 Service、worker、PostgreSQL 隔离 schema 与 HTTP 桩，通过可控时钟验证凌晨消息归前一摘要日、200 条触发 rolling、04:00 后 daily 替换。review 2 轮，最终 `NO_BLOCKING_ISSUES`；修复固定 Run 重放幂等、全话题延续时概况丢失，均有回归测试并复评。显式查询工具待下一提交；无未复评修复。

- S6 显式查询与工具（2026-11-08）：`QuerySummaries`、可信 Scene、日期范围与关键词排序、默认关闭的 `memory__summary`、人格过滤和工具目录完成，全部调用点与文档已同步。Nix devShell 中 build / vet / 全仓 PostgreSQL test 与 race 全通过，lint 0 issues，gofmt / diff check 干净。`TestRuntimeHostSummaryTool` 用真实 devhost、现有聊天模型与固定合成的上周三摘要，工具日志显示成功调用，回复包含对应餐厅安排；最终测试源码复跑 1.56s 通过，未读取或发送真实会话。review 2 轮，最终 `NO_BLOCKING_ISSUES`；补充多个正相关候选，精确验证相关度、日期和重要性排序优先级，PostgreSQL 普通 / race 回归及 lint 通过并复评。无第三轮后未复评修复。S6 完成，S7 未开始。

- S7 评测框架（2026-11-09）：导出、A、B 三个功能点分别提交；只新增测试与文档，生产代码不改。导出在本地 PostgreSQL 上验证 ID 映射、正文替换与手机号脱敏，负向验证仓库内未忽略路径与非法 case 名被拒；A 用合成 case + HTTP 桩回放真实 `Pipeline.Daily`，验证逐 case 推进 `Now`、话题匹配、覆盖率、要点报告与 token；B 抽出共享宿主 helper，用本地 fake OpenAI 桩驱动真实 devhost，上海时间 04:01 后运行 25 问全部收到回复、宿主正常退出（63.5s），日志解析出 `memory__summary` 调用及 args，窗口外召回 2/4、日期正确 4/4、query 正确 2/4、误调用 1/10、冗余调用 1/7，与桩行为逐项一致；A、B 指标计算补纯函数单测。gofmt / build / vet / go test / `go test -race ./internal/memory/...` 通过（无评测环境变量时相关用例跳过）。导出 review 2 轮、A review 2 轮，最终均 `NO_BLOCKING_ISSUES`；B 及 A 指标修正的 review 见下一条。真实 LLM、真实 case 与 20-case 基线尚未运行。

- S7 实现评审（2026-11-09）：对导出、A、B 整体评审。修正 A 过粗率分母（改为生成话题数）、分母为 0 输出 N/A、多次运行按分子分母累加；B 冗余调用率按 §16.3.9 改为 `any` 问题发生调用的比例；共享宿主 helper 恢复中断后正常退出检查、固定文本日志格式；问题集改为每天不同话题，话题类数据放在窗口外且在默认查询范围内，消除「上周」「三周前」等歧义标注；上海时间 04:00 前或 23:00 后开始时跳过；A、B 命令放宽 `-timeout`；补 `TestComputeEvalMetrics`、`TestEvalToolMetricsRecord`。Nix devShell 中 gofmt / vet / `go test -race ./...` / lint 0 issues / devhost vet 通过，B 用本地桩冒烟通过（见上一条）。review 3 轮：前两轮 `NEEDS_FIX`（问题集与注入窗口、日界、超时；话题数据超出默认查询范围），第 3 轮 `NO_BLOCKING_ISSUES`；之后把「最近群里有什么新鲜事」改为只问昨天以免冗余率偏高，此项未再复评。真实 LLM 评测未运行。

- S7 生产验收（2026-11-09）：开发机 devhost 连生产库运行，11-09 04:00 的 `daily_summary` 用时约 51s（11 个切片）完成，为前一天 287 条消息的会话日生成日摘要（8 个话题），同会话日的滚动摘要被替换，`summarized_upto_id` 推进到当天最后一条消息；同批 `cleanup` 正常完成。
