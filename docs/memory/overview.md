# memory/overview.md — 记忆系统总体方案（第 15 章）

本文覆盖第 15 章：记忆系统的定位、技术选型与存储评估、整体架构、模块边界、关键数据结构与数据流、错误处理与分阶段迭代。三层中各层的具体实现方案分文档维护，第一层（摘要层）见 [`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) 第 16 章。

记忆分为三层：摘要层、原子事实层、知识图谱层，用户画像挂在图谱的 User 节点上。方案按「QQ / 群聊人格机器人」的规模裁剪，全部使用公开可用的组件（§15.3）。

**文档索引**

本节是记忆系统的文档索引，查记忆相关问题先看这里。[`../README.md`](../README.md) §1 只列本文作为入口，`docs/memory/` 下的其余文档只在这里登记。

记忆系统文档：

| 文件 | 章号 | 内容 |
| --- | --- | --- |
| [`overview.md`](overview.md) | 15 | 本文。总体方案：定位与边界、三层分工、技术选型与存储评估（TableStorage / GORM）、实体对齐与调度、整体架构与模块边界、作用域与数据结构、数据流、错误处理、分阶段迭代、已确认决策 |
| [`progress.md`](progress.md) | — | 记忆系统实现进度：M0–M5 阶段状态、部署现状、全局规则与契约的落地情况、变更记录 |
| [`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) | 16 | 摘要层实现：高层设计（模块、数据流、数据模型、门面接口、运行约束，§16.1–16.2）与 S1–S7 分阶段实现计划（每阶段的详细设计、改动与验收，§16.3） |
| [`summary-layer/progress.md`](summary-layer/progress.md) | — | 摘要层实现进度：S1–S7 阶段状态与完成清单、S7 后续步骤、评测结果摘要、偏差记录、迭代验证记录 |
| [`deploy.md`](deploy.md) | — | 部署方案：依赖容器（SnowLuma + PostgreSQL）、开启记忆的配置、开发机联调运行、正式部署（kei-deps 上编译、sqlite 存储、systemd）、注意事项、验收与排障 |
| 事实层、图谱与画像、治理与运维（M3–M5） | — | 待编写 |

其他文档中的记忆相关章节：

| 主题 | 位置 |
| --- | --- |
| 依赖范围（GORM、pgvector-go 只允许 `internal/memory`） | [`../../AGENTS.md`](../../AGENTS.md) §2.2 |
| 插件功能边界中的记忆部分 | [`../architecture.md`](../architecture.md) 第 1 章 |
| `Setup` / `Start` / `Stop` 中的记忆步骤、记忆协程 | [`../architecture.md`](../architecture.md) §3.1、§3.2 |
| 根包 `memory.go` / `memory_tools.go` 与 `internal/memory/` 入口的文件职责 | [`../architecture.md`](../architecture.md) 第 5 章；包内文件见 [`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) §16.2.1 |
| `memory_*` 配置键、示例、DSN 环境变量、校验规则 | [`../configuration.md`](../configuration.md) §10.1–10.4 |
| `/agent memory` 命令与输出行 | [`../persona.md`](../persona.md) §8.4 |
| 记忆块注入提示词的位置与格式（S5 起） | [`../persona.md`](../persona.md) §8.5 |
| 内置记忆工具 `memory__*` 的边界与人格过滤 | [`../mcp.md`](../mcp.md) §13.3、§13.5、§14.3；本章 §15.4.3 |
| 数据库测试（`PERSONA_MEMORY_TEST_DSN`）、单测矩阵、devhost 联调宿主 | [`../testing.md`](../testing.md) §11.1、§11.2、§11.5（接真实 QQ 与记忆库的部署见本目录 [`deploy.md`](deploy.md)） |
| S7 评测结果（A / B 的指标与改动摘要） | [`../../internal/memory/summary/testdata/eval/RESULTS.md`](../../internal/memory/summary/testdata/eval/RESULTS.md)（运行方式见 [`../testing.md`](../testing.md) §11.1） |
| 插件整体交付状态（记忆部分只指向 `progress.md`） | [`../roadmap.md`](../roadmap.md) §12.1 |

**同步规则**：`docs/memory/` 下新增、改名、删除文档，或其他文档中新增、移动、删除记忆相关章节时，同一次变更内同步本节两张表；[`../README.md`](../README.md) §1 不再登记记忆子文档。

## 15.1 定位与边界

**做什么**

- 让人格机器人记住「最近聊了什么」（摘要层）、「某人 / 某群是怎样的」（原子事实层）、「谁和什么有关」（知识图谱层与用户画像）。
- 生成回复时，把三层记忆按场景组装成一段软上下文注入系统提示词；并提供少量记忆工具供模型按需查询或显式保存。
- 同一会话内各人格共享记忆：记的是人与群，不是人格本身；机器人自己的发言只标注人格名。

**不做什么**

- 不单独部署服务：记忆子系统是本仓库内的 `internal/memory` 包，随 persona 插件一起 `Start` / `Stop`，只额外需要一个 PostgreSQL（§15.3.3）。
- 不跨群暴露内容：跨群关联默认开启，但只返回关联关系，不返回其他群的原文与摘要正文；群管理员可让本群退出共享（§15.6.4）。
- 不做面向用户的隐私功能：不发记忆说明、不提供个人退出或自助查看 / 删除。要让某个群或某人的私聊完全不被记忆，用名单策略的黑名单排除（§15.6.1）。
- 不记录敏感属性：健康、政治、宗教、性取向、精确住址、证件号、联系方式等一律不进入任何一层。

## 15.2 三层分工

| 层次 | 回答的问题 | 组织维度 | 粒度 | 生命周期 |
| --- | --- | --- | --- | --- |
| 摘要层 | 最近一段时间聊了什么、重点是什么 | 时间（会话 × 天） | 每会话每天一份，按话题拆分 | 短中期，默认保留 90 天 |
| 原子事实层 | 某个人、某个群是怎样的，做过什么 | 主体（人 / 群） | 一条只说一件事，单独可读 | 长期，由有效期管理 |
| 知识图谱层 | 谁和什么有关，关联有多广 | 关系 | 节点 + 边 | 长期，边权重衰减 |

用户画像挂在图谱的 User 节点上：节点属性 + 出边共同构成画像。

作用域分四种：`user_global`（跨场景可迁移的个人信息）、`user_private`（与机器人的私聊）、`member@group`（某人在某群内的信息）、`group`（群整体）。ID 规则见 §15.5.1。

## 15.3 技术选型

### 15.3.1 选用的技术

| 组件 | 选型 | 用途 |
| --- | --- | --- |
| 运行位置 | 插件内 `internal/memory` 包，Go 1.25，随插件启停 | 不单独部署服务、不走 HTTP |
| 数据库 | PostgreSQL 17；M3 起加扩展 `vector`（pgvector）与 `pg_trgm` | 原始消息、摘要、事实、图谱、任务队列全部放在一个库。摘要层（M1–M2）不需要任何扩展 |
| 数据访问 | GORM：`gorm.io/gorm`（与 kei 依赖的 v1.31.x 对齐）+ `gorm.io/driver/postgres` | 模型、建表、事务、upsert、行锁；向量距离与相似度查询用 `Raw` / `Exec` |
| 向量类型 | `github.com/pgvector/pgvector-go`（M3 起） | GORM 模型中的 `vector` 字段 |
| 检索 | 摘要层：候选话题很少，取出后在 Go 中按关键词（汉字 bigram）重叠打分。M3 起：pgvector HNSW（余弦距离）+ pg_trgm 关键词相似度，两路合并 | embedding 不可用时只走关键词路 |
| 图谱 | `mem_node` / `mem_edge` 两张表 + 固定查询模板（≤ 2 跳，JOIN / 递归 CTE） | 不引入图数据库 |
| 实体对齐 | 名称规范化 + 别名表 `mem_entity_alias` + 同类型向量近邻 | 见 §15.3.4 |
| 任务队列 | `mem_job` 表 + `FOR UPDATE SKIP LOCKED` 领取；启动时把遗留的「执行中」任务退回待执行 | 一个库（或 schema）只由一个 kei 进程使用，不做跨进程互斥；进程内多个 worker 靠行锁互不重复领取 |
| 调度 | 插件内调度协程每 60s 一次 tick，任务去重键保证同一任务只建一次 | 见 §15.3.5 |
| LLM | OpenAI 兼容 chat completions（`net/http`），大小模型分开配置，默认回落到插件的 `llm_*` | 切片小结、合并、事实抽取 |
| Embedding | OpenAI 兼容 `/embeddings`（Ollama、vLLM、TEI 或云服务；推荐 bge-m3 一类中文友好模型），M3 起 | 事实、实体的向量 |
| 身份 | 带平台前缀的字符串 ID（§15.5.1），一律视为字符串 | 作用域与节点 ID |
| 群成员关系 | 由发言推导：发过言即视为成员，记 `first_seen` / `last_seen` | 不依赖平台通讯录 |
| User 基础属性 | 平台用户 ID + 昵称（会变） | 职责类信息只来自本人陈述的事实 |
| 记忆工具 | 插件内置 `memory__*`（插件唯一的内置工具，其余工具仍经 MCP） | 见 §15.4.3 |

### 15.3.2 为什么不用 `bot.TableStorage`

kei 给插件提供了表级存储 `PluginContext.Tables`（`bot.TableStorage`，见 kei `docs/storage.md`）。它的模型是「表名 → 行键 → 字节串」，只有建表 / 删表与按行键的增删改查、按行键升序整表枚举。逐项对照记忆的需求：

| 需求 | TableStorage 能提供的 | 结论 |
| --- | --- | --- |
| 向量检索 | 无向量类型与索引；只能 `ListRows` 取出整表，在 Go 里逐条算余弦 | 每次读取都要全量加载，话题 / 事实一多就不可用 |
| 图查询（≤ 2 跳） | 无二级索引，只能按行键取；邻接关系要自己再维护几张「索引表」 | 节点表与索引表之间没有事务，一致性难保证 |
| 按会话 + 时间取消息 | 无范围查询、无分页 | 只能按「会话 × 天」拆表；表名上限 191 字节且含 `plugin:persona:` 前缀，长会话键还要哈希 |
| 任务队列 | 无事务、无 CAS、无行锁 | 做不了「领取」，多个 worker 无法互斥 |
| 持久性 | 宿主默认 `memory` 后端重启即丢；SQL 后端只有 SQLite / MySQL，都没有向量能力 | 依赖宿主额外配置，仍然缺向量 |
| 保留期清理 | 表数据没有 TTL | 需要定期整表扫描 |

结论：TableStorage 适合少量配置与状态（插件现在的会话覆盖就属于这一类），不适合做记忆的主存储。kei 也不向插件暴露它内部的 `*gorm.DB`，所以记忆使用独立的 PostgreSQL 连接（`memory_database_dsn`）。

### 15.3.3 为什么用 GORM 直连 PostgreSQL、写在插件内

**GORM 能满足需求。** `gorm.io/driver/postgres` 底层就是 pgx，所需能力都有现成写法：

| 需求 | GORM 写法 |
| --- | --- |
| 建表与加列 | `AutoMigrate`；扩展、向量列、HNSW / trgm 索引用 `Exec` 执行幂等 DDL |
| 幂等写入 | `Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(...)` |
| 事务 | `db.Transaction(func(tx *gorm.DB) error { ... })` |
| 任务领取 | `Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})` |
| 向量字段 | pgvector-go 的 `pgvector.Vector` 实现了 `Scanner` / `Valuer`，可直接作 GORM 字段 |
| 向量排序、相似度 | `Raw` / `Exec`，如 `ORDER BY embedding <=> ?`、`similarity(...)` |
| 数组 / JSON 字段 | GORM 内置的 `serializer:json` 存成 `jsonb`，不再引入其他依赖 |

kei 自己的 SQL 存储后端也用 GORM，两边写法一致。

**写在插件内的好处**：

- 不用单独部署和运维一个服务，也不用设计 HTTP 接口、鉴权与租户。
- 插件构造的 `scene` 以 Go 参数直接传给 `internal/memory`，身份天然可信。

**代价与应对**：

1. **依赖规则需要放开。** [`../../AGENTS.md`](../../AGENTS.md) §2.2 已改为：`internal/memory` 及其子包可以使用 `gorm.io/gorm`、`gorm.io/driver/postgres`、`github.com/pgvector/pgvector-go`，其余代码仍然只依赖标准库与 kei。M1–M2 只用前两个，pgvector-go 从 M3 起引入。宿主二进制只要导入本插件就会链接这些依赖（即使未启用记忆），这一点接受。
2. **后台任务与聊天跑在同一进程。** 记忆的后台任务有独立的并发上限（worker 数，常量），不占用聊天的 `limits_max_concurrent`。
3. **`Stop` 必须有界。** 正在执行的长任务随 ctx 取消中断，留在「执行中」，下次启动时退回待执行；流水线幂等，重跑即可（§15.7）。

### 15.3.4 实体对齐

群聊里同一个东西常有多种叫法，比如「K8s」「k8s」「Kubernetes」「库伯」。**实体对齐**就是判断这些名字指的是不是同一个东西，并把它们归到同一个图谱节点上。

没有实体对齐会出现三个问题：

- **查询漏人**：图谱里有好几个节点。问「谁懂 Kubernetes」时，只能查到用了这个写法的人。
- **跨群关联失效**：A 群聊「K8s 升级」、B 群聊「Kubernetes 升级」，系统发现不了两个群在关注同一件事。
- **事实无法去重或判冲突**：关于同一实体的事实分散在不同节点上。

做法（M4 实现）：

1. **规范化**：名称经 NFKC、转小写、去掉空白和常见标点，得到 `entity:<type>:<规范化名>`。
2. **查别名表**：`mem_entity_alias` 记录「规范化名 → 实体 ID」，命中就直接用。
3. **向量近邻**：没命中时，在同类型的实体里做向量近邻搜索。相似度 > 0.9 视为同一实体，同时登记一条新别名。
4. **新建**：以上都不满足时，新建实体。

人不参与名字对齐：User 节点一律以平台用户 ID 为准，因为昵称会变，而且同名的可能是不同的人。

### 15.3.5 调度

调度协程只负责「什么时候产生后台任务」，任务本身由 worker 协程执行。每次 tick 做这些事：

- **每日任务链**：每天 `memory_daily_at`（默认 04:00，按 `random_timezone`，与 `{{now}}` 渲染一致）之后，为有未摘要日期的会话创建任务链（日摘要 → 事实抽取 → 图谱 → 画像）。该时刻也是摘要日的日界：当天该时刻至次日该时刻的消息算同一天，到点时前一个摘要日刚好结束（[`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) §16.2.3）。日摘要任务按日期升序补齐最近 7 天内漏跑的日期。
- **全局任务**：每天一次，清理过期数据，并重算边的衰减分数（M4）。

不经过调度的任务：滚动摘要在写入消息时按条数触发（[`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) §16.3.8）；`memory__save` 在工具调用时直接建任务。

## 15.4 整体架构

### 15.4.1 架构图

```text
kei 进程
└─ persona 插件
   ├─ 根包 persona（现有代码 + 少量接入）
   │   ├─ Handler：过滤 → 记历史 → 判决策；+ mem.Enqueue        非阻塞，有界队列，满则丢弃并计数
   │   ├─ generate（后台）
   │   │    ├─ + mem.Context                                     带超时，失败即不注入
   │   │    ├─ 系统提示词追加「记忆块」
   │   │    └─ 工具循环：MCP 工具 + memory__* 工具
   │   ├─ 发送成功后：自身回复 mem.Enqueue（self=true）
   │   └─ 命令：/agent memory …（管理员）
   │              │ 进程内 Go 方法调用
   │              ▼
   └─ internal/memory（记忆子系统；Start 启动，Stop 有界停止）
       ├─ 写协程：攒批写入原始消息
       ├─ 调度协程：每 60s tick（§15.3.5）
       ├─ worker 协程（固定数量）：
       │     daily_summary → fact_extract → graph_upsert → profile_refresh
       │     realtime_save；rolling_summary；cleanup / decay
       ├─ 读取：Context / QuerySummaries / …
       └─ GORM ──▶ PostgreSQL 17（M3 起 + pgvector、pg_trgm）
          net/http ──▶ LLM / Embedding（OpenAI 兼容；Embedding M3 起）
```

### 15.4.2 模块边界

**根包 `persona`** 只做三件事：**采集、取上下文、注入身份**。`memory_database_dsn` 为空时整个功能关闭，行为与现状逐字一致。

| 位置 | 改动 |
| --- | --- |
| 新增 `memory.go` | 把 `bot.Event` / `channelState` 转成 `memory.Record` / `memory.Scene`；取上下文的超时；记忆块渲染 |
| 新增 `memory_tools.go` | 实现既有 `tool` 接口的 `memory__*` 工具 |
| `decision.go` | `handleChat` 记历史后入队；`generate` 构造请求前取记忆上下文、发送成功后自身回复入队 |
| `persona.go` | `renderSystemPrompt` 在模板渲染结果与「本轮要求块」之间追加记忆块（不新增占位符，自定义模板无需改动） |
| `tools.go` | 工具目录 = MCP 工具 + memory 工具；人格 `tools` 名单校验放开 `memory__` 前缀（规则见 [`../mcp.md`](../mcp.md) §14.3） |
| `plugin.go` | `Setup` 构造记忆子系统（不连库）；`Start` 启动它；`Stop` 先停记忆子系统、再取消插件级 ctx |
| `commands.go` | 只扩展管理员命令 `/agent memory …`（`status`，M4 起增加 `share on\|off`）；不新增面向普通用户的命令 |
| `config.go` | 新增 `memory_*` 配置键，组装成 `memory.Config` |

**`internal/memory`** 承担全部记忆逻辑：

| 包 | 职责 |
| --- | --- |
| `internal/memory` | 门面 `Service`（生命周期、入队、读取、统计）、写协程、`scene` → 可见作用域推导 |
| `internal/memory/store` | GORM 模型、迁移、查询 |
| `internal/memory/jobs` | 任务创建 / 领取 / 重试 / 启动时回收、调度 tick、worker 池 |
| `internal/memory/llm` | OpenAI 兼容 chat（JSON 输出与修复重试）；M3 起加 embeddings 客户端 |
| `internal/memory/summary` | 切片、预过滤、话题摘要与合并、话题打分（第 16 章） |
| `internal/memory/facts` | 事实抽取、治理（create/update/skip/conflict/supersede）、按类型衰减召回（M3） |
| `internal/memory/graph` | 实体对齐、节点与边 upsert、边衰减、固定查询模板、跨群关联（M4） |
| `internal/memory/profile` | 画像刷新、`profile_summary` 生成与兜底（M4） |

依赖方向：

- 只能是「根包 → `internal/memory` → 子包」。`internal/memory` 不 import 根包，也不 import kei，与 `bot.Event` 的转换全部在根包 `memory.go` 中完成，这样子系统可以脱离 kei 单测。
- 根包只经 `Service` 的方法调用，接触不到 `*gorm.DB`。

运行期约束与插件其余部分一致（[`../../AGENTS.md`](../../AGENTS.md) §2.3）：

- 所有阻塞操作都带 `context.Context`。
- 所有协程从插件级 `p.ctx` 派生，都能被 `Stop` 终止。
- 共享状态挂在 `Service` 实例上，不使用包级可变状态。

### 15.4.3 记忆工具的来源

记忆查询的可见范围取决于「当前是哪个会话、谁在提问」，这个身份不能由模型提供。本方案的落地方式是：**插件根据事件构造 `scene`（会话、提问人、被 @ 的人），以参数形式交给 `internal/memory`，由它推导可见作用域；工具 schema 中不出现任何身份字段。**

因此记忆工具由插件内置（实现 `tool` 接口），而不是经 MCP 提供：MCP 调用只有模型给出的参数，带不上插件注入的可信身份。

工具来源的口径为「**只内置记忆工具，其余全部经 MCP**」，已写入 [`../mcp.md`](../mcp.md) §13.5、§14.3、§14.6：

| 方面 | 规则 |
| --- | --- |
| 范围 | 内置工具只有 `memory__*`；知识库、天气等其余能力一律经 MCP，不恢复 P8 删除的内置注册表 |
| 装配 | 不设注册表；每次生成按会话构造记忆工具实例（持有本会话 `scene`），排在 MCP 快照之后 |
| 命名 | `memory__` 前缀保留给内置工具，MCP 工具一律为 `mcp__<远端名>`，两者不会冲突 |
| 人格过滤 | 三态规则不变：不写 `tools` 时继承 MCP 工具与已启用的记忆工具；名单中的 `memory__` 名称要求记忆工具已启用 |
| 目录刷新 | `/agent tools` 同时列出两类工具；`/agent tools reload` 只刷新 MCP，不影响记忆工具 |
| 循环 | 共用 `completeWithTools` / `runTool`，超时、截断、日志 status 词表都不变 |

记忆工具默认关闭（`memory_tools_enabled=false`），因为它要求 LLM 服务支持 tool calling，而未配置 MCP 的部署不一定满足。

## 15.5 关键数据结构

### 15.5.1 身份与作用域 ID

| 对象 | ID |
| --- | --- |
| 群会话 / `group` 作用域 | `group:<platform>:<channel_id>` |
| 私聊会话 / `user_private` 作用域 | `private:<platform>:<bot_id>:<user_id>` |
| `member@group` 作用域 | `member:<platform>:<channel_id>:<user_id>` |
| `user_global` 作用域 / User 节点 | `user:<platform>:<user_id>` |

群会话 ID 不含 `bot_id`：同一群内的多个机器人共享该群的群级记忆；私聊含 `bot_id`，因为私聊对象是具体机器人。

**可见性（由 `internal/memory` 根据 `scene` 推导）**

- 群 X 内用户 U 提问：`group(X)`、`member(X, U)`、`member(X, 被 @ 的人)`，以及 U 的 `user_global` 中 `portable=true` 的部分。
- 与 U 私聊：`user_private(U)`、`user_global(U)`。
- 永远不可见：他人的私聊；其他群的原文与摘要正文。

### 15.5.2 门面接口概览

根包只通过 `internal/memory.Service` 的方法使用记忆：

| 方法 | 用途 | 引入阶段 |
| --- | --- | --- |
| `Start(ctx)` / `Stop()` / `Stats()` | 生命周期与 `/agent memory status` 用的内存计数 | M1 |
| `Enqueue(Record) bool` | 非阻塞入队一条原始消息 | M1 |
| `Context(ctx, ContextRequest)` | 主读取入口，返回按场景组装好的记忆块 | M2 起逐层扩展 |
| `QuerySummaries(ctx, SummaryQuery)` | 按日期范围或语义查询摘要 | M2 |
| `SearchFacts(ctx, FactQuery)` | 召回原子事实 | M3 |
| `Save(ctx, SaveRequest)` | 显式保存（实时通道）：建任务后立即返回 | M3 |
| `QueryGraph(ctx, GraphQuery)` | 固定模板图查询（`who_knows`、`groups_focusing`、`user_recent`、`related_entities`） | M4 |
| `SetShare(ctx, conv, enabled)` | 设置某个群是否参与跨群关联 | M4 |
| `Forget(ctx, ForgetRequest)` | 撤回消息的级联处理；删除指定事实 | M5 |

所有读写都带插件构造的 `Scene`；模型只能提供查询词、日期这类参数。字段级定义随各层文档给出（摘要层见 [`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) §16.2.4 与 §16.3 各阶段）。

### 15.5.3 主要的表

| 表 | 关键字段 | 保留期 | 阶段 |
| --- | --- | --- | --- |
| `mem_conversation` | conv_key、type、platform、bot_id、channel_id、peer_user_id、name、share_enabled（默认 true，M4 起生效） | 长期 | M1 |
| `mem_message`（原始消息） | conv_key、dedupe_key（唯一）、msg_id、sender、self、text、reply_to、mentions、sent_at、recalled | 60 天（可配） | M1 |
| `mem_conv_day` | 每会话每天的消息计数与摘要进度 | 随摘要 | M1 |
| `mem_job` | 任务类型、状态、重试 | 完成后 14 天 | M2 |
| `mem_summary` / `mem_topic` | 日摘要与滚动摘要及其话题 | 90 天（可配） | M2 |
| `mem_fact` | 主体、作用域、类型（偏好 / 经历 / 职责 / 决议…）、内容、来源消息 ID、置信度、有效期、状态、`embedding` | 由有效期管理 | M3 |
| `mem_node` / `mem_edge` / `mem_entity_alias` | 节点（含画像）、边（主键 src+rel+dst+scope，带 evidence 与衰减分数）、实体别名 | 长期 | M4 |

## 15.6 关键数据流

### 15.6.1 采集

- 采集口径与「进历史」一致：通过名单策略、非命令消息才入队；被名单拒绝的会话不建状态、不进历史、不采集。
- `/agent off` 只关闭发言，不停止采集。会话状态是懒加载的，入队时不一定知道该会话是否已关闭，因此采集与「进历史」使用同一口径。
- **排除某个群用黑名单**：`group_policy: blacklist`，并把群 ID 加入 `group_list`（运行期用 `/agent list group add <id>`）。私聊同理，用 `private_policy` / `private_list`。被黑名单拒绝的会话既不回复也不采集（[`../participation.md`](../participation.md) §7.8）。不另设记忆专用的排除名单。
- 机器人自身的回复也入队（带人格名），使摘要不缺上下文；但不从机器人发言中抽取关于用户的事实。

### 15.6.2 写入：每日任务链（按会话）

1. **生成日摘要**：切片 → 预过滤 → 切片小结 → 合并与延续判断 → 写入（第 16 章）。
2. **抽取并治理原子事实**，输入是**原始消息，不是摘要**：
   - **用日摘要做导航**：只处理含 key point 或重要性达到阈值的话题。把该话题覆盖的原始消息（预过滤后）连同话题标题与摘要一起交给 LLM，闲聊切片不抽取。
   - **为什么不从摘要抽**：摘要是压缩后的转述，丢失了「谁说的」与原话细节，还可能带入总结偏差。事实必须逐条对应到原始消息（`source_msg_ids`），才能做说话人校验、冲突判定和撤回级联。
   - **治理流程**：候选事实 → 确认说话人与主体 → 召回同作用域、同主体的旧事实 → LLM 判定 create / update / skip / conflict / supersede → 写入。
   - 机器人发言只作上下文，不作为事实来源。
3. **更新图谱**：从刚写入的事实中提取实体与关系，经实体对齐（§15.3.4）后 upsert 节点与边；每条边的 evidence 指向事实 ID。
4. **刷新受影响用户的画像**。

每步成功后才建下一步任务，所以中途失败只重试失败的那一步。全局任务单独执行：重算边衰减分数、清理过期数据。

**实时通道**：寻址轮次中模型调用 `memory__save(content, reason)`，插件调用 `Service.Save` 建 `realtime_save` 任务后立即返回，任务直接走第 2、3 步。证据为本轮提问人的原始消息，`content` 只作为候选事实。「记住……」、明确纠正、群决议都走这条路，Handler 内不做关键词识别。

### 15.6.3 读取：上下文组装

`Service.Context` 内并行获取各层，每一层失败只降级为空，返回结构化的记忆块；根包负责渲染与长度兜底：

```text
# 记忆（仅供参考，可能过时；与当前对话冲突时以当前对话为准）
[近期重点] …
[相关事实] …
[提问人画像] …
[关联信息] …
```

- 短期上下文（最近 N 条原始消息）由插件内存中的 `history` 提供，记忆不重复返回。
- 冲突优先级：当前输入 > 实时工具 > 群明确决议 > 本人陈述 > 第三方陈述 / 系统推断。
- 只在真正进入 `generate` 时取上下文。摘要层只查本地库，随机轮次照常注入；随机轮次是否跳过开销更大的层（如需要 embedding 的事实检索）在 M3 再定。

### 15.6.4 跨群共享

- **默认开启**：所有群都参与跨群关联（随 M4 图谱层生效），不需要额外操作。
- 只返回关联关系（哪些群在关注、谁负责），不返回其他群的原文与摘要正文；私聊内容永远不参与共享。
- 有需要时，群管理员执行 `/agent memory share off` 让本群退出：该群不再出现在其他群的关联结果里，也看不到其他群的关联；`/agent memory share on` 恢复。开关记在 `mem_conversation.share_enabled`，经 `Service.SetShare` 设置。

### 15.6.5 删除与溯源

整条链是「消息 → 摘要 → 事实 → 边 / 画像」，每层都记录上游 ID：

- **删除错误事实**：M5 提供管理员命令 `/agent memory forget <fact_id>`，不面向普通用户。事实删除后，从相关边的 evidence 中移除；evidence 为空的边随之删除，并触发画像刷新。
- **撤回消息**：若适配器上报撤回通知（`bot.EventNotice`），插件调用 `Service.Forget`：
  - 已进入摘要的，标记为撤回并重算当日摘要；
  - 只以该消息为来源的事实同时失效。

## 15.7 错误处理

总原则：**记忆是增强能力，任何故障都不得影响聊天，也不得破坏现有的 Handler 非阻塞、生命周期与并发契约。**

| 场景 | 处理 |
| --- | --- |
| 入队时队列满 | 丢弃本条并计数，`/agent memory status` 可见；Handler 不阻塞 |
| 启动时数据库不可达 | `Start` 不阻塞：后台按退避（1s 起，最长 60s）重连，连上并完成迁移后才启动写协程、调度与 worker。在此之前入队照常（满则丢），读取直接返回「未就绪」，本轮不注入 |
| 迁移失败（缺扩展、权限不足等） | 记 `Error`，记忆保持不可用直到重启；聊天不受影响 |
| 批量写入失败 | 重试 2 次（退避），之后丢弃本批并记 `Warn`；写入按去重键幂等，重试安全 |
| 取上下文失败 | 超时或出错时本轮不注入，记日志；数据库断开时直接返回「未就绪」，不等超时 |
| 记忆工具出错 | 按既有工具契约返回失败文本，不返回 error |
| `Stop` | 关闭准入 → 有界 flush 队列 → 取消 worker ctx，被中断的任务留在「执行中」，下次启动时退回待执行 → 关闭连接池；每步都有超时，整体有界 |
| 任务失败 | 指数退避重试，超过上限标记 `dead`，`/agent memory status` 可见；不推进下一步 |
| LLM 输出不合规 | 解码后逐条校验，不通过用修复提示重试一次；仍失败只放弃该切片 / 该候选，不影响同批其余部分 |
| embedding 不可用（M3 起） | 先写库、向量留空并建回填任务；检索退化为关键词 |
| 画像生成失败或超长 | 保留旧版本 |
| 重复执行 | 各层写入都有唯一键，重跑覆盖、不产生重复数据 |

## 15.8 分阶段迭代

每个阶段都满足：`memory_database_dsn` 为空时行为不变；质量门全绿（[`../testing.md`](../testing.md) §11.1）；可独立上线。

| 阶段 | 内容 | 验收要点 | 预估（1 人） |
| --- | --- | --- | --- |
| M0 契约 | 本章与各层文档、门面契约、作用域规则、配置键 | 文档评审通过 | 2–3 天 |
| M1 采集 | `internal/memory` 骨架、GORM 模型与迁移、写协程与去重、连接重试；根包入队与生命周期 | 消息落库且重复上报不重复写；数据库宕机聊天不受影响；`Stop` 有界并能 flush | 1 周 |
| M2 摘要层 | 任务队列与调度、摘要层的保留期清理、日摘要与滚动摘要、`Context` 返回近期重点、插件注入、`memory__summary`、质量评测 | 机器人能接上「这几天聊过什么」；评测达到 [`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) §16.1 的标准 | 2–2.5 周 |
| M3 事实层 | 抽取与治理、`memory__save` / `memory__search`、按类型衰减、作用域隔离、敏感过滤 | 事实准确率与归属错误率达标；越权用例全部拦截 | 1.5–2 周 |
| M4 图谱与画像 | 节点与边、实体对齐、衰减任务、固定模板与 `memory__graph`、跨群关联（默认开启）与 `/agent memory share on\|off`、画像刷新与注入 | 「该问谁 / 某人最近在忙什么」可答；退出共享的群不出现在关联结果中；删除可级联 | 1.5–2 周 |
| M5 治理与运维 | 撤回处理、`/agent memory forget`、事实层与图谱的保留期清理、成本统计、评测回归 | 删除溯源用例通过；每日 token 成本可见 | 1 周 |

合计约 7–8.5 周。M1、M2 拆成 S1–S7 七个阶段，每阶段的设计、改动与验收见 [`summary-layer/summary-layer.md`](summary-layer/summary-layer.md) §16.3。各阶段的实现状态见 [`progress.md`](progress.md)。建议 M2 结束先上线观察：只有摘要层也能明显改善「记不住前文」，事实层与图谱的抽取也最好用真实数据调参。

## 15.9 已确认决策

| # | 决策 | 结论 |
| --- | --- | --- |
| 1 | 记忆工具来源 | 插件内置 `memory__*`，只内置记忆工具，其余工具全部经 MCP（§15.4.3、[`../mcp.md`](../mcp.md) §13.5） |
| 2 | 实现形态 | 不单独起服务；用 Go 写在本仓库 `internal/memory`，随插件启停（§15.3.3） |
| 3 | 存储 | GORM + PostgreSQL（M3 起加 pgvector、pg_trgm），不用 `bot.TableStorage`（§15.3.2） |
| 4 | 跨群共享 | 默认开启，只共享关联关系；群管理员可 `/agent memory share off` 退出（§15.6.4） |
| 5 | 隐私告知与个人退出 | 不做：不发说明、无 `/memory` 命令、无个人设置表；排除会话用黑名单（§15.6.1） |
| 6 | 多人格是否共享记忆 | 共享；机器人发言只标注人格名（§15.1） |
| 7 | `/agent off` 是否停止采集 | 不停止；要排除某个群就把它加入黑名单（§15.6.1） |
