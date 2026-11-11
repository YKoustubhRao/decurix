'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const EventEmitter = require('node:events');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'mobile-access.js'), 'utf8');
const gatedSource = fs.readFileSync(path.join(__dirname, 'gated-port.js'), 'utf8');

// load runs a module's source with the given modules standing in for require.
function load(text, modules, filename, extra = {}) {
  const module = { exports: {} };
  vm.runInNewContext(text, {
    require: (name) => modules[name], module, exports: module.exports,
    process: { env: {} }, Buffer, setTimeout, clearTimeout, ...extra,
  }, { filename });
  return module.exports;
}

// What main.js passes: the cloudflared shipped with the app.
const tunnelCommand = (port) => ({ file: 'cloudflared.exe', args: ['tunnel', '--url', `http://127.0.0.1:${port}`] });

function harness(namedConfigured = false, held = '', dns = {}) {
  const state = { lan_url: '', lan_tunnel_url: held };
  const spawns = [];
  const warnings = [];
  let child;
  const http = {
    request(url, options, callback) {
      const request = new EventEmitter();
      request.end = (data) => queueMicrotask(() => {
        const route = new URL(url).pathname;
        if (route === '/api/tunnel' && options.method === 'POST') Object.assign(state, JSON.parse(data));
        const body = route === '/api/cftunnel' ? { configured: namedConfigured } : state;
        const response = new EventEmitter();
        response.statusCode = 200;
        response.setEncoding = () => {};
        callback(response);
        response.emit('data', JSON.stringify(body));
        response.emit('end');
      });
      request.destroy = () => {};
      return request;
    },
  };
  const childProcess = {
    spawn(file, args, options) {
      spawns.push({ file, args, options });
      child = new EventEmitter();
      child.stdout = new EventEmitter();
      child.stderr = new EventEmitter();
      child.kill = () => { queueMicrotask(() => child.emit('close', 0)); };
      return child;
    },
  };
  const modules = {
    'node:child_process': childProcess,
    'node:fs': { readFileSync: () => 'DECURIX_LAN_PORT=8123\n' },
    'node:http': http,
    'node:https': {},
    'node:dns': dns,
    'node:path': path,
  };
  modules['./gated-port'] = load(gatedSource, modules, 'gated-port.js');
  const api = load(source, modules, 'mobile-access.js', { console: { warn: (...args) => warnings.push(args) } });
  const setup = api.setupMobileAccess;
  // The gated-port check is its own test below; here the port counts as gated.
  api.setupMobileAccess = (dir, command, options) => setup(dir, command, { probe: async () => true, checkPort: async (port) => port, provider: 'cloudflare', ...options });
  return { api, state, spawns, warnings, get child() { return child; } };
}

test('publishes only the gated quick tunnel URL, then clears the temporary link', async () => {
  const h = harness();
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand);
  assert.equal(h.spawns.length, 1);
  assert.equal(h.spawns[0].file, 'cloudflared.exe');
  assert.deepEqual(Array.from(h.spawns[0].args), ['tunnel', '--url', 'http://127.0.0.1:8123']);
  h.child.stderr.emit('data', 'https://blue-');
  h.child.stderr.emit('data', 'flower.trycloudflare.com');
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.state.lan_tunnel_url, 'https://blue-flower.trycloudflare.com');
  await access.stop();
  assert.equal(h.state.lan_tunnel_url, '');
  assert.equal(h.warnings.length, 0);
});

test('publishes no LAN address: the gated listener is on loopback', async () => {
  const h = harness();
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand);
  assert.equal(h.state.lan_url, '');
  await access.stop();
});

test('does not start a quick tunnel when a named tunnel is configured', async () => {
  const h = harness(true);
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand);
  assert.equal(access, null);
  assert.equal(h.spawns.length, 0);
});

test('a link the control plane still holds is cleared and a new tunnel started', async () => {
  const h = harness(false, 'https://old-dead.trycloudflare.com');
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand);
  assert.equal(h.spawns.length, 1);
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.state.lan_tunnel_url, '');
  await access.stop();
});

test('a tunnel that ends on its own drops its link and is started again', async () => {
  const h = harness();
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand, { retryMs: 1 });
  h.child.stderr.emit('data', 'https://blue-flower.trycloudflare.com');
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.state.lan_tunnel_url, 'https://blue-flower.trycloudflare.com');
  h.child.emit('close', 1);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(h.state.lan_tunnel_url, '');
  assert.equal(h.spawns.length, 2);
  h.child.stderr.emit('data', 'https://green-tree.trycloudflare.com');
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.state.lan_tunnel_url, 'https://green-tree.trycloudflare.com');
  await access.stop();
  assert.equal(h.state.lan_tunnel_url, '');
  assert.equal(h.spawns.length, 2);
});

test('ignores Cloudflare API hostnames in tunnel output', () => {
  const h = harness();
  assert.equal(h.api.quickTunnelPattern.test('https://api.trycloudflare.com'), false);
  assert.equal(h.api.quickTunnelPattern.test('https://blue-flower.trycloudflare.com'), true);
  assert.equal(h.api.lanPort('/profile/AppData/decurix', {}), 8123);
});

test('a tunnel that cannot start leaves the app running', async () => {
  const h = harness();
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand);
  h.child.emit('error', new Error('ENOENT'));
  h.child.emit('close', -1);
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.state.lan_tunnel_url, '');
  assert.match(h.warnings[0][1], /ENOENT/);
  await access.stop();
});

const settle = async () => { for (let i = 0; i < 3; i += 1) await new Promise((resolve) => setImmediate(resolve)); };
const until = async (condition) => {
  const end = Date.now() + 2000;
  while (!condition() && Date.now() < end) await new Promise((resolve) => setTimeout(resolve, 10));
  assert.ok(condition(), 'condition did not become true');
};

test('failed system DNS falls back without changing machine DNS settings', async () => {
  const servers = [];
  const h = harness(false, '', {
    lookup: (_host, _options, done) => done(Object.assign(new Error('cached NXDOMAIN'), { code: 'ENOTFOUND' })),
    Resolver: class {
      setServers(values) { servers.push(...values); }
      resolve4(_host, done) { done(null, ['104.16.230.132']); }
    },
  });
  const result = await new Promise((resolve, reject) => h.api.tunnelLookup('new.trycloudflare.com', { all: true }, (error, values) => error ? reject(error) : resolve(values)));
  assert.deepEqual(servers, ['1.1.1.1', '1.0.0.1']);
  assert.equal(result[0].address, '104.16.230.132');
  assert.equal(result[0].family, 4);
});

test('a working system DNS answer is used without contacting another resolver', async () => {
  const h = harness(false, '', {
    lookup: (_host, _options, done) => done(null, '192.0.2.1', 4),
    Resolver: class { constructor() { throw new Error('unexpected fallback'); } },
  });
  const result = await new Promise((resolve, reject) => h.api.tunnelLookup('new.trycloudflare.com', {}, (error, address) => error ? reject(error) : resolve(address)));
  assert.equal(result, '192.0.2.1');
});

test('does not advertise a URL until its access-code page is reachable', async () => {
  const h = harness();
  let ready = false;
  const access = await h.api.setupMobileAccess('/data', tunnelCommand, { checkMs: 5, probe: async () => ready });
  try {
    h.child.stderr.emit('data', 'https://blue-flower.trycloudflare.com');
    await settle();
    assert.equal(h.state.lan_tunnel_url, '');
    ready = true;
    await until(() => Boolean(h.state.lan_tunnel_url));
    assert.equal(h.state.lan_tunnel_url, 'https://blue-flower.trycloudflare.com');
  } finally { await access.stop(); }
});

test('a Cloudflare process stuck on DNS falls back even without exiting', async () => {
  const h = harness();
  const providers = [];
  const access = await h.api.setupMobileAccess('/data', (port, provider) => {
    providers.push(provider);
    return tunnelCommand(port);
  }, { provider: 'auto', startupMs: 100, retryMs: 1 });
  try {
    await until(() => providers.length >= 2);
    assert.deepEqual(providers.slice(0, 2), ['cloudflare', 'localhost-run']);
    h.child.stdout.emit('data', 'tunneled with tls termination, https://example123.lhr.life');
    await settle();
    assert.equal(h.state.lan_tunnel_url, 'https://example123.lhr.life');
  } finally { await access.stop(); }
});

test('losing public reachability removes the URL and reconnects', async () => {
  const h = harness();
  let ready = true;
  const access = await h.api.setupMobileAccess('/data', tunnelCommand, { checkMs: 5, retryMs: 1, probe: async () => ready });
  try {
    h.child.stderr.emit('data', 'https://blue-flower.trycloudflare.com');
    await settle();
    assert.equal(h.state.lan_tunnel_url, 'https://blue-flower.trycloudflare.com');
    ready = false;
    await until(() => h.spawns.length >= 2);
    assert.equal(h.state.lan_tunnel_url, '');
    assert.equal(h.spawns.length, 2);
  } finally { await access.stop(); }
});

test('a late successful probe cannot republish a stopped tunnel', async () => {
  const h = harness();
  let finish;
  const access = await h.api.setupMobileAccess('/data', tunnelCommand, { probe: () => new Promise((resolve) => { finish = resolve; }) });
  h.child.stderr.emit('data', 'https://blue-flower.trycloudflare.com');
  await access.stop();
  finish(true);
  await settle();
  assert.equal(h.state.lan_tunnel_url, '');
});

test('no tunnel starts on a port that is not the gated listener, and any held link is cleared', async () => {
  const h = harness(false, 'https://old-dead.trycloudflare.com');
  const lines = [];
  const checked = [];
  const access = await h.api.setupMobileAccess('/data', tunnelCommand, {
    retryMs: 1, log: (line) => lines.push(line),
    checkPort: async (port) => { checked.push(port); throw new Error('not the gated listener'); },
  });
  assert.equal(access, null);
  assert.equal(h.spawns.length, 0);
  assert.deepEqual(checked, [8123, 8123, 8123]);
  assert.equal(h.state.lan_tunnel_url, '');
  assert.match(lines.join(' '), /not started - not the gated listener/);
});

test('a gated listener that answers on a later try still gets its tunnel', async () => {
  const h = harness();
  let tries = 0;
  const access = await h.api.setupMobileAccess('/data', tunnelCommand, {
    retryMs: 1, checkPort: async (port) => { tries += 1; if (tries < 2) throw new Error('not up yet'); return port; },
  });
  assert.equal(h.spawns.length, 1);
  await access.stop();
});

// What main.js passes for the permanent name: the token in the environment only.
const permanentCommand = (token) => ({ file: 'cloudflared.exe', args: ['tunnel', '--no-autoupdate', 'run'], env: { TUNNEL_TOKEN: token } });

test('a permanent name is used first: its connector gets the token only in its environment, and its link is published', async () => {
  const h = harness();
  const asked = [];
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand, {
    permanentCommand, firstCheckMs: 1,
    permanent: async (port) => { asked.push(port); return { url: 'https://calm-river-4821.decurix.computer', token: 'tok-1' }; },
  });
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.deepEqual(asked, [8123]);
  assert.equal(h.spawns.length, 1);
  assert.equal(h.spawns[0].args.join(' ').includes('tok-1'), false);
  assert.equal(h.spawns[0].options.env.TUNNEL_TOKEN, 'tok-1');
  // Its log never changes the link, even if it names another address.
  h.child.stderr.emit('data', 'https://blue-flower.trycloudflare.com');
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(h.state.lan_tunnel_url, 'https://calm-river-4821.decurix.computer');
  await access.stop();
  assert.equal(h.state.lan_tunnel_url, '');
});

test('when the link service fails, that start uses a quick tunnel', async () => {
  const h = harness();
  const logs = [];
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand, {
    permanentCommand, log: (line) => logs.push(line),
    permanent: async () => { throw new Error('link service /v1/claim: HTTP 503'); },
  });
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(h.spawns.length, 1);
  assert.deepEqual(Array.from(h.spawns[0].args), ['tunnel', '--url', 'http://127.0.0.1:8123']);
  assert.ok(logs.some((line) => line.includes('HTTP 503')));
  h.child.stderr.emit('data', 'https://blue-flower.trycloudflare.com');
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.state.lan_tunnel_url, 'https://blue-flower.trycloudflare.com');
  await access.stop();
});

test('a permanent connector that exits is started again with a fresh token, under the same link', async () => {
  const h = harness();
  let n = 0;
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand, {
    permanentCommand, firstCheckMs: 1, retryMs: 1,
    permanent: async () => ({ url: 'https://calm-river-4821.decurix.computer', token: `tok-${++n}` }),
  });
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(h.state.lan_tunnel_url, 'https://calm-river-4821.decurix.computer');
  h.child.emit('close', 1);
  await new Promise((resolve) => setTimeout(resolve, 30));
  assert.equal(h.spawns.length, 2);
  assert.equal(h.spawns[1].options.env.TUNNEL_TOKEN, 'tok-2');
  assert.equal(h.state.lan_tunnel_url, 'https://calm-river-4821.decurix.computer');
  await access.stop();
});

test('stopping while the link service is still answering starts no connector', async () => {
  const h = harness();
  let answer;
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix', tunnelCommand, {
    permanentCommand,
    permanent: () => new Promise((resolve) => { answer = resolve; }),
  });
  await access.stop();
  answer({ url: 'https://calm-river-4821.decurix.computer', token: 'tok' });
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(h.spawns.length, 0);
});
