'use strict';

// version-floor.js: the oldest app build still allowed to run, and the newest one there is.
//
// At start the app reads https://decurix.computer/desktop-version-floor.json ({ min, latest, downloadUrl }, published
// with the website from website/desktop-version-floor.json). A build below min is retired (a broken release): the app
// says so and offers the download or quitting, and starts nothing. A build below latest gets a notice it can dismiss.
// Anything that goes wrong - no network, a slow answer, a file that does not parse - lets the app start: the floor
// fails open, so the website being down never stops anyone working.

const FLOOR_URL = 'https://decurix.computer/desktop-version-floor.json';
const FLOOR_TIMEOUT_MS = 10000;
const DEFAULT_DOWNLOAD = 'https://decurix.computer/';
// Where a download may point: the site itself, or the releases on GitHub. Anything else is replaced by the site.
const DOWNLOAD_PREFIXES = ['https://decurix.computer/', 'https://github.com/YKoustubhRao/'];
const MAX_FLOOR_BYTES = 16 * 1024;

// parseVersion reads "1.2.3", "v1.2" or "1.2.3-beta.1" as { parts: [1, 2, 3], pre: 'beta.1' }, or null.
function parseVersion(text) {
  if (typeof text !== 'string') return null;
  const match = /^v?(\d+(?:\.\d+){0,3})(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/.exec(text.trim());
  if (!match) return null;
  return { parts: match[1].split('.').map(Number), pre: match[2] || '' };
}

// compareVersions is -1, 0 or 1 as a is older than, the same as or newer than b, numerically part by part (missing
// parts are 0; a pre-release is older than its release); null when either cannot be read.
function compareVersions(a, b) {
  const x = parseVersion(a);
  const y = parseVersion(b);
  if (!x || !y) return null;
  for (let i = 0; i < Math.max(x.parts.length, y.parts.length); i += 1) {
    const d = (x.parts[i] || 0) - (y.parts[i] || 0);
    if (d !== 0) return d < 0 ? -1 : 1;
  }
  if (x.pre === y.pre) return 0;
  if (!x.pre) return 1;
  if (!y.pre) return -1;
  return x.pre < y.pre ? -1 : 1;
}

// safeDownloadUrl is url when it is an https address on the site or the GitHub releases, else the site.
function safeDownloadUrl(url) {
  if (typeof url !== 'string') return DEFAULT_DOWNLOAD;
  let parsed;
  try { parsed = new URL(url.trim()); } catch { return DEFAULT_DOWNLOAD; }
  if (parsed.protocol !== 'https:' || parsed.username || parsed.password) return DEFAULT_DOWNLOAD;
  return DOWNLOAD_PREFIXES.some((prefix) => parsed.href.startsWith(prefix)) ? parsed.href : DEFAULT_DOWNLOAD;
}

// decide says what to do with this build given the floor file's contents: { action: 'block' | 'notice' | 'none',
// min, latest, downloadUrl }. A file or version that cannot be read is 'none' (fail open).
function decide(current, floor) {
  const none = { action: 'none', min: '', latest: '', downloadUrl: DEFAULT_DOWNLOAD };
  if (!floor || typeof floor !== 'object' || Array.isArray(floor) || !parseVersion(current)) return none;
  const min = parseVersion(floor.min) ? floor.min.trim() : '';
  const latest = parseVersion(floor.latest) ? floor.latest.trim() : '';
  const downloadUrl = safeDownloadUrl(floor.downloadUrl);
  if (min && compareVersions(current, min) < 0) return { action: 'block', min, latest, downloadUrl };
  if (latest && compareVersions(current, latest) < 0) return { action: 'notice', min, latest, downloadUrl };
  return { ...none, min, latest, downloadUrl };
}

// fetchFloor reads the floor file: its parsed JSON, or an error (no network, timeout, not 200, too big, not JSON).
async function fetchFloor(url = FLOOR_URL, timeoutMs = FLOOR_TIMEOUT_MS, fetchImpl = globalThis.fetch) {
  if (typeof fetchImpl !== 'function') throw new Error('no fetch in this runtime');
  const res = await fetchImpl(url, { signal: AbortSignal.timeout(timeoutMs), cache: 'no-store', headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const text = await res.text();
  if (text.length > MAX_FLOOR_BYTES) throw new Error('the file is too big');
  return JSON.parse(text);
}

module.exports = { FLOOR_URL, FLOOR_TIMEOUT_MS, DEFAULT_DOWNLOAD, parseVersion, compareVersions, safeDownloadUrl, decide, fetchFloor };
