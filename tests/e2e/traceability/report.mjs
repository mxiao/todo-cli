// Pure functions that turn test records into the traceability report
// (unit tested in report.test.mjs; wired into Playwright by reporter.mjs).
//
// A record is { title, file, project, tags: ['@AT-01'], annotations:
// [{type, description}], outcome: 'passed'|'failed'|'flaky'|'skipped',
// durationMs, error, regression? }. Regression records come from the
// pre-existing web UI suite (e2e/web) and are counted, not traced.
import { ACCEPTANCE, DEFERRED, MATRIX, QUALITY_GATE } from './requirements.mjs';

const tagIds = (r) => r.tags.map((t) => t.replace(/^@/, ''));
const annotation = (r, type) => r.annotations.find((a) => a.type === type)?.description;

// status of a group of records: failed > passed > not run.
function groupStatus(records) {
  const ran = records.filter((r) => r.outcome !== 'skipped');
  if (!ran.length) return 'not_run';
  if (ran.some((r) => r.outcome === 'failed')) return 'failed';
  return 'passed';
}

export function buildReport(records, meta = {}) {
  const items = ACCEPTANCE.map((a) => {
    const tests = records.filter((r) => tagIds(r).includes(a.id));
    return {
      ...a,
      status: groupStatus(tests),
      tests: tests.map((r) => ({ title: r.title, file: r.file, project: r.project, outcome: r.outcome, durationMs: r.durationMs,
        browser: annotation(r, 'browser') || null, terminal: annotation(r, 'terminal') || null, error: r.error || null,
        latency: r.annotations.filter((x) => x.type === 'latency').map((x) => x.description) })),
    };
  });
  const matrix = MATRIX.map((m) => {
    let tests = records.filter((r) => m.projects.includes(r.project));
    if (m.profile) tests = tests.filter((r) => annotation(r, 'terminal') === m.profile);
    const versions = [...new Set(tests.map((r) => annotation(r, 'browser')).filter(Boolean))];
    let status = m.kind === 'os' ? (meta.os ? 'recorded' : 'not_run') : groupStatus(tests);
    // A branded browser that did not run is represented by its engine.
    let via = null;
    let count = tests.length;
    if (status === 'not_run' && m.engine) {
      const engineProject = { Chromium: 'chromium', WebKit: 'webkit', Gecko: 'firefox' }[m.engine];
      const engineTests = records.filter((r) => r.project === engineProject);
      if (engineTests.length) {
        status = `engine_${groupStatus(engineTests)}`;
        via = engineProject;
        count = engineTests.length;
        versions.push(...new Set(engineTests.map((r) => annotation(r, 'browser')).filter(Boolean)));
      }
    }
    return { target: m.target, kind: m.kind, status, via, versions: [...new Set(versions)], tests: count,
      detail: m.kind === 'os' ? meta.os || null : null, note: m.note };
  });
  const total = (o) => records.filter((r) => r.outcome === o).length;
  const summary = { tests: records.length, passed: total('passed'), failed: total('failed'), flaky: total('flaky'), skipped: total('skipped'),
    itemsPassed: items.filter((i) => i.status === 'passed').length, itemsTotal: items.length,
    regression: records.filter((r) => r.regression).length };
  const untraced = records.filter((r) => !r.regression && !tagIds(r).some((id) => ACCEPTANCE.some((a) => a.id === id))).map((r) => `${r.project} › ${r.title}`);
  return { meta, summary, qualityGate: QUALITY_GATE, items, deferred: DEFERRED, matrix, untraced };
}

const STATUS_TEXT = {
  passed: '✅ 通过', failed: '❌ 失败', not_run: '⏸ 未执行', recorded: 'ℹ️ 已记录',
  engine_passed: '✅ 通过（同引擎）', engine_failed: '❌ 失败（同引擎）', engine_not_run: '⏸ 未执行',
};
const OUTCOME_TEXT = { passed: '通过', failed: '失败', flaky: '重试后通过', skipped: '跳过' };

const cell = (s) => String(s ?? '').replace(/\|/g, '\\|').replace(/\n+/g, ' ');

export function renderMarkdown(rep) {
  const m = rep.meta;
  const out = [];
  out.push('# 端到端验收测试报告', '');
  out.push(`- 运行时间：${m.startedAt || '—'}（耗时 ${m.durationMs != null ? (m.durationMs / 1000).toFixed(1) + ' s' : '—'}）`);
  out.push(`- 代码版本：${m.commit || '—'}${m.branch ? `（${m.branch}）` : ''}`);
  out.push(`- 运行环境：${m.os || '—'} · Node ${m.node || '—'} · Playwright ${m.playwright || '—'}`);
  out.push(`- 执行项目：${(m.projects || []).join('、') || '—'}`);
  out.push(`- 结果：${rep.summary.tests} 个测试，通过 ${rep.summary.passed}，失败 ${rep.summary.failed}，重试后通过 ${rep.summary.flaky}，跳过 ${rep.summary.skipped}（其中回归测试 ${rep.summary.regression} 个）；验收项 ${rep.summary.itemsPassed}/${rep.summary.itemsTotal} 通过`);
  out.push(`- 质量门槛：${rep.qualityGate}`, '');

  out.push('## 验收项追溯', '', '| 编号 | 验收项 | 需求 | 状态 | 测试 |', '|---|---|---|---|---|');
  for (const it of rep.items) {
    const byTitle = new Map();
    for (const t of it.tests) {
      const k = `${t.file} › ${t.title}`;
      if (!byTitle.has(k)) byTitle.set(k, []);
      byTitle.get(k).push(`${t.project}${t.terminal ? `/${t.terminal}` : ''}:${OUTCOME_TEXT[t.outcome] || t.outcome}`);
    }
    const tests = [...byTitle].map(([k, v]) => `${k}（${v.join('，')}）`).join('<br>') || '—';
    out.push(`| ${it.id} | ${cell(it.title)} | ${cell(it.refs.join('、'))} | ${STATUS_TEXT[it.status]} | ${cell(tests)} |`);
  }
  // One line per measurement, even when its test proves several items.
  const latencies = rep.items.flatMap((i) => i.tests.flatMap((t) => t.latency.map((l) => `${t.project}${t.terminal ? `/${t.terminal}` : ''} · ${l}`)));
  if (latencies.length) {
    out.push('', '## 同步延迟实测（上限 2000 ms）', '');
    for (const l of [...new Set(latencies)]) out.push(`- ${l}`);
  }
  out.push('', '## 延期项', '', '| 编号 | 内容 | 决策 |', '|---|---|---|');
  for (const d of rep.deferred) out.push(`| ${d.id} | ${cell(d.title)} | 延期：${cell(d.decision)} |`);

  out.push('', '## 兼容性矩阵', '', '| 目标 | 状态 | 实际版本 / 环境 | 测试数 | 说明 |', '|---|---|---|---|---|');
  for (const x of rep.matrix) {
    const env = x.detail || x.versions.join('、') || '—';
    out.push(`| ${cell(x.target)} | ${STATUS_TEXT[x.status] || x.status}${x.via ? `（${x.via}）` : ''} | ${cell(env)} | ${x.tests} | ${cell(x.note)} |`);
  }
  const failed = rep.items.flatMap((i) => i.tests.filter((t) => t.outcome === 'failed').map((t) => ({ ...t, id: i.id })));
  if (failed.length) {
    out.push('', '## 失败详情', '');
    for (const f of failed) out.push(`- ${f.id} · ${f.project} › ${f.title}：${cell(f.error || '').slice(0, 500)}`);
  }
  if (rep.untraced.length) {
    out.push('', '## 未关联验收项的测试', '');
    for (const t of [...new Set(rep.untraced)]) out.push(`- ${t}`);
  }
  out.push('');
  return out.join('\n');
}
