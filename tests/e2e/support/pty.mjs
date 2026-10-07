// Terminal emulation for the TUI acceptance tests: `todo tui` runs on a real
// pseudo-terminal (node-pty) with the environment of a macOS terminal, and
// keys / mouse reports are written exactly as that terminal sends them.
import { todoBin, root, sleep } from './harness.mjs';

let pty = null;
let loadError = null;
try {
  pty = (await import('node-pty')).default;
} catch (err) {
  loadError = err;
}

// ptyUnavailable is a skip reason, or false when node-pty works.
export const ptyUnavailable = pty ? false : `node-pty is not installed (run \`npm install\`): ${loadError?.message}`;

// Terminal profiles of the compatibility matrix. Both send xterm key
// sequences and SGR (1006) mouse reports; they differ in how they announce
// themselves to programs.
export const PROFILES = [
  { name: 'Terminal.app', env: { TERM: 'xterm-256color', TERM_PROGRAM: 'Apple_Terminal', TERM_PROGRAM_VERSION: '455', LANG: 'zh_CN.UTF-8' } },
  { name: 'iTerm2', env: { TERM: 'xterm-256color', TERM_PROGRAM: 'iTerm.app', TERM_PROGRAM_VERSION: '3.5.4', LC_TERMINAL: 'iTerm2', COLORTERM: 'truecolor', LANG: 'zh_CN.UTF-8' } },
];

export const KEY = { esc: '\x1b', enter: '\r', tab: '\t', ctrlC: '\x03', ctrlS: '\x13', ctrlU: '\x15' };
// SGR (1006) mouse press + release at 1-based column/row.
export const click = (button, col, row) => `\x1b[<${button};${col};${row}M\x1b[<${button};${col};${row}m`;
export const wheelDown = (col, row) => `\x1b[<65;${col};${row}M`;
export const wheelUp = (col, row) => `\x1b[<64;${col};${row}M`;

const ANSI = /\x1b\[[0-9;?<]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>]/g;

// openTerminal starts `todo <args>` in a pseudo-terminal of the profile.
export function openTerminal(app, profile, args, { cols = 100, rows = 30, env = {} } = {}) {
  const term = pty.spawn(todoBin, ['--data-dir', app.dir, ...args], {
    name: profile.env.TERM,
    cols,
    rows,
    cwd: root,
    env: { ...app.env, NO_COLOR: '', TODO_CLI_MOUSE: '', TODO_CLI_KEYMAP: '', ...profile.env, ...env },
  });
  let out = '';
  term.onData((d) => { out += d; });
  const exited = new Promise((resolve) => term.onExit(resolve));
  const session = {
    raw: () => out,
    text: (from = 0) => out.slice(from).replace(ANSI, ''),
    mark: () => out.length,
    async send(s) {
      term.write(s);
      await sleep(80); // like a human typist, so a following ESC is not read as Alt+key
    },
    // waitFor resolves to the milliseconds until `want` appeared after `from`.
    async waitFor(from, want, ms = 10_000) {
      const start = Date.now();
      while (Date.now() - start < ms) {
        if (session.text(from).includes(want)) return Date.now() - start;
        await sleep(20);
      }
      throw new Error(`timed out waiting for ${JSON.stringify(want)}; screen output:\n${session.text(from).slice(-3000)}`);
    },
    async do(input, want) {
      const m = out.length;
      await session.send(input);
      return session.waitFor(m, want);
    },
    async exit(ms = 10_000) {
      return Promise.race([exited, sleep(ms).then(() => { throw new Error(`todo did not exit:\n${session.text().slice(-2000)}`); })]);
    },
    kill() {
      try {
        term.kill();
      } catch {
        // already exited
      }
    },
  };
  return session;
}
