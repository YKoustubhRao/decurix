'use strict';

// stack-health.js: how the app reads the control plane it supervises, and when it may start it again.
//
// The control plane answers GET /healthz with which binary it is (service, version, pid, started_at, instance) and
// whether it is draining: an update recreating its container lets requests in flight finish first, and a drain is
// that deliberate swap, never a death (internal/httpapi/healthz.go). A stack from before /healthz does not answer it;
// main.js then falls back to the favicon's marker.
//
// A control plane that keeps dying is started again a few times, each after a longer wait, and then left stopped
// with a Retry: up to MAX_RESTARTS within RESTART_WINDOW_MS (a sliding window), waiting 1 s, 2 s, 4 s ... up to 30 s.

const HEALTH_SERVICE = 'decurix-control-plane';
const MAX_RESTARTS = 6;
const RESTART_WINDOW_MS = 5 * 60 * 1000;
const FIRST_DELAY_MS = 1000;
const MAX_DELAY_MS = 30000;

// readHealth turns a /healthz answer (status, body) into { version, pid, instance, startedAt, draining }, or null
// when it is not a Decurix control plane's health report (an older stack, another program on the port).
function readHealth(status, body) {
  if (status !== 200 || typeof body !== 'string') return null;
  let report;
  try { report = JSON.parse(body); } catch { return null; }
  if (!report || typeof report !== 'object' || report.service !== HEALTH_SERVICE) return null;
  const text = (value) => (typeof value === 'string' ? value.slice(0, 120) : '');
  return {
    version: text(report.version),
    pid: Number.isInteger(report.pid) ? report.pid : 0,
    instance: text(report.instance),
    startedAt: text(report.started_at),
    draining: report.draining === true,
  };
}

// nextRestart decides whether a control plane that died may be started again now: restarts are the times of the
// earlier restarts. It returns { allowed, delayMs, kept }: kept is restarts still inside the window (the caller adds
// now to it when allowed), delayMs how long to wait first.
function nextRestart(restarts, now, { max = MAX_RESTARTS, windowMs = RESTART_WINDOW_MS, firstDelayMs = FIRST_DELAY_MS, maxDelayMs = MAX_DELAY_MS } = {}) {
  const kept = (restarts || []).filter((at) => now - at < windowMs);
  if (kept.length >= max) return { allowed: false, delayMs: 0, kept };
  return { allowed: true, delayMs: Math.min(firstDelayMs * 2 ** kept.length, maxDelayMs), kept };
}

module.exports = { readHealth, nextRestart, HEALTH_SERVICE, MAX_RESTARTS, RESTART_WINDOW_MS };
