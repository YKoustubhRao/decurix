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
                      PersistMemoryFile (docker-exec write into a desktop)
internal/ollama    — lifecycle of the single shared "flowera-ollama" container
internal/helpnotes — in-memory store of help requests submitted by agents
internal/memory    — in-memory store of worker-memory entries
internal/tunnel    — in-memory store of the current Cloudflare quick tunnel URL,
                      set by scripts/linux/build.sh via POST /api/tunnel, surfaced in
                      the dashboard's Mobile dropdown
internal/httpapi   — the only package touching net/http; all HTTP handlers,
                      composes desktop+helpnotes+memory+tunnel+webui
internal/webui     — html/template rendering (PageData, dashboard/messages pages);
                      parses out of web.TemplatesFS, never off disk
web/embed.go       — //go:embed templates/*.html → web.TemplatesFS; what makes the
                      compiled binary standalone (package web has no internal deps)
web/templates/     — theme.html (shared design tokens + icon sprite + top bar +
                      chrome JS, pulled in by every page via {{template "theme"}} /
                      "sprite" / "topbar" / "chrome-js"; parsed alongside each page
                      by webui.templatePaths), dashboard.html, index.html, messages.html

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
        │                     by both harnesses below
        ├─ messaging.py    — save_message (intake), send_helptext/ask_for_help/write_final_message
        │                     (POSTs to control plane /api/helptext) — shared by both harnesses
        ├─ harness.py      — the ONLY place that branches on AGENT_HARNESS; re-exports
        │                     perform_task/continue_after_help from whichever harness below
        │                     is active, with identical signatures
        ├─ ollama_client.py + parser.py + commands.py + task_runner.py
        │                   — the "ollama" harness: hand-rolled COMMAND:/DONE:/HELP: loop
        │                     driving a local Ollama model
        ├─ claude_harness.py
        │                   — the "claude" harness: shells out to the `claude` CLI in
        │                     non-interactive mode per task; Claude Code runs its own
        │                     agentic loop internally, so this just maps its one-shot
        │                     JSON result to write_final_message/ask_for_help, and tracks
        │                     a session_id (module-private state) to --resume across
        │                     ask_for_help ⇄ human-response round trips
        ├─ watchers.py     — command_watcher (polls COMMAND_DIR, dispatches through harness.py)
        ├─ http_api.py     — Handler(BaseHTTPRequestHandler), POST /message
        └─ app.py          — main(): wait_for_ollama, start watcher thread, serve_forever
```

Control plane → desktop communication happens over the Docker bridge network `flowera-network` (container name `desktop_<id>`, not published ports). Desktop → control plane communication uses `CONTROL_PLANE_URL` (`http://host.docker.internal:8080`).

Go package dependency direction (no cycles): `httpapi` → `desktop`, `helpnotes`, `memory`, `tunnel`, `webui`; `webui` → `desktop`, `helpnotes`, `memory`, `web`; `desktop` → `ollama` (only for the shared `ollama.Model` constant), `flowera` (the root package, for the embedded build context); `ollama`, `helpnotes`, `memory`, `tunnel`, `web`, `flowera` depend on nothing internal.

Three desktop profiles share the same `agent/` contents but differ in installed tooling:
- `docker/desktop.basic.Dockerfile` — VNC desktop only (no `claude` binary — Ollama harness only)
- `docker/desktop.developer.Dockerfile` — adds LibreOffice, git, build tools, Node, Go, Rust, gh, native `claude` binary
- `docker/desktop.ai.Dockerfile` — developer image + `@openai/codex`, native `claude` binary (same install as developer, not the npm package)

### Two agent harnesses

Each desktop picks a harness at creation time (`Desktop.Harness`, `desktop-harness` container label, `AGENT_HARNESS` env var passed into the container — see `internal/httpapi/desktops.go`'s `createDesktopHandler` and `internal/desktop/docker.go`'s `buildDesktopEnv`). Both reach the exact same command/helptext/memory contract through `harness.py`'s dispatch:

- **`ollama`** (default): the existing hand-rolled loop.
- **`claude`**: requires the developer or ai profile (`claude` binary isn't installed in basic) and `ANTHROPIC_API_KEY` set in the control plane's `.env` (passed through to the container only when non-empty). Runs `claude --bare -p "<task>" --output-format json --dangerously-skip-permissions --append-system-prompt "<memory + safety rules>"`, optionally `--resume <session_id>` to continue after a human responds. `--bare` forces API-key-only auth (no OAuth/keychain), matching "the easiest access mode." `--dangerously-skip-permissions` is intentional and matches the Ollama harness's existing unrestricted shell access (`commands.run_command`) — not a new risk category, just the same one applied to a second engine.
- Both `SYSTEM_PROMPT` (ollama) and `SAFETY_SYSTEM_PROMPT` (claude, `claude_harness.py`) instruct the agent to stop and request human approval — via the same `HELP`/`ask_for_help` → dashboard helptext path — before payments, sending messages on the human's behalf (WhatsApp/email/SMS), or any action with real-world consequences it isn't fully confident about.

## Running it

```bash
./scripts/linux/setup.sh         # one-time host setup: Docker, cloudflared, QEMU (needs sudo)
./scripts/linux/build-images.sh  # builds the three flowera-desktop:* images and nothing else
./scripts/linux/build.sh         # build-images.sh, then restarts the control plane via docker
                                  # compose, waits for :8080, then opens a Cloudflare tunnel
./scripts/release/release.sh     # cross-compiles standalone executables + archives into dist/
docker compose up -d             # just the control plane (uses docker/Dockerfile, mounts /var/run/docker.sock)
go build ./... && go vet ./... && go test ./...   # compile, lint, unit tests (internal/memory)
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
| `DESKTOP_HOST` | `httpapi.getHost`, `desktop.ServerHost` | `localhost` | `docker-compose.yaml` sets this directly as a container env var too, which wins over `.env` (godotenv never overrides an already-set var) — edit the compose file to change it under `docker compose`. |
| `DESKTOP_STOP_TIMEOUT_SECONDS` | `desktop.Manager.Stop` | `10` | Parsed with `strconv.Atoi`; invalid/`<=0` values are ignored. |

**Set automatically per-desktop** (baked into the container's env by `buildDesktopEnv` at creation time — not something you set in `.env`, listed here for wiring reference): `VNC_PW`, `VNC_RESOLUTION` (hardcoded `1280x720`), `DESKTOP_ID`, `DESKTOP_NAME`, `CONTROL_PLANE_URL` (hardcoded `http://host.docker.internal:8080`), `OLLAMA_URL` (hardcoded `http://flowera-ollama:11434`), `OLLAMA_MODEL` (`ollama.Model` constant), `AGENT_HARNESS` (`ollama`/`claude`, from the harness chosen at creation).

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
- `desktop.Manager`'s and `helpnotes.Store`'s/`memory.Store`'s mutexes each guard one struct's own state — hold the lock for the whole read-modify-write, not just parts of it.
- `httpapi.jsonError` intentionally does not leak `err.Error()` to the client (logs it server-side, returns a generic message) — preserve that when adding new handlers that call it.
- `memory.WorkerMemoryEntry` records are kept even if `desktop.PersistMemoryFile` (the write into the desktop's container) fails afterward — that's intentional (matches the original single-file behavior), not a bug to "fix" by rolling back the in-memory record.
- `harness.py` is the only file that should ever check `config.AGENT_HARNESS` — `task_runner.py`/`claude_harness.py` and everything upstream of them (`watchers.py`, `messaging.py`, `memory_context.py`) must stay harness-agnostic so adding a third harness later only means adding one more branch in `harness.py`.
- The base image ships `/home/headless` as `root:root` — anything that needs to write directly under it at **build time** (not runtime, where the real entrypoint/volume-mount semantics differ) needs an explicit `chown headless:headless /home/headless` first, same as the existing `RUN ... chown -R headless:headless /home/headless/.config` step. This bit the `claude` native-binary install (`~/.claude`, `~/.local`) until fixed in `docker/desktop.developer.Dockerfile`/`docker/desktop.ai.Dockerfile`.
