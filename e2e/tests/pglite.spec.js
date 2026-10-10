// The in-browser database (PGlite), in the app the server serves and in the GitHub Pages copy.
// PGlite itself comes from a CDN, so these need the network.
import { readFileSync } from 'node:fs';
import path from 'node:path';
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

// The sample databases on the website (docs/sample/<name>/index.html) are share links: a future version must
// still open them, with their tables and saved queries. bikes is the big one, a 1.7 MB link.
const SAMPLES = { crm: ['companies', 'Pipeline by stage'], birds: ['sightings', 'Search, typos and all: robbin'], bikes: ['trips', 'Busiest stations'] };
for (const [name, [table, query]] of Object.entries(SAMPLES)) {
  test(`the ${name} sample link still opens`, async ({ page }) => {
    test.setTimeout(180_000);
    const errors = watchErrors(page);
    const html = readFileSync(path.join(import.meta.dirname, '../../docs/sample', name, 'index.html'), 'utf8');
    const hash = html.match(/href="(?:\.\.\/)+(#pgquire1\.[^"]+)"/)[1];
    await page.goto(env('PAGES') + '/index.html' + hash);
    await bootDone(page);
    await page.locator('.modal', { hasText: 'Open session' }).getByRole('button', { name: 'Open' }).click({ timeout: 60_000 });
    await expect(page.locator('#index').getByText(table, { exact: true })).toBeVisible({ timeout: 120_000 });
    await expect(page.locator('#index')).toContainText(query);
    expect(errors).toEqual([]);
  });
}
