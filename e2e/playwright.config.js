// Browser tests for pgquire. `npm test` here builds the server, starts what it needs (setup.js) and
// runs the specs one after another: they share one server and one Postgres.
import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  globalSetup: './setup.js',
  workers: 1,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    browserName: 'chromium',
    viewport: { width: 1400, height: 900 },
    acceptDownloads: true,
    trace: 'retain-on-failure',
  },
});
