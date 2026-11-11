// The link service: a Cloudflare Worker that gives each Decurix install a permanent public name under the
// company's domain (calm-river-4821.decurix.computer), so a person's phone link survives restarts.
//
// POST /v1/claim   {port}                 -> {hostname, key, token}   a new name, its own tunnel and DNS record
// POST /v1/token   {hostname, key, port}  -> {hostname, token}        the connector token again, on every app start
// POST /v1/release {hostname, key}        -> {released: true}         the install gives its name back
// POST /v1/admin/revoke {hostname}  (Authorization: Bearer ADMIN_KEY)  takes an abused name down
// GET  /v1/health                                                     liveness
// A daily cron removes names not seen for STALE_DAYS and old rate-limit rows.
//
// Every tunnel is remotely managed and routes its one hostname to http://127.0.0.1:<port> on the install's own
// machine - the access-code page (the gated listener); the app checks the port is that one before it asks.
// The install's key is shown once and only its SHA-256 is kept; the client IP is kept only as a hash, for the
// per-IP claim limit. The Cloudflare API token is a Worker secret and never leaves the Worker.

const API = 'https://api.cloudflare.com/client/v4';
const DAY_MS = 24 * 60 * 60 * 1000;

const ADJECTIVES = [
  'amber', 'brave', 'bright', 'calm', 'clear', 'cool', 'crisp', 'eager', 'early', 'fair', 'fresh', 'gentle',
  'golden', 'grand', 'green', 'happy', 'keen', 'kind', 'lively', 'lucky', 'merry', 'mild', 'neat', 'noble',
  'quick', 'quiet', 'rapid', 'ready', 'silver', 'smart', 'solid', 'steady', 'sunny', 'swift', 'tidy', 'warm',
  'wise', 'young', 'bold', 'proud',
];
const NOUNS = [
  'river', 'meadow', 'harbor', 'forest', 'garden', 'valley', 'island', 'summit', 'canyon', 'brook', 'cedar',
  'maple', 'willow', 'falcon', 'heron', 'otter', 'panda', 'tiger', 'lotus', 'orchid', 'comet', 'planet',
  'breeze', 'cloud', 'delta', 'ember', 'field', 'grove', 'lake', 'ocean', 'pebble', 'prairie', 'ridge',
  'shore', 'spring', 'stone', 'trail', 'wave', 'beacon', 'lantern',
];
const NAME_RE = /^[a-z]+-[a-z]+-\d{4}$/;

const SCHEMA = [
  `CREATE TABLE IF NOT EXISTS names (
     name TEXT PRIMARY KEY, tunnel_id TEXT NOT NULL, dns_id TEXT NOT NULL, key_hash TEXT NOT NULL,
     port INTEGER NOT NULL, created_ms INTEGER NOT NULL, last_seen_ms INTEGER NOT NULL)`,
  `CREATE TABLE IF NOT EXISTS claims (ip_hash TEXT NOT NULL, at_ms INTEGER NOT NULL)`,
  `CREATE INDEX IF NOT EXISTS claims_ip ON claims (ip_hash, at_ms)`,
];

// HttpError is a refusal with the status and message the client sees.
class HttpError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

// settings reads the Worker's vars, with defaults.
function settings(env) {
  return {
    domain: env.DOMAIN,
    accountId: env.ACCOUNT_ID,
    zoneId: env.ZONE_ID,
    maxNames: Number(env.MAX_NAMES || 150),
    claimsPerIpPerDay: Number(env.CLAIMS_PER_IP_PER_DAY || 3),
    staleDays: Number(env.STALE_DAYS || 90),
  };
}

const schemaReady = new WeakSet();

// ensureSchema creates the two tables once per database binding.
async function ensureSchema(db) {
  if (schemaReady.has(db)) return;
  for (const sql of SCHEMA) await db.prepare(sql).run();
  schemaReady.add(db);
}

// sha256 returns the hex SHA-256 of text.
async function sha256(text) {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, '0')).join('');
}

// sameHex compares two hex digests in constant time.
function sameHex(a, b) {
  if (typeof a !== 'string' || typeof b !== 'string' || a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i += 1) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

// randomKey returns 32 random bytes as base64url: the install's proof that a name is its own.
function randomKey() {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// randomName returns a name like calm-river-4821.
function randomName() {
  const [a, n, d] = crypto.getRandomValues(new Uint32Array(3));
  return `${ADJECTIVES[a % ADJECTIVES.length]}-${NOUNS[n % NOUNS.length]}-${1000 + (d % 9000)}`;
}

// readPort validates the local port the tunnel routes to; 8090 when absent.
function readPort(value) {
  if (value === undefined || value === null) return 8090;
  const port = Number(value);
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new HttpError(400, 'port must be a TCP port number');
  return port;
}

// readName checks a hostname is one of ours and returns its name part.
function readName(hostname, domain) {
  const suffix = `.${domain}`;
  if (typeof hostname !== 'string' || !hostname.endsWith(suffix)) throw new HttpError(404, 'unknown name');
  const name = hostname.slice(0, -suffix.length);
  if (!NAME_RE.test(name)) throw new HttpError(404, 'unknown name');
  return name;
}

// cf calls the Cloudflare API with the Worker's token and returns the result, throwing on any refusal.
async function cf(env, method, path, body) {
  const res = await fetch(`${API}${path}`, {
    method,
    // A secret piped in from PowerShell can carry a byte-order mark; Cloudflare then refuses the token.
    headers: { Authorization: `Bearer ${String(env.CF_API_TOKEN).replace(/^﻿/, '').trim()}`, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = null;
  try { data = await res.json(); } catch { /* not JSON */ }
  if (!res.ok || !data?.success) {
    const why = data?.errors?.map((e) => `${e.code}: ${e.message}`).join('; ') || `HTTP ${res.status}`;
    throw new Error(`Cloudflare ${method} ${path.split('?')[0]}: ${why}`);
  }
  return data.result;
}

// routeTunnel points the tunnel's one hostname at the install's local port; everything else is a 404.
async function routeTunnel(env, s, tunnelId, hostname, port) {
  await cf(env, 'PUT', `/accounts/${s.accountId}/cfd_tunnel/${tunnelId}/configurations`, {
    config: { ingress: [{ hostname, service: `http://127.0.0.1:${port}` }, { service: 'http_status:404' }] },
  });
}

// tunnelToken returns the token a connector runs the tunnel with.
function tunnelToken(env, s, tunnelId) {
  return cf(env, 'GET', `/accounts/${s.accountId}/cfd_tunnel/${tunnelId}/token`);
}

// removeName deletes a name's DNS record, tunnel and row; a part already gone is not an error.
async function removeName(env, s, row) {
  const quiet = (p) => p.catch((error) => console.warn(`remove ${row.name}: ${error.message}`));
  if (row.dns_id) await quiet(cf(env, 'DELETE', `/zones/${s.zoneId}/dns_records/${row.dns_id}`));
  if (row.tunnel_id) {
    await quiet(cf(env, 'DELETE', `/accounts/${s.accountId}/cfd_tunnel/${row.tunnel_id}/connections`));
    await quiet(cf(env, 'DELETE', `/accounts/${s.accountId}/cfd_tunnel/${row.tunnel_id}`));
  }
  await env.DB.prepare('DELETE FROM names WHERE name = ?').bind(row.name).run();
}

// claim gives a new install a name, a tunnel routed to its port, and a key to ask for the tunnel's token later.
async function claim(env, s, body, ip, now) {
  const port = readPort(body.port);
  const ipHash = await sha256(`claim:${ip}`);
  const recent = await env.DB.prepare('SELECT COUNT(*) AS n FROM claims WHERE ip_hash = ? AND at_ms > ?')
    .bind(ipHash, now - DAY_MS).first();
  if ((recent?.n || 0) >= s.claimsPerIpPerDay) throw new HttpError(429, 'too many new names from this network today');
  const total = await env.DB.prepare('SELECT COUNT(*) AS n FROM names').first();
  if ((total?.n || 0) >= s.maxNames) throw new HttpError(503, 'no names are available right now');

  let name = '';
  for (let i = 0; i < 5 && !name; i += 1) {
    const candidate = randomName();
    const taken = await env.DB.prepare('SELECT 1 AS x FROM names WHERE name = ?').bind(candidate).first();
    if (!taken) name = candidate;
  }
  if (!name) throw new HttpError(503, 'could not pick a free name, try again');

  const hostname = `${name}.${s.domain}`;
  const row = { name, tunnel_id: '', dns_id: '' };
  try {
    const tunnel = await cf(env, 'POST', `/accounts/${s.accountId}/cfd_tunnel`, { name: `decurix-link-${name}`, config_src: 'cloudflare' });
    row.tunnel_id = tunnel.id;
    await routeTunnel(env, s, tunnel.id, hostname, port);
    const record = await cf(env, 'POST', `/zones/${s.zoneId}/dns_records`, {
      type: 'CNAME', name: hostname, content: `${tunnel.id}.cfargotunnel.com`, proxied: true, ttl: 1,
      comment: 'Decurix link service',
    });
    row.dns_id = record.id;
    const key = randomKey();
    await env.DB.prepare(`INSERT INTO names (name, tunnel_id, dns_id, key_hash, port, created_ms, last_seen_ms)
      VALUES (?, ?, ?, ?, ?, ?, ?)`).bind(name, tunnel.id, record.id, await sha256(key), port, now, now).run();
    await env.DB.prepare('INSERT INTO claims (ip_hash, at_ms) VALUES (?, ?)').bind(ipHash, now).run();
    const token = await tunnelToken(env, s, tunnel.id);
    return { hostname, key, token };
  } catch (error) {
    await removeName(env, s, row);
    throw error;
  }
}

// ownedRow returns the row for hostname when key proves the caller owns it; any mismatch is the same 404.
async function ownedRow(env, s, body) {
  const name = readName(body.hostname, s.domain);
  const row = await env.DB.prepare('SELECT * FROM names WHERE name = ?').bind(name).first();
  if (!row || typeof body.key !== 'string' || !sameHex(await sha256(body.key), row.key_hash)) {
    throw new HttpError(404, 'unknown name');
  }
  return row;
}

// token hands an install its tunnel's token again, re-routing the tunnel if its port changed.
async function token(env, s, body, now) {
  const row = await ownedRow(env, s, body);
  const port = body.port === undefined ? row.port : readPort(body.port);
  const hostname = `${row.name}.${s.domain}`;
  if (port !== row.port) await routeTunnel(env, s, row.tunnel_id, hostname, port);
  await env.DB.prepare('UPDATE names SET last_seen_ms = ?, port = ? WHERE name = ?').bind(now, port, row.name).run();
  return { hostname, token: await tunnelToken(env, s, row.tunnel_id) };
}

// json writes a JSON response.
function json(status, value) {
  return new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' } });
}

// handle routes one request.
async function handle(request, env, now) {
  const s = settings(env);
  const url = new URL(request.url);
  if (request.method === 'GET' && url.pathname === '/v1/health') return json(200, { ok: true });
  if (request.method !== 'POST') throw new HttpError(405, 'POST only');
  if (!s.domain || !s.accountId || !s.zoneId || !env.CF_API_TOKEN) throw new HttpError(503, 'the link service is not configured');
  let body;
  try { body = await request.json(); } catch { throw new HttpError(400, 'body must be JSON'); }
  if (!body || typeof body !== 'object') throw new HttpError(400, 'body must be a JSON object');
  await ensureSchema(env.DB);

  switch (url.pathname) {
    case '/v1/claim':
      return json(200, await claim(env, s, body, request.headers.get('CF-Connecting-IP') || 'unknown', now));
    case '/v1/token':
      return json(200, await token(env, s, body, now));
    case '/v1/release':
      await removeName(env, s, await ownedRow(env, s, body));
      return json(200, { released: true });
    case '/v1/admin/revoke': {
      const auth = request.headers.get('Authorization') || '';
      if (!env.ADMIN_KEY || !sameHex(await sha256(auth), await sha256(`Bearer ${env.ADMIN_KEY}`))) throw new HttpError(403, 'forbidden');
      const name = readName(body.hostname, s.domain);
      const row = await env.DB.prepare('SELECT * FROM names WHERE name = ?').bind(name).first();
      if (!row) throw new HttpError(404, 'unknown name');
      await removeName(env, s, row);
      return json(200, { revoked: true });
    }
    default:
      throw new HttpError(404, 'not found');
  }
}

// sweep removes names unseen for staleDays and claim rows older than two days.
async function sweep(env, now) {
  const s = settings(env);
  await ensureSchema(env.DB);
  const { results = [] } = await env.DB.prepare('SELECT * FROM names WHERE last_seen_ms < ?').bind(now - s.staleDays * DAY_MS).all();
  for (const row of results) await removeName(env, s, row);
  await env.DB.prepare('DELETE FROM claims WHERE at_ms < ?').bind(now - 2 * DAY_MS).run();
  return results.length;
}

export default {
  async fetch(request, env) {
    try {
      return await handle(request, env, Date.now());
    } catch (error) {
      if (error instanceof HttpError) return json(error.status, { error: error.message });
      console.error(error.stack || error.message);
      return json(502, { error: 'the link service could not finish that, try again later' });
    }
  },
  async scheduled(event, env, ctx) {
    ctx.waitUntil(sweep(env, Date.now()));
  },
};

export { handle, sweep, randomName, NAME_RE, HttpError };
