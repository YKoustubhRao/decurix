# Running it, the --cli client, distributable builds, env vars

```bash
./scripts/mac/setup.sh           # macOS host setup: Docker, cloudflared, Ollama, QEMU, jq
./scripts/mac/build-images.sh    # builds desktop images with --platform linux/amd64
./scripts/mac/build.sh           # "open the app": builds only missing images, starts compose, opens tunnel
./scripts/mac/rebuild.sh         # "reinstall": removes desktop containers/volumes, wipes decurix-data, rebuilds
./scripts/linux/setup.sh         # Linux host setup: Docker, cloudflared, QEMU (needs sudo)
./scripts/linux/build-images.sh  # builds decurix-desktop:basic and decurix-tools:latest
./scripts/linux/build.sh         # "open the app": builds only missing images, starts the control
                                  # plane if it isn't already running, waits for :8080, opens a
                                  # tunnel. Leaves existing desktop_* containers/volumes and the
                                  # decurix-data volume (the DB) untouched. --cli opens the CLI
                                  # (below) against it instead of the tunnel
./scripts/linux/rebuild.sh       # "reinstall": removes every desktop_* container/volume, and
                                  # (after a confirmation prompt, -y to skip it) deletes the
                                  # decurix-data volume - task history, chats, worker memory, gone
                                  # for good. Rebuilds every image and the control plane too (so
                                  # agent/ or Dockerfile changes land), but with the normal build
                                  # cache, not --no-cache - stays as fast as build.sh. --cli same
                                  # as build.sh's
./scripts/mac/build_app.sh       # the Mac app, from scratch: dist/electron/mac*/Decurix.app (needs Docker Desktop to run)
./scripts/linux/build_app.sh     # the Linux app, from scratch: dist/electron/Decurix-<version>.AppImage (needs Docker to run)
scripts\windows\build_app.ps1    # the Windows app, from scratch: dist\electron\ installer + win-unpacked\Decurix.exe
./scripts/release/release.sh     # cross-compiles standalone executables + archives into dist/
docker compose up -d             # just the control plane (uses docker/Dockerfile, mounts /var/run/docker.sock)
go build ./... && go vet ./... && go test ./...   # compile, lint, unit tests
                                  # (internal/memory, internal/conversation, internal/webui,
                                  #  internal/stepimages, internal/httpapi, internal/tasks,
                                  #  internal/sqlitedb, internal/claudeauth, internal/lanaccess,
                                  #  internal/idlepause, internal/schedule, internal/suggest,
                                  #  internal/mailtrigger,
                                  #  internal/desktop, cmd/decurix's --cli)
PYTHONPATH=agent python3 -m unittest discover -s agent/tests   # computer-use gate/refs/rendering,
                                  # task IDs and usage totals, recordings (input merging,
                                  # privacy, volatility, the store, replay against a fake screen),
                                  # skills (generalising demos, loops, the questions and answers,
                                  # running a skill and asking when stuck, the gui/MCP tools and the
                                  # agent's /skills endpoints)
python3 bench/skills/e2e.py         # teach + run a skill on a real desktop (Docker, ~2 min)
python3 bench/recordings/e2e.py      # record, replay, move, reorder, import on a real desktop (Docker, ~3 min)
python3 bench/recordings/tune.py <dir>   # visual anchors against real frames (see visual.py; no display needed)
python3 bench/agents/gen_trajectories.py <dir>   # BrowserGym solves MiniWoB++ tasks (needs a py3.12 venv; see the file)
python3 bench/agents/e2e.py --traj <dir> --miniwob <html dir>   # replay them in a real desktop, scored by the page's reward
python3 bench/mail/e2e.py [--runs 8]              # email triggers on the real deployment: Dovecot + LMTP mail, this tree's
                                                  # control plane (own compose project and volume), a real desktop and model;
                                                  # passwords at rest, the desktop guard, spoofing, a sleep, then N injected
                                                  # invoices scored (~20 min; your own Decurix must be stopped)
python3 bench/screen/e2e.py <desktop id>          # the screen viewer on a real desktop (Playwright; restarts/pauses/stops it)
python3 bench/screen/teach_e2e.py <desktop id>    # record -> /learn -> run a skill through the viewer, as a person does
go test -tags e2e -run TestAPhoneAppIsButtonsInARealDesktop ./internal/httpapi/   # a phone app's picture, controls and
                                                  # approval gate in a real desktop's Chromium (fake phone; Docker, ~2 min)
python3 bench/gui/run.py --engine oracle --container desktop_<id>   # GUI benchmark, no model
python3 bench/gui/run.py --engine ollama --engine thebot --engine claude  # per-engine success rate
```

`scripts/` is split by who runs it: `mac/` (`setup.sh`, `build-images.sh`, `build.sh`, `rebuild.sh`, `quickstart.sh`, `test-vm.sh`, with `lib/`), `linux/` (`setup.sh`, `build-images.sh`, `build.sh`, `rebuild.sh`, `quickstart.sh`, `test-vm.sh`, with `lib/`), `windows/` (the same as `.ps1`, with `lib/Ollama-Setup.ps1`), and `release/` (`release.sh` + `release.ps1`). Each `.sh` has a `.ps1` twin at the same position in the other directory — they are hand-maintained, not generated from each other, so a change to one needs the same change to the other; the `lib/` sharing is within a platform only; `build.sh`/`build.ps1` and `rebuild.sh`/`rebuild.ps1` each source it independently rather than duplicating the ~200-line "find an Ollama the desktops can reach" logic. Every script resolves the repo root from its own location, never from `$PWD`.

### The `--cli` terminal client

`decurix --cli` (`cmd/decurix/cli.go`) is a text alternative to the dashboard/chat page: an interactive REPL (`list`/`create`/`start`/`stop`/`restart`/`delete`/`chat`/`queries`) that talks to a control plane already running somewhere, purely over `internal/httpapi`'s existing JSON endpoints — no Docker client, no database, nothing the server-mode branch of `main()` sets up. `main()` checks `-cli` immediately after `-version` and before any of that setup, same posture as `-build-images` short-circuiting startup. `chat <id>` polls `GET /api/desktops/chat` (the same snapshot the chat page's polling fallback uses) once a second, tracking how many `Thread.Entries`/`Thread.Steps` it has already printed so a repeated poll only shows what's new, and returns to the prompt once the turn stops `Running` with at least one new entry; Ctrl+C during that wait returns to the prompt without cancelling the task (it keeps running on the desktop) by trapping `os.Interrupt` only for the duration of that wait, never at the top-level prompt.

`build.sh --cli` / `rebuild.ps1 -Cli` (etc.) run their normal flow through "Decurix is running", then run `docker compose exec desktop-manager ./decurix --cli` instead of opening the Cloudflare tunnel — the same binary the container already runs, reached at its own `127.0.0.1:8080` (also `--cli`'s default `-url`, which is why it works unchanged if someone runs the released binary directly against the compose-published port instead). Both scripts clear their INT/TERM trap right before that hand-off: `build.sh`'s otherwise prints a "tunnel stopped" message that would not make sense here, and `rebuild.sh`'s otherwise tears the whole stack back down on Ctrl+C — correct for an interrupted *rebuild*, wrong once the rebuild has already finished and someone is just leaving the CLI.

### Distributable builds

`scripts/release/release.sh` / `release.ps1` cross-compile `cmd/decurix` for linux, windows and darwin × amd64/arm64 with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<version>"`, writing to `dist/`: a bare binary per target, a per-target archive (`.zip` for windows, `.tar.gz` otherwise), and `SHA256SUMS`. Version defaults to `git describe --tags --always --dirty`, and `main.version` backs `decurix --version`.

The binary is fully self-contained: static (`CGO_ENABLED=0`, no libc — which is why the SQLite driver is modernc.org/sqlite, not mattn/go-sqlite3), web templates from `web/embed.go`, desktop build context from `embed.go`, and `.env` optional. The one file it *writes* is `decurix.db` in `sqlitedb.DataDir()` (`DECURIX_DATA_DIR`, else `os.UserConfigDir()/decurix`; the Docker image sets `/data`, which `docker-compose.yaml` mounts as the `decurix-data` volume). So an archive carries **nothing the binary depends on** — a generated `README.txt`, an `.env.example`, and `decurix-android-arm64`, the phone build a phone downloads from the control plane when it sets up its link (optional: without it `/phone/link` says the helper is missing and everything else works). `stageBundle`/`Copy-Bundle` branch on `goos` purely to write that README with one OS's commands (`./decurix` vs `.\decurix.exe`).

Keep it that way: any new runtime file dependency must be `go:embed`ed, not added to the bundle. If you ever have to put a real dependency in the archive, the "single executable is the whole product" claim in README.md needs to change with it.

See [README.md](README.md#-getting-started-running-it-locally) for full step-by-step setup instructions. Quick reference on env vars:

**Set by a human, in the control plane's own `.env`** (loaded via `godotenv` in `cmd/decurix/main.go`; see `internal/desktop/docker.go` for where each is read):

| Var | Read in | Default | Notes |
|---|---|---|---|
| `DEFAULT_PASSWORD` | `docker.buildDesktopEnv` | `admin` | Becomes `VNC_PW` in every new desktop container. |
| `OLLAMA_URL` | `ollama.DesktopURL` | `http://host.docker.internal:11434` | Passed into every new desktop by `buildDesktopEnv`, and the server `ollama.EnsureReady` preloads. `ollama.BundledURL` makes the control plane run and provision `decurix-ollama` itself. |
| `OLLAMA_MODEL` | `ollama.DesktopModel` | `auto` (`.env.example` sets it; `ollama.Model` until the first selection has run) | `auto` or unset: `ollama.SelectModel` chooses for this machine (see the model-selection gotcha). A model name pins it. `scripts/*/lib/ollama-setup.*` pull a pinned model when the server lacks it, and only fall back to a model the server already has if that pull fails. |
| `DESKTOP_STOP_TIMEOUT_SECONDS` | `desktop.Manager.Stop` | `10` | Parsed with `strconv.Atoi`; invalid/`<=0` values are ignored. |
| `DECURIX_DATA_DIR` | `sqlitedb.DataDir` | `os.UserConfigDir()/decurix` (`/data` in `docker/Dockerfile`) | Where `decurix.db` lives. |
| `DECURIX_STEP_IMAGES_MB` | `cmd/decurix.stepImagesMaxBytes` | `1024` | Byte budget of `stepimages`; oldest screenshots evicted first. |
| `DECURIX_LAN_PORT` | `cmd/decurix.lanListenAddr` | `8090` | Port of the second, `lanaccess`-gated listener; also what `docker-compose.yaml`'s `ports:` publishes (`${DECURIX_LAN_PORT:-8090}`, compose's own substitution from the same `.env`). `build.sh`/`rebuild.sh` point their primary `cloudflared` tunnel at `:8080` instead, but also open a second tunnel at this port — see "Two listeners, one process, one set of routes" above. |
| `DECURIX_ADMIN_ADDR` | `cmd/decurix.primaryListenAddrs` | loopback + Docker gateways (host), `:8080` (container) | Comma-separated `host:port` list the unauthenticated primary listener binds; the first is required. Widening it puts full control of the machine on that network. |
| `DECURIX_ADMIN_BIND` | `docker-compose.yaml` | `127.0.0.1` | Host interface compose publishes `:8080` on. Leave it on loopback. |
| `DECURIX_CONTROL_PLANE_URL` | `desktop.controlPlaneURL` | `http://host.docker.internal:8080` (compose sets `http://decurix:8080`) | The callback address baked into each new desktop's `CONTROL_PLANE_URL`. |
| `DECURIX_IDLE_STOP_MINUTES` | `cmd/decurix.idleStopThreshold` | `5` (on) | `idlepause.Checker.Threshold`, in minutes. Unset or invalid means the default 5; `0` (or any non-positive value) is the explicit opt-out, which the checker's own `Run` loop turns into "do nothing" rather than `main.go` needing separate enabled/disabled wiring. |
| `DECURIX_MAIL_POLL_SECONDS` | `cmd/decurix.mailPollInterval` | `60` | How often each connected mailbox is polled; at least 15. |
| `DECURIX_KEEP_AWAKE` | `keepawake.Enabled`, `scripts/*/lib/keep-awake.*` | on | `0` lets the machine idle-sleep even while mailboxes/schedules are active. The scripts read it from `.env` themselves; the container reads the `.env` copied in at image build. |

**Model selection tuning** (all optional, read by `internal/ollama`): `DECURIX_MODEL_AUTOPULL=1` lets auto-selection choose a model that is not installed on an external Ollama (the bundled container always may), `DECURIX_MODEL_CORE_GBPS` (default `1.5`) and `DECURIX_MODEL_MIN_TPS` (default `3`) correct the CPU speed estimate, and `DECURIX_MODEL_BUDGET_FRACTION` (default `0.5`, clamped to `[0,1]`) is how much of the free RAM/VRAM/cores a model may take - see the model-selection gotcha.

**Set automatically per-desktop** (baked into the container's env by `buildDesktopEnv` at creation time — not something you set in `.env`, listed here for wiring reference): `VNC_PW`, `VNC_RESOLUTION` (hardcoded `1280x720`), `DESKTOP_ID` (`sanitizeName(name)` — must stay equal to `Desktop.ID`, see the gotcha below), `DESKTOP_NAME`, `CONTROL_PLANE_URL` (`DECURIX_CONTROL_PLANE_URL` when set — `docker-compose.yaml` sets `http://decurix:8080` — otherwise `http://host.docker.internal:8080`), `OLLAMA_URL` and `OLLAMA_MODEL` (the control plane's own env when set, otherwise `http://host.docker.internal:11434` + `ollama.Model` (`qwen2.5-coder:3b`) — the Ollama on the host running the control plane, reached over the same `ExtraHosts` entry as `CONTROL_PLANE_URL`; that host's Ollama must listen on more than loopback, i.e. `OLLAMA_HOST=0.0.0.0`), `AGENT_HARNESS` (`ollama`/`claude`/`thebot`, from the harness chosen at creation).

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
| `OLLAMA_KEEP_ALIVE` | `ollama.KeepAlive` (Go), `ollama_client.py`, TheBot's `llm.NewOllama` | `60m` (`ollama.DefaultKeepAlive`) | How long Ollama holds the model in memory. See the prompt-cache gotcha below — this is the single biggest lever on how fast a desktop feels. |
| `EMBED_MODEL` | TheBot's `config.Load` / `llm.NewOllama` | `nomic-embed-text` | The embedding model, a separate dependency from the chat model. |

**Recordings**, also read inside the container:

| Var | Read in | Default | Purpose |
|---|---|---|---|
| `DECURIX_RECORDINGS_DIR` | `computer/recording.py` | `~/.decurix/recordings` | Where recordings live. |
| `DECURIX_RECORDINGS_MB` | `computer/recording.py` | `1024` | Disk budget; task and hand-run recordings are pruned oldest first past it, demonstrations never. |
| `DECURIX_REPLAY_FIND_SECONDS` | `computer/replay.py` | `12` | How long a replay step looks for its target before halting. |
| `DECURIX_REPLAY_EXPECT_SECONDS` | `computer/replay.py` | `10` | How long a replay step waits for its expected result. |
| `DECURIX_SKILLS_DIR` | `computer/skills.py` | `~/.decurix/skills` | Where taught skills live (one JSON file each; a skill carries its own steps, so it outlives the recordings it came from). |
