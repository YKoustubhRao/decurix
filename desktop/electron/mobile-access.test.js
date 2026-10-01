'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const EventEmitter = require('node:events');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'mobile-access.js'), 'utf8');

function harness(namedConfigured = false) {
  const state = { lan_url: '', lan_tunnel_url: '' };
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
  const dgram = {
    createSocket() {
      const socket = new EventEmitter();
      socket.connect = (_port, _host, callback) => callback();
      socket.address = () => ({ address: '192.168.1.5' });
      socket.close = () => {};
      return socket;
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
    'node:dgram': dgram,
    'node:fs': { readFileSync: () => 'DECURIX_LAN_PORT=8123\n' },
    'node:http': http,
    'node:path': path,
  };
  const module = { exports: {} };
  vm.runInNewContext(source, {
    require: (name) => modules[name],
    module,
    exports: module.exports,
    process: { env: {} },
    Buffer,
    setTimeout,
    console: { warn: (...args) => warnings.push(args) },
  }, { filename: 'mobile-access.js' });
  return { api: module.exports, state, spawns, warnings, get child() { return child; } };
}

test('publishes only the gated LAN and quick tunnel URLs, then clears the temporary link', async () => {
  const h = harness();
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix');
  assert.equal(h.state.lan_url, 'http://192.168.1.5:8123');
  assert.equal(h.spawns.length, 1);
  assert.equal(h.spawns[0].file, 'cloudflared');
  assert.deepEqual(Array.from(h.spawns[0].args), ['tunnel', '--protocol', 'http2', '--url', 'http://127.0.0.1:8123']);
  h.child.stderr.emit('data', 'https://blue-');
  h.child.stderr.emit('data', 'flower.trycloudflare.com');
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.state.lan_tunnel_url, 'https://blue-flower.trycloudflare.com');
  await access.stop();
  assert.equal(h.state.lan_tunnel_url, '');
  assert.equal(h.warnings.length, 0);
});

test('does not start a quick tunnel when a named tunnel is configured', async () => {
  const h = harness(true);
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix');
  assert.equal(access, null);
  assert.equal(h.spawns.length, 0);
});

test('ignores Cloudflare API hostnames in tunnel output', () => {
  const h = harness();
  assert.equal(h.api.quickTunnelPattern.test('https://api.trycloudflare.com'), false);
  assert.equal(h.api.quickTunnelPattern.test('https://blue-flower.trycloudflare.com'), true);
  assert.equal(h.api.lanPort('/profile/AppData/decurix', {}), 8123);
});


test('missing cloudflared leaves LAN access available without crashing', async () => {
  const h = harness();
  const access = await h.api.setupMobileAccess('/profile/AppData/decurix');
  h.child.emit('error', new Error('ENOENT'));
  h.child.emit('close', -1);
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.state.lan_url, 'http://192.168.1.5:8123');
  assert.equal(h.state.lan_tunnel_url, '');
  assert.match(h.warnings[0][1], /ENOENT/);
  await access.stop();
});
