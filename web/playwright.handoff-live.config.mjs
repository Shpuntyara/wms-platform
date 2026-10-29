import {defineConfig} from '@playwright/test';
if(!process.env.WMS_HANDOFF_LIVE_URL) throw new Error('WMS_HANDOFF_LIVE_URL must identify the disposable real PostgreSQL fixture');
export default defineConfig({
  testDir:'./tests',testMatch:'handoff-live-workflow.spec.mjs',workers:1,retries:0,timeout:110000,
  expect:{timeout:8000},reporter:[['list']],
  outputDir:process.env.WMS_HANDOFF_LIVE_ARTIFACTS||'../work/handoff-live-browser',
  use:{baseURL:process.env.WMS_HANDOFF_LIVE_URL,browserName:'chromium',viewport:{width:1440,height:1000},actionTimeout:10000,navigationTimeout:15000,trace:'retain-on-failure',screenshot:'only-on-failure'},
});
