'use strict';

// A build that downloads can stall: an ESTABLISHED connection to a mirror that stops sending data, with a tool that
// has no timeout of its own, held a first start at one Dockerfile step for 18 minutes while the network was fine
// (2026-11-04). The Dockerfiles now bound every download themselves; this is the app's own backstop. A build that
// prints no new line for stallMs is cancelled and tried once more; a second stall is an error that names the last
// line. A build that keeps printing, however slowly, is never cancelled: only silence counts.

// STALL_MS is the default silence that counts as a stall: long enough for a slow but moving download, which prints
// as it goes.
const STALL_MS = 5 * 60 * 1000;
// GRACE_MS is how long a cancelled build is given to end before its attempt is let go of anyway.
const GRACE_MS = 30 * 1000;

// watchBuild runs attempt(onLine, signal), one build that resolves with its exit code and ends when signal aborts,
// under the stall watchdog: tries times at most (default 2), each cancelled after stallMs with no new line. onStall
// (try number, last line) runs after a stalled try is cancelled, to clean up what it left, and before the next.
// It resolves with { code, stalled, lastLine, tries }: stalled is true when the last try stalled too.
async function watchBuild({ attempt, onLine = () => {}, onStall = async () => {}, stallMs = STALL_MS, tries = 2,
  graceMs = GRACE_MS, timers = { setTimeout, clearTimeout } }) {
  let lastLine = '';
  for (let n = 1; n <= tries; n += 1) {
    const controller = new AbortController();
    let stalled = false;
    let timer = null;
    let wakeOnStall = () => {};
    const stallSeen = new Promise((resolve) => { wakeOnStall = resolve; });
    const arm = () => {
      if (timer !== null) timers.clearTimeout(timer);
      timer = timers.setTimeout(() => { stalled = true; controller.abort(); wakeOnStall(); }, stallMs);
      if (timer && typeof timer.unref === 'function') timer.unref();
    };
    arm();
    const running = Promise.resolve().then(() => attempt((line) => { lastLine = line; arm(); onLine(line); }, controller.signal));
    let code;
    try {
      code = await Promise.race([running, stallSeen.then(() => graceAfterStall(running, graceMs, timers))]);
    } finally {
      if (timer !== null) timers.clearTimeout(timer);
    }
    if (!stalled) return { code, stalled: false, lastLine, tries: n };
    await onStall(n, lastLine);
    if (n === tries) return { code: null, stalled: true, lastLine, tries: n };
  }
  return { code: null, stalled: true, lastLine, tries };
}

// graceAfterStall waits for a cancelled build to end, at most graceMs.
function graceAfterStall(running, graceMs, timers) {
  return new Promise((resolve) => {
    const timer = timers.setTimeout(() => resolve(null), graceMs);
    if (timer && typeof timer.unref === 'function') timer.unref();
    running.then((code) => { timers.clearTimeout(timer); resolve(code); }, () => { timers.clearTimeout(timer); resolve(null); });
  });
}

// stalledMessage is what a person is told when a build stalled twice: what failed, that it made no progress, and the
// last thing it printed.
function stalledMessage(failure, stallMs, lastLine) {
  const minutes = Math.round(stallMs / 60000);
  return `${failure} It stopped making progress twice (nothing new for ${minutes} minute${minutes === 1 ? '' : 's'}).`
    + ` Last line: ${lastLine ? lastLine.slice(0, 300) : '(nothing yet)'}`;
}

module.exports = { watchBuild, stalledMessage, STALL_MS, GRACE_MS };
