'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { createLocalModels } = require('./local-models');
function harness(enabled = false, operate = async () => {}) {
  const actions = [], saved = [];
  return { actions, saved, models: createLocalModels({ readPreference: () => enabled,
    writePreference: value => saved.push(value), operate: async (action, report, signal) => { actions.push(action); await operate(action, report, signal); } }) };
}
test('launch installs only the runtime; toggle on scans, downloads and loads automatically', async () => {
  const h = harness();
  await h.models.resume();
  assert.deepEqual(h.actions, ['install']);
  assert.equal(h.models.status().phase, 'off');
  await h.models.setEnabled(true);
  assert.deepEqual(h.actions, ['install', 'ensure', 'prepare']);
  assert.equal(h.models.status().phase, 'ready');
  assert.deepEqual(h.saved, [true]);
  await h.models.setEnabled(false);
  assert.deepEqual(h.actions, ['install', 'ensure', 'prepare', 'stop']);
  assert.equal(h.models.status().phase, 'off');
  assert.match(h.models.status().detail, /deleted/);
});
test('persisted hosting always installs runtime before hosting and preparing', async () => {
  const h = harness(true);
  await h.models.resume();
  assert.deepEqual(h.actions, ['install', 'ensure', 'prepare']);
});
test('turning off cancels model preparation before unloading and deleting', async () => {
  const h = harness(false, async (action, report, signal) => {
    if (action !== 'prepare') return;
    report({ model: 'qwen3:8b', resources: { cores: 8 }, detail: 'Downloading 42%' });
    await new Promise((resolve, reject) => signal.addEventListener('abort', () => reject(new Error('cancelled')), { once: true }));
  });
  const enabled = h.models.setEnabled(true);
  while (h.models.status().phase !== 'prepare') await new Promise(resolve => setImmediate(resolve));
  assert.equal(h.models.status().busy, true);
  assert.equal(h.models.status().model, 'qwen3:8b');
  await h.models.setEnabled(false);
  await enabled;
  assert.deepEqual(h.actions, ['install', 'ensure', 'prepare', 'stop']);
  assert.equal(h.models.status().phase, 'off');
  assert.equal(h.models.status().model, '');
  assert.equal(h.models.status().resources, null);
});
test('enabling during first-start install waits for it and does not duplicate setup', async () => {
  let finish;
  const h = harness(false, async action => { if (action === 'install') await new Promise(resolve => { finish = resolve; }); });
  const startup = h.models.resume();
  const enabled = h.models.setEnabled(true);
  finish();
  await Promise.all([startup, enabled]);
  assert.deepEqual(h.actions, ['install', 'ensure', 'prepare']);
  assert.equal(h.models.status().phase, 'ready');
});
test('failed preparation is visible and retry rescans resources', async () => {
  let attempts = 0;
  const h = harness(false, async action => { if (action === 'prepare' && ++attempts === 1) throw Error('Download timed out'); });
  await h.models.setEnabled(true);
  assert.equal(h.models.status().phase, 'error');
  await h.models.setEnabled(true);
  assert.equal(h.models.status().phase, 'ready');
  assert.deepEqual(h.actions, ['install', 'ensure', 'prepare', 'ensure', 'prepare']);
});
test('invalid input cannot change the preference', async () => {
  const h = harness();
  await assert.rejects(h.models.setEnabled('true'), /on or off/);
  assert.deepEqual(h.saved, []);
});
