// Model and agent views in the browser (FR-702…FR-704): natural-language
// task creation, decisions with diffs, permission modes, agent runs with
// logs / controls / results, prompt template reuse, configuration, history
// and error states. Model and agent endpoints are mocked (ai-mock.mjs);
// task endpoints are the real `todo serve`, so accepted changes are checked
// with the CLI on the same SQLite database.
import { test, expect, openApp, row, titles } from './fixtures.mjs';
import { mockAI } from './ai-mock.mjs';

const panel = (page) => page.getByTestId('ai-panel');

async function openAI(page, tab) {
  await page.getByTestId('ai-open').click();
  await expect(panel(page)).toBeVisible();
  await page.getByTestId(`ai-tab-btn-${tab}`).click();
}

test('natural language creates tasks after preview; accept, edit, reject and undo', async ({ page, todo }) => {
  const mock = await mockAI(page, todo);
  await openApp(page, todo.url);
  await expect(page.getByTestId('ai-model-chip')).toHaveText('模型：gpt-test');
  await openAI(page, 'intake');

  await page.getByTestId('intake-text').fill('整理需求，再更新页面，最后检查链接');
  await page.getByTestId('intake-submit').click();
  const items = panel(page).getByTestId('session-item');
  await expect(items).toHaveCount(3);
  await expect(panel(page).getByTestId('session-status')).toHaveText('待确认');
  // Preview: nothing is created before the user accepts.
  expect(todo.cli('list').tasks).toEqual([]);
  await expect(items.nth(0).getByTestId('item-fields')).toContainText('整理需求');
  await expect(items.nth(0).getByTestId('item-fields')).toContainText('明天 18:00');
  await expect(items.nth(0).getByTestId('item-fields')).toContainText('高');
  await expect(items.nth(0).getByTestId('item-confirm-reason')).toContainText('执行前确认');

  // Edit before saving (FR-304).
  await items.nth(0).getByTestId('item-edit').click();
  await items.nth(0).getByTestId('item-edit-title').fill('整理发布需求');
  await items.nth(0).getByTestId('item-edit-save').click();
  await expect(items.nth(0).getByTestId('item-title')).toHaveText('整理发布需求');
  expect(mock.requests.find((r) => r.method === 'PATCH').body).toEqual({ title: '整理发布需求' });

  // Accept one, reject one.
  await items.nth(0).getByTestId('item-apply').click();
  await expect(items.nth(0)).toHaveAttribute('data-status', 'applied');
  await expect(row(page, '整理发布需求')).toHaveCount(1);
  await items.nth(2).getByTestId('item-reject').click();
  await expect(items.nth(2)).toHaveAttribute('data-status', 'rejected');
  await expect(panel(page).getByTestId('session-status')).toHaveText('部分执行');
  await expect(panel(page).getByTestId('session-result')).toContainText('新增 1 · 修改 0 · 未执行 2 · 失败 0');
  expect(todo.cli('list').tasks.map((t) => t.title)).toEqual(['整理发布需求']);

  // Accept the rest, then undo one created task.
  await panel(page).getByTestId('session-apply-all').click();
  await expect(items.nth(1)).toHaveAttribute('data-status', 'applied');
  await expect(titles(page)).toHaveText(['整理发布需求', '更新页面']);
  await items.nth(1).getByTestId('item-undo').click();
  await expect(items.nth(1)).toHaveAttribute('data-status', 'undone');
  await expect(row(page, '更新页面')).toHaveCount(0);
  expect(todo.cli('list').tasks.map((t) => t.title)).toEqual(['整理发布需求']);
});

test('missing information is asked once, then the answer continues the intake', async ({ page, todo }) => {
  await mockAI(page, todo);
  await openApp(page, todo.url);
  await openAI(page, 'intake');
  await page.getByTestId('intake-text').fill('准备发布会材料，缺截止时间');
  await page.getByTestId('intake-submit').click();
  await expect(panel(page).getByTestId('session-status')).toHaveText('待补充信息');
  await expect(panel(page).getByTestId('session-question')).toHaveText('这件事的截止时间是什么时候？');
  await panel(page).getByTestId('session-answer-text').fill('周五');
  await panel(page).getByTestId('session-answer').click();
  await expect(panel(page).getByTestId('session-status')).toHaveText('待确认');
  await expect(panel(page).getByTestId('session-item').first().getByTestId('item-fields')).toContainText('周五');
});

test('decision support shows reasons and before/after diffs; accept, reject, undo and re-decide', async ({ page, todo }) => {
  const mock = await mockAI(page, todo);
  todo.cli('add', '发布准备', '--priority', 'medium');
  todo.cli('add', '整理照片', '--priority', 'high');
  await openApp(page, todo.url);
  await openAI(page, 'decide');
  await page.getByTestId('decide-submit').click();

  await expect(panel(page).getByTestId('session-summary')).toContainText('建议先处理「发布准备」');
  await expect(panel(page).getByTestId('risk')).toContainText('截止时间临近');
  const recs = panel(page).getByTestId('recommendation');
  await expect(recs).toHaveCount(2);
  await expect(recs.first()).toContainText('发布准备');
  await expect(recs.first().getByTestId('recommendation-reason')).toHaveText('理由：最紧急，阻塞其他任务');

  const items = panel(page).getByTestId('session-item');
  const diff = items.nth(0).getByTestId('diff-row');
  await expect(diff).toHaveAttribute('data-field', 'priority');
  await expect(diff.getByTestId('diff-before')).toHaveText('中');
  await expect(diff.getByTestId('diff-after')).toHaveText('紧急');
  await expect(items.nth(1).getByTestId('diff-after')).toHaveText('低');

  // Accept the first suggestion only: the real task changes, the other stays.
  await items.nth(0).getByTestId('item-apply').click();
  await expect(items.nth(0)).toHaveAttribute('data-status', 'applied');
  await expect(row(page, '发布准备').getByTestId('task-priority')).toHaveAttribute('data-priority', 'urgent');
  await items.nth(1).getByTestId('item-reject').click();
  await expect(items.nth(1)).toHaveAttribute('data-status', 'rejected');
  expect(todo.cli('list').tasks.find((t) => t.title === '整理照片').priority).toBe('high');

  // Undo the accepted change.
  await items.nth(0).getByTestId('item-undo').click();
  await expect(items.nth(0)).toHaveAttribute('data-status', 'undone');
  await expect(row(page, '发布准备').getByTestId('task-priority')).toHaveAttribute('data-priority', 'medium');

  // Re-decide with feedback: a new session replaces the old one.
  await panel(page).getByTestId('session-feedback').fill('先做发布相关的');
  await panel(page).getByTestId('session-redecide').click();
  await expect(panel(page).getByTestId('session-status')).toHaveText('待确认');
  expect(mock.requests.find((r) => r.path.endsWith('/redecide')).body).toEqual({ feedback: '先做发布相关的' });

  // Ignore all suggestions.
  await panel(page).getByTestId('session-reject-all').click();
  await expect(panel(page).getByTestId('session-status')).toHaveText('已拒绝');
});

test('permission mode switch: auto mode applies directly and is shown everywhere', async ({ page, todo }) => {
  const mock = await mockAI(page, todo);
  await openApp(page, todo.url);
  const mode = page.getByTestId('ai-mode');
  await expect(mode).toHaveValue('confirm');
  await mode.selectOption('auto');
  await expect(page.getByTestId('toast').last()).toContainText('权限模式已切换为「自动执行」');
  expect(mock.requests.find((r) => r.path === '/api/llm/mode').body).toEqual({ mode: 'auto' });

  await openAI(page, 'config');
  await expect(page.getByTestId('config-mode-auto')).toBeChecked();
  await page.getByTestId('config-mode-suggest').check();
  await expect(mode).toHaveValue('suggest');
  await page.getByTestId('config-mode-auto').check();
  await expect(mode).toHaveValue('auto');

  // In auto mode the intake is applied without a confirmation step.
  await page.getByTestId('ai-tab-btn-intake').click();
  await page.getByTestId('intake-text').fill('写周报');
  await page.getByTestId('intake-submit').click();
  await expect(panel(page).getByTestId('session-item')).toHaveAttribute('data-status', 'applied');
  await expect(panel(page).getByTestId('session-item')).toContainText('自动执行');
  await expect(row(page, '写周报')).toHaveCount(1);

  // A one-off mode override is sent with the request.
  await page.getByTestId('intake-mode').selectOption('suggest');
  await page.getByTestId('intake-text').fill('买牛奶');
  await page.getByTestId('intake-submit').click();
  await expect(panel(page).getByTestId('session-item')).toHaveAttribute('data-status', 'pending');
  expect(mock.requests.filter((r) => r.path === '/api/llm/intake').pop().body.mode).toBe('suggest');
});

test('agent run from the task detail: start, logs, pause, cancel, retry and results with source and time', async ({ page, todo }) => {
  const mock = await mockAI(page, todo);
  const task = todo.cli('add', '修复登录页').task;
  await openApp(page, todo.url);
  await row(page, '修复登录页').getByTestId('task-title').click();
  const detail = page.getByTestId('detail');
  await detail.getByTestId('agent-select').selectOption('coder');
  await detail.getByTestId('agent-context').fill('只改前端');

  // The final prompt can be reviewed before starting (FR-506).
  await detail.getByTestId('agent-preview').click();
  await expect(detail.getByTestId('agent-preview-text')).toContainText('上下文：只改前端');
  expect(mock.runs.size).toBe(0);

  await detail.getByTestId('agent-start').click();
  const runRow = detail.getByTestId('run-item');
  await expect(runRow).toHaveAttribute('data-status', 'running');
  const start = mock.requests.find((r) => r.path === `/api/tasks/${task.id}/agent-runs` && !r.body.dry_run);
  expect(start.body).toMatchObject({ agent: 'coder', context: '只改前端', complete_on_success: false });
  const run = mock.lastRun();

  mock.advance(run.id, { stage: '修改代码', progress: 40 }, [{ message: 'editing login.js' }, { message: 'warning: lint', stream: 'stderr' }]);
  await detail.getByTestId('detail-run-open').click();
  const view = page.getByTestId('run-view');
  await expect(view.getByTestId('run-status')).toHaveText('运行中');
  await expect(view.getByTestId('run-stage')).toHaveText('修改代码 · 40%');
  await expect(view).not.toContainText(/null|undefined/);
  await expect(view.getByTestId('run-log-line').filter({ hasText: 'editing login.js' })).toHaveCount(1);
  await expect(view.getByTestId('run-log-line').filter({ hasText: 'warning: lint' })).toContainText('stderr');
  await expect(view.getByTestId('run-task')).toHaveText('修复登录页');
  await expect(view.getByTestId('run-selection')).toContainText('用户指定');
  await view.getByTestId('run-show-prompt').click();
  await expect(view.getByTestId('run-prompt')).toContainText('只改前端');

  // Pause / resume / cancel / retry.
  await view.getByTestId('run-pause').click();
  await expect(view.getByTestId('run-status')).toHaveText('已暂停');
  await view.getByTestId('run-resume').click();
  await expect(view.getByTestId('run-status')).toHaveText('运行中');
  await view.getByTestId('run-cancel').click();
  await expect(view.getByTestId('run-status')).toHaveText('已取消');
  await expect(view.getByTestId('run-error')).toContainText('已被用户取消');
  await view.getByTestId('run-retry-context').fill('补充依赖');
  await view.getByTestId('run-retry').click();
  await expect(view.getByTestId('run-status')).toHaveText('运行中');
  expect(mock.requests.find((r) => r.path.endsWith('/retry')).body).toEqual({ context: '补充依赖' });

  // Results written back: source agent and time on the task (FR-511).
  mock.finish(run.id, [
    { type: 'text', summary: '登录页已修复', data: { text: '修改了表单校验' } },
    { type: 'commit', summary: 'web@main 1a2b3c4d5e', data: { repo: '/src/web', branch: 'main', commit: '1a2b3c4d5e6f', files: ['login.js'] } },
  ]);
  await expect(view.getByTestId('run-status')).toHaveText('成功');
  await expect(view.getByTestId('run-attempt')).toHaveCount(2);
  await expect(view.getByTestId('run-results').getByTestId('result')).toHaveCount(2);
  await page.getByTestId('ai-close').click();
  const results = detail.getByTestId('task-results').getByTestId('result');
  await expect(results).toHaveCount(2);
  await expect(results.first().getByTestId('result-type')).toHaveText('文本');
  await expect(results.first().getByTestId('result-source')).toContainText('来源：智能体 coder');
  await expect(results.first().getByTestId('result-source')).toContainText(/\d{4}-\d{2}-\d{2} \d{2}:\d{2}/);
  await results.nth(1).locator('summary').click();
  await expect(results.nth(1).getByTestId('result-details')).toContainText('1a2b3c4d5e6f');
  await expect(results.nth(1).getByTestId('result-details')).toContainText('main');
});

test('auto agent selection and progress refresh within 2 seconds without user action', async ({ page, todo }) => {
  const mock = await mockAI(page, todo);
  todo.cli('add', '检查链接');
  await openApp(page, todo.url);
  await row(page, '检查链接').getByTestId('task-title').click();
  const detail = page.getByTestId('detail');
  await expect(detail.getByTestId('agent-select')).toHaveValue('auto');
  await detail.getByTestId('agent-start').click();
  await expect(detail.getByTestId('run-item')).toHaveAttribute('data-status', 'running');
  const run = mock.lastRun();
  expect(mock.requests.find((r) => r.path.endsWith('/agent-runs') && r.method === 'POST').body.agent).toBe('auto');

  await detail.getByTestId('detail-run-open').click();
  const view = page.getByTestId('run-view');
  await expect(view.getByTestId('run-selection')).toContainText('大模型自动选择：任务需要修改代码');

  // The page polls: changes appear within 2 s, no click needed (NFR-008).
  mock.advance(run.id, { stage: '扫描', progress: 60 }, [{ message: 'checked 120 links' }]);
  await expect(view.getByTestId('run-stage')).toHaveText('扫描 · 60%', { timeout: 2000 });
  await expect(view.getByTestId('run-log-line').filter({ hasText: 'checked 120 links' })).toHaveCount(1, { timeout: 2000 });
  mock.finish(run.id, [{ type: 'text', summary: '没有失效链接', data: { text: 'ok' } }]);
  await expect(view.getByTestId('run-status')).toHaveText('成功', { timeout: 2000 });
  await expect(page.getByTestId('runs-list').getByTestId('run-item').first()).toHaveAttribute('data-status', 'succeeded', { timeout: 2000 });
  await expect(detail.getByTestId('task-results').getByTestId('result')).toHaveCount(1, { timeout: 2000 });
});

test('agents that need confirmation wait; reject keeps the task unchanged', async ({ page, todo }) => {
  const mock = await mockAI(page, todo);
  mock.agents.push({ name: 'deployer', adapter: 'cli', command: ['deploy'], confirm: true });
  todo.cli('add', '发布');
  await openApp(page, todo.url);
  await row(page, '发布').getByTestId('task-title').click();
  const detail = page.getByTestId('detail');
  await detail.getByTestId('agent-select').selectOption('deployer');
  await detail.getByTestId('agent-start').click();
  const runRow = detail.getByTestId('run-item');
  await expect(runRow).toHaveAttribute('data-status', 'waiting_confirmation');
  await runRow.getByTestId('detail-run-reject').click();
  await expect(runRow).toHaveAttribute('data-status', 'cancelled');
  await detail.getByTestId('agent-start').click();
  await expect(runRow.first()).toHaveAttribute('data-status', 'waiting_confirmation');
  await runRow.first().getByTestId('detail-run-confirm').click();
  await expect(runRow.first()).toHaveAttribute('data-status', 'running');
});

test('prompt templates: view original and summary, reuse with new values to create a task', async ({ page, todo }) => {
  const mock = await mockAI(page, todo);
  await openApp(page, todo.url);
  await openAI(page, 'prompts');
  await page.getByTestId('prompt-item').filter({ hasText: '周报分析' }).click();
  const view = page.getByTestId('prompt-view');
  await expect(view.getByTestId('prompt-goal')).toHaveText('分析项目周报');
  await expect(view.getByTestId('prompt-variables')).toHaveText('{{project}} {{week}}');
  await expect(view.getByTestId('prompt-body')).toContainText('{{project}}');
  await expect(view.getByTestId('prompt-original')).toContainText('原始长提示词');

  // Missing variable → clear error; then render and create.
  await view.getByTestId('prompt-render').click();
  await expect(view.getByTestId('ai-error')).toContainText('缺少模板变量：project');
  const vars = view.getByTestId('prompt-var');
  await expect(vars.nth(1)).toHaveValue('第 41 周');
  await vars.nth(0).fill('官网改版');
  await view.getByTestId('prompt-render').click();
  await expect(view.getByTestId('prompt-rendered')).toHaveText(/目标：分析 官网改版 在 第 41 周 的周报/);
  await view.getByTestId('prompt-task-title').fill('官网周报分析');
  await view.getByTestId('prompt-create-task').click();
  await expect(row(page, '官网周报分析')).toHaveCount(1);
  await expect(page.getByTestId('detail').getByTestId('detail-description')).toContainText('目标：分析 官网改版');
  expect(todo.cli('list').tasks[0]).toMatchObject({ title: '官网周报分析', tags: ['agent'] });

  // The template can be chosen when starting an agent.
  const detail = page.getByTestId('detail');
  await page.getByTestId('ai-close').click();
  await detail.getByTestId('agent-template').selectOption('tpl_1');
  await detail.getByTestId('agent-template-var').first().fill('官网改版');
  await detail.getByTestId('agent-start').click();
  await expect(detail.getByTestId('run-item')).toHaveCount(1);
  const start = mock.requests.find((r) => r.method === 'POST' && r.path.endsWith('/agent-runs'));
  expect(start.body).toMatchObject({ template: 'tpl_1', vars: { project: '官网改版', week: '第 41 周' } });

  // Summarize a new prompt into a template.
  await openAI(page, 'prompts');
  await page.getByTestId('summarize-box').locator('summary').click();
  await page.getByTestId('summarize-text').fill('请检查 {{site}} 的所有链接并输出失效列表');
  await page.getByTestId('summarize-name').fill('链接检查');
  await page.getByTestId('summarize-submit').click();
  await expect(page.getByTestId('prompt-item').filter({ hasText: '链接检查' })).toHaveCount(1);
  await expect(page.getByTestId('prompt-view').getByTestId('prompt-name')).toHaveText('链接检查');
});

test('model and agent configuration: no key is shown or stored; test connection; add an agent', async ({ page, todo }) => {
  const mock = await mockAI(page, todo, { testFails: true });
  await openApp(page, todo.url);
  await openAI(page, 'config');
  const cfg = page.getByTestId('config-view');
  await expect(cfg.getByTestId('config-model-name')).toHaveText('gpt-test');
  await expect(cfg.getByTestId('config-key')).toHaveText('已配置（来源：env）');
  await cfg.getByTestId('config-test').click();
  await expect(cfg.getByTestId('config-test-result')).toContainText('连接失败：authentication failed (HTTP 401)（检查 OPENAI_API_KEY）；可切换到：local');
  await cfg.getByTestId('config-profile-item').filter({ hasText: 'local' }).getByTestId('config-use').click();
  await expect(cfg.getByTestId('config-profile')).toHaveText('local');
  await expect(page.getByTestId('ai-model-chip')).toHaveText('模型：qwen-local');

  await cfg.getByTestId('config-confirm-list').fill('git push\nnpm publish');
  await cfg.getByTestId('config-confirm-save').click();
  await expect(page.getByTestId('toast').last()).toContainText('确认清单已保存');
  expect(mock.requests.find((r) => r.path === '/api/llm/confirm-list').body).toEqual({ entries: ['git push', 'npm publish'] });

  await expect(cfg.getByTestId('config-adapters')).toContainText('cli、http、llm');
  await expect(cfg.getByTestId('config-agent')).toHaveCount(2);
  await cfg.getByTestId('config-agent-new').click();
  const ed = cfg.getByTestId('agent-editor');
  await ed.getByTestId('agent-edit-name').fill('reviewer');
  await ed.getByTestId('agent-edit-command').fill('["codex", "review"]');
  await ed.getByTestId('agent-edit-description').fill('代码评审');
  await ed.getByTestId('agent-edit-timeout_seconds').fill('600');
  await ed.getByTestId('agent-edit-save').click();
  await expect(cfg.getByTestId('config-agent')).toHaveCount(3);
  const put = mock.requests.find((r) => r.method === 'PUT' && r.path === '/api/agents/reviewer');
  expect(put.body).toMatchObject({ name: 'reviewer', adapter: 'cli', command: ['codex', 'review'], timeout_seconds: 600, description: '代码评审' });

  // No secret ever reaches the page or its storage.
  const storage = await page.evaluate(() => JSON.stringify(Object.entries(localStorage)));
  expect(storage).not.toMatch(/key|token|secret/i);
  await expect(page.locator('body')).not.toContainText(/sk-[A-Za-z0-9]/);
});

test('execution history lists runs and model sessions', async ({ page, todo }) => {
  const mock = await mockAI(page, todo);
  todo.cli('add', '写文档');
  await openApp(page, todo.url);
  await row(page, '写文档').getByTestId('task-title').click();
  await page.getByTestId('detail').getByTestId('agent-start').click();
  await expect(page.getByTestId('detail').getByTestId('run-item')).toHaveCount(1);
  mock.finish(mock.lastRun().id, [{ type: 'file', summary: '文件 README.md', data: { path: '/src/README.md', exists: true, size: 120 } }]);
  await openAI(page, 'intake');
  await page.getByTestId('intake-text').fill('买牛奶');
  await page.getByTestId('intake-submit').click();
  await expect(panel(page).getByTestId('session-item')).toHaveCount(1);

  await page.getByTestId('ai-tab-btn-history').click();
  const hist = page.getByTestId('ai-history-view');
  await expect(hist.getByTestId('history-runs').getByTestId('run-item')).toHaveAttribute('data-status', 'succeeded');
  await expect(hist.getByTestId('history-runs')).toContainText('写文档');
  await hist.getByTestId('history-session').first().click();
  await expect(hist.getByTestId('history-session-view').getByTestId('session-item')).toHaveCount(1);
  await hist.getByTestId('history-session-view').getByTestId('item-apply').click();
  await expect(row(page, '买牛奶')).toHaveCount(1);
  await hist.getByTestId('history-runs').getByTestId('run-item').click();
  await expect(page.getByTestId('run-view').getByTestId('run-results')).toContainText('文件 README.md');
});

test('model errors: unavailable module keeps task management working', async ({ page, todo }) => {
  await mockAI(page, todo, { unavailable: true });
  await openApp(page, todo.url);
  await expect(page.getByTestId('ai-model-chip')).toHaveText('大模型不可用');
  await expect(page.getByTestId('ai-mode')).toBeDisabled();
  await page.getByTestId('ai-open').click();
  await expect(page.getByTestId('ai-banner')).toContainText('大模型功能不可用');
  await expect(page.getByTestId('ai-banner')).toContainText('普通任务管理不受影响');
  await page.getByTestId('intake-text').fill('写周报');
  await page.getByTestId('intake-submit').click();
  await expect(panel(page).getByTestId('ai-error')).toContainText('大模型功能不可用');
  await page.getByTestId('ai-close').click();

  // Tasks still work.
  await page.getByTestId('quick-add-input').fill('手动任务');
  await page.getByTestId('quick-add-input').press('Enter');
  await expect(row(page, '手动任务')).toHaveCount(1);
  await row(page, '手动任务').getByTestId('task-title').click();
  await expect(page.getByTestId('detail').getByTestId('detail-agents-error')).toContainText('大模型功能不可用');
});

test('model errors: retryable failure shows the hint and a retry; agent start errors are reported', async ({ page, todo }) => {
  const mock = await mockAI(page, todo);
  mock.failNext('POST', /\/api\/llm\/intake$/, 503,
    { error: { code: 'llm_timeout', message: 'model request timed out after 60s', hint: '稍后重试或切换模型', retryable: true } });
  todo.cli('add', '任务甲');
  await openApp(page, todo.url);
  await openAI(page, 'intake');
  await page.getByTestId('intake-text').fill('写周报');
  await page.getByTestId('intake-submit').click();
  const err = panel(page).getByTestId('ai-error');
  await expect(err).toContainText('模型请求超时（稍后重试或切换模型）');
  await err.getByTestId('ai-retry').click();
  await expect(panel(page).getByTestId('session-item')).toHaveCount(1);
  await expect(panel(page).getByTestId('ai-error')).toHaveCount(0);

  // Invalid state from a control is reported, not swallowed.
  await page.getByTestId('ai-close').click();
  mock.failNext('POST', /\/agent-runs$/, 503, { error: { code: 'agents_unavailable', message: 'agent runs are not available' } });
  await row(page, '任务甲').getByTestId('task-title').click();
  await page.getByTestId('detail').getByTestId('agent-start').click();
  await expect(page.getByTestId('toast').last()).toContainText('启动失败：智能体功能不可用');
});

test('keyboard: i toggles the assistant, g focuses the agent start of the current task', async ({ page, todo }) => {
  await mockAI(page, todo);
  todo.cli('add', '键盘任务');
  await openApp(page, todo.url);
  await page.getByTestId('task-list').focus();
  await page.keyboard.press('i');
  await expect(panel(page)).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(panel(page)).toBeHidden();
  await page.getByTestId('task-list').focus();
  await page.keyboard.press('g');
  await expect(page.getByTestId('detail').getByTestId('agent-select')).toBeFocused();
});

// Without mocks: the real service (no model key configured) drives every
// view, so response shapes the mock might get wrong would surface here.
test('real service: every assistant view works without a configured model', async ({ page, todo }) => {
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const task = todo.cli('add', '真实服务任务', '-p', 'high').task;
  await openApp(page, todo.url);
  await expect(page.getByTestId('ai-model-chip')).not.toHaveText('模型…');
  await expect(page.getByTestId('ai-mode')).toHaveValue('confirm');
  for (const tab of ['intake', 'decide', 'runs', 'prompts', 'config', 'history']) {
    await openAI(page, tab);
    await expect(page.getByTestId(`ai-tab-${tab}`)).toBeVisible();
    await page.getByTestId('ai-close').click();
  }
  await openAI(page, 'config');
  await expect(page.getByTestId('config-model')).toBeVisible();
  await expect(page.getByTestId('config-adapters')).toContainText('cli');
  await page.getByTestId('config-mode-auto').check();
  await expect(page.getByTestId('ai-mode')).toHaveValue('auto');
  expect(todo.cli('llm', 'status').mode).toBe('auto');

  // Decision support degrades to local rules without a model.
  await page.getByTestId('ai-tab-btn-decide').click();
  await page.getByTestId('decide-submit').click();
  await expect(panel(page).getByTestId('session').or(panel(page).getByTestId('ai-error'))).toBeVisible();
  await page.getByTestId('ai-close').click();

  // Agent section on the task: no agents configured → a clear error, no crash.
  await row(page, '真实服务任务').getByTestId('task-title').click();
  await expect(page.getByTestId('detail').getByTestId('task-results-empty')).toBeVisible();
  await page.getByTestId('detail').getByTestId('agent-start').click();
  await expect(page.getByTestId('toast').last()).toContainText('启动失败');
  expect(todo.cli('show', task.id).task.status).toBe('todo');
  expect(errors).toEqual([]);
});

// Without mocks: an agent configured in the browser runs as a real local
// process; its log, progress and four result types (text, file, commit,
// command output) reach the task detail through the same API as the CLI.
test('real service: configure a command-line agent, run it and see its results', async ({ page, todo }) => {
  const script = new URL('../../packages/agent/testdata/mock-agent.sh', import.meta.url).pathname;
  todo.cli('add', '写周报');
  await openApp(page, todo.url);
  await openAI(page, 'config');
  const cfg = page.getByTestId('config-view');
  await cfg.getByTestId('config-agent-new').click();
  await cfg.getByTestId('agent-edit-name').fill('reporter');
  await cfg.getByTestId('agent-edit-command').fill(JSON.stringify(['/bin/sh', script, 'success']));
  await cfg.getByTestId('agent-edit-dir').fill(todo.dir);
  await cfg.getByTestId('agent-edit-save').click();
  await expect(cfg.getByTestId('config-agent')).toHaveCount(1);
  expect(todo.cli('agent', 'list').agents.map((a) => a.name)).toEqual(['reporter']);
  await page.getByTestId('ai-close').click();

  await row(page, '写周报').getByTestId('task-title').click();
  const detail = page.getByTestId('detail');
  await detail.getByTestId('agent-select').selectOption('reporter');
  await detail.getByTestId('agent-complete').check();
  await detail.getByTestId('agent-start').click();
  await expect(detail.getByTestId('run-item')).toHaveAttribute('data-status', 'succeeded', { timeout: 5000 });
  const results = detail.getByTestId('task-results').getByTestId('result');
  await expect(results).toHaveCount(4);
  await expect(detail.getByTestId('task-results')).toContainText('来源：智能体 reporter');
  for (const type of ['text', 'file', 'commit', 'command_output']) await expect(results.and(page.locator(`[data-type="${type}"]`))).toHaveCount(1);
  await expect(row(page, '写周报').getByTestId('task-status')).toHaveAttribute('data-status', 'done');
  await expect(detail.getByTestId('history-entry').filter({ hasText: '智能体 reporter' }).first()).toBeVisible();

  await detail.getByTestId('detail-run-open').click();
  const view = page.getByTestId('run-view');
  await expect(view.getByTestId('run-status')).toHaveText('成功');
  await expect(view.getByTestId('run-log-line').filter({ hasText: 'plain output line' })).toHaveCount(1);
  await expect(view.getByTestId('run-duration')).not.toHaveText('—');
  await view.getByTestId('run-show-prompt').click();
  await expect(view.getByTestId('run-prompt')).toContainText('写周报');
});
