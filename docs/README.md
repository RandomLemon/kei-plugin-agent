# 设计文档

`kei-plugin-agent` 的设计文档集，是本插件**目标实现口径**的唯一来源。文档用中文编写，章号 1-12 连续编号；跨文档引用统一写成 `文件.md#锚点`。

代码已落地（P1-P6 全部完成）。本目录文档与实现保持一致：行为变更先改文档、再改代码（见 [`roadmap.md`](roadmap.md)）。

## 1. 文档索引

| 文件 | 章号 | 责任 |
| --- | --- | --- |
| [`README.md`](README.md) | — | 文档索引、章节↔文件对应、阅读顺序、文档约定 |
| [`architecture.md`](architecture.md) | 1-6 | 项目定位与边界、架构与数据流、生命周期与并发模型、状态与持久化、目录与文件职责、与 kei 核心契约的对应 |
| [`participation.md`](participation.md) | 7 | 接话决策模型（过滤、寻址、随机参与、批处理窗口、冷却/配额/静默时段、伪代码、reason 词表） |
| [`persona.md`](persona.md) | 8 | 人格系统（预设库、绑定与优先级、运行时覆盖与 `/agent` 命令、提示词模板、历史渲染、回复清洗） |
| [`llm.md`](llm.md) | 9 | LLM 客户端（协议/请求/响应、超时重试与错误、长度与成本、密钥与日志安全、替换服务） |
| [`configuration.md`](configuration.md) | 10 | 全量配置键表（权威）、示例配置、环境变量覆盖、结构与校验规则 |
| [`testing.md`](testing.md) | 11 | 质量门、单元测试矩阵与注入缝、mock 适配器端到端联调、竞态与优雅关闭 |
| [`roadmap.md`](roadmap.md) | 12 | 交付物现状（文档与代码均已落地）+ 实现阶段 P1-P6 |

## 2. 章号↔文件对应

| 文档 | 章号 | 内容 |
| --- | --- | --- |
| `docs/README.md` | — | 文档索引、章节↔文件对应、阅读顺序、文档约定 |
| `docs/architecture.md` | 1-6 | 项目定位与边界、架构与数据流、生命周期与并发模型、状态与持久化、目录与文件职责、与 kei 核心契约的对应 |
| `docs/participation.md` | 7 | 接话决策模型：过滤、寻址、随机参与、批处理窗口、冷却/配额/静默时段 |
| `docs/persona.md` | 8 | 人格系统：预设库、绑定与优先级、运行时覆盖与 `/agent` 命令、提示词模板、历史渲染、回复清洗 |
| `docs/llm.md` | 9 | LLM 客户端：协议/请求/响应、超时重试与错误、长度与成本、密钥与日志安全、替换其他 OpenAI 兼容服务 |
| `docs/configuration.md` | 10 | 全量配置键表（权威）、示例配置、环境变量覆盖、`personas`/`bindings` 结构、校验规则 |
| `docs/testing.md` | 11 | 质量门、单元测试矩阵与注入缝、mock 适配器端到端联调、竞态与优雅关闭 |
| `docs/roadmap.md` | 12 | 交付物现状（文档与代码均已落地）+ 实现阶段 P1-P6 |

本表与 [`../AGENTS.md`](../AGENTS.md) 第 6 节文档索引表逐字一致，改动两处必须同步。

## 3. 建议阅读顺序

架构 → 接话决策 → 人格 → LLM → 配置 → 测试：

1. [`architecture.md`](architecture.md) 第 1-6 节：先建立全局模型（定位、数据流、生命周期、状态、目录、与 kei 的契约）。
2. [`participation.md`](participation.md) 第 7 节：插件最核心的行为——何时接话。
3. [`persona.md`](persona.md) 第 8 节：接话时以什么人格、什么提示词生成。
4. [`llm.md`](llm.md) 第 9 节：提示词如何发给 LLM、如何解析与容错。
5. [`configuration.md`](configuration.md) 第 10 节：上述行为由哪些配置键驱动（权威键表）。
6. [`testing.md`](testing.md) 第 11 节：如何验证以上全部行为。
7. [`roadmap.md`](roadmap.md) 第 12 节：交付物现状与实现阶段划分。

## 4. 文档约定

- **章号连续**：1-12 全局唯一，不分文档重置；节号写成 `章.节`（如 §10.1），跨文档引用统一写成 `文件.md#锚点`。示例：[`architecture.md#3-生命周期与并发模型`](architecture.md#3-生命周期与并发模型)、[`participation.md#76-决策伪代码`](participation.md#76-决策伪代码)、[`participation.md#77-决策日志与-reason-词表`](participation.md#77-决策日志与-reason-词表)、[`persona.md#85-系统提示词模板`](persona.md#85-系统提示词模板)、[`configuration.md#101-全量配置键表`](configuration.md#101-全量配置键表)、[`configuration.md#104-校验规则`](configuration.md#104-校验规则)、[`testing.md#112-单元测试矩阵`](testing.md#112-单元测试矩阵)。
- **签名与配置键取自设计口径**：函数签名、结构体字段、配置键名与默认值全部以本文档集为准；配置键以 [`configuration.md`](configuration.md) 第 10 节为唯一权威，其它文档引用必须与其逐字一致。
- **固定字面量**：提示词模板（[`persona.md`](persona.md) §8.5）、决策伪代码与 reason token（[`participation.md`](participation.md) §7.6-7.7）、配置校验错误文案（[`configuration.md`](configuration.md) §10.4）、`/agent` 输出行（[`persona.md`](persona.md) §8.4）都是逐字约定，实现者不得改写。
- **新增文档必须同步两处索引**：本节第 1、2 节两张表与 [`../AGENTS.md`](../AGENTS.md) 第 6 节。
- **状态口径**：文档描述目标实现；与现状有关的判断只在 [`roadmap.md`](roadmap.md) 记录。
