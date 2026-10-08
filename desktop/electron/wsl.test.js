'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const shell = process.platform === 'win32' ? path.join(process.env.ProgramFiles || 'C:/Program Files', 'Git/bin/bash.exe') : '/bin/sh';

for (const mode of ['stop', 'reset', 'delete']) {
  test('shared runtime ' + mode + ' touches only Decurix containers and volumes', { skip: !fs.existsSync(shell) }, () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'decurix-runtime-test-'));
    const log = path.join(temp, 'commands.log');
    const mock = path.join(temp, 'docker');
    fs.writeFileSync(mock, `#!/bin/sh
if [ "$1" != --host ] || [ "$2" != unix:///var/run/docker.sock ]; then exit 9; fi
shift 2
printf '%s\\n' "$*" >> "$MOCK_DOCKER_LOG"
case "$*" in
  'ps -aq --filter label=desktop-manager=true') echo worker-id;;
  'ps -aq --filter label=com.docker.compose.project=decurix') echo stack-id;;
  inspect*) printf '%s\\n' desktop_worker_home decurix-cache unrelated-volume;;
  'ps -aq') echo unrelated-container;;
esac
`, { mode: 0o755 });
    try {
      const result = spawnSync(shell, [...(process.platform === 'win32' ? ['--noprofile', '--norc'] : []), path.join(__dirname, 'wsl/maintain-existing.sh').replace(/\\/g, '/'), mode], {
        env: { ...process.env, PATH: temp.replace(/\\/g, '/') + (process.platform === 'win32' ? ';' : ':') + process.env.PATH,
          MOCK_DOCKER_LOG: log.replace(/\\/g, '/') }, encoding: 'utf8', timeout: 15000,
      });
      assert.equal(result.status, 0, result.error?.message || result.stderr);
      const commands = fs.readFileSync(log, 'utf8');
      assert.ok(!commands.includes('unrelated-container'));
      assert.ok(!commands.includes('unrelated-volume'));
      assert.ok(!commands.includes('prune'));
      if (mode === 'stop') {
        assert.match(commands, /stop worker-id/);
        assert.match(commands, /stop stack-id/);
        assert.ok(!commands.includes('rm '));
      } else {
        assert.match(commands, /rm -f worker-id/);
        assert.match(commands, /volume rm desktop_worker_home/);
        assert.match(commands, /volume rm decurix-data/);
        if (mode === 'reset') {
          assert.ok(!commands.includes('volume rm decurix-cache'));
          assert.ok(!commands.includes('image rm'));
        } else {
          assert.match(commands, /volume rm decurix-cache/);
          assert.match(commands, /image rm decurix-desktop:basic/);
        }
      }
    } finally {
      for (const file of [mock, log]) if (fs.existsSync(file)) fs.unlinkSync(file);
      fs.rmdirSync(temp);
    }
  });
}
