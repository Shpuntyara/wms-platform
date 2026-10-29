import {defineConfig} from '@playwright/test';

if (!process.env.WMS_UI_LIVE_URL) throw new Error('WMS_UI_LIVE_URL must point to the disposable PostgreSQL fixture');

export default defineConfig({
  testDir: './tests',
  testMatch: 'live-workflow.spec.mjs',
  workers: 1,
  retries: 0,
  timeout: 90_000,
  expect: {timeout: 8000},
  reporter: [['list']],
  outputDir: process.env.WMS_UI_LIVE_ARTIFACTS || '../work/ui-live-browser',
  use: {
    baseURL: process.env.WMS_UI_LIVE_URL,
    browserName: 'chromium',
    viewport: {width: 1440, height: 1000},
    actionTimeout: 10_000,
    navigationTimeout: 15_000,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
});
