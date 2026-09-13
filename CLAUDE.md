# Flowera

Flowera is a control plane for spinning up disposable "desktop" VMs (Docker containers running a full XFCE desktop over VNC) and driving them with an autonomous AI agent. A Go server manages desktops via the Docker API; each desktop container runs a Python agent that watches for tasks, executes shell commands to get them done, and asks a human for help through the control plane when it's stuck.

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
                      ensure, SendMessage (POST to a desktop's :8081),
                      ProxyTarget (resolves the VNC reverse-proxy address),
                      PersistMemoryFile (docker-exec write into a desktop),
                      Screenshot (screenshot.go: a minimal RFB client that reads
                      a desktop's framebuffer straight off Xvnc:5901 and returns
                      a cached JPEG thumbnail — what the dashboard cards show),
                      sandbox.go (the seccomp profile + core-dump ulimit every new
                      desktop is created with — what lets Chromium run sandboxed;
                      the profile itself is seccomp-desktop.json, go:embed'ed)
internal/ollama    — DesktopURL/DesktopModel (the Ollama every desktop is given) and
                      preloading the model there at startup; also the lifecycle of the
                      bundled "flowera-ollama" container, used only when OLLAMA_URL names it
internal/helpnotes — in-memory store of help requests submitted by agents
internal/memory    — in-memory store of worker-memory entries
internal/conversation — in-memory per-desktop chat transcript: the messages a human
                      and a desktop's agent exchanged, plus the live steps of the
                      turn running right now. The control plane's own record, which
                      is what makes a desktop's chat the same on every device
internal/tunnel    — in-memory store of the current Cloudflare quick tunnel URL,
                      set by scripts/linux/build.sh via POST /api/tunnel, surfaced in
                      the dashboard's Mobile dropdown
internal/httpapi   — the only package touching net/http; all HTTP handlers,
                      composes desktop+helpnotes+memory+conversation+tunnel+webui
internal/webui     — html/template rendering (PageData, dashboard/messages/desktop-view
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
                      messages.html, desktop.html (the per-desktop chat behind the
                      dashboard's Chat button — one chat for every harness, and
                      nothing else on the page; the screen opens in its own tab)

desktop container (flowera-desktop:{basic,developer,ai})
  ├─ base image: accetto/ubuntu-vnc-xfce-chromium-g3 (XFCE + VNC + Chromium)
  ├─ XDG autostart (agent/flowera-agent.desktop) → agent/start_agent_terminal.sh
  │    → /opt/vm_server.py (detached background process, no visible terminal); it
  │    also reinstalls agent/chromium-browser.init as ~/.chromium-browser.init (the
  │    browser's launch flags) and clears core dumps Chromium left behind
  └─ flowera_agent/ (Python package, imported by vm_server.py; :8081 inside the container)
        ├─ config.py       — env vars (incl. AGENT_HARNESS), /home/headless/{command,helptext,
        │                     final,work,memory} dirs
        ├─ state.py        — shared mutable state (current_task, waiting_for_help, ...) as
        │                     plain module attributes; every other module does
        │                     `from flowera_agent import state` and reads/writes
        │                     state.current_task etc. (module singleton = shared globals)
        ├─ identity.py     — get_identity_context(engine): who this desktop is (name, ID,
        │                     user, work dir), as facts for the system prompt. Shared by all
        │                     three harnesses - without it "what's your desktop name?" was
        │                     unanswerable and came back as a help request
        ├─ memory_context.py — get_memory_context: reads /home/headless/memory/*.txt; shared
        │                     by all three harnesses below
        ├─ messaging.py    — save_message (intake), send_helptext/ask_for_help/write_final_message
        │                     and backend_failure_note (turns a raw engine error into a note
        │                     that says what to do about it)
        │                     (POSTs to control plane /api/helptext, with kind+message so the
        │                     chat needn't re-parse the note) — shared by all three harnesses
        ├─ readiness.py    — wait_for_http_ok(url, label): the one "is it up yet" probe,
        │                     shared by every harness that has a backend to wait for. Never
        │                     raises and never gives up - "not up yet" is a state to sit in
        ├─ progress.py     — emit(): POSTs one live activity line to the control plane's
        │                     /api/agent/events, on a single background sender thread with a
        │                     bounded queue. Best-effort and harness-agnostic: this is what
        │                     makes the chat's step-by-step view identical for all three
        ├─ harness.py      — the ONLY place that branches on AGENT_HARNESS; re-exports
        │                     perform_task/continue_after_help from whichever harness below
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
        │                     write_final_message/ask_for_help, and tracks a session_id
        │                     (module-private state) to --resume across
        │                     ask_for_help ⇄ human-response round trips
        ├─ thebot_harness.py
        │                   — the "thebot" harness: TheBot's `soul` binary (agent/thebot/,
        │                     its own Go module, built into all three images and started as a
        │                     daemon by start_agent_terminal.sh) is a persistent HTTP server
        │                     with its own atomizer/scheduler/skills loop. This submits the
        │                     task, the standing context (identity + worker memory + approval
        │                     rules) and the work dir as separate fields to its /api/chat,
        │                     follows the job over its SSE stream (forwarding each log line to
        │                     progress.emit; polling is the fallback), and maps the job's
        │                     structured `outcome` to write_final_message/ask_for_help. A
        │                     reply goes back with reply=true, which resumes the paused job
        ├─ watchers.py     — command_watcher (polls COMMAND_DIR, dispatches through harness.py;
        │                     leaves a message queued in COMMAND_DIR while state.backend_ready
        │                     is False, rather than failing it)
        ├─ http_api.py     — Handler(BaseHTTPRequestHandler), POST /message
        └─ app.py          — main(): wait_for_ollama, start watcher thread, serve_forever
```

Control plane → desktop communication happens over the Docker bridge network `flowera-network` (container name `desktop_<id>`, not published ports). Desktop → control plane communication uses `CONTROL_PLANE_URL` (`http://host.docker.internal:8080`).

Go package dependency direction (no cycles): `httpapi` → `desktop`, `helpnotes`, `memory`, `conversation`, `tunnel`, `webui`; `webui` → `desktop`, `helpnotes`, `memory`, `web`; `desktop` → `ollama` (only for `ollama.DesktopURL`/`DesktopModel`, the Ollama a new desktop is given), `flowera` (the root package, for the embedded build context); `ollama`, `helpnotes`, `memory`, `conversation`, `tunnel`, `web`, `flowera` depend on nothing internal.

Three desktop profiles share the same `agent/` contents but differ in installed tooling:
- `docker/desktop.basic.Dockerfile` — VNC desktop only (no `claude` binary — Ollama harness only)
- `docker/desktop.developer.Dockerfile` — adds LibreOffice, git, build tools, Node, Go, Rust, gh, native `claude` binary
- `docker/desktop.ai.Dockerfile` — developer image + `@openai/codex`, native `claude` binary (same install as developer, not the npm package)

### Three agent harnesses

Each desktop picks a harness at creation time (`Desktop.Harness`, `desktop-harness` container label, `AGENT_HARNESS` env var passed into the container — see `internal/httpapi/desktops.go`'s `createDesktopHandler` and `internal/desktop/docker.go`'s `buildDesktopEnv`). All three reach the exact same command/helptext/memory/progress contract through `harness.py`'s dispatch:

- **`ollama`** (default): the hand-rolled `COMMAND:`/`DONE:`/`HELP:` loop in `task_runner.py`, driving an Ollama model.
- **`claude`**: requires the developer or ai profile (`claude` binary isn't installed in basic) and `ANTHROPIC_API_KEY` set in the control plane's `.env` (passed through to the container only when non-empty). Runs `claude --bare -p "<task>" --output-format stream-json --verbose --dangerously-skip-permissions --append-system-prompt "<memory + safety rules>"`, optionally `--resume <session_id>` to continue after a human responds. `--bare` forces API-key-only auth (no OAuth/keychain), matching "the easiest access mode." `--dangerously-skip-permissions` is intentional and matches the Ollama harness's existing unrestricted shell access (`commands.run_command`) — not a new risk category, just the same one applied to a second engine. `stream-json` (which *requires* `--verbose`) is what lets the turn be watched as it happens; its final `result` event carries the same fields the one-shot `json` format returned, so only `_run_claude` knows the difference.
- **`thebot`**: TheBot's `soul` binary, built from `agent/thebot/` (its own Go module) into all three images and started as a daemon on port 4900 when `AGENT_HARNESS=thebot`. Reuses the same `OLLAMA_URL`/`OLLAMA_MODEL` every desktop already gets, so it needs no env var of its own. Inside soul a job goes: **recall** (L1 facts) → one shared **system frame** (`internal/prompt`: persona + host context, byte-identical for every model call in the job so Ollama's prompt cache carries across them) → **triage** (`internal/atomizer/triage.go`, deterministic: `chat` / one `skill` / `operate` / `plan`) → a tagged step **graph** from the planner only for `plan` (`[do] [shell] [web] [read] [download] [write] [code] [docs] [answer]`, with `(after N)` dependencies) → the **scheduler** (`internal/scheduler`) runs ready steps in parallel under a process-wide model-concurrency limit (`llm.Limited`), recovers a failed step (retry / decompose / ask), replans the rest once if it still fails, and **pauses** with a `Checkpoint` when a step needs approval or an answer → one **synthesised reply** for multi-step jobs. `operate` is the **operator** skill (`internal/skills/operator.go`): a Go `COMMAND:`/`DONE:`/`HELP:` loop carrying the Ollama harness's measured lessons, which is what lets TheBot answer "what's the kernel version?" by running `uname -r` instead of planning a web search. Consequential commands and requests are stopped by `internal/safety` before they run, whatever the model decided.
- All three system prompts (`SYSTEM_PROMPT` in `ollama_client.py`, `SAFETY_SYSTEM_PROMPT` in `claude_harness.py`, `SAFETY_PREAMBLE` in `thebot_harness.py`) instruct the agent to stop and request human approval — via the same `HELP`/`ask_for_help` → helptext path — before payments, sending messages on the human's behalf (WhatsApp/email/SMS), or any action with real-world consequences it isn't fully confident about. Claude and TheBot signal it with a `NEEDS_APPROVAL:` prefix on their final answer; the Ollama loop uses its `HELP:` control signal.

### One chat, every harness

`/view/<id>` (the dashboard's **Chat** button) renders `web/templates/desktop.html`, which is the chat and nothing else — no embedded screen, identical for all three harnesses. A desktop's screen is a separate thing, opened in its own tab from the dashboard's open-in-a-new-tab button (which is consequently the only emitter of `?ui=touch`; see the `injectMobileEnhancements` note below). Nothing in the chat page branches on which engine is running, because every harness is addressed and reports back the same way:

```
human types in the chat
  → POST /api/desktops/send        → desktop.Manager.SendMessage → the desktop's :8081
                                     → COMMAND_DIR file → watchers.py → harness.py → engine
  → conversation.AppendUserMessage (the control plane's own record)

the agent works
  → progress.emit(line) → POST /api/agent/events → conversation.AppendStep
                                                 → live steps under a "Working…" card

the agent finishes, or needs a human
  → messaging.write_final_message / ask_for_help → POST /api/helptext
      → helpnotes.Add           (the Help rail keeps the whole formatted note)
      → conversation.AppendAgentMessage (the chat shows only the agent's own words)
```

The chat page holds no transcript of its own: it renders whatever snapshot the control plane last sent over `GET /api/desktops/chat/stream` (SSE, whole snapshots rather than deltas, so a reconnect is self-healing), falling back to polling `GET /api/desktops/chat`. That is what makes one desktop's conversation the same on a laptop, on a phone through the tunnel, and in a second window — and why a desktop's agent is genuinely *central* to it, rather than one chat box per browser.

Replying to a blocked agent is just sending it another message: `watchers.command_watcher` routes any message arriving while `state.waiting_for_help` is set to `continue_after_help` instead of starting a new task. So the chat, the Help rail's reply box and the Messages page are three doors into one conversation.

## Running it

```bash
./scripts/linux/setup.sh         # one-time host setup: Docker, cloudflared, QEMU (needs sudo)
./scripts/linux/build-images.sh  # builds the three flowera-desktop:* images and nothing else
./scripts/linux/build.sh         # build-images.sh, then restarts the control plane via docker
                                  # compose, waits for :8080, then opens a Cloudflare tunnel
./scripts/release/release.sh     # cross-compiles standalone executables + archives into dist/
docker compose up -d             # just the control plane (uses docker/Dockerfile, mounts /var/run/docker.sock)
go build ./... && go vet ./... && go test ./...   # compile, lint, unit tests
                                  # (internal/memory, internal/conversation, internal/webui)
```

`scripts/` is split by who runs it: `linux/` (`setup.sh`, `build-images.sh`, `build.sh`), `windows/` (the same three as `.ps1`), and `release/` (`release.sh` + `release.ps1`). Each `.sh` has a `.ps1` twin at the same position in the other directory — they are hand-maintained, not generated from each other, so a change to one needs the same change to the other. Every script resolves the repo root from its own location, never from `$PWD`.

### Distributable builds

`scripts/release/release.sh` / `release.ps1` cross-compile `cmd/flowera` for linux, windows and darwin × amd64/arm64 with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<version>"`, writing to `dist/`: a bare binary per target, a per-target archive (`.zip` for windows, `.tar.gz` otherwise), and `SHA256SUMS`. Version defaults to `git describe --tags --always --dirty`, and `main.version` backs `flowera --version`.

The binary is fully self-contained: static (`CGO_ENABLED=0`, no libc), web templates from `web/embed.go`, desktop build context from `embed.go`, and `.env` optional. So an archive carries **nothing the binary depends on** — only a generated `README.txt` and an `.env.example`. `stageBundle`/`Copy-Bundle` branch on `goos` purely to write that README with one OS's commands (`./flowera` vs `.\flowera.exe`).

Keep it that way: any new runtime file dependency must be `go:embed`ed, not added to the bundle. If you ever have to put a real dependency in the archive, the "single executable is the whole product" claim in README.md needs to change with it.

See [README.md](README.md#-getting-started-running-it-locally) for full step-by-step setup instructions. Quick reference on env vars:

**Set by a human, in the control plane's own `.env`** (loaded via `godotenv` in `cmd/flowera/main.go`; see `internal/desktop/docker.go` for where each is read):

| Var | Read in | Default | Notes |
|---|---|---|---|
| `DEFAULT_PASSWORD` | `docker.buildDesktopEnv` | `admin` | Becomes `VNC_PW` in every new desktop container. |
| `ANTHROPIC_API_KEY` | `docker.buildDesktopEnv` | *(unset → omitted)* | Only passed into a container's env when non-empty. Required for the `claude` harness to authenticate; unused by `ollama`-harness desktops. |
| `DESKTOP_STOP_TIMEOUT_SECONDS` | `desktop.Manager.Stop` | `10` | Parsed with `strconv.Atoi`; invalid/`<=0` values are ignored. |

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

## Conventions / gotchas

- `CODE_MAP.md` is auto-generated (ctags + LLM summaries) — don't hand-edit it; it's regenerated from source after edits.
- Go doc comments: every exported/significant func gets a one-line `// FuncName does X` summary directly above it — keep following that pattern for new functions.
- The agent's output must start with exactly one of `COMMAND:` / `DONE:` / `HELP:` (see `ollama_client.SYSTEM_PROMPT`) — `parser.parse_agent_response` searches for whichever marker appears first, so don't change that contract without updating both sides.
- The desktop agent (`vm_server.py`) is launched as a detached background process by `agent/start_agent_terminal.sh` (via `setsid ... &`), not inside a visible terminal — output goes to `/tmp/flowera-startup.log` inside the container. Don't reintroduce a foreground/`xfce4-terminal --hold` launch; it makes the agent visible as a window on the VNC desktop instead of running silently.
- `flowera_agent/state.py`'s module attributes (`current_task`, `waiting_for_help`, etc.) are the agent's only cross-module shared state. Mutate them as `state.current_task = x` after `from flowera_agent import state` — never `from flowera_agent.state import current_task`, which copies the value instead of sharing it.
- `EnsureImages` no longer pulls. `flowera-desktop:*` are local-only tags that exist in no registry, so the old `ImagePull` could only ever fail with "pull access denied" on a fresh machine; it now just reports which images are missing and points at `--build-images`. Don't reintroduce a pull unless the images actually get published somewhere.
- `embed.go` sits at the module root on purpose — `go:embed` cannot reference parent directories, and the desktop build context spans `docker/` and `agent/`. Anything a desktop image needs at *docker build* time must be under one of those two trees and matched by an `embed.go` pattern, or the image builds fine from the repo and fails from a released binary. `go:embed` also silently skips names starting with `_` or `.`, which is what keeps `agent/__pycache__` out — but would equally skip a real file you name that way.
- `desktop.BuildImages` streams the embedded context to the daemon as a tar built on an `io.Pipe`, and `tar.Header` mode is hardcoded to `0644` because `embed.FS` carries no permissions. The desktop Dockerfiles already `chmod +x` what they need — a new script that relies on its executable bit surviving the tar will not work.
- The web templates are compiled into the binary via `web/embed.go`; `webui` parses them with `template.ParseFS(web.TemplatesFS, ...)`. Paths inside an `embed.FS` are always forward-slashed, so join them with `path.Join`, never `filepath.Join` (which would emit backslashes on a Windows build and fail to resolve). Adding a new template under `web/templates/` is picked up by the existing `//go:embed templates/*.html` glob, but a template in a *subdirectory* needs the glob widened.
- `cmd/flowera`'s `loadEnv` looks for `.env` in the working directory first, then next to the executable — that second lookup is what lets a downloaded binary find the `.env` shipped beside it. `docker compose` runs are unaffected (they get `.env` at the image's `WORKDIR`).
- `docker/Dockerfile`'s runtime stage only copies the binary and `.env` — the templates ride along inside the binary. It still needs a `.env` to exist at build time; `docker compose up` fails without one.
- `desktop.sanitizeName` normalizes user-supplied desktop names into safe container/volume name fragments (lowercase, `[a-z0-9_-]` only) — every `desktop.Manager` method already routes IDs through it; don't bypass it when adding new Docker API calls.
- `Desktop.URL` (`desktop.desktopURL`) is root-relative (`/desktop/<id>/`) on purpose, so the dashboard's preview iframe and "open" button resolve against whatever origin the page was loaded from — localhost, a LAN IP, or the Cloudflare tunnel. Don't rebuild it as an absolute `http://<host>:8080/...` URL: the old `DESKTOP_HOST`-based version pointed phones on the tunnel at their own localhost, and an `http://` iframe inside the tunnel's `https://` page is blocked as mixed content anyway. `index.html` resolves it to an absolute URL client-side only for display/copy. The trailing slash (no `vnc.html` in the visible URL — end users shouldn't need to know noVNC is involved) is rewritten to the underlying `vnc.html` internally by `httpapi.desktopProxyHandler`. noVNC (1.6) builds its WebSocket URL relative to the page too, so `/desktop/<id>/websockify` rides the same proxy.
- `desktop.Manager`'s and `helpnotes.Store`'s/`memory.Store`'s mutexes each guard one struct's own state — hold the lock for the whole read-modify-write, not just parts of it.
- `httpapi.jsonError` intentionally does not leak `err.Error()` to the client (logs it server-side, returns a generic message) — preserve that when adding new handlers that call it.
- `memory.WorkerMemoryEntry` records are kept even if `desktop.PersistMemoryFile` (the write into the desktop's container) fails afterward — that's intentional (matches the original single-file behavior), not a bug to "fix" by rolling back the in-memory record.
- `harness.py` is the only file that should ever check `config.AGENT_HARNESS` — `task_runner.py`/`claude_harness.py`/`thebot_harness.py` and everything upstream of them (`watchers.py`, `messaging.py`, `memory_context.py`, `identity.py`, `progress.py`, `readiness.py`) must stay harness-agnostic, so adding a fourth harness only means adding one more branch in `harness.py`.
- **Prompt evaluation, not generation, is what makes a desktop feel slow**, and `keep_alive` is the lever. Measured on an ordinary CPU-only laptop with `qwen2.5-coder:3b`: the agent's 545-token system prefix took **31.8 seconds** to read cold and **0.6 seconds** with the model still resident. Ollama evicts a model after 5 minutes idle and throws away the evaluated prompt prefix cached alongside it, so a task that pauses to run a command comes back to a cold model. Every caller therefore sends `keep_alive` (`ollama_client.ollama_request`, TheBot's `llm.OllamaProvider.KeepAlive`), and `ollama.Manager.preloadModel` loads the model at control-plane startup so the first human message never pays for it. The corollary: **keep the system prefix byte-identical across calls**, which is why both halves of the Ollama harness go through `ollama_client.build_system_messages` rather than assembling their own, and why `num_ctx` is a fixed 8192 everywhere — changing it makes Ollama reload the model and discard the cache.
- **Warm the Ollama the desktops actually use, at the context size they use.** `ollama.Manager.EnsureReady` used to start the bundled `flowera-ollama` container and preload the model *there* whatever `OLLAMA_URL` said, and without `num_ctx`. Measured on a 7.6 GB CPU-only host with `OLLAMA_URL` pointing at the host's Ollama: the preload held 2 GB of RAM on a server nobody talked to, the real one loaded cold in 210 s inside the first message's 300 s timeout, and even a correctly-aimed 4096-context preload would have been thrown away by the agents' first 8192 request. It now preloads `ollama.DesktopURL()` with `ollama.NumCtx`, and touches the bundled container only when `OLLAMA_URL` is `ollama.BundledURL`. The same rule is why TheBot's `[code]` steps must really be code: `internal/coding` asks for a 32768 window, and one such request reloads the shared model for **every** desktop (161 s, measured) and then reloads it back.
- A **backend failure ends the task** (`write_final_message(..., kind="error")`), it does not pause it with `ask_for_help`. The note tells the human to send the message again, and a paused desktop routes the next message to `continue_after_help` — measured live, a new task was taken as the "reply" to a greeting that had timed out, and reported under the greeting's Task. The exception is a failure inside `continue_after_help` itself, where a resend of the reply should still carry the original task.
- The reasoning model is not the only model. `nomic-embed-text` is a **separate dependency** that only the `thebot` harness needs, and Ollama will not embed with a chat model. Nothing pulled it, and the failure was invisible in the worst way: soul kept answering questions while every write to its Data RAG, its L1 long-term memory and its negative-feedback store failed with `model "nomic-embed-text" not found`. An agent that cannot record what it learned still looks like it works — it just never improves, and repeats the same failure next time. `ollama.EnsureReady` now ensures both models (the embedding one best-effort, since the other two harnesses never embed), and `scripts/linux/build.sh` does the same for a host or remote Ollama the control plane does not manage.
- A model that asks for help before running a single command has not tried anything, and `task_runner` pushes back exactly once (`PREMATURE_HELP_NUDGE`). This is what "hello, whats your desktop name?" needed: it came back as a help request asking the human *why they wanted to know*, blocking the desktop on a question. Two things keep the pushback honest — it fires only once, so a second HELP is always delivered, and its last paragraph explicitly invites a safety pause (payment, outgoing message, real-world consequence) to be re-sent unchanged. Verified against the real model: a bogus HELP becomes `DONE: kiolo`, while "pay the 400 dollar invoice" and "email my landlord" still escalate.
- **A reply with no marker is still an answer.** `parse_agent_response` returning `invalid` used to re-prompt with a generic restatement of the format, up to the 50-step limit, and then tell the human the agent "may be stuck in a loop". Measured live: asked how many files are in `/etc`, the model ran the right command, read the right number and replied `241` — finished at step two — then spent forty-eight more steps being lectured about markers before paging a human about a task it had already completed. Two fixes, both load-bearing: `invalid_format_nudge` quotes the model's own opening back to it (the generic version produced another bogus HELP; the quoting version produced `DONE: 241`), and after `MAX_INVALID_RESPONSES` tries `salvage_unmarked_response` delivers the text as the final answer rather than raising a false alarm.
- **A repeated command is how the Ollama loop actually runs out of steps**, and `task_runner` catches it deterministically rather than trusting the prompt to forbid it. The model writes `COMMAND:` and a *predicted* `DONE:` in the same reply; `parse_agent_response` correctly takes only the command, the prediction is dropped, and the model - which considers itself finished - sends the identical pair again. Measured on "what is the current kernel version?": `uname -r` fifty times. An identical consecutive command now gets `repeated_command_nudge` (the output it already produced, and an instruction to answer), and if it repeats again the task ends with `final_answer_from_command`. That reports **what the command actually printed, never the agent's own trailing DONE:** — on step one of that very case it predicted `5.10.124-microsoft-standard-WSL2` before running anything, and the real answer was `6.6.87.2`.
- `parse_agent_response` matches uppercase markers anywhere in the response, then falls back to a line-anchored, case-insensitive pass for `Help:`, `**DONE:**` and `HELP :` (all seen live). The two passes are not interchangeable: a lowercase `help:` mid-sentence is ordinary English, so accepting it anywhere would truncate a real DONE answer at the word "help:". Keep the loose pattern anchored and second.
- What a human reads when an engine fails must not be the engine's own words. `messaging.backend_failure_note` rewrites `context deadline exceeded` / connection refused / missing-model errors into what happened and what to do, keeping the raw text underneath — the note used to hand someone `ollama: request failed: Post "http://host.docker.internal:11434/api/chat": context deadline exceeded` as the entire explanation for why their desktop stopped. `thebot_harness._escalate` applies the same rewrite to failures TheBot words in its own voice ("could not reason about the failure (...) - stopping here").
- `thebot_harness` sends **the task and the standing context as separate fields** (`prompt` and `context` on soul's `/api/chat`), never one string. They used to be glued together, and soul classified, planned, stored in session history and answered the whole blob: "hello, whats your name?" spent 97 seconds in a planning call and came back as `NEEDS_APPROVAL: Before sending any real-world consequences...` — an answer to the safety rule — and "what is the current kernel version?" became a plan to search the web for the weather in New York. The context now only ever reaches soul's system frame; classifiers, the planner and history see the task alone. Measured before/after on `qwen2.5-coder:3b`, CPU only: the greeting went from 148 s (wrong) to 56 s (right), the kernel version from 205 s (wrong) to 45 s (`uname -r`), and creating a file in the work dir from a failed recovery that tried to `chmod /home/headless` to 18 s.
- soul is **deterministic before it is clever**: `atomizer.Triage` routes a request without a model call, and only a genuinely compound request (`looksCompound`) is sent to the planner. The removed layer asked the model "is this a task or a question?" for every request no classifier claimed — the single most expensive and least reliable call in the agent. Keep new routing rules in `triage.go`, deterministic, with a `triage_test.go` case; a chat answer that turns out to need the machine replies `NEEDS_TOOLS` and the scheduler reroutes it to the operator, so a triage miss costs one short call, not a wrong answer.
- soul keeps **one paused job per session** (`server.paused`). `thebot_harness.perform_task` sends `reply: false` (drop any paused job — this is a new request) and `continue_after_help` sends `reply: true` (resume it). A resumed approval runs the exact command it stopped before; a resumed question hands the human's words to the operator's saved transcript. Don't restate the task in a reply — the checkpoint already has it, and restating it makes the operator start over.
- The prompt frame's **volatile sections are chat-only**: live counters (`selfStats`) and the time of day go into a conversational job's frame and nothing else; every other job gets the date alone. With the counters in every frame, the job-log count — which goes up by one per job — changed the bytes right before the operator's contract and made every job re-read it cold.
- **An operator HELP straight after a successful command gets one grounded push back** (`successHelpNudge`) unless it asks for something only a person can give (`humanOnlyRe`). Measured live: `ls -l /etc | wc -l` → exit 0, `118` → `HELP: It seems there was an error. Please try running the command again.`; and `uname -r` → the correct kernel version, delivered under `HELP:`. Both would have paged a human about finished tasks. Set `THEBOT_LOG_MODEL_REPLIES=1` on the container to see raw operator replies in the job log.
- **The operator's transcript keeps only the command, never the answer the model guessed beside it.** A 3B model writes `COMMAND: df -h DONE: 1.8T total, 20G used, 1.6T available` before anything runs; kept in history, it reads that guess back and reports it — measured, word for word, against a real `df` of 1007G/121G/835G. The same leftover guess made it call the real kernel version "not what was expected". (The Ollama harness's `task_runner` appends the raw response the same way.) Likewise `atomizer.scrubInventedContent` strips quoted content the planner wrote into a write step that follows research, unless the human typed it: the planner put `write "The newest Go version is v1.20.5"` behind a search that found 1.27, and file-write saved the guess.
- An abandoned Ollama request is **not** cancelled: when a caller times out or disappears mid-prompt, this Ollama keeps evaluating it, and the next call queues behind it with its own timeout already running — which is how one oversized prompt turned into a chain of `context deadline exceeded` failures on unrelated small requests. Prompt size is therefore a correctness issue on CPU hosts, not just speed: `THEBOT_WEB_PAGES` / `THEBOT_WEB_PAGE_CHARS` (set to 2 / 2500 by `start_agent_terminal.sh`; soul's own defaults of 3 / 6000 assume a GPU) keep a web step's synthesis prompt under the timeout — at 3 × 6000 it took more than 300 s to read and timed out.
- `thebot_harness` sends `sessionId` (the desktop ID). soul keeps real per-session history and threads it into every turn, but `session.Store`'s `Append` and `History` are both no-ops for an empty id — which is what this harness used to send, making TheBot the only harness with no memory of the message before last.
- `scripts/windows/build.ps1` has **no** counterpart to `build.sh`'s Ollama-selection block (reachability probing, model pinning, the embed-model pull). That is a pre-existing gap in the twin convention above, not a deliberate exception; a Windows user gets whatever is already in `.env`.
- The desktop agent binds `:8081` *before* waiting on its reasoning backend (`app.py` runs `harness.wait_for_backend` on a background thread). It used to call `wait_for_ollama()` inline, so an unreachable Ollama meant the port was never bound at all — and since `desktop.Manager.SendMessage` POSTs there, every human message from the chat, the Messages page and a help reply came back as an unexplained HTTP 500 while the desktop looked healthy in the dashboard. Keep the message channel independent of the model: a task that arrives with the backend down fails *inside* the harness, which reports it through `ask_for_help` as a message the human can actually read. `wait_for_backend` also belongs to `harness.py` for the usual reason — `app.py` must not learn which engine is running.
- A message that arrives before the reasoning backend is up is **queued, never failed**. `watchers.command_watcher` leaves the file sitting in `COMMAND_DIR` while `state.backend_ready` is False and picks it up on a later pass, the same way it already defers a message that arrives mid-task; `harness.wait_for_backend` sets that flag once the engine answers. Dispatching into a backend that is still starting is what turned "Ollama is 20 seconds from ready" into a help request asking a human to fix something that was about to fix itself. Every branch of `wait_for_backend` must mean *reachable*, not *launched* — `thebot` waits on both the shared Ollama and soul's own `/api/health`, because soul reports an unreachable Ollama as a reasoning failure that reads like a real one.
- The command watcher claims the task slot (`state.current_task = content`, under `state.state_lock`) *before* starting the harness thread, not inside `perform_task`. Otherwise the loop tests `current_task` again for the next queued file before the thread it just started has run a line, and dispatches a second task on top of the first — rare when messages trickled in one at a time, routine once a queued backlog can be released all at once.
- Don't hardcode a Cloudflare quick-tunnel URL as anyone's default. Quick-tunnel hostnames are regenerated on every `cloudflared` restart, so a checked-in one is dead code the day it's written — `buildDesktopEnv`'s `OLLAMA_URL` default was one, and every desktop created after that tunnel died was structurally brainless. Tunnel URLs belong in `.env` (or `internal/tunnel`, set at runtime), never in a literal.
- `progress.emit` is decoration and must stay that way: it never blocks, never raises into a harness, and drops lines when its bounded queue is full. A desktop reporting what it is doing must never be able to slow down or fail the task it is describing — which is also why `agentEventHandler` answers a malformed event with 200 rather than an error the agent would then have to handle.
- **A turn's last step can arrive after its reply.** Steps go out on `progress`'s background queue, but the reply is POSTed directly, so TheBot's final log line ("wrote … verified on disk") reached the control plane under a millisecond *after* the answer — and `AppendStep`, seeing no running turn, opened a new one that nothing ever closed: the chat showed "Working…" forever over a finished task. Two guards: `messaging.send_helptext` calls `progress.flush()` (bounded) before sending, and `conversation.AppendStep` attaches a step arriving within `lateStepWindow` of an agent reply to that reply instead of reopening a turn — the second is what covers desktops built from an older image.
- `DESKTOP_ID` (what a desktop calls itself on every callback) and `Desktop.ID` (what the dashboard lists it as, derived from the container name) must produce the same string, since help notes and chat messages are keyed by it. Both now go through `sanitizeName`; they diverged for any name containing a character only `sanitizeName` rewrites (`my.box` stayed `my.box` in the env var while the desktop was listed as `my-box`), and that desktop's callbacks matched no desktop the dashboard knew. Use `desktop.SanitizeID` from outside the package.
- A dashboard card's preview is a JPEG from `/api/desktops/screenshot`, **never an embedded screen**. It used to be `<iframe src="/desktop/<id>/">` — a whole noVNC client per card, measured at 83 requests plus a WebSocket each, all against the control plane's single origin, which a browser serves over at most 6 concurrent HTTP/1.1 connections. Two desktops saturated that pool (178 requests over 7 connections, ~950ms stalls) and the VNC handshake for a screen a human had actually opened queued behind the thumbnails — 11s in one run, "Failed to connect to server" in the next. That is why "spin up two VMs and you can't work on both" was a browser-side bug with a clean server: the Go proxy served 120 concurrent requests with zero errors. `desktop.Screenshot` reads RFB directly instead (~8KB, ~80ms), and `index.html`'s `shotLoop` refreshes cards in one *sequential* sweep so the dashboard never has more than one thumbnail request in flight whatever the desktop count. Don't reintroduce a per-card iframe, and don't parallelise that sweep.
- `desktop.Screenshot` connects as a *shared* RFB client (`ClientInit` shared=1) and only ever sends a FramebufferUpdateRequest — a thumbnail must stay invisible to whoever is using the screen. It offers `Raw` as the only encoding, so the server cannot answer with a compression scheme there is no decoder for, and it reads `VNC_PW` off the container rather than the control plane's `DEFAULT_PASSWORD`, so a desktop created before that value changed still has a readable screen.
- The chat renders `content` through `textContent` only, never `innerHTML` — the text in a bubble is written by a model that can be told to emit anything. `renderContent` builds real `<pre>`/`<code>` nodes for fenced and inline spans and leaves everything else literal; keep that property when extending the formatting.
- `internal/webui/render_test.go` executes every page template. Templates are data to the Go compiler, so a typo'd action or a renamed field is invisible to `go build` and shows up as a panic or a 500 on the first request — add new pages to that test.
- `httpapi.agentProxyHandler` (`/agent/<id>/`) still proxies TheBot's own UI on port 4900, but nothing in the control plane's UI links to it any more: the chat page talks to TheBot through `flowera_agent` like every other harness, so it picks up worker memory, the safety preamble and the approval path, none of which soul's own UI goes through. Keep it as a debugging surface; don't wire it back into the UI as a second way to command a desktop.
- The base image ships `/home/headless` as `root:root` — anything that needs to write directly under it at **build time** (not runtime, where the real entrypoint/volume-mount semantics differ) needs an explicit `chown headless:headless /home/headless` first, same as the existing `RUN ... chown -R headless:headless /home/headless/.config` step. This bit the `claude` native-binary install (`~/.claude`, `~/.local`) until fixed in `docker/desktop.developer.Dockerfile`/`docker/desktop.ai.Dockerfile`.
- **Chromium in a desktop runs sandboxed**, which takes two halves that must both be present. `desktop.Manager.Create` gives every new desktop `internal/desktop/seccomp-desktop.json` — Docker's own default seccomp profile with `clone`/`setns`/`unshare` opened, nothing else — because Chromium's sandbox is built from user/PID/network namespaces that the stock profile forbids (the base image's answer was a hardcoded `--no-sandbox`, and the "unsupported command-line flag: --no-sandbox" bar on every window). And `agent/chromium-browser.init` probes `unshare --user --pid --net` at each browser launch and only adds `--no-sandbox` back when namespaces really are unavailable — a desktop created before the profile existed, or a daemon without seccomp (`desktopSecurityOpt` checks `SecurityOptions` and skips the profile there, since such a daemon rejects the container outright). **A desktop created before this change keeps showing the warning until it is recreated**: the seccomp profile is fixed at container creation. The warning is left visible in that fallback on purpose — the browser really is unsandboxed there.
- Desktops run with **core dumps off** (`desktopUlimits`, plus `ulimit -c 0` in `chromium-browser.init` for desktops created earlier). The `core.<pid>` files that filled a TheBot desktop's `~/Desktop` were not TheBot's: parsed out of the ELF notes, every one was a Chromium utility process (`--utility-sub-type=data_decoder` / `unzip`) dying with `*** stack smashing detected ***` in a restart loop, ~50 MB every fifteen seconds, landing in `~/Desktop` because that is where the XFCE menu starts the browser. `start_agent_terminal.sh` deletes existing ones, matching both the `core.<digits>` name and the ELF `ET_CORE` header.
- The chat renders a sent message **the moment Send is pressed** (`desktop.html`'s `pending` list), then drops that local copy as soon as a transcript snapshot contains the server's own (the first new user message with the same text that was not in the transcript at send time). The server stays the only record — `sendMessageHandler` still appends only after the desktop accepts the message (measured at 5–12 ms) — and a failed send stays on screen with Retry and Edit.
- **The Cloudflare quick tunnel does not deliver the chat's event stream.** Measured through a `trycloudflare.com` tunnel: `/api/desktops/chat/stream` returns its headers and then *no body bytes at all* — not the snapshot the handler flushes on connect, not a message sent a second later — for as long as it was watched (40 s), while the same stream on localhost delivered the message in 70 ms. The chat used to fall back to polling only on a stream *error*, and a held stream never errors, so on a phone a sender's own message appeared only when the agent's reply finally pushed something through: the "my message only shows up with the response" bug. `connect()` now closes a stream that has not delivered its first frame within `FIRST_FRAME_MS` (4 s) and polls instead; over the tunnel a sent message shows in ~70 ms (locally rendered), the server's copy in ~2 s, and live steps from ~2 s. Anything else that relies on SSE through the tunnel needs the same fallback.
- `NEEDS_TOOLS` is taught **only to the chat skill** (`prompt.ChatRule`, appended to the frame by `GeneralSkill`), never in the shared `Persona`: with it in the persona, the operator answered "ping - just reply pong" with `HELP: NEEDS_TOOLS`, which a human received as an entire help request. The operator also treats that marker as a malformed reply.
- The XFCE desktop wallpaper is set by baking `agent/flowera-desktop-wallpaper.xml` (pointing at `agent/flowera-wallpaper.svg`, copied to `/opt/flowera-wallpaper.svg`) into `/home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml` at image build time, in all three desktop Dockerfiles — otherwise XFCE generates its own default (the base image's stock `xfce-shapes.svg`) the first time it starts with no config present. Since `/home/headless` is the container's persistent named volume, this file only seeds *new* desktops (an empty volume on first start); it has no effect on a desktop created before an image rebuild.
