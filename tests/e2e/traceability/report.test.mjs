// Unit tests of the traceability report (node --test).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildReport, renderMarkdown } from './report.mjs';
import { ACCEPTANCE } from './requirements.mjs';

const rec = (over) => ({ title: 't', file: 'tests/e2e/x.spec.mjs', project: 'system', tags: [], annotations: [], outcome: 'passed', durationMs: 10, error: null, ...over });

test('items aggregate their tagged tests: any failure fails, skipped only is not run', () => {
  const rep = buildReport([
    rec({ title: 'a', tags: ['@AT-01'] }),
    rec({ title: 'b', tags: ['@AT-01', '@AT-14'], project: 'webkit', outcome: 'failed', error: 'boom' }),
    rec({ title: 'c', tags: ['@AT-02'], outcome: 'skipped' }),
    rec({ title: 'd', tags: ['@AT-03'], outcome: 'flaky' }),
  ]);
  const item = (id) => rep.items.find((i) => i.id === id);
  assert.equal(item('AT-01').status, 'failed');
  assert.equal(item('AT-01').tests.length, 2);
  assert.equal(item('AT-02').status, 'not_run');
  assert.equal(item('AT-03').status, 'passed');
  assert.equal(item('AT-05').status, 'not_run');
  assert.equal(rep.items.length, ACCEPTANCE.length);
  assert.deepEqual(rep.summary, { tests: 4, passed: 1, failed: 1, flaky: 1, skipped: 1, itemsPassed: 1, itemsTotal: ACCEPTANCE.length, regression: 0 });
});

test('the matrix uses branded browsers when they ran and the engine otherwise', () => {
  const rep = buildReport([
    rec({ project: 'chromium', tags: ['@AT-14'], annotations: [{ type: 'browser', description: 'chromium 153.0' }] }),
    rec({ project: 'webkit', tags: ['@AT-14'], annotations: [{ type: 'browser', description: 'webkit 26.6' }] }),
    rec({ project: 'terminal', tags: ['@AT-15'], annotations: [{ type: 'terminal', description: 'iTerm2' }] }),
  ], { os: 'macOS 15.1 (arm64)' });
  const row = (t) => rep.matrix.find((m) => m.target.startsWith(t));
  assert.equal(row('macOS').status, 'recorded');
  assert.equal(row('macOS').detail, 'macOS 15.1 (arm64)');
  assert.deepEqual([row('Chrome').status, row('Chrome').via, row('Chrome').versions], ['engine_passed', 'chromium', ['chromium 153.0']]);
  assert.deepEqual([row('Edge').status, row('Edge').via], ['engine_passed', 'chromium']);
  assert.deepEqual([row('Safari').status, row('Safari').versions], ['engine_passed', ['webkit 26.6']]);
  assert.equal(row('Firefox').status, 'not_run');
  // A branded browser that ran is reported as itself.
  const branded = buildReport([rec({ project: 'msedge', annotations: [{ type: 'browser', description: 'chromium 141.0' }] })]);
  assert.deepEqual([branded.matrix.find((m) => m.target.startsWith('Edge')).status, branded.matrix.find((m) => m.target.startsWith('Edge')).via], ['passed', null]);
  assert.equal(row('iTerm2').status, 'passed');
  assert.equal(row('Terminal.app').status, 'not_run');
});

test('untraced tests are listed, regression tests are only counted', () => {
  const rep = buildReport([
    rec({ title: 'no tag' }),
    rec({ title: 'old suite', project: 'regression-chromium', regression: true }),
  ]);
  assert.deepEqual(rep.untraced, ['system › no tag']);
  assert.equal(rep.summary.regression, 1);
});

test('the markdown report lists items, deferred voice input, the matrix and failures', () => {
  const md = renderMarkdown(buildReport([
    rec({ title: 'sync | fast', tags: ['@AT-06', '@AT-14'], project: 'firefox', annotations: [{ type: 'latency', description: 'CLI 创建 → 网页: 240 ms' }] }),
    rec({ title: 'tui', tags: ['@AT-15'], project: 'terminal', annotations: [{ type: 'terminal', description: 'iTerm2' }] }),
    rec({ title: 'broken', tags: ['@AT-07'], outcome: 'failed', error: 'expected 1\nreceived 2' }),
  ], { startedAt: '2026-10-07T00:00:00Z', durationMs: 1500, commit: 'abc123', os: 'macOS 15.1', node: '22', playwright: '1.63.0', projects: ['system', 'firefox'] }));
  assert.match(md, /# 端到端验收测试报告/);
  assert.match(md, /代码版本：abc123/);
  assert.match(md, /\| AT-06 \| CLI 与网页双向同步 2 秒内可见 \| .* \| ✅ 通过 \| .*sync \\\| fast（firefox:通过）/);
  assert.match(md, /\| AT-07 .* ❌ 失败/);
  assert.match(md, /tui（terminal\/iTerm2:通过）/);
  assert.match(md, /## 同步延迟实测（上限 2000 ms）\n\n- firefox · CLI 创建 → 网页: 240 ms\n\n/);
  assert.match(md, /\| 验收 9 \| 语音录入与补充 \| 延期：/);
  assert.match(md, /## 兼容性矩阵/);
  assert.match(md, /## 失败详情\n\n- AT-07 · system › broken：expected 1 received 2/);
});
