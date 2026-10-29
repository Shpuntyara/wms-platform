import {defineConfig} from '@playwright/test';

if (!process.env.WMS_INSTALLATION_BROWSER_INPUT) {
  throw new Error('WMS_INSTALLATION_BROWSER_INPUT must reference the protected installation verification input');
}
if (!process.env.WMS_INSTALLATION_NSS_DIR || !process.env.WMS_INSTALLATION_PROFILE_ROOT) {
  throw new Error('Scoped NSS trust and private profile directories are required; global trust and TLS bypass are not used');
}

export default defineConfig({
  testDir: './tests',
  testMatch: 'installation-workflow.spec.mjs',
  workers: 1,
  retries: 0,
  timeout: 180_000,
  expect: {timeout: 15_000},
  reporter: [['list']],
  outputDir: process.env.WMS_INSTALLATION_BROWSER_OUTPUT || '../work/workplace-installed-browser',
  use: {
    browserName: 'firefox',
    viewport: {width: 1440, height: 1000},
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
    // This test handles real credentials. Never retain login forms or tokens.
    trace: 'off',
    screenshot: 'off',
    video: 'off',
    ignoreHTTPSErrors: false,
  },
});
