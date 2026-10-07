# todo-cli 使用手册

> 适用版本：todo-cli v0.1.0（macOS 14+）
> 语言：中文
> 配套文档：[技术手册](TECHNICAL.md) · [部署手册](DEPLOYMENT.md)

本文面向最终用户，介绍如何安装、配置、日常使用 todo-cli 的命令行、TUI、网页三种方式。

---

## 1. 安装与启动

todo-cli 是一个本地优先的 Go 应用，提供单一可执行文件 `todo`。

### 1.1 系统要求

- macOS 14 或更新版本（Apple Silicon 或 Intel）
- 可选：Node.js 20+（仅在使用 `npm run test:*` 系列时需要）

### 1.2 首次安装

从仓库构建：

```bash
git clone <repo-url> todo-cli
cd todo-cli
go build -o bin/todo ./cmd/todo
```

或使用 npm 脚本：

```bash
npm install
npm run build
```

构建完成后建议把 `bin/todo` 加入 PATH：

```bash
echo 'export PATH="$PWD/bin:$PATH"' >> ~/.zshrc   # bash 改成 ~/.bashrc
```

首次执行会在 `~/.todo-cli/` 下自动建立数据目录与 `todo.db`：

```bash
todo status            # 显示数据目录、数据库、模型、运行状态
```

预期输出（JSON 形式，加 `--json` 才有）类似：

```
{
  "version": "0.1.0",
  "state": "ok",
  "data_dir": "/Users/<you>/.todo-cli",
  ...
}
```

### 1.3 安装路径的选择

- **官方仓库构建**：使用上面的 `go build` 流程。
- **直接下载 release 包**（如维护者已发布）：把 `todo` 可执行文件放到 `~/.local/bin/` 或 `/usr/local/bin/`。

> 本机验证：`./bin/todo status` 与 `./bin/todo add "示例" -p high` 已在本机运行成功。

---

## 2. 快速开始：5 分钟跑通

### 2.1 第一次创建任务

```bash
todo add "写周报" -p high --due tomorrow -t work,weekly -c office -n "附上指标"
```

预期输出：

```
Created 1a3f7c9b-... 写周报 (high) due 2026-10-08 (work,weekly; office)
```

说明：

- `1a3f7c9b-...` 是任务 ID，CLI 接受任意**唯一前缀**（比如 `todo show 1a3f`）。
- `--due tomorrow` / `--due 下周一` / `--due 2026-10-09` 都是合法写法。
- `-t` 后的标签会按 `#` / 大小写 / 重复归一化。

### 2.2 列出、搜索

```bash
todo list                          # 默认按手工排序
todo list --status todo,in_progress --priority high,urgent --sort due
todo list --overdue --category office --reverse
todo search 周报 --tag work
```

输出形如：

```
1a3f7c9b  写周报      high    due 2026-10-08  work,weekly (office)
3f2a0b6d  整理项目    medium  due 2026-10-09  work (office)
2 task(s)
```

### 2.3 编辑、状态切换、撤销

```bash
todo edit 1a3f --title "新标题" --due "2026-10-09 18:00" --tag extra --untag old --if-version 3
todo done 1a3f 3f2a               # 多条一起完成
todo start 1a3f                   # 标记进行中
todo reopen 1a3f                  # 重新打开
todo archive 1a3f                 # 归档
todo delete 1a3f                  # 软删除（可恢复）
todo undo                         # 撤销最近一次改动
```

### 2.4 打开 TUI 或网页

```bash
todo                                # 默认进 TUI
todo tui                            # 同上
todo serve                          # 启动 127.0.0.1:3210
todo serve --open                   # 启动并打开默认浏览器
```

---

## 3. 三种入口的关系

```
                        ┌─────────────┐
   ┌──── 命令行 ───────▶│             │
   │                   │  SQLite DB  │◀─────── 网页前端（REST + SSE）
   ├──── TUI ──────────▶│ ~/.todo-cli │──── 智能体回写结果
   │                   │  ·todo.db   │
   └──── ai / llm / ───┤             │
       agent           └─────────────┘
```

- 三种入口共享同一份数据，任何一处修改在其它入口约 1 秒内可见。
- 修改都进入同一个历史栈，按 `u` / `todo undo` / 网页的「撤销」按钮都能撤销。
- 任务 ID 在三端完全一致，可用前缀匹配。

---

## 4. 命令行使用详解

### 4.1 任务增删改

| 命令 | 例子 | 说明 |
|---|---|---|
| `todo add <title> [flags]` | `todo add "写周报" -p high --due tomorrow -t work` | 标题必填；其余字段用 flag |
| `todo edit <id> [flags]` | `todo edit 1a3f --title "新标题" --if-version 3` | 乐观锁：`--if-version` 失败返回 `409 version_conflict` |
| `todo show <id>` | `todo show 1a3f` | 详情 + 子任务 + 历史 |
| `todo done/start/reopen/archive/delete/restore <id>...` | `todo done 1a3f 3f2a` | 一次多条 |
| `todo priority <level> <id>...` | `todo priority urgent 1a3f` | `none/low/medium/high/urgent` |
| `todo move <id>... [--before X | --after X | --top | --bottom]` | `todo move 1a3f --top` | 改手动顺序 |
| `todo move <id>... [--category C | --parent P]` | `todo move 1a3f --category home` | 重新归类/父任务 |
| `todo batch <verb> <id>...` | `todo batch done 1a3f 3f2a` | 原子批量（一次 undo） |

### 4.2 列表、搜索、筛选

```bash
todo list \
  --status todo,in_progress \
  --priority high,urgent \
  --tag work --tag weekly \
  --category office \
  --overdue \
  --sort due --reverse \
  --limit 20
```

常用筛选：

- `--status` 接受 `todo,in_progress,done,archived` 任意组合，或 `all`。
- `--priority` 接受逗号分隔的多个值；`--min-priority` 取最小阈值。
- `--overdue` / `--has-due` / `--no-due` 互斥（`--has-due` 与 `--no-due` 互斥）。
- `--deleted` 只看回收站；`--all` 包含已归档。
- `--sort` 接受 `manual`（默认）/`due` / `priority` / `created` / `updated`。

`todo search <keywords>` 等价于 `todo list --search <keywords>`，并允许在关键词之间继续追加更多搜索词。

### 4.3 时间别名

| 写法 | 含义 |
|---|---|
| `today` / `tomorrow` / `今天` / `明天` / `后天` | 相对今天/明天 |
| `+3d` / `+1w` / `+2h` | 相对当前时刻 |
| `fri` / `周五` / `next mon` / `下周一` | 周缩写（中英） |
| `2026-10-09` | 当日 23:59 本地时区 |
| `"2026-10-09 18:00"` | 本地时区具体时间 |
| `2026-08-09T18:00:00Z` | RFC 3339 / ISO 8601 |

模糊时间（如 `下周` 而非 `下周一`）会被要求明确；任何关键时间字段都会在 `todo show` / TUI 详情中显示解析结果。

### 4.4 撤销、导入导出、备份

```bash
todo undo                                 # 撤销最近一次改动（任意入口）
todo undo                                 # 再按一次回到更早的状态
todo export -o backup.json                # 全量导出（含已删任务与历史）
todo import backup.json [--replace]       # 合并（按 updated_at 较新者胜）或替换
todo backup                               # 写一份数据库备份到数据目录
```

### 4.5 Shell 补全

```bash
# zsh
eval "$(todo completion zsh)"
# bash
eval "$(todo completion bash)"
# fish
todo completion fish > ~/.config/fish/completions/todo.fish
```

补全覆盖命令、flag、flag 参数值（优先级、状态、排序、日期别名）和任务 ID + 标题。

### 4.6 JSON 与退出码

任何命令加 `--json` 输出机器可读 JSON；错误写到 stderr：

```json
{"error":{"code":"version_conflict","message":"task 1a3f is at version 5, expected 3"}}
```

退出码：`0` ok / `1` 错误 / `2` 用法 / `3` not found / `4` version conflict。

---

## 5. TUI（终端交互界面）使用

### 5.1 进入

```bash
todo              # 终端会话中默认进 TUI
todo tui          # 等价别名 todo ui, todo i
todo tui --no-mouse --no-color     # 关鼠标、关颜色
todo tui --keymap ~/my-keys.json   # 自定义键位
```

支持的终端：macOS 系统终端（Terminal.app）、iTerm2、Ghostty 与其它 xterm 兼容终端。`TERM=dumb` 被拒绝（清晰报错），`NO_COLOR=1` 与 UTF-8/CJK 受支持。

### 5.2 主界面
- 顶栏：状态选项卡（全部/待办/进行中/已完成/已归档）。
- 左侧：任务列表；窄屏只显示一个面板。
- 右侧：详情（子任务、历史、操作来源：`命令行/终端界面/网页`）。
- 底部：可点击快捷键提示。

### 5.3 常用键

| 按键 | 作用 |
|---|---|
| `↑↓` / `j k`、`g G`、`PgUp PgDn` / `ctrl+u` `ctrl+d` | 移动选中 |
| `tab` | 切换列表/详情面板；`enter` / `l` 进详情；`esc` / `h` 返回 |
| `a` / `n`、`N` | 新建任务 / 新建子任务（支持快速语法：`写周报 #work @office !high due:tomorrow`） |
| `e` | 编辑表单（`tab` 补全标签/分类/优先级/日期，`ctrl+s` 保存） |
| `x` / `space`、`s`、`A`、`d` | 完成 ↔ 重开、开始、归档、删除 |
| `+` `-`、`K` `J` | 升降优先级；上下移 |
| `u` | 撤销最近一次改动 |
| `/` | 实时搜索（`esc` 还原，`tab` 补全） |
| `f`、`1`–`5`、`[` `]`、`c` | 筛选菜单、状态选项卡、清筛选 |
| `o`、`O` | 排序循环、反向 |
| `:` / `ctrl+p` | 命令面板（含 `add` `search` `filter` `sort` `goto` 等） |
| `m` | 操作菜单（右键菜单的键盘版本） |
| `?` / `F1` | 键位帮助（显示当前生效、含自定义标记） |
| `q` / `ctrl+c` | 退出 |

### 5.4 鼠标操作

在 macOS Terminal.app、iTerm2、Ghostty 等带鼠标上报的终端中：

- 单击选中任务；双击打开详情；右键打开操作菜单。
- 滚轮滚动列表或详情。
- 选项卡、底部快捷键提示、详情按钮、菜单按钮、对话框按钮可点击。

在无鼠标的终端中，所有键盘路径必须完整可用（`--no-mouse` 强制关闭鼠标）。

### 5.5 自定义键位

```bash
todo keys                 # 列出当前生效键位（`*` 表示自定义）
todo keys --init          # 把默认键位写到 <data>/keybindings.json 供编辑
```

文件示例：

```json
{"new": ["ctrl+n", "a"], "delete": "D", "archive": []}
```

- 写法：`a` / `G` / `?` / `ctrl+n` / `alt+x` / `shift+tab` / `enter` / `esc` / `space` / `delete` / `pgup` / `f1`。
- 冲突的键会被提示并自动移走。
- 文件路径可通过 `TODO_CLI_KEYMAP` 或 `--keymap` 覆盖。

### 5.6 冲突处理

你在 TUI 中编辑某个任务时，如果别的端同时改动了它，保存会报冲突并保留你的输入；可保存一次（按版本号覆盖）或先刷一次再编辑。

---

## 6. 网页界面使用

### 6.1 启动

```bash
todo serve                  # 监听 127.0.0.1:3210（端口占用会自动顺延）
todo serve --port 4000      # 指定端口
todo serve --open            # 自动打开浏览器
```

默认监听 `127.0.0.1`（也可 `--host ::1` 或 `--host localhost`），其它地址会被拒绝。服务本身**不提供鉴权**（仅本机访问）。其它源的请求（DNS rebinding）会被 403。

### 6.2 任务管理

页面顶部：

- **状态选项卡**：全部 / 待办 / 进行中 / 已完成 / 已归档 / 回收站。
- **筛选**：关键词搜索、优先级、标签、分类、截止时间（overdue / today / 7 days / 有/无 due）。
- **排序**：手工 / 截止 / 优先级 / 创建 / 更新（可反向）。**所有筛选状态都体现在 URL**（刷新/书签后保持）。

页面中部：

- **列表**：任务标题、截止时间、优先级、标签、分类。
- **多选**：复选框 + shift-click 选区 + 全选；批量完成/开始/重开/归档/删除/恢复/改优先级/移动分类——所有批量操作是**原子**的，对应一个 undo 步骤。

页面右侧（详情面板）：

- 完整字段 + 子任务 + 完整变更历史（actor 标注 `命令行/终端界面/网页`）。
- 编辑、完成、开始、归档、删除、恢复、优先级 `+`/`-`、移动。

顶部操作：

- **新建**：支持快速语法（如 `写周报 #work @office !high due:tomorrow`）与完整表单。
- **撤销**：所有改动以 toast 显示撤销按钮，按 `u` 也行。

快捷键：`n`/`N` 新建、`/` 搜索、`j`/`k` 移动、`Enter` 详情、`e` 编辑、`x` 完成、`s` 开始、`A` 归档、`d` 删除、`space` 多选、`K`/`J` 排序、`u` 撤销、`?` 帮助。

### 6.3 模型与智能体面板（`i` 打开 AI 助手）

- **创建任务**：自然语言描述 → 解析为字段 → 逐条预览接受 / 一次全接受 / 拒绝 / 撤销。
- **辅助决策**：摘要 + 风险 + 建议顺序与理由 + before/after diff；接受/拒绝/编辑/撤销均可；`重新决策` 让模型再次决策。
- **智能体运行**：状态、阶段、进度、起止时间与耗时；最终 prompt；尝试；结果；实时日志（输出/命令/错误）；暂停/继续/取消/重试（可带补充上下文）/确认/拒绝。
- **提示词模板**：原文、结构化总结、模板正文；带变量复用（可预览或直接发起智能体任务）；复制；总结新 prompt。
- **模型与智能体**：profile 切换、连接测试、权限模式与确认清单、智能体增删改（适配器、命令、目录、环境变量名、超时、I/O 模式）、可用适配器列表。
- **执行历史**：智能体运行、模型会话（可点开继续操作）、命令/智能体操作日志。

> 模型相关功能面板页面里**只显示是否已配置 key 与 key 来源**（环境变量 / 钥匙串），从不显示 key 本身。

### 6.4 实时同步与冲突

- 其它端改动后，约 1 秒内本页刷新；重新聚焦或刷新页面也会拉新数据。
- 编辑带 `version`；若版本过期会弹冲突对话框，按 base/theirs/yours 处理后保存。
- 未保存的表单输入存在 `localStorage`，关闭再打开不丢。

---

## 7. 大模型相关功能

todo-cli 的所有大模型相关入口都是**可选**的：未配置或服务不可用时，CLI/TUI/Web 的任务管理能力不受影响，相关面板提示「不可用」。

### 7.1 配置模型服务

任意 OpenAI 兼容服务：OpenAI、DeepSeek、OpenRouter、Moonshot、DashScope、Ollama、LM Studio 或自建 `--base-url`。

```bash
todo llm set --provider deepseek --model deepseek-chat   # 切到配置
todo llm set --profile local --provider ollama --model qwen2.5 --no-key
todo llm set-key                                       # 从 stdin 读取并存到钥匙串
todo llm test                                          # 测试连通性；失败给出原因/重试/切换建议
todo llm use local                                     # 切换激活 profile
todo llm status                                        # 查看 profile / 模型 / 模式 / 确认清单
```

> key 优先来自环境变量 `TODO_CLI_MODEL_API_KEY` 或 profile 指定的 `--key-env`，再来自 macOS 钥匙串（service `todo-cli`），**文件配置不含 key**。

环境变量临时覆盖：

```bash
TODO_CLI_MODEL_PROVIDER=openai TODO_CLI_MODEL=gpt-4o-mini \
  TODO_CLI_MODEL_BASE_URL=https://api.openai.com/v1 TODO_CLI_LLM_MODE=auto todo status
```

### 7.2 权限模式

```bash
todo llm mode suggest    # 仅建议（最保守）
todo llm mode confirm    # 执行前确认（默认）
todo llm mode auto       # 自动执行（已授权操作直接做）
```

- `auto` 模式下，模型可直接创建/修改任务、改优先级、改截止、改状态、改依赖、改顺序；但**删除、外发、提交、系统配置**始终需要确认。
- `sudo` / `su` / `doas` 在任何模式下都被拒绝。
- `todo llm confirm add "npm publish"` 把特定命令加入确认清单；`todo llm confirm add agent.start` 按类型加入；`todo llm confirm add shell` 按类别加入。
- `todo llm commands off` 关闭模型提议命令的路径。

### 7.3 自然语言创建任务

```bash
todo ai add 明天下午前完成发布准备：先整理需求，再更新页面，最后检查链接
todo ai add --select 3f2a 这个很急，再加个子任务：收集数据   # 修改/子任务于选中项
todo ai answer <session> 周五 18:00                          # 回答模型追问
todo ai edit <session> 2 --title … --priority high
todo ai apply <session> [1 2]                               # 接受一条/多条/全部
todo ai reject <session>                                    # 拒绝
todo ai undo <session>                                      # 撤销接受的项
```

输出包含新增 / 修改 / 未执行 / 失败的统计。**所有由大模型产生的变更都记入任务历史，actor 为 `llm`**，摘要含 session id。

### 7.4 大模型决策

```bash
todo ai decide [--select ID]            # 摘要 + 风险 + 顺序（含理由/关注点/耗时预估）+ 变更 diff
todo ai redecide <session> 太激进了     # 反馈后重新决策
todo ai optimize                        # 模型给出针对自身的优化（不自动生效，可 rollback/restore）
todo ai config versions|restore|…        # 配置版本管理
```

每条建议都是带 before/after 的可审查项；自动模式下，模型仅对已授权操作自动应用。

### 7.5 自优化建议

模型会基于执行记录分析失败模式（歧义、工具选择、参数缺失），产出改进建议。**自优化建议默认永不自动应用**（即使 `auto` 模式），通过 `todo ai config versions` / `restore` / `rollback` 管理版本。

---

## 8. 智能体执行

### 8.1 注册智能体

```bash
# 命令行适配器：本地脚本，不通过 shell
todo agent add coder --desc 写代码 --dir ~/src/app \
  --env GITHUB_TOKEN --timeout 20m --success-codes 0 \
  --input json-stdin --output jsonl --max-retries 1 \
  -- my-agent --task {{task_id}}

# 调用本机已安装的其它智能体
todo agent add claude --input prompt-stdin --output text -- claude -p

# HTTP 适配器
todo agent add ci --adapter http --url https://ci.example/run \
  --header-env Authorization=CI_TOKEN

# 提示词智能体（用模型 profile 本身作为智能体）
todo agent add writer --adapter llm --desc 写文档
```

`--adapter` 可选内置名（`cli` / `http` / `llm`）或在 Go 中通过 `agent.Register("name", factory)` 注册的适配器；未知适配器返回「能力不可用」而非假成功。

### 8.2 启动与监控

```bash
todo agent run 1a2b --agent coder                                       # 启动
todo agent run 1a2b --agent auto --template 周报分析 --var 产品=FlowOS  # 模型自动选择 + 模板
todo agent run 1a2b --dry-run                                          # 只显示最终 prompt
todo agent run 1a2b --detach                                           # 后台 worker

todo agent runs [--task 1a2b] [--status running,failed]
todo agent show r3f2
todo agent logs r3f2 [--follow] [--kind output,command,error]
todo agent prompt r3f2                                                 # 查看最终 prompt
todo agent pause r3f2 | resume r3f2 | cancel r3f2 | retry r3f2
todo agent confirm r3f2 | reject r3f2                                  # 需确认的智能体
```

**运行状态**：`queued` / `waiting_confirmation` / `running` / `paused` / `waiting_retry` / `succeeded` / `partial` / `failed` / `cancelled` / `unknown`。

**结果回写类型**（智能体 stdout JSONL 中的 `result_type`）：

- `text` — 文本结果
- `file` — 文件（路径 + 大小 + SHA-256，是否在工作目录之外）
- `commit` — 代码提交（仓库 + 分支 + 哈希）
- `command_output` — 命令执行记录（argv + 退出码 + 输出）
- `data` — 结构化数据
- `task_status` — 任务状态变更（如 `local_status: "done"`）

**安全**：命令运行不通过 shell，不携带模型 key 环境变量；`sudo`/`su`/`doas` 拒收；超时、被杀、退出码非 0、已写入结果后失败 → 状态分别为 `unknown` / `unknown` / `failed` / `partial`，智能体**不会自动**把任务标记为完成。

### 8.3 结果查看

```bash
todo agent results 1a2b                                     # 所有结果（含来源智能体/时间/版本）
todo agent writeback 1a2b '{"result_type":"commit",...}'    # 外部写回结果
```

所有结果都附加到任务历史（actor 为 `agent/<name>`），幂等键 `task id:run id:result type[+key]` 防止重复。

---

## 9. 提示词模板

```bash
todo prompt summarize long-prompt.md --save --name 周报分析   # 总结为模板
todo prompt show 周报分析 [--original] [--version 1]
todo prompt versions 周报分析
todo prompt edit 周报分析 --step … --constraint … --body-file body.md
todo prompt rollback 周报分析 [1]
todo prompt copy 周报分析 [--clipboard]
todo prompt export 周报分析 --format md|txt|json [-o file]
todo prompt render 周报分析 --var 产品=FlowOS
todo prompt task 周报分析 --var 产品=FlowOS                   # 用模板生成智能体任务
```

模板结构包括：目标、上下文、约束、执行步骤、输出格式、变量占位符 `{{var}}`。

---

## 10. 常见问题（FAQ）

**Q1：`todo` 命令找不到。**
A：构建产物在 `./bin/todo`。要么把 `./bin` 加进 `PATH`，要么直接用 `./bin/todo`。`dev:web` / `npm run build` 也会写到 `bin/`。

**Q2：我能在两台 Mac 之间同步任务吗？**
A：首版不内置云同步。两种方法：`todo export -o file.json` + `todo import file.json`，或把 `~/.todo-cli/` 整个放到 iCloud/Dropbox（注意数据库文件锁；建议先 `todo backup` 后复制）。多端同时写入可能产生冲突，按 `mine`/`theirs` 解决。

**Q3：网页的端口被占用怎么办？**
A：`todo serve` 默认在 `3210`，占用时会顺延到下一个可用端口并打印。也可指定 `todo serve --port 4000` 或 `TODO_CLI_PORT=4000 todo serve`。

**Q4：模型服务不可用会影响任务管理吗？**
A：不会。CLI/TUI/Web 的任务管理与智能体执行**不依赖**模型；模型相关入口会清晰显示「不可用」，决策自动降级为本地排序规则，提示词总结降级为本地行规则（都标记 degraded）。

**Q5：我装在 Linux 上能用吗？**
A：首版仅以 macOS 14+ 为基线验证，Windows/Linux 不在回归矩阵中。Go 代码本身可移植，但需在目标平台重新构建并复测。

**Q6：模型 key 存在哪里？安全吗？**
A：环境变量（不落盘）或 macOS 钥匙串（service `todo-cli`）；`~/.todo-cli/llm.json` 仅存 profile 配置（不含 key），文件权限 `0600`。导出的 JSON、运行日志、会话内容都默认脱敏（key/token/`Bearer …`/`*TOKEN*`/`*KEY*` 变量值）。

**Q7：TUI 没有鼠标怎么办？**
A：`todo tui --no-mouse` 或 `TODO_CLI_MOUSE=0`，所有操作都有等价键位。在支持鼠标的终端里鼠标是可选项，不存在只靠鼠标才能用的功能。

**Q8：我误删了任务，能恢复吗？**
A：删除是软删除。`todo restore <id>` 恢复；`todo list --deleted` 看回收站；`todo undo` 撤销最近一次操作；网页端在「回收站」选项卡里恢复。

**Q9：智能体能联网或装软件吗？**
A：智能体**只能以你当前 macOS 用户的权限运行**；`sudo` / `su` / `doas` 被拒。`auto` 模式下模型仍不可执行删除、外发、提交、系统配置等操作——这些始终要你确认。

**Q10：模型会自动升级自己吗？**
A：不会。自优化建议（`todo ai optimize`）仅以变更项呈现，**永不自动应用**（即使 `auto` 模式），并支持版本回滚。

**Q11：我想用其他语言提问模型，行吗？**
A：自然语言录入、决策、提示词模板都不限于中文，模型按你给的语种响应。日期别名支持中英（`明天` / `tomorrow`、`周五` / `fri`）。

**Q12：怎么升级？**
A：用同样的方式重新 `git pull` + `go build` 即可；首次启动会自动备份当前数据库到 `~/.todo-cli/backups/`，迁移失败可回滚。详见 [部署手册 §6](DEPLOYMENT.md#6-升级与回滚)。

**Q13：语音录入呢？**
A：PRD FR-801/802 明确首版不提供；后续加入时会复用现有任务流程，不会单独建一份数据。

**Q14：网页能暴露到局域网/公网吗？**
A：默认仅监听 `127.0.0.1`。如必须外部访问，请自接 nginx / Caddy / Tailscale Funnel，并在前面加鉴权；todo-cli 内置服务**不**提供鉴权。