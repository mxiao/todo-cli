// Browser acceptance on Chromium, WebKit (Safari engine) and Firefox:
// the real page against a real `todo serve`, with the CLI on the same data,
// the fixed mock model and the fixed mock agents. Nothing in the page is
// mocked or intercepted.
import { webTest as test, expect, openApp, openAI, row, rows, measure } from '../support/fixtures.mjs';
import { MODEL_KEY, waitFor } from '../support/harness.mjs';

const tags = (...ids) => ({ tag: [...ids, '@AT-14'] });
const panel = (page) => page.getByTestId('ai-panel');

// latency records a measured sync delay in the report.
function latency(what, ms) {
  test.info().annotations.push({ type: 'latency', description: `${what}: ${ms} ms` });
  expect(ms, what).toBeLessThanOrEqual(2000);
}

async function newestRun(app, taskId, agent) {
  return waitFor(async () => (await app.cli('agent', 'runs', '--task', taskId)).runs.find((r) => r.agent === agent), { message: `run of ${agent}` });
}

test('CLI and web changes are visible on the other side within 2 seconds', tags('@AT-06'), async ({ page, app }) => {
  await openApp(page, app.url);

  // CLI → page
  const t = (await app.cli('add', '命令行新建', '-p', 'urgent')).task;
  latency('CLI 创建 → 网页', await measure(async () => (await row(page, '命令行新建').count()) === 1, 2000, 'CLI create'));
  await row(page, '命令行新建').getByTestId('task-title').click();
  await app.cli('edit', t.id, '--notes', '命令行补充');
  latency('CLI 编辑 → 网页', await measure(async () => (await page.getByTestId('detail-notes').textContent()) === '命令行补充', 2000, 'CLI edit'));
  await app.cli('done', t.id);
  latency('CLI 完成 → 网页', await measure(async () => (await row(page, '命令行新建').getByTestId('task-status').getAttribute('data-status')) === 'done', 2000, 'CLI done'));
  await app.cli('delete', t.id);
  latency('CLI 删除 → 网页', await measure(async () => (await rows(page).count()) === 0, 2000, 'CLI delete'));

  // page → CLI
  await page.getByTestId('quick-add-input').fill('网页新建 #web !high');
  await page.getByTestId('quick-add-input').press('Enter');
  latency('网页创建 → CLI', await measure(async () => (await app.cli('list')).tasks.some((x) => x.title === '网页新建' && x.priority === 'high'), 2000, 'web create'));
  await row(page, '网页新建').getByTestId('task-toggle').click();
  latency('网页完成 → CLI', await measure(async () => (await app.cli('list', '--status', 'done')).tasks.some((x) => x.title === '网页新建'), 2000, 'web done'));
  const hist = (await app.cli('history', (await app.cli('list', '--status', 'all')).tasks[0].id)).history;
  expect(hist.map((e) => `${e.action}:${e.actor}`)).toEqual(['create:web', 'complete:web']);
});

test('concurrent edits: the page reports the conflict and nothing is silently overwritten', tags('@AT-07'), async ({ page, app }) => {
  const t = (await app.cli('add', '共享任务', '-n', '原备注')).task;
  await openApp(page, app.url);
  await row(page, '共享任务').getByTestId('task-title').click();
  await page.getByTestId('detail-edit').click();
  await page.getByTestId('field-notes').fill('网页的备注');
  await app.cli('edit', t.id, '--notes', '命令行的备注', '-p', 'high');
  await expect(page.getByTestId('editor-banner')).toContainText('命令行', { timeout: 2000 });
  await page.getByTestId('editor-save').click();

  const dialog = page.getByTestId('conflict-dialog');
  await expect(dialog).toBeVisible();
  const notes = dialog.locator('[data-testid="conflict-field"][data-field="notes"]');
  await expect(notes).toContainText('原备注');
  await expect(notes).toContainText('命令行的备注');
  await expect(notes).toContainText('网页的备注');
  expect((await app.cli('show', t.id)).task.notes).toBe('命令行的备注');
  await page.getByTestId('conflict-mine').click();
  await expect(dialog).toBeHidden();
  await expect(page.getByTestId('detail-notes')).toHaveText('网页的备注');
  // The deliberate choice applied the web notes on top; the CLI's priority stays.
  expect((await app.cli('show', t.id)).task).toMatchObject({ notes: '网页的备注', priority: 'high' });
});

test('natural language: preview, one clarifying question, accept and undo in the page', tags('@AT-01'), async ({ page, app }) => {
  await openApp(page, app.url);
  await expect(page.getByTestId('ai-model-chip')).toHaveText('模型：mock-gpt');
  await openAI(page, 'intake');
  await page.getByTestId('intake-text').fill('明天下午前完成产品发布准备，先整理需求，再更新页面，最后让命令行智能体检查链接');
  await page.getByTestId('intake-submit').click();
  const items = panel(page).getByTestId('session-item');
  await expect(items).toHaveCount(3);
  await expect(panel(page).getByTestId('session-status')).toHaveText('待确认');
  await expect(items.nth(0).getByTestId('item-fields')).toContainText('整理需求');
  await expect(items.nth(0).getByTestId('item-fields')).toContainText('明天下午前');
  expect((await app.cli('list')).tasks).toEqual([]); // preview only

  await panel(page).getByTestId('session-apply-all').click();
  await expect(items.nth(2)).toHaveAttribute('data-status', 'applied');
  await expect(panel(page).getByTestId('session-result')).toContainText('新增 3');
  for (const title of ['整理需求', '更新页面', '检查链接']) await expect(row(page, title)).toHaveCount(1);
  const tasks = Object.fromEntries((await app.cli('list')).tasks.map((t) => [t.title, t]));
  expect(tasks['检查链接'].depends_on).toEqual([tasks['更新页面'].id]);
  expect((await app.cli('history', tasks['整理需求'].id)).history[0].actor).toBe('llm');

  // Missing deadline → asked once, answered in place.
  await page.getByTestId('intake-text').fill('安排评审会');
  await page.getByTestId('intake-submit').click();
  await expect(panel(page).getByTestId('session-status')).toHaveText('待补充信息');
  await expect(panel(page).getByTestId('session-question')).toHaveText('评审会需要在什么时候之前安排好？');
  await panel(page).getByTestId('session-answer-text').fill('周五');
  await panel(page).getByTestId('session-answer').click();
  await expect(panel(page).getByTestId('session-status')).toHaveText('待确认');
  const item = panel(page).getByTestId('session-item').first();
  await expect(item.getByTestId('item-fields')).toContainText('周五');
  await item.getByTestId('item-apply').click();
  await expect(row(page, '安排评审会')).toHaveCount(1);
  await item.getByTestId('item-undo').click();
  await expect(item).toHaveAttribute('data-status', 'undone');
  await expect(row(page, '安排评审会')).toHaveCount(0);
});

test('decision support: reasons, before/after diff, accept and undo in the page', tags('@AT-02'), async ({ page, app }) => {
  await app.cli('add', '整理照片', '-p', 'low');
  const top = (await app.cli('add', '发布准备', '-p', 'high')).task;
  await openApp(page, app.url);
  await openAI(page, 'decide');
  await page.getByTestId('decide-submit').click();
  await expect(panel(page).getByTestId('session-summary')).toContainText('建议先处理「发布准备」');
  const recs = panel(page).getByTestId('recommendation');
  await expect(recs).toHaveCount(2);
  await expect(recs.first().getByTestId('recommendation-reason')).toHaveText('理由：优先级最高，先做');
  const item = panel(page).getByTestId('session-item').first();
  await expect(item.getByTestId('diff-before')).toHaveText('高');
  await expect(item.getByTestId('diff-after')).toHaveText('紧急');

  await item.getByTestId('item-apply').click();
  await expect(row(page, '发布准备').getByTestId('task-priority')).toHaveAttribute('data-priority', 'urgent');
  expect((await app.cli('show', top.id)).task.priority).toBe('urgent');
  await item.getByTestId('item-undo').click();
  await expect(row(page, '发布准备').getByTestId('task-priority')).toHaveAttribute('data-priority', 'high');
});

test('agent write-back: text, file, command output and commit appear on the task with source and time', tags('@AT-05'), async ({ page, app }) => {
  await app.addAgent('reporter', 'all', '--desc', '写报告');
  const t = (await app.cli('add', '生成周报')).task;
  await openApp(page, app.url);
  await row(page, '生成周报').getByTestId('task-title').click();
  const detail = page.getByTestId('detail');
  await detail.getByTestId('agent-select').selectOption('reporter');
  await detail.getByTestId('agent-complete').check();
  await detail.getByTestId('agent-start').click();
  await expect(detail.getByTestId('run-item')).toHaveAttribute('data-status', 'succeeded', { timeout: 15_000 });

  const results = detail.getByTestId('task-results').getByTestId('result');
  await expect(results).toHaveCount(4);
  for (const type of ['text', 'file', 'command_output', 'commit']) {
    const r = results.and(page.locator(`[data-type="${type}"]`));
    await expect(r).toHaveCount(1);
    await expect(r.getByTestId('result-source')).toContainText('来源：智能体 reporter');
    await expect(r.getByTestId('result-source')).toContainText(/\d{4}-\d{2}-\d{2} \d{2}:\d{2}/);
  }
  const stored = (await app.cli('agent', 'results', t.id)).results;
  const commit = stored.find((r) => r.type === 'commit').data;
  const commitView = results.and(page.locator('[data-type="commit"]'));
  await commitView.locator('summary').click();
  await expect(commitView.getByTestId('result-details')).toContainText(commit.commit);
  await expect(commitView.getByTestId('result-details')).toContainText('main');
  const fileView = results.and(page.locator('[data-type="file"]'));
  await fileView.locator('summary').click();
  await expect(fileView.getByTestId('result-details')).toContainText('report.md');
  await expect(row(page, '生成周报').getByTestId('task-status')).toHaveAttribute('data-status', 'done');
});

test('agent lifecycle in the page: failure, retry with kept attempts, cancel with kept results', tags('@AT-03'), async ({ page, app }) => {
  await app.addAgent('broken', 'fail');
  await app.addAgent('flaky', 'flaky');
  await app.addAgent('slow', 'slow');
  const t = (await app.cli('add', '修复构建')).task;
  await openApp(page, app.url);
  await row(page, '修复构建').getByTestId('task-title').click();
  const detail = page.getByTestId('detail');
  const view = page.getByTestId('run-view');
  const runItem = (id) => detail.locator(`[data-testid="run-item"][data-id="${id}"]`);
  const start = async (agent) => {
    await detail.getByTestId('agent-select').selectOption(agent);
    await detail.getByTestId('agent-start').click();
    return newestRun(app, t.id, agent);
  };

  // Failure: shown with its reason, the task is not completed.
  const failed = await start('broken');
  await expect(runItem(failed.id)).toHaveAttribute('data-status', 'failed', { timeout: 10_000 });
  await runItem(failed.id).getByTestId('detail-run-open').click();
  await expect(view.getByTestId('run-error')).toContainText('missing dependency libfoo');
  await expect(view.getByTestId('run-error')).toContainText('退出码 3');
  await page.getByTestId('ai-close').click();
  await expect(row(page, '修复构建').getByTestId('task-status')).not.toHaveAttribute('data-status', 'done');

  // Retry: the second attempt succeeds, the first is kept.
  const flaky = await start('flaky');
  await expect(runItem(flaky.id)).toHaveAttribute('data-status', 'failed', { timeout: 10_000 });
  await runItem(flaky.id).getByTestId('detail-run-open').click();
  await view.getByTestId('run-retry').click();
  await expect(view.getByTestId('run-status')).toHaveText('成功', { timeout: 10_000 });
  await expect(view.getByTestId('run-attempt')).toHaveCount(2);
  await page.getByTestId('ai-close').click();

  // Cancel: the running agent stops; the intermediate result stays.
  const slow = await start('slow');
  await expect(runItem(slow.id)).toHaveAttribute('data-status', 'running', { timeout: 10_000 });
  await waitFor(async () => (await app.cli('agent', 'show', slow.id)).run.results?.length > 0, { message: 'intermediate result' });
  await runItem(slow.id).getByTestId('detail-run-open').click();
  await view.getByTestId('run-cancel').click();
  await expect(view.getByTestId('run-status')).toHaveText('已取消', { timeout: 10_000 });
  await expect(view.getByTestId('run-results')).toContainText('中间结果');
  await page.getByTestId('ai-close').click();
  await expect(detail.getByTestId('task-results')).toContainText('中间结果');
  expect((await app.cli('show', t.id)).task.status).not.toBe('done');
});

test('prompt templates: summarise in the page, keep the original, reuse with new values', tags('@AT-04'), async ({ page, app }) => {
  await openApp(page, app.url);
  await openAI(page, 'prompts');
  await page.getByTestId('summarize-box').locator('summary').click();
  await page.getByTestId('summarize-text').fill('检查 {{site}} 的所有链接\n1. 抓取页面\n2. 校验状态码\n不要访问登录后的页面\n输出失效列表，保留 <a href="x">&amp;</a>');
  await page.getByTestId('summarize-name').fill('链接检查');
  await page.getByTestId('summarize-submit').click();
  await expect(page.getByTestId('prompt-item').filter({ hasText: '链接检查' })).toHaveCount(1);
  const view = page.getByTestId('prompt-view');
  await expect(view.getByTestId('prompt-name')).toHaveText('链接检查');
  await expect(view.getByTestId('prompt-goal')).toHaveText('检查 {{site}} 的所有链接');
  await expect(view.getByTestId('prompt-variables')).toHaveText('{{site}}');
  await expect(view.getByTestId('prompt-original')).toContainText('<a href="x">&amp;</a>');

  await view.getByTestId('prompt-var').first().fill('example.com');
  await view.getByTestId('prompt-render').click();
  await expect(view.getByTestId('prompt-rendered')).toContainText('检查 example.com 的所有链接');
  await expect(view.getByTestId('prompt-rendered')).toContainText('<a href="x">&amp;</a>');
  await view.getByTestId('prompt-task-title').fill('官网链接检查');
  await view.getByTestId('prompt-create-task').click();
  await expect(row(page, '官网链接检查')).toHaveCount(1);
  const task = (await app.cli('list')).tasks.find((x) => x.title === '官网链接检查');
  expect(task.description).toContain('检查 example.com 的所有链接');
});

test('model outage: model features show a retryable error, task management keeps working', tags('@AT-12'), async ({ page, app }) => {
  app.model.setDown(true);
  await openApp(page, app.url);
  await openAI(page, 'intake');
  await page.getByTestId('intake-text').fill('写周报');
  await page.getByTestId('intake-submit').click();
  const err = panel(page).getByTestId('ai-error');
  await expect(err).toBeVisible();
  await expect(err).toContainText('模型服务出错');
  await expect(err.getByTestId('ai-retry')).toBeVisible();
  await page.getByTestId('ai-close').click();

  // Basic task management in the page and the CLI is unaffected.
  await page.getByTestId('quick-add-input').fill('手动任务 !high');
  await page.getByTestId('quick-add-input').press('Enter');
  await expect(row(page, '手动任务')).toHaveCount(1);
  await row(page, '手动任务').getByTestId('task-toggle').click();
  await expect(row(page, '手动任务').getByTestId('task-status')).toHaveAttribute('data-status', 'done');
  await app.cli('add', '命令行任务');
  await expect(row(page, '命令行任务')).toHaveCount(1, { timeout: 2000 });

  // The model is back: retry works without reloading.
  app.model.setDown(false);
  await openAI(page, 'intake');
  await err.getByTestId('ai-retry').click();
  await expect(panel(page).getByTestId('session-item')).toHaveCount(1);
});

test('no key or token ever reaches the page; agent secrets are redacted in the log view', tags('@AT-10'), async ({ page, app }) => {
  const bodies = [];
  page.on('response', async (res) => {
    try {
      bodies.push(await res.text());
    } catch {
      // streaming responses (event stream) have no final body
    }
  });
  await app.addAgent('leaky', 'secret');
  const t = (await app.cli('add', '部署服务')).task;
  await openApp(page, app.url);
  await openAI(page, 'config');
  await expect(page.getByTestId('config-key')).toContainText('已配置');
  await page.getByTestId('ai-close').click();

  await row(page, '部署服务').getByTestId('task-title').click();
  const detail = page.getByTestId('detail');
  await detail.getByTestId('agent-select').selectOption('leaky');
  await detail.getByTestId('agent-start').click();
  const run = await newestRun(app, t.id, 'leaky');
  await expect(detail.getByTestId('run-item')).toHaveAttribute('data-status', 'succeeded', { timeout: 10_000 });
  await detail.locator(`[data-testid="run-item"][data-id="${run.id}"]`).getByTestId('detail-run-open').click();
  const log = page.getByTestId('run-view');
  await expect(log.getByTestId('run-log-line').filter({ hasText: '[REDACTED]' }).first()).toBeVisible();

  const text = await page.locator('body').innerText();
  const storage = await page.evaluate(() => JSON.stringify(Object.entries(localStorage)));
  for (const secret of [MODEL_KEY, 'sk-mockagentsecret1234567890abcd', 'mockbearertoken0123456789abcdef', 'hunter2hunter2']) {
    expect(text).not.toContain(secret);
    expect(storage).not.toContain(secret);
    for (const b of bodies) expect(b).not.toContain(secret);
  }
});

test('permission revoke: after switching from auto to suggest the model no longer starts agents', tags('@AT-11'), async ({ page, app }) => {
  await app.addAgent('linkcheck', 'all', '--desc', '检查链接');
  const t = (await app.cli('add', '检查链接', '-p', 'high', '-t', 'agent')).task;
  await openApp(page, app.url);
  const runs = async () => (await app.cli('agent', 'runs', '--task', t.id)).runs.length;

  // Granted: auto mode starts the agent directly.
  await page.getByTestId('ai-mode').selectOption('auto');
  await expect(page.getByTestId('toast').last()).toContainText('自动执行');
  await openAI(page, 'decide');
  await page.getByTestId('decide-submit').click();
  const agentItem = panel(page).locator('[data-testid="session-item"][data-kind="agent"]');
  await expect(agentItem).toHaveAttribute('data-status', 'applied');
  expect(await runs()).toBe(1);

  // Revoked: suggestions only, nothing starts.
  await page.getByTestId('ai-close').click();
  await page.getByTestId('ai-mode').selectOption('suggest');
  await expect(page.getByTestId('toast').last()).toContainText('仅建议');
  expect((await app.cli('llm', 'status')).mode).toBe('suggest');
  await openAI(page, 'decide');
  await page.getByTestId('decide-submit').click();
  await expect(agentItem).toHaveAttribute('data-status', 'pending');
  expect(await runs()).toBe(1);
});

test('restart: after a service crash the reopened page shows the same tasks, runs and results', tags('@AT-08'), async ({ page, app }) => {
  await app.addAgent('reporter', 'all');
  await openApp(page, app.url);
  await page.getByTestId('quick-add-input').fill('重启前的任务 #keep !high');
  await page.getByTestId('quick-add-input').press('Enter');
  await row(page, '重启前的任务').getByTestId('task-title').click();
  const detail = page.getByTestId('detail');
  await detail.getByTestId('agent-select').selectOption('reporter');
  await detail.getByTestId('agent-start').click();
  await expect(detail.getByTestId('run-item')).toHaveAttribute('data-status', 'succeeded', { timeout: 15_000 });

  const url = await app.restart({ crash: true });
  await openApp(page, url);
  const r = row(page, '重启前的任务');
  await expect(r).toHaveCount(1);
  await expect(r.getByTestId('task-priority')).toHaveAttribute('data-priority', 'high');
  await r.getByTestId('task-title').click();
  await expect(detail.getByTestId('run-item')).toHaveAttribute('data-status', 'succeeded');
  await expect(detail.getByTestId('task-results').getByTestId('result')).toHaveCount(4);
});
