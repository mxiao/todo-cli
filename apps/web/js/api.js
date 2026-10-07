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
