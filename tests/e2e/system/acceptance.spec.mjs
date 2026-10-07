// System-level acceptance: the real CLI and `todo serve` on one data
// directory, the fixed mock model and the fixed mock agents. These checks
// do not depend on a browser (the browser specs cover the same features in
// the page on Chromium, WebKit and Firefox).
import { createHash } from 'node:crypto';
import { copyFileSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test, expect } from '../support/fixtures.mjs';
import { MODEL_KEY, todoBin, waitFor } from '../support/harness.mjs';
import { INTAKE_CASES, localDay } from '../support/mock-model.mjs';

const byTitle = (tasks) => Object.fromEntries(tasks.map((t) => [t.title, t]));
const runOf = (out) => out.run;

test.describe('model features with the fixed mock model', () => {
  test('natural language: multi-task split with dependencies, one clarifying question, accept and undo', { tag: ['@AT-01'] }, async ({ app }) => {
    // Every case of the fixed test set is exercised somewhere in the suite.
    expect(INTAKE_CASES.map((c) => c.id.slice(0, 5))).toEqual(['NL-01', 'NL-02', 'NL-03', 'NL-04']);

    const s = await app.cli('ai', 'add', '明天下午前完成产品发布准备，先整理需求，再更新页面，最后让命令行智能体检查链接');
    expect(s.status).toBe('pending'); // execute-before-confirm mode: preview only
    expect(s.items.map((i) => i.fields.title)).toEqual(['整理需求', '更新页面', '检查链接']);
    expect((await app.cli('list')).tasks).toEqual([]);

    const applied = await app.cli('ai', 'apply', s.id);
    expect(applied.result).toMatchObject({ created: 3, failed: 0 });
    const tasks = byTitle((await app.cli('list')).tasks);
    expect(tasks['整理需求']).toMatchObject({ priority: 'high', tags: ['发布'] });
    expect(new Date(tasks['整理需求'].due_at).toISOString()).toBe(new Date(`${localDay(1).replace(' ', 'T')}:00+08:00`).toISOString());
    expect(tasks['更新页面'].depends_on).toEqual([tasks['整理需求'].id]);
    expect(tasks['检查链接'].depends_on).toEqual([tasks['更新页面'].id]);
    // No deadline is invented where the user gave none.
    expect(tasks['更新页面'].due_at).toBeNull();
    const hist = (await app.cli('history', tasks['整理需求'].id)).history;
    expect(hist[0]).toMatchObject({ action: 'create', actor: 'llm' });

    // Missing deadline: asked once, then continued with the answer.
    const q = await app.cli('ai', 'add', '安排评审会');
    expect(q.status).toBe('needs_input');
    expect(q.questions).toEqual(['评审会需要在什么时候之前安排好？']);
    const a = await app.cli('ai', 'answer', q.id, '周五');
    expect(a.status).toBe('pending');
    expect(a.items[0].fields).toMatchObject({ title: '安排评审会', due_text: '周五' });
    await app.cli('ai', 'apply', a.id);
    expect(byTitle((await app.cli('list')).tasks)['安排评审会'].due_at).not.toBeNull();

    // A wrongly created task can be undone.
    await app.cli('ai', 'undo', a.id);
    expect(byTitle((await app.cli('list')).tasks)['安排评审会']).toBeUndefined();
    expect(app.model.requests.filter((r) => r.kind === 'intake')).toHaveLength(3);
  });

  test('natural language: a selected task is updated instead of duplicated', { tag: ['@AT-01'] }, async ({ app }) => {
    const t = (await app.cli('add', '准备演示')).task;
    const s = await app.cli('ai', 'add', '--select', t.id, '这个很急');
    expect(s.items).toHaveLength(1);
    expect(s.items[0]).toMatchObject({ kind: 'update', task_id: t.id });
    await app.cli('ai', 'apply', s.id);
    const after = (await app.cli('list')).tasks;
    expect(after).toHaveLength(1);
    expect(after[0].priority).toBe('urgent');
  });

  test('decision support: summary, ordered recommendations with reasons, diff, accept and undo', { tag: ['@AT-02'] }, async ({ app }) => {
    await app.cli('add', '整理照片', '-p', 'low');
    const top = (await app.cli('add', '发布准备', '-p', 'high')).task;
    const d = await app.cli('ai', 'decide');
    expect(d.summary).toContain('建议先处理「发布准备」');
    expect(d.recommendations.map((r) => r.title)).toEqual(['发布准备', '整理照片']);
    expect(d.recommendations[0]).toMatchObject({ reason: '优先级最高，先做', estimate: '1h' });
    expect(d.risks[0]).toMatchObject({ task_id: top.id, level: 'high' });
    expect(d.items[0]).toMatchObject({ kind: 'change', status: 'pending', task_id: top.id });
    expect(d.items[0].diff[0]).toMatchObject({ field: 'priority', before: 'high', after: 'urgent' });

    await app.cli('ai', 'apply', d.id, '1');
    expect((await app.cli('show', top.id)).task.priority).toBe('urgent');
    await app.cli('ai', 'undo', d.id, '1');
    expect((await app.cli('show', top.id)).task.priority).toBe('high');
    const hist = (await app.cli('history', top.id)).history.map((e) => e.actor);
    expect(hist.filter((a) => a === 'llm').length).toBeGreaterThanOrEqual(1);
  });

  test('prompt templates: summarise, keep the original, reuse with variables, keep special characters', { tag: ['@AT-04'] }, async ({ app }) => {
    const original = [
      '分析 {{project}} 在 {{week}} 的周报',
      '背景：团队每周五提交周报',
      '1. 阅读周报全文',
      '2. 列出风险与阻塞',
      '不要编造周报中没有的数据',
      '输出 markdown 表格；保留 `grep -E "a|b" <file>` 与 & 符号',
    ].join('\n');
    const file = path.join(app.dir, 'prompt.md');
    writeFileSync(file, original);
    const s = await app.cli('prompt', 'summarize', file, '--save', '--name', '周报分析');
    expect(s.summary).toMatchObject({
      goal: '分析 {{project}} 在 {{week}} 的周报',
      steps: ['阅读周报全文', '列出风险与阻塞'],
      constraints: ['不要编造周报中没有的数据'],
    });
    expect(s.summary.variables.map((v) => v.name)).toEqual(['project', 'week']);
    expect(s.template.original).toBe(original);

    const shown = await app.cli('prompt', 'show', '周报分析', '--original');
    expect(JSON.stringify(shown)).toContain('grep -E \\"a|b\\" <file>');
    const missing = await app.cliFails('prompt', 'render', '周报分析', '--var', 'project=官网');
    expect(missing.error.message).toContain('week');
    const r = await app.cli('prompt', 'render', '周报分析', '--var', 'project=官网', '--var', 'week=第 41 周');
    expect(r.prompt).toContain('分析 官网 在 第 41 周 的周报');
    expect(r.prompt).toContain('`grep -E "a|b" <file>` 与 & 符号');

    // Editing creates a new version; the original stays viewable.
    await app.cli('prompt', 'edit', '周报分析', '--step', '给出改进建议');
    const versions = await app.cli('prompt', 'versions', '周报分析');
    expect(JSON.stringify(versions)).toContain('"version":2');
    const exported = await app.run(['prompt', 'export', '周报分析', '--format', 'json'], { json: false });
    expect(exported.status).toBe(0);
    const doc = JSON.parse(exported.stdout);
    expect(JSON.stringify(doc)).toContain('grep -E \\"a|b\\" <file>');

    // Directly usable for an agent task.
    const task = await app.cli('prompt', 'task', '周报分析', '--var', 'project=官网', '--var', 'week=第 41 周');
    expect(task.task.description).toContain('分析 官网 在 第 41 周 的周报');
    await app.addAgent('writer', 'all');
    const dry = await app.cli('agent', 'run', task.task.id, '--agent', 'writer', '--dry-run');
    expect(JSON.stringify(dry)).toContain('分析 官网 在 第 41 周 的周报');
  });
});

test.describe('agents with the fixed mock agent script', () => {
  test('four write-back types: text, file, command line output and a real commit', { tag: ['@AT-05'] }, async ({ app }) => {
    await app.addAgent('reporter', 'all', '--desc', '写报告');
    const t = (await app.cli('add', '生成周报')).task;
    const run = runOf(await app.cli('agent', 'run', t.id, '--agent', 'reporter', '--complete'));
    expect(run.status).toBe('succeeded');
    const results = (await app.cli('agent', 'results', t.id)).results;
    const of = (type) => results.find((r) => r.type === type);
    expect(results.map((r) => r.type).sort()).toEqual(['command_output', 'commit', 'file', 'text']);
    for (const r of results) expect(r).toMatchObject({ agent: 'reporter', run_id: run.id, task_id: t.id });

    expect(of('text').data.text).toContain('「生成周报」已完成');
    const report = path.join(app.workspace, 'report.md');
    expect(of('file').data).toMatchObject({ path: report, exists: true });
    expect(of('file').data.sha256).toBe(createHash('sha256').update(readFileSync(report)).digest('hex'));
    const repo = of('commit').data.repo;
    const head = spawnSync('git', ['rev-parse', 'HEAD'], { cwd: repo, encoding: 'utf8' }).stdout.trim();
    expect(of('commit').data).toMatchObject({ branch: 'main', commit: head });
    expect(of('command_output').data).toMatchObject({ argv: ['git', 'log', '--oneline', '-1'], exit_code: 0 });
    expect(of('command_output').data.stdout).toContain(head.slice(0, 7));

    // Visible through the REST API as well (web and CLI read the same data).
    const api = await app.api('GET', `/api/tasks/${t.id}/results`);
    expect(api.body.results.map((r) => r.id).sort()).toEqual(results.map((r) => r.id).sort());
    const show = await app.cli('show', t.id);
    expect(show.task.status).toBe('done');
    expect(show.history.map((e) => e.actor)).toContain('agent/reporter');
  });

  test('lifecycle: failure keeps the task open, retry keeps earlier attempts, cancel keeps intermediate results', { tag: ['@AT-03'] }, async ({ app }) => {
    await app.addAgent('broken', 'fail');
    await app.addAgent('flaky', 'flaky');
    await app.addAgent('slow', 'slow');
    const t = (await app.cli('add', '修复构建')).task;

    // Failure: error and exit code recorded, task never marked done.
    const failed = await app.run(['agent', 'run', t.id, '--agent', 'broken', '--complete']);
    expect(failed.status).not.toBe(0);
    const fr = runOf(JSON.parse(failed.stdout));
    expect(fr).toMatchObject({ status: 'failed', exit_code: 3 });
    expect(fr.error).toContain('missing dependency libfoo');
    expect((await app.cli('show', t.id)).task.status).not.toBe('done');

    // Retry: a new attempt, the failed one is kept.
    const first = runOf(JSON.parse((await app.run(['agent', 'run', t.id, '--agent', 'flaky'])).stdout));
    expect(first.status).toBe('failed');
    const retried = runOf(await app.cli('agent', 'retry', first.id));
    expect(retried.status).toBe('succeeded');
    expect(retried.attempts.map((a) => a.status)).toEqual(['failed', 'succeeded']);
    expect(retried.attempts[0].error).toContain('transient failure');

    // Cancel a running agent: stops, intermediate result stays, reason kept.
    const started = runOf(await app.cli('agent', 'run', t.id, '--agent', 'slow', '--detach'));
    await waitFor(async () => runOf(await app.cli('agent', 'show', started.id)).results?.length > 0, { message: 'intermediate result' });
    await app.cli('agent', 'cancel', started.id);
    const cancelled = await waitFor(async () => {
      const r = runOf(await app.cli('agent', 'show', started.id));
      return r.status === 'cancelled' && r;
    }, { message: 'cancelled run' });
    expect(cancelled.error).toContain('取消');
    expect(cancelled.results.map((r) => r.data.text)).toEqual(['中间结果：已完成一半']);
    expect((await app.cli('show', t.id)).task.status).not.toBe('done');
    const runs = (await app.cli('agent', 'runs', '--task', t.id)).runs.map((r) => `${r.agent}:${r.status}`).sort();
    expect(runs).toEqual(['broken:failed', 'flaky:succeeded', 'slow:cancelled']);
  });
});

test.describe('permissions', () => {
  test('revoking auto mode, adding a confirmation or removing the agent stops further automatic starts', { tag: ['@AT-11'] }, async ({ app }) => {
    await app.addAgent('linkcheck', 'all', '--desc', '检查链接');
    const t = (await app.cli('add', '检查链接', '-p', 'high', '-t', 'agent')).task;
    const runCount = async () => (await app.cli('agent', 'runs', '--task', t.id)).runs.length;

    // Granted: auto mode starts the agent without asking.
    await app.cli('llm', 'mode', 'auto');
    const d1 = await app.cli('ai', 'decide');
    const agentItem = d1.items.find((i) => i.kind === 'agent');
    expect(agentItem.status).toBe('applied');
    expect(await runCount()).toBe(1);
    const actions = (await app.cli('llm', 'actions')).actions;
    expect(actions[0]).toMatchObject({ kind: 'agent', status: 'started', actor: 'llm/auto' });

    // Revoked via the confirm list: the start waits.
    await app.cli('llm', 'confirm', 'add', 'agent.start');
    const d2 = await app.cli('ai', 'decide');
    expect(d2.items.find((i) => i.kind === 'agent')).toMatchObject({ status: 'pending' });
    expect(await runCount()).toBe(1);

    // Revoked via the mode: suggestions only.
    await app.cli('llm', 'confirm', 'remove', 'agent.start');
    await app.cli('llm', 'mode', 'suggest');
    const d3 = await app.cli('ai', 'decide');
    expect(d3.items.find((i) => i.kind === 'agent')).toMatchObject({ status: 'pending', confirm_reason: expect.stringContaining('仅建议') });
    expect(await runCount()).toBe(1);

    // Revoked by removing the agent: reported as unavailable, never started.
    await app.cli('llm', 'mode', 'auto');
    await app.cli('llm', 'agent', 'remove', 'linkcheck');
    const d4 = await app.cli('ai', 'decide');
    expect(d4.items.find((i) => i.kind === 'agent')).toBeUndefined();
    expect(await runCount()).toBe(1);

    // Privilege escalation is refused in every mode.
    const sudo = await app.cliFails('agent', 'add', 'root', '--', 'sudo', 'ls');
    expect(sudo.status).not.toBe(0);
  });
});

test.describe('data safety', () => {
  test('secrets are redacted from model requests, logs, results, exports and stored data', { tag: ['@AT-10'] }, async ({ app }) => {
    const token = 'ghp_abcdefghijklmnopqrstuvwxyz0123456789';
    await app.cli('ai', 'add', `联系运维轮换密钥，token=${token}`);
    const t = (await app.cli('add', '部署服务')).task;
    await app.addAgent('leaky', 'secret');
    const run = runOf(await app.cli('agent', 'run', t.id, '--agent', 'leaky'));
    expect(run.status).toBe('succeeded');

    // The model never received the token; the key only in the auth header.
    for (const r of app.model.requests) {
      expect(r.raw).not.toContain(token);
      expect(r.raw).not.toContain(MODEL_KEY);
      expect(r.authorization).toBe(`Bearer ${MODEL_KEY}`);
    }
    const logs = JSON.stringify(await app.cli('agent', 'logs', run.id));
    const results = JSON.stringify(await app.cli('agent', 'results', t.id));
    for (const s of ['sk-mockagentsecret1234567890abcd', 'mockbearertoken0123456789abcdef', 'hunter2hunter2']) {
      expect(logs).not.toContain(s);
      expect(results).not.toContain(s);
    }
    expect(logs).toContain('[REDACTED]');

    // Status, configuration, exports, sessions and the files on disk never
    // contain the model key.
    const surfaces = [
      JSON.stringify(await app.cli('llm', 'status')),
      JSON.stringify(await app.cli('status')),
      JSON.stringify(await app.cli('ai', 'list')),
      JSON.stringify(await app.cli('llm', 'calls')),
      (await app.run(['export'])).stdout,
      JSON.stringify((await app.api('GET', '/api/llm/status')).body),
    ];
    for (const s of surfaces) expect(s).not.toContain(MODEL_KEY);
    expect(JSON.stringify(await app.cli('ai', 'list'))).not.toContain(token);
    for (const f of readdirSync(app.dir).filter((f) => /\.(db|json|db-wal)$/.test(f))) {
      expect(readFileSync(path.join(app.dir, f)).includes(MODEL_KEY), f).toBe(false);
    }
    expect(JSON.parse(readFileSync(path.join(app.dir, 'llm.json'), 'utf8'))).not.toHaveProperty('api_key');
    expect(statSync(path.join(app.dir, 'llm.json')).mode & 0o077).toBe(0);
  });

  test('model unavailable: task management keeps working, model features report a clear, retryable error', { tag: ['@AT-12'] }, async ({ app }) => {
    app.model.setDown(true);
    const add = await app.cliFails('ai', 'add', '写周报');
    expect(add.error).toMatchObject({ code: 'llm_server_error', retryable: true });
    expect(add.error.hint).toContain('todo llm use');
    const test = await app.run(['llm', 'test']);
    expect(test.status).not.toBe(0);
    expect(JSON.parse(test.stdout)).toMatchObject({ ok: false, error: { code: 'llm_server_error' } });

    // Core task management via CLI and REST API is unaffected.
    const t = (await app.cli('add', '手动任务', '-p', 'high')).task;
    await app.cli('edit', t.id, '--notes', '模型不可用时编辑');
    await app.cli('done', t.id);
    expect((await app.cli('list', '--status', 'done')).tasks.map((x) => x.title)).toEqual(['手动任务']);
    const created = await app.api('POST', '/api/tasks', { title: '网页任务' });
    expect(created.status).toBe(201);
    expect((await app.api('GET', '/api/health')).status).toBe(200);
    // Decisions fall back to local rules and say so.
    const d = await app.cli('ai', 'decide');
    expect(d.degraded).toBe(true);
    expect(d.warnings.join('')).toContain('大模型不可用');
    // Local prompt summaries still work.
    writeFileSync(path.join(app.dir, 'p.md'), '检查链接\n1. 扫描页面\n输出失效列表');
    expect((await app.cli('prompt', 'summarize', path.join(app.dir, 'p.md'), '--local')).summary.steps).toEqual(['扫描页面']);

    // The model comes back: features work again without a restart.
    app.model.setDown(false);
    expect((await app.cli('ai', 'add', '写周报')).items).toHaveLength(1);
  });

  test('export and import: a full round trip into a fresh data directory', { tag: ['@AT-09'] }, async ({ app }) => {
    const a = (await app.cli('add', '导出任务甲', '-p', 'high', '-t', 'x,y', '--due', 'tomorrow', '-n', '备注：中文 & <特殊字符>')).task;
    const b = (await app.cli('add', '导出任务乙', '--parent', a.id)).task;
    await app.cli('done', b.id);
    const c = (await app.cli('add', '已删除任务')).task;
    await app.cli('delete', c.id);
    const file = path.join(app.dir, 'export.json');
    await app.cli('export', '-o', file);
    const exported = JSON.parse(readFileSync(file, 'utf8'));
    expect(exported.tasks).toHaveLength(3);

    const other = mkdtempSync(path.join(tmpdir(), 'todo-import-'));
    try {
      const run = (...args) => spawnSync(todoBin, ['--data-dir', other, '--json', ...args], { env: app.env, encoding: 'utf8' });
      const imp = run('import', file);
      expect(imp.status, imp.stderr).toBe(0);
      expect(JSON.parse(imp.stdout)).toMatchObject({ inserted: 3 });
      const got = byTitle(JSON.parse(run('list', '--status', 'all').stdout).tasks);
      expect(JSON.parse(run('list', '--deleted').stdout).tasks.map((x) => x.id)).toEqual([c.id]);
      expect(got['导出任务甲']).toMatchObject({ id: a.id, priority: 'high', tags: ['x', 'y'], notes: '备注：中文 & <特殊字符>', due_at: a.due_at });
      expect(got['导出任务乙']).toMatchObject({ parent_id: a.id, status: 'done' });
      const hist = JSON.parse(run('history', b.id).stdout).history.map((e) => e.action);
      expect(hist).toEqual(['create', 'complete', 'import']);
      // Importing again changes nothing (merge by id, newer wins).
      expect(JSON.parse(run('import', file).stdout)).toMatchObject({ inserted: 0, updated: 0 });
    } finally {
      rmSync(other, { recursive: true, force: true });
    }
  });

  test('backup and restore: the backup is private and restores tasks, history, agent results and templates', { tag: ['@AT-09'] }, async ({ app }) => {
    await app.addAgent('reporter', 'all');
    const t = (await app.cli('add', '备份任务', '-p', 'urgent')).task;
    await app.cli('agent', 'run', t.id, '--agent', 'reporter');
    writeFileSync(path.join(app.dir, 'p.md'), '总结 {{topic}}\n1. 阅读材料');
    await app.cli('prompt', 'summarize', path.join(app.dir, 'p.md'), '--save', '--name', '总结模板');
    const before = {
      tasks: (await app.cli('list', '--status', 'all')).tasks,
      history: (await app.cli('history', t.id)).history,
      results: (await app.cli('agent', 'results', t.id)).results,
    };

    const { path: backup } = await app.cli('backup');
    expect(statSync(backup).mode & 0o777).toBe(0o600);
    expect(statSync(path.dirname(backup)).mode & 0o077).toBe(0);

    // Disaster: the data directory is lost. Restore = put the backup back.
    await app.stop();
    const restored = mkdtempSync(path.join(tmpdir(), 'todo-restore-'));
    try {
      copyFileSync(backup, path.join(restored, 'todo.db'));
      const run = (...args) => {
        const r = spawnSync(todoBin, ['--data-dir', restored, '--json', ...args], { env: app.env, encoding: 'utf8' });
        expect(r.status, r.stderr).toBe(0);
        return JSON.parse(r.stdout);
      };
      expect(run('list', '--status', 'all').tasks).toEqual(before.tasks);
      expect(run('history', t.id).history).toEqual(before.history);
      expect(run('agent', 'results', t.id).results).toEqual(before.results);
      expect(run('prompt', 'show', '总结模板').template.name).toBe('总结模板');
      // The restored data is writable and consistent.
      run('done', t.id);
      expect(run('show', t.id).task.status).toBe('done');
    } finally {
      rmSync(restored, { recursive: true, force: true });
    }
  });

  test('crash and restart: data, runs and results survive; an interrupted run is marked unknown, never success', { tag: ['@AT-08'] }, async ({ app }) => {
    await app.addAgent('slow', 'slow');
    const t = (await app.cli('add', '长时间任务')).task;
    const started = await app.api('POST', `/api/tasks/${t.id}/agent-runs`, { agent: 'slow' });
    expect(started.status).toBe(201);
    const id = started.body.run.id;
    await waitFor(async () => (await app.api('GET', `/api/agent-runs/${id}`)).body.run.results?.length > 0, { message: 'intermediate result' });
    const edited = (await app.cli('edit', t.id, '--notes', '崩溃前写入')).task;

    await app.restart({ crash: true }); // SIGKILL: no clean shutdown
    const run = await waitFor(async () => {
      const r = (await app.api('GET', `/api/agent-runs/${id}`)).body.run;
      return r.status !== 'running' && r.status !== 'queued' && r;
    }, { message: 'orphaned run detection' });
    expect(run.status).toBe('unknown');
    expect(run.error).toContain('结果未知');
    expect(run.results.map((r) => r.data.text)).toEqual(['中间结果：已完成一半']);
    const task = (await app.api('GET', `/api/tasks/${t.id}`)).body.task;
    expect(task).toMatchObject({ notes: '崩溃前写入', version: edited.version });
    expect(task.status).not.toBe('done');

    // A clean restart keeps everything as well.
    await app.restart();
    expect((await app.api('GET', `/api/tasks/${t.id}`)).body.task.notes).toBe('崩溃前写入');
    expect((await app.api('GET', `/api/agent-runs/${id}`)).body.run.status).toBe('unknown');
    // Retrying an unknown run is a deliberate user action and works.
    expect((await app.api('POST', `/api/agent-runs/${id}/retry`, {})).status).toBe(200);
    await app.api('POST', `/api/agent-runs/${id}/cancel`, {});
  });

  test('a stale CLI edit is refused with a conflict and nothing is overwritten', { tag: ['@AT-07'] }, async ({ app }) => {
    const t = (await app.cli('add', '共享任务', '-n', '原备注')).task;
    const web = await app.api('PATCH', `/api/tasks/${t.id}`, { notes: '网页备注', version: t.version });
    expect(web.status).toBe(200);
    const stale = await app.cliFails('edit', t.id, '--notes', '命令行备注', '--if-version', String(t.version));
    expect(stale.status).toBe(4);
    expect(stale.error.code).toBe('version_conflict');
    expect((await app.cli('show', t.id)).task.notes).toBe('网页备注');
    // And the other way round through the API: 409 with a three-way diff.
    const cur = (await app.cli('show', t.id)).task;
    await app.cli('edit', t.id, '--notes', '命令行再次修改');
    const conflict = await app.api('PATCH', `/api/tasks/${t.id}`, { notes: '网页旧版本修改', version: cur.version });
    expect(conflict.status).toBe(409);
    expect(conflict.body.conflict.fields.find((f) => f.field === 'notes')).toMatchObject({ base: '网页备注', current: '命令行再次修改', yours: '网页旧版本修改' });
    expect((await app.cli('show', t.id)).task.notes).toBe('命令行再次修改');
  });
});
