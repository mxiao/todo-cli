// Unit tests of the fixed mock model (node --test): it must answer every
// feature deterministically and behave like an OpenAI-compatible service.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { classify, startMockModel, localDay } from './mock-model.mjs';

async function chat(model, system, user, key = 'k') {
  const res = await fetch(`${model.baseURL}/chat/completions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` },
    body: JSON.stringify({ model: 'mock', messages: [{ role: 'system', content: system }, { role: 'user', content: user }] }),
  });
  const body = await res.json();
  return { status: res.status, content: body.choices?.[0]?.message?.content, body };
}

test('requests are classified by the response format in the system prompt', () => {
  assert.equal(classify('Connection test. Reply with the single word OK.'), 'test');
  assert.equal(classify('Return one JSON object only:\n{\n  "questions": ["..."]'), 'intake');
  assert.equal(classify('{"summary": "...", "recommendations": []}'), 'decide');
  assert.equal(classify('{"suggestions": [{'), 'optimize');
  assert.equal(classify('{"goal": "...", "context": "..."}'), 'summarize');
  assert.equal(classify('你为待办任务选择最合适的智能体。'), 'agent_select');
  assert.equal(classify('你是执行任务的智能体。'), 'agent');
});

test('fixed answers: intake split, one question, decisions and summaries', async (t) => {
  const model = await startMockModel({ apiKey: 'k' });
  t.after(() => model.close());
  const intake = 'Return one JSON object only: "questions"';

  const split = JSON.parse((await chat(model, intake, JSON.stringify({ input: '明天完成发布准备', tasks: [] }))).content);
  assert.deepEqual(split.items.map((i) => i.title), ['整理需求', '更新页面', '检查链接']);
  assert.equal(split.items[0].due, localDay(1));
  assert.deepEqual(split.items[2].depends_on, ['N2']);

  const ask = JSON.parse((await chat(model, intake, JSON.stringify({ input: '安排评审会' }))).content);
  assert.equal(ask.questions.length, 1);
  const answered = JSON.parse((await chat(model, intake, JSON.stringify({ input: '安排评审会', answers: '周五' }))).content);
  assert.equal(answered.questions, undefined);
  assert.equal(answered.items[0].due_text, '周五');

  const decide = JSON.parse((await chat(model, '"recommendations"', JSON.stringify({
    tasks: [{ ref: 'T1', title: '低', priority: 'low', status: 'todo' }, { ref: 'T2', title: '高', priority: 'high', status: 'todo', tags: ['agent'] }],
    agents: [{ name: 'coder', description: '写代码' }],
  }))).content);
  assert.deepEqual(decide.recommendations.map((r) => r.task), ['T2', 'T1']);
  assert.deepEqual(decide.changes, [{ task: 'T2', field: 'priority', to: 'urgent', reason: '它阻塞其他任务' }]);
  assert.deepEqual(decide.actions.map((a) => [a.agent, a.task]), [['coder', 'T2']]);

  const sum = JSON.parse((await chat(model, '"goal"', JSON.stringify({ prompt: '分析 {{p}}\n1. 读\n不要编造\n输出表格' }))).content);
  assert.deepEqual(sum, { goal: '分析 {{p}}', context: '', constraints: ['不要编造'], steps: ['读'], output_format: '输出表格', variables: [{ name: 'p', description: '每次填写的 p' }] });

  assert.deepEqual(model.requests.map((r) => r.kind), ['intake', 'intake', 'intake', 'decide', 'summarize']);
});

test('wrong key → 401, outage → 503', async (t) => {
  const model = await startMockModel({ apiKey: 'right' });
  t.after(() => model.close());
  assert.equal((await chat(model, 'Connection test', 'ping', 'wrong')).status, 401);
  assert.equal((await chat(model, 'Connection test', 'ping', 'right')).content, 'OK');
  model.setDown(true);
  assert.equal((await chat(model, 'Connection test', 'ping', 'right')).status, 503);
});
