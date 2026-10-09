'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const script = path.join(__dirname, 'wsl', 'native-ollama.ps1');
const quote = value => "'" + value.replace(/'/g, "''") + "'";

test('native server logs normal stderr without killing PowerShell, and preserves its exit code', { skip: process.platform !== 'win32' }, () => {
  const fixture = fs.mkdtempSync(path.join(__dirname, 'native-ollama-test-'));
  try {
    const runtime = path.join(fixture, 'runtime-v0.40.1');
    fs.mkdirSync(runtime);
    const env = { ...process.env, TEMP: fixture, TMP: fixture, LOCALAPPDATA: fixture, APPDATA: fixture,
      USERPROFILE: fixture, SystemRoot: process.env.SystemRoot || 'C:\\Windows', SystemDrive: 'C:' };
    const native = 'public class Fixture { public static int Main(string[] args) { System.Console.Error.WriteLine("normal server startup on stderr"); System.Console.Out.WriteLine("server stdout"); return System.Environment.GetEnvironmentVariable("OLLAMA_KEEP_ALIVE") == "fail" ? 7 : 0; } }';
    const compiled = spawnSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command',
      'Add-Type -TypeDefinition ' + quote(native) + ' -Language CSharp -OutputType ConsoleApplication -OutputAssembly ' + quote(path.join(runtime, 'ollama.exe'))],
      { encoding: 'utf8', env, timeout: 30000, windowsHide: true });
    assert.equal(compiled.status, 0, compiled.stderr || compiled.error?.message);
    const settings = path.join(fixture, 'settings.env');
    for (const [keepAlive, expected] of [['10m', 0], ['fail', 7]]) {
      fs.writeFileSync(settings, 'OLLAMA_KEEP_ALIVE=' + keepAlive + '\n');
      const result = spawnSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', script,
        '-Action', 'serve', '-BindAddress', '127.0.0.1', '-SettingsFile', settings, '-Store', fixture],
        { encoding: 'utf8', env, timeout: 15000, windowsHide: true });
      assert.equal(result.status, expected, result.stderr || result.error?.message);
      assert.match(fs.readFileSync(path.join(fixture, 'server.log'), 'utf8'), /normal server startup on stderr/);
      assert.match(fs.readFileSync(path.join(fixture, 'server.stdout.log'), 'utf8'), /server stdout/);
      assert.doesNotMatch(result.stderr, /NativeCommandError/);
    }
    // Exercise real native CLI calls plus the selected-model copy/load contract.
    fs.writeFileSync(settings, 'OLLAMA_MODEL=auto\n');
    const requests = path.join(fixture, 'requests.jsonl');
    const mock = "function Invoke-RestMethod { param($Uri,$Method,$Body,$ContentType,$TimeoutSec,$Proxy); " +
      "if ($Uri -like '*/selection?*') { return @{selection=@{fits=$true;model=@{name='qwen3:8b'}};resources=@{ram_free_gb=18;cores=16}} }; " +
      "if ($Method -eq 'Post') { Add-Content -LiteralPath " + quote(requests) + " -Value $Body }; return @{} }; ";
    const prepared = spawnSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-Command',
      mock + '& ' + quote(script) + ' -Action prepare -BindAddress 127.0.0.254 -SettingsFile ' + quote(settings) + ' -Store ' + quote(fixture)],
      { encoding: 'utf8', env, timeout: 15000, windowsHide: true });
    assert.equal(prepared.status, 0, prepared.stderr || prepared.error?.message);
    const bodies = fs.readFileSync(requests, 'utf8').trim().split(/\r?\n/).map(line => JSON.parse(line.replace(/^\uFEFF/, '')));
    assert.deepEqual(bodies[0], { source: 'qwen3:8b', destination: 'decurix-local' });
    assert.equal(bodies[1].model, 'decurix-local');
    assert.equal(bodies[1].keep_alive, -1);
    assert.match(prepared.stdout, /DECURIX_STATE.*qwen3:8b/);
  } finally {
    const absolute = path.resolve(fixture);
    assert.ok(absolute.startsWith(path.resolve(__dirname) + path.sep + 'native-ollama-test-'));
    fs.rmSync(absolute, { recursive: true, force: true });
  }
});

test('install-only startup never launches a server or removes existing models', { skip: process.platform !== 'win32' }, () => {
  const fixture = fs.mkdtempSync(path.join(__dirname, 'native-ollama-test-'));
  try {
    const runtime = path.join(fixture, 'runtime-v0.40.1');
    fs.mkdirSync(runtime); fs.writeFileSync(path.join(runtime, 'ollama.exe'), 'fixture');
    fs.writeFileSync(path.join(runtime, '.complete'), 'v0.40.1');
    fs.mkdirSync(path.join(fixture, 'models'));
    fs.writeFileSync(path.join(fixture, 'models', 'keep.bin'), 'model');
    const settings = path.join(fixture, 'settings.env'); fs.writeFileSync(settings, '');
    const command = "function Get-CimInstance { throw 'Unexpected server launch' }; & " + quote(script) +
      ' -Action install -BindAddress 127.0.0.254 -SettingsFile ' + quote(settings) + ' -Store ' + quote(fixture);
    const result = spawnSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-Command', command],
      { encoding: 'utf8', env: { ...process.env, SystemDrive: 'C:', ProgramData: fixture, TEMP: fixture, TMP: fixture }, timeout: 15000, windowsHide: true });
    assert.equal(result.status, 0, result.stderr || result.error?.message);
    assert.match(result.stdout, /runtime installed/);
    assert.equal(fs.readFileSync(path.join(fixture, 'models', 'keep.bin'), 'utf8'), 'model');
  } finally { fs.rmSync(fixture, { recursive: true, force: true }); }
});

test('turning off deletes managed models but keeps runtime and unrelated custom models', { skip: process.platform !== 'win32' }, () => {
  const fixture = fs.mkdtempSync(path.join(__dirname, 'native-ollama-test-'));
  try {
    const custom = path.join(fixture, 'shared models');
    const managed = path.join(custom, 'decurix-managed');
    fs.mkdirSync(managed, { recursive: true }); fs.writeFileSync(path.join(managed, 'weights.bin'), 'managed');
    fs.writeFileSync(path.join(custom, 'personal-model.bin'), 'personal');
    const runtime = path.join(fixture, 'runtime-v0.40.1'); fs.mkdirSync(runtime);
    fs.writeFileSync(path.join(runtime, 'ollama.exe'), 'runtime');
    const settings = path.join(fixture, 'settings.env'); fs.writeFileSync(settings, 'OLLAMA_MODELS=' + custom + '\n');
    const command = 'function Get-CimInstance { @() }; & ' + quote(script) +
      ' -Action stop -BindAddress 127.0.0.254 -SettingsFile ' + quote(settings) + ' -Store ' + quote(fixture);
    const result = spawnSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-Command', command],
      { encoding: 'utf8', env: { ...process.env, SystemDrive: 'C:', ProgramData: fixture, TEMP: fixture, TMP: fixture }, timeout: 15000, windowsHide: true });
    assert.equal(result.status, 0, result.stderr || result.error?.message);
    assert.equal(fs.existsSync(managed), false);
    assert.equal(fs.readFileSync(path.join(custom, 'personal-model.bin'), 'utf8'), 'personal');
    assert.equal(fs.existsSync(path.join(runtime, 'ollama.exe')), true);
  } finally { fs.rmSync(fixture, { recursive: true, force: true }); }
});

test('model cleanup refuses junctions instead of deleting their targets', { skip: process.platform !== 'win32' }, () => {
  const fixture = fs.mkdtempSync(path.join(__dirname, 'native-ollama-test-'));
  try {
    const target = path.join(fixture, 'personal');
    fs.mkdirSync(target); fs.writeFileSync(path.join(target, 'keep.bin'), 'personal');
    fs.symlinkSync(target, path.join(fixture, 'models'), 'junction');
    const settings = path.join(fixture, 'settings.env'); fs.writeFileSync(settings, '');
    const command = 'function Get-CimInstance { @() }; & ' + quote(script) +
      ' -Action stop -BindAddress 127.0.0.254 -SettingsFile ' + quote(settings) + ' -Store ' + quote(fixture);
    const result = spawnSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-Command', command],
      { encoding: 'utf8', env: { ...process.env, SystemDrive: 'C:', ProgramData: fixture, TEMP: fixture, TMP: fixture }, timeout: 15000, windowsHide: true });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /junction or symbolic link/);
    assert.equal(fs.readFileSync(path.join(target, 'keep.bin'), 'utf8'), 'personal');
  } finally { fs.rmSync(fixture, { recursive: true, force: true }); }
});
