// Shared data between the browser and the CLI: persistence across page and
// service restarts (FR-705), identical state changes and history for
// same-named operations (FR-706), live updates and conflict handling.
import { test, expect, openApp, rows, row, titles } from './fixtures.mjs';

test('saved tasks survive closing the page and restarting the service (FR-705)', async ({ browser, todo }) => {
  let context = await browser.newContext();
  let page = await context.newPage();
  await openApp(page, todo.url);
  await page.getByTestId('quick-add-input').fill('网页创建的任务 #keep !high');
  await page.getByTestId('quick-add-input').press('Enter');
  await expect(row(page, '网页创建的任务')).toHaveCount(1);
  await row(page, '网页创建的任务').getByTestId('task-toggle').click();
  await expect(row(page, '网页创建的任务').getByTestId('task-status')).toHaveAttribute('data-status', 'done');
  await context.close();

  const url = await todo.restart();
  context = await browser.newContext();
  page = await context.newPage();
  await openApp(page, url);
  const r = row(page, '网页创建的任务');
  await expect(r).toHaveCount(1);
  await expect(r.getByTestId('task-status')).toHaveAttribute('data-status', 'done');
  await expect(r.getByTestId('task-priority')).toHaveAttribute('data-priority', 'high');
  await r.getByTestId('task-title').click();
  const history = page.getByTestId('history-entry');
  await expect(history).toHaveCount(2);
  await expect(history.nth(0)).toHaveAttribute('data-action', 'complete');
  await expect(history.nth(1)).toHaveAttribute('data-action', 'create');
  await context.close();
});

test('unsaved editor input survives a page reload', async ({ page, todo }) => {
  todo.cli('add', '草稿测试');
  await openApp(page, todo.url);
  await row(page, '草稿测试').getByTestId('task-title').click();
  await page.getByTestId('detail-edit').click();
  await page.getByTestId('field-notes').fill('还没保存的备注');
  await page.reload();
  await expect(page.locator('body[data-ready="true"]')).toBeAttached();
  await expect(page.getByTestId('editor')).toBeVisible();
  await expect(page.getByTestId('field-notes')).toHaveValue('还没保存的备注');
  await expect(page.getByTestId('editor-banner')).toContainText('已恢复');
  await page.getByTestId('editor-save').click();
  await expect(page.getByTestId('editor')).toBeHidden();
  expect(todo.cli('list').tasks[0].notes).toBe('还没保存的备注');
});

// Normalized history: the actor is the only thing allowed to differ.
function historyShape(entries) {
  return entries.map((e) => ({
    action: e.action,
    changes: Object.fromEntries(Object.keys(e.changes || {})
      .filter((k) => !['position', 'updated_at', 'completed_at', 'archived_at', 'deleted_at', 'title'].includes(k))
      .sort()
      .map((k) => [k, e.changes[k]])),
    fields: Object.keys(e.changes || {}).filter((k) => k !== 'position').sort(),
  }));
}

function taskShape(t) {
  const { id, title, position, created_at, updated_at, completed_at, archived_at, deleted_at, ...rest } = t;
  return { ...rest, completed: !!completed_at, archived: !!archived_at, deleted: !!deleted_at };
}

test('web operations produce the same state and history as the CLI commands of the same name (FR-706)', async ({ page, todo }) => {
  await openApp(page, todo.url);
  const detail = page.getByTestId('detail');
  const editor = page.getByTestId('editor');

  // create: web form vs `todo add`
  await page.getByTestId('new-task').click();
  await editor.getByTestId('field-title').fill('网页任务');
  await editor.getByTestId('field-priority').selectOption('high');
  await editor.getByTestId('field-tags').fill('x');
  await editor.getByTestId('editor-save').click();
  await expect(row(page, '网页任务')).toHaveCount(1);
  const cliTask = todo.cli('add', '命令行任务', '-p', 'high', '-t', 'x').task;
  await expect(row(page, '命令行任务')).toHaveCount(1); // live update

  // edit: web form vs `todo edit`
  await row(page, '网页任务').getByTestId('task-title').click();
  await detail.getByTestId('detail-edit').click();
  await editor.getByTestId('field-title').fill('网页任务2');
  await editor.getByTestId('field-priority').selectOption('medium');
  await editor.getByTestId('field-notes').fill('补充');
  await editor.getByTestId('editor-save').click();
  await expect(detail.getByTestId('detail-title')).toHaveText('网页任务2');
  todo.cli('edit', cliTask.id, '--title', '命令行任务2', '-p', 'medium', '-n', '补充');

  // done / reopen / start / archive / reopen
  const step = async (testid, cliCmd, version) => {
    await detail.getByTestId(testid).click();
    await expect(detail.getByTestId('detail-version')).toHaveText(String(version));
    todo.cli(cliCmd, cliTask.id);
  };
  await step('detail-toggle', 'done', 3);
  await step('detail-toggle', 'reopen', 4);
  await step('detail-start', 'start', 5);
  await step('detail-archive', 'archive', 6);
  await step('detail-toggle', 'reopen', 7);

  // priority: keyboard "+" (same "priority" action as `todo priority`)
  await page.getByTestId('detail-close').click();
  await row(page, '网页任务2').getByTestId('task-title').click();
  await page.getByTestId('detail-close').click();
  await page.keyboard.press('+');
  await expect(row(page, '网页任务2').getByTestId('task-priority')).toHaveAttribute('data-priority', 'high');
  todo.cli('priority', 'high', cliTask.id);

  // move to a category: batch move vs `todo move --category`
  await row(page, '网页任务2').getByTestId('task-select').click();
  await page.getByTestId('batch-category').fill('proj');
  await page.getByTestId('batch-move').click();
  await expect(row(page, '网页任务2').getByTestId('task-category')).toHaveText('@proj');
  todo.cli('move', cliTask.id, '--category', 'proj');

  // delete / restore
  await row(page, '网页任务2').getByTestId('task-title').click();
  await detail.getByTestId('detail-delete').click();
  await expect(row(page, '网页任务2')).toHaveCount(0);
  todo.cli('delete', cliTask.id);
  await page.getByTestId('tab-deleted').click();
  await row(page, '网页任务2').getByTestId('task-title').click();
  await detail.getByTestId('detail-restore').click();
  await expect(row(page, '网页任务2')).toHaveCount(0);
  todo.cli('restore', cliTask.id);

  const webTask = todo.cli('list', '--search', '网页任务2').tasks[0];
  const webHist = todo.cli('history', webTask.id).history;
  const cliHist = todo.cli('history', cliTask.id).history;
  expect(webHist.every((e) => e.actor === 'web')).toBe(true);
  expect(cliHist.every((e) => e.actor === 'cli')).toBe(true);
  expect(webHist.map((e) => e.action)).toEqual(['create', 'update', 'complete', 'reopen', 'start', 'archive', 'reopen', 'priority', 'move', 'delete', 'restore']);
  expect(historyShape(webHist)).toEqual(historyShape(cliHist));
  const cliFinal = todo.cli('show', cliTask.id).task;
  expect(taskShape(webTask)).toEqual(taskShape(cliFinal));
});

test('CLI changes appear in the open page within 2 seconds', async ({ page, todo }) => {
  await openApp(page, todo.url);
  const created = todo.cli('add', '命令行新建', '-p', 'urgent').task;
  await expect(row(page, '命令行新建')).toHaveCount(1, { timeout: 2000 });
  await row(page, '命令行新建').getByTestId('task-title').click();
  todo.cli('edit', created.id, '--notes', '命令行补充');
  await expect(page.getByTestId('detail-notes')).toHaveText('命令行补充', { timeout: 2000 });
  await expect(page.getByTestId('history-entry').first()).toContainText('命令行');
  todo.cli('done', created.id);
  await expect(row(page, '命令行新建').getByTestId('task-status')).toHaveAttribute('data-status', 'done', { timeout: 2000 });
  todo.cli('delete', created.id);
  await expect(rows(page)).toHaveCount(0, { timeout: 2000 });
});

test('a concurrent CLI edit is reported as a conflict and never silently overwritten', async ({ page, todo }) => {
  const task = todo.cli('add', '共享任务', '-n', '原备注').task;
  await openApp(page, todo.url);
  await row(page, '共享任务').getByTestId('task-title').click();
  await page.getByTestId('detail-edit').click();
  await page.getByTestId('field-notes').fill('网页的备注');

  // Meanwhile the CLI edits the same task.
  todo.cli('edit', task.id, '--notes', '命令行的备注', '-p', 'high');
  await expect(page.getByTestId('editor-banner')).toContainText('命令行', { timeout: 2000 });
  await page.getByTestId('editor-save').click();

  const dialog = page.getByTestId('conflict-dialog');
  await expect(dialog).toBeVisible();
  const notes = dialog.locator('[data-testid="conflict-field"][data-field="notes"]');
  await expect(notes).toContainText('原备注');
  await expect(notes).toContainText('命令行的备注');
  await expect(notes).toContainText('网页的备注');
  // Nothing was overwritten yet.
  expect(todo.cli('show', task.id).task.notes).toBe('命令行的备注');

  // Postpone: the rejected edit is kept and can be applied from the detail pane.
  await page.getByTestId('conflict-later').click();
  await expect(dialog).toBeHidden();
  await expect(page.getByTestId('detail-conflict')).toBeVisible();
  await page.getByTestId('detail-conflict-open').click();
  await expect(dialog).toBeVisible();
  await page.getByTestId('conflict-mine').click();
  await expect(dialog).toBeHidden();
  await expect(page.getByTestId('detail-notes')).toHaveText('网页的备注');
  await expect(page.getByTestId('detail-conflict')).toHaveCount(0);
  const final = todo.cli('show', task.id);
  expect(final.task).toMatchObject({ notes: '网页的备注', priority: 'high' }); // the CLI's priority change is kept
  expect(final.history.map((e) => `${e.action}:${e.actor}`)).toEqual(['create:cli', 'update:cli', 'update:web']);

  // Second conflict, resolved by discarding the web edit.
  await page.getByTestId('detail-edit').click();
  await page.getByTestId('field-title').fill('网页标题');
  todo.cli('edit', task.id, '--title', '命令行标题');
  await page.getByTestId('editor-save').click();
  await expect(dialog).toBeVisible();
  await page.getByTestId('conflict-theirs').click();
  await expect(dialog).toBeHidden();
  await expect(page.getByTestId('detail-title')).toHaveText('命令行标题');
  expect(todo.cli('show', task.id).task.title).toBe('命令行标题');
});

test('errors from the service are shown and the input is kept', async ({ page, todo }) => {
  await openApp(page, todo.url);
  await page.getByTestId('new-task').click();
  await page.getByTestId('field-title').fill('子任务');
  await page.getByTestId('field-parent').fill('ffffffff-no-such-task');
  await page.getByTestId('editor-save').click();
  await expect(page.getByTestId('editor-error')).toContainText('输入无效');
  await expect(page.getByTestId('editor')).toBeVisible();
  await expect(page.getByTestId('field-title')).toHaveValue('子任务');
  await page.getByTestId('field-parent').fill('');
  await page.getByTestId('editor-save').click();
  await expect(page.getByTestId('editor')).toBeHidden();
  await expect(titles(page)).toHaveText(['子任务']);

  // Nothing to undo -> clear message.
  todo.cli('undo');
  await expect(rows(page)).toHaveCount(0, { timeout: 2000 });
  await page.getByTestId('undo').click();
  await expect(page.getByTestId('toast').last()).toContainText('撤销');
});
