'use strict';

const { app, BrowserWindow, ipcMain, Menu, powerSaveBlocker, shell, dialog } = require('electron');
const { spawn } = require('node:child_process');
const dgram = require('node:dgram');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { setupMobileAccess } = require('./mobile-access');

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
//   - Windows uses a WSL distro of the app's own ("Decurix", Alpine), into
//     which the app installs Docker Engine and where it runs the daemon
//     itself (resources/wsl). No Docker Desktop. When Windows has no WSL yet
//     the app turns it on (one administrator prompt, then a restart).
const windows = process.platform === 'win32';
const DISTRO = 'Decurix';
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

function tunnelCommand(port) {
  return { file: cloudflared(), args: ['tunnel', '--protocol', 'http2', '--url', `http://127.0.0.1:${port}`] };
}

function startMobileAccess() {
  if (mobileAccessPromise || closing) return;
  // No cloudflared, no tunnel: the dashboard works without one.
  if (!cloudflared()) return;
  mobileAccessPromise = setupMobileAccess(dataDir(), tunnelCommand).then(async (access) => {
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

function update(message, failed = false, detail = '') {
  if (window && !window.isDestroyed()) {
    window.webContents.send('startup-status', { message, failed, detail });
  }
}

function logFile() {
  return path.join(dataDir(), 'desktop.log');
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

function checkDashboard() {
  return new Promise((resolve) => {
    const req = http.get(`${dashboard}/favicon.svg`, { timeout: 2000 }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => { body = (body + chunk).slice(0, 20000); });
      res.on('end', () => resolve(res.statusCode === 200 && body.includes(marker) ? 'ready' : 'occupied'));
      res.on('error', () => resolve('absent'));
    });
    req.on('timeout', () => req.destroy());
    req.on('error', () => resolve('absent'));
  });
}

async function waitForDashboard(stopped) {
  for (let attempt = 0; attempt < 120 && !stopped(); attempt += 1) {
    const state = await checkDashboard();
    if (state === 'ready') return;
    if (state === 'occupied') throw new Error('Port 8080 is in use. Close the other server, then retry.');
    await new Promise((resolve) => setTimeout(resolve, 1000));
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
// drive; Linux gives its root where WSL would give C:).
function stackEnv() {
  // The project name a checkout's own `docker compose up` uses, so the app
  // takes over a control plane started that way instead of colliding with
  // its container's name - and keeps its records (the decurix-data volume).
  const env = { ...process.env, PATH: searchPath().join(path.delimiter), COMPOSE_PROJECT_NAME: 'decurix', DECURIX_HOST_HOME: os.homedir() };
  if (process.platform === 'darwin') {
    env.DECURIX_HOST_MNT_C = path.join(dataDir(), 'no-windows-drive');
    fs.mkdirSync(env.DECURIX_HOST_MNT_C, { recursive: true });
  } else {
    env.DECURIX_HOST_MNT_C = fs.existsSync('/mnt/c') ? '/mnt/c' : '/';
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
    const child = spawn(file, args, { windowsHide: true, ...options, stdio: ['ignore', 'pipe', 'pipe'] });
    let rest = '';
    const take = (chunk) => {
      if (log !== null) { try { fs.writeSync(log, chunk); } catch { /* decoration */ } }
      if (!onLine) return;
      const lines = (rest + chunk.toString()).split(/[\r\n]+/);
      rest = lines.pop();
      for (const line of lines) if (line.trim()) onLine(line.trim().slice(0, 240));
    };
    child.stdout.on('data', take);
    child.stderr.on('data', take);
    const done = (result) => {
      if (log !== null) { try { fs.closeSync(log); } catch { /* already closed */ } log = null; }
      resolve(result);
    };
    child.once('error', (error) => done(error.code === 'ENOENT' ? 'missing' : 1));
    child.once('exit', (code) => done(code));
  });
}

// wsl runs wsl.exe. WSL_UTF8 makes it print UTF-8 rather than UTF-16.
function wsl(args, onLine) {
  return run('wsl.exe', args, onLine, { env: { ...process.env, WSL_UTF8: '1' } });
}

// inDistro runs a program as root inside the app's distro.
function inDistro(args, onLine, cwd = '/') {
  return wsl(['-d', DISTRO, '-u', 'root', '--cd', cwd, '-e', ...args], onLine);
}

// docker runs one docker command in the stack's folder - on Windows inside
// the app's distro, given what stackEnv gives elsewhere as plain variables.
function docker(args, onLine) {
  if (windows) {
    return inDistro(['env', 'COMPOSE_PROJECT_NAME=decurix', 'DECURIX_HOST_HOME=/home', 'DECURIX_HOST_MNT_C=/mnt/c', 'docker', ...args], onLine, DISTRO_STACK);
  }
  return run(findProgram('docker') || 'docker', args, onLine, { cwd: stackDir(), env: stackEnv() });
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
    // The control plane runs its own model server, in a container, and
    // chooses the model for this computer. OLLAMA_URL names another.
    fs.writeFileSync(config, 'OLLAMA_URL=http://decurix-ollama:11434\nOLLAMA_MODEL=auto\n', 'utf8');
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
    const code = await inDistro(['cat', `${DISTRO_STACK}/VERSION`], (line) => { staged = line; });
    return code !== 0 || staged.trim() !== shipped;
  }
  try { return fs.readFileSync(path.join(stackDir(), 'VERSION'), 'utf8').trim() !== shipped; } catch { return false; }
}

// dockerReady waits for a Docker that answers. On a Mac, Docker Desktop is
// started when it is installed and not running; on Windows the app's own
// distro is made ready and its daemon started.
async function dockerReady() {
  if (windows) { await linuxReady(); return; }
  const mac = process.platform === 'darwin';
  const needs = mac ? 'Docker Desktop' : 'Docker';
  const ask = () => docker(['version', '--format', '{{.Server.Version}}']);
  let answer = await ask();
  if (answer === 'missing') {
    throw new Error(`Decurix needs ${needs}: its workers are Linux desktops that run in it. Install ${needs}, start it, then retry.`);
  }
  if (answer !== 0 && mac) {
    update('Starting Docker Desktop…');
    spawn('open', ['-ga', 'Docker'], { stdio: 'ignore' }).once('error', () => {});
    for (let attempt = 0; attempt < 60 && answer !== 0 && !closing; attempt += 1) {
      await new Promise((resolve) => setTimeout(resolve, 2000));
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
  return await wsl(['--version']) === 0;
}

// installWsl turns WSL on: Windows asks the person for an administrator's yes
// once, and a computer that never had it then needs a restart.
async function installWsl() {
  const message = 'Turning on Windows Subsystem for Linux. Windows will ask you to allow it.';
  update(message);
  const command = "Start-Process -FilePath wsl.exe -ArgumentList '--install','--no-distribution' -Verb RunAs -Wait";
  const code = await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', command], (line) => update(message, false, line));
  if (code !== 0) throw new Error('Windows Subsystem for Linux could not be turned on (was the administrator prompt declined?). Retry to try again.');
  if (!await wslInstalled()) {
    throw new Error('Windows Subsystem for Linux is installed. Restart Windows to finish, then open Decurix again.');
  }
}

async function distroExists() {
  const names = [];
  const code = await wsl(['--list', '--quiet'], (line) => names.push(line.replace(/\0/g, '').trim()));
  // With no distro at all WSL answers with an error.
  return code === 0 && names.includes(DISTRO);
}

// stopEarlierWindowsApp ends what an earlier version of the app ran on Windows
// itself, which this one replaces: its control plane from the install folder
// (holding port 8080) and the model server it fetched for itself.
async function stopEarlierWindowsApp() {
  const quote = (text) => `'${text.replace(/'/g, "''")}'`;
  const folder = quote(path.join(process.resourcesPath, 'decurix.exe'));
  const store = quote(path.join(path.dirname(distroDir()), 'ollama') + path.sep);
  const script = `$cp = ${folder}; $ol = ${store}; Get-CimInstance Win32_Process | `
    + "? { $_.ExecutablePath -and ($_.ExecutablePath -ieq $cp -or $_.ExecutablePath.StartsWith($ol, 'CurrentCultureIgnoreCase')) } | "
    + '% { Stop-Process -Id $_.ProcessId -Force }';
  await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script]);
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
  if (!await wslInstalled()) await installWsl();

  if (!await distroExists()) {
    const message = 'Setting up Linux for Decurix…';
    update(message);
    fs.mkdirSync(distroDir(), { recursive: true });
    const code = await wsl(['--import', DISTRO, distroDir(), resource(path.join('wsl', 'alpine-minirootfs.tar.gz')), '--version', '2'], (line) => update(message, false, line));
    if (code !== 0) throw new Error(`Could not set up Linux for Decurix. Check ${logFile()}`);
  }

  const message = 'Preparing Linux. The first time, this installs Docker.';
  update(message);
  const setup = await inDistro(['sh', linuxPath(resource(path.join('wsl', 'setup.sh'))), linuxPath(resource('stack')), linuxPath(ensureSettings())], (line) => update(message, false, line));
  if (setup !== 0) throw new Error(`Could not prepare Linux for Decurix. Check ${logFile()}`);

  const ask = () => inDistro(['docker', 'version', '--format', '{{.Server.Version}}']);
  if (await ask() === 0) return;
  update('Starting Docker…');
  if (await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', startHiddenScript(DAEMON_COMMAND)]) !== 0) {
    throw new Error(`Docker could not be started in Decurix's Linux. Check ${logFile()}`);
  }
  for (let attempt = 0; attempt < 60 && !closing; attempt += 1) {
    if (await ask() === 0) return;
    await new Promise((resolve) => setTimeout(resolve, 1000));
  }
  throw new Error(`Docker did not start in Decurix's Linux. Its log is /var/log/dockerd.log there (wsl -d ${DISTRO}).`);
}

// startStack builds and starts the control plane in Docker, and the worker
// images it does not have yet. Every step is safe to repeat: a second start
// finds everything built and takes seconds.
async function startStack() {
  if (windows) ensureSettings(); else stageStack();
  await dockerReady();

  const step = async (message, args, failure) => {
    update(message);
    const code = await docker(args, (line) => update(message, false, line));
    if (code !== 0) throw new Error(`${failure} Check ${logFile()}`);
  };
  await step('Preparing Decurix…', ['compose', 'build'], 'Could not build Decurix in Docker.');
  // The desktop images are built by the control plane itself, from the build
  // context inside it; it skips the ones that exist. Only the desktop is
  // waited for: the tools image (Go, Node, Rust, the coding CLIs) is a long
  // run of downloads a desktop does not need to start, and is built after the
  // dashboard is up (buildToolsLater).
  await step('Building the worker desktop. The first time, this downloads and installs a whole Linux desktop (about ten minutes).',
    ['compose', 'run', '--rm', '--no-deps', '-T', 'desktop-manager', './decurix', '--build-images', '-images', 'desktop'], 'Could not build the worker desktop.');
  await step('Starting Decurix…', ['compose', 'up', '-d', '--no-build'], 'Could not start Decurix in Docker.');
  await waitForDashboard(() => closing);
  buildToolsLater();
  // Under WSL's default networking only this computer reaches the distro's
  // ports, so there is no address on the network to give a phone there; the
  // tunnel is how a phone gets in.
  if (!windows) reportLanAddress();
  return true;
}

// buildToolsLater builds the tools image, if it is missing, without holding
// anything up; its output goes to desktop.log. A desktop made before it is
// done starts without the tools, which the shared tools volume gets once the
// image exists.
function buildToolsLater() {
  docker(['compose', 'run', '--rm', '--no-deps', '-T', 'desktop-manager', './decurix', '--build-images', '-images', 'tools'])
    .then((code) => { if (code !== 0) console.warn('Building the tools image failed; see', logFile()); });
}

// The gated listener is published on the network by compose, so a phone on
// the same Wi-Fi can use it without the tunnel: the Mobile menu is told this
// computer's address, found by asking which one a packet to the internet
// would leave from (nothing is sent). scripts/*/build.sh do the same.
function reportLanAddress() {
  const socket = dgram.createSocket('udp4');
  const give = () => { try { socket.close(); } catch { /* closed */ } };
  socket.once('error', give);
  socket.connect(80, '1.1.1.1', () => {
    let address = '';
    try { address = socket.address().address; } catch { /* no network */ }
    give();
    if (!address || address.startsWith('127.')) return;
    let port = '8090';
    try { port = fs.readFileSync(path.join(dataDir(), '.env'), 'utf8').match(/^\s*DECURIX_LAN_PORT\s*=\s*['"]?(\d+)/m)?.[1] || port; } catch { /* the default */ }
    const body = JSON.stringify({ lan_url: `http://${address}:${port}` });
    const req = http.request(`${dashboard}/api/tunnel`, { method: 'POST', timeout: 5000, headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) } }, (res) => res.resume());
    req.on('timeout', () => req.destroy());
    req.on('error', () => {});
    req.end(body);
  });
}

// A control plane in a container cannot keep the computer awake, and nothing
// runs on a sleeping one - no schedule, no email trigger. So while the app is
// open it asks whether anything needs the computer awake (the question
// scripts/*/lib/keep-awake.sh ask) and holds it awake only while something does.
const KEEP_AWAKE_EVERY_MS = 30000;
let keepingAwake = null;

function askKeepAwake() {
  return new Promise((resolve) => {
    const req = http.get(`${dashboard}/api/keep-awake?holder=script`, { timeout: 5000 }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => { body = (body + chunk).slice(0, 8192); });
      res.on('end', () => { try { const state = JSON.parse(body); resolve(state.enabled !== false && state.wanted === true); } catch { resolve(false); } });
      res.on('error', () => resolve(false));
    });
    req.on('timeout', () => req.destroy());
    req.on('error', () => resolve(false));
  });
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
  try {
    const state = await checkDashboard();
    if (state === 'occupied') throw new Error('Port 8080 is in use. Close the other server, then retry.');
    // A stack left running by an older build of the app answers too, and is
    // replaced: compose recreates the container from the new image.
    if (state === 'absent' || await stackIsStale()) {
      if (!await startStack()) return;
    }
    if (window && !window.isDestroyed()) {
      await window.loadURL(dashboard);
      showingDashboard = true;
      misses = 0;
      startMobileAccess();
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
  // Reject remote pages, child frames and windows other than the dashboard.
  if (!window || event.sender !== window.webContents || !event.senderFrame ||
      event.senderFrame !== event.sender.mainFrame || !isDashboardURL(event.senderFrame.url)) {
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
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
    },
  });

  configureNavigation(window.webContents);
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
}
