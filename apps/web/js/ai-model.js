// Pure helpers of the model and agent views (ai.js): labels, diffs, run
// states and controls, durations, results and log lines. No DOM access, so
// the module is unit tested under Node (apps/web/test/ai-model.test.mjs).

import { STATUS_LABELS, PRIORITY_LABELS, formatDateTime } from './model.js';

// ---- permission modes (FR-305) ----

export const MODES = ['suggest', 'confirm', 'auto'];

export const MODE_LABELS = { suggest: '仅建议', confirm: '执行前确认', auto: '自动执行' };

export const MODE_HINTS = {
  suggest: '大模型只给出建议，任何变更都需要你手动接受。',
  confirm: '大模型的变更先展示差异，确认后才执行。',
  auto: '已授权的变更直接执行并记录；删除、覆盖、外发、提交、系统配置等高风险操作仍需确认。',
};

// ---- model sessions ----

export const SESSION_KIND_LABELS = { intake: '自然语言创建', decide: '辅助决策', optimize: '自我优化建议' };

export const SESSION_STATUS_LABELS = {
  needs_input: '待补充信息',
  pending: '待确认',
  partial: '部分执行',
  applied: '已处理',
  rejected: '已拒绝',
  undone: '已撤销',
  superseded: '已被重新决策替代',
};

export const ITEM_KIND_LABELS = {
  create: '新建任务',
  subtask: '新建子任务',
  update: '修改任务',
  change: '调整',
  command: '执行命令',
  agent: '启动智能体',
  optimize: '优化建议',
};

export const ITEM_STATUS_LABELS = {
  pending: '待确认',
  applied: '已执行',
  rejected: '已拒绝',
  skipped: '未执行',
  failed: '失败',
  undone: '已撤销',
};

const DIFF_FIELD_LABELS = {
  title: '标题',
  description: '描述',
  due: '截止时间',
  due_at: '截止时间',
  priority: '优先级',
  tags: '标签',
  category: '分类',
  status: '状态',
  depends_on: '依赖',
  order: '顺序',
  delete: '删除',
  parent: '父任务',
  command: '命令',
  agent: '智能体',
};

export function diffFieldLabel(field) {
  if (DIFF_FIELD_LABELS[field]) return DIFF_FIELD_LABELS[field];
  return String(field || '');
}

// diffValue renders one side of a diff: priorities and statuses get their
// Chinese names, arrays are joined, objects become JSON.
export function diffValue(field, v) {
  if (v === null || v === undefined || v === '') return '（空）';
  if (Array.isArray(v)) return v.length ? v.map((x) => diffValue(field, x)).join('、') : '（空）';
  if (typeof v === 'object') return JSON.stringify(v, null, 2);
  const s = String(v);
  if (field === 'priority' && PRIORITY_LABELS[s]) return PRIORITY_LABELS[s];
  if (field === 'status' && STATUS_LABELS[s]) return STATUS_LABELS[s];
  if ((field === 'due' || field === 'due_at') && /^\d{4}-\d{2}-\d{2}T/.test(s)) return formatDateTime(s) || s;
  return s;
}

// diffRows turns an item's diff into display rows (FR-404). Line diffs of
// optimisation suggestions (field "x (lines)") are kept as text blocks.
export function diffRows(item) {
  return (item && item.diff ? item.diff : []).map((d) => ({
    field: d.field,
    label: diffFieldLabel(d.field),
    before: diffValue(d.field, d.before),
    after: diffValue(d.field, d.after),
    changed: JSON.stringify(d.before ?? null) !== JSON.stringify(d.after ?? null),
  }));
}

// itemTitle names an item the way the CLI's session listing does.
export function itemTitle(item) {
  if (!item) return '';
  if (item.fields && item.fields.title) return item.fields.title;
  if (item.task_title) return item.task_title;
  if (item.optimize && item.optimize.target) return item.optimize.target;
  if (item.command) {
    if (item.command.agent) return `智能体 ${item.command.agent}`;
    if (item.command.argv && item.command.argv.length) return item.command.argv.join(' ');
  }
  return `#${item.n}`;
}

// itemFields lists the proposed fields of a create/update item.
export function itemFields(item) {
  const f = item && item.fields;
  if (!f) return [];
  const out = [];
  if (f.title != null) out.push(['标题', f.title]);
  if (f.description) out.push(['描述', f.description]);
  if (f.due) out.push(['截止时间', formatDateTime(f.due) + (f.due_text ? `（${f.due_text}）` : '')]);
  else if (f.due === '') out.push(['截止时间', '（清除）']);
  if (f.priority) out.push(['优先级', PRIORITY_LABELS[f.priority] || f.priority]);
  if (f.tags && f.tags.length) out.push(['标签', f.tags.map((t) => '#' + t).join(' ')]);
  if (f.category) out.push(['分类', '@' + f.category]);
  if (f.depends_on && f.depends_on.length) out.push(['依赖', f.depends_on.join('、')]);
  return out;
}

// itemActions says which buttons an item offers.
export function itemActions(item, session) {
  const open = session && !['superseded', 'undone', 'rejected', 'needs_input'].includes(session.status);
  const pending = item.status === 'pending';
  const editable = ['create', 'subtask', 'update', 'change'].includes(item.kind);
  return {
    apply: open && pending,
    reject: open && pending,
    edit: open && editable && (pending || item.status === 'skipped'),
    undo: item.status === 'applied' && session && session.status !== 'superseded' && item.kind !== 'command' && item.kind !== 'agent',
  };
}

export function sessionCounts(session) {
  const c = { pending: 0, applied: 0, rejected: 0, skipped: 0, failed: 0, undone: 0 };
  for (const it of (session && session.items) || []) c[it.status] = (c[it.status] || 0) + 1;
  return c;
}

// resultLine is the FR-306 summary: created, updated, not executed, failed.
export function resultLine(result) {
  if (!result) return '';
  return `新增 ${result.created || 0} · 修改 ${result.updated || 0} · 未执行 ${result.not_executed || 0} · 失败 ${result.failed || 0}`;
}

// ---- agent runs (FR-502…FR-504) ----

export const RUN_STATUS_LABELS = {
  queued: '排队中',
  waiting_confirmation: '等待确认',
  running: '运行中',
  paused: '已暂停',
  waiting_retry: '等待重试',
  succeeded: '成功',
  partial: '部分成功',
  failed: '失败',
  cancelled: '已取消',
  unknown: '结果未知',
};

const TERMINAL = ['succeeded', 'partial', 'failed', 'cancelled', 'unknown'];

export function isTerminal(status) {
  return TERMINAL.includes(status);
}

export function runStatusLabel(run) {
  if (!run) return '';
  if (run.status === 'running' && run.control === 'pause') return '正在暂停…';
  if (run.status === 'running' && run.control === 'cancel') return '正在取消…';
  return RUN_STATUS_LABELS[run.status] || run.status_label || run.status;
}

// runActions mirrors the state checks of packages/agent's controls, so the
// page only offers what the server accepts.
export function runActions(run) {
  const s = run ? run.status : '';
  const ctl = run ? run.control || '' : '';
  return {
    pause: (s === 'running' && ctl === '') || s === 'queued' || s === 'waiting_retry',
    resume: s === 'paused' || (s === 'running' && ctl === 'pause'),
    cancel: !!run && !isTerminal(s) && ctl !== 'cancel',
    retry: ['failed', 'partial', 'cancelled', 'unknown', 'waiting_retry'].includes(s),
    confirm: s === 'waiting_confirmation',
    reject: s === 'waiting_confirmation',
  };
}

export function selectionLabel(sel) {
  if (!sel) return '';
  switch (sel.by) {
    case 'llm': return '大模型自动选择' + (sel.reason ? `：${sel.reason}` : '') + (sel.degraded ? '（模型不可用，已按规则选择）' : '');
    case 'routing': return '按路由规则选择' + (sel.reason ? `：${sel.reason}` : '');
    case 'only': return '唯一配置的智能体';
    case 'user': return '用户指定';
  }
  return sel.by || '';
}

export function initiatorLabel(who) {
  return { web: '网页', cli: '命令行', tui: '终端界面', llm: '大模型' }[who] || who || '';
}

// formatDuration renders milliseconds as "850 毫秒", "12 秒", "3 分 05 秒",
// "1 小时 02 分".
export function formatDuration(ms) {
  if (ms == null || Number.isNaN(ms) || ms < 0) return '';
  if (ms < 1000) return `${Math.round(ms)} 毫秒`;
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s} 秒`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} 分 ${String(s % 60).padStart(2, '0')} 秒`;
  return `${Math.floor(m / 60)} 小时 ${String(m % 60).padStart(2, '0')} 分`;
}

// runDuration is the execution time: the server's figure once the run
// ended, otherwise measured from the start, minus paused time.
export function runDuration(run, now = new Date()) {
  if (!run) return null;
  if (isTerminal(run.status) || !run.started_at) return run.duration_ms ?? null;
  const started = new Date(run.started_at).getTime();
  if (Number.isNaN(started)) return run.duration_ms ?? null;
  return Math.max(0, now.getTime() - started - (run.paused_ms || 0));
}

export function progressPercent(run) {
  if (!run) return 0;
  if (run.status === 'succeeded') return 100;
  const p = Number(run.progress || 0);
  return Math.min(100, Math.max(0, p));
}

export const EVENT_KIND_LABELS = {
  state: '状态',
  system: '系统',
  output: '输出',
  log: '日志',
  progress: '进度',
  command: '命令',
  error: '错误',
  result: '结果',
};

// eventLine formats one run event for the log view.
export function eventLine(ev) {
  const at = ev.at ? new Date(ev.at) : null;
  const time = at && !Number.isNaN(at.getTime())
    ? `${String(at.getHours()).padStart(2, '0')}:${String(at.getMinutes()).padStart(2, '0')}:${String(at.getSeconds()).padStart(2, '0')}`
    : '';
  const kind = EVENT_KIND_LABELS[ev.kind] || ev.kind;
  const stream = ev.kind === 'output' && ev.stream ? ev.stream : '';
  return { time, kind, stream, text: ev.message || '', attempt: ev.attempt || 0, cls: `ev-${ev.kind}${stream === 'stderr' ? ' ev-stderr' : ''}` };
}

// ---- results (FR-508, FR-511) ----

export const RESULT_TYPE_LABELS = {
  text: '文本',
  file: '文件',
  commit: '提交记录',
  command_output: '命令输出',
  data: '结构化数据',
  task_status: '任务状态',
};

// resultDetails lists the metadata worth showing for a result type.
export function resultDetails(r) {
  const d = (r && r.data) || {};
  const rows = [];
  switch (r && r.type) {
    case 'text':
      if (d.text) rows.push(['内容', d.text]);
      break;
    case 'file':
      rows.push(['路径', d.path || '']);
      if (d.exists === false) rows.push(['状态', '文件不存在']);
      if (d.size != null) rows.push(['大小', `${d.size} 字节`]);
      if (d.outside_workdir) rows.push(['注意', '文件在工作目录之外']);
      break;
    case 'commit':
      rows.push(['仓库', d.repo || ''], ['分支', d.branch || ''], ['提交', d.commit || '']);
      if (d.message) rows.push(['说明', d.message]);
      if (d.files && d.files.length) rows.push(['文件', d.files.join('\n')]);
      if (d.url) rows.push(['链接', d.url]);
      break;
    case 'command_output':
      rows.push(['命令', d.command || ''], ['退出码', String(d.exit_code ?? '')]);
      if (d.stdout) rows.push(['标准输出', d.stdout]);
      if (d.stderr) rows.push(['标准错误', d.stderr]);
      break;
    case 'task_status':
      rows.push(['状态', d.status || '']);
      if (d.system) rows.push(['系统', d.system]);
      if (d.external_id) rows.push(['外部编号', d.external_id]);
      if (d.url) rows.push(['链接', d.url]);
      if (d.local_status) rows.push(['本任务状态', STATUS_LABELS[d.local_status] || d.local_status]);
      break;
    case 'data':
      rows.push(['数据', JSON.stringify(d.data ?? d, null, 2)]);
      break;
  }
  return rows;
}

export function agentSource(agent) {
  if (!agent) return '未知来源';
  if (agent === 'web') return '网页手动回写';
  if (agent === 'cli') return '命令行手动回写';
  return `智能体 ${agent}`;
}

// ---- prompt templates ----

// templateVariables returns the variables of a template version.
export function templateVariables(tpl) {
  const v = tpl && (tpl.latest || tpl);
  return ((v && v.summary && v.summary.variables) || []).map((x) => ({ name: x.name, description: x.description || '', default: x.default || '' }));
}

// ---- errors ----

const AI_ERRORS = {
  llm_unavailable: '大模型功能不可用（模型模块未能启动）',
  agents_unavailable: '智能体功能不可用',
  llm_not_configured: '尚未配置大模型：请在命令行用 `todo llm` 配置模型与密钥',
  llm_auth_failed: '模型认证失败，请检查密钥',
  llm_not_found: '模型或接口地址不存在',
  llm_rate_limited: '模型服务限流，请稍后重试',
  llm_server_error: '模型服务出错',
  llm_timeout: '模型请求超时',
  llm_unreachable: '无法连接模型服务',
  llm_bad_request: '模型拒绝了请求',
  llm_bad_response: '模型返回了无法解析的结果',
  invalid_state: '当前状态不允许该操作',
  forbidden: '该操作被权限策略禁止',
  missing_variables: '缺少模板变量',
  partial_failure: '部分操作失败',
};

// aiErrorMessage turns model and agent API errors into short Chinese text,
// with the server's hint (never a key: the server redacts messages).
export function aiErrorMessage(err, fallback) {
  if (!err) return '';
  const code = err.code || '';
  const body = (err.body && err.body.error) || {};
  let msg = AI_ERRORS[code];
  if (!msg) return fallback ? fallback(err) : err.message || String(err);
  if (code === 'missing_variables' && body.variables) msg += '：' + body.variables.join('、');
  else if (['invalid_state', 'forbidden', 'partial_failure'].includes(code) && err.message) msg += '：' + err.message;
  if (body.hint) msg += `（${body.hint}）`;
  return msg;
}

// isRetryable says whether a retry button makes sense for an error.
export function isRetryable(err) {
  if (!err) return false;
  if (err.code === 'network_error') return true;
  const body = (err.body && err.body.error) || {};
  return !!body.retryable || ['llm_timeout', 'llm_unreachable', 'llm_rate_limited', 'llm_server_error'].includes(err.code);
}
