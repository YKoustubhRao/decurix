# Architecture: packages, desktop container, listeners

Sections this tree calls "below" that are not in this file live in [gotchas.md](gotchas.md) (grep the title),
[computer-use.md](computer-use.md), [recordings.md](recordings.md), [skills.md](skills.md) and [healing.md](healing.md);
"The --cli terminal client" is in [running.md](running.md).

```
cmd/decurix/main.go — entrypoint: loads .env, builds the Docker client, wires
                       the packages below, registers routes, and serves the
                       primary listener (:8080) plus a second, access-code-gated
                       one (:8090 by default - see "Two listeners, one process"
                       below); the practice broker listens on :8091 of the
                       practice network (docs/simulation.md). Flags that exit
                       instead: --version; --build-images (--rebuild, --images
                       desktop|tools|all) builds the images from the embedded
                       context; --cli (--url) hands off to cli.go; --phone and
                       --phone-setup run the phone link (phone.go); and
                       --restore-backup <file> (--force) puts a backup's data
                       files back (restore.go)
dockerstartup.go   — dockerStartup: what needs Docker, run by
                       Manager.WatchDocker once Docker answers: the network, the
                       per-desktop networks (sweep and join), the screen
                       passwords held again, the sweeps of hops, home-copy and
                       host-app helpers, images and the tools volume, and the
                       cleanup of practice and readiness containers
cli.go             — runCLI: an interactive REPL that is the same binary's
                       --cli mode, a text alternative to the dashboard/chat
                       page for a control plane already running somewhere -
                       reached purely over internal/httpapi's existing JSON
                       API (GET/POST /api/desktops*, /api/querynotes), so it
                       needs no Docker client and no database of its own
restore.go         — runRestore: a backup's database (and, in an old backup,
                       key files) into the data dir; never deletes (replaced
                       files go to before-restore-<time>/), refuses while a
                       control plane answers at --url unless --force
cmd/decurix-model-selector — the Windows app's companion (127.0.0.1:11436 by
                       default): measures the Windows machine and chooses the
                       model for the native Ollama (DECURIX_WINDOWS_SELECTOR_URL;
                       built by scripts/windows/build-electron.ps1)
embed.go           — package decurix at the MODULE ROOT: //go:embed of
                       docker/desktop.basic.Dockerfile, docker/tools.Dockerfile
                       and the agent files the images COPY (not agent/tests)
                       → DesktopBuildContext. agent/thebot is its own module, so
                       `go generate .` packs it as agent/thebot.tar.gz (and may
                       prebuild agent/thebot-soul); a binary built without that
                       step cannot build the images. It has to live at the root
                       because go:embed cannot reach above its own directory.

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
                      Screenshot (screenshot.go: a minimal RFB client that reads
                      a desktop's framebuffer straight off Xvnc:5901 and returns
                      a cached JPEG thumbnail — what the dashboard cards show),
                      sandbox.go (the seccomp profile + core-dump ulimit every new
                      desktop is created with — what lets Chromium run sandboxed;
                      the profile itself is seccomp-desktop.json, go:embed'ed),
                      desknet.go (each new desktop's own bridge network,
                      decurix-vm-<id>, a /28 of DECURIX_DESKTOP_SUBNETS; the
                      control plane joins each with the lowest gateway priority),
                      hostfence.go (on the Windows app's WSL host, no desktop is
                      made or started while the fence between desktop networks is
                      missing), DesktopModelURL (the OLLAMA_URL a desktop gets: the
                      control plane's /api/native-ollama proxy, never the model
                      server itself), agenttoken.go (the agent token, the gate's
                      key and the per-desktop screen password), homefs.go / hop.go /
                      practice.go (see the sections at the end and docs/simulation.md)
internal/ollama    — DesktopURL/DesktopModel (the model server the control plane uses and
                      the model every desktop is given). EnsureReady at startup chooses the
                      model and, for the bundled "decurix-ollama" container (used only when
                      OLLAMA_URL names it), provisions it and pulls; it loads nothing.
                      WarmAsync (debounced) loads the model when a desktop starts, a chat
                      opens or a request is sent, and only when some desktop uses the local
                      model or no hosted one is set up (SetWarmNeeded)
                      select.go/autoselect.go/resources.go (+ mem_<os>.go): the model
                      chooser - probes RAM/CPU/GPU and picks the smartest chat model that
                      fits 50% of what is free (see "The chat model is chosen for this
                      machine" below); served at GET /api/model (httpapi/model.go) and
                      shown as the chip at the top left of every page (theme.html's
                      "topbar")
internal/sqlitedb  — opens the one embedded SQLite database (modernc.org/sqlite, pure Go,
                      so releases stay CGO_ENABLED=0) at DECURIX_DATA_DIR/decurix.db, and
                      Migrate(db, component, steps): each store below owns its own tables
                      and appends schema steps, never edits an applied one. Also InstanceID
                      (the random instance tag, see the end) and KeepVacuumed (freed pages
                      returned to the disk, a batch an hour)
internal/querynotes — queries submitted by agents (SQLite). A note is resolved, never deleted: List is the open
                      ones (the rail, the badge, the Queries tab), ListResolved the history
internal/memory    — worker-memory entries (SQLite); deleted with the desktop they were learned on (DeleteForDesktop)
internal/delivery  — what crosses between the control plane and a desktop, kept across a restart (after
                      agent-orchestrator's detached hosts). Sending: a request is written down (as the records would
                      keep it, withheld) before it is sent and removed once tasks hold it; one left by a crash is
                      settled at the next start by asking its desktop (agent GET /requests/<task>?since=, receipts.py):
                      recorded, dropped with "send it again", or kept unconfirmed so its late outcome is accepted
                      (httpapi/deliveries.go, ReconcileDeliveries). Applied events: a desktop's message carries an
                      event_id and is resent until acknowledged; one id is applied once (Store.Apply)
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
                      Keep/Forget/ForgetDesktop (an app kept running, ready to show), DeleteDesktop
                      (a deleted desktop's host browser profiles removed by the bridge's dropprofiles
                      verb: exact folder names under its own profile root, links never followed; a
                      stop keeps them, they hold the worker's sign-ins), controls.go
                      (Controls: the app's controls from UI Automation, and Picture);
                      token.go signs and checks the launch, exec and open tokens (LoadSecret keeps the key in the data dir). Takes its host
                      command as a Starter, so it depends on nothing internal and is tested
                      against a bash fake speaking the same line protocol
internal/tunnel    — in-memory store of the addresses scripts/linux/build.sh
                      (or rebuild.sh, or their Windows twins) discovers and
                      reports via POST /api/tunnel: a Cloudflare quick tunnel
                      URL for the gated listener below (LANTunnelURL) and this
                      machine's own LAN address for it (LANURL), surfaced in
                      the dashboard's Mobile menu; Get/AdminURL (the primary
                      listener's) are legacy - no current script reports them
internal/idlepause — Checker.Run: a ticker (every 2 min) that pauses a desktop once it
                      has sat idle (tasks.Store.Busy empty, and past Threshold since the
                      later of its container's own start time, its last task
                      activity, and desktop.Manager.LastViewed), or stops it when
                      DECURIX_IDLE_ACTION=stop - on by default at 5 minutes
                      (cmd/decurix.defaultIdleStopMinutes), DECURIX_IDLE_STOP_MINUTES=0
                      opts out, and it never does more than the Pause/Stop a human's
                      own button does; Busy is asked again under LockDesktop.
                      FastPauseAfter checks a desktop again soon after its task ends.
                      It acts only on desktops this control plane created
                      (desktop.OwnerLabel, Desktop.Owned): a second control plane on
                      the same Docker never pauses, stops or sweeps another's.
                      room.go (lightweight L2): a desktop paused for StopAfter
                      (DECURIX_IDLE_STOP_AFTER_MINUTES, 30) is stopped; and MakeRoom,
                      desktop.Manager's RoomMaker, runs before every start (not a wake:
                      a paused desktop still holds its memory) and stops the longest-idle
                      paused desktops, only as many as needed, until the Docker host's
                      MemAvailable covers the start (min(its limit, 700 MB)) plus a
                      512 MB floor; with nothing to stop it starts one that fits without
                      the floor and refuses the rest (desktop.NoRoomError, "not enough
                      memory: N desktops are busy"). Starts let through count 90 s
                      against free memory, so two at once do not share it. Never stopped:
                      not paused, Busy (task, ticket, open question or approval), a
                      practice/eval/readiness desktop, or started, woken, looked at before
                      its pause or worked on in the last 10 min (a paused desktop's frozen
                      thumbnail does not count: opening its screen wakes it). Each stop is
                      under the lifecycle lock with Busy asked again; MakeRoom only
                      TryLockDesktop's, so a request holding the lock wins. The stopped
                      desktop's Desktop.rest_reason ("idle" | "memory"), rest_note and
                      rested_at say why until it runs again (in memory only).
                      gone.go: the live tasks of a desktop whose container is gone are
                      closed at start and on each sweep (never the control plane's own
                      practice/readiness workers on a sweep)
internal/resourcemon — Monitor.Run: every 5 s, a running desktop's CPU and memory
                      (Docker stats) against DECURIX_RESOURCE_CPU_THRESHOLD /
                      DECURIX_RESOURCE_MEM_THRESHOLD (80%); above either, an alert with a
                      predicted size, shown on the screen page and the dashboard
                      (/api/desktops/resource-alerts|increase|dismiss|report); Increase
                      applies it and cools that desktop down for 5 min.
                      hostmem.go: HostMemory.AvailableMB, the Docker host's MemAvailable
                      (what idlepause.MakeRoom measures), read from the control plane's own
                      /proc/meminfo and trusted only when its MemTotal is within 5% of the
                      daemon's (desktop.Manager.HostMemTotalMB): true in a container on the
                      Docker host or a binary on Linux; elsewhere (a binary on macOS) it is
                      "not known" and desktops start without making room
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
                      contract. SetOpenPaths lets the phone-setup paths past the
                      gate (each checks a token of its own); CloseDevice ends a
                      revoked device's open screens and streams (streams.go).
                      Depends on nothing internal by design, same
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
internal/outbox    — emails agents drafted (`office email`, POST /api/outbox
                      from a desktop), waiting for a person: the Triggers page's Outbox lists
                      them with their attachments, and only a person's Send (humansOnly)
                      hands one to mailtrigger.Store.SendMail. Claim (draft -> sending) makes
                      a double click send once; a send interrupted by a restart is marked
                      failed, never resent on its own. Depends on nothing internal but sqlitedb
internal/sealed    — AES-256-GCM for the few secrets kept in SQLite, key in a file of its
                      own in the data dir (mailtrigger's mail.key, cftunnel's cloudflare.key, llm's llm.key;
                      an old claude-session.key is left unread since the Claude login went, 2026-11-07)
internal/runs      — the run record (docs/trajectory.md): Puller takes a run's bundle off its desktop when the
                      run ends (limit DECURIX_RUN_BUNDLE_MB, default 64, runs.BundleLimit) and Build
                      assembles decurix.trajectory/v1. A desktop delete first pulls what is missing; runs it
                      still could not save make the delete answer 409 unless a person forces it (a
                      delete_anyway judgement names who and which runs were given up)
internal/purge     — purges a leaked secret by value from the never-delete stores (blobs,
                      llm_calls, judgements, task records, chat, concierge, query notes, run
                      bundles): each copy becomes `[purged]` in place through a small
                      PurgeValue/PurgeForms method each store owns, keeping blob names, row ids
                      and lengths so records still join, and records a `purge` judgement (who,
                      why, counts; never the value). Served by httpapi/privacy_purge.go
                      (RegisterLocalRoutes, humansOnly, owner role) and `decurix --purge-secret`
                      (docs/trajectory.md, "Purging a leaked secret")
internal/codemap   — writes CODE_MAP.md from every tracked file's own header comment
                      (`go run codemap_gen.go`); TestCodeMapIsCurrent fails when it is stale
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
internal/schedule  — recurring or one-time-future requests set up from the
                      requests page (not the chat): Store persists each one
                      (desktop, request text, either a standard 5-field cron
                      expression or a single future run time (RunOnce), next/
                      last run) in SQLite; cron.go is a small pure-Go
                      parser/matcher (no external dependency, matching the
                      sqlitedb/modernc.org choice to stay CGO_ENABLED=0) with
                      the standard day-of-month/day-of-week OR rule, matched
                      on the wall clock of the schedule's own IANA zone
                      (`zone`: the browser's at creation, else the control
                      plane's TZ or /etc/localtime, else UTC; rows from
                      before zones read UTC). Daylight saving follows vixie
                      cron: a fixed-time job in the skipped hour runs at the
                      jump, and once in a repeated hour. The binary embeds
                      time/tzdata. A DAG's cron trigger has its own zone
                      (`cron_zone`) under the same rule. Runner ticks (20s default) and fires a schedule
                      through the Submitter interface once its next-run time
                      has arrived, appending "\n\nTIME:{<RFC3339 in its zone>}" to the
                      request text so the agent knows when that run
                      triggered. Every occurrence has a row in a ledger
                      (occurrences.go, `schedule_runs`, GET /api/schedules/runs?id=):
                      written down as `sending` before it is sent and settled
                      in one transaction with the schedule's advance, so a
                      crash between the two never sends it twice - a row a
                      crash left `sending` is settled from the task records
                      (Submitter's optional RanChecker: an execution with this
                      schedule as its origin, after a 2-minute grace for
                      ReconcileDeliveries), sent now when they say it never
                      arrived, and marked `unconfirmed` (not sent again, the
                      reason kept as LastError) when nothing can say. A run that
                      did not reach its desktop is `retrying` after 5, 15 and
                      30 minutes, still carrying its own TIME, unless the
                      schedule's next occurrence would come first; then it is
                      `failed` and the schedule moves on with the reason. A
                      busy desktop still holds a run (`waiting`) without
                      spending a try. Occurrences that came due while nothing
                      could send them are one `missed` row and a note beside
                      the schedule ("N more run(s) came due ..."): one catch-up
                      run, never a backlog, never silent. Pausing gives up a
                      pending retry. Techniques from hermes-agent's cron
                      (occurrence slots, missed runs, retry only when nothing
                      started). A RunOnce schedule
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
                      needs it awake (Keeper.Wanted: a watched mailbox, an
                      email-triggered job waiting to be sent, a waiting schedule
                      or DAG cron, a desktop working on a task) and releases it the moment
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
internal/httpapi   — the control plane's HTTP handlers and route registration (RegisterRoutes,
                      RegisterLocalRoutes, RegisterSimulationRoutes, RegisterEvalRoutes,
                      RegisterAccessRoutes, RegisterAccessLocalRoutes) and the middleware both
                      listeners are wrapped in (guard.go, proxied.go, bodylimit.go); composes
                      every store above. Other packages use net/http too (lanaccess's gate,
                      cftunnel's API client, wsconn, the skillhub/knowledge/handoff/workqueue
                      handlers httpapi mounts). schedules.go: the
                      requests page's recurring-request CRUD (/api/schedules,
                      .../create, .../delete, .../toggle) and Server.Submit, which
                      implements schedule.Submitter by calling the same submitLocked
                      every human-typed request already goes through - a schedule's
                      fired request is a fourth door onto Plan/deliver/Commit, after
                      the chat, the requests page and a query reply. lanaccess.go: the routes
                      RegisterLocalRoutes registers only on the primary listener -
                      the current LAN/tunnel access code, the button that rotates
                      it, and the paired devices and their revoke - never served
                      back over the listener that code itself gates. The Claude
                      login API (/api/claude-auth/*) and desktop's claudelogin.go
                      were deleted on 2026-11-07: no page called them. The claude
                      harness and internal/claudeauth went the same day; the
                      claude_session table keeps its migration steps
                      (backup.go's MigrateLegacyClaudeSession, whose appended
                      step empties it) and the backup still scrubs it.
                      hostgui.go: the host-gui API
                      (/api/host-gui/start|ws|stop and the /host-gui/view page; the ws route uses
                      internal/wsconn, hand-written RFC 6455 - no library, to keep the binary static):
                      a copied Windows app's launcher in a VM opens that page in the VM's own
                      browser, which shows the app and sends the person's mouse and keyboard
                      back - see "Windows apps run on the host and show in the VM" below.
                      recordings.go (incl. the import of a recording made elsewhere): the recordings page and the
                      /api/desktops/recordings* + /api/desktops/demonstration proxy, replay
                      as a task (Busy check, Plan/Commit, parameters never recorded), and the
                      ticket API (/api/desktops/record opens one, /api/desktops/ticket* drives
                      it - the GUI screen's own toolbar). screen.go puts that toolbar (ticketToolbar)
                      on the control plane's screen page; proxy.go's rewriteDesktopHTML splices it,
                      and the resource-alert popup, into noVNC's own page.
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
                      dashboard's Chat button — one chat for every desktop, and
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
  ├─ base startup run with --skip-novnc: Xvnc on 5901 and no websockify (agent/decurix-entrypoint.sh;
  │    DECURIX_DESKTOP_NOVNC=1 at creation starts it on 6901 as before)
  ├─ XDG autostart (agent/decurix-agent.desktop) → agent/start_agent_terminal.sh
  │    → /opt/vm_server.py (detached background process, no visible terminal); it
  │    also reinstalls agent/chromium-browser.init as ~/.chromium-browser.init (the
  │    browser's launch flags) and clears core dumps Chromium left behind
  └─ decurix_agent/ (Python package, imported by vm_server.py; :8081 inside the container)
        ├─ config.py       — env vars (AGENT_HARNESS kept as CREATED_HARNESS), /home/headless/.decurix/
        │                     {command,querytext,final,memory} dirs and /home/headless/work
        ├─ state.py        — shared mutable state (current_task, waiting_for_query, ...) as
        │                     plain module attributes; every other module does
        │                     `from decurix_agent import state` and reads/writes
        │                     state.current_task etc. (module singleton = shared globals)
        ├─ identity.py     — get_identity_context(engine): who this desktop is (name, ID,
        │                     user, work dir), as facts for the standing context - without
        │                     it "what's your desktop name?" was
        │                     unanswerable and came back as a query
        ├─ memory_context.py — get_memory_context: reads ~/.decurix/memory/*.txt
        ├─ messaging.py    — save_request (intake), send_query/ask_for_query/write_final_reply
        │                     and backend_failure_note (turns a raw engine error into a note
        │                     that says what to do about it)
        │                     (POSTs to control plane /api/querytext, with kind+reply so the
        │                     chat needn't re-parse the note)
        ├─ readiness.py    — wait_for_http_ok(url, label): the one "is it up yet" probe,
        │                     what the engine's wait_for_backend uses. Never
        │                     raises and never gives up - "not up yet" is a state to sit in
        ├─ task_context.py — the control plane's task ID for the request being worked on:
        │                     carried in the command file's name (<stem>.t<16 hex>.txt), set
        │                     by watchers.py, and written to /tmp/decurix-current-task so the
        │                     `gui` command (another process) reports it too
        ├─ usage.py        — per-task model/tokens/cost totals the engine adds to, sent by
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
        │                     makes the chat's step-by-step view the same for every task kind
        ├─ harness.py      — the ONLY place that knows the engine (TheBot since the ollama,
        │                     claude and azure harnesses were removed, 2026-11-07; a desktop
        │                     created with one runs TheBot and logs so); re-exports
        │                     perform_task/continue_after_query from thebot_harness, plus
        │                     wait_for_backend() which blocks until the engine is reachable
        │                     and then sets state.backend_ready - the flag watchers.py gates on
        ├─ ollama_client.py — the local model: kept resident (ensure_model_resident) and
        │                     asked one-off questions (ask_once, harness.ask_model's fallback)
        ├─ llm_client.py   — the control plane's hosted model (/api/llm/chat): answers
        │                     harness.ask_model and the agent's own judging calls when set up
        ├─ commands.py     — run_command and dangerous_reason, the shell check long_task and
        │                     workflow_task hold commands with (gated_command: the gate's yes)
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
        │                   — the engine: TheBot's `soul` binary (agent/thebot/,
        │                     its own Go module, built into all three images and started as a
        │                     daemon by start_agent_terminal.sh) is a persistent HTTP server
        │                     with its own atomizer/scheduler/skills loop. This submits the
        │                     task, the standing context (identity + worker memory + approval
        │                     rules) and the work dir as separate fields to its /api/chat,
        │                     follows the job over its SSE stream (forwarding each log line to
        │                     progress.emit; polling is the fallback), and maps the job's
        │                     structured `outcome` to write_final_reply/ask_for_query. A
        │                     reply goes back with reply=true, which resumes the paused job
        ├─ computer/       — computer use: the ONE action layer the engine and the replays use to see
        │                     and work the screen (see "Computer use" below). core.py has
        │                     observe/act/verify/open (selecting.py, sheets.py and changes.py
        │                     are parts of it split out, O-10); screen.py (python3-xlib: framebuffer ->
        │                     PNG, window list), atspi.py (GTK/XFCE/LibreOffice widgets),
        │                     cdp.py (Chromium over DevTools, stdlib websocket), xinput.py
        │                     (xdotool), elements.py (one element shape, stable refs, the text
        │                     rendering), approval.py (the gate + pending/grant file store),
        │                     audit.py (every action into recordings + progress.emit_action),
        │                     cli.py (the `gui` shell command, agent/gui -> /usr/local/bin/gui), and the
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
        │                     gui skill / gui do - it refuses
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

Control plane → desktop communication happens over Docker bridge networks, never published ports. Each desktop made since 2026-11-05 is on a network of its own, `decurix-vm-<id>` (`desknet.go`, a /28 of `DECURIX_DESKTOP_SUBNETS`, default `10.213.0.0/16`), which the control plane's container joins with the lowest gateway priority, so its default route and published ports stay on `decurix-network`; no desktop reaches another's 5901, 6901 or 8081. A desktop made before that stays on the shared `decurix-network` until it is recreated, and its card says so. Every lookup of a desktop's address goes through `desktopEndpoint`. Desktop → control plane communication uses `CONTROL_PLANE_URL` (`http://decurix:8080` by container name when the control plane runs from `docker-compose.yaml`, which sets `DECURIX_CONTROL_PLANE_URL`; `http://host.docker.internal:8080` for the released binary on the host, see `desktop.controlPlaneURL`) — always the primary listener below, never the gated one, since an agent's own callbacks must never depend on a human having typed an access code anywhere. The desktop's model server is `CONTROL_PLANE_URL/api/native-ollama` (an allow-listed proxy: no pull, delete or create, no unload or reload of the shared model). Details: [gate-boundary.md](gate-boundary.md), "Between desktops".

**Two listeners, one process.** `cmd/decurix/main.go` builds two `*http.ServeMux`s from the same `Server`. Both get `RegisterRoutes` and `RegisterAccessRoutes`. Only the primary one also gets `RegisterLocalRoutes` (the access code and its devices, the host bridge and host-GUI views, host files, backup and restore, the Cloudflare and hosted-model settings, the model proxies `/api/llm/` and `/api/native-ollama/`, the grounder, handoffs, the work queue, readiness, run records), `RegisterSimulationRoutes`, `RegisterEvalRoutes` and `RegisterAccessLocalRoutes`. The primary handler is `practice.RefuseSubnets(RefuseForwarded(GuardPrimary(BodyLimit(GzipMiddleware(mux)))))`; the gated one is `practice.RefuseSubnets(GuardGated(BodyLimit(lanAccess.Middleware(GzipMiddleware(MarkGated(lanMux))))))`. The primary listener (`:8080`) has no login, and what it serves is host-level power: it creates desktops (Docker socket) and `/api/host-bridge/*` runs commands on the host. So it is **never exposed beyond this machine and its own desktops**, by four independent layers: (1) `docker-compose.yaml` publishes it on `${DECURIX_ADMIN_BIND:-127.0.0.1}` only, so the LAN cannot reach it; desktops call back over `decurix-network` instead of through the published port (`DECURIX_CONTROL_PLANE_URL=http://decurix:8080`); (2) no script tunnels it - `scripts/*/build.sh`/`rebuild.sh` open at most one Cloudflare quick tunnel, to the gated listener (`lan_tunnel_url`), only when `DECURIX_PUBLIC_LINK=1`, and only to a port that is not 8080 and is seen serving the access-code page (`gated_port_ok`); the Electron app does the same through `desktop/electron/gated-port.js`; (3) `httpapi.RefuseForwarded` wraps it and answers 403 to any request carrying `Cf-Connecting-Ip`/`Cf-Ray`/`X-Forwarded-For`/`X-Forwarded-Host`/`Forwarded`/`X-Real-Ip`, so a stale script or a hand-run `cloudflared` pointed at `127.0.0.1:8080` gets nothing (a safety net, not the boundary - headers are a deny-list); (4) `RegisterLocalRoutes` puts everything that runs or shows something on the host (`/api/host-bridge/{exec,open}`, `/api/host-gui/*`, `/host-gui/view`) and the access code itself on the primary mux only, so the gated listener - reachable by anyone holding the code - has no route to host code execution (`TestHostRoutesAreOnlyOnThePrimaryListener` pins the split; a test that needs those routes must call `RegisterLocalRoutes` too). The second listener (`:8090` by default, `DECURIX_LAN_PORT`) is wrapped in `internal/lanaccess.Store.Middleware` and gives out a rotating access code (shown in the dashboard's Mobile menu) before granting a session cookie - published on all interfaces by compose (`DECURIX_LAN_BIND`; the Windows app publishes it on the WSL distro's 127.0.0.1, since its LAN link and tunnel come in through Windows' own loopback), reachable on the local network as `lan_url` and off it through its own tunnel; the comment on `lanaccess.Store.recordFailure` says what changes once real tunnel traffic reaches it (every tunnel visitor shares one lockout bucket: 5 wrong codes in 5 minutes lock it for 5 minutes). **Consequence:** a desktop created before this change has `CONTROL_PLANE_URL=http://host.docker.internal:8080` baked in, and on a native-Linux host that address (the docker0 gateway) no longer reaches a port published on loopback - recreate such desktops. **The host bridge only runs what a launcher was signed for.** Anything running in a desktop (an agent that browses the web and runs shell commands) can reach the primary listener over `decurix-network`, so `/api/host-bridge/exec` requires `desktop` + `token`, where `token` is `winhost.ExecToken` (HMAC over the desktop ID, program and the JSON-encoded arguments, keyed by the same `hostgui.key` as launch tokens; `ValidExecToken` in `hostBridgeExecHandler`). A launcher gets its token from `hostFallback` when the app is copied; the arguments must match exactly, so a launcher's own `"$@"` is no longer passed on. `host-run` has two modes only - `open <token> <https link>` (a web app's launcher, `winhost.OpenToken`) and `signed <token> <cmd> args` - and the generic `host-run <anything>`, `win-run`/`mac-run` and the reverse-SSH fallbacks (an unsigned path around the check) are gone. `/api/host-bridge/open` opens only the link a launcher was signed for, from that desktop, and only a `safeOpenURL` link (plain https to a public name on port 443: no address in any spelling, no localhost/`.internal`/`.local`/single-label name, no other port, no user name - a GET from the person's browser to `127.0.0.1:8080` arrives as the person). There is no clipboard route: a credential the worker holds could not stop a prompt-injected worker from putting a command there for the person to paste. A desktop can still ask for exactly the commands and links its launchers carry; it cannot run or open anything else. Launchers written before this stop working (403, and `host-run` says to copy the app again: `POST /api/desktops/host-apps/copy`). **Still true:** the control plane holds the Docker socket, which is root on the host - it is what creates desktops and, on a WSL host, runs the privileged `nsenter` hop behind a signed Windows command. Removing it means a narrower Docker access path (a rootless/remote daemon or a proxy that filters `--privileged`, host mounts and `pid=host`), not a network fix. `desktop.Manager.StaleControlPlane` logs, once at startup, the desktops still carrying the old callback URL. The released binary binds the primary listener to loopback, and on Linux also to the Docker bridge gateways once Docker answers (`primaryListenAddrs`, `Manager.GatewayIPs`: `decurix-network`'s and `bridge`'s IPv4 gateway - what `host.docker.internal` resolves to on Linux), so desktops still call back through `host.docker.internal` and the LAN cannot reach it (checked live: `127.0.0.1` and a container answer, the LAN address refuses). On macOS and Windows only loopback is bound: Docker runs in a VM there and `host.docker.internal` reaches host loopback. Inside a container it binds every interface, since compose decides what is published. `DECURIX_ADMIN_ADDR` (comma-separated `host:port`) overrides it. A gateway address that cannot be bound is logged as a warning.

**Who may call what, since the review of 2026-11-02** (`internal/httpapi/guard.go`). The review created a desktop and queued a task from a page on another site, so both listeners now sit behind a guard, and each check is code, not a convention:

- **No cross-site writes.** `GuardPrimary` and `GuardGated` refuse any method but GET/HEAD/OPTIONS, and any WebSocket, unless it is same-origin (`Origin` equals the `Host`, or the tunnel's `X-Forwarded-Host` on the gated listener) or carries no `Origin` at all (a script, the Electron main process, an agent's urllib). `Sec-Fetch-Site: cross-site`/`same-site` and `Origin: null` are refused.
- **No DNS rebinding.** The primary listener serves only Host names it is known by: an IP address, `localhost`/`*.localhost`, `host.docker.internal`, a single-label name (`decurix`, a container name), or one listed in `DECURIX_ALLOWED_HOSTS` (comma-separated). Anything else gets 421.
- **On the primary listener only the host is a person** (third round, 2026-11-03; `desktop.NetView`, `Manager.Networks`, `httpapi.origin`). A person is loopback or the gateway the host comes in through: decurix-network's, from Docker (`dockerNetworks`), since the ports compose publishes are on decurix-network and Docker's proxy hands them over from its gateway. Not the default bridge's, not a desktop network's, never the practice network's. Only while Docker has named none does the control plane's own network's default-route gateway count (`/proc/net/route`, no Docker needed). Every bridge network's range is a container range, and an address in no known range is unknown. On the primary listener, anything that is not the host is held to the desktops' callbacks below and must be a desktop's own address there: verified live in the second round, a container on a network someone else made read the access code and the backup, and created a DAG and a desktop. The gated listener has a login of its own and refuses only a known container address, so a phone or a LAN laptop reaches the login even while Docker answers nothing. What the interfaces say (the control plane's own network in compose; the Docker bridges docker0/br-* on a Linux host) keeps a desktop refused there too. The network view and the address-to-desktop map are each fetched by one call at a time and shared (`desktop.refreshed`). A known address is answered without waiting for Docker. An unknown one makes Docker be asked again at most every 2 s, and the address map is dropped whenever a desktop is made or removed here.
- **A desktop gets its callbacks and nothing else.** A request from anyone but the host (`fromContainer`) may use only the routes in `containerRoutes` (the model proxy, grounder, agent events, query notes, its own memory, its own past runs searched (read only), outbox drafts, org knowledge and skills read-only, handoffs, host-file checks, the signed host bridge and host-GUI views). Everything else is 403: sending requests (a worker answering its own approval), creating desktops, the LAN access code, verdicts, DAG/cron jobs, the tunnel link, Claude login, settings. Adding a callback means adding it to that list on purpose; `TestContainerRoutesAreServed` checks each entry is a real route.
- **A desktop is who its address says.** `Manager.DesktopAt` maps a container's address on its desktop network (its own, or `decurix-network` for an older desktop; `desktopEndpoint`) to its desktop ID, and the guard puts it in the request (`requestDesktop`). `actingAs` makes every callback act as that desktop and refuses a body naming another one (memory, agent events, query text, outbox, host files, the host bridge's token desktop); the handoff routes do the same through `handoff.Handler.Caller`. An address in the network that is no desktop's gets nothing (403). When Docker cannot be asked (`Manager.CallerAt` returns `ErrCallersUnread`), a caller the last map names is still served only while that map is younger than `callerTTL` and of the current desktop list; any other caller gets 503 "try again" with a `Retry-After`, never a 403 a worker would take as final, and is never let through.
- **The gated listener serves no container at all.** It refuses every address inside a known Docker network. A desktop that hairpins through Docker's proxy on a port published on every address arrives from the gateway, indistinguishable from the host, and the Windows app publishes the port on loopback only for that reason. A checkout's compose still publishes it everywhere for the LAN; there the hairpin shares the gateway's lockout bucket.
- **What a desktop serves through the control plane is sandboxed.** `/desktop/<id>/...` (noVNC's own page) and `/agent/<id>/...` (TheBot's page) come from inside a container but would run as the control plane's origin; they now carry `Content-Security-Policy: sandbox ...`, so their scripts have an opaque origin the guard refuses. The screen a person uses is the control plane's own page with its own noVNC (`screen.go`) and is unaffected.
- **A reply answers its own card.** `/api/desktops/send` takes `note_id`; when the desktop has asked something newer, or the note is closed, it answers 409 (`replyStillAnswers`), so a "yes" under an old card cannot approve a newer action.
- **Per-desktop secrets.** A new desktop gets `DECURIX_AGENT_TOKEN` (HMAC of its ID under `hostgui.key`, `Manager.AgentToken`), which the control plane sends as `Authorization: Bearer` on every call to its agent (:8081, and soul :4900 through `/agent/<id>/`), and a VNC password of its own (`screenPassword`, 8 characters, VNC's limit) instead of the shared "admin". Both protect a desktop from other desktops and from web pages. The token is also kept from the worker's own programs since 2026-11-08: it is in no environment of the worker's user, and its two holders (the agent and soul) are non-dumpable ([gate-boundary.md](gate-boundary.md), "The agent token's two holders"); the screen password is the screen user's in broker mode. **The desktop's side:** `http_api.refusal` checks the token (constant-time) on every :8081 route but `GET /health`, and, with or without a token, refuses a request carrying `Origin` or `Sec-Fetch-*` (a web page in the desktop's Chrome), a Host that is a DNS name (rebinding; the control plane calls by IP or container name) and a write with a non-JSON body; a desktop without a token is otherwise served as before until it is recreated. In-container callers send it through `config.agent_auth_headers()` (the TheBot bridge's calls to soul). A reply is a reply only when its `task_id` is the waiting task's (`messaging.save_request`, docs/chat.md). The browser extension and its backend (`agent/browser-agent`, `ext_server.py`) were deleted on 2026-11-03; the image no longer installs them. Soul's browser skill was removed too (4d24556).
- **Nothing of the person's computer is copied into a desktop at creation.** `host_paths` is refused (it reached `/data`: the keys and the database); the host-app list packs launchers only, never a browser profile, SSH keys, tokens, keyrings or dotfiles; a browser is not copied either: the VM gets one window of the person's own running browser, out of their sight ("In place" under Host apps). The shared folder is the one way to give a worker files.

- **Fail closed** (second round, same day). A desktop's agent (:8081) and soul (:4900) serve nothing but their health check without a token, so a desktop created before tokens must be recreated. No desktop is created when the control plane could not load its secret (`hostgui.key`): it would get no token. That includes a practice desktop (`CreatePractice` answers `ErrNoSecret`, and the run's note says why). `DEFAULT_PASSWORD` is no longer read; every desktop gets its own screen password. `fromContainer` takes an address that is not loopback, not a gateway and in no known range as a reason to ask Docker again in the background (at most every 2 s). On the primary listener that address is never a person (third round). The practice network is one of the container ranges and never one of the addresses the control plane listens on (`GatewayIPs`); its subnet cache asks again every second while it knows no range.
- **Nothing a desktop serves runs as the dashboard.** `relayAgent` (recordings, skills) gets the same sandbox policy and `nosniff` as the proxies, and every response of both listeners says `frame-ancestors 'self'` / `X-Frame-Options: SAMEORIGIN`, so another site cannot frame an Approve button. The legacy `/desktop/<id>/vnc.html` and TheBot's own page at `/agent/<id>/` no longer work inside the sandbox; the dashboard uses neither.
- **`/api/host-bridge/open`** hands the link to Windows' URL handler (`rundll32 url.dll,FileProtocolHandler`) as one argument (`hostOpenCommand`): no `cmd.exe` parses it, so `&` and `%` in a link stay text. Signed host commands are shell-quoted too (Go's `%q` is not shell quoting). Whether a host command is a Windows program (the hop to Windows from a control plane in a container) is read from its first word (`isWindowsProgram`): reading the base name of the whole line broke every open in the Windows app.
- **Host files are decided on as they resolve** (`resolveHostFile`). Each part of the path must be a name its folder lists, which excludes an 8.3 alias, a trailing dot or space, and a different case (the listed name is used). Every symlink and junction is followed and checked again from the drive's root, and anything under AppData (or its old junction names), ProgramData, Windows or a dot-folder, or that cannot be resolved, is refused. `/api/host-files/read` serves only documents (`readableHostDocument`).
- **Deleting a desktop while it is being created** stops the creation and waits for it to clean up before anything is removed; its name stays claimed until then, and a creation only ever removes its own entry, so a re-created desktop of the same name is never touched.

- **The gated listener's surface is smaller** (review 2026-11-03). It no longer serves `/api/host-apps/detect`, `/api/desktops/host-apps/copy` or `POST /api/tunnel`; `/api/claude-auth/*` is deleted everywhere (2026-11-07). `/api/host-paths/validate` and `/api/channels/inbound` are deleted. A desktop create there refuses host apps, as it refuses `shared_folder`. Org skill and knowledge writes are host-operator only (O-7). `gated_surface_test.go` names each of these.
- **Every request body has an outer cap** (`httpapi.BodyLimit`), on both listeners and the gated sign-in form included: 1 MiB by default, and `bodyLimits` names the routes built for more.
- **Every desktop-callable route says whose data it serves.** `desktop_reads_test.go` fails on a route in `containerRoutes` that does not bind its reads to the calling desktop.
- **No handler sends `err.Error()` to a client.** `internalError`, `badForm`, `badJSON` and the named `refusal`, `providerSaid` and `viewerReason` helpers (`json.go`) are the only ways to answer with an error. `TestNoResponseEchoesAnError` (`noecho_test.go`) walks every httpapi file. It is a net, not a proof: it catches error text kept in a variable, `fmt.Sprint*`/`Errorf` of an error-named variable, writes to any `http.ResponseWriter` parameter and `json.NewEncoder(w).Encode`. A package sentinel's `.Error()` is exempt only when its qualifier is one of the file's imports. DAG store refusals are `dag.InputError`; mail-trigger and schedule validation errors are `InputError`, shown in their own words.
- **Every LAN forward and tunnel the Electron app makes goes through `desktop/electron/gated-port.js` first**: never the primary port, and `127.0.0.1:<port>` must be seen serving the access-code page. The Windows forward's firewall rule is the Private profile, LocalSubnet, on the interface carrying the LAN address only. A network Windows calls Public gets no LAN link.
- **The public link is off by default** (owner decision O-5). The Electron app starts the Cloudflare quick tunnel only after the person turns on "Public link" in the dashboard's Mobile menu (an Electron-only switch, injected by `desktop/electron/preload.js`; kept in `public-link.json` in the data folder). The scripts start it only with `DECURIX_PUBLIC_LINK=1`. The LAN link is unchanged.

Desktops created before these changes have no agent token and the old screen password: they must be recreated.

**What is still not a boundary.** The approval gate now runs as a Linux user of its own (`decurixgate`), and a yes is made only from a reply the control plane signed ([gate-boundary.md](gate-boundary.md)). The worker's shell still runs as the same user as the agent: it can read the agent token and its own screen password, report its own task's outcome ("The gap" in gate-boundary.md), and give input around the gate (owner decision O-1, the input broker, is open). Everything above keeps out other desktops and web pages, not the worker itself.

The scripts also discover this machine's own LAN-facing IP (`ip route get 1.1.1.1`'s `src` field on Linux, `scripts/linux/lib/lan-address.sh`, which under WSL2 forwards Windows' own LAN address on the gated port instead, unless `DECURIX_WSL_LAN_FORWARD=0`; a UDP connect to a public IP on macOS and Windows, the same trick — no packet is sent) and `POST /api/tunnel` `lan_url` (the gated port) built from it — stored on the same `tunnel.Store` as the gated tunnel's `lan_tunnel_url` and shown together in the Mobile menu (tunnel link first since it works off-network), so a phone on the same network can skip the tunnel round-trip entirely. They no longer report `admin_url` (the primary listener is not reachable from the LAN) or `url` (nothing tunnels it); `tunnelHandler` and the store still accept both, only so an older script's report does not error, and the Mobile menu shows them under a clearly-labelled unauthenticated heading if one arrives. `httpapi.tunnelHandler`'s POST only ever sets the fields present in that call's body, since the LAN report (once, right after startup) and each tunnel-URL report (once per line the `cloudflared` process prints) are separate calls that must never blank out each other's fields.

Go package dependency direction (no cycles; checked against the imports on 2026-11-07): `httpapi` → nearly every internal package (the stores, `desktop`, `ollama`, `llm`, `llmrec`, `runs`, `practice`, `eval`, `concierge`, `dag`, `idlepause`, `resourcemon`, `keepawake`, `lanaccess`, `devices`, `tunnel`, `cftunnel`, `phonehost`, `wsconn`, `winhost`, `withhold`, `webui`, and the workforce packages `skillhub`, `knowledge`, `handoff`, `access`, `workqueue`: docs/skillhub.md, docs/handoff.md, docs/access.md, docs/workqueue.md); `webui` → `dag`, `desktop`, `memory`, `querynotes`, `tasks`, `web`; `desktop` → `ollama` (`DesktopURL`/`DesktopModel`, the model a new desktop is given), `winhost` (`Token`, `ExecToken`, `OpenToken`, `Target`, `Manager`), `sqlitedb` (`DataDir`, `InstanceID`), `withhold` (the screen passwords it holds), `qualification` (readiness case files), `decurix` (the root package, for the embedded build context); `idlepause` → `desktop` (the `desktop.Desktop` struct and the `DesktopLister` interface `*desktop.Manager` satisfies, so it can be tested with a fake instead of a real Docker client), `tasks` (`gone.go`: `Worker`, `CloseOpen`); `resourcemon` → `desktop` (through its own `DesktopManager` interface); `schedule` → `sqlitedb`, `tasks` (the `Origin` constants only) - its `Runner` reaches `httpapi` only through the `Submitter` interface `*httpapi.Server` satisfies (`Submit`), the same narrowing `idlepause` uses for `desktop`; `suggest` → `desktop` (the `desktop.Desktop` struct), `memory`, `sqlitedb`, with the embedder (`ollama.Embed`) and the tasks store handed in by `main` through interfaces; `stepimages`, `devices`, `outbox`, `skillhub`, `knowledge`, `access` → `sqlitedb` only; `querynotes`, `memory`, `conversation`, `tasks` → `sqlitedb`, `withhold`; `workqueue`, `handoff` → `sqlitedb`, `tasks` (`workqueue` reads the tasks component's `executions` table for spend; `handoff` uses status constants only) - each reaches desktops, task submission and worker lookup only through interfaces `httpapi` backs (`internal/httpapi/handoffs.go`); `mailtrigger` → `sqlitedb`, `sealed`, `schedule`, `tasks`; `cftunnel`, `llm` → `sqlitedb`, `sealed`; `purge` → the stores it rewrites and `sqlitedb`; `codemap` → nothing internal; `practice` → `desktop`, `llm`, `llmrec`, `qualification`, `sqlitedb`, `tasks`, `withhold` (Simulate: docs/simulation.md); `phonehost` → `wsconn`; `phoneagent` → `wsconn`, `phonehost`; `ollama`, `sqlitedb`, `sealed`, `blobstore`, `wsconn`, `tunnel`, `lanaccess`, `winhost`, `keepawake`, `web`, `decurix` depend on nothing internal. `BeginProvisioning` takes no harness: every new desktop runs TheBot (`HarnessTheBot`).

One desktop image is built, `docker/desktop.basic.Dockerfile` (`decurix-desktop:basic`), plus the shared tools image `docker/tools.Dockerfile` (`internal/desktop/images.go`). `BuildImages` also tags the basic image as `decurix-desktop:developer` and `decurix-desktop:ai`, so a desktop created with one of those older profile names still starts on the same image. The separate developer and ai Dockerfiles were never built and have been removed.

## The app (`desktop/electron`)

The Windows, Mac and Linux app is an Electron shell around the same stack: the control plane in Docker (`stack/Dockerfile`, the runtime stage of `docker/Dockerfile` with the binary built by `scripts/*/build_app.*`) and the dashboard at `http://127.0.0.1:8080` in its window.
- `main.js`: start-up (`start`, `startStack`, `waitForDashboard`, `watchBackend`), the keep-awake hold, the LAN address report and the public link. On Windows it also picks the runtime (`selectWindowsRuntime`: the app's own WSL distro "Decurix", or, when the person agrees, an existing distro's Docker Engine), installs WSL when Windows lacks it, imports the distro and prepares it (`wsl/setup.sh`, `wsl/dockerd.sh`).
- The stack's environment on Windows: `main.js` runs compose with `DECURIX_HOST_KIND=wsl` and `DECURIX_LAN_BIND=127.0.0.1`; `windows-runtime.js` writes the model settings for the native model server (`OLLAMA_URL`, `DECURIX_WINDOWS_OLLAMA_URL`, `DECURIX_WINDOWS_SELECTOR_URL`). `windows-runtime.json` in the data folder records what a shared engine already held (`preexisting`).
- `local-models.js` and `wsl/native-ollama.ps1`: the model server on Windows itself, outside WSL's memory limit (port 11435), with `decurix-model-selector.exe` beside it.
- `gated-port.js`: the one check before any LAN forward or tunnel (never 8080, and the port must serve the access-code page). `windows-lan.js`: the Windows LAN link (netsh portproxy to 127.0.0.1 and a Private-profile firewall rule). `mobile-access.js`: the public link's tunnel (cloudflared by default; `DECURIX_TUNNEL_PROVIDER=localhost-run` or `auto` uses or falls back to localhost.run over ssh) and its health probe.
- `preload.js`: the start screen's status and the dashboard's Electron-only "Public link" switch (`public-link.json`).
- `build-watchdog.js`: a build that prints nothing for a while is cancelled and tried once more.
- `wsl/`: `setup.sh`, `dockerd.sh` (the daemon and the fence between desktop networks), `reset.sh`, `apk-safe.sh`, and for a shared engine `stage-existing.sh`, `maintain-existing.sh`, `cleanup-existing.ps1`.

Tests: `npm test` in `desktop/electron` (`node --test`, the files named in `package.json`).

## The shared folder: one folder of the person's computer, opt-in

A worker cannot see files that live only on the person's computer, so it had to push every value through a host app's viewer a key at a time (slow, and cut off at the operator's command limit with rows missing). A desktop can be made with **one shared folder**: `shared_folder` on `POST /api/desktops/create` (the create form's "Shared folder" field), e.g. `C:\Users\Administrator\Documents\Decurix demo`. The worker computes with its own tools in it and the person's own apps open the same files.

**What the worker can and cannot touch.** Only that folder, read-write. Nothing else of the computer is mounted, no HTTP route is added, and a desktop's chat, skills and memory are as before. It can read, change, create and delete files in it (including links: a link it makes there is just a link object on the computer, resolved inside the container where it leads nowhere on the PC). It cannot reach a parent folder: the bind's root is the container's own view, and `shared.contained` checks a path's real location before the agent reads it. Secrets stay withheld (`privacy.py` is unaffected). It is fixed at creation, like the rest of the desktop's state in `$HOME`: to share another folder, make a new worker.

**How.** `desktop.ParseSharedFolder` validates what the person typed and refuses, in a sentence: a relative path, a network path (`\\host...`), `..`, another drive than `C:`, a drive root, a system folder (Windows, Program Files, ProgramData, a Linux/macOS `/etc` `/usr` `/var`...), the Users folder, a whole home folder, anything under AppData/`.ssh`/`.aws`/`.config`..., a file, a folder that does not exist, a link (a junction could point at a system folder), and control characters. A Windows folder is checked where the control plane sees the `C:` drive (`DECURIX_HOST_MNT_C`, `/mnt/c` in the container, read-only) and bound from where the Docker daemon sees it (`DECURIX_HOST_MNT_C_SOURCE`, default `/mnt/c`, for a daemon that mounts the drive elsewhere); a Linux or macOS folder is bound as it is. The bind is a `HostConfig.Mounts` entry, not a `Binds` string: the daemon refuses a source that does not exist, where a bind would make it on the host. It is mounted at `/mnt/pc`, **outside `$HOME`**: the base image's startup runs `sudo chown -R $UID /dockerstartup $HOME` on every start, which would walk into the person's own files. The agent links `~/PC` to it when it starts (`shared.ensure_link`; a recursive `chown` does not follow a link - checked). The container gets `DECURIX_SHARED_FOLDER` (the folder as written) and `DECURIX_SHARED_MOUNT`, and the label `desktop-shared-folder`, which `Desktop.SharedFolder` reads for the card ("Shares C:...").

**Also refused** (review 2026-11-03): the control plane's own data folder, a folder that holds it, or one inside it (compared after resolving); `Library`, `.local`, `.cache`, `.pki` and `snap` under a home; and any path with an 8.3 short name (`~1`), since its real name was never checked.

**Two listeners.** `shared_folder` is honoured only on the primary listener: `httpapi.MarkGated` tags every request that reaches the gated listener and the create handler answers 403 to a `shared_folder` there, as it must for anything that reaches the host.

**Path mapping (agent, `shared.py`).** A path the person writes under their folder - any case, `/` or `\`, or `/mnt/c/...` - is `/home/headless/PC/...` here (`shared.to_local`). `office` maps it itself (`_path`); the final check asks the disk instead of the host route for a file saved there (`produced._ask_host`, `copy_from_host`: the local file is read where it is) and counts what the task made there as written (`produced.files`); the judge reads the inputs the task was given there (`verify._shared_inputs`); soul traces a typed `/home/headless/PC/...` path (`workFolders`) and tells the operator in one generic line (`sharedRule`, only when the desktop has one). A request that names an app to work in is still done with that app: `behindTheApp` lets the content be made with tools in the shared folder (the app sees the same file), and `sharedAppUntouchedNudge` sends a DONE back once, and then asks the person, if no command ever opened the app.

**Host hops.** When the control plane runs in a container, everything it runs on the host goes through `desktop.HopCommand`: a privileged alpine container in the host's PID namespace, named `decurix-hop-<purpose>-<random>` and labelled `decurix.hop=1` and `decurix.hop.instance=<instance tag>`, removed with `docker rm -f` when its context is cancelled or its session ends (`winhost.endHost`), and swept at start (`SweepHopsAtStart`). The start sweep removes only this instance's hops; untagged or unnamed older hops are reported, not removed. Never start one with a bare `docker run`.

**The instance tag.** Each control plane has a random tag, kept in the `instance-id` file in its data folder (`sqlitedb.InstanceID`). The file is made with an exclusive create; an empty one older than 5 s is a crash's leftover and is made again. The tag was a hash of the data folder's path before 2026-11-03, but every containerised control plane has `/data`, so they shared one tag. Hops, home-copy helpers (`decurix.homefs.instance`) and host-app helpers carry it.

**In place (a browser given to a VM): one window of the person's own browser.** A browser is not copied and not given a profile of
its own: its cookies and account tokens are encrypted (Chrome 127 and later, format "v20", app-bound) so that only Chrome running from
the profile's own folder can open them. Found 2026-11-12 on Chrome 157: Chrome's log said "Failed to decrypt token" for all three
Google accounts on a copy made from the person's real files and started the ordinary way (and by the host bridge: the same), and said
nothing on the real folder. Every copy of a profile is therefore a signed-out browser, however it is made; the decryption key in `Local
State` is identical in the copy and does not help. The first design ran the browser from the real folder on the helper's private
desktop, out of sight - and took the person's own Chrome from them (found 2026-11-13): Chrome allows one process per profile folder,
on one desktop, so every launch of theirs was handed to the hidden process and opened where they could not see it. So the VM is given
**one window of the person's own running Chrome** (`Native.Borrow`, "A borrowed window" in bridge.ps1; `Test-RunsInPlace` and
`Start-Resolved` decide it), and their Chrome stays theirs to open and use at any time.

How it works: the helper asks the browser for a window the way any second launch does (`chrome --new-window about:blank` with the
shortcut's own `--profile-directory`; started if it is not running - one launch either way, nothing minimised), finds it as the new
window whose page says about:blank (`NewWindow`: every new window is veiled at once and judged by its page; one the person opened in the
same moment is given back), and makes it invisible to the person while it stays on screen to Chrome: layered at an alpha of 1/255,
click-through, a tool window (no taskbar button, not in Alt-Tab), topmost (`Veil`). Measured 2026-11-13, a window of an ordinary Chrome
off the screen stops painting, and one on the screen at alpha 1/255 keeps painting, covered or not - which is why it is made
invisible and never moved away (the one move off the screen is `VeilAsync`'s first, non-blocking step, undone by the work behind
it). The picture is `PrintWindow`, input is posted messages (the person's cursor and keyboard are not taken), the window is marked
with a window property so a helper that is ended without ceremony leaves a window the next helper takes over, and it is closed when
the session ends (given back to the person, still open, if it will not close: a page asking "leave this site?").

Rules, each pinned by `TestABorrowedWindowIsTheOnlyThingTouched`: every other window of the browser is the person's and is never
touched (`Belongs`; `Shutdown` closes and terminates nothing of the browser's); the app's host session is one for every VM
(`packContext.Shared`, `internal/desktop/inplace.go`), so VMs share the one window; it is never started ahead or kept ready; only a
plainly started browser is borrowed (`--app` windows and web-app launchers keep a private profile); nothing of the browser's
accessibility or background mode is changed. Keys: a modifier (Ctrl, Shift, Alt) is carried by joining the browser's input queue for
about 40 ms (`WithModifiersShared`), waiting while the person holds a key themselves, and failing loudly if it cannot - that queue is
also the one their own Chrome windows type into. The browser's own UI thread can be busy for a long time after a cold start with the
person's real profile, and everything done to its window waits for it: that work is queued on its own thread per window and nothing waits
on it unbounded.

Popups and several profiles. A page can open a window of its own (a sign-in popup): another top-level window of the same browser, on
the person's screen, with nothing in it to say whose it is. It is the VM's when it appeared within five seconds of something sent
to the VM's window and the person had pressed no key and no mouse button for the second before (`WatchPopups`; `NoteInput` stamps the
time, before the input is sent): nobody else could have opened it. Such a window is veiled like the first, drawn in the same picture at
its own place and driven with it (`Belongs`: the main window, the popups, and what they own), and closed with the session. When the
person is working at that very moment it cannot be told apart and is left alone - theirs, on their screen, as before. A browser has
one input queue for all its windows, so the focus it reports can be inside a window of the person's: keys are sent only to a window that
`Belongs` (`KeyTarget`). Each VM app is a helper of its own, so each profile (each shortcut's `--profile-directory`) is its own
borrowed window of the same browser; a window's mark is its helper's process id, a window is taken over only when that helper has
gone (`HelperAlive`), and one helper at a time looks for its new window (a named mutex), so two starting together never take each
other's. Not carried yet: the window of a popup is placed where the browser put it, not made to fit the VM's screen.

Which profile. A Chromium browser keeps several profiles in one folder, each signed in to its own accounts, and the VM's window is one
profile's. The app list used to know only shortcut files: "Google Chrome" (no profile: the browser picks one) and the shortcut Chrome puts
on the desktop for one profile - found 2026-11-13, the person worked in a profile that had no shortcut, so the VM was given another
whose Google sign-in had lapsed on their own screen too, and YouTube was signed out there. So each browser's own profile list (its
`Local State`, of the user who used it last) makes one app per profile, named for the profile and its account ("Google Chrome - Work
(pat@example.com)"), run with `--profile-directory`; the browser's plain entry and its profile shortcuts are left out
(`internal/desktop/browserprofiles.go`; a web app's shortcut, `--app-id`, stays). A window of one profile is taken over only by a helper of
the same profile (`ProfileMark`, a hash of the launch's arguments). The window is found by its blank page (the one page that says the
same in every language) and then sent to the browser's new tab page through its address bar (`OpenStartPage`: a `chrome://` address
on the command line is dropped by the browser), so the VM opens on the person's own new tab page.

The keyboard. A browser types only into a window it holds active: keys posted to an inactive window of it go nowhere (measured
2026-11-13, another program in front: nothing arrived). The VM's window is never activated by a click or by Windows
(`WS_EX_NOACTIVATE` - before it, a posted click made the invisible window the person's foreground whenever they were in that browser,
and their keys went into it). For a keyboard command (`Keyboard` around `type`, `newline`, `key`, `copy`) it is lent its keyboard
(`LendKeyboard`): told, and only it told, that it is active (`WM_NCACTIVATE`, `WM_ACTIVATE`; `WM_SETFOCUS` alone is not enough). With
another program in front the foreground stays there and the keys arrive. With a window of the same browser in front, the browser takes
the foreground for real when told - so that is done only once the person has paused (no key for 400 ms); while it lasts every key of the
person's is held by a low-level keyboard hook (real keys only) and ends the lend at once, and the foreground goes back to them and their
keys are played into their window (`EndLend`, from the watcher); otherwise it goes back once the VM has sent nothing for 1.5 s. Not on a
timer after the keys: the browser reads them when it gets to them, and one it reads after it is no longer active is lost (of an address,
"chr" arrived; `WaitForInputIdle` did not wait). The watcher gives the foreground back whenever the VM's window or a popup of it has it
otherwise (a popup the page opened takes it), and after a told activation, and after the borrow, the person's own window of the browser
is made its last active one again (`PersonLastActive`): the browser opens a link from another program in its last active window, and one
opened after the VM's window was activated opened in it, out of sight.

**Host apps.** Every host-app helper carries `-DecurixInstance <instance tag>` on its command line. The start-up sweep, the reaper and the file-release script end only this instance's helpers; untagged helpers from older builds are logged and left alone. So two control planes on one PC no longer end each other's helpers (W-04). An app is a web app only when its command or AppID is an http(s) target; no product is guessed from a name (B-20). Host session ids are `prefix-prefix-<12 hex of sha256(desktop\0slug)>`, and `winhost.Start` never joins a session running another app. A private browser profile with an old import marker (a copy an older build made, which can never be opened) is deleted before launch. Host apps start suspended and run only once they are assigned to the kill-on-close job.

**Requested host apps that are not added.** `MigrateHostApps` never skips one silently: each requested app it does not add is a `desktop.MissingApp` (name, plain reason, `Pending`), kept by the Manager (`missingapps.go`), returned in the create answer, `/api/desktops/status`, `/api/desktops/activity` (the card) and `POST /api/desktops/host-apps/copy`, and written to `~/.decurix/missing-apps.json` for the desktop's agent (`identity.py` lists them; `gui open` of one is refused instead of opening a stand-in). An app the host-app check could not reach (`HostAppItem.Unverified`: the helper died or timed out) is not a verdict: it is never cached, it is asked about again when a desktop wants it, and `retryPendingApps` tries it in the background for about six minutes, copying its launcher into the running desktop when it checks out. `RestoreMissingApps` reads the files back at start.
## The desktop container since the review of 2026-11-03

**Files in and out of a desktop's home.** Every copy into or out of a desktop's home goes through `internal/desktop/homefs.go` (`ReadTar`/`WriteTar`). It runs tar inside the container as 1001:1001 with an empty environment:
- a running desktop: tar is exec'd in it, and the worker's processes keep running;
- a paused one: it is unpaused for the copy and paused again after. Copies into one paused desktop decide one at a time (a per-desktop lock, also held while re-pausing), so a second copy joins the first's unpause. A desktop resumed from outside meanwhile is re-read, copied into, and not paused afterwards;
- a stopped one: a throwaway helper container from the same image, with only the home volume, user 1001, no network, no capabilities, no-new-privileges and a read-only root, removed after.

Docker's `CopyFromContainer`/`CopyToContainer` run as root and resolve parent folders through links the worker made, so they are allowed only in the functions `internal/desktop/rootcopy_test.go` lists, each with its reason (a fixed root-owned path, or a container that has never run). `execTarOut` freezes the worker's processes and is for offline containers only. A file written into a desktop is sent as a tar on stdin with a constant script, never as a heredoc built from text.

**Volumes.** A desktop mounts its home (`desktop_<id>_home`), its own package cache (`desktop_<id>_cache` at `/var/cache/decurix`, where `PIP_CACHE_DIR`, `NPM_CONFIG_CACHE`, `GOMODCACHE` and `HF_HOME` live), the tools (`decurix-tools`, read-only) and the shared model cache (`decurix-models`, read-only, at `/var/cache/decurix-models`, named in `DECURIX_SHARED_MODELS`). No writable volume is shared between desktops (B-02, owner decision O-2). Home and cache volumes carry `decurix=true`, `decurix.desktop=<id>` and `decurix.creation=<id of the creation>`. A creation refuses a home volume another creation made, and reuses only an unlabelled one (made before 2026-11-03). A creation that fails removes only what it made.

**The image.** Desktops run with `no-new-privileges`, and the image strips every setuid and setgid bit, so nothing in the desktop can gain privileges (Chromium uses its namespace sandbox, not the setuid helper). The seccomp profile allows ptrace (upstream's default on kernels 4.8 and later). Xvnc is told `-nolisten tcp` through TigerVNC's mandatory config (`/etc/tigervnc/vncserver-config-mandatory`), so nothing on the Docker network can reach X. The agent terminal no longer runs `xhost +`: X access control stays on, with only the worker's local user allowed. KSM is off unless `DECURIX_ENABLE_KSM=1`, because it shares memory pages across desktops that do not trust each other. All of this is fixed at creation: **recreate desktops** after rebuilding the image. Check a running one with `DECURIX_X11_LIVE_DESKTOP=desktop_<id> go test ./internal/desktop -run XServerHasNoTCPLive -v`.

**What a desktop does not run (2026-11-07, docs/lightweight.md).** No websockify: the control plane bridges the screen's WebSocket to Xvnc on 5901 itself (`internal/httpapi/screenbridge.go`), so `agent/decurix-entrypoint.sh` hands the base image's `startup.sh` `--skip-novnc` (VNC and the startup's wait on it are unchanged; about 22 MB a desktop). A desktop created with `DECURIX_DESKTOP_NOVNC=1` in its environment starts websockify on 6901 as before (the `scripts/*/test-vm` desktops, reached on 6901 directly, set it), and only such a desktop serves noVNC's own `/desktop/<id>/vnc.html`. `DECURIX_VNC_BRIDGE=0` on the control plane still proxies the stream to a desktop's websockify when one answers, and bridges it anyway, saying so in the log, when none does. Creation waits for Xvnc (5901), not 6901 (`Manager.WaitForGUIReady`). Session services a worker's desktop has no use for are switched off by a `Hidden=true` entry of the same name in `~/.config/autostart` (`agent/decurix-autostart-off.desktop`, harmless where the package is absent): xfce4-notifyd (D-Bus starts it on the first notification), xfce4-power-manager, light-locker, xscreensaver, xfce4-screensaver (a lock screen on a desktop whose user has no password locks everyone out) and update-notifier. at-spi (what `gui` reads), the clipboard manager, gnome-keyring (Chrome's cookie key), xdg-user-dirs, Thunar's daemon and tumbler stay. Chrome's Memory Saver is on by policy at its moderate level (`HighEfficiencyModeEnabled`, `MemorySaverModeSavings: 0` in `agent/chrome-policies.json`): a background tab left long enough is discarded and reloads when opened; the tab on screen never is. All of it is image state: new or recreated desktops only.

**Lifecycle.** `Manager.LockDesktop(id)` is the per-desktop lifecycle lock; the idle sweep re-checks Busy under it. `DECURIX_IDLE_ACTION` other than exactly `stop` pauses. Docker's one-shot stats carry no previous CPU sample, so a CPU % needs the previous reading.

**When Docker is down.** The control plane no longer exits when Docker does not answer. Both listeners start, every page shows a "Docker is not running" banner, and the Docker-dependent start-up (`cmd/decurix/dockerstartup.go`) runs once Docker answers (`Manager.WatchDocker`, every 10 s; `Manager.DockerReachable()`).

**The tools volume.** The tools image's populate rule writes its ready marker last, and waits while any desktop is not exited or created (running, paused or restarting all count; a failed listing counts as running). A refresh that waits exits 75; the control plane logs it, `/api/docker/status` reports `tools_waiting`, every page shows a quiet banner, and the refresh is tried again when a desktop is next created. `BuildImages` populates under the same lock as a creation. Starting an existing desktop does not wait for a refresh (a follow-up). The owner rebuilds the tools image to pick this up.

## The run record on the control plane (phase 2)

- `internal/blobstore` keeps content-addressed blobs under the data folder, `blobs/sha256/<2>/<64>`: model-call
  messages, answers, tool lists and images (`internal/llmrec`) and the files of run bundles (`internal/runs`). Nothing
  deletes a blob.
- `internal/llmrec` is the recording proxy's store (`llm_calls`): every hosted call through `/api/llm/...`, with its
  purpose, task, turn and caller (the `X-Decurix-*` headers; the desktop is the connection's), the deployment that
  served it, usage and latency, and its messages withheld and stored once each by hash. Recording never fails a call.
- `tasks` holds the judgements (every human input with its author, append-only), the structured verdict
  (`executions.verdict`) and what each execution ran (origin, skill, resolved model).
- `internal/runs` pulls a run's bundle off its desktop when the run ends and before the desktop is deleted, and builds
  `decurix.trajectory/v1` (docs/trajectory.md).
- `internal/runsearch` is an FTS5 index of the task records' stored text, searched by `/api/runs/search` (a person's)
  and `/api/agent/runs` (a desktop's callback, read only, its own runs only: `actingAs`); `internal/sftexport` writes
  the checked-done runs as a training set (`/api/runs/training-export`, `decurix --export-training`). Chat (docs/chat.md),
  trajectory.md "Training export".
- All of it is host-only: `/api/llm/calls`, `/api/llm/blobs/`, `/api/judgements`, `/api/tasks/pass-rates`,
  `/api/runs/`, `/api/runs/search` and `/api/runs/training-export` are on the primary listener behind `humansOnly`;
  `/api/desktops/learned` and `/api/agent/runs` are desktops' callbacks there.
  The gated listener forwards no soul route but its read-only health (`/agent/<id>/api/health`).
