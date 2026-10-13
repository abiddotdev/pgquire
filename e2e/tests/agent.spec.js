// Agent access: an AI agent (played here by the test, over MCP) uses a database the user shared with
// it, in the browser (PGlite). One page for the whole file, in order: each step builds on the last.
import { spawn } from 'node:child_process';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { test, expect } from '@playwright/test';
import { DB, connect, dsn, env, openApp, runSQL, watchErrors } from './helpers.js';

test.describe.configure({ mode: 'serial', retries: 0 });

let ctx, page, errors, sid;
const agentToken = () => readFileSync(path.join(path.dirname(env('CONFIG')), 'agent-token'), 'utf8').trim();

/** One JSON-RPC call to /mcp, as Claude Code does over HTTP. */
async function mcp(method, params, id = Math.floor(Math.random() * 1e9)) {
  const r = await fetch(env('URL') + '/mcp', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream', Authorization: `Bearer ${agentToken()}`, ...(sid ? { 'Mcp-Session-Id': sid } : {}) },
    body: JSON.stringify({ jsonrpc: '2.0', id, method, params }),
  });
  sid = r.headers.get('mcp-session-id') || sid;
  return r.json();
}
const tool = async (name, args = {}) => (await mcp('tools/call', { name, arguments: args })).result.content[0].text;

test.beforeAll(async ({ browser }) => {
  ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  page = await ctx.newPage();
  errors = watchErrors(page);
  await openApp(page);
  await runSQL(page, 'create table if not exists pets(id int primary key, name text); truncate pets; insert into pets values (1, \'Rex\')');
  await mcp('initialize', { protocolVersion: '2025-06-18', clientInfo: { name: 'claude-code' }, capabilities: {} });
});
test.afterAll(async () => { await Promise.race([ctx?.close(), new Promise(r => setTimeout(r, 10_000))]); });
test.afterEach(() => expect(errors, 'page errors and server failures').toEqual([]));

test('nothing is shared until the user shares it', async () => {
  expect(await tool('list_databases')).toContain('no database is shared with agents');
  expect(await tool('run_sql', { sql: 'select 1' })).toMatch(/^ERROR: /);
  await page.click('#agentBtn');
  await page.locator('.menu').getByText(/^Share “.*” with agents…$/).click();
  await page.locator('.modal').last().getByRole('button', { name: 'Share' }).click();
  await expect(page.locator('#agentBtn')).toHaveClass(/shared/);
  const name = await page.locator('#dbName').innerText();
  await expect.poll(() => tool('list_databases')).toContain(`“${name}” — PGlite in the browser`);
  expect(await tool('list_databases')).toContain('open — the user is looking at it');
});

test('reads run at once, in a conversation of the agent\'s own', async () => {
  const out = await tool('run_sql', { sql: 'select name from pets' });
  expect(out).toMatch(/^\[pgquire: “.*” — PGlite in the browser\]\ncolumns: \["name"\]\n1 rows:\n\["Rex"\]/);
  expect(await tool('get_schema')).toContain('pets');
  await expect(page.locator('.conv-switch')).toContainText('Claude Code (agent)');
  await expect(page.locator('#clerkLog')).toContainText('select name from pets');
  await expect(page.locator('.agent-compose')).toBeVisible();
});

test('changes wait for approval; declined ones don\'t run', async () => {
  const pending = tool('run_sql', { sql: 'insert into pets values (2, \'Tom\')' });
  const ask = page.locator('.r-ask');
  await expect(ask).toContainText('Claude Code wants to change the database');
  await expect(ask.getByRole('button', { name: 'Approve all this session' })).toHaveCount(0);   // never for agents
  await ask.getByRole('button', { name: 'Approve' }).click();
  expect(await pending).toContain('OK (1 rows affected)');

  const declined = tool('run_sql', { sql: 'delete from pets' });
  await page.locator('.r-ask').getByRole('button', { name: 'Decline' }).click();
  expect(await declined).toContain('DECLINED');
  expect((await runSQL(page, 'select count(*) from pets')).rows[0][0]).toBe('2');
});

test('a transaction left open is rolled back; SET is put back', async () => {
  const pending = tool('run_sql', { sql: 'begin; insert into pets values (3, \'Kit\')' });
  await page.locator('.r-ask').getByRole('button', { name: 'Approve' }).click();
  expect(await pending).toContain('rolled back');
  const set = tool('run_sql', { sql: 'set search_path to nowhere; create temp table scratch(x int)' });   // a change, so the SET runs for real
  await page.locator('.r-ask').getByRole('button', { name: 'Approve' }).click();
  expect(await set).toContain('session settings (SET …) are put back');
  expect((await runSQL(page, 'show search_path')).rows[0][0]).not.toContain('nowhere');
  expect((await runSQL(page, 'select count(*) from pets')).rows[0][0]).toBe('2');
});

test('a database that isn\'t open: the user is offered to open it', async () => {
  const shared = await page.locator('#dbName').innerText();
  await page.click('#dbSwitch');
  await page.locator('.menu').getByText('New database', { exact: true }).click();
  await page.locator('.modal').last().locator('input').fill('scratch');
  await page.locator('.modal').last().locator('.modal-f button.ink').click();
  await expect(page.locator('#dbName')).toHaveText('scratch');
  await expect(page.locator('#agentBtn')).not.toHaveClass(/shared/);

  expect(await tool('list_databases')).toContain('not open');
  expect(await tool('run_sql', { database: shared, sql: 'select 1' })).toContain(`“${shared}” isn't open in pgquire right now. Nothing ran.`);
  await page.locator('.toast').getByRole('button', { name: 'Open it here' }).click();
  await expect(page.locator('#dbName')).toHaveText(shared);
  await expect.poll(() => tool('run_sql', { database: shared, sql: 'select count(*) from pets' })).toContain('["2"]');
});

test('pgquire mcp on stdio reaches the same tab', async () => {
  const p = spawn(env('BIN'), ['mcp', '-config', env('CONFIG')], { stdio: ['pipe', 'pipe', 'inherit'] });
  const lines = [];
  let buf = '';
  p.stdout.on('data', d => { buf += d; let i; while ((i = buf.indexOf('\n')) >= 0) { lines.push(JSON.parse(buf.slice(0, i))); buf = buf.slice(i + 1); } });
  const send = m => p.stdin.write(JSON.stringify({ jsonrpc: '2.0', ...m }) + '\n');
  send({ id: 1, method: 'initialize', params: { clientInfo: { name: 'codex' } } });
  send({ id: 2, method: 'tools/call', params: { name: 'run_sql', arguments: { sql: 'select name from pets order by 1' } } });
  await expect.poll(() => lines.length).toBe(2);
  p.kill();
  expect(lines[1].result.content[0].text).toContain('["Rex"]');
  await page.click('.conv-switch');   // its own conversation, not brought forward over Claude Code's
  await expect(page.locator('.menu')).toContainText('Codex (agent)');
  await page.keyboard.press('Escape');
});

test('unsharing stops it', async () => {
  await page.click('#agentBtn');
  await page.locator('.menu').getByText(/^Stop sharing/).click();
  await expect.poll(() => tool('run_sql', { sql: 'select 1' })).toContain('no database is shared with agents');
});

test('a remote database: the same tools, through the tab\'s connection', async () => {
  await connect(page, 'agent-e2e', dsn());
  await expect(page.locator('#dbName')).toHaveText(DB);
  await page.click('#agentBtn');
  await page.locator('.menu').getByText(/^Share “.*” with agents…$/).click();
  await page.locator('.modal').last().getByRole('button', { name: 'Share' }).click();
  await expect.poll(() => tool('list_databases')).toMatch(new RegExp(`“${DB}” — PostgreSQL on 127\\.0\\.0\\.1 · id \\S+ · open — the user is looking at it`));
  expect(await tool('run_sql', { database: DB, sql: 'select count(*) from customers' })).toContain('["250"]');
  await page.click('#agentBtn');
  await page.locator('.menu').getByText(/^Stop sharing/).click();
  await expect.poll(() => tool('list_databases')).toContain('no database is shared with agents');

  // forget the connection: the remote tests that follow use the same server
  await page.click('#dbSwitch');
  await page.getByText('Connect remote Postgres…').click();
  await page.locator('.remote-row:has-text("agent-e2e")').first().locator('button').last().click();
  await page.locator('.modal').last().getByRole('button', { name: 'Forget' }).click();
  await expect(page.locator('.remote-row:has-text("agent-e2e")')).toHaveCount(0);
});
