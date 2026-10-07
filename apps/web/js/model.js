// Pure helpers shared by the web UI: labels, view <-> query mapping, date
// formatting, quick-add parsing and error messages. No DOM access here so
// the module can be unit tested under Node (apps/web/test).

export const STATUSES = ['todo', 'in_progress', 'done', 'archived'];

export const STATUS_LABELS = {
  todo: '待办',
  in_progress: '进行中',
  done: '已完成',
  archived: '已归档',
};

export const PRIORITIES = ['urgent', 'high', 'medium', 'low', 'none'];

export const PRIORITY_LABELS = {
  none: '无',
  low: '低',
  medium: '中',
  high: '高',
  urgent: '紧急',
};

// Status tabs. "" is the CLI's default listing: everything not archived.
export const STATUS_TABS = [
  { value: '', label: '全部' },
  { value: 'todo', label: '待办' },
  { value: 'in_progress', label: '进行中' },
  { value: 'done', label: '已完成' },
  { value: 'archived', label: '已归档' },
  { value: 'deleted', label: '回收站' },
];

export const SORTS = [
  { value: 'manual', label: '手动顺序' },
  { value: 'due', label: '截止时间' },
  { value: 'priority', label: '优先级' },
  { value: 'created', label: '创建时间' },
  { value: 'updated', label: '更新时间' },
];

export const DUE_FILTERS = [
  { value: '', label: '任意截止时间' },
  { value: 'overdue', label: '已逾期' },
  { value: 'today', label: '今天截止' },
  { value: 'week', label: '7 天内截止' },
  { value: 'has', label: '有截止时间' },
  { value: 'none', label: '无截止时间' },
];

export const ACTOR_LABELS = {
  cli: '命令行',
  tui: '终端界面',
  web: '网页',
  import: '导入',
  llm: '大模型',
  agent: '智能体',
};

export const ACTION_LABELS = {
  create: '创建',
  update: '编辑',
  complete: '完成',
  start: '开始',
  reopen: '重新打开',
  archive: '归档',
  delete: '删除',
  restore: '恢复',
  priority: '修改优先级',
  move: '移动',
  reorder: '调整顺序',
  revert: '恢复历史版本',
  import: '导入',
  agent_run: '启动智能体',
  agent_result: '智能体结果回写',
  agent_succeeded: '智能体执行成功',
  agent_partial: '智能体部分成功',
  agent_failed: '智能体执行失败',
  agent_cancelled: '智能体已取消',
  agent_unknown: '智能体结果未知',
};

export const FIELD_LABELS = {
  title: '标题',
  description: '描述',
  notes: '备注',
  due_at: '截止时间',
  priority: '优先级',
  tags: '标签',
  category: '分类',
  parent_id: '父任务',
  status: '状态',
  position: '顺序',
  completed_at: '完成时间',
  archived_at: '归档时间',
  deleted_at: '删除时间',
  depends_on: '依赖',
  run: '运行',
  agent: '智能体',
  result: '结果',
  result_type: '结果类型',
  result_version: '结果版本',
  error: '错误',
};

export function actionLabel(action) {
  if (ACTION_LABELS[action]) return ACTION_LABELS[action];
  if (action && action.startsWith('undo_')) return '撤销' + (ACTION_LABELS[action.slice(5)] || action.slice(5));
  if (action && action.startsWith('conflict_')) return '冲突处理';
  return action || '';
}

// actorLabel names who made a change; agents record "agent/<name>" and
// model changes "llm" (FR-307, FR-511).
export function actorLabel(actor) {
  if (actor && actor.startsWith('agent/')) return `智能体 ${actor.slice(6)}`;
  if (actor && actor.startsWith('llm/')) return `大模型（${actor.slice(4) === 'auto' ? '自动执行' : '用户确认'}）`;
  return ACTOR_LABELS[actor] || actor || '未知';
}

// ---- view state (filters, search, sort) ----

export function defaultView() {
  return { status: '', q: '', priority: '', tag: '', category: '', due: '', sort: 'manual', reverse: false };
}

const VIEW_KEYS = ['status', 'q', 'priority', 'tag', 'category', 'due', 'sort'];

// parseView reads the view from the page URL's query string so a reload
// (or a bookmark) shows the same list.
export function parseView(search) {
  const p = new URLSearchParams(search || '');
  const v = defaultView();
  for (const k of VIEW_KEYS) {
    if (p.has(k)) v[k] = p.get(k) || '';
  }
  if (!STATUS_TABS.some((t) => t.value === v.status)) v.status = '';
  if (!SORTS.some((s) => s.value === v.sort)) v.sort = 'manual';
  if (v.priority && !PRIORITIES.includes(v.priority)) v.priority = '';
  if (!DUE_FILTERS.some((d) => d.value === v.due)) v.due = '';
  v.reverse = p.get('reverse') === '1';
  return v;
}

// serializeView is the inverse of parseView; defaults are left out.
export function serializeView(view, extra = {}) {
  const p = new URLSearchParams();
  const def = defaultView();
  for (const k of VIEW_KEYS) {
    if (view[k] && view[k] !== def[k]) p.set(k, view[k]);
  }
  if (view.reverse) p.set('reverse', '1');
  for (const [k, val] of Object.entries(extra)) {
    if (val) p.set(k, val);
  }
  const s = p.toString();
  return s ? '?' + s : '';
}

export function isFiltered(view) {
  const def = defaultView();
  return ['q', 'priority', 'tag', 'category', 'due'].some((k) => view[k] !== def[k]);
}

function startOfDay(d) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

function endOfDay(d) {
  const e = new Date(d);
  e.setHours(23, 59, 59, 0);
  return e;
}

// viewToQuery maps the view to GET /api/tasks parameters.
export function viewToQuery(view, now = new Date()) {
  const p = new URLSearchParams();
  const q = (view.q || '').trim();
  if (q) p.set('q', q);
  switch (view.status) {
    case '':
      break;
    case 'deleted':
      p.set('deleted', 'only');
      p.set('status', 'all');
      break;
    default:
      p.set('status', view.status);
  }
  if (view.priority) p.set('priority', view.priority);
  if (view.tag) p.set('tag', view.tag);
  if (view.category) p.set('category', view.category);
  switch (view.due) {
    case 'overdue':
      p.set('overdue', 'true');
      break;
    case 'today':
      p.set('due_after', startOfDay(now).toISOString());
      p.set('due_before', endOfDay(now).toISOString());
      break;
    case 'week': {
      const d = endOfDay(now);
      d.setDate(d.getDate() + 7);
      p.set('due_after', startOfDay(now).toISOString());
      p.set('due_before', d.toISOString());
      break;
    }
    case 'has':
      p.set('has_due', 'true');
      break;
    case 'none':
      p.set('has_due', 'false');
      break;
  }
  p.set('sort', view.sort || 'manual');
  if (view.reverse) p.set('reverse', 'true');
  return p;
}

// ---- dates ----

const pad = (n) => String(n).padStart(2, '0');

function sameDay(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

function dayDiff(d, now) {
  const a = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const b = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  return Math.round((a - b) / 86400000);
}

// formatDue renders a due time relative to now in local time:
// "今天 18:00", "明天", "10月9日 18:00", "2027年1月2日".
export function formatDue(iso, now = new Date()) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const endOfDayTime = d.getHours() === 23 && d.getMinutes() === 59;
  const time = endOfDayTime ? '' : ` ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const diff = dayDiff(d, now);
  let day;
  if (diff === 0) day = '今天';
  else if (diff === 1) day = '明天';
  else if (diff === -1) day = '昨天';
  else if (d.getFullYear() === now.getFullYear()) day = `${d.getMonth() + 1}月${d.getDate()}日`;
  else day = `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`;
  return day + time;
}

export function isOverdue(task, now = new Date()) {
  if (!task || !task.due_at) return false;
  if (task.status !== 'todo' && task.status !== 'in_progress') return false;
  return new Date(task.due_at) < now;
}

export function isDueToday(task, now = new Date()) {
  return !!(task && task.due_at && sameDay(new Date(task.due_at), now));
}

export function formatDateTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// toLocalInput converts an ISO time to a datetime-local input value.
export function toLocalInput(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// fromLocalInput converts a datetime-local value (browser local time) to
// RFC 3339 UTC; "" stays "" (meaning: no due time).
export function fromLocalInput(value) {
  if (!value) return '';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '';
  return d.toISOString();
}

const WEEKDAYS = {
  sun: 0, sunday: 0, '日': 0, '天': 0,
  mon: 1, monday: 1, '一': 1,
  tue: 2, tuesday: 2, '二': 2,
  wed: 3, wednesday: 3, '三': 3,
  thu: 4, thursday: 4, '四': 4,
  fri: 5, friday: 5, '五': 5,
  sat: 6, saturday: 6, '六': 6,
};

function addDays(d, n) {
  const out = new Date(d);
  out.setDate(out.getDate() + n);
  return out;
}

// parseWeekday mirrors the CLI: "fri"/"周五" is the next such day after
// today; "next fri"/"下周五" is that day of the following Monday-based week.
function parseWeekday(v, today) {
  let next = false;
  for (const p of ['next ', '下周', '下星期', '下礼拜']) {
    if (v.startsWith(p)) {
      v = v.slice(p.length);
      next = true;
      break;
    }
  }
  if (!next) {
    for (const p of ['周', '星期', '礼拜']) {
      if (v.startsWith(p)) v = v.slice(p.length);
    }
  }
  if (!(v in WEEKDAYS)) return null;
  const wd = WEEKDAYS[v];
  if (next) {
    const sinceMonday = (today.getDay() + 6) % 7;
    return addDays(today, -sinceMonday + 7 + ((wd + 6) % 7));
  }
  const days = (wd - today.getDay() + 7) % 7 || 7;
  return addDays(today, days);
}

// parseDueWord understands the same due shortcuts as `todo add --due`:
// today, tomorrow, 今天, 明天, 后天, +3d, +1w, +2h, weekdays (fri, 周五,
// next mon, 下周一), YYYY-MM-DD, YYYY/MM/DD and YYYY-MM-DD HH:MM, in local
// time. A date without a time is the end of that day. Returns an ISO
// string, or null when the input is not understood.
export function parseDueWord(input, now = new Date()) {
  const v = String(input || '').trim().toLowerCase();
  if (!v) return null;
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const date = (d) => endOfDay(d).toISOString();
  switch (v) {
    case 'today': case '今天': case '今日':
      return date(today);
    case 'tomorrow': case '明天': case '明日':
      return date(addDays(today, 1));
    case '后天':
      return date(addDays(today, 2));
  }
  let m = v.match(/^\+(\d+)([dwh])$/);
  if (m) {
    const n = Number(m[1]);
    if (m[2] === 'h') {
      const t = new Date(now.getTime() + n * 3600000);
      t.setSeconds(0, 0);
      return t.toISOString();
    }
    return date(addDays(today, m[2] === 'w' ? 7 * n : n));
  }
  const wd = parseWeekday(v, today);
  if (wd) return date(wd);
  m = v.match(/^(\d{4})[-/](\d{1,2})[-/](\d{1,2})(?:[ t](\d{1,2}):(\d{2}))?$/);
  if (m) {
    const [y, mo, d] = [Number(m[1]), Number(m[2]) - 1, Number(m[3])];
    const t = m[4] !== undefined ? new Date(y, mo, d, Number(m[4]), Number(m[5])) : endOfDay(new Date(y, mo, d));
    if (Number.isNaN(t.getTime()) || t.getMonth() !== mo || t.getDate() !== d) return null;
    return t.toISOString();
  }
  const iso = new Date(String(input).trim());
  if (/^\d{4}-\d{2}-\d{2}T/.test(String(input).trim()) && !Number.isNaN(iso.getTime())) return iso.toISOString();
  return null;
}

// ---- input parsing ----

// parseTags splits "a, b #c" into normalized tags (as the server does).
export function parseTags(input) {
  const out = [];
  for (const raw of String(input || '').split(/[,，\s]+/)) {
    const t = raw.trim().replace(/^#/, '');
    if (t && !out.includes(t)) out.push(t);
  }
  return out.sort();
}

// parseQuickAdd reads the TUI's quick-add syntax:
//   写周报 #work @office !high due:tomorrow
// Unknown !words and unparseable due: values stay in the title.
export function parseQuickAdd(text, now = new Date()) {
  const out = { title: '', tags: [], category: '', priority: '', due_at: '' };
  const words = [];
  for (const w of String(text || '').trim().split(/\s+/)) {
    if (!w) continue;
    if (w.length > 1 && (w[0] === '#' || w[0] === '＃')) {
      const tag = w.slice(1);
      if (!out.tags.includes(tag)) out.tags.push(tag);
      continue;
    }
    if (w.length > 1 && (w[0] === '@' || w[0] === '＠')) {
      out.category = w.slice(1);
      continue;
    }
    if (w.length > 1 && (w[0] === '!' || w[0] === '！')) {
      const p = normalizePriority(w.slice(1));
      if (p) {
        out.priority = p;
        continue;
      }
    }
    const due = w.match(/^(?:due|截止)[:：](.+)$/i);
    if (due) {
      const iso = parseDueWord(due[1], now);
      if (iso) {
        out.due_at = iso;
        continue;
      }
    }
    words.push(w);
  }
  out.title = words.join(' ');
  out.tags.sort();
  return out;
}

export function normalizePriority(s) {
  switch (String(s || '').trim().toLowerCase()) {
    case 'urgent': case 'u': case '4': case 'p4': case '紧急': return 'urgent';
    case 'high': case 'h': case '3': case 'p3': case '高': return 'high';
    case 'medium': case 'med': case 'm': case '2': case 'p2': case '中': return 'medium';
    case 'low': case 'l': case '1': case 'p1': case '低': return 'low';
    case 'none': case '0': case 'p0': case '无': return 'none';
  }
  return '';
}

// formPatch compares editor values with the task being edited and returns
// only the changed fields, so an edit never rewrites what it did not touch.
export function formPatch(task, values) {
  const patch = {};
  const str = (k) => {
    if ((values[k] ?? '') !== (task[k] ?? '')) patch[k] = values[k] ?? '';
  };
  str('title');
  str('description');
  str('notes');
  str('category');
  if ((values.parent_id ?? '') !== (task.parent_id ?? '')) patch.parent_id = values.parent_id ?? '';
  if ((values.priority || 'none') !== (task.priority || 'none')) patch.priority = values.priority || 'none';
  if ((values.status || task.status) !== task.status) patch.status = values.status;
  const tags = [...(values.tags || [])].sort();
  const cur = [...(task.tags || [])].sort();
  if (tags.join('\u0000') !== cur.join('\u0000')) patch.tags = tags;
  const due = values.due_at || '';
  const curDue = task.due_at || '';
  if (!due && curDue) patch.due_at = null;
  else if (due && (!curDue || new Date(due).getTime() !== new Date(curDue).getTime())) patch.due_at = due;
  return patch;
}

// ---- display helpers ----

export function shortId(id) {
  return (id || '').slice(0, 8);
}

function displayValue(field, v) {
  if (v === null || v === undefined || v === '') return '（空）';
  if (field === 'status') return STATUS_LABELS[v] || String(v);
  if (field === 'priority') return PRIORITY_LABELS[v] || String(v);
  if (field === 'due_at' || field.endsWith('_at')) return formatDateTime(v) || String(v);
  if (field === 'parent_id') return shortId(String(v));
  if (Array.isArray(v)) return v.length ? v.map((t) => '#' + t).join(' ') : '（空）';
  return String(v);
}

export function fieldValue(field, v) {
  return displayValue(field, v);
}

// describeChanges summarizes a history entry's field changes.
export function describeChanges(changes) {
  if (!changes) return [];
  return Object.keys(changes)
    .filter((f) => f !== 'position' || Object.keys(changes).length === 1)
    .sort()
    .map((f) => ({
      field: f,
      label: FIELD_LABELS[f] || f,
      from: displayValue(f, changes[f]?.from),
      to: displayValue(f, changes[f]?.to),
    }));
}

const ERROR_MESSAGES = {
  network_error: '无法连接本地服务，请确认 `todo serve` 仍在运行。',
  invalid_input: '输入无效',
  invalid_json: '请求格式错误',
  ambiguous_id: '任务编号不唯一，请输入更多字符',
  not_found: '任务不存在（可能已被删除）',
  version_conflict: '任务已在其他地方被修改',
  nothing_to_undo: '没有可以撤销的操作',
  conflict_resolved: '该冲突已经处理过了',
  task_deleted: '任务已被删除',
  version_required: '缺少任务版本号',
  forbidden_host: '仅允许本机访问',
  forbidden_origin: '拒绝跨站请求',
  body_too_large: '内容过长',
  internal_error: '本地服务内部错误，请查看服务日志',
  llm_unavailable: '大模型功能不可用',
  agents_unavailable: '智能体功能不可用',
};

// errorMessage turns an API error into a short Chinese message; the
// server's English detail is appended for invalid input.
export function errorMessage(err) {
  if (!err) return '';
  const code = err.code || '';
  const base = ERROR_MESSAGES[code];
  if (!base) return err.message || String(err);
  if (code === 'invalid_input' && err.message) {
    return `${base}：${err.message.replace(/^invalid_input:\s*/, '')}`;
  }
  return base;
}
