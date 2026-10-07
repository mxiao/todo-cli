// todo-cli web task manager. All data lives in the local service's SQLite
// store (the same one the CLI and TUI use); this page only keeps view state
// (in the URL) and unsaved form drafts (in localStorage).

import { api, ApiError } from './api.js';
import { createStore } from './state.js';
import { connectLive } from './live.js';
import { h, $, $$, clear, options } from './dom.js';
import {
  STATUSES, STATUS_LABELS, FIELD_LABELS, PRIORITIES, PRIORITY_LABELS, STATUS_TABS, SORTS, DUE_FILTERS,
  actionLabel, actorLabel, parseView, serializeView, viewToQuery, isFiltered, defaultView,
  formatDue, formatDateTime, isOverdue, isDueToday, toLocalInput, fromLocalInput,
  parseTags, parseQuickAdd, formPatch, shortId, describeChanges, fieldValue, errorMessage,
} from './model.js';

const PAGE = 200;
const DRAFT_KEY = 'todo-web:draft';
const QUICK_KEY = 'todo-web:quick';
const PRIORITY_ORDER = ['none', 'low', 'medium', 'high', 'urgent'];

const initialParams = new URLSearchParams(location.search);
const store = createStore({
  view: parseView(location.search),
  tasks: [],
  count: 0,
  revision: 0,
  loaded: false,
  listError: null,
  facets: { tags: [], categories: [] },
  cursor: null,
  openId: initialParams.get('task') || null,
  detail: null,
  selected: new Set(),
  limit: PAGE,
  live: 'connecting',
});

// Editor state lives outside the store: the form itself holds the values.
let editor = null; // { mode: 'new'|'edit', task, version, remoteVersion }
let conflict = null; // the conflict view shown in the dialog
let lastClickedId = null;

// ---------------------------------------------------------------- loading

let listSeq = 0;
async function loadTasks() {
  const seq = ++listSeq;
  const { view } = store.get();
  try {
    const res = await api.listTasks(viewToQuery(view));
    if (seq !== listSeq) return;
    const prev = store.get();
    const ids = new Set(res.tasks.map((t) => t.id));
    const selected = new Set([...prev.selected].filter((id) => ids.has(id)));
    let cursor = prev.cursor;
    if (cursor && !ids.has(cursor)) {
      const oldIndex = prev.tasks.findIndex((t) => t.id === cursor);
      const next = res.tasks[Math.min(Math.max(oldIndex, 0), res.tasks.length - 1)];
      cursor = next ? next.id : null;
    }
    store.set({ tasks: res.tasks, count: res.count, revision: res.revision, loaded: true, listError: null, selected, cursor });
  } catch (err) {
    if (seq !== listSeq) return;
    store.set({ loaded: true, listError: errorMessage(err) });
  }
}

async function loadFacets() {
  try {
    store.set({ facets: await api.facets() });
  } catch {
    // Filters keep their previous choices; the list shows the error.
  }
}

let detailSeq = 0;
async function loadDetail(id) {
  const seq = ++detailSeq;
  if (!id) {
    store.set({ detail: null });
    return;
  }
  try {
    const res = await api.getTask(id);
    if (seq !== detailSeq || store.get().openId !== id) return;
    store.set({ detail: res });
  } catch (err) {
    if (seq !== detailSeq) return;
    if (err instanceof ApiError && err.status === 404) {
      store.set({ openId: null, detail: null });
      toast('该任务已不存在', { kind: 'error' });
    } else {
      toast(errorMessage(err), { kind: 'error' });
    }
  }
}

function refresh() {
  const { openId } = store.get();
  return Promise.all([loadTasks(), loadFacets(), openId ? loadDetail(openId) : null]);
}

let refreshTimer = 0;
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(refresh, 60);
}

function setView(patch) {
  const view = { ...store.get().view, ...patch };
  store.set({ view, limit: PAGE });
  loadTasks();
}

function openTask(id) {
  store.set({ openId: id, cursor: id || store.get().cursor, detail: id === store.get().openId ? store.get().detail : null });
  loadDetail(id);
}

// ---------------------------------------------------------------- writes

function taskById(id) {
  const s = store.get();
  return s.tasks.find((t) => t.id === id) || (s.detail && s.detail.task.id === id ? s.detail.task : null);
}

function titleOf(task) {
  const t = task.title || '';
  return t.length > 30 ? t.slice(0, 30) + '…' : t;
}

// run performs a write and refreshes; version conflicts open the conflict
// dialog when the server kept the rejected edit, otherwise they reload.
async function run(fn, { success, undo = true } = {}) {
  try {
    const res = await fn();
    if (success) toast(typeof success === 'function' ? success(res) : success, { kind: 'success', undo });
    await refresh();
    return res;
  } catch (err) {
    handleWriteError(err);
    return null;
  }
}

function handleWriteError(err) {
  if (err instanceof ApiError && err.isConflict) {
    if (err.body && err.body.conflict) {
      showConflict(err.body.conflict);
    } else {
      toast('任务已在其他地方被修改，已刷新为最新内容，请检查后重试。', { kind: 'error' });
    }
    refresh();
    return;
  }
  if (err instanceof ApiError && (err.status === 404 || err.status === 410)) {
    toast(errorMessage(err) + '，已刷新列表。', { kind: 'error' });
    refresh();
    return;
  }
  toast(errorMessage(err), { kind: 'error' });
}

async function quickAdd(text) {
  const parsed = parseQuickAdd(text);
  if (!parsed.title) {
    toast('请输入任务标题', { kind: 'error' });
    return false;
  }
  const body = { title: parsed.title };
  if (parsed.tags.length) body.tags = parsed.tags;
  if (parsed.category) body.category = parsed.category;
  if (parsed.priority) body.priority = parsed.priority;
  if (parsed.due_at) body.due_at = parsed.due_at;
  const res = await run(() => api.createTask(body), { success: (r) => `已新增「${titleOf(r.task)}」` });
  if (res) store.set({ cursor: res.task.id });
  return !!res;
}

function setStatus(task, status) {
  const verb = { done: '已完成', todo: '已重新打开', in_progress: '已开始', archived: '已归档' }[status];
  return run(() => api.setStatus(task.id, status, task.version), { success: `${verb}「${titleOf(task)}」` });
}

function toggleDone(task) {
  return setStatus(task, task.status === 'done' || task.status === 'archived' ? 'todo' : 'done');
}

function deleteTask(task) {
  return run(() => api.deleteTask(task.id, task.version), { success: `已删除「${titleOf(task)}」` });
}

function restoreTask(task) {
  return run(() => api.restoreTask(task.id, task.version), { success: `已恢复「${titleOf(task)}」` });
}

// bumpPriority uses the same "priority" action as `todo priority`.
function bumpPriority(task, delta) {
  const i = PRIORITY_ORDER.indexOf(task.priority) + delta;
  if (i < 0 || i >= PRIORITY_ORDER.length) return null;
  const priority = PRIORITY_ORDER[i];
  return run(() => api.batch('priority', [{ id: task.id, version: task.version }], { priority }),
    { success: `「${titleOf(task)}」优先级改为${PRIORITY_LABELS[priority]}` });
}

// moveInOrder moves a task one place up (-1) or down (+1) in the manual
// order, relative to its neighbour in the visible list.
function moveInOrder(task, dir) {
  const { tasks, view } = store.get();
  if (view.sort !== 'manual') {
    toast('切换到“手动顺序”排序后才能调整顺序', { kind: 'error' });
    return null;
  }
  const i = tasks.findIndex((t) => t.id === task.id);
  const j = i + dir;
  if (i < 0 || j < 0 || j >= tasks.length) return null;
  const up = view.reverse ? dir > 0 : dir < 0;
  const placement = up ? { before: tasks[j].id } : { after: tasks[j].id };
  return run(() => api.moveTask(task.id, { ...placement, version: task.version }), { success: '已调整顺序' });
}

const BATCH_VERBS = {
  complete: '完成', start: '开始', reopen: '重新打开', archive: '归档', delete: '删除', restore: '恢复',
  priority: '修改优先级', move: '移动',
};

async function runBatch(action, extra = {}) {
  const { selected, tasks } = store.get();
  const items = tasks.filter((t) => selected.has(t.id)).map((t) => ({ id: t.id, version: t.version }));
  if (!items.length) return;
  try {
    const res = await api.batch(action, items, extra);
    store.set({ selected: new Set() });
    toast(`已${BATCH_VERBS[action]} ${res.changed} 个任务` + (res.changed < items.length ? `（${items.length - res.changed} 个无需变更）` : ''),
      { kind: 'success', undo: res.changed > 0 });
    await refresh();
  } catch (err) {
    if (err instanceof ApiError && err.isConflict) {
      toast('部分任务已在其他地方被修改，批量操作未执行。列表已刷新，请检查后重试。', { kind: 'error' });
      refresh();
      return;
    }
    handleWriteError(err);
  }
}

async function undo() {
  try {
    const res = await api.undo();
    const op = res.undo.operation;
    const n = res.undo.restored.length + res.undo.removed.length;
    toast(`已撤销：${actionLabel(op.kind)}（${actorLabel(op.actor)}，${n} 个任务）`, { kind: 'success' });
    await refresh();
  } catch (err) {
    toast(errorMessage(err), { kind: 'error' });
  }
}

// ---------------------------------------------------------------- toasts

function toast(message, { kind = 'info', undo: withUndo = false, timeout } = {}) {
  const box = $('#toasts');
  const el = h('div', { class: `toast ${kind}`, 'data-testid': 'toast', role: kind === 'error' ? 'alert' : 'status' },
    h('span', { class: 'toast-msg' }, message));
  const close = () => el.remove();
  if (withUndo) {
    el.append(h('button', { type: 'button', class: 'link', 'data-testid': 'toast-undo', onclick: () => { close(); undo(); } }, '撤销'));
  }
  el.append(h('button', { type: 'button', class: 'link close', 'aria-label': '关闭', onclick: close }, '×'));
  box.append(el);
  while (box.children.length > 4) box.firstChild.remove();
  setTimeout(close, timeout || (kind === 'error' ? 8000 : withUndo ? 7000 : 4000));
}

// ---------------------------------------------------------------- rendering

function buildTabs() {
  const tabs = $('#status-tabs');
  for (const t of STATUS_TABS) {
    tabs.append(h('button', {
      type: 'button', role: 'tab', class: 'tab', 'aria-selected': 'false', 'data-status': t.value,
      'data-testid': `tab-${t.value || 'all'}`, onclick: () => setView({ status: t.value }),
    }, t.label));
  }
}

function renderToolbar() {
  const { view, facets } = store.get();
  for (const tab of $$('#status-tabs .tab')) tab.setAttribute('aria-selected', String(view.status === tab.dataset.status));
  const search = $('#search');
  // Never overwrite what the user is typing (facet refreshes re-render this).
  if (document.activeElement !== search) search.value = view.q;
  options($('#filter-priority'), [{ value: '', label: '全部优先级' }, ...PRIORITIES.map((p) => ({ value: p, label: `优先级：${PRIORITY_LABELS[p]}` }))], view.priority);
  const facetOptions = (list, current, all, prefix) => {
    const items = [{ value: '', label: all }, ...list.map((f) => ({ value: f.name, label: `${prefix}${f.name}（${f.count}）` }))];
    if (current && !list.some((f) => f.name === current)) items.push({ value: current, label: prefix + current });
    return items;
  };
  options($('#filter-tag'), facetOptions(facets.tags, view.tag, '全部标签', '#'), view.tag);
  options($('#filter-category'), facetOptions(facets.categories, view.category, '全部分类', '@'), view.category);
  options($('#filter-due'), DUE_FILTERS, view.due);
  options($('#sort'), SORTS, view.sort);
  $('#reverse').setAttribute('aria-pressed', String(view.reverse));
  $('#clear-filters').disabled = !isFiltered(view);

  const fill = (id, list) => {
    const dl = $(id);
    const sig = list.map((f) => f.name).join('\u0000');
    if (dl.dataset.sig === sig) return;
    dl.dataset.sig = sig;
    clear(dl);
    for (const f of list) dl.append(h('option', { value: f.name }));
  };
  fill('#category-options', facets.categories);
  fill('#tag-options', facets.tags);
}

function priorityBadge(p) {
  if (!p || p === 'none') return null;
  return h('span', { class: `badge prio-${p}`, 'data-testid': 'task-priority', 'data-priority': p }, PRIORITY_LABELS[p]);
}

function statusBadge(task) {
  if (task.deleted_at) return h('span', { class: 'badge status-deleted', 'data-testid': 'task-status', 'data-status': 'deleted' }, '已删除');
  return h('span', { class: `badge status-${task.status}`, 'data-testid': 'task-status', 'data-status': task.status }, STATUS_LABELS[task.status]);
}

function taskRow(task, s) {
  const now = new Date();
  const selected = s.selected.has(task.id);
  const done = task.status === 'done' || task.status === 'archived';
  const overdue = isOverdue(task, now);
  return h('li', {
    class: ['task-row', done && 'is-done', task.id === s.cursor && 'is-cursor', task.id === s.openId && 'is-open', selected && 'is-selected'].filter(Boolean).join(' '),
    role: 'option', 'aria-selected': String(selected), 'data-testid': 'task-row', 'data-id': task.id, 'data-status': task.status,
  },
    h('input', { type: 'checkbox', class: 'select', 'data-testid': 'task-select', checked: selected, 'aria-label': `选择「${task.title}」` }),
    task.deleted_at
      ? h('span', { class: 'toggle placeholder' })
      : h('button', {
        type: 'button', class: 'toggle', 'data-testid': 'task-toggle', 'aria-pressed': String(done),
        title: done ? '重新打开' : '标记完成', 'aria-label': done ? `重新打开「${task.title}」` : `完成「${task.title}」`,
      }, done ? '✓' : ''),
    h('span', { class: 'title', 'data-testid': 'task-title' }, task.title),
    h('span', { class: 'meta' },
      priorityBadge(task.priority),
      task.due_at ? h('span', { class: ['due', overdue && 'overdue', isDueToday(task, now) && 'today'].filter(Boolean).join(' '), 'data-testid': 'task-due', title: formatDateTime(task.due_at) },
        (overdue ? '已逾期 · ' : '') + formatDue(task.due_at, now)) : null,
      (task.tags || []).map((t) => h('span', { class: 'tag', 'data-testid': 'task-tag' }, '#' + t)),
      task.category ? h('span', { class: 'category', 'data-testid': 'task-category' }, '@' + task.category) : null,
      task.parent_id ? h('span', { class: 'muted', title: '子任务' }, '↳') : null,
      statusBadge(task)),
  );
}

// renderRows updates the list in place: rows whose task and row state are
// unchanged keep their DOM node, so a refresh (e.g. the live-update echo of
// a write) never swaps an element out from under a click.
let rowCache = new Map();
function renderRows(list, visible, s) {
  const minute = Math.floor(Date.now() / 60000); // due labels are relative to now
  const next = new Map();
  visible.forEach((t, i) => {
    const sig = [t.version, !!t.deleted_at, s.selected.has(t.id), t.id === s.cursor, t.id === s.openId, minute].join('|');
    const old = rowCache.get(t.id);
    const el = old && old.sig === sig ? old.el : taskRow(t, s);
    if (old && old.el !== el) old.el.remove();
    next.set(t.id, { el, sig });
    const at = list.children[i];
    if (at !== el) list.insertBefore(el, at || null);
  });
  for (const [id, c] of rowCache) if (!next.has(id)) c.el.remove();
  rowCache = next;
}

let renderedList = null;
function renderList() {
  const s = store.get();
  const key = [s.tasks, s.selected, s.cursor, s.openId, s.limit, s.listError, s.loaded];
  if (renderedList && key.every((v, i) => v === renderedList[i])) return;
  renderedList = key;

  renderRows($('#task-list'), s.tasks.slice(0, s.limit), s);

  const err = $('#list-error');
  err.hidden = !s.listError;
  err.textContent = s.listError ? `加载失败：${s.listError}` : '';

  const empty = $('#empty');
  empty.hidden = !s.loaded || s.tasks.length > 0 || !!s.listError;
  empty.textContent = isFiltered(s.view) || s.view.status
    ? '没有符合条件的任务。'
    : '还没有任务。在上方输入框输入标题并回车即可新增。';

  const more = $('#show-more');
  more.hidden = s.tasks.length <= s.limit;
  more.textContent = `显示更多（还有 ${s.tasks.length - s.limit} 项）`;

  const overdue = s.tasks.filter((t) => isOverdue(t)).length;
  $('#list-summary').textContent = s.loaded ? `共 ${s.count} 项` + (overdue ? ` · ${overdue} 项已逾期` : '') : '加载中…';

  const all = $('#select-all');
  all.checked = s.tasks.length > 0 && s.selected.size === s.tasks.length;
  all.indeterminate = s.selected.size > 0 && s.selected.size < s.tasks.length;
}

function renderBatchBar() {
  const { selected, view } = store.get();
  const bar = $('#batch-bar');
  bar.hidden = selected.size === 0;
  $('#batch-count').textContent = `已选 ${selected.size} 项`;
  const deletedView = view.status === 'deleted';
  for (const b of $$('[data-batch]', bar)) b.hidden = deletedView ? b.dataset.batch !== 'restore' : b.dataset.batch === 'restore';
  $('#batch-priority').hidden = deletedView;
  $('.batch-move', bar).hidden = deletedView;
}

function detailField(label, value, testid) {
  return [h('dt', null, label), h('dd', { 'data-testid': testid }, value === '' || value == null ? h('span', { class: 'muted' }, '—') : value)];
}

// detailSignature identifies what the detail pane shows; a refetch that
// returns the same content leaves the pane (and pending clicks) alone.
function detailSignature(d) {
  if (!d) return '';
  const t = d.task;
  return [t.id, t.version, (d.history || []).length, (d.conflicts || []).map((c) => c.id).join(','),
    (d.subtasks || []).map((x) => `${x.id}:${x.version}`).join(','), Math.floor(Date.now() / 60000)].join('|');
}

let renderedDetail = null;
function renderDetail() {
  const { detail, openId } = store.get();
  const sig = openId ? `${openId}#${detailSignature(detail)}` : '';
  $('#detail').hidden = !openId;
  if (sig === renderedDetail) return;
  renderedDetail = sig;
  const pane = $('#detail');
  pane.hidden = !openId;
  clear(pane);
  if (!openId) return;
  if (!detail) {
    pane.append(h('p', { class: 'muted' }, '加载中…'));
    return;
  }
  const t = detail.task;
  const deleted = !!t.deleted_at;
  const done = t.status === 'done' || t.status === 'archived';
  const btn = (label, testid, onclick, cls) => h('button', { type: 'button', 'data-testid': testid, onclick, class: cls }, label);

  pane.append(
    h('header', { class: 'detail-head' },
      h('h2', { 'data-testid': 'detail-title' }, t.title),
      h('button', { type: 'button', class: 'link close', 'data-testid': 'detail-close', 'aria-label': '关闭详情', onclick: () => openTask(null) }, '×')),
    h('div', { class: 'detail-actions' },
      deleted
        ? btn('恢复', 'detail-restore', () => restoreTask(t), 'primary')
        : [
          btn('编辑', 'detail-edit', () => openEditor(t), 'primary'),
          btn(done ? '重新打开' : '完成', 'detail-toggle', () => toggleDone(t)),
          t.status !== 'in_progress' && !done ? btn('开始', 'detail-start', () => setStatus(t, 'in_progress')) : null,
          t.status !== 'archived' ? btn('归档', 'detail-archive', () => setStatus(t, 'archived')) : null,
          btn('新增子任务', 'detail-add-subtask', () => openEditor(null, { parent_id: t.id })),
          btn('删除', 'detail-delete', () => deleteTask(t), 'danger'),
        ]),
  );

  for (const c of detail.conflicts || []) {
    pane.append(h('div', { class: 'banner warn', 'data-testid': 'detail-conflict' },
      `有一项未应用的修改（${actorLabel(c.actor)}，基于版本 ${c.base_version}，${formatDateTime(c.created_at)}）与当前版本冲突。`,
      h('button', { type: 'button', class: 'link', 'data-testid': 'detail-conflict-open', onclick: () => openStoredConflict(c.id) }, '查看并处理')));
  }

  const parent = t.parent_id ? h('a', { href: '#', onclick: (e) => { e.preventDefault(); openTask(t.parent_id); } }, shortId(t.parent_id)) : '';
  pane.append(h('dl', { class: 'fields' },
    detailField('编号', h('code', null, shortId(t.id)), 'detail-id'),
    detailField('状态', deleted ? '已删除' : STATUS_LABELS[t.status], 'detail-status'),
    detailField('优先级', PRIORITY_LABELS[t.priority], 'detail-priority'),
    detailField('截止', t.due_at ? `${formatDue(t.due_at)}（${formatDateTime(t.due_at)}）` : '', 'detail-due'),
    detailField('标签', (t.tags || []).map((g) => '#' + g).join(' '), 'detail-tags'),
    detailField('分类', t.category, 'detail-category'),
    detailField('父任务', parent, 'detail-parent'),
    detailField('描述', t.description ? h('div', { class: 'pre' }, t.description) : '', 'detail-description'),
    detailField('备注', t.notes ? h('div', { class: 'pre' }, t.notes) : '', 'detail-notes'),
    detailField('创建', formatDateTime(t.created_at), 'detail-created'),
    detailField('更新', formatDateTime(t.updated_at), 'detail-updated'),
    t.completed_at ? detailField('完成', formatDateTime(t.completed_at), 'detail-completed') : null,
    t.archived_at ? detailField('归档', formatDateTime(t.archived_at), 'detail-archived') : null,
    detailField('版本', String(t.version), 'detail-version'),
  ));

  if (detail.subtasks && detail.subtasks.length) {
    pane.append(h('h3', null, `子任务（${detail.subtasks.length}）`), h('ul', { class: 'subtasks', 'data-testid': 'detail-subtasks' },
      detail.subtasks.map((st) => h('li', null,
        statusBadge(st), ' ',
        h('a', { href: '#', onclick: (e) => { e.preventDefault(); openTask(st.id); } }, st.title)))));
  }

  const history = [...(detail.history || [])].reverse();
  pane.append(h('h3', null, `历史记录（${history.length}）`), h('ol', { class: 'history', 'data-testid': 'history' },
    history.map((e) => h('li', { 'data-testid': 'history-entry', 'data-action': e.action, 'data-actor': e.actor },
      h('div', { class: 'history-head' },
        h('strong', null, actionLabel(e.action)),
        h('span', { class: 'muted' }, ` · ${actorLabel(e.actor)} · ${formatDateTime(e.created_at)}`)),
      e.action === 'create' ? null : h('ul', { class: 'changes' }, describeChanges(e.changes).map((c) =>
        h('li', null, h('span', { class: 'field' }, c.label), `：${c.from} → ${c.to}`)))))));
}

const LIVE_TEXT = {
  connecting: '连接中…',
  live: '● 实时同步',
  offline: '已断开，正在重连…',
  closed: '连接已关闭，请刷新页面',
  unsupported: '浏览器不支持实时更新',
};

function renderLive() {
  const el = $('#live-status');
  const { live } = store.get();
  el.dataset.state = live;
  el.textContent = LIVE_TEXT[live] || live;
  el.title = live === 'live' ? '命令行和其他窗口的修改会自动出现在这里' : '';
}

function syncUrl() {
  const { view, openId } = store.get();
  const url = location.pathname + serializeView(view, { task: openId });
  if (url !== location.pathname + location.search) history.replaceState(null, '', url);
}

let renderedView;
let renderedFacets;
function render() {
  const s = store.get();
  if (s.view !== renderedView || s.facets !== renderedFacets) {
    renderedView = s.view;
    renderedFacets = s.facets;
    renderToolbar();
  }
  renderList();
  renderBatchBar();
  renderDetail();
  renderLive();
  syncUrl();
}

// ---------------------------------------------------------------- editor

function fillEditorOptions(mode) {
  const form = $('#editor-form');
  options(form.elements.status, STATUSES.map((s) => ({ value: s, label: STATUS_LABELS[s] })), 'todo');
  options(form.elements.priority, PRIORITIES.map((p) => ({ value: p, label: PRIORITY_LABELS[p] })), 'none');
  // A new task cannot start out archived (the CLI's `add` has no such option).
  form.elements.status.querySelector('option[value="archived"]').disabled = mode === 'new';
  const dl = $('#parent-options');
  clear(dl);
  for (const t of store.get().tasks.slice(0, 500)) {
    if (editor && editor.task && t.id === editor.task.id) continue;
    dl.append(h('option', { value: t.id }, t.title));
  }
}

function editorValues() {
  const f = $('#editor-form').elements;
  return {
    title: f.title.value.trim(),
    description: f.description.value,
    notes: f.notes.value,
    status: f.status.value,
    priority: f.priority.value,
    due_at: fromLocalInput(f.due_at.value),
    tags: parseTags(f.tags.value),
    category: f.category.value.trim(),
    parent_id: f.parent_id.value.trim(),
  };
}

function rawEditorValues() {
  const f = $('#editor-form').elements;
  const out = {};
  for (const k of ['title', 'description', 'notes', 'status', 'priority', 'due_at', 'tags', 'category', 'parent_id']) out[k] = f[k].value;
  return out;
}

function setEditorValues(v) {
  const f = $('#editor-form').elements;
  for (const [k, val] of Object.entries(v)) if (f[k] !== undefined && val !== undefined) f[k].value = val;
}

function taskToRaw(t) {
  return {
    title: t.title, description: t.description || '', notes: t.notes || '', status: t.status, priority: t.priority || 'none',
    due_at: toLocalInput(t.due_at), tags: (t.tags || []).join(', '), category: t.category || '', parent_id: t.parent_id || '',
  };
}

function saveDraft() {
  if (!editor) return;
  try {
    localStorage.setItem(DRAFT_KEY, JSON.stringify({
      mode: editor.mode, id: editor.task ? editor.task.id : null, version: editor.version, values: rawEditorValues(),
    }));
  } catch {
    // Private mode or quota: drafts are a convenience only.
  }
}

function readDraft() {
  try {
    return JSON.parse(localStorage.getItem(DRAFT_KEY) || 'null');
  } catch {
    return null;
  }
}

function dropDraft() {
  try {
    localStorage.removeItem(DRAFT_KEY);
  } catch {
    // ignore
  }
}

function editorBanner(text) {
  const b = $('#editor-banner');
  b.hidden = !text;
  b.textContent = text || '';
}

function editorError(text) {
  const b = $('#editor-error');
  b.hidden = !text;
  b.textContent = text || '';
}

// openEditor opens the full form for a new task (task = null; preset holds
// initial values such as parent_id) or for editing task.
function openEditor(task, preset = {}, draft = null) {
  editor = { mode: task ? 'edit' : 'new', task, version: task ? task.version : 0, remoteVersion: 0 };
  fillEditorOptions(editor.mode);
  $('#editor-title').textContent = task ? `编辑任务 ${shortId(task.id)}` : preset.parent_id ? `新增子任务（父任务 ${shortId(preset.parent_id)}）` : '新增任务';
  setEditorValues(task ? taskToRaw(task) : { ...taskToRaw({ title: '', status: 'todo', priority: 'none' }), ...preset });
  editorBanner('');
  editorError('');
  if (draft) {
    setEditorValues(draft.values);
    editor.version = draft.version || editor.version;
    editorBanner(task && task.version !== editor.version
      ? `已恢复未保存的修改。该任务在此期间已被修改（当前版本 ${task.version}），保存时会进行冲突检查。`
      : '已恢复上次未保存的输入。');
  }
  const dlg = $('#editor');
  if (!dlg.open) dlg.showModal();
  $('#editor-form').elements.title.focus();
  // Only typed input is kept as a draft; an untouched form is not restored.
  if (draft) saveDraft();
}

function closeEditor() {
  editor = null;
  dropDraft();
  const dlg = $('#editor');
  if (dlg.contains(document.activeElement)) document.activeElement.blur();
  if (dlg.open) dlg.close();
  // Hand the keyboard back to the list (some engines leave focus on the
  // hidden form field, which would swallow the next shortcut).
  $('#task-list').focus({ preventScroll: true });
}

async function saveEditor() {
  if (!editor) return;
  const values = editorValues();
  if (!values.title) {
    editorError('标题不能为空');
    $('#editor-form').elements.title.focus();
    return;
  }
  const save = $('#editor-save');
  save.disabled = true;
  try {
    if (editor.mode === 'new') {
      const body = { title: values.title, status: values.status, priority: values.priority, tags: values.tags };
      for (const k of ['description', 'notes', 'category', 'parent_id', 'due_at']) if (values[k]) body[k] = values[k];
      const res = await api.createTask(body);
      closeEditor();
      store.set({ cursor: res.task.id });
      toast(`已新增「${titleOf(res.task)}」`, { kind: 'success', undo: true });
    } else {
      const patch = formPatch(editor.task, values);
      if (Object.keys(patch).length === 0) {
        closeEditor();
        return;
      }
      const res = await api.updateTask(editor.task.id, editor.version, patch);
      closeEditor();
      toast(`已保存「${titleOf(res.task)}」`, { kind: 'success', undo: true });
    }
    await refresh();
  } catch (err) {
    if (err instanceof ApiError && err.isConflict && err.body && err.body.conflict) {
      // The server kept the rejected edit as a conflict: nothing is lost.
      closeEditor();
      showConflict(err.body.conflict);
      refresh();
    } else {
      editorError(errorMessage(err));
    }
  } finally {
    save.disabled = false;
  }
}

// ---------------------------------------------------------------- conflicts

function showConflict(view) {
  conflict = view;
  const lastOther = view.current ? view.current : null;
  $('#conflict-actor').textContent = lastOther ? `当前版本 ${lastOther.version}` : '';
  const body = $('#conflict-fields');
  clear(body);
  for (const f of view.fields || []) {
    body.append(h('tr', { class: f.conflicting ? 'conflicting' : '', 'data-testid': 'conflict-field', 'data-field': f.field },
      h('th', null, (f.conflicting ? '⚠ ' : '') + (FIELD_LABELS[f.field] || f.field)),
      h('td', null, fieldValue(f.field, f.base)),
      h('td', { class: f.changed_by_others ? 'changed' : '' }, fieldValue(f.field, f.current)),
      h('td', { class: f.changed_by_you ? 'changed' : '' }, fieldValue(f.field, f.yours))));
  }
  const dlg = $('#conflict-dialog');
  if (!dlg.open) dlg.showModal();
}

async function openStoredConflict(cid) {
  try {
    showConflict((await api.getConflict(cid)).conflict);
  } catch (err) {
    toast(errorMessage(err), { kind: 'error' });
  }
}

async function resolveConflict(resolution) {
  if (!conflict) return;
  const c = conflict;
  try {
    await api.resolveConflict(c.id, resolution, c.current ? c.current.version : 0);
    $('#conflict-dialog').close();
    conflict = null;
    toast(resolution === 'mine' ? '已应用你的修改' : '已放弃你的修改，保留对方的版本', { kind: 'success', undo: resolution === 'mine' });
    await refresh();
  } catch (err) {
    if (err instanceof ApiError && err.isConflict && err.body && err.body.conflict) {
      showConflict(err.body.conflict);
      toast('任务又被修改了，请再次确认。', { kind: 'error' });
    } else {
      $('#conflict-dialog').close();
      conflict = null;
      toast(errorMessage(err), { kind: 'error' });
      refresh();
    }
  }
}

// ---------------------------------------------------------------- selection & keyboard

function toggleSelected(id, range = false) {
  const { selected, tasks } = store.get();
  const next = new Set(selected);
  if (range && lastClickedId) {
    const a = tasks.findIndex((t) => t.id === lastClickedId);
    const b = tasks.findIndex((t) => t.id === id);
    if (a >= 0 && b >= 0) {
      const on = !selected.has(id);
      for (let i = Math.min(a, b); i <= Math.max(a, b); i++) on ? next.add(tasks[i].id) : next.delete(tasks[i].id);
      store.set({ selected: next, cursor: id });
      lastClickedId = id;
      return;
    }
  }
  next.has(id) ? next.delete(id) : next.add(id);
  lastClickedId = id;
  store.set({ selected: next, cursor: id });
}

function moveCursor(delta) {
  const { tasks, cursor, openId, limit } = store.get();
  if (!tasks.length) return;
  let i = tasks.findIndex((t) => t.id === cursor);
  i = i < 0 ? 0 : Math.min(Math.max(i + delta, 0), tasks.length - 1);
  const id = tasks[i].id;
  store.set({ cursor: id, limit: i >= limit ? limit + PAGE : limit });
  if (openId) openTask(id);
  const row = $(`.task-row[data-id="${CSS.escape(id)}"]`);
  if (row) row.scrollIntoView({ block: 'nearest' });
}

function cursorTask() {
  const { cursor } = store.get();
  return cursor ? taskById(cursor) : null;
}

function isTyping(e) {
  const t = e.target;
  return t instanceof HTMLElement && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName));
}

function onKey(e) {
  if (document.querySelector('dialog[open]')) return;
  if (isTyping(e)) {
    if (e.key === 'Escape') e.target.blur();
    return;
  }
  if (e.altKey) return;
  // Enter/Space on a focused button, link or tab activate it, not a shortcut.
  if ((e.key === 'Enter' || e.key === ' ') && e.target instanceof HTMLElement &&
      e.target.closest('button, a, summary, [role="tab"], [contenteditable]')) return;
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'a') {
    e.preventDefault();
    store.set({ selected: new Set(store.get().tasks.map((t) => t.id)) });
    return;
  }
  if (e.metaKey || e.ctrlKey) return;
  const t = cursorTask();
  const live = t && !t.deleted_at;
  const keys = {
    n: () => $('#quick-title').focus(),
    N: () => openEditor(null),
    '/': () => $('#search').focus(),
    j: () => moveCursor(1),
    ArrowDown: () => moveCursor(1),
    k: () => moveCursor(-1),
    ArrowUp: () => moveCursor(-1),
    Enter: () => t && openTask(t.id),
    e: () => live && openEditor(t),
    x: () => live && toggleDone(t),
    s: () => live && setStatus(t, 'in_progress'),
    A: () => live && setStatus(t, 'archived'),
    d: () => live && deleteTask(t),
    Delete: () => live && deleteTask(t),
    ' ': () => t && toggleSelected(t.id),
    '+': () => live && bumpPriority(t, 1),
    '=': () => live && bumpPriority(t, 1),
    '-': () => live && bumpPriority(t, -1),
    K: () => live && moveInOrder(t, -1),
    J: () => live && moveInOrder(t, 1),
    u: () => undo(),
    '?': () => $('#help-dialog').showModal(),
    Escape: () => {
      if (store.get().selected.size) store.set({ selected: new Set() });
      else if (store.get().openId) openTask(null);
    },
  };
  const fn = keys[e.key];
  if (fn) {
    e.preventDefault();
    fn();
  }
}

// ---------------------------------------------------------------- wiring

function bind() {
  buildTabs();
  const quick = $('#quick-title');
  try {
    quick.value = localStorage.getItem(QUICK_KEY) || '';
  } catch {
    // ignore
  }
  quick.addEventListener('input', () => {
    try {
      localStorage.setItem(QUICK_KEY, quick.value);
    } catch {
      // ignore
    }
  });
  $('#quick-add').addEventListener('submit', async (e) => {
    e.preventDefault();
    if (await quickAdd(quick.value)) {
      quick.value = '';
      try {
        localStorage.removeItem(QUICK_KEY);
      } catch {
        // ignore
      }
    }
  });
  $('#new-task').addEventListener('click', () => openEditor(null, quick.value.trim() ? { title: quick.value.trim() } : {}));
  $('#undo').addEventListener('click', undo);
  $('#help').addEventListener('click', () => $('#help-dialog').showModal());
  $('#help-close').addEventListener('click', () => $('#help-dialog').close());

  let searchTimer = 0;
  $('#search').addEventListener('input', (e) => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => setView({ q: e.target.value }), 150);
  });
  $('#search').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      clearTimeout(searchTimer);
      setView({ q: e.target.value });
    }
  });
  $('#filter-priority').addEventListener('change', (e) => setView({ priority: e.target.value }));
  $('#filter-tag').addEventListener('change', (e) => setView({ tag: e.target.value }));
  $('#filter-category').addEventListener('change', (e) => setView({ category: e.target.value }));
  $('#filter-due').addEventListener('change', (e) => setView({ due: e.target.value }));
  $('#sort').addEventListener('change', (e) => setView({ sort: e.target.value }));
  $('#reverse').addEventListener('click', () => setView({ reverse: !store.get().view.reverse }));
  $('#clear-filters').addEventListener('click', () => {
    const { status, sort, reverse } = store.get().view;
    setView({ ...defaultView(), status, sort, reverse });
  });

  const list = $('#task-list');
  list.addEventListener('click', (e) => {
    const row = e.target.closest('.task-row');
    if (!row) return;
    const id = row.dataset.id;
    if (e.target.closest('.select')) {
      e.preventDefault();
      toggleSelected(id, e.shiftKey);
      return;
    }
    if (e.target.closest('.toggle')) {
      const t = taskById(id);
      if (t) toggleDone(t);
      return;
    }
    if (e.shiftKey || e.metaKey || e.ctrlKey) {
      toggleSelected(id, e.shiftKey);
      return;
    }
    openTask(id);
  });
  list.addEventListener('dblclick', (e) => {
    const row = e.target.closest('.task-row');
    if (!row || e.target.closest('.select, .toggle')) return;
    const t = taskById(row.dataset.id);
    if (t && !t.deleted_at) openEditor(t);
  });
  $('#show-more').addEventListener('click', () => store.set({ limit: store.get().limit + PAGE }));
  $('#select-all').addEventListener('change', (e) => {
    store.set({ selected: e.target.checked ? new Set(store.get().tasks.map((t) => t.id)) : new Set() });
  });

  options($('#batch-priority'), [{ value: '', label: '设置优先级…' }, ...PRIORITIES.map((p) => ({ value: p, label: PRIORITY_LABELS[p] }))], '');
  for (const b of $$('[data-batch]')) b.addEventListener('click', () => runBatch(b.dataset.batch));
  $('#batch-priority').addEventListener('change', (e) => {
    const priority = e.target.value;
    e.target.value = '';
    if (priority) runBatch('priority', { priority });
  });
  $('#batch-move').addEventListener('click', () => runBatch('move', { category: $('#batch-category').value.trim() }));
  $('#batch-clear').addEventListener('click', () => store.set({ selected: new Set() }));

  const form = $('#editor-form');
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    saveEditor();
  });
  form.addEventListener('input', saveDraft);
  form.addEventListener('change', saveDraft);
  form.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      saveEditor();
    }
  });
  $('#editor-cancel').addEventListener('click', closeEditor);
  $('#editor').addEventListener('cancel', (e) => {
    e.preventDefault();
    closeEditor();
  });

  $('#conflict-mine').addEventListener('click', () => resolveConflict('mine'));
  $('#conflict-theirs').addEventListener('click', () => resolveConflict('theirs'));
  $('#conflict-dialog').addEventListener('close', () => {
    if (!$('#conflict-dialog').open) conflict = null; // also covers Esc
  });
  $('#conflict-later').addEventListener('click', () => {
    $('#conflict-dialog').close();
    conflict = null;
    toast('冲突已保留，可在任务详情中继续处理。');
  });

  document.addEventListener('keydown', onKey);
  // Catch up whenever the page comes back into view (FR-004 / NFR-008).
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') refresh();
  });
  window.addEventListener('focus', scheduleRefresh);
}

function onRemoteChange(ev) {
  scheduleRefresh();
  if (editor && editor.task && ev.task_id === editor.task.id && ev.task && ev.task.version > editor.version &&
      ev.task.version > editor.remoteVersion) {
    editor.remoteVersion = ev.task.version;
    editorBanner(`该任务刚刚被${actorLabel(ev.actor)}修改（版本 ${ev.task.version}）。你可以继续编辑，保存时会检查冲突，不会静默覆盖。`);
  }
}

async function restoreDraft() {
  const draft = readDraft();
  if (!draft || !draft.values) return;
  if (draft.mode === 'edit' && draft.id) {
    try {
      const res = await api.getTask(draft.id);
      if (res.task.deleted_at) {
        dropDraft();
        return;
      }
      openEditor(res.task, {}, draft);
    } catch {
      dropDraft();
    }
  } else {
    openEditor(null, {}, draft);
  }
}

async function main() {
  bind();
  store.subscribe(render);
  render();
  connectLive({
    onReady: () => scheduleRefresh(),
    onChange: onRemoteChange,
    onResync: () => refresh(),
    onStatus: (live) => store.set({ live }),
  });
  await refresh();
  const { openId, tasks } = store.get();
  if (!store.get().cursor && tasks.length) store.set({ cursor: openId || tasks[0].id });
  await restoreDraft();
  document.body.dataset.ready = 'true';
}

main();
