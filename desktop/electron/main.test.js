'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const EventEmitter = require('node:events');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
const favicon = fs.readFileSync(path.join(__dirname, '..', '..', 'web', 'favicon.svg'), 'utf8');

async function boot(initialState) {
  const windows = [];
  const spawns = [];
  const copied = [];
  const written = [];
  const external = [];
  let state = initialState;

  class FakeWindow extends EventEmitter {
    constructor(options) {
      super();
      this.options = options;
      this.webContents = new EventEmitter();
      this.webContents.setWindowOpenHandler = (handler) => { this.popup = handler; };
      this.webContents.send = (channel, value) => { this.lastStatus = { channel, value }; };
      this.loadFile = async () => {};
      this.loadURL = async (url) => { this.url = url; };
      this.isDestroyed = () => false;
      windows.push(this);
    }
  }

  const app = new EventEmitter();
  app.isPackaged = true;
  app.getPath = () => '/profile/AppData';
  app.setAppUserModelId = () => {};
  app.requestSingleInstanceLock = () => true;
  app.whenReady = () => Promise.resolve();
  app.quit = () => {};

  const http = {
    get(_url, _options, callback) {
      const request = new EventEmitter();
      request.destroy = () => request.emit('error', new Error('timeout'));
      queueMicrotask(() => {
        if (state === 'absent') {
          request.emit('error', new Error('connection refused'));
          return;
        }
        const response = new EventEmitter();
        response.statusCode = 200;
        response.setEncoding = () => {};
        callback(response);
        response.emit('data', state === 'ready' ? favicon : '<svg>another app</svg>');
        response.emit('end');
      });
      return request;
    },
  };

  const childProcess = {
    spawn(file, args, options) {
      spawns.push({ file, args, options });
      const child = new EventEmitter();
      child.unref = () => {};
      if (args[0] === '--build-images') queueMicrotask(() => child.emit('exit', 0));
      else state = 'ready';
      return child;
    },
  };

  const fileSystem = {
    mkdirSync() {},
    existsSync() { return false; },
    copyFileSync(from, to) { copied.push({ from, to }); },
    writeFileSync(file, content) { written.push({ file, content }); },
    openSync() { return 1; },
    closeSync() {},
  };

  const modules = {
    electron: { app, BrowserWindow: FakeWindow, ipcMain: new EventEmitter(), shell: { openExternal: (url) => external.push(url) } },
    'node:child_process': childProcess,
    'node:fs': fileSystem,
    'node:http': http,
    'node:path': path,
  };
  vm.runInNewContext(source, {
    require: (name) => modules[name],
    process: { resourcesPath: '/installed/resources' },
    URL,
    setTimeout: (callback) => queueMicrotask(callback),
    __dirname,
  }, { filename: 'main.js' });
  for (let i = 0; i < 12; i += 1) await new Promise((resolve) => setImmediate(resolve));
  return { window: windows[0], spawns, copied, written, external };
}

test('reuses a running dashboard without launching another backend', async () => {
  const result = await boot('ready');
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.equal(result.spawns.length, 0);
});

test('builds images and starts the packaged Windows backend when absent', async () => {
  const result = await boot('absent');
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.deepEqual(result.spawns.map((call) => Array.from(call.args)), [['--build-images'], []]);
  for (const call of result.spawns) {
    assert.equal(call.file, '/installed/resources/decurix.exe');
    assert.equal(call.options.cwd, '/profile/AppData/decurix');
    assert.equal(call.options.windowsHide, true);
  }
  assert.equal(result.copied[0].from, '/installed/resources/.env.example');
  assert.equal(result.written[0].file, '/profile/AppData/decurix/.env');
  assert.match(result.written[0].content, /OLLAMA_URL=http:\/\/decurix-ollama:11434/);
});

test('refuses another server on port 8080', async () => {
  const result = await boot('occupied');
  assert.equal(result.spawns.length, 0);
  assert.equal(result.window.lastStatus.value.failed, true);
});

test('keeps local popups in Electron and sends external links to the browser', async () => {
  const result = await boot('ready');
  assert.equal(result.window.popup({ url: 'http://127.0.0.1:8080/view/abc' }).action, 'allow');
  assert.equal(result.window.popup({ url: 'https://example.com/' }).action, 'deny');
  assert.deepEqual(result.external, ['https://example.com/']);
});
