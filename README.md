# todo-cli

基于命令行的todo工具，支持web命理用网页打开。比较偏向现代cli，可以通过键盘鼠标快速执行，也可以通过大模型来创建任务。打开cli后，大模型可以帮我决策做什么，并且可以启动对应的智能体帮我完成任务，也可以通过语音输入来完成一些录入，补充等操作，决策上比较简单。

## 文档

- [使用手册（面向最终用户）](docs/USER_GUIDE.md) — 安装、快速开始、CLI/TUI/Web 三种入口、自然语言创建、决策、智能体执行、提示词模板、FAQ。
- [技术手册（面向开发者）](docs/TECHNICAL.md) — 架构、模块职责、数据模型、对外接口、配置项、开发与排障。
- [部署手册（面向运维）](docs/DEPLOYMENT.md) — 环境要求、构建、本地运行、生产打包、升级/回滚、故障排查。

由 FlowOS 初始化；代码由各开发任务的 PR 加入。

## Layout

```
cmd/todo/          `todo` binary entry point
packages/core/     task model + SQLite store (single source of truth for every entry point)
packages/cli/      non-interactive CLI (flags + --json), shell completion, `todo tui` launcher
packages/tui/      interactive full-screen terminal UI (keyboard + mouse)
packages/server/   local web service: REST API + live change events over the same store, serves apps/web
packages/llm/      OpenAI-compatible model client, permission policy, NL intake, decisions, self-optimisation
packages/prompt/   prompt summaries → versioned, reusable templates with {{variables}}
packages/agent/    agent runs: adapters (command line, HTTP, prompt agent, registered), logs, control, result write-back
apps/web/          browser task manager + model/agent views (plain HTML/CSS/ES modules, embedded into the binary)
e2e/               node-pty end-to-end tests of the TUI; e2e/web: Playwright tests of the web UI
tests/e2e/         end-to-end acceptance & compatibility suite (`npm run test:e2e`, see tests/e2e/README.md)
```

Model features (`packages/llm`, `packages/prompt`) and the agents (`packages/agent`) build
on top of `packages/core`. All of them share the same task IDs, fields and status rules.

### Stack decision

The task brief proposed Node.js 20 + TypeScript. The repository was already scaffolded as a
Go module (`go.mod`, Go CI, `go test ./...` as the verification gate), so this release is
written in Go. It uses the same `packages/core` / `packages/cli` split and SQLite through
the pure-Go `modernc.org/sqlite` driver, which needs no cgo and no runtime dependencies.
`npm test` runs `go test ./...` and the node-pty TUI suite. Target platform: macOS 14+.

## Build & test

```bash
go build -o bin/todo ./cmd/todo   # or: npm run build
go test ./...                     # Go unit tests + TUI tests on a real pseudo-terminal
npm install && npm test           # go test ./..., web module unit tests and the node-pty TUI suite (e2e/)
npx playwright install chromium firefox webkit   # once
npm run test:e2e                  # acceptance + compatibility suite (tests/e2e) on Chromium, Firefox, WebKit,
                                  # the TUI on node-pty (Terminal.app / iTerm2 profiles), CLI/API/data checks,
                                  # and the web UI regression suite (e2e/web, Chromium)
TODO_E2E_CHANNELS=chrome,msedge npm run test:e2e   # also the installed branded Chrome / Edge
npm run test:e2e:all              # regression suite on all three engines as well
```

`npm run test:e2e` runs the real binary with a fixed mock model (OpenAI-compatible) and fixed mock agent
scripts, so no model key or network is needed. It writes a traceable report to `reports/e2e/`
(`traceability.md`: acceptance items → requirements → tests per browser/terminal, measured sync latency,
compatibility matrix, deferred items; plus HTML, JUnit and JSON reports).

`npm install` pulls `node-pty` (dev only) and restores the executable bit on its prebuilt macOS
`spawn-helper`. Without `node-pty` the node suite is skipped; the Go PTY tests still cover the same flows.

## Data

- One SQLite database: `~/.todo-cli/todo.db`. Override it with `--data-dir DIR` or `TODO_CLI_HOME`.
- The data directory is `0700` and database/backup files are `0600`.
- Writes are transactional: one command, including a batch, is one transaction. They are all-or-nothing.
- Every task has a `version` (optimistic locking via `todo edit --if-version N` or the API's `version`), a full
  change history and a stored snapshot of every version.
- Delete is a soft delete (`todo restore`, `todo list --deleted`).
- `todo undo` reverts the most recent change (single edit, batch or import). Repeat it to step further back.
- Schema migrations run automatically on open. Before an existing database is upgraded, a copy is written
  to `~/.todo-cli/backups/`. A failed migration rolls back completely. A database from a newer
  version is refused.
- `todo export` / `todo import [--replace]` exchange the full data set (tasks, including deleted ones,
  plus history) as JSON. An import merges by task id: newer `updated_at` wins. With `--replace`, tasks
  missing from the file are soft-deleted.

## Usage

```bash
todo add "写周报" -p high --due tomorrow -t work,weekly -c office -n "附上指标"
todo add "Slides" --parent 1a2b          # ids may be any unique prefix
todo list --status todo,in_progress --priority high,urgent --tag work --sort due
todo list --due-before fri --overdue --category office --sort priority --reverse
todo search 周报 --tag work
todo show 1a2b                           # details, subtasks, history
todo edit 1a2b --title "新标题" --due "2026-10-09 18:00" --tag extra --untag old --if-version 3
todo done 1a2b 3c4d   |  todo reopen 1a2b  |  todo start 1a2b  |  todo archive 1a2b  |  todo delete 1a2b
todo move 1a2b --before 3c4d             # manual order (--after / --top / --bottom)
todo batch done 1a2b 3c4d 5e6f
todo batch move 1a2b 3c4d --category home --parent none
todo batch priority urgent 1a2b 3c4d
todo batch archive 1a2b 3c4d
todo undo
todo export -o backup.json  |  todo import backup.json [--replace]
todo status                              # data dir, database, model config, runtime
```

Due times accept `today`, `tomorrow`, `今天`, `明天`, `后天`, `+3d`, `+1w`, `+2h`, weekdays
(`fri`, `周五`, `next mon`, `下周一`), `2026-10-09`, `"2026-10-09 18:00"` and RFC 3339. They are
interpreted in the local time zone. A date without a time means the end of that day.

Sorts: `manual` (default), `due` (soonest first, undated last), `priority` (most urgent first),
`created` / `updated` (newest first). `--reverse` flips the order.

### JSON & exit codes

Add `--json` to any command for machine-readable output on stdout. Errors go to stderr as
`{"error":{"code":"…","message":"…"}}`. Exit codes: `0` ok, `1` error, `2` usage,
`3` not found, `4` version conflict.

### Model configuration

Any OpenAI-compatible service works (OpenAI, DeepSeek, OpenRouter, Moonshot, DashScope, Ollama,
LM Studio or a custom `--base-url`); the default is `openai` / `gpt-4o-mini`. Profiles live in
`<data dir>/llm.json` (mode 0600) and **never contain a key**: the key comes from an environment
variable first (`TODO_CLI_MODEL_API_KEY`, or the profile's `--key-env`), then the macOS keychain
(service `todo-cli`, account = profile name).

```sh
todo llm set --provider deepseek --model deepseek-chat   # change the active profile
todo llm set --profile local --provider ollama --model qwen2.5 --no-key
todo llm set-key                       # reads the key from stdin into the keychain
todo llm test                          # connection test; on failure: retry, or switch:
todo llm use local                     # switch the active profile
todo llm status                        # model, key source (never the key), mode, confirm list
```

`TODO_CLI_MODEL_PROVIDER`, `TODO_CLI_MODEL`, `TODO_CLI_MODEL_BASE_URL`, `TODO_CLI_MODEL_PROFILE` and
`TODO_CLI_LLM_MODE` override the profile for one process. Failures are classified
(`llm_not_configured`, `llm_auth_failed`, `llm_not_found`, `llm_rate_limited`, `llm_server_error`,
`llm_timeout`, `llm_unreachable`, `llm_bad_response`) with a Chinese hint, whether a retry may help
and which other profiles exist. Without a usable model, task management, the TUI and the web UI work
unchanged; decisions fall back to local ranking rules and prompt summaries to local line rules
(both marked as degraded).

**Privacy.** Only `title`, `description`, `tags`, `status` and `priority` are sent by default
(decisions add `due_at` and `depends_on`; change with `todo llm fields`). Tasks are referenced as
`T1…`, never by id. Keys, tokens, passwords and `Bearer …` values are redacted from everything sent
to the model and from everything stored (sessions, call log, command output, templates).

### Permission modes

`todo llm mode suggest|confirm|auto` (仅建议 / 执行前确认 — default / 自动执行). In auto mode
authorised operations run without asking: creating tasks and changing priority, due time, status,
dependencies and order. Deleting, overwriting a title/description, sending data out (`curl`,
`git push`, …), committing and system configuration (`defaults`, `launchctl`, `brew install`, …)
always wait for confirmation; `todo llm confirm add "npm publish"` (or a kind such as `agent.start`,
or a category such as `shell`) adds more. `sudo`/`su` are refused in every mode: commands run without
a shell, with the user's own permissions and without the model keys in their environment.
`todo llm commands off` stops the model from proposing commands; `todo llm agent add coder -- claude -p`
registers a command-line agent the model may start (the task prompt goes to stdin and is shown first).

### Natural language and decisions (`todo ai`)

```sh
todo ai add 明天下午前完成发布准备：先整理需求，再更新页面，最后检查链接
todo ai add --select 3f2a 这个很急，再加个子任务：收集数据   # update / subtask of the selection
todo ai answer <session> 周五 18:00      # the model asks at most once when key facts are missing
todo ai edit <session> 2 --title … --due "明天 18:00" --priority high --tag web --dep N1
todo ai apply <session> [1 2]            # accept one/several/all · todo ai reject … · todo ai undo …
todo ai decide [--select ID]             # summary, risks, order with reasons/focus/estimate, changes
todo ai redecide <session> 太激进了       # reject and decide again with feedback
todo ai optimize                         # the model reviews its own results and suggests changes
todo ai config versions|restore <v>|rollback   # versioned prompts, ranking rules, routing, failure handling
todo ai list / show <session>; todo llm calls / actions   # audit trail
```

Every proposal is a session with numbered items, a before → after diff, and a result
(新增 / 修改 / 未执行 / 失败). Deadlines are only kept when the user's own words state them. Task
changes are applied as one undoable operation with history actor `llm` (the operation summary names
the session); undoing an item reverts the accept step it was applied in. Self-optimisation
suggestions never apply themselves (not even in auto mode) and cannot widen permissions; applying,
undoing or restoring one writes a new configuration version.

### Prompt templates (`todo prompt`)

```sh
todo prompt summarize long-prompt.md --save --name 周报分析   # goal/context/constraints/steps/output/variables
todo prompt show 周报分析 [--original] [--version 1]; todo prompt versions 周报分析
todo prompt edit 周报分析 --step … --constraint … [--body-file body.md]   # each edit is a new version
todo prompt rollback 周报分析 [1]        # restore an older version as a new version
todo prompt copy 周报分析 [--clipboard]  # duplicate, or copy the text to the clipboard
todo prompt export 周报分析 --format md|txt|json [-o file]   # json keeps the original + all versions
todo prompt render 周报分析 --var 产品=FlowOS
todo prompt task 周报分析 --var 产品=FlowOS   # agent task whose description is the final prompt
```

### Agents (`todo agent`)

An agent is any program, service or model prompt that works on a task. Agents are configured once and
started from a task (`todo agent run`, `POST /api/tasks/{id}/agent-runs`) or from a model decision
(an accepted `agent` item of `todo ai decide` starts a run). `--agent auto` lets the model choose
from the agents' descriptions; without a usable model the routing rules of the assistant
configuration (`tag:NAME` or a keyword) decide, then the only configured agent.

```sh
# generic command-line adapter: program + args, run without a shell
todo agent add coder --desc 写代码 --dir ~/src/app --env GITHUB_TOKEN --timeout 20m \
  --success-codes 0 --input json-stdin --output jsonl --max-retries 1 -- my-agent --task {{task_id}}
todo agent add claude --input prompt-stdin --output text -- claude -p      # another local agent
todo agent add ci --adapter http --url https://ci.example/run --header-env Authorization=CI_TOKEN
todo agent add writer --adapter llm --desc 写文档                          # prompt agent (model profile)
todo agent list

todo agent run 1a2b --agent coder [--context …] [--constraint …] [--format …] [--complete]
todo agent run 1a2b --agent auto --template 周报分析 --var 产品=FlowOS      # template → instructions
todo agent run 1a2b --dry-run              # show the chosen agent and the final prompt only
todo agent run 1a2b --detach               # background worker; follow with logs --follow
todo agent runs [--task 1a2b] [--status running,failed]
todo agent show r3f2 | logs r3f2 [--follow] [--kind output,command,error] | prompt r3f2
todo agent pause r3f2 | resume r3f2 | cancel r3f2 | retry r3f2 [--context 改用 --fast]
todo agent confirm r3f2 | reject r3f2      # agents added with --confirm wait for this
todo agent results 1a2b                    # every result with source agent, time and version
todo agent writeback 1a2b '{"result_type":"commit","repo":"app","branch":"main","commit":"89abcdef"}'
```

**Run states** (FR-502): `queued` 排队中, `waiting_confirmation` 等待确认, `running` 运行中, `paused`
已暂停, `waiting_retry` 等待重试, `succeeded` 成功, `partial` 部分成功, `failed` 失败, `cancelled` 已取消,
`unknown` 结果未知. Every run shows its start and end time and its duration without pauses; each retry is a
new attempt and earlier attempts keep their status, exit code and log. Exit codes in `success_codes`
(default 0) mean success unless the agent reports otherwise; a failure that already wrote results back is
`partial`; an attempt killed by its timeout, or whose executing process died, is `unknown` and is never
retried automatically. Automatic retries (`--max-retries`) only follow plain failures without results.
A run never marks its task done unless it succeeded and was started with `--complete` or the agent wrote a
`task_status` result with `"local_status":"done"`. The first attempt moves a `todo` task to in progress.

**Control.** Runs are controlled through the database, so `pause`/`resume`/`cancel` work from any
terminal or the web page whichever process executes the run. The command-line adapter starts the agent
in its own process group: pause/resume send SIGSTOP/SIGCONT, cancel sends SIGTERM and SIGKILL after a
grace period. At most 4 attempts run at once (`TODO_CLI_AGENT_CONCURRENCY`); more runs wait queued.

**Protocol `todo-agent/v1`.** The agent receives a JSON document (`--input json-stdin`, the default, or
`json-arg`; `prompt-stdin`/`prompt-arg` pass only the final prompt; the file is also in
`$TODO_AGENT_INPUT_FILE`): `protocol`, `run_id`, `attempt`, `task` (id, title, description, notes,
status, priority, tags, category, due), `context` (parent, dependencies, subtasks, `previous_results`,
template instructions, user context), `constraints`, `output_format`, `idempotency_key` and `prompt` — the
final prompt combining all of it, shown by `todo agent prompt` before and after the start (FR-506).
Arguments may use `{{prompt}}`, `{{input}}`, `{{input_file}}`, `{{task_id}}`, `{{run_id}}`,
`{{attempt}}`, `{{workdir}}`. Standard output is JSON Lines (other lines are kept as plain output):

```json
{"type":"progress","stage":"build","percent":40,"message":"…"}
{"type":"log","message":"…"}   {"type":"command","argv":["git","commit"],"exit_code":0}
{"type":"error","message":"…","retryable":false}
{"type":"result","result_type":"text","text":"…","summary":"…"}
{"type":"result","result_type":"file","path":"out/report.md","summary":"…"}
{"type":"result","result_type":"commit","repo":"…","branch":"main","commit":"<hash>","message":"…"}
{"type":"result","result_type":"command_output","argv":["make","test"],"exit_code":0,"stdout":"…"}
{"type":"result","result_type":"data","data":{…}}
{"type":"result","result_type":"task_status","status":"Resolved","url":"…","local_status":"done"}
{"type":"status","status":"succeeded|partial|failed","message":"…"}
```

`--output json` reads one document `{status, message, results, events}` at the end, `--output text`
turns all of stdout into one text result. The HTTP adapter posts the same JSON input (with
`Idempotency-Key: <task id>:<run id>`, identical on every attempt) and reads either
`application/x-ndjson` lines or one JSON document; 2xx is exit code 0, other statuses are the exit code.
Other applications are connected by registering an adapter in Go (`agent.Register("name", factory)`)
and adding an agent with `--adapter name --option key=value`; an unknown adapter or agent is reported
as not available, never as success.

**Results** (FR-508/509/511) are stored per task with their source agent, run, attempt and time, and
appear in the task history (`agent_run`, `agent_result`, `agent_succeeded`/`agent_failed`/…, actor
`agent/<name>`). The idempotency key is `task id:run id:result type` (plus the result's own `key` when a
run writes several results of one type). Results are append-only: the same content again is recognised
and not stored twice, different content under the same key becomes a new version and the earlier versions
stay. File results record path, size and SHA-256 (and whether they lie outside the working directory),
commit results repository, branch and hash.

**Safety.** Agents run with the current user's permissions only: commands containing `sudo`, `su`,
`doas` … are refused, commands run without a shell, and the environment is reduced to `PATH`, `HOME`,
`USER`, locale/terminal variables, the agent's `--env` whitelist and `TODO_AGENT_*` (model keys are never
passed, not even when whitelisted). Prompts, inputs, logs and results are redacted like the model
features (keys, tokens, passwords, `Bearer …`, the values of whitelisted `*TOKEN*`/`*KEY*` variables and
HTTP header tokens).

## Local web service (`todo serve`)

```bash
todo serve                 # http://127.0.0.1:3210/ — the next free port if 3210 is taken
todo serve --port 4000 --open
todo serve --json          # {"url": "...", "port": ..., "data_dir": ..., "pid": ...}
```

The service reads and writes the same SQLite database as the CLI and the TUI (WAL mode, one source
of truth), so a change made in any of them is visible in the others. It listens on loopback only
(`--host` accepts `127.0.0.1`, `::1` or `localhost`; anything else is refused) and has no login. Requests
whose `Host` is not a loopback name (DNS rebinding) and writes from another origin are rejected with
`403`. `TODO_CLI_PORT` changes the default port. History records web changes with actor `web`.

### Web task manager

Open the address `todo serve` prints (or `todo serve --open`). The page covers the same task operations
as the CLI (FR-701):

- **List** with status tabs (全部/待办/进行中/已完成/已归档/回收站), keyword search, filters for
  priority, tag, category and due time (overdue, today, 7 days, with/without due), and sorting by manual
  order, due time, priority, created or updated time (reversible). The view lives in the URL, so a
  reload or bookmark shows the same list.
- **Create** with the quick-add box (TUI syntax: `写周报 #work @office !high due:明天`) or the full form
  (title, description, notes, status, priority, due time, tags, category, parent task).
- **Detail** pane with all fields, subtasks and the full change history (who: 命令行/终端界面/网页).
- **Edit**, complete/reopen, start, archive, delete and restore (recycle bin), priority `+`/`-`,
  manual reordering.
- **Batch**: select rows (checkbox, shift-click for a range, select all) and complete, start, reopen,
  archive, delete/restore, set priority or move to a category in one atomic, undoable step.
- **Undo**: every change shows a toast with 撤销; the 撤销 button and `u` undo the most recent change.
- **Keyboard**: `n`/`N` new, `/` search, `j`/`k` move, `Enter` details, `e` edit, `x` done, `s` start,
  `A` archive, `d` delete, `space` select, `K`/`J` reorder, `u` undo, `?` help.

The page keeps no task data of its own: every action goes through the REST API into the same SQLite
database the CLI uses, with the same history actions as the CLI command of the same name (FR-706),
recorded with actor `web`. Changes from the CLI, the TUI or another tab arrive over `/api/events`
within about a second; the page also reloads when it regains focus. Edits carry the task version: if
the task changed meanwhile, a dialog shows base/theirs/yours per field and lets you apply your edit,
discard it or decide later (the rejected edit is kept as a conflict). Unsaved form input survives a
reload (it is kept in `localStorage` until saved or cancelled). The page is plain HTML, CSS and ES
modules with no build step, embedded into the binary and served with a strict Content-Security-Policy;
it supports current Safari, Chrome, Edge and Firefox. `TODO_CLI_WEB_DIR=apps/web todo serve` (or
`npm run dev:web`) serves the files from disk while working on the page.

### Model and agents in the browser

The top bar shows the active model and the **permission mode** (仅建议 / 执行前确认 / 自动执行), which
can be switched there (FR-305). **AI 助手** (`i`) opens a panel with the same features as `todo ai`,
`todo agent` and `todo prompt`, over the same REST API and data (FR-702…FR-704):

- **创建任务**: describe tasks in natural language; the parsed tasks are previewed with their fields
  and can be edited, accepted one by one or all at once, rejected or undone. A clarifying question is
  answered in place; the result counts created / updated / not executed / failed items.
- **辅助决策**: summary, risks and the suggested order with reasons; each proposed change shows its
  before/after diff and can be accepted, rejected, edited or undone; 重新决策 asks again with
  feedback. 自我优化建议 lists the model's improvement proposals with their diffs.
- **智能体运行**: every run with status, stage and progress, start/end time and duration, the selection
  (user or model), the final prompt, attempts, results and the live log (output, commands, errors);
  pause, resume, cancel, retry (with extra context), confirm or reject a start.
- **提示词模板**: original prompt, structured summary and body of each template; reuse it with new
  variable values (preview or create an agent task), copy it, or summarize a new prompt into one.
- **模型与智能体**: model profiles (switch, test connection), permission mode and custom confirm list,
  agents (add/edit/delete with adapter, command, directory, environment names, timeout, I/O modes) and
  the available adapters.
- **执行历史**: agent runs, model sessions (open one to act on it) and the command/agent action log.

The task detail pane has an **智能体执行** block: choose an agent or 自动选择, optionally a prompt
template with its variables and extra context, preview the final prompt, start (`g` jumps there), and
see the task's runs with their controls and every written-back result with its type, source agent and
time (FR-501, FR-511). Running work refreshes every second while anything is active. Model keys never
reach the page: it only shows whether a key is set and where it comes from. When the model module is
unavailable the panel says so and task management keeps working (FR-606).

### REST API

All responses are JSON (`Cache-Control: no-store`); errors are `{"error": {"code", "message"}}`.
Task ids may be unique prefixes, as in the CLI.

| Method & path | Purpose |
|---|---|
| `GET /api/health` | status, schema version, current `revision` |
| `GET /api/facets` | tags and categories in use, with task counts → `{tags: [{name, count}], categories: [...]}` |
| `GET /api/tasks` | list/search: `q`, `status` (csv or `all`), `priority`, `min_priority`, `tag`, `category`, `parent` (id or `none`), `due_before`, `due_after`, `overdue`, `has_due`, `include_archived`, `deleted=include\|only`, `sort=manual\|due\|priority\|created\|updated`, `reverse`, `limit` → `{tasks, count, revision}` |
| `POST /api/tasks` | create (`title` required; `description`, `notes`, `due_at`, `priority`, `tags`, `category`, `parent_id`, `status`) → `201` |
| `GET /api/tasks/{id}` | task + subtasks + history + open conflicts (`ETag` = version) |
| `PATCH /api/tasks/{id}` | edit; **requires** the edited `version` (body or `If-Match`), else `428`. Fields: `title`, `description`, `notes`, `category`, `parent_id`, `due_at` (`null` clears), `priority`, `tags`, `add_tags`, `remove_tags`, `status` |
| `POST /api/tasks/{id}/status` | `{"status": "done", "version"?}` — same history action as `todo done/start/reopen/archive` |
| `DELETE /api/tasks/{id}` | soft delete (`?version=` optional) · `POST /api/tasks/{id}/restore` |
| `POST /api/tasks/{id}/move` | manual order: `before` / `after` / `top` / `bottom` |
| `POST /api/batch` | `{"action": "complete\|start\|reopen\|archive\|delete\|restore\|priority\|move", "ids": [...] or "items": [{"id","version"}], "priority"?, "category"?, "parent_id"?}` — atomic, one undo step |
| `POST /api/undo` | undo the most recent change (any entry point) |
| `GET /api/tasks/{id}/history` | change log with actor (`cli`, `tui`, `web`, …) |
| `GET /api/tasks/{id}/versions[/{n}]` | every stored version of the task · `POST …/versions/{n}/revert` (needs current `version`) |
| `GET /api/tasks/{id}/conflicts[?all=1]`, `GET /api/conflicts/{cid}` | rejected edits with a three-way field diff |
| `POST /api/conflicts/{cid}/resolve` | `{"resolution": "mine"\|"theirs", "version"?}` |
| `GET /api/events` | server-sent events (see below) |

Model and prompt endpoints (keys are never accepted or returned): `GET /api/llm/status`,
`POST /api/llm/test` `{profile?}`, `POST /api/llm/use` `{profile}`, `POST /api/llm/mode` `{mode}`,
`PUT /api/llm/confirm-list` `{entries}`, `POST /api/llm/intake` `{text, selected?, mode?, profile?}`,
`POST /api/llm/decide` `{selected?, mode?, feedback?}`, `POST /api/llm/optimize`,
`GET /api/llm/sessions[?kind=]`, `GET /api/llm/sessions/{sid}`,
`POST /api/llm/sessions/{sid}/answer|redecide|apply|reject|undo` (`{items: [n…]}`; none = all),
`PATCH /api/llm/sessions/{sid}/items/{n}`, `GET /api/llm/config[/versions]`,
`POST /api/llm/config/rollback`, `POST /api/llm/config/versions/{v}/restore`, `GET /api/llm/calls|actions`;
`GET|POST /api/prompts`, `POST /api/prompts/summarize` `{text, save?, name?, local?}`,
`GET|PUT|DELETE /api/prompts/{pid}`, `GET /api/prompts/{pid}/versions`,
`POST /api/prompts/{pid}/rollback|copy|render|task`, `GET /api/prompts/{pid}/export?format=md|txt|json`.
Model failures answer `503` with `{code, message, hint, retryable, profiles}`; session state errors `409 invalid_state`;
missing template variables `400 missing_variables`.

Agent endpoints: `GET /api/agents` (agents and adapters), `PUT|DELETE /api/agents/{name}`,
`POST /api/tasks/{id}/agent-runs` `{agent ("auto"), context?, constraints?, output_format?, template?, vars?,
complete_on_success?, max_retries?, dry_run?}` → `201` (runs in the background of the server),
`GET /api/agent-runs[?task=&status=a,b&limit=]`, `GET /api/agent-runs/{rid}` (attempts, results, duration),
`GET /api/agent-runs/{rid}/events?after=<id>&attempt=&kind=` (log for live views, with `done`),
`GET /api/agent-runs/{rid}/prompt`, `POST /api/agent-runs/{rid}/pause|resume|cancel|retry|confirm|reject`
(`retry` takes `{context?}`), `GET /api/tasks/{id}/results`, and `POST /api/tasks/{id}/results`
`{result_type, …, run_id?, source?}` for external write-back (`201`, or `200` with `duplicate: true` for a
result already stored). A run state that does not allow the action answers `409 invalid_state`; an
unknown agent `404`. Stopping the server cancels the runs it executes.

Status codes: `400 invalid_input|invalid_json|ambiguous_id`, `404 not_found`, `409 version_conflict|nothing_to_undo|conflict_resolved`,
`410 task_deleted`, `413`, `415` (bodies must be `application/json`), `428 version_required`.

### Versions and conflicts

Every write bumps the task's server-generated `version`. The server keeps a full snapshot of each version.
A write that carries an outdated version is rejected with `409` and never overwrites anything. The rejected
edit is stored as a *conflict*. The response contains `conflict.base` (what you edited), `conflict.current`
(what won), `conflict.yours`, `conflict.merged` (your edit applied on top of the current version) and
`conflict.fields` (per field: base/current/yours, who changed it, whether both sides disagree). Resolve it
later with `mine` (applies your edit, itself version-checked) or `theirs` (discards it). Any overwritten
version can be brought back with `…/versions/{n}/revert`.

### Live updates

`GET /api/events` is a server-sent event stream: `ready` (`{"revision"}`), then one `task` event per change
(`{revision, task_id, action, actor, changes, at, task}`), whichever process made it. The server polls the
database every 200 ms, so CLI and TUI changes arrive well within 2 seconds. Event ids are revisions:
a reconnecting `EventSource` sends `Last-Event-ID` (or use `?since=N`) and gets the missed changes replayed.
If too many were missed, it gets a `resync` event, meaning reload the list. Clients should also refetch
`GET /api/tasks` on focus or reload.

Tests use Go's `net/http/httptest` against a real listener (the Go counterpart of Supertest).

## Interactive TUI

```bash
todo            # in a terminal: opens the interactive UI (in scripts/pipes it prints help)
todo tui        # same; aliases: todo ui, todo i
todo tui --no-mouse --no-color --keymap ~/my-keys.json
```

The screen has status tabs (全部/待办/进行中/已完成/已归档) on top, the task list on the left, the
detail pane on the right (narrow terminals show one pane at a time), a status line and a row of
clickable key hints at the bottom. Supported terminals: macOS Terminal and iTerm2 (any
xterm-compatible terminal works). UTF-8/CJK text, `NO_COLOR=1` and `TERM=dumb` (refused with a clear
message) are handled.

| Keys | Action |
|---|---|
| `↑↓` / `j k`, `g G`, `PgUp PgDn` / `ctrl+u ctrl+d` | move the selection |
| `tab` | switch between list and detail pane; `enter` / `l` opens details, `esc` / `h` goes back |
| `a` / `n`, `N` | new task / new subtask (quick syntax: `写周报 #work @office !high due:tomorrow`) |
| `e` | edit form (all fields; `tab` completes tags/categories/priorities/dates, `ctrl+s` saves) |
| `x` / `space`, `s`, `A`, `d` | done ↔ reopen, start, archive, delete (with confirmation) |
| `+` `-`, `K` `J` | raise/lower priority, move up/down in the manual order |
| `u` | undo the most recent change |
| `/` | live search (`esc` restores, `tab` completes) |
| `f`, `1`–`5`, `[` `]`, `c` | filter menu, status tabs, clear search and filters |
| `o`, `O` | cycle sort (manual/due/priority/created/updated), reverse |
| `:` / `ctrl+p` | command palette: every action plus `add`, `search`, `filter`, `sort`, `priority`, `due`, `tag`, `untag`, `category`, `goto` with argument completion |
| `m` | action menu (keyboard equivalent of right click) |
| `?` / `F1` | key binding help (shows the effective, customised keys) |
| `q` / `ctrl+c` | quit |

Mouse (in terminals that report it; enabled via SGR 1006 mode): click selects a task, double click
opens its details, click the detail pane to focus it, right click opens the action menu, the wheel
scrolls the list or the details, and tabs, footer hints, detail buttons, menu items and dialog
buttons are clickable. The keyboard cursor and the mouse selection are the same state, and modal
inputs (new task, edit form) are never disturbed by clicks. Disable reporting with `--no-mouse` or
`TODO_CLI_MOUSE=0`; every action stays reachable from the keyboard.

Changes made elsewhere (another `todo` command, the web UI) appear within about a second.
Editing a task that changed meanwhile reports a conflict and keeps your input; saving again
overwrites deliberately. History records TUI changes with actor `tui`.

### Custom key bindings

```bash
todo keys                # list effective bindings (* = customised), also --json
todo keys --init         # write the defaults to ~/.todo-cli/keybindings.json as a template
```

The file maps action names to a key or a list of keys; listed actions replace their defaults and
a key taken by another action is moved:

```json
{"new": ["ctrl+n", "a"], "delete": "D", "archive": []}
```

Keys look like `a`, `G`, `?`, `ctrl+n`, `alt+x`, `shift+tab`, `enter`, `esc`, `space`, `delete`,
`pgup`, `f1`. `TODO_CLI_KEYMAP` or `todo tui --keymap FILE` use another file. An invalid file is
reported (with "did you mean" hints) and the defaults are used.

### Shell completion

```bash
eval "$(todo completion zsh)"     # or bash; fish: todo completion fish > ~/.config/fish/completions/todo.fish
```

Completes commands, flags, flag values (priorities, statuses, sorts, due shortcuts) and task ids
with their titles. Mistyped commands get a suggestion (`unknown command "lsit"; did you mean "list"?`).

## Scope notes

- Voice input is deferred (FR-801). When it is added later, it must reuse the normal task create/edit
  flow and must not create a separate voice data store (FR-802).
