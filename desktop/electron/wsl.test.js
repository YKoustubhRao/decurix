'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const shell = process.platform === 'win32' ? path.join(process.env.ProgramFiles || 'C:/Program Files', 'Git/bin/bash.exe') : '/bin/sh';
const skip = !fs.existsSync(shell);

// A docker stand-in for one shared engine: Decurix's resources (a worker, the
// stack, the model server, their volumes, an image, the network) beside
// another project's. MOCK_DOCKER_FAIL names a resource whose removal fails.
const mockDocker = `#!/bin/sh
if [ "$1" != --host ] || [ "$2" != unix:///var/run/docker.sock ]; then exit 9; fi
shift 2
printf '%s\\n' "$*" >> "$MOCK_DOCKER_LOG"
case "$*" in
  version) exit 0;;
  'ps -a --filter label=desktop-manager=true --format {{.Names}}') echo desktop_worker;;
  'ps -a --filter label=com.docker.compose.project=decurix --format {{.Names}}') echo decurix-stack;;
  'ps -a --filter label=decurix=true --format {{.Names}}') echo decurix-ollama;;
  'ps -a'*) ;;
  inspect*) printf '%s\\n' desktop_worker_home decurix-cache unrelated-volume;;
  'volume inspect decurix-data'|'volume inspect decurix-cache'|'volume inspect decurix_ollama') exit 0;;
  'volume inspect'*) exit 1;;
  'volume ls -q --filter label=decurix=true') echo decurix-tools;;
  'image inspect decurix-desktop:basic') exit 0;;
  'image inspect'*) exit 1;;
  'network ls --filter label=decurix=true --format {{.Name}}') echo decurix-network;;
  'network ls'*) ;;
  *" $MOCK_DOCKER_FAIL") exit 1;;
esac
exit 0
`;

// run runs maintain-existing.sh with args against the stand-in, and returns
// its result and the docker commands it ran.
function run(args, { failOn = 'nothing-fails' } = {}) {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'decurix-runtime-test-'));
  const log = path.join(temp, 'commands.log');
  const mock = path.join(temp, 'docker');
  fs.writeFileSync(mock, mockDocker, { mode: 0o755 });
  fs.writeFileSync(log, '');
  try {
    const result = spawnSync(shell, [...(process.platform === 'win32' ? ['--noprofile', '--norc'] : []), path.join(__dirname, 'wsl/maintain-existing.sh').replace(/\\/g, '/'), ...args], {
      env: { ...process.env, PATH: temp.replace(/\\/g, '/') + (process.platform === 'win32' ? ';' : ':') + process.env.PATH,
        MOCK_DOCKER_LOG: log.replace(/\\/g, '/'), MOCK_DOCKER_FAIL: failOn }, encoding: 'utf8', timeout: 30000,
    });
    return { result, commands: fs.readFileSync(log, 'utf8') };
  } finally {
    fs.rmSync(temp, { recursive: true, force: true });
  }
}

for (const mode of ['stop', 'reset', 'delete']) {
  test('shared runtime ' + mode + ' touches only Decurix containers and volumes', { skip }, () => {
    const { result, commands } = run([mode]);
    assert.equal(result.status, 0, result.error?.message || result.stderr);
    assert.ok(!commands.includes('unrelated-volume'));
    assert.ok(!commands.includes('prune'));
    if (mode === 'stop') {
      for (const name of ['desktop_worker', 'decurix-stack', 'decurix-ollama']) assert.match(commands, new RegExp(`stop ${name}`));
      assert.ok(!commands.includes('rm '));
    } else {
      for (const name of ['desktop_worker', 'decurix-stack', 'decurix-ollama']) assert.match(commands, new RegExp(`rm -f ${name}`));
      assert.match(commands, /volume rm desktop_worker_home/);
      assert.match(commands, /volume rm decurix-data/);
      if (mode === 'reset') {
        assert.ok(!commands.includes('volume rm decurix-cache'));
        assert.ok(!commands.includes('image rm'));
        assert.ok(!commands.includes('network rm'));
      } else {
        for (const name of ['decurix-cache', 'decurix-tools', 'decurix_ollama']) assert.match(commands, new RegExp(`volume rm ${name}`));
        assert.match(commands, /image rm decurix-desktop:basic/);
        assert.ok(!commands.includes('image rm decurix-desktop:ai'));
        assert.match(commands, /network rm decurix-network/);
      }
    }
  });
}

test('what was in the engine before the app used it is never removed', { skip }, () => {
  const { result, commands } = run(['delete', 'decurix-data', 'decurix-stack', 'desktop_worker', 'desktop_worker_home']);
  assert.equal(result.status, 0, result.stderr);
  for (const name of ['decurix-data', 'decurix-stack', 'desktop_worker', 'desktop_worker_home']) {
    assert.ok(!new RegExp(`rm (-f )?${name}\\b`).test(commands), `${name} was removed`);
  }
  assert.match(commands, /rm -f decurix-ollama/);
  assert.match(commands, /volume rm decurix-cache/);
});

test('inventory lists Decurix resources only, and changes nothing', { skip }, () => {
  const { result, commands } = run(['inventory']);
  assert.equal(result.status, 0, result.stderr);
  const listed = result.stdout.trim().split('\n').map((line) => line.replace(/^decurix-resource /, '')).sort();
  assert.deepEqual(listed, ['decurix-cache', 'decurix-data', 'decurix-desktop:basic', 'decurix-ollama', 'decurix-stack', 'decurix-tools',
    'decurix_ollama', 'desktop_worker', 'desktop_worker_home'].sort());
  assert.ok(!/\b(rm|stop)\b/.test(commands));
});

test('a removal that fails does not stop the rest, and the script says what is left', { skip }, () => {
  const { result, commands } = run(['reset'], { failOn: 'decurix-stack' });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /could not reset: decurix-stack/);
  assert.match(commands, /volume rm decurix-data/);
});

test('a name to keep that is not a resource name is refused before anything runs', { skip }, () => {
  const { result, commands } = run(['delete', 'a b;c']);
  assert.equal(result.status, 2);
  assert.equal(commands, '');
});
