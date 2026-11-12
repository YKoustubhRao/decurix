'use strict';

// stack-health.test.js: reading /healthz, and the sliding restart window with its doubling wait.

const test = require('node:test');
const assert = require('node:assert/strict');
const { readHealth, nextRestart } = require('./stack-health');

test('a Decurix health report is read; anything else is not', () => {
  const body = JSON.stringify({ service: 'decurix-control-plane', version: '7e3e6f97', pid: 7, started_at: '2026-11-11T10:00:00Z', instance: 'abc', draining: true });
  assert.deepEqual(readHealth(200, body), { version: '7e3e6f97', pid: 7, instance: 'abc', startedAt: '2026-11-11T10:00:00Z', draining: true });
  assert.equal(readHealth(200, JSON.stringify({ service: 'decurix-control-plane', version: 'x' })).draining, false);
  assert.equal(readHealth(503, body), null);
  assert.equal(readHealth(200, '<svg>favicon</svg>'), null);
  assert.equal(readHealth(200, JSON.stringify({ service: 'another-app', draining: true })), null);
  assert.equal(readHealth(200, 'null'), null);
  // Only a literal true drains: a supervisor must never stop restarting over a malformed answer.
  assert.equal(readHealth(200, JSON.stringify({ service: 'decurix-control-plane', draining: 'yes' })).draining, false);
});

test('restarts wait 1 s doubling to 30 s, at most 6 in a sliding 5 minutes', () => {
  const now = 10 * 60 * 1000;
  let restarts = [];
  const delays = [];
  for (let i = 0; i < 6; i += 1) {
    const plan = nextRestart(restarts, now + i);
    assert.equal(plan.allowed, true);
    delays.push(plan.delayMs);
    restarts = [...plan.kept, now + i];
  }
  assert.deepEqual(delays, [1000, 2000, 4000, 8000, 16000, 30000]);
  assert.equal(nextRestart(restarts, now + 10).allowed, false);

  // The window slides: once the oldest restart is five minutes old, one more is allowed, after the wait the five
  // still inside it earn.
  const later = nextRestart(restarts, now + 5 * 60 * 1000);
  assert.equal(later.allowed, true);
  assert.equal(later.kept.length, 5);
  assert.equal(later.delayMs, 30000);
  // A quiet stretch forgets them all.
  assert.deepEqual(nextRestart(restarts, now + 20 * 60 * 1000), { allowed: true, delayMs: 1000, kept: [] });
});
