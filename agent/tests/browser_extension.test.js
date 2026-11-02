'use strict';

// The browser extension's own checks (agent/browser-agent/extension/background.js), review 2026-11-02:
// a run is done only when it reached its end, a password never leaves its field, and submitting a form is
// checked like clicking its submit button. The functions are taken from background.js by name and run
// against a few fake DOM nodes - no browser, no extension APIs.
//
//     node --test agent/tests/browser_extension.test.js

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const SOURCE = fs.readFileSync(path.join(__dirname, '..', 'browser-agent', 'extension', 'background.js'), 'utf8');

// The source of top-level `function name(...) {...}`, by counting brackets.
function functionSource(name) {
  const start = SOURCE.search(new RegExp(`^(async )?function ${name}\\(`, 'm'));
  assert.ok(start >= 0, `background.js has no function ${name}`);
  // The body's brace comes after the parameter list, which may hold a destructuring pattern of its own.
  let parens = 0;
  let close = SOURCE.indexOf('(', start);
  for (; close < SOURCE.length; close++) {
    if (SOURCE[close] === '(') parens += 1;
    else if (SOURCE[close] === ')' && --parens === 0) break;
  }
  let depth = 0;
  for (let i = SOURCE.indexOf('{', close); i < SOURCE.length; i++) {
    if (SOURCE[i] === '{') depth += 1;
    else if (SOURCE[i] === '}' && --depth === 0) return SOURCE.slice(start, i + 1);
  }
  throw new Error(`unbalanced braces in ${name}`);
}

function load(name, globals = {}) {
  const names = Object.keys(globals);
  // eslint-disable-next-line no-new-func
  return new Function(...names, `${functionSource(name)}\nreturn ${name};`)(...names.map((n) => globals[n]));
}

// ---- a few fake DOM nodes ------------------------------------------------
class Node {
  constructor(tagName, attrs = {}, props = {}) {
    this.tagName = tagName;
    this.attrs = { ...attrs };
    this.parentElement = null;
    this.children = [];
    this.events = [];
    Object.assign(this, props);
  }
  append(...kids) { for (const k of kids) { k.parentElement = this; this.children.push(k); } return this; }
  getAttribute(n) { return n in this.attrs ? this.attrs[n] : null; }
  hasAttribute(n) { return n in this.attrs; }
  setAttribute(n, v) { this.attrs[n] = String(v); }
  matches(selector) {
    return selector.split(',').map((s) => s.trim()).some((s) => {
      if (s === 'form') return this.tagName === 'FORM';
      if (s === 'button') return this.tagName === 'BUTTON';
      if (s === 'input[type=submit]') return this.tagName === 'INPUT' && this.type === 'submit';
      return false;
    });
  }
  closest(selector) {
    for (let n = this; n; n = n.parentElement) if (n.matches(selector)) return n;
    return null;
  }
  querySelector() { return null; }
  scrollIntoView() {}
  focus() {}
  dispatchEvent(e) { this.events.push(e.type); return true; }
}

class HTMLInputElement extends Node {
  constructor(attrs = {}, { value = '', ...props } = {}) { super('INPUT', attrs, props); this._value = value; this.typed = []; }
}
Object.defineProperty(HTMLInputElement.prototype, 'value', {
  get() { return this._value; },
  set(v) { this._value = v; this.typed.push(v); },
});
class HTMLTextAreaElement extends HTMLInputElement {}
class Event { constructor(type) { this.type = type; } }
class KeyboardEvent extends Event {}

function page(nodesBySelector, host = 'shop.example') {
  return {
    document: { title: 'Checkout', querySelectorAll: (s) => nodesBySelector[s] || [] },
    location: { host, href: `https://${host}/checkout` },
    HTMLInputElement, HTMLTextAreaElement, Event, KeyboardEvent,
  };
}

function formWith(buttonLabel) {
  const field = new HTMLInputElement({ 'aria-label': 'Coupon code' }, { type: 'text' });
  const button = new Node('BUTTON', {}, { type: 'submit', innerText: buttonLabel });
  const form = new Node('FORM', {}, { submitted: 0 });
  form.append(field, button);
  form.elements = [field, button];
  form.requestSubmit = () => { form.submitted += 1; };
  field.form = form;
  return { field, button, form };
}

// ---- requestSubmit goes through the gate -----------------------------------
test('a search submitted in a form whose button pays is stopped before anything is touched', () => {
  const { field, form } = formWith('Pay now');
  const domAction = load('domAction', page({ '#coupon': [field] }));
  const result = domAction({ type: 'search', selector: '#coupon', text: 'SAVE10' }, []);
  assert.equal(result.success, false);
  assert.equal(result.needsApproval.action, 'click "Pay now" on shop.example');
  assert.equal(form.submitted, 0, 'the form was not submitted');
  assert.deepEqual(field.typed, [], 'nothing was typed');
});

test('a yes for that click lets the submit through once, and says it was used', () => {
  const { field, form } = formWith('Pay now');
  const domAction = load('domAction', page({ '#coupon': [field] }));
  const result = domAction({ type: 'search', selector: '#coupon', text: 'SAVE10' }, ['click "Pay now" on shop.example']);
  assert.equal(result.success, true);
  assert.deepEqual(result.usedApproval, ['click "Pay now" on shop.example']);
  assert.equal(form.submitted, 1);
});

test('an ordinary search form is submitted with no question', () => {
  const { field, form } = formWith('Search');
  const domAction = load('domAction', page({ '#coupon': [field] }));
  const result = domAction({ type: 'search', selector: '#coupon', text: 'milk' }, []);
  assert.equal(result.success, true);
  assert.equal(form.submitted, 1);
});

// ---- a password never leaves its field ------------------------------------
test('typing into a password field says so, so the step keeps no text', () => {
  const field = new HTMLInputElement({}, { type: 'password' });
  const domAction = load('domAction', page({ '#pw': [field] }));
  const result = domAction({ type: 'type', selector: '#pw', text: 'hunter2' }, []);
  assert.equal(result.success, true);
  assert.equal(result.secretField, true);
  const shownAction = load('shownAction');
  assert.deepEqual(shownAction({ type: 'type', selector: '#pw', text: 'hunter2' }, result),
    { type: 'type', selector: '#pw', text: '(hidden)' });
  const plain = { type: 'type', selector: '#q', text: 'milk' };
  assert.equal(shownAction(plain, { success: true, secretField: false }), plain);
});

test('a recorded password step cannot be replayed blind', () => {
  const field = new HTMLInputElement({}, { type: 'password' });
  const domAction = load('domAction', page({ '#pw': [field] }));
  const result = domAction({ type: 'type', selector: '#pw', text: '', secret: true }, []);
  assert.equal(result.success, false);
  assert.match(result.error, /password/);
  assert.deepEqual(field.typed, []);
});

test('the snapshot says a secret field is filled, never what it holds', () => {
  const pw = new HTMLInputElement({ id: 'pw' }, { type: 'password', id: 'pw', name: 'pw', value: 'hunter2' });
  const otp = new HTMLInputElement({ autocomplete: 'one-time-code' }, { type: 'text', id: 'otp', name: 'otp', value: '482910' });
  const email = new HTMLInputElement({}, { type: 'email', id: 'email', name: 'email', value: 'ana@example.com' });
  const markupPw = new HTMLInputElement({ type: 'password', value: 'prefilled-secret' }, { type: 'password' });
  const clone = {
    querySelector: () => null,
    querySelectorAll: (s) => (s === 'input' ? [markupPw] : []),
    get outerHTML() { return `<input type="password" value="${markupPw.getAttribute('value')}">`; },
  };
  const snapshotPage = load('snapshotPage', {
    document: {
      title: 'Sign in',
      documentElement: { cloneNode: () => clone },
      querySelectorAll: () => [pw, otp, email],
      body: { innerText: 'Sign in' },
    },
    location: { href: 'https://bank.example/login' },
  });
  const snap = snapshotPage();
  const text = JSON.stringify(snap);
  for (const secret of ['hunter2', '482910', 'prefilled-secret']) assert.ok(!text.includes(secret), `${secret} leaked`);
  assert.ok(text.includes('ana@example.com'), 'an ordinary field is still read');
  assert.equal(snap.liveFields.find((f) => f.id === 'pw').value, '(filled - hidden)');
});

test('the recorder keeps no value for a password', () => {
  const handlers = {};
  const sent = [];
  const recorderInject = load('recorderInject', {
    window: {},
    document: { addEventListener: (type, fn) => { handlers[type] = fn; } },
    chrome: { runtime: { sendMessage: (m) => { sent.push(m); return Promise.resolve(); } } },
    CSS: { escape: (s) => s },
  });
  recorderInject();
  const pw = new HTMLInputElement({}, { type: 'password', id: 'pw', name: 'pw', value: 'hunter2' });
  const user = new HTMLInputElement({}, { type: 'text', id: 'user', name: 'user', value: 'ana' });
  handlers.change({ target: pw });
  handlers.change({ target: user });
  assert.deepEqual(sent.map((m) => m.step.text), ['', 'ana']);
  assert.equal(sent[0].step.secret, true);
  assert.ok(!JSON.stringify(sent).includes('hunter2'));
});

// ---- a run is done only when it reached its end -----------------------------
test('a run that did not reach its end is not done', () => {
  const runOutcome = load('runOutcome');
  assert.equal(runOutcome({}, 'instruction').status, 'incomplete', 'the default is not done');
  assert.deepEqual(runOutcome({ completed: false, endedWhy: 'Stopped: reached the max-steps safety cap.' }, 'instruction'),
    { status: 'incomplete', error: 'Stopped: reached the max-steps safety cap.' });
  assert.equal(runOutcome({ completed: true }, 'instruction').status, 'done');
  assert.equal(runOutcome({ completed: true, fatalError: 'model returned no usable action' }).status, 'error');
  assert.equal(runOutcome({ completed: true, needsApproval: { reason: 'pays' } }).status, 'needs_approval');
  assert.equal(runOutcome({}, 'program').error, 'the program did not run to its end');
  assert.equal(runOutcome(null).status, 'error');
});

test('the listener never starts from done', () => {
  const loop = functionSource('runListenerLoop');
  assert.ok(!/let status = "done"/.test(loop), 'status must not default to done');
  assert.match(loop, /runOutcome\(run, req\.kind\)/);
});
