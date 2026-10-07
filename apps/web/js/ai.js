// Model and agent views of the web task manager (FR-702…FR-704): natural
// language task creation, decision support, agent runs with live logs,
// results, prompt templates, model / permission / agent configuration and
// execution history. Everything goes through the same REST API as the CLI
// (`todo ai`, `todo agent`, `todo prompt`); nothing is stored in the page
// and model keys are never sent to or shown in the browser.

import { aiApi, ApiError } from './api.js';
import { h, $, clear, options } from './dom.js';
import { PRIORITIES, PRIORITY_LABELS, formatDateTime, shortId, errorMessage } from './model.js';
import {
  MODES, MODE_LABELS, MODE_HINTS, SESSION_KIND_LABELS, SESSION_STATUS_LABELS, ITEM_KIND_LABELS, ITEM_STATUS_LABELS,
  diffRows, itemTitle, itemFields, itemActions, sessionCounts, resultLine, RUN_STATUS_LABELS, isTerminal,
  runStatusLabel, runActions, selectionLabel, initiatorLabel, formatDuration, runDuration, progressPercent, eventLine,
  RESULT_TYPE_LABELS, resultDetails, agentSource, templateVariables, aiErrorMessage, isRetryable,
} from './ai-model.js';

const TABS = [
  { value: 'intake', label: '创建任务' },
  { value: 'decide', label: '辅助决策' },
  { value: 'runs', label: '智能体运行' },
  { value: 'prompts', label: '提示词模板' },
  { value: 'config', label: '模型与智能体' },
  { value: 'history', label: '执行历史' },
];

const RUN_FILTERS = [
  { value: '', label: '全部运行' },
  { value: 'queued,running,paused,waiting_confirmation,waiting_retry', label: '进行中' },
  { value: 'succeeded,partial', label: '成功' },
  { value: 'failed,unknown', label: '失败 / 结果未知' },
  { value: 'cancelled', label: '已取消' },
];

// POLL_MS keeps run progress, logs and results within the 2 s refresh
// target (NFR-008) while anything is still running.
const POLL_MS = 1000;

let ctx = null; // { toast, refresh, openTask, selectedIds, taskById }

const ai = {
  open: false,
  tab: 'intake',
  status: null,
  statusError: null,
  agents: [],
  adapters: [],
  agentsError: null,
  sessions: { intake: null, decide: null, history: null },
  errors: {}, // per view: { message, retry }
  busy: {},
  editing: null, // { sid, n }
  runs: [],
  runsFilter: '',
  runsLoadedAt: null,
  runId: null,
  run: null,
  events: [],
  lastEventId: 0,
  runPrompt: null,
  prompts: [],
  promptId: null,
  prompt: null,
  rendered: null,
  promptValues: {}, // typed variable values of the open template
  promptTitle: '',
  testReport: null,
  agentForm: null, // agent being edited (null = closed, {} = new)
  history: null,
  task: null, // { id, runs, results, error }
};

// ---------------------------------------------------------------- helpers

const btn = (label, testid, onclick, cls, extra = {}) => h('button', { type: 'button', 'data-testid': testid, onclick, class: cls, ...extra }, label);

function taskTitle(id, fallback) {
  const t = ctx.taskById(id);
  return (t && t.title) || fallback || shortId(id);
}

function errorBox(view, err, retry) {
  ai.errors[view] = err ? { message: aiErrorMessage(err, errorMessage), retry: retry && isRetryable(err) ? retry : null } : null;
}

function renderError(view) {
  const e = ai.errors[view];
  if (!e) return null;
  return h('div', { class: 'banner error', 'data-testid': 'ai-error', role: 'alert' },
    e.message,
    e.retry ? h('button', { type: 'button', class: 'link', 'data-testid': 'ai-retry', onclick: e.retry }, '重试') : null);
}

// call runs an API request for a view: busy flag, error box, re-render.
async function call(view, fn, { retry, render } = {}) {
  ai.busy[view] = true;
  errorBox(view, null);
  render && render();
  try {
    return await fn();
  } catch (err) {
    errorBox(view, err, retry);
    if (err instanceof ApiError && (err.code === 'llm_unavailable' || err.code === 'agents_unavailable')) loadStatus();
    return null;
  } finally {
    ai.busy[view] = false;
    render && render();
  }
}

function modeOptions(current) {
  const label = ai.status ? MODE_LABELS[ai.status.mode] || ai.status.mode : '';
  return [{ value: '', label: `当前模式${label ? `（${label}）` : ''}` }, ...MODES.map((m) => ({ value: m, label: `本次：${MODE_LABELS[m]}` }))];
}

// ---------------------------------------------------------------- status & mode

async function loadStatus() {
  try {
    ai.status = await aiApi.status();
    ai.statusError = null;
  } catch (err) {
    ai.status = null;
    ai.statusError = err;
  }
  renderTopbar();
  renderBanner();
  if (ai.open && ai.tab === 'config') renderConfig();
}

async function loadAgents() {
  try {
    const res = await aiApi.agents();
    ai.agents = res.agents || [];
    ai.adapters = res.adapters || [];
    ai.agentsError = null;
  } catch (err) {
    ai.agentsError = err;
  }
  if (ai.task) renderTaskStartForm(true);
  if (ai.open && ai.tab === 'config') renderConfig();
}

function renderTopbar() {
  const chip = $('#ai-model-chip');
  const sel = $('#ai-mode');
  options(sel, MODES.map((m) => ({ value: m, label: MODE_LABELS[m] })), ai.status ? ai.status.mode : 'confirm');
  sel.disabled = !ai.status;
  sel.title = ai.status ? MODE_HINTS[ai.status.mode] || '' : '大模型功能不可用';
  if (!ai.status) {
    chip.dataset.state = 'unavailable';
    chip.textContent = '大模型不可用';
    chip.title = ai.statusError ? aiErrorMessage(ai.statusError, errorMessage) : '';
    return;
  }
  const m = ai.status.model || {};
  chip.dataset.state = ai.status.available ? 'ok' : 'unconfigured';
  chip.textContent = ai.status.available ? `模型：${m.model || m.profile || '已配置'}` : '模型未配置';
  chip.title = ai.status.available ? `${m.provider || ''} ${m.model || ''}（配置 ${m.profile || ''}）` : (m.problem || ai.status.error || '请用 todo llm 配置模型');
  for (const id of ['#intake-mode', '#decide-mode']) options($(id), modeOptions(), $(id).value || '');
}

function renderBanner() {
  const b = $('#ai-banner');
  let text = '';
  if (ai.statusError) text = `${aiErrorMessage(ai.statusError, errorMessage)}。普通任务管理不受影响。`;
  else if (ai.status && !ai.status.available) {
    const p = (ai.status.model && ai.status.model.problem) || ai.status.error || '';
    text = `模型尚未配置${p ? `（${p}）` : ''}：自然语言创建和决策会退回本地规则；可在命令行用 todo llm 配置。`;
  }
  b.hidden = !text;
  b.textContent = text;
  b.className = ai.statusError ? 'banner error' : 'banner warn';
}

async function setMode(mode) {
  try {
    ai.status = await aiApi.setMode(mode);
    ctx.toast(`权限模式已切换为「${MODE_LABELS[mode]}」`, { kind: 'success' });
  } catch (err) {
    ctx.toast(aiErrorMessage(err, errorMessage), { kind: 'error' });
  }
  renderTopbar();
  if (ai.tab === 'config') renderConfig();
}

// ---------------------------------------------------------------- panel

function setTab(tab) {
  ai.tab = tab;
  for (const b of document.querySelectorAll('#ai-tabs .tab')) b.setAttribute('aria-selected', String(b.dataset.tab === tab));
  for (const s of document.querySelectorAll('#ai-panel .ai-tab')) s.hidden = s.dataset.tab !== tab;
  switch (tab) {
    case 'intake': renderSessionView('intake'); break;
    case 'decide': renderSessionView('decide'); break;
    case 'runs': renderRuns(); loadRuns(); if (ai.runId) loadRun(ai.runId); break;
    case 'prompts': renderPrompts(); loadPrompts(); break;
    case 'config': renderConfig(); loadStatus(); loadAgents(); break;
    case 'history': renderHistory(); loadHistory(); break;
  }
}

function openPanel(tab) {
  ai.open = true;
  $('#ai-panel').hidden = false;
  $('#ai-open').setAttribute('aria-pressed', 'true');
  setTab(tab || ai.tab);
}

function closePanel() {
  ai.open = false;
  $('#ai-panel').hidden = true;
  $('#ai-open').setAttribute('aria-pressed', 'false');
  if ($('#ai-panel').contains(document.activeElement)) $('#task-list').focus({ preventScroll: true });
}

// ---------------------------------------------------------------- sessions (FR-702)

function sessionContainer(view) {
  return { intake: $('#intake-session'), decide: $('#decide-session'), history: historySessionBox }[view];
}

function renderSessionView(view) {
  const box = sessionContainer(view);
  if (!box) return;
  clear(box);
  if (view === 'intake') $('#intake-submit').disabled = !!ai.busy.intake;
  if (view === 'decide') {
    $('#decide-submit').disabled = !!ai.busy.decide;
    $('#optimize-submit').disabled = !!ai.busy.decide;
  }
  if (ai.busy[view]) box.append(h('p', { class: 'muted busy', 'data-testid': 'ai-busy' }, '正在请求大模型…'));
  const err = renderError(view);
  if (err) box.append(err);
  const sess = ai.sessions[view];
  if (sess) box.append(sessionView(view, sess));
}

function setSession(view, res) {
  if (!res) return;
  const sess = res.session || res;
  ai.sessions[view] = sess;
  if (res.error && res.error.code === 'partial_failure') errorBox(view, { code: 'partial_failure', message: res.error.message, body: res });
  ctx.refresh();
}

async function sessionAction(view, fn) {
  const res = await call(view, fn, { render: () => renderSessionView(view), retry: () => sessionAction(view, fn) });
  if (res) {
    setSession(view, res);
    renderSessionView(view);
  }
  return res;
}

function sessionView(view, sess) {
  const counts = sessionCounts(sess);
  const open = !['superseded', 'undone', 'rejected'].includes(sess.status);
  const el = h('div', { class: 'session', 'data-testid': 'session', 'data-id': sess.id, 'data-status': sess.status, 'data-kind': sess.kind });
  el.append(h('div', { class: 'session-head' },
    h('strong', null, SESSION_KIND_LABELS[sess.kind] || sess.kind),
    h('span', { class: `badge session-${sess.status}`, 'data-testid': 'session-status' }, SESSION_STATUS_LABELS[sess.status] || sess.status),
    h('span', { class: 'muted' }, ` 权限：${MODE_LABELS[sess.mode] || sess.mode} · ${formatDateTime(sess.created_at)}` +
      (sess.model ? ` · 模型 ${sess.model}` : '') + ` · ${shortId(sess.id)}`)));
  if (sess.degraded) {
    el.append(h('div', { class: 'banner warn', 'data-testid': 'session-degraded' },
      `模型不可用，结果由本地规则生成${sess.degraded_reason ? `（${sess.degraded_reason}）` : ''}，请仔细检查。`));
  }
  if (sess.input) el.append(h('p', { class: 'session-input pre muted' }, sess.input));
  if (sess.summary) el.append(h('p', { class: 'session-summary', 'data-testid': 'session-summary' }, sess.summary));
  for (const w of sess.warnings || []) el.append(h('div', { class: 'banner warn', 'data-testid': 'session-warning' }, w));

  if (sess.status === 'needs_input') {
    const text = h('textarea', { rows: 3, 'data-testid': 'session-answer-text', 'aria-label': '补充信息' });
    el.append(h('div', { class: 'questions', 'data-testid': 'session-questions' },
      h('strong', null, '大模型需要先确认：'),
      h('ol', null, (sess.questions || []).map((q) => h('li', { 'data-testid': 'session-question' }, q))),
      text,
      h('div', { class: 'ai-form-row' },
        btn('提交补充信息', 'session-answer', () => {
          if (!text.value.trim()) return;
          sessionAction(view, () => aiApi.answer(sess.id, text.value));
        }, 'primary'),
        btn('放弃', 'session-reject-all', () => sessionAction(view, () => aiApi.reject(sess.id))))));
  }

  if (sess.risks && sess.risks.length) {
    el.append(h('h4', null, '风险提示'), h('ul', { class: 'risks' }, sess.risks.map((r) =>
      h('li', { 'data-testid': 'risk', 'data-level': r.level },
        h('span', { class: `badge risk-${r.level}` }, { high: '高', medium: '中', low: '低' }[r.level] || r.level), ' ',
        r.title ? h('strong', null, r.title + '：') : null, r.message))));
  }
  if (sess.recommendations && sess.recommendations.length) {
    el.append(h('h4', null, '建议顺序'), h('ol', { class: 'recommendations', 'data-testid': 'recommendations' },
      sess.recommendations.map((r) => h('li', { 'data-testid': 'recommendation', 'data-task': r.task_id },
        h('a', { href: '#', onclick: (e) => { e.preventDefault(); if (r.task_id) ctx.openTask(r.task_id); } }, r.title || taskTitle(r.task_id)),
        r.reason ? h('div', { class: 'muted', 'data-testid': 'recommendation-reason' }, `理由：${r.reason}`) : null,
        r.focus || r.estimate ? h('div', { class: 'muted' }, [r.focus ? `关注：${r.focus}` : '', r.estimate ? `预计：${r.estimate}` : ''].filter(Boolean).join(' · ')) : null,
        r.task_id ? btn('启动智能体', 'recommendation-agent', () => startForTask(r.task_id), 'link') : null))));
  }

  if (sess.items && sess.items.length) {
    el.append(h('h4', null, sess.kind === 'decide' ? '建议的变更' : sess.kind === 'optimize' ? '优化建议' : '解析结果'),
      h('ol', { class: 'session-items' }, sess.items.map((it) => itemView(view, sess, it))));
  } else if (sess.status !== 'needs_input') {
    el.append(h('p', { class: 'muted', 'data-testid': 'session-empty' }, '没有需要执行的变更。'));
  }

  const actions = h('div', { class: 'ai-form-row session-actions' });
  if (open && counts.pending > 0) {
    actions.append(btn(`全部接受（${counts.pending}）`, 'session-apply-all', () => sessionAction(view, () => aiApi.apply(sess.id)), 'primary'),
      btn(sess.kind === 'decide' ? '忽略建议' : '全部拒绝', 'session-reject-all', () => sessionAction(view, () => aiApi.reject(sess.id))));
  }
  if (counts.applied > 0 && sess.status !== 'superseded') {
    actions.append(btn('撤销已执行的变更', 'session-undo-all', () => sessionAction(view, () => aiApi.undo(sess.id))));
  }
  if (sess.kind === 'decide' && sess.status !== 'superseded') {
    const fb = h('input', { 'data-testid': 'session-feedback', placeholder: '补充要求（可选），例如：先做发布相关的', 'aria-label': '重新决策的补充要求' });
    actions.append(fb, btn('重新决策', 'session-redecide', () => sessionAction(view, () => aiApi.redecide(sess.id, fb.value))));
  }
  if (actions.children.length) el.append(actions);
  if (sess.result) {
    el.append(h('div', { class: 'session-result', 'data-testid': 'session-result' }, h('strong', null, resultLine(sess.result)),
      (sess.result.messages || []).length ? h('ul', { class: 'muted' }, sess.result.messages.map((m) => h('li', null, m))) : null));
  }
  if (sess.superseded_by) el.append(h('p', { class: 'muted' }, `已被新的决策 ${shortId(sess.superseded_by)} 替代。`));
  return el;
}

function itemView(view, sess, it) {
  const act = itemActions(it, sess);
  const li = h('li', { class: `session-item item-${it.status}`, 'data-testid': 'session-item', 'data-n': String(it.n), 'data-status': it.status, 'data-kind': it.kind },
    h('div', { class: 'item-head' },
      h('span', { class: 'badge' }, ITEM_KIND_LABELS[it.kind] || it.kind),
      h('strong', { 'data-testid': 'item-title' }, itemTitle(it)),
      h('span', { class: `badge item-status-${it.status}`, 'data-testid': 'item-status' }, ITEM_STATUS_LABELS[it.status] || it.status),
      it.applied_by ? h('span', { class: 'muted' }, it.applied_by === 'auto' ? '自动执行' : '用户确认') : null));
  if (it.reason) li.append(h('p', { class: 'muted', 'data-testid': 'item-reason' }, `理由：${it.reason}`));
  const fields = itemFields(it);
  if (fields.length && it.kind !== 'update') {
    li.append(h('dl', { class: 'fields item-fields', 'data-testid': 'item-fields' }, fields.map(([k, v]) => [h('dt', null, k), h('dd', null, v)])));
  }
  const rows = diffRows(it);
  if (rows.length) {
    li.append(h('table', { class: 'diff', 'data-testid': 'item-diff' },
      h('thead', null, h('tr', null, h('th', null, '字段'), h('th', null, '变更前'), h('th', null, '变更后'))),
      h('tbody', null, rows.map((r) => h('tr', { 'data-testid': 'diff-row', 'data-field': r.field },
        h('th', null, r.label),
        h('td', { class: 'before pre', 'data-testid': 'diff-before' }, r.before),
        h('td', { class: `after pre${r.changed ? ' changed' : ''}`, 'data-testid': 'diff-after' }, r.after))))));
  }
  if (it.optimize) {
    const o = it.optimize;
    li.append(h('dl', { class: 'fields', 'data-testid': 'item-optimize' },
      [['对象', o.target], ['问题', o.problem], ['方案', o.proposal], ['预期影响', o.expected_impact], ['验证方式', o.verification], ['回滚', o.rollback]]
        .filter(([, v]) => v).map(([k, v]) => [h('dt', null, k), h('dd', null, v)])));
  }
  if (it.command && it.command.prompt) {
    li.append(h('details', { class: 'item-prompt' }, h('summary', null, '将传给智能体的提示词'), h('pre', { 'data-testid': 'item-prompt' }, it.command.prompt)));
  }
  if (it.needs_confirm && it.status === 'pending' && it.confirm_reason) {
    li.append(h('p', { class: 'confirm-reason', 'data-testid': 'item-confirm-reason' }, `需要确认：${it.confirm_reason}`));
  }
  if (it.message) li.append(h('p', { class: 'muted', 'data-testid': 'item-message' }, it.message));

  if (ai.editing && ai.editing.sid === sess.id && ai.editing.n === it.n) {
    li.append(itemEditor(view, sess, it));
    return li;
  }
  const actions = h('div', { class: 'item-actions' });
  if (act.apply) actions.append(btn('接受', 'item-apply', () => sessionAction(view, () => aiApi.apply(sess.id, [it.n])), 'primary'));
  if (act.reject) actions.append(btn('拒绝', 'item-reject', () => sessionAction(view, () => aiApi.reject(sess.id, [it.n]))));
  if (act.edit) actions.append(btn('修改', 'item-edit', () => { ai.editing = { sid: sess.id, n: it.n }; renderSessionView(view); }));
  if (act.undo) actions.append(btn('撤销', 'item-undo', () => sessionAction(view, () => aiApi.undo(sess.id, [it.n]))));
  if (it.result_task_id) actions.append(btn('查看任务', 'item-open-task', () => ctx.openTask(it.result_task_id), 'link'));
  else if (it.task_id) actions.append(btn('查看任务', 'item-open-task', () => ctx.openTask(it.task_id), 'link'));
  if (it.command && it.command.run_id) actions.append(btn('查看运行', 'item-open-run', () => openRun(it.command.run_id), 'link'));
  if (actions.children.length) li.append(actions);
  return li;
}

// itemEditor edits a pending item before it is accepted (FR-304).
function itemEditor(view, sess, it) {
  const form = h('form', { class: 'item-editor', 'data-testid': 'item-editor' });
  const input = (name, value, label, attrs = {}) => h('label', null, label, h('input', { name, value: value ?? '', 'data-testid': `item-edit-${name}`, ...attrs }));
  if (it.kind === 'change') {
    const cur = it.change ? it.change.to : '';
    form.append(input('to', Array.isArray(cur) ? cur.join(',') : cur, `新的${it.change ? it.change.field : ''}值`));
  } else {
    const f = it.fields || {};
    const prio = h('select', { name: 'priority', 'data-testid': 'item-edit-priority' });
    options(prio, [{ value: '', label: '不变' }, ...PRIORITIES.map((p) => ({ value: p, label: PRIORITY_LABELS[p] }))], f.priority || '');
    form.append(
      input('title', f.title, '标题'),
      input('due', f.due_text || (f.due ? formatDateTime(f.due) : ''), '截止时间（如 明天、2026-10-09 18:00，留空清除）'),
      h('label', null, '优先级', prio),
      input('tags', (f.tags || []).join(', '), '标签'),
      input('category', f.category, '分类'),
      h('label', null, '描述', h('textarea', { name: 'description', rows: 2, 'data-testid': 'item-edit-description' }, f.description || '')));
  }
  const save = async () => {
    const el = form.elements;
    const edit = {};
    if (it.kind === 'change') edit.to = el.to.value;
    else {
      const f = it.fields || {};
      if (el.title.value !== (f.title || '')) edit.title = el.title.value;
      const dueBefore = f.due_text || (f.due ? formatDateTime(f.due) : '');
      if (el.due.value !== dueBefore) edit.due = el.due.value.trim();
      if (el.priority.value && el.priority.value !== (f.priority || '')) edit.priority = el.priority.value;
      const tags = el.tags.value.split(/[,，\s]+/).map((t) => t.replace(/^#/, '').trim()).filter(Boolean);
      if (tags.join(',') !== (f.tags || []).join(',')) edit.tags = tags;
      if (el.category.value !== (f.category || '')) edit.category = el.category.value;
      if (el.description.value !== (f.description || '')) edit.description = el.description.value;
    }
    const res = await sessionAction(view, () => aiApi.editItem(sess.id, it.n, edit));
    if (res) {
      ai.editing = null;
      renderSessionView(view);
    }
  };
  form.addEventListener('submit', (e) => { e.preventDefault(); save(); });
  form.append(h('div', { class: 'item-actions' },
    h('button', { type: 'submit', class: 'primary', 'data-testid': 'item-edit-save' }, '保存修改'),
    btn('取消', 'item-edit-cancel', () => { ai.editing = null; renderSessionView(view); })));
  return form;
}

async function submitIntake() {
  const text = $('#intake-text').value.trim();
  if (!text) {
    errorBox('intake', { code: 'invalid_input', message: '请输入要创建的任务描述' });
    renderSessionView('intake');
    return;
  }
  const body = { text, mode: $('#intake-mode').value };
  if ($('#intake-selected').checked) body.selected = ctx.selectedIds();
  const res = await sessionAction('intake', () => aiApi.intake(body));
  if (res) $('#intake-text').value = '';
}

function submitDecide() {
  return sessionAction('decide', () => aiApi.decide({ selected: ctx.selectedIds(), mode: $('#decide-mode').value }));
}

// ---------------------------------------------------------------- runs (FR-703)

async function loadRuns() {
  const params = { limit: '100' };
  if (ai.runsFilter) params.status = ai.runsFilter;
  try {
    ai.runs = (await aiApi.runs(params)).runs || [];
    ai.runsLoadedAt = new Date();
    errorBox('runs', null);
  } catch (err) {
    errorBox('runs', err, loadRuns);
  }
  if (ai.open && ai.tab === 'runs') renderRunsList();
}

function runRow(run, { onclick, selected } = {}) {
  const pct = progressPercent(run);
  return h('li', {
    class: `run-item${selected ? ' is-selected' : ''}`, 'data-testid': 'run-item', 'data-id': run.id, 'data-status': run.status, onclick,
  },
    h('div', { class: 'run-item-head' },
      h('strong', null, run.agent || '（未选择）'),
      h('span', { class: `badge run-${run.status}`, 'data-testid': 'run-item-status', 'data-status': run.status }, runStatusLabel(run))),
    h('div', { class: 'muted run-item-task' }, taskTitle(run.task_id, run.input && run.input.task ? run.input.task.title : '')),
    !isTerminal(run.status) && pct > 0 ? h('progress', { max: '100', value: String(pct) }) : null,
    h('div', { class: 'muted small' },
      [run.started_at ? `开始 ${formatDateTime(run.started_at)}` : `创建 ${formatDateTime(run.created_at)}`,
        runDuration(run) != null ? `耗时 ${formatDuration(runDuration(run))}` : '', run.attempt > 1 ? `第 ${run.attempt} 次` : '']
        .filter(Boolean).join(' · ')));
}

function renderRunsList() {
  const list = $('#runs-list');
  clear(list);
  const err = renderError('runs');
  if (err) list.append(h('li', null, err));
  if (!ai.runs.length && !err) list.append(h('li', { class: 'muted empty-small', 'data-testid': 'runs-empty' }, '还没有智能体运行。在任务详情中选择智能体并启动。'));
  for (const r of ai.runs) list.append(runRow(r, { selected: r.id === ai.runId, onclick: () => openRun(r.id) }));
  $('#runs-updated').textContent = ai.runsLoadedAt ? `更新于 ${ai.runsLoadedAt.toLocaleTimeString('zh-CN', { hour12: false })}` : '';
}

function renderRuns() {
  options($('#runs-filter'), RUN_FILTERS, ai.runsFilter);
  renderRunsList();
  renderRunView();
}

function openRun(id) {
  if (ai.runId !== id) {
    ai.runId = id;
    ai.run = null;
    ai.events = [];
    ai.lastEventId = 0;
    ai.runPrompt = null;
  }
  if (!ai.open || ai.tab !== 'runs') openPanel('runs');
  else {
    renderRunsList();
    renderRunView();
    loadRun(id);
  }
}

let runSeq = 0;
async function loadRun(id) {
  const seq = ++runSeq;
  try {
    const [res, ev] = await Promise.all([aiApi.run(id), aiApi.runEvents(id, ai.lastEventId)]);
    if (seq !== runSeq || ai.runId !== id) return;
    ai.run = res.run;
    for (const e of ev.events || []) {
      if (e.id > ai.lastEventId) {
        ai.events.push(e);
        ai.lastEventId = e.id;
      }
    }
    errorBox('run', null);
  } catch (err) {
    if (seq !== runSeq) return;
    errorBox('run', err, () => loadRun(id));
  }
  if (ai.open && ai.tab === 'runs') renderRunView();
  const i = ai.runs.findIndex((r) => r.id === id);
  if (ai.run && i >= 0 && ai.runs[i].status !== ai.run.status) {
    ai.runs[i] = { ...ai.runs[i], ...ai.run };
    renderRunsList();
  }
}

async function control(run, verb) {
  const fns = {
    pause: () => aiApi.pauseRun(run.id),
    resume: () => aiApi.resumeRun(run.id),
    cancel: () => aiApi.cancelRun(run.id),
    retry: () => aiApi.retryRun(run.id, retryInput(run.id)),
    confirm: () => aiApi.confirmRun(run.id),
    reject: () => aiApi.rejectRun(run.id),
  };
  const done = { pause: '已暂停', resume: '已继续', cancel: '已取消', retry: '已重新运行', confirm: '已确认启动', reject: '已拒绝启动' };
  try {
    const res = await fns[verb]();
    if (ai.runId === run.id) ai.run = res.run;
    ctx.toast(`${done[verb]}：${run.agent} 运行 ${shortId(run.id)}`, { kind: 'success' });
  } catch (err) {
    ctx.toast(aiErrorMessage(err, errorMessage), { kind: 'error' });
  }
  if (ai.runId === run.id) loadRun(run.id);
  loadRuns();
  if (ai.task && ai.task.id === run.task_id) loadTaskAgents(run.task_id);
  ctx.refresh();
}

function runControls(run, prefix) {
  const a = runActions(run);
  const out = [];
  const add = (verb, label, cls) => a[verb] && out.push(btn(label, `${prefix}-${verb}`, () => control(run, verb), cls));
  add('confirm', '确认启动', 'primary');
  add('reject', '拒绝启动', 'danger');
  add('pause', '暂停');
  add('resume', '继续', 'primary');
  add('cancel', '取消', 'danger');
  add('retry', '重试', 'primary');
  return out;
}

function resultView(r) {
  const details = resultDetails(r);
  return h('li', { class: 'result', 'data-testid': 'result', 'data-type': r.type, 'data-run': r.run_id },
    h('div', { class: 'result-head' },
      h('span', { class: 'badge', 'data-testid': 'result-type' }, RESULT_TYPE_LABELS[r.type] || r.type),
      h('strong', { 'data-testid': 'result-summary' }, r.summary || ''),
      r.version > 1 ? h('span', { class: 'muted' }, `v${r.version}`) : null),
    h('div', { class: 'muted small', 'data-testid': 'result-source' },
      `来源：${agentSource(r.agent)} · ${formatDateTime(r.created_at)}` + (r.run_id && r.run_id !== 'manual' ? ` · 运行 ${shortId(r.run_id)}` : '')),
    details.length ? h('details', { class: 'result-details' }, h('summary', null, '详情'),
      h('dl', { class: 'fields', 'data-testid': 'result-details' }, details.map(([k, v]) => [h('dt', null, k), h('dd', { class: 'pre' }, v)]))) : null);
}

// retryInput is the extra context typed for a retry of the open run.
function retryInput(id) {
  const el = ai.runId === id ? $('#run-view').querySelector('[data-testid="run-retry-context"]') : null;
  return el ? el.value.trim() : '';
}

let logFollow = true;
function renderRunView() {
  const box = $('#run-view');
  const oldLog = box.querySelector('.run-log');
  if (oldLog) logFollow = oldLog.scrollTop + oldLog.clientHeight >= oldLog.scrollHeight - 8;
  const retryValue = ai.runId ? retryInput(ai.runId) : '';
  clear(box);
  const err = renderError('run');
  if (err) box.append(err);
  if (!ai.runId) {
    box.append(h('p', { class: 'muted' }, '选择左侧的一次运行查看进度、实时输出、日志和结果。'));
    return;
  }
  const run = ai.run;
  if (!run) {
    box.append(h('p', { class: 'muted' }, '加载中…'));
    return;
  }
  const pct = progressPercent(run);
  box.append(
    h('header', { class: 'run-head' },
      h('h3', { 'data-testid': 'run-agent' }, `智能体 ${run.agent}`),
      h('span', { class: `badge run-${run.status}`, 'data-testid': 'run-status', 'data-status': run.status }, runStatusLabel(run))),
    h('dl', { class: 'fields' },
      h('dt', null, '任务'), h('dd', null, h('a', { href: '#', 'data-testid': 'run-task', onclick: (e) => { e.preventDefault(); ctx.openTask(run.task_id); } },
        taskTitle(run.task_id, run.input && run.input.task ? run.input.task.title : ''))),
      h('dt', null, '选择'), h('dd', { 'data-testid': 'run-selection' }, selectionLabel(run.selection) + ` · ${run.adapter || 'cli'} 适配器`),
      h('dt', null, '发起'), h('dd', null, `${initiatorLabel(run.initiator)}${run.session_id ? `（模型会话 ${shortId(run.session_id)}）` : ''}`),
      h('dt', null, '开始'), h('dd', { 'data-testid': 'run-started' }, run.started_at ? formatDateTime(run.started_at) : '尚未开始'),
      h('dt', null, '结束'), h('dd', { 'data-testid': 'run-ended' }, run.ended_at ? formatDateTime(run.ended_at) : '—'),
      h('dt', null, '耗时'), h('dd', { 'data-testid': 'run-duration' }, formatDuration(runDuration(run)) || '—'),
      h('dt', null, '尝试'), h('dd', null, `第 ${run.attempt || 0} 次${run.options && run.options.max_retries ? `（最多自动重试 ${run.options.max_retries} 次）` : ''}`)),
    h('div', { class: 'run-progress', 'data-testid': 'run-progress', 'data-percent': String(Math.round(pct)) },
      h('progress', { max: '100', value: String(pct) }),
      h('span', { 'data-testid': 'run-stage' }, (run.stage ? `${run.stage} · ` : '') + `${Math.round(pct)}%`)),
  );
  if (run.error) {
    box.append(h('div', { class: 'banner error', 'data-testid': 'run-error' },
      `${run.error}${run.error_kind ? `（${run.error_kind}）` : ''}${run.exit_code != null ? ` · 退出码 ${run.exit_code}` : ''}`));
  }
  const ctl = h('div', { class: 'item-actions run-controls', 'data-testid': 'run-controls' }, runControls(run, 'run'));
  if (runActions(run).retry) {
    ctl.append(h('input', { 'data-testid': 'run-retry-context', placeholder: '重试时补充说明（可选）', value: retryValue, 'aria-label': '重试补充说明' }));
  }
  ctl.append(btn(ai.runPrompt ? '收起提示词' : '查看最终提示词', 'run-show-prompt', async () => {
    if (ai.runPrompt) ai.runPrompt = null;
    else {
      try {
        ai.runPrompt = await aiApi.runPrompt(run.id);
      } catch (e) {
        ctx.toast(aiErrorMessage(e, errorMessage), { kind: 'error' });
      }
    }
    renderRunView();
  }));
  box.append(ctl);
  if (ai.runPrompt) box.append(h('pre', { class: 'prompt-text', 'data-testid': 'run-prompt' }, ai.runPrompt.prompt || '（空）'));

  if (run.attempts && run.attempts.length > 1) {
    box.append(h('h4', null, '执行记录'), h('table', { class: 'diff attempts', 'data-testid': 'run-attempts' },
      h('thead', null, h('tr', null, h('th', null, '尝试'), h('th', null, '状态'), h('th', null, '开始'), h('th', null, '耗时'), h('th', null, '错误'))),
      h('tbody', null, run.attempts.map((a) => h('tr', { 'data-testid': 'run-attempt' },
        h('td', null, String(a.attempt)), h('td', null, RUN_STATUS_LABELS[a.status] || a.status), h('td', null, formatDateTime(a.started_at)),
        h('td', null, formatDuration(a.duration_ms)), h('td', null, a.error || ''))))));
  }
  const results = run.results || [];
  box.append(h('h4', null, `结果（${results.length}）`));
  box.append(results.length
    ? h('ul', { class: 'results', 'data-testid': 'run-results' }, results.map(resultView))
    : h('p', { class: 'muted' }, isTerminal(run.status) ? '这次运行没有回写结果。' : '运行结束后结果会显示在这里。'));

  box.append(h('h4', null, `实时输出与日志（${ai.events.length}）`));
  const log = h('ol', { class: 'run-log', 'data-testid': 'run-log' }, ai.events.map((ev) => {
    const l = eventLine(ev);
    return h('li', { class: l.cls, 'data-testid': 'run-log-line', 'data-kind': ev.kind },
      h('span', { class: 'muted' }, l.time), ' ', h('span', { class: 'ev-kind' }, l.stream || l.kind), ' ', h('span', { class: 'pre' }, l.text));
  }));
  box.append(log);
  if (logFollow) log.scrollTop = log.scrollHeight;
}

// ---------------------------------------------------------------- task detail: start, runs, results (FR-501, FR-511)

const taskBox = h('section', { class: 'detail-agents', 'data-testid': 'detail-agents' });
const taskForm = h('form', { class: 'agent-start', 'data-testid': 'agent-start-form', autocomplete: 'off' });
const taskRuns = h('div', { 'data-testid': 'detail-runs' });
const taskResults = h('div', { 'data-testid': 'detail-results' });
taskBox.append(h('h3', null, '智能体执行'), taskForm, taskRuns, taskResults);

function renderTaskStartForm(keep) {
  const prev = keep && taskForm.elements.agent ? {
    agent: taskForm.elements.agent.value, complete: taskForm.elements.complete.checked, context: taskForm.elements.context.value,
    template: taskForm.elements.template.value,
  } : null;
  clear(taskForm);
  const agentSel = h('select', { name: 'agent', 'data-testid': 'agent-select', 'aria-label': '选择智能体' });
  options(agentSel, [{ value: 'auto', label: '自动选择（由大模型决定）' },
    ...ai.agents.map((a) => ({ value: a.name, label: `${a.name}（${a.adapter || 'cli'}）${a.description ? ' · ' + a.description : ''}` }))], prev ? prev.agent : 'auto');
  const tplSel = h('select', { name: 'template', 'data-testid': 'agent-template', 'aria-label': '提示词模板' });
  options(tplSel, [{ value: '', label: '不使用提示词模板' }, ...ai.prompts.map((p) => ({ value: p.id, label: `模板：${p.name}` }))], prev ? prev.template : '');
  const vars = h('div', { class: 'template-vars', 'data-testid': 'agent-template-vars' });
  tplSel.addEventListener('change', () => fillTemplateVars(vars, tplSel.value));
  taskForm.append(
    h('div', { class: 'ai-form-row' }, agentSel, tplSel),
    vars,
    h('textarea', { name: 'context', rows: 2, 'data-testid': 'agent-context', placeholder: '附加上下文或约束（可选）', 'aria-label': '附加上下文' }, prev ? prev.context : ''),
    h('div', { class: 'ai-form-row' },
      h('label', { class: 'inline' }, h('input', { type: 'checkbox', name: 'complete', 'data-testid': 'agent-complete', checked: prev ? prev.complete : false }), ' 成功后将任务标记为完成'),
      btn('预览提示词', 'agent-preview', () => startRun(true)),
      h('button', { type: 'submit', class: 'primary', 'data-testid': 'agent-start' }, '启动智能体')),
    h('pre', { class: 'prompt-text', 'data-testid': 'agent-preview-text', hidden: true }));
  if (ai.agentsError) taskForm.append(h('p', { class: 'muted', 'data-testid': 'agents-unavailable' }, aiErrorMessage(ai.agentsError, errorMessage)));
  if (prev && prev.template) fillTemplateVars(vars, prev.template);
}
taskForm.addEventListener('submit', (e) => { e.preventDefault(); startRun(false); });

async function fillTemplateVars(box, pid) {
  clear(box);
  if (!pid) return;
  try {
    const t = await aiApi.prompt(pid);
    for (const v of templateVariables(t)) {
      box.append(h('label', null, `${v.name}${v.description ? `（${v.description}）` : ''}`,
        h('input', { 'data-var': v.name, 'data-testid': 'agent-template-var', value: v.default })));
    }
  } catch (err) {
    box.append(h('p', { class: 'muted' }, aiErrorMessage(err, errorMessage)));
  }
}

async function startRun(dryRun) {
  if (!ai.task) return;
  const f = taskForm.elements;
  const body = { agent: f.agent.value, context: f.context.value.trim(), complete_on_success: f.complete.checked, dry_run: dryRun };
  if (f.template.value) {
    body.template = f.template.value;
    body.vars = {};
    for (const inp of taskForm.querySelectorAll('[data-var]')) body.vars[inp.dataset.var] = inp.value;
  }
  const preview = taskForm.querySelector('[data-testid="agent-preview-text"]');
  try {
    const res = await aiApi.startRun(ai.task.id, body);
    if (dryRun) {
      preview.hidden = false;
      preview.textContent = `智能体：${res.run.agent}（${selectionLabel(res.run.selection)}）\n\n${res.run.prompt || ''}`;
      return;
    }
    preview.hidden = true;
    f.context.value = '';
    ctx.toast(`已启动智能体 ${res.run.agent}（${RUN_STATUS_LABELS[res.run.status] || res.run.status}）`, { kind: 'success' });
    loadTaskAgents(ai.task.id);
    if (ai.open && ai.tab === 'runs') loadRuns();
    ctx.refresh();
  } catch (err) {
    ctx.toast('启动失败：' + aiErrorMessage(err, errorMessage), { kind: 'error' });
  }
}

let taskSeq = 0;
async function loadTaskAgents(id) {
  const seq = ++taskSeq;
  try {
    const [runs, results] = await Promise.all([aiApi.runs({ task: id, limit: '20' }), aiApi.taskResults(id)]);
    if (seq !== taskSeq || !ai.task || ai.task.id !== id) return;
    ai.task.runs = runs.runs || [];
    ai.task.results = results.results || [];
    ai.task.error = null;
  } catch (err) {
    if (seq !== taskSeq || !ai.task || ai.task.id !== id) return;
    ai.task.error = err;
  }
  renderTaskAgents();
}

function renderTaskAgents() {
  const t = ai.task;
  clear(taskRuns);
  clear(taskResults);
  if (!t) return;
  if (t.error) {
    taskRuns.append(h('p', { class: 'muted', 'data-testid': 'detail-agents-error' }, aiErrorMessage(t.error, errorMessage)));
    return;
  }
  if (t.runs === null) {
    taskRuns.append(h('p', { class: 'muted' }, '加载中…'));
    return;
  }
  if (t.runs.length) {
    taskRuns.append(h('h4', null, `运行（${t.runs.length}）`), h('ul', { class: 'runs-list compact' }, t.runs.map((r) => {
      const li = runRow(r, { onclick: (e) => { if (!e.target.closest('button')) openRun(r.id); } });
      li.append(h('div', { class: 'item-actions' }, runControls(r, 'detail-run'), btn('日志', 'detail-run-open', () => openRun(r.id), 'link')));
      return li;
    })));
  }
  taskResults.append(h('h4', null, `执行结果（${t.results.length}）`));
  taskResults.append(t.results.length
    ? h('ul', { class: 'results', 'data-testid': 'task-results' }, t.results.map(resultView))
    : h('p', { class: 'muted', 'data-testid': 'task-results-empty' }, '暂无回写结果。'));
}

// taskSection is the agent block of the task detail pane. The element is
// reused across re-renders so the start form keeps what the user chose.
function taskSection(task) {
  if (!task || task.deleted_at) return null;
  if (!ai.task || ai.task.id !== task.id) {
    ai.task = { id: task.id, runs: null, results: [], error: null };
    renderTaskStartForm(false);
    renderTaskAgents();
    loadTaskAgents(task.id);
  }
  return taskBox;
}

function startForTask(id) {
  ctx.openTask(id);
  if (ai.open) closePanel();
  setTimeout(() => {
    const sel = taskForm.querySelector('[data-testid="agent-select"]');
    if (sel) {
      sel.scrollIntoView({ block: 'nearest' });
      sel.focus();
    }
  }, 100);
}

// ---------------------------------------------------------------- prompt templates (FR-507)

async function loadPrompts() {
  try {
    ai.prompts = (await aiApi.prompts()).templates || [];
    errorBox('prompts', null);
  } catch (err) {
    errorBox('prompts', err, loadPrompts);
  }
  if (ai.open && ai.tab === 'prompts') renderPromptList();
  if (ai.task) renderTaskStartForm(true);
}

function renderPrompts() {
  renderPromptList();
  renderPromptView();
}

function renderPromptList() {
  const list = $('#prompt-list');
  clear(list);
  const err = renderError('prompts');
  if (err) list.append(h('li', null, err));
  if (!ai.prompts.length && !err) list.append(h('li', { class: 'muted empty-small' }, '还没有提示词模板。'));
  for (const p of ai.prompts) {
    list.append(h('li', { class: `run-item${p.id === ai.promptId ? ' is-selected' : ''}`, 'data-testid': 'prompt-item', 'data-id': p.id, onclick: () => openPrompt(p.id) },
      h('strong', null, p.name), h('div', { class: 'muted small' }, `版本 ${p.current_version} · 更新于 ${formatDateTime(p.updated_at)}`)));
  }
}

async function openPrompt(id) {
  ai.promptId = id;
  ai.rendered = null;
  ai.promptValues = {};
  ai.promptTitle = '';
  renderPromptList();
  try {
    ai.prompt = await aiApi.prompt(id);
  } catch (err) {
    ai.prompt = null;
    errorBox('prompt', err);
  }
  renderPromptView();
}

function copyText(text, label) {
  const done = () => ctx.toast(`已复制${label}`, { kind: 'success' });
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, () => ctx.toast('浏览器不允许复制，请手动选择文本复制', { kind: 'error' }));
  } else {
    ctx.toast('浏览器不支持复制，请手动选择文本复制', { kind: 'error' });
  }
}

function renderPromptView() {
  const box = $('#prompt-view');
  clear(box);
  const err = renderError('prompt');
  if (err) box.append(err);
  const t = ai.prompt;
  if (!t) {
    box.append(h('p', { class: 'muted' }, '选择一个模板查看原始提示词、总结版本，并用新参数复用。'));
    return;
  }
  const v = t.latest || {};
  const s = v.summary || {};
  const list = (items) => (items && items.length ? h('ol', null, items.map((x) => h('li', null, x))) : '—');
  box.append(
    h('header', { class: 'run-head' }, h('h3', { 'data-testid': 'prompt-name' }, t.name), h('span', { class: 'muted' }, `当前版本 ${t.current_version} · ${v.source === 'model' ? '大模型总结' : '手动'}`)),
    h('dl', { class: 'fields', 'data-testid': 'prompt-summary' },
      h('dt', null, '目标'), h('dd', { 'data-testid': 'prompt-goal' }, s.goal || '—'),
      h('dt', null, '上下文'), h('dd', null, s.context || '—'),
      h('dt', null, '约束'), h('dd', null, list(s.constraints)),
      h('dt', null, '步骤'), h('dd', null, list(s.steps)),
      h('dt', null, '输出格式'), h('dd', null, s.output_format || '—'),
      h('dt', null, '变量'), h('dd', { 'data-testid': 'prompt-variables' }, (s.variables || []).map((x) => `{{${x.name}}}`).join(' ') || '—')),
    h('h4', null, '模板正文'),
    h('pre', { class: 'prompt-text', 'data-testid': 'prompt-body' }, v.body || ''),
    h('details', { class: 'ai-box' }, h('summary', null, '原始提示词'), h('pre', { class: 'prompt-text', 'data-testid': 'prompt-original' }, t.original || '')),
    h('div', { class: 'item-actions' },
      btn('复制正文', 'prompt-copy-text', () => copyText(v.body || '', '模板正文')),
      btn('复制为新模板', 'prompt-copy', async () => {
        try {
          const c = await aiApi.copyPrompt(t.id, `${t.name}-副本`);
          ctx.toast(`已复制为「${c.name}」`, { kind: 'success' });
          await loadPrompts();
          openPrompt(c.id);
        } catch (e) {
          ctx.toast(aiErrorMessage(e, errorMessage), { kind: 'error' });
        }
      })),
  );
  const vars = templateVariables(t);
  const form = h('form', { class: 'ai-form', 'data-testid': 'prompt-reuse-form', autocomplete: 'off' },
    h('h4', null, '用新参数复用'),
    vars.map((x) => h('label', null, `${x.name}${x.description ? `（${x.description}）` : ''}`,
      h('input', { 'data-var': x.name, 'data-testid': 'prompt-var', value: ai.promptValues[x.name] ?? x.default }))),
    h('label', null, '任务标题（可选）', h('input', { name: 'title', 'data-testid': 'prompt-task-title', value: ai.promptTitle })),
    h('div', { class: 'ai-form-row' },
      btn('预览最终提示词', 'prompt-render', () => reuse(form, false)),
      h('button', { type: 'submit', class: 'primary', 'data-testid': 'prompt-create-task' }, '创建智能体任务')));
  form.addEventListener('submit', (e) => { e.preventDefault(); reuse(form, true); });
  form.addEventListener('input', (e) => {
    if (e.target.dataset.var) ai.promptValues[e.target.dataset.var] = e.target.value;
    else if (e.target.name === 'title') ai.promptTitle = e.target.value;
  });
  box.append(form);
  if (ai.rendered != null) box.append(h('pre', { class: 'prompt-text', 'data-testid': 'prompt-rendered' }, ai.rendered));
}

async function reuse(form, create) {
  const values = {};
  for (const inp of form.querySelectorAll('[data-var]')) values[inp.dataset.var] = inp.value;
  const t = ai.prompt;
  try {
    if (create) {
      const res = await aiApi.promptTask(t.id, values, form.elements.title.value.trim());
      ai.rendered = res.prompt;
      ctx.toast(`已用模板创建任务「${res.task.title}」`, { kind: 'success' });
      await ctx.refresh();
      ctx.openTask(res.task.id);
    } else {
      ai.rendered = (await aiApi.renderPrompt(t.id, values)).prompt;
    }
    errorBox('prompt', null);
  } catch (err) {
    errorBox('prompt', err);
  }
  renderPromptView();
}

async function summarize() {
  const text = $('#summarize-text').value.trim();
  const name = $('#summarize-name').value.trim();
  if (!text || !name) {
    ctx.toast('请填写原始提示词和模板名称', { kind: 'error' });
    return;
  }
  $('#summarize-submit').disabled = true;
  try {
    const res = await aiApi.summarize({ text, name, save: true });
    ctx.toast(res.degraded ? `模型不可用，已按本地规则整理并保存「${name}」` : `已总结并保存模板「${name}」`, { kind: res.degraded ? 'info' : 'success' });
    $('#summarize-text').value = '';
    $('#summarize-name').value = '';
    $('#summarize-box').open = false;
    await loadPrompts();
    if (res.template) openPrompt(res.template.id);
  } catch (err) {
    ctx.toast(aiErrorMessage(err, errorMessage), { kind: 'error' });
  } finally {
    $('#summarize-submit').disabled = false;
  }
}

// ---------------------------------------------------------------- configuration (FR-704)

function renderConfig() {
  const box = $('#config-view');
  clear(box);
  if (ai.statusError) {
    box.append(h('div', { class: 'banner error', 'data-testid': 'config-unavailable' }, aiErrorMessage(ai.statusError, errorMessage)),
      btn('重试', 'config-retry', () => { loadStatus(); loadAgents(); }));
    return;
  }
  const st = ai.status;
  if (!st) {
    box.append(h('p', { class: 'muted' }, '加载中…'));
    return;
  }
  const m = st.model || {};
  const keyText = (p) => (p.api_key_set ? `已配置（来源：${p.key_source || '环境变量'}）` : p.key_source === 'none' ? '不需要' : '未配置');
  box.append(h('h3', null, '模型'), h('dl', { class: 'fields', 'data-testid': 'config-model' },
    h('dt', null, '当前配置'), h('dd', { 'data-testid': 'config-profile' }, m.profile || '—'),
    h('dt', null, '服务商'), h('dd', null, m.provider || '—'),
    h('dt', null, '模型'), h('dd', { 'data-testid': 'config-model-name' }, m.model || '—'),
    h('dt', null, '接口地址'), h('dd', null, m.base_url || '—'),
    h('dt', null, '密钥'), h('dd', { 'data-testid': 'config-key' }, keyText(m)),
    h('dt', null, '状态'), h('dd', null, st.available ? '可用' : `不可用${m.problem ? `：${m.problem}` : ''}`)),
  h('p', { class: 'muted small' }, '密钥只通过环境变量或系统钥匙串（todo llm set-key）提供，网页不会存储或显示密钥。'));

  if (st.profiles && st.profiles.length > 1) {
    box.append(h('ul', { class: 'profiles', 'data-testid': 'config-profiles' }, st.profiles.map((p) => h('li', { 'data-testid': 'config-profile-item', 'data-profile': p.profile },
      h('strong', null, p.profile), ` ${p.provider} · ${p.model} · 密钥${keyText(p)}`,
      p.profile === m.profile ? h('span', { class: 'badge' }, '当前') : btn('设为当前', 'config-use', async () => {
        try {
          ai.status = await aiApi.use(p.profile);
          ctx.toast(`已切换到模型配置 ${p.profile}`, { kind: 'success' });
        } catch (e) {
          ctx.toast(aiErrorMessage(e, errorMessage), { kind: 'error' });
        }
        renderTopbar();
        renderConfig();
      }, 'link')))));
  }
  const test = h('div', { class: 'ai-form-row' }, btn('测试连接', 'config-test', async () => {
    ai.testReport = { pending: true };
    renderConfig();
    try {
      ai.testReport = await aiApi.test('');
    } catch (e) {
      ai.testReport = { ok: false, error: { message: aiErrorMessage(e, errorMessage) } };
    }
    renderConfig();
  }));
  const r = ai.testReport;
  if (r) {
    test.append(h('span', { 'data-testid': 'config-test-result', 'data-ok': String(!!r.ok), class: r.ok ? 'ok' : 'error-text' },
      r.pending ? '测试中…' : r.ok ? `连接成功（${r.latency_ms} 毫秒）` :
        `连接失败：${(r.error && (r.error.message + (r.error.hint ? `（${r.error.hint}）` : ''))) || ''}` +
        (r.alternatives && r.alternatives.length ? `；可切换到：${r.alternatives.join('、')}` : '')));
  }
  box.append(test);

  // Permission mode and confirm list (FR-305, FR-604).
  const modeBox = h('div', { class: 'modes', 'data-testid': 'config-modes', role: 'radiogroup', 'aria-label': '权限模式' });
  for (const mode of MODES) {
    modeBox.append(h('label', { class: `mode-option${st.mode === mode ? ' is-current' : ''}` },
      h('input', { type: 'radio', name: 'ai-mode-radio', value: mode, checked: st.mode === mode, 'data-testid': `config-mode-${mode}`, onchange: () => setMode(mode) }),
      h('strong', null, MODE_LABELS[mode]), h('span', { class: 'muted small' }, MODE_HINTS[mode])));
  }
  const confirm = h('textarea', { rows: 3, 'data-testid': 'config-confirm-list', 'aria-label': '自定义确认清单' }, (st.confirm_list || []).join('\n'));
  box.append(h('h3', null, '权限'), modeBox,
    h('p', { class: 'muted small' }, `默认需要确认的高风险类别：${(st.default_confirm || []).join('、')}`),
    h('label', { class: 'block' }, '自定义确认清单（每行一项：类别、操作类型或命令前缀，如 git push）', confirm),
    h('div', { class: 'ai-form-row' }, btn('保存确认清单', 'config-confirm-save', async () => {
      try {
        ai.status = await aiApi.setConfirmList(confirm.value.split('\n').map((x) => x.trim()).filter(Boolean));
        ctx.toast('确认清单已保存', { kind: 'success' });
      } catch (e) {
        ctx.toast(aiErrorMessage(e, errorMessage), { kind: 'error' });
      }
      renderConfig();
    })));

  // Agents and adapters (FR-505, FR-510).
  box.append(h('h3', null, '智能体与适配器'));
  if (ai.agentsError) box.append(h('p', { class: 'muted' }, aiErrorMessage(ai.agentsError, errorMessage)));
  box.append(h('p', { class: 'muted small', 'data-testid': 'config-adapters' }, `可用适配器：${ai.adapters.join('、') || '—'}`));
  box.append(h('ul', { class: 'agents', 'data-testid': 'config-agents' }, ai.agents.map((a) => h('li', { 'data-testid': 'config-agent', 'data-name': a.name },
    h('div', { class: 'item-head' }, h('strong', null, a.name), h('span', { class: 'badge' }, `${a.adapter || 'cli'} 适配器`), a.confirm ? h('span', { class: 'badge' }, '启动前确认') : null),
    a.description ? h('div', { class: 'muted' }, a.description) : null,
    h('code', { class: 'small' }, a.command && a.command.length ? a.command.join(' ') : a.url || (a.profile ? `模型配置 ${a.profile}` : '')),
    h('div', { class: 'item-actions' },
      btn('编辑', 'config-agent-edit', () => { ai.agentForm = { ...a }; renderConfig(); }, 'link'),
      btn('删除', 'config-agent-delete', async () => {
        if (!window.confirm(`删除智能体 ${a.name}？已有的运行记录会保留。`)) return;
        try {
          await aiApi.deleteAgent(a.name);
          ctx.toast(`已删除智能体 ${a.name}`, { kind: 'success' });
        } catch (e) {
          ctx.toast(aiErrorMessage(e, errorMessage), { kind: 'error' });
        }
        loadAgents();
      }, 'link danger'))))));
  if (!ai.agentForm) box.append(btn('新增智能体', 'config-agent-new', () => { ai.agentForm = {}; renderConfig(); }));
  else box.append(agentEditor(ai.agentForm));
}

function agentEditor(a) {
  const isNew = !a.name;
  const adapter = h('select', { name: 'adapter', 'data-testid': 'agent-edit-adapter' });
  options(adapter, (ai.adapters.length ? ai.adapters : ['cli', 'http', 'llm']).map((x) => ({ value: x, label: x })), a.adapter || 'cli');
  const input = (name, value, label, attrs = {}) => h('label', null, label, h('input', { name, value: value ?? '', 'data-testid': `agent-edit-${name}`, ...attrs }));
  const io = (name, list, value) => {
    const s = h('select', { name, 'data-testid': `agent-edit-${name}` });
    options(s, [{ value: '', label: '默认' }, ...list.map((x) => ({ value: x, label: x }))], value || '');
    return s;
  };
  const form = h('form', { class: 'ai-form agent-editor', 'data-testid': 'agent-editor', autocomplete: 'off' },
    h('h4', null, isNew ? '新增智能体' : `编辑智能体 ${a.name}`),
    input('name', a.name, '名称（一个词）', isNew ? { required: true } : { readonly: true }),
    h('label', null, '适配器', adapter),
    input('description', a.description, '说明'),
    input('command', a.command ? JSON.stringify(a.command) : '', '启动命令（JSON 数组，如 ["claude","-p"]；cli 适配器）'),
    input('dir', a.dir, '工作目录'),
    input('url', a.url, '接口地址（http 适配器）'),
    input('profile', a.profile, '模型配置（llm 适配器，留空为当前）'),
    input('env', (a.env || []).join(', '), '传递的环境变量名（逗号分隔，不填值）'),
    input('timeout_seconds', a.timeout_seconds || '', '超时（秒）', { type: 'number', min: '0' }),
    input('success_codes', (a.success_codes || []).join(','), '成功退出码（逗号分隔，默认 0）'),
    h('div', { class: 'ai-form-row' },
      h('label', null, '输入方式 ', io('input', ['json-stdin', 'json-arg', 'prompt-stdin', 'prompt-arg', 'none'], a.input)),
      h('label', null, '输出解析 ', io('output', ['jsonl', 'json', 'text'], a.output)),
      h('label', { class: 'inline' }, h('input', { type: 'checkbox', name: 'confirm', checked: !!a.confirm, 'data-testid': 'agent-edit-confirm' }), ' 启动前需要确认')),
    h('div', { class: 'item-actions' },
      h('button', { type: 'submit', class: 'primary', 'data-testid': 'agent-edit-save' }, '保存'),
      btn('取消', 'agent-edit-cancel', () => { ai.agentForm = null; renderConfig(); })));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const f = form.elements;
    const spec = { ...a, name: f.name.value.trim(), adapter: f.adapter.value, description: f.description.value.trim(), dir: f.dir.value.trim(),
      url: f.url.value.trim(), profile: f.profile.value.trim(), input: f.input.value, output: f.output.value, confirm: f.confirm.checked,
      env: f.env.value.split(/[,，\s]+/).filter(Boolean), timeout_seconds: Number(f.timeout_seconds.value) || 0,
      success_codes: f.success_codes.value.split(/[,，\s]+/).filter(Boolean).map(Number) };
    const cmd = f.command.value.trim();
    try {
      spec.command = !cmd ? [] : cmd.startsWith('[') ? JSON.parse(cmd) : cmd.split(/\s+/);
    } catch {
      ctx.toast('启动命令需要是 JSON 字符串数组', { kind: 'error' });
      return;
    }
    try {
      await aiApi.putAgent(spec.name, spec);
      ctx.toast(`已保存智能体 ${spec.name}`, { kind: 'success' });
      ai.agentForm = null;
      await loadAgents();
    } catch (err) {
      ctx.toast(aiErrorMessage(err, errorMessage), { kind: 'error' });
    }
  });
  return form;
}

// ---------------------------------------------------------------- history (FR-702 变更记录, FR-704 执行历史)

const historySessionBox = h('div', { class: 'history-session', 'data-testid': 'history-session-view' });

async function loadHistory() {
  try {
    const [sessions, runs, actions] = await Promise.all([aiApi.sessions('', 30), aiApi.runs({ limit: '50' }), aiApi.actions(30)]);
    ai.history = { sessions: sessions.sessions || [], runs: runs.runs || [], actions: actions.actions || [] };
    errorBox('history', null);
  } catch (err) {
    errorBox('history', err, loadHistory);
  }
  if (ai.open && ai.tab === 'history') renderHistory();
}

function renderHistory() {
  const box = $('#ai-history-view');
  const keep = ai.sessions.history;
  clear(box);
  const err = renderError('history');
  if (err) box.append(err);
  const hist = ai.history;
  if (!hist) {
    if (!err) box.append(h('p', { class: 'muted' }, '加载中…'));
    return;
  }
  box.append(h('h3', null, '智能体执行历史'), hist.runs.length
    ? h('ul', { class: 'runs-list', 'data-testid': 'history-runs' }, hist.runs.map((r) => runRow(r, { onclick: () => openRun(r.id) })))
    : h('p', { class: 'muted' }, '暂无运行。'));
  box.append(h('h3', null, '大模型会话与变更记录'), hist.sessions.length
    ? h('ul', { class: 'runs-list', 'data-testid': 'history-sessions' }, hist.sessions.map((s) => h('li', {
      class: `run-item${keep && keep.id === s.id ? ' is-selected' : ''}`, 'data-testid': 'history-session', 'data-id': s.id, 'data-kind': s.kind,
      onclick: async () => {
        try {
          ai.sessions.history = await aiApi.session(s.id);
        } catch (e) {
          errorBox('history', e);
        }
        renderHistory();
      },
    },
      h('div', { class: 'run-item-head' }, h('strong', null, SESSION_KIND_LABELS[s.kind] || s.kind),
        h('span', { class: 'badge' }, SESSION_STATUS_LABELS[s.status] || s.status)),
      h('div', { class: 'muted run-item-task' }, s.summary || s.input || ''),
      h('div', { class: 'muted small' }, `${formatDateTime(s.created_at)} · ${MODE_LABELS[s.mode] || s.mode} · ${s.items} 项变更` + (s.pending ? `，${s.pending} 项待确认` : '')))))
    : h('p', { class: 'muted' }, '暂无模型会话。'));
  clear(historySessionBox);
  if (keep) historySessionBox.append(sessionView('history', keep));
  box.append(historySessionBox);
  if (hist.actions.length) {
    box.append(h('h3', null, '命令与智能体启动记录'), h('table', { class: 'diff', 'data-testid': 'history-actions' },
      h('thead', null, h('tr', null, h('th', null, '时间'), h('th', null, '类型'), h('th', null, '内容'), h('th', null, '结果'), h('th', null, '发起'))),
      h('tbody', null, hist.actions.map((a) => h('tr', { 'data-testid': 'history-action' },
        h('td', null, formatDateTime(a.created_at)), h('td', null, ITEM_KIND_LABELS[a.kind] || a.kind),
        h('td', null, a.request && a.request.argv ? a.request.argv.join(' ') : a.run_id ? `运行 ${shortId(a.run_id)}` : ''),
        h('td', null, a.status + (a.result ? ` · 退出码 ${a.result.exit_code}` : '')), h('td', null, a.actor))))));
  }
}

// ---------------------------------------------------------------- polling

let tickBusy = false;
let ticks = 0;
async function tick() {
  if (tickBusy || document.visibilityState === 'hidden') return;
  tickBusy = true;
  ticks++;
  try {
    const jobs = [];
    if (ai.task && ai.task.runs && ai.task.runs.some((r) => !isTerminal(r.status))) jobs.push(loadTaskAgents(ai.task.id));
    if (ai.open && ai.tab === 'runs') {
      if (ai.runs.some((r) => !isTerminal(r.status)) || ticks % 5 === 0) jobs.push(loadRuns());
      if (ai.runId && (!ai.run || !isTerminal(ai.run.status))) jobs.push(loadRun(ai.runId));
    }
    await Promise.all(jobs);
  } finally {
    tickBusy = false;
  }
}

// ---------------------------------------------------------------- wiring

export function initAI(context) {
  ctx = context;
  const tabs = $('#ai-tabs');
  for (const t of TABS) {
    tabs.append(h('button', { type: 'button', role: 'tab', class: 'tab', 'data-tab': t.value, 'data-testid': `ai-tab-btn-${t.value}`, 'aria-selected': 'false', onclick: () => setTab(t.value) }, t.label));
  }
  $('#ai-open').addEventListener('click', () => (ai.open ? closePanel() : openPanel()));
  $('#ai-close').addEventListener('click', closePanel);
  $('#ai-mode').addEventListener('change', (e) => setMode(e.target.value));
  $('#intake-form').addEventListener('submit', (e) => { e.preventDefault(); submitIntake(); });
  $('#intake-text').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      submitIntake();
    }
  });
  $('#decide-form').addEventListener('submit', (e) => { e.preventDefault(); submitDecide(); });
  $('#optimize-submit').addEventListener('click', () => sessionAction('decide', () => aiApi.optimize()));
  $('#runs-filter').addEventListener('change', (e) => { ai.runsFilter = e.target.value; loadRuns(); });
  $('#runs-refresh').addEventListener('click', () => { loadRuns(); if (ai.runId) loadRun(ai.runId); });
  $('#summarize-form').addEventListener('submit', (e) => { e.preventDefault(); summarize(); });
  renderTopbar();
  loadStatus();
  loadAgents();
  loadPrompts();
  setInterval(tick, POLL_MS);
  return {
    isOpen: () => ai.open,
    toggle: () => (ai.open ? closePanel() : openPanel()),
    open: openPanel,
    close: closePanel,
    taskSection,
    startForTask,
    // onRemoteChange reacts to the shared event stream: agent runs record
    // their progress in the task history, so the detail catches up at once.
    onRemoteChange(ev) {
      if (ai.task && ev && ev.task_id === ai.task.id) loadTaskAgents(ai.task.id);
      if (ai.open && ai.tab === 'runs') loadRuns();
    },
    onDetailClosed() {
      ai.task = null;
    },
  };
}
