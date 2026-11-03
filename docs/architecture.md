# Architecture: packages, desktop container, listeners

```
cmd/decurix/main.go — entrypoint: loads .env, builds the Docker client, wires
                       the packages below, registers routes, ListenAndServe(:8080)
                       plus a second, access-code-gated ListenAndServe (:8090 by
                       default - see "Two listeners, one process, one set of
                       routes" below); --version, and --build-images/--rebuild
                       which build the
                       desktop images from the embedded context and exit;
                       --cli, which skips all of the above and hands off to
                       cli.go instead (see "The --cli terminal client" below)
cli.go             — runCLI: an interactive REPL that is the same binary's
                       --cli mode, a text alternative to the dashboard/chat
                       page for a control plane already running somewhere -
                       reached purely over internal/httpapi's existing JSON
                       API (GET/POST /api/desktops*, /api/querynotes), so it
                       needs no Docker client and no database of its own
embed.go           — package decurix at the MODULE ROOT: //go:embed of
                       docker/desktop.*.Dockerfile + agent/ → DesktopBuildContext.
                       It has to live at the root because go:embed cannot reach
                       above its own directory and the context spans two dirs.

internal/desktop   — owns the Docker client; desktop image build
                      (images.go: MissingImages/BuildImages, tars
                      decurix.DesktopBuildContext to the daemon); container CRUD
                      (Create/Start/Stop/Restart/Delete/List), image/network
                      ensure, SendRequest (POST to a desktop's :8081),
                      List/desktopFromContainer: one ContainerInspect per
                      desktop, fanned out up to listInspectConcurrency (8) at
                      once rather than one at a time — List is what the
                      dashboard, the JSON API and internal/idlepause's sweep
                      all call, so its cost has to stay roughly flat as the
                      desktop count grows, not linear in wall-clock time,
                      LastViewed (screenshot.go/proxy.go touch it on every
                      thumbnail fetch and every proxied screen request — the
                      "someone has this open" signal internal/idlepause reads,
                      cleaned up in Delete alongside the shots cache),
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
                      preloading the model there at startup, plus WarmAsync (debounced
                      background warm-up called when a desktop starts, a chat opens or a
                      request is sent - see the memory-starved-host gotcha at the end); also the lifecycle of the
                      bundled "decurix-ollama" container, used only when OLLAMA_URL names it
                      select.go/autoselect.go/resources.go (+ mem_<os>.go): the model
                      chooser - probes RAM/CPU/GPU and picks the smartest chat model that
                      fits 50% of what is free (see "The chat model is chosen for this
                      machine" below); served at GET /api/model (httpapi/model.go) and
                      shown as the chip at the top left of every page (theme.html's
                      "topbar")
internal/sqlitedb  — opens the one embedded SQLite database (modernc.org/sqlite, pure Go,
                      so releases stay CGO_ENABLED=0) at DECURIX_DATA_DIR/decurix.db, and
                      Migrate(db, component, steps): each store below owns its own tables
                      and appends schema steps, never edits an applied one
internal/querynotes — queries submitted by agents (SQLite). A note is resolved, never deleted: List is the open
                      ones (the rail, the badge, the Queries tab), ListResolved the history
internal/memory    — worker-memory entries (SQLite); deleted with the desktop they were learned on (DeleteForDesktop)
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
                      deterministic task type and intervention reason ("/learn" is type
                      `teaching`, kept out of autonomy like a demonstration; "/skill" is
                      `screen`). report.go: autonomy by engine/worker/type and the
                      reasons people stepped in
internal/winhost   — runs a Windows app on a PRIVATE WIN32 DESKTOP of the host, so it has no
                      window on the person's own screen, and lets the control plane see
                      and drive it: bridge.ps1 (embedded; the Windows half) is spoken to
                      over the stdin/stdout of the process that runs it - no listener, so
                      nothing but the Manager that started it can reach the app. Manager/
                      Session: one session per id plus ONE WARM SPARE helper (a launch
                      finds a helper already running, and one is started for the next);
                      the helper pushes pictures (Watch/Viewer: a whole picture first, then
                      only the rectangle that changed), Do/Mouse/Wheel/Type/Key, Copy/Paste,
                      Check (which programs can run hidden, from the same helper), Stop/StopAll;
                      token.go signs and checks the launch tokens (LoadSecret keeps the key in the data dir). Takes its host
                      command as a Starter, so it depends on nothing internal and is tested
                      against a bash fake speaking the same line protocol
internal/tunnel    — in-memory store of the addresses scripts/linux/build.sh
                      (or rebuild.sh, or their Windows twins) discovers and
                      reports via POST /api/tunnel: a Cloudflare quick tunnel
                      URL for the gated listener below (LANTunnelURL) and this
                      machine's own LAN address for it (LANURL), surfaced in
                      the dashboard's Mobile menu; Get/AdminURL (the primary
                      listener's) are legacy - no current script reports them
internal/idlepause — Checker.Run: a ticker that stops a desktop once it has sat
                      idle (tasks.Store.Busy empty, and past Threshold since the
                      later of its container's own start time, its last task
                      activity, and desktop.Manager.LastViewed) - on by default
                      at 5 minutes (cmd/decurix.defaultIdleStopMinutes),
                      DECURIX_IDLE_STOP_MINUTES=0 opts out, and it never does
                      more than the same Stop a human's own button does
internal/lanaccess — Store: the rotating access code gating the second,
                      LAN-facing listener main.go starts alongside the
                      primary one (see "Two listeners, one process, one set of
                      routes" below) - Middleware shows an "enter the code"
                      page to anything without a valid session cookie, Rotate
                      replaces the code and drops every session it granted,
                      RunAutoRotate calls Rotate on its own every
                      AutoRotateInterval (48h) so a leaked code doesn't stay
                      valid indefinitely - always started (see idlepause's
                      "always started" precedent), same non-positive-disables
                      contract. Depends on nothing internal by design, same
                      tier as tunnel/sqlitedb: it must never be able to
                      reach, and must never be reached by, the primary
                      listener's own routing
internal/devices   — the phones/laptops paired with the gated listener ("Remember this
                      device" on the access-code page): a long-lived token per device,
                      stored as a SHA-256 only, surviving code rotation until revoked from
                      the Mobile menu (/api/lan-access/devices*, primary listener only).
                      Implements lanaccess.DeviceRegistry, so lanaccess stays dependency-free
internal/llm       — the hosted models a desktop's brain can be: Claude (claude.go: the
                      OpenAI chat shape translated to the Messages API through the official
                      Go SDK - system/tool calls/tool results/images, turns merged to
                      alternate, temperature dropped and forced tool_choice made "auto"
                      because the current models refuse both), Azure AI Foundry (Client,
                      api-key added) and any OpenAI-compatible server (the same Client with
                      Bearer: OpenAI itself by default, OpenRouter, a Brainiac box on the own
                      network - plain http allowed only for a private host, privateHost).
                      Hub holds all three, set up by a PERSON in the create
                      dialog (never .env) and saved in SQLite with the key sealed (llm.key);
                      Save runs Test first, so a key the provider refuses is never kept, and
                      an empty key on a save keeps the saved one. Served on the primary
                      listener only: /api/llm/<provider>/{chat,v1/chat/completions,v1/models}
                      for the desktops (/api/llm/chat is Azure, for older desktops),
                      /api/llm/settings{,/forget} humansOnly. Resolve allows only
                      "default"/"fast". /api/llm (both listeners) is the status, never a key.
                      Depends on sqlitedb and sealed
internal/outbox    — emails agents drafted (the azure harness's draft_email, POST /api/outbox
                      from a desktop), waiting for a person: the Triggers page's Outbox lists
                      them with their attachments, and only a person's Send (humansOnly)
                      hands one to mailtrigger.Store.SendMail. Claim (draft -> sending) makes
                      a double click send once; a send interrupted by a restart is marked
                      failed, never resent on its own. Depends on nothing internal but sqlitedb
internal/sealed    — AES-256-GCM for the few secrets kept in SQLite, key in a file of its
                      own in the data dir (mailtrigger's mail.key, cftunnel's cloudflare.key)
internal/cftunnel  — "Your own link": a named Cloudflare Tunnel giving the gated listener a
                      fixed https://<name>.<domain> instead of a quick tunnel's changing URL.
                      Configure does the Cloudflare side once through the API (zone, a
                      remotely-managed tunnel, ingress to 127.0.0.1:<gated port> only + a
                      404 catch-all, a proxied CNAME); Run keeps `cloudflared tunnel run` as
                      a supervised child (token in TUNNEL_TOKEN, never argv). Several
                      connectors can run one tunnel - Cloudflare picks any that is up - which
                      is what a multi-device setup will build on
internal/wsconn    — the one WebSocket implementation (RFC 6455, hand-written, no library):
                      Upgrade (server side: the host-app viewer, the phone link, the phone-app
                      viewer) and Dial (client side: the phone agent dialling out)
internal/phonehost — the control plane's end of a phone link: Hub.Serve runs one linked
                      phone's WebSocket; Apps asks it for its installed apps (cached 2 min),
                      Open starts an app on a new virtual display of the phone and returns a
                      Session (video frames out, Input in). Depends on nothing internal but wsconn
internal/phoneagent — the phone's end, run by `decurix --phone` in Termux on an Android phone:
                      pairs with a pairing link (Pair, like the "Pair this device" form), dials
                      /api/phones/connect with its device cookie, and drives the phone through
                      local ADB and scrcpy's own server (pinned v4.1, downloaded once and checked
                      against the release's SHA-256): list_apps for the app list, and per opened
                      app one server with new_display=WxH/dpi plus a START_APP control message.
                      Depends on nothing internal but wsconn and phonehost (the protocol)
internal/claudeauth — the one shared Claude Code login every claude-harness
                      desktop is seeded from (in place of an ANTHROPIC_API_KEY):
                      a single-row SQLite table holding the raw
                      ~/.claude/.credentials.json content plus the account's
                      email, so it survives a control-plane restart, build.sh
                      leaves it alone, and rebuild.sh wipes it for free as part
                      of wiping decurix-data — no script changes needed for
                      either half of that
internal/schedule  — recurring or one-time-future requests set up from the
                      requests page (not the chat): Store persists each one
                      (desktop, request text, either a standard 5-field cron
                      expression or a single future run time (RunOnce), next/
                      last run) in SQLite; cron.go is a small pure-Go
                      parser/matcher (no external dependency, matching the
                      sqlitedb/modernc.org choice to stay CGO_ENABLED=0) with
                      the standard day-of-month/day-of-week OR rule, run in
                      UTC. Runner ticks (20s default) and fires a schedule
                      through the Submitter interface once its next-run time
                      has arrived, appending "\n\nTIME:{<RFC3339>}" to the
                      request text so the agent knows when that run
                      triggered; a recurring schedule always advances to its
                      next occurrence after firing (success or not), so a
                      control-plane outage causes one catch-up run on
                      restart, never a backlog - a RunOnce schedule instead
                      disables itself after its one firing, the same
                      terminal state a cron schedule reaches when it runs out
                      of matches. Depends on nothing internal but sqlitedb
internal/mailtrigger — email triggers (set up on the Triggers page, /triggers, or from the
                      requests page's compose form; /email redirects there): a mailbox
                      connection (Account: IMAP over TLS, an app password
                      encrypted in SQLite - secret.go - and never serialised)
                      is polled every DECURIX_MAIL_POLL_SECONDS (60); each
                      email above the last UID read is matched against that
                      mailbox's rules (a case-insensitive regex on the sender
                      and/or subject) and a match queues a Job (the rule's
                      request + the email, Render) that goes out through
                      SubmitTracked - the automated door, which sends only to
                      a free desktop. store.go (mailboxes, RecordPoll),
                      rules.go, jobs.go (claim -> send -> sent/retry/failed),
                      runner.go (poll, match, dispatch, sleep detection),
                      imap.go (a hand-written read-only client: EXAMINE,
                      BODY.PEEK - never marks anything read), message.go
                      (headers/MIME to text; the provider's own DMARC/DKIM
                      verdict for Rule.VerifiedOnly). Depends on nothing
                      internal but sqlitedb (Submitter is a narrowed
                      interface over *httpapi.Server)
internal/keepawake — keeps the machine from idle-sleeping while something
                      needs it awake (Keeper.Wanted: a watched mailbox, a
                      waiting schedule or DAG cron) and releases it the moment
                      nothing does: SetThreadExecutionState on Windows (a
                      locked OS thread), a powershell.exe helper fed on/off over
                      stdin under WSL, systemd-inhibit on Linux, caffeinate on
                      macOS. Inside a container it does nothing and
                      GET /api/keep-awake tells the host script instead
                      (scripts/*/lib/keep-awake.sh, Keep-Awake.ps1). Depends on
                      nothing internal
internal/suggest   — picks a desktop for a request before it is sent, for the
                      requests page's compose form: the one most recently
                      actually in use, or - when some desktop's own worker
                      memory (internal/memory) closely matches the typed
                      request - that desktop instead. Mirrors idlepause's
                      narrow-interface pattern (DesktopLister/ActivityStore,
                      plus a MemoryLister over memory.Store) so it needs no
                      changes to desktop/tasks/memory themselves. Embeds text
                      through the shared Ollama (ollama.Embed, EmbedModel)
                      and ranks by cosine similarity; a memory entry's
                      embedding is cached in its own SQLite table
                      (suggestion_embeddings) the first time it's compared
                      against, so a suggest call after that only ever pays
                      for one embedding round trip - the typed text's own.
                      Every failure path (no Ollama, no embedding model,
                      nothing in memory yet) just falls back to recency and
                      never blocks or fails sending a request - the same
                      best-effort posture ollama.Manager.preloadModel already
                      takes. Depends on desktop (narrow interface), tasks
                      (narrow interface), memory (narrow interface), ollama,
                      sqlitedb
internal/httpapi   — the only package touching net/http; all HTTP handlers,
                      composes desktop+querynotes+memory+conversation+stepimages+tasks+
                      tunnel+claudeauth+lanaccess+schedule+suggest+webui. schedules.go: the
                      requests page's recurring-request CRUD (/api/schedules,
                      .../create, .../delete, .../toggle) and Server.Submit, which
                      implements schedule.Submitter by calling the same submitLocked
                      every human-typed request already goes through - a schedule's
                      fired request is a fourth door onto Plan/deliver/Commit, after
                      the chat, the requests page and a query reply. lanaccess.go: the two routes
                      RegisterLocalRoutes registers only on the primary listener -
                      the current LAN/tunnel access code and the button that rotates
                      it - never served back over the listener that code itself
                      gates. claudeauth.go: the dashboard's Claude
                      login popup (/api/claude-auth/status + login/start + login/code
                      + login/cancel), backed by desktop.Manager's claudelogin.go and
                      saved through claudeauth.Store. hostgui.go: the host-gui API
                      (/api/host-gui/start|ws|stop and the /host-gui/view page; the ws route uses
                      internal/wsconn, hand-written RFC 6455 - no library, to keep the binary static):
                      a copied Windows app's launcher in a VM opens that page in the VM's own
                      browser, which shows the app and sends the person's mouse and keyboard
                      back - see "Windows apps run on the host and show in the VM" below.
                      recordings.go (incl. the import of a recording made elsewhere): the recordings page and the
                      /api/desktops/recordings* + /api/desktops/demonstration proxy, replay
                      as a task (Busy check, Plan/Commit, parameters never recorded), and the
                      ticket API (/api/desktops/record opens one, /api/desktops/ticket* drives
                      it - the GUI screen's own toolbar). proxy.go: desktopProxyHandler's
                      injectTicketToolbar splices that toolbar into every load of the screen.
                      activity.go: GET /api/desktops/activity, what each dashboard card
                      says its desktop is doing (read off the chat snapshot).
                      skills.go: the skills page and its API (/skills/<id>, /api/desktops/skills*,
                      /api/desktops/recordings/learn) - list/show/delete proxied to the desktop's
                      :8081, and "teach" and "run" as tasks (Busy check, Plan, the desktop's own
                      endpoint, Commit) exactly like a replay, with skillContent/learnContent
                      building the "/skill ..." and "/learn ..." request the chat shows
internal/webui     — html/template rendering (PageData, dashboard/requests/desktop-view
                      pages); parses out of web.TemplatesFS, never off disk.
                      render_test.go executes every page, so a broken template fails
                      the test run instead of the first request that reaches it
web/embed.go       — //go:embed templates/*.html → web.TemplatesFS and favicon.svg →
                      web.FaviconSVG; what makes the compiled binary standalone
                      (package web has no internal deps)
web/novnc/         — rfb.js: noVNC 1.6.0's core bundled into one ES module (MPL 2.0, see
                      its README for the rebuild command), embedded as web.NoVNC and served
                      once, cached for good, at /static/novnc/rfb-1.6.0.js - what the screen
                      viewer (templates/screen.html) runs
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
                      screen.html (a desktop's screen, /desktop/<id>/: see "The screen is
                      the control plane's page" below),
                      hostgui.html (the viewer for one app running on a private host
                      desktop: a canvas drawn from patches pushed over one WebSocket, which
                      also carries the input back), tasks.html (the autonomy dashboard, /tasks), task.html (one
                      task record with its steps, screenshots, interventions, verdict), trace.html
                      (/tasks/<id>/trace: the same record as a full trace - every step with its
                      timestamp and kind, questions and answers, the final report - with
                      /tasks/<id>/trace.txt as the plain-text twin for copy/download; opened
                      from the task dropdown on each dashboard card, which polls
                      /api/desktops/tasks), recordings.html (/recordings/<id>: record a demonstration,
                      browse recordings step by step, replay, export, and Teach - which
                      starts a lesson from it and any other demonstrations picked) and
                      skills.html (/skills/<id>: what the desktop was taught - run one
                      with its values, see its steps, delete it, finish an unfinished lesson)

desktop container (decurix-desktop:{basic,developer,ai})
  ├─ base image: accetto/ubuntu-vnc-xfce-chromium-g3 (XFCE + VNC); its Chromium 112 is replaced by
  │    Google Chrome stable behind /usr/bin/chromium-browser (see "The browser is Google Chrome" below)
  ├─ XDG autostart (agent/decurix-agent.desktop) → agent/start_agent_terminal.sh
  │    → /opt/vm_server.py (detached background process, no visible terminal); it
  │    also reinstalls agent/chromium-browser.init as ~/.chromium-browser.init (the
  │    browser's launch flags) and clears core dumps Chromium left behind
  └─ decurix_agent/ (Python package, imported by vm_server.py; :8081 inside the container)
        ├─ config.py       — env vars (incl. AGENT_HARNESS), /home/headless/.decurix/
        │                     {command,querytext,final,memory} dirs and /home/headless/work
        ├─ state.py        — shared mutable state (current_task, waiting_for_query, ...) as
        │                     plain module attributes; every other module does
        │                     `from decurix_agent import state` and reads/writes
        │                     state.current_task etc. (module singleton = shared globals)
        ├─ identity.py     — get_identity_context(engine): who this desktop is (name, ID,
        │                     user, work dir), as facts for the system prompt. Shared by all
        │                     three harnesses - without it "what's your desktop name?" was
        │                     unanswerable and came back as a query
        ├─ memory_context.py — get_memory_context: reads ~/.decurix/memory/*.txt; shared
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
        │                     by watchers.py, and written to /tmp/decurix-current-task so the
        │                     `gui` command and the MCP server (other processes) report it too
        ├─ usage.py        — per-task model/tokens/cost totals each harness adds to, sent by
        │                     messaging.send_query with every reply (llm_client.chat adds every hosted
        │                     call itself; TheBot's own calls come back with each soul job)
        ├─ playbooks.py    — what a worker keeps from a task it worked out itself, once its result was
        │                     checked: a script it wrote and proved (shell and file work), its own run as
        │                     a recording to replay (screen work), the commands as a fallback
        │                     (see "A worker gets cheaper at its work" below)
        ├─ tool_task.py    — a request done by such a script with no engine turn: one small call reads the
        │                     request into its arguments, it runs, the final check reads the result back.
        │                     Dispatched by the watcher before the engine; reports as engine "tool"
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
        ├─ azure_harness.py + llm_client.py
        │                   — the "azure" harness ("Office (GPT)" in the create dialog): a
        │                     tool-calling loop on the hosted model, reached through the
        │                     control plane (llm_client -> /api/llm/chat). Tools: shell (the
        │                     dangerous ones wait for a yes), the MCP server's own screen /
        │                     recording / skill tools (so the approval gate and the audit trail
        │                     apply as for Claude), tools/office.py (read_document, read/create/
        │                     update_workbook, combine_pdf), draft_email (to the Outbox - it can
        │                     never send), ask_human, finish. A pause keeps the conversation in
        │                     module state; continue_after_query answers the paused tool call.
        │                     llm_client also answers every harness's ask_model when DECURIX_LLM=1
        ├─ study.py        — /learn with a hosted model: the model studies the demonstration (steps, notes,
        │                     key screens) into an `understanding`, asks only what it could not work out, one
        │                     at a time, folds each answer in (see "A worker learns with the brain" below)
        ├─ verify.py       — the check before any success is reported: the model judges the screen now,
        │                     the demonstration's end screen and what the tools observed; done / not_done /
        │                     unsure, and only a confident done is done
        ├─ guidance.py     — a taught task handed to the engine: what its model is told, and what the final
        │                     gate checks the engine's "done" against
        ├─ binding.py      — a taught skill's values read from the run's documents by the hosted model
        │                     (each parameter's recorded value is the example of what to read); used by
        │                     skill_task when a /skill run was not given them - see "Values come from the
        │                     run's documents" below
        ├─ tools/office.py — documents for back-office work, also the `office` shell command: PDFs
        │                     (pdftotext; a scan is shown as page pictures), images, workbooks
        │                     (openpyxl), Word, PowerPoint, .eml. update_workbook edits the sheet XML in
        │                     place (openpyxl drops charts): cells, plus replace_refs to extend what
        │                     charts/tables/names read; fullCalcOnLoad, calcChain dropped. create makes
        │                     .xlsx/.docx/.pdf/.pptx from JSON; update_presentation edits a deck in place
        ├─ procedures.py   — a standing procedure a person gives in a request ("from now on ..."), kept and
        │                     written under later "as usual" requests (see "Kept procedures" below)
        ├─ workflow_task.py + workflow_runner.py + workflow_catalog.py
        │                   — `/workflow`: durable multi-stage runs whose stages are ordinary engine or
        │                     taught-skill tasks, advanced only by artifact checks (bench/workflows/README.md)
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
        │                     replay.py (compile + run a recording with no model), and the
        │                     skills (see "Skills" below): learning.py (demonstrations ->
        │                     a draft skill, its questions, reading the answers), skills.py
        │                     (the store, ~/.decurix/skills/<name>.json), and what runs
        │                     teach it (see "Skills that heal, escalate and learn" below):
        │                     healing.py (re-find a control that was renamed or moved),
        visual.py (a picture of what was around a coordinate click, found again before clicking), lint.py (how far a recording can be trusted before it runs), constraints.py (what a typed field takes), ingest.py (a Chrome DevTools Recorder export read as a recording), ingest_agents.py (what a BrowserGym / AgentLab or OpenHands agent did, read as a recording), workflow.py (a program as subtasks with completion criteria, UI-Mate's shape), skillmd.py (a skill as a SKILL.md: Understudy's intent / routes / GUI-hints layers in the Agent Skills format),
        │                     escalation.py (the ladder: heal, ask the engine, ask a person),
        │                     experience.py (what a successful run keeps)
        ├─ record_task.py  — "/record <task>" opens a ticket: dispatched by the watcher instead
        │                     of the harness, holds the task open (module-private state, the way
        │                     replay_task.py holds its params) until end() finishes it, reports as
        │                     engine "human". begin()/pause()/resume()/end() are called directly
        │                     over HTTP by the GUI screen's own toolbar - not through the command
        │                     file/watcher path, since they only ever act on the ticket already open
        ├─ replay_task.py  — "/replay <recording>" as a task: dispatched by the watcher instead
        │                     of the harness, reports through messaging as engine "replay",
        │                     pauses/resumes on approval or a private step
        ├─ skill_task.py   — "/learn <recording>" asks a person what a recording cannot say and
        │                     saves a skill; "/skill <name> [param=value]" runs one with no model,
        │                     stopping to ask when unsure. Dispatched by the watcher instead of the
        │                     harness, reports as engines "learn" / "skill", pauses through
        │                     state.paused_skill the way replay_task uses state.paused_replay
        │                     (computer/skill_engine.py is the same for an *engine*: gui skills /
        │                     gui skill / gui do and the MCP tools skills / run_skill - it refuses
        │                     and says what is missing where skill_task would ask a person)
        ├─ ask_task.py     — `chatgpt "where is x"`, `ask chatgpt ...`, `google x`, `search youtube
        │                     for x`, `wiki x`: opens the service in Chromium with the question in
        │                     its address (core.open_target, audited) and reads the answer off the
        │                     page over DevTools - no model. Dispatched by the watcher after mimic,
        │                     before the harness; reports as engine "web"
        ├─ watchers.py     — command_watcher (polls COMMAND_DIR, dispatches through harness.py;
        │                     leaves a request queued in COMMAND_DIR while state.backend_ready
        │                     is False, rather than failing it)
        ├─ http_api.py     — Handler(BaseHTTPRequestHandler): POST /request, and the
        │                     recordings/demonstration/replay API the control plane proxies;
        │                     refusal() first: the agent token, no browser, JSON writes
        └─ app.py          — main(): wait for the backend, watcher thread, tidy recordings
                              (import the old audit trail, close an interrupted demonstration),
                              ThreadingHTTPServer.serve_forever
```

Control plane → desktop communication happens over the Docker bridge network `decurix-network` (container name `desktop_<id>`, not published ports). Desktop → control plane communication uses `CONTROL_PLANE_URL` (`http://decurix:8080` over `decurix-network` when the control plane runs from `docker-compose.yaml`, which sets `DECURIX_CONTROL_PLANE_URL`; `http://host.docker.internal:8080` for the released binary on the host, see `desktop.controlPlaneURL`) — always the primary listener below, never the gated one, since an agent's own callbacks must never depend on a human having typed an access code anywhere.

**Two listeners, one process, one set of routes.** `cmd/decurix/main.go` builds two `*http.ServeMux`s from the same `Server` and calls `RegisterRoutes` on both, so they serve identical behavior; only `RegisterLocalRoutes` (the LAN access code + rotate button, see `internal/httpapi`'s `lanaccess.go` above) is added to one of them. The primary listener (`:8080`) has no login, and what it serves is host-level power: it creates desktops (Docker socket) and `/api/host-bridge/*` runs commands on the host. So it is **never exposed beyond this machine and its own desktops**, by four independent layers: (1) `docker-compose.yaml` publishes it on `${DECURIX_ADMIN_BIND:-127.0.0.1}` only, so the LAN cannot reach it; desktops call back over `decurix-network` instead of through the published port (`DECURIX_CONTROL_PLANE_URL=http://decurix:8080`); (2) no script tunnels it - `scripts/*/build.sh`/`rebuild.sh` open exactly one Cloudflare quick tunnel, to the gated listener (`lan_tunnel_url`), and their foreground loop only keeps the script alive; (3) `httpapi.RefuseForwarded` wraps it and answers 403 to any request carrying `Cf-Connecting-Ip`/`Cf-Ray`/`X-Forwarded-*`/`Forwarded`/`X-Real-Ip`, so a stale script or a hand-run `cloudflared` pointed at `127.0.0.1:8080` gets nothing (a safety net, not the boundary - headers are a deny-list); (4) `RegisterLocalRoutes` puts everything that runs or shows something on the host (`/api/host-bridge/{exec,open}`, `/api/host-gui/*`, `/host-gui/view`) and the access code itself on the primary mux only, so the gated listener - reachable by anyone holding the code - has no route to host code execution (`TestHostRoutesAreOnlyOnThePrimaryListener` pins the split; a test that needs those routes must call `RegisterLocalRoutes` too). The second listener (`:8090` by default, `DECURIX_LAN_PORT`) is wrapped in `internal/lanaccess.Store.Middleware` and gives out a rotating access code (shown in the dashboard's Mobile menu) before granting a session cookie - published on all interfaces by compose (`DECURIX_LAN_BIND`; the Windows app publishes it on the WSL distro's 127.0.0.1, since its LAN link and tunnel come in through Windows' own loopback), reachable on the local network as `lan_url` and off it through its own tunnel; see the lockout note on `lanaccess.Store.recordFailure` below for what changes once real tunnel traffic reaches it. **Consequence:** a desktop created before this change has `CONTROL_PLANE_URL=http://host.docker.internal:8080` baked in, and on a native-Linux host that address (the docker0 gateway) no longer reaches a port published on loopback - recreate such desktops. **The host bridge only runs what a launcher was signed for.** Anything running in a desktop (an agent that browses the web and runs shell commands) can reach the primary listener over `decurix-network`, so `/api/host-bridge/exec` requires `desktop` + `token`, where `token` is `winhost.ExecToken` (HMAC over the desktop ID, program and the JSON-encoded arguments, keyed by the same `hostgui.key` as launch tokens; `ValidExecToken` in `hostBridgeExecHandler`). A launcher gets its token from `hostFallback` when the app is copied; the arguments must match exactly, so a launcher's own `"$@"` is no longer passed on. `host-run` has two modes only - `open <token> <https link>` (a web app's launcher, `winhost.OpenToken`) and `signed <token> <cmd> args` - and the generic `host-run <anything>`, `win-run`/`mac-run` and the reverse-SSH fallbacks (an unsigned path around the check) are gone. `/api/host-bridge/open` opens only the link a launcher was signed for, from that desktop, and only a `safeOpenURL` link (plain https to a public name on port 443: no address in any spelling, no localhost/`.internal`/`.local`/single-label name, no other port, no user name - a GET from the person's browser to `127.0.0.1:8080` arrives as the person). There is no clipboard route: a credential the worker holds could not stop a prompt-injected worker from putting a command there for the person to paste. A desktop can still ask for exactly the commands and links its launchers carry; it cannot run or open anything else. Launchers written before this stop working (403, and `host-run` says to copy the app again: `POST /api/desktops/host-apps/copy`). **Still true:** the control plane holds the Docker socket, which is root on the host - it is what creates desktops and, on a WSL host, runs the privileged `nsenter` hop behind a signed Windows command. Removing it means a narrower Docker access path (a rootless/remote daemon or a proxy that filters `--privileged`, host mounts and `pid=host`), not a network fix. `desktop.Manager.StaleControlPlane` logs, once at startup, the desktops still carrying the old callback URL. The released binary binds the primary listener to loopback plus the Docker bridge gateways (`primaryListenAddrs`, `Manager.GatewayIPs`: `decurix-network`'s and `bridge`'s IPv4 gateway - what `host.docker.internal` resolves to on Linux), so desktops still call back through `host.docker.internal` and the LAN cannot reach it (checked live: `127.0.0.1` and a container answer, the LAN address refuses). Inside a container it binds every interface, since compose decides what is published. `DECURIX_ADMIN_ADDR` (comma-separated `host:port`) overrides it. A gateway address that cannot be bound (Docker Desktop, where `host.docker.internal` reaches host loopback anyway) is skipped with a warning.

**Who may call what, since the review of 2026-11-02** (`internal/httpapi/guard.go`). The review created a desktop and queued a task from a page on another site, so both listeners now sit behind a guard, and each check is code, not a convention:

- **No cross-site writes.** `GuardPrimary` and `GuardGated` refuse any method but GET/HEAD/OPTIONS, and any WebSocket, unless it is same-origin (`Origin` equals the `Host`, or the tunnel's `X-Forwarded-Host` on the gated listener) or carries no `Origin` at all (a script, the Electron main process, an agent's urllib). `Sec-Fetch-Site: cross-site`/`same-site` and `Origin: null` are refused.
- **No DNS rebinding.** The primary listener serves only Host names it is known by: an IP address, `localhost`/`*.localhost`, `host.docker.internal`, a single-label name (`decurix`, a container name), or one listed in `DECURIX_ALLOWED_HOSTS` (comma-separated). Anything else gets 421.
- **On the primary listener only the host is a person** (third round, 2026-11-03; `desktop.NetView`, `Manager.Networks`, `httpapi.origin`). A person is loopback or a gateway the host comes in through: decurix-network's and the default bridge's (from Docker), and in compose the control plane's own network's default-route gateway (`/proc/net/route`, no Docker needed), which is where a port compose publishes is handed over from. Every bridge network's range is a container range, and an address in no known range is unknown. On the primary listener, anything that is not the host is held to the desktops' callbacks below and must be a desktop's own address there: verified live in the second round, a container on a network someone else made read the access code and the backup, and created a DAG and a desktop. The gated listener has a login of its own and refuses only a known container address, so a phone or a LAN laptop reaches the login even while Docker answers nothing. What the interfaces say (the control plane's own network in compose; the Docker bridges docker0/br-* on a Linux host) keeps a desktop refused there too. The network view and the address-to-desktop map are each fetched by one call at a time and shared (`desktop.refreshed`). A known address is answered without waiting for Docker. An unknown one makes Docker be asked again at most every 2 s, and the address map is dropped whenever a desktop is made or removed here.
- **A desktop gets its callbacks and nothing else.** A request from anyone but the host (`fromContainer`) may use only the routes in `containerRoutes` (the model proxy, grounder, agent events, query notes, its own memory, outbox drafts, org knowledge and skills read-only, handoffs, host-file checks, the signed host bridge and host-GUI views). Everything else is 403: sending requests (a worker answering its own approval), creating desktops, the LAN access code, verdicts, DAG/cron jobs, the tunnel link, Claude login, settings. Adding a callback means adding it to that list on purpose; `TestContainerRoutesAreServed` checks each entry is a real route.
- **A desktop is who its address says.** `Manager.DesktopAt` maps a container's address on `decurix-network` to its desktop ID, and the guard puts it in the request (`requestDesktop`). `actingAs` makes every callback act as that desktop and refuses a body naming another one (memory, agent events, query text, outbox, host files, the host bridge's token desktop); the handoff routes do the same through `handoff.Handler.Caller`. An address in the network that is no desktop's gets nothing.
- **The gated listener serves no container at all.** It refuses every address inside a known Docker network. A desktop that hairpins through Docker's proxy on a port published on every address arrives from the gateway, indistinguishable from the host, and the Windows app publishes the port on loopback only for that reason. A checkout's compose still publishes it everywhere for the LAN; there the hairpin shares the gateway's lockout bucket.
- **What a desktop serves through the control plane is sandboxed.** `/desktop/<id>/...` (noVNC's own page) and `/agent/<id>/...` (TheBot's page) come from inside a container but would run as the control plane's origin; they now carry `Content-Security-Policy: sandbox ...`, so their scripts have an opaque origin the guard refuses. The screen a person uses is the control plane's own page with its own noVNC (`screen.go`) and is unaffected.
- **A reply answers its own card.** `/api/desktops/send` takes `note_id`; when the desktop has asked something newer, or the note is closed, it answers 409 (`replyStillAnswers`), so a "yes" under an old card cannot approve a newer action.
- **Per-desktop secrets.** A new desktop gets `DECURIX_AGENT_TOKEN` (HMAC of its ID under `hostgui.key`, `Manager.AgentToken`), which the control plane sends as `Authorization: Bearer` on every call to its agent (:8081, and soul :4900 through `/agent/<id>/`), and a VNC password of its own (`screenPassword`, 8 characters, VNC's limit) instead of the shared "admin". Both protect a desktop from other desktops and from web pages; the worker's own shell runs as the same user as its agent and can read them. **The desktop's side:** `http_api.refusal` checks the token (constant-time) on every :8081 route but `GET /health`, and, with or without a token, refuses a request carrying `Origin` or `Sec-Fetch-*` (a web page in the desktop's Chrome), a Host that is a DNS name (rebinding; the control plane calls by IP or container name) and a write with a non-JSON body; a desktop without a token is otherwise served as before until it is recreated. In-container callers send it through `config.agent_auth_headers()` (the TheBot bridge's calls to soul). A reply is a reply only when its `task_id` is the waiting task's (`messaging.save_request`, docs/chat.md). The browser extension's backend `ext_server.py` (:8092, 127.0.0.1 only) follows the same rules in `extension_pipeline/guard.py` - the extension's `chrome-extension://` origin or the token, no CORS - and takes an `approved` list only from a token-bearing caller for an action its gate stopped on (agent/BROWSER_INTEGRATION.md).
- **Nothing of the person's computer is copied into a desktop at creation.** `host_paths` is refused (it reached `/data`: the keys and the database); the host-app list packs launchers only, never a browser profile, SSH keys, tokens, keyrings or dotfiles; the Windows bridge no longer seeds its private browser from the person's own profile (`DECURIX_IMPORT_HOST_BROWSER_PROFILE=1` opts back in). The shared folder is the one way to give a worker files.

- **Fail closed** (second round, same day). A desktop's agent (:8081) and soul (:4900) serve nothing but their health check without a token, so a desktop created before tokens must be recreated. No desktop is created when the control plane could not load its secret (`hostgui.key`): it would get no token. That includes a practice desktop (`CreatePractice` answers `ErrNoSecret`, and the run's note says why). `DEFAULT_PASSWORD` is no longer read; every desktop gets its own screen password. `fromContainer` takes an address that is not loopback, not a gateway and in no known range as a reason to ask Docker again in the background (at most every 2 s). On the primary listener that address is never a person (third round). The practice network is one of the container ranges and never one of the addresses the control plane listens on (`GatewayIPs`); its subnet cache asks again every second while it knows no range.
- **Nothing a desktop serves runs as the dashboard.** `relayAgent` (recordings, skills) gets the same sandbox policy and `nosniff` as the proxies, and every response of both listeners says `frame-ancestors 'self'` / `X-Frame-Options: SAMEORIGIN`, so another site cannot frame an Approve button. The legacy `/desktop/<id>/vnc.html` and TheBot's own page at `/agent/<id>/` no longer work inside the sandbox; the dashboard uses neither.
- **`/api/host-bridge/open`** hands the link to Windows' URL handler (`rundll32 url.dll,FileProtocolHandler`) as one argument (`hostOpenCommand`): no `cmd.exe` parses it, so `&` and `%` in a link stay text. Signed host commands are shell-quoted too (Go's `%q` is not shell quoting). Whether a host command is a Windows program (the hop to Windows from a control plane in a container) is read from its first word (`isWindowsProgram`): reading the base name of the whole line broke every open in the Windows app.
- **Host files are decided on as they resolve** (`resolveHostFile`). Each part of the path must be a name its folder lists, which excludes an 8.3 alias, a trailing dot or space, and a different case (the listed name is used). Every symlink and junction is followed and checked again from the drive's root, and anything under AppData (or its old junction names), ProgramData, Windows or a dot-folder, or that cannot be resolved, is refused. `/api/host-files/read` serves only documents (`readableHostDocument`).
- **Deleting a desktop while it is being created** stops the creation and waits for it to clean up before anything is removed; its name stays claimed until then, and a creation only ever removes its own entry, so a re-created desktop of the same name is never touched.

Desktops created before these changes have no agent token and the old screen password: they must be recreated.

**What is still not a boundary.** The worker's shell runs as the same Linux user as the agent that holds the approval gate, its state, its reply channel and its token. It can write a reply file, read the token, edit `approval.json`, or click with `xdotool` directly. Everything above keeps out other desktops and web pages, not the worker itself. Closing that needs the gate in a separate user or process (see the 2026-11-02 recheck).

The scripts also discover this machine's own LAN-facing IP (`ip route get 1.1.1.1`'s `src` field on Linux/macOS — asks the kernel which address it would use, no packet actually sent; a UDP `Connect` to a public IP on Windows, the same trick) and `POST /api/tunnel` `lan_url` (the gated port) built from it — stored on the same `tunnel.Store` as the gated tunnel's `lan_tunnel_url` and shown together in the Mobile menu (tunnel link first since it works off-network), so a phone on the same network can skip the tunnel round-trip entirely. They no longer report `admin_url` (the primary listener is not reachable from the LAN) or `url` (nothing tunnels it); `tunnelHandler` and the store still accept both, only so an older script's report does not error, and the Mobile menu shows them under a clearly-labelled unauthenticated heading if one arrives. `httpapi.tunnelHandler`'s POST only ever sets the fields present in that call's body, since the LAN report (once, right after startup) and each tunnel-URL report (once per line the `cloudflared` process prints) are separate calls that must never blank out each other's fields.

Go package dependency direction (no cycles): `httpapi` → `llm`, `outbox`, `desktop`, `querynotes`, `memory`, `conversation`, `stepimages`, `tasks`, `tunnel`, `claudeauth`, `lanaccess`, `schedule`, `mailtrigger`, `keepawake`, `suggest`, `webui`, `winhost`; `webui` → `desktop`, `querynotes`, `memory`, `tasks`, `web`; `desktop` → `ollama` (only for `ollama.DesktopURL`/`DesktopModel`, the Ollama a new desktop is given), `winhost` (only `winhost.Token`, to sign a launcher's token), `decurix` (the root package, for the embedded build context); `idlepause` → `desktop` (only for the `desktop.Desktop` struct and the `DesktopLister` interface `*desktop.Manager` satisfies) via a narrow interface, so it can be tested with a fake instead of a real Docker client; `schedule` has no internal dependency of its own - its `Runner` reaches `httpapi` only through the `Submitter` interface `*httpapi.Server` satisfies (`Submit`), the same narrowing `idlepause` uses for `desktop`; `suggest` → `memory`, `ollama` (only for `ollama.Embed`), `desktop` (only for the `desktop.Desktop` struct and its own `DesktopLister`, the same narrowing `idlepause` uses), `tasks` (only for its own `ActivityStore`, narrowing `tasks.Store.LastActivity`), `sqlitedb`; `querynotes`, `memory`, `conversation`, `stepimages`, `tasks`, `claudeauth`, `schedule`, `suggest`, `devices`, `outbox` → `sqlitedb` only; `mailtrigger`, `cftunnel`, `llm` → `sqlitedb`, `sealed`; `phonehost` → `wsconn`; `phoneagent` → `wsconn`, `phonehost`; `httpapi` → `skillhub`, `knowledge`, `handoff`, `access`, `workqueue` (the workforce: docs/skillhub.md, docs/handoff.md, docs/access.md, docs/workqueue.md); `httpapi` → `practice` (Simulate: docs/simulation.md; `practice` → `qualification`, `llm`, and the desktop manager through interfaces); `skillhub`, `knowledge`, `access`, `workqueue` → `sqlitedb` only (`workqueue` reads the tasks component's `executions` table for spend); `handoff` → `sqlitedb`, `tasks` (status constants only) - each reaches desktops, task submission and worker lookup only through interfaces `httpapi` backs (`internal/httpapi/handoffs.go`); `ollama`, `sqlitedb`, `sealed`, `wsconn`, `tunnel`, `lanaccess`, `winhost`, `keepawake`, `web`, `decurix` depend on nothing internal. Note that `desktop` does *not* depend on `claudeauth`: `httpapi.createDesktopHandler` reads the stored session and hands `BeginProvisioning` its raw credentials JSON as a plain string, and `claudelogin.go`'s login flow hands the same shape back to `httpapi` to save — `desktop` only ever sees bytes, never the store.

One desktop image is built, `docker/desktop.basic.Dockerfile` (`decurix-desktop:basic`), plus the shared tools image `docker/tools.Dockerfile` (`internal/desktop/images.go`). `BuildImages` also tags the basic image as `decurix-desktop:developer` and `decurix-desktop:ai`, so a desktop created with one of those older profile names still starts on the same image. The separate developer and ai Dockerfiles were never built and have been removed.

## The shared folder: one folder of the person's computer, opt-in

A worker cannot see files that live only on the person's computer, so it had to push every value through a host app's viewer a key at a time (slow, and cut off at the operator's command limit with rows missing). A desktop can be made with **one shared folder**: `shared_folder` on `POST /api/desktops/create` (the create form's "Shared folder" field), e.g. `C:\Users\Administrator\Documents\Decurix demo`. The worker computes with its own tools in it and the person's own apps open the same files.

**What the worker can and cannot touch.** Only that folder, read-write. Nothing else of the computer is mounted, no HTTP route is added, and a desktop's chat, skills and memory are as before. It can read, change, create and delete files in it (including links: a link it makes there is just a link object on the computer, resolved inside the container where it leads nowhere on the PC). It cannot reach a parent folder: the bind's root is the container's own view, and `shared.contained` checks a path's real location before the agent reads it. Secrets stay withheld (`privacy.py` is unaffected). It is fixed at creation, like the rest of the desktop's state in `$HOME`: to share another folder, make a new worker.

**How.** `desktop.ParseSharedFolder` validates what the person typed and refuses, in a sentence: a relative path, a network path (`\\host...`), `..`, another drive than `C:`, a drive root, a system folder (Windows, Program Files, ProgramData, a Linux/macOS `/etc` `/usr` `/var`...), the Users folder, a whole home folder, anything under AppData/`.ssh`/`.aws`/`.config`..., a file, a folder that does not exist, a link (a junction could point at a system folder), and control characters. A Windows folder is checked where the control plane sees the `C:` drive (`DECURIX_HOST_MNT_C`, `/mnt/c` in the container, read-only) and bound from where the Docker daemon sees it (`DECURIX_HOST_MNT_C_SOURCE`, default `/mnt/c`, for a daemon that mounts the drive elsewhere); a Linux or macOS folder is bound as it is. The bind is a `HostConfig.Mounts` entry, not a `Binds` string: the daemon refuses a source that does not exist, where a bind would make it on the host. It is mounted at `/mnt/pc`, **outside `$HOME`**: the base image's startup runs `sudo chown -R $UID /dockerstartup $HOME` on every start, which would walk into the person's own files. The agent links `~/PC` to it when it starts (`shared.ensure_link`; a recursive `chown` does not follow a link - checked). The container gets `DECURIX_SHARED_FOLDER` (the folder as written) and `DECURIX_SHARED_MOUNT`, and the label `desktop-shared-folder`, which `Desktop.SharedFolder` reads for the card ("Shares C:...").

**Two listeners.** `shared_folder` is honoured only on the primary listener: `httpapi.MarkGated` tags every request that reaches the gated listener and the create handler answers 403 to a `shared_folder` there, as it must for anything that reaches the host.

**Path mapping (agent, `shared.py`).** A path the person writes under their folder - any case, `/` or `\`, or `/mnt/c/...` - is `/home/headless/PC/...` here (`shared.to_local`). `office` maps it itself (`_path`); the final check asks the disk instead of the host route for a file saved there (`produced._ask_host`, `copy_from_host`: the local file is read where it is) and counts what the task made there as written (`produced.files`); the judge reads the inputs the task was given there (`verify._shared_inputs`); soul traces a typed `/home/headless/PC/...` path (`workFolders`) and tells the operator in one generic line (`sharedRule`, only when the desktop has one). A request that names an app to work in is still done with that app: `behindTheApp` lets the content be made with tools in the shared folder (the app sees the same file), and `sharedAppUntouchedNudge` sends a DONE back once, and then asks the person, if no command ever opened the app.

**Host hops.** When the control plane runs in a container, everything it runs on the host goes through `desktop.HopCommand`: a privileged alpine container in the host's PID namespace, named `decurix-hop-<purpose>-<random>` and labelled `decurix.hop=1`, removed with `docker rm -f` when its context is cancelled or its session ends (`winhost.endHost`), and swept at start (`SweepHopsAtStart`). Never start one with a bare `docker run`.

**Requested host apps that are not added.** `MigrateHostApps` never skips one silently: each requested app it does not add is a `desktop.MissingApp` (name, plain reason, `Pending`), kept by the Manager (`missingapps.go`), returned in the create answer, `/api/desktops/status`, `/api/desktops/activity` (the card) and `POST /api/desktops/host-apps/copy`, and written to `~/.decurix/missing-apps.json` for the desktop's agent (`identity.py` lists them; `gui open` of one is refused instead of opening a stand-in). An app the host-app check could not reach (`HostAppItem.Unverified`: the helper died or timed out) is not a verdict: it is never cached, it is asked about again when a desktop wants it, and `retryPendingApps` tries it in the background for about six minutes, copying its launcher into the running desktop when it checks out. `RestoreMissingApps` reads the files back at start.
