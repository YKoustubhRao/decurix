'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const EventEmitter = require('node:events');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { gatedPort, lanPort, servesAccessPage } = require('./gated-port');

// answer is a stand-in for http.get that replies with status and body (or
// fails with an error when status is null).
const answer = (status, body = '') => (_url, _options, callback) => {
  const req = new EventEmitter();
  req.destroy = () => {};
  queueMicrotask(() => {
    if (status === null) { req.emit('error', new Error('connection refused')); return; }
    const res = new EventEmitter();
    res.statusCode = status;
    res.setEncoding = () => {};
    callback(res);
    res.emit('data', body);
    res.emit('end');
  });
  return req;
};

test('the primary listener\'s port is never accepted, whatever it serves', async () => {
  let probed = false;
  await assert.rejects(gatedPort(8080, { probe: async () => { probed = true; return true; } }), /primary listener/);
  assert.equal(probed, false);
});

test('something that is not a port is refused before any probe', async () => {
  for (const value of [0, -1, 65536, 8090.5, NaN, '8090']) {
    await assert.rejects(gatedPort(value, { probe: async () => true }), /not a port/);
  }
});

test('a port is accepted only once it is seen serving the access-code page', async () => {
  await assert.rejects(gatedPort(8090, { probe: async () => false }), /does not serve the access-code page/);
  assert.equal(await gatedPort(8090, { probe: async () => true }), 8090);
});

test('the probe needs a 200 carrying the access-code page', async () => {
  assert.equal(await servesAccessPage(8090, { get: answer(200, '<form action="/__decurix_access/login">') }), true);
  assert.equal(await servesAccessPage(8090, { get: answer(200, '<html>a dashboard with no login</html>') }), false);
  assert.equal(await servesAccessPage(8090, { get: answer(302, '/__decurix_access/login') }), false);
  assert.equal(await servesAccessPage(8090, { get: answer(null) }), false);
});

test('lanPort reads DECURIX_LAN_PORT from .env, CRLF and comments included, else 8090', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gated-port-'));
  try {
    assert.equal(lanPort(dir, {}), 8090);
    fs.writeFileSync(path.join(dir, '.env'), 'A=1\r\nDECURIX_LAN_PORT="8123" # phone\r\n');
    assert.equal(lanPort(dir, {}), 8123);
    assert.equal(lanPort(dir, { DECURIX_LAN_PORT: '8124' }), 8124);
    fs.writeFileSync(path.join(dir, '.env'), 'DECURIX_LAN_PORT=99999\n');
    assert.equal(lanPort(dir, {}), 8090);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});
