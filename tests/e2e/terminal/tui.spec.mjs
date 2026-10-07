// Terminal acceptance: the interactive TUI on a pseudo-terminal with the
// Terminal.app and iTerm2 profiles — keyboard and mouse, the keyboard-only
// path without mouse reporting, UTF-8 Chinese text, no-colour mode and
// live sync with the local web service.
import { test, expect, measure } from '../support/fixtures.mjs';
import { PROFILES, KEY, click, wheelDown, wheelUp, openTerminal, ptyUnavailable } from '../support/pty.mjs';

const byTitle = async (app) => Object.fromEntries((await app.cli('list', '--status', 'all')).tasks.map((t) => [t.title, t]));

for (const profile of PROFILES) {
  test.describe(profile.name, () => {
    test.skip(!!ptyUnavailable, ptyUnavailable || '');
    test.beforeEach(() => {
      test.info().annotations.push({ type: 'terminal', description: profile.name });
    });

    test('keyboard and mouse drive the core task flow', { tag: ['@AT-13', '@AT-15'] }, async ({ app }) => {
      await app.cli('add', '第一项');
      await app.cli('add', '第二项');
      const s = openTerminal(app, profile, ['tui']);
      try {
        await s.waitFor(0, '第二项');
        expect(s.raw()).toContain('\x1b[?1049h'); // alternate screen
        expect(s.raw()).toContain('\x1b[?1000h\x1b[?1006h'); // SGR mouse reporting

        // Create with the keyboard (quick-add syntax, UTF-8 Chinese).
        await s.do('a', '新任务');
        await s.do('写周报 #work !high due:tomorrow\r', '已创建：写周报');

        // Mouse click selects row 1; the keyboard acts on the same selection.
        await s.send(click(0, 10, 3));
        await s.do('x', '已完成：第一项');
        await s.do('js', '已开始：第二项');

        // Right click → context menu at the pointer → click 删除 → confirm.
        await s.do(click(2, 12, 4), '打开详情');
        await s.do(click(0, 15, 13), '确定删除「第二项」');
        await s.do('y', '已删除：第二项');
        await s.do('u', '已撤销');

        // Wheel scrolling, search, help, command palette.
        await s.send(wheelDown(10, 5) + wheelUp(10, 5));
        await s.do('/写周\r', '搜索「写周」');
        await s.send(KEY.esc);
        await s.do('?', '快捷键帮助');
        await s.send(KEY.esc);
        await s.do(':', '命令面板');
        await s.do('sort priority\r', '排序：优先级');

        // Edit form: ctrl+u clears, ctrl+s saves.
        await s.do('ge', '编辑任务');
        await s.do(`${KEY.ctrlU}周报（改）${KEY.ctrlS}`, '已保存：周报（改）');

        await s.send('q');
        expect((await s.exit()).exitCode).toBe(0);
        expect(s.raw()).toContain('\x1b[?1000l'); // mouse reporting off again
        expect(s.raw().trimEnd().endsWith('\x1b[?1049l')).toBe(true); // main screen restored
      } finally {
        s.kill();
      }
      const tasks = await byTitle(app);
      expect(tasks['第一项'].status).toBe('done');
      expect(tasks['第二项']).toMatchObject({ status: 'in_progress', deleted_at: null });
      expect(tasks['周报（改）']).toMatchObject({ priority: 'high', tags: ['work'] });
      expect(tasks['周报（改）'].due_at).not.toBeNull();
      const hist = (await app.cli('history', tasks['第一项'].id)).history;
      expect(hist.at(-1)).toMatchObject({ action: 'complete', actor: 'tui' });
    });

    test('keyboard-only path without mouse reporting and without colours', { tag: ['@AT-13', '@AT-15'] }, async ({ app }) => {
      await app.cli('add', '仅键盘');
      const s = openTerminal(app, profile, ['tui', '--no-mouse'], { cols: 80, rows: 24, env: { NO_COLOR: '1' } });
      try {
        await s.waitFor(0, '仅键盘');
        expect(s.raw()).not.toContain('\x1b[?1000h');
        // No foreground/background colour sequences in NO_COLOR mode.
        expect(s.raw()).not.toMatch(/\x1b\[(?:[0-9;]*;)?(?:3[0-79]|4[0-79]|9[0-7]|10[0-7]|38;[25]|48;[25])(?:;[0-9;]*)?m/);
        await s.do(KEY.enter, '详情 · tab'); // narrow terminal: one pane at a time
        await s.do('x', '已完成：仅键盘');
        await s.send(KEY.tab);
        await s.do('m', '重新打开'); // keyboard equivalent of the right-click menu
        await s.do('x', '已重新打开：仅键盘');
        await s.do('f', '仅显示逾期');
        await s.do('2', '筛选：待办');
        await s.do('c', '已清除搜索与筛选');
        await s.send(KEY.ctrlC);
        expect((await s.exit()).exitCode).toBe(0);
      } finally {
        s.kill();
      }
      expect((await byTitle(app))['仅键盘'].status).toBe('todo');
    });

    test('the TUI and the web service see each other\'s changes within 2 seconds', { tag: ['@AT-06', '@AT-15'] }, async ({ app }) => {
      const s = openTerminal(app, profile, ['tui']);
      try {
        await s.waitFor(0, '还没有任务'); // the empty list is drawn
        // Web (REST API) → TUI
        const m = s.mark();
        const created = await app.api('POST', '/api/tasks', { title: '网页创建的任务' });
        expect(created.status).toBe(201);
        const toTui = await s.waitFor(m, '网页创建的任务', 2000);
        test.info().annotations.push({ type: 'latency', description: `网页创建 → TUI: ${toTui} ms` });

        // TUI → web
        await s.do('a', '新任务');
        await s.do('终端创建的任务\r', '已创建：终端创建的任务');
        const toWeb = await measure(async () => (await app.api('GET', '/api/tasks?q=终端创建')).body.tasks.length === 1, 2000, 'TUI create via API');
        test.info().annotations.push({ type: 'latency', description: `TUI 创建 → 网页: ${toWeb} ms` });
        await s.send('q');
        await s.exit();
      } finally {
        s.kill();
      }
      const hist = (await app.cli('history', (await byTitle(app))['终端创建的任务'].id)).history;
      expect(hist[0]).toMatchObject({ action: 'create', actor: 'tui' });
    });
  });
}
