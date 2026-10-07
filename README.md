# todo-cli

基于命令行的todo工具，支持web命理用网页打开。比较偏向现代cli，可以通过键盘鼠标快速执行，也可以通过大模型来创建任务。打开cli后，大模型可以帮我决策做什么，并且可以启动对应的智能体帮我完成任务，也可以通过语音输入来完成一些录入，补充等操作，决策上比较简单。

由 FlowOS 初始化；代码由各开发任务的 PR 加入。

## Layout

```
cmd/todo/          `todo` binary entry point
packages/core/     task model + SQLite store (single source of truth for every entry point)
packages/cli/      non-interactive CLI (flags + --json)
```

Later releases add the interactive TUI, the local web server/UI, LLM features and agents
on top of `packages/core`. All of them share the same task IDs, fields and status rules.

### Stack decision

The task brief proposed Node.js 20 + TypeScript. The repository was already scaffolded as a
Go module (`go.mod`, Go CI, `go test ./...` as the verification gate), so this release is
written in Go. It uses the same `packages/core` / `packages/cli` split and SQLite through
the pure-Go `modernc.org/sqlite` driver, which needs no cgo and no runtime dependencies.
`npm test` is wired to `go test ./...`. Target platform: macOS 14+.

## Build & test

```bash
go build -o bin/todo ./cmd/todo   # or: npm run build
go test ./...                     # or: npm test
```

## Data

- One SQLite database: `~/.todo-cli/todo.db`. Override it with `--data-dir DIR` or `TODO_CLI_HOME`.
- The data directory is `0700` and database/backup files are `0600`.
- Writes are transactional: one command, including a batch, is one transaction. They are all-or-nothing.
- Every task has a `version` (optimistic locking via `todo edit --if-version N`) and a full change history.
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

`todo status` reports the model configuration read from the environment: `TODO_CLI_MODEL_PROVIDER`,
`TODO_CLI_MODEL`, `TODO_CLI_MODEL_BASE_URL` and `TODO_CLI_MODEL_API_KEY`. The key is never printed,
and credentials or query strings in the base URL are stripped. Task management works without any
model configured.

## Scope notes

- Voice input is deferred (FR-801). When it is added later, it must reuse the normal task create/edit
  flow and must not create a separate voice data store (FR-802).
