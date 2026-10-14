// Agent access: an AI agent (played here by the test, over MCP) uses a database the user shared with
// it, in the browser (PGlite). One page for the whole file, in order: each step builds on the last.
import { spawn } from 'node:child_process';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { test, expect } from '@playwright/test';
import { DB, admin, connect, dsn, env, openApp, runSQL, watchErrors } from './helpers.js';

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
/** The agent gives up on request id (Esc in Claude Code, say). */
const giveUp = id => fetch(env('URL') + '/mcp', { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${agentToken()}`, 'Mcp-Session-Id': sid },
  body: JSON.stringify({ jsonrpc: '2.0', method: 'notifications/cancelled', params: { requestId: id } }) });
const pets = async () => (await runSQL(page, 'select count(*) from pets')).rows[0][0];
const tool = async (name, args = {}) => (await mcp('tools/call', { name, arguments: name === 'run_sql' ? { purpose: 'a test query', ...args } : args })).result.content[0].text;

test.beforeAll(async ({ browser }) => {
  ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await ctx.addInitScript(() => {   // notifications, recorded instead of shown
    window.__notes = [];
    window.Notification = class { static permission = 'granted'; static requestPermission() { return Promise.resolve('granted'); }
      constructor(title, o) { this.i = window.__notes.push({ title, body: o?.body, open: true }) - 1; } close() { window.__notes[this.i].open = false; } };
  });
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
  await expect(page.locator('#agentBtn')).toHaveText('Agents');
  await expect(page.locator('#agentBtn')).toHaveAttribute('title', /^AI agents on your computer \(Claude, Codex…\)/);
  const name = await page.locator('#dbName').innerText();
  await expect.poll(() => tool('list_databases')).toContain(`“${name}” — PGlite in the browser`);
  expect(await tool('list_databases')).toContain('open — the user is looking at it');
});

test('reads run at once, in a conversation of the agent\'s own', async () => {
  let holds = 0;   // the tab tells pgquire it's still working, so long calls aren't given up on
  page.on('request', r => { if (r.url().endsWith('/api/agent/reply') && r.postData()?.includes('"hold":true')) holds++; });
  const out = await tool('run_sql', { sql: 'select name from pets' });
  expect(out).toMatch(/^\[pgquire: “.*” — PGlite in the browser\]\ncolumns: \["name"\]\n1 rows:\n\["Rex"\]/);
  expect(await tool('get_schema')).toContain('pets');
  const big = await tool('run_sql', { sql: "select g, repeat('x', 400) from generate_series(1, 60) g" });
  expect(big).toContain('60 rows (first 50)');
  expect(big).toMatch(/… \(cut at 9,000 characters of [\d,]+: select fewer or narrower columns, or fewer rows\)$/);
  await expect(page.locator('.conv-switch')).toContainText('Claude Code (agent)');
  await expect(page.locator('#clerkLog')).toContainText('select name from pets');
  await expect(page.locator('.agent-compose')).toBeVisible();
  expect(holds).toBeGreaterThan(0);
});

test('get_user_view: what the user has open', async () => {
  await runSQL(page, 'select name from pets order by id');
  const view = await tool('get_user_view');
  expect(view).toContain('The user is in the query editor tab');
  expect(view).toContain('select name from pets order by id');
  expect(view).toContain('Last result: 1 rows, columns name');
  expect(await tool('open_table', { table: 'pets', where: 'id = 1' })).toContain('Opened. 1 rows match.');
  expect(await tool('get_user_view')).toContain('The user is viewing the table pets (rows view), filtered by WHERE id = 1');
});

test('the agent\'s name: tidied on screen, as it gave it on hover', async () => {
  const other = async (method, params, id) => {   // Claude's app, through mcp-remote: a session of its own
    const r = await fetch(env('URL') + '/mcp', { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${agentToken()}`, ...(other.sid ? { 'Mcp-Session-Id': other.sid } : {}) },
      body: JSON.stringify({ jsonrpc: '2.0', id, method, params }) });
    other.sid ??= r.headers.get('mcp-session-id');
    return r.json();
  };
  await other('initialize', { protocolVersion: '2025-06-18', clientInfo: { name: 'claude-ai (via mcp-remote 0.14.3)' }, capabilities: {} }, 1);
  await other('tools/call', { name: 'run_sql', arguments: { sql: 'select 42', purpose: 'Checking the name' } }, 2);
  await expect(page.locator('.conv-switch')).toContainText('Claude (agent)');
  await expect(page.locator('#clerkLog .receipt .r-head').last()).toContainText('Claude · Query');
  await expect(page.locator('#clerkLog .receipt .r-head').last()).toHaveAttribute('title', 'claude-ai (via mcp-remote 0.14.3)');
  const sig = page.locator('#clerkLog .c-ai').last().locator('.sig > span').first();
  await expect(sig).toHaveText('— Claude');
  await expect(sig).toHaveAttribute('title', 'claude-ai (via mcp-remote 0.14.3)');
  await tool('run_sql', { sql: 'select 1' });   // back to Claude Code's log for what follows
});

test('a big schema is cut between tables, says so, and can be read in parts', async () => {
  const cols = Array.from({ length: 15 }, (_, i) => `a_rather_long_column_name_${i} text default 'a fairly long default value'`).join(', ');
  await runSQL(page, `do $$ begin for i in 1..60 loop execute format('create table wide_%s (id int primary key, ${cols.replace(/'/g, "''")})', i); end loop; end $$`);
  const all = await tool('get_schema', { purpose: 'Reading the schema' });
  expect(all).toMatch(/… \d+ more tables? not shown in full \(20,000 characters at most\): .*wide_60.*\. Call get_schema with tables \(or schema\) for the rest\.$/s);
  expect(all).not.toMatch(/\(id integer PK, a_rather[^)]*$/m);   // no table cut off mid-line
  const one = await tool('get_schema', { tables: ['wide_60'] });
  expect(one).toContain('table wide_60 [0 rows] (id integer PK');
  expect(one).not.toContain('not shown');
  expect(await tool('get_schema', { tables: ['nope'] })).toContain('ERROR: no such tables');
  await runSQL(page, `do $$ begin for i in 1..60 loop execute format('drop table wide_%s', i); end loop; end $$`);
});

test('changes wait for approval; declined ones don\'t run', async () => {
  const pending = tool('run_sql', { sql: 'insert into pets values (2, \'Tom\')', purpose: 'add Tom, the second pet' });
  const said = page.locator('#clerkLog .c-ai').last();   // what it says it's doing, above the card, before you stamp it
  await expect(said).toContainText('— Claude Code');
  await expect(said).toContainText('adding Tom, the second pet');
  const ask = page.locator('.r-ask');
  await expect(ask).toContainText('Claude Code wants to change the database');
  await expect(ask.getByRole('button', { name: 'Approve all this session' })).toHaveCount(0);   // never for agents
  await ask.getByRole('button', { name: 'Approve', exact: true }).click();
  expect(await pending).toContain('OK (1 rows affected)');

  const declined = tool('run_sql', { sql: 'delete from pets' });
  await page.locator('.r-ask').getByRole('button', { name: 'Decline' }).click();
  expect(await declined).toContain('DECLINED');
  expect((await runSQL(page, 'select count(*) from pets')).rows[0][0]).toBe('2');
});

test('a change can\'t pass for a read: a COMMIT hidden in a$$, read-only switched off by a quoted name', async () => {
  for (const sql of ['select 1 as a$$; commit; delete from pets; select 1 as b$$;', 'set "transaction_read_only" = off; delete from pets']) {
    const pending = tool('run_sql', { sql });
    await page.locator('.r-ask').getByRole('button', { name: 'Decline' }).click();
    expect(await pending).toContain('DECLINED');
    expect(await pets()).toBe('2');
  }
});

test('a transaction left open is rolled back; SET is put back', async () => {
  const pending = tool('run_sql', { sql: 'begin; insert into pets values (3, \'Kit\')' });
  await page.locator('.r-ask').getByRole('button', { name: 'Approve', exact: true }).click();
  expect(await pending).toContain('rolled back');
  const set = tool('run_sql', { sql: 'set search_path to nowhere; create temp table scratch(x int)' });   // a change, so the SET runs for real
  await page.locator('.r-ask').getByRole('button', { name: 'Approve', exact: true }).click();
  expect(await set).toContain('session settings (SET …) are put back');
  expect((await runSQL(page, 'show search_path')).rows[0][0]).not.toContain('nowhere');
  expect((await runSQL(page, 'select count(*) from pets')).rows[0][0]).toBe('2');
});

test('a transaction that fails partway: the real error, and that it was rolled back', async () => {
  const pending = tool('run_sql', { sql: 'begin; insert into pets values (3, \'Kit\'); insert into pets values (1, \'Rex again\'); commit;' });
  await page.locator('.r-ask').getByRole('button', { name: 'Approve', exact: true }).click();
  const out = await pending;
  expect(out).toContain('duplicate key value violates unique constraint');
  expect(out).toContain('The transaction it opened was rolled back, so nothing in it was kept.');
  expect(out).not.toContain('BEGIN without COMMIT');
  expect(await pets()).toBe('2');
});

test('the user\'s open transaction: the agent can read, not write', async () => {
  await runSQL(page, 'begin');
  expect(await tool('run_sql', { sql: 'insert into pets values (7, \'Zed\')' })).toContain('the user has a transaction of their own open');
  expect(await tool('run_sql', { sql: 'select count(*) from pets' })).toContain('["2"]');
  await expect(page.locator('.r-ask')).toHaveCount(0);
  await runSQL(page, 'rollback');
});

test('the agent gives up while its change waits: withdrawn, nothing runs', async () => {
  const pending = mcp('tools/call', { name: 'run_sql', arguments: { sql: 'delete from pets', purpose: 'Emptying pets' } }, 777);
  await expect(page.locator('.r-ask')).toBeVisible();
  await giveUp(777);
  await expect(page.locator('.r-ask')).toHaveCount(0);
  await expect(page.locator('#clerkLog .receipt').last()).toContainText('Not run — Claude Code stopped waiting');
  expect((await pending).result.content[0].text).toMatch(/^UNKNOWN OUTCOME/);
  expect(await pets()).toBe('2');
});

test('a call queued behind a waiting change can be given up: it never runs', async () => {
  const first = tool('run_sql', { sql: 'insert into pets values (3, \'Kit\')' });
  await expect(page.locator('.r-ask')).toBeVisible();
  const held = page.waitForRequest(r => r.url().endsWith('/api/agent/reply') && JSON.parse(r.postData()).hold);   // the tab has it, queued
  const queued = mcp('tools/call', { name: 'run_sql', arguments: { sql: 'delete from pets', purpose: 'Emptying pets' } }, 778);
  await held;
  await giveUp(778);
  expect((await queued).result.content[0].text).toMatch(/^UNKNOWN OUTCOME/);
  await page.locator('.r-ask').getByRole('button', { name: 'Approve', exact: true }).click();
  expect(await first).toContain('OK (1 rows affected)');
  await page.waitForTimeout(500);   // its turn came and went
  await expect(page.locator('.r-ask')).toHaveCount(0);
  expect(await pets()).toBe('3');
  await runSQL(page, 'delete from pets where id = 3');
});

test('made read-only while a change waits: it doesn\'t run; the Approve bar survives a redraw', async () => {
  const pending = tool('run_sql', { sql: 'delete from pets' });
  await expect(page.locator('.r-ask')).toBeVisible();
  await page.locator('.clerk [title="Close (Ctrl+J)"]').click();
  await page.click('#clerkToggle');
  await expect(page.locator('.r-ask')).toBeVisible();   // closing and reopening the Clerk redraws the log
  await page.click('#agentBtn');
  await page.locator('.menu').getByText('Make it read-only for agents').click();
  await page.locator('.r-ask').getByRole('button', { name: 'Approve', exact: true }).click();
  expect(await pending).toContain('NOT RUN: the user made this database read-only for agents meanwhile');
  expect(await pets()).toBe('2');
  await page.click('#agentBtn');
  await page.locator('.menu').getByText('Let agents change it too').click();
});

test('a message being typed to the Clerk is kept when the agent\'s log comes up', async () => {
  await page.locator('.agent-compose').getByRole('button', { name: 'Ask the Clerk' }).click();
  await page.locator('#clerkText').fill('my half-written question');
  await tool('run_sql', { sql: 'select 1' });
  await expect(page.locator('.conv-switch')).toContainText('Claude Code (agent)');
  await page.click('.conv-switch');
  await page.locator('.menu button', { hasText: 'New conversation' }).filter({ has: page.locator('.hint') }).first().click();   // the newest: the one being typed in
  await expect(page.locator('#clerkText')).toHaveValue('my half-written question');
  await page.locator('#clerkText').fill('');
});

test('in the background, a waiting change shows a notification until answered', async () => {
  await page.evaluate(() => { document.hasFocus = () => false; });
  const pending = tool('run_sql', { sql: 'delete from pets where id = 99' });
  await expect.poll(() => page.evaluate(() => window.__notes.at(-1))).toMatchObject({ title: expect.stringMatching(/^Claude Code wants to change “.+”$/), body: 'delete from pets where id = 99', open: true });
  await page.locator('.r-ask').getByRole('button', { name: 'Decline' }).click();
  expect(await pending).toContain('DECLINED');
  expect(await page.evaluate(() => window.__notes.at(-1).open)).toBe(false);
  await page.evaluate(() => { delete document.hasFocus; });
});

test('approve all from an agent, until stopped; the tab title says when it waits', async () => {
  const first = tool('run_sql', { sql: 'insert into pets values (5, \'Bo\')' });
  await expect(page).toHaveTitle(/^● Approve\? /);
  await page.locator('.r-ask').getByRole('button', { name: 'Approve all from Claude Code' }).click();
  expect(await first).toContain('1 rows affected');
  await expect(page).not.toHaveTitle(/●/);
  await expect(page.locator('.agent-compose')).toContainText('Approving all from Claude Code');
  expect(await tool('run_sql', { sql: 'insert into pets values (6, \'Mo\')' })).toContain('1 rows affected');   // no stamp asked
  // Another program giving the same name doesn't get it: approve all is for the connection it was given to
  const other = async (method, params, id) => {
    const r = await fetch(env('URL') + '/mcp', { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${agentToken()}`, ...(other.sid ? { 'Mcp-Session-Id': other.sid } : {}) },
      body: JSON.stringify({ jsonrpc: '2.0', id, method, params }) });
    other.sid ??= r.headers.get('mcp-session-id');
    return r.json();
  };
  await other('initialize', { protocolVersion: '2025-06-18', clientInfo: { name: 'claude-code' }, capabilities: {} }, 1);
  const posing = other('tools/call', { name: 'run_sql', arguments: { sql: 'insert into pets values (7, \'Ix\')', purpose: 'Posing' } }, 2);
  await page.locator('.r-ask').getByRole('button', { name: 'Decline' }).click();
  expect((await posing).result.content[0].text).toContain('DECLINED');
  await page.locator('.agent-compose').getByRole('button', { name: 'Stop approving all' }).click();
  const asked = tool('run_sql', { sql: 'delete from pets where id >= 5' });
  await page.locator('.r-ask').getByRole('button', { name: 'Approve', exact: true }).click();
  expect(await asked).toContain('2 rows affected');
});

test('the journal names the agent and keeps its purpose', async () => {
  await page.click('#journalToggle');
  const row = page.locator('.jrow', { hasText: 'delete from pets where id >= 5' }).first();
  await expect(row.locator('.js')).toHaveText('Claude Code');
  await row.click();
  await expect(row.locator('.jp')).toHaveText('a test query');
  await page.click('#journalToggle');
});

test('a new agent token: the old one stops working', async () => {
  await page.click('#agentBtn');
  await page.locator('.menu').getByText('Set up an agent…').click();
  await page.locator('.modal').last().getByRole('button', { name: 'New token…' }).click();
  await page.locator('.modal').last().getByRole('button', { name: 'Replace' }).click();
  await expect(page.locator('#toasts')).toContainText('New agent token');
  const shown = await page.locator('.modal pre').last().innerText();
  await page.locator('.modal').last().getByRole('button', { name: 'Done' }).click();
  expect(shown).toContain(agentToken());   // the dialog shows the new one, kept in agent-token
  expect(await tool('list_databases')).toContain('open');
});

test('shared read-only: changes are refused without asking', async () => {
  await page.click('#agentBtn');
  await page.locator('.menu').getByText('Make it read-only for agents').click();
  await expect.poll(() => tool('list_databases')).toContain('PGlite in the browser, read-only');
  expect(await tool('run_sql', { sql: 'delete from pets' })).toContain('shared this database with agents read-only');
  await expect(page.locator('.r-ask')).toHaveCount(0);
  // A filter is SQL too: one that writes fails, rather than running unasked
  await runSQL(page, "create table bumps(x int); create function bump() returns int language sql as 'insert into bumps values (1) returning 1'");
  expect(await tool('open_table', { table: 'pets', where: '(select bump()) is not null' })).toMatch(/\nERROR: cannot execute INSERT in a read-only transaction/);
  expect((await runSQL(page, 'select count(*) from bumps')).rows[0][0]).toBe('0');
  await runSQL(page, 'drop function bump(); drop table bumps');
  expect(await tool('run_sql', { sql: 'select count(*) from pets' })).toContain('["2"]');
  await page.click('#agentBtn');
  await page.locator('.menu').getByText('Let agents change it too').click();
  await expect.poll(() => tool('list_databases')).not.toContain('read-only');
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

test('switching database while a change waits: withdrawn, the agent is told, nothing runs', async () => {
  const shared = await page.locator('#dbName').innerText();
  const pending = tool('run_sql', { sql: 'delete from pets' });
  await expect(page.locator('.r-ask')).toBeVisible();
  await page.click('#dbSwitch');
  await page.locator('.menu').getByText('scratch', { exact: true }).click();
  await expect(page.locator('#dbName')).toHaveText('scratch');
  expect(await pending).toMatch(/^MOVED: “.+” was closed in that pgquire tab\. Nothing ran\./);
  await page.click('#dbSwitch');
  await page.locator('.menu').getByText(shared, { exact: true }).click();
  await expect(page.locator('#dbName')).toHaveText(shared);
  expect(await pets()).toBe('2');
});

test('pgquire mcp on stdio reaches the same tab', async () => {
  const p = spawn(env('BIN'), ['mcp', '-config', env('CONFIG')], { stdio: ['pipe', 'pipe', 'inherit'] });
  const lines = [];
  let buf = '';
  p.stdout.on('data', d => { buf += d; let i; while ((i = buf.indexOf('\n')) >= 0) { lines.push(JSON.parse(buf.slice(0, i))); buf = buf.slice(i + 1); } });
  const send = m => p.stdin.write(JSON.stringify({ jsonrpc: '2.0', ...m }) + '\n');
  send({ id: 1, method: 'initialize', params: { clientInfo: { name: 'codex' } } });
  send({ id: 2, method: 'tools/call', params: { name: 'run_sql', arguments: { sql: 'select name from pets order by 1', purpose: 'list the pets' } } });
  await expect.poll(() => lines.length).toBe(2);
  p.kill();
  expect(lines[1].result.content[0].text).toContain('["Rex"]');
  await expect(page.locator('.conv-switch')).toContainText('Codex (agent)');   // its own log, brought up over Claude Code's
});

test('whatever an agent does, its log comes up, the Clerk opening if it was closed', async () => {
  await page.locator('.clerk [title="Close (Ctrl+J)"]').click();
  await expect(page.locator('#app')).toHaveClass(/clerk-closed/);
  expect(await tool('run_sql', { sql: 'select 1' })).toContain('1 rows');
  await expect(page.locator('#app')).not.toHaveClass(/clerk-closed/);
  await expect(page.locator('.conv-switch')).toContainText('Claude Code (agent)');
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
  expect(await tool('run_sql', { database: DB, sql: 'select g from generate_series(1, 60000) g' })).toContain('60000 rows (first 50)');   // past -max-rows: the real count

  // The agent gives up on a slow query: it's stopped on the server, not left running.
  const sleeping = () => admin("select count(*)::int as n from pg_stat_activity where query = 'select pg_sleep(60)'").then(r => r[0].n);
  const slow = mcp('tools/call', { name: 'run_sql', arguments: { database: DB, sql: 'select pg_sleep(60)', purpose: 'Waiting' } }, 4242);
  await expect.poll(sleeping).toBe(1);
  await fetch(env('URL') + '/mcp', { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${agentToken()}`, 'Mcp-Session-Id': sid },
    body: JSON.stringify({ jsonrpc: '2.0', method: 'notifications/cancelled', params: { requestId: 4242 } }) });
  expect((await slow).result.content[0].text).toMatch(/^UNKNOWN OUTCOME/);
  await expect.poll(sleeping, { timeout: 5000 }).toBe(0);
  await page.click('#agentBtn');
  await page.locator('.menu').getByText(/^Stop sharing/).click();
  await expect.poll(() => tool('list_databases')).toContain('no database is shared with agents');

  // forget the connection: the remote tests that follow use the same server
  await page.click('#dbSwitch');
  await page.getByText('Connect remote Postgres…').click();
  await page.locator('.remote-row:has-text("agent-e2e")').first().locator('button').last().click();
  await page.locator('.modal').last().getByRole('button', { name: 'Forget' }).click();
  await expect(page.locator('.remote-row:has-text("agent-e2e")')).toHaveCount(0);
  await page.keyboard.press('Escape');   // close the Connect dialog
  await expect(page.locator('.modal-bg')).toHaveCount(0);
});

test('what the user does while a change waits stays theirs: their transaction, their SET', async () => {
  await connect(page, 'agent-wait', dsn());
  await page.click('#agentBtn');
  await page.locator('.menu').getByText(/^Share “.*” with agents…$/).click();
  await page.locator('.modal').last().getByRole('button', { name: 'Share' }).click();
  await expect.poll(() => tool('list_databases')).toContain(`“${DB}” — PostgreSQL`);
  await admin('create table if not exists wait_notes(id int primary key, body text); truncate wait_notes');
  const approve = () => page.locator('.r-ask').getByRole('button', { name: 'Approve', exact: true }).click();

  // The user opens a transaction while the agent's change waits: the change doesn't run, theirs is kept
  let pending = tool('run_sql', { database: DB, sql: "insert into wait_notes values (1, 'agent')" });
  await expect(page.locator('.r-ask')).toBeVisible();
  await runSQL(page, "begin; insert into wait_notes values (2, 'user')");
  await approve();
  expect(await pending).toContain('NOT RUN: the user opened a transaction meanwhile');
  await expect(page.locator('#clerkLog .receipt').last()).toContainText('Not run — you opened a transaction meanwhile');
  await runSQL(page, 'commit');
  expect(await admin('select id from wait_notes order by id')).toEqual([{ id: 2 }]);

  // …or runs a SET: it stays, while the agent's own SET is put back
  await runSQL(page, "set statement_timeout = '123s'");
  pending = tool('run_sql', { database: DB, sql: "set application_name = 'agent'; insert into wait_notes values (3, 'agent')" });
  await expect(page.locator('.r-ask')).toBeVisible();
  await runSQL(page, "set statement_timeout = '124s'");
  await approve();
  expect(await pending).toContain('session settings (SET …) are put back');
  expect((await runSQL(page, 'show statement_timeout')).rows[0][0]).toBe('124s');
  expect((await runSQL(page, 'show application_name')).rows[0][0]).not.toBe('agent');
  await runSQL(page, 'reset statement_timeout');

  // A filter that writes fails, on a remote database too
  await admin("create or replace function wait_bump() returns int language sql as 'insert into wait_notes values (99, null) returning 1'");
  expect(await tool('open_table', { database: DB, table: 'wait_notes', where: '(select wait_bump()) is not null' })).toMatch(/\nERROR: cannot execute INSERT in a read-only transaction/);
  expect(await admin('select count(*)::int n from wait_notes where id = 99')).toEqual([{ n: 0 }]);
  await admin('drop function wait_bump(); drop table wait_notes');

  await page.click('#agentBtn');
  await page.locator('.menu').getByText(/^Stop sharing/).click();
  await page.click('#dbSwitch');
  await page.getByText('Connect remote Postgres…').click();
  await page.locator('.remote-row:has-text("agent-wait")').first().locator('button').last().click();
  await page.locator('.modal').last().getByRole('button', { name: 'Forget' }).click();
  await expect(page.locator('.remote-row:has-text("agent-wait")')).toHaveCount(0);
  await page.keyboard.press('Escape');
  await expect(page.locator('.modal-bg')).toHaveCount(0);
});

test('production: every change asks, twice, and there\'s no approving all', async () => {
  await page.click('#dbSwitch');
  await page.getByText('Connect remote Postgres…').click();
  const dlg = page.locator('.modal').last();
  await dlg.getByPlaceholder('e.g. staging').fill('agent-prod');
  await dlg.locator('input[placeholder^="postgres://"]').fill(dsn());
  await dlg.getByRole('checkbox', { name: /^Production/ }).check();
  await dlg.getByRole('checkbox', { name: /^Read-only/ }).uncheck();   // writable production
  await dlg.locator('.modal-f button.ink').click();
  await page.locator('.modal:has-text("Databases on")').getByRole('button', { name: 'Just this one' }).click();
  await page.click('#agentBtn');
  await page.locator('.menu').getByText(/^Share “.*” with agents…$/).click();
  await page.locator('.modal').last().getByRole('button', { name: 'Share' }).click();
  await expect.poll(() => tool('list_databases')).toContain('PRODUCTION · id');

  const pending = tool('run_sql', { database: DB, sql: 'update customers set active = active where id = 1' });
  const ask = page.locator('.r-ask');
  await expect(ask).toContainText('Claude Code wants to change the database');
  await expect(ask.getByRole('button', { name: /Approve all/ })).toHaveCount(0);
  await ask.getByRole('button', { name: 'Approve', exact: true }).click();
  await page.locator('.modal').last().getByRole('button', { name: 'Run on production' }).click();
  expect(await pending).toContain('1 rows affected');

  await page.click('#agentBtn');
  await page.locator('.menu').getByText(/^Stop sharing/).click();
  await page.click('#dbSwitch');
  await page.getByText('Connect remote Postgres…').click();
  await page.locator('.remote-row:has-text("agent-prod")').first().locator('button').last().click();
  await page.locator('.modal').last().getByRole('button', { name: 'Forget' }).click();
  await expect(page.locator('.remote-row:has-text("agent-prod")')).toHaveCount(0);
  await page.keyboard.press('Escape');   // close the Connect dialog
  await expect(page.locator('.modal-bg')).toHaveCount(0);
});
