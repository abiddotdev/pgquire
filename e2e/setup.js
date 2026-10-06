// Starts what the browser tests need and returns the teardown:
//   Postgres   PGQUIRE_E2E_PG (a server URL, e.g. CI's service), or a throwaway Docker container
//              (PGQUIRE_E2E_IMAGE, default postgres:17-alpine); seeded with the e2e_shop database
//   pgquire    built from ../server and started on a free port, with its own token and connections file
//   Pages      ../index.html served as a static file, as GitHub Pages does
// The tests read where everything is from E2E_* variables.
import { execFileSync, spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createServer } from 'node:http';
import os from 'node:os';
import path from 'node:path';
import pg from 'pg';

const root = path.resolve(import.meta.dirname, '..');
export const DB = 'e2e_shop';
export const ROLE = 'pgquire_e2e_ro';

export default async function setup() {
  const undo = [];
  const teardown = async () => { for (const f of undo.reverse()) await Promise.resolve().then(f).catch(e => console.error('teardown:', e.message)); };
  try {
    const tmp = mkdtempSync(path.join(os.tmpdir(), 'pgquire-e2e-'));
    undo.push(() => rmSync(tmp, { recursive: true, force: true }));

    let pgUrl = process.env.PGQUIRE_E2E_PG;
    if (!pgUrl) {
      const name = `pgquire-e2e-${process.pid}`;
      execFileSync('docker', ['run', '-d', '--rm', '--name', name, '-e', 'POSTGRES_PASSWORD=secret', '-p', '127.0.0.1::5432', process.env.PGQUIRE_E2E_IMAGE || 'postgres:17-alpine'], { stdio: 'ignore' });
      undo.push(() => execFileSync('docker', ['stop', name], { stdio: 'ignore' }));
      const port = execFileSync('docker', ['port', name, '5432/tcp']).toString().split('\n')[0].split(':').pop();
      pgUrl = `postgres://postgres:secret@127.0.0.1:${port}/postgres?sslmode=disable`;
    }
    await waitFor(() => query(pgUrl, 'select 1'), 'Postgres');
    await seed(pgUrl);

    const bin = path.join(tmp, process.platform === 'win32' ? 'pgquire.exe' : 'pgquire');
    execFileSync('go', ['build', '-o', bin, '.'], { cwd: path.join(root, 'server'), stdio: 'inherit' });
    const token = randomBytes(12).toString('hex');
    const port = await freePort();
    const srv = spawn(bin, ['-listen', `127.0.0.1:${port}`, '-no-open', '-token', token, '-config', path.join(tmp, 'connections.json')], { stdio: ['ignore', 'ignore', 'inherit'] });
    undo.push(() => srv.kill());
    const url = `http://127.0.0.1:${port}`;
    await waitFor(async () => { if (!(await fetch(url + '/api/health')).ok) throw new Error('not up'); }, 'pgquire');

    const html = readFileSync(path.join(root, 'index.html'));
    const pages = createServer((req, res) => {
      if (!['/', '/index.html'].includes(req.url.split('?')[0])) return res.writeHead(404).end();
      res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' }).end(html);
    });
    await new Promise(r => pages.listen(0, '127.0.0.1', r));
    undo.push(() => new Promise(r => { pages.closeAllConnections(); pages.close(r); }));

    Object.assign(process.env, {
      E2E_PG: pgUrl, E2E_BIN: bin, E2E_TMP: tmp, E2E_URL: url, E2E_TOKEN: token,
      E2E_CONFIG: path.join(tmp, 'connections.json'), E2E_PAGES: `http://127.0.0.1:${pages.address().port}`,
    });
  } catch (e) {
    await teardown();
    throw e;
  }
  return teardown;
}

async function seed(pgUrl) {
  await query(pgUrl, `drop database if exists ${DB} with (force)`);
  await query(pgUrl, `do $$ begin if exists (select 1 from pg_roles where rolname = '${ROLE}') then drop owned by ${ROLE}; drop role ${ROLE}; end if; end $$`);
  await query(pgUrl, `create database ${DB}`);
  await query(withDb(pgUrl, DB), `
    create table customers(id serial primary key, name text not null, email text unique, created timestamptz default now(), active boolean default true, meta jsonb);
    create table orders(id serial primary key, customer_id int references customers(id), total numeric(10,2), placed date default current_date, tags text[], receipt bytea);
    insert into customers(name, email, meta) select 'Customer ' || g, 'c' || g || '@ex.com', jsonb_build_object('tier', g % 3) from generate_series(1, 250) g;
    insert into orders(customer_id, total, tags, receipt) select (g % 250) + 1, (g * 7.31)::numeric(10,2), array['a','b'], '\\xdeadbeef' from generate_series(1, 1200) g;
    create view big_spenders as select c.name, sum(o.total) total from customers c join orders o on o.customer_id = c.id group by c.name;`);
}

export function withDb(pgUrl, db) {
  const u = new URL(pgUrl);
  u.pathname = '/' + db;
  return u.toString();
}

export async function query(pgUrl, sql, params) {
  const c = new pg.Client({ connectionString: pgUrl });
  await c.connect();
  try { return (await c.query(sql, params)).rows; } finally { await c.end(); }
}

async function waitFor(fn, what, ms = 60_000) {
  const end = Date.now() + ms;
  for (;;) {
    try { return await fn(); } catch (e) {
      if (Date.now() > end) throw new Error(`${what} didn't come up: ${e.message}`);
      await new Promise(r => setTimeout(r, 500));
    }
  }
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = createServer().listen(0, '127.0.0.1', () => { const { port } = s.address(); s.close(() => resolve(port)); });
    s.on('error', reject);
  });
}
