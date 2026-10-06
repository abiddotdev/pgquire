import { expect } from '@playwright/test';
import { DB, query, withDb } from '../setup.js';

export { DB, ROLE } from '../setup.js';
export const env = name => process.env['E2E_' + name];

/** A connection string for a database on the test Postgres (the e2e_shop one by default). */
export const dsn = (db = DB) => withDb(env('PG'), db);
/** Runs SQL on the test Postgres directly, to check what pgquire did. */
export const admin = (sql, params, db = DB) => query(dsn(db), sql, params);

/** Opens pgquire through its token link and waits for the boot screen to go. */
export async function openApp(page, url = `${env('URL')}/?t=${env('TOKEN')}`) {
  await page.goto(url);
  await bootDone(page);
}

// The boot screen fades out (opacity 0) rather than being hidden, so wait for its class.
export const bootDone = page => page.waitForSelector('#boot.gone', { state: 'attached', timeout: 90_000 });

/** The visible text of a page element; '' when it isn't there. */
export const text = (page, sel) => page.locator(sel).first().innerText({ timeout: 1000 }).catch(() => '');

/** Runs SQL in a new query tab and returns what the results area shows. */
export async function runSQL(page, sql) {
  await page.click('#newQueryBtn');
  const editor = page.locator('#pane textarea').first();
  await editor.fill(sql);
  await editor.press('Control+Enter');
  await expect(page.locator('#pane .results')).not.toContainText('Running…', { timeout: 30_000 });
  const rows = await page.$$eval('#pane .results table.grid tbody tr', trs =>
    trs.slice(0, 5).map(tr => [...tr.querySelectorAll('td[data-c]')].map(td => td.textContent)));
  return { meta: await text(page, '#pane .run-meta'), error: await text(page, '#pane .pane-error'), rows };
}

/** Adds a remote connection through the dialog. With pick, the database picker that follows gets
 *  "Add" (the ticked ones) instead of "Just this one". */
export async function connect(page, name, connString, { readOnly = false, save = false, pick = false } = {}) {
  await page.click('#dbSwitch');
  await page.getByText('Connect remote Postgres…').click();
  const dlg = page.locator('.modal').last();
  await dlg.getByPlaceholder('e.g. staging').fill(name);
  await dlg.locator('input[placeholder^="postgres://"]').fill(connString);
  if (readOnly) await dlg.getByRole('checkbox', { name: /^Read-only/ }).check();
  if (save) await dlg.getByRole('checkbox', { name: /^Remember on the server/ }).check();
  await dlg.locator('.modal-f button.ink').click();
  const picker = page.locator('.modal:has-text("Databases on")');
  await expect(picker).toBeVisible();
  await picker.getByRole('button', { name: pick ? 'Add' : 'Just this one' }).click();
  await expect(picker).toBeHidden();
}

/** Opens a database from the switcher, by the group it's listed under (e.g. "e2e — 127.0.0.1"). */
export async function openDb(page, group, name) {
  await page.click('#dbSwitch');
  const found = await page.evaluate(([group, name]) => {
    let inGroup = false;
    for (const el of document.querySelector('.menu').querySelectorAll(':scope > *, :scope > * > *')) {
      if (el.classList.contains('mlabel')) { inGroup = el.textContent.trim() === group; continue; }
      if (inGroup && el.textContent.trim().replace(/open$/, '').trim() === name) { el.click(); return true; }
    }
    return false;
  }, [group, name]);
  expect(found, `"${name}" under "${group}" in the switcher`).toBe(true);
  await expect(page.locator('#dbName')).toHaveText(name);
}

/** Keeps the latest session the page opened (POST /api/sessions): its login and what it may do. */
export function watchSessions(page) {
  const seen = { last: null };
  page.on('response', async r => {
    if (r.request().method() === 'POST' && /\/api\/sessions$/.test(r.url())) seen.last = await r.json().catch(() => null);
  });
  return seen;
}

/** Collects uncaught page errors and server failures (5xx); Postgres errors are 400s by design. */
export function watchErrors(page) {
  const errors = [];
  page.on('pageerror', e => errors.push('pageerror: ' + e.message));
  page.on('response', r => r.status() >= 500 && errors.push(`HTTP ${r.status()} ${r.url()}`));
  return errors;
}

/** Saves the download a click starts and returns its text. */
export async function download(page, click) {
  const [dl] = await Promise.all([page.waitForEvent('download'), click()]);
  const file = await dl.path();
  return (await import('node:fs')).readFileSync(file, 'utf8');
}
