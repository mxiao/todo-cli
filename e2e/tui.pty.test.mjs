// End-to-end tests of the interactive TUI (`todo tui`) on a real
// pseudo-terminal via node-pty: keys and mouse escape sequences are written
// exactly as Terminal.app / iTerm2 send them, then the stored data is
// checked through the non-interactive CLI.
//
// Run with `npm test` (builds bin/todo first) or `npm run test:tui`.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { execFileSync, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const bin = process.env.TODO_BIN || join(root, 'bin', 'todo');

let pty;
let skip = false;
try {
  pty = (await import('node-pty')).default;
} catch (err) {
  skip = `node-pty is not installed (run \`npm install\`): ${err.message}`;
}

const ANSI = /\x1b\[[0-9;?<]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>]/g;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function tempDir(t) {
  const dir = mkdtempSync(join(tmpdir(), 'todo-tui-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function todo(dir, ...args) {
  return execFileSync(bin, ['--data-dir', dir, ...args], { encoding: 'utf8' });
}

function allTasks(dir) {
  const data = JSON.parse(todo(dir, 'export'));
  return Object.fromEntries(data.tasks.map((t) => [t.title, t]));
}

// SGR (1006) mouse press + release at 1-based column/row.
const click = (button, col, row) => `\x1b[<${button};${col};${row}M\x1b[<${button};${col};${row}m`;
const wheelDown = (col, row) => `\x1b[<65;${col};${row}M`;
const wheelUp = (col, row) => `\x1b[<64;${col};${row}M`;

function startTodo(t, args, { cols = 100, rows = 30, env = {} } = {}) {
  const term = pty.spawn(bin, args, {
    name: 'xterm-256color',
    cols,
    rows,
    cwd: root,
    env: { ...process.env, TERM: 'xterm-256color', NO_COLOR: '', TODO_CLI_MOUSE: '', TODO_CLI_KEYMAP: '', ...env },
  });
  let out = '';
  term.onData((d) => {
    out += d;
  });
  const exited = new Promise((resolve) => term.onExit(resolve));
  t.after(() => {
    try {
      term.kill();
    } catch {
      // already exited
    }
  });
  const session = {
    raw: () => out,
    text: (from = 0) => out.slice(from).replace(ANSI, ''),
    async send(s) {
      term.write(s);
      // A pause like a human typist so a following ESC is not read as Alt+key.
      await sleep(80);
    },
    async waitFor(from, want, ms = 10000) {
      const deadline = Date.now() + ms;
      while (Date.now() < deadline) {
        if (session.text(from).includes(want)) return;
        await sleep(20);
      }
      assert.fail(`timed out waiting for ${JSON.stringify(want)}; output:\n${session.text()}`);
    },
    async do(input, want) {
      const mark = out.length;
      await session.send(input);
      await session.waitFor(mark, want);
    },
    async exit() {
      const timeout = sleep(10000).then(() => assert.fail(`todo did not exit:\n${session.text()}`));
      return Promise.race([exited, timeout]);
    },
  };
  return session;
}

test('keyboard and mouse drive the core task flow', { skip }, async (t) => {
  const dir = tempDir(t);
  todo(dir, 'add', '第一项');
  todo(dir, 'add', '第二项');
  const s = startTodo(t, ['--data-dir', dir, 'tui']);
  await s.waitFor(0, '第二项');
  assert.ok(s.raw().includes('\x1b[?1049h'), 'alternate screen');
  assert.ok(s.raw().includes('\x1b[?1000h\x1b[?1006h'), 'SGR mouse reporting enabled');

  // Create with the keyboard (quick-add syntax).
  await s.do('a', '新任务');
  await s.do('写周报 #work !high due:tomorrow\r', '已创建：写周报');

  // Click the first row (screen row 3) and complete it from the keyboard:
  // the mouse selection and keyboard focus are the same.
  await s.send(click(0, 10, 3));
  await s.do('x', '已完成：第一项');
  await s.do('js', '已开始：第二项');

  // Right click row 4 → context menu at the pointer; click its 9th item (删除).
  await s.do(click(2, 12, 4), '打开详情');
  await s.do(click(0, 15, 13), '确定删除「第二项」');
  await s.do('y', '已删除：第二项');
  await s.do('u', '已撤销');

  // Wheel scrolling, search, help, command palette.
  await s.send(wheelDown(10, 5) + wheelUp(10, 5));
  await s.do('/写周\r', '搜索「写周」');
  await s.send('\x1b');
  await s.do('?', '快捷键帮助');
  await s.send('\x1b');
  await s.do(':', '命令面板');
  await s.do('sort priority\r', '排序：优先级');

  // Edit the top task (写周报, highest priority) in the form; ctrl+s saves.
  await s.do('ge', '编辑任务');
  await s.do('\x15周报（改）\x13', '已保存：周报（改）');

  await s.send('q');
  const { exitCode } = await s.exit();
  assert.equal(exitCode, 0);
  assert.ok(s.raw().includes('\x1b[?1000l'), 'mouse reporting disabled on exit');
  assert.ok(s.raw().trimEnd().endsWith('\x1b[?1049l'), 'main screen restored on exit');

  const tasks = allTasks(dir);
  assert.equal(tasks['第一项'].status, 'done');
  assert.equal(tasks['第二项'].status, 'in_progress');
  assert.equal(tasks['第二项'].deleted_at, null, 'undo restored the deleted task');
  assert.equal(tasks['周报（改）'].priority, 'high');
  assert.deepEqual(tasks['周报（改）'].tags, ['work']);
  assert.ok(tasks['周报（改）'].due_at, 'due date from quick add');
});

test('keyboard-only path works without mouse reporting', { skip }, async (t) => {
  const dir = tempDir(t);
  todo(dir, 'add', '仅键盘');
  const s = startTodo(t, ['--data-dir', dir, 'tui', '--no-mouse'], { cols: 80, rows: 24 });
  await s.waitFor(0, '仅键盘');
  assert.ok(!s.raw().includes('\x1b[?1000h'), '--no-mouse must not enable mouse reporting');
  // Narrow terminal: enter opens the detail view, actions work there.
  await s.do('\r', '详情 · tab');
  await s.do('x', '已完成：仅键盘');
  await s.send('\t');
  await s.do('m', '重新打开'); // keyboard equivalent of the right-click menu
  await s.do('x', '已重新打开：仅键盘'); // an item's key runs it inside the menu
  await s.do('f', '仅显示逾期'); // filter menu
  await s.do('2', '筛选：待办');
  await s.do('c', '已清除搜索与筛选');
  await s.send('\x03'); // ctrl+c
  assert.equal((await s.exit()).exitCode, 0);
  assert.equal(allTasks(dir)['仅键盘'].status, 'todo');
});

test('bare `todo` in a terminal opens the TUI; custom keys apply', { skip }, async (t) => {
  const dir = tempDir(t);
  todo(dir, 'add', '自定义');
  const keymap = join(dir, 'my-keys.json');
  writeFileSync(keymap, JSON.stringify({ toggle_done: 'D', quit: ['Q', 'ctrl+c'] }));
  const s = startTodo(t, ['--data-dir', dir], { env: { TODO_CLI_KEYMAP: keymap } });
  await s.waitFor(0, 'D 完成');
  await s.do('x', '按键 x 未绑定任何操作');
  await s.do('D', '已完成：自定义');
  await s.do('?', '快捷键帮助');
  await s.send('\x1b');
  await s.send('Q');
  assert.equal((await s.exit()).exitCode, 0);
  assert.equal(allTasks(dir)['自定义'].status, 'done');
});

test('without a terminal the TUI refuses with a clear error', (t) => {
  const dir = tempDir(t);
  const r = spawnSync(bin, ['--data-dir', dir, 'tui'], { encoding: 'utf8' });
  assert.equal(r.status, 2);
  assert.match(r.stderr, /needs a terminal/);
  const help = spawnSync(bin, ['--data-dir', dir], { encoding: 'utf8' });
  assert.equal(help.status, 0);
  assert.match(help.stdout, /commands:/);
});
