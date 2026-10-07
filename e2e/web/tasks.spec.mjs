// Core task management in the browser (FR-701): create, edit, complete,
// delete/undo, search, filter, sort and batch operations. Every test
// reloads the page and checks the result against the CLI, which reads the
// same SQLite database, so "saved" means saved in the shared store.
import { test, expect, openApp, rows, row, titles } from './fixtures.mjs';

test('create, edit, complete and delete a task; the data survives a reload', async ({ page, todo }) => {
  await openApp(page, todo.url);
  await expect(page.getByTestId('empty')).toBeVisible();

  // Quick add with the TUI's inline syntax.
  await page.getByTestId('quick-add-input').fill('写周报 #work @office !high');
  await page.getByTestId('quick-add-input').press('Enter');
  const weekly = row(page, '写周报');
  await expect(weekly).toHaveCount(1);
  await expect(weekly.getByTestId('task-priority')).toHaveAttribute('data-priority', 'high');
  await expect(weekly.getByTestId('task-tag')).toHaveText('#work');
  await expect(weekly.getByTestId('task-category')).toHaveText('@office');
  await expect(page.getByTestId('quick-add-input')).toHaveValue('');

  // Full form: every field.
  await page.getByTestId('new-task').click();
  const editor = page.getByTestId('editor');
  await expect(editor).toBeVisible();
  await editor.getByTestId('field-title').fill('整理项目材料');
  await editor.getByTestId('field-description').fill('本周的项目材料');
  await editor.getByTestId('field-priority').selectOption('urgent');
  await editor.getByTestId('field-due').fill('2030-01-15T18:30');
  await editor.getByTestId('field-tags').fill('work, 文档');
  await editor.getByTestId('field-category').fill('office');
  await editor.getByTestId('field-notes').fill('附上指标');
  await editor.getByTestId('editor-save').click();
  await expect(editor).toBeHidden();
  const material = row(page, '整理项目材料');
  await expect(material).toHaveCount(1);
  await expect(material.getByTestId('task-due')).toContainText('2030年1月15日 18:30');

  // Detail pane and edit.
  await material.getByTestId('task-title').click();
  const detail = page.getByTestId('detail');
  await expect(detail.getByTestId('detail-title')).toHaveText('整理项目材料');
  await expect(detail.getByTestId('detail-notes')).toHaveText('附上指标');
  await expect(detail.getByTestId('detail-tags')).toHaveText('#work #文档');
  await detail.getByTestId('detail-edit').click();
  await editor.getByTestId('field-title').fill('整理本周项目材料');
  await editor.getByTestId('field-priority').selectOption('medium');
  await editor.getByTestId('field-notes').fill('附上指标\n补充参考文件');
  await editor.getByTestId('editor-save').click();
  await expect(editor).toBeHidden();
  await expect(detail.getByTestId('detail-title')).toHaveText('整理本周项目材料');
  await expect(detail.getByTestId('detail-version')).toHaveText('2');
  await expect(detail.getByTestId('history-entry').first()).toHaveAttribute('data-action', 'update');
  await expect(detail.getByTestId('history-entry').first()).toContainText('网页');

  // Complete from the list.
  await row(page, '写周报').getByTestId('task-toggle').click();
  await expect(row(page, '写周报').getByTestId('task-status')).toHaveAttribute('data-status', 'done');
  await expect(page.getByTestId('toast').last()).toContainText('已完成「写周报」');

  // Delete, undo, delete again.
  await page.getByTestId('quick-add-input').fill('临时任务');
  await page.getByTestId('quick-add-input').press('Enter');
  await expect(row(page, '临时任务')).toHaveCount(1);
  await row(page, '临时任务').getByTestId('task-title').click();
  await detail.getByTestId('detail-delete').click();
  await expect(row(page, '临时任务')).toHaveCount(0);
  await page.getByTestId('toast').filter({ hasText: '已删除「临时任务」' }).getByTestId('toast-undo').click();
  await expect(row(page, '临时任务')).toHaveCount(1);
  await row(page, '临时任务').getByTestId('task-title').click();
  await detail.getByTestId('detail-delete').click();
  await expect(row(page, '临时任务')).toHaveCount(0);

  // Reload: same list, and the CLI sees exactly the same data.
  await page.reload();
  await openApp(page, page.url());
  await expect(titles(page)).toHaveText(['写周报', '整理本周项目材料']);
  const list = todo.cli('list').tasks;
  expect(list.map((t) => t.title)).toEqual(['写周报', '整理本周项目材料']);
  const material2 = list.find((t) => t.title === '整理本周项目材料');
  expect(material2).toMatchObject({ priority: 'medium', category: 'office', notes: '附上指标\n补充参考文件', description: '本周的项目材料', status: 'todo' });
  expect(material2.tags).toEqual(['work', '文档']);
  expect(new Date(material2.due_at).toISOString()).toBe('2030-01-15T10:30:00.000Z'); // 18:30 Asia/Shanghai
  expect(list.find((t) => t.title === '写周报')).toMatchObject({ status: 'done', priority: 'high', category: 'office', tags: ['work'] });
  const deleted = todo.cli('list', '--deleted').tasks;
  expect(deleted.map((t) => t.title)).toEqual(['临时任务']);

  // The recycle bin tab shows it and can restore it.
  await page.getByTestId('tab-deleted').click();
  await expect(titles(page)).toHaveText(['临时任务']);
  await row(page, '临时任务').getByTestId('task-title').click();
  await detail.getByTestId('detail-restore').click();
  await expect(rows(page)).toHaveCount(0);
  await page.getByTestId('tab-all').click();
  await expect(row(page, '临时任务')).toHaveCount(1);
});

test('search, filter and sort; the view and its order survive a reload', async ({ page, todo }) => {
  todo.cli('add', '写周报', '-p', 'low', '--due', '2030-03-01', '-t', 'work');
  todo.cli('add', '买牛奶', '-p', 'urgent', '--due', '2030-01-01', '-t', 'home', '-c', 'life');
  todo.cli('add', '周报评审', '-p', 'high', '-t', 'work', '-n', '和团队一起');
  todo.cli('add', '修自行车', '-p', 'medium', '--due', '2030-02-01', '-c', 'life');
  const done = todo.cli('add', '交房租', '-p', 'high', '-c', 'life').task;
  todo.cli('done', done.id);

  await openApp(page, todo.url);
  await expect(titles(page)).toHaveText(['写周报', '买牛奶', '周报评审', '修自行车', '交房租']);

  // Keyword search (title, notes, tags...).
  await page.getByTestId('search').fill('周报');
  await expect(titles(page)).toHaveText(['写周报', '周报评审']);
  await page.getByTestId('search').fill('团队');
  await expect(titles(page)).toHaveText(['周报评审']);
  await page.getByTestId('search').fill('');
  await expect(rows(page)).toHaveCount(5);

  // Status tabs.
  await page.getByTestId('tab-done').click();
  await expect(titles(page)).toHaveText(['交房租']);
  await page.getByTestId('tab-todo').click();
  await expect(titles(page)).toHaveText(['写周报', '买牛奶', '周报评审', '修自行车']);

  // Priority, tag, category and due filters.
  await page.getByTestId('filter-priority').selectOption('high');
  await expect(titles(page)).toHaveText(['周报评审']);
  await page.getByTestId('filter-priority').selectOption('');
  await page.getByTestId('filter-tag').selectOption('work');
  await expect(titles(page)).toHaveText(['写周报', '周报评审']);
  await page.getByTestId('filter-tag').selectOption('');
  await page.getByTestId('filter-category').selectOption('life');
  await expect(titles(page)).toHaveText(['买牛奶', '修自行车']);
  await page.getByTestId('clear-filters').click();
  await page.getByTestId('filter-due').selectOption('none');
  await expect(titles(page)).toHaveText(['周报评审']);
  await page.getByTestId('filter-due').selectOption('has');
  await expect(titles(page)).toHaveText(['写周报', '买牛奶', '修自行车']);
  await page.getByTestId('filter-due').selectOption('');

  // Sorting.
  await page.getByTestId('sort').selectOption('priority');
  await expect(titles(page)).toHaveText(['买牛奶', '周报评审', '修自行车', '写周报']);
  await page.getByTestId('sort').selectOption('due');
  await expect(titles(page)).toHaveText(['买牛奶', '修自行车', '写周报', '周报评审']);
  await page.getByTestId('sort-reverse').click();
  await expect(titles(page)).toHaveText(['写周报', '修自行车', '买牛奶', '周报评审']);
  await page.getByTestId('sort-reverse').click();
  await page.getByTestId('sort').selectOption('created');
  await expect(titles(page)).toHaveText(['修自行车', '周报评审', '买牛奶', '写周报']);

  // The web order matches the CLI's for the same sort.
  await page.getByTestId('sort').selectOption('due');
  await page.getByTestId('filter-category').selectOption('life');
  const cliOrder = todo.cli('list', '--status', 'todo', '--category', 'life', '--sort', 'due').tasks.map((t) => t.title);
  await expect(titles(page)).toHaveText(cliOrder);

  // Reload keeps the view (status tab, filter, sort) and the data.
  await page.reload();
  await openApp(page, page.url());
  await expect(page.getByTestId('tab-todo')).toHaveAttribute('aria-selected', 'true');
  await expect(page.getByTestId('sort')).toHaveValue('due');
  await expect(page.getByTestId('filter-category')).toHaveValue('life');
  await expect(titles(page)).toHaveText(cliOrder);

  // Manual order: move a task up with the keyboard (K), as `todo move --before`.
  await page.getByTestId('clear-filters').click();
  await page.getByTestId('tab-all').click();
  await page.getByTestId('sort').selectOption('manual');
  await expect(titles(page)).toHaveText(['写周报', '买牛奶', '周报评审', '修自行车', '交房租']);
  await row(page, '周报评审').getByTestId('task-title').click();
  await page.getByTestId('detail-close').click();
  await page.keyboard.press('Shift+K');
  await expect(titles(page)).toHaveText(['写周报', '周报评审', '买牛奶', '修自行车', '交房租']);
  expect(todo.cli('list').tasks.map((t) => t.title)).toEqual(['写周报', '周报评审', '买牛奶', '修自行车', '交房租']);
});

test('batch complete, priority, move, archive and delete with undo', async ({ page, todo }) => {
  for (const t of ['任务一', '任务二', '任务三', '任务四']) todo.cli('add', t);
  await openApp(page, todo.url);
  await expect(rows(page)).toHaveCount(4);
  const batchBar = page.getByTestId('batch-bar');
  await expect(batchBar).toBeHidden();

  // Select two tasks -> batch complete.
  await row(page, '任务一').getByTestId('task-select').click();
  await row(page, '任务二').getByTestId('task-select').click();
  await expect(page.getByTestId('batch-count')).toHaveText('已选 2 项');
  await page.getByTestId('batch-complete').click();
  await expect(page.getByTestId('toast').last()).toContainText('已完成 2 个任务');
  await expect(batchBar).toBeHidden();
  await expect(row(page, '任务一').getByTestId('task-status')).toHaveAttribute('data-status', 'done');
  await expect(row(page, '任务二').getByTestId('task-status')).toHaveAttribute('data-status', 'done');

  // Select all -> batch priority and batch move to a category.
  await page.getByTestId('select-all').check();
  await expect(page.getByTestId('batch-count')).toHaveText('已选 4 项');
  await page.getByTestId('batch-priority').selectOption('urgent');
  await expect(page.getByTestId('toast').last()).toContainText('已修改优先级 4 个任务');
  await expect(rows(page).getByTestId('task-priority')).toHaveCount(4);
  // Shift-click selects a range.
  await row(page, '任务二').getByTestId('task-select').click();
  await row(page, '任务四').getByTestId('task-select').click({ modifiers: ['Shift'] });
  await expect(page.getByTestId('batch-count')).toHaveText('已选 3 项');
  await page.getByTestId('batch-category').fill('project');
  await page.getByTestId('batch-move').click();
  await expect(page.getByTestId('toast').last()).toContainText('已移动 3 个任务');
  await expect(rows(page).getByTestId('task-category')).toHaveCount(3);

  // Batch archive: archived tasks leave the default list.
  await row(page, '任务一').getByTestId('task-select').click();
  await row(page, '任务二').getByTestId('task-select').click();
  await page.getByTestId('batch-archive').click();
  await expect(titles(page)).toHaveText(['任务三', '任务四']);

  // Batch delete, then undo it from the toast: one undo step for the batch.
  await page.getByTestId('select-all').check();
  await page.getByTestId('batch-delete').click();
  await expect(rows(page)).toHaveCount(0);
  await page.getByTestId('toast').filter({ hasText: '已删除 2 个任务' }).getByTestId('toast-undo').click();
  await expect(titles(page)).toHaveText(['任务三', '任务四']);

  // Reload and cross-check with the CLI.
  await page.reload();
  await openApp(page, page.url());
  await expect(titles(page)).toHaveText(['任务三', '任务四']);
  await page.getByTestId('tab-archived').click();
  await expect(titles(page)).toHaveText(['任务一', '任务二']);
  const all = Object.fromEntries(todo.cli('list', '--all').tasks.map((t) => [t.title, t]));
  expect(all['任务一']).toMatchObject({ status: 'archived', priority: 'urgent', category: '' });
  expect(all['任务二']).toMatchObject({ status: 'archived', priority: 'urgent', category: 'project' });
  expect(all['任务三']).toMatchObject({ status: 'todo', priority: 'urgent', category: 'project' });
  expect(all['任务四']).toMatchObject({ status: 'todo', priority: 'urgent', category: 'project' });
  expect(todo.cli('list', '--deleted').tasks).toEqual([]);
});

test('keyboard: navigate, complete, edit, delete and undo without the mouse', async ({ page, todo }) => {
  todo.cli('add', '甲');
  todo.cli('add', '乙');
  await openApp(page, todo.url);
  await page.keyboard.press('j');
  await page.keyboard.press('x');
  await expect(row(page, '乙').getByTestId('task-status')).toHaveAttribute('data-status', 'done');
  await page.keyboard.press('k');
  await page.keyboard.press('e');
  await expect(page.getByTestId('editor')).toBeVisible();
  await page.getByTestId('field-title').fill('甲（已改）');
  await page.keyboard.press('Control+Enter');
  await expect(page.getByTestId('editor')).toBeHidden();
  await expect(row(page, '甲（已改）')).toHaveCount(1);
  await page.keyboard.press('d');
  await expect(rows(page)).toHaveCount(1);
  await page.keyboard.press('u');
  await expect(rows(page)).toHaveCount(2);
  await page.keyboard.press('n');
  await expect(page.getByTestId('quick-add-input')).toBeFocused();
  await page.keyboard.type('丙 !urgent');
  await page.keyboard.press('Enter');
  await expect(row(page, '丙').getByTestId('task-priority')).toHaveAttribute('data-priority', 'urgent');
  await page.keyboard.press('Escape');
  await page.keyboard.press('/');
  await expect(page.getByTestId('search')).toBeFocused();
  await page.keyboard.press('Escape');
  await page.keyboard.press('?');
  await expect(page.getByTestId('help-dialog')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.getByTestId('help-dialog')).toBeHidden();

  // Enter on a focused button activates the button, not a list shortcut.
  await page.getByTestId('new-task').focus();
  await page.keyboard.press('Enter');
  await expect(page.getByTestId('editor')).toBeVisible();
  await expect(page.locator('[data-testid="field-status"] option[value="archived"]')).toHaveJSProperty('disabled', true);
  await page.keyboard.press('Escape');
  await expect(page.getByTestId('editor')).toBeHidden();
  // An existing task can be archived from the form.
  await row(page, '丙').getByTestId('task-title').dblclick();
  await expect(page.getByTestId('editor')).toBeVisible();
  await page.getByTestId('field-status').selectOption('archived');
  await page.getByTestId('editor-save').click();
  await expect(row(page, '丙')).toHaveCount(0);
  expect(todo.cli('list', '--status', 'archived').tasks.map((t) => t.title)).toEqual(['丙']);
});
