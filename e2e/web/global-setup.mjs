// Builds the todo binary the web tests drive (`todo serve` and CLI calls).
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

export default function globalSetup() {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
  execFileSync('go', ['build', '-o', 'bin/todo', './cmd/todo'], { cwd: root, stdio: 'inherit' });
}
