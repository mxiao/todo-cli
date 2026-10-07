# todo-cli

基于命令行的todo工具，支持web命理用网页打开。比较偏向现代cli，可以通过键盘鼠标快速执行，也可以通过大模型来创建任务。打开cli后，大模型可以帮我决策做什么，并且可以启动对应的智能体帮我完成任务，也可以通过语音输入来完成一些录入，补充等操作，决策上比较简单。

由 FlowOS 初始化；代码由各开发任务的 PR 加入。

## Layout

```
cmd/todo/          `todo` binary entry point
packages/core/     task model + SQLite store (single source of truth for every entry point)
packages/cli/      non-interactive CLI (flags + --json), shell completion, `todo tui` launcher
packages/tui/      interactive full-screen terminal UI (keyboard + mouse)
e2e/               node-pty end-to-end tests of the TUI
```

Later releases add the local web server/UI, LLM features and agents
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
npm install && npm test           # go test ./... plus the node-pty TUI suite (e2e/)
```

`npm install` pulls `node-pty` (dev only) and restores the executable bit on its prebuilt macOS
`spawn-helper`. Without `node-pty` the node suite is skipped; the Go PTY tests still cover the same flows.

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

Changes made elsewhere (another `todo` command, later the web UI) appear within about a second.
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
