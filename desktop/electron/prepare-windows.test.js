// Tests for wsl/prepare-windows.ps1, what the installer runs to make Windows
// ready for Decurix's Linux, and for how installer.nsh and the build use it.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const script = path.join(__dirname, 'wsl', 'prepare-windows.ps1');
const installer = fs.readFileSync(path.join(__dirname, 'installer.nsh'), 'utf8');
const powershell = process.platform === 'win32' ? path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe') : '';
const skip = !powershell || !fs.existsSync(powershell);

// A computer with everything on and running.
const ready = { arch: 'AMD64', build: 26200, wslFeature: true, vmpFeature: true, wsl: true, hypervisor: true, firmware: false, extensions: false, rebootPending: false };

// decide runs the script's plan on each set of facts; it changes nothing on this computer.
function decide(cases) {
  const result = spawnSync(powershell, ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', script, '-Action', 'decide', '-Facts', JSON.stringify(cases)], { encoding: 'utf8', timeout: 60000 });
  assert.equal(result.status, 0, result.stderr);
  return result.stdout.trim().split(/\r?\n/);
}

test('what the installer does for each kind of computer', { skip }, () => {
  const cases = [
    // Ready: no prompt, nothing to do. A running hypervisor hides the firmware flags, which then mean nothing.
    [ready, 'elevate=0 bits=0 why=ready'],
    // A computer that never had WSL (Windows 10 or 11): one administrator prompt.
    [{ ...ready, wslFeature: false, vmpFeature: false, wsl: false, hypervisor: false, firmware: true, extensions: true }, 'elevate=1 bits=0 why=missing'],
    // Only WSL itself missing (Windows 11's in-box stub, or Windows 10 without the Store package).
    [{ ...ready, wsl: false }, 'elevate=1 bits=0 why=missing'],
    // Virtualization off in the firmware: the features are still turned on, and the person is told.
    [{ ...ready, vmpFeature: false, hypervisor: false, firmware: false, extensions: true }, 'elevate=1 bits=2 why=missing'],
    [{ ...ready, hypervisor: false, firmware: false, extensions: true }, 'elevate=0 bits=2 why=firmware'],
    // Firmware flags not known: never taken for off.
    [{ ...ready, vmpFeature: false, hypervisor: false, firmware: null, extensions: null }, 'elevate=1 bits=0 why=missing'],
    // Everything on, a restart still owed: no prompt, a restart.
    [{ ...ready, hypervisor: false, firmware: true, extensions: true, rebootPending: true }, 'elevate=0 bits=1 why=restart'],
    // Everything on and restarted, yet no hypervisor: the boot settings are looked at (an administrator's).
    [{ ...ready, hypervisor: false, firmware: true, extensions: true }, 'elevate=1 bits=0 why=hypervisor'],
    // What cannot run it at all.
    [{ ...ready, arch: 'ARM64' }, 'elevate=0 bits=16 why=arm'],
    [{ ...ready, build: 18363 }, 'elevate=0 bits=16 why=build'],
    [{ ...ready, build: 19041, wsl: false }, 'elevate=1 bits=0 why=missing'],
  ];
  assert.deepEqual(decide(cases.map(([facts]) => facts)), cases.map(([, plan]) => plan));
});

test('the script parses in Windows PowerShell 5.1', { skip }, () => {
  const check = `$e = $null; [void][System.Management.Automation.Language.Parser]::ParseFile('${script.replace(/'/g, "''")}', [ref]$null, [ref]$e); if ($e) { $e | % { $_.ToString() }; exit 1 }`;
  const result = spawnSync(powershell, ['-NoProfile', '-NonInteractive', '-Command', check], { encoding: 'utf8', timeout: 60000 });
  assert.equal(result.status, 0, result.stdout + result.stderr);
});

test('every exit of ensure and enable carries the 100 base', () => {
  const text = fs.readFileSync(script, 'utf8');
  const body = text.slice(text.search(/^switch \(\$Action\)/m));
  for (const action of ['enable', 'ensure']) {
    const start = body.indexOf(`'${action}' {`);
    const block = body.slice(start, start + body.slice(start).search(/\r?\n {4}\}/));
    // Statements only, not the word in a logged sentence.
    const exits = block.match(/(?:^|[;{])\s*exit [^\r\n;}]+/gm);
    assert.ok(exits.length >= 2, action);
    for (const exit of exits) assert.match(exit, /\$BASE/, `${action}: ${exit}`);
  }
});

test('the script logs with Say, never Write-Output, inside its functions', () => {
  // Write-Output in a PowerShell function becomes part of what it returns, which would corrupt the exit bits.
  const body = fs.readFileSync(script, 'utf8').split(/^switch \(\$Action\)/m)[0];
  assert.doesNotMatch(body.replace(/^\s*#.*$/gm, ''), /Write-Output/);
});

test('the installer runs it on a first install only, as 64-bit PowerShell, and offers a reboot when it asks', () => {
  const install = installer.slice(installer.indexOf('!macro customInstall'), installer.indexOf('!macroend'));
  assert.match(install, /\$\{ifNot\} \$\{isUpdated\}/);
  assert.match(install, /Sysnative\\WindowsPowerShell\\v1\.0\\powershell\.exe/);
  assert.match(install, /resources\\wsl\\prepare-windows\.ps1" -Action ensure/);
  assert.match(install, /SetRebootFlag true/);
  // Its answer is 100 plus bits: PowerShell's own exit 1 (a blocked or broken script) is a failure, never a restart.
  assert.match(install, /\$\{if\} \$R4 < 100\s+\$\{orIf\} \$R4 > 131\s+StrCpy \$R4 "4"\s+\$\{else\}\s+IntOp \$R4 \$R4 - 100/);
  // Every message box has a silent-install answer.
  for (const box of install.match(/MessageBox [^\n]*/g)) assert.match(box, /\/SD ID(OK|NO)/);
});

test('the build ships it', () => {
  const build = fs.readFileSync(path.join(__dirname, '..', '..', 'scripts', 'windows', 'build-electron.ps1'), 'utf8');
  assert.match(build, /wsl\/prepare-windows\.ps1/);
  assert.match(fs.readFileSync(path.join(__dirname, 'package.json'), 'utf8'), /prepare-windows\.test\.js/);
});
