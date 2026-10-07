// Harness for the acceptance suite: an isolated data directory, the fixed
// mock model, a real `todo serve` on that data and helpers to run the real
// CLI against the same SQLite database. Nothing here is mocked inside the
// product: the page, the CLI and the service run as shipped.
import { spawn } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { startMockModel, TZ } from './mock-model.mjs';

export const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');
export const todoBin = process.env.TODO_BIN || path.join(root, 'bin', 'todo');
export const mockAgent = path.join(root, 'tests', 'e2e', 'agents', 'mock-agent.mjs');

// The model key used by every test. It must never appear in the page,
// logs, exports, history or anything sent to the model besides the
// Authorization header.
export const MODEL_KEY = 'sk-e2e-acceptance-key-0123456789abcdef';

// baseEnv is the environment of every todo process: no inherited todo
// settings, a fixed time zone, no colours and the mock model.
function baseEnv(model, extra = {}) {
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('TODO_CLI_') && !k.startsWith('TODO_AGENT_')));
  return {
    ...env,
    TZ,
    NO_COLOR: '1',
    TODO_CLI_MODEL_PROVIDER: 'openai',
    TODO_CLI_MODEL: 'mock-gpt',
    TODO_CLI_MODEL_BASE_URL: model.baseURL,
    TODO_CLI_MODEL_API_KEY: MODEL_KEY,
    ...extra,
  };
}

function startServe(dir, env) {
  return new Promise((resolve, reject) => {
    const child = spawn(todoBin, ['--data-dir', dir, 'serve', '--json', '--port', '0'], { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let out = '';
    let err = '';
    const timer = setTimeout(() => reject(new Error(`todo serve did not start: ${out} ${err}`)), 15_000);
    child.stdout.on('data', (b) => {
      out += b;
      try {
        const info = JSON.parse(out); // `serve --json` prints one object once listening
        clearTimeout(timer);
        resolve({ child, info, stderr: () => err });
      } catch {
        // not complete yet
      }
    });
    child.stderr.on('data', (b) => { err += b; });
    child.on('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`todo serve exited ${code}: ${err}`));
    });
  });
}

function stopServe(child, signal = 'SIGTERM') {
  return new Promise((resolve) => {
    if (child.exitCode !== null || child.signalCode !== null) return resolve();
    child.removeAllListeners('exit');
    child.once('exit', () => resolve());
    child.kill(signal);
  });
}

export class CLIError extends Error {
  constructor(args, r) {
    super(`todo ${args.join(' ')} exited ${r.status}: ${r.stderr}`);
    this.status = r.status;
    this.stderr = r.stderr;
    this.stdout = r.stdout;
  }
}

// createApp starts the mock model and a `todo serve` on a fresh data dir.
export async function createApp() {
  const dir = mkdtempSync(path.join(tmpdir(), 'todo-acceptance-'));
  const workspace = path.join(dir, 'workspace');
  mkdirSync(workspace);
  const model = await startMockModel({ apiKey: MODEL_KEY });
  const env = baseEnv(model);
  let server = await startServe(dir, env);

  const app = {
    dir,
    workspace,
    model,
    env,
    get url() {
      return server.info.url;
    },
    // run executes `todo --data-dir dir <args>` and resolves to
    // { status, stdout, stderr }. It is asynchronous on purpose: the mock
    // model lives in this process and must keep answering meanwhile.
    run(args, { input, json = true, env: extra } = {}) {
      const full = ['--data-dir', dir, ...(json ? ['--json'] : []), ...args];
      return new Promise((resolve, reject) => {
        const child = spawn(todoBin, full, { env: { ...env, ...extra }, stdio: [input == null ? 'ignore' : 'pipe', 'pipe', 'pipe'] });
        let stdout = '';
        let stderr = '';
        const timer = setTimeout(() => child.kill('SIGKILL'), 60_000);
        child.stdout.on('data', (b) => { stdout += b; });
        child.stderr.on('data', (b) => { stderr += b; });
        child.on('error', reject);
        child.on('close', (status) => {
          clearTimeout(timer);
          resolve({ status, stdout, stderr });
        });
        if (input != null) {
          child.stdin.on('error', () => {}); // the command may exit before reading all of it
          child.stdin.end(input);
        }
      });
    },
    // cli runs a JSON command and resolves to the parsed output; it rejects
    // with a CLIError on a non-zero exit.
    async cli(...args) {
      const r = await app.run(args);
      if (r.status !== 0) throw new CLIError(args, r);
      return r.stdout.trim() ? JSON.parse(r.stdout) : null;
    },
    // cliFails runs a command that must fail and resolves to { status, error }.
    async cliFails(...args) {
      const r = await app.run(args);
      if (r.status === 0) throw new Error(`todo ${args.join(' ')} unexpectedly succeeded: ${r.stdout}`);
      let error = null;
      try {
        error = JSON.parse(r.stderr).error;
      } catch {
        error = { message: r.stderr };
      }
      return { status: r.status, error };
    },
    async api(method, p, body) {
      const res = await fetch(new URL(p, server.info.url), {
        method,
        headers: body ? { 'Content-Type': 'application/json' } : {},
        body: body ? JSON.stringify(body) : undefined,
      });
      const text = await res.text();
      return { status: res.status, body: text ? JSON.parse(text) : null };
    },
    // addAgent registers the mock agent in one behaviour under a name.
    addAgent(name, behaviour, ...flags) {
      return app.cli('agent', 'add', name, '--dir', workspace, ...flags, '--', process.execPath, mockAgent, behaviour);
    },
    // restart stops the service (SIGTERM, or SIGKILL to simulate a crash)
    // and starts a new process on the same data.
    async restart({ crash = false } = {}) {
      if (server) await stopServe(server.child, crash ? 'SIGKILL' : 'SIGTERM');
      server = await startServe(dir, env);
      return server.info.url;
    },
    async stop() {
      if (server) await stopServe(server.child);
      server = null;
    },
    async close() {
      if (server) await stopServe(server.child);
      await model.close();
      rmSync(dir, { recursive: true, force: true });
    },
  };
  return app;
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// waitFor polls fn until it returns a truthy value or the time is up.
export async function waitFor(fn, { timeout = 10_000, interval = 50, message = 'condition' } = {}) {
  const deadline = Date.now() + timeout;
  let last;
  while (Date.now() < deadline) {
    last = await fn();
    if (last) return last;
    await sleep(interval);
  }
  throw new Error(`timed out after ${timeout} ms waiting for ${message}; last value: ${JSON.stringify(last)}`);
}
