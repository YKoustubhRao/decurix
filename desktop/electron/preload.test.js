'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
class Node {
  constructor(tag) { this.tag = tag; this.children = []; this.events = {}; this.attributes = {}; this.hidden = false; this.className = ''; this.classList = { add: name => { this.className += ' ' + name; }, toggle: (name, on) => { const s = new Set(this.className.split(' ')); on ? s.add(name) : s.delete(name); this.className = [...s].join(' '); } }; }
  append(...nodes) { this.children.push(...nodes); }
  prepend(node) { this.children.unshift(node); }
  after(node) { this.afterNode = node; }
  setAttribute(key, value) { this.attributes[key] = value; }
  addEventListener(key, fn) { this.events[key] = fn; }
  contains(node) { return node === this || this.children.some(n => n instanceof Node && n.contains(node)); }
  focus() { this.focused = true; }
  scrollIntoView() { this.scrolled = true; }
}
const all = node => [node, ...node.children.filter(n => n instanceof Node).flatMap(all)];
async function boot(landing = true, initial = {}, preferences = {}) {
  let state = { enabled: false, busy: false, phase: 'off', detail: 'Ollama is installed.', model: '', resources: null, ...initial };
  const calls = [], callbacks = {}, timers = [], header = new Node('header'), anchor = new Node('div'), pageHead = new Node('div');
  header.querySelector = () => anchor;
  const document = { body: new Node('body'), head: new Node('head'), documentElement: new Node('html'), createElement: tag => new Node(tag),
    querySelector: sel => sel === '.topbar' ? header : sel === 'main .page-head' ? pageHead : null,
    getElementById: () => landing ? new Node('button') : null, addEventListener: (key, fn) => { document[key] = fn; } };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8'), {
    require: () => ({ contextBridge: { exposeInMainWorld() {} }, ipcRenderer: { send() {}, invoke: async (name, value) => {
      if (name === 'local-model-hosting') { calls.push(value); state = { ...state, enabled: value, phase: value ? 'ready' : 'off', detail: value ? 'Model loaded' : 'Model files deleted' }; }
      return state;
    } } }), process: { platform: 'win32' }, document,
    window: { localStorage: { getItem: key => preferences[key] || null, setItem: (key, value) => { preferences[key] = value; } }, addEventListener: (name, fn) => { (callbacks[name] = callbacks[name] || []).push(fn); }, matchMedia: () => ({ matches: false, addEventListener() {} }) },
    MutationObserver: class { observe() {} }, getComputedStyle: () => ({ getPropertyValue: () => '#ffffff' }),
    setInterval: fn => timers.push(fn),
  });
  for (const fn of callbacks.DOMContentLoaded) fn();
  await new Promise(resolve => setImmediate(resolve));
  const rail = document.body.children[0], sidebar = document.body.children[1], card = sidebar.children[1].children[0];
  const trigger = rail.children[0], close = sidebar.children[0].children[1];
  return { card, rail, sidebar, trigger, close, calls, document, preferences, async refresh(next) { state = { ...state, ...next }; await timers[0](); } };
}
test('left sidebar contains the accessible hosting toggle', async () => {
  const ui = await boot();
  assert.equal(ui.card.attributes['aria-label'], 'Local intelligence');
  const toggle = all(ui.card).find(n => n.tag === 'input');
  assert.equal(toggle.attributes.role, 'switch');
  toggle.checked = true; await toggle.events.change();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(ui.calls, [true]);
  assert.equal(toggle.checked, true);
  assert.ok(all(ui.card).some(n => n.textContent === 'Model loaded'));
  toggle.checked = false; toggle.events.change();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(ui.calls, [true, false]);
  assert.ok(all(ui.card).some(n => /deletes its downloads/.test(n.textContent || '')));
});
test('model download can be switched off; hardware and selected model are displayed', async () => {
  const ui = await boot(true, { enabled: true, busy: true, phase: 'prepare', model: 'qwen3:8b',
    resources: { ram_free_gb: 18.4, cores: 16, gpu_name: 'GPU', vram_free_gb: 10.2 } });
  const toggle = all(ui.card).find(n => n.tag === 'input');
  assert.equal(toggle.disabled, false);
  const texts = all(ui.card).map(n => n.textContent);
  for (const value of ['qwen3:8b', '18.4 GB', '10.2 GB VRAM', '16']) assert.ok(texts.includes(value));
  ui.close.events.click();
  assert.equal(ui.sidebar.hidden, true);
  assert.deepEqual(ui.calls, []);
  assert.equal(toggle.checked, true);
  ui.trigger.events.click();
  assert.equal(ui.sidebar.hidden, false);
  assert.equal(ui.close.focused, true);
});
test('sidebar closes with Escape and reopens from the persistent activity rail', async () => {
  const ui = await boot(false);
  assert.equal(ui.sidebar.hidden, false);
  assert.equal(ui.trigger.attributes['aria-expanded'], 'true');
  assert.equal(ui.trigger.attributes['aria-controls'], ui.sidebar.id);
  ui.document.keydown({ key: 'Escape', target: ui.close });
  assert.equal(ui.sidebar.hidden, true);
  assert.equal(ui.trigger.attributes['aria-expanded'], 'false');
  assert.equal(ui.trigger.focused, true);
  assert.deepEqual(ui.calls, []);
  ui.trigger.events.click();
  assert.equal(ui.sidebar.hidden, false);
});
test('sidebar visibility persists across pages without changing local hosting', async () => {
  const preferences = {};
  const ui = await boot(true, {}, preferences);
  ui.close.events.click();
  const next = await boot(false, {}, preferences);
  assert.equal(next.sidebar.hidden, true);
  next.trigger.events.click();
  assert.equal(next.sidebar.hidden, false);
  assert.equal(preferences['decurix.localModelSidebar'], 'open');
  assert.deepEqual(next.calls, []);
});
test('Ctrl+Shift+L toggles the sidebar and Escape elsewhere does not close it', async () => {
  const ui = await boot();
  ui.document.keydown({ key: 'Escape', target: ui.document.body });
  assert.equal(ui.sidebar.hidden, false);
  let prevented = false;
  ui.document.keydown({ key: 'L', ctrlKey: true, shiftKey: true, preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(ui.sidebar.hidden, true);
  assert.deepEqual(ui.calls, []);
});

// publicLink boots the preload with a Mobile menu panel and a public-link
// status, and records what is asked of main.js.
async function publicLink(status) {
  const calls = [], callbacks = [], observers = [];
  const panel = new Node('div');
  Object.defineProperty(panel, 'firstChild', { get() { return this.children[0]; } });
  panel.prepend = function (node) { this.children = [node, ...this.children.filter(n => n !== node)]; };
  const document = { body: new Node('body'), head: new Node('head'), documentElement: new Node('html'), createElement: tag => new Node(tag),
    querySelector: () => null, getElementById: id => id === 'tunnelPanel' ? panel : null, addEventListener() {} };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8'), {
    require: () => ({ contextBridge: { exposeInMainWorld() {} }, ipcRenderer: { send() {}, invoke: async (name, value) => {
      calls.push([name, value]);
      if (name === 'public-link-set') status = { ...status, enabled: value };
      return status;
    } } }), process: { platform: 'linux' }, document,
    window: { addEventListener: (name, fn) => { if (name === 'DOMContentLoaded') callbacks.push(fn); } },
    MutationObserver: class { constructor(fn) { observers.push(fn); } observe() {} },
  });
  for (const fn of callbacks) await fn();
  return { panel, calls, observers, row: panel.children[0] };
}
test('the Mobile menu shows the public link switch off by default, and turning it on asks main', async () => {
  const ui = await publicLink({ available: true, enabled: false });
  const toggle = all(ui.row).find(n => n.tag === 'input');
  assert.equal(toggle.attributes.role, 'switch');
  assert.equal(toggle.checked, false);
  assert.ok(all(ui.row).some(n => /^Off:/.test(n.textContent || '')));
  toggle.checked = true;
  await toggle.events.change();
  assert.deepEqual(ui.calls.at(-1), ['public-link-set', true]);
  assert.equal(toggle.checked, true);
  assert.ok(all(ui.row).some(n => /^On:/.test(n.textContent || '')));
});
test('the switch stays at the top of the Mobile menu when the menu redraws', async () => {
  const ui = await publicLink({ available: true, enabled: false });
  ui.panel.children = [new Node('p'), new Node('p')];
  ui.observers[0]();
  assert.equal(ui.panel.children[0], ui.row);
  assert.equal(ui.panel.children.length, 3);
});
test('no switch is shown where there is no tunnel program', async () => {
  const ui = await publicLink({ available: false, enabled: false });
  assert.equal(ui.panel.children.length, 0);
});
