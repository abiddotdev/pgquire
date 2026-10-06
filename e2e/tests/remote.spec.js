// Remote Postgres through the pgquire server, as a user goes: connect, query, edit, create a
// read-only login. One page for the whole file, in order: each step builds on the last.
import { test, expect } from '@playwright/test';
import { readFileSync, statSync } from 'node:fs';
import { DB, ROLE, admin, connect, download, dsn, env, openApp, openDb, runSQL, text, watchErrors, watchSessions } from './helpers.js';

test.describe.configure({ mode: 'serial', retries: 0 }); // the steps share server state, so a retry can't start clean

let ctx, page, errors, sessions;
const savedConnections = () => JSON.parse(readFileSync(env('CONFIG'), 'utf8')).connections;

test.beforeAll(async ({ browser }) => {
  ctx = await browser.newContext({ viewport: { width: 1400, height: 900 }, acceptDownloads: true });
  page = await ctx.newPage();
  errors = watchErrors(page);
  sessions = watchSessions(page);
  await openApp(page);
});
test.afterAll(async () => {
  await Promise.race([ctx?.close(), new Promise(r => setTimeout(r, 10_000))]);
});
test.afterEach(() => expect(errors, 'page errors and server failures').toEqual([]));

test('test and connect a remembered, read-only connection', async () => {
  await page.click('#dbSwitch');
  await page.getByText('Connect remote Postgres…').click();
  const dlg = page.locator('.modal').last();
  await dlg.getByPlaceholder('e.g. staging').fill('e2e');
  await dlg.locator('input[placeholder^="postgres://"]').fill(dsn());
  await dlg.getByRole('checkbox', { name: /^Read-only/ }).check();
  await dlg.getByRole('button', { name: 'Test' }).click();
  // a test connection reports the server, where it landed, and that this login could write anyway
  await expect(dlg.locator('.modal-f')).toContainText(new RegExp(`PostgreSQL \\d+.* · ${DB} as postgres · not encrypted`));
  await expect(dlg.locator('.modal-f')).toContainText('can write, so read-only will only be a guard rail');

  await dlg.getByRole('checkbox', { name: /^Remember on the server/ }).check();
  await dlg.locator('.modal-f button.ink').click();
  const picker = page.locator('.modal:has-text("Databases on")');
  await expect(picker.getByRole('checkbox', { name: /postgres/ })).not.toBeChecked();
  await expect(picker.getByRole('checkbox', { name: new RegExp(DB) })).toBeChecked();
  await picker.getByRole('button', { name: 'Add' }).click();
  await expect(page.locator('#dbName')).toHaveText(DB);
  await expect(page.locator('#dbKind')).toHaveText(/read-only/i);

  const saved = savedConnections();
  expect(saved).toHaveLength(1);
  expect(saved[0]).toMatchObject({ name: 'e2e', readOnly: true });
  if (process.platform !== 'win32') expect(statSync(env('CONFIG')).mode & 0o777).toBe(0o600);
  expect(sessions.last.role).toMatchObject({ user: 'postgres', superuser: true, writable: true });
});

test('other databases on the same server', async () => {
  await page.click('#dbSwitch');
  await page.getByText('Other databases on this server…').click();
  const picker = page.locator('.modal').last();
  await expect(picker).toContainText(`${DB}`);
  await picker.getByRole('checkbox', { name: /postgres/ }).check();
  await picker.getByRole('button', { name: 'Add' }).click();
  await expect(page.locator('#dbName')).toHaveText('postgres');
  await openDb(page, 'e2e — 127.0.0.1', DB);
  for (const t of ['customers', 'orders', 'big_spenders']) await expect(page.locator('#index')).toContainText(t);
});

test('queries: values, errors, the row cap, Stop, transactions', async () => {
  let r = await runSQL(page, `select 42::int i, 12.50::numeric n, '{"a":[1,2]}'::jsonb j, '\\xdeadbeef'::bytea b, null::text nul`);
  expect(r.rows[0].slice(0, 4)).toEqual(['42', '12.50', '{"a":[1,2]}', '\\xdeadbeef']);

  r = await runSQL(page, 'select nope from customers');
  expect(r.error).toContain('column "nope" does not exist');

  // past -max-rows the page gets the first 50,000; "CSV (all)" streams every row
  r = await runSQL(page, 'select g from generate_series(1, 60000) g');
  expect(r.meta).toContain('first 50,000 of 60,000 rows');
  const csv = (await download(page, () => page.locator('#pane button:has-text("CSV (all")').click())).trim().split('\n');
  expect(csv).toHaveLength(60_001);
  expect([csv[0], csv[60_000]]).toEqual(['g', '60000']);

  await page.click('#newQueryBtn');
  await page.locator('#pane textarea').first().fill('select pg_sleep(30)');
  await page.locator('#pane textarea').first().press('Control+Enter');
  await expect(page.locator('#pane button:has-text("Stop")')).toBeVisible();
  // Stop cancels what's running on the server, so wait until Postgres is running it
  await expect.poll(async () => (await admin(
    "select count(*)::int n from pg_stat_activity where query = 'select pg_sleep(30)' and state = 'active'", [], 'postgres'))[0].n).toBe(1);
  await page.locator('#pane button:has-text("Stop")').click();
  await expect(page.locator('#pane .pane-error')).toContainText('canceling statement due to user request', { timeout: 5000 });
  r = await runSQL(page, 'select count(*) from orders');
  expect(r.rows[0][0]).toMatch(/^1,?200$/); // the session survived the cancel

  r = await runSQL(page, "insert into customers(name) values ('nope')");
  expect(r.error).toContain('read-only transaction');

  await runSQL(page, 'begin; select 1');
  await expect(page.locator('#txBanner')).toBeVisible();
  await page.click('#txRollback');
  await expect(page.locator('#txBanner')).toBeHidden();
});

test('tables: paging, filters, CSV export, editing a cell', async () => {
  await connect(page, 'e2e-rw', dsn()); // writable, this run only
  await expect(page.locator('#dbWhere')).toHaveText(/e2e-rw/i);
  await page.locator('#index').getByText('customers', { exact: true }).first().click();
  const foot = page.locator('#pane .grid-foot');
  await expect(foot).toContainText('1–100 of 250');
  await page.locator('#pane button:has-text("Next")').click();
  await expect(foot).toContainText('101–200 of 250');

  const filter = page.locator('#pane input[placeholder^="price"]');
  await filter.fill('id > 240');
  await filter.press('Enter');
  await expect(foot).toContainText('1–10 of 10');
  const exported = (await download(page, () => page.locator('#pane button[title="Export CSV (current filter)"]').click())).trim().split('\n');
  expect(exported).toHaveLength(11);
  expect(exported[0]).toBe('id,name,email,created,active,meta');

  // a filter is one expression: anything after a ; is a syntax error, not a second statement
  await filter.fill('id = 245; delete from orders');
  await filter.press('Enter');
  await expect(page.locator('#pane')).toContainText('syntax error');
  expect((await admin('select count(*)::int n from orders'))[0].n).toBe(1200);

  await filter.fill('id = 245');
  await filter.press('Enter');
  await expect(foot).toContainText('1–1 of 1');
  const value = `O'Brien "quoted" ✓`;
  await page.locator('#pane table.grid tbody tr').first().locator('td[data-c]').nth(1).dblclick();
  const input = page.locator('textarea:focus, input:focus').first();
  await input.fill(value);
  await input.press('Enter');
  await expect(page.locator('#toasts')).toContainText('Updated customers.name');
  expect((await admin('select name from customers where id = 245'))[0].name).toBe(value);
});

test('read-only login: create, switch, refuse writes, remove with the connection', async () => {
  await openDb(page, 'e2e — 127.0.0.1', DB);
  await expect.poll(() => sessions.last?.role?.user).toBe('postgres');

  await page.click('#dbSwitch');
  await page.getByText('Create a read-only login…').click();
  const dlg = page.locator('.modal').last();
  await dlg.getByRole('textbox').first().fill(ROLE);
  const preview = dlg.locator('.sqlpreview');
  await expect(preview).toContainText(`create role "${ROLE}" login password '<choose a password>'`);
  await expect(preview).toContainText(/grant pg_read_all_data|grant select on all tables/);
  await expect(dlg.locator('.modal-f button.ink')).toBeEnabled();
  await dlg.locator('.modal-f button.ink').click();
  await expect(page.locator('#toasts')).toContainText(`Now connected as ${ROLE}`);

  const [stored] = await admin('select rolpassword from pg_authid where rolname = $1', [ROLE], 'postgres');
  expect(stored.rolpassword).toMatch(/^SCRAM-SHA-256\$4096:/); // never sent in plain text
  await expect.poll(() => sessions.last?.role?.user).toBe(ROLE);
  expect(sessions.last.role.writable).toBe(false);
  expect(savedConnections()[0]).toMatchObject({ user: ROLE, createdRole: ROLE });

  let r = await runSQL(page, 'select count(*) from customers');
  expect(r.rows[0][0]).toBe('250');
  // switching pgquire's guard rail off doesn't help: the login itself may not write
  r = await runSQL(page, "set transaction_read_only = off; set default_transaction_read_only = off; commit; insert into customers(name) values ('x')");
  expect(r.error).toContain('permission denied');
  await page.click('#dbSwitch');
  await expect(page.locator('.menu')).not.toContainText('Create a read-only login…');
  await page.keyboard.press('Escape');

  // forgetting the connection drops the login too, using the connection's original superuser
  await page.click('#dbSwitch');
  await page.getByText('Connect remote Postgres…').click();
  await page.locator('.remote-row:has-text("e2e")').first().locator('button').last().click();
  const forget = page.locator('.modal').last();
  await expect(forget).toContainText(`Also remove the login ${ROLE}`);
  await forget.getByRole('button', { name: 'Forget' }).click();
  await expect(page.locator('#toasts')).toContainText(`removed the login ${ROLE}`);
  expect(await admin('select 1 from pg_roles where rolname = $1', [ROLE], 'postgres')).toHaveLength(0);
  expect(savedConnections()).toHaveLength(0);
});
