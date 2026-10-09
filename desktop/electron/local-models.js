'use strict';

// Install the runtime at launch; models and inference follow the hosting intent.
function createLocalModels({ readPreference, writePreference, operate }) {
  let enabled = readPreference();
  let installed = false, task = null, abort = null;
  let state = { enabled, busy: false, phase: 'waiting', runtimeInstalled: false, detail: '', model: '', resources: null };
  const status = () => ({ ...state });
  function begin(actions) {
    abort = new AbortController();
    const signal = abort.signal;
    const pending = (async () => {
      state.busy = true;
      try {
        for (const action of actions) {
          if (action !== 'install' && action !== 'stop' && (!enabled || signal.aborted)) break;
          state.phase = action; state.detail = '';
          await operate(action, update => {
            if (typeof update === 'string') state.detail = update;
            else if (update) { if (update.model) state.model = update.model; if (update.resources) state.resources = update.resources; if (update.detail) state.detail = update.detail; }
          }, signal);
          if (action === 'install') { installed = true; state.runtimeInstalled = true; }
        }
        if (!signal.aborted) {
          state.phase = actions.includes('stop') ? 'off' : enabled ? 'ready' : 'off';
          state.detail = enabled ? 'Local model loaded and ready for your workers.' : 'Ollama is installed. Turn on local hosting to choose and download a model.';
          if (actions.includes('stop')) { state.model = ''; state.resources = null; state.detail = 'Model files deleted. Local inference is stopped and memory released.'; }
        }
      } catch (error) {
        if (!signal.aborted) { state.phase = 'error'; state.detail = error.message; }
      } finally { state.busy = false; }
      return status();
    })();
    task = pending;
    pending.finally(() => { if (task === pending) task = null; });
    return pending;
  }
  async function setEnabled(value) {
    if (typeof value !== 'boolean') throw new Error('Hosting must be on or off.');
    writePreference(value);
    enabled = value; state.enabled = value;
    if (task) {
      // Keep the first-start runtime download, but cancel model setup/inference.
      if (!value && state.phase !== 'install') abort.abort();
      await task;
      if (enabled !== value) return status();
      if (value && state.phase === 'ready') return status();
    }
    return begin(value ? (installed ? ['ensure', 'prepare'] : ['install', 'ensure', 'prepare']) : ['stop']);
  }
  return { status, setEnabled, resume: () => task || begin(['install', 'ensure', 'prepare']), prepare: () => setEnabled(true) };
}
module.exports = { createLocalModels };
