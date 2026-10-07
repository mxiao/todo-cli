# todo-cli 技术手册

> 适用版本：todo-cli v0.1.0（target platform: macOS 14+）
> 语言：中文
> 配套文档：[使用手册](USER_GUIDE.md) · [部署手册](DEPLOYMENT.md)

本文面向开发者与系统集成方，介绍 todo-cli 的整体架构、模块划分、数据模型、对外接口、配置项、开发流程，便于二次定制、性能调优与排障。

---

## 1. 架构概览

todo-cli 是一个本地优先的任务管理工具集，包含 CLI、TUI、本地 Web 服务、网页前端、大模型接入与智能体执行六大入口。所有入口共享同一份 SQLite 数据，互为同一事实来源（single source of truth）。

### 1.1 顶层组件

| 组件 | 路径 | 形态 | 职责 |
|---|---|---|---|
| `todo` 二进制 | `cmd/todo` | Go CLI | 命令行入口，分发到各子命令；解析为 TUI 或 `serve` |
| 非交互 CLI | `packages/cli` | Go 包 | flag 解析、参数校验、JSON 输出、shell 补全、启动 TUI/Web |
| 交互式 TUI | `packages/tui` | Go 包 | 终端全屏界面（键盘 + 鼠标），基于 `golang.org/x/term` |
| 本地 Web 服务 | `packages/server` | Go 包 | REST API + SSE 实时事件流 + 静态资源服务 |
| 网页前端 | `apps/web` | HTML/CSS/ESM | 浏览器任务管理界面，编译进二进制 |
| 大模型模块 | `packages/llm` | Go 包 | OpenAI 兼容客户端、权限策略、自然语言录入、版本管理 |
| 智能体模块 | `packages/agent` | Go 包 | 适配器（命令行/HTTP/Prompt/已注册）、日志、结果回写 |
| 提示词模板 | `packages/prompt` | Go 包 | 长 prompt 总结为带版本的可复用模板（`{{var}}` 占位符） |
| 任务模型与事务 | `packages/core` | Go 包 | 任务结构、SQLite 存储、版本快照、undo、冲突 |

### 1.2 协作关系

```
            ┌──────────────┐
            │  todo binary │
            └──────┬───────┘
                   │
   ┌───────────────┼────────────────┬─────────────┐
   ▼               ▼                ▼                 ▼
 packages/cli   packages/tui    packages/server   ...
   │               │                │
   └───────────────┴────────────────┘
                   │
                   ▼
            packages/core      ◀──── packages/llm
            (SQLite + WAL)           packages/agent
                   ▲                       │
                   │                       │
                   └──────── packages/prompt ┘
```

- **CLI / TUI / Web 服务** 都直接依赖 `packages/core` 完成读写，**不依赖彼此**。
- `packages/core` 是唯一与 SQLite 直接耦合的层，WAL 模式下并发读写。
- `packages/server` 周期轮询（200ms）以产生 SSE 事件；任何入口的修改都能在约 1 秒内推到浏览器。
- `packages/llm` 与 `packages/agent` 也以 `packages/core` 为基础，能读写任务并写回结果。
- 所有长耗时 I/O（HTTP、文件、命令）都在 server 进程内的后台 goroutine 中运行，不阻塞 CLI/TUI/网页操作。

### 1.3 数据共享与同步

- **同一份 SQLite**：`~/.todo-cli/todo.db`（可用 `--data-dir DIR` 或 `TODO_CLI_HOME` 覆盖）。
- **乐观锁**：每次写入要求客户端传入 `version`（CLI `--if-version N`，API `version` 字段或 `If-Match`），版本失配返回 `409`。
- **冲突保留**：被拒绝的修改以 conflict 记录保留，可在 `todo show` 或 `/api/tasks/{id}/conflicts` 看到 `base / current / yours / merged / fields`，提供 `mine`/`theirs` 二选一解决。
- **撤销栈**：每次操作在 store 层写入 undo entry；CLI 的 `todo undo` 与 Web 的「撤销」按钮共享同一栈。
- **SSE 实时事件**：`/api/events` 流式下发 `task` 事件（含 revision/actor/changes）；客户端用 `Last-Event-ID` 或 `?since=N` 重连续传。

### 1.4 入口互通的关键保证

- 同一任务标识（UUIDv4，CLI/UI 接受任意唯一前缀）。
- 同一份字段定义（见 §3 数据模型）。
- 同一状态流转规则（todo → in_progress → done → archived，可 reopen/delete/restore）。
- 同一历史记录（actor 字段区分 `cli` / `tui` / `web` / `llm` / `agent/<name>`）。

---

## 2. 目录结构与各模块职责

```
.
├── cmd/
│   └── todo/                    入口二进制，仅调用 packages/cli.Run
├── packages/
│   ├── core/                    任务模型 + SQLite 存储 + 版本/undo/冲突（其它模块的唯一依赖）
│   ├── cli/                     非交互命令行 + 启动 TUI + 启动 serve（参考点表）
│   ├── tui/                     终端交互界面（屏幕、按键、命令面板、鼠标）
│   ├── server/                  HTTP 路由 + 静态资源 + SSE 事件流
│   ├── llm/                     OpenAI 兼容客户端 + 权限策略 + 自然语言 + 决策 + 自优化
│   ├── agent/                   智能体执行：适配器、JSONL 协议、结果回写、控制信号
│   └── prompt/                  长 prompt 总结为可复用模板
├── apps/
│   └── web/                     前端 HTML/CSS/ESM，编译进 server 二进制
│       ├── index.html           入口
│       ├── styles.css           样式
│       ├── js/                  模块化 JS（app.js, api.js, ai.js, model.js, …）
│       └── test/                Node 单元测试
├── e2e/                         node-pty 终端 E2E + Playwright web E2E（保留来自原脚手架）
├── tests/e2e/                   端到端验收 + 兼容性测试（含 traceable 报告）
├── docs/                        本目录：技术手册 / 使用手册 / 部署手册
├── scripts/                     运维脚本（fix-node-pty.mjs 等）
├── bin/                         构建产物（`go build -o bin/todo ./cmd/todo`）
├── go.mod / go.work             Go 模块/工作区配置
├── package.json                 npm 脚本：test、build、test:e2e、lint、dev:web
└── playwright.config.mjs        Playwright 项目定义
```

### 2.1 模块职责细节

| 包 | 关键源文件 | 职责 |
|---|---|---|
| `packages/core` | `task.go` 任务结构、`store.go` SQLite CRUD、`txn.go` 事务封装、`versions.go` 版本快照、`undo.go` 撤销栈、`migrations.go` 自动迁移、`exchange.go` 导入导出 | 唯一权威存储 |
| `packages/cli` | `cli.go` 命令路由、`commands.go` 27 个命令实现、`interactive.go` TUI 启动参数、`serve.go` 启动 server、`agent.go` `llm.go` `ai.go` `prompts.go` 配套命令、`dates.go` 时间别名解析 | 命令行入口 |
| `packages/tui` | `app.go` 主循环、`view.go` 渲染、`input.go` 按键、`palette.go` 命令面板、`keymap.go` 自定义键位、`mouse.go` 鼠标支持、`screen.go` 屏幕布局、`form.go` 表单、`term.go` 终端能力探测 | 终端 UI |
| `packages/server` | `server.go` 监听 + 路由、`routes.go` 任务 API、`events.go` SSE、`conflicts.go` 冲突 API、`llm_routes.go` 模型 API、`agent_routes.go` 智能体 API、`static.go` 嵌入前端 |
| `packages/llm` | `client.go` HTTP 客户端、`config.go` 配置文件 + 环境变量、`policy.go` 权限模式、`intake.go` 自然语言创建、`decide.go` 决策建议、`apply.go` 接受/拒绝、`optimize.go` 自优化、`redact.go` 敏感字段、`secrets.go` 钥匙串 |
| `packages/agent` | `manager.go` 智能体生命周期、`adapter.go` 适配器接口、`cli_adapter.go` 命令行、`http_adapter.go` HTTP、`input.go` 输入 JSON 构造、`results.go` 结果回写、`select.go` 路由选择 |
| `packages/prompt` | `library.go` 模板持久化、`prompt.go` 总结与渲染 | 提示词模板 |

---

## 3. 数据模型与存储

### 3.1 任务结构（packages/core/task.go）

```go
type Task struct {
    ID          string     // UUIDv4
    Title       string     // 必填，去空白
    Description string
    DueAt       *time.Time // UTC
    Priority    Priority   // 0:none 1:low 2:medium 3:high 4:urgent
    Tags        []string   // 标准化（去 #、去重、排序）
    ParentID    string     // 父任务 ID（子任务）
    DependsOn  []string   // 依赖任务 ID 列表
    Notes       string
    Status      Status     // todo | in_progress | done | archived
    Position    float64    // 手动排序键
    CreatedAt   time.Time
    UpdatedAt   time.Time
    CompletedAt *time.Time
    ArchivedAt  *time.Time
    DeletedAt   *time.Time   // 软删除标记
    Version     int64        // 乐观锁版本号
}
```

字段约束（节选）：

- `Title` 必填，不能为空或纯空白。
- `Priority` 仅接受 `none|low|medium|high|urgent`（也接受数字 0-4 与 `p0`-`p4`）。
- `Status` 仅接受 `todo|in_progress|done|archived`（及常见别名 `open/pending/doing/complete` 等）。
- `ParentID != ID`、`DependsOn` 不包含自身。
- `Tags` 自动规范化（`NormalizeTags`）。
- `Description` / `Notes` / `Title` 都被当作自由文本，不解析嵌入指令。

### 3.2 存储引擎

- **SQLite**，通过纯 Go 驱动 `modernc.org/sqlite`，无需 cgo。
- 数据库模式：`WAL`（write-ahead logging），单写多读。
- 文件权限：数据目录 `0700`，`todo.db` 与导出文件 `0600`。
- 自动迁移：每次打开数据库会检查 `schema_version`；升级前自动在 `~/.todo-cli/backups/` 写一份当前数据库，迁移失败完全回滚；版本过新的数据库会被拒绝（`ErrSchemaTooNew`）。

### 3.3 表（核心字段）

- `tasks` — 当前任务（含 `version`、`position`、`deleted_at`）。
- `task_versions` — 每个版本完整快照（含字段值 + actor + 时间）。
- `task_history` — 变更日志：动作（`create`/`update`/`status`/`move`/`delete`/`restore`/`undo`/`agent_run`/`agent_result`…）、actor、before/after。
- `undo_log` — 每条 undo entry 引用一段操作序列，可批量回滚。
- `llm_profiles` / `llm_calls` / `llm_actions` / `llm_sessions` / `llm_session_items` — 大模型相关（不含 key）。
- `agent_runs` / `agent_run_attempts` / `agent_run_events` / `agent_results` — 智能体执行。
- `prompt_templates` / `prompt_template_versions` — 提示词模板。
- `conflicts` — 被拒绝的编辑（base/current/yours）。

### 3.4 索引与查询能力：`packages/core/query.go`

`Filter` 结构支持：

- 状态（`Statuses` 列表或 `IncludeArchived`）、优先级（`Priorities` 或 `MinPriority`）。
- 关键词 `Query`（匹配 title/description/notes/tags/category）。
- 标签 `Tags`（全部命中）、分类 `Category`、父子关系 `ParentID`（含 `none`）。
- 截止时间范围 `DueBefore` / `DueAfter` / `Overdue` / `HasDue`。
- 排序 `manual`（默认）/ `due` / `priority` / `created` / `updated`，`Reverse` 反向。
- 仅软删除 `OnlyDeleted`，限制 `Limit`。

### 3.5 状态机

```
todo ──start──▶ in_progress ──done──▶ done ──reopen──▶ todo
                       │                                        
                       └──done──▶ done                          
done ──archive──▶ archived ──reopen──▶ todo
任意 ──delete──▶ (deleted_at != nil) ──restore──▶ 原始状态
```

`applyStatus` 维护 `CompletedAt` / `ArchivedAt` 时间戳；切换回非完成态时自动清空。

---

## 4. 对外接口

todo-cli 提供三类对外接口：命令行（最常用）、HTTP REST API（前端与脚本）、LLM/Agent 的配置文件格式。

### 4.1 命令行命令（`packages/cli/cli.go`）

运行 `todo help` 可看到完整列表（已在本机运行验证）：

| 命令 | 别名 | 用途 |
|---|---|---|
| `add` | `new`, `create` | 创建任务 |
| `list` | `ls` | 列出 + 筛选 + 排序 |
| `search` | `find` | 关键词搜索 |
| `show` | `view`, `get` | 查看单条 + 子任务 + 历史 |
| `edit` | `update` | 编辑字段 |
| `done` / `start` / `reopen` / `archive` | `done` 别名 `complete` | 状态切换 |
| `delete` | `rm` | 软删除 |
| `restore` | | 恢复 |
| `priority` | `prio` | 改优先级 |
| `move` | `mv` | 重新归类或手工排序 |
| `batch` | | 批量原子操作 |
| `undo` | | 撤销最近一次修改 |
| `history` | `log` | 单条任务变更日志 |
| `export` / `import` | | JSON 全量交换 |
| `backup` | | 写一份数据库备份到数据目录 |
| `ai` | | 自然语言创建、决策、自优化 |
| `llm` | `model` | 模型服务、凭据、权限模式 |
| `agent` | `agents` | 智能体注册、启动、查看、控制 |
| `prompt` | `prompts` | 提示词模板 |
| `status` | | 数据目录、模型、运行状态 |
| `serve` | `web`, `server` | 启动本地 Web 服务 |
| `tui` | `ui`, `i` | 启动交互式 TUI |
| `keys` | `keybindings` | 查看/初始化键位 |
| `completion` | | 打印 bash/zsh/fish 补全 |
| `version` / `help` | | 版本 / 帮助 |

每个命令支持 `--json` 输出（机器可读）；错误以 `{"error":{"code":"…","message":"…"}}` 输出到 stderr。退出码：`0` ok，`1` 错误，`2` 用法，`3` not found，`4` version conflict。

### 4.2 HTTP REST API（`packages/server`）

仅监听回环地址（`--host` 接受 `127.0.0.1`/`::1`/`localhost`，其它拒绝）。所有响应 `Cache-Control: no-store`。完整接口见 `README.md` §「REST API」一节，节选：

| Method & path | Purpose |
|---|---|
| `GET /api/health` | 健康检查 + 当前 `revision` |
| `GET /api/tasks` | 列表/搜索/筛选/排序 |
| `POST /api/tasks` | 创建 |
| `GET /api/tasks/{id}` | 详情（含子任务、历史、冲突） |
| `PATCH /api/tasks/{id}` | 编辑（必须带 `version`） |
| `POST /api/tasks/{id}/status` | 状态切换 |
| `DELETE /api/tasks/{id}` + `POST …/restore` | 软删/恢复 |
| `POST /api/tasks/{id}/move` | 手工排序 |
| `POST /api/batch` | 批量原子操作 |
| `POST /api/undo` | 撤销最近一次修改 |
| `GET /api/tasks/{id}/history` | 变更日志 |
| `GET /api/tasks/{id}/versions[/{n}]` | 历史版本；`POST …/versions/{n}/revert` 回滚 |
| `GET /api/tasks/{id}/conflicts` / `GET /api/conflicts/{cid}` | 冲突列表与详情 |
| `POST /api/conflicts/{cid}/resolve` | `mine` / `theirs` 解决冲突 |
| `GET /api/events` | SSE 实时事件流 |
| `GET /api/facets` | 标签/分类汇总 |
| LLM/Agent/Prompt 端点 | 见 README.md 表格 |

### 4.3 配置文件格式

- `~/.todo-cli/llm.json` — 大模型 profiles，文件权限 `0600`，**不含 key**。示例结构（来自 `packages/llm/config.go`）：
  ```json
  {
    "active_profile": "default",
    "profiles": {
      "default": {
        "provider": "openai",
        "model": "gpt-4o-mini",
        "base_url": "https://api.openai.com/v1",
        "api_key_env": "TODO_CLI_MODEL_API_KEY",
        "mode": "confirm"
      }
    }
  }
  ```
- `~/.todo-cli/keybindings.json` — TUI 自定义键位，结构见 README.md。
- `~/.todo-cli/agents.json` — 智能体列表（也可由 `todo agent add` 维护）。
- `~/.todo-cli/prompts/` — 模板与版本。

### 4.4 智能体协议 `todo-agent/v1`

智能体通过 stdin / arg / 环境变量收到输入。详细见 README.md §"Protocol todo-agent/v1"。要点：

- 输入包含 `protocol`、`run_id`、`attempt`、`task`、`context`、`constraints`、`output_format`、`idempotency_key`、`prompt`。
- stdout 是 JSON Lines（其它行视为纯文本日志）。事件类型：`progress` / `log` / `command` / `error` / `result` / `status`。
- `result_type`（submit 见类型）：`text` / `file` / `commit` / `command_output` / `data` / `task_status`。
- 幂等键：`task id:run id:result type[+result.key]`，相同内容不重复写入。

### 4.5 时间与日期别名

`packages/core/dates.go` 接受：

- `today` / `tomorrow` / `今天` / `明天` / `后天` / `+3d` / `+1w` / `+2h`
- `fri` / `周五` / `next mon` / `下周一`
- `2026-10-09`（纯日期解析为当日 23:59:59 本地时区）
- `"2026-10-09 18:00"` / `2026-08-09T18:00:00Z`
- 仅含日期但语义模糊（如 `下周` 而非 `下周一`）会被拒绝，要求明确指定。

---

## 5. 配置项与环境变量

### 5.1 环境变量总览

| 变量 | 取值 | 默认 | 作用 |
|---|---|---|---|
| `TODO_CLI_HOME` | 路径 | `~/.todo-cli` | 数据目录 |
| `TODO_CLI_PORT` | 端口号 | `3210`（自动顺延至空闲） | Web 服务首选端口 |
| `TODO_CLI_WEB_DIR` | 路径 | （嵌入的资源） | 从磁盘服务前端，便于开发 |
| `TODO_CLI_KEYMAP` | 路径 | `<data>/keybindings.json` | TUI 自定义键位文件 |
| `TODO_CLI_MOUSE` | `0`/`off` | 开启 | 关闭鼠标上报 |
| `NO_COLOR` | 任意值 | 关闭 | 关闭彩色输出 |
| `TODO_CLI_AGENT_CONCURRENCY` | 正整数 | `4` | 同时执行的智能体运行数 |
| `TODO_CLI_MODEL_PROVIDER` | 字符串 | （profile） | 临时覆盖 provider |
| `TODO_CLI_MODEL` | 字符串 | （profile） | 临时覆盖模型名 |
| `TODO_CLI_MODEL_BASE_URL` | URL | （profile） | 临时覆盖 base URL |
| `TODO_CLI_MODEL_PROFILE` | profile 名 | （profile） | 一次性选择 profile |
| `TODO_CLI_MODEL_API_KEY` | 字符串 | — | API key（首选） |
| `<profile.api_key_env>` | 字符串 | — | profile 自定义的 key 环境变量 |
| `TODO_CLI_LLM_MODE` | `suggest`/`confirm`/`auto` | `confirm` | 临时覆盖权限模式 |
| `TODO_AGENT_INPUT_FILE` | 路径 | 由 server 注入 | 智能体协议输入文件路径 |

### 5.2 权限模式（LLM & 智能体）

| 模式 | 别名 | 行为 |
|---|---|---|
| `suggest` | 仅建议 | 模型只输出建议，不应用 |
| `confirm` | 执行前确认（默认） | 增删改、优先级、状态切换需要用户确认；删除、外发、提交、系统配置始终确认 |
| `auto` | 自动执行 | 在已授权范围内的常规操作直接执行；删除/外发/提交/系统配置仍需确认 |

无论何种模式，`sudo` / `su` / `doas` 都被拒绝；命令运行不通过 shell、不携带模型 key 环境变量。

### 5.3 敏感信息：macOS 钥匙串

`packages/llm/secrets.go` 使用 macOS 钥匙串（service `todo-cli`，account = profile name）。读取顺序：环境变量 → 钥匙串 → 错误。

---

## 6. 开发环境搭建

### 6.1 必备工具

- macOS 14+（Apple Silicon 或 Intel）
- Go 1.26+（`go version` 验证；本机已验证 `go1.26.0 darwin/arm64`）
- Node.js 20+（用于前端测试与 Playwright）
- `git`

### 6.2 首次克隆与构建

```bash
git clone <repo>
cd todo-cli
go build -o bin/todo ./cmd/todo     # 或：npm run build
./bin/todo status                   # 验证：默认数据目录 ~/.todo-cli
```

如需运行前端开发模式（从 `apps/web` 直接读磁盘）：

```bash
TODO_CLI_WEB_DIR=apps/web ./bin/todo serve --open
```

或 `npm run dev:web`。

### 6.3 常用命令

```bash
go test ./...                       # 全部 Go 单元测试
go vet ./...                        # 静态检查
gofmt -l .                          # 列出未格式化文件
npm run build                       # 同 go build
npm run test:web                    # 前端单元测试（node --test）
npm run test:e2e:unit               # 验收测试套件的子集（traceability/support）
npm test                            # go test + web + e2e:unit + tui
```

### 6.4 编辑代码后的最小自检

1. `gofmt -l .` 与 `go vet ./...` 必须为空。
2. `go test ./...` 全部通过。
3. 如修改了 `packages/core`：检查 `packages/cli`、`packages/server`、`packages/tui`、`packages/agent`、`packages/llm`、`packages/prompt` 是否仍能编译。
4. 如修改了 API 路径：同步更新 `tests/e2e/system/acceptance.spec.mjs` 的相关断言。
5. 如修改了前端：运行 `npm run test:web`。

---

## 7. 如何运行测试

### 7.1 单元测试

```bash
go test ./...                       # Go 全量单元测试（推荐主入口）
go test ./packages/core/...         # 单独跑核心层
go test -run TestStore -v ./packages/core# 单文件单测试
```

测试用 SQLite 临时文件，运行结束后自动清理。

### 7.2 端到端验收测试

```bash
npm install                         # 安装 node-pty、Playwright 等
npx playwright install chromium firefox webkit  # 一次性
npm run test:e2e                    # 全部 e2e 项目（system / terminal / chromium / firefox / webkit）
npx playwright test --project system          # 单项目
npx playwright test --grep @AT-06             # 单条验收项
TODO_E2E_CHANNELS=chrome,msedge npm run test:e2e  # 加跑本机 Chrome/Edge
npm run test:e2e:report             # 打开报告
```

E2E 用 `support/mock-model.mjs` 替代真实模型，用 `agents/mock-agent.mjs` 替代真实智能体（写真实文件、真实 git commit）；不需要真实凭据与网络。

报告产物：

- `reports/e2e/traceability.md` — 验收项 ↔ 测试 ↔ 浏览器/终端矩阵
- `reports/e2e/html/` — Playwright HTML 报告
- `reports/e2e/junit.xml` / `results.json`

### 7.3 TUI PTY 测试

`npm run test:tui`（需先 `npm run build`）通过 node-pty 真实终端跑 TUI 行为；macOS Terminal.app / iTerm2 profile。

### 7.4 跳过网络的可重现 CI

仓库没有强制外部网络；CI 默认通过 mock 模型和 mock 智能体完成验收。

---

## 8. 已知限制

| 限制 | 说明 | 影响 |
|---|---|---|
| 仅 macOS 14+ | 其它平台/版本未列入首版 |
| 用户投入精力大但未验证云平台部署 | 服务端绑定 127.0.0.1；多用户/远程部署需自行加反代/鉴权 |
| 语音录入延期 | PRD FR-801/802 明确首版不提供 |
| 无 cloud 同步 | 多台 Mac 之间不会自动同步；可用 `export`/`import` 或自建 Git 备份 |
| Windows / Linux 一等支持 | 上游 SQLite 在 Apple Silicon 通过原生优先；其它平台未列入回归矩阵 |
| LLM 调用依赖外部服务 | 服务不可用时大模型相关功能不可用，CLI/TUI/Web 任务管理不受影响（自动降级） |
| 智能体只能跑当前用户权限 | 任何越权企图均不可能 |
| 长 prompt 语法分析 | 上下文过长时自动总结（依赖 `packages/prompt`），无显式上限阈值 |
| 模型决策不自动应用 | 自优化建议永远以变更项呈现，需要用户确认（即使 `auto` 模式） |
| 网络端口 | `serve` 默认 3210，仅在本机回环地址监听，跨设备多用户需自接 |

> 验证基础：本机 `go1.26.0 darwin/arm64`，macOS 14+，SQLite WAL，`modernc.org/sqlite v1.60.1`，无 cgo，无运行时 Go 依赖；前端 `apps/web/test/*` 全部用 `node --test`（ESM，无打包）。