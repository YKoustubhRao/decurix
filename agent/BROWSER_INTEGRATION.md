# Browser-agent: what's here, and how TheBot will end up using it

`browser-agent/` in this directory started as a copy of
`research/experiments/browser-agent` — a standalone research tool. The
extension is now actually wired into every desktop's Chromium (see the
checklist at the bottom for exactly what's done vs. still open) — this file
is both the integration plan and, in its status section, the current state
of that wiring.

The short version of where this is going: **TheBot orchestrates it**. The
tool doesn't keep its standalone UI as the way tasks get driven — a human
doesn't open the browser-agent popup and drive it by hand. TheBot's own job
graph gains a new step kind that calls into its server the same way
`[shell]`/`[web]`/`[code]` steps already call into their own tools, and the
result comes back through TheBot's normal synthesis into one reply. That
skill doesn't exist yet — today, driving a task still means opening the
extension's own popup (or POSTing to `/requests` directly), same as the
research tool always worked.

## Browser control: the extension pipeline, not the Playwright one

The research tool had **two independent pipelines** sharing the same `core/`
LLM plumbing; only one is carried here:

- `playwright_pipeline/` — a throwaway Chromium instance Playwright owns
  outright. **Not part of this integration, and removed from this copy**
  (with the research `eval/` and `legacy/` directories; see git history). A
  desktop already has a real, persistent Chromium that the human and the
  agent's own computer-use layer (`decurix_agent/computer/cdp.py`) both use,
  and its model-written Playwright code would bypass the approval gate.
- `extension_pipeline/` + `extension/` — the one this integration is for.
  `extension/` is a normal Chrome/Chromium extension (Manifest V3, load
  unpacked); `extension_pipeline/ext_server.py` is its backend on port 8092.
  Point is: it drives the desktop's **real** Chromium, in the **real**
  logged-in session the human/agent already has open — not a second browser.

### Getting the extension into the desktop's Chromium

The desktop's Chromium already gets its launch flags from
`agent/chromium-browser.init` (`CHROMIUM_FLAGS`, reinstalled into
`~/.chromium-browser.init` on every start by `start_agent_terminal.sh`, since
`$HOME` is the persistent volume). Loading the extension is one more flag
pair there:

```
--load-extension=/opt/browser-agent/extension --disable-extensions-except=/opt/browser-agent/extension
```

This is plain open-source Chromium (the base image, not Chrome-branded), so
`--load-extension` works with no developer-mode toggle needed — that
restriction is a Chrome-stable-channel policy, not something Chromium itself
enforces. **Done**: all three `docker/desktop.*.Dockerfile`s now `COPY`
`browser-agent/core`, `browser-agent/extension` and
`browser-agent/extension_pipeline` to `/opt/browser-agent/`, and
`agent/chromium-browser.init` carries the two flags above. Verified live: a
container built from the updated `desktop.basic.Dockerfile`, with Chromium
launched inside it, shows the extension's manifest-v3 service worker
(`chrome-extension://<id>/background.js`) as a live CDP target at
`127.0.0.1:9222/json` — the extension is genuinely loaded, not just present
on disk.

### Logins persist, and that's why it matters

`chromium-browser.init` deliberately passes no `--user-data-dir`, so
Chromium uses its own default profile location, `~/.config/chromium` —
which is already inside `/home/headless`, this desktop's persistent named
volume, not something that resets on a restart. Verified live: the profile
directory is genuinely there, at exactly that path, after a fresh launch.
Signing into Google (or anything else) through this Chromium therefore
persists across a stop/start of *that same desktop*, the same way its
cookies and history already do — no code change was needed for this, only
confirming it and writing it down where the rest of the Chromium launch
decisions already live.

This is not just convenience. A logged-in, cookie-carrying browser looks
nothing like fresh/anonymous automated traffic to a site's bot defenses —
and the difference is not hypothetical: a live run against Google (below)
hit its real CAPTCHA page precisely because the session was logged out. The
one case this does not cover is a *new* desktop, or one actually *deleted*
(as opposed to stopped) — either way starts from an empty volume, hence a
logged-out profile, same as installing Chromium fresh anywhere.

### Getting ext_server.py running

`ext_server.py` has to be alive on `127.0.0.1:8092` before the extension is
useful (both `background.js` and `popup.js` hardcode that URL). **Done**:
`start_agent_terminal.sh` now starts it, unconditionally (like `vm_server.py`
— it's a property of this desktop's browser, not of which harness is
running), from a dedicated venv (`/opt/browser-agent-venv`, built in each
Dockerfile from `browser-agent/requirements.txt`: fastapi, uvicorn, pydantic,
beautifulsoup4, psutil, requests),
logging to `/tmp/decurix-ext-server.log`. No readiness gate yet (see the
checklist) — it just starts and is reachable within a second or two.

The correctness fix this needed first: `core/models.py` used to hardcode
`OLLAMA = "http://127.0.0.1:11434"` — fine on the researcher's own machine,
wrong inside a desktop container, where Ollama is only reachable at
`OLLAMA_URL` (`http://host.docker.internal:11434`, same convention every
other harness already follows — see `decurix_agent/ollama_client.py`).
**Done** — it now reads `os.environ.get("OLLAMA_URL", ...)` the same way.
Verified live: `ext_server`'s `/stats` from inside a running desktop reported
the real host Ollama's resident models, and a full run (below) got a real
answer back from `devstral-small-2:24b` running on the host.

### How TheBot will actually drive it

`ext_server.py`'s `/requests` inbox is designed for exactly this: "anything
at all — a script, a CLI, another app — can hand the agent work by POSTing to
`/requests`" (its own docstring). TheBot doesn't need MCP tools or a new
CDP/xdotool code path here — it needs a **skill** that:

1. `POST /requests` with `{kind: "instruction", task: "..."}` (or
   `kind: "program"` to replay a saved recording with no model call at all).
2. Polls `GET /requests/status` (or `/requests/next`-style waiting) until
   that request's `status` is `done`.
3. Hands back the result's `answer` / `steps` / `error` the way the operator
   skill already reports a `gui` command's outcome.

This slots into the step graph the same way `[web]`/`[code]` do today
(`internal/atomizer`'s planner tags, per the main decurix README) — most
likely as a new `[browser]` tag, since today's only "acts on the screen" path
is the operator's `gui` command loop (xdotool/AT-SPI/CDP), and this is a
genuinely different, better-grounded way to drive a *web page* specifically
(CSS-selector actions off the page's own cleaned HTML, not coordinates or a
generic accessibility tree). `atomizer.Triage`'s `wantsScreen` routing is the
existing precedent for "route this deterministically before asking a model
to decide" — the same idea extends to "this needs a live web page, route to
`[browser]`."

**Approval gate.** `background.js`'s `domAction` carries
`computer/approval.py`'s rules (keep the two in step): a click on a control
whose own label commits the person (`CONSEQUENTIAL_CONTROL`: Send/Pay/Buy/
Publish-style) and Enter in a messaging page outside a search box are not
carried out. The run stops with status `needs_approval` and the action
(`click "Send" on mail.google.com`); TheBot's browser skill turns that into
`NeedsApproval`, the scheduler asks the person, and on a yes the step runs
again with `approved: [that action]` on the request - the extension lets it
through once. A yes is one action's. In the popup, Retry on a run the gate
stopped is the person's yes to that action.

## What "TheBot will control everything" means concretely

The tool keeps no human-facing entry point once this lands: no
`Browser-Agent.desktop` icon opening a UI on the desktop — the extension's
own popup stays (it's the extension's normal toolbar surface, useful for a
human watching or taking over), but the *task-driving* path is `/requests`,
called by TheBot's skill, not a human typing into the popup.

It becomes a plain HTTP backend that only TheBot's scheduler ever talks to,
the same relationship `internal/coding` already has to its own backend per
the main CLAUDE.md, or the operator skill has to the `gui` command.

## Status / next steps checklist

1. ✅ Patch `core/models.py` to read `OLLAMA_URL`.
2. ✅ `COPY` `browser-agent/{core,extension,extension_pipeline,requirements.txt}`
   into each `docker/desktop.*.Dockerfile` (`desktop.basic.Dockerfile` also
   gained `python3-pip`/`python3-venv`, which the developer/ai profiles
   already had); `--load-extension`/`--disable-extensions-except` added to
   `agent/chromium-browser.init`.
3. ✅ Start `ext_server.py` as a background daemon from
   `start_agent_terminal.sh`. **Still open**: no readiness gate (the
   `decurix_agent/readiness.py`'s `wait_for_http_ok` pattern) — it's a minor
   gap since nothing currently depends on it being up by a specific time.
4. ✅ Approval-gate bridge for the extension pipeline's actions (see "Approval
   gate" above).
5. ✅ Add a TheBot `[browser]` skill/step tag in `internal/skills` +
   `internal/atomizer`, calling `ext_server`'s HTTP API and reporting results
   back through the normal step/checkpoint machinery. **Partially open**: the
   skill has no `Checkpoint`/`Resume` support (see below) — a paused browser
   job cannot be resumed as a browser job, unlike the operator's.
6. ⬜ Retire the standalone popup as anything a human is expected to open
   directly on the desktop, once TheBot's own path covers the same ground.
7. ✅ The extension listens on its own, with no human ever opening its
   popup — needed because TheBot has no way to click "Start Listening"
   itself. Originally built here as a `chrome.alarms`-driven
   `ensureListening`; superseded (see "Upstream rewrite" below) by a
   better, first-party version from `research/experiments/browser-agent`
   that makes always-on listening the extension's own default behavior,
   with no Decurix-specific code needed for it at all.
8. ✅ Chromium logins (Google's included) persist across a desktop restart —
   confirmed to already work via the default profile location, not a new
   mechanism. See "Logins persist, and that's why it matters" above.

### How 1–3 were verified

Built `decurix-desktop:basic` with the changes above, ran a throwaway
container from it (no control plane involved), and:

- Confirmed `ext_server.py` was running and `/stats` showed the real host
  Ollama's resident models (`OLLAMA_URL` reaching `host.docker.internal`
  correctly from inside the container).
- Launched `chromium-browser` inside the container and confirmed the
  extension's service worker appeared as a live target at
  `127.0.0.1:9222/json` — the extension genuinely loaded via
  `--load-extension`, not just shipped in the image.
- Drove the extension's own popup over CDP (there's no automation hook for
  the real toolbar-icon popup, so the popup page was opened as an ordinary
  tab and pointed at a second, real tab instead of itself — `chrome.scripting`
  can't inject into an extension's own page or a `chrome://` page) with a
  simple task against `https://example.com`. The run went through the real
  `/step` loop against `devstral-small-2:24b` on the host and came back with
  the correct answer, `Example Domain` — a genuine end-to-end pass through
  extension → `ext_server.py` → Ollama → the live tab's DOM → the answer.

### How the TheBot skill (item 5) was verified

`internal/atomizer/triage.go` gained `wantsBrowser` (checked before
`wantsScreen`, same reasoning): an explicit "browse the web"/"browse to"
phrase, or a named destination (a URL, a bare domain, "go to/visit/open the
website") paired with something to actually do there (search, click, type,
fill, log in, add to cart, ...). It routes to a new `RouteBrowser` ->
`internal/skills/browser.go`'s `BrowserSkill`, which `POST`s the whole
request to `ext_server.py`'s `/requests` inbox, polls `/requests/status` for
that request's id, and reports the extension's own outcome back
(`done`/`answer` -> verified success; `blocked`/`paused`/`stalled`/`timeout`
-> `needsUserInput`, the same shape every other skill's failure already
takes). Registered in `cmd/soul/main.go` next to the operator. The planner's
tool vocabulary also gained a `[browser]` tag (`toolTypes`, `toolList`,
`planRules`) so a genuinely compound plan can route one of its steps here
too.

Verified live, through the real chat API end to end (not a direct CDP call
this time) - `POST /api/desktops/send` on three fresh `thebot`-harness
desktops:

- `"Open google.com. Search for OpenAI. Do that."` routed correctly
  (`atomizer: an interactive task in a real web page - handing it to the
  browser extension`, `scheduler: ... [browser] -> skill=browser`), and the
  extension genuinely opened Google, typed into the real search box, and hit
  Google's own bot-check page (`.../sorry/index?...`) - which the extension's
  own verify step correctly detected and reported as `paused` rather than a
  false success. Real anti-automation defense, not an integration bug.
- A similar Wikipedia search paused on a verify-timing race (the search
  itself landed correctly on the OpenAI article - `final_url` proved it -
  but the verify call's own page snapshot was taken a beat before Wikipedia's
  client-side redirect finished, so it judged against the stale homepage).
  Pre-existing behavior of the extension's own verify step, not something
  this integration changed.
- `"Browse the web to example.com and tell me the page's main heading."`
  came back as a normal chat reply, `Example Domain` - the correct answer,
  through the complete real path this time: chat -> TheBot triage -> the
  browser skill -> `ext_server.py` -> the extension -> the live tab -> the
  answer -> back through TheBot's own synthesis into the chat.

One unrelated environment gap surfaced by the first two runs: whenever the
browser skill's result is unverified, the scheduler's own failure-recovery
reasoning fires (as it would for any skill), and on this host that call
targets `qwen2.5-coder:3b` - the `thebot` harness's own default model
(`ollama.DesktopModel()`), which happens not to be pulled on this particular
Ollama (only `qwen2.5-coder:7b` is). That is a pre-existing model-
availability mismatch, unrelated to the browser skill itself, and never
surfaces at all when a browsing task succeeds cleanly (as the third run
did).

## Upstream rewrite: what changed in research/experiments/browser-agent

The research copy this was originally taken from was itself substantially
reworked afterward, independent of this integration, and those changes were
pulled in wholesale (`extension/background.js`, `popup.js`, `popup.html`,
`manifest.json`, `extension_pipeline/ext_server.py`, `ext_codegen.py`,
`README.md` all replaced with the newer versions; `core/models.py` kept
this repo's own `OLLAMA_URL` patch on top, since the source's copy had
reverted to a hardcoded address). The parts that matter here:

- **Always-on listening is now the extension's own default, not something
  Decurix bolted on.** A global `LISTENER` (not tied to any tab) persists
  its enabled/settings state via `chrome.storage.local`, and with no stored
  state at all (a fresh profile) it starts itself with
  `DEFAULT_LISTENER_SETTINGS` rather than sitting idle until a popup click.
  It re-arms on `chrome.runtime.onStartup`/`onInstalled` and once more at
  plain module load (covering a service-worker respawn Chrome triggers on
  its own). This is a strictly better version of the `chrome.alarms`-driven
  `ensureListening` this integration built to solve the exact same problem
  (item 7 in the checklist below) — that code is gone now, replaced by
  this, and nothing here needed to change to keep working with it.
- **Every request gets its own fresh tab** (`req.start_url || "https://www.google.com/"`),
  never a tab the popup happened to have open. This is also strictly better
  than this integration's own original fix for the same underlying problem
  (a fresh `chrome://newtab` can't be scripted, so the very first snapshot
  used to fail) — same fix, applied per-request instead of once at startup.
- **Two atomizer flavours**, selectable per run/listener: `live` (unchanged,
  the default) decides one instruction at a time from the live page; `stack`
  plans the whole task once up front (`core/stack_atomizer.py`, already
  present but unused before this) and grounds each planned step against the
  live page as it goes, cutting per-task model calls. The browser skill
  doesn't set `flavour` in its request body, so every TheBot-driven run uses
  the default (`live`) — adopting `stack` there is future work, not
  something this pass needed.
- **Per-step state capture and live streaming**: every step now captures a
  screenshot + URL/title/visible-text/HTML regardless of vision mode
  (`background.js`'s `stateOf()`), attached to each step and to
  `final_state` in the eventual `/requests/result`, and also pushed
  immediately via `POST /requests/{id}/step` for a remote viewer polling
  `GET /requests/{id}/live?since=N`. The browser skill's Go struct doesn't
  declare these fields and so silently ignores them (safe: unmarshalling
  extra JSON fields is a no-op) — surfacing live screenshots into the chat
  as a step runs is a natural follow-up, not implemented here.

Re-verified live after the port, through the real chat API again: `"Browse
the web to example.com and tell me the page's main heading."` on a fresh
`thebot` desktop came back as `Example Domain` — the same clean pass as
before the port, confirming the browser skill needed no changes to keep
working against the rewritten extension.

## Verification off, no retries, cancellation - live tuning after real use

Three changes made after actually chatting with a `thebot` desktop and
watching real browsing runs:

- **Step verification is off by default.** `extension/background.js`'s
  `DEFAULT_LISTENER_SETTINGS.verifyEnabled` and `popup.html`'s `verify`
  checkbox both flipped from on to off. Verification was a second model
  call after every action, judging whether it actually achieved the step's
  goal - roughly doubling steps-per-task, and the thing that turned a
  genuine success (Wikipedia's search really did land on the right article)
  into a false "paused" over a verify-timing race against the page's own
  client-side redirect (see the Wikipedia run further up this file). The
  action's own result (did the click/type/navigate itself succeed) is still
  checked either way - only the separate did-it-achieve-the-goal judgment
  is now opt-in.
- **A browse that doesn't fully finish is reported, not retried.**
  `internal/skills/browser.go`'s `report()` used to turn `blocked`/
  `paused`/`stalled`/`timeout` into a terse failure with `needsUserInput`,
  which sent every one of them through the scheduler's failure-reasoning
  call (`internal/scheduler/recovery.go`) - an extra model round-trip to
  reach a retry that, per the person actually using this, "rarely succeeds
  once it fails." Every outcome except "done with no answer" now goes
  through `summarizeBrowserRun()`, which compiles what the browser actually
  saw and did from the extension's own step-by-step log into one narrative,
  and the whole thing is reported as a **verified** answer - not a failure
  to recover from. Genuine infrastructure failures (`ext_server.py`
  unreachable, no request id, TheBot's own poll timing out with nothing to
  show) are the one case still marked `fatal: true` instead: nothing was
  observed, so there's nothing to compile, and there's equally nothing a
  same-approach retry would fix.
- **TheBot can actually cancel a browsing run, and the extension really
  stops.** `POST /api/jobs/{id}/cancel` (already existed - `cmd/soul`'s
  own job cancellation, and already called by `decurix_agent/
  thebot_harness.py._cancel_job` when the harness gives up waiting on a
  job) used to only ever stop TheBot's own side; the browser extension had
  no way to hear about it and kept running against the real page, orphaned.
  Now: `ext_server.py` gained `POST /requests/{id}/cancel` (flags a
  `pending` request done immediately; flags a `running` one for the
  extension to notice) and `GET /requests/{id}/cancelled` (a lightweight
  poll target). `background.js`'s `runListenerLoop` runs a 1s poll of that
  flag alongside the request it's processing, and reacts by both setting
  `run.stop` (so the loop's own top-of-iteration check catches it between
  steps) and aborting whatever model call is *currently in flight*
  (`run.currentAbort`, threaded through `getJSON`/`postJSON`'s new optional
  `run` argument via `callSignal()` - a manual `setTimeout`+`AbortController`
  pairing, since this desktop's Chromium 112 predates `AbortSignal.any`).
  `internal/skills/browser.go`'s `Run()` now calls this cancel endpoint
  (`cancelAndReport`, its own background context - `rc.Ctx` is already dead
  by the time this fires) the moment TheBot's own job context is cancelled,
  then gives the extension a short grace window to actually stop and post
  its real result, so what comes back is a genuine compiled account of
  whatever it saw before being cancelled - handled by the exact same
  `report()`/`summarizeBrowserRun()` path as any other outcome, which is
  also what makes "TheBot can still answer questions about what it
  observed" true: the compiled narrative is the skill's real output either
  way, cancelled or not.

Verified live: submitted a long, multi-step Wikipedia task directly to
`ext_server.py`, let it start (confirmed `status: "running"`, one step
already logged), then called `POST /requests/{id}/cancel` directly. The
request was `"status": "done"` with `result.status: "cancelled"` within
about a second - the poll interval's own granularity - with the real log up
to that point intact (`Navigate to wikipedia.org` succeeded, then the
in-flight second model call was the one actually aborted).
