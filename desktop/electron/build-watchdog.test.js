'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { watchBuild, stalledMessage } = require('./build-watchdog');

// fakeClock is a clock the test moves by hand: no test here waits on real time.
function fakeClock() {
  let now = 0;
  let next = 1;
  const pending = new Map();
  return {
    timers: {
      setTimeout(fn, ms) { const id = next++; pending.set(id, { at: now + ms, fn }); return id; },
      clearTimeout(id) { pending.delete(id); },
    },
    // advance moves the clock by ms, running each timer that falls due, in order.
    async advance(ms) {
      const end = now + ms;
      for (;;) {
        await new Promise((resolve) => setImmediate(resolve));
        const due = [...pending.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
        if (!due) break;
        pending.delete(due[0]);
        now = due[1].at;
        due[1].fn();
      }
      now = end;
      await new Promise((resolve) => setImmediate(resolve));
    },
  };
}

// silentBuild prints one line and then nothing, ending only when cancelled.
function silentBuild(starts) {
  return (onLine, signal) => new Promise((resolve) => {
    starts.push(signal);
    onLine('#5 [2/4] RUN apk add --no-cache docker-cli bash curl ca-certificates');
    signal.addEventListener('abort', () => resolve(143), { once: true });
  });
}

test('a build that prints nothing is cancelled, tried once more, then an error naming its last line', async () => {
  const clock = fakeClock();
  const starts = [];
  const stalls = [];
  const result = watchBuild({ attempt: silentBuild(starts), stallMs: 1000, timers: clock.timers,
    onStall: async (n, last) => { stalls.push([n, last]); } });
  await clock.advance(999);
  assert.equal(starts.length, 1);
  assert.equal(starts[0].aborted, false);
  await clock.advance(1);
  assert.equal(starts[0].aborted, true);
  await clock.advance(1000);
  const done = await result;
  assert.equal(starts.length, 2);
  assert.equal(starts[1].aborted, true);
  assert.deepEqual(stalls.map(([n]) => n), [1, 2]);
  assert.equal(done.stalled, true);
  assert.equal(done.tries, 2);
  assert.match(done.lastLine, /apk add/);
  assert.match(stalledMessage('Could not build Decurix in Docker.', 300000, done.lastLine), /nothing new for 5 minutes.*apk add/);
});

test('a build that keeps printing, however slowly, is never cancelled, and its code is kept', async () => {
  const clock = fakeClock();
  const starts = [];
  let say;
  let finish;
  const result = watchBuild({ stallMs: 1000, timers: clock.timers, attempt: (onLine, signal) => new Promise((resolve) => {
    starts.push(signal);
    say = onLine;
    finish = resolve;
  }) });
  await clock.advance(0);
  for (let i = 0; i < 500; i += 1) {
    await clock.advance(900);
    say(`#7 sha256:… ${i} MB / 900 MB`);
  }
  finish(0);
  const done = await result;
  assert.equal(starts.length, 1);
  assert.equal(starts[0].aborted, false);
  assert.deepEqual([done.code, done.stalled, done.tries], [0, false, 1]);
});

test('a first try that stalls and a second that works give the second\'s code', async () => {
  const clock = fakeClock();
  const starts = [];
  const result = watchBuild({ stallMs: 1000, timers: clock.timers, attempt: (onLine, signal) => {
    starts.push(signal);
    if (starts.length === 1) return silentBuild([])(onLine, signal);
    return Promise.resolve(0);
  } });
  await clock.advance(1000);
  const done = await result;
  assert.deepEqual([done.code, done.stalled, done.tries], [0, false, 2]);
});

test('a cancelled build that never ends is let go of after the grace, and still counts as stalled', async () => {
  const clock = fakeClock();
  const result = watchBuild({ stallMs: 1000, graceMs: 500, tries: 1, timers: clock.timers,
    attempt: () => new Promise(() => {}) });
  await clock.advance(1000);
  await clock.advance(500);
  const done = await result;
  assert.equal(done.stalled, true);
});
