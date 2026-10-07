// Fixed mock of an OpenAI-compatible chat model for the acceptance suite.
//
// `todo serve` and the CLI talk to it over real HTTP (TODO_CLI_MODEL_BASE_URL),
// so the whole product path runs: prompt building, redaction, permission
// policy, sessions, history and audit. Answers are deterministic: each feature
// is recognised by the response format in its system prompt and answered from
// the fixed test set below, never from a real model (quality gate decision:
// fixed test set + mock responses, no external model in CI).
import http from 'node:http';

export const TZ = 'Asia/Shanghai';

// Local wall-clock "YYYY-MM-DD HH:MM" in TZ, n days from now.
export function localDay(days, time = '18:00') {
  const d = new Date(Date.now() + days * 86400000);
  const ymd = new Intl.DateTimeFormat('sv-SE', { timeZone: TZ, year: 'numeric', month: '2-digit', day: '2-digit' }).format(d);
  return `${ymd} ${time}`;
}

// Days from today until the next Friday (1..7) in TZ.
function daysToFriday() {
  const wd = new Intl.DateTimeFormat('en-US', { timeZone: TZ, weekday: 'short' }).format(new Date());
  const idx = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'].indexOf(wd);
  return ((5 - idx + 7) % 7) || 7;
}

const PRIORITY_RANK = { urgent: 4, high: 3, medium: 2, low: 1, none: 0 };

// ---- fixed test set: natural-language intake ----
// Each case: match(input) -> response. The order matters (first match wins).
export const INTAKE_CASES = [
  {
    id: 'NL-01 多条任务 + 依赖 + 相对截止时间',
    match: (u) => u.input.includes('发布准备'),
    respond: () => ({
      summary: '拆分为 3 条有先后顺序的发布准备任务',
      items: [
        { action: 'create', ref: 'N1', title: '整理需求', due: localDay(1), due_text: '明天下午前', priority: 'high', tags: ['发布'], reason: '第一步' },
        { action: 'create', ref: 'N2', title: '更新页面', tags: ['发布'], depends_on: ['N1'], reason: '依赖需求整理' },
        { action: 'create', ref: 'N3', title: '检查链接', tags: ['发布'], depends_on: ['N2'], reason: '交给命令行智能体检查' },
      ],
    }),
  },
  {
    id: 'NL-02 缺少截止时间 → 只追问一次',
    match: (u) => u.input.includes('评审会'),
    respond: (u) => (u.answers
      ? { summary: '已按回答补全截止时间', items: [{ action: 'create', ref: 'N1', title: '安排评审会', due: localDay(daysToFriday()), due_text: u.answers, priority: 'medium', reason: '用户补充了时间' }] }
      : { questions: ['评审会需要在什么时候之前安排好？'], items: [{ action: 'create', ref: 'N1', title: '安排评审会', reason: '缺少截止时间' }] }),
  },
  {
    id: 'NL-03 选中任务 → 修改已有任务',
    match: (u) => Array.isArray(u.selected) && u.selected.length > 0,
    respond: (u) => ({
      summary: '提高选中任务的优先级',
      items: [{ action: 'update', task: u.selected[0], priority: 'urgent', reason: '用户说这个很急' }],
    }),
  },
  {
    id: 'NL-04 单条任务（默认）',
    match: () => true,
    respond: (u) => ({ summary: '创建 1 条任务', items: [{ action: 'create', ref: 'N1', title: u.input.replace(/[。！!]+$/, '').trim(), reason: '用户描述的事项' }] }),
  },
];

function intake(user) {
  return INTAKE_CASES.find((c) => c.match(user)).respond(user);
}

// ---- decisions: rank by priority, then the given order ----
function decide(user) {
  const tasks = (user.tasks || []).filter((t) => t.status !== 'done' && t.status !== 'archived');
  const ranked = tasks
    .map((t, i) => ({ t, i }))
    .sort((a, b) => (PRIORITY_RANK[b.t.priority] || 0) - (PRIORITY_RANK[a.t.priority] || 0) || a.i - b.i)
    .map((x) => x.t);
  const top = ranked[0];
  const resp = {
    summary: top ? `共 ${tasks.length} 个待办，建议先处理「${top.title}」。` : '没有待办任务。',
    risks: top ? [{ task: top.ref, level: 'high', message: '优先级最高，影响后续任务' }] : [],
    recommendations: ranked.slice(0, 3).map((t, i) => ({
      task: t.ref, reason: i === 0 ? '优先级最高，先做' : '随后处理', focus: '先确认范围', estimate: i === 0 ? '1h' : '30m',
    })),
    changes: top && top.priority !== 'urgent' ? [{ task: top.ref, field: 'priority', to: 'urgent', reason: '它阻塞其他任务' }] : [],
    actions: [],
  };
  // An agent action for tasks tagged "agent" when the user registered agents.
  const agents = user.agents || [];
  const forAgent = ranked.find((t) => (t.tags || []).includes('agent'));
  if (forAgent && agents.length) {
    resp.actions.push({ type: 'agent', agent: agents[0].name || agents[0], task: forAgent.ref, reason: '交给智能体执行' });
  }
  return resp;
}

// ---- prompt summaries: structure by line rules, keep {{variables}} ----
function summarize(user) {
  const text = String(user.prompt || '');
  const lines = text.split('\n').map((l) => l.trim()).filter(Boolean);
  const vars = [...new Set([...text.matchAll(/\{\{\s*([\w一-鿿]+)\s*\}\}/g)].map((m) => m[1]))];
  const isStep = (l) => /^\d+[.、]/.test(l);
  const steps = lines.filter(isStep).map((l) => l.replace(/^\d+[.、]\s*/, ''));
  const constraints = lines.filter((l) => /必须|不要|不得|禁止/.test(l));
  const output = lines.find((l) => /输出|格式/.test(l)) || '';
  const rest = lines.filter((l) => !isStep(l) && !constraints.includes(l) && l !== output);
  return {
    goal: rest[0] || lines[0] || '',
    context: rest.slice(1).join('\n'),
    constraints,
    steps,
    output_format: output,
    variables: vars.map((name) => ({ name, description: `每次填写的 ${name}` })),
  };
}

function selectAgent(user) {
  const agents = user.agents || [];
  const text = `${user.task?.title || ''} ${user.task?.description || ''}`;
  const hit = agents.find((a) => a.description && text.includes(a.description)) || agents[0];
  return { agent: hit ? hit.name : '', reason: hit ? `任务「${user.task?.title}」适合智能体 ${hit.name}` : '没有合适的智能体' };
}

// classify maps a request to the feature that made it.
export function classify(system) {
  if (system.includes('Connection test')) return 'test';
  if (system.includes('"questions"')) return 'intake';
  if (system.includes('"recommendations"')) return 'decide';
  if (system.includes('"suggestions"')) return 'optimize';
  if (system.includes('"goal"')) return 'summarize';
  if (system.includes('选择最合适的智能体')) return 'agent_select';
  return 'agent';
}

function answer(kind, user) {
  switch (kind) {
    case 'test': return 'OK';
    case 'intake': return JSON.stringify(intake(JSON.parse(user)));
    case 'decide': return JSON.stringify(decide(JSON.parse(user)));
    case 'optimize': return JSON.stringify({ suggestions: [] });
    case 'summarize': return JSON.stringify(summarize(JSON.parse(user)));
    case 'agent_select': return JSON.stringify(selectAgent(JSON.parse(user)));
    default: return `已完成：${user.split('\n')[0].slice(0, 60)}`;
  }
}

// startMockModel listens on 127.0.0.1 and returns { baseURL, requests,
// setDown(bool), close() }. While "down" every call answers 503, so the
// product sees a real outage (llm_server_error).
export async function startMockModel({ apiKey } = {}) {
  const requests = [];
  let down = false;
  const server = http.createServer((req, res) => {
    let raw = '';
    req.on('data', (c) => { raw += c; });
    req.on('end', () => {
      const send = (status, body) => {
        res.writeHead(status, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify(body));
      };
      if (req.method !== 'POST' || !req.url.endsWith('/chat/completions')) return send(404, { error: { message: 'not found' } });
      let body;
      try {
        body = JSON.parse(raw);
      } catch {
        return send(400, { error: { message: 'invalid json' } });
      }
      const system = body.messages.find((m) => m.role === 'system')?.content || '';
      const user = body.messages.filter((m) => m.role === 'user').map((m) => m.content).join('\n');
      const kind = classify(system);
      requests.push({ kind, raw, authorization: req.headers.authorization || '' });
      if (down) return send(503, { error: { message: 'mock model is down' } });
      if (apiKey && req.headers.authorization !== `Bearer ${apiKey}`) return send(401, { error: { message: 'invalid api key' } });
      send(200, { model: body.model, choices: [{ index: 0, message: { role: 'assistant', content: answer(kind, user) }, finish_reason: 'stop' }] });
    });
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address();
  return {
    baseURL: `http://127.0.0.1:${port}/v1`,
    requests,
    setDown(v) { down = v; },
    close: () => new Promise((resolve) => { server.closeAllConnections?.(); server.close(() => resolve()); }),
  };
}
