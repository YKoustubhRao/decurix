'use strict';

// The app's permanent public name (calm-river-4821.decurix.computer), from the link service (cloud/links).
//
// The first time the public link is turned on, the service makes this install a name and a tunnel routed to the
// gated port, and returns a key; the app keeps {hostname, key} in public-name.json in its data folder and, on every
// later start, trades the key for the tunnel's connector token. The token is never written down: main.js hands it
// to cloudflared through its environment, not its command line. A name the service no longer knows (released,
// revoked, unused for months) is replaced by a new one. DECURIX_LINK_SERVICE=off (env or .env) turns this off and
// the app uses a quick tunnel, as before; any failure here does the same (mobile-access.js).

const fs = require('node:fs');
const https = require('node:https');
const path = require('node:path');

const defaultService = 'https://links.decurix.computer';
const hostnamePattern = /^[a-z0-9]+(?:-[a-z0-9]+)*(?:\.[a-z0-9]+(?:-[a-z0-9]+)*)+$/;

// linkService returns the service's https origin, or '' when it is turned off or not a valid https address.
function linkService(dataDir, env = process.env) {
  let value = env.DECURIX_LINK_SERVICE;
  if (value === undefined) {
    try { value = fs.readFileSync(path.join(dataDir, '.env'), 'utf8').match(/^\s*DECURIX_LINK_SERVICE\s*=\s*['"]?([^'"\s#]*)/m)?.[1]; } catch { /* default */ }
  }
  if (!value) return defaultService;
  if (value === 'off') return '';
  try {
    const url = new URL(value);
    return url.protocol === 'https:' ? url.origin : '';
  } catch { return ''; }
}

// postJSON sends one JSON POST and resolves to {status, body}; a network failure rejects.
function postJSON(origin, route, body, timeoutMs = 15000) {
  return new Promise((resolve, reject) => {
    const data = JSON.stringify(body);
    const req = https.request(`${origin}${route}`, {
      method: 'POST', timeout: timeoutMs,
      headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) },
    }, (res) => {
      let text = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => { text = (text + chunk).slice(0, 16384); });
      res.on('error', reject);
      res.on('end', () => {
        let parsed = null;
        try { parsed = JSON.parse(text); } catch { /* not JSON */ }
        resolve({ status: res.statusCode, body: parsed });
      });
    });
    req.on('timeout', () => req.destroy(new Error('the link service did not answer')));
    req.on('error', reject);
    req.end(data);
  });
}

function nameFile(dataDir) {
  return path.join(dataDir, 'public-name.json');
}

// readSaved returns the saved {hostname, key}, or null when there is none or it is not well formed.
function readSaved(dataDir) {
  try {
    const saved = JSON.parse(fs.readFileSync(nameFile(dataDir), 'utf8'));
    if (typeof saved?.hostname === 'string' && hostnamePattern.test(saved.hostname) && typeof saved.key === 'string' && saved.key) {
      return { hostname: saved.hostname, key: saved.key };
    }
  } catch { /* none */ }
  return null;
}

// save writes {hostname, key} readable by this user only, through a temporary file so a crash leaves the old one.
function save(dataDir, hostname, key) {
  fs.mkdirSync(dataDir, { recursive: true });
  const file = nameFile(dataDir);
  const temp = `${file}.tmp`;
  fs.writeFileSync(temp, JSON.stringify({ hostname, key }) + '\n', { encoding: 'utf8', mode: 0o600 });
  fs.renameSync(temp, file);
}

function failure(route, res) {
  const why = typeof res.body?.error === 'string' ? ` - ${res.body.error.slice(0, 200)}` : '';
  return new Error(`link service ${route}: HTTP ${res.status}${why}`);
}

// namedLink returns {url, token} for this install's permanent name, claiming one the first time, or null when the
// service is turned off. It throws when the service cannot be reached or refuses; the caller then falls back.
async function namedLink(dataDir, port, { service = linkService(dataDir), post = postJSON, log = () => {} } = {}) {
  if (!service) return null;
  const saved = readSaved(dataDir);
  if (saved) {
    const res = await post(service, '/v1/token', { hostname: saved.hostname, key: saved.key, port });
    if (res.status === 200 && typeof res.body?.token === 'string' && res.body.token) {
      return { url: `https://${saved.hostname}`, token: res.body.token };
    }
    if (res.status !== 404) throw failure('/v1/token', res);
    log(`Public name ${saved.hostname} is no longer this computer's; asking for a new one`);
    try { fs.unlinkSync(nameFile(dataDir)); } catch { /* already gone */ }
  }
  const res = await post(service, '/v1/claim', { port });
  const { hostname, key, token } = res.body || {};
  if (res.status !== 200 || typeof hostname !== 'string' || !hostnamePattern.test(hostname) ||
      typeof key !== 'string' || !key || typeof token !== 'string' || !token) {
    throw failure('/v1/claim', res);
  }
  save(dataDir, hostname, key);
  log(`Public name: ${hostname}`);
  return { url: `https://${hostname}`, token };
}

module.exports = { defaultService, linkService, namedLink, nameFile };
