'use strict';

// Tests for public-name.js: claiming and keeping this install's permanent name, with the link service faked.

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { linkService, namedLink, nameFile, defaultService } = require('./public-name');

function tempDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'decurix-public-name-'));
}

// service fakes the link service: answers[route] is a list of replies, used in order.
function service(answers) {
  const calls = [];
  const post = async (origin, route, body) => {
    calls.push({ origin, route, body });
    const reply = answers[route]?.shift();
    if (!reply) throw new Error(`unexpected ${route}`);
    if (reply instanceof Error) throw reply;
    return reply;
  };
  return { calls, post };
}

const claimed = { status: 200, body: { hostname: 'calm-river-4821.decurix.computer', key: 'key-1', token: 'tok-1' } };

test('the first start claims a name and keeps only the name and key', async () => {
  const dir = tempDir();
  const s = service({ '/v1/claim': [claimed] });
  const link = await namedLink(dir, 8090, { service: 'https://links.example', post: s.post });
  assert.deepEqual(link, { url: 'https://calm-river-4821.decurix.computer', token: 'tok-1' });
  assert.deepEqual(s.calls, [{ origin: 'https://links.example', route: '/v1/claim', body: { port: 8090 } }]);
  const saved = JSON.parse(fs.readFileSync(nameFile(dir), 'utf8'));
  assert.deepEqual(saved, { hostname: 'calm-river-4821.decurix.computer', key: 'key-1' });
  assert.equal(fs.readFileSync(nameFile(dir), 'utf8').includes('tok-1'), false);
});

test('later starts keep the same name and only ask for the token', async () => {
  const dir = tempDir();
  await namedLink(dir, 8090, { service: 'https://links.example', post: service({ '/v1/claim': [claimed] }).post });
  const s = service({ '/v1/token': [{ status: 200, body: { hostname: 'calm-river-4821.decurix.computer', token: 'tok-2' } }] });
  const link = await namedLink(dir, 8091, { service: 'https://links.example', post: s.post });
  assert.deepEqual(link, { url: 'https://calm-river-4821.decurix.computer', token: 'tok-2' });
  assert.deepEqual(s.calls.map((c) => c.route), ['/v1/token']);
  assert.deepEqual(s.calls[0].body, { hostname: 'calm-river-4821.decurix.computer', key: 'key-1', port: 8091 });
});

test('a name the service no longer knows is replaced by a new one', async () => {
  const dir = tempDir();
  await namedLink(dir, 8090, { service: 'https://links.example', post: service({ '/v1/claim': [claimed] }).post });
  const s = service({
    '/v1/token': [{ status: 404, body: { error: 'unknown name' } }],
    '/v1/claim': [{ status: 200, body: { hostname: 'brave-otter-1200.decurix.computer', key: 'key-2', token: 'tok-3' } }],
  });
  const link = await namedLink(dir, 8090, { service: 'https://links.example', post: s.post });
  assert.equal(link.url, 'https://brave-otter-1200.decurix.computer');
  assert.equal(JSON.parse(fs.readFileSync(nameFile(dir), 'utf8')).key, 'key-2');
});

test('a service that is down or refuses keeps the saved name and throws, so the app falls back', async () => {
  const dir = tempDir();
  await namedLink(dir, 8090, { service: 'https://links.example', post: service({ '/v1/claim': [claimed] }).post });
  for (const reply of [{ status: 503, body: { error: 'busy' } }, new Error('ECONNREFUSED')]) {
    await assert.rejects(namedLink(dir, 8090, { service: 'https://links.example', post: service({ '/v1/token': [reply] }).post }));
    assert.equal(JSON.parse(fs.readFileSync(nameFile(dir), 'utf8')).hostname, 'calm-river-4821.decurix.computer');
  }
});

test('a malformed claim answer is refused and nothing is saved', async () => {
  for (const body of [{ hostname: 'evil host', key: 'k', token: 't' }, { hostname: 'a.b', key: '', token: 't' }, null]) {
    const dir = tempDir();
    await assert.rejects(namedLink(dir, 8090, { service: 'https://links.example', post: service({ '/v1/claim': [{ status: 200, body }] }).post }));
    assert.equal(fs.existsSync(nameFile(dir)), false);
  }
});

test('the service address: default, off, https only, also from the data folder .env', () => {
  const dir = tempDir();
  assert.equal(linkService(dir, {}), defaultService);
  assert.equal(linkService(dir, { DECURIX_LINK_SERVICE: 'off' }), '');
  assert.equal(linkService(dir, { DECURIX_LINK_SERVICE: 'http://links.example' }), '');
  assert.equal(linkService(dir, { DECURIX_LINK_SERVICE: 'https://links.example/x' }), 'https://links.example');
  fs.writeFileSync(path.join(dir, '.env'), 'DECURIX_LINK_SERVICE=off\n');
  assert.equal(linkService(dir, {}), '');
});

test('turned off: no request, no name', async () => {
  const s = service({});
  assert.equal(await namedLink(tempDir(), 8090, { service: '', post: s.post }), null);
  assert.equal(s.calls.length, 0);
});
