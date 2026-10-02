'use strict';

const { spawn } = require('node:child_process');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');

const dashboard = 'http://127.0.0.1:8080';
const quickTunnelPattern = /https:\/\/[a-z0-9]+(?:-[a-z0-9]+)+\.trycloudflare\.com/i;

function lanPort(dataDir, env = process.env) {
  let value = env.DECURIX_LAN_PORT;
  if (!value) {
    try {
      const config = fs.readFileSync(path.join(dataDir, '.env'), 'utf8');
      value = config.match(/^\s*DECURIX_LAN_PORT\s*=\s*['"]?(\d+)['"]?\s*(?:#.*)?$/m)?.[1];
    } catch { /* The Go control plane also defaults to 8090. */ }
  }
  const port = Number(value || 8090);
  return Number.isInteger(port) && port > 0 && port < 65536 ? port : 8090;
}

function requestJSON(route, method = 'GET', body) {
  return new Promise((resolve, reject) => {
    const data = body === undefined ? '' : JSON.stringify(body);
    const req = http.request(`${dashboard}${route}`, {
      method,
      timeout: 5000,
      headers: data ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) } : {},
    }, (res) => {
      let result = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => { result = (result + chunk).slice(0, 8192); });
      res.on('error', reject);
      res.on('end', () => {
        if (res.statusCode < 200 || res.statusCode >= 300) return reject(new Error(`HTTP ${res.statusCode}`));
        try { resolve(JSON.parse(result)); } catch (error) { reject(error); }
      });
    });
    req.on('timeout', () => req.destroy(new Error('request timed out')));
    req.on('error', reject);
    req.end(data);
  });
}

// tunnelCommand(port) is the command line that runs cloudflared against the
// gated listener: main.js runs the copy shipped with the app.
//
// No LAN address is published. On a person's own computer the gated listener
// is bound to loopback (cmd/decurix nativeLANAddr: a listener on every
// interface makes Windows Firewall ask about it), so a phone on the same
// Wi-Fi cannot reach it and the tunnel is the phone's way in.
async function setupMobileAccess(dataDir, tunnelCommand) {
  const port = lanPort(dataDir);

  try {
    const named = await requestJSON('/api/cftunnel');
    if (named.configured) return null;
    const current = await requestJSON('/api/tunnel');
    if (current.lan_tunnel_url) return null;
  } catch (error) {
    console.warn('Could not check existing tunnels:', error.message);
    return null;
  }

  // Only the code-gated listener is exposed. The primary :8080 dashboard
  // remains local to this machine and its own worker containers.
  const { file, args } = tunnelCommand(port);
  const child = spawn(file, args, { windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });

  let output = '';
  let published = false;
  let stopping = false;
  let lastURL = '';
  let publish = Promise.resolve();
  const onOutput = (chunk) => {
    if (stopping) return;
    output = (output + chunk.toString()).slice(-8192);
    const match = output.match(quickTunnelPattern);
    if (!match || match[0] === lastURL) return;
    lastURL = match[0];
    const discoveredURL = lastURL;
    output = '';
    publish = publish.then(() => requestJSON('/api/tunnel', 'POST', { lan_tunnel_url: discoveredURL }))
      .then(() => { published = true; })
      .catch((error) => console.warn('Could not publish quick tunnel URL:', error.message));
  };
  child.stdout?.on('data', onOutput);
  child.stderr?.on('data', onOutput);
  child.on('error', (error) => console.warn('Quick tunnel unavailable:', error.message));

  const stop = async () => {
    if (stopping) return;
    stopping = true;
    child.kill();
    await publish;
    if (published) {
      await requestJSON('/api/tunnel', 'POST', { lan_tunnel_url: '' });
      published = false;
    }
  };
  child.once('close', () => {
    stop().catch((error) => console.warn('Could not clear quick tunnel URL:', error.message));
  });
  return { stop };
}

module.exports = { lanPort, quickTunnelPattern, setupMobileAccess };
