import {defineConfig} from '@playwright/test';
export default defineConfig({
  testDir: './tests', testMatch: ['workplace.spec.mjs', 'critic-workplace.spec.mjs'], fullyParallel: true,
  timeout: 30000, forbidOnly: !!process.env.CI, retries: 0,
  reporter: [['list'], ['json', {outputFile: 'work/browser-results.json'}]],
  use: {baseURL: 'http://127.0.0.1:18744', trace: 'retain-on-failure'},
  webServer: {command: 'node serve.mjs --port 18744', url: 'http://127.0.0.1:18744/app/', reuseExistingServer: !process.env.CI}
});
