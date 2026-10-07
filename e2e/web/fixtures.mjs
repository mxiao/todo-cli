// Test fixtures: a private `todo serve` per test over a temporary data
// directory, plus helpers to run the real CLI against the same database.
import { test as base, expect } from '@playwright/test';
import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
export const todoBin = path.join(root, 'bin', 'todo');

function startServe(dir) {
  return new Promise((resolve, reject) => {
    const child = spawn(todoBin, ['--data-dir', dir, 'serve', '--json', '--port', '0'], {
      env: { ...process.env, NO_COLOR: '1' },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let out = '';
    let err = '';
    const timer = setTimeout(() => reject(new Error(`todo serve did not start: ${out} ${err}`)), 10_000);
    child.stdout.on('data', (b) => {
      out += b;
      let info;
      try {
        info = JSON.parse(out); // `serve --json` prints one (indented) object once listening
      } catch {
        return;
      }
      clearTimeout(timer);
      resolve({ child, info });
    });
    child.stderr.on('data', (b) => { err += b; });
    child.on('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`todo serve exited ${code}: ${err}`));
    });
  });
}

function stopServe(child) {
  return new Promise((resolve) => {
    if (child.exitCode !== null) return resolve();
    child.removeAllListeners('exit');
    child.once('exit', () => resolve());
    child.kill('SIGTERM');
  });
}

export const test = base.extend({
  // todo: { dir, url, cli(...args) -> parsed JSON, restart() }
  todo: async ({}, use) => {
    const dir = mkdtempSync(path.join(tmpdir(), 'todo-web-e2e-'));
    let server = await startServe(dir);
    const todo = {
      dir,
      get url() {
        return server.info.url;
      },
      // cli runs `todo --json <args>` on the same data directory.
      cli(...args) {
        const out = execFileSync(todoBin, ['--data-dir', dir, '--json', ...args], {
          env: { ...process.env, NO_COLOR: '1' },
          encoding: 'utf8',
        });
        return out.trim() ? JSON.parse(out) : null;
      },
      // restart stops the service and starts a new one on the same data
      // (possibly on another port).
      async restart() {
        await stopServe(server.child);
        server = await startServe(dir);
        return server.info.url;
      },
      async api(method, p, body) {
        const res = await fetch(new URL(p, server.info.url), {
          method,
          headers: body ? { 'Content-Type': 'application/json' } : {},
          body: body ? JSON.stringify(body) : undefined,
        });
        return { status: res.status, body: await res.json() };
      },
    };
    await use(todo);
    await stopServe(server.child);
    rmSync(dir, { recursive: true, force: true });
  },
});

export { expect };

// openApp loads the page and waits until the first list load finished.
export async function openApp(page, url) {
  await page.goto(url);
  await expect(page.locator('body[data-ready="true"]')).toBeAttached();
  await expect(page.getByTestId('live-status')).toHaveAttribute('data-state', 'live');
}

export const rows = (page) => page.getByTestId('task-row');
export const row = (page, title) => page.getByTestId('task-row').filter({ has: page.getByTestId('task-title').getByText(title, { exact: true }) });
export const titles = (page) => page.getByTestId('task-title');
