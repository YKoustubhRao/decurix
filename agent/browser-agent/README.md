# browser-agent

An LLM-driven browser agent, built to answer one question: how well can a
**local** model (Devstral-24B and others, served by Ollama, on a single
16GB-VRAM card) actually drive a real browser? Part of the local-LLM research
in `experiments/`.

There are **two independent pipelines** here, sharing the same core LLM
plumbing but driving the browser two different ways. Both work; they exist
in parallel deliberately, as two different answers to "how do you get a weak
local model to reliably act in a browser."

| | Playwright pipeline | Extension pipeline |
|---|---|---|
| Browser | A throwaway Chromium instance Playwright owns | Your real Chrome, via a browser extension |
| Entry point | `playwright_pipeline/server.py` (port 8090) | `extension_pipeline/ext_server.py` (port 8092) + load `extension/` in Chrome |
| Complex/bulk actions | Model writes real Playwright code (`codegen.py`) | Not supported -- fixed action grammar only |
| Simple actions | Executed deterministically, no code generation (`core/worker.py`'s `action` mode) | Executed deterministically by the extension's content script |
| Sessions/cookies | None -- fresh profile every run | Your actual logged-in sessions |

Use the Playwright pipeline for anything that benefits from a disposable,
scriptable browser (bulk downloads, arbitrary DOM loops, repeatable evals).
Use the extension pipeline for anything that needs your real sessions
(shopping carts, logged-in sites) or when you want to watch it work in your
actual browser window.

## How a step works (both pipelines)

Older versions of this project made 3-4 separate LLM calls per step (decide
the next instruction, state an expectation, generate the action, verify the
outcome). Both pipelines now collapse that into **one structured-output
call per step** (`fast_step.py` / `ext_codegen.py`'s `decide_and_act`),
using Ollama's/llama-server's grammar-constrained JSON output so the
response can't come back malformed. That one call returns:

- `evaluation_previous_goal` -- did the LAST action actually work, judged
  against the current page? Free self-correction, no extra call.
- `memory` -- a scratchpad the model rewrites each turn, carried across
  steps (and across a pause/retry).
- `next_instruction` / `remaining` -- what happens now, and what's left of
  the task.
- `expectation` -- one checkable sentence, used by a separate verify call
  after the action runs (optional, toggled by `verify_enabled`).
- `action` -- one of `search` / `select` / `click` / `type` / `navigate` /
  `scroll` / `answer` / `done`, executed deterministically (never
  model-authored code). The Playwright pipeline adds one more:
  `complex`, which falls through to `codegen.py` for anything the fixed
  grammar can't express (a loop over several elements, a bulk operation).

On any hard failure (or hitting the step cap), a run **pauses** rather than
auto-retrying -- retry is a manual action (`/retry` in the Playwright UI, or
re-running in the extension popup), which resumes from exactly where it
stopped, memory included. A deterministic stagnation guard also pauses a run
that keeps repeating the same action with no visible effect on the page,
rather than burning the whole step budget on a stuck loop.

## Atomizer flavours: live vs. stack

Independent of the step format above, each pipeline offers two ways to
decide *what the next instruction is*, selectable per run (a dropdown next
to model selection in both UIs):

- **Live** (default): the instruction is decided fresh every step, from the
  actual current page -- no upfront plan. Reacts to anything just by
  deciding differently next call, at the cost of a decide-call every step.
- **Stack**: `stack_atomizer.plan()` (`core/stack_atomizer.py`) runs ONCE at
  the start and decides the full ordered list of atomic steps up front,
  held as a stack (top = next to execute). Each step still grounds the
  current top-of-stack instruction in the live page -- a plan can't know the
  live DOM -- via `fast_step.decide_stack_step()` /
  `ext_codegen.decide_stack_step()`, but no longer decides *what* comes
  next, cutting per-task planning calls from one-per-step to one-per-task.
  Two things it can still do reactively, both decided in the same call as
  the grounded action:
  - `push_before` -- something the plan couldn't have known about (a cookie
    banner, an unexpected redirect) is blocking the current step; describe
    it here and `action` addresses that detour instead, leaving the
    original step on the stack to retry next turn.
  - `action.type: "skip"` -- the current step's goal is *already* true on
    the page (e.g. "navigate to X" when already there) -- pop it with no
    browser action and no verify call, straight to the next step. This is
    different from `action.type: "done"`, which ends the *entire* run
    early (rare -- an overshot plan), not just the current step.

  The extension pipeline's `/step` resolves a run of `skip`s server-side
  (bounded) before returning, so the client doesn't pay a round-trip for a
  step that turned out to need nothing. The Playwright pipeline's
  `runner.py` does the equivalent with an inline loop `continue`.

## Layout

```
core/                  Shared by both pipelines
  models.py               LLM call abstraction (Ollama + the spec-decode backend)
  spec_backend.py          llama-server backend for speculative decoding (qwen3-spec)
  atomizer.py              is_done() + the old free-text atomize call (slow-path fallback)
  stack_atomizer.py        The stack flavour's plan() call + the Stack data structure
  reasoning.py             expected_outcome / verify_outcome / answer_from_page
  worker.py                Playwright subprocess: page snapshot, code exec, fixed-action exec
  stats.py                 VRAM/RAM/model-residency info for the UI

playwright_pipeline/    Playwright-owned throwaway browser
  server.py                FastAPI backend + SSE event stream (entry point, port 8090)
  runner.py                The step loop (fast path + complex/codegen fallback)
  codegen.py                Writes a Playwright snippet for one "complex" instruction
  fast_step.py              The combined structured-output decision call
  browser_host.py           Owns the one long-lived Chromium instance
  screencast.py             MJPEG screencast for the UI
  interrogate.py            One-off read-only Q&A about the current page
  docs/playwright_reference.md
  static/                  Browser-based UI (index.html)
  downloads/, results/     Run artifacts (gitignored)

extension_pipeline/     Chrome-extension-owned real browser
  ext_server.py             FastAPI backend, no browser ownership (entry point, port 8092)
  ext_codegen.py             The combined structured-output decision call (selector-grounded)

extension/              The Chrome extension itself (load unpacked in chrome://extensions)
  background.js             Orchestrates the step loop, tab locking, the in-page sidecar
  popup.html/js              The toolbar popup UI
  manifest.json

eval/
  eval_tasks.md            200-task hand-graded eval log for the Playwright pipeline
  eval100.py               100-task eval harness for the extension pipeline (enqueue/status/report)

legacy/                 Superseded prototypes, kept for reference
  agent.py                 The original single-file ReAct loop (index-addressed elements,
                            regex-parsed actions) -- predates the atomize/codegen split
  planner.py                Upfront whole-task planner -- predates live per-step atomization
  dom_snapshot.js, dom_elements.js   JS snapshot helpers agent.py used
  results/                  agent.py's own old run traces/screenshots
```

Both `playwright_pipeline/` and `extension_pipeline/` import `core/` modules
by plain name (`import models`, not `from core import models`) via a small
`_pathfix.py` in each directory that adds `core/` to `sys.path` -- these are
run directly (`python server.py`) or launched as subprocesses, not via
`python -m`, so this was simpler and more robust than converting the whole
thing into a dotted-import package.

## Running it

**Playwright pipeline:**
```
cd playwright_pipeline
python server.py          # http://127.0.0.1:8090
```

**Extension pipeline:**
```
cd extension_pipeline
python ext_server.py      # http://127.0.0.1:8092
```
Then load `extension/` as an unpacked extension in `chrome://extensions`
(Developer mode on, "Load unpacked"). The request listener is on by
default (see `background.js`'s `LISTENER`) -- no popup interaction needed;
it persists across service-worker and browser restarts, and each incoming
request gets its own fresh tab.

Both talk to Ollama at `http://127.0.0.1:11434` (see `core/models.py` for
the model list) and expect it already running.

Every step captures the browser's actual state -- a screenshot plus
URL/title/visible-text/HTML (`background.js`'s `stateOf()`) -- regardless of
whether vision mode feeds a screenshot to the model; this is the returned
record, not a model input. It's attached to each `run.trace` entry and
included per-step (plus a `final_state`) in what the listener posts back to
`/requests/result` -- but for someone remote watching a request run who
isn't the one driving the browser, that final bundle only arrives once the
whole task is done. For live, step-by-step visibility instead:

- The listener POSTs each step's state to `POST /requests/{id}/step` the
  moment it's captured (`background.js`'s `makeLivePusher`), not batched.
- A remote viewer polls `GET /requests/{id}/live?since=<last index seen>`
  to fetch only what's new since its last poll -- pass `-1` for everything
  so far. Kept to the most recent 30 steps per request server-side
  (`MAX_LIVE_STEPS` in `ext_server.py`) so a long task doesn't grow this
  without bound in memory.

## Model notes

Default model is `devstral-small-2:24b` (`core/models.py`'s `DEFAULT_MODEL`)
-- the only one that won a real accuracy bake-off on this hardware (see
`experiments/kv-ram/`). Devstral's raw GGUF has no tool-calling chat
template, but Ollama's own Modelfile template for it does support
schema-constrained JSON output, which is what both pipelines' fast paths
rely on -- no need for llama-server's `--jinja` tool-calling machinery.
