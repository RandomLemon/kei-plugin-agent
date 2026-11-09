# memory/progress.md — 记忆系统实现进度

本文跟踪记忆系统（[`overview.md`](overview.md) 第 15 章）各阶段的实现状态，是记忆系统**现状判断的唯一记录处**；[`../roadmap.md`](../roadmap.md) §12.1 只指向这里。各层内部的细分进度写在该层的进度文档里，本文只记录阶段级结论。

**更新规则**

- 阶段状态只用三个词：`未开始`、`进行中`、`已完成`。`已完成` 表示该阶段的验收要点全部通过、质量门全绿、文档已同步。
- 某层的进度文档更新后，同步本表对应行的状态与日期。
- 设计变更（改方案、改阶段划分）记入文末「变更记录」。

## 阶段进度

| 阶段 | 内容 | 设计文档 | 细分进度 | 状态 | 完成日期 |
| --- | --- | --- | --- | --- | --- |
| M0 契约 | 总体方案与各层文档、门面契约、作用域规则 | [`overview.md`](overview.md)、[`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) | — | 进行中 | — |
| M1 采集 | 存储与写入、根包接入采集 | [`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) S1–S2 | [`summary-layer/progress.md`](summary-layer/progress.md) | 已完成 | 2026-11-05 |
| M2 摘要层 | 任务框架、日摘要、读取注入、滚动摘要与显式查询、评测 | [`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) S3–S7 | [`summary-layer/progress.md`](summary-layer/progress.md) | 进行中 | — |
| M3 事实层 | 抽取与治理、`memory__save` / `memory__search`；引入 pgvector、pg_trgm 与 embedding | 待编写 | — | 未开始 | — |
| M4 图谱与画像 | 节点与边、实体对齐、`memory__graph`、跨群关联、画像 | 待编写 | — | 未开始 | — |
| M5 治理与运维 | 撤回处理、`/agent memory forget`、其余层清理、成本统计 | 待编写 | — | 未开始 | — |

M0 剩余工作：事实层、图谱层、治理层的设计文档。摘要层方案已定稿，可以先行实现 M1、M2。

## 部署现状

2026-11-09：bot 按 [`deploy.md`](deploy.md)「开发机联调运行」方式在开发机上运行 `cmd/devhost`，连接 kei-deps 上的 SnowLuma 与 PostgreSQL；「正式部署」（systemd 服务 `kei`、`/opt/kei`）尚未进行。kei-deps 上残留 Docker 卷 `kei-gomod`、`kei-gocache`，正式部署时确认是否保留。切换到正式部署前，先停止开发机上的 devhost（Ctrl+C），避免两个宿主同时连同一个 bot 账号。

## 规则与契约落地

记忆系统需要改动的全局规则与跨文档契约，在这里记录是否已经写入：

| 项 | 位置 | 状态 |
| --- | --- | --- |
| 依赖放开：GORM 两个包只允许 `internal/memory` 使用，pgvector-go M3 起使用 | [`../../AGENTS.md`](../../AGENTS.md) §2.2 | 已写入 |
| 工具来源口径：只内置记忆工具，其余经 MCP | [`../mcp.md`](../mcp.md) §13.5 | 已写入 |
| `memory_*` 配置键与校验 | [`../configuration.md`](../configuration.md) §10.1、§10.4 | 随各阶段写入；S2–S6 的采集、调度、保留期、LLM、上下文与记忆工具键和校验已写入 |
| 生命周期、协程表、目录结构 | [`../architecture.md`](../architecture.md) 第 1、3、5 章 | 随各阶段写入；第 5 章目录结构已随 S1 写入，第 1 章、§3.1、§3.2 与根包 `memory.go` 已随 S2 写入 |
| `/agent memory status` 输出行、记忆块格式 | [`../persona.md`](../persona.md) §8.4、§8.5 | 随各阶段写入；S3 的 status 三行与用法文本已写入，S5 的记忆块位置与格式已写入 |
| `go.mod` 引入 GORM | [`../../go.mod`](../../go.mod) | 已引入（S1：`gorm.io/gorm` v1.31.2、`gorm.io/driver/postgres` v1.6.3） |

## 变更记录

| 日期 | 变更 |
| --- | --- |
| 2026-11-05 | 新建本文。摘要层方案定稿：写在插件内 `internal/memory`，用 GORM + PostgreSQL；摘要层不使用向量与数据库扩展，pgvector 与 embedding 推迟到 M3 |
| 2026-11-05 | 摘要层精简：按单实例部署去掉跨进程互斥（迁移锁、调度锁、租约与心跳），改为启动时重置执行中任务；日摘要改为每会话一个任务、按日期升序逐天处理，去掉 `ErrDefer` 与迟到重算；去掉引用上下文、熔断器、`continuity` 打分、`memory_random_rounds` 与 `Layer`；`memory_worker_concurrency` 改为常量；保留滚动摘要 |

| 2026-11-08 | S3–S5 已完成；切片按正文 rune 预算优先，单条上限 2000（含省略号），最多十条重叠；真实会话日与真实模型运行抽验、真实宿主记忆注入及数据库链路故障降级验收通过 |

| 2026-11-08 | S6 已完成摘要日日界、滚动摘要触发与增量合并、日摘要替换、读取去重；显式查询工具继续实现，M2 保持进行中 |

| 2026-11-08 | S6 已完成：滚动摘要、短期窗口去重、显式日期 / 关键词查询与内置工具；Service 滚动生命周期和真实宿主工具调用验收通过，两部分质量门全绿、review 均 2 轮通过。S7 评测未开始，M2 保持进行中 |

| 2026-11-09 | S7 改为持续阶段：评测分为摘要与话题归类（A，门槛为 §16.1 三项，补充过碎、过粗、延续观察指标）和 `memory__summary` 调用时机（B，只观察），补充调优循环；评测数据随采集积累，与 M3 等后续开发并行，首次在 20 个 case 上达标时 M2 完成。实现细节确定：A 回放进隔离 schema 调用真实流水线，评测模型分大小两个；case 用 `store.Message` 字段子集（含 `sender_is_bot`、`persona`），导出时同一会话共用本地 ID 映射表，case 不进公开仓库，`RESULTS.md` 只记指标；回放逐 case 推进 `Now` 调用 `Daily`，测试内多次运行取平均；B 用生产人格、每问独立群号、`bot` 触发词与生产时区。实现约 500 行测试代码，分导出、A、B 三次提交 |
| 2026-11-09 | S7 评测框架完成（导出、A、B 三次提交）：`TestEvalExport`、`TestEvalDaily`、`TestEvalSummaryTool`、共享宿主辅助与 25 条问题集；真实 case、基线与 20-case 达标待积累，M2 保持进行中 |
