# memory/deploy.md — 部署方案

本文是接真实 QQ 与记忆库的部署方案：依赖容器（SnowLuma + PostgreSQL）的搭建、开启记忆的配置、开发机联调运行、正式部署（kei-deps 上编译并以 systemd 常驻）、注意事项与验收。联调宿主 `cmd/devhost` 本身（文件、环境变量覆盖规则、运行命令、VS Code 调试）见 [`../testing.md`](../testing.md) §11.5，本文不重复。

## 总体结构

QQ 侧用 [SnowLuma](https://github.com/SnowLuma/SnowLuma)（注入官方 QQ 客户端，对外提供 OneBot v11），与 PostgreSQL 一起以 Docker Compose 跑在同一个长期运行的 LXC（下称 `kei-deps`，IP 记作 `<DEPS_IP>`）里，与开发机在同一网桥下经内网 IP 互通。宿主主动连 SnowLuma（`forward_ws`），开发机不需要开放端口，任一侧重启后自动重连；改完插件代码只重启宿主，依赖一直运行。

```
kei-deps（LXC，常驻）                     开发机
├── snowluma :3001 OneBot WS  ◀──────────  devhost（forward_ws，Bearer token）
│            :5099 WebUI / :6081 noVNC
└── postgres :5432            ◀──────────  devhost（memory_database_dsn）
```

SnowLuma 与 kei 的对接点：正向 WS 服务端用 `role: "Universal"`（kei 的事件与 API 调用共用一条连接），鉴权走 `Authorization: Bearer <token>`（kei 握手即如此发送），消息格式 `array`。SnowLuma 的许可为源码可见、仅限非商业自用；它属于非官方客户端，bot 用小号。

数据库用 `pgvector/pgvector:pg17`：PostgreSQL 17 与 [`overview.md`](overview.md) §15.3 的选型一致，镜像自带 M3 起需要的 `vector` 与 `pg_trgm`，届时不必重建库。

## 依赖容器

**1. 建 LXC**：Debian 13 模板、非特权、2 核 / 4 GB / 20 GB、vmbr0 静态 IP。Docker 需要嵌套：

```bash
pct set <CTID> --features nesting=1   # 在 PVE 宿主上执行
```

`keyctl` 不必开：未开时容器内 keyctl 系统调用返回 ENOSYS，Docker（runc）会容忍并照常运行。宿主还需加载 `overlay` 内核模块（`modprobe overlay`，并写入 `/etc/modules-load.d/` 持久化），即时生效，不必重启 CT。

**2. 装 Docker**（容器内）：

```bash
apt update && apt install -y curl ca-certificates
curl -fsSL https://get.docker.com | sh
```

**3. Compose**：`/opt/kei-deps/compose.yaml`，同目录 `.env`（权限 600）写 `PG_PASSWORD=<强密码>` 与 `VNC_PASSWD=<8 位密码>`。SnowLuma 的 `cap_add`、`security_opt`、`shm_size` 是其官方 Docker 文档要求的（ptrace 注入 QQ 进程、Chromium 共享内存），不能省；镜像名与端口以其 [Docker 部署文档](https://snowluma.github.io/zh/docs/guide/deploy/docker) 为准。

```yaml
services:
  snowluma:
    image: motricseven7/snowluma:latest
    restart: unless-stopped
    shm_size: 1gb
    cap_add: [SYS_PTRACE]
    security_opt: [seccomp=unconfined]
    ulimits:
      nofile: { soft: 65536, hard: 524288 }   # 非特权 LXC 的上限，超过会启动失败
    environment:
      SNOWLUMA_WEBUI_HOST: 0.0.0.0
      SNOWLUMA_WEBUI_PORT: "5099"
      SNOWLUMA_QQ_FLAGS: --disable-gpu --disable-software-rasterizer --disable-gpu-compositing
      VNC_PASSWD: ${VNC_PASSWD}
    ports:
      - "3001:3001"   # OneBot WS，给 devhost 连
      - "5099:5099"   # WebUI
      - "6081:6081"   # noVNC，扫码登录用
    volumes:          # 升级镜像时不要删这三个卷，否则要重新登录
      - snowluma-data:/app/data
      - qq-config:/app/.config
      - qq-data:/app/.local/share

  postgres:
    image: pgvector/pgvector:pg17
    restart: unless-stopped
    environment:
      POSTGRES_USER: persona
      POSTGRES_PASSWORD: ${PG_PASSWORD}
      POSTGRES_DB: persona
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  snowluma-data:
  qq-config:
  qq-data:
  pgdata:
```

```bash
cd /opt/kei-deps && docker compose up -d
```

SnowLuma 的 ptrace 注入在「LXC 内嵌 Docker」下未在本项目验证过；若 snowluma 起不来或日志报注入失败，把它挪到 PVE 虚拟机里跑（Compose 不变），PostgreSQL 留在 LXC。

若 snowluma 启动报 `error setting rlimit type 7: operation not permitted`：`nofile` 的 `hard` 超过了 LXC 的上限（非特权 CT 为 524288），按模板改小即可。

VNC 密码：启动时依次取环境变量 `VNC_PASSWD`、卷内 `config/vnc-password`，都没有才随机生成并在日志打印 `远程桌面密码:`；选定值写回 `config/vnc-password`（在 `snowluma-data` 卷里，重启与重建都保留）。VNC 只认前 8 位。改密码：改 `.env` 的 `VNC_PASSWD` 后 `docker compose up -d snowluma`（重建容器，QQ 可能需要在 noVNC 里重新登录）。

**4. 登录 QQ 与配置 OneBot**：

1. WebUI 临时密码只在首次启动的日志里打印，凭据在标题下几行：`docker compose logs snowluma 2>&1 | grep -A6 "initial credentials"`。修改密码前不要重启或重建 snowluma，否则临时密码会重新生成。VNC 密码见 `.env` 的 `VNC_PASSWD`。
2. 浏览器开 `http://<DEPS_IP>:6081/`（noVNC，输 VNC 密码），在 QQ 窗口里用手机扫码登录 bot 的小号；登录后 hook 自动进入工作模式。
3. 开 `http://<DEPS_IP>:5099/`（用户 `admin` + 临时密码），先修改密码。
4. 核对正向 WS 服务端：登录后生成的账号配置 `config/onebot_<QQ号>.json`（在 `snowluma-data` 卷里）的 `networks.wsServers`。SnowLuma 默认的 `ws-default` 条目已符合下面的要求，且自带随机 `accessToken`，通常不用改，只需取出 token 填进 devhost 的 `KEI_BOTS_QQ_MAIN_ACCESS_TOKEN`：`docker compose exec snowluma grep accessToken /app/data/config/onebot_<QQ号>.json`（`httpServers` 也有一个 `accessToken`，取 `wsServers` 里的）。要求如下：

   ```json
   {
     "name": "ws-default",
     "host": "0.0.0.0",
     "port": 3001,
     "path": "/",
     "role": "Universal",
     "accessToken": "<WS_TOKEN>",
     "messageFormat": "array",
     "reportSelfMessage": false
   }
   ```

   `messageFormat` 必须是 `array`（CQ 码字符串会丢失 @ 信息）；`reportSelfMessage` 保持 `false`（插件自己记录回复，打开会让自身消息再作为入站事件回来一遍）。

**5. 数据库**：真实 bot 用默认 `public` schema，表结构由插件启动时自动迁移，无需手动建表。mock 用的 schema 见「注意事项」。

**6. 网络**：`3001`、`5099`、`6081`、`5432` 只应内网可达。`pgvector` 镜像默认允许任意来源密码登录，用强密码，或在 PVE 防火墙里只放行开发机 IP。开发机自检：`nc -zv <DEPS_IP> 3001 && nc -zv <DEPS_IP> 5432`。

## 开启记忆

记忆默认关闭，开发机联调与正式部署都按下面的键开启。完整键表与校验规则见 [`../configuration.md`](../configuration.md) §10.1、§10.4，环境变量写法见 §10.3。

- **必需**：`memory_database_dsn`（`KEI_PLUGINS_PERSONA_MEMORY_DATABASE_DSN`）。配上即开启采集、每日 / 滚动摘要、清理与回复前的记忆块注入；为空时整个记忆子系统不启动。
- **建议**：`random_timezone`（`KEI_PLUGINS_PERSONA_RANDOM_TIMEZONE=Asia/Shanghai`）与 `memory_daily_at`（默认 `04:00`）共同决定摘要日的日界与每日摘要时刻。
- **可选：显式查询工具**：`memory_tools_enabled: true`（`KEI_PLUGINS_PERSONA_MEMORY_TOOLS_ENABLED=true`）装配 `memory__summary`，要求聊天模型支持 tool calling。人格配了 `tools` 名单时只能用名单内的工具，需把 `memory__summary` 加进 `personas.<名>.tools`；没配 `tools` 的人格不用改。
- **可选：摘要模型**：`memory_llm_base_url` 为空时沿用 `llm_base_url` / `llm_api_key` / `llm_extra_headers`，`memory_llm_model_small` 为空时用 `llm_model`，`memory_llm_model_large` 为空时用 `memory_llm_model_small`；摘要想用更便宜的模型时再单独配置。

开启后按「验收」第 4 步用 `/agent memory status` 确认。

## 开发机联调运行

前提：devShell 内（`GOTOOLCHAIN=local`），同级目录有 kei 检出（`../kei`，`cmd/devhost/go.mod` 的 replace 指向它）。

`cmd/devhost/.env`（复制 `.env.example` 后填写）：

```bash
KEI_PLUGINS_PERSONA_LLM_BASE_URL=https://<服务>/v1
KEI_PLUGINS_PERSONA_LLM_API_KEY=<key>
KEI_PLUGINS_PERSONA_LLM_MODEL=<model>

KEI_BOTS_QQ_MAIN_ENABLED=true
KEI_BOTS_QQ_MAIN_WS_URL=ws://<DEPS_IP>:3001
KEI_BOTS_QQ_MAIN_ACCESS_TOKEN=<WS_TOKEN>
KEI_BOTS_QQ_MAIN_SELF_ID=<bot QQ号>             # 事件缺少 self_id 时的兜底
KEI_PLUGINS_PERSONA_SELF_IDS=[<bot QQ号>]
KEI_PLUGINS_PERSONA_GROUP_LIST=[<测试群号>]

KEI_AUTH_ADMIN_USERS=onebot:<你的QQ号>,mock:u1   # OneBot 的平台名是 onebot
KEI_PLUGINS_PERSONA_MEMORY_DATABASE_DSN=postgres://persona:<PG_PASSWORD>@<DEPS_IP>:5432/persona?sslmode=disable
KEI_PLUGINS_PERSONA_RANDOM_TIMEZONE=Asia/Shanghai   # 缺省 Local；影响 {{now}}、静默时段与每日摘要时刻
```

```bash
cd cmd/devhost
(set -a && . ./.env && exec go run .)
```

日常迭代：改插件代码 → Ctrl+C → 重新执行上面的命令；SnowLuma 与 PostgreSQL 不用动。用 Ctrl+C（SIGINT）正常退出，不要 `kill -9`：`Stop` 会把记忆写入队列里未落库的消息刷进数据库。

## 正式部署

bot 长期在线（持续积累记忆）时，宿主与依赖容器一起跑在 `kei-deps` 上，由 systemd 常驻。宿主仍是 `cmd/devhost`（与 kei `cmd/bot` 同构），只是换了编译位置与配置。

**为什么在 kei-deps 上编译**：devhost 空导入了 kei 的 SQL 存储方言（`pkg/kei/sql`），正式部署用其中的 sqlite 持久化 `/agent` 运行时覆盖；sqlite 驱动是 cgo 版，二进制动态链接 glibc。devShell 编出的二进制链接的是 nix store 里的 glibc，拷到 Debian 上无法运行，因此在 kei-deps 上用 Debian 版 Go 官方镜像一次性编译，产物直接链接本机 glibc，kei-deps 不必安装 Go。

**1. 拉取源码**（kei-deps）：从 GitHub 克隆本仓库与 kei，放在同一父目录，kei 的目录名必须是 `kei`（`cmd/devhost/go.mod` 的 replace 依赖这个相对位置）：

```bash
mkdir -p ~/kei-src && cd ~/kei-src
git clone https://github.com/RandomLemon/kei
git clone -b main https://github.com/Grosser-Kurfuerst/kei-plugin-agent   # -b 换成要部署的分支或 tag
```

**2. 编译**（kei-deps）：镜像的 Go 版本不低于各 `go.mod` 的 `go` 指令，发行版与 kei-deps 一致（Debian 13 即 `trixie`）。两个命名卷缓存依赖与编译结果，之后重编只需几秒：

```bash
cd ~/kei-src
docker run --rm -v "$PWD:/src" -v kei-gomod:/go/pkg/mod -v kei-gocache:/root/.cache/go-build \
  -w /src/kei-plugin-agent/cmd/devhost golang:1.25-trixie go build -o /src/devhost .
sudo install -D -m 755 devhost /opt/kei/devhost
sudo install -m 644 kei-plugin-agent/cmd/devhost/config.yaml /opt/kei/config.yaml
```

**3. 配置**：`/opt/kei/.env`（权限 600）以开发机 `.env` 为底，地址改为本机，并补上正式运行的几项：

```bash
KEI_BOTS_QQ_MAIN_WS_URL=ws://127.0.0.1:3001
KEI_PLUGINS_PERSONA_MEMORY_DATABASE_DSN=postgres://persona:<PG_PASSWORD>@127.0.0.1:5432/persona?sslmode=disable
KEI_AUTH_ADMIN_USERS=onebot:<你的QQ号>                # 去掉 mock:u1
KEI_BOTS_MOCK_MAIN_ENABLED=false                      # 关闭 mock 注入口
KEI_STORAGE_TYPE=sqlite                               # /agent 覆盖重启保留
KEI_STORAGE_DSN=/opt/kei/kei.db
KEI_PLUGINS_PERSONA_RANDOM_TIMEZONE=Asia/Shanghai     # 必须显式写：LXC 默认 UTC，缺省 Local 会让日界、静默时段与 {{now}} 偏 8 小时
```

- **mock 必须关**：mock 在 `127.0.0.1` 开 `/inject`，本机任何进程都能注入消息；`KEI_AUTH_ADMIN_USERS` 若还带 `mock:u1`，注入的消息还会被当作管理员。联调用的 `config.yaml` 保持 mock 开启，只在正式 `.env` 里关。
- **存储覆盖优先于配置**：`/agent persona|on|off|policy|list` 写入 sqlite 后跨重启保留，并优先于 `config.yaml` 的 `bindings`、`group_policy`、`group_list` 等（[`../persona.md`](../persona.md) §8.3、[`../participation.md`](../participation.md) §7.8）。改了配置不生效时先查运行时覆盖：会话人格与开关用 `/agent reset` 清除，名单用 `/agent policy`、`/agent list` 改回。
- **备份**：记忆在 PostgreSQL（`pgdata` 卷），运行时覆盖在 `/opt/kei/kei.db`，备份时两者都要带上。

**4. systemd**：`/etc/systemd/system/kei.service`：

```ini
[Unit]
Description=kei persona host
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=/opt/kei
EnvironmentFile=/opt/kei/.env
ExecStart=/opt/kei/devhost -config /opt/kei/config.yaml
Restart=on-failure
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now kei && journalctl -u kei -f
```

`EnvironmentFile` 不解析 shell 引号，值保持 `KEY=VALUE` 裸写。systemd 停止时发 SIGTERM，宿主照常优雅退出（记忆写入队列先落库）。

**5. 升级**：`cd ~/kei-src && git -C kei pull && git -C kei-plugin-agent pull`（换版本时 `git checkout <分支或 tag>`），重新执行第 2 步的 `docker run` 与 `install devhost`，再 `sudo systemctl restart kei`。只改了 `.env` 时直接重启服务。

## 注意事项

- **同一时间只跑一个宿主**：OneBot 正向 WS 服务端会把事件推给所有已连接的客户端，两个宿主连同一个 QQ 会重复回复；记忆库一个库（或 schema）只由一个 kei 进程使用，不做跨进程互斥（[`overview.md`](overview.md) §15.1）。开发机联调前先停正式实例（`systemctl stop kei`）；要并行，开发版另配 QQ 小号（SnowLuma 支持多账号，每个账号单独端口）与独立 schema。
- **mock 测记忆用独立 schema，每轮重建**：mock 的发送 ID 每次启动从 `mock-1` 重新计数，重启后同一 mock 会话里机器人自身回复会与上一轮撞去重键（[`../testing.md`](../testing.md) §11.5）。
  ```bash
  # kei-deps 上
  docker compose exec postgres psql -U persona -d persona \
    -c "DROP SCHEMA IF EXISTS mock CASCADE; CREATE SCHEMA mock"
  # 开发机上：本次运行关掉 QQ，DSN 指向 mock schema
  (set -a && . ./.env && \
    KEI_BOTS_QQ_MAIN_ENABLED=false \
    KEI_PLUGINS_PERSONA_GROUP_LIST='[g1]' \
    KEI_PLUGINS_PERSONA_TRIGGER_KEYWORDS='[bot]' \
    KEI_PLUGINS_PERSONA_MEMORY_DATABASE_DSN="postgres://persona:<PG_PASSWORD>@<DEPS_IP>:5432/persona?sslmode=disable&search_path=mock" \
    exec go run .)
  ```
- **DSN 写错不报错**：数据库不可达时 `Start` 不阻塞，后台按 1s→60s 退避重连，`/agent memory status` 一直显示「连接中」。
- **随机插话缺省开启**（`random_probability` 0.12）：只测 @ 回复时设 `KEI_PLUGINS_PERSONA_RANDOM_ENABLED=false`；测试群只放自己与小号。
- **联调时 `/agent` 运行时覆盖重启即丢**：`config.yaml` 的 kei 自身存储为 `memory`，见 [`../testing.md`](../testing.md) §11.5；要保留按「正式部署」第 3 步改用 sqlite。
- **升级 SnowLuma**：`docker compose pull snowluma && docker compose up -d`，三个卷保留即免重新登录。

## 验收

| # | 操作 | 预期 |
| --- | --- | --- |
| 1 | 启动 devhost | 无 `onebot: 正向 WebSocket 连接失败` 持续刷屏（SnowLuma 不可达时按 1s→30s 退避重连，不影响 mock） |
| 2 | 测试群里 @bot | 收到回复；非白名单群不回复 |
| 3 | 管理员发 `/agent status` | 有输出（管理员前缀配置正确） |
| 4 | 管理员发 `/agent memory status` | `数据库=就绪`；再发几条消息后「已写入」增长，「重复」基本不涨 |
| 5 | 重启 devhost | 自动重连，旧记忆仍在并继续写入 |
| 6 | `docker compose restart snowluma` | devhost 日志出现退避重连，SnowLuma 就绪后自动恢复 |

## 排障

| 现象 | 先查 |
| --- | --- |
| WS 一直连不上 | `accessToken` 与 `KEI_BOTS_QQ_MAIN_ACCESS_TOKEN` 是否一致；`wsServers` 条目是否存在（若带 `enabled` 字段须为 `true`）、`role` 是否 `Universal`；端口映射与防火墙 |
| @ 了不回复 | `GROUP_LIST` 群号、`SELF_IDS` 的 bot QQ 号；`messageFormat` 是否 `array` |
| `/agent` 无反应 | `KEI_AUTH_ADMIN_USERS` 是否写成 `onebot:<QQ号>` |
| 记忆一直「连接中」 | DSN 的密码与 IP；`5432` 是否可达；是否带 `sslmode=disable` |
| 回复被记两遍或自言自语 | `reportSelfMessage` 是否被打开 |
