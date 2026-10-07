# todo-cli 部署手册

> 适用版本：todo-cli v0.1.0（macOS 14+）
> 语言：中文
> 配套文档：[技术手册](TECHNICAL.md) · [使用手册](USER_GUIDE.md)

本文面向运维与发布人员，覆盖源码构建、本地运行、生产打包、配置项、启动后自检、升级/回滚、故障排查。todo-cli 是「本地优先」产品：默认单机部署，监听回环地址；多设备/云平台/公网访问需自接反向代理与传输保护，未在本机验证。

---

## 1. 运行环境要求

### 1.1 操作系统

- **macOS 14 Sonoma 或更新版本**（Apple Silicon 或 Intel）
- 首版回归基线为 macOS 14+
- Windows / Linux 不在首版兼容矩阵中

### 1.2 运行时

| 组件 | 要求 | 备注 |
|---|---|---|
| Go（构建用） | 1.26+ | 仅构建时需要；运行时无 Go 进程 |
| Node.js 20+ | 可选 | 仅在 `npm test` / `npm run test:e2e` / `npm run dev:web` 时需要 |
| `git` | 任意较新版本 | 拉取代码 |
| 浏览器 | Safari / Chrome / Edge / Firefox 最新两个稳定版 | 网页前端 |
| 终端 | macOS Terminal.app、iTerm2、Ghostty | TUI |

### 1.3 资源

- 单 SQLite 文件（典型 100 条任务 < 50 KB），磁盘占用低。
- 内存占用：CLI/TUI < 50 MB；`todo serve` + 浏览器 ≈ 80–120 MB。
- 不依赖网络服务（除非启用大模型/智能体）。
- 端口：默认 3210；占用时自动顺延。

### 1.4 权限

- 普通 macOS 用户权限足够；不需要 `sudo`。
- 钥匙串访问（用于模型 key）需要登录 keychain 密码。
- 智能体以当前用户权限运行；`sudo`/`su`/`doas` 在所有模式下都被拒绝。

---

## 2. 从源码构建

### 2.1 克隆仓库

```bash
git clone <repo-url> todo-cli
cd todo-cli
```

### 2.2 直接构建（仅 Go）

```bash
go build -o bin/todo ./cmd/todo
ls -lh bin/todo
./bin/todo version
```

> 本机已验证：`go build -o bin/todo ./cmd/todo` 在 `go1.26.0 darwin/arm64` 下产出 18 MB 单文件二进制，依赖 `golang.org/x/sys`、`golang.org/x/term`、`modernc.org/sqlite`，无 cgo、无运行时 Go 依赖。

### 2.3 通过 npm 构建

```bash
npm install        # 装 dev deps（含 node-pty、Playwright）
npm run build      # 内部即 go build -o bin/todo ./cmd/todo
```

`postinstall` 脚本（`scripts/fix-node-pty.mjs`）会恢复 node-pty 预编译二进制（macOS spawn-helper）的可执行位。**不运行 node 的话可以跳过 `npm install`**。

### 2.4 交叉编译（仅参考）

```bash
GOOS=darwin GOARCH=arm64 go build -o bin/todo-darwin-arm64 ./cmd/todo
GOOS=darwin GOARCH=amd64 go build -o bin/todo-darwin-amd64 ./cmd/todo
```

> 未在本机验证交叉编译产出在其它平台上的完整运行——回归矩阵仅 macOS arm64 / amd64 + macOS 14+。

---

## 3. 本地运行

### 3.1 数据目录

默认：`~/.todo-cli/`，首次运行自动创建。

```text
~/.todo-cli/
├── todo.db                 # SQLite 数据库（WAL 模式，0600）
├── todo.db-wal             # WAL 文件
├── todo.db-shm             # shared memory 文件
├── llm.json                # 模型 profile（0600，不含 key）
├── agents.json             # 智能体列表
├── keybindings.json        # TUI 自定义键位（可选）
├── prompts/                # 提示词模板（含版本）
└── backups/                # 升级前的自动备份
```

覆盖方式：

- `--data-dir DIR`（全局 flag）。
- 环境变量 `TODO_CLI_HOME=DIR`。

> 本机已验证：`./bin/todo --data-dir /tmp/todo-doc-test add "测试"` 在临时目录正确生成 `todo.db`。

### 3.2 CLI 与 TUI

```bash
./bin/todo add "示例"
./bin/todo list
./bin/todo            # 默认进 TUI
./bin/todo tui --no-color
```

退出 TUI：`q` 或 `ctrl+c`。

### 3.3 本地 Web 服务

```bash
./bin/todo serve                   # 默认 127.0.0.1:3210
./bin/todo serve --port 4000       # 指定端口
./bin/todo serve --open            # 启动后用默认浏览器打开
./bin/todo serve --host ::1        # 显式监听 IPv6 回环（默认拒绝其它地址）
```

开发时直接从磁盘读前端：

```bash
TODO_CLI_WEB_DIR=apps/web ./bin/todo serve --open
# 等价于 npm run dev:web
```

后台启动（建议 `tmux` / `launchd` / `nohup`）：

```bash
nohup ./bin/todo serve --port 3210 > ~/Library/Logs/todo-cli.log 2>&1 &
```

---

## 4. 生产打包

todo-cli 没有官方 Docker 镜像或安装包（首版定位为单机本地工具）。以下是常见打包方案。

### 4.1 单文件二进制 + 路径分发

```bash
go build -ldflags="-s -w" -o todo ./cmd/todo
cp todo /usr/local/bin/todo        # 或 ~/.local/bin/todo
todo version
```

- 单一可执行文件，包含前端（`apps/web` 通过 `//go:embed` 嵌入）。
- `apps/web/embed.go` 把 `apps/web/index.html` / `styles.css` / `js/` 编译进二进制；非开发态不依赖磁盘。
- 数据目录与配置文件由运行时创建在用户主目录下。

### 4.2 macOS `.app` Bundle（自封装 / 未在本机验证）

如需 `.app` 图标化启动，可写一个最小的 wrapper：

```bash
mkdir -p Todo.app/Contents/MacOS Todo.app/Contents/Resources
cp bin/Todo Todo.app/Contents/MacOS/Todo
cat > Todo.app/Contents/Info.plist <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleExecutable</key><string>Todo</string>
  <key>CFBundleIdentifier</key><string>dev.local.todo-cli</string>
  <key>CFBundleName</key><string>Todo</string>
</dict></plist>
PLIST
open Todo.app
```

> **未在本机验证**：未做 `.app` 全流程签名/公证，公开发布前请走 Apple Developer ID 流程。

### 4.3 Homebrew Formula（参考模板）

```ruby
class TodoCli < Formula
  desc "Local-first todo manager"
  homepage "https://example/todo-cli"
  url "https://example/todo-cli-v0.1.0.tar.gz"
  sha256 "…"
  depends_on "go" => :build

  def install
    system "go", "build", "-o", bin/"todo", "./cmd/todo"
  end

  test do
    system bin/"todo", "version"
  end
end
```

> **未在本机验证**：仅给出模板，未实际 tap 与 bottle。

### 4.4 容器镜像（参考 / 未在本机验证）

理论上可以打包为单文件二进制在容器中运行，但 todo-cli 监听回环且依赖宿主用户文件权限，**首版不建议**。如确需：

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN go build -o /out/todo ./cmd/todo

FROM alpine:3.20
COPY --from=build /out/todo /usr/local/bin/todo
USER 1000
ENTRYPOINT ["/usr/local/bin/todo"]
```

> **未在本机验证**：容器内 SQLite + WAL 在 overlay2 上可能需要特殊挂载参数；服务监听 `0.0.0.0` 不被支持（会被 `--host` 校验拒绝）。

---

## 5. 配置项

### 5.1 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `TODO_CLI_HOME` | `~/.todo-cli` | 数据目录覆盖 |
| `TODO_CLI_PORT` | `3210` | Web 服务首选端口；占用顺延 |
| `TODO_CLI_WEB_DIR` | （嵌入资源） | 从磁盘服务前端，开发时使用 |
| `TODO_CLI_KEYMAP` | `<data>/keybindings.json` | TUI 自定义键位 |
| `TODO_CLI_MOUSE` | 开启 | `0`/`off` 关闭鼠标上报 |
| `NO_COLOR` | 关闭 | 任意非空值关闭彩色 |
| `TODO_CLI_AGENT_CONCURRENCY` | `4` | 同时执行的智能体运行数 |
| `TODO_CLI_MODEL_PROVIDER` | profile | 临时覆盖 provider |
| `TODO_CLI_MODEL` | profile | 临时覆盖模型 |
| `TODO_CLI_MODEL_BASE_URL` | profile | 临时覆盖 base URL |
| `TODO_CLI_MODEL_PROFILE` | 默认 | 临时选择 profile |
| `TODO_CLI_MODEL_API_KEY` | — | 首选 key 来源 |
| `<profile.api_key_env>` | — | profile 指定的 key 环境变量 |
| `TODO_CLI_LLM_MODE` | `confirm` | 临时覆盖模式 |
| `TODO_AGENT_INPUT_FILE` | server 注入 | 智能体协议输入文件路径 |

### 5.2 文件配置

- `~/.todo-cli/llm.json`：profile 文件，`0600`，**不含 key**。key 仅从环境变量或钥匙串读取。
- `~/.todo-cli/agents.json`、`keybindings.json`：智能体与键位。

### 5.3 macOS 钥匙串

- Service: `todo-cli`
- Account: profile name
- 通过 `todo llm set-key` 写入；后续 `todo llm test` 等命令从钥匙串读取。

### 5.4 启动示例（写入 `~/.zshrc`）

```sh
export TODO_CLI_HOME="$HOME/.todo-cli"
# 一次性大模型 key（推荐通过钥匙串助手 `todo llm set-key` 写入）：
# export TODO_CLI_MODEL_API_KEY="…"
alias td=/path/to/bin/todo
```

---

## 6. 升级与回滚

### 6.1 升级前

```bash
todo backup                          # 在当前数据目录写一份备份
todo export -o ~/todo-export-$(date +%Y%m%d).json
```

### 6.2 升级步骤

```bash
git pull                             # 或下载新版本 tarball
go build -o bin/todo ./cmd/todo      # 重新编译
./bin/todo version                   # 确认版本号
./bin/todo status                    # 确认数据库可读、schema 自动升级
```

- 首次启动时 `packages/core/migrations.go` 会检查 schema；**自动把升级前的一份数据写入 `~/.todo-cli/backups/`**。
- 若 schema 迁移失败，事务整体回滚，新二进制仍可继续工作。
- 拒绝打开来自更新版本的数据库（`ErrSchemaTooNew`）。

### 6.3 回滚

如果新版本不能接受，立即切回旧二进制：

```bash
# 用旧 binary 直接跑，无需修改数据库
./bin/todo.old version
./bin/todo.old status
```

回滚到上一个 schema 版本：找到 `backups/todo-YYYYMMDD-HHMMSS.db`，复制回 `todo.db`（先停掉所有 `todo` 进程）。

### 6.4 数据迁移（不同机器）

```bash
todo export -o export.json
# 把 export.json 拷到新机器后：
./bin/todo import export.json                # 合并（按 updated_at 较新者胜）
./bin/todo import export.json --replace       # 替换（缺失的会软删除）
```

---

## 7. 启动后确认运行正常

### 7.1 健康检查

```bash
todo status
```

字段：`version` / `state` / `data_dir` / `database.{path,size_bytes,schema_version,latest_schema_version}` / `tasks.{total,by_status,deleted,overdue}` / `model.{status,provider,model}` / `backups.count`。

REST：

```bash
curl -s http://127.0.0.1:3210/api/health | jq .
```

期望返回：

```json
{
  "state": "ok",
  "revision": <number>,
  "schema_version": 4
}
```

### 7.2 端到端冒烟

```bash
todo add "smoke" -p high --json
todo list --json | jq '.count'
todo done $(todo list --json | jq -r '.tasks[0].id')   # 把刚才那条完成
todo undo                                                # 撤销
todo serve --port 3999 --host 127.0.0.1 &
SERVE_PID=$!
curl -s http://127.0.0.1:3999/api/health
kill $SERVE_PID
```

> 已在本机跑过 `todo add`、`todo list`、`todo status`，输出符合预期。

### 7.3 验收回归

```bash
npm install
npx playwright install chromium firefox webkit
npm run test:e2e
npm run test:e2e:report
```

报告：`reports/e2e/traceability.md` + `reports/e2e/html/` + `junit.xml`。

---

## 8. 部署到服务器或云平台

> **未在本机验证**：todo-cli 首版定位为本地工具，没有针对云平台做过端到端测试。任何云上部署都需要自接反向代理、鉴权和传输保护。

### 8.1 单机 / 个人服务器（自建 VPS / NAS）

如果需要从其他设备使用：

- 不直接暴露 `3210` 到公网。
- 在前面加 **Tailscale** / **Cloudflare Tunnel** / **WireGuard** 之类零信任通道；或在反代（nginx / Caddy）后强制 BasicAuth / OIDC。
- 反代必须把 `Host` 头保留给 todo-cli（依赖回环校验），并将流量走 HTTPS。

> todo-cli 默认仅监听 `127.0.0.1`/`::1`/`localhost`，其它地址会被拒绝；因此**必须**通过 SSH 本地端口转发（`ssh -L 3210:127.0.0.1:3210`）或隧道访问，不能直接 listen `0.0.0.0`。

### 8.2 launchd（macOS 自启动 / 守护进程）

写一个 `~/Library/LaunchAgents/dev.local.todo-cli.plist`：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.local.todo-cli</string>
  <key>ProgramArguments</key>
  <array><string>/usr/local/bin/todo</string><string>serve</string><string>--port</string><string>3210</string></array>
  <key>WorkingDirectory</key><string>/Users/you</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/you/Library/Logs/todo-cli.log</string>
  <key>StandardErrorPath</key><string>/Users/you/Library/Logs/todo-cli.log</string>
</dict></plist>
```

加载：

```bash
launchctl load -w ~/Library/LaunchAgents/dev.local.todo-cli.plist
launchctl list | grep todo-cli
```

卸载：

```bash
launchctl unload ~/Library/LaunchAgents/dev.local.todo-cli.plist
rm ~/Library/LaunchAgents/dev.local.todo-cli.plist
```

> 已在本机验证 `bin/todo` 可执行；launchd 装载方式未在本机验证完整守护流程。

### 8.3 云平台参考

- **Vercel / Render / Fly.io**：todo-cli 是 Go 二进制 + SQLite，不适合（没有持久磁盘或容器绑定）。
- **AWS Lightsail / EC2 / 阿里云 ECS**：与 §8.1 相同，走 SSH 隧道或 nginx 反代 + 鉴权；数据库放 EBS。
- **Docker**：参考 §4.4，**未在本机验证**。
- **Kubernetes**：不推荐首版使用。

---

## 9. 常见故障排查

### 9.1 `todo` 命令找不到

```bash
which todo || true                # 路径
echo "$PATH" | tr ':' '\n'        # 包含 ./bin 吗
ls -lh bin/todo                   # 构建产物存在吗
go build -o bin/todo ./cmd/todo   # 重新构建
```

### 9.2 端口占用 / 启动失败

```bash
lsof -nP -iTCP:3210 -sTCP:LISTEN  # 看占用
todo serve --port 4000            # 指定其它端口
TODO_CLI_PORT=4000 todo serve     # 等价
```

Web 服务只接受回环地址：传入 `--host 0.0.0.0` 会被拒绝（明确报错）。

### 9.3 数据目录无权限

```bash
ls -ld ~/.todo-cli                # 0700
ls -l ~/.todo-cli/todo.db         # 0600
# 若被外部改动：todo restore / todo undo 撤销最近一次破坏性操作
```

### 9.4 `version_conflict`（CLI 退 4 / API 409）

并发修改同一条任务。CLI 用 `--if-version N` 或去掉该 flag 重试；API 端通过 `GET /api/tasks/{id}` 拉取新版本后重发。

### 9.5 数据库迁移失败 / 拒绝打开

```bash
todo status --json                # 看 schema_version
ls -la ~/.todo-cli/backups/       # 是否有自动备份
# 把旧 binary + 备份恢复到原路径：
cp ~/.todo-cli/backups/todo-*.db ~/.todo-cli/todo.db
```

`ErrSchemaTooNew`：当前 binary 比数据库旧；更新 binary 或导出 + 用更新版 `todo import`。

### 9.6 模型服务不可用

`todo llm test` 与 `todo llm status` 会给出具体错误码（`llm_not_configured` / `llm_auth_failed` / `llm_not_found` / `llm_rate_limited` / `llm_server_error` / `llm_timeout` / `llm_unreachable` / `llm_bad_response`），并附中文提示、是否可重试、可用的其它 profile。**任务管理不受影响**。

钥匙串读取失败时，临时绕过：把 key 写入 `TODO_CLI_MODEL_API_KEY` 环境变量。

### 9.7 智能体运行卡住 / 失败

```bash
todo agent runs --status running,waiting_retry
todo agent show <run-id>                       # 看 attempts、exec、results
todo agent logs <run-id> [--follow]           # 实时日志
todo agent cancel <run-id>                    # 终止（SIGTERM → 等待 → SIGKILL）
```

常见原因：

- 程序不在 PATH —— 用绝对路径或 `which` 验证。
- 超时（`--timeout`）；延长或调大 `TODO_CLI_AGENT_CONCURRENCY`。
- 含 `sudo / su / doas` —— 服务拒收，**不会**自动绕开。
- 工作目录不存在或权限不足 —— 用 `--dir` 指明绝对路径。

### 9.8 网页端没数据 / 实时同步异常

- 检查事件流：`curl -N http://127.0.0.1:3210/api/events` 应看到 `ready` 与 `task` 事件。
- 数据冲突：详情面板提示「版本过期」，刷新后重试。
- 浏览器缓存：硬刷新（`shift + reload`）。
- 反向代理若启用，可能破坏 SSE / `Cache-Control: no-store`；确认没有强制缓存。

### 9.9 TUI 渲染异常

- `TERM=dumb` 会被拒；改回 `xterm-256color` / `ansi` / `iTerm2`。
- 中文/UTF-8 显示异常：终端编码设为 UTF-8，字体支持中文。
- 颜色异常：设置 `NO_COLOR=1` 或 `--no-color`。
- 鼠标异常：`--no-mouse` 或 `TODO_CLI_MOUSE=0`。

### 9.10 日志与诊断

- 进程日志：TUI 不写日志（交互阻塞时不可写）；CLI 错误写到 stderr；`serve` 在前台输出所有 HTTP/事件，可重定向到文件。
- 导出诊断包：`todo export -o ~/diagnostics.json`（不含 key/token），可附在 issue 中。
- E2E 验收报告：`npm run test:e2e:report` → `reports/e2e/html/`、`reports/e2e/traceability.md`。

---

## 10. 安全清单

- [ ] 数据目录权限 `0700`，数据库与导出文件 `0600`（自动满足）。
- [ ] 模型 key 仅来自环境变量或 macOS 钥匙串（不要写到 `llm.json`）。
- [ ] 不把 `todo serve` 暴露到 `0.0.0.0`；需外部访问时走 SSH 隧道 / Tailscale / 反代 + 鉴权。
- [ ] 不以 root 运行 `todo`（智能体会继承 root 权限，违反「用户权限」约束）。
- [ ] 升级前 `todo backup`，确认 `reports/e2e/traceability.md` 通过。
- [ ] 启动失败时优先看 `todo status --json` / `launchctl list | grep todo-cli`，不要先重启服务。
- [ ] 密钥 / token 出现在导出文件 / 日志中会被自动脱敏，但请勿主动把诊断包发到不可信渠道。