'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const EventEmitter = require('node:events');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
const gatedSource = fs.readFileSync(path.join(__dirname, 'gated-port.js'), 'utf8');

// loadGatedPort runs gated-port.js against the harness's fake fs and http, as
// main.js would see it.
function loadGatedPort(fileSystem, http) {
  const module = { exports: {} };
  vm.runInNewContext(gatedSource, {
    require: (name) => ({ 'node:fs': fileSystem, 'node:http': http, 'node:path': path })[name],
    module, exports: module.exports, process: { env: {} },
  }, { filename: 'gated-port.js' });
  return module.exports;
}
const favicon = fs.readFileSync(path.join(__dirname, '..', '..', 'web', 'favicon.svg'), 'utf8');
const log = Buffer.from('2026/10/01 18:05:17 Decurix dev starting\r\n2026/10/01 18:05:17 listen tcp 127.0.0.1:8080: bind: address in use\r\n\r\n');

test('native folder chooser maps Windows selections to the backend drive mount', async () => {
  const result = await boot('ready', { pickedFolders: ['C:\\Users\\ana\\Documents\\Project files', 'c:\\Code\\demo'] });
  const selected = await result.pickFolders();
  assert.deepEqual(Array.from(selected.paths, (item) => item.path), ['/mnt/c/Users/ana/Documents/Project files', '/mnt/c/Code/demo']);
  assert.equal(selected.paths[0].display_path, 'C:\\Users\\ana\\Documents\\Project files');
  assert.equal(result.pickerCalls[0].owner, result.window);
  assert.deepEqual(Array.from(result.pickerCalls[0].options.properties), ['openDirectory', 'multiSelections']);
});

test('native folder chooser handles cancel and rejects unmounted Windows drives', async () => {
  const canceled = await boot('ready', { canceledPicker: true });
  assert.equal((await canceled.pickFolders()).paths.length, 0);
  const otherDrive = await boot('ready', { pickedFolders: ['D:\\Projects'] });
  await assert.rejects(otherDrive.pickFolders(), /Only folders on the C: drive/);
});

test('native folder chooser only accepts the dashboard main frame', async () => {
  const result = await boot('ready');
  await assert.rejects(result.pickFolders('https://example.com/'), /only available from the desktop dashboard/);
  await assert.rejects(result.pickFolders('http://127.0.0.1:8080/', true), /only available from the desktop dashboard/);
  assert.equal(result.pickerCalls.length, 0);
});

test('native folder chooser maps home folders on Mac and Linux, and never mounts the Linux root', async () => {
  const homeFolder = path.join('/Users/ana', 'Documents', 'demo');
  for (const platform of ['darwin', 'linux']) {
    const result = await boot('ready', { platform, pickedFolders: [homeFolder] });
    assert.equal((await result.pickFolders()).paths[0].path, '/host-home/Documents/demo');
  }
  const linux = await boot('ready', { platform: 'linux', pickedFolders: [path.resolve('/opt/demo')] });
  await assert.rejects(linux.pickFolders(), /inside your home folder/);
  // Linux under WSL gives the Windows drive, as scripts/linux/build.sh does.
  const wslLinux = await boot('ready', { platform: 'linux', files: ['/mnt/c'], pickedFolders: [path.resolve('/mnt/c/Users/ana/demo')] });
  assert.equal((await wslLinux.pickFolders()).paths[0].path, '/mnt/c/Users/ana/demo');
  const mac = await boot('ready', { platform: 'darwin', pickedFolders: [path.resolve('/Users/another/demo')] });
  await assert.rejects(mac.pickFolders(), /inside your home folder/);
});

// path.join, not literals: on Windows (where this is built) it joins with "\".
const data = path.join('/profile/AppData', 'decurix');

// backendStarts: whether a control plane that is started comes up (answers on
// 8080) or never does.
// stopFails: stopping the phone link throws, as it does when the control plane
// it reports to is gone.
// platform: 'win32' runs the stack in the app's own WSL distro; 'darwin' and
// 'linux' in the computer's Docker. dockerRun(args) is each docker command's
// exit code ('missing': no docker program), files the paths that exist besides
// what the app writes, and staged the VERSION of a stack an earlier run left
// behind.
// wsl (Windows): installed - WSL answers --version (installedAfterAsking: it
// does once the person allowed turning it on); distro - the app's distro is
// imported; daemon - its Docker answers before dockerd.sh is started. staged
// is read from inside the distro there.
async function boot(initialState, {
  backendStarts = true, stopFails = false, platform = 'win32', dockerRun = () => 0, files = [], staged = platform === 'win32' ? 'build-2' : null,
  wsl = {}, pickedFolders = [], canceledPicker = false, savedRuntime, hosting = false, holdOllama = false, ollamaFails = false,
  release = '10.0.26200', arch = 'x64', freeGB = 100, restartAsked = null, restartedSince = true,
  lanAddress = null, lanForwards = true, settings = '', publicLink = null, adopt = true,
} = {}) {
  const machine = { installed: true, installedAfterAsking: false, distro: true, daemon: true, setupFails: false, existing: {}, ...wsl };
  const intervals = [];
  const windows = [];
  const spawns = [];
  const children = [];
  const copied = [];
  const written = [];
  const external = [];
  const mobileCalls = [];
  const pickerCalls = [];
  const questions = [];
  const ipcMain = new EventEmitter();
  const ipcHandlers = new Map();
  ipcMain.handle = (name, handler) => ipcHandlers.set(name, handler);
  let tunnel = null;
  let quits = 0;
  const blockers = [];
  const home = '/Users/ana';
  const stack = path.join(data, 'stack');
  const present = new Set(files);
  let state = initialState;

  class FakeWindow extends EventEmitter {
    constructor(options) {
      super();
      this.options = options;
      this.webContents = new EventEmitter();
      this.webContents.setWindowOpenHandler = (handler) => { this.popup = handler; };
      this.webContents.send = (channel, value) => { this.lastStatus = { channel, value }; };
      this.loadFile = async () => { this.loadFileCount = (this.loadFileCount || 0) + 1; };
      this.loadURL = async (url) => { this.url = url; };
      this.isDestroyed = () => false;
      this.setTitleBarOverlay = (palette) => { this.captionPalette = palette; };
      windows.push(this);
    }
  }

  const app = new EventEmitter();
  app.isPackaged = true;
  app.getPath = () => '/profile/AppData';
  app.paths = {};
  app.setPath = (name, value) => { app.paths[name] = value; };
  app.setAppUserModelId = () => {};
  app.requestSingleInstanceLock = () => true;
  app.whenReady = () => Promise.resolve();
  app.quit = () => { quits += 1; };

  const posts = [];
  const lanCalls = [];
  const http = {
    request(url, _options, callback) {
      const request = new EventEmitter();
      request.destroy = () => {};
      request.end = (body) => {
        posts.push({ url, body });
        queueMicrotask(() => { const response = new EventEmitter(); response.resume = () => {}; callback(response); });
      };
      return request;
    },
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

  // What one docker command does: says a line and ends; `up` is what makes
  // the dashboard answer.
  const runDocker = (args, child) => {
    const result = dockerRun(args);
    setImmediate(() => {
      if (result === 'missing') { child.emit('error', Object.assign(new Error('spawn docker ENOENT'), { code: 'ENOENT' })); return; }
      child.stdout.emit('data', Buffer.from(`docker ${args[0]} ${args[1]}\n`));
      if (result === 0 && args.includes('up') && backendStarts) state = 'ready';
      child.emit('exit', result);
    });
  };

  // wsl.exe: WSL itself (--version, --list, --import), or a program in the
  // app's distro (after -e).
  const runWsl = (args, child) => {
    const say = (text, code) => setImmediate(() => {
      if (text) child.stdout.emit('data', Buffer.from(`${text}\n`));
      child.emit('exit', code);
    });
    if (args[0] === '--version') { say('WSL version: 3.0.1.0', machine.installed ? 0 : 1); return; }
    if (!machine.installed) { say('Windows Subsystem for Linux is not installed.', 1); return; }
    if (args[0] === '--list') { say([...new Set(['Ubuntu', ...Object.keys(machine.existing), ...(machine.distro ? ['Decurix'] : [])])].join('\n'), 0); return; }
    if (args[0] === '--import') {
      machine.imports = (machine.imports || 0) + 1;
      if (machine.importSays && !(machine.importWorksAfterAsking && machine.askedForPlatform)) { say(machine.importSays, 1); return; }
      machine.distro = true; say('The operation completed successfully.', 0); return;
    }
    const program = args.slice(args.indexOf('-e') + 1);
    const distro = args[args.indexOf('-d') + 1];
    const available = distro === 'Decurix' ? machine.distro : Object.hasOwn(machine.existing, distro);
    if (!available) { say(`There is no distribution with the supplied name.`, 1); return; }
    if (program[0] === 'sh' && program[1] === '-c') {
      if (program[2] === 'ip -4 route show default') { say('default via 172.28.0.1 dev eth0', 0); return; }
      if (program[2].startsWith('docker_root=')) { say('104857600', 0); return; }
      if (program[2].startsWith('umask 077 && tr ')) { say('', 0); return; }
      say('runtime probe', machine.existing[distro] ? 0 : 1); return;
    }
    if (distro !== 'Decurix' && !machine.existing[distro]) { say('Docker is not running', 1); return; }
    if (program[0] === 'cat') { say(staged, staged === null ? 1 : 0); return; }
    if (program[0] === 'sh' && /maintain-existing\.sh$/.test(program[1] || '')) {
      say((machine.inventory || []).map((name) => `decurix-resource ${name}`).join('\n'), machine.inventoryFails ? 1 : 0); return;
    }
    if (program[0] === 'sh') { say('decurix-setup: ready', machine.setupFails ? 1 : 0); return; }
    if (program[0] === '/opt/decurix/dockerd.sh') { machine.daemon = true; return; }
    let docker = program[0] === 'env' ? program.slice(program.indexOf('docker') + 1) : program.slice(1);
    if (docker[0] === '--host') docker = docker.slice(2);
    if (docker[0] === 'version') { say(machine.daemon ? '29.8.2' : 'Cannot connect to the Docker daemon', machine.daemon ? 0 : 1); return; }
    runDocker(docker, child);
  };

  const childProcess = {
    spawn(file, args, options) {
      spawns.push({ file, args, options });
      const child = new EventEmitter();
      children.push(child);
      child.unref = () => {};
      child.stdout = new EventEmitter();
      child.stderr = new EventEmitter();
      if (path.basename(file) === 'docker') { runDocker(Array.from(args), child); return child; }
      if (file === 'wsl.exe') { runWsl(Array.from(args), child); return child; }
      if (file === 'powershell.exe') {
        if (Array.from(args).join(' ').includes('native-ollama.ps1')) {
          if (!holdOllama) setImmediate(() => child.emit('exit', ollamaFails ? 1 : 0));
          return child;
        }
        // Turning WSL on, when asked to; or ending an earlier version's
        // Windows control plane.
        if (Array.from(args).join(' ').includes('--install') && machine.installedAfterAsking) machine.installed = true;
        if (Array.from(args).join(' ').includes('--install')) machine.askedForPlatform = true;
        // The daemon, started hidden through WMI (startHiddenScript).
        if (Array.from(args).join(' ').includes('Win32_Process') && Array.from(args).join(' ').includes('/opt/decurix/dockerd.sh')) machine.daemon = true;
        setImmediate(() => child.emit('exit', 0));
        return child;
      }
      return child;
    },
  };

  const fileSystem = {
    mkdirSync() {},
    existsSync(file) { return present.has(file); },
    copyFileSync(from, to) { copied.push({ from, to }); },
    writeFileSync(file, content) { written.push({ file, content }); },
    writeSync() {},
    readFileSync(file) {
      if (settings && file === path.join(data, '.env')) return settings;
      const saved = written.findLast(entry => entry.file === file);
      if (saved) return saved.content;
      if (file === path.join(data, 'local-hosting.json')) return JSON.stringify({ enabled: hosting });
      if (file === path.join(data, 'public-link.json') && publicLink !== null) return JSON.stringify({ enabled: publicLink });
      // An earlier start asked for a restart (restartedSince: and Windows has booted since).
      if (file === path.join(data, 'restart-asked.json') && restartAsked) return JSON.stringify({ reason: restartAsked, at: restartedSince ? 0 : Date.now() });
      if (file === path.join(data, 'windows-runtime.json') && savedRuntime !== undefined) return savedRuntime;
      if (file === path.join('/installed/resources', 'stack', 'VERSION')) return 'build-2';
      if (file === path.join(stack, 'VERSION') && staged !== null) return staged;
      throw Object.assign(new Error('no such file'), { code: 'ENOENT' });
    },
    openSync() { return 1; },
    closeSync() {},
    statfsSync() { return { bavail: freeGB * 1024 * 1024, bsize: 1024 }; },
    fstatSync() { return { size: log.length }; },
    readSync(_file, buffer, _offset, length, position) { return log.copy(buffer, 0, position, position + length); },
  };

  const modules = {
    electron: {
      app, BrowserWindow: FakeWindow, ipcMain, shell: { openExternal: (url) => external.push(url) },
      dialog: { showMessageBox: async (owner, options) => {
        questions.push(options || owner);
        return { response: adopt ? 0 : 1 };
      }, showOpenDialog: async (owner, options) => {
        pickerCalls.push({ owner, options });
        return { canceled: canceledPicker, filePaths: pickedFolders };
      } },
      powerSaveBlocker: { start: (kind) => { blockers.push(kind); return blockers.length; }, stop: (id) => { blockers.push(`stop ${id}`); } },
    },
    'node:child_process': childProcess,
    // Finding this computer's address on the network is not what is tested here.
    'node:dgram': { createSocket: () => ({ once() {}, connect(_port, _host, callback) { if (lanAddress) setImmediate(callback); }, address: () => ({ address: lanAddress }), close() {} }) },
    'node:os': { homedir: () => home, tmpdir: () => path.join(home, 'tmp'), release: () => release, uptime: () => 60 },
    'node:fs': fileSystem,
    'node:http': http,
    'node:path': path,
    './local-models': require('./local-models'),
    './build-watchdog': require('./build-watchdog'),
    './windows-runtime': {
      readSettings: () => ({ DECURIX_WINDOWS_NATIVE_OLLAMA: '1' }),
      configureResources: () => ({ memoryMB: 8192, workerCPUs: 2, workerMB: 2048, cpus: 8, modelMB: 6144 }),
      usesWindowsOllama: () => true,
      windowsHost: () => '172.28.0.1',
      stackSettings: () => 'OLLAMA_URL=http://172.28.0.1:11435\n',
    },
    './gated-port': loadGatedPort(fileSystem, http),
    './windows-lan': { ensureLanForward: async (dir, address, port) => { lanCalls.push({ dir, address, port }); return lanForwards; } },
    './mobile-access': { setupMobileAccess: async (_dir, tunnelCommand) => {
      mobileCalls.push('start');
      tunnel = tunnelCommand(8090);
      return { stop: async () => { mobileCalls.push('stop'); if (stopFails) throw new Error('connect ECONNREFUSED 127.0.0.1:8080'); } };
    } },
  };
  vm.runInNewContext(source, {
    require: (name) => modules[name],
    process: { resourcesPath: '/installed/resources', platform, arch, env: { PATH: '/usr/bin', LOCALAPPDATA: '/profile/Local' } },
    URL,
    AbortController,
    setTimeout: (callback) => setImmediate(callback),
    // The watch on the control plane: a test makes its clock tick by hand.
    setInterval: (callback) => { intervals.push(callback); },
    Buffer,
    console: { ...console, error() {}, warn() {} },
    __dirname,
  }, { filename: 'main.js' });
  const settle = async (turns = 12) => { for (let i = 0; i < turns; i += 1) await new Promise((resolve) => setImmediate(resolve)); };
  await settle(40);
  const wslWords = (s) => Array.from(s.args).join(' ');
  return {
    window: windows[0], app, spawns, questions, children, copied, written, external, mobileCalls, posts, lanCalls, intervals, settle, blockers, stack, home, machine, pickerCalls,
    titleBarTheme: (palette, url = 'http://127.0.0.1:8080/', { childFrame = false, otherWindow = false } = {}) => {
      const sender = windows[0].webContents;
      sender.mainFrame = { url };
      ipcMain.emit('window-titlebar-theme', {
        sender: otherWindow ? {} : sender,
        senderFrame: childFrame ? { url } : sender.mainFrame,
      }, palette);
    },
    pickFolders: (url = 'http://127.0.0.1:8080/', childFrame = false) => {
      const sender = windows[0].webContents;
      sender.mainFrame = { url };
      return ipcHandlers.get('pick-host-folders')({ sender, senderFrame: childFrame ? { url } : sender.mainFrame });
    },
    modelAction: (channel, value, url = 'http://127.0.0.1:8080/', childFrame = false) => {
      const sender = windows[0].webContents;
      sender.mainFrame = { url };
      return ipcHandlers.get(channel)({ sender, senderFrame: childFrame ? { url } : sender.mainFrame }, value);
    },
    // The docker commands run, each as its words - on Windows, inside the distro.
    get docker() {
      return spawns.flatMap((s) => {
        if (path.basename(s.file) === 'docker') return [Array.from(s.args).join(' ')];
        if (s.file !== 'wsl.exe') return [];
        const args = Array.from(s.args);
        const at = args.indexOf('docker');
        let command = args.slice(at + 1);
        if (command[0] === '--host') command = command.slice(2);
        return at >= 0 && args.includes('-e') ? [command.join(' ')] : [];
      });
    },
    // WSL itself and what was run in the distro (not docker), as words.
    get wsl() { return spawns.filter((s) => s.file === 'wsl.exe').map(wslWords).filter((w) => !w.includes(' docker ')); },
    // How many times the stack was brought up.
    get ups() { return this.docker.filter((d) => d === 'compose up -d --no-build').length; },
    // One tick of the watch, and time for what it starts.
    tick: async (times = 1) => { for (let i = 0; i < times; i += 1) { await Promise.all(intervals.map((run) => run())); await settle(40); } },
    setState: (next) => { state = next; },
    get tunnel() { return tunnel; }, get quits() { return quits; },
  };
}

// ---- Windows: the stack in the app's own WSL distro ----

test('reuses a running dashboard and installs only the Ollama runtime in the background', async () => {
  const result = await boot('ready');
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  // Only the question which build the distro's stack is.
  assert.deepEqual(result.docker, []);
  assert.ok(result.wsl.includes('-d Decurix -u root --cd / -e cat /var/lib/decurix/stack/VERSION'));
  assert.ok(result.spawns.some(s => Array.from(s.args).join(' ').includes('native-ollama.ps1') && Array.from(s.args).includes('install')));
  assert.ok(!result.spawns.some(s => Array.from(s.args).includes('ensure')));
  assert.ok(!result.spawns.some(s => Array.from(s.args).includes('serve')));
});

test('a first launch imports the distro, prepares it, starts Docker and the stack', async () => {
  const result = await boot('absent', { wsl: { distro: false, daemon: false } });
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  const resources = '/installed/resources';
  const words = result.wsl;
  const importAt = words.findIndex((w) => w.startsWith('--import'));
  assert.equal(words[importAt], `--import Decurix ${path.join('/profile/Local', 'Decurix', 'wsl')} ${path.join(resources, 'wsl', 'alpine-minirootfs.tar.gz')} --version 2`);
  // Prepared with what the app ships, as the distro sees it, and the person's settings.
  const setup = words.find((w) => w.includes(' -e sh '));
  assert.equal(setup, `-d Decurix -u root --cd / -e sh ${path.join(resources, 'wsl', 'setup.sh').replace(/\\/g, '/')} ${path.join(resources, 'stack').replace(/\\/g, '/')} ${path.join(data, '.env').replace(/\\/g, '/')}`);
  assert.ok(words.indexOf(setup) > importAt);

  // The daemon outlives the window and has none: Windows (WMI) starts its
  // wsl.exe hidden, not this app - no wsl.exe child of the app runs it.
  const daemon = result.spawns.find((s) => s.file === 'powershell.exe' && Array.from(s.args).join(' ').includes('/opt/decurix/dockerd.sh'));
  assert.ok(daemon, 'the Docker daemon was not started');
  const daemonWords = Array.from(daemon.args).join(' ');
  assert.ok(daemonWords.includes('Win32_Process -MethodName Create') && daemonWords.includes('ShowWindow=[uint16]0'), daemonWords);
  assert.equal(daemon.options.windowsHide, true);
  assert.ok(!result.spawns.some((s) => s.file === 'wsl.exe' && Array.from(s.args).includes('/opt/decurix/dockerd.sh')));
  assert.ok(!result.spawns.some((s) => s.options && s.options.detached), 'nothing is started detached: it would get a window');

  assert.deepEqual(result.docker, [
    'version --format {{.Server.Version}}',
    'version --format {{.Server.Version}}',
    'compose build',
    'network inspect decurix-network',
    'compose run --rm --no-deps -T desktop-manager ./decurix --build-images -images desktop',
    'compose up -d --no-build',
    // The tools image only once the dashboard is up, waited for by nothing.
    'compose run --rm --no-deps -T desktop-manager ./decurix --build-images -images tools',
  ]);
  // Compose runs in the staged stack, with the project a checkout's stack has.
  const up = result.spawns.find((s) => Array.from(s.args).includes('up'));
  assert.deepEqual(Array.from(up.args).slice(0, 10), ['-d', 'Decurix', '-u', 'root', '--cd', '/var/lib/decurix/stack', '-e', 'env', 'COMPOSE_PROJECT_NAME=decurix', 'DECURIX_HOST_HOME=/home']);
  // The gated port is published on the distro's loopback only: a desktop must not come back in through Docker's
  // proxy on another address of the distro, where it looks like this computer (review 2026-11-02).
  assert.ok(Array.from(up.args).includes('DECURIX_LAN_BIND=127.0.0.1'));
  assert.equal(up.options.windowsHide, true);

  // The person's settings name the control plane's own model server.
  const settings = result.written.find((w) => w.file === path.join(data, '.env'));
  assert.equal(settings.content, 'DECURIX_WINDOWS_NATIVE_OLLAMA=1\nOLLAMA_MODEL=auto\n');
  // Nothing of the old Windows control plane is started; it is ended if running.
  assert.ok(!result.spawns.some((s) => /decurix\.exe$/.test(s.file)));
  assert.ok(result.spawns.some((s) => s.file === 'powershell.exe' && Array.from(s.args).join(' ').includes('decurix.exe')));
});

test('a second start finds the distro and its Docker, and only brings the stack up', async () => {
  const result = await boot('absent');
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.ok(!result.wsl.some((w) => w.startsWith('--import')));
  assert.ok(!result.spawns.some((s) => Array.from(s.args).join(' ').includes('/opt/decurix/dockerd.sh')));
  assert.equal(result.ups, 1);
});

test('the Windows control plane of an earlier version on 8080 is replaced by the stack', async () => {
  // The distro was never set up: what answers on 8080 is not this app's stack.
  const result = await boot('ready', { wsl: { distro: false, daemon: false } });
  assert.ok(result.wsl.some((w) => w.startsWith('--import')));
  assert.equal(result.ups, 1);
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
});

test('a stack an older build of the app staged is brought up again', async () => {
  const result = await boot('ready', { staged: 'build-1' });
  assert.ok(result.wsl.some((w) => w.includes(' -e sh ')));
  assert.equal(result.ups, 1);
});

test('turns WSL on when Windows does not have it, and says to restart', async () => {
  const result = await boot('absent', { wsl: { installed: false } });
  const ask = result.spawns.find((s) => s.file === 'powershell.exe' && Array.from(s.args).join(' ').includes('--install'));
  assert.ok(ask, 'WSL was not turned on');
  assert.match(Array.from(ask.args).join(' '), /-Verb RunAs/);
  assert.equal(result.window.lastStatus.value.failed, true);
  assert.match(result.window.lastStatus.value.message, /Restart Windows/);
  assert.deepEqual(result.docker, []);
});

test('asks for a restart for WSL once: after it, fetches WSL from GitHub and says what is wrong', async () => {
  const first = await boot('absent', { wsl: { installed: false } });
  assert.ok(first.written.some((w) => w.file.endsWith('restart-asked.json') && w.content.includes('"wsl"')));

  // WSL comes from GitHub, never the Store, from the first prompt on.
  const ask = first.spawns.find((s) => s.file === 'powershell.exe' && Array.from(s.args).join(' ').includes('--install'));
  assert.match(Array.from(ask.args).join(' '), /--install --no-distribution --web-download & wsl\.exe --update --web-download/);

  const after = await boot('absent', { wsl: { installed: false }, restartAsked: 'wsl' });
  assert.equal(after.window.lastStatus.value.failed, true);
  assert.match(after.window.lastStatus.value.message, /even after a restart.*wsl --update --web-download/);

  // Not restarted yet: asked again, the same way.
  const notYet = await boot('absent', { wsl: { installed: false }, restartAsked: 'wsl', restartedSince: false });
  assert.match(notYet.window.lastStatus.value.message, /^Windows Subsystem for Linux is installed\. Restart Windows/);
});

test('goes on when turning WSL on needs no restart', async () => {
  const result = await boot('absent', { wsl: { installed: false, installedAfterAsking: true, distro: false, daemon: false } });
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.ok(result.wsl.some((w) => w.startsWith('--import')));
});

// ---- a computer that never had Decurix: said in plain words, before anything is set up ----

test('a first install refuses an ARM computer, an old Windows and a full disk, and sets nothing up', async () => {
  for (const [options, says] of [
    [{ arch: 'arm64' }, /ARM processor/],
    [{ release: '10.0.18363' }, /Windows 10 version 2004 or newer.*build 18363/],
    [{ freeGB: 12 }, /about 20 GB of free disk space.*has 12 GB/],
  ]) {
    const result = await boot('absent', { wsl: { distro: false, daemon: false }, ...options });
    assert.equal(result.window.lastStatus.value.failed, true);
    assert.match(result.window.lastStatus.value.message, says);
    assert.ok(!result.wsl.some((w) => w.startsWith('--import')));
    assert.ok(!result.spawns.some((s) => Array.from(s.args).join(' ').includes('--install')));
  }
});

test('an installed Decurix is not held to the first install\'s disk space', async () => {
  const result = await boot('absent', { freeGB: 12 });
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
});

test('a Windows that cannot start Linux is asked to turn the platform on, then goes on', async () => {
  const result = await boot('absent', { wsl: { distro: false, daemon: false, importWorksAfterAsking: true,
    importSays: 'WSL2 is not supported with your current machine configuration. Please enable the "Virtual Machine Platform" optional component' } });
  const ask = result.spawns.find((s) => s.file === 'powershell.exe' && Array.from(s.args).join(' ').includes('--install'));
  assert.match(Array.from(ask.args).join(' '), /-Verb RunAs/);
  assert.equal(result.machine.imports, 2);
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
});

test('virtualization that stays off says to restart, then where it is switched on', async () => {
  const result = await boot('absent', { wsl: { distro: false, daemon: false,
    importSays: 'Error code: Wsl/Service/CreateInstance/CreateVm/HCS/HCS_E_HYPERV_NOT_INSTALLED' } });
  assert.equal(result.window.lastStatus.value.failed, true);
  assert.match(result.window.lastStatus.value.message, /Restart Windows.*BIOS\/UEFI/);
  assert.deepEqual(result.docker, []);
});

test('virtualization still off after the restart is not answered with another restart', async () => {
  const result = await boot('absent', { restartAsked: 'platform', wsl: { distro: false, daemon: false,
    importSays: 'Error code: Wsl/Service/CreateInstance/CreateVm/HCS/HCS_E_HYPERV_NOT_INSTALLED' } });
  assert.equal(result.window.lastStatus.value.failed, true);
  assert.doesNotMatch(result.window.lastStatus.value.message, /Restart Windows, then/);
  assert.match(result.window.lastStatus.value.message, /after the restart.*HCS_E_HYPERV_NOT_INSTALLED.*BIOS\/UEFI.*hypervisorlaunchtype/);
  assert.equal(result.machine.imports, 1);
  assert.ok(!result.spawns.some((s) => Array.from(s.args).join(' ').includes('--install')));
});

test('a distro that cannot be imported for another reason names the log, and asks for nothing', async () => {
  const result = await boot('absent', { wsl: { distro: false, daemon: false, importSays: 'The disk is full.' } });
  assert.match(result.window.lastStatus.value.message, /Could not set up Linux/);
  assert.equal(result.machine.imports, 1);
  assert.ok(!result.spawns.some((s) => Array.from(s.args).join(' ').includes('--install')));
});

test('a distro that cannot be prepared stops the start, with the log', async () => {
  const result = await boot('absent', { wsl: { setupFails: true } });
  assert.equal(result.window.lastStatus.value.failed, true);
  assert.match(result.window.lastStatus.value.message, /Could not prepare Linux/);
  assert.match(result.window.lastStatus.value.detail, /address in use/);
  assert.deepEqual(result.docker, []);
});

test('Windows gets the plain IP link once the address is forwarded to WSL', async () => {
  const result = await boot('absent', { lanAddress: '192.168.1.40' });
  await result.settle(20);
  assert.deepEqual(result.lanCalls.map((c) => [c.address, c.port]), [['192.168.1.40', 8090]]);
  const post = result.posts.find((p) => p.url.endsWith('/api/tunnel'));
  assert.equal(post.body, JSON.stringify({ lan_url: 'http://192.168.1.40:8090' }));
});

test('the IP link is reported when the stack was already running, and again each minute without asking again', async () => {
  const result = await boot('ready', { lanAddress: '192.168.1.40' });
  await result.settle(20);
  assert.equal(result.lanCalls.length, 1);
  const tunnelPosts = () => result.posts.filter((p) => p.url.endsWith('/api/tunnel')).length;
  assert.equal(tunnelPosts(), 1);
  for (const tick of result.intervals) tick();
  await result.settle(20);
  assert.equal(tunnelPosts(), 2);
  assert.equal(result.lanCalls.length, 1);
});

test('no IP link is given when the forwarding was declined, or turned off in .env', async () => {
  const declined = await boot('absent', { lanAddress: '192.168.1.40', lanForwards: false });
  await declined.settle(20);
  assert.equal(declined.lanCalls.length, 1);
  assert.ok(declined.posts.some((p) => p.url.endsWith('/api/tunnel') && JSON.parse(p.body).lan_url === ''));

  const off = await boot('absent', { lanAddress: '192.168.1.40', settings: 'DECURIX_WSL_LAN_FORWARD=0\n' });
  await off.settle(20);
  assert.equal(off.lanCalls.length, 0);
  assert.ok(off.posts.some((p) => p.url.endsWith('/api/tunnel') && JSON.parse(p.body).lan_url === ''));
});

test('keeps the Electron profile out of the control plane data dir', async () => {
  const result = await boot('ready');
  assert.equal(result.app.paths.userData, path.join(data, 'electron'));
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

test('shows a retry screen with the last log line when the stack never comes up', async () => {
  const result = await boot('absent', { dockerRun: (args) => (args.includes('up') ? 1 : 0) });
  assert.equal(result.window.loadFileCount, 1);
  assert.equal(result.window.lastStatus.value.failed, true);
  assert.match(result.window.lastStatus.value.message, /Could not start Decurix in Docker/);
  assert.match(result.window.lastStatus.value.detail, /address in use/);
});

// Found by killing the control plane under an open window: it had been started
// by an earlier run of the app, so nothing told this one, and the window went on
// showing a dashboard that no longer answered.
test('starts the stack again when it dies under an open window', async () => {
  const result = await boot('ready', { publicLink: true });
  assert.equal(result.ups, 0);

  // One missed answer is a busy moment, not a death.
  result.setState('absent');
  await result.tick(1);
  result.setState('ready');
  await result.tick(3);
  assert.equal(result.ups, 0);
  assert.equal(result.window.loadFileCount, 1);

  result.setState('absent');
  await result.tick(3);
  assert.equal(result.ups, 1, 'the stack was not started again');
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.equal(result.window.loadFileCount, 2);
  // The phone link belonged to the one that died, and is opened again.
  assert.deepEqual(result.mobileCalls, ['start', 'stop', 'start']);
});

// Found on the installed app: stopping the phone link reports to the control
// plane, which is exactly what is gone - and that error stopped the restart.
test('starts it again even though the phone link cannot be cleared', async () => {
  const result = await boot('ready', { stopFails: true, publicLink: true });
  result.setState('absent');
  await result.tick(3);
  assert.equal(result.ups, 1, 'the stack was not started again');
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.deepEqual(result.mobileCalls, ['start', 'stop', 'start']);
});

test('one that keeps dying is left stopped, with a Retry', async () => {
  const result = await boot('ready');
  for (let death = 0; death < 3; death += 1) {
    result.setState('absent');
    await result.tick(3);
  }
  assert.equal(result.ups, 3);

  result.setState('absent');
  await result.tick(3);
  assert.equal(result.ups, 3, 'it was started a fourth time');
  assert.equal(result.window.lastStatus.value.failed, true);
  assert.match(result.window.lastStatus.value.message, /keeps stopping/);
  assert.match(result.window.lastStatus.value.detail, /address in use/);
});

test('does not start it again while the app is closing', async () => {
  const result = await boot('ready');
  result.window.emit('closed');
  result.app.emit('window-all-closed');
  result.setState('absent');
  await result.tick(4);
  assert.equal(result.ups, 0);
});

test('the public link is off until the person turns it on: no tunnel by default', async () => {
  const result = await boot('ready');
  assert.deepEqual(result.mobileCalls, []);
  assert.equal(result.tunnel, null);
  assert.deepEqual({ ...(await result.modelAction('public-link-status')) }, { available: true, enabled: false });
});

test('turning the public link on from the dashboard starts the tunnel, saves the choice, and off stops it', async () => {
  const result = await boot('ready');
  assert.deepEqual({ ...(await result.modelAction('public-link-set', true)) }, { available: true, enabled: true });
  await result.settle(3);
  assert.deepEqual(result.mobileCalls, ['start']);
  const saved = result.written.findLast((w) => w.file === path.join(data, 'public-link.json'));
  assert.deepEqual(JSON.parse(saved.content), { enabled: true });
  assert.deepEqual({ ...(await result.modelAction('public-link-set', false)) }, { available: true, enabled: false });
  assert.deepEqual(result.mobileCalls, ['start', 'stop']);
});

test('only the dashboard may turn the public link on, and only with a yes or no', async () => {
  const result = await boot('ready');
  await assert.rejects(async () => result.modelAction('public-link-set', true, 'https://example.com/'), /only set from the desktop dashboard/);
  await assert.rejects(async () => result.modelAction('public-link-set', true, 'http://127.0.0.1:8080/', true), /only set from the desktop dashboard/);
  await assert.rejects(async () => result.modelAction('public-link-set', 'yes'), /on or off/);
  assert.deepEqual(result.mobileCalls, []);
});

test('runs the shipped cloudflared for the phone link and stops it when the app closes', async () => {
  const result = await boot('ready', { publicLink: true });
  assert.deepEqual(result.mobileCalls, ['start']);
  assert.equal(result.tunnel.file, path.join('/installed/resources', 'cloudflared.exe'));
  assert.equal(result.tunnel.args.at(-1), 'http://127.0.0.1:8090');
  result.window.emit('closed');
  result.app.emit('window-all-closed');
  await result.settle(3);
  assert.deepEqual(result.mobileCalls, ['start', 'stop']);
  assert.equal(result.quits, 1);
});

// ---- a Mac or Linux: the control plane runs in Docker ----

const dockerOnPath = path.join('/usr/local/bin', 'docker');
const cloudflaredOnPath = path.join('/opt/homebrew/bin', 'cloudflared');

test('the external network is created before compose runs anything that needs it, when it is missing', async () => {
  const result = await boot('absent', { dockerRun: (args) => (args.join(' ') === 'network inspect decurix-network' ? 1 : 0) });
  const at = (words) => result.docker.indexOf(words);
  assert.ok(at('network create decurix-network') > at('network inspect decurix-network'));
  assert.ok(at('network create decurix-network') < at('compose run --rm --no-deps -T desktop-manager ./decurix --build-images -images desktop'));
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
});

test('on a Mac, a first launch builds and starts the control plane in Docker', async () => {
  const result = await boot('absent', { platform: 'darwin', files: [dockerOnPath] });
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.deepEqual(result.docker, [
    'version --format {{.Server.Version}}',
    'compose build',
    'network inspect decurix-network',
    'compose run --rm --no-deps -T desktop-manager ./decurix --build-images -images desktop',
    'compose up -d --no-build',
    // The tools image only once the dashboard is up, waited for by nothing.
    'compose run --rm --no-deps -T desktop-manager ./decurix --build-images -images tools',
  ]);

  // Docker is found where Docker Desktop puts it, which an app started from
  // the Finder does not have on its PATH - and its helpers need that PATH too.
  const run = result.spawns[0];
  assert.equal(run.file, dockerOnPath);
  assert.equal(run.options.cwd, result.stack);
  assert.ok(run.options.env.PATH.split(path.delimiter).includes('/usr/local/bin'));
  assert.equal(run.options.env.DECURIX_HOST_HOME, result.home);
  // The project a checkout's own compose stack has, so the app takes it over.
  assert.equal(run.options.env.COMPOSE_PROJECT_NAME, 'decurix');
  assert.equal(run.options.env.DECURIX_HOST_MNT_C, path.join(data, 'no-windows-drive'));

  // What the app ships is staged where docker builds from it, beside the
  // person's settings, which name the control plane's own model server.
  const stagedNames = result.copied.filter((c) => c.to.startsWith(result.stack)).map((c) => path.basename(c.to)).sort();
  assert.deepEqual(stagedNames, ['.env', 'Dockerfile', 'decurix', 'decurix-android-arm64', 'docker-compose.yaml']);
  const settings = result.written.find((w) => w.file === path.join(data, '.env'));
  assert.equal(settings.content, 'OLLAMA_URL=http://decurix-ollama:11434\nOLLAMA_MODEL=auto\n');
  // No Windows control plane is started there.
  assert.ok(!result.spawns.some((s) => /decurix\.exe$/.test(s.file)));
});

test('says Docker Desktop is needed when the Mac has none', async () => {
  const result = await boot('absent', { platform: 'darwin', dockerRun: () => 'missing' });
  assert.equal(result.window.lastStatus.value.failed, true);
  assert.match(result.window.lastStatus.value.message, /needs Docker Desktop/);
  assert.deepEqual(result.docker, ['version --format {{.Server.Version}}']);
});

test('starts Docker Desktop when it is installed and not running', async () => {
  let asked = 0;
  // Not answering the first two times it is asked, then up.
  const dockerRun = (args) => (args[0] === 'version' && (asked += 1) <= 2 ? 1 : 0);
  const result = await boot('absent', { platform: 'darwin', files: [dockerOnPath], dockerRun });
  await result.settle(20);
  assert.ok(result.spawns.some((s) => s.file === 'open' && Array.from(s.args).join(' ') === '-ga Docker'));
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
});

test('on Linux a Docker that does not answer is not started, and the person is told', async () => {
  const result = await boot('absent', { platform: 'linux', files: [dockerOnPath], dockerRun: () => 1 });
  assert.equal(result.window.lastStatus.value.failed, true);
  assert.match(result.window.lastStatus.value.message, /Docker is not running, or this user may not use it/);
  assert.ok(!result.spawns.some((s) => s.file === 'open'));
});

test('a worker image that cannot be built stops the start, with the log', async () => {
  const dockerRun = (args) => (args.includes('desktop') && args.includes('--build-images') ? 1 : 0);
  const result = await boot('absent', { platform: 'darwin', files: [dockerOnPath], dockerRun });
  assert.equal(result.window.lastStatus.value.failed, true);
  assert.match(result.window.lastStatus.value.message, /Could not build the worker desktop/);
  assert.match(result.window.lastStatus.value.detail, /address in use/);
  assert.ok(!result.docker.includes('compose up -d --no-build'));
});

test('a stack an older build of the app left running is replaced by this one', async () => {
  const result = await boot('ready', { platform: 'darwin', files: [dockerOnPath], staged: 'build-1' });
  assert.ok(result.docker.includes('compose up -d --no-build'));
  assert.ok(result.written.some((w) => w.file === path.join(result.stack, 'VERSION') && w.content === 'build-2'));
});

test('a stack this build started is used as it is', async () => {
  const result = await boot('ready', { platform: 'darwin', files: [dockerOnPath], staged: 'build-2' });
  assert.deepEqual(result.docker, []);
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
});

test('the phone link there uses the cloudflared the computer has, and is left out without one', async () => {
  const without = await boot('ready', { platform: 'darwin', publicLink: true });
  assert.deepEqual(without.mobileCalls, []);

  const off = await boot('ready', { platform: 'darwin', files: [cloudflaredOnPath] });
  assert.deepEqual(off.mobileCalls, []);

  const withOne = await boot('ready', { platform: 'darwin', files: [cloudflaredOnPath], publicLink: true });
  assert.deepEqual(withOne.mobileCalls, ['start']);
  assert.equal(withOne.tunnel.file, cloudflaredOnPath);
});

test('Windows caption controls overlay the app header without changing other platforms', async () => {
  const windows = await boot('ready');
  assert.equal(windows.window.options.titleBarStyle, 'hidden');
  assert.equal(windows.window.options.titleBarOverlay.height, 52);
  for (const platform of ['darwin', 'linux']) {
    const result = await boot('ready', { platform });
    assert.equal(result.window.options.titleBarStyle, undefined);
    assert.equal(result.window.options.titleBarOverlay, undefined);
  }
});

test('caption colors follow the dashboard theme and reject untrusted frames or invalid colors', async () => {
  const result = await boot('ready');
  const dark = { color: '#191817', symbolColor: '#f2f0ea' };
  result.titleBarTheme(dark);
  assert.equal(result.window.captionPalette.color, dark.color);
  assert.equal(result.window.captionPalette.symbolColor, dark.symbolColor);
  result.window.captionPalette = undefined;
  result.titleBarTheme(dark, 'https://example.com/');
  result.titleBarTheme(dark, undefined, { childFrame: true });
  result.titleBarTheme(dark, undefined, { otherWindow: true });
  result.titleBarTheme({ color: 'invalid', symbolColor: '#ffffff' });
  result.titleBarTheme(null);
  assert.equal(result.window.captionPalette, undefined);
  const light = { color: '#fffefc', symbolColor: '#1a1917' };
  result.titleBarTheme(light);
  assert.equal(result.window.captionPalette.color, light.color);
});

test('new Windows installs reuse a compatible existing WSL runtime, once the person agrees, without importing or installing Linux', async () => {
  const result = await boot('absent', { wsl: { distro: false, existing: { Ubuntu: true }, inventory: ['decurix-data', 'desktop_old'] } });
  assert.equal(result.questions.length, 1);
  assert.match(result.questions[0].message, /Use the Docker Engine in your WSL distro "Ubuntu"/);
  assert.equal(result.questions[0].defaultId, 1);
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.ok(!result.wsl.some((w) => w.startsWith('--import')));
  assert.ok(!result.spawns.some((s) => Array.from(s.args).join(' ').includes('--install')));
  assert.ok(!result.spawns.some((s) => Array.from(s.args).join(' ').includes('/opt/decurix/dockerd.sh')));
  assert.ok(result.wsl.some((w) => w.includes('stage-existing.sh')));
  assert.ok(!result.wsl.some((w) => /[\\/]wsl[\\/]setup.sh/.test(w)));
  const up = result.spawns.find((s) => Array.from(s.args).includes('up'));
  assert.equal(up.args[1], 'Ubuntu');
  assert.ok(Array.from(up.args).includes('unix:///var/run/docker.sock'));
  const saved = result.written.find((w) => w.file === path.join(data, 'windows-runtime.json'));
  // What the engine held before Decurix used it is kept by an uninstall.
  assert.deepEqual(JSON.parse(saved.content), { distro: 'Ubuntu', adopted: true, preexisting: ['decurix-data', 'desktop_old'], preexistingComplete: true });
});

test("a person who declines sharing their WSL runtime gets Decurix's own Linux", async () => {
  const result = await boot('absent', { adopt: false, wsl: { distro: false, existing: { Ubuntu: true } } });
  assert.equal(result.questions.length, 1);
  assert.ok(result.wsl.some((w) => w.startsWith('--import Decurix')));
  assert.ok(!result.wsl.some((w) => w.includes('maintain-existing.sh')));
  const saved = result.written.find((w) => w.file === path.join(data, 'windows-runtime.json'));
  assert.deepEqual(JSON.parse(saved.content), { distro: 'Decurix' });
});

test('a runtime whose existing resources cannot be listed is not adopted, and nothing is saved', async () => {
  const result = await boot('absent', { wsl: { distro: false, existing: { Ubuntu: true }, inventoryFails: true } });
  assert.equal(result.window.url, undefined);
  assert.match(result.window.lastStatus.value.message, /Could not list what "Ubuntu"/);
  assert.ok(!result.written.some((w) => w.file === path.join(data, 'windows-runtime.json')));
  assert.equal(result.ups, 0);
});

test('existing app-owned workers stay in Decurix even when another WSL runtime is available', async () => {
  const result = await boot('absent', { wsl: { existing: { Ubuntu: true } } });
  const up = result.spawns.find((s) => Array.from(s.args).includes('up'));
  assert.equal(up.args[1], 'Decurix');
  assert.ok(!result.wsl.some((w) => w.includes('stage-existing.sh')));
});

test('an incompatible WSL distro falls back to the managed distro', async () => {
  const result = await boot('absent', { wsl: { distro: false, existing: { Ubuntu: false, 'docker-desktop': true } } });
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.ok(result.wsl.some((w) => w.startsWith('--import Decurix')));
  assert.ok(!result.spawns.some((s) => s.file === 'wsl.exe' && s.args[0] === '-d' && s.args[1] === 'docker-desktop'));
});

test('the saved existing runtime is reused and never silently replaced when it is unavailable', async () => {
  const savedRuntime = JSON.stringify({ distro: 'Ubuntu' });
  const result = await boot('absent', { savedRuntime, wsl: { existing: { Ubuntu: true } } });
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.ok(result.wsl.some((w) => w.includes('stage-existing.sh')));
  const unavailable = await boot('absent', { savedRuntime, wsl: { existing: { Ubuntu: false } } });
  assert.equal(unavailable.window.url, undefined);
  assert.match(unavailable.window.lastStatus.value.message, /saved WSL runtime.*unavailable/);
  assert.ok(!unavailable.wsl.some((w) => w.startsWith('--import')));
  assert.equal(unavailable.ups, 0);
});

test('invalid runtime records stop startup instead of changing where workers live', async () => {
  const result = await boot('absent', { savedRuntime: '{invalid' });
  assert.equal(result.ups, 0);
  assert.match(result.window.lastStatus.value.message, /Could not read the saved WSL runtime/);
});


test('an unreachable native server does not block Windows startup', async () => {
  const result = await boot('absent', { dockerRun: args => args.includes('curl') ? 7 : 0 });
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.ok(result.docker.some(command => command.includes('--build-images')));
  assert.equal(result.ups, 1);
});

test('persisted local hosting starts after the dashboard opens and cannot block it', async () => {
  const result = await boot('absent', { hosting: true, holdOllama: true });
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  const state = await result.modelAction('local-model-status');
  assert.equal(state.busy, true);
  assert.equal(state.phase, 'install');
  assert.equal(result.ups, 1);
});

test('hosting controls persist the choice and keep setup failures in the dashboard', async () => {
  const result = await boot('ready', { ollamaFails: true });
  const state = await result.modelAction('local-model-hosting', true);
  assert.equal(state.phase, 'error');
  assert.equal(state.busy, false);
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.ok(result.written.some(entry => entry.file.endsWith('local-hosting.json') && JSON.parse(entry.content).enabled));
});

test('only the trusted dashboard can control local hosting', async () => {
  const result = await boot('ready');
  for (const channel of ['local-model-status', 'local-model-hosting']) {
    assert.throws(() => result.modelAction(channel, true, 'https://example.test'), /only available/);
    assert.throws(() => result.modelAction(channel, true, 'http://127.0.0.1:8080/', true), /only available/);
  }
});

test('native connectivity errors affect hosting status after the dashboard opens', async () => {
  const result = await boot('absent', { hosting: true, dockerRun: args => args.includes('curl') ? 7 : 0 });
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  const state = await result.modelAction('local-model-status');
  assert.equal(state.phase, 'error');
  assert.match(state.detail, /Linux cannot reach/);
});

test('Linux without a Windows drive mounts an empty folder as the control plane\'s C:, never the host root', async () => {
  const result = await boot('absent', { platform: 'linux', files: [dockerOnPath] });
  const run = result.spawns.find((s) => path.basename(s.file) === 'docker');
  assert.equal(run.options.env.DECURIX_HOST_MNT_C, path.join(data, 'no-windows-drive'));
  const wsl = await boot('absent', { platform: 'linux', files: [dockerOnPath, '/mnt/c'] });
  assert.equal(wsl.spawns.find((s) => path.basename(s.file) === 'docker').options.env.DECURIX_HOST_MNT_C, '/mnt/c');
});

test('calls that only ask something are ended if they hang, so a start can always fail and be retried', async () => {
  const result = await boot('absent');
  const quick = (words) => result.spawns.filter((s) => Array.from(s.args).join(' ').includes(words));
  for (const words of ['--version', '--list --quiet', 'ip -4 route show default', 'version --format']) {
    const calls = quick(words);
    assert.ok(calls.length > 0, `no call with ${words}`);
    for (const call of calls) assert.ok(call.options.timeout > 0 && call.options.timeout <= 60000, words);
  }
  // A build is not a quick call.
  for (const call of quick('compose build')) assert.equal(call.options.timeout, undefined);
});

test('Windows is asked to forward the LAN address once while the app runs, not every minute', async () => {
  const result = await boot('ready', { lanAddress: '192.168.1.40', lanForwards: false });
  await result.settle(20);
  for (let minute = 0; minute < 3; minute += 1) { for (const tick of result.intervals) tick(); await result.settle(20); }
  assert.equal(result.lanCalls.length, 1);
});

test('a full disk does not hide a dashboard that is already running; it is said beside it', async () => {
  const result = await boot('ready', { freeGB: 1 });
  await result.settle(20);
  assert.equal(result.window.url, 'http://127.0.0.1:8080');
  assert.ok(result.questions.some((q) => q.type === 'warning' && /GB is free/.test(q.message)));
});

test('the settings staged in WSL (they hold keys) are written root-only', async () => {
  const result = await boot('ready');
  const copy = result.spawns.find((s) => Array.from(s.args).some((a) => /tr -d/.test(a)));
  const script = Array.from(copy.args).find((a) => /tr -d/.test(a));
  assert.match(script, /^umask 077 && tr -d .* && chmod 600 "\$2"$/);
});
