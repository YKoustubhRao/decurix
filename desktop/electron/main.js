'use strict';

const { app, BrowserWindow, ipcMain, Menu, nativeTheme, powerSaveBlocker, shell, dialog } = require('electron');
const { spawn } = require('node:child_process');
const dgram = require('node:dgram');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { setupMobileAccess } = require('./mobile-access');
const { namedLink } = require('./public-name');
const { lanPort } = require('./gated-port');
const { ensureLanForward } = require('./windows-lan');
const windowsRuntime = require('./windows-runtime');
const { createLocalModels } = require('./local-models');
const { watchBuild, stalledMessage } = require('./build-watchdog');

// The app is a window on the control plane's dashboard, and what starts it.
//
// A worker is a Linux desktop (XFCE, shown through noVNC) in a Docker
// container, and the control plane runs in Docker beside them - a control
// plane outside Docker cannot reach a desktop (Docker Desktop does not route a
// container's address to the host). The app carries the Linux control plane
// with a Dockerfile and a compose file (resources/stack) and runs that, as
// scripts/linux/build.sh does from a checkout.
//
// Where Docker comes from:
//   - a Mac or Linux uses the Docker the computer has;
//   - Windows reuses a compatible WSL Docker Engine on new installs, or uses
//     a WSL distro of the app's own ("Decurix", Alpine), into
//     which the app installs Docker Engine and where it runs the daemon
//     itself (resources/wsl). No Docker Desktop. When Windows has no WSL yet
//     the app turns it on (one administrator prompt, then a restart).
const windows = process.platform === 'win32';
const DISTRO = 'Decurix';
let runtimeDistro = DISTRO;
let runtimeSelected = false;
// Where the stack is staged inside the distro (resources/wsl/setup.sh).
const DISTRO_STACK = '/var/lib/decurix/stack';
const dashboard = 'http://127.0.0.1:8080';
const marker = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" role="img" aria-label="Decurix"';
const dashboardOrigin = new URL(dashboard).origin;
let window;
let starting = false;
let closing = false;
let mobileAccess = null;
let mobileAccessPromise = null;

// Watching the control plane once the dashboard is showing. It runs detached
// and outlives this window, so the one this window is looking at was often
// started by an earlier run of the app - and nothing tells this process when
// that one dies. Found by killing it under an open window: the window went on
// showing a dashboard that no longer answered, with no word and no way back.
// So it is asked every few seconds, and when it has not answered several
// times in a row it is started again; one that keeps dying is left stopped,
// with its last log line and a Retry.
const WATCH_EVERY_MS = 5000;
const LOST_AFTER_MISSES = 3;
const MAX_RESTARTS = 3;
const RESTART_WINDOW_MS = 10 * 60 * 1000;
let showingDashboard = false;
let watching = false;
let misses = 0;
let restarts = [];

function resource(name) {
  if (app.isPackaged) return path.join(process.resourcesPath, name);
  if (name === '.env.example') return path.join(__dirname, '..', '..', '.env.example');
  return path.join(__dirname, 'build', name);
}

// The control plane's data directory: its database, the workers' folders, the
// editable .env and the start-up log. The same folder decurix.exe chooses by
// itself (os.UserConfigDir()/decurix).
function dataDir() {
  return path.join(app.getPath('appData'), 'decurix');
}

// Windows: where the app's WSL distro keeps its disk (its images and the
// desktops' homes - gigabytes, so not the roaming profile dataDir is in).
function distroDir() {
  return path.join(process.env.LOCALAPPDATA || path.join(os.homedir(), 'AppData', 'Local'), 'Decurix', 'wsl');
}

// linuxPath names a Windows file as the distro sees it: C:\a\b is /mnt/c/a/b
// (WSL's default automount, which setup.sh leaves alone).
function linuxPath(file) {
  const full = file.replace(/\\/g, '/');
  const drive = /^([A-Za-z]):\/(.*)$/.exec(full);
  return drive ? `/mnt/${drive[1].toLowerCase()}/${drive[2]}` : full;
}

// Where programs are looked for. An app started from the Finder or a desktop
// menu is not given a shell's PATH, and Docker and Homebrew put theirs in
// folders that the bare one leaves out.
function searchPath() {
  const extra = ['/usr/local/bin', '/opt/homebrew/bin', '/Applications/Docker.app/Contents/Resources/bin', path.join(os.homedir(), '.docker', 'bin')];
  return [...(process.env.PATH || '').split(path.delimiter), ...extra].filter(Boolean);
}

function findProgram(name) {
  for (const dir of searchPath()) {
    const file = path.join(dir, name);
    if (fs.existsSync(file)) return file;
  }
  return '';
}

// The phone link's tunnel, against the access-code-gated listener: the copy
// of cloudflared shipped with the Windows app, else the one the computer has.
function cloudflared() {
  return windows ? resource('cloudflared.exe') : findProgram('cloudflared');
}

function tunnelCommand(port, provider = 'cloudflare') {
  if (provider === 'localhost-run') {
    return { file: windows ? path.join(process.env.WINDIR || 'C:\\Windows', 'System32', 'OpenSSH', 'ssh.exe') : 'ssh', args: [
      '-T', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=accept-new',
      '-o', `UserKnownHostsFile=${path.join(dataDir(), 'tunnel-known-hosts')}`,
      '-o', 'ServerAliveInterval=30', '-o', 'ServerAliveCountMax=3',
      '-o', 'ExitOnForwardFailure=yes', '-o', 'ConnectTimeout=15',
      '-R', `80:127.0.0.1:${port}`, 'nokey@localhost.run',
    ] };
  }
  return { file: cloudflared(), args: ['tunnel', '--protocol', 'http2', '--url', `http://127.0.0.1:${port}`] };
}

// permanentTunnelCommand runs cloudflared for this install's permanent name
// (public-name.js). The token goes in the environment, never on the command
// line, where any process on the computer could read it.
function permanentTunnelCommand(token) {
  return {
    file: cloudflared(),
    args: ['tunnel', '--no-autoupdate', '--protocol', 'http2', 'run'],
    env: { ...process.env, TUNNEL_TOKEN: token },
  };
}

// The public link is a tunnel from the internet to the gated listener. It is
// off until the person turns it on in the Mobile menu (preload.js): a firm
// that never asked for phone access should not have an internet-reachable
// sign-in page. The choice is the app's own setting, kept beside its others.
function publicLinkFile() {
  return path.join(dataDir(), 'public-link.json');
}

function publicLinkEnabled() {
  try { return JSON.parse(fs.readFileSync(publicLinkFile(), 'utf8')).enabled === true; }
  catch (error) { if (error.code !== 'ENOENT') console.warn('Could not read the public link setting:', error.message); return false; }
}

function publicLinkStatus() {
  return { available: Boolean(cloudflared()), enabled: publicLinkEnabled() };
}

async function setPublicLink(enabled) {
  if (typeof enabled !== 'boolean') throw new Error('The public link is turned on or off, nothing else.');
  fs.mkdirSync(dataDir(), { recursive: true });
  fs.writeFileSync(publicLinkFile(), JSON.stringify({ enabled }) + '\n', 'utf8');
  logLine(`Public link turned ${enabled ? 'on' : 'off'} from the dashboard`);
  if (enabled) { if (showingDashboard) startMobileAccess(); } else await stopMobileAccess();
  return publicLinkStatus();
}

for (const [channel, action] of [
  ['public-link-status', () => publicLinkStatus()],
  ['public-link-set', value => setPublicLink(value)],
]) {
  ipcMain.handle(channel, (event, value) => {
    if (!fromDashboard(event)) throw new Error('The public link is only set from the desktop dashboard.');
    return action(value);
  });
}

function startMobileAccess() {
  if (mobileAccessPromise || closing) return;
  // Off unless the person turned it on (publicLinkEnabled).
  if (!publicLinkEnabled()) return;
  // No cloudflared, no tunnel: the dashboard works without one.
  if (!cloudflared()) return;
  mobileAccessPromise = setupMobileAccess(dataDir(), tunnelCommand, {
    log: logLine,
    permanent: (port) => namedLink(dataDir(), port, { log: logLine }),
    permanentCommand: permanentTunnelCommand,
  }).then(async (access) => {
    if (closing) await access?.stop();
    else mobileAccess = access;
  }).catch((error) => console.warn('Mobile access setup failed:', error.message));
}

async function stopMobileAccess() {
  await mobileAccessPromise;
  // Forgotten before it is stopped: a stop that fails must not leave a link
  // that can never be opened again.
  mobileAccessPromise = null;
  const access = mobileAccess;
  mobileAccess = null;
  await access?.stop();
}

// The startup screen's paper and ink (loading.html, the dashboard's own
// tokens), for the window behind it and the caption buttons over it.
function startupPalette() {
  return nativeTheme && nativeTheme.shouldUseDarkColors
    ? { color: '#100f0e', symbolColor: '#f2f0ea' }
    : { color: '#f6f5f2', symbolColor: '#1a1917' };
}

// The start screen's latest status, numbered. It is sent again whenever the screen is loaded again, and the screen
// says which one it has shown (preload.js, startup-status-shown). A start that failed with the screen frozen on a
// line from the middle of a build, and no error (2026-11-04), is why: a failure the screen has not shown within
// SHOWN_WITHIN_MS loads the screen again and is put in front of the person in a dialog of the app's own, with Retry.
let lastStatus = null;
let statusSeq = 0;
let shownSeq = 0;
let reloadedAt = 0;
const SHOWN_WITHIN_MS = 10000;

// update tells the start screen where the start is. It never throws: a screen that cannot take it is loaded again.
function update(message, failed = false, detail = '') {
  statusSeq += 1;
  lastStatus = { message, failed, detail, seq: statusSeq };
  if (failed) logLine(`Start failed: ${message}${detail ? ` | ${detail}` : ''}`);
  sendStatus();
  if (failed) {
    const seq = statusSeq;
    setTimeout(() => {
      if (shownSeq < seq && lastStatus && lastStatus.seq === seq) failureNotShown(lastStatus);
    }, SHOWN_WITHIN_MS);
  }
}

// sendStatus sends the latest status to the start screen, loading the screen again when it cannot take it.
function sendStatus() {
  if (!lastStatus || showingDashboard || !window || window.isDestroyed()) return;
  try {
    window.webContents.send('startup-status', lastStatus);
  } catch (error) {
    logLine(`The start screen did not take a status (${error.message}); loading it again`);
    reloadStartScreen();
  }
}

// reloadStartScreen loads the start screen again (a renderer that died or stopped answering) and sends it the latest
// status. At most once in five seconds, so a screen that keeps failing is not loaded in a loop.
function reloadStartScreen() {
  if (showingDashboard || !window || window.isDestroyed() || Date.now() - reloadedAt < 5000) return;
  reloadedAt = Date.now();
  Promise.resolve()
    .then(() => window.loadFile(path.join(__dirname, 'loading.html')))
    .then(() => sendStatus())
    .catch((error) => logLine(`The start screen could not be loaded again: ${error.message}`));
}

// failureNotShown puts a failure the start screen did not show in front of the person, in a dialog with Retry.
function failureNotShown(status) {
  logLine(`The start screen did not show the failure; asking in a dialog: ${status.message}`);
  reloadStartScreen();
  if (!window || window.isDestroyed()) return;
  Promise.resolve()
    .then(() => dialog.showMessageBox(window, { type: 'error', title: 'Decurix could not start', message: status.message,
      detail: status.detail || '', buttons: ['Retry', 'Close'], defaultId: 0, cancelId: 1 }))
    .then((answer) => { if (answer && answer.response === 0 && !starting && window) start(); })
    .catch((error) => logLine(`The failure dialog could not be shown: ${error.message}`));
}

function logFile() {
  return path.join(dataDir(), 'desktop.log');
}

// logLine adds one line of the app's own to desktop.log, beside what the
// programs it runs print there.
function logLine(line) {
  try { fs.appendFileSync(logFile(), `${new Date().toISOString()} ${line}\n`); } catch { /* the log is decoration */ }
}

// The newest line of desktop.log: why the control plane stopped. Empty when
// the log cannot be read; it is only ever decoration.
function lastLogLine() {
  try {
    const file = fs.openSync(logFile(), 'r');
    try {
      const size = fs.fstatSync(file).size;
      const tail = Buffer.alloc(Math.min(size, 16384));
      fs.readSync(file, tail, 0, tail.length, size - tail.length);
      const lines = tail.toString('utf8').split(/[\r\n]+/).map((line) => line.trim()).filter(Boolean);
      return (lines.pop() || '').slice(0, 240);
    } finally { fs.closeSync(file); }
  } catch { return ''; }
}

// getText resolves with a GET's status and the first limit characters of its
// body, or null when there is no answer.
function getText(url, timeout, limit) {
  return new Promise((resolve) => {
    const req = http.get(url, { timeout }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => { body = (body + chunk).slice(0, limit); });
      res.on('end', () => resolve({ status: res.statusCode, body }));
      res.on('error', () => resolve(null));
    });
    req.on('timeout', () => req.destroy());
    req.on('error', () => resolve(null));
  });
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function checkDashboard() {
  const res = await getText(`${dashboard}/favicon.svg`, 2000, 20000);
  if (!res) return 'absent';
  return res.status === 200 && res.body.includes(marker) ? 'ready' : 'occupied';
}

async function waitForDashboard(stopped) {
  for (let attempt = 0; attempt < 120 && !stopped(); attempt += 1) {
    const state = await checkDashboard();
    if (state === 'ready') return;
    if (state === 'occupied') throw new Error('Port 8080 is in use. Close the other server, then retry.');
    await sleep(1000);
  }
  if (!stopped()) throw new Error(`Decurix did not start. Check ${logFile()}`);
}

async function backendLost() {
  showingDashboard = false;
  const detail = lastLogLine();
  // Stopping the phone link also tells the control plane the link is gone -
  // which fails, because the control plane is what died. Found live: that
  // error ended the restart before it began, and the window stayed dead.
  try { await stopMobileAccess(); } catch (error) { console.warn('Could not stop quick tunnel:', error.message); }
  if (closing || !window || window.isDestroyed()) return;
  if (windows) window.setTitleBarOverlay(startupPalette());
  await window.loadFile(path.join(__dirname, 'loading.html'));

  const now = Date.now();
  restarts = restarts.filter((at) => now - at < RESTART_WINDOW_MS);
  if (restarts.length >= MAX_RESTARTS) {
    update(`Decurix keeps stopping. Check ${logFile()}`, true, detail);
    return;
  }
  restarts.push(now);
  update('Decurix stopped. Starting it again…', false, detail);
  start();
}

function watchBackend() {
  if (watching) return;
  watching = true;
  watchKeepAwake();
  // The control plane keeps the link in memory only: told again every minute,
  // so a control plane that restarted on its own shows it again.
  setInterval(() => { if (!closing && showingDashboard) reportLanAddress(); }, LAN_REPORT_EVERY_MS);
  setInterval(async () => {
    if (closing || starting || !showingDashboard || !window || window.isDestroyed()) return;
    if (await checkDashboard() === 'ready') { misses = 0; return; }
    misses += 1;
    if (misses < LOST_AFTER_MISSES) return;
    misses = 0;
    try { await backendLost(); } catch (error) { console.error('Could not restart Decurix:', error); }
  }, WATCH_EVERY_MS);
}

// ---- the control plane in Docker ----

function stackDir() {
  return path.join(dataDir(), 'stack');
}

// What docker is run with: a PATH it and its helpers are found on, and the
// two host folders the compose file mounts (scripts/mac/lib/host-mounts.sh,
// scripts/linux/build.sh: a Mac shares its home folder and has no Windows
// drive; Linux under WSL gives the Windows drive, and Linux without one an
// empty folder - never the host's root, which would put /etc and every home
// folder in reach of the control plane's host-file checks).
function stackEnv() {
  // The project name a checkout's own `docker compose up` uses, so the app
  // takes over a control plane started that way instead of colliding with
  // its container's name - and keeps its records (the decurix-data volume).
  const env = { ...process.env, PATH: searchPath().join(path.delimiter), COMPOSE_PROJECT_NAME: 'decurix', DECURIX_HOST_HOME: os.homedir() };
  if (process.platform !== 'darwin' && fs.existsSync('/mnt/c')) {
    env.DECURIX_HOST_MNT_C = '/mnt/c';
  } else {
    env.DECURIX_HOST_MNT_C = path.join(dataDir(), 'no-windows-drive');
    fs.mkdirSync(env.DECURIX_HOST_MNT_C, { recursive: true });
  }
  return env;
}

// run starts one program, its output going to desktop.log, and resolves with
// its exit code - or 'missing' when there is no such program. onLine is given
// each line it prints.
function run(file, args, onLine, options = {}) {
  return new Promise((resolve) => {
    let log = null;
    try { log = fs.openSync(logFile(), 'a'); } catch { /* the log is decoration */ }
    // Not in the app's own folder: a program that outlives the app (a
    // download a helper script started) would keep that folder from being
    // replaced by an update or removed by the uninstaller.
    const child = spawn(file, args, { windowsHide: true, cwd: os.tmpdir(), ...options, stdio: ['ignore', 'pipe', 'pipe'] });
    let rest = '';
    let warned = false;
    // A callback that throws (a start screen that cannot take a line) must not stop the output being read and logged,
    // nor the program's end being seen.
    const give = (line) => {
      try { onLine(line); } catch (error) {
        if (!warned) { warned = true; logLine(`${path.basename(file)}: a line of its output could not be shown: ${error.message}`); }
      }
    };
    const take = (chunk) => {
      if (log !== null) { try { fs.writeSync(log, chunk); } catch { /* decoration */ } }
      if (!onLine) return;
      const lines = (rest + chunk.toString()).split(/[\r\n]+/);
      rest = lines.pop();
      for (const line of lines) if (line.trim()) give(line.trim().slice(0, 4096));
    };
    child.stdout.on('data', take);
    child.stderr.on('data', take);
    let settled = false;
    const done = (result) => {
      if (settled) return;
      settled = true;
      // The last line, printed without a newline, is often the error.
      if (onLine && rest.trim()) give(rest.trim().slice(0, 4096));
      rest = '';
      if (log !== null) { try { fs.closeSync(log); } catch { /* already closed */ } log = null; }
      resolve(result);
    };
    child.once('error', (error) => done(error.code === 'ENOENT' ? 'missing' : 1));
    child.once('exit', (code) => done(code));
  });
}

// wsl runs wsl.exe. WSL_UTF8 makes it print UTF-8 rather than UTF-16.
function wsl(args, onLine, options = {}) {
  return run('wsl.exe', args, onLine, { ...options, env: { ...process.env, WSL_UTF8: '1' } });
}

// inDistro runs a program as root inside the selected WSL distro.
function inDistro(args, onLine, cwd = '/', options = {}) {
  return wsl(['-d', runtimeDistro, '-u', 'root', '--cd', cwd, '-e', ...args], onLine, options);
}

// QUICK is the spawn option for a call that only asks something (a version,
// a list, a file, a route): a wsl.exe or docker that hangs - WSL starting up,
// a daemon that stopped answering - is ended after it, and counts as a
// failure, instead of holding the start (and its Retry) for good. Calls that
// install, build or pull legitimately take long and get none.
const QUICK = { timeout: 30000 };

// docker runs one docker command in the stack's folder - on Windows inside
// the app's distro, given what stackEnv gives elsewhere as plain variables.
// On Windows the gated port is published on the distro's 127.0.0.1 only
// (DECURIX_LAN_BIND): the LAN link and the tunnel reach it through Windows'
// own 127.0.0.1 (windows-lan.js), and published on every address it let a
// desktop come back in through Docker's proxy looking like this computer.
// DECURIX_HOST_KIND=wsl tells the control plane it runs on the app's WSL host,
// whose desktops are kept apart by dockerd.sh's fence: it then refuses to
// make or start a desktop while that fence is not in place (desktop.HostFence).
function docker(args, onLine, options = {}) {
  if (windows) {
    return inDistro(['env', 'COMPOSE_PROJECT_NAME=decurix', 'DECURIX_HOST_HOME=/home', 'DECURIX_HOST_MNT_C=/mnt/c', 'DECURIX_LAN_BIND=127.0.0.1', 'DECURIX_HOST_KIND=wsl', 'docker', ...(runtimeDistro !== DISTRO ? ['--host', 'unix:///var/run/docker.sock'] : []), ...args], onLine, DISTRO_STACK, options);
  }
  return run(findProgram('docker') || 'docker', args, onLine, { ...options, cwd: stackDir(), env: stackEnv() });
}

// ensureSettings makes the person's .env, the one file of settings they edit
// (in the data folder), and returns its path; each start copies it to where
// compose and the image read it.
function ensureSettings() {
  fs.mkdirSync(dataDir(), { recursive: true });
  const example = path.join(dataDir(), '.env.example');
  if (!fs.existsSync(example)) fs.copyFileSync(resource('.env.example'), example);
  const config = path.join(dataDir(), '.env');
  if (!fs.existsSync(config)) {
    // Windows uses native Ollama; other platforms keep the container default.
    // An explicit remote OLLAMA_URL still names another server.
    fs.writeFileSync(config, windows
      ? 'DECURIX_WINDOWS_NATIVE_OLLAMA=1\nOLLAMA_MODEL=auto\n'
      : 'OLLAMA_URL=http://decurix-ollama:11434\nOLLAMA_MODEL=auto\n', 'utf8');
  }
  return config;
}

// stageStack puts what the app ships into the data folder, where docker can
// build from it and the person's own .env sits beside it. Copied again only
// when the app is another build than the one that staged it. (On Windows the
// distro's setup.sh does this, inside the distro.)
function stageStack() {
  const from = resource('stack');
  fs.mkdirSync(stackDir(), { recursive: true });
  const shipped = fs.readFileSync(path.join(from, 'VERSION'), 'utf8');
  let staged = '';
  try { staged = fs.readFileSync(path.join(stackDir(), 'VERSION'), 'utf8'); } catch { /* first run */ }
  if (staged !== shipped) {
    for (const name of ['decurix', 'decurix-android-arm64', 'Dockerfile', 'docker-compose.yaml']) {
      fs.copyFileSync(path.join(from, name), path.join(stackDir(), name));
    }
    fs.writeFileSync(path.join(stackDir(), 'VERSION'), shipped, 'utf8');
  }
  fs.copyFileSync(ensureSettings(), path.join(stackDir(), '.env'));
}

// stackIsStale reports whether what is staged (and so what is running) came
// from another build of the app than this one. On Windows a distro that does
// not answer counts as stale: whatever answers on 8080 then is not this app's
// stack (the Windows control plane of an earlier version of the app).
async function stackIsStale() {
  let shipped;
  try { shipped = fs.readFileSync(path.join(resource('stack'), 'VERSION'), 'utf8').trim(); } catch { return false; }
  if (windows) {
    let staged = '';
    const code = await inDistro(['cat', `${DISTRO_STACK}/VERSION`], (line) => { staged = line; }, '/', QUICK);
    return code !== 0 || staged.trim() !== shipped;
  }
  try { return fs.readFileSync(path.join(stackDir(), 'VERSION'), 'utf8').trim() !== shipped; } catch { return false; }
}

// Keep a chosen runtime across launches: switching daemons would hide the
// person's workers and records. Older installs keep their Decurix distro.
async function selectWindowsRuntime() {
  if (!windows || runtimeSelected) return;
  const file = path.join(dataDir(), 'windows-runtime.json');
  try {
    const saved = JSON.parse(fs.readFileSync(file, 'utf8'));
    if (typeof saved.distro !== 'string' || !saved.distro.trim() || /[\r\n\0]/.test(saved.distro) || /^docker-desktop/i.test(saved.distro)) {
      throw new Error('Invalid saved WSL runtime');
    }
    runtimeDistro = saved.distro;
    runtimeSelected = true;
    return;
  } catch (error) {
    if (error.code !== 'ENOENT') throw new Error('Could not read the saved WSL runtime. Check ' + file);
  }

  const names = [];
  let candidate = '';
  const listed = await wsl(['--list', '--quiet'], (line) => names.push(line.replace(/\0/g, '').trim()), { timeout: 15000 });
  if (listed === 0 && !names.includes(DISTRO)) {
    update('Checking existing WSL runtimes…');
    for (const name of names.filter((name) => name && !/^docker-desktop/i.test(name))) {
      // The app needs a local Linux Docker Engine, Compose, Windows interop,
      // and host mounts. Docker Desktop's proxy socket leads to another VM
      // where our host-app bridge cannot run, so it is not compatible here.
      const probe = 'test -S /var/run/docker.sock && test -d /mnt/c && command -v bash && command -v wslpath && ' +
        'docker --host unix:///var/run/docker.sock version --format "{{.Server.Version}}" && ' +
        'docker --host unix:///var/run/docker.sock compose version && ' +
        'os=$(docker --host unix:///var/run/docker.sock info --format "{{.OperatingSystem}}") && ' +
        'case "$os" in *[Dd]ocker" "[Dd]esktop*) exit 1;; *) exit 0;; esac';
      if (await wsl(['-d', name, '-u', 'root', '--cd', '/', '-e', 'sh', '-c', probe], undefined, { timeout: 15000 }) === 0) {
        candidate = name;
        break;
      }
    }
  }
  const record = { distro: DISTRO };
  if (candidate && await askToAdopt(candidate)) {
    record.distro = candidate;
    record.adopted = true;
    record.preexisting = await inventoryOf(candidate);
    // inventoryOf throws unless every listing answered; an uninstall keeps
    // everything when a record lacks this (wsl/cleanup-existing.ps1, A-04).
    record.preexistingComplete = true;
  }
  runtimeDistro = record.distro;
  fs.mkdirSync(dataDir(), { recursive: true });
  fs.writeFileSync(file, JSON.stringify(record) + '\n', 'utf8');
  runtimeSelected = true;
}

// askToAdopt asks the person whether Decurix may use the Docker Engine an
// existing WSL distro already runs - its containers, volumes and compose
// project live beside theirs - or should set up a Linux of its own. The
// default is its own: sharing is a choice, never a silent takeover.
async function askToAdopt(name) {
  const options = {
    type: 'question',
    title: 'Decurix',
    message: `Use the Docker Engine in your WSL distro "${name}"?`,
    detail: `Decurix can run its control plane and workers in "${name}"'s Docker Engine, beside what you run there, `
      + 'or set up a Linux of its own. If it uses yours, uninstalling Decurix later removes only what Decurix created there; '
      + 'what is there now stays.',
    buttons: [`Use "${name}"`, 'Set up Decurix\'s own Linux'],
    defaultId: 1,
    cancelId: 1,
    noLink: true,
  };
  const { response } = window && !window.isDestroyed() ? await dialog.showMessageBox(window, options) : await dialog.showMessageBox(options);
  return response === 0;
}

// inventoryOf lists the Decurix-looking resources (containers, volumes,
// images) an engine already holds before Decurix first uses it, so an
// uninstall keeps them (wsl/maintain-existing.sh). A runtime whose inventory
// cannot be read in full is not adopted: the script fails when any one listing
// does.
async function inventoryOf(name) {
  const found = [];
  const code = await wsl(['-d', name, '-u', 'root', '--cd', '/', '-e', 'sh', linuxPath(resource(path.join('wsl', 'maintain-existing.sh'))), 'inventory'],
    (line) => { const match = /^decurix-resource ([A-Za-z0-9_.:/-]+)$/.exec(line.trim()); if (match) found.push(match[1]); }, { timeout: 60000 });
  if (code !== 0) throw new Error(`Could not list what "${name}"'s Docker Engine already holds, so Decurix did not start using it. Start its Docker Engine, then retry.`);
  return found;
}

// dockerReady waits for a Docker that answers. On a Mac, Docker Desktop is
// started when it is installed and not running; Windows uses its saved WSL
// runtime, or prepares the app-owned fallback and starts its daemon.
async function dockerReady() {
  if (windows) {
    await selectWindowsRuntime();
    if (runtimeDistro === DISTRO) { await linuxReady(); return; }
    await stopEarlierWindowsApp();
    const message = 'Using your existing WSL runtime (' + runtimeDistro + ')…';
    update(message);
    const ready = await inDistro(['sh', linuxPath(resource(path.join('wsl', 'stage-existing.sh'))), linuxPath(resource('stack')), linuxPath(ensureSettings())],
      (line) => update(message, false, line));
    if (ready !== 0) throw new Error('The saved WSL runtime (' + runtimeDistro + ') is unavailable or incompatible. Start its Docker Engine, then retry. Your workers remain in that runtime.');
    return;
  }
  const mac = process.platform === 'darwin';
  const needs = mac ? 'Docker Desktop' : 'Docker';
  const ask = () => docker(['version', '--format', '{{.Server.Version}}'], undefined, QUICK);
  let answer = await ask();
  if (answer === 'missing') {
    throw new Error(`Decurix needs ${needs}: its workers are Linux desktops that run in it. Install ${needs}, start it, then retry.`);
  }
  if (answer !== 0 && mac) {
    update('Starting Docker Desktop…');
    spawn('open', ['-ga', 'Docker'], { stdio: 'ignore' }).once('error', () => {});
    for (let attempt = 0; attempt < 60 && answer !== 0 && !closing; attempt += 1) {
      await sleep(2000);
      answer = await ask();
    }
  }
  if (answer !== 0) {
    throw new Error(`${needs} is not running${mac ? '' : ', or this user may not use it (the docker group)'}. Start it, then retry.`);
  }
}

// ---- Windows: the app's own WSL distro ----

// wslInstalled: whether this Windows has WSL 2 - the Store version, which
// answers --version (the older in-box one does not).
async function wslInstalled() {
  return await wsl(['--version'], undefined, QUICK) === 0;
}

// A restart is asked for at most once per reason. Found on a second PC: WSL
// still did not answer after the restart, the app asked for another, and
// another - a loop with no way out. What was asked, and when, is kept; a
// Windows booted since then has had its restart, and is not asked again.
function restartFile() {
  return path.join(dataDir(), 'restart-asked.json');
}

function restartedSinceAsked(reason) {
  try {
    const asked = JSON.parse(fs.readFileSync(restartFile(), 'utf8'));
    const booted = Date.now() - os.uptime() * 1000;
    return asked.reason === reason && booted > asked.at;
  } catch { return false; }
}

// installerRestartPending: the installer turned Windows' features on and asked
// for a restart that has not happened yet (wsl/prepare-windows.ps1 notes it).
// WSL missing or Linux not starting is then that restart's to fix, not another
// administrator prompt's.
function installerRestartPending() {
  try {
    const asked = JSON.parse(fs.readFileSync(restartFile(), 'utf8'));
    return asked.reason === 'installer' && !restartedSinceAsked('installer');
  } catch { return false; }
}

const RESTART_AFTER_INSTALL = 'Decurix\'s installer turned on what Windows needs to run Linux. Restart Windows to finish, then open Decurix again.';

function askForRestart(reason, message) {
  try {
    fs.mkdirSync(dataDir(), { recursive: true });
    fs.writeFileSync(restartFile(), JSON.stringify({ reason, at: Date.now() }) + '\n', 'utf8');
  } catch { /* without the note the person is asked again: the old behaviour */ }
  throw new Error(message);
}

// wslSays is the first thing WSL prints for these arguments, for a message.
async function wslSays(args) {
  const lines = [];
  await wsl(args, (line) => { const text = line.replace(/\0/g, '').trim(); if (text) lines.push(text); }, QUICK);
  return lines.slice(0, 2).join(' ').slice(0, 300);
}

// The one administrator prompt that turns WSL on: --install enables its
// Windows features, and --update installs WSL itself. Both fetch from GitHub
// (--web-download), never the Microsoft Store: on a PC whose Store download
// fails, `wsl --update` installed nothing and said nothing, and only
// `wsl --update --web-download` by hand got it going (found 2026-10-13).
const ENABLE_WSL = "Start-Process -FilePath cmd.exe -ArgumentList '/c','wsl.exe --install --no-distribution --web-download & wsl.exe --update --web-download' -Verb RunAs -Wait";

// installWsl turns WSL on: Windows asks the person for an administrator's yes
// once, and a computer that never had it then needs a restart.
async function installWsl() {
  const restarted = restartedSinceAsked('wsl');
  const message = 'Turning on Windows Subsystem for Linux. Windows will ask you to allow it.';
  update(message);
  const code = await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', ENABLE_WSL], (line) => update(message, false, line));
  if (code !== 0) throw new Error('Windows Subsystem for Linux could not be turned on (was the administrator prompt declined?). Retry to try again.');
  if (await wslInstalled()) return;
  if (!restarted) askForRestart('wsl', 'Windows Subsystem for Linux is installed. Restart Windows to finish, then open Decurix again.');
  const said = await wslSays(['--status']);
  throw new Error('Windows Subsystem for Linux still does not start, even after a restart'
    + (said ? ` (Windows says: ${said})` : '') + '. Open PowerShell as administrator and run: '
    + 'wsl --update --web-download - then restart Windows and open Decurix again. '
    + `Details are in ${logFile()}`);
}

// What a first install needs of the computer, asked before anything is set up
// so a person is told in plain words rather than left with a log to read. The
// distro is x86-64 Linux (the worker images are amd64), WSL 2 needs Windows 10
// 2004 (build 19041), and the first start downloads and builds the worker
// desktop: FIRST_INSTALL_GB is that, Docker, and room to work in.
const MIN_WINDOWS_BUILD = 19041;
const FIRST_INSTALL_GB = 20;

function checkThisComputer() {
  const arch = [process.arch, process.env.PROCESSOR_ARCHITECTURE, process.env.PROCESSOR_ARCHITEW6432].filter(Boolean).join(' ');
  if (/arm64/i.test(arch)) {
    throw new Error('Decurix needs a 64-bit Intel or AMD computer. This one has an ARM processor, which its workers cannot run on.');
  }
  const build = Number(os.release().split('.')[2]);
  if (build && build < MIN_WINDOWS_BUILD) {
    throw new Error(`Decurix needs Windows 10 version 2004 or newer, or Windows 11. Update Windows (this is build ${build}), then open Decurix again.`);
  }
  let free = Infinity;
  try {
    fs.mkdirSync(distroDir(), { recursive: true });
    const disk = fs.statfsSync(distroDir());
    free = Number(disk.bavail) * Number(disk.bsize);
  } catch { /* a disk that cannot be asked is not known to be full */ }
  if (free < FIRST_INSTALL_GB * 1024 ** 3) {
    throw new Error(`Setting up Decurix needs about ${FIRST_INSTALL_GB} GB of free disk space, and this computer has ${Math.floor(free / 1024 ** 3)} GB. Free some space, then retry.`);
  }
}

// What WSL says when it cannot start a Linux at all: the Virtual Machine
// Platform feature is off, or the processor's virtualization is switched off
// in the computer's firmware.
const NO_VIRTUALIZATION = /virtuali[sz]ation|virtual machine platform|HCS_E_HYPERV_NOT_INSTALLED|0x80370102|0x80370114|0x8004032d|WSL2 is not supported/i;

// importDistro makes the app's distro from the rootfs it ships. A Windows
// whose Virtual Machine Platform is off is asked to turn it on (the same
// administrator prompt as turning WSL on), once; that needs a restart, and
// what a restart does not cure is the firmware's setting.
async function importDistro() {
  const message = 'Setting up Linux for Decurix…';
  const attempt = async () => {
    update(message);
    let said = '';
    const code = await wsl(['--import', DISTRO, distroDir(), resource(path.join('wsl', 'alpine-minirootfs.tar.gz')), '--version', '2'],
      (line) => { said += line.replace(/\0/g, '') + '\n'; update(message, false, line); });
    return { code, said };
  };
  const first = await attempt();
  if (first.code === 0) return;
  const said = first.said.split('\n').map((line) => line.trim()).filter(Boolean).slice(0, 2).join(' ').slice(0, 300);
  if (!NO_VIRTUALIZATION.test(first.said)) throw new Error(`Could not set up Linux for Decurix${said ? ` (WSL says: ${said})` : ''}. Check ${logFile()}`);
  if (installerRestartPending()) throw new Error(RESTART_AFTER_INSTALL);

  // Already restarted once for this: another prompt and another restart
  // change nothing, so say what is left to do.
  if (restartedSinceAsked('platform')) {
    throw new Error('Windows still cannot run Linux after the restart'
      + (said ? ` (WSL says: ${said})` : '') + '. Virtualization is switched off: turn on Intel VT-x, AMD-V or SVM '
      + 'in the computer\'s BIOS/UEFI settings. If it is already on, open PowerShell as administrator, run '
      + '"bcdedit /set hypervisorlaunchtype auto", restart Windows and open Decurix again.');
  }

  const asking = 'Turning on Windows\' Virtual Machine Platform. Windows will ask you to allow it.';
  update(asking);
  // Its exit is not the answer (a declined prompt and a done one both end it): the import tried again is.
  await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', ENABLE_WSL], (line) => update(asking, false, line));
  if ((await attempt()).code === 0) return;
  askForRestart('platform', 'Windows cannot run Linux on this computer yet. Restart Windows, then open Decurix again. '
    + 'If you see this again after the restart, virtualization is switched off in the computer\'s BIOS/UEFI settings '
    + '(Intel VT-x, AMD-V or SVM): turn it on there.');
}

async function distroExists() {
  const names = [];
  const code = await wsl(['--list', '--quiet'], (line) => names.push(line.replace(/\0/g, '').trim()), QUICK);
  // With no distro at all WSL answers with an error.
  return code === 0 && names.includes(DISTRO);
}

// stopEarlierWindowsApp ends what an earlier version of the app ran on Windows
// itself, which this one replaces: its control plane from the install folder
// (holding port 8080). The native model server is now kept running.
async function stopEarlierWindowsApp() {
  const quote = (text) => `'${text.replace(/'/g, "''")}'`;
  const folder = quote(path.join(process.resourcesPath, 'decurix.exe'));
  const script = `$cp = ${folder}; Get-CimInstance Win32_Process | `
    + "? { $_.ExecutablePath -and $_.ExecutablePath -ieq $cp } | "
    + '% { Stop-Process -Id $_.ProcessId -Force }';
  // Best effort: an earlier app still holding 8080 is found by the next check (checkDashboard, 'occupied').
  await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], undefined, QUICK);
}

// The wsl.exe that runs the daemon (dockerd.sh) is what keeps the distro, the
// daemon and every desktop running, after this window is closed too - so it
// must outlive this app, and must have no window. A child of this app cannot
// do both: libuv puts every child that is not detached in a job that ends it
// with the app, and a detached one has no console, so wsl.exe makes one, which
// Windows 11 hands to Windows Terminal - an empty "wsl.exe" window, found on
// the first run of the app. So Windows' own process service (WMI) starts it:
// not this app's child, and with a console that is created hidden.
const DAEMON_COMMAND = `wsl.exe -d ${DISTRO} -u root -e /opt/decurix/dockerd.sh`;

function startHiddenScript(commandLine) {
  return "$s = New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ShowWindow=[uint16]0}; "
    + `$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine='${commandLine.replace(/'/g, "''")}'; ProcessStartupInformation=$s}; `
    + 'exit [int]$r.ReturnValue';
}

// linuxReady makes the distro ready and its Docker daemon running: WSL on,
// the distro imported, Docker Engine installed and the stack staged
// (setup.sh), then the daemon started (dockerd.sh) and waited for.
async function linuxReady() {
  await stopEarlierWindowsApp();
  const hasWsl = await wslInstalled();
  if (!hasWsl || !await distroExists()) {
    checkThisComputer();
    if (!hasWsl && installerRestartPending()) throw new Error(RESTART_AFTER_INSTALL);
    if (!hasWsl) await installWsl();
    await importDistro();
  }

  const message = 'Preparing Linux. The first time, this installs Docker.';
  update(message);
  // setup.sh downloads Docker Engine on a first start, so it runs under the stall watchdog like a build: every apk
  // call in it is bounded and falls back to another mirror (apk-safe.sh), and says it is still running every 30 s, so
  // silence for SETUP_STALL_MS means something else hung. Then it is ended in the distro and run again (it is safe
  // to run again), and a second stall is an error with its last line and the start's Retry.
  const setup = await watchBuild({
    stallMs: SETUP_STALL_MS,
    attempt: (line, signal) => inDistro(['sh', linuxPath(resource(path.join('wsl', 'setup.sh'))), linuxPath(resource('stack')), linuxPath(ensureSettings())],
      line, '/', { signal }),
    onLine: (line) => update(message, false, line),
    onStall: async (n, last) => {
      console.warn(`setup.sh: no output for ${Math.round(SETUP_STALL_MS / 60000)} minutes; ended (try ${n}). Last line: ${last}`);
      await stopSetup();
      if (n === 1) update(`${message} It stopped making progress, so it is being tried again.`, false, last);
    },
  });
  if (setup.stalled) throw new Error(`${stalledMessage('Could not prepare Linux for Decurix.', SETUP_STALL_MS, setup.lastLine)} Check ${logFile()}`);
  if (setup.code !== 0) throw new Error(`Could not prepare Linux for Decurix. Check ${logFile()}`);

  const ask = () => inDistro(['docker', 'version', '--format', '{{.Server.Version}}'], undefined, '/', QUICK);
  if (await ask() === 0) return;
  update('Starting Docker…');
  if (await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', startHiddenScript(DAEMON_COMMAND)]) !== 0) {
    throw new Error(`Docker could not be started in Decurix's Linux. Check ${logFile()}`);
  }
  for (let attempt = 0; attempt < 60 && !closing; attempt += 1) {
    if (await ask() === 0) return;
    await sleep(1000);
  }
  throw new Error(`Docker did not start in Decurix's Linux. Its log is /var/log/dockerd.log there (wsl -d ${DISTRO}).`);
}

// SETUP_STALL_MS is the silence that counts as a stalled setup.sh: its apk calls print at least every 30 s while
// they run (apk-safe.sh), and nothing else in it waits on the network.
const SETUP_STALL_MS = 5 * 60 * 1000;

// stopSetup ends a setup.sh the watchdog cancelled, in the distro (ending wsl.exe does not end it there), and the apk
// it was waiting on, so the next run takes apk's lock cleanly.
async function stopSetup() {
  try {
    await inDistro(['pkill', '-f', 'wsl/setup.sh'], undefined, '/', QUICK);
    await inDistro(['pkill', '-x', 'apk'], undefined, '/', QUICK);
  } catch { /* the next run, or its error, says what is wrong */ }
}

// Windows gets a native model server; only the control plane and desktops use
// Linux. Stage derived settings separately so the user's remote URL and auto
// model choice are never overwritten by a changing WSL gateway address.
let windowsResourcePlan;
let nativeModelHost = '';
const localModels = createLocalModels({
  readPreference() {
    try { return JSON.parse(fs.readFileSync(path.join(dataDir(), 'local-hosting.json'), 'utf8')).enabled === true; }
    catch (error) { if (error.code !== 'ENOENT') console.warn('Could not read local hosting preference:', error.message); return false; }
  },
  writePreference(enabled) {
    fs.mkdirSync(dataDir(), { recursive: true });
    fs.writeFileSync(path.join(dataDir(), 'local-hosting.json'), JSON.stringify({ enabled }), 'utf8');
  },
  async operate(action, report, signal) {
    if (action !== 'install' && !nativeModelHost) throw new Error('Native hosting is unavailable with this Ollama URL or Windows network configuration.');
    const code = await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass',
      '-File', resource(path.join('wsl', 'native-ollama.ps1')), '-Action', action,
      '-BindAddress', nativeModelHost || '127.0.0.1', '-SettingsFile', ensureSettings()], line => {
        if (line.startsWith('DECURIX_STATE ')) {
          try { report(JSON.parse(line.slice(14))); } catch { report(line); }
        } else report(line);
      }, { signal });
    if (code !== 0) throw new Error('Local model ' + action + ' failed. Check ' + logFile());
    if (action === 'ensure') {
      report('Checking Linux workers can reach Windows Ollama…');
      for (const endpoint of ['http://' + nativeModelHost + ':11435/api/tags', 'http://' + nativeModelHost + ':11436/health']) {
        const reachable = await docker(['compose', 'exec', '-T', 'desktop-manager', 'curl', '--fail', '--silent', '--show-error',
          '--connect-timeout', '5', '--max-time', '10', endpoint]);
        if (reachable !== 0) throw new Error('Linux cannot reach Windows Ollama or its selector. Check the private WSL adapter and firewall.');
      }
    }
  },
});

for (const [channel, action] of [
  ['local-model-status', () => localModels.status()],
  ['local-model-hosting', value => localModels.setEnabled(value)],
]) {
  ipcMain.handle(channel, (event, value) => {
    if (!windows || !fromDashboard(event)) {
      throw new Error('Local hosting is only available from the Windows desktop dashboard.');
    }
    return action(value);
  });
}
function prepareWindowsResources() {
  const settings = windowsRuntime.readSettings(fs.readFileSync(ensureSettings(), 'utf8'));
  windowsResourcePlan = windowsRuntime.configureResources(path.join(os.homedir(), '.wslconfig'), settings, runtimeDistro === DISTRO,
    (message) => { update(message); console.log(message); });
}

async function prepareWindowsModels() {
  const settingsFile = ensureSettings();
  const original = fs.readFileSync(settingsFile, 'utf8');
  const settings = windowsRuntime.readSettings(original);
  if (!windowsResourcePlan) prepareWindowsResources();
  const plan = windowsResourcePlan;
  // Both the Linux filesystem and its backing Windows disk need actual free
  // space. The expandable VHD limit is not a disk reservation.
  const minGB = Number(settings.DECURIX_MIN_DISK_FREE_GB || 10);
  if (!Number.isFinite(minGB) || minGB <= 0) throw new Error('DECURIX_MIN_DISK_FREE_GB must be positive.');
  const disk = fs.statfsSync(runtimeDistro === DISTRO ? distroDir() : (process.env.LOCALAPPDATA || os.homedir()));
  if (Number(disk.bavail) * Number(disk.bsize) < minGB * 1024 ** 3) {
    throw new Error('Less than ' + minGB + ' GB is free on the Windows disk. Free space before starting more workers or downloading models.');
  }
  let freeKB = 0;
  const checked = await inDistro(['sh', '-c', "docker_root=$(docker --host unix:///var/run/docker.sock info --format '{{.DockerRootDir}}') && df -Pk \"$docker_root\" | awk 'NR==2 {print $4}'"], line => { freeKB = Number(line); }, '/', QUICK);
  if (checked !== 0 || !freeKB) throw new Error('Could not check the worker disk capacity in WSL.');
  if (freeKB < minGB * 1024 ** 2) throw new Error('Less than ' + minGB + ' GB is free on the WSL worker disk. Free space or expand its VHD before starting more workers.');
  let host = '';
  if (windowsRuntime.usesWindowsOllama(settings)) {
    let route = '';
    if (await inDistro(['sh', '-c', 'ip -4 route show default'], line => { route += line + '\n'; }, '/', QUICK) !== 0) {
      throw new Error('Could not discover the Windows model server address from WSL.');
    }
    host = windowsRuntime.windowsHost(route, undefined, settings.DECURIX_WINDOWS_OLLAMA_HOST);
    nativeModelHost = host;
  }
  const derived = path.join(dataDir(), 'windows-stack.env');
  fs.writeFileSync(derived, windowsRuntime.stackSettings(original, settings, plan, host), 'utf8');
  // The settings hold the person's keys: root-only from the moment they exist.
  const copied = await inDistro(['sh', '-c', 'umask 077 && tr -d "\\r" < "$1" > "$2" && chmod 600 "$2"', 'decurix-settings', linuxPath(derived), DISTRO_STACK + '/.env'], undefined, '/', QUICK);
  if (copied !== 0) throw new Error('Could not stage the Windows model settings in WSL.');
  return host ? 'http://' + host + ':11435' : '';

}

// startStack builds and starts the control plane in Docker, and the worker
// images it does not have yet. Every step is safe to repeat: a second start
// finds everything built and takes seconds.
async function startStack() {
  if (windows) ensureSettings(); else stageStack();
  if (windows) prepareWindowsResources();
  await dockerReady();
  if (windows) await prepareWindowsModels();

  const step = async (message, args, failure) => {
    update(message);
    const code = await docker(args, (line) => update(message, false, line));
    if (code !== 0) throw new Error(`${failure} Check ${logFile()}`);
  };
  // A build step that downloads runs under the stall watchdog (build-watchdog.js): no new line for its bound and it
  // is cancelled and tried once more; a second stall is an error with the last line, and the start's Retry.
  const buildStep = async (message, args, failure, stallMs, stop) => {
    update(message);
    const result = await watchedDocker(args, (line) => update(message, false, line), stallMs, stop,
      (last, why) => update(`${message} It ${why === 'failed' ? 'failed' : 'stopped making progress'}, so it is being tried once more.`, false, last));
    if (result.stalled) throw new Error(`${stalledMessage(failure, stallMs, result.lastLine)} Check ${logFile()}`);
    if (result.code !== 0) throw new Error(`${failure} Check ${logFile()}`);
  };
  await buildStep('Preparing Decurix…', ['compose', 'build'], 'Could not build Decurix in Docker.', COMPOSE_STALL_MS, stopComposeBuild);
  // The compose file declares the network external (compose will not adopt one the control plane made), so it must
  // exist before compose runs anything. On a fresh install, or after a `compose down`, it does not.
  if (await docker(['network', 'inspect', 'decurix-network'], undefined, QUICK) !== 0) {
    await step('Preparing Decurix…', ['network', 'create', 'decurix-network'], 'Could not create the Docker network.');
  }
  // The desktop images are built by the control plane itself, from the build
  // context inside it; it skips the ones that exist. Only the desktop is
  // waited for: the tools image (Go, Node, Rust, the coding CLIs) is a long
  // run of downloads a desktop does not need to start, and is built after the
  // dashboard is up (buildToolsLater).
  await buildStep('Checking the worker desktop image. A missing or outdated image is built once; existing images and cached layers are reused.',
    ['compose', 'run', '--rm', '--no-deps', '-T', 'desktop-manager', './decurix', '--build-images', '-images', 'desktop'], 'Could not build the worker desktop.',
    IMAGES_STALL_MS, stopImagesRun);
  await step('Starting Decurix…', ['compose', 'up', '-d', '--no-build'], 'Could not start Decurix in Docker.');
  await waitForDashboard(() => closing);
  buildToolsLater();
  return true;
}

// buildToolsLater builds the tools image, if it is missing, without holding
// anything up; its output goes to desktop.log. A desktop made before it is
// done starts without the tools, which the shared tools volume gets once the
// image exists.
// It runs under the stall watchdog too, so a stalled download never holds it (and its
// container) for good: cancelled and tried once more, then given up with its last line.
function buildToolsLater() {
  watchedDocker(['compose', 'run', '--rm', '--no-deps', '-T', 'desktop-manager', './decurix', '--build-images', '-images', 'tools'],
    undefined, IMAGES_STALL_MS, stopImagesRun, (last, why) => console.warn(`Building the tools image ${why === 'failed' ? 'failed' : 'stopped making progress'}; trying once more. Last line:`, last))
    .then((result) => {
      if (result.stalled) console.warn(stalledMessage('Building the tools image failed.', IMAGES_STALL_MS, result.lastLine), 'See', logFile());
      else if (result.code !== 0) console.warn('Building the tools image failed; see', logFile());
    });
}

// The silence that counts as a stalled build. compose build prints BuildKit's progress (a layer's bytes as they
// come, a RUN's own output) as it goes, so five minutes of nothing is a stall. The desktop and tools images are built
// by the control plane through the daemon's classic builder, which prints a RUN's output but nothing while a quiet
// step works (a Go build of the soul with -p 1 on two cores takes minutes and prints nothing), so they get fifteen;
// every download inside them has its own timeout in the Dockerfile as well.
const COMPOSE_STALL_MS = 5 * 60 * 1000;
const IMAGES_STALL_MS = 15 * 60 * 1000;

// watchedDocker runs one docker build command under the stall watchdog: cancelled (its process ended, then stop()
// cleans up what it left) when it prints nothing for stallMs, and tried once more. onRetry is told the last line
// when a stalled try is to be tried again.
// A build that fails (exits non-zero, not a stall) is tried once more on its own: its cached layers make that cheap,
// and a moment's failure of a CDN (a ReadTimeout from PyPI, 2026-11-04) is the common cause. onRetry is told why
// ('stalled' or 'failed') and the last line.
async function watchedDocker(args, onLine, stallMs, stop, onRetry = () => {}) {
  const once = () => watchBuild({
    stallMs,
    attempt: (line, signal) => docker(args, line, { signal }),
    onLine: onLine || (() => {}),
    onStall: async (n, last) => {
      console.warn(`docker ${args.join(' ')}: no output for ${Math.round(stallMs / 60000)} minutes; cancelled (try ${n}). Last line: ${last}`);
      try { await stop(); } catch { /* the next try or the error says what is wrong */ }
      if (n === 1) onRetry(last, 'stalled');
    },
  });
  const result = await once();
  if (result.stalled || result.code === 0 || closing) return result;
  logLine(`docker ${args.join(' ')} exited ${result.code}: build failed, trying once more. Last line: ${result.lastLine}`);
  onRetry(result.lastLine, 'failed');
  try { await stop(); } catch { /* the second try or its error says what is wrong */ }
  return once();
}

// stopComposeBuild ends a cancelled compose build. Outside Windows the docker client the app started is the build's
// own process, and ending it (the watchdog's signal) cancels the build. On Windows that process is wsl.exe, and the
// client runs on inside the distro, so it is ended there too.
async function stopComposeBuild() {
  if (windows) await inDistro(['pkill', '-TERM', '-f', 'docker compose build'], undefined, '/', QUICK);
}

// stopImagesRun ends a cancelled image build run by `compose run` (the control plane's --build-images): its one-off
// container is removed, which ends the build it asked the daemon for. A container left by an earlier run is too.
async function stopImagesRun() {
  const ids = [];
  await docker(['ps', '-q', '--filter', 'label=com.docker.compose.project=decurix', '--filter', 'label=com.docker.compose.oneoff=True'],
    (line) => ids.push(line), QUICK);
  if (ids.length > 0) await docker(['rm', '-f', ...ids], undefined, QUICK);
  await stopComposeBuild();
}

// The gated listener is published on the network by compose, so a phone on
// the same Wi-Fi can use it without the tunnel: the Mobile menu is told this
// computer's address, found by asking which one a packet to the internet
// would leave from (nothing is sent). scripts/*/build.sh do the same. On
// Windows the stack is in WSL, which hands its ports to this computer only:
// windows-lan.js forwards the address there first (one administrator prompt;
// DECURIX_WSL_LAN_FORWARD=0 in .env skips it), and no link is given without it.
//
// It is reported at every start - an earlier version reported it only when it
// started the stack itself, so a PC whose stack was already running never got
// the link - and again every LAN_REPORT_EVERY_MS. Whether the forward is in
// place is asked once per address (lanForward), not every minute: Windows is
// asked to forward an address and port at most once while the app runs (a
// failed or declined forward is tried again at the next start, as its log
// line says), and never while an earlier ask is still open - a prompt that
// came back every minute, and stacked behind one not yet answered, was the
// fault (review 2026-11-03).
const LAN_REPORT_EVERY_MS = 60000;
let lanForward = { address: '', port: 0, ok: false };
let lanAsking = null;
const lanAsked = new Set();

function publishLanURL(url) {
  const body = JSON.stringify({ lan_url: url });
  const req = http.request(`${dashboard}/api/tunnel`, { method: 'POST', timeout: 5000, headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) } }, (res) => res.resume());
  req.on('timeout', () => req.destroy());
  req.on('error', () => {});
  req.end(body);
}

function reportLanAddress() {
  const socket = dgram.createSocket('udp4');
  const give = () => { try { socket.close(); } catch { /* closed */ } };
  socket.once('error', () => { give(); publishLanURL(''); });
  socket.connect(80, '1.1.1.1', async () => {
    let address = '';
    try { address = socket.address().address; } catch { /* no network */ }
    give();
    if (!address || address.startsWith('127.')) {
      if (windows && lanForward.address !== '-') { lanForward = { address: '-', ok: false }; logLine('LAN link: this computer has no network address'); }
      publishLanURL('');
      return;
    }
    // The same reading of .env as the tunnel's (gated-port.js); whether it
    // really is the gated listener is checked before Windows forwards it.
    const port = String(lanPort(dataDir(), {}));
    let settings = '';
    try { settings = fs.readFileSync(path.join(dataDir(), '.env'), 'utf8'); } catch { /* the defaults */ }
    if (windows) {
      if (/^\s*DECURIX_WSL_LAN_FORWARD\s*=\s*['"]?0/m.test(settings)) { publishLanURL(''); return; }
      const key = `${address}:${port}`;
      if (lanForward.address !== address || lanForward.port !== Number(port) || !lanForward.ok) {
        if (lanAsking) { publishLanURL(''); return; }
        lanForward = { address, port: Number(port), ok: false };
        if (!lanAsked.has(key)) {
          lanAsked.add(key);
          lanAsking = ensureLanForward(dataDir(), address, Number(port), logLine)
            .catch((error) => { logLine(`LAN link: could not forward ${address}: ${error.message}`); return false; });
          try { lanForward.ok = await lanAsking; } finally { lanAsking = null; }
        }
      }
      if (!lanForward.ok) { publishLanURL(''); return; }
    }
    publishLanURL(`http://${address}:${port}`);
  });
}

// A control plane in a container cannot keep the computer awake, and nothing
// runs on a sleeping one - no schedule, no email trigger. So while the app is
// open it asks whether anything needs the computer awake (the question
// scripts/*/lib/keep-awake.sh ask) and holds it awake only while something does.
const KEEP_AWAKE_EVERY_MS = 30000;
let keepingAwake = null;

async function askKeepAwake() {
  const res = await getText(`${dashboard}/api/keep-awake?holder=script`, 5000, 8192);
  if (!res) return false;
  try { const state = JSON.parse(res.body); return state.enabled !== false && state.wanted === true; } catch { return false; }
}

function releaseAwake() {
  if (keepingAwake === null) return;
  powerSaveBlocker.stop(keepingAwake);
  keepingAwake = null;
}

function watchKeepAwake() {
  setInterval(async () => {
    if (closing || !showingDashboard) return;
    const wanted = await askKeepAwake();
    if (wanted && keepingAwake === null) keepingAwake = powerSaveBlocker.start('prevent-app-suspension');
    if (!wanted) releaseAwake();
  }, KEEP_AWAKE_EVERY_MS);
}

async function start() {
  if (starting || !window || window.isDestroyed()) return;
  starting = true;
  let refreshModels = false;
  try {
    const state = await checkDashboard();
    if (state === 'occupied') throw new Error('Port 8080 is in use. Close the other server, then retry.');
    await selectWindowsRuntime();
    // A stack left running by an older build of the app answers too, and is
    // replaced: compose recreates the container from the new image.
    if (state === 'absent' || await stackIsStale()) {
      if (!await startStack()) return;
    } else if (windows) {
      refreshModels = true;
    }
    if (window && !window.isDestroyed()) {
      await window.loadURL(dashboard);
      showingDashboard = true;
      // A dashboard that is already running is shown first: the disk check
      // and the model routing refresh (prepareWindowsModels) then run beside
      // it, and what they find goes to the log, never in front of the
      // workers the person came to see.
      const ready = refreshModels
        ? prepareWindowsModels().catch((error) => {
          logLine(`Model routing not refreshed: ${error.message}`);
          // Said, not hidden: a full disk matters before the next worker starts.
          if (window && !window.isDestroyed()) dialog.showMessageBox(window, { type: 'warning', title: 'Decurix', message: error.message }).catch(() => {});
        })
        : Promise.resolve();
      if (windows) ready.then(() => localModels.resume()).catch(error => console.warn(error.message));
      misses = 0;
      startMobileAccess();
      reportLanAddress();
      watchBackend();
    }
  } catch (error) {
    update(error.message, true, lastLogLine());
  } finally {
    starting = false;
  }
}

function isDashboardURL(url) {
  try { return new URL(url).origin === dashboardOrigin; } catch { return false; }
}

// fromDashboard: whether an IPC message comes from this window's main frame,
// showing the dashboard - not a remote page, a child frame or another window.
function fromDashboard(event) {
  return !!window && event.sender === window.webContents && !!event.senderFrame &&
    event.senderFrame === event.sender.mainFrame && isDashboardURL(event.senderFrame.url);
}

// The backend runs in Docker, so the app opens the host's native chooser and
// translates its results to the read-only mounts the backend can actually copy.
function hostFolderPath(folder) {
  if (windows) {
    const normalized = path.win32.normalize(folder);
    if (!/^C:\\/i.test(normalized)) throw new Error('Only folders on the C: drive are available to copy.');
    return '/mnt/c/' + normalized.slice(3).replace(/\\/g, '/');
  }
  const mounts = stackEnv();
  for (const [root, mount] of [[mounts.DECURIX_HOST_HOME, '/host-home'], [mounts.DECURIX_HOST_MNT_C, '/mnt/c']]) {
    const relative = path.relative(root, folder);
    if (relative !== '..' && !relative.startsWith('..' + path.sep) && !path.isAbsolute(relative)) {
      return mount + (relative ? '/' + relative.split(path.sep).join('/') : '');
    }
  }
  throw new Error('Choose a folder inside your home folder; other folders are not mounted in Decurix.');
}

let pickingHostFolders = false;
ipcMain.handle('pick-host-folders', async (event) => {
  if (!fromDashboard(event)) {
    throw new Error('Folder selection is only available from the desktop dashboard.');
  }
  if (pickingHostFolders) return { busy: true };
  pickingHostFolders = true;
  try {
    const result = await dialog.showOpenDialog(window, {
      title: 'Choose folders for the new desktop',
      properties: ['openDirectory', 'multiSelections'],
    });
    if (result.canceled) return { paths: [] };
    return { paths: result.filePaths.map((folder) => ({ path: hostFolderPath(folder), display_path: folder })) };
  } finally {
    pickingHostFolders = false;
  }
});

// Only the dashboard's main frame may change the caption button palette.
ipcMain.on('window-titlebar-theme', (event, palette) => {
  if (!windows || !window || window.isDestroyed() || !fromDashboard(event)) return;
  if (!palette || !/^#[0-9a-f]{6}$/i.test(palette.color) ||
      !/^#[0-9a-f]{6}$/i.test(palette.symbolColor)) return;
  window.setTitleBarOverlay({ color: palette.color, symbolColor: palette.symbolColor });
});

function configureNavigation(contents) {
  contents.setWindowOpenHandler(({ url }) => {
    if (isDashboardURL(url)) return { action: 'allow' };
    if (/^https?:\/\//i.test(url)) shell.openExternal(url);
    return { action: 'deny' };
  });
  contents.on('will-navigate', (event, url) => {
    if (isDashboardURL(url)) return;
    event.preventDefault();
    if (/^https?:\/\//i.test(url)) shell.openExternal(url);
  });
  contents.on('did-create-window', (child) => configureNavigation(child.webContents));
}

function openWindow() {
  // No File / Edit / View / Window bar: the app is its dashboard, and nothing in
  // that menu is the app's own. A Mac keeps its menu - it is where Quit and
  // copy and paste live there, at the top of the screen and not in the window.
  if (process.platform !== 'darwin' && Menu) Menu.setApplicationMenu(null);

  window = new BrowserWindow({
    width: 1280,
    height: 850,
    minWidth: 800,
    minHeight: 600,
    title: 'Decurix',
    // What shows before a page has painted: never a white flash.
    backgroundColor: startupPalette().color,
    // Keep native Windows controls (including Snap Layouts) inside the app header.
    ...(windows ? {
      titleBarStyle: 'hidden',
      titleBarOverlay: { ...startupPalette(), height: 52 },
    } : {}),
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
    },
  });

  configureNavigation(window.webContents);
  // A start screen whose renderer died or stopped answering is loaded again and given the latest status, so a start
  // never sits behind a frozen screen.
  window.webContents.on('render-process-gone', (_event, details) => {
    logLine(`The window's renderer ended (${(details && details.reason) || 'unknown'})`);
    reloadStartScreen();
  });
  window.on('unresponsive', () => {
    logLine('The window stopped answering');
    reloadStartScreen();
  });
  window.loadFile(path.join(__dirname, 'loading.html')).then(start);
  window.on('closed', () => { window = null; });
}

// Electron's own profile defaults to %APPDATA%\Decurix, which on Windows is
// the control plane's data dir: its files would sit beside decurix.db and go
// into every backup, which takes each file there. Before the lock, which
// lives in it.
app.setPath('userData', path.join(dataDir(), 'electron'));
app.setAppUserModelId('com.decurix.desktop');
if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on('second-instance', () => {
    if (window) {
      if (window.isMinimized()) window.restore();
      window.focus();
    }
  });
  app.whenReady().then(openWindow);
  app.on('window-all-closed', async () => {
    closing = true;
    releaseAwake();
    try { await stopMobileAccess(); }
    catch (error) { console.warn('Could not stop quick tunnel:', error.message); }
    app.quit();
  });
  ipcMain.on('retry-startup', () => { if (!starting && window) start(); });
  // The start screen says which status it has shown (update, failureNotShown).
  ipcMain.on('startup-status-shown', (event, seq) => {
    if (window && event && event.sender === window.webContents) shownSeq = Math.max(shownSeq, Number(seq) || 0);
  });
  // What would otherwise vanish goes to desktop.log, where a failed start is looked for.
  if (typeof process.on === 'function') {
    process.on('uncaughtException', (error) => { logLine(`Uncaught: ${error && error.stack ? error.stack : error}`); });
    process.on('unhandledRejection', (error) => { logLine(`Unhandled: ${error && error.stack ? error.stack : error}`); });
  }
}
