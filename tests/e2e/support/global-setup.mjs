// Builds the todo binary every project drives (`todo serve`, CLI, TUI).
import { execFileSync } from 'node:child_process';
import { root } from './harness.mjs';

export default function globalSetup() {
  if (process.env.TODO_BIN) return;
  execFileSync('go', ['build', '-o', 'bin/todo', './cmd/todo'], { cwd: root, stdio: 'inherit' });
}
