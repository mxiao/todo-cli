// Playwright reporter that writes the traceable acceptance report:
// <outputDir>/traceability.json and traceability.md. Each test is mapped to
// the acceptance items of requirements.mjs through its @AT-xx tags, with the
// project (browser / terminal), the browser version and the outcome.
import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { buildReport, renderMarkdown } from './report.mjs';

function tryRun(cmd, args) {
  try {
    return execFileSync(cmd, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  } catch {
    return null;
  }
}

function osName() {
  if (process.platform === 'darwin') {
    const v = tryRun('sw_vers', ['-productVersion']);
    return `macOS ${v || os.release()} (${os.arch()})`;
  }
  return `${os.type()} ${os.release()} (${os.arch()})`;
}

function playwrightVersion() {
  try {
    const require = createRequire(import.meta.url);
    return JSON.parse(readFileSync(require.resolve('@playwright/test/package.json'), 'utf8')).version;
  } catch {
    return null;
  }
}

const stripAnsi = (s) => String(s || '').replace(/\x1b\[[0-9;]*m/g, '');

export default class TraceabilityReporter {
  constructor(options = {}) {
    this.outputDir = options.outputDir || 'reports/e2e';
    this.regression = options.regressionPrefix || 'regression';
    this.records = new Map();
  }

  printsToStdio() {
    return false;
  }

  onBegin(config, suite) {
    this.rootDir = config.rootDir;
    this.startedAt = new Date();
    this.projects = [...new Set(suite.allTests().map((t) => t.parent.project()?.name).filter(Boolean))];
  }

  onTestEnd(test, result) {
    const project = test.parent.project()?.name || '';
    const outcome = { expected: 'passed', unexpected: 'failed', flaky: 'flaky', skipped: 'skipped' }[test.outcome()];
    const annotations = [...test.annotations, ...(result.annotations || [])];
    this.records.set(test.id, {
      title: test.title,
      file: path.relative(process.cwd(), test.location.file),
      project,
      tags: test.tags,
      annotations,
      outcome,
      durationMs: result.duration,
      error: result.error ? stripAnsi(result.error.message).split('\n').slice(0, 3).join(' ') : null,
      regression: project.startsWith(this.regression),
    });
  }

  onEnd(result) {
    const meta = {
      startedAt: this.startedAt.toISOString(),
      durationMs: result.duration ?? Date.now() - this.startedAt.getTime(),
      status: result.status,
      commit: process.env.GITHUB_SHA || tryRun('git', ['rev-parse', 'HEAD']),
      branch: process.env.GITHUB_REF_NAME || tryRun('git', ['rev-parse', '--abbrev-ref', 'HEAD']),
      os: osName(),
      node: process.versions.node,
      playwright: playwrightVersion(),
      projects: this.projects,
      ci: !!process.env.CI,
    };
    const report = buildReport([...this.records.values()], meta);
    mkdirSync(this.outputDir, { recursive: true });
    writeFileSync(path.join(this.outputDir, 'traceability.json'), JSON.stringify(report, null, 2) + '\n');
    writeFileSync(path.join(this.outputDir, 'traceability.md'), renderMarkdown(report));
    console.log(`\n验收追溯报告：${path.join(this.outputDir, 'traceability.md')}（验收项 ${report.summary.itemsPassed}/${report.summary.itemsTotal} 通过）`);
  }
}
