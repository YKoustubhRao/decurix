# Decurix 🌸

### AI workers that learn to do your work.

Decurix is a platform for creating **AI-powered workers that can actually operate a computer and get work done**.

Give a worker its own computer. Teach it how you work. Decurix watches, learns, and gradually takes over the task — allowing your workers to operate independently whenever you assign them work.

When a worker reaches something that requires human judgment, it doesn't just fail. **It asks you.** Approve a payment, merge a pull request, send a message, make a decision — you stay in control. The worker learns from your response and becomes better at handling similar situations in the future.

The goal is simple:

> **Teach once. Delegate forever.**

Decurix is built around a fundamentally different way of working with AI: **continuous learning with humans in the loop.**

Instead of asking AI to answer questions, we're building AI that can **take responsibility for tasks**.

---

## 🚀 The Vision

We believe the next generation of AI won't just live inside chat boxes.

It will have **computers, tools, responsibilities, and the ability to learn.**

A Decurix worker should be able to join your team, learn how a particular job is done, execute it thousands of times, raise a query when necessary, and continuously improve through real-world experience.

Imagine creating workers for:

* Operations
* Research
* Customer support
* Sales
* Finance
* Recruiting
* Internal tooling
* Data work
* And eventually, almost any computer-based job

You shouldn't have to explain the same process to an AI every time.

**Teach it. Let it work. Answer it when it gets stuck. Watch it get better.**

---

## 🧠 Human-in-the-Loop, Not Human-Out-of-the-Loop

Decurix isn't trying to remove humans from the system.

It's trying to make humans **10× more capable**.

Workers handle the repetitive execution. Humans handle the judgment calls.

When something unexpected happens, the worker can stop and raise a query. That interaction becomes part of its experience — so the next time it encounters a similar situation, it has a better chance of handling it itself.

Over time, the boundary between what requires you and what your workers can handle **keeps moving forward.**

---

## 🌱 From One Worker to a Workforce

Decurix starts with a single worker.

But the long-term vision is much bigger.

Create a team of AI workers, each with different responsibilities. Give them tasks. Let them operate independently. Step in only when they need you.

A company where humans don't spend their days pushing buttons and moving information between systems.

A company where **AI workers execute the work and humans direct the organization.**

---

## 🛠️ What's Here Today

Decurix currently provides the foundation for:

* Creating and managing AI workers
* Giving each worker its own computer
* Chatting with each worker — one conversation per worker, streamed live as it works
* Teaching workers through real interaction
* Assigning tasks to workers
* Monitoring active workers
* Human approval and assistance when workers get stuck
* Computer use: every worker can see and operate its desktop's apps — web pages, LibreOffice, file manager — through one shared action layer, with Send/Pay-style buttons stopped for your approval and every action recorded with before/after screenshots
* A GUI benchmark (`bench/gui/`) measuring each engine's success rate on real graphical tasks
* Task records: every request becomes a stored task (what was asked, each attempt's steps and screenshots, every query or approval request with your reply, how it ended, how long it took and what it cost), kept in an embedded SQLite database so a restart loses nothing
* A one-tap **Correct / Wrong** on every answer in the chat, since "done" is only the agent's own claim
* An autonomy dashboard (**Tasks** in the top bar): autonomy rate by engine, worker and task type, and the most common reasons people had to step in
* Persistent worker environments
* A central dashboard for managing everything

This is the beginning.

---

## ✅ Feature Overview

A rundown of what's actually built and working, grouped by area. See [`docs/`](docs/architecture.md) for the full architecture behind each of these.

**Workers & environments**
* Create, start, stop, restart and delete disposable desktop VMs (Docker containers running a full XFCE desktop over VNC) from a single dashboard
* Three desktop profiles — `basic` (VNC only), `developer` (+ LibreOffice, git, Node, Go, Rust, gh, Claude Code) and `ai` (developer + Codex) — picked per worker at creation time
* Persistent per-worker environments and worker memory that survive a container being recreated
* Idle workers stop themselves automatically after a configurable timeout, and a stopped worker auto-starts (and the request waits for it) the moment it's given something to do
* Live desktop thumbnails on every dashboard card, driven by a direct RFB screenshot reader rather than an embedded VNC iframe, so the dashboard stays fast at any worker count

**Multiple AI engines, one contract**
* Three interchangeable agent harnesses per worker — **Ollama** (local, hand-rolled command loop), **Claude Code** (real Claude Code CLI with a shared OAuth login, no API key needed) and **TheBot** (a purpose-built Go agent with its own planner/scheduler/skills loop)
* Identical chat, task, query, approval and memory behavior across all three — the harness is invisible from the chat page
* A GUI benchmark suite (`bench/gui/`) with an "oracle" mode (scripted, no model) to score each engine's real success rate on web, spreadsheet and desktop-app tasks

**One conversation per worker**
* A single, persistent chat thread per worker — the same conversation on a laptop, a phone through the tunnel, or a second browser tab, live-streamed (SSE, with automatic polling fallback for tunnels that don't carry SSE)
* Replying to a paused agent is just sending another message — the same chat box, the Requests page and a query reply are three doors onto the same conversation
* Per-task token/model/cost accounting shown alongside every reply
* One-tap **Correct / Wrong** verdicts, since "done" is only the agent's own claim

**Computer use**
* One shared action layer (`observe` / `act` / `verify` / `open`) every engine drives — Claude via MCP tools, Ollama/TheBot via a `gui` shell command — so screen access, approval and auditing can't be bypassed by any engine
* Screen understanding via AT-SPI (GTK/XFCE/LibreOffice), the Chrome DevTools Protocol (Chromium) and `xdotool` input
* Consequential actions (Send/Pay/Publish-style controls) are stopped for explicit human approval before anything happens
* Every action recorded with before/after screenshots, shown inline in the chat

**Recordings, demonstrations & replay**
* Every on-screen action a worker takes is recorded to a structured, replayable event log
* Record a human demonstration directly from the desktop screen via a floating **ticket toolbar** — start/stop/continue capturing, annotate with a reason, and end to produce a downloadable, replayable recording
* Replay any recording as a task — from the Recordings page, by typing `/replay <id>` in chat, or via an engine's own `gui replay` / MCP tools — with parameterized values and privacy-safe halting instead of guessing
* Built-in privacy scrubbing: passwords and credential-shaped text are withheld at the source, never recorded

**Task tracking & autonomy**
* Every request becomes a stored task: attempts, steps, screenshots, interventions and their outcome, kept in an embedded SQLite database that survives restarts
* An autonomy dashboard (**Tasks**) breaking down autonomy rate by engine, worker and task type, and the most common reasons a human had to step in — with a single, precise definition of "autonomous"

**Scheduling & automation**
* **Durable business workflows** (`/workflow`): 25 editable examples plus arbitrary custom workflow specifications and learned-skill stages; stage checkpoints, bounded retries, approval boundaries and independently checked output artifacts. See [usage and validation](bench/workflows/README.md) for Excel/Word/PowerPoint handoffs, recovery, offline soak tests and the live desktop runner.
* Recurring requests on a standard 5-field cron expression, or a one-time request for a future moment — set up from the Requests page and fired automatically by a built-in scheduler (no external cron dependency)
* **DAG workflows** (`/dags`): a drag-and-drop canvas for wiring multiple workers' requests into a directed graph, where a node's output can be referenced in a downstream node's request text, dispatch is gated on every upstream node succeeding, and a failed node skips everything only reachable through it — runnable on its own cron trigger or on demand
* A worker-recommender that suggests the best desktop for a newly typed request, based on which worker is most recently active or whose past memory most closely matches the request (embedding similarity), with recency as the always-available fallback

**Access & connectivity**
* An always-on primary listener for local/dashboard use and desktop callbacks, plus a second, access-code-gated listener for exposing Decurix safely off-network — the code rotates on demand and automatically every 48 hours
* Cloudflare quick tunnels (one per listener) plus direct LAN URLs, all surfaced together in the dashboard's Mobile menu
* An interactive `--cli` terminal client for driving a running control plane without a browser

**Distribution**
* A single self-contained executable (Go, `CGO_ENABLED=0`) with the dashboard UI and desktop build context embedded — no separate assets, one file to run
* Cross-platform release builds (Linux/macOS/Windows, amd64/arm64) via `scripts/release/release.sh`

---

## 🗺️ What's Next

Decurix is being built toward a world where AI workers can:

* Learn complex workflows from demonstration
* Work autonomously for long periods
* Understand when they need to raise a query
* Learn continuously from human feedback
* Use different tools and applications
* Coordinate with other workers
* Handle increasingly complex responsibilities
* Become reliable enough to own entire business processes

---

## 📍 Roadmap

* [ ] Smarter worker learning
* [ ] Long-term worker memory
* [ ] More AI agent harnesses
* [ ] Worker-to-worker collaboration
* [ ] AI assistant for managing Decurix
* [ ] Documentation understanding and search
* [ ] Better worker observability
* [ ] Significantly improved UI
* [ ] Desktop application
* [ ] More powerful task orchestration
* [ ] Production-ready multi-worker teams

---

## 🪟 Install on Windows (no setup, no Docker Desktop)

Download `Decurix Setup <version>.exe`, double-click it, and open Decurix from the Start menu. The app sets everything else up itself.

**What the computer needs**

* Windows 10 version 2004 or newer, or Windows 11, on a 64-bit Intel or AMD processor (not ARM).
* Virtualization switched on in the BIOS/UEFI (Intel VT-x, AMD-V/SVM). Most PCs ship with it on.
* About 20 GB of free disk space and 8 GB of RAM or more.
* An internet connection for the first start.

**What happens on the first start**

1. The installer is not code-signed yet, so Windows SmartScreen may say "Windows protected your PC": choose **More info**, then **Run anyway**.
2. If Windows Subsystem for Linux is not on, Decurix turns it on. Windows asks for an administrator's yes once, and a PC that never had it needs one restart; open Decurix again afterwards.
3. Decurix sets up its own small Linux, installs Docker in it and builds the worker desktop. This takes roughly 10 to 20 minutes once, with progress shown in the window. Later starts take seconds.
4. The dashboard opens. Create a worker, and in the create dialog choose its brain: a hosted model (paste your Claude or Azure key there) or the local model.

Your workers and settings live in `%LOCALAPPDATA%\Decurix` and `%APPDATA%\decurix`; updating by running a newer installer keeps them, and the uninstaller asks before deleting them. If a start fails, the window says why, and the details are in `%APPDATA%\decurix\desktop.log`.

To build the installer yourself: `scripts\windows\build_app.ps1` (needs Go and Node.js); it is written to `dist\electron\`.

---

## ⚙️ Getting Started (running it locally)

Decurix has two moving pieces: a Go **control plane** (the dashboard + Docker orchestration) and per-worker **desktop images** (disposable VNC desktops the control plane spins up). Both run in Docker.

### Prerequisites

* Docker + Docker Compose
  * Linux: run `sudo ./scripts/linux/setup.sh` once if you don't already have Docker (also installs `cloudflared` and QEMU) — or just install Docker yourself.
  * macOS: Docker Desktop (run `./scripts/mac/setup.sh` to check prerequisites, configure Ollama, and verify Docker).
  * Windows: Docker Desktop (`scripts\windows\setup.ps1`).
* [Ollama](https://ollama.com/download), the local model server the workers think with. You can skip it and Decurix will run its own Ollama in a container, but a native install is faster, and it's the only way to use a GPU. See [Setting up Ollama](#-setting-up-ollama).
* Optional: Go 1.26+, needed only to run the control plane directly with `go run` while developing it, or to build the standalone executables with `scripts/release/release.sh` (step 5).

### The scripts

Everything scripted lives under [`scripts/`](scripts/), split by who runs it. `linux/`, `mac/`, and `windows/` hold the scripts in each platform's dialect; `release/` is the cross-compiler and runs from Unix or Windows; `testing/` holds a small stdlib-only stub shared by all platforms' `test-vm` script.

| | Linux | macOS | Windows (PowerShell) | What it does |
|---|---|---|---|---|
| **One-shot** | `scripts/linux/quickstart.sh` | `scripts/mac/quickstart.sh` | `scripts\windows\quickstart.ps1` | Fresh machine to running dashboard in one command: host setup (if Docker's missing) then Start, below. |
| **Host setup** | `scripts/linux/setup.sh` | `scripts/mac/setup.sh` | `scripts\windows\setup.ps1` | One-time: Docker, `cloudflared`, Ollama / QEMU. |
| **Desktop images** | `scripts/linux/build-images.sh` | `scripts/mac/build-images.sh` | `scripts\windows\build-images.ps1` | Builds `decurix-desktop:{basic,developer,ai}`. |
| **Start** | `scripts/linux/build.sh` | `scripts/mac/build.sh` | `scripts\windows\build.ps1` | Like opening the app: builds only missing images, starts the control plane if it isn't already running, opens a tunnel. Leaves existing desktops and the database untouched. |
| **Rebuild** | `scripts/linux/rebuild.sh` | `scripts/mac/rebuild.sh` | `scripts\windows\rebuild.ps1` | Like reinstalling: rebuilds every image and the control plane (still cached, so it's fast), every `desktop_*` container/volume removed, and (after a confirmation prompt) the app database wiped. |
| **Standalone VM test** | `scripts/linux/test-vm.sh` | `scripts/mac/test-vm.sh` | `scripts\windows\test-vm.ps1` | One throwaway desktop container + a dummy control-plane stub, no full Decurix stack needed. See below. |
| **Release** | `scripts/release/release.sh` | `scripts/release/release.sh` | `scripts\release\release.ps1` | Cross-compiles the downloadable executables into `dist/`. |

Starting from nothing?
- **macOS:** `./scripts/mac/quickstart.sh`
- **Linux:** `./scripts/linux/quickstart.sh`
- **Windows:** `.\scripts\windows\quickstart.ps1`

This is the one-shot version of steps 1-4 below — it checks Docker/`cloudflared`/Ollama, builds every image, and starts the dashboard with a tunnel.

Every script figures out the repo root on its own, so it doesn't matter where you run it from.

**On macOS:**
The desktop images and control plane run under `--platform linux/amd64` (Docker Desktop handles Rosetta 2 emulation automatically on Apple Silicon M-series chips). Make sure Docker Desktop is running before starting.

**On Windows:** run the `.ps1` scripts from an ordinary PowerShell window, not WSL. Only `setup.ps1` needs Administrator. Docker Desktop must be running first. Windows won't run downloaded scripts by default, so allow them for that window before the first one:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\windows\quickstart.ps1
```

`build.sh` / `build.ps1` do everything: build any missing images, set up Ollama, start the control plane if it isn't already running, and open the tunnel. `rebuild.sh` / `rebuild.ps1` mirror this and rebuild from scratch.

### 1. Clone and configure

```bash
git clone <this repo>
cd decurix
cp .env.example .env        # Windows: copy .env.example .env
```

(Skipping this is fine if you use `build.sh`/`build.ps1`, which create `.env` from `.env.example` when there isn't one.)

Open `.env` and fill in what you need. See [Environment variables](#-environment-variables) below — every variable already has a working default. The two you're most likely to change are `OLLAMA_URL` and `OLLAMA_MODEL`, which pick the model server and model every worker uses. The Claude harness needs no `.env` entry at all — see [Setting up the Claude harness](#-setting-up-the-claude-harness) below.

### 🦙 Setting up Ollama

Workers on the `ollama` and `thebot` harnesses reason with a model served by [Ollama](https://ollama.com). Two settings in `.env` pick which one:

```bash
OLLAMA_URL=http://host.docker.internal:11434   # the machine running Decurix, as a desktop container sees it
OLLAMA_MODEL=qwen2.5-coder:3b                  # the chat model every new desktop uses
```

**1. Install Ollama and pull the models.**

```bash
ollama pull qwen2.5-coder:3b     # the chat model (~2 GB), whatever OLLAMA_MODEL names
ollama pull nomic-embed-text     # embeddings, needed only by the thebot harness (~270 MB)
```

`qwen2.5-coder:3b` is the default because it's the smallest model we tested that reliably follows the agent's `COMMAND:` / `DONE:` / `HELP:` contract, and it runs on a CPU-only laptop. `qwen3:0.6b` was too weak, and `llama3.2` guessed at destructive commands where it should have raised a query. If you have a GPU, a bigger model such as `qwen2.5-coder:14b` works better. Pull it and set `OLLAMA_MODEL` to its name.

`nomic-embed-text` is a separate model, not an alternative to the chat model. Without it, the `thebot` harness still answers but can't remember anything it learns.

**2. Make Ollama reachable from containers.** Desktops are Docker containers, so they reach Ollama over the network, not on `127.0.0.1`:

* **macOS:** By default, Ollama on macOS binds strictly to `127.0.0.1`, which rejects connections from Docker containers arriving via `host.docker.internal`. Configure it to bind to `0.0.0.0` by running `launchctl setenv OLLAMA_HOST 0.0.0.0` (or `export OLLAMA_HOST=0.0.0.0 && ollama serve`). Running `./scripts/mac/setup.sh` configures this automatically.
* **Linux (including Docker Engine inside WSL):** Ollama must listen on more than loopback. Set `OLLAMA_HOST=0.0.0.0` for its service (`sudo systemctl edit ollama`, add `Environment="OLLAMA_HOST=0.0.0.0"` under `[Service]`, then `sudo systemctl restart ollama`).
* **Windows with Docker Desktop:** `host.docker.internal` forwards to localhost. If unreachable, set `OLLAMA_HOST=0.0.0.0` in user environment variables and restart the Ollama app from the Start menu.

Check it from a container:

```bash
docker run --rm --add-host host.docker.internal:host-gateway curlimages/curl -s http://host.docker.internal:11434/api/tags
```

This should print JSON listing the models you pulled.

**Or let the build/rebuild script handle it.** `./scripts/mac/build.sh`, `./scripts/linux/build.sh`/`rebuild.sh`, and `.\scripts\windows\build.ps1`/`rebuild.ps1` all do this, checking reachability from inside a container:

1. Use `OLLAMA_URL` from `.env` if a container can reach it; otherwise try the host's Ollama.
2. If Ollama is running on the host but only on loopback, make it listen on all interfaces. On Linux that means `OLLAMA_HOST=0.0.0.0` on its systemd service (with `sudo`). On Windows it means `OLLAMA_HOST` in your user environment, followed by a restart of the Ollama app.
3. If no Ollama is reachable at all, point `OLLAMA_URL` at a bundled `decurix-ollama` container that the control plane starts and fills with models itself.
4. Pull `OLLAMA_MODEL` and `nomic-embed-text` if they're missing, then write the result back into `.env` and print the URL and model the desktops will use.

`OLLAMA_URL` can also point at an Ollama on another machine, e.g. `http://10.0.0.5:11434`. At startup the control plane loads `OLLAMA_MODEL` into memory on that server, so the first request doesn't wait for it.

> **Changing the model only affects desktops created afterwards.** Each desktop gets `OLLAMA_URL`/`OLLAMA_MODEL` when it is created. After editing `.env`, restart the control plane (`docker compose up --build -d`, since `.env` is baked into its image) and create a new desktop.

### 🔐 Setting up the Claude harness

The `claude` harness authenticates with a real Claude Code login instead of an API key — one shared sign-in used by every desktop created on the `claude` harness, not a separate login per desktop.

1. Open the dashboard and start creating a desktop (the **+** button).
2. Pick **Claude Code** as the agent harness. If nobody has signed in yet, you'll see a **Log in to Claude** prompt there.
3. Click it: a popup opens with a sign-in link and a box for a verification code — the same flow as running `claude auth login` yourself. Open the link, sign in, and paste the code it gives you back into the popup.

That's it — the popup calls the control plane, which runs the login in a throwaway container and stores the resulting session in `decurix.db`. Every claude-harness desktop created from then on is seeded with it automatically; you only do this again if you want to switch which account desktops sign in as. `build.sh`/`rebuild.sh` treat it exactly like the rest of `decurix.db`: `build.sh` never touches it, `rebuild.sh` wipes it along with everything else in that database.

### 2. Build the desktop images

```bash
./scripts/mac/build-images.sh        # macOS (Apple Silicon / Intel)
./scripts/linux/build-images.sh      # Linux
.\scripts\windows\build-images.ps1   # Windows (PowerShell)
```

which is a wrapper around:

```bash
docker build --platform linux/amd64 -f docker/desktop.basic.Dockerfile     -t decurix-desktop:basic     .
docker build --platform linux/amd64 -f docker/desktop.developer.Dockerfile -t decurix-desktop:developer .
docker build --platform linux/amd64 -f docker/desktop.ai.Dockerfile        -t decurix-desktop:ai        .
```

Re-run whichever of these changed whenever you edit `agent/` or a `docker/desktop.*.Dockerfile` — the control plane doesn't rebuild them for you.

### 3. Start the control plane

```bash
docker compose up --build -d
```

Open **http://localhost:8080** — that's the dashboard. Create a desktop from there (pick a profile and a harness), and its live desktop screen is reachable at `http://localhost:8080/desktop/<id>/`.

Hit **Chat** on a desktop to talk to its agent: one conversation per worker, with every step it takes streamed in as it works. The chat is the same whichever harness that worker runs, and it lives on the control plane rather than in one browser — so the same conversation is there after a refresh, and on your phone through the tunnel. The worker's screen is its own thing: open it in a new tab with the button next to **Chat**.

### 4. Or: do steps 2+3 in one go

```bash
./scripts/mac/build.sh          # macOS
./scripts/linux/build.sh        # Linux
.\scripts\windows\build.ps1     # Windows (PowerShell)
```

This is the "open the app" script: it builds only whichever desktop images don't exist yet, [sets up Ollama](#-setting-up-ollama) (creating `.env` if needed), starts the control plane if it isn't already running, and waits for the dashboard to respond. It does **not** touch any existing `desktop_*` container/volume or the app database — running it again is like reopening an app, not reinstalling it. If `cloudflared` is installed, it opens a public tunnel at the end and shows the tunnel's URL in the dashboard's Mobile menu. Without `cloudflared` the script stops with an error at that last step, but the dashboard is already running on `:8080`. Install `cloudflared` (via `setup.sh` / `setup.ps1`), or ignore the error if you only want local access. Ctrl+C stops watching the tunnel; Decurix keeps running (`docker compose down` to stop it).

The tunnel link points at the same unauthenticated listener as `localhost:8080` — full control of this machine to whoever has the link, so treat it like a password and don't post it anywhere public. If you'd rather hand out a link that needs a one-time code instead, the script also opens a *second* Cloudflare tunnel to a gated listener on `:8090` and shows that one in the Mobile menu too, right above the code — so the code-protected link works from anywhere, not only your own network. The code itself is always in the dashboard's Mobile menu, next to a button to generate a new one on demand (which signs out anything already connected); it also rotates itself automatically every 48 hours, so a code that leaked doesn't stay valid indefinitely. See [Environment variables](#-environment-variables) for `DECURIX_LAN_PORT` if `:8090` collides with something else on your machine.

The script also prints (and the Mobile menu also shows) two more addresses built from the machine's own network IP: an **Admin URL** (`:8080`, no code — full control, same as sitting at this machine, so keep it to yourself) and a **LAN access URL** (the gated `:8090` — needs the code, like its tunnel link above — reachable on your own network without going through either Cloudflare tunnel at all). All four — the two tunnel links, LAN, and admin — live in the one Mobile menu, kept current if the code is ever rotated.

Idle desktops stop themselves automatically after 5 minutes with no task running and no screen open (`DECURIX_IDLE_STOP_MINUTES`, on by default) — it only ever does what the dashboard's own Stop button does. Set it to `0` in `.env` to turn that off. Sending a stopped desktop a request starts it back up and waits for it automatically, so this is meant to be the normal state for a desktop between tasks, not something you have to work around — this is what makes running many desktops practical: a stopped container costs Decurix (and your machine) essentially nothing, and each one only actually runs while it has something to do.

Want a clean slate instead — fresh images, no leftover desktops, an empty dashboard?

```bash
./scripts/mac/rebuild.sh          # macOS
./scripts/linux/rebuild.sh        # Linux
.\scripts\windows\rebuild.ps1     # Windows (PowerShell)
```

This is the "reinstall" script. After a confirmation prompt (`-y`/`-Yes` to skip it), it removes every `desktop_*` container and volume, removes the bundled `decurix-ollama` container (its pulled-models volume is kept — no reason to redownload gigabytes of models), and **permanently deletes the `decurix-data` volume**, so all task history, chat transcripts, query notes and worker memory are gone. It also rebuilds every desktop image and the control plane image — so a change to `agent/` or a Dockerfile is picked up — but with Docker's normal build cache rather than `--no-cache`, so it's just as fast as `build.sh` when nothing actually changed. `.env` is left as it is. Ctrl+C during a rebuild tears the whole stack down rather than leaving it half old, half fresh.

On WSL, the Linux scripts use a memory-limited BuildKit builder with one build step at a time. The first run with this builder downloads BuildKit and starts a separate build cache, so it can take longer than later rebuilds. The scripts leave WSL kernel memory settings alone.

### Testing one desktop image standalone

Iterating on `agent/` or a `docker/desktop.*.Dockerfile` doesn't need the whole control plane running. `test-vm` starts one throwaway desktop container plus a stdlib-only dummy control plane (`scripts/testing/dummy_control_plane.py`) that just prints whatever the agent sends it — no `docker compose`, no dashboard, no touching any desktop you already have running:

```bash
./scripts/mac/test-vm.sh --profile basic --harness thebot --task "say hi"     # macOS
./scripts/linux/test-vm.sh --profile basic --harness thebot --task "say hi"   # Linux
.\scripts\windows\test-vm.ps1 -DesktopProfile basic -Harness thebot -Task "say hi"   # Windows
```

VNC comes up at `http://localhost:16901/vnc.html` and, for the `thebot` harness, TheBot's own built-in UI directly at `http://localhost:14900/` — both published straight to the host, bypassing the control plane's proxy entirely. That UI is a debugging surface here, not the normal way to use a worker: it talks to TheBot's engine directly, so it skips the Decurix agent and with it worker memory and the approval path. The dashboard's **Chat** is the real path. Watch the agent's progress and query/completion callbacks with `docker logs -f decurix-dummy-cp`. Tear down with `./scripts/linux/test-vm.sh --down` (or the Windows equivalent) once you're done; it only ever removes containers this script created.

### 5. Or: run a prebuilt executable (no Go, no repo)

The control plane also ships as a **fully self-contained executable**. The dashboard's HTML *and* the desktop image build context are compiled into the binary, so a single file is the whole product: no Go, no repo, no runtime, no files beside it. It builds its own desktop images. Build the downloadable bundles with:

```bash
./scripts/release/release.sh          # Linux/macOS
.\scripts\release\release.ps1         # Windows (PowerShell)
```

Both cross-compile from a single machine (Go's cross-compilation needs no target toolchain) and write to `dist/`:

| Target | Binary | Archive |
|---|---|---|
| `linux/amd64`, `linux/arm64` | `decurix_<version>_linux_<arch>` | `.tar.gz` |
| `windows/amd64`, `windows/arm64` | `decurix_<version>_windows_<arch>.exe` | `.zip` |
| `darwin/amd64`, `darwin/arm64` | `decurix_<version>_darwin_<arch>` | `.tar.gz` |

plus a `SHA256SUMS` file. Useful flags:

```bash
./scripts/release/release.sh linux/amd64 windows/amd64   # only these targets
./scripts/release/release.sh --version v0.2.0             # stamp a version (default: git describe)
./scripts/release/release.sh --binaries-only              # skip the archives
./scripts/release/release.sh --output /tmp/out            # write somewhere else
```

An archive is just the binary plus a `README.txt` written for that OS and an optional `.env.example` — nothing the binary actually depends on. On the target machine:

```bash
tar -xzf decurix_<version>_linux_amd64.tar.gz && cd decurix_<version>_linux_amd64
./decurix --build-images     # build the three desktop images (one time, slow)
./decurix                    # dashboard on http://localhost:8080
```

`--build-images` builds `decurix-desktop:{basic,developer,ai}` straight out of the executable, streaming Docker's output; it skips images that already exist unless you add `--rebuild`. Until they exist the dashboard still starts, and says which ones are missing.

Configuration is optional — every setting has a working default, and `.env` is read from the working directory or from the folder the executable sits in (plain environment variables work too). `./decurix --version` prints the build it came from.

The only thing the executable needs from the host is a reachable Docker daemon.

### Running the control plane outside Docker (Go development)

```bash
go generate .                                       # pack TheBot's sources for go:embed; needed before building
                                                    # a binary that will run --build-images (the release and
                                                    # installer scripts do it themselves)
go build ./... && go vet ./... && go test ./...   # compile, lint, unit tests
go run ./cmd/decurix                                # run it directly (needs Docker + the images from step 2)
```

The templates in `web/templates/` are embedded into the binary with `go:embed` (see [`web/embed.go`](web/embed.go)), so editing one needs a rebuild — `go run` does that for you, but a binary you already built won't pick the change up.

### 🍎 macOS Special Steps & Notes

If you are running Decurix on macOS (especially Apple Silicon M-series):

1. **Architecture Emulation (Rosetta 2):**
   * The desktop images and Docker containers run with `--platform linux/amd64`.
   * Docker Desktop for Mac handles this automatically via Rosetta 2 / emulation. In Docker Desktop **Settings > General**, ensure *"Use Rosetta for x86/amd64 emulation on Apple Silicon"* is checked for maximum performance.
   * `scripts/mac/` handles `--platform linux/amd64` flags automatically.

2. **Ollama on Mac (Metal GPU Acceleration):**
   * Running Ollama natively on macOS gives the workers full Apple Silicon Metal GPU acceleration.
   * By default, the macOS Ollama app binds only to `127.0.0.1`. Docker containers talk to the host via `http://host.docker.internal:11434`, which requires Ollama to listen on `0.0.0.0`.
   * Run:
     ```bash
     export OLLAMA_HOST=0.0.0.0
     ollama serve
     ```
     Or configure it system-wide with `launchctl setenv OLLAMA_HOST 0.0.0.0` (already handled if you ran `./scripts/mac/setup.sh`).
   * Remember to pull the required models:
     ```bash
     ollama pull qwen2.5-coder:3b
     ollama pull nomic-embed-text
     ```

3. **Docker Socket Access:**
   * Decurix mounts `/var/run/docker.sock` to orchestrate desktop containers.
   * In Docker Desktop **Settings > Advanced**, ensure *"Allow the default Docker socket to be used"* is enabled.

4. **Remote Access / Mobile Tunnel (Optional):**
   * If you'd like a public tunnel to interact with workers from your phone, install `cloudflared`:
     ```bash
     brew install cloudflared
     ```

### 🔑 Environment variables

Set these in your own `.env` (copied from `.env.example`) — the control plane loads it via `godotenv` on startup.

| Variable | Required? | Default | Purpose |
|---|---|---|---|
| `DEFAULT_PASSWORD` | No | `admin` | VNC login password baked into every new desktop. |
| `OLLAMA_URL` | No | `http://host.docker.internal:11434` | The Ollama server every new desktop reasons with. The default is the host running Decurix. See [Setting up Ollama](#-setting-up-ollama). |
| `OLLAMA_MODEL` | No | `qwen2.5-coder:3b` | The chat model every new desktop uses. It must already be pulled on `OLLAMA_URL`'s server (`build.sh` pulls it for you). |
| `DESKTOP_STOP_TIMEOUT_SECONDS` | No | `10` | Seconds to wait for a desktop container to stop gracefully before force-killing it. |
| `DECURIX_DATA_DIR` | No | your user config dir + `/decurix` (`/data` in the Docker image) | Where `decurix.db` lives: task records, chats, queries, worker memory, action screenshots, and the shared Claude session (see [Setting up the Claude harness](#-setting-up-the-claude-harness)). `docker compose` keeps it on the `decurix-data` volume. |
| `DECURIX_STEP_IMAGES_MB` | No | `1024` | Disk budget for the before/after screenshots of on-screen actions. The oldest are dropped first. A desktop keeps its full-size originals regardless. |
| `DECURIX_LAN_PORT` | No | `8090` | Port of the second, access-code-gated listener — the **LAN access URL**, reachable directly on your own network. Changes which host port `docker compose` publishes it on too. `build.sh`/`rebuild.sh` open their primary tunnel against `:8080` instead, but also open a second tunnel against this port for the gated URL. |
| `DECURIX_IDLE_STOP_MINUTES` | No | `5` (on) | Stop a desktop automatically once it's sat idle — no task running, queued, or waiting on a reply, and no screen/thumbnail open — for this many minutes. Set to `0` to turn it off. It only ever does what the dashboard's own Stop button does. |

Every other setting (the agent harness, the control-plane URL a desktop calls home to, etc.) is set up for each desktop when you create it. See [`docs/architecture.md`](docs/architecture.md) if you're changing that wiring.

---

## 🏗️ Project

Decurix is being built from the ground up around the idea that **AI should be able to do things, not just talk about them.**

The infrastructure underneath Decurix is intentionally modular and designed to support increasingly capable workers as the system evolves.

For developers interested in the current implementation, see:

* [`CLAUDE.md`](CLAUDE.md) and [`docs/`](docs/architecture.md) — architecture reference (packages, agent harnesses, conventions)
* [`CODE_MAP.md`](CODE_MAP.md) — codebase reference
* [`scripts/`](scripts/) — `linux/`, `mac/`, and `windows/` hold the scripts per platform, `release/` builds the downloadable executables

---

## 🌸 The Bigger Idea

Today, software gives you tools.

Tomorrow, software will give you **workers**.

Decurix is building toward that future.

**Give AI a computer. Give it a job. Let it learn.**

### Windows desktop app (Electron)

A Windows app that runs the same thing as everywhere else - Linux desktops
(XFCE, shown in the browser through noVNC) in Docker - without Docker Desktop.
It brings its own: a small Linux (Alpine) in **WSL**, into which it installs
Docker Engine the first time it starts. On a PC that has never had WSL, the
app turns it on (Windows asks once for an administrator's yes) and asks you to
restart Windows.

To build it, install Go 1.26+ and Node.js/npm, then run this from a
**Windows PowerShell** window in a checkout on a Windows drive (for example
`C:\src\decurix`), or right-click the file and choose *Run with PowerShell*:

```powershell
.\scripts\windows\build_app.ps1          # a fresh build, then shows you the result
.\scripts\windows\build_app.ps1 -Run     # ... and starts it
```

It clears the last build and makes two things in `dist\electron\`, either of
which you run with a double click: `Decurix Setup <version>.exe` (the
installer) and `win-unpacked\Decurix.exe` (the same app, run where it is). Your
workers and settings are not touched. `build_app.ps1` calls
`build-electron.ps1`, which does the building and can be run by itself to
rebuild without clearing first. Run either without redirecting its error
output (`2>&1`): Windows PowerShell treats npm's warnings as errors when you
do.

Install it, then launch **Decurix** from the Start menu or desktop shortcut.

* On a new install, the app first looks for a compatible existing WSL distro
  with a running local Docker Engine, Compose, and Windows host integration.
  Otherwise it sets up its own `Decurix` distro and installs Docker Engine.
  Existing app-owned installations keep that distro and their worker data.
* WSL/Hyper-V provides virtualization; each worker still needs Decurix's
  Linux desktop image. Up-to-date images and cached build layers are reused;
  a missing image is built once, which can take several minutes. The app
  installs **native Windows Ollama** in the background on first launch. The
  dashboard stays usable while its SHA-256-verified runtime downloads.
  The closeable **Local model setup** sidebar controls inference. Its chip
  icon stays in the left activity rail; Ctrl+Shift+L toggles the panel.
  Closing the panel keeps hosting running, and its visibility is remembered. Turning **Host locally** on scans Windows
  RAM, GPU and CPU capacity, selects a suitable model (or uses your pinned
  OLLAMA_MODEL), downloads it plus embeddings, and loads it into memory.
  Turning it off cancels model setup, unloads models, stops the app-owned
  server, and deletes its managed model files. The Ollama runtime stays
  installed; the next on cycle scans again and downloads models again.
  Runtime installation, model preparation and live hosting have separate
  status indicators. New workers use a stable model alias, so reselection
  after an off/on cycle does not change their configured model endpoint.
  Models stay loaded while hosting is on unless OLLAMA_KEEP_ALIVE overrides
  this. Hosted-provider workers can run while local hosting is off.
* Docker keeps running in that distro after you close the window, so workers
  and schedules continue; opening the app again reconnects.
* A worker is a Linux desktop of its own: Chrome, a terminal, a file manager,
  LibreOffice. Its screen opens from its card, in the browser.

Where things live:

* `%LOCALAPPDATA%\Decurix\wsl\` - the distro's disk: Docker, the images, every
  worker and its files, and the records. `wsl -d Decurix` opens a
  shell in it.
* `%LOCALAPPDATA%\Decurix\ollama\` - native Ollama, models and server.log.
  The server uses port 11435 on the private WSL-facing Windows address and
  stays running after closing the window. Installed user Ollama on port 11434
  is left alone. OLLAMA_MODELS can move models to another Windows disk.
* `%APPDATA%\decurix\` - the editable `.env` (and an example),
  `desktop.log`, and `windows-runtime.json`, which records the chosen distro.
  When reusing an existing WSL runtime, worker data and images live in that
  runtime's Docker Engine. The app stages only its files in
  `/var/lib/decurix/stack`; it does not change shared WSL or daemon settings
  unless you explicitly set a DECURIX_WSL_* resource override.
  If the saved runtime is unavailable, startup asks you to start its Docker
  Engine rather than silently switching to another runtime.


#### Windows resource sizing for several workers

The app gives its own WSL runtime half of physical RAM by default. Its memory
setting is an upper limit, not RAM reserved from Windows; higher existing
limits do not block startup with a fixed model-memory requirement. Linux can use all
logical processors; CPU time is shared with Windows. New workers default to
2 logical CPUs and 2 GB RAM, configurable in both the create dialog and .env.
The launcher reports an approximate active-worker count after reserving 1 GB
for Linux/control-plane overhead. This is a RAM estimate, not a throughput or
capacity guarantee; browser-heavy workers often need 4 GB or more.

For example, on a **32 GB machine** with enough processors, these settings in
`%APPDATA%\decurix\.env` leave 16 GB outside WSL and allow roughly seven active
2 GB workers, subject to other Linux workloads:

```dotenv
DECURIX_WSL_MEMORY_MB=16384
DECURIX_WSL_CPUS=8
DECURIX_WSL_SWAP_MB=4096
DECURIX_WSL_DISK_GB=256
DECURIX_DEFAULT_DESKTOP_CPUS=2
DECURIX_DEFAULT_DESKTOP_MEMORY_MB=2048
OLLAMA_NUM_PARALLEL=1
```

The app preserves existing .wslconfig values and unrelated settings, backing up
that file as .wslconfig.decurix.bak before changing it. Shared runtimes are not
tuned automatically. Explicit overrides affect **all WSL 2 distros**. To apply
changes to an already running WSL VM, finish/stop its workloads, run
`wsl --shutdown` in PowerShell, then reopen Decurix. The app never shuts down
other distros for you. Memory reclamation returns idle cache to Windows.
See [Microsoft's WSL settings](https://learn.microsoft.com/windows/wsl/wsl-config).

The disk limit is an expandable VHD maximum for **new** distros, not space
reserved per worker. Worker homes are persistent Docker volumes on the Linux
filesystem, with shared image layers and tools. Keep enough real space for
worker files, images and model downloads; startup checks for 10 GB free on the
Windows app disk and Linux Docker filesystem. Changing the limit does not
resize an existing disk; use `wsl --manage Decurix --resize 512GB` with a
supported WSL version when an existing app-owned disk needs expansion. In a
shared distro also check the physical Windows disk containing its VHD.

The existing resource-based model selector runs natively on Windows alongside
Ollama. With OLLAMA_MODEL=auto, it samples available Windows RAM, CPU load and
supported GPU memory, lists installed models and chooses the best fit using
the same policy as the other platforms. The Linux dashboard requests fresh
Windows evaluations, rather than treating a WSL limit or a startup snapshot
as available model memory. Loaded-model memory is added back by the existing
probe, so the current model does not look too large simply because it is
already resident. Selection remains stable while existing workers depend on
the chosen model; the dashboard still reports what fits now. The companion
listens on port 11436 on the same private adapter, and logs to selector.log.
By default several workers share a queued model server, avoiding one loaded
LLM per worker. Increase OLLAMA_NUM_PARALLEL only when RAM/VRAM can hold the
extra contexts. A pinned OLLAMA_MODEL remains your choice. For mirrored WSL
networking set DECURIX_WINDOWS_OLLAMA_HOST to a Windows IPv4 adapter reachable
from WSL; use a private adapter because Ollama's API has no authentication.
See [Ollama's Windows runtime documentation](https://docs.ollama.com/windows).

A remote OLLAMA_URL continues to use that server. Set
DECURIX_WINDOWS_NATIVE_OLLAMA=0 to retain the old Linux model-server behavior.
Older workers retain their original model URL until recreated; their volumes
and model downloads are never deleted automatically. The old Linux Ollama
container is left running while those workers still depend on it. After moving
all of them, stop it with `wsl -d <runtime> -u root -- docker stop decurix-ollama`
to release its memory. Idle worker stopping still works as before.

Uninstalling (Settings > Apps, or the Start menu's own uninstaller) stops the
app-owned distro (or only Decurix containers in a shared runtime) and removes
the program. Shared distros and unrelated Docker resources are kept. It also
deletes the app's distro, every worker, your records and your settings, without
asking, so installing again starts fresh. To keep them for a reinstall, take a
backup first (**Back up**), or uninstall from a terminal with
`"Uninstall Decurix.exe" /S --keep-app-data`. An update installed over the app
keeps everything. Deleting a single worker from the
dashboard deletes its files, its chat and everything it learned; take a backup
first (**Back up**) to restore its skills and memory into another worker.

For a phone, the app opens a temporary Cloudflare link to the access-code-gated
listener while its window is open (`cloudflared` ships with the app). The
unauthenticated dashboard on port 8080 is never exposed.

The desktop checks that the public URL serves Decurix's access-code page before
showing it. With `DECURIX_TUNNEL_PROVIDER=auto`, if Cloudflare cannot connect or
its URL cannot resolve, it switches after 45 seconds to a free
[localhost.run](https://localhost.run/docs/) tunnel using the OpenSSH client.
No account or domain purchase is needed; both services
use temporary addresses and require the app and computer to stay running.
Set `DECURIX_TUNNEL_PROVIDER=localhost-run` in the desktop's `.env` to use that
service directly, or `cloudflare` to use only Cloudflare (the default).
Restart the app after changing this setting. The Windows settings file is
`%APPDATA%\decurix\.env`. SSH host keys are saved in `tunnel-known-hosts` there.

If system DNS reports a newly allocated public hostname as missing, the
desktop's reachability check retries against Cloudflare DNS for that request.
This does not change the computer's DNS settings. A phone or browser using a
router/ISP resolver that returns incorrect NXDOMAIN responses may also need
Cloudflare DNS (`1.1.1.1`, `1.0.0.1`) or Secure DNS enabled on that device.
Public-link startup, DNS failures, and readiness are recorded in `desktop.log`.

The IP URL is a **same-network** address, not a public internet address. Windows
may ask for administrator approval to forward the gated port through WSL and
allow it through the firewall. The app checks that the forwarded address serves
the access-code page. Guest Wi-Fi/client isolation can still prevent another
device from reaching it; use the public HTTPS tunnel in that case or when using
mobile data. Forwarding a private IP cannot make it public through carrier NAT.

### Mac and Linux desktop app

```bash
./scripts/mac/build_app.sh      # Decurix.app (and a .zip of it) in dist/electron/
./scripts/linux/build_app.sh    # Decurix-<version>.AppImage in dist/electron/
```

Each builds the app from scratch on the computer it is run on (it needs Go and
Node.js/npm; the Mac one also Xcode's command line tools), and the result is
opened with a double click.

On a Mac or Linux a worker is a Linux desktop in a container, so this app
**needs Docker** (Docker Desktop on a Mac) and nothing else - no Go, no
checkout. It carries the control plane already compiled, with the Dockerfile
and compose file it runs under, and does by itself what `scripts/mac/build.sh`
does from a checkout: starts Docker Desktop if it is installed and not running,
builds what is missing, starts the control plane and opens the dashboard. The
first start builds the worker desktop image, which takes a long while, once;
later starts take seconds. Its settings are the `.env` in
`~/Library/Application Support/decurix/` (Mac) or `~/.config/decurix/`
(Linux), beside `desktop.log`; a new one uses the control plane's own model
server in a container, and `OLLAMA_URL` there names another (an Ollama running
on the Mac itself is faster: `http://host.docker.internal:11434`).

It uses the same containers, volumes and records as the scripts, so it picks up
where they left off. Closing the window leaves Decurix running; to stop it,
`docker stop decurix`. The Mac app is signed ad hoc, not with a developer
certificate: built on your own Mac it just opens, and on another Mac the first
open is right-click, *Open*.
