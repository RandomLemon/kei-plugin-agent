# mcp.md — 工具调用与 MCP 接入（第 13-14 章）

本文统一定义工具调用与外部 MCP 接入。第 13 章保留通用接口、调用循环、业务边界和日志契约；第 14 章定义 MCP 客户端、运行时目录刷新、生命周期与分阶段实施规划。MCP 是唯一工具来源。

配置键与默认值以 [`configuration.md#101-全量配置键表`](configuration.md#101-全量配置键表) 为准，交付状态见 [`roadmap.md#121-现状`](roadmap.md#121-现状)。LLM 请求/响应结构见 [`llm.md`](llm.md) §9.1-9.2；生成流程见 [`participation.md`](participation.md) §7.6。

## 13. 通用工具调用

### 13.1 调用目标与边界

LLM 自行决定是否调用当前人格可用的工具。调用发生在后台 `generate` 协程内，使用插件级 `p.ctx`；Handler 保持毫秒级返回。工具失败作为结果文本交还模型，由模型照常回答或输出 skip token。

工具循环统一限制调用轮数、每轮调用数、单次超时和结果长度。工具调用与结果不写入 `channelState.history`，历史只保存聊天轮次；不做固定预检索，也不提供公开 Go 工具注册 API。MCP 是唯一工具来源，范围与业务实现边界见 §14.1、§13.5。

**前提**：LLM 服务必须支持 OpenAI 兼容的 `tools` / `tool_calls`。自建 vLLM 需以 `--enable-auto-tool-choice --tool-call-parser <解析器>` 启动；Ollama 需使用支持工具的模型。不支持时服务通常返回 4xx，每轮记 `llm_error`，可用 `debug_prompts=true` 排查。

### 13.2 工具抽象

工具定义与调用结构体见 [`llm.md`](llm.md) §9.1（`toolDef`、`toolCall`）。保留现有私有接口（逐字），生产实现只有 `mcpTool`；测试仍可直接注入桩工具：

```go
// tool 是一个可供 LLM 调用的工具。
// Call 必须尊重 ctx：ctx 带有循环施加的 tools_call_timeout，取消后应尽快返回。
// 工具自身的失败（网络、上游错误、参数非法）不返回 error，
// 而是返回交给 LLM 的失败文本。
type tool interface {
	Def() toolDef
	Call(ctx context.Context, args string) toolResult // args 为 tool_calls[].function.arguments 原文
}

// toolResult 是一次工具调用的结果。
type toolResult struct {
	Content string // 作为 role=tool 消息的 content（循环会再按 tools_result_max_chars 截断）
	Status  string // 工具只返回 "ok" 或 "error"；其余取值由循环设置（§13.6）
	Detail  string // 工具自定义的细分原因，只进日志，可为空（如 "no_result"、"http_503"）
}
```

### 13.3 工具选择

`toolsFor(personaName)` 复制当前目录并按人格过滤，整个生成固定使用这一快照。配置启用、名称和人格三态的唯一规则见 §14.3；目录读取、替换与并发规则见 §14.6。

可用工具为空时，请求不带 `tools` 字段，只调用一次 `Complete`，普通聊天仍可运行。`p.tools` 仍是测试注入缝（[`testing.md`](testing.md) §11.2）。

### 13.4 工具调用循环

`generate` 调用 `p.completeWithTools`（[`participation.md`](participation.md) §7.6）：

```text
completeWithTools(st, epoch, personaName, req) (content string, err error):
  tools = p.toolsFor(personaName)
  if len(tools) == 0:
    res, err = p.completer.Complete(p.ctx, req)
    return res.Content, err

  req.Tools = defs(tools)
  for round = 0; ; round++:
    req.ToolChoice = ""                               # 省略 tool_choice，即服务端默认 auto
    if round >= tools_max_rounds: req.ToolChoice = "none"   # 轮数用尽：仍带 tools，但禁止再调用
    res, err = p.completer.Complete(p.ctx, req)
    if err != nil:                         return "", err                 # → reason llm_error
    if len(res.ToolCalls) == 0 || req.ToolChoice == "none":
                                           return res.Content, nil        # 轮数用尽仍返回 tool_calls 时只取 content
    if st.epoch != epoch（加锁读取）:       return "", errStale            # → reason stale，不再调用工具

    req.Followups += {role: "assistant", content: res.Content（空则 null）, tool_calls: res.ToolCalls}
    for i, call in res.ToolCalls:
      r = p.runTool(tools, i, call)
      req.Followups += {role: "tool", tool_call_id: call.ID, content: r.Content}
    if p.ctx.Err() != nil:                 return "", p.ctx.Err()

runTool(tools, i, call) toolResult:            # 循环层兜底，对所有工具一致
  if i >= maxToolCallsPerRound:  r = {Content: "[工具调用被忽略] 本轮工具调用过多。", Status: "ignored"}
  else if tools 中找不到 call.Function.Name:
                                 r = {Content: "[工具调用失败] 未知工具 <name>。", Status: "unknown_tool"}
  else:
    cctx, cancel = context.WithTimeout(p.ctx, tools_call_timeout)
    r = tool.Call(cctx, call.Function.Arguments)
    if cctx.Err() == context.DeadlineExceeded:
                                 r = {Content: "[工具调用失败] 工具 <name> 超时。", Status: "timeout", Detail: r.Detail}
    else if r.Status != "ok":    r.Status = "error"   # 工具返回的非法 status 一律按 error 处理
    cancel()
  r.Content = truncateRunes(r.Content, tools_result_max_chars)（被截断时末尾加 "…"）
  log Debug "persona 工具调用"（§13.6）
  return r
```

要点：

- **常量**：`maxToolCallsPerRound = 3`。模型一次返回多个调用时按顺序执行前 3 个；其余**仍须**回一条 `role=tool` 消息（OpenAI 协议要求每个 `tool_call_id` 都有对应结果），内容为忽略文本。
- **轮数**：`tools_max_rounds`（默认 `2`）是最多允许几轮工具调用；之后再发起一次带 `tool_choice: "none"` 的请求得到最终文本。因此单次生成最多 `tools_max_rounds + 1` 次 LLM 请求。最后一轮仍带 `tools` 而不是去掉它：部分兼容服务在消息含 `tool_calls` 却缺少 `tools` 时会报错。
- **超时兜底**：每次调用由循环派生 `tools_call_timeout`（默认 `8s`）的 ctx，工具内部不再另设超时。循环不能抢占不尊重 ctx 的 Go 代码（另起协程放弃等待会泄漏协程，违反 [`../AGENTS.md`](../AGENTS.md) §2.3），因此「尊重 ctx」是 §13.2 接口契约的一部分，由各工具的单测保证。
- **长度兜底**：任何工具的结果都按 `tools_result_max_chars`（默认 `2000`，rune）硬截断；MCP 结果转换见 [`mcp.md`](mcp.md) §14.5。
- **未知工具**：模型调用了不在当前人格可用列表里的工具（含幻觉出的名字）时返回未知工具文本，不执行。
- **顺序执行**：同一轮的多个调用串行执行，工具调用本身不额外起协程（[`architecture.md`](architecture.md) §3.2）。
- **信号量**：整个循环都在 `generate` 已持有的全局信号量内完成，工具调用不额外占用或释放并发名额。
- **epoch**：每轮执行工具前检查 `st.epoch`；会话期间被 `/persona off|switch|reset` 时立即放弃，reason `stale`，不再浪费工具与 LLM 请求。发送前的 epoch 复核（§7.6）保持不变。
- **ctx**：工具调用一律用插件级 `p.ctx` 派生，禁止 Handler ctx 与 `context.Background()`。`Stop` 取消 `p.ctx` 后循环在当前请求返回时退出。
- **耗时上界**：`(tools_max_rounds + 1) × LLM 单次上界（llm.md §9.3） + tools_max_rounds × maxToolCallsPerRound × tools_call_timeout`。默认值下约为 `3 × 40s + 2 × 3 × 8s`，均脱离 Handler，不受 `RuleTimeout`/`EventTimeout` 约束。
- **清洗与发送**：循环返回的 `content` 照常走回复清洗管线（[`persona.md`](persona.md) §8.7），reason 词表不变（[`participation.md`](participation.md) §7.7）。清洗会压平换行并按 `reply_max_chars` 截断，资料类人格宜只回答关键步骤并附链接。

### 13.5 业务工具边界

persona 只负责发现、适配与调用，不实现知识库、天气等业务接口。若需要原有 `knowledge_search` 能力，由外部 MCP 服务封装知识库接口，发现后作为 `mcp__knowledge_search` 暴露给模型；工具描述、参数 schema 与返回内容由服务定义。外部业务工具实现不属于本仓库交付。

### 13.6 日志

每次工具调用由循环以 `Debug` 输出一行（`debug_prompts` 无关）：

```text
msg="persona 工具调用" channel=<key> tool=<name> status=<status> detail=<detail> duration_ms=<n>
```

- `status` 是**循环层**的固定取值：`ok`、`error`、`timeout`、`ignored`、`unknown_tool`。新增工具不扩充该词表；工具自己的细分原因放 `detail`，MCP 的转换与错误细分见 [`mcp.md`](mcp.md) §14.5。
- 工具参数原文只在 `debug_prompts=true` 时附加（字段 `args`，按 rune 截断 256）；LLM 请求体日志（[`llm.md`](llm.md) §9.5）此时已包含工具消息。
- MCP 客户端的错误与刷新日志见 [`mcp.md`](mcp.md) §14.5，凭据与 session ID 不进日志。
- 工具调用不计入 `/persona status` 的计数器；一次生成最终仍只记一条决策行（[`participation.md`](participation.md) §7.7）。

## 14. MCP 接入与运行时目录

### 14.1 目标与范围

persona 作为 MCP 客户端连接外部服务，将服务端的每个工具分别暴露给 LLM。服务端增加、删除或修改工具后，管理员在 persona 中刷新目录，后续生成使用新目录，无需重新编译或重启 persona。

第一版连接一个配置好的服务端，支持 **Streamable HTTP + 普通同步工具调用**，**MCP 是唯一工具来源，删除现有内置工具链路**。支持 JSON 与 POST 响应中的 SSE，工具结果支持文本和结构化 JSON。

P8 同时移除 `toolSpec`、`builtinTools`、`findToolSpec`、`buildTools`、`rag.go` 与对应的专用测试，删除 `tools_enabled` 和 `rag_*` 配置。保留 `tool` / `toolResult`、人格过滤和通用工具循环。不保留内置工具兼容层；需要知识库检索时，由外部 MCP 服务提供工具。

刷新通过管理员命令触发，不建立 GET 通知长连接、不轮询，不实现 stdio、旧版 HTTP+SSE、OAuth、resources、prompts、sampling、elicitation 或 tasks。服务端地址和凭据仍是启动配置，修改它们需要重启；运行时修改的是该服务端提供的工具目录。

### 14.2 结构与调用链

```text
PluginContext.Config / HTTPClient
              ↓
          mcpClient ── initialize / tools/list ──→ 外部 MCP 服务端
              ↓                                      ↑
     mcpTool（每个远程工具一个） ── tools/call ──────────┘
              ↓
       MCP 工具 → Plugin.tools（受锁保护）
              ↓
   toolsFor：复制快照并按人格过滤
              ↓
     completeWithTools → runTool → tool.Call
```

只新增两个具体类型，不引入通用 provider 接口、全局注册表或工具管理框架：

- `mcpClient`：负责 JSON-RPC、初始化、HTTP/SSE、会话、列表分页与工具调用。HTTP 客户端使用 `PluginContext.HTTPClient`，允许连接管理员配置的内网服务。
- `mcpTool`：实现现有 `tool`。保存本地 `toolDef`、远端原始名称和客户端引用；`Def()` 返回该版本定义，`Call(ctx, args)` 调用远端并转换为 `toolResult`。

现有 [`tool` 与 `toolResult`](../tools.go)、[`toolDef`](../llm.go) 无需导出。LLM 请求格式和工具循环无需增加 MCP 专用分支；工具来源变化只影响装配与 `toolsFor` 的快照读取。

### 14.3 配置、命名与人格过滤

示例（省略已有的 LLM 配置）：

```yaml
plugins:
  persona:
    enabled: true
    mcp_url: http://127.0.0.1:9000/mcp # 完整 MCP 端点，不自行追加路径
    mcp_extra_headers: {}            # 需要时配置 Authorization 等固定请求头
    mcp_timeout: 8s                  # 初始化或完整目录刷新操作的总预算
    personas:
      default:
        prompt: 普通的群友，说话随意。
        # 不写 tools：继承当前全部 MCP 工具
      helper:
        prompt: 回答电脑问题时先查资料。
        tools: [mcp__search_manual]
      quiet:
        prompt: 只闲聊。
        tools: []
```

- `mcp_url` 为空时不构造客户端、不产生 MCP 网络请求，工具目录为空；非空即加载该端点提供的全部工具。无需另加 `mcp_enabled` 或全局工具启用名单。
- 本地名字为 `mcp__<远端名称>`，调用时使用保存的原始名称；工具目录按本地名称排序。
- 为适配当前 OpenAI function calling，转换后的名称限定为 1–64 个 ASCII 字母、数字、下划线或连字符。远端名称含点或过长时，目录加载明确报错；第一版不做自动改名或哈希别名。重复名称、非法 `inputSchema`、要求 tasks 的工具也报错，不悄悄隐藏条目。错误细节带出错工具的远端名（转义、按 rune 截断 64）：`bad_tool_name:"x.y"`、`duplicate_tool_name:"x"`、`bad_tool_schema:"x"`、`tasks_required:"x"`；整个目录一次校验，任一条目非法即整体失败，不发布半份目录。
- `name`、`description`、`inputSchema` 分别转换为 `toolDef.Function.Name`、`Description`、`Parameters`；schema 保留原 JSON 对象，不实现完整 JSON Schema 校验器或 schema 改写器。
- 人格 `tools` 不写或为 null：继承当前全部工具；`[]`：无工具；名单：取与当前目录的交集，保持装配顺序。
- 人格名单只接受 `mcp__` 名称，在配置阶段校验名称格式及 `mcp_url` 非空，不要求远端当前存在；不存在时不会暴露给模型。这样可以先配置将来新增的工具，删除远端工具也无需同步修改人格配置。

### 14.4 MCP 协议最小实现

协议基线固定为 **2025-11-25**，不是自动追随最新版本；第一版只接受这一协商结果。纯 `net/http`、`encoding/json` 与标准库 SSE 解析，不引入 MCP SDK。

1. `initialize`：携带协议版本、插件客户端信息和空客户端 capabilities；要求服务端声明 `tools` 能力。
2. 成功后发送 `notifications/initialized`，接受 HTTP 202，再调用 `tools/list`。
3. 按 `nextCursor` 拉取全部分页，所有页共用一次操作的 `mcp_timeout`；完整收集并校验后才发布目录。成功返回空列表就是删除全部 MCP 工具。
4. 模型调用 `mcp__search_manual` 时，解析参数为 JSON 对象，再发送 `tools/call`，使用远端名字 `search_manual`。非法参数不发请求。

HTTP/JSON-RPC 必须处理的部分：

- 每条消息单独 POST；请求头为 `Content-Type: application/json`、`Accept: application/json, text/event-stream`。初始化完成后携带协商版本；服务端返回 session ID 时保存并在后续请求中携带。
- 请求 ID 在客户端实例内唯一；响应校验 JSON-RPC 版本、对应 ID 和 result/error。通知没有 ID。JSON 响应读取单个响应；SSE 按事件边界拼接多行 `data:`，收到对应响应后关闭响应体，不等待流自然结束。
- SSE 中的空事件、注释和通知不当成响应。第一版不使用列表变更通知来自动刷新；对服务端的 `ping` 正常回复，对未支持的请求返回 JSON-RPC method-not-found，不宣告不存在的客户端能力。
- 不做 HTTP 重试或 SSE 断线续传。响应到达前连接断开，返回调用失败；尤其不能重发可能已经执行的 `tools/call`。
- 带 session ID 的请求收到 HTTP 404 时，清除失效会话并按协议发起新初始化；同一客户端的重新初始化串行、受当前操作的剩余时间约束。原工具调用返回失败，**不自动重放**。目录通过管理员 reload 重新确认。
- 管理操作用 `mcp_timeout`；工具调用直接使用循环施加的 `tools_call_timeout`，会话恢复也计入这个预算，不额外叠加超时。单次操作内所有响应（含初始化与分页）累计读取上限 1 MiB：超限返回 `response_too_large`，不把截断 JSON 当成功结果，目录总量超过该上限的服务端按加载失败处理而不是继续累积内存。
- `mcp_extra_headers` 不得覆盖 Content-Type、Accept、MCP-Protocol-Version 和 MCP-Session-Id（大小写不敏感）。凭据和 session ID 不进日志，不复用图片下载客户端。

固定协议版本、同步调用、结果类型和命名限制是第一版的兼容范围；接入前需确认服务端满足这些要求。

### 14.5 工具结果

`mcpTool.Call` 负责协议结果到现有 `toolResult` 的转换：

| 结果 | 转换 |
| --- | --- |
| `content` 含文本块 | 按原顺序拼接文本；非文本块用简短的类型标记表示未展示内容，不下载资源或转发 base64 |
| 无文本且有 `structuredContent` | 序列化为紧凑 JSON 文本，不重复输出服务端已经放在文本块中的 JSON |
| 成功且没有任何内容 | 明确返回「工具执行成功，无返回内容」 |
| 仅有不支持的内容类型 | 返回该内容类型不受支持的失败文本 |
| `isError=true` | 保留服务端提供的文本错误；没有内容时返回「工具执行失败，无错误信息」，`Status=error`、`Detail=tool_error` |
| HTTP、JSON-RPC、解析或网络失败 | 返回简短工具失败文本，细分原因进 `Detail` 和日志，不把原始 HTTP 错误页或认证信息发给模型 |

正常结果 `Status=ok`。现有 `runTool` 继续施加结果长度限制、日志与超时文本；内容通过 `role=tool` 交给模型，不插入 system prompt，也不写入聊天历史。日志沿用 `tool`、`status`、`detail`、`duration_ms`；目录刷新另记 `operation=mcp_reload`、`tool_count` 和是否成功，不建立第二套指标系统。

### 14.6 运行时刷新与并发

新增管理员命令：

- `/persona tools`：显示当前全部工具名称，只读本地目录，不联网。
- `/persona tools reload`：后台重新拉取 MCP 工具目录；未启用 MCP 时明确提示。只允许一个刷新在途，重复命令立即提示正在刷新，不排队。

reload 的 Handler 只做参数校验、记录回复目标、登记任务并立即回复「开始刷新」。协程从插件级 `p.ctx` 派生超时，完成后用 `BotAPI.Send` 向原会话发送成功条数或失败原因；发送同样有超时。不持有 Handler ctx、Reply 或可变 Event 指针，命令不进聊天历史。

刷新过程：

```text
取得完整 tools/list → 转换、校验候选工具 → 短暂加写锁
    → 一次性替换整个工具目录 → 解锁 → 报告结果
```

网络与工具执行期间不持有目录锁。失败返回错误，原目录不变；这只是原有状态的事务性保留，不尝试切换备用端点或静默生成替代工具。

`toolsFor` 在读锁内复制可用工具切片，返回后释放锁。每次 `completeWithTools` 固定使用同一快照，包括定义和实现；刷新创建新的 `mcpTool`，不修改旧定义。因此目录增删与 schema 更新从下一次生成生效。当前生成遇到远端已经删除的工具时照常得到失败结果；快照不保证远端还保留该工具，也不承诺立即撤销已开始的执行。

状态全部挂在 `Plugin` 实例上：目录锁、客户端引用、刷新在途标记。`toolsMu` 只保护工具切片，`mcpRefreshing` 与 `closing` 一起由现有 `writeMu` 保护；复用 `p.wg` 跟踪刷新协程。必须在准入锁内判定关闭状态并登记任务，避免 Stop 开始后再 Add。无需额外 worker、队列、引用计数或目录持久化。

### 14.7 生命周期与改动位置

| 阶段 | 行为 |
| --- | --- |
| Setup | 解析配置、构造客户端，工具目录初始为空；不做 MCP 网络操作 |
| Start | 保存插件级 ctx；配置了 MCP 时，在阶段 ctx 与 `mcp_timeout` 的共同预算内完成初始化和首次目录加载。失败时先记一条 `Warn`（`operation=mcp_load`，`err` 里带出错工具名），再清理 session 并返回 error 阻止启动，与现有配置显式启用能力的原则一致 |
| 运行期 | 工具调用在现有 generate 协程内执行；仅 reload 额外启动一个短期协程 |
| Stop | 标记关闭后拒绝新刷新；沿用现有取消与等待顺序，取消在途 MCP 请求并等待刷新/生成退出。有 session 时用 Stop 阶段 ctx 加超时发送 DELETE；405 视为服务端不支持显式结束，其余清理错误返回，不重试 |

实现按以下文件职责落地，不改 kei 核心：

| 文件 | 改动 |
| --- | --- |
| `mcp.go`（新增） | 客户端、协议与 SSE、列表获取、`mcpTool` 和结果转换；文件过长时再按客户端/适配器拆分 |
| [`plugin.go`](../plugin.go) | MCP 客户端、目录锁与刷新状态；Start/Stop 生命周期接入 |
| [`tools.go`](../tools.go) | 删除内置注册与装配；`toolsFor` 复制受锁保护的快照，整体替换目录；不改调用循环 |
| [`config.go`](../config.go) | 删除内置工具/RAG 配置；增加 MCP 配置和工具名称校验 |
| `rag.go`、`rag_test.go` | P8 删除内置 RAG 工具及专用测试；业务实现放在外部 MCP 服务 |
| [`commands.go`](../commands.go) | 两个管理入口和后台刷新；现有规则继续提供管理员权限 |
| 测试文件 | 新增用例集中在 `mcp_test.go`；配置、人格与命令只补现有表格，复用工具循环和生命周期测试 |

### 14.8 验证与实施顺序

新增测试只验证 MCP 带来的行为，集中在 `mcp_test.go`，共五组：P8 的调用闭环、失败不重放、调用取消；P9 的目录刷新、并发快照与关闭。具体断言见 [§14.10 分阶段实现规划](#1410-分阶段实现规划) 的阶段测试表。

- 共用一个小型 `httptest.Server` MCP 桩，支持目录设置、调用记录和通道阻塞；JSON/SSE 只作为响应模式参数，不为各协议方法建立独立 mock。
- 调用与刷新测试直接复用 `newTestEnv`、`stubCompleter`、`fakeBotAPI`，从已注册 Handler 走到最终回复；不再在 `e2e_test.go` 重建同一条 MCP 链路。
- 默认值、配置拒绝、人格动态名称和简单命令分支只在现有测试表中补少量条目。工具循环的轮数、调用数、未知工具、超时、截断和 epoch 已有测试，不为 MCP 重测。
- 失败与取消各选一个代表场景，不逐项枚举 HTTP 状态、非法名称/schema、内容类型和日志字段；生产代码仍按 §14.3-14.7 实现完整契约。并发测试使用通道控制顺序，所有等待有上限；避免依赖 sleep 猜测执行时机，也不把独立场景塞进一个超长测试。

预计新增测试 **300-450 行**，包含共享 MCP 桩和现有表格的补充，比原估算 500-800 行减少约四成；不设硬行数上限。另删除随内置工具退出的 RAG、注册与配置专用用例，保留通用工具循环测试。P8、P9 各自完成对应测试并通过现有质量门，`go test -race ./...` 仍为必过项。

扩展时沿现有边界推进：有第二个服务端需求再把单客户端变成命名客户端集合；有自动更新需求再增加通知读取与刷新触发；有新结果类型需求只扩展 `mcpTool` 的转换。第一版不预先实现这些管理能力。

### 14.9 协议依据

本方案核对的是 MCP 2025-11-25 官方规范：

- [生命周期与能力协商](https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle)
- [Streamable HTTP、JSON/SSE、会话与协议版本头](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
- [工具列表、分页、调用与结果类型](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)

### 14.10 分阶段实现规划

阶段编号接续 [`roadmap.md`](roadmap.md) 的 P1-P7。按 P8 → P9 顺序实现；每阶段是一个可独立启动和验证的交付，阶段内部的编码步骤不作为独立交付。实现状态只在 [`roadmap.md#121-现状`](roadmap.md#121-现状) 记录。

#### 需求与阶段对应

| 需求 | 目标契约 | 阶段 |
| --- | --- | --- |
| R1 连接外部 MCP 服务并发现工具 | §14.4：初始化、JSON/SSE、会话、分页、超时和协议错误 | P8 |
| R2 模型能够选择并调用外部工具 | §14.2、§14.5：`mcpTool` 接入现有 `tool`，结果交回 LLM | P8 |
| R3 通过配置启用 MCP，并限制人格可用工具 | §14.3：三个配置键、命名前缀、人格三态与目录交集 | P8 |
| R4 启动、取消和关闭符合宿主生命周期 | §14.7：Setup 不联网，Start 有界加载，Stop 取消并清理 | P8；P9 扩展到刷新任务 |
| R5 在不重启 persona 的情况下更新工具目录 | §14.6：管理员 reload、完整替换、固定快照、失败报告 | P9 |
| R6 保持实现简洁并留出扩展边界 | §14.2：具体客户端与适配器；§14.8：按实际需求扩展 | 两阶段共同遵守 |
| R7 工具仅通过 MCP 接入 | §14.1：删除内置注册、RAG 实现、专用配置与测试；保留通用调用能力 | P8 |

| 阶段 | 完成后系统的运行能力 | 本阶段的明确边界 |
| --- | --- | --- |
| P8 启动时接入外部工具 | 未配置 MCP 时正常聊天；配置 MCP 后可完成外部工具调用并回复群聊/私聊消息 | 删除内置工具；目录在 Start 时确定，更新仍需重启；不暴露未实现的 reload 命令 |
| P9 运行时目录刷新 | 保留 P8 能力，管理员可以查看并刷新目录，后续生成使用新工具 | 单端点和手动刷新，地址/凭据修改仍需重启 |

#### 14.10.1 P8：启动时接入外部工具

**实现对应 R1-R4、R7，交付一个完整的外部工具调用闭环。** 该阶段把内置工具移除、MCP 客户端、配置、装配、调用和关闭一起完成，不要求 P9 代码才能使用 MCP。

**文件与改动点**

| 文件/入口 | 本阶段改动 | 对应需求 |
| --- | --- | --- |
| [`config.go`](../config.go) 的 `config` / `loadConfig` | 删除 `toolsEnabled`、全部 `rag*` 字段、`readToolsEnabled` 及对应读取/校验；保留三个通用 `tools_*` 限制。增加 `mcpURL string`、`mcpExtraHeaders map[string]string`、`mcpTimeout time.Duration`，在 `readPersonas` 前读取并校验 | R3、R7 |
| 同文件的 `readPersonas` | 名单只接受符合 §14.3 的 MCP 名称，不再查询内置注册表 | R3、R7 |
| `mcp.go`（新增） | `mcpClient`、协议 DTO、HTTP/JSON-RPC/SSE、初始化、工具列表和调用、session 清理；`mcpTool`、定义转换与结果转换 | R1、R2 |
| [`plugin.go`](../plugin.go) 的 `Plugin` / `setup` | 删除 `buildTools` 调用，`p.tools` 初始为空；增加 `mcp *mcpClient`，仅在 `mcp_url` 非空时用 `pc.HTTPClient` 构造 | R3、R4、R7 |
| 同文件的 `Start` / `Stop` | Start 完整加载后设置整个工具目录；Stop 取消在途调用，等待生成退出，再结束 session | R1、R4 |
| [`tools.go`](../tools.go) | 删除 `toolSpec`、`builtinTools`、`findToolSpec`、`buildTools`；保留接口与循环。`toolsFor` 每次返回独立切片，P8 的目录在 Start 后不再写入，此阶段不增加目录锁 | R2、R3、R7 |
| `rag.go`、`rag_test.go` | 删除两个文件，persona 不再直连 itxiaRAG | R7 |
| `mcp_test.go`（新增）、`config_test.go`、`tools_test.go` | 新增三组 MCP 测试；删除内置注册与 RAG 配置专用断言，迁移既有桩和配置表，保留通用循环覆盖 | R1-R4、R7 |

**按以下顺序编码，接口不对包外导出**

1. 删除 `rag.go`、`rag_test.go`、内置注册/装配函数和 `config.go` 中的专用字段、读取与校验，清理失去用途的 import；同时删除 `TestBuiltinToolsNamesMatch` 与 RAG 配置用例。保留三个通用 `tools_*` 限制。读取三个 MCP 配置键，使用现有 `cfgReader`、`asStringMap` 和配置错误格式。请求头用一个专用函数做字符串类型与保留头校验，不改已有 `readHeaders`。MCP 配置必须先于 `readPersonas` 就绪；人格名单只接受合法 MCP 名称，格式非法报 `不是合法 MCP 工具名`，端点为空时报 `未启用 MCP`。
2. 在 `mcp.go` 定义最小 DTO：`mcpToolDef` 保存名称、描述、`inputSchema` 和工具的 task 要求；`mcpCallResult` 保存 content、structuredContent、isError。只保留实际使用的协议字段。实现客户端的 POST 请求与 JSON/SSE 响应解析，再完成 §14.4 的初始化、分页、调用和会话清理。
3. 同文件实现 `mcpTool`，保存 `definition toolDef`、`remoteName string` 和 `client *mcpClient`。`Def()` 只读不可变定义；`Call()` 解析 JSON 对象参数并委托客户端，结果按 §14.5 转换。定义转换集中在一个函数：验证名称/schema、拒绝重复项和 tasks-required 工具，然后排序；不要为每项重复做网络初始化。
4. 在 `plugin.go` 构造客户端，增加一次完整目录加载函数。首次装配和 P9 刷新都将使用这个函数；它只返回候选工具，不修改 `p.tools`。相关函数边界如下（仅表示签名，不是待填空的实现模板）：

   ```go
   func newMCPClient(cfg *config, hc *http.Client) *mcpClient
   func (c *mcpClient) initialize(ctx context.Context) error
   func (c *mcpClient) listTools(ctx context.Context) ([]mcpToolDef, error)
   func (c *mcpClient) callTool(ctx context.Context, name string, args json.RawMessage) (mcpCallResult, error)
   func (c *mcpClient) close(ctx context.Context) error
   func buildMCPTools(c *mcpClient, defs []mcpToolDef) ([]tool, error)
   func (p *Plugin) loadMCPTools(ctx context.Context) ([]tool, error)
   ```

   `loadMCPTools` 在传入 ctx 上派生一个 `mcp_timeout`，保证初始化、所有列表页和转换共享总预算；调用已经初始化的客户端时，`initialize` 不再发网络请求。函数不做重试。普通 `callTool` 完全使用传入的工具调用 ctx。
5. Setup 不再装配工具。Start 沿用现有的插件级 ctx 构造和 `loadPolicy(ctx)`；MCP 未启用时以空目录完成启动，启用时用 **Start 阶段 ctx** 调 `loadMCPTools`，成功后直接设置 `p.tools`。失败时取消已创建的插件级 ctx，在剩余阶段预算内清理已建立的 session 并返回原错误；清理失败记录日志，不用清理错误覆盖启动错误。这里不启动初始化协程。
6. `toolsFor` 复制其选择结果，维持「全部工具 / 无工具 / 名单交集」语义。保持 `completeWithTools`、`runTool`、`callTool`、`llm.go` 和 `generate` 的调用逻辑；无需增加 MCP 分支、第二个生成循环或新的 LLM 请求格式。

   随入口改动同步 `plugin.go`、`tools.go`、`llm.go` 注释中的工具文档路径，统一指向 `mcp.md`；RAG 源码里的旧引用随文件删除。
7. Stop 保持现有「关闭准入 → 等写穿透 → cancel → 停定时器 → 等生成」顺序，在最后用 Stop 阶段 ctx 加 `mcp_timeout` 调客户端 `close`。没有 session 时不发 DELETE；清理请求不使用已经取消的 `p.ctx`。成功结束后清空 session，使再次 Stop 不重复删除；405 的处理沿用 §14.7。
8. 随实现同步下面的三组测试。复用 [`helpers_test.go`](../helpers_test.go) 的 `newTestEnv` / `fakeBotAPI` 与 [`tools_test.go`](../tools_test.go) 的 completer 桩；在 `mcp_test.go` 增加一个可设置目录、记录调用的 `httptest.Server` 桩即可。只加实际需要的响应模式和阻塞控制，不搭通用脚本引擎、测试框架、专用开发命令或生产管理端点。

   既有通用测试的 `newToolEnv` 去掉 RAG 配置，桩名称改用测试内定义的 `mcp__search` 等名称；工具仍在启动后直接注入 `p.tools`。人格过滤/调用数测试需要的名单在启动后、投递任何事件前设置到测试配置的 Persona 上，不为这些桩用例启用 MCP 或建立 HTTP 服务。配置解析的名单规则由 `config_test.go` 单独覆盖。

客户端允许现有的多个 generate 并发调用。请求 ID 使用实例内计数器；session 状态受客户端自身同步保护，初始化串行且等待能被调用 ctx 取消，不建立 pending-request map 或后台调度器。工具 DTO 与已发布的 `mcpTool` 不可变。会话失效按 §14.4 重新初始化，但不重放失败的工具调用。

**阶段结束时的运行方式**

- 不配置 `mcp_url`，人格不配置工具名单：用原宿主启动，普通聊天可运行、目录为空，MCP 请求数为零。
- 配置 MCP：使用 §14.3 示例并补齐已有 LLM 配置，不需要任何 RAG 专用配置。
- 迁移原配置：删除 `tools_enabled`、`rag_*`；人格中的 `knowledge_search` 等内置名字改为外部服务提供的 `mcp__<名称>`，或删去名单以继承全部 MCP 工具。外部业务工具实现不属于本仓库交付。
- 对话流程必须实际走通：群聊或已启用的私聊 → LLM 选择 `mcp__search_manual` → MCP 收到 `search_manual` 与对象参数 → 工具结果返回 LLM → BotAPI 发送最终回复。此阶段还没有 `/persona tools` 子命令。

**验收条件与测试落点**

| 测试组/位置 | 必须观察到的结果 |
| --- | --- |
| 调用闭环：`mcp_test.go` | JSON/SSE 两个表驱动场景共用初始化、两页列表和一次调用流程；桩检查初始化顺序、协商版本、session/header，并返回匹配请求 ID 的响应。通过 `testEnv.deliver` 触发生成，检查模型收到本地名称/描述/schema，远端收到原名和对象参数，文本及结构化结果进入后续补全，最终 BotAPI 发出回复；Stop 结束 session。SSE 场景在返回目标响应后保持流打开，确认客户端能够及时返回 |
| 失败不重放：`mcp_test.go` | 一次带 session 的 `tools/call` 请求收到 404；客户端重新初始化，原调用返回失败，服务端工具调用计数仍为 1。用同一 MCP 桩，不另建错误响应框架 |
| 调用取消：`mcp_test.go` | 用通道阻塞远端工具调用，确认请求已进入后执行 Stop；客户端调用与生成在有界等待内退出，session 清理完成。验证真实 HTTP 请求取消，不重复通用工具超时测试 |
| 补现有表格：`config_test.go`、`tools_test.go` | 默认值断言增加 MCP 禁用和超时；配置表补有效配置（含尚未发现的 MCP 名称）、非法地址、保留请求头、非 MCP 名称及禁用时引用 MCP 名称；人格名单表补一个远端缺失名称取交集的场景。删除内置工具专用断言，保留原有三态与循环测试 |

每项验收在本阶段完成，并通过 [`testing.md#111-质量门`](testing.md#111-质量门) 的全部命令。阶段代码中不留空方法、占位成功结果、只有目录却不可执行的远程工具，或面向 P9 的临时开关。

#### 14.10.2 P9：运行时目录刷新

**实现对应 R5，并将 R4 扩展到刷新任务。** 直接复用 P8 的客户端与 `loadMCPTools`，新增目录发布和命令入口；不再实现一套 MCP 请求流程。

**文件与改动点**

| 文件/入口 | 本阶段改动 | 对应需求 |
| --- | --- | --- |
| [`plugin.go`](../plugin.go) 的 `Plugin` | 增加 `toolsMu sync.RWMutex`、`mcpRefreshing bool`；后者由现有 `writeMu` 保护 | R4、R5 |
| [`tools.go`](../tools.go) 的 `toolsFor` | 在读锁内复制工具切片并过滤；新增一个整体替换工具目录的函数 | R5 |
| [`commands.go`](../commands.go) | `/persona tools` 和 `/persona tools reload`；目录展示与刷新准入、后台任务、结果发送 | R4、R5 |
| [`plugin.go`](../plugin.go) 的 Stop 与任务登记 | 沿用 `closing` / `p.wg`，阻止关闭期间新刷新；注释与实际状态保护一致 | R4 |
| `mcp_test.go`、`tools_test.go`、`decision_test.go` | 新增两组刷新测试；既有工具桩写入适配目录锁，简单命令分支补现有测试表 | R4、R5 |
| [`docs/persona.md`](persona.md)、[`docs/testing.md`](testing.md)、[`docs/roadmap.md`](roadmap.md) | 命令字面量、阶段验收和交付状态与实现同步 | R5 |

**按以下顺序编码**

1. 给 `p.tools` 的运行期读写加目录锁。`toolsFor` 在锁内构造独立切片，然后立即释放锁；`completeWithTools` 继续只调用一次 `toolsFor`。`Def()` 和 `Call()` 不在目录锁内做网络操作。调整测试中直接替换 `p.tools` 的写入点，遵守同一锁约定，不改桩工具接口。
2. 在 `tools.go` 新增 `replaceMCPTools(remote []tool)`：候选工具由 P8 完整加载和校验，进入写锁后直接将 `p.tools` 替换为候选切片，已发布的切片和工具对象不再修改。不筛选工具来源、不合并目录、不保存第二份目录。空 remote 表示删除全部工具；失败的加载不能调用替换函数。
3. 在 `commands.go` 增加 `tools` 分支，并更新 `personaUsage`。无参数时在读锁内提取当前**全部**目录名称，不按当前人格过滤，也不调用 `tools/list`；`reload` 为唯一允许的参数，其余回用法文本。输出格式见 [`persona.md`](persona.md) §8.4。沿用现有 `bot.WithAdmin()` 规则，不引入新的权限判断或 LLM 调用。
4. reload 在 Handler 内复制原会话的 `bot.Target`，按现有 group/private 发送目标构造方式保存字符串 ID 和消息类型。然后在现有 `writeMu` 内检查 `closing`、`mcpRefreshing` 和客户端是否启用；允许刷新时置标记并 `p.wg.Add(1)`，解锁后启动一个协程。即时回复使用现有 `Reply`；关闭、未启用和刷新在途均立即回说明，不排队。锁内不发送消息、不联网。
5. 协程用 `p.ctx` 调 `loadMCPTools`；成功且未进入关闭时调用 `replaceMCPTools`，失败报告错误。发布前按 `writeMu → toolsMu` 的固定顺序复核关闭状态，避免 Stop 后发布候选目录；读取目录的路径只持 `toolsMu`，不反向获取准入锁。
6. 刷新操作结束后，完成通知使用从 `p.ctx` **重新派生**的 `mcp_timeout`，通过 `BotAPI.Send` 发回保存的 target；不要用可能已经超时的目录加载 ctx，也不捕获 Handler ctx、Reply 或 Event。消息的 Group/Private 构造沿用 [`decision.go`](../decision.go) 的方式，发送失败只记日志，不再次刷新或重发。Stop 已取消插件 ctx 时不发送完成通知。协程退出时在 `writeMu` 下清除刷新标记，再 `p.wg.Done()`，通知结束前仍视为刷新在途。
7. Stop 的关闭判定和刷新 Add 使用同一个准入锁，维持已有取消、等待和 session 清理顺序；不增加独立 WaitGroup、常驻协程或关闭队列。目录刷新日志使用 §14.5 的字段。按下面的验收矩阵完成本阶段测试。

此阶段新增逻辑集中在工具快照、完整替换与一个短期刷新任务。`mcpClient` 的协议实现、`mcpTool`、LLM 请求格式和工具调用循环沿用 P8；没有新的配置键、provider 接口、自动刷新或目录持久化。

**阶段结束时的运行方式与验收顺序**

以下步骤由同一组目录刷新测试覆盖，无需另建 mock 适配器端到端测试：

1. 用 P8 相同配置启动 persona，选择不写 `tools`、继承全部工具的人格，调用一次已有 MCP 工具；执行 `/persona tools`，确认能看到该目录且服务端未收到新的 list 请求。使用同一套可变目录 MCP 桩和能指定调用名称的 completer 桩完成后续验收；不依赖真实模型必然选择某个工具。
2. 服务端一次更新目录：新增 `get_weather`、删除 `search_manual`、修改另一保留工具的 schema；执行 `/persona tools reload`。收到成功通知后，新生成使用新目录与新 schema，实际调用 `mcp__get_weather` 能得到最终回复，人格名单里保留缺失名字也能正常运行。
3. 让一次 reload 在后续分页失败，确认收到失败通知且旧目录完整保留。
4. 返回空列表后 reload 成功，确认工具目录为空，下一次生成直接得到普通文本回复。

| 测试组/位置 | 必须观察到的结果 |
| --- | --- |
| 目录刷新：`mcp_test.go` | 覆盖上面的运行流程；增加、删除与 schema 修改合并在一次目录更新中检查，另检查空列表后的无工具回复。一次分页中途失败后旧目录完整保留。通过 `testEnv.command` 发起 reload，观察即时回复、完成通知和更新后的实际工具调用；其中一次改用 `privateCommand`，检查通知目标。新增工具通过 `deliver` 到最终回复即可，无需再次逐字段断言 P8 的协议 |
| 并发快照与关闭：`mcp_test.go` | 分成两个短子场景：① 通道阻塞当前生成的 MCP 调用，期间成功 reload 并修改该工具 schema；释放调用后，当前生成的后续补全仍带旧定义，下一次生成带新定义。② 通道阻塞一次 reload，确认 Handler 立即返回，重复 reload 不增加任务；执行 Stop 后刷新被取消、不发布候选目录，新 reload 被拒。阻塞桩按请求 ctx 退出，所有等待有上限 |
| 补现有命令表：`decision_test.go` | 只补非法参数与 MCP 未启用两项；目录展示不联网、重复刷新和关闭期间拒绝已由上述两组覆盖 |
| 现有质量门 | 全量用例继续通过；运行 `go test -race ./...` 时上述通道场景同时覆盖目录读写和共享 session 的并发访问 |

通过 [质量门](testing.md#111-质量门) 后，本阶段的宿主可以独立运行完整的第一版 MCP 功能。两个阶段都只修改本章列出的相关入口；SDK 依赖、通用工具接口与生成流程保持既有约定，新增能力沿客户端/适配器边界扩展。
