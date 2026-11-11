'use strict';

const { spawn } = require('node:child_process');
const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const dns = require('node:dns');
const path = require('node:path');
const { gatedPort, lanPort } = require('./gated-port');

const dashboard = 'http://127.0.0.1:8080';
const quickTunnelPattern = /https:\/\/[a-z0-9]+(?:-[a-z0-9]+)+\.trycloudflare\.com/i;
const alternativePattern = /https:\/\/[a-z0-9-]+\.(?:lhr\.life|lhr\.rocks|localhost\.run)\b/i;

// A newly allocated hostname can be negatively cached by the router/ISP.
// Retry failed system lookups against Cloudflare DNS for this request only.
function tunnelLookup(hostname, options, callback) {
  dns.lookup(hostname, options, (error, address, family) => {
    if (!error || !['ENOTFOUND', 'EAI_AGAIN'].includes(error.code)) return callback(error, address, family);
    const resolver = new dns.Resolver({ timeout: 2500, tries: 1 });
    resolver.setServers(['1.1.1.1', '1.0.0.1']);
    resolver.resolve4(hostname, (fallbackError, addresses) => {
      if (fallbackError || !addresses?.length) return callback(fallbackError || error);
      if (options.all) return callback(null, addresses.map((value) => ({ address: value, family: 4 })));
      callback(null, addresses[0], 4);
    });
  });
}

function tunnelProvider(dataDir, env = process.env) {
  let value = env.DECURIX_TUNNEL_PROVIDER;
  if (!value) {
    try { value = fs.readFileSync(path.join(dataDir, '.env'), 'utf8').match(/^\s*DECURIX_TUNNEL_PROVIDER\s*=\s*['"]?([\w-]+)/m)?.[1]; } catch { /* default */ }
  }
  return ['auto', 'cloudflare', 'localhost-run'].includes(value) ? value : 'cloudflare';
}

// An unauthenticated request must reach our actual access-code page, rather
// than a provider's error page. Never send the pairing code to a health check.
function probeURL(url, log = () => {}) {
  return new Promise((resolve) => {
    const req = https.get(url, { timeout: 8000, family: 4, lookup: tunnelLookup }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => { body = (body + chunk).slice(0, 65536); });
      res.on('error', () => resolve(false));
      res.on('end', () => {
        const ok = res.statusCode === 200 && body.includes('/__decurix_access/login');
        if (!ok) log(`Public link check: HTTP ${res.statusCode}, access page ${body.includes('/__decurix_access/login')}`);
        resolve(ok);
      });
    });
    req.on('error', (error) => { log(`Public link check: ${error.code || error.message}`); resolve(false); });
    req.on('timeout', () => req.destroy());
  });
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
//
// permanent(port), when given, is tried first on every start: it returns this
// install's permanent name and connector token from the link service
// (public-name.js), and permanentCommand(token) runs cloudflared for it. When it
// returns null or fails, that start uses a quick tunnel instead.
async function setupMobileAccess(dataDir, tunnelCommand, {
  retryMs = 5000, maxRetryMs = 60000, startupMs = 45000, checkMs = 15000,
  probe, provider = tunnelProvider(dataDir), log = () => {}, checkPort = gatedPort, portTries = 3,
  permanent = null, permanentCommand = null, firstCheckMs = 3000,
} = {}) {
  probe = probe || ((url) => probeURL(url, log));
  const port = lanPort(dataDir);

  try {
    const named = await requestJSON('/api/cftunnel');
    if (named.configured) return null;
  } catch (error) {
    console.warn('Could not check existing tunnels:', error.message);
    return null;
  }

  // Only the access-code-gated listener may be tunnelled: the port .env names
  // is checked to be it before anything starts (gated-port.js). The control
  // plane may still be coming up, so a few tries; then no tunnel at all.
  for (let attempt = 1; ; attempt += 1) {
    try { await checkPort(port); break; } catch (error) {
      if (attempt >= portTries) {
        log(`Public link: not started - ${error.message}`);
        try { await requestJSON('/api/tunnel', 'POST', { lan_tunnel_url: '' }); } catch { /* nothing held */ }
        return null;
      }
      await new Promise((resolve) => setTimeout(resolve, retryMs));
    }
  }

  let stopping = false;
  let child = null;
  let timer = null;
  let healthTimer = null;
  let startupTimer = null;
  let currentProvider = provider === 'localhost-run' ? provider : 'cloudflare';
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
  const launch = async () => {
    timer = null;
    if (stopping) return;
    let link = null;
    if (permanent && permanentCommand) {
      try { link = await permanent(port); } catch (error) { log(`Public name unavailable (${error.message}); using a quick tunnel`); }
      if (stopping) return;
    }
    const { file, args, env } = link ? permanentCommand(link.token) : tunnelCommand(port, currentProvider);
    log(link ? `Public link: starting ${link.url} on gated port ${port}` : `Public link: starting ${currentProvider} on gated port ${port}`);
    let output = '';
    let candidate = '';
    let checking = false;
    let failures = 0;
    const tunnel = spawn(file, args, { windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'], ...(env ? { env } : {}) });
    child = tunnel;
    const retire = () => {
      if (stopping || child !== tunnel) return;
      child = null;
      clearTimeout(startupTimer);
      clearTimeout(healthTimer);
      if (lastURL) { lastURL = ''; setURL(''); }
      tunnel.kill();
      // A live cloudflared can retry forever when DNS or port 7844 is
      // blocked. Switch providers even when that process never exits.
      if (provider === 'auto') currentProvider = currentProvider === 'cloudflare' ? 'localhost-run' : 'cloudflare';
      timer = setTimeout(launch, delay);
      delay = Math.min(delay * 2, maxRetryMs);
    };
    const check = async () => {
      if (checking || !candidate || stopping || child !== tunnel) return;
      checking = true;
      let ok = false;
      try { ok = await probe(candidate); } catch { /* retry */ }
      checking = false;
      if (stopping || child !== tunnel) return;
      if (ok) {
        failures = 0;
        clearTimeout(startupTimer);
        delay = retryMs;
        if (lastURL !== candidate) { lastURL = candidate; setURL(candidate); log(`Public link ready: ${candidate}`); }
      } else if (lastURL && ++failures >= 3) {
        console.warn('Public tunnel no longer reachable; reconnecting:', currentProvider);
        retire();
        return;
      }
      healthTimer = setTimeout(check, checkMs);
    };
    startupTimer = setTimeout(() => {
      console.warn('Public tunnel did not become reachable; reconnecting:', currentProvider);
      retire();
    }, startupMs);
    const onOutput = (chunk) => {
      // A permanent name is known up front; its connector's log has no link to find.
      if (stopping || link) return;
      output = (output + chunk.toString()).slice(-8192);
      const match = output.match(currentProvider === 'cloudflare' ? quickTunnelPattern : alternativePattern);
      if (!match || match[0] === candidate) return;
      candidate = match[0];
      log(`Public link: checking ${candidate}`);
      output = '';
      check();
    };
    tunnel.stdout?.on('data', onOutput);
    tunnel.stderr?.on('data', onOutput);
    tunnel.on('error', (error) => console.warn('Quick tunnel unavailable:', error.message));
    tunnel.once('close', () => {
      retire();
    });
    if (link) {
      candidate = link.url;
      log(`Public link: checking ${candidate}`);
      healthTimer = setTimeout(check, firstCheckMs);
    }
  };
  launch();

  const stop = async () => {
    if (stopping) return;
    stopping = true;
    if (timer) clearTimeout(timer);
    clearTimeout(startupTimer);
    clearTimeout(healthTimer);
    child?.kill();
    await publish;
    if (published) {
      await requestJSON('/api/tunnel', 'POST', { lan_tunnel_url: '' });
      published = false;
    }
  };
  return { stop };
}

module.exports = { lanPort, quickTunnelPattern, alternativePattern, tunnelProvider, tunnelLookup, setupMobileAccess };
