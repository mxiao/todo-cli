// REST client for the local todo-cli service (packages/server). Every call
// returns the decoded JSON body or throws an ApiError carrying the server's
// error code, HTTP status and full body (409 bodies include the conflict).

export class ApiError extends Error {
  constructor(status, code, message, body) {
    super(message || code);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.body = body;
  }

  get isConflict() {
    return this.status === 409 && this.code === 'version_conflict';
  }
}

async function request(method, path, body) {
  const init = { method, headers: { Accept: 'application/json' }, cache: 'no-store', credentials: 'same-origin' };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, init);
  } catch (e) {
    throw new ApiError(0, 'network_error', e && e.message ? e.message : 'network error', null);
  }
  const text = await res.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const err = (data && data.error) || {};
    throw new ApiError(res.status, err.code || `http_${res.status}`, err.message || res.statusText, data);
  }
  return data;
}

const enc = encodeURIComponent;

export const api = {
  health: () => request('GET', '/api/health'),
  facets: () => request('GET', '/api/facets'),
  listTasks: (params) => request('GET', '/api/tasks?' + params.toString()),
  getTask: (id) => request('GET', `/api/tasks/${enc(id)}`),
  createTask: (fields) => request('POST', '/api/tasks', fields),
  // updateTask sends only the changed fields plus the version they were
  // based on; the server rejects stale versions with 409 + conflict.
  updateTask: (id, version, fields) => request('PATCH', `/api/tasks/${enc(id)}`, { ...fields, version }),
  setStatus: (id, status, version) => request('POST', `/api/tasks/${enc(id)}/status`, { status, version }),
  deleteTask: (id, version) => request('DELETE', `/api/tasks/${enc(id)}`, version ? { version } : undefined),
  restoreTask: (id, version) => request('POST', `/api/tasks/${enc(id)}/restore`, version ? { version } : {}),
  moveTask: (id, placement) => request('POST', `/api/tasks/${enc(id)}/move`, placement),
  // batch applies one action to many tasks atomically; items are
  // [{id, version}] so a task changed elsewhere fails the whole batch.
  batch: (action, items, extra = {}) => request('POST', '/api/batch', { action, items, ...extra }),
  getConflict: (cid) => request('GET', `/api/conflicts/${enc(cid)}`),
  undo: () => request('POST', '/api/undo', {}),
  resolveConflict: (cid, resolution, version) =>
    request('POST', `/api/conflicts/${enc(cid)}/resolve`, version ? { resolution, version } : { resolution }),
};

// Model, prompt-template and agent endpoints (packages/server llm_routes.go
// and agent_routes.go): the same API the CLI's `todo ai` / `todo agent`
// commands use. Keys are never sent or received here.
const items = (ns) => ({ items: ns || [] });

export const aiApi = {
  status: () => request('GET', '/api/llm/status'),
  test: (profile) => request('POST', '/api/llm/test', { profile: profile || '' }),
  use: (profile) => request('POST', '/api/llm/use', { profile }),
  setMode: (mode) => request('POST', '/api/llm/mode', { mode }),
  setConfirmList: (entries) => request('PUT', '/api/llm/confirm-list', { entries }),
  calls: (limit = 50) => request('GET', `/api/llm/calls?limit=${limit}`),
  actions: (limit = 50) => request('GET', `/api/llm/actions?limit=${limit}`),
  sessions: (kind = '', limit = 30) => request('GET', `/api/llm/sessions?kind=${enc(kind)}&limit=${limit}`),
  session: (sid) => request('GET', `/api/llm/sessions/${enc(sid)}`),
  intake: (body) => request('POST', '/api/llm/intake', body),
  decide: (body) => request('POST', '/api/llm/decide', body),
  optimize: () => request('POST', '/api/llm/optimize', {}),
  answer: (sid, answers) => request('POST', `/api/llm/sessions/${enc(sid)}/answer`, { answers }),
  redecide: (sid, feedback) => request('POST', `/api/llm/sessions/${enc(sid)}/redecide`, { feedback }),
  apply: (sid, ns) => request('POST', `/api/llm/sessions/${enc(sid)}/apply`, items(ns)),
  reject: (sid, ns) => request('POST', `/api/llm/sessions/${enc(sid)}/reject`, items(ns)),
  undo: (sid, ns) => request('POST', `/api/llm/sessions/${enc(sid)}/undo`, items(ns)),
  editItem: (sid, n, edit) => request('PATCH', `/api/llm/sessions/${enc(sid)}/items/${n}`, edit),

  prompts: () => request('GET', '/api/prompts'),
  prompt: (pid) => request('GET', `/api/prompts/${enc(pid)}`),
  promptVersions: (pid) => request('GET', `/api/prompts/${enc(pid)}/versions`),
  summarize: (body) => request('POST', '/api/prompts/summarize', body),
  renderPrompt: (pid, values, version) => request('POST', `/api/prompts/${enc(pid)}/render`, { values, version: version || 0 }),
  promptTask: (pid, values, title) => request('POST', `/api/prompts/${enc(pid)}/task`, { values, title: title || '' }),
  copyPrompt: (pid, name) => request('POST', `/api/prompts/${enc(pid)}/copy`, { name: name || '' }),
  rollbackPrompt: (pid, version) => request('POST', `/api/prompts/${enc(pid)}/rollback`, { version }),

  agents: () => request('GET', '/api/agents'),
  putAgent: (name, spec) => request('PUT', `/api/agents/${enc(name)}`, spec),
  deleteAgent: (name) => request('DELETE', `/api/agents/${enc(name)}`),
  startRun: (taskId, body) => request('POST', `/api/tasks/${enc(taskId)}/agent-runs`, body),
  taskResults: (taskId) => request('GET', `/api/tasks/${enc(taskId)}/results`),
  runs: (params) => request('GET', '/api/agent-runs?' + new URLSearchParams(params || {}).toString()),
  run: (rid) => request('GET', `/api/agent-runs/${enc(rid)}`),
  runEvents: (rid, after) => request('GET', `/api/agent-runs/${enc(rid)}/events?after=${after || 0}&limit=500`),
  runPrompt: (rid) => request('GET', `/api/agent-runs/${enc(rid)}/prompt`),
  pauseRun: (rid) => request('POST', `/api/agent-runs/${enc(rid)}/pause`, {}),
  resumeRun: (rid) => request('POST', `/api/agent-runs/${enc(rid)}/resume`, {}),
  cancelRun: (rid) => request('POST', `/api/agent-runs/${enc(rid)}/cancel`, {}),
  retryRun: (rid, context) => request('POST', `/api/agent-runs/${enc(rid)}/retry`, context ? { context } : {}),
  confirmRun: (rid) => request('POST', `/api/agent-runs/${enc(rid)}/confirm`, {}),
  rejectRun: (rid) => request('POST', `/api/agent-runs/${enc(rid)}/reject`, {}),
};
