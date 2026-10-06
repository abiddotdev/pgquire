// Starting pgquire with -dsn / PGQUIRE_DSN: it connects before it starts, and the page opens there.
// Each test runs its own server (from the binary setup.js built), apart from the shared one.
import { test, expect } from '@playwright/test';
import { spawn } from 'node:child_process';
import path from 'node:path';
import { DB, bootDone, dsn, env, runSQL, watchErrors } from './helpers.js';

/** Starts the built server with args (and extra environment); resolves with its output once it has
 *  printed the open line, or exited. */
function start(args, extraEnv = {}) {
  const srv = spawn(env('BIN'), ['-listen', '127.0.0.1:0', '-no-open', '-token', 'e2e-start', ...args], { env: { ...process.env, ...extraEnv } });
  let out = '';
  return new Promise(resolve => {
    const done = code => resolve({ srv, out, code });
    srv.stdout.on('data', d => { out += d; if (/connections: /.test(out)) done(null); });
    srv.stderr.on('data', d => { out += d; });
    srv.on('exit', done);
  });
}

test('a wrong password stops it with the reason', async () => {
  const bad = new URL(dsn());
  bad.password = 'wrong';
  const { srv, code, out } = await start(['-dsn', bad.toString(), '-config', path.join(env('TMP'), 'start-bad.json')]);
  if (code === null) srv.kill(); // it should have refused to start
  expect(code).toBe(1);
  expect(out).toContain('-dsn:');
  expect(out).toContain('password authentication failed');
});

test('PGQUIRE_DSN opens that database in the page', async ({ page }) => {
  const errors = watchErrors(page);
  const { srv, out, code } = await start(['-config', path.join(env('TMP'), 'start-ok.json')], { PGQUIRE_DSN: dsn() });
  try {
    expect(code, out).toBeNull();
    expect(out).toMatch(new RegExp(`connected: ${DB} on 127\\.0\\.0\\.1 \\(PostgreSQL [\\d.]+, not encrypted\\)`));
    expect(out).toContain('(0 saved)');
    await page.goto(out.match(/open: (\S+)/)[1]);
    await bootDone(page);
    await expect(page.locator('#dbName')).toHaveText(DB);
    const r = await runSQL(page, 'select current_user, current_database()');
    expect(r.rows[0]).toEqual(['postgres', DB]);
    expect(errors).toEqual([]);
  } finally {
    srv.kill();
  }
});
