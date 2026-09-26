# AGENTS.md

`kei-plugin-agent` 仓库的全局硬性规则与结构概览。**具体设计、接口契约、实现方式不在这里**：按领域拆到 [`docs/`](docs/README.md)，见第 6 节文档索引。改动某领域前，先读该领域文档与既有实现。

## 1. 项目定位

- 项目名 `kei-plugin-agent`，模块路径 `github.com/RandomLemon/kei-plugin-agent`，根包 `agent`。
- 目标：给 [`kei`](https://github.com/RandomLemon/kei) 提供「LLM 人格代理」插件——在群聊里按人格预设偶尔插话，像群里一个普通真人。
- 接入形态：进程内插件。`init()` 调 `bot.RegisterPlugin(&Plugin{})`，宿主空导入 `import _ "github.com/RandomLemon/kei-plugin-agent"` + 配置 `plugins.agent.enabled: true` 即启用；kei 核心零改动。
- 非目标：不做平台协议（适配器职责）、不做私聊参与、不引入 LLM SDK、不做持久化数据库。
- 现状：设计文档与 P1-P5 实现均已落地；代码与文档保持一致，行为变更先改文档再改代码，逐项状态见 [`docs/roadmap.md`](docs/roadmap.md)。

## 2. 硬性规则

### 2.1 分层与依赖

- 只依赖 `github.com/RandomLemon/kei/pkg/bot`（唯一公开 SDK）与 `github.com/RandomLemon/kei/pkg/message`（消息段构建器）；禁止 import `github.com/RandomLemon/kei/internal/...`。
- 禁止任何平台名分支（`switch platform`、`if platform == "feishu"` 等）：本插件只处理 `bot.MessageGroup`，与具体平台无关。
- 配置与密钥只能经 `PluginContext.Config` 读取；禁止直接读环境变量、配置文件或 `os.Getenv`。
- `init()` 只注册插件，禁止在 `init()` 内读取配置或建立依赖；所有运行期依赖只能在 `Setup`/`Start` 经 `bot.PluginContextFrom(ctx)` 取得。
- 禁止用 Go `plugin` 包；扩展走进程内注册。

### 2.2 技术栈与依赖

- Go 1.25+（`go.mod` 为 `go 1.25.0`）。
- 优先标准库：`context`、`log/slog`、`net/http`、`encoding/json`、`sync`、`math/rand`、`time`、`strings`、`unicode/utf8`。
- 零第三方运行时依赖，LLM 调用用纯 `net/http`；不引入任何 LLM SDK。
- 唯一非标准库依赖是 `github.com/RandomLemon/kei`（提供 `pkg/bot`、`pkg/message`）。
- 本地开发用 `replace github.com/RandomLemon/kei => ../kei` 指向本地检出（kei 无 release tag，见 [`docs/architecture.md`](docs/architecture.md) 第 6 节）。

### 2.3 运行时契约

- 所有阻塞操作必须带 `context.Context`，不得存在无超时等待。
- **Handler 必须非阻塞**：禁止在 Handler 内做 LLM 或网络调用或任何阻塞等待；只允许记历史、判决策、布防定时器，毫秒级返回。理由见 [`docs/architecture.md`](docs/architecture.md) 第 3 节。
- Handler 的 `ctx` 会在规则超时（`RuleTimeout`，默认 10s）后取消，**禁止用它派生异步链路**；异步工作一律用 `Start` 阶段保存的插件级 ctx 派生，禁止 `context.Background()`。
- 所有 goroutine 必须有退出机制，必须能被 `Stop` 终止，禁止泄漏。
- 所有共享状态挂在 `Plugin` 实例上，禁止包级可变状态；`map` 并发访问必须加锁。
- 错误必须返回，禁止用 panic 表达运行期失败（仅启动阶段不可恢复错误可返回 error 阻止启动）。
- 时间统一 UTC；展示与 `{{now}}` 渲染时按 `random_timezone` 转换。

## 3. 质量门（合并前必须全绿）

```bash
gofmt -l .          # 必须无输出
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

- 公开符号必须有文档注释；注释与实现不符视为缺陷。
- 行为变更（配置键、提示词模板、决策参数、日志字段）必须同步更新 `docs/` 对应文档。

质量门的完整口径、单元测试矩阵与端到端联调步骤见 [`docs/testing.md`](docs/testing.md) 第 11 节。

## 4. 代码约定

1. 包名小写、简短，不使用下划线。
2. 公开符号必须有文档注释。
3. 所有错误必须返回，不得 panic，除非是启动阶段不可恢复错误。
4. 所有 goroutine 必须有退出机制，避免泄漏。
5. 所有 `map` 并发访问必须加锁。
6. 所有时间使用 `time.Time`，时区统一 UTC。
7. 所有 ID 使用字符串，不假设是数字。
8. 日志使用 `log/slog`，字段化输出；键名与值风格沿用 kei 核心（小写、下划线）。
9. 测试使用标准库 `testing`，不引入第三方 mock 库。
10. 禁止在插件中引入 `internal/` 包。

## 5. 项目结构

```text
kei-plugin-agent/
├── go.mod           module github.com/RandomLemon/kei-plugin-agent（require kei）
├── register.go      init() 注册 + Metadata（插件名 agent、权限声明）
├── plugin.go        Plugin 结构、Setup/Start/Stop、runtime 装配
├── config.go        配置结构、读取与默认值、校验（loadConfig）
├── persona.go       人格预设解析、绑定匹配、提示词模板渲染
├── decision.go      接话决策：过滤、寻址判定、随机参与、批处理定时器
├── state.go         每会话状态、历史环、计数器、Storage 读写
├── llm.go           OpenAI 兼容客户端（请求/响应/重试/超时）
├── commands.go      /agent 管理命令
├── config_test.go / decision_test.go / llm_test.go / persona_test.go
├── helpers_test.go  测试桩与测试环境构造（fake BotAPI/Storage/Registrar、日志捕获）
├── e2e_test.go      Mock 适配器端到端测试
├── docs/            设计文档（本目录即实现口径）
├── AGENTS.md        硬性规则与结构概览
└── LICENSE          MIT
```

目录与每个文件的职责逐项说明见 [`docs/architecture.md`](docs/architecture.md) 第 5 节。

## 6. 文档索引

| 文档 | 章号 | 内容 |
| --- | --- | --- |
| `docs/README.md` | — | 文档索引、章节↔文件对应、阅读顺序、文档约定 |
| `docs/architecture.md` | 1-6 | 项目定位与边界、架构与数据流、生命周期与并发模型、状态与持久化、目录与文件职责、与 kei 核心契约的对应 |
| `docs/participation.md` | 7 | 接话决策模型：过滤、寻址、随机参与、批处理窗口、冷却/配额/静默时段 |
| `docs/persona.md` | 8 | 人格系统：预设库、绑定与优先级、运行时覆盖与 `/agent` 命令、提示词模板、历史渲染、回复清洗 |
| `docs/llm.md` | 9 | LLM 客户端：协议/请求/响应、超时重试与错误、长度与成本、密钥与日志安全、替换其他 OpenAI 兼容服务 |
| `docs/configuration.md` | 10 | 全量配置键表（权威）、示例配置、环境变量覆盖、`personas`/`bindings` 结构、校验规则 |
| `docs/testing.md` | 11 | 质量门、单元测试矩阵与注入缝、mock 适配器端到端联调、竞态与优雅关闭 |
| `docs/roadmap.md` | 12 | 交付物现状（文档与代码均已落地）+ 实现阶段 P1-P5 |

`docs/README.md` 第 2 节含与本表逐字一致的「章号↔文件对应」表。

## 7. 工作流

- 改代码前先读对应领域文档与既有实现，沿用仓库既有模式；禁止并存第二套约定。
- 修改导出符号前先查全部调用点并迁移所有调用方，不留兼容垫片。
- 设计歧义以 `docs/` 为准；文档未覆盖时取最简单、可测试的方案。
- 配置键与默认值以 [`docs/configuration.md`](docs/configuration.md) 第 10 节为唯一权威，其它文档引用必须逐字一致。
