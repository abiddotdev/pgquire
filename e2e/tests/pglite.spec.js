// The in-browser database (PGlite), in the app the server serves and in the GitHub Pages copy.
// PGlite itself comes from a CDN, so these need the network.
import { test, expect } from '@playwright/test';
import { bootDone, env, openApp, runSQL, watchErrors } from './helpers.js';

async function sampleAndQuery(page) {
  await page.locator('#pane').getByText('Load the sample', { exact: true }).click();
  await expect(page.locator('#index')).toContainText('authors', { timeout: 30_000 });
  const r = await runSQL(page, 'select count(*) from books');
  expect(Number(r.rows[0][0])).toBeGreaterThan(0);
}

test('the app: sample data in the browser database', async ({ page }) => {
  const errors = watchErrors(page);
  await openApp(page);
  await expect(page.locator('#dbWhere')).toHaveText(/in this browser/i);
  await sampleAndQuery(page);
  expect(errors).toEqual([]);
});

test('the GitHub Pages copy: no server, PGlite only', async ({ page }) => {
  const errors = watchErrors(page);
  await page.goto(env('PAGES') + '/index.html');
  await bootDone(page);
  await sampleAndQuery(page);
  await page.click('#dbSwitch');
  await expect(page.locator('.menu')).toContainText('needs the pgquire app'); // remote is offered, but explained
  expect(errors).toEqual([]);
});
