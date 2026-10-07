// `npm run test:e2e`: end-to-end acceptance and compatibility suite.
//
// Projects
//   system                 tests/e2e/system   CLI + REST API + data safety (no browser)
//   terminal               tests/e2e/terminal TUI on node-pty, Terminal.app and iTerm2 profiles
//   chromium/firefox/webkit tests/e2e/browser web acceptance on all three engines
//   chrome, msedge, …      the same on branded browsers installed on this Mac,
//                          opt-in: TODO_E2E_CHANNELS=chrome,msedge
//   regression-<browser>   the web UI suite e2e/web (Chromium by default;
//                          TODO_E2E_BROWSERS=chromium,firefox,webkit for all)
//
// Every project runs against real `todo serve` / CLI processes on private
// data directories, with the fixed mock model and mock agents of tests/e2e.
// Reports: reports/e2e/{traceability.md,traceability.json,junit.xml,results.json,html/}.
import { defineConfig, devices } from '@playwright/test';

const engines = { chromium: 'Desktop Chrome', firefox: 'Desktop Firefox', webkit: 'Desktop Safari' };
const list = (v, d) => (v ?? d).split(',').map((s) => s.trim()).filter(Boolean);
const regression = list(process.env.TODO_E2E_BROWSERS, 'chromium');
const channels = list(process.env.TODO_E2E_CHANNELS, '');
const browserUse = { locale: 'zh-CN', timezoneId: 'Asia/Shanghai' };

export default defineConfig({
  globalSetup: './tests/e2e/support/global-setup.mjs',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  timeout: 45_000,
  expect: { timeout: 5_000 },
  outputDir: 'test-results',
  reporter: [
    ['list'],
    ['html', { outputFolder: 'reports/e2e/html', open: 'never' }],
    ['junit', { outputFile: 'reports/e2e/junit.xml' }],
    ['json', { outputFile: 'reports/e2e/results.json' }],
    ['./tests/e2e/traceability/reporter.mjs', { outputDir: 'reports/e2e' }],
  ],
  use: { trace: 'retain-on-failure', ...browserUse },
  projects: [
    { name: 'system', testDir: 'tests/e2e/system', testMatch: /.*\.spec\.mjs$/ },
    { name: 'terminal', testDir: 'tests/e2e/terminal', testMatch: /.*\.spec\.mjs$/ },
    ...Object.entries(engines).map(([name, device]) => ({
      name, testDir: 'tests/e2e/browser', testMatch: /.*\.spec\.mjs$/, use: { ...devices[device], ...browserUse },
    })),
    ...channels.map((channel) => ({
      name: channel, testDir: 'tests/e2e/browser', testMatch: /.*\.spec\.mjs$/, use: { ...devices['Desktop Chrome'], ...browserUse, channel },
    })),
    ...regression.map((name) => ({
      name: `regression-${name}`, testDir: 'e2e/web', testMatch: /.*\.spec\.mjs$/, use: { ...devices[engines[name]], ...browserUse },
    })),
  ],
});
