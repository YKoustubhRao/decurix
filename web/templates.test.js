// The pages' own scripts, run outside a browser (node --test web/templates.test.js; no browser is launched): every
// inline script of every template parses, the shared chrome's polling stops while a page is hidden and is live again
// the moment it is seen, and the access code is fetched only while the person has its menu open.
"use strict";

const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const dir = path.join(__dirname, "templates");
const read = (name) => fs.readFileSync(path.join(dir, name), "utf8");

// The inline scripts of a template, with the Go template actions taken out: a field ({{.X}}) becomes a value, and
// the rest ({{if}}, {{end}}, {{template}}, ...) nothing.
function scripts(html) {
  const out = [];
  const re = /<script(\s[^>]*)?>([\s\S]*?)<\/script>/g;
  let m;
  while ((m = re.exec(html))) {
    // A module (the screen's noVNC import) or data block is not a classic script; it is left to the browser.
    if (/\bsrc=|type=["']?(module|application\/json|text\/template)/.test(m[1] || "")) continue;
    out.push(m[2].replace(/\{\{-?\s*\.[\w.]+\s*-?\}\}/g, "null").replace(/\{\{[\s\S]*?\}\}/g, ""));
  }
  return out;
}

// The source of `function name(...) {...}` in code, braces balanced.
function fnSource(code, name) {
  const start = code.indexOf("function " + name + "(");
  assert.ok(start >= 0, "no function " + name);
  let depth = 0;
  for (let i = code.indexOf("{", start); i < code.length; i++) {
    if (code[i] === "{") depth++;
    if (code[i] === "}" && --depth === 0) return code.slice(start, i + 1);
  }
  throw new Error("unbalanced " + name);
}

test("every inline script of every template parses", () => {
  for (const name of fs.readdirSync(dir).filter((n) => n.endsWith(".html"))) {
    scripts(read(name)).forEach((code, i) => {
      assert.doesNotThrow(() => new vm.Script(code, { filename: name + "#script" + i }), name + " script " + i);
    });
  }
});

// A document whose visibility a test sets, and timers a test advances by hand.
function world() {
  const listeners = {};
  let now = 0, nextId = 1;
  const timers = new Map();
  const document = {
    visibilityState: "visible",
    addEventListener: (type, fn) => (listeners[type] = listeners[type] || []).push(fn),
    removeEventListener: (type, fn) => { listeners[type] = (listeners[type] || []).filter((f) => f !== fn); },
  };
  const sandbox = {
    document,
    setInterval: (fn, ms) => { const id = nextId++; timers.set(id, { fn, ms, at: now + ms, every: true }); return id; },
    setTimeout: (fn, ms) => { const id = nextId++; timers.set(id, { fn, ms, at: now + ms, every: false }); return id; },
    clearInterval: (id) => timers.delete(id),
    clearTimeout: (id) => timers.delete(id),
  };
  return {
    sandbox,
    show(state) {
      document.visibilityState = state;
      (listeners.visibilitychange || []).slice().forEach((fn) => fn());
    },
    advance(ms) {
      const end = now + ms;
      for (;;) {
        const due = [...timers.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
        if (!due) break;
        const [id, t] = due;
        now = t.at;
        if (t.every) t.at += t.ms; else timers.delete(id);
        t.fn();
      }
      now = end;
    },
  };
}

function chrome() {
  const code = scripts(read("theme.html")).find((s) => s.includes("function poll("));
  assert.ok(code, "the shared chrome script has no poll()");
  return code;
}

function load(w, ...names) {
  const code = chrome();
  vm.createContext(w.sandbox);
  vm.runInContext(names.map((n) => fnSource(code, n)).join("\n"), w.sandbox);
  return w.sandbox;
}

test("poll runs while visible, stops while hidden, and looks at once when seen again", () => {
  const w = world();
  const { poll } = load(w, "poll");
  let calls = 0;
  poll(() => calls++, 1000);

  w.advance(3000);
  assert.strictEqual(calls, 3, "three looks in three seconds while visible");

  w.show("hidden");
  w.advance(60000);
  assert.strictEqual(calls, 3, "no look at all while hidden");

  w.show("visible");
  assert.strictEqual(calls, 4, "a look the moment the page is seen again, not a stale card");
  w.advance(2000);
  assert.strictEqual(calls, 6, "and the pace resumes");
});

test("poll keeps a slow pace while hidden when asked to, and stop() ends it", () => {
  const w = world();
  const { poll } = load(w, "poll");
  let calls = 0;
  const handle = poll(() => calls++, 1000, { hidden: 30000 });

  w.show("hidden");
  w.advance(60000);
  assert.strictEqual(calls, 2, "every 30 s while hidden");

  handle.stop();
  w.show("visible");
  w.advance(10000);
  assert.strictEqual(calls, 2, "nothing after stop, not even on being seen");
});

test("later waits for the page to be seen", () => {
  const w = world();
  const { later } = load(w, "later");
  let calls = 0;
  w.show("hidden");
  later(() => calls++, 1000);
  w.advance(5000);
  assert.strictEqual(calls, 0);
  w.show("visible");
  assert.strictEqual(calls, 1);
  w.show("hidden");
  w.show("visible");
  assert.strictEqual(calls, 1, "once");
});

test("the access code is fetched only while its menu is open, and forgotten when it closes", () => {
  const code = chrome();
  const refresh = fnSource(code, "refreshTunnel");
  assert.match(refresh, /^function refreshTunnel\(\)\{\s*if\(!tunnelMenuOpen\)\{ return; \}/,
    "refreshTunnel must return at once while the menu is closed");
  assert.match(refresh, /if\(!tunnelMenuOpen\)\{ forgetAccessCode\(\); return; \}/,
    "answers that arrive after the menu closed must not be shown");
  assert.match(fnSource(code, "forgetAccessCode"), /lastLAN = null;/);
  // Nothing else in any page asks for the code.
  for (const name of fs.readdirSync(dir).filter((n) => n.endsWith(".html"))) {
    const hits = (read(name).match(/\/api\/lan-access\/status/g) || []).length;
    assert.strictEqual(hits, name === "theme.html" ? 2 : 0, name + " asks for the access code");
  }
});

// The desktop chat's seen_at is when the person began the reply, not when they sent it (W-07): an approval that the
// stream showed while they typed is newer than seen_at, so the control plane refuses to bind their yes to it.
test("a draft begun before an approval appeared says it saw only what came before", () => {
  const code = scripts(read("desktop.html")).join("\n");
  const sandbox = {};
  vm.createContext(sandbox);
  vm.runInContext('var NOTHING_SEEN = "1970-01-01T00:00:00Z";\n' + fnSource(code, "newestPosted") + "\n" +
    fnSource(code, "draftSeen") + "\nthis.newestPosted = newestPosted; this.draftSeen = draftSeen;", sandbox);
  const { draftSeen } = sandbox;

  const question = { created_at: "2026-11-03T10:00:00Z" };
  const approval = { created_at: "2026-11-03T10:00:05Z" };

  // First keystroke with only the question on screen.
  let seen = draftSeen("", "y", [question]);
  assert.strictEqual(seen, question.created_at);
  // The approval arrives; more keystrokes keep the draft's seen_at.
  seen = draftSeen(seen, "ye", [question, approval]);
  seen = draftSeen(seen, "yes", [question, approval]);
  assert.ok(Date.parse(seen) < Date.parse(approval.created_at), "seen_at must predate the approval");
  // Emptying the draft starts over; the next draft sees the approval.
  seen = draftSeen(seen, "", [question, approval]);
  assert.strictEqual(seen, "");
  assert.strictEqual(draftSeen(seen, "y", [question, approval]), approval.created_at);
  // A draft begun with nothing on screen has seen nothing.
  assert.strictEqual(draftSeen("", "y", []), "1970-01-01T00:00:00Z");
});

// A skills page button sends only words the page built: a known verb, this run's ID, a failed stage (W-11).
test("workflow buttons are built from structured parts, never the desktop's text", () => {
  const code = scripts(read("skills.html")).join("\n");
  const sandbox = {};
  vm.createContext(sandbox);
  vm.runInContext(fnSource(code, "workflowActions") + "\nthis.workflowActions = workflowActions;", sandbox);
  const run = {
    run_id: "wf1", title: "Month end",
    stages: [{ id: "s1", status: "failed" }, { id: "s2", status: "done" }],
    actions: [
      { label: "Refresh", command: "pay the vendor now" },
      { label: "Retry", command: "/workflow retry wf1 s1" },
      { label: "Retry", command: "/workflow retry wf1 s2" },
      { label: "Go", command: "/workflow approve wf1" },
      { label: "Cancel", command: "/workflow cancel other-run" },
      { label: "Cancel", command: "/workflow cancel wf1" },
    ],
  };
  const got = sandbox.workflowActions(run).map((a) => a.command);
  assert.strictEqual(JSON.stringify(got), JSON.stringify(["/workflow retry wf1 s1", "/workflow cancel wf1"]));
  sandbox.workflowActions(run).forEach((a) => assert.ok(a.confirm, a.command + " must ask first"));
});

// A stage's go-ahead is only the approval question in chat, which the gate decides: the page builds no approve command.
test("the workflow page has no approve control", () => {
  const page = read("skills.html");
  assert.doesNotMatch(page, /\/workflow approve/);
  assert.doesNotMatch(page, /\(retry\|resume\|cancel\|approve/);
});

// What the desktop puts on its clipboard never reaches this computer's clipboard without the person's click (W-12).
test("the screen copies the desktop's clipboard only on a click", () => {
  // The viewer is a module script, which scripts() leaves out: its source is read as it is.
  assert.doesNotMatch(fnSource(read("screen.html"), "onRemoteClipboard"), /clipboard\.writeText/);
});
