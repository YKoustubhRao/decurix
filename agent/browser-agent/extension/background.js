// Orchestrates the same atomize -> (expectation) -> act -> verify loop as
// runner.py's run_task, but sourced from a live Chrome tab (this extension's
// own snapshot + action execution) instead of Playwright/CDP, talking to
// ext_server.py -- server.py and browser_host.py are never involved.
//
// Behavior parity with server.py/runner.py, deliberately carried over:
// - PAUSE on failure or a NOT_MET verification, same as runner.py's docstring
//   ("on ANY failure the run PAUSES rather than auto-retrying").
// - "swapping model into VRAM" awareness before a call that will evict the
//   other resident model, via ext_server.py's /will_swap.
// - Captcha detection halts the run outright.
// One deliberate deviation: a MAX_STEPS safety cap, because here it's the
// user's live, logged-in tab rather than a throwaway Playwright profile.
//
// Beyond the run loop this file owns:
// - Tab-group tracking + an in-page side-car: the moment a run starts, its
//   tab (and any tab it opens, grouped via chrome.tabGroups) is locked
//   against real user input and shows a translucent panel with what the
//   agent is doing right now, plus a Stop button that works with no popup
//   open at all.
// - Record/replay: real user click/type/navigation captured verbatim and
//   replayable with no model call.
// - A service-worker keepalive, without which runs die mid-step (see
//   updateKeepAlive).
// - The request listener (see LISTENER below): always-on by design -- once
//   started, it persists across service-worker restarts and browser
//   restarts via chrome.storage, and re-arms itself on chrome.runtime
//   onStartup/onInstalled, so "listening" isn't something that quietly
//   stops just because the popup was closed or Chrome got relaunched. Each
//   incoming request gets its OWN fresh tab (never the tab the popup
//   happened to be open on when listening was turned on) so the agent never
//   hijacks whatever the user is doing elsewhere in the browser.

const SERVER = "http://127.0.0.1:8092";
const MAX_STEPS = 40;
const STEP_SETTLE_MS = 600;
const LISTENER_STORAGE_KEY = "browserAgentListener";

// Keyed by the tab that was active when the user hit Run/Record -- NOT by
// whichever tab the agent is currently acting on, since that can change
// mid-run (see onNewTabJoined) while the popup only ever knows the original.
// The request listener also lands here, keyed by the fresh tab it creates
// per request -- see runListenerLoop().
const runs = new Map();

// The request listener's state is GLOBAL, not tied to any tab -- unlike
// `runs`, there is no "primary tab" for a listener that starts itself on
// browser launch with no popup ever having been opened. `settings` holds
// the model/vision/verify/flavour/timing the listener was (re)started
// with, persisted so a restart resumes with the same configuration instead
// of silently falling back to defaults.
const LISTENER = { active: false, handled: 0, current: null, stop: false, settings: null };

function getRun(primaryTabId) {
  if (!runs.has(primaryTabId)) {
    runs.set(primaryTabId, {
      primaryTabId, tabId: primaryTabId, windowId: null, groupId: null,
      lockedTabIds: new Set(),
      log: [], steps: [], trace: [], currentStage: "",
      running: false, stop: false, paused: null,
      recording: null,
    });
  }
  return runs.get(primaryTabId);
}

function findRunByLockedTab(tabId) {
  for (const run of runs.values()) if (run.lockedTabIds.has(tabId)) return run;
  return null;
}

// Only a run that is still CAPTURING counts here. run.recording outlives
// the capture itself -- it holds the buffered steps until the user saves or
// discards them -- so `active` is what "am I recording right now" means.
function findRecordingRun(tabId) {
  for (const run of runs.values()) {
    if (run.recording?.active && (run.tabId === tabId || run.primaryTabId === tabId)) return run;
  }
  return null;
}

// ---- service worker keepalive -------------------------------------------
// An MV3 service worker is torn down after ~30s idle. A single model call on
// this hardware routinely takes longer than that, so a run would simply stop
// dead mid-step whenever the popup wasn't open to keep chatter flowing --
// which is exactly the "it randomly stops working when minimised" symptom.
// Any extension API call resets the idle timer, so ping one while work is in
// flight. Stops as soon as nothing is running, so an idle browser pays
// nothing for this.
let keepAliveTimer = null;

function updateKeepAlive() {
  // Listening counts as active: an idle listener that lets the service
  // worker die would silently stop answering requests.
  const active = LISTENER.active
    || [...runs.values()].some((r) => r.running || r.recording?.active);
  if (active && !keepAliveTimer) {
    keepAliveTimer = setInterval(() => {
      chrome.runtime.getPlatformInfo(() => void chrome.runtime.lastError);
    }, 20000);
  } else if (!active && keepAliveTimer) {
    clearInterval(keepAliveTimer);
    keepAliveTimer = null;
  }
}

// finishRun marks a run stopped, unlocks its tabs unless it is paused, and
// tells the popup.
async function finishRun(run) {
  run.running = false;
  if (!run.paused) await unlockRun(run);
  updateKeepAlive();
  chrome.runtime.sendMessage({ type: "run_finished", tabId: run.primaryTabId }).catch(() => {});
}

// runFailed marks a run that could not start (or replay) stopped, saying why.
function runFailed(run, why) {
  run.running = false;
  log(run, why);
  updateKeepAlive();
  chrome.runtime.sendMessage({ type: "run_finished", tabId: run.primaryTabId }).catch(() => {});
}

// ---- state broadcast ----------------------------------------------------
function log(run, line) {
  run.log.push(line);
  chrome.runtime.sendMessage({ type: "log", tabId: run.primaryTabId, line }).catch(() => {});
}

function stage(run, text) {
  run.currentStage = text;
  chrome.runtime.sendMessage({ type: "stage", tabId: run.primaryTabId, text }).catch(() => {});
  syncSidecar(run);
}

function upsertStep(run, index, patch) {
  run.steps[index] = { ...(run.steps[index] || { index }), ...patch };
  chrome.runtime.sendMessage({ type: "steps", tabId: run.primaryTabId, steps: run.steps }).catch(() => {});
  syncSidecar(run);
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// A model call can legitimately take a minute on this hardware, but it must
// not be able to hang forever: one task in a 104-task run sat for 406s
// against a 120s cap, because the cap is only checked between steps and a
// single stalled call never returned.
const CALL_TIMEOUT_MS = 180000;

// Chromium 112 (this desktop's base image) has no AbortSignal.any, so one
// call's own timeout and an external cancel share ONE controller instead:
// callSignal() makes a fresh one per call (a run's fetches happen one at a
// time, in sequence, so each needs its own timeout window), and tracks it
// on `run.currentAbort` for the run's whole duration - the request-listener
// loop's cancellation poll (see runListenerLoop) aborts whatever that
// currently points at, immediately, rather than waiting for CALL_TIMEOUT_MS
// or for the loop to next check `run.stop` between steps.
function callSignal(run) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(new DOMException("Timed out", "TimeoutError")), CALL_TIMEOUT_MS);
  if (run) run.currentAbort = controller;
  return {
    signal: controller.signal,
    release() {
      clearTimeout(timer);
      if (run && run.currentAbort === controller) run.currentAbort = null;
    },
  };
}

// `run` is optional and only ever needed for a call that should be
// interruptible by an external cancel (see callSignal) - /models, /stats,
// /requests/next and the rest of the listener's own bookkeeping calls don't
// belong to any one run and are left as plain timeouts.
async function getJSON(path, run = null) {
  const call = callSignal(run);
  try {
    const resp = await fetch(SERVER + path, { signal: call.signal });
    return await resp.json();
  } finally {
    call.release();
  }
}

async function postJSON(path, body, run = null) {
  const call = callSignal(run);
  try {
    const resp = await fetch(SERVER + path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: call.signal,
    });
    const data = await resp.json();
    if (!resp.ok || data.error) throw new Error(data.error || `${path} failed: ${resp.status}`);
    return data;
  } finally {
    call.release();
  }
}

// Builds a per-request live-push callback for runLoop/handleProgram's
// onStepState -- fire-and-forget (a screenshot upload hanging or failing
// must never stall the agent's own progress), so someone remote watching
// GET /requests/{id}/live sees each step as it happens instead of only the
// final bundled result.
function makeLivePusher(requestId) {
  return (entry) => {
    postJSON(`/requests/${encodeURIComponent(requestId)}/step`, entry).catch(() => {});
  };
}

const PURPOSE_LABELS = {
  atomize: "deciding the next step",
  expectation: "stating what success looks like",
  action: "choosing an action",
  verify: "checking whether it worked",
};

async function announceCall(run, modelKey, purpose) {
  let swap = false;
  try {
    swap = (await getJSON(`/will_swap?model_key=${encodeURIComponent(modelKey)}`, run)).swap;
  } catch { /* ext_server unreachable -- the call itself will surface that */ }
  const text = swap
    ? `Loading ${modelKey} into VRAM (${PURPOSE_LABELS[purpose] || purpose}) -- can take 10-30s...`
    : `Asking ${modelKey} (${PURPOSE_LABELS[purpose] || purpose})...`;
  stage(run, text);
  log(run, text);
}

// ---- in-page side-car ---------------------------------------------------
// Injected into every locked tab. One component, two modes:
//  - "run": a translucent backdrop that swallows real user input (the agent
//    itself acts through chrome.scripting, which never touches these
//    pointer events), plus a live panel showing the current stage and the
//    last few steps.
//  - "record": the panel only, NO backdrop -- the user has to be able to
//    interact, that's the entire point of recording.
// The panel is always interactive, so Stop works whether or not the popup
// is open.
function sidecarRender(state) {
  const ID = "__ba_sidecar__";
  const BACKDROP = "__ba_backdrop__";

  let backdrop = document.getElementById(BACKDROP);
  if (state.mode === "run") {
    if (!backdrop) {
      backdrop = document.createElement("div");
      backdrop.id = BACKDROP;
      backdrop.style.cssText = "position:fixed;inset:0;z-index:2147483646;"
        + "background:rgba(12,14,18,.16);cursor:not-allowed;";
      const block = (e) => { e.stopPropagation(); e.preventDefault(); };
      backdrop.addEventListener("click", block, true);
      backdrop.addEventListener("mousedown", block, true);
      document.documentElement.appendChild(backdrop);
    }
  } else if (backdrop) {
    backdrop.remove();
  }

  let el = document.getElementById(ID);
  if (!el) {
    el = document.createElement("div");
    el.id = ID;
    el.style.cssText = "position:fixed;top:14px;right:14px;width:292px;max-height:70vh;"
      + "z-index:2147483647;background:rgba(22,23,27,.82);backdrop-filter:blur(10px);"
      + "-webkit-backdrop-filter:blur(10px);color:#eceef2;border:1px solid rgba(255,255,255,.12);"
      + "border-radius:12px;box-shadow:0 8px 34px rgba(0,0,0,.42);"
      + "font:12px/1.45 -apple-system,'Segoe UI',Roboto,sans-serif;display:flex;"
      + "flex-direction:column;overflow:hidden;cursor:default;";
    el.innerHTML =
      '<div style="display:flex;align-items:center;gap:8px;padding:9px 10px 9px 12px;'
      + 'border-bottom:1px solid rgba(255,255,255,.1)">'
      + '<span data-el="dot" style="width:8px;height:8px;border-radius:50%;flex:none"></span>'
      + '<span data-el="title" style="font-weight:600;flex:1;white-space:nowrap;overflow:hidden;'
      + 'text-overflow:ellipsis"></span>'
      + '<button data-el="stop" style="background:#e03131;color:#fff;border:none;border-radius:6px;'
      + 'padding:4px 10px;font:600 11px inherit;cursor:pointer">Stop</button></div>'
      + '<div data-el="stage" style="padding:8px 12px;color:#9fb4ff;'
      + 'border-bottom:1px solid rgba(255,255,255,.07)"></div>'
      + '<div data-el="list" style="padding:6px 8px 8px;overflow-y:auto"></div>';
    // Bubble phase, NOT capture: these exist to stop a panel click from
    // reaching the page's own document-level handlers, but a capture-phase
    // stopPropagation() here would kill the event before it ever reached the
    // Stop button inside this very panel.
    el.addEventListener("click", (e) => e.stopPropagation());
    el.addEventListener("mousedown", (e) => e.stopPropagation());
    el.querySelector('[data-el="stop"]').addEventListener("click", () => {
      chrome.runtime.sendMessage({
        type: el.dataset.mode === "record" ? "stopRecordingFromPage" : "stopByOverlay",
      });
    });
    document.documentElement.appendChild(el);
  }
  el.dataset.mode = state.mode;

  const recording = state.mode === "record";
  const count = state.count || 0;
  el.querySelector('[data-el="dot"]').style.background = recording ? "#e03131" : "#6f8bff";
  el.querySelector('[data-el="title"]').textContent = recording ? "Recording your actions" : "Agent is working";
  el.querySelector('[data-el="stop"]').textContent = recording ? "Stop recording" : "Stop";
  el.querySelector('[data-el="stage"]').textContent = recording
    ? `${count} action${count === 1 ? "" : "s"} captured`
    : (state.stage || "");

  const colors = { ok: "#51cf66", running: "#6f8bff", failed: "#ff6b6b", not_met: "#ffa94d" };
  const list = el.querySelector('[data-el="list"]');
  list.textContent = "";
  for (const s of state.steps || []) {
    const row = document.createElement("div");
    row.style.cssText = "display:flex;gap:7px;padding:3px 4px;align-items:baseline";
    const dot = document.createElement("span");
    dot.style.cssText = "width:6px;height:6px;border-radius:50%;flex:none;margin-top:5px;background:"
      + (colors[s.status] || "#8b8f99");
    const txt = document.createElement("span");
    txt.style.cssText = "flex:1;color:" + (s.status === "running" ? "#eceef2" : "#b9bec9");
    txt.textContent = `${s.index + 1}. ${s.instruction || ""}`;
    row.appendChild(dot);
    row.appendChild(txt);
    list.appendChild(row);
  }
  list.scrollTop = list.scrollHeight;
}

function sidecarRemove() {
  document.getElementById("__ba_sidecar__")?.remove();
  document.getElementById("__ba_backdrop__")?.remove();
}

function sidecarState(run, mode) {
  return {
    mode,
    stage: run.currentStage || "",
    count: run.recording ? run.recording.steps.length : 0,
    steps: run.steps.slice(-6).map((s) => ({ index: s.index, instruction: s.instruction, status: s.status })),
  };
}

// Fire-and-forget: a sidecar refresh must never be able to fail a run.
function syncSidecar(run, mode = "run") {
  const payload = sidecarState(run, mode);
  const targets = mode === "record" ? [run.tabId] : [...run.lockedTabIds];
  for (const tabId of targets) {
    chrome.scripting.executeScript({ target: { tabId }, func: sidecarRender, args: [payload] }).catch(() => {});
  }
}

async function lockTab(run, tabId, mode = "run") {
  try {
    await chrome.scripting.executeScript({
      target: { tabId }, func: sidecarRender, args: [sidecarState(run, mode)],
    });
  } catch { /* chrome:// page or mid-navigation -- not injectable, not fatal */ }
}

async function unlockTab(tabId) {
  try {
    await chrome.scripting.executeScript({ target: { tabId }, func: sidecarRemove });
  } catch { /* tab may already be closed */ }
}

async function unlockRun(run) {
  for (const tabId of run.lockedTabIds) await unlockTab(tabId);
  run.lockedTabIds.clear();
}

// Runs inside the page (chrome.scripting), so it is self-contained.
// liveFields mirrors worker.py's get_live_field_values: outerHTML is the
// DOM's ORIGINAL markup and never reflects a later .value set via typing
// or JS, so without this verify_outcome sees a just-typed field as empty.
// The side-car is stripped out too -- it's our own UI, not page content,
// and letting the model see it just wastes context and invites it to
// try clicking our own Stop button.
// A secret field (a password, a one-time code, a card number) is never read
// out: it says it is filled, not what it holds. This snapshot goes to the
// model, the step trace, TheBot's result and the live viewer.
function snapshotPage() {
  const SECRET_AUTOCOMPLETE = /(^|\s)(current-password|new-password|one-time-code|cc-number|cc-csc)(\s|$)/i;
  const isSecret = (e) => e.tagName === "INPUT"
    && (String(e.type || "").toLowerCase() === "password"
      || SECRET_AUTOCOMPLETE.test(e.getAttribute("autocomplete") || ""));
  // Clone, then drop our own UI from the clone -- regex-stripping the
  // serialized string can't reliably find the matching close tag, and
  // removing the nodes from the live DOM would make the panel flicker.
  const clone = document.documentElement.cloneNode(true);
  clone.querySelector("#__ba_sidecar__")?.remove();
  clone.querySelector("#__ba_backdrop__")?.remove();
  for (const f of clone.querySelectorAll("input")) {
    if (isSecret(f) && f.hasAttribute("value")) f.setAttribute("value", "(hidden)");
  }
  const liveFields = Array.from(document.querySelectorAll("input, textarea, select"))
    .slice(0, 30)
    .map((e) => {
      const val = e.tagName === "SELECT"
        ? (e.selectedOptions[0] ? e.selectedOptions[0].textContent.trim() : "")
        : e.value;
      if (!val) return null;
      return { tag: e.tagName.toLowerCase(), id: e.id, name: e.name, value: isSecret(e) ? "(filled - hidden)" : val };
    })
    .filter(Boolean);
  // The rendered text, separately from the markup. A big article cleans
  // down to ~100k chars of link-dense HTML, and asking a small model to
  // find one fact in the middle of that is a needle-in-haystack job it
  // reliably loses -- the same fact sits near the top of the prose in a
  // few hundred characters.
  const visibleText = (document.body?.innerText || "")
    .replace(/\n{3,}/g, "\n\n").trim().slice(0, 6000);
  return {
    url: location.href,
    title: document.title,
    html: clone.outerHTML,
    visibleText,
    liveFields,
  };
}

// withScreenshot is off by default on purpose: the screenshot is only ever
// sent when vision mode is on, but this used to grab one on EVERY snapshot
// regardless -- wasted work on every step, and enough captureVisibleTab
// calls to trip Chrome's ~2/second quota and kill a task outright once
// tasks ran back to back in a batch.
async function captureSnapshot(run, withScreenshot = false) {
  const [{ result }] = await chrome.scripting.executeScript({
    target: { tabId: run.tabId },
    func: snapshotPage,
  });
  if (!withScreenshot) return { ...result, screenshot_b64: null };

  // windowId is pinned to the tab this run started on -- captureVisibleTab
  // with no windowId grabs whatever window has focus, which silently
  // screenshots the wrong tab if the user switches away mid-run.
  const grab = async () => {
    const dataUrl = await chrome.tabs.captureVisibleTab(run.windowId, { format: "jpeg", quality: 70 });
    return dataUrl.split(",")[1];
  };
  let screenshot_b64 = null;
  try {
    screenshot_b64 = await grab();
  } catch {
    // Quota (~2/sec) or a tab that isn't visible right now. A screenshot is
    // an optional input -- back off once, then carry on without it rather
    // than failing the whole step over it.
    await sleep(700);
    try { screenshot_b64 = await grab(); } catch { /* proceed text-only */ }
  }
  return { ...result, screenshot_b64 };
}

// The state record attached to each run.trace entry -- image + UI, per the
// caller's request to return the browser's actual state alongside each
// step, not just the instruction/action text. html is capped: a page can
// clean down to several hundred KB, and this runs once per step for the
// life of the task, so an uncapped copy compounds fast over a long run.
const TRACE_HTML_MAX_CHARS = 60_000;

function stateOf(snap) {
  const html = snap.html || "";
  return {
    url: snap.url,
    title: snap.title,
    screenshot_b64: snap.screenshot_b64 || null,
    visible_text: snap.visibleText || "",
    html: html.length > TRACE_HTML_MAX_CHARS
      ? html.slice(0, TRACE_HTML_MAX_CHARS) + "\n...(truncated)"
      : html,
  };
}

// Records one step in run.trace AND, if the caller supplied a live-push
// callback (see runListenerLoop), forwards it immediately -- POSTed to the
// server per-step rather than only ever appearing once the whole task
// finishes, so someone remote who isn't the one driving the browser still
// gets to watch it happen turn by turn instead of getting one big dump at
// the end.
function pushTrace(run, onStepState, entry) {
  const full = { time: Date.now(), ...entry };
  run.trace.push(full);
  if (onStepState) {
    try { onStepState(full); } catch { /* a broken live viewer must never take the run down */ }
  }
}

function waitForTabLoad(tabId, timeout = 15000) {
  return new Promise((resolve) => {
    const timer = setTimeout(finish, timeout);
    function finish() {
      chrome.tabs.onUpdated.removeListener(listener);
      resolve();
    }
    function listener(id, info) {
      if (id === tabId && info.status === "complete") {
        clearTimeout(timer);
        finish();
      }
    }
    chrome.tabs.onUpdated.addListener(listener);
  });
}

function normalizeUrl(url) {
  // Only a bare domain gets a scheme bolted on. Anything that already has
  // one is left alone -- a recorded navigation can legitimately be file://,
  // and blindly prefixing https:// turned those into "https://file:///..."
  // on replay, which lands the tab on a chrome-error page.
  return /^[a-z][a-z0-9+.-]*:/i.test(url) ? url : "https://" + url;
}

// ---- tab-group tracking --------------------------------------------------
chrome.tabs.onCreated.addListener((tab) => {
  if (tab.openerTabId == null) return;
  const run = findRunByLockedTab(tab.openerTabId);
  if (!run || !(run.running || run.paused)) return;
  onNewTabJoined(run, tab.id);
});

async function ensureGroup(run) {
  if (run.groupId != null) return;
  try {
    run.groupId = await chrome.tabs.group({ tabIds: [run.primaryTabId] });
    await chrome.tabGroups.update(run.groupId, { title: "\u{1F916} Agent", color: "blue" });
  } catch (e) {
    log(run, `Could not create a tab group: ${e.message || e}`);
  }
}

async function onNewTabJoined(run, tabId) {
  if (run.lockedTabIds.has(tabId)) return; // already handled -- execAction calls this directly too
  run.lockedTabIds.add(tabId);
  await lockTab(run, tabId);
  await ensureGroup(run);
  if (run.groupId != null) {
    try {
      await chrome.tabs.group({ tabIds: [tabId], groupId: run.groupId });
    } catch (e) {
      log(run, `Could not add new tab to the group: ${e.message || e}`);
    }
  }

  log(run, `New tab opened (id ${tabId}) -- waiting to see where it goes...`);
  await waitForTabLoad(tabId, 8000);
  await lockTab(run, tabId); // re-apply -- the navigation just wiped the first injection
  const t = await chrome.tabs.get(tabId).catch(() => null);
  const loadedReal = t && t.url && t.url !== "about:blank"
    && !t.url.startsWith("chrome://") && !t.url.startsWith("chrome-error://");
  if (loadedReal) {
    log(run, `New tab loaded ${t.url} -- continuing the work there.`);
    run.tabId = tabId;
  } else {
    log(run, "New tab never loaded real content -- staying on the current tab.");
  }
}

// ---- fixed, safe DOM action execution -----------------------------------
// The model never supplies code to eval, only a selector/text/direction from
// ext_codegen.py's small grammar (or a recorded selector on replay), so a
// page's own content can never get more than "which element gets clicked or
// typed into" -- it can't make the extension run arbitrary script.
function domAction(act, approved = []) {
  // The approval gate, with agent/decurix_agent/computer/approval.py's rules
  // (keep them in step): a click on a control whose own label commits the
  // person - sends, pays, buys, publishes, closes an account - and Enter in a
  // messaging page outside a search box are not carried out unless a person
  // approved exactly that action for this request. A yes is one action's.
  // Checked before anything on the page is touched. Self-contained: this
  // function runs inside the page.
  const CONSEQUENTIAL = new RegExp(
    "^\\s*("
    + "send|send now|send (it|message|email|e-mail|mail|newsletter|invite|invitation|reply|sms|text|money|payment)"
    + "|pay|pay now|make (a )?payment|submit payment|authori[sz]e payment"
    + "|confirm (and pay|payment|purchase|order|transfer|booking)"
    + "|buy|buy now|purchase|place (your |my )?order|complete (order|purchase|payment|checkout)"
    + "|checkout|check out(?=\\s*(now)?\\s*$)|proceed to (checkout|payment)"
    + "|transfer( money| funds| now)?|donate|withdraw"
    + "|publish|post|post now|tweet|reply all"
    + "|delete (my )?account|close (my )?account|cancel (my )?(subscription|membership|account|order)"
    + "|book now|reserve now|subscribe( now)?"
    + ")\\b", "i");
  const MESSAGING = new RegExp(
    "(whatsapp|telegram|messenger|slack\\.com|\\bslack\\b|discord|signal|teams\\.microsoft|"
    + "mail\\.google|gmail|outlook|mail\\.yahoo|proton\\.me|protonmail|thunderbird|"
    + "\\bcompose\\b|new message|linkedin\\.com/messaging|twitter\\.com|x\\.com/)", "i");
  const SEARCH_FIELD = /^\s*(search|find|filter|look up)\b/i;
  const SECRET_AUTOCOMPLETE = /(^|\s)(current-password|new-password|one-time-code|cc-number|cc-csc)(\s|$)/i;
  const isSecret = (e) => e.tagName === "INPUT"
    && (String(e.type || "").toLowerCase() === "password"
      || SECRET_AUTOCOMPLETE.test(e.getAttribute("autocomplete") || ""));
  const ACTIVATING = "button, a[href], summary, input[type=submit], input[type=button], [role=button], [role=link], [role=menuitem], [role=tab]";
  const labelOf = (n) => String(n.getAttribute("aria-label") || n.innerText || n.value || n.title || "")
    .replace(/\s+/g, " ").trim().slice(0, 60);
  const isSearch = (f) => f.type === "search" || f.getAttribute("role") === "searchbox"
    || SEARCH_FIELD.test(f.getAttribute("aria-label") || f.placeholder || f.name || "");
  function gate(action, reason) {
    return approved.includes(action) ? { usedApproval: action } : { success: false, needsApproval: { action, reason }, error: `stopped before ${action} - ${reason}; a person has to approve it` };
  }
  function clickGate(el) {
    const control = el.closest(ACTIVATING);
    const label = control ? labelOf(control) : "";
    if (!control || !CONSEQUENTIAL.test(label)) return null;
    return gate(`click "${label}" on ${location.host}`, `"${label}" sends, pays, buys or publishes something on your behalf`);
  }
  // Submitting a form is clicking its submit button, and goes through the
  // same check: a search typed into a checkout form whose button is "Pay now"
  // is a payment.
  function submitGate(form) {
    const submitter = Array.from(form.elements || []).find((e) => String(e.type || "").toLowerCase() === "submit");
    return submitter ? clickGate(submitter) : null;
  }
  function enterGate(field) {
    if (isSearch(field) || !MESSAGING.test(`${location.href} ${document.title}`)) return null;
    const name = String(field.getAttribute("aria-label") || field.placeholder || field.name || "the message box").trim().slice(0, 60);
    return gate(`press Enter in "${name}" on ${location.host}`,
      "pressing Enter here sends what was typed as a message");
  }

  // Pages repeat the same control per item (an "Add to cart" per product, a
  // "Download" per row). Positional selectors get this wrong constantly, so
  // an action may carry near="<the item's own text>": among all matches,
  // take the one whose own text or enclosing item mentions it. If near= was
  // given and nothing matches, fail loudly rather than silently acting on
  // the wrong item -- clicking the wrong product is worse than stopping.
  function pick(act) {
    const nodes = Array.from(document.querySelectorAll(act.selector));
    if (!nodes.length) return { el: null, error: `no element matches selector: ${act.selector}` };
    if (!act.near) return { el: nodes[0] };
    const needle = String(act.near).toLowerCase();
    const textOf = (n) => (n.innerText || n.textContent || "").toLowerCase();

    // The control's own text is the strongest signal (a link whose text IS
    // the product name).
    for (const n of nodes) if (textOf(n).includes(needle)) return { el: n };

    // Otherwise climb to this candidate's own item container: the highest
    // ancestor that still contains ONLY this candidate. Climbing a fixed
    // number of levels instead would reach <body>, whose text contains every
    // item on the page -- which made the very first candidate match every
    // time, exactly the wrong-item bug near= exists to prevent.
    for (const n of nodes) {
      let ctx = n;
      while (ctx.parentElement && nodes.filter((o) => ctx.parentElement.contains(o)).length === 1) {
        ctx = ctx.parentElement;
      }
      if (textOf(ctx).includes(needle)) return { el: n };
    }
    return { el: null, error: `no "${act.selector}" belongs to an item matching near="${act.near}"` };
  }

  try {
    if (act.type === "click") {
      const { el, error } = pick(act);
      if (!el) return { success: false, error };
      const held = clickGate(el);
      if (held && !held.usedApproval) return held;
      el.scrollIntoView({ block: "center" });
      // A synthetic el.click() from an extension-injected script carries no
      // user activation, and Chrome silently refuses target="_blank"
      // navigation without it -- confirmed: the tab opens but sits on
      // about:blank forever. Hand it to the caller's privileged tabs API.
      const anchor = el.closest("a[target=_blank], a[target=_new]");
      if (anchor && anchor.href) return { success: true, openNewTab: anchor.href, ...held };
      el.click();
      return { success: true, ...held };
    }
    if (act.type === "select") {
      const { el, error } = pick(act);
      if (!el) return { success: false, error };
      const sel = el.tagName === "SELECT" ? el : el.querySelector("select");
      if (!sel) return { success: false, error: `"${act.selector}" is not a <select>` };
      const want = String(act.text || "").toLowerCase();
      const opt = Array.from(sel.options).find(
        (o) => (o.textContent || "").trim().toLowerCase() === want
          || (o.value || "").toLowerCase() === want
          || (o.textContent || "").trim().toLowerCase().includes(want));
      if (!opt) return { success: false, error: `no option matching "${act.text}"` };
      sel.value = opt.value;
      sel.dispatchEvent(new Event("input", { bubbles: true }));
      sel.dispatchEvent(new Event("change", { bubbles: true }));
      return { success: true };
    }
    if (act.type === "type" || act.type === "search") {
      const found = pick(act);
      if (!found.el) return { success: false, error: found.error };
      // The model often targets a wrapper -- a <form>, a search container --
      // rather than the field itself. Calling HTMLInputElement's value setter
      // on a <form> throws an opaque "Illegal invocation", which was the
      // single largest failure cause across a 104-task run. Resolve to a real
      // editable field, or say plainly that there isn't one.
      const asField = (n) => {
        if (!n) return null;
        if (n.tagName === "INPUT" || n.tagName === "TEXTAREA" || n.isContentEditable) return n;
        return n.querySelector(
          "input:not([type=hidden]):not([type=submit]):not([type=button]), textarea, [contenteditable=true]");
      };
      const el = asField(found.el);
      if (!el) {
        return { success: false, error: `"${act.selector}" is not a text field and contains none` };
      }
      const secretField = isSecret(el);
      // A recorded password step keeps no value (recorderInject): it cannot be
      // replayed, and typing nothing into the field would be a silent wrong.
      if (act.secret && !act.text) {
        return { success: false, secretField, error: "this step types a password, which a recording never keeps - type it in yourself, then carry on" };
      }
      const form = el.form || el.closest("form");
      const holds = act.type === "search" ? [enterGate(el), form ? submitGate(form) : null].filter(Boolean) : [];
      const stopped = holds.find((h) => !h.usedApproval);
      if (stopped) return { ...stopped, secretField };
      const used = holds.map((h) => h.usedApproval);
      const held = used.length ? { usedApproval: used } : null;
      el.scrollIntoView({ block: "center" });
      el.focus();
      if (el.isContentEditable && el.tagName !== "INPUT" && el.tagName !== "TEXTAREA") {
        el.textContent = act.text;
      } else {
        // Native prototype setter, so React/Vue-style controlled inputs (which
        // hook the setter) actually notice -- what Playwright's .fill() does.
        // Chosen from the element's OWN type: using the input prototype on a
        // textarea (or anything else) throws "Illegal invocation".
        const proto = el.tagName === "TEXTAREA" ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
        const setter = Object.getOwnPropertyDescriptor(proto, "value")?.set;
        if (setter) setter.call(el, act.text); else el.value = act.text;
      }
      el.dispatchEvent(new Event("input", { bubbles: true }));
      el.dispatchEvent(new Event("change", { bubbles: true }));
      if (act.type === "search") {
        // Real search boxes (DuckDuckGo, Amazon, YouTube) submit on Enter via
        // a keydown handler far more reliably than they expose a findable
        // submit button, so dispatch a genuine Enter first; requestSubmit()
        // covers the plainer case of a real <form> with no such handler.
        const opts = { key: "Enter", code: "Enter", keyCode: 13, which: 13, bubbles: true };
        el.dispatchEvent(new KeyboardEvent("keydown", opts));
        el.dispatchEvent(new KeyboardEvent("keypress", opts));
        el.dispatchEvent(new KeyboardEvent("keyup", opts));
        if (form && form.requestSubmit) form.requestSubmit();
      }
      return { success: true, secretField, ...held };
    }
    if (act.type === "scroll") {
      window.scrollBy(0, act.direction === "down" ? 800 : -800);
      return { success: true };
    }
    return { success: false, error: `unknown action type: ${act.type}` };
  } catch (e) {
    return { success: false, error: String(e) };
  }
}

async function execAction(run, action) {
  let result = null;
  if (action.type === "navigate") {
    await chrome.tabs.update(run.tabId, { url: normalizeUrl(action.url) });
    await waitForTabLoad(run.tabId);
  } else {
    [{ result }] = await chrome.scripting.executeScript({
      target: { tabId: run.tabId }, func: domAction, args: [action, run.approved || []],
    });
    await sleep(STEP_SETTLE_MS);
    if (result.needsApproval) run.needsApproval = result.needsApproval;
    if (!result.success) return result;
    // A yes lets one action through, once.
    const used = [].concat(result.usedApproval || []);
    if (used.length) run.approved = (run.approved || []).filter((a) => !used.includes(a));
    if (result.openNewTab) {
      // Called directly rather than left to the onCreated listener alone, so
      // the group/lock/switch decision is settled before the next step's
      // snapshot; onNewTabJoined guards against handling the tab twice.
      const newTab = await chrome.tabs.create({
        url: result.openNewTab, openerTabId: run.tabId, windowId: run.windowId,
      });
      await onNewTabJoined(run, newTab.id);
    }
  }
  if (run.lockedTabIds.size) await lockTab(run, run.tabId); // re-apply -- navigation wipes the DOM
  return { success: true, secretField: !!(result && result.secretField) };
}

// An action as the step record keeps it: text typed into a secret field (a
// password) is replaced, since steps and traces go to TheBot, the live viewer
// and saved runs.
function shownAction(action, result) {
  return result && result.secretField && action && typeof action.text === "string"
    ? { ...action, text: "(hidden)" } : action;
}

function pause(run, { task, remaining, modelKey, vision, verifyEnabled, error, memory, flavour, stack }) {
  run.paused = { task, remaining, modelKey, vision, verifyEnabled, lastError: error, memory, flavour, stack };
  log(run, `Paused: ${error}`);
  stage(run, `Paused -- ${String(error).split("\n")[0]}`);
}

async function runLoop(run, { task, remaining, modelKey, vision, verifyEnabled, lastError, memory,
                                flavour = "live", stack = null, deadline, onStepState = null }) {
  run.running = true;
  run.stop = false;
  run.paused = null;
  run.fatalError = null;
  run.stalled = null;
  run.blocked = null;
  // Set only where the task is genuinely finished (the model said done, or
  // answered); everything else that ends the loop says why (runOutcome).
  run.completed = false;
  run.endedWhy = null;
  updateKeepAlive();

  const useVision = vision && (await getJSON("/models"))[modelKey]?.vision;
  let repeatKey = null;
  let repeatCount = 0;
  let lastSignature = null;
  let doneNudged = false;
  let parseNudges = 0;

  try {
    for (let i = 0; i < MAX_STEPS; i++) {
      if (run.cancelledByUser) { stage(run, "Cancelled."); log(run, "Cancelled."); break; }
      if (run.stop) { run.endedWhy = "Stopped by user."; stage(run, "Stopped by user."); log(run, "Stopped by user."); break; }
      if (deadline && Date.now() > deadline) {
        stage(run, "Stopped: task time limit reached.");
        log(run, "Stopped: task time limit reached.");
        run.timedOut = true;
        break;
      }

      // Screenshot is always captured now, not just when vision feeds it to
      // the model -- this copy is for the returned record (run.trace below),
      // independent of whatever the model itself gets to see.
      stage(run, "Reading page (HTML + screenshot)...");
      const snap = await captureSnapshot(run, true);

      await announceCall(run, modelKey, "atomize");
      let stepResp;
      try {
        stepResp = await postJSON("/step", {
          full_task: task, remaining, url: snap.url, raw_html: snap.html, title: snap.title,
          model_key: modelKey, screenshot_b64: useVision ? snap.screenshot_b64 : null,
          last_error: lastError, verify_enabled: verifyEnabled, live_fields: snap.liveFields,
          visible_text: snap.visibleText, memory, flavour, stack,
        }, run);
      } catch (e) {
        // Grammar-constrained output (STEP_SCHEMA) means the model can no
        // longer emit an unparseable action line -- the one remaining
        // inconsistency it can still produce is action.type "done" without
        // remaining also saying "done" (or vice versa). Recoverable the same
        // way the old free-text parse failure was: re-ask once, bounded.
        const msg = String(e.message || e);
        if (/no usable action/i.test(msg) && parseNudges < 3) {
          parseNudges += 1;
          lastError = "Your action.type and remaining disagreed about whether the task is done -- "
            + 'if it is genuinely finished, set BOTH remaining to "done" and action.type to '
            + '"done"; otherwise pick a real next action and describe what remains.';
          log(run, "Inconsistent done/remaining -- re-asking once.");
          stage(run, "Reminding the model to keep done/remaining consistent...");
          continue;
        }
        throw e;
      }
      if (stepResp.memory !== undefined) memory = stepResp.memory;
      if (stepResp.evaluation_previous_goal) {
        log(run, `  self-check: ${stepResp.evaluation_previous_goal}`);
      }
      if (stepResp.planned) {
        // Only present on the very first /step call of a stack-flavour run
        // (the sentinel for "not planned yet" was stack === null) -- the
        // upfront plan, decided once, before any step executes.
        log(run, `Plan: ${stepResp.planned.map((s, idx) => `${idx + 1}. ${s}`).join("  ")}`);
      }
      if (flavour === "stack" && stepResp.stack !== undefined) stack = stepResp.stack;

      if (stepResp.done) {
        // "Done" having done nothing is not done. The model short-circuits
        // to DONE on the first turn for questions it should have ANSWERed
        // and for searches it never ran -- and reporting that as success is
        // worse than failing, because it hides the failure. Re-ask once with
        // the mistake named, then stop honestly if it repeats.
        const didSomething = run.steps.filter(Boolean).some((s) => s.status === "ok");
        if (!stepResp.answer && !didSomething) {
          if (!doneNudged) {
            doneNudged = true;
            lastError = "You replied DONE but nothing has been done yet and no answer was given. "
              + "If the task asks for information, reply with ACTION: ANSWER(\"...\"). "
              + "Otherwise carry out the first real step of the task.";
            log(run, "Model claimed done before doing anything -- re-asking once.");
            stage(run, "Model claimed done too early -- re-asking...");
            continue;
          }
          const msg = "Stopped: model declared the task done without doing anything or answering.";
          stage(run, msg);
          log(run, msg);
          run.paused = { task, remaining, modelKey, vision, verifyEnabled, lastError: msg, memory, flavour, stack };
          break;
        }
        if (stepResp.answer) {
          // A question answered off the page: the answer IS the result, so
          // surface it as a step rather than letting it vanish into a log.
          run.answer = stepResp.answer;
          upsertStep(run, i, {
            instruction: `Answer: ${stepResp.answer}`,
            action: { type: "answer", text: stepResp.answer }, status: "ok",
          });
          stage(run, `Answer: ${stepResp.answer}`);
          log(run, `Answer: ${stepResp.answer}`);
        } else {
          stage(run, "Task complete.");
          log(run, "Task complete.");
        }
        run.completed = true;
        break;
      }
      if (stepResp.captcha) {
        // Record it, don't just log it: a captcha halt that set no flag was
        // reported as a clean "done" -- the same failure-wearing-a-success
        // label as a thrown error.
        run.blocked = stepResp.captcha;
        stage(run, "Halted -- captcha / bot check");
        log(run, `Halted -- captcha detected: ${stepResp.captcha}`);
        break;
      }

      // Deterministic runaway guard. The model repeatedly re-issues an
      // identical action when that action changes nothing it can see -- a
      // 104-task run had four tasks burn 40/40/31/17 steps clicking the same
      // element. Prompting has not reliably fixed this, so stop on the
      // evidence instead of hoping.
      const actionKey = JSON.stringify(stepResp.action);
      // Signature of what the page actually looks like now. A repeated
      // action on a page that did not change is a no-op by definition, so
      // one repeat is already enough evidence -- no need to wait for three.
      const signature = `${snap.url}|${(snap.visibleText || "").length}|${(snap.visibleText || "").slice(0, 120)}`;
      const pageUnchanged = signature === lastSignature;
      lastSignature = signature;
      if (actionKey === repeatKey) {
        repeatCount += 1;
        if (pageUnchanged) {
          const msg = "Stopped: repeated the same action and the page did not change.";
          stage(run, msg);
          log(run, `${msg} Action: ${actionKey}`);
          // Distinguish "did nothing and got stuck" from "did the work, then
          // couldn't tell it had". A SELECT that lands correctly leaves no
          // visible trace, so the model repeats it -- calling that a failure
          // is as wrong as calling it a success.
          if (run.steps.filter(Boolean).some((s) => s.status === "ok")) {
            run.stalled = msg;
          } else {
            run.paused = { task, remaining, modelKey, vision, verifyEnabled, lastError: msg, memory, flavour, stack };
          }
          break;
        }
        if (repeatCount >= 3) {
          const msg = "Stopped: the same action repeated 3 times without changing the page.";
          run.endedWhy = msg;
          stage(run, msg);
          log(run, `${msg} Last action: ${actionKey}`);
          break;
        }
      } else {
        repeatKey = actionKey;
        repeatCount = 1;
      }

      upsertStep(run, i, {
        instruction: stepResp.next_instruction, expectation: stepResp.expectation,
        action: stepResp.action, status: "running",
      });
      log(run, `Step ${i + 1}: ${stepResp.next_instruction}`);

      stage(run, "Running action in the browser...");
      const result = await execAction(run, stepResp.action);
      const action = shownAction(stepResp.action, result);
      if (action !== stepResp.action) upsertStep(run, i, { action });
      if (!result.success) {
        upsertStep(run, i, { status: "failed", error: result.error });
        // No fresh capture on a failed action -- the page may be mid-crash
        // or mid-navigation, and snap (taken just before the action) is
        // still informative about the state the action actually acted on.
        pushTrace(run, onStepState, {
          index: i, instruction: stepResp.next_instruction, action,
          result: "failed", error: result.error, state: stateOf(snap),
        });
        pause(run, { task, remaining, modelKey, vision, verifyEnabled, error: result.error, memory, flavour, stack });
        break;
      }

      // Always captured now (not just when verify needs it) -- this is what
      // "return the browser's state, image and UI" per step means: the
      // result of the action just taken, reused below for verify so this
      // never runs twice for the same step.
      stage(run, "Capturing resulting page state...");
      const afterSnap = await captureSnapshot(run, true);
      const stepState = stateOf(afterSnap);

      let verifyResp = null;
      if (verifyEnabled && stepResp.expectation) {
        await announceCall(run, modelKey, "verify");
        verifyResp = await postJSON("/verify", {
          expectation: stepResp.expectation, url: afterSnap.url, raw_html: afterSnap.html,
          title: afterSnap.title, model_key: modelKey, live_fields: afterSnap.liveFields,
          screenshot_b64: useVision ? afterSnap.screenshot_b64 : null,
        }, run);
        log(run, `  verify: ${verifyResp.met ? "MET" : "NOT_MET"} ${verifyResp.reason || ""}`);
        if (!verifyResp.met) {
          upsertStep(run, i, { status: "not_met", verify: verifyResp });
          pushTrace(run, onStepState, {
            index: i, instruction: stepResp.next_instruction, action,
            result: "not_met", verify: verifyResp, state: stepState,
          });
          pause(run, {
            task, remaining, modelKey, vision, verifyEnabled, memory, flavour, stack,
            error: `Expected: ${stepResp.expectation}\nBut: ${verifyResp.reason}`,
          });
          break;
        }
      }

      upsertStep(run, i, { status: "ok", verify: verifyResp });
      pushTrace(run, onStepState, {
        index: i, instruction: stepResp.next_instruction, action,
        result: "ok", verify: verifyResp, state: stepState,
      });

      if (flavour === "stack") {
        // /step's contract: the returned stack still has the just-completed
        // target at the front (not yet popped, since /step ran before this
        // action was executed/verified) -- pop it now that it's actually done.
        stack = stack.slice(1);
      } else {
        remaining = stepResp.remaining;
      }
      lastError = null;

      if (i === MAX_STEPS - 1) {
        run.endedWhy = "Stopped: reached the max-steps safety cap.";
        stage(run, "Stopped: reached the max-steps safety cap.");
        log(run, "Stopped: reached the max-steps safety cap.");
      }
    }
  } catch (exc) {
    // Record it on the run, not just in the log: the caller decides a run's
    // outcome from these flags, and an error that set neither of them was
    // being reported as a clean "done" -- a failure wearing a success label.
    run.fatalError = String(exc.message || exc);
    log(run, `Fatal error: ${run.fatalError}`);
    stage(run, `Fatal error: ${run.fatalError}`);
  } finally {
    await finishRun(run);
  }
}

async function startRun(tabId, { task, modelKey, vision, verifyEnabled, flavour = "live" }) {
  const run = getRun(tabId);
  if (run.recording) throw new Error("Stop recording before starting an agent run.");
  run.tabId = tabId;
  run.log = [];
  run.steps = [];
  run.answer = null;
  run.trace = [];
  run.approved = [];
  run.needsApproval = null;
  run.currentStage = "Starting...";
  const tab = await chrome.tabs.get(tabId);
  run.windowId = tab.windowId;
  run.lockedTabIds.add(tabId);
  await lockTab(run, tabId);
  await runLoop(run, {
    task, remaining: task, modelKey, vision, verifyEnabled, lastError: null, memory: "",
    flavour, stack: null,
  });
}

async function retryRun(tabId) {
  const run = getRun(tabId);
  if (!run.paused) return;
  const { task, remaining, modelKey, vision, verifyEnabled, lastError, memory, flavour, stack } = run.paused;
  // Retry on a run the approval gate stopped is the person's yes to that one action.
  run.approved = run.needsApproval ? [run.needsApproval.action] : [];
  run.needsApproval = null;
  const tab = await chrome.tabs.get(run.tabId);
  run.windowId = tab.windowId;
  run.lockedTabIds.add(run.tabId);
  await lockTab(run, run.tabId);
  await runLoop(run, {
    task, remaining, modelKey, vision, verifyEnabled, lastError, memory: memory || "",
    flavour: flavour || "live", stack: stack ?? null,
  });
}

async function stopRun(tabId) {
  const run = runs.get(tabId);
  if (!run) return;
  run.stop = true;
  if (run.paused) { run.paused = null; await unlockRun(run); }
  if (!run.running) { await unlockRun(run); updateKeepAlive(); }
}

// ---- request listener -----------------------------------------------------
// While listening, the agent waits on ext_server.py's /requests inbox and
// carries out whatever arrives, in the user's OWN browser with their real
// sessions. A request is either an INSTRUCTION (natural language, driven by
// the model loop) or a PROGRAM (a saved recording, replayed deterministically
// with no model call at all). Anything that can POST -- a script, a CLI,
// another app -- can hand the agent work this way.
async function evaluateCheck(run, check) {
  if (!check || !check.type) return null;
  if (check.type === "any_tab_url_contains") {
    for (const tabId of [...run.lockedTabIds, run.tabId]) {
      const t = await chrome.tabs.get(tabId).catch(() => null);
      if (t && (t.url || "").toLowerCase().includes(String(check.value).toLowerCase())) return true;
    }
    return false;
  }
  try {
    const [{ result }] = await chrome.scripting.executeScript({
      target: { tabId: run.tabId },
      // Declarative, never eval'd: MV3's CSP blocks eval in injected code,
      // and a check expression from outside has no business being executed
      // as code in the user's page anyway.
      func: (c) => {
        const needle = String(c.value || "").toLowerCase();
        if (c.type === "url_contains") return location.href.toLowerCase().includes(needle);
        if (c.type === "title_contains") return document.title.toLowerCase().includes(needle);
        if (c.type === "body_contains") return (document.body.innerText || "").toLowerCase().includes(needle);
        if (c.type === "field_has_value") {
          const el = document.querySelector(c.selector);
          return !!el && String(el.value || "").toLowerCase().includes(needle);
        }
        return null;
      },
      args: [check],
    });
    return result;
  } catch {
    return null;
  }
}

// Leave the browser as we found it between tasks: close whatever tabs the
// task opened, drop the group, unlock everything.
async function resetAfterTask(run) {
  for (const tabId of [...run.lockedTabIds]) {
    if (tabId === run.primaryTabId) continue;
    await chrome.tabs.remove(tabId).catch(() => {});
  }
  await unlockRun(run);
  if (run.groupId != null) {
    await chrome.tabs.ungroup([run.primaryTabId]).catch(() => {});
    run.groupId = null;
  }
  run.tabId = run.primaryTabId;
}

function broadcastListener() {
  // Global, unlike log()/stage()/upsertStep() -- there is no single tab a
  // listener event belongs to, so popup.js's handler for this message type
  // doesn't filter by tabId (see its "listener" case).
  chrome.runtime.sendMessage({ type: "listener", listener: LISTENER }).catch(() => {});
}

async function persistListenerState() {
  await chrome.storage.local.set({
    [LISTENER_STORAGE_KEY]: { enabled: LISTENER.active, settings: LISTENER.settings },
  }).catch(() => {});
}

// A PROGRAM request: replay a saved recording, looked up by name or id.
async function handleProgram(run, req, deadline, onStepState = null) {
  const { savedRecordings = [] } = await chrome.storage.local.get("savedRecordings");
  const wanted = String(req.program || "").toLowerCase();
  const rec = savedRecordings.find((r) => r.id === req.program)
    || savedRecordings.find((r) => (r.name || "").toLowerCase() === wanted)
    || savedRecordings.find((r) => (r.name || "").toLowerCase().includes(wanted));
  if (!rec) throw new Error(`no saved program matching "${req.program}"`);

  log(run, `Replaying program "${rec.name}" (${rec.steps.length} steps)`);
  run.completed = false;
  for (let i = 0; i < rec.steps.length; i++) {
    if (run.stop || LISTENER.stop) { run.endedWhy = "Program stopped."; log(run, "Program stopped."); return; }
    if (deadline && Date.now() > deadline) { run.timedOut = true; return; }
    const step = rec.steps[i];
    const label = step.type === "navigate" ? step.url : (step.label || step.selector);
    upsertStep(run, i, { instruction: `${step.type}: ${label}`, action: step, status: "running" });
    stage(run, `Program step ${i + 1}/${rec.steps.length}`);
    const result = await execAction(run, step);
    if (!result.success) {
      upsertStep(run, i, { status: "failed", error: result.error });
      if (result.needsApproval) { log(run, `Stopped: ${result.error}`); return; }
      throw new Error(`program step ${i + 1} failed: ${result.error}`);
    }
    upsertStep(run, i, { status: "ok" });
    const afterSnap = await captureSnapshot(run, true);
    pushTrace(run, onStepState, {
      index: i, instruction: `${step.type}: ${label}`, action: shownAction(step, result),
      result: "ok", state: stateOf(afterSnap),
    });
  }
  run.completed = true;
}

// What a finished request reports. Only a run that reached its end - the
// model said the task was done (or answered it), or a program ran every
// step - is "done"; anything else says what stopped it. The default is NOT
// done: a run that hit the step cap, repeated itself or was stopped used to
// be reported as a success.
function runOutcome(run, kind) {
  if (!run) return { status: "error", error: "the request never started" };
  if (run.cancelledByUser) return { status: "cancelled", error: "Cancelled by request." };
  if (run.needsApproval) return { status: "needs_approval", error: run.needsApproval.reason };
  if (run.blocked) return { status: "blocked", error: run.blocked };
  if (run.fatalError) return { status: "error", error: run.fatalError };
  if (run.timedOut) return { status: "timeout", error: null };
  if (run.paused) return { status: "paused", error: run.paused.lastError };
  if (run.stalled) return { status: "stalled", error: run.stalled };
  if (run.completed === true) return { status: "done", error: null };
  return {
    status: "incomplete",
    error: run.endedWhy || (kind === "program" ? "the program did not run to its end" : "the run ended before the task was finished"),
  };
}

// Starts (or no-ops if already running) the always-on request listener.
// Persists `settings` so a service-worker/browser restart can resume with
// the same configuration -- see resumeListenerIfEnabled(), wired to
// chrome.runtime.onStartup/onInstalled below.
async function startListener(settings) {
  if (LISTENER.active) return;
  LISTENER.active = true;
  LISTENER.handled = 0;
  LISTENER.current = null;
  LISTENER.stop = false;
  LISTENER.settings = settings;
  updateKeepAlive();
  await persistListenerState();
  broadcastListener();

  runListenerLoop(settings).catch((e) => {
    LISTENER.active = false;
    LISTENER.current = null;
    persistListenerState();
    updateKeepAlive();
    broadcastListener();
    console.error("browser-agent listener crashed:", e);
  });
}

function stopListener() {
  LISTENER.stop = true;
}

async function runListenerLoop({ modelKey, vision, verifyEnabled, flavour = "live", perTaskSeconds, pollMs }) {
  // 900s (15min) default, not 120s -- Devstral on this hardware measured at
  // ~90-180s per step (one atomize/decide call plus the action itself), so
  // 120s wasn't even enough for a two-step task; confirmed live when a real
  // 3-step task got cut off by the old default after step 2.
  const capMs = (perTaskSeconds || 900) * 1000;
  const idleMs = pollMs || 2000;

  try {
    while (!LISTENER.stop) {
      let req = null;
      try {
        req = (await getJSON("/requests/next")).request;
      } catch (e) {
        // The server going away is not a reason to stop listening -- it may
        // simply be restarting. Back off and keep waiting.
        await sleep(Math.max(idleMs, 5000));
        continue;
      }
      if (!req) {
        await sleep(idleMs);
        continue;
      }

      LISTENER.current = req;
      broadcastListener();

      const started = Date.now();
      let status = "error"; // never "done" unless runOutcome says so
      let error = null;
      let run = null;

      try {
        // Every request gets its OWN fresh tab -- never the tab that
        // happened to be active when listening was (re)started (which,
        // for a listener resumed automatically on browser launch, may not
        // even exist), and never something the user is using for anything
        // else. getRun(newTab.id) makes this tab that run's primaryTabId,
        // so resetAfterTask() below leaves it open (just unlocked) and
        // only closes whatever secondary tabs the task itself opened.
        // "about:blank" is NOT a valid default here even though it sounds
        // like the neutral choice: chrome.scripting.executeScript can't
        // inject into it regardless of <all_urls> host permissions (that
        // only covers http(s)/file/ftp), so captureSnapshot's very first
        // call would fail before the agent ever got to run its own
        // navigate action. A real page is required to land on first.
        const newTab = await chrome.tabs.create({
          url: req.start_url || "https://www.google.com/", active: true,
        });
        run = getRun(newTab.id);
        run.windowId = newTab.windowId;
        run.tabId = newTab.id;
        run.lockedTabIds.add(newTab.id);
        run.log = [];
        run.steps = [];
        run.answer = null;
        run.trace = [];
        run.paused = null;
        run.timedOut = false;
        run.cancelledByUser = false;
        run.approved = Array.isArray(req.approved) ? [...req.approved] : [];
        run.needsApproval = null;

        const label = req.kind === "program" ? `program "${req.program}"` : req.task;
        log(run, `── Request ${req.id} [${req.kind}]: ${label}`);
        if (req.start_url) await waitForTabLoad(newTab.id);
        await lockTab(run, newTab.id);
        // A tab group only forms if the task itself opens a second tab (see
        // onNewTabJoined) -- not proactively here, so a listener handling
        // many single-tab requests back to back doesn't leave a trail of
        // throwaway one-tab "Agent" groups behind it.

        const pushLiveStep = makeLivePusher(req.id);

        // Polled independent of the step loop's own pacing, so a cancel
        // (POST /requests/{id}/cancel -- TheBot's browser skill calls this
        // when its own job is cancelled) interrupts a slow model call
        // immediately (run.currentAbort, set by callSignal) rather than
        // waiting for it to finish or time out, and still catches a run
        // between steps if nothing happened to be in flight. Cleared in the
        // finally below the instant this request is done, one way or another.
        const cancelPoll = setInterval(async () => {
          try {
            const { cancelled } = await getJSON(`/requests/${encodeURIComponent(req.id)}/cancelled`);
            if (cancelled && !run.cancelledByUser) {
              run.cancelledByUser = true;
              run.stop = true;
              log(run, "Cancelled by request.");
              stage(run, "Cancelling...");
              run.currentAbort?.abort(new DOMException("Cancelled", "AbortError"));
            }
          } catch { /* ext_server hiccup -- try again next tick */ }
        }, 1000);

        try {
          if (req.kind === "program") {
            run.running = true;
            try {
              await handleProgram(run, req, started + capMs, pushLiveStep);
            } finally {
              run.running = false;
            }
          } else {
            await runLoop(run, {
              task: req.task, remaining: req.task, modelKey, vision, verifyEnabled,
              lastError: null, memory: "", flavour, stack: null, deadline: started + capMs,
              onStepState: pushLiveStep,
            });
          }
          ({ status, error } = runOutcome(run, req.kind));
        } finally {
          clearInterval(cancelPoll);
        }
      } catch (exc) {
        if (run?.cancelledByUser) {
          status = "cancelled";
          error = "Cancelled by request.";
        } else {
          status = "error";
          error = String(exc.message || exc);
        }
        if (run) log(run, `Request failed: ${error}`);
      }

      let checkPassed = null;
      if (run) {
        try {
          checkPassed = await evaluateCheck(run, req.meta && req.meta.check);
        } catch { /* a failed check must never take the listener down */ }
      }

      const finalTab = run ? await chrome.tabs.get(run.tabId).catch(() => null) : null;
      try {
        // Each step carries its own captured browser state (screenshot +
        // URL/title/visible-text/HTML, see stateOf()) via run.trace, keyed
        // by the same index run.steps uses -- merged in here so the caller
        // gets both the step summary and what the page actually looked like
        // at that point, not just the text.
        const traceByIndex = new Map((run ? run.trace : []).map((t) => [t.index, t]));
        const lastState = run && run.trace.length ? run.trace[run.trace.length - 1].state : null;
        await postJSON("/requests/result", {
          id: String(req.id), status, error, answer: (run && run.answer) || null,
          steps: run ? run.steps.filter(Boolean).map((s) => ({
            index: s.index, instruction: s.instruction, action: s.action,
            status: s.status, verify: s.verify || null, error: s.error || null,
            state: traceByIndex.get(s.index)?.state || null,
          })) : [],
          log: run ? run.log.slice(-40) : [],
          final_url: finalTab ? finalTab.url : null,
          final_state: lastState,
          check_passed: checkPassed,
          elapsed_s: Math.round((Date.now() - started) / 100) / 10,
          approval: (run && run.needsApproval) || null,
        });
      } catch (e) {
        if (run) log(run, `Could not post result for ${req.id}: ${e.message || e}`);
      }

      if (run) {
        run.paused = null;
        await resetAfterTask(run);
      }
      LISTENER.handled += 1;
      LISTENER.current = null;
      broadcastListener();
    }
  } finally {
    LISTENER.active = false;
    LISTENER.current = null;
    await persistListenerState();
    updateKeepAlive();
    broadcastListener();
  }
}

// Listening is the DEFAULT, not something the user has to opt into once --
// no stored state at all (a fresh install, or storage that hasn't been
// written to yet) means "never configured", not "off", so it starts with
// these defaults rather than sitting idle until someone opens the popup and
// clicks Start. An explicit Stop persists enabled:false and is respected --
// this only fills in the unconfigured case.
const DEFAULT_LISTENER_SETTINGS = {
  // verifyEnabled off by default: an extra model call after every single
  // action, second-guessing it against an expectation, roughly doubles
  // steps-per-task and is what turned a real success (the search actually
  // landed) into a false "paused" over a verify-timing race against a
  // page's own client-side redirect (see agent/BROWSER_INTEGRATION.md).
  // The action's own result (did the click/type/navigate itself succeed)
  // is still checked either way - this only turns off the SEPARATE
  // did-it-achieve-the-goal judgment call layered on top.
  modelKey: "devstral", vision: false, verifyEnabled: false, flavour: "live", perTaskSeconds: 900,
};

// Re-arms the listener after a service-worker restart or a fresh browser
// launch -- this is what makes "always listening" true across restarts
// rather than only within one popup session. Idempotent with
// startListener()'s own guard, so this is safe to call speculatively.
async function resumeListenerIfEnabled() {
  const stored = await chrome.storage.local.get(LISTENER_STORAGE_KEY).catch(() => ({}));
  const state = stored[LISTENER_STORAGE_KEY];
  if (state === undefined) {
    await startListener(DEFAULT_LISTENER_SETTINGS);
  } else if (state && state.enabled && state.settings) {
    await startListener(state.settings);
  }
}

chrome.runtime.onStartup.addListener(() => { resumeListenerIfEnabled(); });
chrome.runtime.onInstalled.addListener(() => { resumeListenerIfEnabled(); });
// The service worker itself can also be torn down and respawned by Chrome
// independent of either event above (MV3 workers are not long-lived) --
// this top-level call covers that respawn, since module-level code reruns
// every time the worker restarts.
resumeListenerIfEnabled();

// ---- record / replay ------------------------------------------------------
// Captures real user click/type/navigation verbatim and replays it with no
// model call at all -- plain deterministic chrome.scripting execution
// through the same domAction() agent runs use.
function recorderInject() {
  if (window.__agentRecorderActive) return;
  window.__agentRecorderActive = true;

  function buildSelector(el) {
    if (el.id) return `#${CSS.escape(el.id)}`;
    const testid = el.getAttribute("data-testid");
    if (testid) return `[data-testid="${CSS.escape(testid)}"]`;
    if (el.name) return `${el.tagName.toLowerCase()}[name="${CSS.escape(el.name)}"]`;
    const aria = el.getAttribute("aria-label");
    if (aria) return `[aria-label="${CSS.escape(aria)}"]`;
    const parts = [];
    let node = el;
    for (let i = 0; i < 5 && node && node.nodeType === 1 && node !== document.body; i++) {
      let part = node.tagName.toLowerCase();
      const parent = node.parentElement;
      if (parent) {
        const siblings = Array.from(parent.children).filter((c) => c.tagName === node.tagName);
        if (siblings.length > 1) part += `:nth-of-type(${siblings.indexOf(node) + 1})`;
      }
      parts.unshift(part);
      node = parent;
    }
    return parts.join(" > ");
  }

  const inOwnUi = (el) => !!(el.closest && el.closest("#__ba_sidecar__, #__ba_backdrop__"));
  // A secret field's value (a password, a one-time code, a card number) is
  // never recorded: the step says a secret was typed there, and a replay
  // stops at it for a person (domAction).
  const SECRET_AUTOCOMPLETE = /(^|\s)(current-password|new-password|one-time-code|cc-number|cc-csc)(\s|$)/i;
  const isSecret = (e) => e.tagName === "INPUT"
    && (String(e.type || "").toLowerCase() === "password"
      || SECRET_AUTOCOMPLETE.test(e.getAttribute("autocomplete") || ""));

  document.addEventListener("click", (e) => {
    if (inOwnUi(e.target)) return; // never record clicks on our own side-car
    const el = e.target.closest("a, button, [role=button], input[type=submit], input[type=button]") || e.target;
    chrome.runtime.sendMessage({
      type: "recordedEvent",
      step: { type: "click", selector: buildSelector(el), label: (el.innerText || el.value || "").trim().slice(0, 40) },
    }).catch(() => {});
  }, true);

  document.addEventListener("change", (e) => {
    const el = e.target;
    if (inOwnUi(el) || !("value" in el) || el.tagName === "SELECT") return;
    const secret = isSecret(el);
    const step = { type: "type", selector: buildSelector(el), text: secret ? "" : el.value, label: (el.placeholder || el.name || "").slice(0, 40) };
    chrome.runtime.sendMessage({
      type: "recordedEvent",
      step: secret ? { ...step, secret: true } : step,
    }).catch(() => {});
  }, true);
}

const recordNavListeners = new Map(); // tabId -> listener, so stopRecording can detach it

// A navigation the user performed themselves (typed a URL, used a bookmark,
// hit back) has to be recorded, or replay silently starts from the wrong
// page. A navigation caused by a click we ALREADY recorded must not be --
// replaying that click navigates on its own.
function recordNavigation(run, url) {
  if (!run.recording?.active || !url || url === "about:blank") return;
  const steps = run.recording.steps;
  const last = steps[steps.length - 1];
  if (last && last.type === "navigate" && last.url === url) return;
  if (last && last.type === "click" && Date.now() - (last.t || 0) < 3000) return;
  steps.push({ type: "navigate", url, t: Date.now() });
  chrome.runtime.sendMessage({ type: "recordingProgress", tabId: run.primaryTabId, count: steps.length }).catch(() => {});
  syncSidecar(run, "record");
}

async function startRecording(tabId) {
  const run = getRun(tabId);
  if (run.running) throw new Error("An agent run is already active on this tab.");
  run.tabId = tabId;
  run.recording = { steps: [], active: true };
  const tab = await chrome.tabs.get(tabId);
  run.windowId = tab.windowId;
  recordNavigation(run, tab.url); // where the recording starts from
  await chrome.scripting.executeScript({ target: { tabId }, func: recorderInject });
  await lockTab(run, tabId, "record"); // panel only -- no backdrop, the user must interact
  updateKeepAlive();

  const listener = (id, info) => {
    if (id !== tabId || !run.recording) return;
    if (info.url) recordNavigation(run, info.url);
    if (info.status === "complete") {
      // A fresh document means a fresh JS context -- the listeners and the
      // side-car both have to be put back.
      chrome.scripting.executeScript({ target: { tabId }, func: recorderInject }).catch(() => {});
      lockTab(run, tabId, "record");
    }
  };
  recordNavListeners.set(tabId, listener);
  chrome.tabs.onUpdated.addListener(listener);
}

async function stopRecording(tabId) {
  const run = getRun(tabId);
  const listener = recordNavListeners.get(tabId);
  if (listener) {
    chrome.tabs.onUpdated.removeListener(listener);
    recordNavListeners.delete(tabId);
  }
  // Capture stops, but the buffered steps stay until saved or discarded.
  if (run.recording) run.recording.active = false;
  await unlockTab(run.tabId);
  updateKeepAlive();
  chrome.runtime.sendMessage({ type: "recording_stopped", tabId: run.primaryTabId }).catch(() => {});
  return run.recording ? run.recording.steps : [];
}

async function saveRecording(tabId, name) {
  const run = getRun(tabId);
  const steps = run.recording ? run.recording.steps : [];
  const { savedRecordings = [] } = await chrome.storage.local.get("savedRecordings");
  savedRecordings.push({ id: `${Date.now()}`, name, createdAt: Date.now(), steps });
  await chrome.storage.local.set({ savedRecordings });
  run.recording = null;
  return savedRecordings;
}

async function deleteRecording(id) {
  const { savedRecordings = [] } = await chrome.storage.local.get("savedRecordings");
  const next = savedRecordings.filter((r) => r.id !== id);
  await chrome.storage.local.set({ savedRecordings: next });
  return next;
}

async function replayRecording(tabId, recordingId) {
  const run = getRun(tabId);
  if (run.running) throw new Error("An agent run is already active on this tab.");
  const { savedRecordings = [] } = await chrome.storage.local.get("savedRecordings");
  const recording = savedRecordings.find((r) => r.id === recordingId);
  if (!recording) throw new Error("Recording not found.");

  run.tabId = tabId;
  run.log = [];
  run.steps = [];
  run.running = true;
  run.stop = false;
  run.paused = null;
  run.currentStage = "Replaying...";
  const tab = await chrome.tabs.get(tabId);
  run.windowId = tab.windowId;
  run.lockedTabIds.add(tabId);
  await lockTab(run, tabId);
  updateKeepAlive();

  try {
    for (let i = 0; i < recording.steps.length; i++) {
      if (run.stop) { log(run, "Replay stopped by user."); stage(run, "Stopped by user."); break; }
      const step = recording.steps[i];
      const label = step.type === "navigate" ? step.url : (step.label || step.selector);
      upsertStep(run, i, { instruction: `${step.type}: ${label}`, action: step, status: "running" });
      stage(run, `Replaying ${i + 1}/${recording.steps.length}`);
      const result = await execAction(run, step);
      if (!result.success) {
        upsertStep(run, i, { status: "failed", error: result.error });
        log(run, `Replay paused at step ${i + 1}: ${result.error}`);
        stage(run, `Step ${i + 1} no longer works -- ${result.error}`);
        run.paused = { replay: true, recordingId, fromStep: i, lastError: result.error };
        return;
      }
      upsertStep(run, i, { status: "ok" });
    }
    if (!run.stop) { stage(run, "Replay complete."); log(run, "Replay complete."); }
  } finally {
    await finishRun(run);
  }
}

// ---- messaging --------------------------------------------------------
chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg.type === "run") {
    startRun(msg.tabId, msg).catch((e) => runFailed(getRun(msg.tabId), `Fatal error starting run: ${e.message || e}`));
    sendResponse({ ok: true });
  } else if (msg.type === "retry") {
    retryRun(msg.tabId).catch((e) => log(getRun(msg.tabId), `Fatal error resuming run: ${e.message || e}`));
    sendResponse({ ok: true });
  } else if (msg.type === "stop") {
    // Stop the work in flight, but leave the listener listening -- otherwise
    // cancelling one bad request would silently take the agent offline.
    stopRun(msg.tabId);
    sendResponse({ ok: true });
  } else if (msg.type === "startListening") {
    // Global, not tied to msg.tabId -- see LISTENER. The popup only supplies
    // the settings (model/vision/verify/flavour/timing); which tab it
    // happens to be open on is irrelevant, since every request gets its own
    // fresh tab regardless.
    startListener({
      modelKey: msg.modelKey, vision: msg.vision, verifyEnabled: msg.verifyEnabled,
      flavour: msg.flavour, perTaskSeconds: msg.perTaskSeconds, pollMs: msg.pollMs,
    });
    sendResponse({ ok: true });
  } else if (msg.type === "stopListening") {
    stopListener();
    sendResponse({ ok: true });
  } else if (msg.type === "stopByOverlay") {
    const tabId = sender.tab && sender.tab.id;
    const run = tabId != null ? findRunByLockedTab(tabId) : null;
    if (run) stopRun(run.primaryTabId);
  } else if (msg.type === "stopRecordingFromPage") {
    const tabId = sender.tab && sender.tab.id;
    const run = tabId != null ? findRecordingRun(tabId) : null;
    if (run) stopRecording(run.primaryTabId);
  } else if (msg.type === "getState") {
    const run = getRun(msg.tabId);
    sendResponse({
      log: run.log, steps: run.steps, stage: run.currentStage, running: run.running,
      paused: !!run.paused, pausedError: run.paused?.lastError,
      isReplay: !!run.paused?.replay, recording: !!run.recording?.active,
      recordedCount: run.recording ? run.recording.steps.length : 0,
      pendingRecording: !!(run.recording && !run.recording.active && run.recording.steps.length),
      hasTrace: run.trace.length > 0,
      listener: LISTENER,
    });
  } else if (msg.type === "ask") {
    (async () => {
      try {
        const run = getRun(msg.tabId);
        run.tabId = msg.tabId;
        const tab = await chrome.tabs.get(msg.tabId);
        run.windowId = tab.windowId;
        const snap = await captureSnapshot(run, !!msg.vision);
        const resp = await postJSON("/ask", {
          question: msg.question, url: snap.url, raw_html: snap.html,
          model_key: msg.modelKey, screenshot_b64: msg.vision ? snap.screenshot_b64 : null,
        });
        sendResponse({ ok: true, answer: resp.answer });
      } catch (exc) {
        sendResponse({ ok: false, error: String(exc.message || exc) });
      }
    })();
    return true;
  } else if (msg.type === "saveTrace") {
    (async () => {
      const run = getRun(msg.tabId);
      const { savedRuns = [] } = await chrome.storage.local.get("savedRuns");
      savedRuns.push({ id: `${Date.now()}`, name: msg.name, createdAt: Date.now(), task: msg.task, trace: run.trace });
      await chrome.storage.local.set({ savedRuns });
      sendResponse({ ok: true });
    })();
    return true;
  } else if (msg.type === "startRecording") {
    startRecording(msg.tabId)
      .then(() => sendResponse({ ok: true }))
      .catch((e) => sendResponse({ ok: false, error: String(e.message || e) }));
    return true;
  } else if (msg.type === "stopRecording") {
    stopRecording(msg.tabId).then((steps) => sendResponse({ ok: true, steps }));
    return true;
  } else if (msg.type === "saveRecording") {
    saveRecording(msg.tabId, msg.name).then((list) => sendResponse({ ok: true, savedRecordings: list }));
    return true;
  } else if (msg.type === "discardRecording") {
    const run = getRun(msg.tabId);
    run.recording = null;
    updateKeepAlive();
    sendResponse({ ok: true });
  } else if (msg.type === "listRecordings") {
    chrome.storage.local.get("savedRecordings").then(({ savedRecordings = [] }) => sendResponse({ savedRecordings }));
    return true;
  } else if (msg.type === "deleteRecording") {
    deleteRecording(msg.id).then((list) => sendResponse({ ok: true, savedRecordings: list }));
    return true;
  } else if (msg.type === "replayRecording") {
    replayRecording(msg.tabId, msg.recordingId).catch((e) => runFailed(getRun(msg.tabId), `Replay failed: ${e.message || e}`));
    sendResponse({ ok: true });
  } else if (msg.type === "recordedEvent") {
    const tabId = sender.tab && sender.tab.id;
    const run = tabId != null ? findRecordingRun(tabId) : null;
    if (run && run.recording) {
      run.recording.steps.push({ ...msg.step, t: Date.now() });
      chrome.runtime.sendMessage({
        type: "recordingProgress", tabId: run.primaryTabId, count: run.recording.steps.length,
      }).catch(() => {});
      syncSidecar(run, "record");
    }
  }
});
