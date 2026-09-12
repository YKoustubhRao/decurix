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
                      a cached JPEG thumbnail — what the dashboard cards show)
internal/ollama    — lifecycle of the single shared "flowera-ollama" container
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
  │    → /opt/vm_server.py (detached background process, no visible terminal)
  └─ flowera_agent/ (Python package, imported by vm_server.py; :8081 inside the container)
        ├─ config.py       — env vars (incl. AGENT_HARNESS), /home/headless/{command,helptext,
        │                     final,work,memory} dirs
        ├─ state.py        — shared mutable state (current_task, waiting_for_help, ...) as
        │                     plain module attributes; every other module does
        │                     `from flowera_agent import state` and reads/writes
        │                     state.current_task etc. (module singleton = shared globals)
        ├─ memory_context.py — get_memory_context: reads /home/headless/memory/*.txt; shared
        │                     by all three harnesses below
        ├─ messaging.py    — save_message (intake), send_helptext/ask_for_help/write_final_message
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
        │                     with its own atomizer/scheduler/skills loop. This submits a task
        │                     to its /api/chat and follows the job over its SSE stream,
        │                     forwarding each log line to progress.emit; polling is the
        │                     fallback for a stream that drops before the job finishes
        ├─ watchers.py     — command_watcher (polls COMMAND_DIR, dispatches through harness.py;
        │                     leaves a message queued in COMMAND_DIR while state.backend_ready
        │                     is False, rather than failing it)
        ├─ http_api.py     — Handler(BaseHTTPRequestHandler), POST /message
        └─ app.py          — main(): wait_for_ollama, start watcher thread, serve_forever
```

Control plane → desktop communication happens over the Docker bridge network `flowera-network` (container name `desktop_<id>`, not published ports). Desktop → control plane communication uses `CONTROL_PLANE_URL` (`http://host.docker.internal:8080`).

Go package dependency direction (no cycles): `httpapi` → `desktop`, `helpnotes`, `memory`, `conversation`, `tunnel`, `webui`; `webui` → `desktop`, `helpnotes`, `memory`, `web`; `desktop` → `ollama` (only for the shared `ollama.Model` constant), `flowera` (the root package, for the embedded build context); `ollama`, `helpnotes`, `memory`, `conversation`, `tunnel`, `web`, `flowera` depend on nothing internal.

Three desktop profiles share the same `agent/` contents but differ in installed tooling:
- `docker/desktop.basic.Dockerfile` — VNC desktop only (no `claude` binary — Ollama harness only)
- `docker/desktop.developer.Dockerfile` — adds LibreOffice, git, build tools, Node, Go, Rust, gh, native `claude` binary
- `docker/desktop.ai.Dockerfile` — developer image + `@openai/codex`, native `claude` binary (same install as developer, not the npm package)

### Three agent harnesses

Each desktop picks a harness at creation time (`Desktop.Harness`, `desktop-harness` container label, `AGENT_HARNESS` env var passed into the container — see `internal/httpapi/desktops.go`'s `createDesktopHandler` and `internal/desktop/docker.go`'s `buildDesktopEnv`). All three reach the exact same command/helptext/memory/progress contract through `harness.py`'s dispatch:

- **`ollama`** (default): the hand-rolled `COMMAND:`/`DONE:`/`HELP:` loop in `task_runner.py`, driving an Ollama model.
- **`claude`**: requires the developer or ai profile (`claude` binary isn't installed in basic) and `ANTHROPIC_API_KEY` set in the control plane's `.env` (passed through to the container only when non-empty). Runs `claude --bare -p "<task>" --output-format stream-json --verbose --dangerously-skip-permissions --append-system-prompt "<memory + safety rules>"`, optionally `--resume <session_id>` to continue after a human responds. `--bare` forces API-key-only auth (no OAuth/keychain), matching "the easiest access mode." `--dangerously-skip-permissions` is intentional and matches the Ollama harness's existing unrestricted shell access (`commands.run_command`) — not a new risk category, just the same one applied to a second engine. `stream-json` (which *requires* `--verbose`) is what lets the turn be watched as it happens; its final `result` event carries the same fields the one-shot `json` format returned, so only `_run_claude` knows the difference.
- **`thebot`**: TheBot's `soul` binary, built from `agent/thebot/` (its own Go module) into all three images and started as a daemon on port 4900 when `AGENT_HARNESS=thebot`. Reuses the same `OLLAMA_URL`/`OLLAMA_MODEL` every desktop already gets, so it needs no env var of its own.
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

**Set automatically per-desktop** (baked into the container's env by `buildDesktopEnv` at creation time — not something you set in `.env`, listed here for wiring reference): `VNC_PW`, `VNC_RESOLUTION` (hardcoded `1280x720`), `DESKTOP_ID` (`sanitizeName(name)` — must stay equal to `Desktop.ID`, see the gotcha below), `DESKTOP_NAME`, `CONTROL_PLANE_URL` (hardcoded `http://host.docker.internal:8080`), `OLLAMA_URL` and `OLLAMA_MODEL` (the control plane's own env when set, otherwise `http://host.docker.internal:11434` + `qwen3:latest` — the Ollama on the host running the control plane, reached over the same `ExtraHosts` entry as `CONTROL_PLANE_URL`; that host's Ollama must listen on more than loopback, i.e. `OLLAMA_HOST=0.0.0.0`), `AGENT_HARNESS` (`ollama`/`claude`/`thebot`, from the harness chosen at creation).

**Claude-harness-only, read directly by `claude_harness.py`** — these currently have no UI/`.env` wiring (no `buildDesktopEnv` entry), so they're fixed at their defaults unless you add them to `buildDesktopEnv` or set them on the container yourself:

| Var | Default | Purpose |
|---|---|---|
| `CLAUDE_TASK_TIMEOUT_SECONDS` | `3600` | Max seconds a single `claude -p` invocation may run before being killed. |
| `CLAUDE_MAX_BUDGET_USD` | `5` | Passed as `--max-budget-usd` to `claude`, capping spend for one invocation. |

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
- `harness.py` is the only file that should ever check `config.AGENT_HARNESS` — `task_runner.py`/`claude_harness.py`/`thebot_harness.py` and everything upstream of them (`watchers.py`, `messaging.py`, `memory_context.py`, `progress.py`, `readiness.py`) must stay harness-agnostic, so adding a fourth harness only means adding one more branch in `harness.py`.
- The desktop agent binds `:8081` *before* waiting on its reasoning backend (`app.py` runs `harness.wait_for_backend` on a background thread). It used to call `wait_for_ollama()` inline, so an unreachable Ollama meant the port was never bound at all — and since `desktop.Manager.SendMessage` POSTs there, every human message from the chat, the Messages page and a help reply came back as an unexplained HTTP 500 while the desktop looked healthy in the dashboard. Keep the message channel independent of the model: a task that arrives with the backend down fails *inside* the harness, which reports it through `ask_for_help` as a message the human can actually read. `wait_for_backend` also belongs to `harness.py` for the usual reason — `app.py` must not learn which engine is running.
- A message that arrives before the reasoning backend is up is **queued, never failed**. `watchers.command_watcher` leaves the file sitting in `COMMAND_DIR` while `state.backend_ready` is False and picks it up on a later pass, the same way it already defers a message that arrives mid-task; `harness.wait_for_backend` sets that flag once the engine answers. Dispatching into a backend that is still starting is what turned "Ollama is 20 seconds from ready" into a help request asking a human to fix something that was about to fix itself. Every branch of `wait_for_backend` must mean *reachable*, not *launched* — `thebot` waits on both the shared Ollama and soul's own `/api/health`, because soul reports an unreachable Ollama as a reasoning failure that reads like a real one.
- The command watcher claims the task slot (`state.current_task = content`, under `state.state_lock`) *before* starting the harness thread, not inside `perform_task`. Otherwise the loop tests `current_task` again for the next queued file before the thread it just started has run a line, and dispatches a second task on top of the first — rare when messages trickled in one at a time, routine once a queued backlog can be released all at once.
- Don't hardcode a Cloudflare quick-tunnel URL as anyone's default. Quick-tunnel hostnames are regenerated on every `cloudflared` restart, so a checked-in one is dead code the day it's written — `buildDesktopEnv`'s `OLLAMA_URL` default was one, and every desktop created after that tunnel died was structurally brainless. Tunnel URLs belong in `.env` (or `internal/tunnel`, set at runtime), never in a literal.
- `progress.emit` is decoration and must stay that way: it never blocks, never raises into a harness, and drops lines when its bounded queue is full. A desktop reporting what it is doing must never be able to slow down or fail the task it is describing — which is also why `agentEventHandler` answers a malformed event with 200 rather than an error the agent would then have to handle.
- `DESKTOP_ID` (what a desktop calls itself on every callback) and `Desktop.ID` (what the dashboard lists it as, derived from the container name) must produce the same string, since help notes and chat messages are keyed by it. Both now go through `sanitizeName`; they diverged for any name containing a character only `sanitizeName` rewrites (`my.box` stayed `my.box` in the env var while the desktop was listed as `my-box`), and that desktop's callbacks matched no desktop the dashboard knew. Use `desktop.SanitizeID` from outside the package.
- A dashboard card's preview is a JPEG from `/api/desktops/screenshot`, **never an embedded screen**. It used to be `<iframe src="/desktop/<id>/">` — a whole noVNC client per card, measured at 83 requests plus a WebSocket each, all against the control plane's single origin, which a browser serves over at most 6 concurrent HTTP/1.1 connections. Two desktops saturated that pool (178 requests over 7 connections, ~950ms stalls) and the VNC handshake for a screen a human had actually opened queued behind the thumbnails — 11s in one run, "Failed to connect to server" in the next. That is why "spin up two VMs and you can't work on both" was a browser-side bug with a clean server: the Go proxy served 120 concurrent requests with zero errors. `desktop.Screenshot` reads RFB directly instead (~8KB, ~80ms), and `index.html`'s `shotLoop` refreshes cards in one *sequential* sweep so the dashboard never has more than one thumbnail request in flight whatever the desktop count. Don't reintroduce a per-card iframe, and don't parallelise that sweep.
- `desktop.Screenshot` connects as a *shared* RFB client (`ClientInit` shared=1) and only ever sends a FramebufferUpdateRequest — a thumbnail must stay invisible to whoever is using the screen. It offers `Raw` as the only encoding, so the server cannot answer with a compression scheme there is no decoder for, and it reads `VNC_PW` off the container rather than the control plane's `DEFAULT_PASSWORD`, so a desktop created before that value changed still has a readable screen.
- The chat renders `content` through `textContent` only, never `innerHTML` — the text in a bubble is written by a model that can be told to emit anything. `renderContent` builds real `<pre>`/`<code>` nodes for fenced and inline spans and leaves everything else literal; keep that property when extending the formatting.
- `internal/webui/render_test.go` executes every page template. Templates are data to the Go compiler, so a typo'd action or a renamed field is invisible to `go build` and shows up as a panic or a 500 on the first request — add new pages to that test.
- `httpapi.agentProxyHandler` (`/agent/<id>/`) still proxies TheBot's own UI on port 4900, but nothing in the control plane's UI links to it any more: the chat page talks to TheBot through `flowera_agent` like every other harness, so it picks up worker memory, the safety preamble and the approval path, none of which soul's own UI goes through. Keep it as a debugging surface; don't wire it back into the UI as a second way to command a desktop.
- The base image ships `/home/headless` as `root:root` — anything that needs to write directly under it at **build time** (not runtime, where the real entrypoint/volume-mount semantics differ) needs an explicit `chown headless:headless /home/headless` first, same as the existing `RUN ... chown -R headless:headless /home/headless/.config` step. This bit the `claude` native-binary install (`~/.claude`, `~/.local`) until fixed in `docker/desktop.developer.Dockerfile`/`docker/desktop.ai.Dockerfile`.
- The XFCE desktop wallpaper is set by baking `agent/flowera-desktop-wallpaper.xml` (pointing at `agent/flowera-wallpaper.svg`, copied to `/opt/flowera-wallpaper.svg`) into `/home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml` at image build time, in all three desktop Dockerfiles — otherwise XFCE generates its own default (the base image's stock `xfce-shapes.svg`) the first time it starts with no config present. Since `/home/headless` is the container's persistent named volume, this file only seeds *new* desktops (an empty volume on first start); it has no effect on a desktop created before an image rebuild.
