'use strict';

// The plain-IP link on Windows: http://<this PC's address>:<gated port>.
//
// The stack runs in WSL, whose default networking hands a port compose
// publishes only to this computer's own 127.0.0.1. So the link needs Windows
// to forward <address>:<port> there (netsh portproxy) and its firewall to let
// the port in - one administrator prompt, then nothing until the address
// changes. Forwarding to 127.0.0.1, not to the distro's own address, keeps the
// rule good when WSL hands the distro a new address after a restart (measured
// on this repo's host: the address failed before the rule and answered after).
// Only the access-code-gated port is ever forwarded, never 8080.

const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

const ruleName = (port) => `Decurix LAN access (${port})`;

function powershell(script) {
  return new Promise((resolve) => {
    let out = '';
    let err = '';
    const child = spawn('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], { windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
    child.stdout.on('data', (chunk) => { out = (out + chunk.toString()).slice(-16384); });
    child.stderr.on('data', (chunk) => { err = (err + chunk.toString()).slice(-4096); });
    child.once('error', (error) => resolve({ code: 1, out, err: error.message }));
    child.once('exit', (code) => resolve({ code, out, err }));
  });
}

// forwardedIn reads `netsh interface portproxy show v4tov4`: whether address:port
// already goes to 127.0.0.1:port.
function forwardedIn(table, address, port) {
  return table.split(/\r?\n/).some((line) => {
    const cells = line.trim().split(/\s+/);
    return cells.length === 4 && cells[0] === address && cells[1] === String(port) && cells[2] === '127.0.0.1' && cells[3] === String(port);
  });
}

async function isForwarded(address, port) {
  const { out } = await powershell(`netsh interface portproxy show v4tov4; if (Get-NetFirewallRule -DisplayName '${ruleName(port)}' -ErrorAction SilentlyContinue | Where-Object { $_.Enabled -eq 'True' -and $_.Direction -eq 'Inbound' -and $_.Action -eq 'Allow' }) { 'DECURIX-FIREWALL-OK' }; try { $r = Invoke-WebRequest -UseBasicParsing -Uri 'http://${address}:${port}/' -TimeoutSec 3; if ($r.StatusCode -eq 200 -and $r.Content.Contains('/__decurix_access/login')) { 'DECURIX-LISTENER-OK' } } catch { }`);
  return forwardedIn(out, address, port) && out.includes('DECURIX-FIREWALL-OK') && out.includes('DECURIX-LISTENER-OK');
}

// The elevated half: replaces any rule on address:port (an older one may point
// at WSL's old address), and opens the port in the firewall. The app cannot
// read an elevated process's output, so with a transcript path it writes what
// netsh, the firewall and the IP Helper service (which runs portproxy) said
// there - lan-forward.log in the data folder.
function forwardScript(address, port, transcript = '') {
  const steps = [
    "$ErrorActionPreference = 'Stop'",
    'Set-Service iphlpsvc -StartupType Automatic',
    'Start-Service iphlpsvc',
    `netsh interface portproxy delete v4tov4 listenaddress=${address} listenport=${port}`,
    `netsh interface portproxy add v4tov4 listenaddress=${address} listenport=${port} connectaddress=127.0.0.1 connectport=${port}`,
    "if ($LASTEXITCODE -ne 0) { throw 'Could not create the LAN port forward' }",
    `Remove-NetFirewallRule -DisplayName '${ruleName(port)}' -ErrorAction SilentlyContinue`,
    `New-NetFirewallRule -DisplayName '${ruleName(port)}' -Direction Inbound -Protocol TCP -LocalPort ${port} -Action Allow | Out-Null`,
    'Get-Service iphlpsvc | Format-List Name,Status,StartType',
  ].join('; ');
  if (!transcript) return steps;
  return `& { ${steps} } *> '${transcript.replace(/'/g, "''")}'`;
}

function elevated(script) {
  const encoded = Buffer.from(script, 'utf16le').toString('base64');
  return `Start-Process -FilePath powershell.exe -ArgumentList '-NoProfile','-NonInteractive','-EncodedCommand','${encoded}' -Verb RunAs -Wait -WindowStyle Hidden`;
}

// ensureLanForward makes address:port reach the stack, asking Windows for an
// administrator's yes when it does not yet. Only a real no is remembered - for
// that address, so a person who declined is not asked at every start, and a
// new address asks again. Start-Process fails when the prompt is declined and
// succeeds once the elevated script ran; a script that ran and still left no
// forward is a failure, logged and tried again at the next start. An earlier
// version took any failure for a no, and that PC never asked again.
// log(line) says each step, for desktop.log. Returns whether the link works.
async function ensureLanForward(dataDir, address, port, log = () => {}) {
  if (!/^\d{1,3}(\.\d{1,3}){3}$/.test(address) || !Number.isInteger(port) || port <= 0 || port >= 65536) {
    log(`LAN link: no usable address (${address}:${port})`);
    return false;
  }
  if (await isForwarded(address, port)) { log(`LAN link: ${address}:${port} is forwarded`); return true; }
  const file = path.join(dataDir, 'lan-forward.json');
  try {
    if (JSON.parse(fs.readFileSync(file, 'utf8')).declined === address) {
      log(`LAN link: not forwarded - the administrator prompt was declined for ${address} (delete ${file} to be asked again)`);
      return false;
    }
  } catch { /* never declined */ }
  log(`LAN link: asking Windows to forward ${address}:${port}`);
  const transcript = path.join(dataDir, 'lan-forward.log');
  const asked = await powershell(elevated(forwardScript(address, port, transcript)));
  if (await isForwarded(address, port)) { log(`LAN link: ${address}:${port} forwarded`); return true; }
  if (asked.code !== 0) {
    const why = (asked.err || asked.out).trim().split(/\r?\n/)[0] || `exit ${asked.code}`;
    log(`LAN link: the administrator prompt was declined or could not be shown: ${why}`);
    // Only cancellation is a refusal. Missing elevation or a script error
    // must not disable LAN forwarding permanently for this address.
    if (/cancel[l]?ed|1223|0x800704c7/i.test(asked.err || asked.out)) {
      try { fs.writeFileSync(file, JSON.stringify({ declined: address }) + '\n', 'utf8'); } catch { /* asked again next start */ }
    }
  } else {
    let said = '';
    try { said = fs.readFileSync(transcript, 'utf8').replace(/\0/g, '').replace(/\s+/g, ' ').trim().slice(0, 600); } catch { /* no transcript */ }
    log(`LAN link: forwarding ${address}:${port} did not take; trying again at the next start. Windows said: ${said || '(nothing)'}`);
  }
  return false;
}

module.exports = { ensureLanForward, forwardedIn, forwardScript };
