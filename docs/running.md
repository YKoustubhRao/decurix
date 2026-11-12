# Running it, the --cli client, distributable builds, env vars

```bash
./scripts/mac/setup.sh           # macOS host setup (Homebrew): Docker Desktop, cloudflared, Ollama, QEMU, jq, git
./scripts/mac/build-images.sh    # builds desktop images with --platform linux/amd64
./scripts/mac/build.sh           # "open the app": builds only missing images, starts compose, opens the
                                  # public link only with DECURIX_PUBLIC_LINK=1
./scripts/mac/rebuild.sh         # "reinstall": removes desktop containers/volumes, wipes decurix-data, rebuilds
./scripts/linux/setup.sh         # Linux host setup: Docker, cloudflared, QEMU (needs sudo)
./scripts/linux/build-images.sh  # builds decurix-desktop:basic and decurix-tools:latest
./scripts/linux/build.sh         # "open the app": builds only missing images, starts the control
                                  # plane if it isn't already running, waits for :8080, opens a
                                  # tunnel to the gated listener only with DECURIX_PUBLIC_LINK=1
                                  # (off by default). Leaves existing desktop_* containers/volumes and the
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
scripts\windows\build-electron.ps1  # what build_app.ps1 runs to build the installer (no .sh twin): the Linux control plane, the phone
                                  # build, decurix-model-selector.exe, the WSL distro's files, cloudflared.exe
./scripts/release/release.sh     # cross-compiles standalone executables + archives into dist/
docker network create decurix-network   # once, before compose by hand: compose takes it as external
docker compose up -d             # just the control plane (uses docker/Dockerfile, mounts /var/run/docker.sock)
go build ./... && go vet ./... && go test ./...   # compile, lint, every package's unit tests
(cd desktop/electron && npm test)  # the app's own tests (node --test, the files named in package.json)
PYTHONPATH=agent python3 -m unittest discover -s agent/tests   # computer-use gate/refs/rendering,
                                  # task IDs and usage totals, recordings (input merging,
                                  # privacy, volatility, the store, replay against a fake screen),
                                  # skills (generalising demos, loops, the questions and answers,
                                  # running a skill and asking when stuck, the gui/MCP tools and the
                                  # agent's /skills endpoints)
                                  # Tests that need the live-run fixtures (DECURIX_TEST_FIXTURES, outside the repo) or
                                  # the script sandbox skip where those are missing, and the run ends listing them.
                                  # DECURIX_REQUIRE_FIXTURES=1 / DECURIX_REQUIRE_SANDBOX=1 make such a skip a failure:
                                  # use the first before a release (CI cannot - the fixtures stay out of it); CI sets the second.
node --test web/templates.test.js   # the dashboard pages' scripts parse; the shared chrome's polling and access-code menu
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
python3 bench/resources/run.py [--eval-run <id> --arm <name>] [--label ...]   # what a desktop, gui, a dashboard tab
                                                  # and a task cost: idle RAM/CPU by process group (Chrome or not,
                                                  # running and paused), gui p50/p95 and CPU, the dashboard's req/s and
                                                  # bytes/s (a scripted viewer on the page's own timers), host RAM with
                                                  # 0 and 1-2 desktops, input tokens per task and the cached share. One
                                                  # throwaway desktop, ~10 min; rows in bench/resources/results/. From
                                                  # Windows: --cp http://127.0.0.1:18080 --host-shell "wsl -d Decurix -u root --"
python3 bench/resources/run.py --only density [--max 20] [--floor 1024] [--browser] [--settle 20]   # how many desktops
                                                  # fit (L7, docs/lightweight.md): made one at a time through the control
                                                  # plane until the docker host's available memory (and, on Windows, the
                                                  # host's own) would fall under --floor MB or --max are up; each one's
                                                  # create-to-ready and to-agent time, memory after --settle s (Chrome
                                                  # open in each with --browser), and wake from paused; then the count,
                                                  # why it stopped, and every one deleted. Never part of the default run
./scripts/linux/pin-digests.sh                    # (or scripts\windows\pin-digests.ps1) prints every image still taken by
                                                  # tag alone - each Dockerfile FROM, the bundled Ollama, the host hop's
                                                  # alpine - as the line to paste with its digest (B-24); changes no file.
                                                  # Needs Docker and the registry; exits 1 when a digest could not be read
sh bench/security/boundary.sh [--image decurix-desktop:basic] [--layer]   # the approval gate's boundary on an image:
                                                  # every self-approval attack of the security reviews (root on restart,
                                                  # the key in an environment, forged and misbound replies). Run on every
                                                  # new image; --layer puts this checkout's entrypoint and agent on top
                                                  # first. Linux Docker as root (the Decurix distro: wsl -d Decurix -u root
                                                  # -- sh /mnt/c/.../bench/security/boundary.sh --layer); one throwaway
                                                  # desktop, ~2 min. Its network row needs the control plane (--cp,
                                                  # default http://127.0.0.1:8080): it makes two desktops there, probes
                                                  # B's 8081/5901/6901 from inside A on their own networks, and deletes
                                                  # them (--desktops A,B uses two that exist instead). Its screen rows
                                                  # probe the input broker's paths read-only (XTEST, the VNC password,
                                                  # DevTools, the cookie store, the screen side's /proc environ):
                                                  # --broker makes the desktop in broker mode, where each must PASS;
                                                  # without it they say OPEN (open by design). --screen-only stops
                                                  # after them (no restarts, no network row, no control plane)
DECURIX_PRACTICE_IMAGE=decurix-desktop:basic go test ./internal/desktop -run TestPracticeResetLive -count=1 -v
                                                  # the practice reset on a throwaway container of the real image:
                                                  # planted state is gone, the gate key kept, /health 200 after. Run
                                                  # after every desktop image rebuild, in the Decurix distro (a golang
                                                  # container with the Docker socket). Without the variable it skips
                                                  # silently, so a plain go test proves nothing about the reset
```

`scripts/` is split by who runs it: `mac/` (`setup.sh`, `build-images.sh`, `build.sh`, `rebuild.sh`, `build_app.sh`, `quickstart.sh`, `test-vm.sh`, with `lib/`: `ollama-setup.sh`, `keep-awake.sh`, `host-mounts.sh`), `linux/` (the same, with `lib/`: `ollama-setup.sh`, `keep-awake.sh`, `lan-tunnel.sh`, `lan-address.sh`, `build-resources.sh` + `buildkitd.toml`), `windows/` (the same as `.ps1`, plus `build-electron.ps1`, with `lib/`: `Ollama-Setup.ps1`, `Keep-Awake.ps1`, `Lan-Tunnel.ps1`), `release/` (`release.sh` + `release.ps1`), and `testing/` (`dummy_control_plane.py`, the stub `test-vm.sh` pairs with; `control_plane_smoke.py`; the `ollama-env` tests; the `lan-tunnel` tests of the tunnel's restart loop). Each `.sh` has a `.ps1` twin at the same position in the other directory — they are hand-maintained, not generated from each other, so a change to one needs the same change to the other. The `lib/` files are sourced by `build.*` and `rebuild.*` rather than duplicated (the "find an Ollama the desktops can reach" logic is ~300-400 lines each); the Mac scripts source `linux/lib/lan-tunnel.sh` too. Every script resolves the repo root from its own location, never from `$PWD`.

### The `--cli` terminal client

`decurix --cli` (`cmd/decurix/cli.go`) is a text alternative to the dashboard/chat page: an interactive REPL (`list`/`create`/`start`/`pause`/`unpause`/`stop`/`restart`/`delete`/`chat`/`queries`) that talks to a control plane already running somewhere, purely over `internal/httpapi`'s existing JSON endpoints — no Docker client, no database, nothing the server-mode branch of `main()` sets up. `main()` checks `-cli` immediately after `-version` and before any of that setup, same posture as `-build-images` short-circuiting startup. `chat <id>` polls `GET /api/desktops/chat` (the same snapshot the chat page's polling fallback uses) once a second, tracking how many `Thread.Entries`/`Thread.Steps` it has already printed so a repeated poll only shows what's new, and returns to the prompt once the turn stops `Running` with at least one new entry; Ctrl+C during that wait returns to the prompt without cancelling the task (it keeps running on the desktop) by trapping `os.Interrupt` only for the duration of that wait, never at the top-level prompt.

`build.sh --cli` / `rebuild.ps1 -Cli` (etc.) run their normal flow through "Decurix is running", then run `docker compose exec desktop-manager ./decurix --cli` instead of opening the Cloudflare tunnel — the same binary the container already runs, reached at its own `127.0.0.1:8080` (also `--cli`'s default `-url`, which is why it works unchanged if someone runs the released binary directly against the compose-published port instead). Both scripts clear their INT/TERM trap right before that hand-off: `build.sh`'s otherwise prints a "tunnel stopped" message that would not make sense here, and `rebuild.sh`'s otherwise tears the whole stack back down on Ctrl+C — correct for an interrupted *rebuild*, wrong once the rebuild has already finished and someone is just leaving the CLI.

### The binary's other flags

Each runs instead of the server and exits. `--build-images` builds the images from the embedded context (`--rebuild` rebuilds existing ones; `--images desktop|tools|all`, default `all`). `--phone "<pairing link>"` (`--phone-name`, `--phone-serial`) runs the phone link in Termux (`internal/phoneagent`); with no link it reconnects. `--phone-setup BASE TOKEN` is what the phone link page's setup script runs. `--restore-backup <file>` puts a backup's database back into the data dir; it moves every file it replaces into `before-restore-<time>/` and refuses while a control plane answers at `--url` unless `--force`. `--purge-secret --reason "why"` reads a leaked value from stdin (never argv), posts it to `POST /api/privacy/purge` on the control plane at `--url`, and prints the counts per store and the judgement id; the control plane must be running. The same purge is at the foot of the Tasks page ("Purge a leaked secret"), primary listener only, owner only; values under 6 characters are refused (docs/trajectory.md).

Schedules and DAG crons need no zoneinfo on the host (`time/tzdata` is embedded). `TZ`, or where `/etc/localtime` points, sets the zone of a schedule created without one; otherwise it is UTC.

### Distributable builds

`scripts/release/release.sh` / `release.ps1` first run `go generate .` (TheBot's sources packed for `go:embed`; without it a released binary cannot build the desktop images), then cross-compile `cmd/decurix` for linux, windows and darwin × amd64/arm64, and android/arm64, with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<version>"`, writing to `dist/`: a bare binary per target, a per-target archive (`.zip` for windows, `.tar.gz` otherwise), and `SHA256SUMS`. Version defaults to `git describe --tags --always --dirty`, and `main.version` backs `decurix --version`.

The binary is fully self-contained: static (`CGO_ENABLED=0`, no libc — which is why the SQLite driver is modernc.org/sqlite, not mattn/go-sqlite3), web templates from `web/embed.go`, desktop build context from `embed.go`, and `.env` optional. What it *writes* goes in `sqlitedb.DataDir()`: `decurix.db`, the key files (`hostgui.key`, `mail.key`, `llm.key`, `cloudflare.key`; a `claude-session.key` from before 2026-11-07 is no longer read), `instance-id` and the blob store (`blobs/`) (`DECURIX_DATA_DIR`, else `os.UserConfigDir()/decurix`; the Docker image sets `/data`, which `docker-compose.yaml` mounts as the `decurix-data` volume). So an archive carries **nothing the binary depends on** — a generated `README.txt`, an `.env.example`, and `decurix-android-arm64`, the phone build a phone downloads from the control plane when it sets up its link (optional: without it `/phone/link` says the helper is missing and everything else works). `stageBundle`/`Copy-Bundle` branch on `goos` purely to write that README with one OS's commands (`./decurix` vs `.\decurix.exe`); the android archive gets a Termux README and no `.env.example`.

Keep it that way: any new runtime file dependency must be `go:embed`ed, not added to the bundle. If you ever have to put a real dependency in the archive, the "single executable is the whole product" claim in README.md needs to change with it.

See [README.md](README.md#-getting-started-running-it-locally) for full step-by-step setup instructions. Quick reference on env vars:

**Set by a human, in the control plane's own `.env`** (loaded via `godotenv` in `cmd/decurix/main.go`, from the working directory or beside the executable; compose hands it to the container as `env_file`). `DEFAULT_PASSWORD` is no longer read: every desktop gets its own screen password.

| Var | Read in | Default | Notes |
|---|---|---|---|
| `OLLAMA_URL` | `ollama.DesktopURL` | `http://host.docker.internal:11434` | The model server the control plane uses, warms and forwards the desktops' `/api/native-ollama` proxy to (a desktop's own `OLLAMA_URL` is that proxy). `ollama.BundledURL` (`http://decurix-ollama:11434`) makes the control plane run and provision `decurix-ollama` itself. |
| `OLLAMA_MODEL` | `ollama.DesktopModel` | `auto` (`.env.example` sets it; `ollama.Model` until the first selection has run) | `auto` or unset: `ollama.SelectModel` chooses for this machine (see the model-selection gotcha). A model name pins it. `scripts/*/lib/ollama-setup.*` pull a pinned model when the server lacks it, and only fall back to a model the server already has if that pull fails. |
| `DESKTOP_STOP_TIMEOUT_SECONDS` | `desktop.Manager.Stop` | `10` | Parsed with `strconv.Atoi`; invalid/`<=0` values are ignored. |
| `DECURIX_DATA_DIR` | `sqlitedb.DataDir` | `os.UserConfigDir()/decurix` (`/data` in `docker/Dockerfile`) | Where `decurix.db` lives. |
| `DECURIX_STEP_IMAGES_MB` | `cmd/decurix.stepImagesMaxBytes` | `1024` | Byte budget of `stepimages`; oldest screenshots evicted first. |
| `DECURIX_LAN_PORT` | `cmd/decurix.lanListenAddr` | `8090` | Port of the second, `lanaccess`-gated listener; also what `docker-compose.yaml`'s `ports:` publishes (`${DECURIX_LAN_PORT:-8090}`, compose's own substitution from the same `.env`). With `DECURIX_PUBLIC_LINK=1`, `build.sh`/`rebuild.sh` open one Cloudflare tunnel, to this port only; nothing tunnels `:8080` (architecture.md, "Two listeners, one process, one set of routes"). |
| `DECURIX_LAN_ADDR` | `cmd/decurix.lanListenAddr` | unset | `host:port` the gated listener of a host-run binary binds. Not `host:port` is ignored with a warning. |
| `DECURIX_PUBLIC_LINK` | `scripts/*/lib/lan-tunnel.*` | off | `1` (env or `.env`) turns the build/rebuild scripts' public tunnel on. It only ever points at a port serving the access-code page. `cloudflared` is needed only then. The Electron app has its own switch (Mobile menu, "Public link"). |
| `DECURIX_IDLE_ACTION` | `cmd/decurix` | pause | `stop` stops idle desktops; anything else pauses them. |
| `DECURIX_ENABLE_KSM` | `internal/desktop/ksm_linux.go`, the scripts | off | `1` turns KSM on. It merges memory pages across desktops that do not trust each other, so it is the owner's choice. At start KSM is turned off only when Decurix's own marker (`<data dir>/ksm-enabled-by-decurix`) says Decurix turned it on, or with `DECURIX_DISABLE_KSM=1` / `DECURIX_ENABLE_KSM=0`. KSM an owner enabled is left on, with a log line. |
| `DECURIX_IMPORT_HOST_BROWSER_PROFILE` | `internal/winhost` | off | `1` lets the Windows bridge seed a host app's private browser from the person's own profile. Without it, a private profile with an old import marker is deleted once before launch. |
| `DECURIX_ADMIN_ADDR` | `cmd/decurix.primaryListenAddrs` | loopback (host; on Linux also the Docker gateways, once Docker answers), `:8080` (container) | Comma-separated `host:port` list the unauthenticated primary listener binds; the first is required. Widening it puts full control of the machine on that network. |
| `DECURIX_ADMIN_BIND` | `docker-compose.yaml` | `127.0.0.1` | Host interface compose publishes `:8080` on. Leave it on loopback. |
| `DECURIX_CONTROL_PLANE_URL` | `desktop.controlPlaneURL` | `http://host.docker.internal:8080` (compose sets `http://decurix:8080`) | The callback address baked into each new desktop's `CONTROL_PLANE_URL`. |
| `DECURIX_IDLE_STOP_MINUTES` | `cmd/decurix.idleStopThreshold` | `5` (on) | `idlepause.Checker.Threshold`, in minutes. Unset or invalid means the default 5; `0` (or any non-positive value) is the explicit opt-out, which the checker's own `Run` loop turns into "do nothing" rather than `main.go` needing separate enabled/disabled wiring. |
| `DECURIX_IDLE_STOP_AFTER_MINUTES` | `cmd/decurix.idleStopAfter` | `30` | `idlepause.Checker.StopAfter`: a desktop left paused this long is stopped to give its memory back (its home stays; it starts again by itself when given work, and its card shows `rest_reason`). `0` keeps paused desktops paused. Off with idle-pause itself (`DECURIX_IDLE_STOP_MINUTES=0`). Separately, every start makes room first: when the Docker host's MemAvailable is below the desktop's need (its limit or 700 MB, the smaller) plus 512 MB, the longest-idle paused desktops are stopped, and a start that cannot fit is refused ("not enough memory: N desktops are busy"). |
| `DECURIX_EVAL_MIN_FREE_MB` | `cmd/decurix.evalMinFreeBytes` | unset (2.5 GB) | The free memory an eval run needs before it starts (`eval.Service.MinFreeBytes`). At least 1024 (one practice desktop's working set); unset, invalid or lower keeps the 2.5 GB default. For a small host shared with another workload. |
| `DECURIX_MAIL_POLL_SECONDS` | `cmd/decurix.mailPollInterval` | `60` | How often each connected mailbox is polled; at least 15. |
| `DECURIX_KEEP_AWAKE` | `keepawake.Enabled`, `scripts/*/lib/keep-awake.*` | on | `0` (or `false`/`off`/`no`) lets the machine idle-sleep even while mailboxes/schedules are active. The scripts read it from `.env` themselves; the container gets `.env` from compose's `env_file` at start (it is never copied into an image). |
| `DECURIX_LAN_BIND` | `docker-compose.yaml` | `0.0.0.0` | Host interface compose publishes the gated port on. The Windows app sets `127.0.0.1`. |
| `DECURIX_ALLOWED_HOSTS` | `httpapi.knownHost` | unset | Comma-separated extra Host names the primary listener answers (anything else not an IP, `localhost`, `host.docker.internal` or a single label gets 421). |
| `DECURIX_DEFAULT_DESKTOP_CPUS` / `DECURIX_DEFAULT_DESKTOP_MEMORY_MB` | `httpapi.createDesktopHandler` | `1` / `2048` | A new desktop's size when the create form names none. Only 512 MB of it is reserved; the rest is a ceiling. |
| `DECURIX_DESKTOP_SHM_MB` | `desktop.buildContainerConfigs` | `512` | `/dev/shm` of a new desktop. |
| `DECURIX_DESKTOP_SUBNETS` | `desktop.desktopSubnetRange` | `10.213.0.0/16` | The IPv4 range desktop networks (a /28 each) are carved from. On the Windows app it must lie inside the fence's pool. |
| `DECURIX_HOST_HOME` | `desktop` (host apps), `docker-compose.yaml` | unset; compose mounts the host's `${DECURIX_HOST_HOME:-/home}` read-only at `/host-home` and sets it to that | Where the control plane looks for host users' homes, for host-app discovery. |
| `DECURIX_HOST_KIND` | `desktop.HostFence` | empty | `wsl` (set by the Windows app): no desktop is made or started unless `/tmp/decurix-wsl/fence` says the fence holds. |
| `DECURIX_VNC_BRIDGE` | `httpapi.screenBridgeOn` | on | `0`/`off` sends screen streams to the desktop's websockify again instead of the control plane's own RFB bridge. |
| `DECURIX_SCREEN_BROKER` | `desktop.screenBrokerOn` | on (since 2026-11-08, owner decision O-1) | Every desktop created while it is on gets `DECURIX_SCREEN_USER=1`: its screen is the `decurixscreen` user's and `gui` reaches it only through the screen broker (gate-boundary.md, "Next: the input broker"). `0` (or `false`/`off`/`no`) creates desktops in the default mode instead (the worker's user owns the screen); unset, empty or anything else is on. The dashboard's create dialog has no control for it. A desktop keeps what it was created with: changing it re-creates nothing, and a desktop made before 2026-11-08 stays in the default mode until it is recreated. The entrypoint then runs the X server (Ubuntu's `Xtigervnc`, which has the SECURITY extension), the window manager, the wallpaper, the window list on the bottom bar (`computer/windowlist.py`: lists and raises windows, starts nothing) and the broker as that user, the panel (without its own window list, `agent/decurix-panel-broker.xml`) and the agent as the worker with an untrusted X cookie, and the agent's own screen calls go through the broker too (`computer/screen_call.py`). The worker's state stays the worker's: what a screen command must know of it comes with each command and what it writes of it (recordings, evidence) is handed back for the worker to write; files a command names are handed over as descriptors; the worker's store commands (`skills`, `do`, `replay`, `record`, ...) run as the worker. The broker reads the worker's apps' widgets on the worker's accessibility bus, and the browser launcher run as the worker hands its pages to the broker. What a person loses in this mode: the desktop's icons. What stays open: the agent token is readable by the worker (gate-boundary.md, "Broker mode is the default"). |
| `DECURIX_MODEL_MAX_CTX` | `httpapi.maxModelCtx` | `32768` | The one context size above 8192 a desktop may ask the model proxy for. |
| `DECURIX_RESOURCE_CPU_THRESHOLD` / `DECURIX_RESOURCE_MEM_THRESHOLD` | `resourcemon.NewMonitor` | `80` | Percent of a desktop's CPU / memory that raises a resource alert. |
| `DECURIX_LLM_REQUIRE_PURPOSE` | `cmd/decurix` | on | Hosted-model calls that carry no purpose header are refused; `0` lets them through (only for desktops made before every call was tagged). On since the live round of 2026-11-08: 58 calls, 0 untagged. |
| `DECURIX_DATE_ORDER` | `cmd/decurix.dateOrderFromEnv` | unset | `day` or `month`: the organisation's date order; unset lets each desktop decide from what it reads. |
| `DECURIX_SHUTDOWN_DRAIN` | `httpapi.DrainBudget`, read by `cmd/decurix` at shutdown | `90s` | How long a stopping control plane lets requests in flight finish (a Go duration or seconds; `0` drains nothing). Keep it under the compose files' `stop_grace_period: 100s`, or Docker kills the process mid-drain. See "Replacing the control plane" below. |
| `DECURIX_RUN_BUNDLE_MB` | `runs.BundleLimit`, read once by `cmd/decurix` | `64` | MB of one run's recording (frames, observations, program) a pulled bundle keeps; past it frames go first, oldest first, each named in `integrity.missing`. The labels (run folder, ledger, notes, trace) are never cut for it. Whole numbers 8 to 1024; outside that the nearest bound is used, anything else the default, and the start log says so. Applies to every puller (desktops, Simulate, evals). [trajectory.md](trajectory.md), "The bundle". |
| `DECURIX_PRACTICE_MAX_TOKENS` | `practice.MaxTokensFromEnv` | `2000000` | Token cap of one Simulate run (docs/simulation.md). |
| `DECURIX_GROUNDER_MODEL` (+ `_URL`, `_API`, `_KEY`, `_COORDS`, `_MAX_SIDE`, `_TIMEOUT_SECONDS`, `_KEEP_ALIVE`) | `grounder.FromEnv` | unset (off) | A GUI-grounding model for `gui click --see` (`/api/grounder`). |
| `DECURIX_PHONE_BINARY` | `httpapi.phoneBinaryPath` | `decurix-android-arm64` beside the binary | The phone build `/phone/decurix` hands out. |
| `DECURIX_TUNNEL_PROVIDER` | `desktop/electron/mobile-access.js` | `cloudflare` | The app's public-link tunnel: `cloudflare`, `localhost-run`, or `auto` (switch on failure). |
| `DECURIX_LINK_SERVICE` | `desktop/electron/public-name.js` | `https://links.decurix.computer` | Where the app gets its permanent public name ([cloud/links](../cloud/links/README.md)). `off` uses a quick tunnel every time; any other value must be an `https://` address. |
| `DECURIX_WSL_LAN_FORWARD` | `scripts/linux/lib/lan-address.sh` | on | `0` skips forwarding Windows' LAN address into WSL for the LAN link. |
| `OLLAMA_NUM_THREAD`, `OLLAMA_KV_CACHE_TYPE`, `OLLAMA_FLASH_ATTENTION`, `OLLAMA_NUM_BATCH`, `OLLAMA_NUM_PARALLEL`, `OLLAMA_MAX_LOADED_MODELS` | `internal/ollama`, `buildDesktopEnv` | CPUs-2 above 4 CPUs (CPUs-1 for 3-4, else 1), `q8_0`, `1`, unset, `1`, `1` | Model-server tuning; all but the last are also copied into every new desktop. |

`DECURIX_WINDOWS_OLLAMA_URL` and `DECURIX_WINDOWS_SELECTOR_URL` are written by the Windows app (`windows-runtime.js`) when the model server runs on Windows itself; they are not for hand-setting.

**Model selection tuning** (all optional, read by `internal/ollama`): `DECURIX_MODEL_AUTOPULL=1` lets auto-selection choose a model that is not installed on an external Ollama (the bundled container always may), `DECURIX_MODEL_CORE_GBPS` (default `1.5`) and `DECURIX_MODEL_MIN_TPS` (default `3`) correct the CPU speed estimate, and `DECURIX_MODEL_BUDGET_FRACTION` (default `0.5`, clamped to `[0,1]`) is how much of the free RAM/VRAM/cores a model may take - see the model-selection gotcha.

**Shared folder** (`internal/desktop/sharedfolder.go`): `DECURIX_HOST_MNT_C` is where the control plane sees the host's C: drive (compose sets `/mnt/c` read-only); `DECURIX_HOST_MNT_C_SOURCE` (default `/mnt/c`) is where the *Docker daemon* sees it, for the bind of a desktop's shared folder. Create one with `POST /api/desktops/create` and `shared_folder=C:\Users\you\Documents\Shared`; see architecture.md, "The shared folder".

**Set automatically per-desktop** (baked into the container's env at creation time by `buildDesktopEnv`, plus the per-desktop secrets from `agenttoken.go` — not something you set in `.env`, listed here for wiring reference): `VNC_PW` (the desktop's own 8-character screen password, `screenPassword`), `DECURIX_AGENT_TOKEN`, `DECURIX_GATE_REQUIRED=1`, `VNC_RESOLUTION` (hardcoded `1280x720`), `DESKTOP_ID` (`sanitizeName(name)` — must stay equal to `Desktop.ID`, see the gotchas), `DESKTOP_NAME`, `CONTROL_PLANE_URL` (`DECURIX_CONTROL_PLANE_URL` when set — `docker-compose.yaml` sets `http://decurix:8080` — otherwise `http://host.docker.internal:8080`), `OLLAMA_URL` (`desktop.DesktopModelURL`: `<CONTROL_PLANE_URL>/api/native-ollama`, with `/v1` when the model server is OpenAI-compatible; a desktop never reaches the model server's own API), `OLLAMA_MODEL` (`ollama.DesktopModel`: `OLLAMA_MODEL` when it names a model, else the auto-selected one, else `ollama.Model`, `devstral-small-2:24b`; soul's `CHAT_MODEL`, `start_agent_terminal.sh` and the agent's `config.py` fall back to the same model), `AGENT_HARNESS` (always `thebot`; a desktop made with a removed harness keeps its old value and runs TheBot; what the person chooses is its brain, `DECURIX_BRAIN`, see harnesses.md), the hosted-brain settings when the brain is hosted (`LLM_PROVIDER=openai`, `OPENAI_BASE_URL=<control plane>/api/llm/<brain>`, a placeholder key, `DECURIX_LLM`, `DECURIX_BRAIN_MODEL`, `DECURIX_BRAIN_VISION`), `DESKTOP_SPECIALIZATION` / `DESKTOP_PARENT_ID` when given, `DECURIX_SCREEN_USER=1` unless the control plane runs with `DECURIX_SCREEN_BROKER=0`, `DECURIX_SHARED_FOLDER` / `DECURIX_SHARED_MOUNT` with a shared folder, and the tool paths (`PATH`, `GOROOT`, caches under `/var/cache/decurix`, `DECURIX_SHARED_MODELS`).

**Ollama pacing**, read inside the container rather than by `buildDesktopEnv` — set them on the container if the defaults are wrong for your hardware. The defaults assume the worst case this actually runs on, a CPU-only host:

| Var | Read in | Default | Purpose |
|---|---|---|---|
| `OLLAMA_TIMEOUT_SECONDS` | `ollama_client.py` | `300` | How long the agent's own one-off question to the local model (`ask_once`) waits for one `/api/chat`. |
| `OLLAMA_TIMEOUT_MS` | TheBot's `llm.NewOllama` | `300000` | A *floor*, not a replacement: raises any per-call `Options.TimeoutMs` shorter than it, and leaves longer ones alone. TheBot's own budgets (60s to plan, 90s to answer) were chosen against a GPU and all expire in sequence on a CPU. |
| `OLLAMA_KEEP_ALIVE` | `ollama.KeepAlive` (Go), `ollama_client.py`, TheBot's `llm.NewOllama` | `60m` (`ollama.DefaultKeepAlive`) | How long Ollama holds the model in memory. See the prompt-cache gotcha (gotchas.md) — this is the single biggest lever on how fast a desktop feels. |
| `EMBED_MODEL` | TheBot's `config.Load` / `llm.NewOllama` | `nomic-embed-text` | The embedding model, a separate dependency from the chat model. |

`docker/Dockerfile` takes the build arg `DECURIX_VERSION` (default `dev`).

**Recordings**, also read inside the container:

| Var | Read in | Default | Purpose |
|---|---|---|---|
| `DECURIX_RECORDINGS_DIR` | `computer/recording.py` | `~/.decurix/recordings` | Where recordings live. |
| `DECURIX_RECORDINGS_MB` | `computer/recording.py` | `1024` | Disk budget; task and hand-run recordings are pruned oldest first past it, demonstrations never. |
| `DECURIX_RECORDING_LOCK_S` | `computer/recording.py` | `30` | How long a write waits for a recording's lock. Then it raises `TimeoutError` (an `OSError`) and the action is logged, never blocked forever. |
| `DECURIX_REPLAY_FIND_SECONDS` | `computer/replay.py` | `12` | How long a replay step looks for its target before halting. |
| `DECURIX_REPLAY_EXPECT_SECONDS` | `computer/replay.py` | `10` | How long a replay step waits for its expected result. |
| `DECURIX_SKILLS_DIR` | `computer/skills.py` | `~/.decurix/skills` | Where taught skills live (one JSON file each; a skill carries its own steps, so it outlives the recordings it came from). |

## Since the review of 2026-11-03

**Replacing the control plane: /healthz, the drain, the version floor** (2026-11-11). An app update recreates the control plane's container while the desktops keep running. *Health:* `GET /healthz`, primary listener only (`RegisterLocalRoutes`; the gated listener does not serve it), answers `{service: "decurix-control-plane", version, pid, started_at, instance, draining}` from memory (no database, no Docker), so a supervisor knows which binary answered. *Drain* (`internal/httpapi/healthz.go`, `httpapi.Life`): on SIGTERM or Ctrl+C the listeners stay open, every new request except `/healthz` gets 503 with `Retry-After` (what is left of the drain, 1-30 s) and `Connection: close`, and `/healthz` says `draining: true`. Long-lived streams are ended at once and never waited for: any request with an `Upgrade` header (the screen's WebSocket, host and phone app streams, phone links) and any event stream (`Accept: text/event-stream`, or a path ending `/stream`, such as the chat's) gets a context that ends when the drain starts; their clients reconnect to the next control plane. Every other request in flight (a model call through `/api/llm/`, a desktop being created) may finish within `DECURIX_SHUTDOWN_DRAIN` (90 s). After that the rest are cancelled (the listeners' `BaseContext`), the listeners close (5 s more), and only then does `main` return and close the database and release the data folder's lock, in that order. A second Ctrl+C quits at once. Both compose files set `stop_grace_period: 100s` so Docker waits for it. A desktop's model client retries the 503 three times after `Retry-After`, then a refused connection for about a minute (TheBot `internal/llm/retry.go`), which spans a full drain and the new start. *The app's supervision* (`desktop/electron/stack-health.js`, `main.js`): it asks `/healthz` every 5 s, falling back to the favicon's marker for a stack from before `/healthz`, and logs which version and pid answer to `desktop.log`. A control plane that drains is being replaced and is never restarted over; it and its successor get 2 minutes before missed answers count. One that stops answering three times is started again after 1 s, doubling to 30 s, at most 6 times in a sliding 5 minutes, then left stopped with Retry. *Version floor* (`desktop/electron/version-floor.js`): at start the app reads `https://decurix.computer/desktop-version-floor.json` (`website/desktop-version-floor.json`, deployed with the site: `{min, latest, downloadUrl}`) with a 10 s timeout. A build below `min` starts nothing: a dialog offers Download (opens `downloadUrl`) or Quit, and the app quits either way. A build below `latest` gets a notice it can dismiss, once per `latest` (`version-notice.json` in the data folder). `downloadUrl` is used only when it starts with `https://decurix.computer/` or `https://github.com/YKoustubhRao/`; anything else is replaced by the site. Any failure (offline, timeout, not JSON, an unreadable version) lets the app start. To retire a broken build, raise `min` in that file and redeploy the site; bump `latest` with each release.

**Start-up without Docker.** The control plane no longer exits when Docker is down. The listeners start, every page shows a "Docker is not running" banner, and the Docker-dependent start-up (`cmd/decurix/dockerstartup.go`) runs once Docker answers (`Manager.WatchDocker`, every 10 s; `Manager.DockerReachable()`).

**The app's permanent public name.** Since 2026-11-11 (owner's decision: anyone who installs, names chosen automatically, served by a Cloudflare Worker) the app's public link is a permanent name such as `calm-river-4821.decurix.computer`. The link service ([cloud/links](../cloud/links/README.md)) makes each install its own tunnel, routed only to the gated port, and a key the app keeps in `public-name.json` in its data folder; on every start the app trades the key for the connector token and runs its shipped cloudflared with the token in the environment, never on the command line. A start where the service cannot be reached or refuses uses a quick tunnel, as before. A name not used for 90 days is removed; an abused one is revoked with the service's admin key. The build/rebuild scripts still use quick tunnels.

**The public link and the phone.** The public link (a Cloudflare quick tunnel to the gated listener, or the app's permanent name above) is off by default (owner decision O-5). In the app it starts only after the person turns on "Public link" at the top of the dashboard's Mobile menu; the choice is kept in `public-link.json` in the data folder. The scripts start it only with `DECURIX_PUBLIC_LINK=1`, and start it again if it exits (`Watch-LanTunnel` / `supervise_lan_tunnel`: 1 s doubling to 60 s, back to 1 s after 10 minutes up, each exit logged with its code; the switch is read again before each restart and an off switch ends the loop; a restarted tunnel has a new address, reported again). The LAN link is unchanged. Phone setup uses Wireless debugging only; it never runs `adb tcpip 5555` (unencrypted, on every network, until reboot). Wireless debugging's port can change with the network, and setup keeps the new serial. If the phone cannot hear mDNS, a Wi-Fi change needs `adb connect localhost:<new port>` again (a live check on a real phone is still owed). The setup one-liner checks the script's SHA-256 (sent with the setup token) before running it, and the script checks the helper binary's. Over plain http this does not protect the page itself, and the page says so.

**What the Windows installer does to Windows.** Since 2026-11-11 the installer, not the app's first start, turns on what WSL 2 needs, on Windows 10 (2004 and newer) and 11, Home included: the `Microsoft-Windows-Subsystem-Linux` and `VirtualMachinePlatform` features (DISM), WSL itself (the Microsoft-signed MSI from GitHub's latest WSL release, `wsl --update --web-download` if that fails), and `hypervisorlaunchtype auto` when the boot settings keep the hypervisor off (BitLocker paused for that one restart first, so it does not ask for the recovery key). It is `resources\wsl\prepare-windows.ps1 -Action ensure`, run from `installer.nsh` (`customInstall`) as 64-bit PowerShell; it looks without an administrator first, so a ready PC sees no prompt, and an update never runs it. Its exit code is 100 plus bits (1 restart, 2 firmware, 4 failed, 8 declined, 16 unsupported), so PowerShell's own exit 1 for a script a policy blocked reads as a failure, not a restart. A restart it needs sets the finish page's "Reboot now" and is noted in `restart-asked.json` (reason `installer`), so an app opened before that restart says to restart instead of prompting again. Virtualization switched off in the BIOS/UEFI cannot be changed from Windows: the installer says where to turn it on and can restart into the UEFI settings. `-Action facts` prints what it sees on a PC, changing nothing; the log is `%LOCALAPPDATA%\Decurix\logs\prepare-windows.log`. The app's own first-start path (installWsl, importDistro) remains for what the installer could not do.

**The Windows app on a shared WSL runtime.** Adopting an existing distro's Docker Engine is now asked; the default is the app's own Linux. What the engine already held is recorded as `preexisting` in `windows-runtime.json`, and uninstall or reset remove only the rest (`wsl/maintain-existing.sh`, `cleanup-existing.ps1`). `maintain-existing.sh inventory` fails (prints nothing, exit 1) when any one Docker listing fails, so a partial list is never saved; the app then does not adopt the runtime. A complete record has `preexistingComplete: true`. An uninstall or `-Fresh` removes nothing from a shared runtime whose record lacks it (exit 3, and the uninstaller says so); records saved by earlier versions included. Reset and delete report a failed listing as "could not ...", never as nothing there.

**Ollama setup.** No script sets `OLLAMA_HOST=0.0.0.0` any more. On Mac and Windows Ollama stays on loopback (Docker Desktop reaches it). On native Linux it binds the address that `host.docker.internal` resolves to inside a container (Docker's host-gateway); since 2026-11-05 only the control plane calls it, and desktops go through its `/api/native-ollama` proxy. A fallback server or model is used for **one run only**: it goes in a compose override file with a "THIS RUN ONLY" warning, and `.env` keeps the person's choice. The bundled `ollama/ollama` image is pinned to 0.40.1.

**Images and tools.** A failing install in the tools image fails the build. The tools volume's sentinel is a hash of the tools tree, so a rebuilt tools image replaces an old volume's contents. A refresh waits while any desktop is running, paused or restarting (architecture.md, "The tools volume"). Rebuild the desktop and tools images, then **recreate desktops**, to pick up this review's container changes (no-new-privileges, the setuid strip, per-desktop caches, `-nolisten tcp`, no `xhost +`, new host session ids).

**Upgrading to the random instance tag.** The instance tag is now a random `instance-id` file in the data folder (architecture.md, "The instance tag"). On the first start after upgrading, this instance's own home-copy helpers and hops tagged with the old path hash look foreign and are left running, once. When no other control plane runs, remove them by hand: `docker ps -a --filter label=decurix.hop.instance` and `--filter label=decurix.homefs.instance` list them.

**Python tests without the office libraries.** Office tests skip with their reason when openpyxl, python-docx or lxml is missing (`agent/tests/needs.py`: `@needs`, `require` for an office-only module, `optional`). On WSL's system `python3` the suite runs with skips, not failures; the Windows `py -3` (3.11) runs everything. The replay gate's in-repo corpus always runs; run once with `DECURIX_REQUIRE_FIXTURES=1` on the machine that has the private corpus.

**What the owner runs after a build.** The live checks the writers could not run are listed in [follow-ups.md](follow-ups.md), "Live checks after the next build". After a desktop image rebuild, run `DECURIX_PRACTICE_IMAGE=decurix-desktop:basic go test ./internal/desktop -run TestPracticeResetLive` (with `-count=1 -v`, as above): it is the one proof that the practice reset works on the real image, and it skips without the variable.
