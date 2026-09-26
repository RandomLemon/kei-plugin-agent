# testing.md — 质量与测试（第 11 章）

本文覆盖第 11 章：质量门、单元测试矩阵与注入缝、mock 适配器端到端联调、竞态与优雅关闭。完成定义：质量门全绿 + §11.2 矩阵全过 + §11.3 端到端可复现。

## 11.1 质量门

合并前必须全绿（与 [`../AGENTS.md`](../AGENTS.md) 第 3 节一致）：

```bash
gofmt -l .          # 必须无输出
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

`go test -race ./...` 是**必过项**：本插件大量使用定时器与并发状态，竞态检测不可省。

## 11.2 单元测试矩阵

```text
用例 → 断言可观察结果
```

| 用例 | 断言 |
| --- | --- |
| 无发送者 | 决策结果 `reason=no_sender` |
| 非群聊（`Kind=private`） | 决策结果 `reason=not_group` |
| 机器人发送者且 `ignore_bots=true` | 决策结果 `reason=bot_sender` |
| 命令消息且 `respond_to_commands=false` | 决策结果 `reason=command` |
| `/agent` 命令 | 决策结果 `reason=command`，且**不进历史** |
| 文本为空 | 决策结果 `reason=empty_text` |
| 文本短于 `trigger_min_chars` | 决策结果 `reason=too_short` |
| 会话被 `/agent off` 后入站 | 决策结果 `reason=channel_off` |
| Storage 懒加载未完成时入站 | 决策结果 `reason=loading` |
| 非寻址且 `random_enabled=false` | 决策结果 `reason=not_addressed` |
| `random_max_per_hour=0` | 决策结果 `reason=hour_quota`（随机插话关闭） |
| 寻址三种来源：`bot.SegAt` / `bot.SegReply` / `trigger_keywords` | 均判定为寻址（不给出 `not_addressed`） |
| `self_ids` 为空 vs 非空：非空时 At 到他人 ID | 空 → 寻址；非空且未命中 → 非寻址 |
| `self_ids` 非空且 At 命中 `Data[bot.KeyUserID]` | 判定为寻址 |
| `random_probability=0` | 永不命中（`reason=probability`） |
| `random_probability=1`（其余条件满足） | 必定进入 `schedule()` |
| `random_min_participants` 不足 | 决策结果 `reason=min_participants` |
| 距 `lastAgentAt` < `random_cooldown`（注入 `p.now` 桩） | 决策结果 `reason=cooldown` |
| 寻址且距 `lastAgentAt` < `mention_min_interval` | 决策结果 `reason=cooldown` |
| 近 1 小时回复数达 `random_max_per_hour`（注入 `p.now`） | 决策结果 `reason=hour_quota` |
| `random_quiet_hours` 跨零点（如 `23:00-07:00`，注入 `p.now`） | 窗口内 `reason=quiet_hours`，窗口外放行 |
| `random_timezone` 影响 `{{now}}` 与静默判定 | 注入不同时区，静默判定边界随之移动 |
| 批处理窗口合并（`batch_window=50ms`，真实定时器） | 窗口内多条消息合并为**一次**生成；`batch_max_window` 上限生效 |
| `st.epoch` 变化（`/agent off` 后定时器回调） | 在途结果被丢弃，`reason=stale`，不发送 |
| 全局信号量占满（`limits_max_concurrent=1` 且已有在途） | `reason=semaphore_full`，不排队 |
| LLM 返回 skip token | `reason=skipped_by_llm` |
| LLM 返回空串 | `reason=empty_reply` |
| LLM 返回与最近自己发言重复且 `reply_dedupe=true` | `reason=duplicate_reply` |
| 发送失败 | `reason=send_error`，不重排、不重发 |
| 模板渲染 10 个占位符 | 每个占位符被替换为预期值 |
| 模板含未知占位符（如 `{{unknown}}`） | 原样保留 |
| 历史渲染 6 种回落 | `[图片]` / `[表情]` / `[文件]` / `[卡片]` / `[引用]` / `[消息]` 分别命中 |
| 历史裁剪 | 超 `context_max_messages` 或 `llm_history_max_chars`（rune）从最旧丢弃，保留最新一条 |
| 清洗管线 8 步（见 [`persona.md`](persona.md) §8.7） | 每条输入→输出样例一致 |
| `/agent status` | 输出固定字段顺序一行 |
| `/agent persona` | 输出 `agent: persona=<name> 来源=<override\|binding\|default>` |
| `/agent on`、`/agent off` | 切换开关，触发 `Storage.Set`，关闭时递增 `st.epoch` |
| `/agent reset` | 清历史/覆盖/计数器，置为开启，递增 `st.epoch` |
| `llm.go` 用 `httptest.Server`：200 | 返回解析后的文本 content |
| `llm.go` 429 重试 / 500 重试 | 按 `llm_max_retries` 重试并最终成功 |
| `llm.go` 400 不重试 | 立即返回错误，不重试 |
| `llm.go` 非法 JSON | 返回错误 |
| `llm.go` 缺 choices | 返回错误 |
| `llm.go` 超时（`llm_timeout` 极短） | 返回错误，`reason=llm_error` |

reason 词表（22 个）单测覆盖：`no_sender`、`not_group`、`bot_sender`、`command`、`empty_text`、`too_short`、`channel_off`、`loading`、`not_addressed`、`min_participants`、`cooldown`、`hour_quota`、`quiet_hours`、`inflight`、`probability`、`semaphore_full`、`skipped_by_llm`、`empty_reply`、`duplicate_reply`、`llm_error`、`send_error`、`stale`。

### 注入缝

必须在 `Plugin` 上定义（默认指向真实实现，测试替换为桩）：

| 字段 | 默认 | 用途 |
| --- | --- | --- |
| `p.now` (`func() time.Time`) | `time.Now` | 冷却、配额、静默时段的确定性时间 |
| `p.randFloat` (`func() float64`) | `rand.Float64` | 概率边界的确定性 |
| `p.completer` | `*openaiClient` | 屏蔽真实 LLM（实现 completer 接口），返回固定文本/错误 |
| `p.api` (`bot.BotAPI`) | 注入的 `PluginContext.Bot` | 捕获 `Send` 调用；命令测试用 `bot.NewNoopReply()` 断言 `PlainText()` |

禁止为测试引入第三方 mock 库，全部用标准库 testing + 手写桩。

## 11.3 端到端联调（mock 适配器）

以 kei 仓库检出为宿主，构建一个最小宿主 main：空导入本插件 + 启用 mock 适配器 + `plugins.agent`。为稳定复现，配置 `random_probability: 1.0`、`mention_min_interval: 0s`、`random_cooldown: 0s`。

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

说明：mock 适配器的 HTTP `/inject` 只支持文本段，无法构造 `bot.SegAt`。因此「寻址（@/引用）」用例改用 Go 侧 `Adapter.Inject(ctx, ev)` 注入含任意 `Segments` 的事件（写在 `e2e_test.go`），HTTP 路径只覆盖随机插话与命令。

## 11.4 竞态与优雅关闭

- `go test -race ./...` 必过。
- **停止后不发送**：用假 `BotAPI` 计数，断言 `Stop` 之后不再有 `Send`。
- **及时返回**：在存在待生成协程时调用 `Stop`，断言在 15s 内返回（`wg.Wait` 配合插件级 ctx cancel）。
- **无定时器泄漏**：断言 `Stop` 停掉了全部 `time.AfterFunc` 布防的定时器（跟踪每个 `channelState.timer` 并在 `Stop` 时 `Stop()`）。
- **幂等**：重复调用 `Stop` 安全无 panic。
