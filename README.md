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

A Flowera worker should be able to join your team, learn how a particular job is done, execute it thousands of times, ask for help when necessary, and continuously improve through real-world experience.

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

**Teach it. Let it work. Help it when it gets stuck. Watch it get better.**

---

## 🧠 Human-in-the-Loop, Not Human-Out-of-the-Loop

Flowera isn't trying to remove humans from the system.

It's trying to make humans **10× more capable**.

Workers handle the repetitive execution. Humans handle the judgment calls.

When something unexpected happens, the worker can stop and ask for help. That interaction becomes part of its experience — so the next time it encounters a similar situation, it has a better chance of handling it itself.

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
* Teaching workers through real interaction
* Assigning tasks to workers
* Monitoring active workers
* Human approval and assistance when workers get stuck
* Persistent worker environments
* A central dashboard for managing everything

This is the beginning.

---

## 🗺️ What's Next

Flowera is being built toward a world where AI workers can:

* Learn complex workflows from demonstration
* Work autonomously for long periods
* Understand when they need human help
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
* Optional: Go 1.26+, needed only to run the control plane directly with `go run` while developing it, or to build the standalone executables with `scripts/release/release.sh` (step 5).

### The scripts

Everything scripted lives under [`scripts/`](scripts/), split by who runs it. `linux/` and `windows/` hold the same scripts in each platform's dialect; `release/` is the cross-compiler and runs from either; `testing/` holds a small stdlib-only stub shared by both platforms' `test-vm` script.

| | Linux / macOS | Windows (PowerShell) | What it does |
|---|---|---|---|
| **One-shot** | `scripts/linux/quickstart.sh` | `scripts\windows\quickstart.ps1` | Fresh machine to running dashboard in one command: host setup (if Docker's missing) then Everything, below. |
| **Host setup** | `scripts/linux/setup.sh` | `scripts\windows\setup.ps1` | One-time: Docker, `cloudflared`, QEMU. Needs root/admin. |
| **Desktop images** | `scripts/linux/build-images.sh` | `scripts\windows\build-images.ps1` | Builds `flowera-desktop:{basic,developer,ai}`. |
| **Everything** | `scripts/linux/build.sh` | `scripts\windows\build.ps1` | Images, then restart the control plane and open a tunnel. |
| **Standalone VM test** | `scripts/linux/test-vm.sh` | `scripts\windows\test-vm.ps1` | One throwaway desktop container + a dummy control-plane stub, no full Flowera stack needed. See below. |
| **Release** | `scripts/release/release.sh` | `scripts\release\release.ps1` | Cross-compiles the downloadable executables into `dist/`. |

Starting from nothing? `./scripts/linux/quickstart.sh` (or `.\scripts\windows\quickstart.ps1`) is the one-shot version of steps 1-4 below — it installs Docker/`cloudflared`/QEMU if they're missing, builds every image, and starts the dashboard with a tunnel.

Every script figures out the repo root on its own, so it doesn't matter where you run it from.

### 1. Clone and configure

```bash
git clone <this repo>
cd flowera
cp .env.example .env
```

Open `.env` and fill in what you need — see [Environment variables](#-environment-variables) below. At minimum you can leave it as-is; every variable has a working default except `ANTHROPIC_API_KEY`, which you only need if you want to use the Claude harness (see below).

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

Open **http://localhost:8080** — that's the dashboard. Create a desktop from there (pick a profile and a harness), and its live VNC session is reachable at `http://localhost:8080/desktop/<id>/vnc.html`.

### 4. Or: do steps 2+3 in one go

```bash
./scripts/linux/build.sh        # Linux/macOS
.\scripts\windows\build.ps1     # Windows (PowerShell)
```

This runs the `build-images` script for all three desktop images, tears down any old `desktop_*` containers/volumes, starts the control plane, waits for the dashboard to respond, and — if `cloudflared` is installed — opens a public tunnel to it at the end. If you don't have `cloudflared` installed, the script errors out on that last step, but the dashboard is already up on `:8080` by then regardless; install `cloudflared` (via `scripts/linux/setup.sh`) or ignore that final error if you only want local access.

### Testing one desktop image standalone

Iterating on `agent/` or a `docker/desktop.*.Dockerfile` doesn't need the whole control plane running. `test-vm` starts one throwaway desktop container plus a stdlib-only dummy control plane (`scripts/testing/dummy_control_plane.py`) that just prints whatever the agent sends it — no `docker compose`, no dashboard, no touching any desktop you already have running:

```bash
./scripts/linux/test-vm.sh --profile basic --harness thebot --task "say hi"   # Linux/macOS
.\scripts\windows\test-vm.ps1 -DesktopProfile basic -Harness thebot -Task "say hi"   # Windows
```

VNC comes up at `http://localhost:16901/vnc.html` and, for the `thebot` harness, its agent chat UI directly at `http://localhost:14900/` — both published straight to the host, bypassing the control plane's proxy entirely. Watch the agent's help/completion callbacks with `docker logs -f flowera-dummy-cp`. Tear down with `./scripts/linux/test-vm.sh --down` (or the Windows equivalent) once you're done; it only ever removes containers this script created.

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
| `ANTHROPIC_API_KEY` | Only to use the **Claude harness** | *(unset)* | Passed through into a desktop's container so the `claude` CLI can authenticate — this is literally all that's needed, no login/OAuth step. Only desktops created with the "Claude Code" harness use it. |
| `DESKTOP_HOST` | No | `localhost` | Public hostname used to build each desktop's VNC URL — set this to your Cloudflare tunnel domain (or similar) when exposing Flowera publicly. **Note:** `docker-compose.yaml` also sets `DESKTOP_HOST=localhost` directly as a container environment variable, which takes precedence over `.env` under `docker compose` (env-file values never override a variable that's already set) — to change it there, edit `docker-compose.yaml`'s `environment:` block instead of `.env`. |
| `DESKTOP_STOP_TIMEOUT_SECONDS` | No | `10` | Seconds to wait for a desktop container to stop gracefully before force-killing it. |

Every other setting (which Ollama model, the agent harness, the control-plane URL a desktop calls home to, etc.) is wired up automatically per-desktop when you create it — see [`CLAUDE.md`](CLAUDE.md) if you're modifying that wiring.

---

## 🏗️ Project

Flowera is being built from the ground up around the idea that **AI should be able to do things, not just talk about them.**

The infrastructure underneath Flowera is intentionally modular and designed to support increasingly capable workers as the system evolves.

For developers interested in the current implementation, see:

* [`CLAUDE.md`](CLAUDE.md) — architecture reference (packages, agent harnesses, conventions)
* [`CODE_MAP.md`](CODE_MAP.md) — codebase reference
* [`scripts/`](scripts/) — `linux/` and `windows/` hold the same three scripts per platform, `release/` builds the downloadable executables

---

## 🌸 The Bigger Idea

Today, software gives you tools.

Tomorrow, software will give you **workers**.

Flowera is building toward that future.

**Give AI a computer. Give it a job. Let it learn.**
