# Flowera 🌸

### AI workers that learn to do your work.

Flowera is a platform for creating **AI-powered workers that can actually operate a computer and get work done**.

Give a worker its own computer. Teach it how you work. Flowera watches, learns, and gradually takes over the task — allowing your workers to operate independently whenever you assign them work.

When a worker reaches something that requires human judgment, it doesn't just fail. **It asks you.** Approve a payment, merge a pull request, send a message, make a decision — you stay in control. The worker learns from your response and becomes better at handling similar situations in the future.

The goal is simple:

> **Teach once. Delegate forever.**

Flowera is built around a fundamentally different way of working with AI: **continuous learning with humans in the loop.**

Instead of asking AI to answer questions, we're building AI that can **take responsibility for tasks**.

---

## 🚀 The Vision

We believe the next generation of AI won't just live inside chat boxes.

It will have **computers, tools, responsibilities, and the ability to learn.**

A Flowera worker should be able to join your team, learn how a particular job is done, execute it thousands of times, raise a query when necessary, and continuously improve through real-world experience.

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

Flowera isn't trying to remove humans from the system.

It's trying to make humans **10× more capable**.

Workers handle the repetitive execution. Humans handle the judgment calls.

When something unexpected happens, the worker can stop and raise a query. That interaction becomes part of its experience — so the next time it encounters a similar situation, it has a better chance of handling it itself.

Over time, the boundary between what requires you and what your workers can handle **keeps moving forward.**

---

## 🌱 From One Worker to a Workforce

Flowera starts with a single worker.

But the long-term vision is much bigger.

Create a team of AI workers, each with different responsibilities. Give them tasks. Let them operate independently. Step in only when they need you.

A company where humans don't spend their days pushing buttons and moving information between systems.

A company where **AI workers execute the work and humans direct the organization.**

---

## 🛠️ What's Here Today

Flowera currently provides the foundation for:

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

## 🗺️ What's Next

Flowera is being built toward a world where AI workers can:

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
* [ ] AI assistant for managing Flowera
* [ ] Documentation understanding and search
* [ ] Better worker observability
* [ ] Significantly improved UI
* [ ] Desktop application
* [ ] More powerful task orchestration
* [ ] Production-ready multi-worker teams

---

## ⚙️ Getting Started (running it locally)

Flowera has two moving pieces: a Go **control plane** (the dashboard + Docker orchestration) and per-worker **desktop images** (disposable VNC desktops the control plane spins up). Both run in Docker.

### Prerequisites

* Docker + Docker Compose
  * Linux: run `sudo ./scripts/linux/setup.sh` once if you don't already have Docker (also installs `cloudflared` and QEMU) — or just install Docker yourself.
  * macOS / Windows: Docker Desktop.
* [Ollama](https://ollama.com/download), the local model server the workers think with. You can skip it and Flowera will run its own Ollama in a container, but a native install is faster, and it's the only way to use a GPU. See [Setting up Ollama](#-setting-up-ollama).
* Optional: Go 1.26+, needed only to run the control plane directly with `go run` while developing it, or to build the standalone executables with `scripts/release/release.sh` (step 5).

### The scripts

Everything scripted lives under [`scripts/`](scripts/), split by who runs it. `linux/` and `windows/` hold the same scripts in each platform's dialect; `release/` is the cross-compiler and runs from either; `testing/` holds a small stdlib-only stub shared by both platforms' `test-vm` script.

| | Linux / macOS | Windows (PowerShell) | What it does |
|---|---|---|---|
| **One-shot** | `scripts/linux/quickstart.sh` | `scripts\windows\quickstart.ps1` | Fresh machine to running dashboard in one command: host setup (if Docker's missing) then Start, below. |
| **Host setup** | `scripts/linux/setup.sh` | `scripts\windows\setup.ps1` | One-time: Docker, `cloudflared`, QEMU. Needs root/admin. |
| **Desktop images** | `scripts/linux/build-images.sh` | `scripts\windows\build-images.ps1` | Builds `flowera-desktop:{basic,developer,ai}`. |
| **Start** | `scripts/linux/build.sh` | `scripts\windows\build.ps1` | Like opening the app: builds only missing images, starts the control plane if it isn't already running, opens a tunnel. Leaves existing desktops and the database untouched. |
| **Rebuild** | `scripts/linux/rebuild.sh` | `scripts\windows\rebuild.ps1` | Like reinstalling: rebuilds every image and the control plane (still cached, so it's fast), every `desktop_*` container/volume removed, and (after a confirmation prompt) the app database wiped. |
| **Standalone VM test** | `scripts/linux/test-vm.sh` | `scripts\windows\test-vm.ps1` | One throwaway desktop container + a dummy control-plane stub, no full Flowera stack needed. See below. |
| **Release** | `scripts/release/release.sh` | `scripts\release\release.ps1` | Cross-compiles the downloadable executables into `dist/`. |

Starting from nothing? `./scripts/linux/quickstart.sh` (or `.\scripts\windows\quickstart.ps1`) is the one-shot version of steps 1-4 below — it installs Docker/`cloudflared`/QEMU if they're missing, builds every image, and starts the dashboard with a tunnel.

Every script figures out the repo root on its own, so it doesn't matter where you run it from.

**On Windows:** run the `.ps1` scripts from an ordinary PowerShell window, not WSL. Only `setup.ps1` needs Administrator. Docker Desktop must be running first. Windows won't run downloaded scripts by default, so allow them for that window before the first one:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\windows\quickstart.ps1
```

`build.ps1` does everything `build.sh` does: builds any missing images, sets up Ollama, starts the control plane if it isn't already running, and opens the tunnel. `rebuild.ps1` mirrors `rebuild.sh`.

### 1. Clone and configure

```bash
git clone <this repo>
cd flowera
cp .env.example .env        # Windows: copy .env.example .env
```

(Skipping this is fine if you use `build.sh`/`build.ps1`, which create `.env` from `.env.example` when there isn't one.)

Open `.env` and fill in what you need. See [Environment variables](#-environment-variables) below — every variable already has a working default. The two you're most likely to change are `OLLAMA_URL` and `OLLAMA_MODEL`, which pick the model server and model every worker uses. The Claude harness needs no `.env` entry at all — see [Setting up the Claude harness](#-setting-up-the-claude-harness) below.

### 🦙 Setting up Ollama

Workers on the `ollama` and `thebot` harnesses reason with a model served by [Ollama](https://ollama.com). Two settings in `.env` pick which one:

```bash
OLLAMA_URL=http://host.docker.internal:11434   # the machine running Flowera, as a desktop container sees it
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

* **Linux (including Docker Engine inside WSL):** Ollama must listen on more than loopback. Set `OLLAMA_HOST=0.0.0.0` for its service (`sudo systemctl edit ollama`, add `Environment="OLLAMA_HOST=0.0.0.0"` under `[Service]`, then `sudo systemctl restart ollama`).
* **macOS / Windows with Docker Desktop:** `host.docker.internal` already forwards to the host's localhost, so a default install usually works as-is. If it doesn't, set `OLLAMA_HOST=0.0.0.0` in your user environment variables and restart the Ollama app from the Start menu.

Check it from a container:

```bash
docker run --rm --add-host host.docker.internal:host-gateway curlimages/curl -s http://host.docker.internal:11434/api/tags
```

This should print JSON listing the models you pulled.

**Or let the build/rebuild script handle it.** `./scripts/linux/build.sh`/`rebuild.sh` and `.\scripts\windows\build.ps1`/`rebuild.ps1` all do this, checking reachability from inside a container:

1. Use `OLLAMA_URL` from `.env` if a container can reach it; otherwise try the host's Ollama.
2. If Ollama is running on the host but only on loopback, make it listen on all interfaces. On Linux that means `OLLAMA_HOST=0.0.0.0` on its systemd service (with `sudo`). On Windows it means `OLLAMA_HOST` in your user environment, followed by a restart of the Ollama app.
3. If no Ollama is reachable at all, point `OLLAMA_URL` at a bundled `flowera-ollama` container that the control plane starts and fills with models itself.
4. Pull `OLLAMA_MODEL` and `nomic-embed-text` if they're missing, then write the result back into `.env` and print the URL and model the desktops will use.

`OLLAMA_URL` can also point at an Ollama on another machine, e.g. `http://10.0.0.5:11434`. At startup the control plane loads `OLLAMA_MODEL` into memory on that server, so the first request doesn't wait for it.

> **Changing the model only affects desktops created afterwards.** Each desktop gets `OLLAMA_URL`/`OLLAMA_MODEL` when it is created. After editing `.env`, restart the control plane (`docker compose up --build -d`, since `.env` is baked into its image) and create a new desktop.

### 🔐 Setting up the Claude harness

The `claude` harness authenticates with a real Claude Code login instead of an API key — one shared sign-in used by every desktop created on the `claude` harness, not a separate login per desktop.

1. Open the dashboard and start creating a desktop (the **+** button).
2. Pick **Claude Code** as the agent harness. If nobody has signed in yet, you'll see a **Log in to Claude** prompt there.
3. Click it: a popup opens with a sign-in link and a box for a verification code — the same flow as running `claude auth login` yourself. Open the link, sign in, and paste the code it gives you back into the popup.

That's it — the popup calls the control plane, which runs the login in a throwaway container and stores the resulting session in `flowera.db`. Every claude-harness desktop created from then on is seeded with it automatically; you only do this again if you want to switch which account desktops sign in as. `build.sh`/`rebuild.sh` treat it exactly like the rest of `flowera.db`: `build.sh` never touches it, `rebuild.sh` wipes it along with everything else in that database.

### 2. Build the desktop images

```bash
./scripts/linux/build-images.sh      # Linux/macOS
.\scripts\windows\build-images.ps1   # Windows (PowerShell)
```

which is just a wrapper around:

```bash
docker build -f docker/desktop.basic.Dockerfile     -t flowera-desktop:basic     .
docker build -f docker/desktop.developer.Dockerfile -t flowera-desktop:developer .
docker build -f docker/desktop.ai.Dockerfile        -t flowera-desktop:ai        .
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
./scripts/linux/build.sh        # Linux/macOS
.\scripts\windows\build.ps1     # Windows (PowerShell)
```

This is the "open the app" script: it builds only whichever desktop images don't exist yet, [sets up Ollama](#-setting-up-ollama) (creating `.env` if needed), starts the control plane if it isn't already running, and waits for the dashboard to respond. It does **not** touch any existing `desktop_*` container/volume or the app database — running it again is like reopening an app, not reinstalling it. If `cloudflared` is installed, it opens a public tunnel at the end and shows the tunnel's URL in the dashboard's Mobile menu. Without `cloudflared` the script stops with an error at that last step, but the dashboard is already running on `:8080`. Install `cloudflared` (via `setup.sh` / `setup.ps1`), or ignore the error if you only want local access. Ctrl+C stops watching the tunnel; Flowera keeps running (`docker compose down` to stop it).

Want a clean slate instead — fresh images, no leftover desktops, an empty dashboard?

```bash
./scripts/linux/rebuild.sh        # Linux/macOS
.\scripts\windows\rebuild.ps1     # Windows (PowerShell)
```

This is the "reinstall" script. After a confirmation prompt (`-y`/`-Yes` to skip it), it removes every `desktop_*` container and volume, removes the bundled `flowera-ollama` container (its pulled-models volume is kept — no reason to redownload gigabytes of models), and **permanently deletes the `flowera-data` volume**, so all task history, chat transcripts, query notes and worker memory are gone. It also rebuilds every desktop image and the control plane image — so a change to `agent/` or a Dockerfile is picked up — but with Docker's normal build cache rather than `--no-cache`, so it's just as fast as `build.sh` when nothing actually changed. `.env` is left as it is. Ctrl+C during a rebuild tears the whole stack down rather than leaving it half old, half fresh.

### Testing one desktop image standalone

Iterating on `agent/` or a `docker/desktop.*.Dockerfile` doesn't need the whole control plane running. `test-vm` starts one throwaway desktop container plus a stdlib-only dummy control plane (`scripts/testing/dummy_control_plane.py`) that just prints whatever the agent sends it — no `docker compose`, no dashboard, no touching any desktop you already have running:

```bash
./scripts/linux/test-vm.sh --profile basic --harness thebot --task "say hi"   # Linux/macOS
.\scripts\windows\test-vm.ps1 -DesktopProfile basic -Harness thebot -Task "say hi"   # Windows
```

VNC comes up at `http://localhost:16901/vnc.html` and, for the `thebot` harness, TheBot's own built-in UI directly at `http://localhost:14900/` — both published straight to the host, bypassing the control plane's proxy entirely. That UI is a debugging surface here, not the normal way to use a worker: it talks to TheBot's engine directly, so it skips the Flowera agent and with it worker memory and the approval path. The dashboard's **Chat** is the real path. Watch the agent's progress and query/completion callbacks with `docker logs -f flowera-dummy-cp`. Tear down with `./scripts/linux/test-vm.sh --down` (or the Windows equivalent) once you're done; it only ever removes containers this script created.

### 5. Or: run a prebuilt executable (no Go, no repo)

The control plane also ships as a **fully self-contained executable**. The dashboard's HTML *and* the desktop image build context are compiled into the binary, so a single file is the whole product: no Go, no repo, no runtime, no files beside it. It builds its own desktop images. Build the downloadable bundles with:

```bash
./scripts/release/release.sh          # Linux/macOS
.\scripts\release\release.ps1         # Windows (PowerShell)
```

Both cross-compile from a single machine (Go's cross-compilation needs no target toolchain) and write to `dist/`:

| Target | Binary | Archive |
|---|---|---|
| `linux/amd64`, `linux/arm64` | `flowera_<version>_linux_<arch>` | `.tar.gz` |
| `windows/amd64`, `windows/arm64` | `flowera_<version>_windows_<arch>.exe` | `.zip` |
| `darwin/amd64`, `darwin/arm64` | `flowera_<version>_darwin_<arch>` | `.tar.gz` |

plus a `SHA256SUMS` file. Useful flags:

```bash
./scripts/release/release.sh linux/amd64 windows/amd64   # only these targets
./scripts/release/release.sh --version v0.2.0             # stamp a version (default: git describe)
./scripts/release/release.sh --binaries-only              # skip the archives
./scripts/release/release.sh --output /tmp/out            # write somewhere else
```

An archive is just the binary plus a `README.txt` written for that OS and an optional `.env.example` — nothing the binary actually depends on. On the target machine:

```bash
tar -xzf flowera_<version>_linux_amd64.tar.gz && cd flowera_<version>_linux_amd64
./flowera --build-images     # build the three desktop images (one time, slow)
./flowera                    # dashboard on http://localhost:8080
```

`--build-images` builds `flowera-desktop:{basic,developer,ai}` straight out of the executable, streaming Docker's output; it skips images that already exist unless you add `--rebuild`. Until they exist the dashboard still starts, and says which ones are missing.

Configuration is optional — every setting has a working default, and `.env` is read from the working directory or from the folder the executable sits in (plain environment variables work too). `./flowera --version` prints the build it came from.

The only thing the executable needs from the host is a reachable Docker daemon.

### Running the control plane outside Docker (Go development)

```bash
go build ./... && go vet ./... && go test ./...   # compile, lint, unit tests
go run ./cmd/flowera                                # run it directly (needs Docker + the images from step 2)
```

The templates in `web/templates/` are embedded into the binary with `go:embed` (see [`web/embed.go`](web/embed.go)), so editing one needs a rebuild — `go run` does that for you, but a binary you already built won't pick the change up.

### 🔑 Environment variables

Set these in your own `.env` (copied from `.env.example`) — the control plane loads it via `godotenv` on startup.

| Variable | Required? | Default | Purpose |
|---|---|---|---|
| `DEFAULT_PASSWORD` | No | `admin` | VNC login password baked into every new desktop. |
| `OLLAMA_URL` | No | `http://host.docker.internal:11434` | The Ollama server every new desktop reasons with. The default is the host running Flowera. See [Setting up Ollama](#-setting-up-ollama). |
| `OLLAMA_MODEL` | No | `qwen2.5-coder:3b` | The chat model every new desktop uses. It must already be pulled on `OLLAMA_URL`'s server (`build.sh` pulls it for you). |
| `DESKTOP_STOP_TIMEOUT_SECONDS` | No | `10` | Seconds to wait for a desktop container to stop gracefully before force-killing it. |
| `FLOWERA_DATA_DIR` | No | your user config dir + `/flowera` (`/data` in the Docker image) | Where `flowera.db` lives: task records, chats, queries, worker memory, action screenshots, and the shared Claude session (see [Setting up the Claude harness](#-setting-up-the-claude-harness)). `docker compose` keeps it on the `flowera-data` volume. |
| `FLOWERA_STEP_IMAGES_MB` | No | `1024` | Disk budget for the before/after screenshots of on-screen actions. The oldest are dropped first. A desktop keeps its full-size originals regardless. |

Every other setting (the agent harness, the control-plane URL a desktop calls home to, etc.) is set up for each desktop when you create it. See [`CLAUDE.md`](CLAUDE.md) if you're changing that wiring.

---

## 🏗️ Project

Flowera is being built from the ground up around the idea that **AI should be able to do things, not just talk about them.**

The infrastructure underneath Flowera is intentionally modular and designed to support increasingly capable workers as the system evolves.

For developers interested in the current implementation, see:

* [`CLAUDE.md`](CLAUDE.md) — architecture reference (packages, agent harnesses, conventions)
* [`CODE_MAP.md`](CODE_MAP.md) — codebase reference
* [`scripts/`](scripts/) — `linux/` and `windows/` hold the same scripts per platform, `release/` builds the downloadable executables

---

## 🌸 The Bigger Idea

Today, software gives you tools.

Tomorrow, software will give you **workers**.

Flowera is building toward that future.

**Give AI a computer. Give it a job. Let it learn.**
