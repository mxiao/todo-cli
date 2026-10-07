#!/usr/bin/env node
// Fixed mock agent for the acceptance suite, started by the generic
// command-line adapter exactly like a real agent: the todo-agent/v1 JSON
// input arrives on stdin, JSON Lines go to stdout. The first argument picks
// a scripted behaviour:
//
//   all     progress + log + command events, then the four write-back
//           types: text, file (a real file in the working directory),
//           command_output (a real `git` command) and commit (a real commit
//           in a fresh repository under the working directory)
//   fail    a non-zero exit with an error on stderr (dependency missing)
//   flaky   fails on the first attempt, succeeds on a retry
//   slow    keeps working until it is cancelled; writes one result first
//   secret  prints credentials; the product must redact them
import { spawnSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';

const mode = process.argv[2] || 'all';
const input = JSON.parse(readFileSync(0, 'utf8') || '{}');
const attempt = Number(process.env.TODO_AGENT_ATTEMPT || input.attempt || 1);
const emit = (o) => process.stdout.write(JSON.stringify(o) + '\n');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const title = input.task?.title || 'task';

function git(cwd, ...args) {
  const r = spawnSync('git', ['-c', 'user.name=Mock Agent', '-c', 'user.email=agent@example.invalid', '-c', 'commit.gpgsign=false', '-c', 'init.defaultBranch=main', ...args], { cwd, encoding: 'utf8' });
  if (r.status !== 0) throw new Error(`git ${args.join(' ')}: ${r.stderr}`);
  return r.stdout.trim();
}

switch (mode) {
  case 'all': {
    emit({ type: 'progress', stage: '准备', percent: 10, message: `收到任务：${title}` });
    emit({ type: 'log', message: `prompt bytes: ${Buffer.byteLength(input.prompt || '')}` });
    // text
    emit({ type: 'result', result_type: 'text', text: `「${title}」已完成。\n检查项全部通过。`, summary: '执行摘要' });
    // file
    writeFileSync('report.md', `# ${title}\n\n- 状态：完成\n- 特殊字符：<&> "quotes" {{var}}\n`);
    emit({ type: 'result', result_type: 'file', path: 'report.md', summary: '执行报告' });
    // commit: a real repository and commit
    const repo = path.resolve('repo');
    mkdirSync(repo, { recursive: true });
    git(repo, 'init', '-q');
    writeFileSync(path.join(repo, 'CHANGELOG.md'), `- ${title}\n`);
    git(repo, 'add', 'CHANGELOG.md');
    git(repo, 'commit', '-q', '-m', `docs: ${title}`);
    const hash = git(repo, 'rev-parse', 'HEAD');
    const branch = git(repo, 'rev-parse', '--abbrev-ref', 'HEAD');
    emit({ type: 'command', argv: ['git', 'commit', '-m', `docs: ${title}`], exit_code: 0 });
    emit({ type: 'result', result_type: 'commit', repo, branch, commit: hash, message: `docs: ${title}`, files: ['CHANGELOG.md'] });
    // command_output: a real command
    const log = spawnSync('git', ['log', '--oneline', '-1'], { cwd: repo, encoding: 'utf8' });
    emit({ type: 'result', result_type: 'command_output', argv: ['git', 'log', '--oneline', '-1'], exit_code: log.status, stdout: log.stdout.trim() });
    emit({ type: 'progress', stage: '完成', percent: 100 });
    emit({ type: 'status', status: 'succeeded', message: '四类结果已回写' });
    break;
  }
  case 'fail':
    console.log('starting');
    console.error('boom: missing dependency libfoo');
    process.exit(3);
    break;
  case 'flaky':
    if (attempt < 2) {
      console.error(`transient failure on attempt ${attempt}`);
      process.exit(1);
    }
    emit({ type: 'result', result_type: 'text', text: `第 ${attempt} 次尝试成功`, summary: '重试成功' });
    break;
  case 'slow': {
    emit({ type: 'result', result_type: 'text', text: '中间结果：已完成一半', summary: '中间结果' });
    for (let i = 0; i < 600; i++) {
      emit({ type: 'progress', stage: '处理中', percent: Math.min(90, i), message: `tick ${i}` });
      await sleep(100);
    }
    break;
  }
  case 'secret':
    console.log('api_key=sk-mockagentsecret1234567890abcd');
    console.log('Authorization: Bearer mockbearertoken0123456789abcdef');
    console.error('password=hunter2hunter2');
    emit({ type: 'result', result_type: 'text', text: 'used key sk-mockagentsecret1234567890abcd' });
    break;
  default:
    console.error(`unknown mode ${mode}`);
    process.exit(64);
}
