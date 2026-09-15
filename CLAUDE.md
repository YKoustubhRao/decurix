# Flowera

Flowera is a control plane for spinning up disposable "desktop" VMs (Docker containers running a full XFCE desktop over VNC) and driving them with an autonomous AI agent. A Go server manages desktops via the Docker API; each desktop container runs a Python agent that watches for tasks, executes shell commands to get them done, and raises a query with a human through the control plane when it's stuck.

## Architecture

```
cmd/flowera/main.go — entrypoint: loads .env, builds the Docker client, wires
                       the packages below, registers routes, ListenAndServe(:8080);
                       --version, and --build-images/--rebuild which build the
                       desktop images from the embedded context and exit
embed.go           — package flowera at the MODULE ROOT: //go:embed of
                       docker/desktop.*.Dockerfile + agent/ → DesktopBuildContext.
                       It has to live at the root because go:embed cannot reach
                       above its own directory and the context spans two dirs.

internal/desktop   — owns the Docker client; desktop image build
                      (images.go: MissingImages/BuildImages, tars
                      flowera.DesktopBuildContext to the daemon); container CRUD
                      (Create/Start/Stop/Restart/Delete/List), image/network
                      ensure, SendRequest (POST to a desktop's :8081),
                      ProxyTarget (resolves the VNC reverse-proxy address),
                      AgentRequest (recordings.go: one call to a desktop's :8081 API,
                      how recordings are read off the desktop rather than copied),
                      PersistMemoryFile (docker-exec write into a desktop),
                      claudelogin.go: the interactive half of the shared Claude
                      Code login (StartClaudeLogin/SubmitClaudeLoginCode/
                      CancelClaudeLogin, driving `claude auth login` in a
                      throwaway container so the dashboard's popup can hand a
                      human its sign-in URL and feed back the verification
                      code) and PersistClaudeCredentials (PersistMemoryFile's
                      write-into-a-desktop pattern, seeding a new claude-harness
                      desktop with the session internal/claudeauth stored),
                      Screenshot (screenshot.go: a minimal RFB client that reads
                      a desktop's framebuffer straight off Xvnc:5901 and returns
                      a cached JPEG thumbnail — what the dashboard cards show),
                      sandbox.go (the seccomp profile + core-dump ulimit every new
                      desktop is created with — what lets Chromium run sandboxed;
                      the profile itself is seccomp-desktop.json, go:embed'ed)
internal/ollama    — DesktopURL/DesktopModel (the Ollama every desktop is given) and
                      preloading the model there at startup; also the lifecycle of the
                      bundled "flowera-ollama" container, used only when OLLAMA_URL names it
internal/sqlitedb  — opens the one embedded SQLite database (modernc.org/sqlite, pure Go,
                      so releases stay CGO_ENABLED=0) at FLOWERA_DATA_DIR/flowera.db, and
                      Migrate(db, component, steps): each store below owns its own tables
                      and appends schema steps, never edits an applied one
internal/querynotes — queries submitted by agents (SQLite)
internal/memory    — worker-memory entries (SQLite)
internal/conversation — per-desktop chat transcript: the requests and replies a human and a
                      desktop's agent exchanged (each tagged with its task ID and the
                      human's verdict), plus the live steps of the turn running right
                      now. Served from memory, written through to SQLite, loaded back
                      per thread on first use. What makes a desktop's chat the same on
                      every device
internal/stepimages — byte-bounded SQLite store of the before/after screenshots attached
                      to on-screen action steps (PNG in, JPEG out), served at
                      /api/desktops/step-image; the desktop keeps the full-size originals
internal/tasks     — the task records: Task → Execution → Steps → Interventions →
                      Outcome, per Worker (a desktop ID, kept after its container is
                      deleted). store.go routes a human request (Plan/Commit: new task,
                      reply to an open intervention, or retry of a failed task), attributes
                      steps and outcomes (by echoed task ID, else the worker's live
                      execution), verdicts, CloseOpen on stop/restart/delete. classify.go:
                      deterministic task type and intervention reason. report.go: autonomy
                      by engine/worker/type and the reasons people stepped in
internal/tunnel    — in-memory store of the current Cloudflare quick tunnel URL,
                      set by scripts/linux/build.sh or rebuild.sh via POST /api/tunnel,
                      surfaced in the dashboard's Mobile dropdown
internal/claudeauth — the one shared Claude Code login every claude-harness
                      desktop is seeded from (in place of an ANTHROPIC_API_KEY):
                      a single-row SQLite table holding the raw
                      ~/.claude/.credentials.json content plus the account's
                      email, so it survives a control-plane restart, build.sh
                      leaves it alone, and rebuild.sh wipes it for free as part
                      of wiping flowera-data — no script changes needed for
                      either half of that
internal/httpapi   — the only package touching net/http; all HTTP handlers,
                      composes desktop+querynotes+memory+conversation+stepimages+tasks+
                      tunnel+claudeauth+webui. claudeauth.go: the dashboard's Claude
                      login popup (/api/claude-auth/status + login/start + login/code
                      + login/cancel), backed by desktop.Manager's claudelogin.go and
                      saved through claudeauth.Store. recordings.go: the recordings page and the
                      /api/desktops/recordings* + /api/desktops/demonstration proxy, replay
                      as a task (Busy check, Plan/Commit, parameters never recorded), and the
                      ticket API (/api/desktops/record opens one, /api/desktops/ticket* drives
                      it - the GUI screen's own toolbar). proxy.go: desktopProxyHandler's
                      injectTicketToolbar splices that toolbar into every load of the screen
internal/webui     — html/template rendering (PageData, dashboard/requests/desktop-view
                      pages); parses out of web.TemplatesFS, never off disk.
                      render_test.go executes every page, so a broken template fails
                      the test run instead of the first request that reaches it
web/embed.go       — //go:embed templates/*.html → web.TemplatesFS and favicon.svg →
                      web.FaviconSVG; what makes the compiled binary standalone
                      (package web has no internal deps)
web/favicon.svg    — the brand mark (theme.html's #i-blossom on a filled tile), served
                      at /favicon.svg by httpapi.faviconHandler and linked from the
                      shared "theme" head partial, so it shows in the browser tab
web/templates/     — theme.html (shared design tokens + icon sprite + top bar +
                      chrome JS, pulled in by every page via {{template "theme"}} /
                      "sprite" / "topbar" / "chrome-js"; parsed alongside each page
                      by webui.templatePaths), dashboard.html, index.html,
                      requests.html, desktop.html (the per-desktop chat behind the
                      dashboard's Chat button — one chat for every harness, and
                      nothing else on the page; the screen opens in its own tab),
                      tasks.html (the autonomy dashboard, /tasks), task.html (one
                      task record with its steps, screenshots, interventions, verdict)
                      and recordings.html (/recordings/<id>: record a demonstration,
                      browse recordings step by step, replay, export)

desktop container (flowera-desktop:{basic,developer,ai})
  ├─ base image: accetto/ubuntu-vnc-xfce-chromium-g3 (XFCE + VNC + Chromium)
  ├─ XDG autostart (agent/flowera-agent.desktop) → agent/start_agent_terminal.sh
  │    → /opt/vm_server.py (detached background process, no visible terminal); it
  │    also reinstalls agent/chromium-browser.init as ~/.chromium-browser.init (the
  │    browser's launch flags) and clears core dumps Chromium left behind
  └─ flowera_agent/ (Python package, imported by vm_server.py; :8081 inside the container)
        ├─ config.py       — env vars (incl. AGENT_HARNESS), /home/headless/.flowera/
        │                     {command,querytext,final,memory} dirs and /home/headless/work
        ├─ state.py        — shared mutable state (current_task, waiting_for_query, ...) as
        │                     plain module attributes; every other module does
        │                     `from flowera_agent import state` and reads/writes
        │                     state.current_task etc. (module singleton = shared globals)
        ├─ identity.py     — get_identity_context(engine): who this desktop is (name, ID,
        │                     user, work dir), as facts for the system prompt. Shared by all
        │                     three harnesses - without it "what's your desktop name?" was
        │                     unanswerable and came back as a query
        ├─ memory_context.py — get_memory_context: reads ~/.flowera/memory/*.txt; shared
        │                     by all three harnesses below
        ├─ messaging.py    — save_request (intake), send_query/ask_for_query/write_final_reply
        │                     and backend_failure_note (turns a raw engine error into a note
        │                     that says what to do about it)
        │                     (POSTs to control plane /api/querytext, with kind+reply so the
        │                     chat needn't re-parse the note) — shared by all three harnesses
        ├─ readiness.py    — wait_for_http_ok(url, label): the one "is it up yet" probe,
        │                     shared by every harness that has a backend to wait for. Never
        │                     raises and never gives up - "not up yet" is a state to sit in
        ├─ task_context.py — the control plane's task ID for the request being worked on:
        │                     carried in the command file's name (<stem>.t<16 hex>.txt), set
        │                     by watchers.py, and written to /tmp/flowera-current-task so the
        │                     `gui` command and the MCP server (other processes) report it too
        ├─ usage.py        — per-task model/tokens/cost totals each harness adds to, sent by
        │                     messaging.send_query with every reply
        ├─ progress.py     — emit(): POSTs one live activity line to the control plane's
        │                     /api/agent/events, on a single background sender thread with a
        │                     bounded queue. Best-effort and harness-agnostic: this is what
        │                     makes the chat's step-by-step view identical for all three
        ├─ harness.py      — the ONLY place that branches on AGENT_HARNESS; re-exports
        │                     perform_task/continue_after_query from whichever harness below
        │                     is active, with identical signatures, plus wait_for_backend()
        │                     which blocks until that harness's engine is reachable and then
        │                     sets state.backend_ready - the flag watchers.py gates on
        ├─ ollama_client.py + parser.py + commands.py + task_runner.py
        │                   — the "ollama" harness: hand-rolled COMMAND:/DONE:/HELP: loop
        │                     driving a local Ollama model
        ├─ claude_harness.py
        │                   — the "claude" harness: shells out to the `claude` CLI in
        │                     non-interactive mode per task; Claude Code runs its own
        │                     agentic loop internally, so this just streams its events to
        │                     progress.emit and maps the final "result" event to
        │                     write_final_reply/ask_for_query, and tracks a session_id
        │                     (module-private state) to --resume across
        │                     ask_for_query ⇄ human-response round trips
        ├─ thebot_harness.py
        │                   — the "thebot" harness: TheBot's `soul` binary (agent/thebot/,
        │                     its own Go module, built into all three images and started as a
        │                     daemon by start_agent_terminal.sh) is a persistent HTTP server
        │                     with its own atomizer/scheduler/skills loop. This submits the
        │                     task, the standing context (identity + worker memory + approval
        │                     rules) and the work dir as separate fields to its /api/chat,
        │                     follows the job over its SSE stream (forwarding each log line to
        │                     progress.emit; polling is the fallback), and maps the job's
        │                     structured `outcome` to write_final_reply/ask_for_query. A
        │                     reply goes back with reply=true, which resumes the paused job
        ├─ computer/       — computer use: the ONE action layer all three engines share to see
        │                     and work the screen (see "Computer use" below). core.py has
        │                     observe/act/verify/open; screen.py (python3-xlib: framebuffer ->
        │                     PNG, window list), atspi.py (GTK/XFCE/LibreOffice widgets),
        │                     cdp.py (Chromium over DevTools, stdlib websocket), xinput.py
        │                     (xdotool), elements.py (one element shape, stable refs, the text
        │                     rendering), approval.py (the gate + pending/grant file store),
        │                     audit.py (every action into recordings + progress.emit_action),
        │                     cli.py (the `gui` shell command, agent/gui -> /usr/local/bin/gui),
        │                     mcp_server.py (the same as MCP tools for Claude Code), and the
        │                     recordings (see "Recordings" below): recording.py (the store),
        │                     privacy.py (what is never kept), volatility.py (text a replay
        │                     must not check), processing.py (raw input -> actions),
        │                     demonstration.py (a person's input over VNC, via XInput2),
        │                     replay.py (compile + run a recording with no model)
        ├─ record_task.py  — "/record <task>" opens a ticket: dispatched by the watcher instead
        │                     of the harness, holds the task open (module-private state, the way
        │                     replay_task.py holds its params) until end() finishes it, reports as
        │                     engine "human". begin()/pause()/resume()/end() are called directly
        │                     over HTTP by the GUI screen's own toolbar - not through the command
        │                     file/watcher path, since they only ever act on the ticket already open
        ├─ replay_task.py  — "/replay <recording>" as a task: dispatched by the watcher instead
        │                     of the harness, reports through messaging as engine "replay",
        │                     pauses/resumes on approval or a private step
        ├─ watchers.py     — command_watcher (polls COMMAND_DIR, dispatches through harness.py;
        │                     leaves a request queued in COMMAND_DIR while state.backend_ready
        │                     is False, rather than failing it)
        ├─ http_api.py     — Handler(BaseHTTPRequestHandler): POST /request, and the
        │                     recordings/demonstration/replay API the control plane proxies
        └─ app.py          — main(): wait for the backend, watcher thread, tidy recordings
                              (import the old audit trail, close an interrupted demonstration),
                              ThreadingHTTPServer.serve_forever
```

Control plane → desktop communication happens over the Docker bridge network `flowera-network` (container name `desktop_<id>`, not published ports). Desktop → control plane communication uses `CONTROL_PLANE_URL` (`http://host.docker.internal:8080`).

Go package dependency direction (no cycles): `httpapi` → `desktop`, `querynotes`, `memory`, `conversation`, `stepimages`, `tasks`, `tunnel`, `claudeauth`, `webui`; `webui` → `desktop`, `querynotes`, `memory`, `tasks`, `web`; `desktop` → `ollama` (only for `ollama.DesktopURL`/`DesktopModel`, the Ollama a new desktop is given), `flowera` (the root package, for the embedded build context); `querynotes`, `memory`, `conversation`, `stepimages`, `tasks`, `claudeauth` → `sqlitedb` only; `ollama`, `sqlitedb`, `tunnel`, `web`, `flowera` depend on nothing internal. Note that `desktop` does *not* depend on `claudeauth`: `httpapi.createDesktopHandler` reads the stored session and hands `Create` its raw credentials JSON as a plain string, and `claudelogin.go`'s login flow hands the same shape back to `httpapi` to save — `desktop` only ever sees bytes, never the store.

Three desktop profiles share the same `agent/` contents but differ in installed tooling:
- `docker/desktop.basic.Dockerfile` — VNC desktop only (no `claude` binary — Ollama harness only)
- `docker/desktop.developer.Dockerfile` — adds LibreOffice, git, build tools, Node, Go, Rust, gh, native `claude` binary
- `docker/desktop.ai.Dockerfile` — developer image + `@openai/codex`, native `claude` binary (same install as developer, not the npm package)

### Three agent harnesses

Each desktop picks a harness at creation time (`Desktop.Harness`, `desktop-harness` container label, `AGENT_HARNESS` env var passed into the container — see `internal/httpapi/desktops.go`'s `createDesktopHandler` and `internal/desktop/docker.go`'s `buildDesktopEnv`). All three reach the exact same command/querytext/memory/progress contract through `harness.py`'s dispatch:

- **`ollama`** (default): the hand-rolled `COMMAND:`/`DONE:`/`HELP:` loop in `task_runner.py`, driving an Ollama model.
- **`claude`**: requires the developer or ai profile (`claude` binary isn't installed in basic) and a shared Claude Code login completed once through the dashboard's popup (see `internal/claudeauth` and `internal/desktop/claudelogin.go` above) rather than an `ANTHROPIC_API_KEY` in `.env` — `httpapi.createDesktopHandler` reads the stored session and `desktop.Manager.Create` seeds it into the new container's `~/.claude/.credentials.json` (`PersistClaudeCredentials`) only when the harness is `claude` and someone has logged in. Runs `claude -p "<task>" --output-format stream-json --verbose --dangerously-skip-permissions --append-system-prompt "<memory + safety rules>"`, optionally `--resume <session_id>` to continue after a human responds. No `--bare`: that flag forces API-key-only auth ("OAuth and keychain are never read" — confirmed against the CLI's own `--help`), which is exactly what this harness now needs to *not* do; the trade-off is that `--bare`'s other isolation (hooks, LSP, plugin sync, attribution, auto-memory, CLAUDE.md auto-discovery) is back on too, same as running `claude` anywhere else — none of it matters inside a desktop's empty `~/work`. `--dangerously-skip-permissions` is intentional and matches the Ollama harness's existing unrestricted shell access (`commands.run_command`) — not a new risk category, just the same one applied to a second engine. `stream-json` (which *requires* `--verbose`) is what lets the turn be watched as it happens; its final `result` event carries the same fields the one-shot `json` format returned, so only `_run_claude` knows the difference.
- **`thebot`**: TheBot's `soul` binary, built from `agent/thebot/` (its own Go module) into all three images and started as a daemon on port 4900 when `AGENT_HARNESS=thebot`. Reuses the same `OLLAMA_URL`/`OLLAMA_MODEL` every desktop already gets, so it needs no env var of its own. Inside soul a job goes: **recall** (L1 facts) → one shared **system frame** (`internal/prompt`: persona + host context, byte-identical for every model call in the job so Ollama's prompt cache carries across them) → **triage** (`internal/atomizer/triage.go`, deterministic: `chat` / one `skill` / `operate` / `plan`) → a tagged step **graph** from the planner only for `plan` (`[do] [shell] [web] [read] [download] [write] [code] [docs] [answer]`, with `(after N)` dependencies) → the **scheduler** (`internal/scheduler`) runs ready steps in parallel under a process-wide model-concurrency limit (`llm.Limited`), recovers a failed step (retry / decompose / ask), replans the rest once if it still fails, and **pauses** with a `Checkpoint` when a step needs approval or an answer → one **synthesised reply** for multi-step jobs. `operate` is the **operator** skill (`internal/skills/operator.go`): a Go `COMMAND:`/`DONE:`/`HELP:` loop carrying the Ollama harness's measured lessons, which is what lets TheBot answer "what's the kernel version?" by running `uname -r` instead of planning a web search. Consequential commands and requests are stopped by `internal/safety` before they run, whatever the model decided.
- All three system prompts (`SYSTEM_PROMPT` in `ollama_client.py`, `SAFETY_SYSTEM_PROMPT` in `claude_harness.py`, `SAFETY_PREAMBLE` in `thebot_harness.py`) instruct the agent to stop and request human approval — via the same `HELP`/`ask_for_query` → querytext path — before payments, sending messages on the human's behalf (WhatsApp/email/SMS), or any action with real-world consequences it isn't fully confident about. Claude and TheBot signal it with a `NEEDS_APPROVAL:` prefix on their final answer; the Ollama loop uses its `HELP:` control signal.

### One chat, every harness

`/view/<id>` (the dashboard's **Chat** button) renders `web/templates/desktop.html`, which is the chat and nothing else — no embedded screen, identical for all three harnesses. A desktop's screen is a separate thing, opened in its own tab from the dashboard's open-in-a-new-tab button (the only emitter of `?ui=touch`; see the `injectMobileEnhancements` note below - the ticket toolbar described next is unconditional, like the branding rewrite, since noVNC's page loads for no other reason). Nothing in the chat page branches on which engine is running, because every harness is addressed and reports back the same way:

```
human types in the chat
  → POST /api/desktops/send        → tasks.Plan (new task / reply / retry → a task ID)
                                   → desktop.Manager.SendRequest(content, task_id) → :8081
                                     → COMMAND_DIR/<stem>.t<id>.txt → watchers.py
                                     → task_context.start(id) → harness.py → engine
                                   → tasks.Commit + conversation.AppendUserRequest

the agent works
  → progress.emit(line) → POST /api/agent/events {task_id} → conversation.AppendStep
                                                           → tasks.AddStep (the record)

the agent finishes, or needs a human
  → messaging.write_final_reply / ask_for_query(kind) → POST /api/querytext
        {task_id, kind: final|error|query|approval, engine, model, tokens, cost_usd}
      → querynotes.Add          (the Query rail keeps the whole formatted note)
      → tasks.RecordOutcome     (done/error closes the execution; query/approval opens
                                 an intervention the next human request replies to)
      → conversation.AppendAgentReply (the chat shows only the agent's own words)

the human judges the answer
  → POST /api/tasks/verdict (chat's Correct/Wrong, or the task page) → tasks.SetVerdict
                                                                    → conversation.SetVerdict
```

The chat page holds no transcript of its own: it renders whatever snapshot the control plane last sent over `GET /api/desktops/chat/stream` (SSE, whole snapshots rather than deltas, so a reconnect is self-healing), falling back to polling `GET /api/desktops/chat`. That is what makes one desktop's conversation the same on a laptop, on a phone through the tunnel, and in a second window — and why a desktop's agent is genuinely *central* to it, rather than one chat box per browser.

Replying to a blocked agent is just sending it another request: `watchers.command_watcher` routes any request arriving while `state.waiting_for_query` is set to `continue_after_query` instead of starting a new task. So the chat, the Query rail's reply box and the Requests page are three doors into one conversation.

### Computer use: one action layer, every engine

Every desktop can see and act on its own screen through `flowera_agent/computer`. The layer lives in the desktop, below the engines, so the approval gate and the audit trail cannot be skipped by any of them:

```
observe()  screenshot (python3-xlib) + windows + elements: AT-SPI for GTK/XFCE/LibreOffice,
           the Chrome DevTools Protocol for Chromium (its AX tree + one DOMSnapshot for boxes)
act()      observe -> resolve the target (ref | role+name | path | x,y) -> approval.check
           -> do it (CDP input / AT-SPI action or caret / xdotool) -> wait for the screen to
           settle -> observe -> compare -> audit.record
verify()   observe again, compare with the last observation, optionally check text/element
```

How each engine reaches it:

- **Claude** gets them as MCP tools (`mcp__computer__observe/act/verify/open`) via `claude_harness.MCP_CONFIG` (verified: the stream's init event lists the server `connected` with all four tools). Every result carries the screenshot, because Claude can read one.
- **Ollama** and **TheBot** use the `gui` shell command through their existing `COMMAND:` loops (no contract change) and read the element list **as text only**. A 3B model on a CPU cannot read pixels; `elements.render` is budgeted to `FLOWERA_GUI_TEXT_CHARS` (3500) because `task_runner` shows a model at most 4000 characters of any command's output. TheBot's `atomizer.Triage` routes screen work (`wantsScreen`: a surface *and* an action) to the operator, ahead of the web skill that would otherwise fetch any URL itself.

`gui` exit codes are a contract harnesses read: 0 done, 1 could not, **3 NEEDS_APPROVAL (nothing happened)**, 4 verify failed.

Approval travels the same way for all three: `approval.check` stops a click on a Send/Pay/Buy/Publish-like control (or Enter in a messaging page), records a *pending* approval and returns NEEDS_APPROVAL. The harness pauses deterministically — `task_runner.pause_for_screen_approval` on exit 3, `operator.screenApprovalReason` → `NeedsApproval` checkpoint, `claude_harness._handle_result` if a pending approval exists whatever Claude wrote. The human's reply reaches `watchers.command_watcher`, which calls `approval.resolve_reply` **before** the harness sees it: a yes (`is_approval`, the same regexes as soul's `safety.IsApproval`) becomes a one-shot grant fingerprinted to that action; anything else drops it. The repeated action consumes the grant. A new task calls `approval.reset()`.

Every action — done, failed, or paused — is recorded twice: as an event of the task's recording on the desktop's persistent volume (see "Recordings" below), and as a chat step via `progress.emit_action` → `POST /api/agent/events` with `action` + half-size `before_png`/`after_png` → `conversation.AppendActionStep` + `stepimages.Store`. The chat renders those steps with Before/After thumbnails. Both copies go through `privacy.py` first.

### Recordings: demonstrations, the audit trail, and replay

Built on OpenAdapt's design (openadapt-capture's recorder and secret exclusion, openadapt-flow's recording layout, volatility classifier, resolution ladder and run outcomes), on Flowera's own action layer. One format holds everything done on a desktop's screen, whoever did it:

```
~/.flowera/recordings/<session>/          task-<task id> | adhoc-YYYYMMDD | demo-<when>-<rand>
  meta.json      kind, title, task_id, started/ended, screen, actors, counts, status (demos)
  events.jsonl   one action per line: i, t (seconds on the session clock), actor (agent|human),
                 kind, element (role/name/path, no backend handle), text or withheld{},
                 before/after frame names, change (sentence) + expect (the same as data)
  frames/        PNGs named by a hash of their pixels - an "after" that is the next "before"
                 is stored once
```

- **Agent actions** get there through `audit.record` (from `core.act` / `open_target`), under the session of `task_context.current()`; the task's request titles it (`task_context.current_request`, beside the task ID file).
- **A person's demonstration** is `demonstration.py`, a process of its own started by `POST /demonstration/start` on the agent: XInput2 raw events → `processing.merge` (openadapt-capture's click/double-click/drag/scroll/shortcut rules, plus one `type` per run of characters with BackSpace applied) → the element under each action from the last observation of a settled screen → `recording.append` with `actor: "human"`. **Pause** drops all input and frames for a private step (a sign-in) and records a `manual` step on resume, which a replay hands back to a person.
- **Replay** is `replay.compile_program` + `replay.run`: typed text becomes parameters (secret ones have no default and must be given), each step's target is found again by path then role+name (never guessed: none or two halts), coordinates only for a step recorded without an element on the same screen size and window, and each step waits for what it left on screen when recorded (a URL shape with id-like segments as `*`, a window, a dialog/heading/alert, a field holding the typed text), with volatile text never checked. Outcomes, never a success it did not see: `verified`, `completed_unverified`, `halted_before_effect`, `halted`, `reconciliation_required` (a consequential step ran and its result never appeared — never retried), `needs_approval`, `needs_human`. Every replayed step goes through `core.act`, so the approval gate and the recording apply (`replay_of` on each event).
- **A ticket: a person doing a task, recorded from the GUI screen itself.** The screen's own top-of-page toolbar (`proxy.go`'s `injectTicketToolbar`, spliced into every load of noVNC's `vnc.html` the way `rewriteFloweraBranding` is) is a `+` that opens a ticket and, once one is open, Record/Stop/Continue/Reason/End. `+` sends the typed task as `/record <task>` through `POST /api/desktops/record` — refused while the agent is busy, like a replay — and it is otherwise an ordinary request (Plan/Commit, in the transcript, doesn't wait for the backend, neither does `/replay`). The watcher dispatches it to `record_task.perform`, never the engine. **Opening a ticket does not itself start capturing** — Record does, so a person can create the ticket, walk over to the screen, open the app they'll use, and start only when actually ready. From there Stop is `demonstration.pause` (a private step, e.g. signing in) and Continue is `resume`; Reason (`record_task.note` → `demonstration.note`) appends a person's own written explanation of what they are doing to the recording — its own event kind, `note`, never compiled into a replay step (`replay.compile_program` skips it outright, not merely leaves it out of `REPLAYABLE`, so it can never reach a replay's "cannot note" halt) and, like a typed field, withheld rather than kept if the text itself looks like a credential. **The toolbar pauses before Reason opens its text box, not after submitting it** — clicking Reason calls the same `pause` action Stop does (so nothing on screen is kept while the explanation is composed, not just while it's read back), and submitting calls `note`, which — whether it finds recording already paused (this pause, or an earlier Stop) or not — always writes the note and then (re-)pauses, `demonstration.note` calling `pause()` itself with the note's own (privacy-safe) summary as the reason, refreshing it if one was already set. `note` no longer refuses a paused call — that used to be its only guard; now, whichever pause it lands in, it just rewrites the reason and stays paused. Whether the toolbar then continues automatically is its own call, not `note`'s: the toolbar tracks whether *its own* Reason click is what paused things (`reasonAutoPaused`) and only follows a successful submit with an explicit `resume` when it was — a pause already in effect before Reason was opened (Stop, pausing for something the note is *about*) is left exactly as the person set it, since auto-continuing could hand the very moment the note was warning about straight back to the recording. End (`record_task.end`) stops capturing if it had started and finishes the ticket's task, which is what makes the recording's export downloadable — the toolbar offers that link the moment End answers. Every one of Record/Stop/Continue/Reason/End is a direct call to `/api/desktops/ticket/action` (`ticketActionHandler`), not a command file: they only ever act on whatever ticket `record_task.py`'s own state already has open, so they don't go through the watcher. `GET /api/desktops/ticket` (`ticketStatusHandler`) is what the toolbar polls (every open tab, and any Record/Stop click, keep this and the recordings page's own view in sync) — deliberately separate from the recordings page's own ad-hoc "Record a demonstration" (`POST /demonstration/start`, no ticket, no task), so the two can never fight over the same session. Its task type is `demonstration` (`tasks.IsRecordCommand`), which an on-screen step never turns into `screen` and `tasks.Report` leaves out of autonomy — no engine attempted it.
- It is reachable three ways: the recordings page's Replay button (control plane → `POST /recordings/<id>/replay` → a `/replay <id>` command file → `replay_task.perform`, a task with engine `replay`), typing `/replay <id>` into the chat, and by an engine: `gui recordings [words]` / `gui recording <id>` / `gui replay <id> [--param n=v] [--from N]`, or the MCP tools `recordings` / `replay`. `gui replay` exit codes follow the gui contract (3 on approval, 4 on reconciliation_required).
- **Privacy is applied at the source** (`privacy.py`): a value typed into a password field (AT-SPI `password text`, `input[type=password]`, a secret `autocomplete`, a field labelled for a secret) or text shaped like a credential (API keys, tokens, private keys, Luhn-valid card numbers) is withheld with a reason, never rewritten; secret fields' boxes are painted over in saved frames; URLs lose userinfo and secret-named parameter values; a value over 512 characters is omitted, not cut. The engine is handed `[text withheld: password field]` too.
- The control plane never copies recordings: `desktop.Manager.AgentRequest` proxies `/api/desktops/recordings*` and `/api/desktops/demonstration` to the desktop's `:8081` (the page is `/recordings/<id>`, linked from each dashboard card and from a task that acted on screen). Recordings of tasks and hand-run actions are pruned oldest first past `FLOWERA_RECORDINGS_MB` (default 1024); demonstrations are never pruned.

The benchmark is `bench/gui/`: 20 tasks (12 in a web admin panel, 5 in LibreOffice, 3 in GTK apps, 3 of them approval checks), fixtures and outcome checks that run inside a desktop (`fixtures/bench.py`), and `run.py`, which drives each engine through the control plane's chat API on a fresh desktop and reports success rate per engine. `--engine oracle --container <desktop>` runs each task's scripted `gui` solution with no model — the proof that a task is solvable and its check is right. Results go to `bench/gui/results/` (gitignored).

## Running it

```bash
./scripts/linux/setup.sh         # one-time host setup: Docker, cloudflared, QEMU (needs sudo)
./scripts/linux/build-images.sh  # builds the three flowera-desktop:* images and nothing else
./scripts/linux/build.sh         # "open the app": builds only missing images, starts the control
                                  # plane if it isn't already running, waits for :8080, opens a
                                  # tunnel. Leaves existing desktop_* containers/volumes and the
                                  # flowera-data volume (the DB) untouched
./scripts/linux/rebuild.sh       # "reinstall": removes every desktop_* container/volume, and
                                  # (after a confirmation prompt, -y to skip it) deletes the
                                  # flowera-data volume - task history, chats, worker memory, gone
                                  # for good. Rebuilds every image and the control plane too (so
                                  # agent/ or Dockerfile changes land), but with the normal build
                                  # cache, not --no-cache - stays as fast as build.sh
./scripts/release/release.sh     # cross-compiles standalone executables + archives into dist/
docker compose up -d             # just the control plane (uses docker/Dockerfile, mounts /var/run/docker.sock)
go build ./... && go vet ./... && go test ./...   # compile, lint, unit tests
                                  # (internal/memory, internal/conversation, internal/webui,
                                  #  internal/stepimages, internal/httpapi, internal/tasks,
                                  #  internal/sqlitedb, internal/claudeauth)
PYTHONPATH=agent python3 -m unittest discover -s agent/tests   # computer-use gate/refs/rendering,
                                  # task IDs and usage totals, recordings (input merging,
                                  # privacy, volatility, the store, replay against a fake screen)
python3 bench/gui/run.py --engine oracle --container desktop_<id>   # GUI benchmark, no model
python3 bench/gui/run.py --engine ollama --engine thebot --engine claude  # per-engine success rate
```

`scripts/` is split by who runs it: `linux/` (`setup.sh`, `build-images.sh`, `build.sh`, `rebuild.sh`, `quickstart.sh`, `test-vm.sh`, plus a `lib/ollama-setup.sh` the last two source rather than duplicate), `windows/` (the same as `.ps1`, with `lib/Ollama-Setup.ps1`), and `release/` (`release.sh` + `release.ps1`). Each `.sh` has a `.ps1` twin at the same position in the other directory — they are hand-maintained, not generated from each other, so a change to one needs the same change to the other; the `lib/` sharing is within a platform only; `build.sh`/`build.ps1` and `rebuild.sh`/`rebuild.ps1` each source it independently rather than duplicating the ~200-line "find an Ollama the desktops can reach" logic. Every script resolves the repo root from its own location, never from `$PWD`.

### Distributable builds

`scripts/release/release.sh` / `release.ps1` cross-compile `cmd/flowera` for linux, windows and darwin × amd64/arm64 with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<version>"`, writing to `dist/`: a bare binary per target, a per-target archive (`.zip` for windows, `.tar.gz` otherwise), and `SHA256SUMS`. Version defaults to `git describe --tags --always --dirty`, and `main.version` backs `flowera --version`.

The binary is fully self-contained: static (`CGO_ENABLED=0`, no libc — which is why the SQLite driver is modernc.org/sqlite, not mattn/go-sqlite3), web templates from `web/embed.go`, desktop build context from `embed.go`, and `.env` optional. The one file it *writes* is `flowera.db` in `sqlitedb.DataDir()` (`FLOWERA_DATA_DIR`, else `os.UserConfigDir()/flowera`; the Docker image sets `/data`, which `docker-compose.yaml` mounts as the `flowera-data` volume). So an archive carries **nothing the binary depends on** — only a generated `README.txt` and an `.env.example`. `stageBundle`/`Copy-Bundle` branch on `goos` purely to write that README with one OS's commands (`./flowera` vs `.\flowera.exe`).

Keep it that way: any new runtime file dependency must be `go:embed`ed, not added to the bundle. If you ever have to put a real dependency in the archive, the "single executable is the whole product" claim in README.md needs to change with it.

See [README.md](README.md#-getting-started-running-it-locally) for full step-by-step setup instructions. Quick reference on env vars:

**Set by a human, in the control plane's own `.env`** (loaded via `godotenv` in `cmd/flowera/main.go`; see `internal/desktop/docker.go` for where each is read):

| Var | Read in | Default | Notes |
|---|---|---|---|
| `DEFAULT_PASSWORD` | `docker.buildDesktopEnv` | `admin` | Becomes `VNC_PW` in every new desktop container. |
| `OLLAMA_URL` | `ollama.DesktopURL` | `http://host.docker.internal:11434` | Passed into every new desktop by `buildDesktopEnv`, and the server `ollama.EnsureReady` preloads. `ollama.BundledURL` makes the control plane run and provision `flowera-ollama` itself. |
| `OLLAMA_MODEL` | `ollama.DesktopModel` | `ollama.Model` (`qwen2.5-coder:3b`) | In `.env.example`. `scripts/linux/build.sh`/`rebuild.sh` pull it when the server lacks it, and only fall back to a model the server already has if that pull fails. |
| `DESKTOP_STOP_TIMEOUT_SECONDS` | `desktop.Manager.Stop` | `10` | Parsed with `strconv.Atoi`; invalid/`<=0` values are ignored. |
| `FLOWERA_DATA_DIR` | `sqlitedb.DataDir` | `os.UserConfigDir()/flowera` (`/data` in `docker/Dockerfile`) | Where `flowera.db` lives. |
| `FLOWERA_STEP_IMAGES_MB` | `cmd/flowera.stepImagesMaxBytes` | `1024` | Byte budget of `stepimages`; oldest screenshots evicted first. |

**Set automatically per-desktop** (baked into the container's env by `buildDesktopEnv` at creation time — not something you set in `.env`, listed here for wiring reference): `VNC_PW`, `VNC_RESOLUTION` (hardcoded `1280x720`), `DESKTOP_ID` (`sanitizeName(name)` — must stay equal to `Desktop.ID`, see the gotcha below), `DESKTOP_NAME`, `CONTROL_PLANE_URL` (hardcoded `http://host.docker.internal:8080`), `OLLAMA_URL` and `OLLAMA_MODEL` (the control plane's own env when set, otherwise `http://host.docker.internal:11434` + `ollama.Model` (`qwen2.5-coder:3b`) — the Ollama on the host running the control plane, reached over the same `ExtraHosts` entry as `CONTROL_PLANE_URL`; that host's Ollama must listen on more than loopback, i.e. `OLLAMA_HOST=0.0.0.0`), `AGENT_HARNESS` (`ollama`/`claude`/`thebot`, from the harness chosen at creation).

**Claude-harness-only, read directly by `claude_harness.py`** — these currently have no UI/`.env` wiring (no `buildDesktopEnv` entry), so they're fixed at their defaults unless you add them to `buildDesktopEnv` or set them on the container yourself:

| Var | Default | Purpose |
|---|---|---|
| `CLAUDE_TASK_TIMEOUT_SECONDS` | `3600` | Max seconds a single `claude -p` invocation may run before being killed. |
| `CLAUDE_MAX_BUDGET_USD` | `5` | Passed as `--max-budget-usd` to `claude`, capping spend for one invocation. |

**Ollama pacing**, read inside the container rather than by `buildDesktopEnv` — set them on the container if the defaults are wrong for your hardware. The defaults assume the worst case this actually runs on, a CPU-only host:

| Var | Read in | Default | Purpose |
|---|---|---|---|
| `OLLAMA_TIMEOUT_SECONDS` | `ollama_client.py` | `300` | How long the `ollama` harness waits for one `/api/chat`. |
| `OLLAMA_TIMEOUT_MS` | TheBot's `llm.NewOllama` | `300000` | A *floor*, not a replacement: raises any per-call `Options.TimeoutMs` shorter than it, and leaves longer ones alone. TheBot's own budgets (60s to plan, 90s to answer) were chosen against a GPU and all expire in sequence on a CPU. |
| `OLLAMA_KEEP_ALIVE` | `ollama_client.py`, TheBot's `llm.NewOllama` | `30m` | How long Ollama holds the model in memory. See the prompt-cache gotcha below — this is the single biggest lever on how fast a desktop feels. |
| `EMBED_MODEL` | TheBot's `config.Load` / `llm.NewOllama` | `nomic-embed-text` | The embedding model, a separate dependency from the chat model. |

**Recordings**, also read inside the container:

| Var | Read in | Default | Purpose |
|---|---|---|---|
| `FLOWERA_RECORDINGS_DIR` | `computer/recording.py` | `~/.flowera/recordings` | Where recordings live. |
| `FLOWERA_RECORDINGS_MB` | `computer/recording.py` | `1024` | Disk budget; task and hand-run recordings are pruned oldest first past it, demonstrations never. |
| `FLOWERA_REPLAY_FIND_SECONDS` | `computer/replay.py` | `12` | How long a replay step looks for its target before halting. |
| `FLOWERA_REPLAY_EXPECT_SECONDS` | `computer/replay.py` | `10` | How long a replay step waits for its expected result. |

## Conventions / gotchas

- `CODE_MAP.md` is auto-generated (ctags + LLM summaries) — don't hand-edit it; it's regenerated from source after edits.
- Go doc comments: every exported/significant func gets a one-line `// FuncName does X` summary directly above it — keep following that pattern for new functions.
- The agent's output must start with exactly one of `COMMAND:` / `DONE:` / `HELP:` (see `ollama_client.SYSTEM_PROMPT`) — `parser.parse_agent_response` searches for whichever marker appears first, so don't change that contract without updating both sides.
- The desktop agent (`vm_server.py`) is launched as a detached background process by `agent/start_agent_terminal.sh` (via `setsid ... &`), not inside a visible terminal — output goes to `/tmp/flowera-startup.log` inside the container. Don't reintroduce a foreground/`xfce4-terminal --hold` launch; it makes the agent visible as a window on the VNC desktop instead of running silently.
- `flowera_agent/state.py`'s module attributes (`current_task`, `waiting_for_query`, etc.) are the agent's only cross-module shared state. Mutate them as `state.current_task = x` after `from flowera_agent import state` — never `from flowera_agent.state import current_task`, which copies the value instead of sharing it.
- `EnsureImages` no longer pulls. `flowera-desktop:*` are local-only tags that exist in no registry, so the old `ImagePull` could only ever fail with "pull access denied" on a fresh machine; it now just reports which images are missing and points at `--build-images`. Don't reintroduce a pull unless the images actually get published somewhere.
- `embed.go` sits at the module root on purpose — `go:embed` cannot reference parent directories, and the desktop build context spans `docker/` and `agent/`. Anything a desktop image needs at *docker build* time must be under one of those two trees and matched by an `embed.go` pattern, or the image builds fine from the repo and fails from a released binary. `go:embed` also silently skips names starting with `_` or `.`, which is what keeps `agent/__pycache__` out — but would equally skip a real file you name that way.
- `desktop.BuildImages` streams the embedded context to the daemon as a tar built on an `io.Pipe`, and `tar.Header` mode is hardcoded to `0644` because `embed.FS` carries no permissions. The desktop Dockerfiles already `chmod +x` what they need — a new script that relies on its executable bit surviving the tar will not work.
- The web templates are compiled into the binary via `web/embed.go`; `webui` parses them with `template.ParseFS(web.TemplatesFS, ...)`. Paths inside an `embed.FS` are always forward-slashed, so join them with `path.Join`, never `filepath.Join` (which would emit backslashes on a Windows build and fail to resolve). Adding a new template under `web/templates/` is picked up by the existing `//go:embed templates/*.html` glob, but a template in a *subdirectory* needs the glob widened.
- `cmd/flowera`'s `loadEnv` looks for `.env` in the working directory first, then next to the executable — that second lookup is what lets a downloaded binary find the `.env` shipped beside it. `docker compose` runs are unaffected (they get `.env` at the image's `WORKDIR`).
- `docker/Dockerfile`'s runtime stage only copies the binary and `.env` — the templates ride along inside the binary. It still needs a `.env` to exist at build time; `docker compose up` fails without one.
- `desktop.sanitizeName` normalizes user-supplied desktop names into safe container/volume name fragments (lowercase, `[a-z0-9_-]` only) — every `desktop.Manager` method already routes IDs through it; don't bypass it when adding new Docker API calls.
- `Desktop.URL` (`desktop.desktopURL`) is root-relative (`/desktop/<id>/`) on purpose, so the dashboard's preview iframe and "open" button resolve against whatever origin the page was loaded from — localhost, a LAN IP, or the Cloudflare tunnel. Don't rebuild it as an absolute `http://<host>:8080/...` URL: the old `DESKTOP_HOST`-based version pointed phones on the tunnel at their own localhost, and an `http://` iframe inside the tunnel's `https://` page is blocked as mixed content anyway. `index.html` resolves it to an absolute URL client-side only for display/copy. The trailing slash (no `vnc.html` in the visible URL — end users shouldn't need to know noVNC is involved) is rewritten to the underlying `vnc.html` internally by `httpapi.desktopProxyHandler`. noVNC (1.6) builds its WebSocket URL relative to the page too, so `/desktop/<id>/websockify` rides the same proxy.
- `desktop.Manager`'s, `conversation.Store`'s, `stepimages.Store`'s and `tasks.Store`'s mutexes each guard one struct's own state — hold the lock for the whole read-modify-write, not just parts of it. `querynotes`/`memory` have none: each of their operations is one SQL statement.
- **The SQLite pool is one connection** (`sqlitedb.open`). It removes SQLITE_BUSY between writers, and it means **no query may run while a `*sql.Rows` is still open** — that deadlocks the pool, silently, in production and in tests alike. Every store reads rows to the end (and closes them) before its next statement; keep it that way.
- A schema change is a **new step appended** to that component's `schema` slice, never an edit to an applied one: `sqlitedb.Migrate` records how many steps each component has run and only runs the rest.
- **What the task records trust is the task ID the desktop echoes, then the worker's live execution.** `tasks.resolve` takes the reported ID only while that execution is live or ended within `lateWindow`; a stale ID (the task file still names a finished task) falls through to what is live, and a step with no live task is *dropped* rather than inventing a task — a `gui` command typed by hand is not something anyone asked for. A reported ID is checked with `tasks.ValidID` before use.
- **`tasks.Plan` mirrors the desktop's watcher, and must keep doing so.** A request sent while an intervention is open is recorded as its reply, because `watchers.command_watcher` hands any request arriving while `waiting_for_query` to `continue_after_query`. The less obvious half is `absorbQueuedReply`: a request *held behind* a running task that then pauses is consumed by the watcher as the reply too, so the queued "task" is deleted and its text becomes the intervention's reply. Change the watcher's routing and the records will quietly describe something that did not happen.
- Stopping, restarting or deleting a desktop calls `tasks.CloseOpen`: a running task becomes `abandoned`, one waiting on a human ends as `query`. Without it, the next request after a restart was routed as a reply to a pause the fresh agent had no memory of.
- Resending a failed task's exact words within `retryWindow` is **another execution of the same task** (the failure note literally says "send the request again"), so a task needing a resend is never counted autonomous.
- **Autonomy is defined once** (`tasks.AutonomyDefinition`, shown verbatim on the dashboard): only attempt ended done, no query or approval asked, not marked wrong. Approvals count as stepping in — they are the agent behaving, but a person was still paged — and the reasons table keeps them apart (`IsApprovalReason`). Task types and reasons are deterministic regexes with tests (`classify_test.go`); an on-screen action turns a task into `screen` work unless it is `consequential`.
- Clearing a chat no longer deletes its screenshots: the task records still point at them. `stepimages` only forgets by its byte budget.
- `ask_for_query(problem, kind="approval")` for a safety pause, plain `ask_for_query(problem)` for being stuck — every harness's approval path passes the kind (`task_runner.pause_for_screen_approval`, both `NEEDS_APPROVAL` branches, soul's `needs_approval`, a pending screen approval in `claude_harness`). The Ollama model's own `HELP:` can still be an approval; `tasks.InterventionReason` reads the words for that case.
- Claude's `total_cost_usd` is summed across a task's runs (the task and each `--resume`), on the assumption that each result is that process's own spend. That is not verified against a live resumed run: searching the 2.1.266 CLI found no cost restore on a print-mode resume, but did not prove there is none. If resumed runs turn out to report session totals, `claude_harness._record_usage` double-counts the cost of a task that paused.
- **The dashboard's Claude login popup drives the real `claude auth login --claudeai`, not a reimplementation of OAuth.** `desktop.Manager.StartClaudeLogin` runs it in a throwaway container (the developer image, entrypoint overridden straight to `claude` — the desktop's normal tini/startup.sh never runs), attached with `Tty: false`. That matters: confirmed live, a `Tty: true` attach gets the CLI's colored/OSC8-hyperlinked output (the sign-in URL appears twice, once inside an OSC8 escape), while a plain non-tty pipe gets exactly `Opening browser to sign in…` / `If the browser didn't open, visit: <url>` / `Paste code here if prompted > ` with no escape codes to strip — the second is what `extractClaudeSignInURL` parses. `stdcopy.StdCopy` demuxes the non-tty stream into one combined buffer; parsing waits for the `Paste code here if prompted` prompt as a sync point before trusting the `visit:` line is fully flushed. There is one shared login attempt for the whole app at a time (`Manager.claudeLogin`), matching one shared session; starting a new one tears down whatever was in progress. A rejected code (confirmed live: `Invalid code. Please make sure the full code was copied.`) leaves the container alive for another try on the same login ID — `SubmitClaudeLoginCode` distinguishes that from every other failure via `ErrClaudeLoginInvalidCode` so the popup can prompt again instead of restarting the flow. The session is seeded into a desktop **only at creation time** (`Manager.Create` → `PersistClaudeCredentials`, `chmod 600`d): logging in after a claude-harness desktop already exists does not retroactively authenticate it — recreate the desktop, or exec in and repeat the login by hand.
- `httpapi.jsonError` intentionally does not leak `err.Error()` to the client (logs it server-side, returns a generic reason) — preserve that when adding new handlers that call it.
- `memory.WorkerMemoryEntry` records are kept even if `desktop.PersistMemoryFile` (the write into the desktop's container) fails afterward — that's intentional (matches the original single-file behavior), not a bug to "fix" by rolling back the in-memory record.
- `harness.py` is the only file that should ever check `config.AGENT_HARNESS` — `task_runner.py`/`claude_harness.py`/`thebot_harness.py` and everything upstream of them (`watchers.py`, `messaging.py`, `memory_context.py`, `identity.py`, `progress.py`, `readiness.py`) must stay harness-agnostic, so adding a fourth harness only means adding one more branch in `harness.py`.
- **Prompt evaluation, not generation, is what makes a desktop feel slow**, and `keep_alive` is the lever. Measured on an ordinary CPU-only laptop with `qwen2.5-coder:3b`: the agent's 545-token system prefix took **31.8 seconds** to read cold and **0.6 seconds** with the model still resident. Ollama evicts a model after 5 minutes idle and throws away the evaluated prompt prefix cached alongside it, so a task that pauses to run a command comes back to a cold model. Every caller therefore sends `keep_alive` (`ollama_client.ollama_request`, TheBot's `llm.OllamaProvider.KeepAlive`), and `ollama.Manager.preloadModel` loads the model at control-plane startup so the first human request never pays for it. The corollary: **keep the system prefix byte-identical across calls**, which is why both halves of the Ollama harness go through `ollama_client.build_system_messages` rather than assembling their own, and why `num_ctx` is a fixed 8192 everywhere — changing it makes Ollama reload the model and discard the cache.
- **Warm the Ollama the desktops actually use, at the context size they use.** `ollama.Manager.EnsureReady` used to start the bundled `flowera-ollama` container and preload the model *there* whatever `OLLAMA_URL` said, and without `num_ctx`. Measured on a 7.6 GB CPU-only host with `OLLAMA_URL` pointing at the host's Ollama: the preload held 2 GB of RAM on a server nobody talked to, the real one loaded cold in 210 s inside the first request's 300 s timeout, and even a correctly-aimed 4096-context preload would have been thrown away by the agents' first 8192 request. It now preloads `ollama.DesktopURL()` with `ollama.NumCtx`, and touches the bundled container only when `OLLAMA_URL` is `ollama.BundledURL`. The same rule is why TheBot's `[code]` steps must really be code: `internal/coding` asks for a 32768 window, and one such request reloads the shared model for **every** desktop (161 s, measured) and then reloads it back.
- A **backend failure ends the task** (`write_final_reply(..., kind="error")`), it does not pause it with `ask_for_query`. The note tells the human to send the request again, and a paused desktop routes the next request to `continue_after_query` — measured live, a new task was taken as the "reply" to a greeting that had timed out, and reported under the greeting's Task. The exception is a failure inside `continue_after_query` itself, where a resend of the reply should still carry the original task.
- The reasoning model is not the only model. `nomic-embed-text` is a **separate dependency** that only the `thebot` harness needs, and Ollama will not embed with a chat model. Nothing pulled it, and the failure was invisible in the worst way: soul kept answering questions while every write to its Data RAG, its L1 long-term memory and its negative-feedback store failed with `model "nomic-embed-text" not found`. An agent that cannot record what it learned still looks like it works — it just never improves, and repeats the same failure next time. `ollama.EnsureReady` now ensures both models (the embedding one best-effort, since the other two harnesses never embed), and `scripts/linux/lib/ollama-setup.sh` (shared by `build.sh` and `rebuild.sh`) does the same for a host or remote Ollama the control plane does not manage.
- A model that asks a query before running a single command has not tried anything, and `task_runner` pushes back exactly once (`PREMATURE_QUERY_NUDGE`). This is what "hello, whats your desktop name?" needed: it came back as a query asking the human *why they wanted to know*, blocking the desktop on a question. Two things keep the pushback honest — it fires only once, so a second HELP is always delivered, and its last paragraph explicitly invites a safety pause (payment, outgoing message, real-world consequence) to be re-sent unchanged. Verified against the real model: a bogus HELP becomes `DONE: kiolo`, while "pay the 400 dollar invoice" and "email my landlord" still escalate.
- **A reply with no marker is still an answer.** `parse_agent_response` returning `invalid` used to re-prompt with a generic restatement of the format, up to the 50-step limit, and then tell the human the agent "may be stuck in a loop". Measured live: asked how many files are in `/etc`, the model ran the right command, read the right number and replied `241` — finished at step two — then spent forty-eight more steps being lectured about markers before paging a human about a task it had already completed. Two fixes, both load-bearing: `invalid_format_nudge` quotes the model's own opening back to it (the generic version produced another bogus HELP; the quoting version produced `DONE: 241`), and after `MAX_INVALID_RESPONSES` tries `salvage_unmarked_response` delivers the text as the final answer rather than raising a false alarm.
- **A repeated command is how the Ollama loop actually runs out of steps**, and `task_runner` catches it deterministically rather than trusting the prompt to forbid it. The model writes `COMMAND:` and a *predicted* `DONE:` in the same reply; `parse_agent_response` correctly takes only the command, the prediction is dropped, and the model - which considers itself finished - sends the identical pair again. Measured on "what is the current kernel version?": `uname -r` fifty times. An identical consecutive command now gets `repeated_command_nudge` (the output it already produced, and an instruction to answer), and if it repeats again the task ends with `final_answer_from_command`. That reports **what the command actually printed, never the agent's own trailing DONE:** — on step one of that very case it predicted `5.10.124-microsoft-standard-WSL2` before running anything, and the real answer was `6.6.87.2`.
- `parse_agent_response` matches uppercase markers anywhere in the response, then falls back to a line-anchored, case-insensitive pass for `Help:`, `**DONE:**` and `HELP :` (all seen live). The two passes are not interchangeable: a lowercase `help:` mid-sentence is ordinary English, so accepting it anywhere would truncate a real DONE answer at the word "help:". Keep the loose pattern anchored and second.
- What a human reads when an engine fails must not be the engine's own words. `messaging.backend_failure_note` rewrites `context deadline exceeded` / connection refused / missing-model errors into what happened and what to do, keeping the raw text underneath — the note used to hand someone `ollama: request failed: Post "http://host.docker.internal:11434/api/chat": context deadline exceeded` as the entire explanation for why their desktop stopped. `thebot_harness._escalate` applies the same rewrite to failures TheBot words in its own voice ("could not reason about the failure (...) - stopping here").
- `thebot_harness` sends **the task and the standing context as separate fields** (`prompt` and `context` on soul's `/api/chat`), never one string. They used to be glued together, and soul classified, planned, stored in session history and answered the whole blob: "hello, whats your name?" spent 97 seconds in a planning call and came back as `NEEDS_APPROVAL: Before sending any real-world consequences...` — an answer to the safety rule — and "what is the current kernel version?" became a plan to search the web for the weather in New York. The context now only ever reaches soul's system frame; classifiers, the planner and history see the task alone. Measured before/after on `qwen2.5-coder:3b`, CPU only: the greeting went from 148 s (wrong) to 56 s (right), the kernel version from 205 s (wrong) to 45 s (`uname -r`), and creating a file in the work dir from a failed recovery that tried to `chmod /home/headless` to 18 s.
- soul is **deterministic before it is clever**: `atomizer.Triage` routes a request without a model call, and only a genuinely compound request (`looksCompound`) is sent to the planner. The removed layer asked the model "is this a task or a question?" for every request no classifier claimed — the single most expensive and least reliable call in the agent. Keep new routing rules in `triage.go`, deterministic, with a `triage_test.go` case; a chat answer that turns out to need the machine replies `NEEDS_TOOLS` and the scheduler reroutes it to the operator, so a triage miss costs one short call, not a wrong answer.
- soul keeps **one paused job per session** (`server.paused`). `thebot_harness.perform_task` sends `reply: false` (drop any paused job — this is a new request) and `continue_after_query` sends `reply: true` (resume it). A resumed approval runs the exact command it stopped before; a resumed question hands the human's words to the operator's saved transcript. Don't restate the task in a reply — the checkpoint already has it, and restating it makes the operator start over.
- The prompt frame's **volatile sections are chat-only**: live counters (`selfStats`) and the time of day go into a conversational job's frame and nothing else; every other job gets the date alone. With the counters in every frame, the job-log count — which goes up by one per job — changed the bytes right before the operator's contract and made every job re-read it cold.
- **An operator HELP straight after a successful command gets one grounded push back** (`successQueryNudge`) unless it asks for something only a person can give (`humanOnlyRe`). Measured live: `ls -l /etc | wc -l` → exit 0, `118` → `HELP: It seems there was an error. Please try running the command again.`; and `uname -r` → the correct kernel version, delivered under `HELP:`. Both would have paged a human about finished tasks. Set `THEBOT_LOG_MODEL_REPLIES=1` on the container to see raw operator replies in the job log.
- **The operator's transcript keeps only the command, never the answer the model guessed beside it.** A 3B model writes `COMMAND: df -h DONE: 1.8T total, 20G used, 1.6T available` before anything runs; kept in history, it reads that guess back and reports it — measured, word for word, against a real `df` of 1007G/121G/835G. The same leftover guess made it call the real kernel version "not what was expected". (The Ollama harness's `task_runner` appends the raw response the same way.) Likewise `atomizer.scrubInventedContent` strips quoted content the planner wrote into a write step that follows research, unless the human typed it: the planner put `write "The newest Go version is v1.20.5"` behind a search that found 1.27, and file-write saved the guess.
- An abandoned Ollama request is **not** cancelled: when a caller times out or disappears mid-prompt, this Ollama keeps evaluating it, and the next call queues behind it with its own timeout already running — which is how one oversized prompt turned into a chain of `context deadline exceeded` failures on unrelated small requests. Prompt size is therefore a correctness issue on CPU hosts, not just speed: `THEBOT_WEB_PAGES` / `THEBOT_WEB_PAGE_CHARS` (set to 2 / 2500 by `start_agent_terminal.sh`; soul's own defaults of 3 / 6000 assume a GPU) keep a web step's synthesis prompt under the timeout — at 3 × 6000 it took more than 300 s to read and timed out.
- `thebot_harness` sends `sessionId` (the desktop ID). soul keeps real per-session history and threads it into every turn, but `session.Store`'s `Append` and `History` are both no-ops for an empty id — which is what this harness used to send, making TheBot the only harness with no memory of the request before last.
- `scripts/windows/build.ps1`/`rebuild.ps1` mirror `build.sh`/`rebuild.sh` step for step, including the Ollama-selection block, which - like its Linux counterpart - lives in a shared lib file (`lib/Ollama-Setup.ps1`, dot-sourced by both, mirroring `lib/ollama-setup.sh`) rather than being duplicated in each script. Three things in it are specific to Windows PowerShell 5.1 (the PowerShell every Windows machine has), and each one breaks the script without failing loudly:
  - The Python probes go into `flowera-desktop:basic` **on stdin, with their inputs as arguments**, never as a `-c` string, because 5.1 mangles double quotes in arguments to native programs.
  - `.env` is written as **UTF-8 without a BOM, with LF endings**. 5.1's `Set-Content`/`Out-File` add a BOM or write UTF-16, which hides the first key from `godotenv`.
  - A function's array output is unrolled, so a one-model list must be wrapped in `@()`. Otherwise `[0]` returns the name's first character.

  Each script's own `finally` tears its own stack management down only when it did *not* leave through `Exit-Build` - that covers Ctrl+C and the tunnel exiting on its own - but the two scripts' `Invoke-Cleanup` do different things on purpose: `build.ps1`'s just reports that the tunnel stopped and Flowera is still running (it never tore anything down to begin with, matching `build.sh`), while `rebuild.ps1`'s tears the whole stack down (matching `rebuild.sh`'s signal trap), since an interrupted rebuild must not leave a stack half old, half fresh. A failed build or a missing `cloudflared` leaves a running Flowera alone either way (that path always goes through `Exit-Build`).
- Windows `.ps1` twin steps with no Linux equivalent are platform fixes, not drift: `build.ps1`/`rebuild.ps1` check that Docker Desktop is actually running, and on Windows the loopback-only Ollama fix is a user environment variable plus an app restart rather than a systemd drop-in.
- The desktop agent binds `:8081` *before* waiting on its reasoning backend (`app.py` runs `harness.wait_for_backend` on a background thread). It used to call `wait_for_ollama()` inline, so an unreachable Ollama meant the port was never bound at all — and since `desktop.Manager.SendRequest` POSTs there, every human request from the chat, the Requests page and a query reply came back as an unexplained HTTP 500 while the desktop looked healthy in the dashboard. Keep the request channel independent of the model: a task that arrives with the backend down fails *inside* the harness, which reports it through `ask_for_query` as a reply the human can actually read. `wait_for_backend` also belongs to `harness.py` for the usual reason — `app.py` must not learn which engine is running.
- A request that arrives before the reasoning backend is up is **queued, never failed**. `watchers.command_watcher` leaves the file sitting in `COMMAND_DIR` while `state.backend_ready` is False and picks it up on a later pass, the same way it already defers a request that arrives mid-task; `harness.wait_for_backend` sets that flag once the engine answers. Dispatching into a backend that is still starting is what turned "Ollama is 20 seconds from ready" into a query asking a human to fix something that was about to fix itself. Every branch of `wait_for_backend` must mean *reachable*, not *launched* — `thebot` waits on both the shared Ollama and soul's own `/api/health`, because soul reports an unreachable Ollama as a reasoning failure that reads like a real one.
- The command watcher claims the task slot (`state.current_task = content`, under `state.state_lock`) *before* starting the harness thread, not inside `perform_task`. Otherwise the loop tests `current_task` again for the next queued file before the thread it just started has run a line, and dispatches a second task on top of the first — rare when requests trickled in one at a time, routine once a queued backlog can be released all at once.
- Don't hardcode a Cloudflare quick-tunnel URL as anyone's default. Quick-tunnel hostnames are regenerated on every `cloudflared` restart, so a checked-in one is dead code the day it's written — `buildDesktopEnv`'s `OLLAMA_URL` default was one, and every desktop created after that tunnel died was structurally brainless. Tunnel URLs belong in `.env` (or `internal/tunnel`, set at runtime), never in a literal.
- `progress.emit` is decoration and must stay that way: it never blocks, never raises into a harness, and drops lines when its bounded queue is full. A desktop reporting what it is doing must never be able to slow down or fail the task it is describing — which is also why `agentEventHandler` answers a malformed event with 200 rather than an error the agent would then have to handle.
- **A turn's last step can arrive after its reply.** Steps go out on `progress`'s background queue, but the reply is POSTed directly, so TheBot's final log line ("wrote … verified on disk") reached the control plane under a millisecond *after* the answer — and `AppendStep`, seeing no running turn, opened a new one that nothing ever closed: the chat showed "Working…" forever over a finished task. Two guards: `messaging.send_query` calls `progress.flush()` (bounded) before sending, and `conversation.AppendStep` attaches a step arriving within `lateStepWindow` of an agent reply to that reply instead of reopening a turn — the second is what covers desktops built from an older image.
- `DESKTOP_ID` (what a desktop calls itself on every callback) and `Desktop.ID` (what the dashboard lists it as, derived from the container name) must produce the same string, since query notes and chat entries are keyed by it. Both now go through `sanitizeName`; they diverged for any name containing a character only `sanitizeName` rewrites (`my.box` stayed `my.box` in the env var while the desktop was listed as `my-box`), and that desktop's callbacks matched no desktop the dashboard knew. Use `desktop.SanitizeID` from outside the package.
- A dashboard card's preview is a JPEG from `/api/desktops/screenshot`, **never an embedded screen**. It used to be `<iframe src="/desktop/<id>/">` — a whole noVNC client per card, measured at 83 requests plus a WebSocket each, all against the control plane's single origin, which a browser serves over at most 6 concurrent HTTP/1.1 connections. Two desktops saturated that pool (178 requests over 7 connections, ~950ms stalls) and the VNC handshake for a screen a human had actually opened queued behind the thumbnails — 11s in one run, "Failed to connect to server" in the next. That is why "spin up two VMs and you can't work on both" was a browser-side bug with a clean server: the Go proxy served 120 concurrent requests with zero errors. `desktop.Screenshot` reads RFB directly instead (~8KB, ~80ms), and `index.html`'s `shotLoop` refreshes cards in one *sequential* sweep so the dashboard never has more than one thumbnail request in flight whatever the desktop count. Don't reintroduce a per-card iframe, and don't parallelise that sweep.
- `desktop.Screenshot` connects as a *shared* RFB client (`ClientInit` shared=1) and only ever sends a FramebufferUpdateRequest — a thumbnail must stay invisible to whoever is using the screen. It offers `Raw` as the only encoding, so the server cannot answer with a compression scheme there is no decoder for, and it reads `VNC_PW` off the container rather than the control plane's `DEFAULT_PASSWORD`, so a desktop created before that value changed still has a readable screen.
- The chat renders `content` through `textContent` only, never `innerHTML` — the text in a bubble is written by a model that can be told to emit anything. `renderContent` builds real `<pre>`/`<code>` nodes for fenced and inline spans and leaves everything else literal; keep that property when extending the formatting.
- `internal/webui/render_test.go` executes every page template. Templates are data to the Go compiler, so a typo'd action or a renamed field is invisible to `go build` and shows up as a panic or a 500 on the first request — add new pages to that test.
- `httpapi.agentProxyHandler` (`/agent/<id>/`) still proxies TheBot's own UI on port 4900, but nothing in the control plane's UI links to it any more: the chat page talks to TheBot through `flowera_agent` like every other harness, so it picks up worker memory, the safety preamble and the approval path, none of which soul's own UI goes through. Keep it as a debugging surface; don't wire it back into the UI as a second way to command a desktop.
- The base image ships `/home/headless` as `root:root` — anything that needs to write directly under it at **build time** (not runtime, where the real entrypoint/volume-mount semantics differ) needs an explicit `chown headless:headless /home/headless` first, same as the existing `RUN ... chown -R headless:headless /home/headless/.config` step. This bit the `claude` native-binary install (`~/.claude`, `~/.local`) until fixed in `docker/desktop.developer.Dockerfile`/`docker/desktop.ai.Dockerfile`.
- **Chromium in a desktop runs sandboxed**, which takes two halves that must both be present. `desktop.Manager.Create` gives every new desktop `internal/desktop/seccomp-desktop.json` — Docker's own default seccomp profile with `clone`/`setns`/`unshare` opened, nothing else — because Chromium's sandbox is built from user/PID/network namespaces that the stock profile forbids (the base image's answer was a hardcoded `--no-sandbox`, and the "unsupported command-line flag: --no-sandbox" bar on every window). And `agent/chromium-browser.init` probes `unshare --user --pid --net` at each browser launch and only adds `--no-sandbox` back when namespaces really are unavailable — a desktop created before the profile existed, or a daemon without seccomp (`desktopSecurityOpt` checks `SecurityOptions` and skips the profile there, since such a daemon rejects the container outright). **A desktop created before this change keeps showing the warning until it is recreated**: the seccomp profile is fixed at container creation. The warning is left visible in that fallback on purpose — the browser really is unsandboxed there.
- Desktops run with **core dumps off** (`desktopUlimits`, plus `ulimit -c 0` in `chromium-browser.init` for desktops created earlier). The `core.<pid>` files that filled a TheBot desktop's `~/Desktop` were not TheBot's: parsed out of the ELF notes, every one was a Chromium utility process (`--utility-sub-type=data_decoder` / `unzip`) dying with `*** stack smashing detected ***` in a restart loop, ~50 MB every fifteen seconds, landing in `~/Desktop` because that is where the XFCE menu starts the browser. `start_agent_terminal.sh` deletes existing ones, matching both the `core.<digits>` name and the ELF `ET_CORE` header.
- The chat renders a sent request **the moment Send is pressed** (`desktop.html`'s `pending` list), then drops that local copy as soon as a transcript snapshot contains the server's own (the first new user entry with the same text that was not in the transcript at send time). The server stays the only record — `sendRequestHandler` still appends only after the desktop accepts the request (measured at 5–12 ms) — and a failed send stays on screen with Retry and Edit.
- **The Cloudflare quick tunnel does not deliver the chat's event stream.** Measured through a `trycloudflare.com` tunnel: `/api/desktops/chat/stream` returns its headers and then *no body bytes at all* — not the snapshot the handler flushes on connect, not a request sent a second later — for as long as it was watched (40 s), while the same stream on localhost delivered the request in 70 ms. The chat used to fall back to polling only on a stream *error*, and a held stream never errors, so on a phone a sender's own request appeared only when the agent's reply finally pushed something through: the "my request only shows up with the response" bug. `connect()` now closes a stream that has not delivered its first frame within `FIRST_FRAME_MS` (4 s) and polls instead; over the tunnel a sent request shows in ~70 ms (locally rendered), the server's copy in ~2 s, and live steps from ~2 s. Anything else that relies on SSE through the tunnel needs the same fallback.
- `NEEDS_TOOLS` is taught **only to the chat skill** (`prompt.ChatRule`, appended to the frame by `GeneralSkill`), never in the shared `Persona`: with it in the persona, the operator answered "ping - just reply pong" with `HELP: NEEDS_TOOLS`, which a human received as an entire query. The operator also treats that marker as a malformed reply.
- The XFCE desktop wallpaper is set by baking `agent/flowera-desktop-wallpaper.xml` (pointing at `agent/flowera-wallpaper.svg`, copied to `/opt/flowera-wallpaper.svg`) into `/home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml` at image build time, in all three desktop Dockerfiles — otherwise XFCE generates its own default (the base image's stock `xfce-shapes.svg`) the first time it starts with no config present. Since `/home/headless` is the container's persistent named volume, this file only seeds *new* desktops (an empty volume on first start); it has no effect on a desktop created before an image rebuild.
- **The base image sets `NO_AT_BRIDGE=1`**, which silently turns off every GTK app's AT-SPI bridge: the accessibility tree was empty until the desktop Dockerfiles set `ENV NO_AT_BRIDGE=0` (and `computer.session.ACCESSIBLE_APP_ENV` sets it for anything the layer launches). Thunar's windows belong to the XFCE session's Thunar *daemon*, so on a desktop created before that ENV change they stay invisible until the daemon is restarted — which is why `bench.py` runs `thunar --quit` before a file-manager task. Like the seccomp profile, the ENV is fixed at container creation: an existing desktop needs recreating.
- **LibreOffice misreports where it draws.** Under XFCE its AT-SPI extents for everything VCL paints (the document, table cells, toolbars) are 27 px above the real position — only its native GTK menu bar is right. So the layer never clicks LibreOffice content by coordinates: text goes into a Writer cell or paragraph through `Atspi.Text.set_caret_offset` (`atspi.place_caret`), and a Calc cell is reached by coordinate through the Name Box shortcut (Ctrl+Shift+F5, the reference, Enter — `core._goto_sheet_cell`), because LibreOffice 24.2 does not expose the Name Box over AT-SPI at all. A Calc sheet MANAGES_DESCENDANTS with 2^31 children and is never walked; its visible used range is read cell by cell through the Table interface (`atspi._sheet_cells`). Writer's document sets the same flag with a handful of children and *is* walked — the rule is the child count, not the flag.
- **A ref means "the element at this path"**, not "the Nth element of this observation" (`elements.assign_refs`). Paths are rooted at the window title with a leading `*` stripped (an editor's unsaved-changes marker) for AT-SPI, and at the **site** (host:port), not the page title, for Chromium — so a navigation link keeps its ref on every page of a site. Renumbering per observation made "e12" point at something else after one row was inserted.
- A GTK list/tree cell's AT-SPI action is `activate` — *open the file*, not select it — so `core._perform` clicks cells, list items and icons by position instead of invoking their action. Every other widget prefers its own action over a coordinate click.
- **Chromium's DevTools port (9222, `agent/chromium-browser.init`) is how pages are read and worked**, bound to 127.0.0.1 inside a container with no published ports. It is also a way for any process in that desktop to drive the logged-in browser — the same trust the agent's unrestricted shell already has, not a new one. A Chromium started *without* the port (before this change, or by hand) cannot be read; `observe` says so in a note rather than returning an empty page. Chromium 112 accepts a DevTools websocket with no Origin header; Chrome 136+ refuses remote debugging on the default profile, so a base-image upgrade will need `--user-data-dir`.
- `logging_utils.log` prints to stdout, which is the *output* for `gui` (an engine reads it as the result of its action) and the *protocol channel* for the MCP server. Both call `logging_utils.log_to_file` first (`/tmp/flowera-gui.log`, `/tmp/flowera-mcp.log`). A stray `print` in the computer package breaks Claude's tools.
- The audit trail's `gui` process is short-lived, so `cli.main` calls `progress.flush()` in its `finally` — without it the action step (and its screenshots) die with the process before the background sender posts them. `progress.emit_action` drops the screenshots, never the step, when the queue is a quarter full.
- The approval gate's patterns (`approval.CONSEQUENTIAL_CONTROL`) match a control's *label*, anchored at its start ("Send newsletter", "Pay now INV-1042"), and only for activating roles. They are deliberately narrow for the reason `internal/safety` gives. The gate guards against a model's mistakes, not a hostile model: an engine with a shell could write the grant file itself, exactly as it could run xdotool directly.
- `bench.py` kills apps by **process name**, never `pkill -f <word>`: task ids like `mousepad-save-as` are in its own command line, and the first version killed itself.
- **A dispatched request file is renamed to `*.txt.done`** (`watchers._mark_handled`). `state.processed_commands` is memory and `COMMAND_DIR` is the persistent volume, so every desktop restart used to replay every request the desktop had ever received — found by the GUI benchmark, which restarts a desktop between tasks: each task queued behind replays of all earlier ones, and a spreadsheet task was answered with questions about signing in to an admin panel. A request deferred behind a running task or a backend that is not up yet keeps its `.txt` name until it is actually dispatched.
- `bench/gui/run.py` restarts a desktop after any task that did not end in a plain answer or error (`agent_is_idle`): a paused agent takes the next request as the *reply* to its query. And an approval task only passes when the gate itself fired (a step with `outcome: needs_approval`), not when the agent merely raised a query for some other reason — which leaves the invoice unpaid too.
- **A demonstration tells the person from the agent by input device, not by timing.** XInput2 raw events name their source device: a person using noVNC arrives as Xvnc's `TigerVNC pointer/keyboard`, every xdotool action as `Virtual core XTEST pointer/keyboard` (measured: devices 6/7 vs 4/5), and `demonstration.py` drops XTEST. The client must announce XInput **2.2** (`XIQueryVersion`) — only from 2.1 are raw events delivered while another client holds a grab, i.e. inside an open menu or a drag. python-xlib 0.33 parses XInput device events but not raw ones: the recorder registers a raw-event struct with `Display.ge_add_event_data` (a method of the high-level `Display`, not `display.display`) and matches `ge.GenericEventCode`, since `X.GenericEvent` does not exist there.
- **Xvnc changes the keymap while someone types.** It maps the keysyms a VNC client sends onto keycodes on the fly and types a capital by toggling Caps Lock around it, announcing both with `MappingNotify`. The recorder refreshes its keymap on that event, names keys from its own `KEY_NAMES` table (python-xlib's `keysym_to_string` returns None for Shift_L, Caps_Lock and every other named key), and reads the Lock state back from the server whenever a key goes down with nothing held. Measured before each fix: "Human Co" recorded as four runs split by `keycode_66`, then as "hUMAN cO" when an earlier session had left Caps Lock on.
- **A typing run keeps its field until something moves focus** (`demonstration._focus_after`), and text typed into a field that cannot be told is withheld whenever a secret field is on screen. Both close a leak found live: an unrecognised key split a password's typing in two, the second half had no element, and it was stored as plain text.
- **A demonstration's "what changed" is discarded when the person's next input landed before the screen was read** (`change_missing`). Observing takes a second or two and people do not wait: a sign-in click's "Welcome back" was recorded as what the typing before it did, and a replay would have waited for it after the wrong step.
- A replay is **refused (409) while the desktop has a task running, queued or waiting** (`tasks.Store.Busy`), rather than routed: sent to a paused agent the watcher takes it as the reply, and queued behind a running task it is absorbed as the reply if that task pauses. A replay's parameters travel only in the request body to the desktop and live in `replay_task`'s memory until it runs — the chat, the task record and the command file see `/replay <id> [from=N]`, never a value.
- The agent's HTTP server is a `ThreadingHTTPServer`: exporting a recording streams a zip for as long as it takes, and a single-threaded server would hold every chat request behind it.
- **Everything the agent keeps for itself lives under `~/.flowera`**: `config`'s command/querytext/final/memory dirs, the computer layer's `computer/` and `recordings/` (an `audit/` from before recordings is imported into them once at startup by `recording.import_legacy_audit`, with its typed secrets withheld), and TheBot's `WORKSPACE_ROOT` (`ai/`). `~/work` is the only directory it adds to the top of the home, because that is where tasks run and what a human browses. `start_agent_terminal.sh` moves an older volume's `~/command`, `~/helptext`, `~/final`, `~/memory`, `~/audit` (and `~/ai`, on a TheBot desktop only) into place before the agent or soul starts, never overwriting an entry already there - a second step then renames a `~/.flowera/helptext` it finds (freshly moved, or already there) to `~/.flowera/querytext`. A container keeps the image it was created from, so a desktop created before the move still runs an agent that reads `~/memory` — which is why `desktop.Manager.PersistMemoryFile` writes there while it is that desktop's only memory directory.
- **A ticket is opened the same way any request is, but Record/Stop/Continue/Reason/End are not command files.** `record_task.py` keeps the open ticket as its own module-private state (`_ticket`, the way `replay_task.py` keeps `_held`) rather than in `state.py`, because nothing outside the module reads it directly — the GUI toolbar's begin/pause/resume/end calls go straight into `record_task.begin()` etc. over HTTP, bypassing the command directory and the watcher entirely, since they only ever act on whatever ticket is already open. `tasks.Store.Busy` is still what keeps a second ticket (or a replay) from being opened while one is: `perform()` occupies the task slot for the ticket's entire lifetime, exactly like an agent's task would.
- **`httpapi.callAgent`'s payload parameter is `any`, and a nil `map[string]string` boxed into it is not the untyped `nil` the function tests against** — Go's classic typed-nil-in-an-interface trap. `ticketActionHandler` hit this for `begin`/`resume`/`end` (no fields to send): a `var payload map[string]string` left nil and passed straight through would satisfy `payload != nil` anyway, and `json.Marshal` would send the literal `null` as the body — which the agent's `_body()` rejects (`json.loads("null")` isn't a dict) as 400 rather than sending no body. Pass the bare `nil` literal itself for a call with no fields, not a nil-valued typed variable.
- **The ticket toolbar's own poll must not rebuild the panel when nothing changed.** It reads GET `/api/desktops/ticket` every 2.5s the way `desktop.html`'s chat polls the transcript, but unlike the chat's whole-snapshot renders, rebuilding this panel on every tick would wipe out a task name being typed into the `+` input mid-keystroke (new `<input>`, empty value) even though the ticket state hadn't actually changed. `refresh()` compares the fetched ticket against the last one (`JSON.stringify`) and skips the render entirely when they're equal; a create-ticket **failure** (busy desktop, dropped request) also preserves what was typed, via a `draftTask` variable read back into whatever `<input>` gets rebuilt next, so a rejected submission doesn't make someone retype it.
- **End does not wait for `record_task.perform()`'s own thread to clear the ticket, so its HTTP response can still show the ticket "open" for an instant.** `end()` sets `demonstration.stop()` (if capturing had started) and signals `_ended`, then returns immediately — `perform()`'s background thread wakes and clears `_ticket` a moment later, off this request's critical path. Trusting that response's `ticket` field for the toolbar's post-End render would flash the ticket panel once more before the next poll corrected it; the toolbar's `act("end", ...)` special-cases the answer instead — captures `session_id` for the download link if the response carried one, sets its own `ticket = null` unconditionally, and renders the "recording saved" view right away.
- **The dashboard card has no Record button of its own** — `index.html`'s `.dcard-actions` only opens the chat and the screen (plus restart/stop/delete); recording (and browsing past recordings) lives solely in the ticket toolbar inside the screen itself, reachable through the screen's own "Browse recordings ↗" link. Don't re-add a card-level entry point for either — that split is deliberate: the toolbar is the one thing that already knows whether a ticket is open, recording, or paused, and a second, unsynced entry point on the card would just be another place for that state to go stale.
- **The ticket toolbar's tab is a drag handle, not just a click target.** Pointer-down on `#flowera-ticket-tab` starts tracking; past `DRAG_THRESHOLD` (6px) it "pins" the toolbar (`flowera-pinned` class, cancelling the default centered `transform`) to an explicit `left`/`top` clamped to the viewport, so it can be moved off whatever it's covering on the desktop underneath. A drag's terminal `pointerup` still fires a `click` on the tab — the same handler that opens/closes the panel — so the click listener checks the `dragged` flag first and swallows that one click rather than also toggling the panel.
- **Every rewrite in the proxy's `ModifyResponse` chain writes UTF-8, but the proxied container's own `Content-Type` never says so.** `rewriteFloweraBranding` (always first) now sets `Content-Type: text/html; charset=utf-8` explicitly. It has to be there and not later in the chain: `injectTicketToolbar` splices its own copy in right before `</body>`, far past a browser's ~1024-byte `<meta charset>` sniffing window, so by the time that text exists in the document the browser has already committed to whatever encoding it guessed for the whole thing. Without the header, a guess other than UTF-8 turns every non-ASCII byte added anywhere in the chain to mojibake — this is what an em dash in the toolbar's own copy needs the header for, not anything wrong with how that string is written in Go.
- **A note written while paused stays paused; whether to continue afterward is the toolbar's call, not `demonstration.note`'s.** `note()` no longer refuses a call while paused (that used to be its only guard on `_running()`'s state) — it writes the note into whatever pause it finds, active or not, and always (re-)pauses after with its own summary as the reason. The toolbar's Reason button pauses *before* opening its text box (not after submitting, like Stop), and only follows a successful submit with an explicit `resume` when its own click is what caused that pause (`reasonAutoPaused`, reset on every path that leaves the box) — never when Reason was opened on top of a pause already in effect, since that pause may be covering exactly what the note is warning about, and auto-continuing would hand it straight back to the recording.
