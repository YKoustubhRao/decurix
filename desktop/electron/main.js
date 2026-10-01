'use strict';

const { app, BrowserWindow, ipcMain, shell } = require('electron');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { setupMobileAccess } = require('./mobile-access');

const dashboard = 'http://127.0.0.1:8080';
const marker = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" role="img" aria-label="Decurix"';
const dashboardOrigin = new URL(dashboard).origin;
let window;
let starting = false;
let closing = false;
let mobileAccess = null;
let mobileAccessPromise = null;

function resource(name) {
  if (app.isPackaged) return path.join(process.resourcesPath, name);
  if (name === '.env.example') return path.join(__dirname, '..', '..', '.env.example');
  return path.join(__dirname, 'build', name);
}

function dataDir() {
  return path.join(app.getPath('appData'), 'decurix');
}

function startMobileAccess() {
  if (mobileAccessPromise || closing) return;
  mobileAccessPromise = setupMobileAccess(dataDir()).then(async (access) => {
    if (closing) await access?.stop();
    else mobileAccess = access;
  }).catch((error) => console.warn('Mobile access setup failed:', error.message));
}

async function stopMobileAccess() {
  await mobileAccessPromise;
  mobileAccessPromise = null;
  const access = mobileAccess;
  mobileAccess = null;
  await access?.stop();
}

function update(message, failed = false) {
  if (window && !window.isDestroyed()) {
    window.webContents.send('startup-status', { message, failed });
  }
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

function runBackend(args) {
  return new Promise((resolve, reject) => {
    const log = fs.openSync(path.join(dataDir(), 'desktop.log'), 'a');
    const child = spawn(resource('decurix.exe'), args, {
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

async function waitForDashboard() {
  for (let attempt = 0; attempt < 120; attempt += 1) {
    const state = await checkDashboard();
    if (state === 'ready') return;
    if (state === 'occupied') throw new Error('Port 8080 is in use. Close the other server, then retry.');
    await new Promise((resolve) => setTimeout(resolve, 1000));
  }
  throw new Error(`Decurix did not start. Check ${path.join(dataDir(), 'desktop.log')}`);
}

async function start() {
  if (starting || !window || window.isDestroyed()) return;
  starting = true;
  try {
    const state = await checkDashboard();
    if (state === 'occupied') throw new Error('Port 8080 is in use. Close the other server, then retry.');
    if (state === 'absent') {
      fs.mkdirSync(dataDir(), { recursive: true });
      const example = path.join(dataDir(), '.env.example');
      if (!fs.existsSync(example)) fs.copyFileSync(resource('.env.example'), example);
      const config = path.join(dataDir(), '.env');
      if (!fs.existsSync(config)) {
        fs.writeFileSync(config, 'OLLAMA_URL=http://decurix-ollama:11434\nOLLAMA_MODEL=auto\n', 'utf8');
      }
      update('Checking Docker Desktop and building any missing worker images. The first run can take a while.');
      const code = await runBackend(['--build-images']);
      if (code !== 0) {
        throw new Error(`Worker image build failed. Start Docker Desktop and check ${path.join(dataDir(), 'desktop.log')}`);
      }
      update('Starting Decurix…');
      const serverExit = runBackend([]);
      const showStopped = async (reason) => {
        if (!window || window.isDestroyed()) return;
        await stopMobileAccess();
        await window.loadFile(path.join(__dirname, 'loading.html'));
        update(`${reason} Check ${path.join(dataDir(), 'desktop.log')}`, true);
      };
      serverExit.then(
        (code) => showStopped(`Decurix exited with code ${code}.`),
        (error) => showStopped(`Decurix could not start: ${error.message}.`),
      ).catch((error) => console.error('Could not show backend status:', error));
      await Promise.race([waitForDashboard(), serverExit.then((code) => {
        throw new Error(`Decurix exited with code ${code}. Check ${path.join(dataDir(), 'desktop.log')}`);
      })]);
    }
    if (window && !window.isDestroyed()) {
      await window.loadURL(dashboard);
      startMobileAccess();
    }
  } catch (error) {
    update(error.message, true);
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
    try { await stopMobileAccess(); }
    catch (error) { console.warn('Could not stop quick tunnel:', error.message); }
    app.quit();
  });
  ipcMain.on('retry-startup', () => { if (!starting && window) start(); });
}
