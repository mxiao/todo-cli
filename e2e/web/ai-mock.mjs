// Mock of the model, prompt-template and agent endpoints for the web UI
// tests (ai.spec.mjs). Task endpoints stay real: the page runs against a
// private `todo serve` (fixtures.mjs), and accepted model changes are
// applied through the real task API, so the list, the CLI and the history
// show them exactly as they would with a real model.
//
// The mock follows the response shapes of packages/server llm_routes.go and
// agent_routes.go (apps/web's Go tests check that every path the page calls
// exists on the real server). Tests drive agent runs with advance() and
// finish(), and inject errors with failNext().

const now = () => new Date().toISOString();

const AI_PATH = /^\/api\/(llm|prompts|agents|agent-runs)(\/|$)|^\/api\/tasks\/[^/]+\/(agent-runs|results)$/;

const MODE_LABELS = { suggest: '仅建议', confirm: '执行前确认', auto: '自动执行' };

const ROUTES = [
  'GET /llm/status',
  'POST /llm/mode',
  'POST /llm/use',
  'PUT /llm/confirm-list',
  'POST /llm/test',
  'GET /llm/sessions',
  'GET /llm/sessions/*',
  'GET /llm/actions',
  'POST /llm/intake',
  'POST /llm/sessions/*/answer',
  'POST /llm/decide',
  'POST /llm/sessions/*/redecide',
  'POST /llm/optimize',
  'POST /llm/sessions/*/apply',
  'POST /llm/sessions/*/reject',
  'POST /llm/sessions/*/undo',
  'PATCH /llm/sessions/*/items/*',
  'GET /prompts',
  'GET /prompts/*',
  'POST /prompts/summarize',
  'POST /prompts/*/render',
  'POST /prompts/*/task',
  'POST /prompts/*/copy',
  'GET /agents',
  'PUT /agents/*',
  'DELETE /agents/*',
  'POST /tasks/*/agent-runs',
  'GET /tasks/*/results',
  'GET /agent-runs',
  'GET /agent-runs/*',
  'GET /agent-runs/*/events',
  'GET /agent-runs/*/prompt',
  'POST /agent-runs/*/pause',
  'POST /agent-runs/*/resume',
  'POST /agent-runs/*/cancel',
  'POST /agent-runs/*/retry',
  'POST /agent-runs/*/confirm',
  'POST /agent-runs/*/reject',
];

// routeKey maps a request to its route template ('*' = an id).
function routeKey(method, segs) {
  for (const r of ROUTES) {
    const [rm, rp] = r.split(' ');
    const ps = rp.split('/').filter(Boolean);
    if (rm === method && ps.length === segs.length && ps.every((x, i) => x === '*' || x === segs[i])) return r;
  }
  return `${method} /${segs.join('/')}`;
}

function json(route, status, body) {
  return route.fulfill({ status, contentType: 'application/json; charset=utf-8', body: JSON.stringify(body) });
}

function apiError(code, message, extra = {}) {
  return { error: { code, message, ...extra } };
}

export async function mockAI(page, todo, opts = {}) {
  const profiles = [
    { profile: 'default', provider: 'openai', model: 'gpt-test', base_url: 'https://api.example.com/v1', status: 'ok', source: 'file', key_source: 'env', api_key_set: true },
    { profile: 'local', provider: 'ollama', model: 'qwen-local', base_url: 'http://127.0.0.1:11434/v1', status: 'ok', source: 'file', key_source: 'none', api_key_set: false },
  ];
  const m = {
    requests: [],
    failures: [],
    settings: { active: 'default', mode: opts.mode || 'confirm', confirm_list: [] },
    sessions: new Map(),
    agents: [
      { name: 'coder', adapter: 'cli', command: ['claude', '-p'], description: '代码智能体', input: 'prompt-stdin', output: 'text' },
      { name: 'linkcheck', adapter: 'cli', command: ['lychee', '.'], description: '检查链接' },
    ],
    adapters: ['cli', 'http', 'llm'],
    runs: new Map(),
    events: [],
    results: [],
    prompts: new Map(),
    seq: 0,
  };
  m.prompts.set('tpl_1', {
    id: 'tpl_1', name: '周报分析', original: '请帮我分析本周的项目周报，项目名称是 XX，重点看风险，并输出 markdown 表格……（原始长提示词）',
    current_version: 1, created_at: now(), updated_at: now(),
    latest: {
      version: 1, source: 'model', created_at: now(),
      summary: {
        goal: '分析项目周报', context: '本周项目进展', constraints: ['只引用周报中的事实'], steps: ['阅读周报', '列出风险'], output_format: 'markdown 表格',
        variables: [{ name: 'project', description: '项目名称' }, { name: 'week', description: '周次', default: '第 41 周' }],
      },
      body: '目标：分析 {{project}} 在 {{week}} 的周报\n约束：只引用周报中的事实\n输出：markdown 表格',
    },
  });

  const status = () => {
    const model = profiles.find((p) => p.profile === m.settings.active);
    return {
      model, available: true, mode: m.settings.mode, mode_label: MODE_LABELS[m.settings.mode], profiles,
      confirm_list: m.settings.confirm_list, default_confirm: ['delete', 'overwrite', 'send', 'commit', 'system_config'],
      send_fields: ['title'], decision_fields: ['due_at'], allow_commands: false, agents: m.agents.map((a) => a.name), assist_config_version: 1,
    };
  };

  // ---- task API helpers (real server) ----
  const realTasks = async () => (await todo.api('GET', '/api/tasks?sort=manual')).body.tasks;
  const realTask = async (id) => (await todo.api('GET', `/api/tasks/${id}`)).body.task;

  // ---- sessions ----
  const newSession = (kind, mode, extra) => {
    const s = { id: `ses_${++m.seq}`, kind, status: 'pending', mode: mode || m.settings.mode, items: [], asked: false, origin: 'web', model: 'gpt-test',
      created_at: now(), updated_at: now(), ...extra };
    m.sessions.set(s.id, s);
    return s;
  };
  const refresh = (s) => {
    const c = { pending: 0, applied: 0, rejected: 0 };
    for (const it of s.items) c[it.status] = (c[it.status] || 0) + 1;
    if (s.status === 'needs_input' || s.status === 'superseded' || s.status === 'undone') return;
    s.status = c.pending && c.applied ? 'partial' : c.pending ? 'pending' : c.applied ? 'applied' : c.rejected ? 'rejected' : 'applied';
    s.result = {
      created: s.items.filter((i) => i.status === 'applied' && i.kind === 'create').length,
      updated: s.items.filter((i) => i.status === 'applied' && i.kind !== 'create').length,
      not_executed: s.items.filter((i) => !['applied', 'failed'].includes(i.status)).length,
      failed: s.items.filter((i) => i.status === 'failed').length,
      messages: s.items.filter((i) => i.message).map((i) => `#${i.n} ${i.message}`),
    };
  };
  const intakeItems = (text) => {
    const parts = text.split(/[，,。；;]|再|最后/).map((x) => x.replace(/^(先|然后)/, '').trim()).filter(Boolean).slice(0, 3);
    const tomorrow = new Date(Date.now() + 86400000);
    tomorrow.setHours(18, 0, 0, 0);
    return parts.map((title, i) => ({
      n: i + 1, kind: 'create', status: 'pending', ref: `N${i + 1}`, needs_confirm: true, confirm_reason: '执行前确认模式',
      fields: { title, priority: i === 0 ? 'high' : 'medium', tags: ['发布'], ...(i === 0 ? { due: tomorrow.toISOString(), due_text: '明天 18:00' } : {}) },
      reason: '从描述中拆分',
    }));
  };
  const applyItem = async (s, it, by) => {
    if (it.kind === 'create') {
      const f = it.fields;
      const body = { title: f.title, priority: f.priority || 'none', tags: f.tags || [] };
      if (f.due) body.due_at = f.due;
      if (f.description) body.description = f.description;
      if (f.category) body.category = f.category;
      const res = await todo.api('POST', '/api/tasks', body);
      it.result_task_id = res.body.task.id;
      it.message = '已创建';
    } else if (it.kind === 'change') {
      const t = await realTask(it.task_id);
      const res = await todo.api('PATCH', `/api/tasks/${it.task_id}`, { [it.change.field]: it.change.to, version: t.version });
      if (res.status !== 200) {
        it.status = 'failed';
        it.message = res.body.error.message;
        return;
      }
      it.message = '已调整';
    }
    it.status = 'applied';
    it.applied_by = by;
    it.needs_confirm = false;
  };
  const undoItem = async (it) => {
    if (it.kind === 'create' && it.result_task_id) {
      await todo.api('DELETE', `/api/tasks/${it.result_task_id}`);
    } else if (it.kind === 'change') {
      const t = await realTask(it.task_id);
      await todo.api('PATCH', `/api/tasks/${it.task_id}`, { [it.change.field]: it.diff[0].before, version: t.version });
    }
    it.status = 'undone';
    it.message = '已撤销';
  };
  const decideSession = async (mode, selected, feedback) => {
    let tasks = (await realTasks()).filter((t) => t.status !== 'done');
    if (selected && selected.length) tasks = tasks.filter((t) => selected.includes(t.id));
    const s = newSession('decide', mode, { feedback, summary: `共 ${tasks.length} 个待办，建议先处理「${tasks[0] ? tasks[0].title : '—'}」。` });
    s.risks = tasks[0] ? [{ task_id: tasks[0].id, title: tasks[0].title, level: 'high', message: '截止时间临近' }] : [];
    s.recommendations = tasks.slice(0, 3).map((t, i) => ({ order: i + 1, task_id: t.id, title: t.title, reason: i === 0 ? '最紧急，阻塞其他任务' : '可以随后处理', focus: '先确认范围', estimate: '1 小时' }));
    s.items = tasks.slice(0, 2).map((t, i) => ({
      n: i + 1, kind: 'change', status: 'pending', task_id: t.id, task_title: t.title, needs_confirm: true, confirm_reason: '执行前确认模式',
      change: { field: 'priority', to: i === 0 ? 'urgent' : 'low' }, reason: i === 0 ? '截止时间最近' : '可以延后',
      diff: [{ field: 'priority', before: t.priority, after: i === 0 ? 'urgent' : 'low' }],
    }));
    if (s.mode === 'auto') for (const it of s.items) await applyItem(s, it, 'auto');
    refresh(s);
    return s;
  };

  // ---- runs ----
  const addEvent = (run, kind, message, extra = {}) => {
    const ev = { id: m.events.length + 1, run_id: run.id, attempt: run.attempt, kind, message, at: now(), ...extra };
    m.events.push(ev);
    return ev;
  };
  const runView = (r) => ({ ...r, results: m.results.filter((x) => x.run_id === r.id), duration_ms: r.duration_ms ?? 0 });

  // advance changes a run (status, stage, progress …) and logs events.
  m.advance = (rid, patch = {}, lines = []) => {
    const r = m.runs.get(rid);
    if (patch.status && patch.status !== r.status) addEvent(r, 'state', `状态：${patch.status}`);
    Object.assign(r, patch, { updated_at: now() });
    if (patch.stage || patch.progress) addEvent(r, 'progress', `${patch.stage || ''} ${patch.progress || 0}%`);
    for (const l of lines) addEvent(r, l.kind || 'output', l.message, l.stream ? { stream: l.stream } : {});
  };
  // finish ends a run successfully and writes back results (FR-508).
  m.finish = (rid, results = []) => {
    const r = m.runs.get(rid);
    for (const res of results) {
      m.results.push({ id: m.results.length + 1, task_id: r.task_id, run_id: r.id, attempt: r.attempt, version: 1, agent: r.agent, created_at: now(), hash: 'h', key: 'k', ...res });
      addEvent(r, 'result', `结果：${res.summary}`);
    }
    m.advance(rid, { status: 'succeeded', progress: 100, stage: '完成', ended_at: now(), duration_ms: 65_000 });
    const a = r.attempts[r.attempts.length - 1];
    Object.assign(a, { status: 'succeeded', ended_at: now(), duration_ms: 65_000, results: results.length });
  };
  m.lastRun = () => [...m.runs.values()].pop();

  const startRun = (taskId, body) => {
    const auto = !body.agent || body.agent === 'auto';
    const agent = auto ? 'coder' : body.agent;
    const spec = m.agents.find((a) => a.name === agent);
    if (!spec) return [400, apiError('invalid_input', `invalid_input: no agent "${agent}"`)];
    const prompt = `任务：${taskId}\n${body.context ? '上下文：' + body.context + '\n' : ''}完成后输出结果摘要；未实际完成时不要声称已完成。`;
    const run = {
      id: `run_${++m.seq}`, task_id: taskId, agent, adapter: spec.adapter, status: spec.confirm ? 'waiting_confirmation' : 'running', status_label: '', attempt: 1,
      initiator: 'web', selection: auto ? { by: 'llm', reason: '任务需要修改代码' } : { by: 'user' }, prompt, spec, options: { complete_on_success: !!body.complete_on_success },
      stage: spec.confirm ? '' : '准备', progress: spec.confirm ? 0 : 5, created_at: now(), started_at: spec.confirm ? undefined : now(), updated_at: now(), duration_ms: 0,
      attempts: spec.confirm ? [] : [{ attempt: 1, status: 'running', started_at: now(), duration_ms: 0, results: 0 }],
    };
    if (body.dry_run) return [200, { run }];
    m.runs.set(run.id, run);
    addEvent(run, 'state', `创建运行：智能体 ${agent}`);
    if (!spec.confirm) addEvent(run, 'system', `启动命令：${spec.command.join(' ')}`);
    return [201, { run }];
  };
  const control = (r, verb, body) => {
    const terminal = ['succeeded', 'partial', 'failed', 'cancelled', 'unknown'].includes(r.status);
    const bad = (msg) => [409, apiError('invalid_state', `invalid_state: ${msg}`)];
    switch (verb) {
      case 'pause':
        if (r.status !== 'running') return bad('not running');
        m.advance(r.id, { status: 'paused' });
        break;
      case 'resume':
        if (r.status !== 'paused') return bad('not paused');
        m.advance(r.id, { status: 'running' });
        break;
      case 'cancel':
        if (terminal) return bad('already ended');
        m.advance(r.id, { status: 'cancelled', error: '已被用户取消', error_kind: 'cancelled', ended_at: now(), duration_ms: 3000 });
        Object.assign(r.attempts[r.attempts.length - 1] || {}, { status: 'cancelled', error: '已被用户取消', ended_at: now(), duration_ms: 3000 });
        break;
      case 'retry':
        if (!['failed', 'partial', 'cancelled', 'unknown', 'waiting_retry'].includes(r.status)) return bad('cannot retry');
        r.attempt += 1;
        r.attempts.push({ attempt: r.attempt, status: 'running', started_at: now(), duration_ms: 0, results: 0 });
        m.advance(r.id, { status: 'running', error: '', error_kind: '', ended_at: undefined, started_at: now(), progress: 5, stage: '准备' },
          body.context ? [{ kind: 'system', message: `重试补充说明：${body.context}` }] : []);
        break;
      case 'confirm':
        if (r.status !== 'waiting_confirmation') return bad('not waiting');
        r.attempts.push({ attempt: 1, status: 'running', started_at: now(), duration_ms: 0, results: 0 });
        m.advance(r.id, { status: 'running', started_at: now(), stage: '准备', progress: 5 });
        break;
      case 'reject':
        if (r.status !== 'waiting_confirmation') return bad('not waiting');
        m.advance(r.id, { status: 'cancelled', error: '启动未被确认', error_kind: 'rejected', ended_at: now() });
        break;
    }
    return [200, { run: runView(r) }];
  };

  // failNext makes the next request matching "METHOD /path-regex" fail.
  m.failNext = (method, pathRe, status, body) => m.failures.push({ method, pathRe, status, body });

  async function handle(route) {
    const req = route.request();
    const url = new URL(req.url());
    const method = req.method();
    const path = url.pathname;
    let body = {};
    try {
      body = req.postData() ? JSON.parse(req.postData()) : {};
    } catch {
      body = {};
    }
    m.requests.push({ method, path, body, query: Object.fromEntries(url.searchParams) });
    const fi = m.failures.findIndex((f) => f.method === method && f.pathRe.test(path));
    if (fi >= 0) {
      const [f] = m.failures.splice(fi, 1);
      return json(route, f.status, f.body);
    }
    if (opts.unavailable) return json(route, 503, apiError('llm_unavailable', 'model features are not available: open llm.json: permission denied'));
    const seg = path.split('/').filter(Boolean); // ['api', ...]
    const key = routeKey(method, seg.slice(1));
    const sid = seg[3];
    const s = sid ? m.sessions.get(sid) : null;
    const reply = (st, b) => json(route, st, b);
    const sessionReply = (sess) => reply(200, { session: sess, revision: 0 });

    switch (key) {
      case 'GET /llm/status': return reply(200, status());
      case 'POST /llm/mode':
        if (!MODE_LABELS[body.mode]) return reply(400, apiError('invalid_input', 'unknown mode'));
        m.settings.mode = body.mode;
        return reply(200, status());
      case 'POST /llm/use': m.settings.active = body.profile; return reply(200, status());
      case 'PUT /llm/confirm-list': m.settings.confirm_list = body.entries; return reply(200, status());
      case 'POST /llm/test':
        return reply(200, opts.testFails
          ? { ok: false, model: status().model, latency_ms: 0, error: { code: 'llm_auth_failed', message: 'authentication failed (HTTP 401)', hint: '检查 OPENAI_API_KEY', retryable: false }, alternatives: ['local'] }
          : { ok: true, model: status().model, latency_ms: 120, reply: 'pong' });
      case 'GET /llm/sessions':
        return reply(200, { sessions: [...m.sessions.values()].reverse().map((x) => ({ id: x.id, kind: x.kind, status: x.status, mode: x.mode, input: x.input, summary: x.summary, items: x.items.length, pending: x.items.filter((i) => i.status === 'pending').length, created_at: x.created_at })) });
      case 'GET /llm/sessions/*': return s ? reply(200, s) : reply(404, apiError('not_found', 'no session'));
      case 'GET /llm/actions': return reply(200, { actions: [] });
      case 'POST /llm/intake': {
        const text = String(body.text || '');
        if (/缺/.test(text)) {
          const sess = newSession('intake', body.mode, { input: text, status: 'needs_input', asked: true, questions: ['这件事的截止时间是什么时候？'] });
          return sessionReply(sess);
        }
        const sess = newSession('intake', body.mode, { input: text, summary: '解析出以下任务' });
        sess.items = intakeItems(text);
        if (sess.mode === 'auto') for (const it of sess.items) await applyItem(sess, it, 'auto');
        refresh(sess);
        return sessionReply(sess);
      }
      case 'POST /llm/sessions/*/answer': {
        s.answers = body.answers;
        s.status = 'pending';
        s.items = intakeItems(`${s.input.replace(/缺/g, '')}`).map((it) => ({ ...it, fields: { ...it.fields, due_text: body.answers } }));
        refresh(s);
        return sessionReply(s);
      }
      case 'POST /llm/decide': return sessionReply(await decideSession(body.mode, body.selected, body.feedback));
      case 'POST /llm/sessions/*/redecide': {
        for (const it of s.items) if (it.status === 'pending') Object.assign(it, { status: 'rejected', message: '已重新决策' });
        const next = await decideSession(s.mode, [], body.feedback);
        next.supersedes = s.id;
        s.status = 'superseded';
        s.superseded_by = next.id;
        return sessionReply(next);
      }
      case 'POST /llm/optimize': {
        const sess = newSession('optimize', 'confirm', { summary: '近期 2 次运行因缺少文件路径失败' });
        sess.items = [{ n: 1, kind: 'optimize', status: 'pending', needs_confirm: true, confirm_reason: '配置修改需要确认',
          optimize: { target: 'routing', problem: '缺少文件路径', proposal: '先提取路径参数', expected_impact: '减少失败', verification: '重跑失败任务', rollback: '恢复上一版本' },
          diff: [{ field: 'routing', before: '[]', after: '[{"match":"路径","agent":"coder"}]' }] }];
        refresh(sess);
        return sessionReply(sess);
      }
      case 'POST /llm/sessions/*/apply':
        for (const it of s.items) if (it.status === 'pending' && (!body.items.length || body.items.includes(it.n))) await applyItem(s, it, 'user');
        refresh(s);
        return sessionReply(s);
      case 'POST /llm/sessions/*/reject':
        if (s.status === 'needs_input') s.status = 'rejected';
        for (const it of s.items) if (it.status === 'pending' && (!body.items.length || body.items.includes(it.n))) Object.assign(it, { status: 'rejected', message: '已拒绝' });
        refresh(s);
        return sessionReply(s);
      case 'POST /llm/sessions/*/undo':
        for (const it of s.items) if (it.status === 'applied' && (!body.items.length || body.items.includes(it.n))) await undoItem(it);
        refresh(s);
        return sessionReply(s);
      case 'PATCH /llm/sessions/*/items/*': {
        const it = s.items.find((x) => x.n === Number(seg[5]));
        if (body.title !== undefined && !body.title.trim()) return reply(400, apiError('invalid_input', 'invalid_input: the title cannot be empty'));
        for (const k of ['title', 'priority', 'tags', 'category', 'description']) if (body[k] !== undefined) it.fields[k] = body[k];
        if (body.due !== undefined) it.fields.due_text = body.due;
        if (body.to !== undefined) {
          it.change.to = body.to;
          it.diff = [{ ...it.diff[0], after: body.to }];
        }
        it.message = '已手动修改';
        return sessionReply(s);
      }

      case 'GET /prompts': return reply(200, { templates: [...m.prompts.values()].map(({ latest, ...t }) => t) });
      case 'GET /prompts/*': {
        const t = m.prompts.get(seg[2]);
        return t ? reply(200, t) : reply(404, apiError('not_found', 'no template'));
      }
      case 'POST /prompts/summarize': {
        const t = { id: `tpl_${++m.seq}`, name: body.name, original: body.text, current_version: 1, created_at: now(), updated_at: now(),
          latest: { version: 1, source: 'model', created_at: now(), summary: { goal: body.text.slice(0, 20), constraints: [], steps: [], variables: [] }, body: body.text } };
        m.prompts.set(t.id, t);
        return reply(200, { summary: t.latest.summary, body: t.latest.body, degraded: false, template: t });
      }
      case 'POST /prompts/*/render':
      case 'POST /prompts/*/task': {
        const t = m.prompts.get(seg[2]);
        const vars = t.latest.summary.variables;
        const missing = vars.filter((v) => !(body.values[v.name] || v.default)).map((v) => v.name);
        if (missing.length) return reply(400, apiError('missing_variables', `missing variables: ${missing.join(', ')}`, { variables: missing }));
        const text = t.latest.body.replace(/\{\{\s*(\w+)\s*\}\}/g, (_, n) => body.values[n] || vars.find((v) => v.name === n).default);
        if (seg[3] === 'render') return reply(200, { prompt: text });
        const res = await todo.api('POST', '/api/tasks', { title: body.title || `${t.name}：${body.values.project}`, description: text, tags: ['agent'] });
        return reply(201, { task: res.body.task, prompt: text, revision: 0 });
      }
      case 'POST /prompts/*/copy': {
        const t = m.prompts.get(seg[2]);
        const c = { ...t, id: `tpl_${++m.seq}`, name: body.name || `${t.name}-copy` };
        m.prompts.set(c.id, c);
        return reply(201, c);
      }

      case 'GET /agents': return reply(200, { agents: m.agents, adapters: m.adapters });
      case 'PUT /agents/*': {
        const spec = { ...body, name: decodeURIComponent(seg[2]) };
        if (!m.adapters.includes(spec.adapter || 'cli')) return reply(400, apiError('invalid_input', 'unknown adapter'));
        m.agents = [...m.agents.filter((a) => a.name !== spec.name), spec];
        return reply(200, { agent: spec });
      }
      case 'DELETE /agents/*':
        m.agents = m.agents.filter((a) => a.name !== decodeURIComponent(seg[2]));
        return reply(200, { removed: seg[2] });
      case 'POST /tasks/*/agent-runs': {
        const [st, b] = startRun(seg[2], body);
        return reply(st, b);
      }
      case 'GET /tasks/*/results': return reply(200, { results: m.results.filter((r) => r.task_id === seg[2]) });
      case 'GET /agent-runs': {
        let list = [...m.runs.values()].reverse();
        if (url.searchParams.get('task')) list = list.filter((r) => r.task_id === url.searchParams.get('task'));
        if (url.searchParams.get('status')) list = list.filter((r) => url.searchParams.get('status').split(',').includes(r.status));
        return reply(200, { runs: list.map(runView) });
      }
      case 'GET /agent-runs/*': {
        const r = m.runs.get(seg[2]);
        return r ? reply(200, { run: runView(r) }) : reply(404, apiError('not_found', 'no run'));
      }
      case 'GET /agent-runs/*/events': {
        const r = m.runs.get(seg[2]);
        const after = Number(url.searchParams.get('after') || 0);
        return reply(200, { events: m.events.filter((e) => e.run_id === r.id && e.id > after), status: r.status, done: false });
      }
      case 'GET /agent-runs/*/prompt': {
        const r = m.runs.get(seg[2]);
        return reply(200, { run_id: r.id, agent: r.agent, prompt: r.prompt });
      }
      case 'POST /agent-runs/*/pause':
      case 'POST /agent-runs/*/resume':
      case 'POST /agent-runs/*/cancel':
      case 'POST /agent-runs/*/retry':
      case 'POST /agent-runs/*/confirm':
      case 'POST /agent-runs/*/reject': {
        const [st, b] = control(m.runs.get(seg[2]), seg[3], body);
        return reply(st, b);
      }
    }
    return reply(404, apiError('not_found', `mock: no handler for ${key}`));
  }

  await page.route((url) => AI_PATH.test(url.pathname), handle);
  return m;
}
