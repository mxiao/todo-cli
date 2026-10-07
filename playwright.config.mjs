// Playwright suite for the web task manager (e2e/web/*.spec.mjs). Each test
// starts its own `todo serve` on a fresh data directory (see
// e2e/web/fixtures.mjs), so tests run in parallel and the CLI can be driven
// against the same SQLite database.
//
// Chromium is the default. TODO_E2E_BROWSERS=chromium,firefox,webkit adds
// the other engines (Firefox, Safari/WebKit) when they are installed.
import { defineConfig, devices } from '@playwright/test';

const engines = { chromium: 'Desktop Chrome', firefox: 'Desktop Firefox', webkit: 'Desktop Safari' };
const wanted = (process.env.TODO_E2E_BROWSERS || 'chromium').split(',').map((s) => s.trim()).filter(Boolean);

export default defineConfig({
  testDir: 'e2e/web',
  testMatch: /.*\.spec\.mjs$/,
  globalSetup: './e2e/web/global-setup.mjs',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  timeout: 30_000,
  expect: { timeout: 5_000 },
  use: { trace: 'retain-on-failure', locale: 'zh-CN', timezoneId: 'Asia/Shanghai' },
  projects: wanted.map((name) => ({ name, use: { ...devices[engines[name]] } })),
});
