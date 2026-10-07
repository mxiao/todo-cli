// Filters that depend on the clock, and the 10,000-task baseline
// (NFR-013: first interaction within 3 s, queries within 2 s).
import { writeFileSync } from 'node:fs';
import path from 'node:path';
import { randomUUID } from 'node:crypto';
import { test, expect, openApp, rows, titles } from './fixtures.mjs';

test('overdue and no-due filters', async ({ page, todo }) => {
  todo.cli('add', '早就过期', '--due', '2020-01-01');
  todo.cli('add', '将来', '--due', '2099-01-01');
  todo.cli('add', '没有期限');
  await openApp(page, todo.url);
  await expect(rows(page).filter({ hasText: '已逾期' })).toHaveCount(1);
  await expect(page.getByTestId('list-summary')).toContainText('1 项已逾期');
  await page.getByTestId('filter-due').selectOption('overdue');
  await expect(titles(page)).toHaveText(['早就过期']);
  await page.getByTestId('filter-due').selectOption('none');
  await expect(titles(page)).toHaveText(['没有期限']);
});

test('10,000 tasks: the page is usable within 3 seconds and queries stay fast', async ({ page, todo }) => {
  test.slow();
  const now = new Date().toISOString();
  const prios = ['none', 'low', 'medium', 'high', 'urgent'];
  const tasks = Array.from({ length: 10000 }, (_, i) => ({
    id: randomUUID(), title: `任务 ${String(i).padStart(5, '0')}${i === 7777 ? ' 关键词' : ''}`, description: '', notes: '',
    due_at: null, priority: prios[i % 5], tags: [i % 2 ? 'odd' : 'even'], category: `c${i % 10}`, parent_id: '',
    status: i % 4 === 0 ? 'done' : 'todo', position: i + 1, created_at: now, updated_at: now,
    completed_at: i % 4 === 0 ? now : null, archived_at: null, deleted_at: null, version: 1,
  }));
  const file = path.join(todo.dir, 'seed.json');
  writeFileSync(file, JSON.stringify({ format: 'todo-cli/export', format_version: 1, schema_version: 3, exported_at: now, tasks, history: [] }));
  todo.cli('import', file);

  const began = Date.now();
  await openApp(page, todo.url);
  await expect(page.getByTestId('list-summary')).toContainText('共 10000 项');
  await expect(rows(page)).toHaveCount(200); // rendered in pages
  expect(Date.now() - began).toBeLessThan(3000);

  let t = Date.now();
  await page.getByTestId('search').fill('关键词');
  await expect(titles(page)).toHaveText(['任务 07777 关键词']);
  expect(Date.now() - t).toBeLessThan(2000);

  await page.getByTestId('search').fill('');
  t = Date.now();
  await page.getByTestId('filter-priority').selectOption('urgent');
  await page.getByTestId('filter-tag').selectOption('odd');
  await expect(page.getByTestId('list-summary')).toContainText('共 1000 项');
  await page.getByTestId('sort').selectOption('created');
  await expect(page.getByTestId('list-summary')).toContainText('共 1000 项');
  expect(Date.now() - t).toBeLessThan(4000);

  await page.getByTestId('show-more').click();
  await expect(rows(page)).toHaveCount(400);
  t = Date.now();
  await rows(page).first().getByTestId('task-toggle').click();
  await expect(page.getByTestId('toast').last()).toContainText('已完成');
  expect(Date.now() - t).toBeLessThan(2000);
});
