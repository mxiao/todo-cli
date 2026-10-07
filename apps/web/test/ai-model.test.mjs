// Unit tests for apps/web/js/ai-model.js (run: node --test apps/web/test/).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  diffRows, diffValue, itemTitle, itemFields, itemActions, sessionCounts, resultLine, isTerminal, runStatusLabel,
  runActions, selectionLabel, formatDuration, runDuration, progressPercent, eventLine, resultDetails, agentSource,
  templateVariables, aiErrorMessage, isRetryable,
} from '../js/ai-model.js';
import { actorLabel, actionLabel } from '../js/model.js';

test('diffs show priorities, statuses and dates by their names', () => {
  const rows = diffRows({ diff: [
    { field: 'priority', before: 'medium', after: 'urgent' },
    { field: 'status', before: 'todo', after: 'in_progress' },
    { field: 'depends_on', before: [], after: ['发布准备', 'N1'] },
    { field: 'order', before: '当前位置', after: '第 1 位' },
    { field: 'prompts.decide', before: { a: 1 }, after: { a: 1 } },
  ] });
  assert.deepEqual(rows.map((r) => [r.label, r.before, r.after, r.changed]), [
    ['优先级', '中', '紧急', true],
    ['状态', '待办', '进行中', true],
    ['依赖', '（空）', '发布准备、N1', true],
    ['顺序', '当前位置', '第 1 位', true],
    ['prompts.decide', '{\n  "a": 1\n}', '{\n  "a": 1\n}', false],
  ]);
  assert.equal(diffValue('due_at', null), '（空）');
  assert.match(diffValue('due', '2026-10-09T10:00:00Z'), /^2026-10-09 \d{2}:00$/);
  assert.deepEqual(diffRows({}), []);
});

test('items: titles, proposed fields and the buttons each state allows', () => {
  const create = { n: 1, kind: 'create', status: 'pending', fields: { title: '整理需求', priority: 'high', tags: ['a', 'b'], due: '2026-10-09T10:00:00Z', due_text: '明天' } };
  assert.equal(itemTitle(create), '整理需求');
  assert.equal(itemTitle({ n: 2, kind: 'change', task_title: '发布' }), '发布');
  assert.equal(itemTitle({ n: 3, kind: 'agent', command: { agent: 'coder' } }), '智能体 coder');
  assert.equal(itemTitle({ n: 4, kind: 'command', command: { argv: ['make', 'test'] } }), 'make test');
  assert.equal(itemTitle({ n: 5, kind: 'optimize', optimize: { target: 'routing' } }), 'routing');
  const fields = Object.fromEntries(itemFields(create));
  assert.equal(fields['优先级'], '高');
  assert.equal(fields['标签'], '#a #b');
  assert.match(fields['截止时间'], /（明天）$/);

  const pending = { status: 'pending' };
  assert.deepEqual(itemActions(create, pending), { apply: true, reject: true, edit: true, undo: false });
  assert.deepEqual(itemActions({ ...create, status: 'applied' }, { status: 'partial' }), { apply: false, reject: false, edit: false, undo: true });
  assert.deepEqual(itemActions({ ...create, status: 'skipped' }, pending).edit, true);
  // Nothing is offered on a superseded decision, and commands cannot be undone.
  assert.deepEqual(itemActions(create, { status: 'superseded' }), { apply: false, reject: false, edit: false, undo: false });
  assert.equal(itemActions({ kind: 'command', status: 'applied' }, pending).undo, false);
  assert.equal(itemActions({ kind: 'agent', status: 'pending' }, pending).edit, false);

  assert.deepEqual(sessionCounts({ items: [pending, { status: 'applied' }, { status: 'applied' }] }),
    { pending: 1, applied: 2, rejected: 0, skipped: 0, failed: 0, undone: 0 });
  assert.equal(resultLine({ created: 2, updated: 1, not_executed: 1, failed: 0 }), '新增 2 · 修改 1 · 未执行 1 · 失败 0');
  assert.equal(resultLine(null), '');
});

test('run controls follow the server state machine', () => {
  const acts = (status, control = '') => Object.entries(runActions({ status, control })).filter(([, v]) => v).map(([k]) => k).sort();
  assert.deepEqual(acts('running'), ['cancel', 'pause']);
  assert.deepEqual(acts('running', 'pause'), ['cancel', 'resume']);
  assert.deepEqual(acts('running', 'cancel'), []);
  assert.deepEqual(acts('queued'), ['cancel', 'pause']);
  assert.deepEqual(acts('paused'), ['cancel', 'resume']);
  assert.deepEqual(acts('waiting_confirmation'), ['cancel', 'confirm', 'reject']);
  assert.deepEqual(acts('waiting_retry'), ['cancel', 'pause', 'retry']);
  for (const s of ['failed', 'partial', 'cancelled', 'unknown']) assert.deepEqual(acts(s), ['retry'], s);
  assert.deepEqual(acts('succeeded'), []);
  assert.equal(isTerminal('unknown'), true);
  assert.equal(isTerminal('paused'), false);
  assert.equal(runStatusLabel({ status: 'running', control: 'pause' }), '正在暂停…');
  assert.equal(runStatusLabel({ status: 'waiting_retry' }), '等待重试');
});

test('selection, durations and progress', () => {
  assert.equal(selectionLabel({ by: 'llm', reason: '需要改代码' }), '大模型自动选择：需要改代码');
  assert.equal(selectionLabel({ by: 'llm', degraded: true }), '大模型自动选择（模型不可用，已按规则选择）');
  assert.equal(selectionLabel({ by: 'user' }), '用户指定');
  assert.equal(formatDuration(850), '850 毫秒');
  assert.equal(formatDuration(12_400), '12 秒');
  assert.equal(formatDuration(185_000), '3 分 05 秒');
  assert.equal(formatDuration(3_720_000), '1 小时 02 分');
  assert.equal(formatDuration(null), '');
  const now = new Date('2026-10-07T10:00:30Z');
  assert.equal(runDuration({ status: 'running', started_at: '2026-10-07T10:00:00Z', paused_ms: 10_000 }, now), 20_000);
  assert.equal(runDuration({ status: 'succeeded', started_at: '2026-10-07T10:00:00Z', duration_ms: 5000 }, now), 5000);
  assert.equal(runDuration({ status: 'queued', duration_ms: 0 }, now), 0);
  assert.equal(progressPercent({ status: 'running', progress: 140 }), 100);
  assert.equal(progressPercent({ status: 'running' }), 0);
  assert.equal(progressPercent({ status: 'succeeded', progress: 20 }), 100);
});

test('log lines and results', () => {
  const l = eventLine({ kind: 'output', stream: 'stderr', message: 'boom', at: '2026-10-07T10:00:05Z', attempt: 2 });
  assert.equal(l.stream, 'stderr');
  assert.equal(l.cls, 'ev-output ev-stderr');
  assert.match(l.time, /^\d{2}:\d{2}:05$/);
  assert.equal(eventLine({ kind: 'progress', message: 'x' }).kind, '进度');

  const commit = Object.fromEntries(resultDetails({ type: 'commit', data: { repo: '/r', branch: 'main', commit: 'abc1234', files: ['a.js', 'b.js'] } }));
  assert.deepEqual(commit, { 仓库: '/r', 分支: 'main', 提交: 'abc1234', 文件: 'a.js\nb.js' });
  const cmd = Object.fromEntries(resultDetails({ type: 'command_output', data: { command: 'make test', exit_code: 0, stdout: 'ok' } }));
  assert.equal(cmd['退出码'], '0');
  const file = Object.fromEntries(resultDetails({ type: 'file', data: { path: '/x/r.md', exists: false, outside_workdir: true } }));
  assert.equal(file['状态'], '文件不存在');
  assert.ok(file['注意']);
  const st = Object.fromEntries(resultDetails({ type: 'task_status', data: { status: 'merged', url: 'https://x', local_status: 'done' } }));
  assert.equal(st['本任务状态'], '已完成');
  assert.equal(agentSource('coder'), '智能体 coder');
  assert.equal(agentSource('web'), '网页手动回写');
  assert.deepEqual(templateVariables({ latest: { summary: { variables: [{ name: 'p', default: 'x' }] } } }), [{ name: 'p', description: '', default: 'x' }]);
  assert.deepEqual(templateVariables(null), []);
});

test('model and agent errors become short Chinese messages with hints', () => {
  const err = (code, message, extra = {}) => ({ code, message, body: { error: { code, message, ...extra } } });
  assert.equal(aiErrorMessage(err('llm_timeout', 'timed out', { hint: '稍后重试', retryable: true })), '模型请求超时（稍后重试）');
  assert.equal(aiErrorMessage(err('missing_variables', 'x', { variables: ['project', 'week'] })), '缺少模板变量：project、week');
  assert.equal(aiErrorMessage(err('invalid_state', 'run already ended')), '当前状态不允许该操作：run already ended');
  assert.equal(aiErrorMessage(err('llm_unavailable', 'x')), '大模型功能不可用（模型模块未能启动）');
  assert.equal(aiErrorMessage(err('weird', 'raw message')), 'raw message');
  assert.equal(aiErrorMessage(err('weird', 'raw'), () => 'fallback'), 'fallback');
  assert.equal(isRetryable(err('llm_auth_failed', 'x', { retryable: false })), false);
  assert.equal(isRetryable(err('llm_rate_limited', 'x')), true);
  assert.equal(isRetryable({ code: 'network_error' }), true);
});

test('history names model and agent actors and agent events', () => {
  assert.equal(actorLabel('agent/coder'), '智能体 coder');
  assert.equal(actorLabel('llm'), '大模型');
  assert.equal(actorLabel('llm/auto'), '大模型（自动执行）');
  assert.equal(actionLabel('agent_result'), '智能体结果回写');
  assert.equal(actionLabel('agent_failed'), '智能体执行失败');
});
