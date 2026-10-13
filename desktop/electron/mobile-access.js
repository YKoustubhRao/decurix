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
// gated listener: main.js runs the copy shipped with the app. The plain-IP link
// is not made here: windows-lan.js and main.js's reportLanAddress do that.
//
// The quick tunnel is this app's own. A link the control plane already holds
// is not trusted: the control plane outlives the window, and a link left from a
// tunnel that has since ended is dead - an earlier version saw it, started no
// tunnel, and the Mobile menu handed out that dead link for good. So the old
// link is cleared at once and a new tunnel started; a tunnel that ends on its
// own (the network dropped) takes its link with it and is started again.
async function setupMobileAccess(dataDir, tunnelCommand, { retryMs = 5000, maxRetryMs = 60000 } = {}) {
  const port = lanPort(dataDir);

  try {
    const named = await requestJSON('/api/cftunnel');
    if (named.configured) return null;
  } catch (error) {
    console.warn('Could not check existing tunnels:', error.message);
    return null;
  }

  let stopping = false;
  let child = null;
  let timer = null;
  let delay = retryMs;
  let lastURL = '';
  let published = true; // whatever link the control plane holds is cleared first
  let publish = Promise.resolve();
  const setURL = (url) => {
    publish = publish.then(() => requestJSON('/api/tunnel', 'POST', { lan_tunnel_url: url }))
      .then(() => { published = Boolean(url); })
      .catch((error) => console.warn('Could not publish quick tunnel URL:', error.message));
  };
  setURL('');

  // Only the code-gated listener is exposed. The primary :8080 dashboard
  // remains local to this machine and its own worker containers.
  const launch = () => {
    timer = null;
    if (stopping) return;
    const { file, args } = tunnelCommand(port);
    let output = '';
    const tunnel = spawn(file, args, { windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
    child = tunnel;
    const onOutput = (chunk) => {
      if (stopping) return;
      output = (output + chunk.toString()).slice(-8192);
      const match = output.match(quickTunnelPattern);
      if (!match || match[0] === lastURL) return;
      lastURL = match[0];
      output = '';
      delay = retryMs;
      setURL(lastURL);
    };
    tunnel.stdout?.on('data', onOutput);
    tunnel.stderr?.on('data', onOutput);
    tunnel.on('error', (error) => console.warn('Quick tunnel unavailable:', error.message));
    tunnel.once('close', () => {
      if (stopping || child !== tunnel) return;
      if (lastURL) { lastURL = ''; setURL(''); }
      timer = setTimeout(launch, delay);
      delay = Math.min(delay * 2, maxRetryMs);
    });
  };
  launch();

  const stop = async () => {
    if (stopping) return;
    stopping = true;
    if (timer) clearTimeout(timer);
    child?.kill();
    await publish;
    if (published) {
      await requestJSON('/api/tunnel', 'POST', { lan_tunnel_url: '' });
      published = false;
    }
  };
  return { stop };
}

module.exports = { lanPort, quickTunnelPattern, setupMobileAccess };
