'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

// wsl/apk-safe.sh against a fake apk: one that stalls whenever the repositories point at the first mirror (as
// dl-cdn's community index did), or fails everywhere. It needs a POSIX sh with timeout, sed and sleep: skipped on
// Windows unless DECURIX_SH_TESTS=1 says one is on PATH (Git Bash).
const skip = process.platform === 'win32' && process.env.DECURIX_SH_TESTS !== '1' ? 'needs a POSIX shell' : false;
const helper = path.join(__dirname, 'wsl', 'apk-safe.sh');

function sandbox(apkBody) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'apk-safe-'));
  fs.mkdirSync(path.join(dir, 'bin'));
  const repositories = path.join(dir, 'repositories');
  fs.writeFileSync(repositories, 'https://dl-cdn.alpinelinux.org/alpine/v3.24/main\nhttps://dl-cdn.alpinelinux.org/alpine/v3.24/community\n');
  const apk = path.join(dir, 'bin', 'apk');
  fs.writeFileSync(apk, '#!/bin/sh\necho "$* @ $(head -n 1 "$APK_REPOSITORIES")" >> "$CALLS"\n' + apkBody + '\n');
  fs.chmodSync(apk, 0o755);
  return { dir, repositories, calls: path.join(dir, 'calls') };
}

function run(box, script) {
  const result = spawnSync('sh', ['-c', `. "$HELPER"; ${script}`], {
    encoding: 'utf8',
    timeout: 60000, // only turns a hang into a failure
    env: { ...process.env, PATH: path.join(box.dir, 'bin') + path.delimiter + process.env.PATH, HELPER: helper.replace(/\\/g, '/'),
      APK_REPOSITORIES: box.repositories, CALLS: box.calls, APK_HEARTBEAT: '1' },
  });
  const calls = fs.existsSync(box.calls) ? fs.readFileSync(box.calls, 'utf8').trim().split('\n') : [];
  return { ...result, calls };
}

test('a mirror that stalls is left after the bound for the next, which is kept for the next call', { skip }, () => {
  const box = sandbox('case "$(cat "$APK_REPOSITORIES")" in *dl-cdn*) exec sleep 30;; esac\nexit 0');
  const result = run(box, 'apk_safe 3 update && apk_safe 3 add --no-progress docker');
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.calls, [
    'update @ https://dl-cdn.alpinelinux.org/alpine/v3.24/main',
    'update @ https://mirrors.edge.kernel.org/alpine/v3.24/main',
    'add --no-progress docker @ https://mirrors.edge.kernel.org/alpine/v3.24/main',
  ]);
  assert.equal(fs.readFileSync(box.repositories, 'utf8'),
    'https://mirrors.edge.kernel.org/alpine/v3.24/main\nhttps://mirrors.edge.kernel.org/alpine/v3.24/community\n');
  assert.match(result.stdout, /apk-safe: apk update still running/);
  assert.match(result.stdout, /from https:\/\/dl-cdn\.alpinelinux\.org\/alpine; trying the next mirror/);
});

test('every mirror failing is a clear failure, after each was tried once', { skip }, () => {
  const box = sandbox('exit 1');
  const result = run(box, 'apk_safe 3 update');
  assert.notEqual(result.status, 0);
  assert.equal(result.calls.length, 3);
  assert.match(result.stderr, /failed from every Alpine mirror/);
});
