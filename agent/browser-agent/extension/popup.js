const SERVER = "http://127.0.0.1:8092";
const STORAGE_KEY = "browserAgentPopupState";

const $ = (id) => document.getElementById(id);
const taskEl = $("task");
const modelEl = $("model");
const flavourEl = $("flavour");
const visionEl = $("vision");
const verifyEl = $("verify");
const runBtn = $("run");
const stopBtn = $("stop");
const retryBtn = $("retry");
const saveRunBtn = $("saveRunBtn");
const saveRunRow = $("saveRunRow");
const saveRunName = $("saveRunName");
const saveRunConfirm = $("saveRunConfirm");
const saveRunCancel = $("saveRunCancel");
const statusEl = $("status");
const stageLineEl = $("stageLine");
const lockNoteEl = $("lockNote");
const statsBarEl = $("statsBar");
const questionEl = $("question");
const askBtn = $("ask");
const answerBox = $("answerBox");
const logEl = $("log");
const stepsEl = $("steps");

const startListenBtn = $("startListening");
const stopListenBtn = $("stopListening");
const listenCapEl = $("listenCap");
const listenStatusEl = $("listenStatus");
const listenIndicator = $("listenIndicator");
const listenCountEl = $("listenCount");
const startRecordBtn = $("startRecord");
const stopRecordBtn = $("stopRecord");
const recIndicator = $("recIndicator");
const recCountEl = $("recCount");
const recordSaveRow = $("recordSaveRow");
const recordNameEl = $("recordName");
const saveRecordBtn = $("saveRecordBtn");
const discardRecordBtn = $("discardRecordBtn");
const recordStatusEl = $("recordStatus");
const recordingsListEl = $("recordingsList");
const runsListEl = $("runsList");

let activeTabId = null;

const show = (el, on) => el.classList.toggle("hidden", !on);

// chrome.storage.local briefly reads as undefined right after the extension
// is reloaded from chrome://extensions (its APIs re-attach to the popup a
// beat after the document itself loads) -- these wrap every call so that
// transient window instead throws an uncaught rejection that kills whatever
// else was running in this script, rather than just skipping one draft
// save/load.
async function storageGet(keys) {
  try {
    return await chrome.storage.local.get(keys);
  } catch {
    return {};
  }
}
async function storageSet(items) {
  try {
    await chrome.storage.local.set(items);
  } catch { /* best effort -- a draft/setting not saved once isn't worth crashing the popup over */ }
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function classifyLine(line) {
  if (/^(Fatal error|Paused:|Replay failed|failed:|no element matches)/.test(line)) return "err";
  if (/^(Task complete\.|Replay complete\.|  verify: MET)/.test(line)) return "ok";
  if (/^(Loading .* into VRAM|Halted|  verify: NOT_MET|Stopped)/.test(line)) return "warn";
  if (/^Step \d+:/.test(line)) return "step";
  if (/^(Reading page|Asking |Running action|New tab)/.test(line)) return "muted";
  return "";
}

function appendLog(line) {
  const div = document.createElement("div");
  div.className = classifyLine(line);
  div.textContent = line;
  logEl.appendChild(div);
  logEl.scrollTop = logEl.scrollHeight;
}

function renderLog(lines) {
  logEl.textContent = "";
  for (const line of lines) appendLog(line);
}

// The atomizer's decision, the action it chose and the verify result, one
// card per step -- rather than making the user parse a flat log.
function renderSteps(steps) {
  stepsEl.textContent = "";
  const real = (steps || []).filter(Boolean);
  if (!real.length) {
    stepsEl.innerHTML = '<div class="empty-note">No steps yet.</div>';
    return;
  }
  for (const s of real) {
    const card = document.createElement("div");
    card.className = "step-card";
    const badgeText = { running: "running", ok: "done", failed: "failed", not_met: "not met" }[s.status] || "pending";
    card.innerHTML =
      '<div class="step-head">'
      + `<span class="step-instr">${s.index + 1}. ${escapeHtml(s.instruction || "")}</span>`
      + `<span class="badge ${s.status || ""}">${badgeText}</span></div>`
      + (s.expectation ? `<div class="step-detail">expecting: ${escapeHtml(s.expectation)}</div>` : "")
      + (s.action ? `<div class="step-action">${escapeHtml(JSON.stringify(s.action))}</div>` : "")
      + (s.error ? `<div class="step-verify bad">${escapeHtml(s.error)}</div>` : "")
      + (s.verify
        ? `<div class="step-verify ${s.verify.met ? "ok" : "bad"}">`
          + (s.verify.met ? "✓ verified" : "✗ " + escapeHtml(s.verify.reason || "not met"))
          + "</div>"
        : "");
    stepsEl.appendChild(card);
  }
  stepsEl.scrollTop = stepsEl.scrollHeight;
}

async function currentTabId() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  return tab.id;
}

// An MV3 popup is a fresh, empty document every time it opens, so the task
// text and toggles live in chrome.storage.local rather than in the DOM.
async function saveDraft() {
  await storageSet({
    [STORAGE_KEY]: {
      task: taskEl.value, model: modelEl.value, vision: visionEl.checked, verify: verifyEl.checked,
    },
  });
}

async function restoreDraft() {
  const data = (await storageGet(STORAGE_KEY))[STORAGE_KEY];
  if (!data) return;
  taskEl.value = data.task || "";
  visionEl.checked = !!data.vision;
  verifyEl.checked = data.verify !== false;
}

for (const el of [taskEl, modelEl, visionEl, verifyEl]) {
  el.addEventListener(el === taskEl ? "input" : "change", saveDraft);
}

async function loadModels() {
  try {
    const models = await (await fetch(SERVER + "/models")).json();
    modelEl.textContent = "";
    for (const [key, cfg] of Object.entries(models)) {
      const opt = document.createElement("option");
      opt.value = key;
      opt.textContent = cfg.label + (cfg.vision ? " (vision)" : "");
      modelEl.appendChild(opt);
    }
    const saved = (await storageGet(STORAGE_KEY))[STORAGE_KEY];
    if (saved?.model && [...modelEl.options].some((o) => o.value === saved.model)) modelEl.value = saved.model;
  } catch {
    setStatus("ext_server.py not reachable on :8092", "err");
  }
}

function fmtGb(mb) { return mb == null ? "?" : (mb / 1024).toFixed(1) + "GB"; }

async function refreshStats() {
  try {
    const s = await (await fetch(SERVER + "/stats")).json();
    const vram = s.vram ? `VRAM <b>${fmtGb(s.vram.used_mb)}</b>/${fmtGb(s.vram.total_mb)}` : "VRAM n/a";
    const loaded = (s.ollama_models || []).map((m) => escapeHtml(m.name)).join(", ") || "none";
    statsBarEl.innerHTML = `${vram} &nbsp;|&nbsp; loaded: ${loaded}`;
  } catch {
    statsBarEl.textContent = "ext_server.py not reachable on :8092";
  }
}

function setStatus(text, kind) {
  statusEl.className = "status-line" + (kind ? " " + kind : "");
  statusEl.textContent = text || "";
}

function setStage(text) {
  stageLineEl.textContent = text || "";
  show(stageLineEl, !!text);
}

// One place decides what every control looks like for a given state, so the
// popup can't drift into showing "running..." after a run has finished (which
// it used to, because the class was cleared before it was tested).
function applyState(state) {
  const running = !!state.running;
  const paused = !!state.paused;
  const recording = !!state.recording;
  const listening = !!state.listener?.active;

  show(startListenBtn, !listening);
  show(stopListenBtn, listening);
  show(listenIndicator, listening);
  startListenBtn.disabled = running || recording;
  if (listening) {
    const n = state.listener.handled || 0;
    const cur = state.listener.current;
    listenCountEl.textContent = `${n} handled`;
    listenStatusEl.className = "status-line running";
    listenStatusEl.textContent = cur
      ? (cur.kind === "program" ? `running program "${cur.program}"` : `running: ${String(cur.task).slice(0, 44)}`)
      : "waiting for a request";
  } else if (state.listener?.handled) {
    listenStatusEl.className = "status-line";
    listenStatusEl.textContent = `${state.listener.handled} request${state.listener.handled === 1 ? "" : "s"} handled`;
  }

  runBtn.disabled = running || recording || listening;
  startRecordBtn.disabled = running || recording || listening;
  show(startRecordBtn, !recording);
  show(stopRecordBtn, recording);
  show(recIndicator, recording);
  if (recording) {
    const n = state.recordedCount || 0;
    recCountEl.textContent = `${n} action${n === 1 ? "" : "s"} captured`;
  }

  show(retryBtn, paused && !state.isReplay);
  show(saveRunBtn, !running && !!state.hasTrace && saveRunRow.classList.contains("hidden"));
  show(lockNoteEl, running || paused);

  if (running) setStatus("running...", "running");
  else if (paused) setStatus(`Paused: ${(state.pausedError || "a step failed").split("\n")[0]}`, "paused");
  else setStatus("");

  setStage(running || paused ? state.stage : "");
}

async function refreshState() {
  const state = await chrome.runtime.sendMessage({ type: "getState", tabId: activeTabId });
  renderLog(state.log || []);
  renderSteps(state.steps || []);
  applyState(state);
  return state;
}

runBtn.addEventListener("click", async () => {
  const task = taskEl.value.trim();
  if (!task) return;
  await saveDraft();
  stepsEl.textContent = "";
  logEl.textContent = "";
  show(saveRunRow, false);
  await chrome.runtime.sendMessage({
    type: "run", tabId: activeTabId, task,
    modelKey: modelEl.value, vision: visionEl.checked, verifyEnabled: verifyEl.checked,
    flavour: flavourEl.value,
  });
  await refreshState();
});

retryBtn.addEventListener("click", async () => {
  await chrome.runtime.sendMessage({ type: "retry", tabId: activeTabId });
  await refreshState();
});

stopBtn.addEventListener("click", async () => {
  await chrome.runtime.sendMessage({ type: "stop", tabId: activeTabId });
  await refreshState();
});

// prompt() isn't reliable inside an MV3 popup (it can dismiss the popup
// outright), so naming happens in an inline row instead.
saveRunBtn.addEventListener("click", () => {
  show(saveRunBtn, false);
  show(saveRunRow, true);
  saveRunName.value = taskEl.value.trim().slice(0, 40);
  saveRunName.focus();
});

saveRunCancel.addEventListener("click", () => {
  show(saveRunRow, false);
  refreshState();
});

saveRunConfirm.addEventListener("click", async () => {
  const name = saveRunName.value.trim() || `Run ${new Date().toLocaleString()}`;
  await chrome.runtime.sendMessage({ type: "saveTrace", tabId: activeTabId, name, task: taskEl.value.trim() });
  show(saveRunRow, false);
  await refreshRunsList();
  await refreshState();
});

askBtn.addEventListener("click", async () => {
  const question = questionEl.value.trim();
  if (!question) return;
  answerBox.innerHTML = '<span class="status-line running">thinking...</span>';
  askBtn.disabled = true;
  const resp = await chrome.runtime.sendMessage({
    type: "ask", tabId: activeTabId, question, modelKey: modelEl.value, vision: visionEl.checked,
  });
  askBtn.disabled = false;
  answerBox.innerHTML = resp.ok
    ? escapeHtml(resp.answer)
    : `<span class="status-line err">${escapeHtml(resp.error)}</span>`;
});

// ---- listener ----
startListenBtn.addEventListener("click", async () => {
  stepsEl.textContent = "";
  logEl.textContent = "";
  // Global, not tied to this tab -- the listener creates its own fresh tab
  // per request regardless of which tab the popup happens to be open on.
  await chrome.runtime.sendMessage({
    type: "startListening",
    modelKey: modelEl.value, vision: visionEl.checked, verifyEnabled: verifyEl.checked,
    flavour: flavourEl.value, perTaskSeconds: parseInt(listenCapEl.value, 10) || 900,
  });
  await refreshState();
});

stopListenBtn.addEventListener("click", async () => {
  await chrome.runtime.sendMessage({ type: "stopListening" });
  await refreshState();
});

// ---- record mode ----
startRecordBtn.addEventListener("click", async () => {
  recordStatusEl.textContent = "";
  const resp = await chrome.runtime.sendMessage({ type: "startRecording", tabId: activeTabId });
  if (!resp.ok) {
    recordStatusEl.className = "status-line err";
    recordStatusEl.textContent = resp.error;
    return;
  }
  show(recordSaveRow, false);
  await refreshState();
});

stopRecordBtn.addEventListener("click", () => finishRecording());

async function finishRecording() {
  const resp = await chrome.runtime.sendMessage({ type: "stopRecording", tabId: activeTabId });
  await refreshState();
  const steps = resp?.steps || [];
  if (steps.length) {
    show(recordSaveRow, true);
    recordNameEl.value = "";
    recordStatusEl.className = "status-line";
    recordStatusEl.textContent = `${steps.length} action${steps.length === 1 ? "" : "s"} captured.`;
    recordNameEl.focus();
  } else {
    recordStatusEl.className = "status-line";
    recordStatusEl.textContent = "Nothing was captured.";
  }
}

saveRecordBtn.addEventListener("click", async () => {
  const name = recordNameEl.value.trim() || `Recording ${new Date().toLocaleString()}`;
  await chrome.runtime.sendMessage({ type: "saveRecording", tabId: activeTabId, name });
  show(recordSaveRow, false);
  recordStatusEl.textContent = "Saved.";
  await refreshRecordingsList();
});

discardRecordBtn.addEventListener("click", async () => {
  await chrome.runtime.sendMessage({ type: "discardRecording", tabId: activeTabId });
  show(recordSaveRow, false);
  recordStatusEl.textContent = "Discarded.";
});

function savedRow(item, meta, actions) {
  const row = document.createElement("div");
  row.className = "saved-item";
  const info = document.createElement("div");
  info.className = "info";
  info.innerHTML = `<div class="name">${escapeHtml(item.name)}</div><div class="meta">${meta}</div>`;
  row.appendChild(info);
  for (const btn of actions) row.appendChild(btn);
  return row;
}

function makeBtn(text, cls, onClick) {
  const b = document.createElement("button");
  b.textContent = text;
  b.className = cls;
  b.addEventListener("click", onClick);
  return b;
}

async function refreshRecordingsList() {
  const { savedRecordings = [] } = await chrome.runtime.sendMessage({ type: "listRecordings" });
  recordingsListEl.textContent = "";
  if (!savedRecordings.length) {
    recordingsListEl.innerHTML = '<div class="empty-note">None yet.</div>';
    return;
  }
  for (const rec of savedRecordings.slice().reverse()) {
    const meta = `${rec.steps.length} steps &middot; ${new Date(rec.createdAt).toLocaleString()}`;
    const replay = makeBtn("Replay", "small", async () => {
      stepsEl.textContent = "";
      logEl.textContent = "";
      await chrome.runtime.sendMessage({ type: "replayRecording", tabId: activeTabId, recordingId: rec.id });
      await refreshState();
    });
    const del = makeBtn("✕", "icon", async () => {
      await chrome.runtime.sendMessage({ type: "deleteRecording", id: rec.id });
      await refreshRecordingsList();
    });
    recordingsListEl.appendChild(savedRow(rec, meta, [replay, del]));
  }
}

async function refreshRunsList() {
  const { savedRuns = [] } = await storageGet("savedRuns");
  runsListEl.textContent = "";
  if (!savedRuns.length) {
    runsListEl.innerHTML = '<div class="empty-note">None yet.</div>';
    return;
  }
  for (const run of savedRuns.slice().reverse()) {
    const meta = `${run.trace.length} steps &middot; ${new Date(run.createdAt).toLocaleString()}`;
    const del = makeBtn("✕", "icon", async () => {
      const { savedRuns: cur = [] } = await storageGet("savedRuns");
      await storageSet({ savedRuns: cur.filter((r) => r.id !== run.id) });
      await refreshRunsList();
    });
    runsListEl.appendChild(savedRow(run, meta, [del]));
  }
}

chrome.runtime.onMessage.addListener((msg) => {
  if (msg.tabId !== activeTabId) return;
  if (msg.type === "log") appendLog(msg.line);
  if (msg.type === "stage") setStage(msg.text);
  if (msg.type === "steps") renderSteps(msg.steps);
  if (msg.type === "run_finished") refreshState();
  if (msg.type === "listener") refreshState();
  if (msg.type === "recordingProgress") {
    const n = msg.count || 0;
    recCountEl.textContent = `${n} action${n === 1 ? "" : "s"} captured`;
  }
  // Recording can also be stopped from the panel on the page itself, with
  // this popup closed or open -- either way the save row should appear.
  if (msg.type === "recording_stopped") finishRecording();
});

(async () => {
  activeTabId = await currentTabId();
  await restoreDraft();
  await loadModels();
  await refreshState();
  await refreshStats();
  await refreshRecordingsList();
  await refreshRunsList();
  setInterval(refreshStats, 3000);
})();
