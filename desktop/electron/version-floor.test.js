'use strict';

// version-floor.test.js: comparing versions, the download URLs accepted, the decision, and reading the floor file.

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { compareVersions, safeDownloadUrl, decide, fetchFloor } = require('./version-floor');

test('versions compare numerically, part by part', () => {
  assert.equal(compareVersions('0.1.0', '0.1.0'), 0);
  assert.equal(compareVersions('0.1', '0.1.0'), 0);
  assert.equal(compareVersions('v1.2.3', '1.2.3'), 0);
  assert.equal(compareVersions('0.9.0', '0.10.0'), -1);
  assert.equal(compareVersions('1.10.0', '1.9.9'), 1);
  assert.equal(compareVersions('2.0.0-beta.1', '2.0.0'), -1);
  assert.equal(compareVersions('2.0.0', '2.0.0-rc.1'), 1);
  assert.equal(compareVersions('2.0.0-alpha', '2.0.0-beta'), -1);
  assert.equal(compareVersions('1.0.0+build.5', '1.0.0'), 0);
  assert.equal(compareVersions('dev', '1.0.0'), null);
  assert.equal(compareVersions('1.0.0', ''), null);
  assert.equal(compareVersions(undefined, '1.0.0'), null);
});

test('a download points at the site or its GitHub releases, or is the site', () => {
  assert.equal(safeDownloadUrl('https://decurix.computer/#download'), 'https://decurix.computer/#download');
  assert.equal(safeDownloadUrl('https://github.com/YKoustubhRao/decurix-site/releases/latest'), 'https://github.com/YKoustubhRao/decurix-site/releases/latest');
  for (const bad of ['http://decurix.computer/', 'https://decurix.computer.evil.com/', 'https://decurix.computer@evil.com/',
    'https://user:pw@decurix.computer/', 'https://github.com/someone-else/x', 'javascript:alert(1)', 'file:///C:/x.exe', '', 42, null]) {
    assert.equal(safeDownloadUrl(bad), 'https://decurix.computer/', String(bad));
  }
});

test('below min blocks, below latest is a notice, anything unreadable starts', () => {
  const floor = { min: '0.2.0', latest: '0.3.0', downloadUrl: 'https://decurix.computer/' };
  assert.equal(decide('0.1.9', floor).action, 'block');
  assert.equal(decide('0.2.0', floor).action, 'notice');
  assert.equal(decide('0.3.0', floor).action, 'none');
  assert.equal(decide('0.4.0', floor).action, 'none');
  assert.equal(decide('dev', floor).action, 'none');
  assert.equal(decide('0.1.0', null).action, 'none');
  assert.equal(decide('0.1.0', ['0.2.0']).action, 'none');
  assert.equal(decide('0.1.0', { min: 'later', latest: 0.2 }).action, 'none');
  assert.equal(decide('0.1.0', { latest: '0.2.0' }).action, 'notice');
  assert.equal(decide('0.1.0', { min: '0.2.0', downloadUrl: 'https://evil.example/' }).downloadUrl, 'https://decurix.computer/');
});

test('the published floor file lets the current app build start', () => {
  const published = JSON.parse(fs.readFileSync(path.join(__dirname, '..', '..', 'website', 'desktop-version-floor.json'), 'utf8'));
  const app = JSON.parse(fs.readFileSync(path.join(__dirname, 'package.json'), 'utf8')).version;
  assert.equal(decide(app, published).action, 'none');
  assert.equal(safeDownloadUrl(published.downloadUrl), published.downloadUrl);
});

test('the floor file is read with a timeout, and every failure is an error the app starts through', async () => {
  const answer = (status, text) => async (_url, options) => {
    assert.ok(options.signal, 'no timeout');
    return { ok: status === 200, status, text: async () => text };
  };
  assert.deepEqual(await fetchFloor('https://x/', 10, answer(200, '{"min":"0.0.0"}')), { min: '0.0.0' });
  await assert.rejects(fetchFloor('https://x/', 10, answer(404, '')), /HTTP 404/);
  await assert.rejects(fetchFloor('https://x/', 10, answer(200, 'not json')));
  await assert.rejects(fetchFloor('https://x/', 10, answer(200, ' '.repeat(20000))), /too big/);
  await assert.rejects(fetchFloor('https://x/', 10, async () => { throw new Error('getaddrinfo ENOTFOUND'); }), /ENOTFOUND/);
  await assert.rejects(fetchFloor('https://x/', 10, null), /no fetch/);
});
