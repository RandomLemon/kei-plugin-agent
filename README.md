# kei-plugin-agent

[`kei`](https://github.com/RandomLemon/kei) 的「LLM 人格代理」进程内插件：在群聊里按人格预设**偶尔**插话，像群里一个普通真人；私聊里只要对方开口就必回。

- 插件名 `agent`，模块 `github.com/RandomLemon/kei-plugin-agent`，Go 1.25+。
- 运行时零第三方依赖：纯标准库 `net/http` 调用 OpenAI 兼容的 `chat/completions`，不引入任何 LLM SDK；唯一依赖是 kei 的 `pkg/bot`、`pkg/message`。
- 与平台无关：只消费 `pkg/bot` 抽象，不 import kei `internal/...`，不做任何平台名分支。
- 与 kei 核心零改动：宿主空导入本包 + 配置 `plugins.agent.enabled: true` 即启用。

## 功能

| 能力 | 说明 |
| --- | --- |
| 群聊偶尔插话 | 按人格预设生成一句话；是否说话由可配置的接话决策模型决定（[§7](docs/participation.md)） |
| 私聊必回 | 私聊消息一律视为寻址，默认策略下需显式开 `private_policy: open`；不走随机路径 |
| 人格系统 | `personas` 预设库 + `bindings` 按 `platform`/`bot_id`/`channel_id` 绑定；解析优先级：运行时覆盖 > binding > `default_persona`（[§8](docs/persona.md)） |
| 接话决策 | 过滤 → 寻址判定（At / 引用自己 / 关键词）→ 随机参与（概率、冷却、小时配额、静默时段、活跃人数）→ 批处理窗口合并成一轮生成（[§7](docs/participation.md)） |
| 名单策略 | 群聊按频道 ID、私聊按发送者 ID，各一套 `off`/`open`/`whitelist`/`blacklist` 模式 + 单列表（[§7.8](docs/participation.md)） |
| 管理命令 | `/agent status｜persona｜on｜off｜reset｜policy｜list`，仅管理员，不调用 LLM、不进历史（[§8.4](docs/persona.md)） |
| 持久化 | 每会话的人格覆盖与开关、插件级名单策略写入 `bot.Storage`；进程重启丢历史与计数器、保留覆盖与策略 |
| 可观测 | 每条消息以 `Debug` 输出 `decision=reply|skip reason=<token>` 决策行（24 个固定 reason token），`/agent status` 汇总计数 |

设计边界（非目标）：不做平台协议、不做持久化数据库、不做流式输出、不解析图片/文件等多模态内容。

## 使用方法

### 1. 接入宿主

宿主 `go.mod` 指向本插件（本地开发同时用 `replace` 指向同级 kei 检出）：

```go
require github.com/RandomLemon/kei-plugin-agent v0.0.0
// 本地开发：replace github.com/RandomLemon/kei-plugin-agent => ../kei-plugin-agent
```

宿主 `main` 空导入即完成注册：

```go
import (
	_ "github.com/RandomLemon/kei-plugin-agent" // 空导入即注册插件
	"github.com/RandomLemon/kei/pkg/kei"
)
```

配置里启用插件：

```yaml
plugins:
  agent:
    enabled: true
```

### 2. 最小配置

`personas` 与 `llm_model` 必填；密钥推荐用环境变量 `KEI_PLUGINS_AGENT_LLM_API_KEY` 注入（可留空以接入本地/无鉴权推理服务）。全量键表与默认值见 [`docs/configuration.md` §10.1](docs/configuration.md)（唯一权威）。

```yaml
plugins:
  agent:
    enabled: true

    personas:
      default: "普通的群友：说话随意、口语、短。"
      tsundere:
        prompt: "傲娇：嘴硬但对人不错，偶尔吐槽。"
        temperature: 0.9

    default_persona: "default"
    bindings:
      - { channel_id: "g1", persona: tsundere }

    llm_base_url: "https://api.openai.com/v1"
    llm_model: "gpt-4o-mini"
    llm_api_key: ""                # 或 export KEI_PLUGINS_AGENT_LLM_API_KEY=sk-xxxx

    group_policy: "open"           # 群聊默认全部参与
    private_policy: "off"          # 私聊默认关闭；要启用私聊需显式改 open
```

替换其他 OpenAI 兼容服务只需改 `llm_base_url` + `llm_model`（DeepSeek / Ollama / vLLM 示例见 [`docs/llm.md` §9.6](docs/llm.md)）。

### 3. 管理命令

`/agent` 需要核心 `auth.admin_users` + Auth 中间件（`WithAdmin()` 按 `Event.Sender.ID` 精确比较）：

```text
用法: /agent status | persona [name] | on | off | reset | policy [group|private mode] | list [group|private [add|del id]]
```

| 子命令 | 行为 |
| --- | --- |
| `/agent status` | 输出开关、当前人格与来源、历史条数、小时配额、错误/跳过计数 |
| `/agent persona [name]` | 查看或切换当前会话人格（写运行时覆盖并持久化） |
| `/agent on` / `off` / `reset` | 开关本会话 / 清历史与覆盖 |
| `/agent policy [group\|private mode]` | 查看或设置名单模式 |
| `/agent list [group\|private [add\|del id]]` | 查看或增删名单项 |

## 项目结构

```text
kei-plugin-agent/
├── register.go      init() 注册 + Metadata（插件名 agent、network/storage/send_message 权限声明）
├── plugin.go        Plugin 结构、Setup/Start/Stop、从 PluginContext 装配 runtime
├── config.go        配置读取、默认值、loadConfig 校验
├── persona.go       人格解析与优先级、persona_template 渲染、历史渲染、回复清洗
├── decision.go      过滤、寻址判定、随机参与、批处理定时器（handleChat/schedule/onBatch/generate）
├── state.go         channelState、历史环、计数器、LRU 淘汰、Storage 懒加载/写穿透
├── llm.go           OpenAI 兼容客户端（请求/响应/超时/重试）
├── commands.go      /agent status|persona|on|off|reset|policy|list
├── policy.go        插件级名单策略：模式判定、命令渲染、agent:policy 读写
├── *_test.go        领域单测 + helpers_test.go 测试桩 + e2e_test.go mock 适配器端到端
├── docs/            设计文档（实现口径的唯一来源）
├── AGENTS.md        硬性规则与结构概览
├── flake.nix        nix devShell（go/gopls/golangci-lint/dlv）；.envrc 提供 direnv 自动加载
└── LICENSE          MIT
```

各文件职责的逐项说明见 [`docs/architecture.md` §5](docs/architecture.md)。

## 开发

在 `flake.nix` devShell 内（`direnv allow` 后自动加载，`GOTOOLCHAIN=local`），合并前质量门必须全绿：

```bash
gofmt -l .          # 必须无输出
go build ./...
go vet ./...
go test ./...
go test -race ./... # 必过：插件大量使用定时器与并发状态
```

端到端联调（mock 适配器 + 本地 LLM 桩服务）步骤见 [`docs/testing.md` §11.3](docs/testing.md)。

## 文档

`docs/` 是目标实现口径的唯一来源，行为变更先改文档、再改代码：

| 文档 | 章号 | 内容 |
| --- | --- | --- |
| [`architecture.md`](docs/architecture.md) | 1-6 | 定位与边界、架构与数据流、生命周期与并发、状态与持久化、目录职责、与 kei 核心契约 |
| [`participation.md`](docs/participation.md) | 7 | 接话决策模型：过滤、寻址、随机参与、批处理窗口、冷却/配额/静默时段、名单策略 |
| [`persona.md`](docs/persona.md) | 8 | 人格系统：预设库、绑定与优先级、`/agent` 命令、提示词模板、历史渲染、回复清洗 |
| [`llm.md`](docs/llm.md) | 9 | LLM 客户端：协议/请求/响应、超时重试、长度与成本、密钥与日志安全、替换服务 |
| [`configuration.md`](docs/configuration.md) | 10 | 全量配置键表（权威）、示例配置、环境变量覆盖、校验规则 |
| [`testing.md`](docs/testing.md) | 11 | 质量门、单元测试矩阵与注入缝、mock 适配器端到端、竞态与优雅关闭 |
| [`roadmap.md`](docs/roadmap.md) | 12 | 交付物现状与实现阶段 P1-P6；已知缺口（无指标、无多模态、无流式） |

## License

MIT，见 [`LICENSE`](LICENSE)。
