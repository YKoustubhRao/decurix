'use strict';

const { app, BrowserWindow, dialog, ipcMain, Menu, powerSaveBlocker, shell } = require('electron');
const { spawn } = require('node:child_process');
const dgram = require('node:dgram');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { setupMobileAccess } = require('./mobile-access');

// The app is the control plane and a window on its dashboard.
//
// On Windows the control plane is resources\decurix.exe, run as it is. Nothing
// else has to be installed: its workers are private desktops of this computer,
// run by what ships in resources\runtime, and it fetches its own local model
// the first time one is needed.
//
// On a Mac or Linux a worker is a Linux desktop in a container, and a control
// plane outside Docker cannot reach one (Docker Desktop does not route a
// container's address to the host). So there the app carries the Linux control
// plane with a Dockerfile and a compose file (resources/stack) and runs that -
// what scripts/mac/build.sh does from a checkout - and Docker is the one thing
// the computer must already have.
const native = process.platform === 'win32';
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
  return native ? resource('cloudflared.exe') : findProgram('cloudflared');
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

// The control plane. Detached and unref'd: it keeps serving workers and
// schedules after the window is closed.
function runBackend() {
  return new Promise((resolve, reject) => {
    const log = fs.openSync(logFile(), 'a');
    const child = spawn(resource('decurix.exe'), [], {
      cwd: dataDir(),
      windowsHide: true,
      detached: true,
      stdio: ['ignore', log, log],
    });
    fs.closeSync(log);
    child.once('error', reject);
    child.once('exit', (code) => resolve(code));
    child.unref();
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
  if (!native) watchKeepAwake();
  setInterval(async () => {
    if (closing || starting || !showingDashboard || !window || window.isDestroyed()) return;
    if (await checkDashboard() === 'ready') { misses = 0; return; }
    misses += 1;
    if (misses < LOST_AFTER_MISSES) return;
    misses = 0;
    try { await backendLost(); } catch (error) { console.error('Could not restart Decurix:', error); }
  }, WATCH_EVERY_MS);
}

// startNative runs the Windows control plane. False when it stopped before
// its dashboard answered (the window already says so).
async function startNative() {
  const example = path.join(dataDir(), '.env.example');
  if (!fs.existsSync(example)) fs.copyFileSync(resource('.env.example'), example);
  const config = path.join(dataDir(), '.env');
  if (!fs.existsSync(config)) {
    // The model is chosen for this computer; the control plane runs its
    // own model runtime unless OLLAMA_URL names another.
    fs.writeFileSync(config, 'OLLAMA_MODEL=auto\n', 'utf8');
  }

  update('Starting Decurix…');
  let exited = false;
  const serverExit = runBackend();
  const showStopped = async (reason) => {
    exited = true;
    // Once the dashboard is showing, losing the control plane is the
    // watch's to deal with (it starts it again); this is for one that
    // never came up.
    if (showingDashboard) return;
    if (!window || window.isDestroyed()) return;
    const detail = lastLogLine();
    await stopMobileAccess();
    await window.loadFile(path.join(__dirname, 'loading.html'));
    update(`${reason} Check ${logFile()}`, true, detail);
  };
  const stopped = serverExit.then(
    (code) => showStopped(`Decurix exited with code ${code}.`),
    (error) => showStopped(`Decurix could not start: ${error.message}.`),
  ).catch((error) => console.error('Could not show backend status:', error));
  await Promise.race([waitForDashboard(() => exited), stopped]);
  return !exited;
}

// ---- Mac and Linux: the control plane in Docker ----

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

// docker runs one docker command in the stack's folder, its output going to
// desktop.log, and resolves with its exit code - or 'missing' when there is no
// docker program at all. onLine is given each line it prints.
function docker(args, onLine) {
  return new Promise((resolve) => {
    let log = null;
    try { log = fs.openSync(logFile(), 'a'); } catch { /* the log is decoration */ }
    const child = spawn(findProgram('docker') || 'docker', args, { cwd: stackDir(), env: stackEnv(), stdio: ['ignore', 'pipe', 'pipe'] });
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

// stageStack puts what the app ships into the data folder, where docker can
// build from it and the person's own .env sits beside it. Copied again only
// when the app is another build than the one that staged it.
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

  const example = path.join(dataDir(), '.env.example');
  if (!fs.existsSync(example)) fs.copyFileSync(resource('.env.example'), example);
  // The person's settings are the .env in the data folder, as on Windows;
  // the image and compose both read the copy made here at each start.
  const config = path.join(dataDir(), '.env');
  if (!fs.existsSync(config)) {
    // The control plane runs its own model server, in a container, and
    // chooses the model for this computer. OLLAMA_URL names another.
    fs.writeFileSync(config, 'OLLAMA_URL=http://decurix-ollama:11434\nOLLAMA_MODEL=auto\n', 'utf8');
  }
  fs.copyFileSync(config, path.join(stackDir(), '.env'));
}

// stackIsStale reports whether what is staged (and so what is running) came
// from another build of the app than this one.
function stackIsStale() {
  try {
    return fs.readFileSync(path.join(stackDir(), 'VERSION'), 'utf8') !== fs.readFileSync(path.join(resource('stack'), 'VERSION'), 'utf8');
  } catch { return false; }
}

// dockerReady waits for a Docker that answers. On a Mac, Docker Desktop is
// started when it is installed and not running.
async function dockerReady() {
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

// startStack builds and starts the control plane in Docker, and the worker
// images it does not have yet. Every step is safe to repeat: a second start
// finds everything built and takes seconds.
async function startStack() {
  stageStack();
  await dockerReady();

  const step = async (message, args, failure) => {
    update(message);
    const code = await docker(args, (line) => update(message, false, line));
    if (code !== 0) throw new Error(`${failure} Check ${logFile()}`);
  };
  await step('Preparing Decurix…', ['compose', 'build'], 'Could not build Decurix in Docker.');
  // The desktop images are built by the control plane itself, from the build
  // context inside it; it skips the ones that exist.
  await step('Building the worker desktop. The first time, this downloads and installs a whole Linux desktop and can take a long while.',
    ['compose', 'run', '--rm', '--no-deps', '-T', 'desktop-manager', './decurix', '--build-images'], 'Could not build the worker desktop.');
  await step('Starting Decurix…', ['compose', 'up', '-d', '--no-build'], 'Could not start Decurix in Docker.');
  await waitForDashboard(() => closing);
  reportLanAddress();
  return true;
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
    if (state === 'absent' || (!native && stackIsStale())) {
      fs.mkdirSync(dataDir(), { recursive: true });
      if (!(native ? await startNative() : await startStack())) return;
    }
    if (window && !window.isDestroyed()) {
      await window.loadURL(dashboard);
      showingDashboard = true;
      misses = 0;
      startMobileAccess();
      watchBackend();
    }
  } catch (error) {
    update(error.message, true, native ? '' : lastLogLine());
  } finally {
    starting = false;
  }
}

function isDashboardURL(url) {
  try { return new URL(url).origin === dashboardOrigin; } catch { return false; }
}

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

  if (ipcMain.handle) {
    ipcMain.handle('open-explorer', async (_event, targetPath) => {
      try {
        if (targetPath && fs.existsSync(targetPath)) {
          shell.showItemInFolder(targetPath);
          return { ok: true };
        }
        shell.openPath(os.homedir());
        return { ok: true };
      } catch (err) {
        return { ok: false, error: err.message };
      }
    });

    ipcMain.handle('pick-paths', async (_event, options = {}) => {
      try {
        if (!dialog || !dialog.showOpenDialog) return { canceled: true, paths: [] };
        const isFolder = options.type === 'folder';
        const properties = isFolder ? ['openDirectory', 'multiSelections'] : ['openFile', 'multiSelections'];
        const res = await dialog.showOpenDialog(window, {
          title: isFolder ? 'Select folder(s) to copy into VM' : 'Select file(s) to copy into VM',
          properties: properties,
        });
        if (res.canceled) return { canceled: true, paths: [] };
        return { canceled: false, paths: res.filePaths };
      } catch (err) {
        return { canceled: true, paths: [], error: err.message };
      }
    });
  }
}
