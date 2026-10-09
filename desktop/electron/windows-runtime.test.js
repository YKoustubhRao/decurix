'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const runtime = require('./windows-runtime');

test('WSL RAM is a ceiling, not an allocation or a fixed model reserve', () => {
  const plan = runtime.resourcePlan({}, 32 * 1024 ** 3, 16);
  assert.equal(plan.memoryMB, 16384);
  assert.equal(plan.slots, 7);
  assert.equal(plan.workerCPUs, 2);
  assert.equal(plan.cpus, 16);
  assert.equal(plan.diskGB, 256);
  assert.equal(runtime.resourcePlan({ DECURIX_WSL_MEMORY_MB: '13312' }, 16 * 1024 ** 3, 8).memoryMB, 13312);
  assert.equal(runtime.resourcePlan({ DECURIX_WSL_SWAP_MB: '0' }, 32 * 1024 ** 3, 16).swapMB, 0);
  assert.equal(runtime.resourcePlan({ DECURIX_WSL_MEMORY_MB: '32768' }, 32 * 1024 ** 3, 16).memoryMB, 32768);
  assert.throws(() => runtime.resourcePlan({ DECURIX_WSL_MEMORY_MB: '33792' }, 32 * 1024 ** 3, 16), /exceeds physical RAM/);
  assert.throws(() => runtime.resourcePlan({ DECURIX_WSL_CPUS: '99' }, 32 * 1024 ** 3, 16), /logical processors/);
  assert.throws(() => runtime.resourcePlan({ DECURIX_DEFAULT_DESKTOP_MEMORY_MB: '65536' }, 32 * 1024 ** 3, 16), /Worker defaults/);
});

test('WSL configuration preserves custom settings and changes only explicit limits', () => {
  const original = '# custom\n[wsl2]\nmemory=6GB\nprocessors=4\nkernel=C:\\custom\\kernel\n[experimental]\nautoMemoryReclaim=dropCache\n';
  const plan = runtime.resourcePlan({}, 32 * 1024 ** 3, 16);
  const merged = runtime.mergeWSLConfig(original, plan, {});
  assert.match(merged, /memory=6GB/);
  assert.match(merged, /processors=4/);
  assert.match(merged, /kernel=C:\\custom\\kernel/);
  assert.match(merged, /autoMemoryReclaim=dropCache/);
  assert.match(merged, /swap=4096MB/);
  const explicit = runtime.mergeWSLConfig(merged, plan, { DECURIX_WSL_MEMORY_MB: '16384' });
  assert.match(explicit, /memory=16384MB/);
  assert.equal(runtime.mergeWSLConfig(explicit, plan, { DECURIX_WSL_MEMORY_MB: '16384' }), explicit);
});

test('shared runtime keeps global WSL settings and uses their effective resource limits', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'decurix-wsl-budget-'));
  const file = path.join(dir, '.wslconfig');
  const original = '[wsl2]\nmemory=2GB\nprocessors=1\n';
  fs.writeFileSync(file, original);
  try {
    const plan = runtime.configureResources(file, {}, false, () => {});
    assert.equal(plan.memoryMB, 2048);
    assert.equal(plan.cpus, 1);
    assert.equal(fs.readFileSync(file, 'utf8'), original);
    assert.ok(!fs.existsSync(file + '.decurix.bak'));
  } finally { fs.unlinkSync(file); fs.rmdirSync(dir); }
});

test('native URL is resolved to a local Windows adapter and remote URLs stay external', () => {
  const adapters = { wsl: [{ family: 'IPv4', internal: false, address: '172.28.0.1' }] };
  assert.equal(runtime.windowsHost('default via 172.28.0.1 dev eth0', adapters), '172.28.0.1');
  assert.throws(() => runtime.windowsHost('default via 192.168.1.1', adapters), /mirrored networking/);
  assert.throws(() => runtime.windowsHost('', adapters, '8.8.8.8'), /local Windows/);
  assert.ok(runtime.usesWindowsOllama({ OLLAMA_URL: 'http://decurix-ollama:11434' }));
  assert.ok(!runtime.usesWindowsOllama({ OLLAMA_URL: 'https://example.test/v1' }));
  assert.ok(!runtime.usesWindowsOllama({ DECURIX_WINDOWS_NATIVE_OLLAMA: '1', OLLAMA_URL: 'https://example.test/v1' }));
  assert.ok(!runtime.usesWindowsOllama({ DECURIX_WINDOWS_NATIVE_OLLAMA: '0' }));
});

test('derived stack settings keep private settings intact and do not duplicate URLs', () => {
  const original = '# settings\nOLLAMA_URL=http://decurix-ollama:11434\nOLLAMA_MODEL=auto\nPRIVATE_VALUE="keep me"\n';
  const settings = runtime.readSettings(original);
  const plan = runtime.resourcePlan({}, 32 * 1024 ** 3, 16);
  const derived = runtime.stackSettings(original, settings, plan, '172.28.0.1');
  assert.match(derived, /^OLLAMA_URL=http:\/\/172.28.0.1:11435$/m);
  assert.equal(derived.match(/^OLLAMA_URL=/gm).length, 1);
  assert.match(derived, /PRIVATE_VALUE="keep me"/);
  assert.match(derived, /^OLLAMA_MODEL=decurix-local$/m);
  assert.match(derived, /^OLLAMA_KEEP_ALIVE=-1$/m);
  assert.match(original, /OLLAMA_MODEL=auto/);
  assert.match(derived, /DECURIX_DEFAULT_DESKTOP_CPUS=2/);
  assert.match(derived, /DECURIX_WINDOWS_SELECTOR_URL=http:\/\/172.28.0.1:11436/);
  assert.ok(!derived.includes('DECURIX_OLLAMA_RAM_MB'));
  assert.match(derived, /^DECURIX_MODEL_AUTOPULL=0$/m);
  const remote = runtime.stackSettings(original, settings, plan, '');
  assert.match(remote, /OLLAMA_URL=http:\/\/decurix-ollama:11434/);
  assert.ok(!remote.includes('DECURIX_WINDOWS_OLLAMA_URL='));
});
