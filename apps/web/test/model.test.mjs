// Unit tests for apps/web/js/model.js (run: node --test apps/web/test/).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  parseView, serializeView, viewToQuery, defaultView, isFiltered, parseQuickAdd, parseDueWord, parseTags,
  formPatch, formatDue, isOverdue, toLocalInput, fromLocalInput, describeChanges, errorMessage, actionLabel,
  normalizePriority,
} from '../js/model.js';

// Tuesday 2026-10-06 10:00 local time.
const now = new Date(2026, 9, 6, 10, 0, 0);
const local = (iso) => {
  const d = new Date(iso);
  return [d.getFullYear(), d.getMonth() + 1, d.getDate(), d.getHours(), d.getMinutes()];
};

test('view round-trips through the URL and drops defaults', () => {
  assert.equal(serializeView(defaultView()), '');
  const v = { ...defaultView(), status: 'todo', q: '周报 草稿', tag: 'work', sort: 'due', reverse: true };
  const s = serializeView(v, { task: 'abc' });
  assert.deepEqual(parseView(s), v);
  assert.equal(new URLSearchParams(s).get('task'), 'abc');
  // Unknown values fall back to defaults instead of breaking the page.
  assert.deepEqual(parseView('?status=bogus&sort=x&priority=zzz&due=y'), defaultView());
  assert.equal(isFiltered(v), true);
  assert.equal(isFiltered({ ...defaultView(), status: 'done', sort: 'due' }), false);
});

test('view maps to the REST list query', () => {
  const q = (v) => Object.fromEntries(viewToQuery({ ...defaultView(), ...v }, now));
  assert.deepEqual(q({}), { sort: 'manual' });
  assert.deepEqual(q({ status: 'done', q: ' 周报 ', priority: 'high', tag: 'work', category: 'office', sort: 'priority', reverse: true }),
    { status: 'done', q: '周报', priority: 'high', tag: 'work', category: 'office', sort: 'priority', reverse: 'true' });
  assert.deepEqual(q({ status: 'deleted' }), { deleted: 'only', status: 'all', sort: 'manual' });
  assert.deepEqual(q({ due: 'overdue' }), { overdue: 'true', sort: 'manual' });
  assert.deepEqual(q({ due: 'none' }), { has_due: 'false', sort: 'manual' });
  assert.deepEqual(q({ due: 'has' }), { has_due: 'true', sort: 'manual' });
  assert.deepEqual(local(q({ due: 'today' }).due_after), [2026, 10, 6, 0, 0]);
  assert.deepEqual(local(q({ due: 'today' }).due_before), [2026, 10, 6, 23, 59]);
  assert.deepEqual(local(q({ due: 'week' }).due_after), [2026, 10, 6, 0, 0]);
  assert.deepEqual(local(q({ due: 'week' }).due_before), [2026, 10, 13, 23, 59]);
});

test('due shortcuts match the CLI', () => {
  const at = (s) => local(parseDueWord(s, now));
  assert.deepEqual(at('today'), [2026, 10, 6, 23, 59]);
  assert.deepEqual(at('明天'), [2026, 10, 7, 23, 59]);
  assert.deepEqual(at('后天'), [2026, 10, 8, 23, 59]);
  assert.deepEqual(at('+3d'), [2026, 10, 9, 23, 59]);
  assert.deepEqual(at('+1w'), [2026, 10, 13, 23, 59]);
  assert.deepEqual(at('+2h'), [2026, 10, 6, 12, 0]);
  assert.deepEqual(at('fri'), [2026, 10, 9, 23, 59]);
  assert.deepEqual(at('周五'), [2026, 10, 9, 23, 59]);
  assert.deepEqual(at('tue'), [2026, 10, 13, 23, 59]); // today is Tuesday: next week's
  assert.deepEqual(at('下周一'), [2026, 10, 12, 23, 59]);
  assert.deepEqual(at('next fri'), [2026, 10, 16, 23, 59]);
  assert.deepEqual(at('2026-10-09'), [2026, 10, 9, 23, 59]);
  assert.deepEqual(at('2026/10/9'), [2026, 10, 9, 23, 59]);
  assert.deepEqual(at('2026-10-09 18:30'), [2026, 10, 9, 18, 30]);
  assert.equal(parseDueWord('2026-02-30', now), null);
  assert.equal(parseDueWord('someday', now), null);
  assert.equal(parseDueWord('', now), null);
});

test('quick add syntax', () => {
  const r = parseQuickAdd('写周报 #work @office !high due:明天 #weekly', now);
  assert.equal(r.title, '写周报');
  assert.deepEqual(r.tags, ['weekly', 'work']);
  assert.equal(r.category, 'office');
  assert.equal(r.priority, 'high');
  assert.deepEqual(local(r.due_at), [2026, 10, 7, 23, 59]);
  // Unknown markers stay in the title rather than being dropped.
  const keep = parseQuickAdd('Fix !bang due:someday # @', now);
  assert.equal(keep.title, 'Fix !bang due:someday # @');
  assert.equal(keep.priority, '');
  assert.equal(keep.due_at, '');
  assert.equal(parseQuickAdd('   ').title, '');
  assert.equal(normalizePriority('紧急'), 'urgent');
});

test('tags are normalized like the server does', () => {
  assert.deepEqual(parseTags('b, #a，c  a'), ['a', 'b', 'c']);
  assert.deepEqual(parseTags(''), []);
});

test('formPatch only sends changed fields', () => {
  const task = { title: 'T', description: '', notes: 'n', category: '', parent_id: '', priority: 'high', status: 'todo',
    tags: ['b', 'a'], due_at: '2030-01-01T10:00:00Z' };
  const same = { title: 'T', description: '', notes: 'n', category: '', parent_id: '', priority: 'high', status: 'todo',
    tags: ['a', 'b'], due_at: '2030-01-01T10:00:00.000Z' };
  assert.deepEqual(formPatch(task, same), {});
  assert.deepEqual(formPatch(task, { ...same, title: 'T2', priority: 'low', tags: ['a'], due_at: '', status: 'done', notes: '' }),
    { title: 'T2', priority: 'low', tags: ['a'], due_at: null, status: 'done', notes: '' });
  assert.deepEqual(formPatch({ ...task, due_at: null }, { ...same, due_at: '2030-02-01T00:00:00.000Z' }), { due_at: '2030-02-01T00:00:00.000Z' });
});

test('date display and datetime-local conversion', () => {
  assert.equal(formatDue(new Date(2026, 9, 6, 18, 0).toISOString(), now), '今天 18:00');
  assert.equal(formatDue(new Date(2026, 9, 7, 23, 59, 59).toISOString(), now), '明天');
  assert.equal(formatDue(new Date(2026, 9, 5, 9, 5).toISOString(), now), '昨天 09:05');
  assert.equal(formatDue(new Date(2026, 11, 24, 23, 59, 59).toISOString(), now), '12月24日');
  assert.equal(formatDue(new Date(2027, 0, 2, 8, 0).toISOString(), now), '2027年1月2日 08:00');
  assert.equal(formatDue(null, now), '');
  const iso = new Date(2030, 0, 15, 18, 30).toISOString();
  assert.equal(toLocalInput(iso), '2030-01-15T18:30');
  assert.equal(fromLocalInput('2030-01-15T18:30'), iso);
  assert.equal(fromLocalInput(''), '');
  assert.equal(isOverdue({ status: 'todo', due_at: new Date(2026, 9, 5).toISOString() }, now), true);
  assert.equal(isOverdue({ status: 'done', due_at: new Date(2026, 9, 5).toISOString() }, now), false);
  assert.equal(isOverdue({ status: 'todo', due_at: null }, now), false);
});

test('history and error text', () => {
  const rows = describeChanges({ status: { from: 'todo', to: 'done' }, position: { from: 1, to: 2 }, tags: { from: null, to: ['x'] } });
  assert.deepEqual(rows.map((r) => [r.label, r.from, r.to]), [['状态', '待办', '已完成'], ['标签', '（空）', '#x']]);
  assert.deepEqual(describeChanges({ position: { from: 1, to: 2 } }).map((r) => r.label), ['顺序']);
  assert.equal(actionLabel('undo_delete'), '撤销删除');
  assert.equal(actionLabel('priority'), '修改优先级');
  assert.equal(errorMessage({ code: 'version_conflict' }), '任务已在其他地方被修改');
  assert.equal(errorMessage({ code: 'invalid_input', message: 'invalid_input: title is required' }), '输入无效：title is required');
  assert.equal(errorMessage({ code: 'weird', message: 'boom' }), 'boom');
});
