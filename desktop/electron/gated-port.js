'use strict';

// The one decision about what this app may expose beyond this computer.
//
// The control plane has two listeners: the primary one (127.0.0.1:8080) has no
// login and host-level power, and must never leave this machine; the gated one
// (DECURIX_LAN_PORT, 8090 by default) asks for the access code, and is the
// only one a LAN forward or a public tunnel may carry. Every forward and every
// tunnel calls gatedPort before it changes anything: a port read from .env is
// only a claim, so the listener on it must be seen serving the access-code
// page first. A misconfigured .env (DECURIX_LAN_PORT naming the primary
// listener, or another program's port) then exposes nothing.

const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');

const PRIMARY_PORT = 8080;
const DEFAULT_GATED_PORT = 8090;
const ACCESS_PAGE_MARK = '/__decurix_access/login';

// lanPort is the gated port .env names (DECURIX_LAN_PORT), else the default.
// It is what the person configured, not yet a checked fact: see gatedPort.
function lanPort(dataDir, env = process.env) {
  let value = env.DECURIX_LAN_PORT;
  if (!value) {
    try {
      const config = fs.readFileSync(path.join(dataDir, '.env'), 'utf8');
      value = config.match(/^\s*DECURIX_LAN_PORT\s*=\s*['"]?(\d+)['"]?\s*(?:#.*)?\r?$/m)?.[1];
    } catch { /* The Go control plane also defaults to 8090. */ }
  }
  const port = Number(value || DEFAULT_GATED_PORT);
  return Number.isInteger(port) && port > 0 && port < 65536 ? port : DEFAULT_GATED_PORT;
}

// servesAccessPage resolves whether 127.0.0.1:port answers a GET of / with the
// access-code page, the page only the gated listener serves. No code is sent.
function servesAccessPage(port, { timeout = 4000, get = http.get } = {}) {
  return new Promise((resolve) => {
    let settled = false;
    const done = (value) => { if (!settled) { settled = true; resolve(value); } };
    const req = get(`http://127.0.0.1:${port}/`, { timeout }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => { body = (body + chunk).slice(0, 65536); });
      res.on('error', () => done(false));
      res.on('end', () => done(res.statusCode === 200 && body.includes(ACCESS_PAGE_MARK)));
    });
    req.on('timeout', () => { req.destroy(); done(false); });
    req.on('error', () => done(false));
  });
}

// gatedPort resolves with port when it is the access-code-gated listener, and
// rejects otherwise: not a port, the primary listener's port, or a listener
// that does not serve the access-code page. Callers expose nothing on a
// rejection.
async function gatedPort(port, { primaryPort = PRIMARY_PORT, probe = servesAccessPage } = {}) {
  if (!Number.isInteger(port) || port <= 0 || port >= 65536) throw new Error(`not a port: ${port}`);
  if (port === primaryPort) {
    throw new Error(`port ${port} is the primary listener, which has no login; only the access-code-gated listener may be exposed (check DECURIX_LAN_PORT in .env)`);
  }
  if (!await probe(port)) {
    throw new Error(`127.0.0.1:${port} does not serve the access-code page, so it is not the gated listener; nothing was exposed`);
  }
  return port;
}

module.exports = { ACCESS_PAGE_MARK, DEFAULT_GATED_PORT, PRIMARY_PORT, gatedPort, lanPort, servesAccessPage };
