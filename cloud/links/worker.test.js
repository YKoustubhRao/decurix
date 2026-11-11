// Tests for the link service Worker, with a real SQLite standing in for D1 and a fake Cloudflare API.
// Run with `node --test` in this folder.

import test from 'node:test';
import assert from 'node:assert/strict';
import { DatabaseSync } from 'node:sqlite';
import worker, { sweep, NAME_RE } from './worker.js';

// d1 wraps node:sqlite in the slice of D1's API the Worker uses.
function d1() {
  const db = new DatabaseSync(':memory:');
  return {
    raw: db,
    prepare(sql) {
      let args = [];
      const stmt = {
        bind(...values) { args = values; return stmt; },
        async first() { return db.prepare(sql).get(...args) ?? null; },
        async all() { return { results: db.prepare(sql).all(...args) }; },
        async run() { db.prepare(sql).run(...args); return { success: true }; },
      };
      return stmt;
    },
  };
}

// fakeCloudflare records every API call and answers like Cloudflare does.
function fakeCloudflare({ failOn = '' } = {}) {
  const calls = [];
  const tunnels = new Map();
  const records = new Map();
  let next = 1;
  globalThis.fetch = async (url, init = {}) => {
    const path = new URL(url).pathname.replace('/client/v4', '');
    const method = init.method || 'GET';
    const body = init.body ? JSON.parse(init.body) : undefined;
    calls.push({ method, path, body, auth: init.headers?.Authorization });
    const ok = (result) => new Response(JSON.stringify({ success: true, result }), { status: 200 });
    if (failOn && `${method} ${path}`.includes(failOn)) {
      return new Response(JSON.stringify({ success: false, errors: [{ code: 1, message: 'refused' }] }), { status: 400 });
    }
    let m;
    if (method === 'POST' && /\/cfd_tunnel$/.test(path)) {
      const id = `tun-${next++}`;
      tunnels.set(id, { id, name: body.name });
      return ok({ id });
    }
    if ((m = path.match(/\/cfd_tunnel\/([^/]+)\/configurations$/))) { tunnels.get(m[1]).config = body.config; return ok({}); }
    if ((m = path.match(/\/cfd_tunnel\/([^/]+)\/token$/))) return ok(`token-for-${m[1]}`);
    if ((m = path.match(/\/cfd_tunnel\/([^/]+)\/connections$/))) return ok(null);
    if (method === 'DELETE' && (m = path.match(/\/cfd_tunnel\/([^/]+)$/))) { tunnels.delete(m[1]); return ok({}); }
    if (method === 'POST' && /\/dns_records$/.test(path)) {
      const id = `dns-${next++}`;
      records.set(id, body);
      return ok({ id });
    }
    if (method === 'DELETE' && (m = path.match(/\/dns_records\/([^/]+)$/))) { records.delete(m[1]); return ok({ id: m[1] }); }
    return new Response('{}', { status: 404 });
  };
  return { calls, tunnels, records };
}

function env(extra = {}) {
  return {
    DB: d1(), DOMAIN: 'example.test', ACCOUNT_ID: 'acc', ZONE_ID: 'zone',
    CF_API_TOKEN: 'cf-secret', ADMIN_KEY: 'admin-secret', ...extra,
  };
}

async function call(e, path, body, { ip = '203.0.113.7', headers = {}, method = 'POST' } = {}) {
  const res = await worker.fetch(new Request(`https://links.example.test${path}`, {
    method,
    headers: { 'Content-Type': 'application/json', 'CF-Connecting-IP': ip, ...headers },
    body: method === 'POST' ? JSON.stringify(body) : undefined,
  }), e);
  return { status: res.status, body: await res.json() };
}

test('a claim makes a name, a tunnel routed only to the given port, a proxied record and a token', async () => {
  const cf = fakeCloudflare();
  const e = env();
  const res = await call(e, '/v1/claim', { port: 8090 });
  assert.equal(res.status, 200);
  const { hostname, key, token } = res.body;
  assert.match(hostname, /^[a-z]+-[a-z]+-\d{4}\.example\.test$/);
  assert.ok(NAME_RE.test(hostname.split('.')[0]));
  assert.ok(key.length >= 40);
  const [tunnel] = [...cf.tunnels.values()];
  assert.equal(token, `token-for-${tunnel.id}`);
  assert.deepEqual(tunnel.config.ingress, [
    { hostname, service: 'http://127.0.0.1:8090' }, { service: 'http_status:404' },
  ]);
  const [record] = [...cf.records.values()];
  assert.equal(record.name, hostname);
  assert.equal(record.content, `${tunnel.id}.cfargotunnel.com`);
  assert.equal(record.proxied, true);
  // The key is never stored, only its hash; the IP only as a hash.
  const row = e.DB.raw.prepare('SELECT * FROM names').get();
  assert.notEqual(row.key_hash, key);
  assert.equal(JSON.stringify(e.DB.raw.prepare('SELECT * FROM claims').all()).includes('203.0.113.7'), false);
  assert.ok(cf.calls.every((c) => c.auth === 'Bearer cf-secret'));
});

test('the token comes back only with the right key, and a port change re-routes the tunnel', async () => {
  const cf = fakeCloudflare();
  const e = env();
  const { body: claimed } = await call(e, '/v1/claim', {});
  const tunnel = [...cf.tunnels.values()][0];
  assert.equal(tunnel.config.ingress[0].service, 'http://127.0.0.1:8090');

  const wrong = await call(e, '/v1/token', { hostname: claimed.hostname, key: 'not-the-key' });
  assert.equal(wrong.status, 404);
  const missing = await call(e, '/v1/token', { hostname: 'calm-river-1234.example.test', key: claimed.key });
  assert.equal(missing.status, 404);
  const foreign = await call(e, '/v1/token', { hostname: 'www.other.test', key: claimed.key });
  assert.equal(foreign.status, 404);

  const again = await call(e, '/v1/token', { hostname: claimed.hostname, key: claimed.key, port: 9000 });
  assert.equal(again.status, 200);
  assert.equal(again.body.token, claimed.token);
  assert.equal(tunnel.config.ingress[0].service, 'http://127.0.0.1:9000');
});

test('claims are limited per network per day and in total', async () => {
  fakeCloudflare();
  const e = env({ CLAIMS_PER_IP_PER_DAY: '2', MAX_NAMES: '3' });
  assert.equal((await call(e, '/v1/claim', {})).status, 200);
  assert.equal((await call(e, '/v1/claim', {})).status, 200);
  assert.equal((await call(e, '/v1/claim', {})).status, 429);
  assert.equal((await call(e, '/v1/claim', {}, { ip: '198.51.100.1' })).status, 200);
  assert.equal((await call(e, '/v1/claim', {}, { ip: '198.51.100.2' })).status, 503);
});

test('a bad port is refused before anything is created', async () => {
  const cf = fakeCloudflare();
  const e = env();
  for (const port of [0, 70000, 'x', 80.5]) {
    assert.equal((await call(e, '/v1/claim', { port })).status, 400);
  }
  assert.equal(cf.calls.length, 0);
});

test('a claim that fails half way leaves nothing behind', async () => {
  const cf = fakeCloudflare({ failOn: 'POST /zones/zone/dns_records' });
  const e = env();
  const res = await call(e, '/v1/claim', {});
  assert.equal(res.status, 502);
  assert.equal(JSON.stringify(res.body).includes('cf-secret'), false);
  assert.equal(cf.tunnels.size, 0);
  assert.equal(cf.records.size, 0);
  assert.equal(e.DB.raw.prepare('SELECT COUNT(*) AS n FROM names').get().n, 0);
});

test('release needs the key; revoke needs the admin key; both remove tunnel, record and row', async () => {
  const cf = fakeCloudflare();
  const e = env();
  const { body: a } = await call(e, '/v1/claim', {});
  const { body: b } = await call(e, '/v1/claim', {});

  assert.equal((await call(e, '/v1/release', { hostname: a.hostname, key: b.key })).status, 404);
  assert.equal((await call(e, '/v1/release', { hostname: a.hostname, key: a.key })).status, 200);
  assert.equal(cf.tunnels.size, 1);
  assert.equal(cf.records.size, 1);

  assert.equal((await call(e, '/v1/admin/revoke', { hostname: b.hostname })).status, 403);
  assert.equal((await call(e, '/v1/admin/revoke', { hostname: b.hostname }, { headers: { Authorization: 'Bearer wrong' } })).status, 403);
  assert.equal((await call(e, '/v1/admin/revoke', { hostname: b.hostname }, { headers: { Authorization: 'Bearer admin-secret' } })).status, 200);
  assert.equal(cf.tunnels.size, 0);
  assert.equal(cf.records.size, 0);
  assert.equal((await call(e, '/v1/token', { hostname: b.hostname, key: b.key })).status, 404);
});

test('the daily sweep removes only names unseen for the stale period', async () => {
  const cf = fakeCloudflare();
  const e = env({ STALE_DAYS: '90' });
  const { body: old } = await call(e, '/v1/claim', {});
  const { body: fresh } = await call(e, '/v1/claim', {});
  e.DB.raw.prepare('UPDATE names SET last_seen_ms = ? WHERE name = ?').run(Date.now() - 91 * 86400000, old.hostname.split('.')[0]);
  assert.equal(await sweep(e, Date.now()), 1);
  assert.equal((await call(e, '/v1/token', { hostname: old.hostname, key: old.key })).status, 404);
  assert.equal((await call(e, '/v1/token', { hostname: fresh.hostname, key: fresh.key })).status, 200);
  assert.equal(cf.tunnels.size, 1);
});

test('health answers GET; anything unconfigured or unknown is refused', async () => {
  fakeCloudflare();
  assert.deepEqual((await call(env(), '/v1/health', null, { method: 'GET' })).body, { ok: true });
  assert.equal((await call(env({ CF_API_TOKEN: '' }), '/v1/claim', {})).status, 503);
  assert.equal((await call(env(), '/v1/nope', {})).status, 404);
  assert.equal((await call(env(), '/v1/claim', null, { method: 'GET' })).status, 405);
});
