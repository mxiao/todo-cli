// Playwright fixtures of the acceptance suite.
//
//   test     `app`: mock model + real `todo serve` + CLI on a private data
//            directory (harness.mjs); used by the system and terminal specs.
//   webTest  `test` plus the browser name and version recorded as an
//            annotation, so the traceability report shows what really ran.
import { test as base, expect } from '@playwright/test';
import { createApp } from './harness.mjs';

export const test = base.extend({
  // eslint-disable-next-line no-empty-pattern
  app: async ({}, use) => {
    const app = await createApp();
    await use(app);
    await app.close();
  },
});

export const webTest = test.extend({
  browserInfo: [async ({ browser, browserName }, use, testInfo) => {
    testInfo.annotations.push({ type: 'browser', description: `${browserName} ${browser.version()}` });
    await use();
  }, { auto: true }],
});

export { expect };

// openApp loads the page and waits for the first list load and the live
// event stream.
export async function openApp(page, url) {
  await page.goto(url);
  await expect(page.locator('body[data-ready="true"]')).toBeAttached();
  await expect(page.getByTestId('live-status')).toHaveAttribute('data-state', 'live');
}

export const rows = (page) => page.getByTestId('task-row');
export const row = (page, title) => page.getByTestId('task-row').filter({ has: page.getByTestId('task-title').getByText(title, { exact: true }) });

export async function openAI(page, tab) {
  if (!(await page.getByTestId('ai-panel').isVisible())) await page.getByTestId('ai-open').click();
  await expect(page.getByTestId('ai-panel')).toBeVisible();
  await page.getByTestId(`ai-tab-btn-${tab}`).click();
}

// measure resolves to the milliseconds until fn() is truthy (polling every
// 25 ms) or throws after the limit.
export async function measure(fn, limit, what) {
  const start = Date.now();
  for (;;) {
    if (await fn()) return Date.now() - start;
    if (Date.now() - start > limit) throw new Error(`${what} not visible within ${limit} ms`);
    await new Promise((r) => setTimeout(r, 25));
  }
}
