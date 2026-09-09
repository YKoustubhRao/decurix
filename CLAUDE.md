# Flowera

Flowera is a control plane for spinning up disposable "desktop" VMs (Docker containers running a full XFCE desktop over VNC) and driving them with an autonomous AI agent. A Go server manages desktops via the Docker API; each desktop container runs a Python agent that watches for tasks, executes shell commands to get them done, and asks a human for help through the control plane when it's stuck.

## Architecture

```
main.go (Go control plane, :8080)
  ├─ Docker API (github.com/docker/docker/client) — create/start/stop/delete desktop containers
  ├─ templates/*.html — dashboard, messages, help-notes UI (html/template, re-parsed per request)
  ├─ reverse proxy /desktop/<id>/ → container's VNC port 6901
  └─ shared "flowera-ollama" container (Ollama, port 11434) used by every desktop's agent

desktop container (flowera-desktop:{basic,developer,ai})
  ├─ base image: accetto/ubuntu-vnc-xfce-chromium-g3 (XFCE + VNC + Chromium)
  ├─ XDG autostart (flowera-agent.desktop) → start_agent_terminal.sh → vm_server.py
  └─ vm_server.py (Python, :8081 inside the container)
        ├─ HTTP endpoint POST /message — receives tasks from the control plane
        ├─ command_watcher() — polls /home/headless/command/ for new task files
        ├─ perform_task() — drives an agent loop against Ollama, parses COMMAND:/DONE:/HELP:
        │    and runs shell commands via subprocess
        ├─ ask_for_help() / write_final_message() — POSTs to control plane /api/helptext
        └─ get_memory_context() — reads /home/headless/memory/*.txt into the prompt
```

Control plane → desktop communication happens over the Docker bridge network `flowera-network` (container name `desktop_<id>`, not published ports). Desktop → control plane communication uses `CONTROL_PLANE_URL` (`http://host.docker.internal:8080`).

Three desktop profiles share the same `vm_server.py`/`start_agent_terminal.sh`/autostart wiring but differ in installed tooling:
- `DesktopDockerfile.basic` — VNC desktop only
- `DesktopDockerfile.developer` — adds LibreOffice, git, build tools, Node, Go, Rust, gh
- `DesktopDockerfile.ai` — developer image + `@anthropic-ai/claude-code`, `@openai/codex`

## Running it

```bash
./setup.sh          # one-time host setup: Docker, cloudflared, QEMU (Linux, needs sudo)
./build.sh           # builds all three desktop images, restarts the control plane via docker compose,
                      # waits for :8080, then opens a Cloudflare tunnel
docker compose up -d # just the control plane (uses Dockerfile, mounts /var/run/docker.sock)
go test ./...        # Go unit tests (main_test.go)
```

The control plane needs `.env` (see `.env.example`) with at least `DEFAULT_PASSWORD`. Relevant env vars: `DESKTOP_HOST`, `DESKTOP_STOP_TIMEOUT_SECONDS`, and (inside desktop containers) `CONTROL_PLANE_URL`, `OLLAMA_URL`, `OLLAMA_MODEL`, `DESKTOP_ID`, `DESKTOP_NAME`.

## Conventions / gotchas

- `CODE_MAP.md` is auto-generated (ctags + LLM summaries) — don't hand-edit it; it's regenerated from source after edits.
- `main.go` doc comments: every exported/significant func gets a one-line `// funcName does X` summary directly above it — keep following that pattern for new functions.
- `vm_server.py` agent output must start with exactly one of `COMMAND:` / `DONE:` / `HELP:` (see `SYSTEM_PROMPT`) — the parser (`parse_agent_response`) searches for whichever marker appears first, so don't change that contract without updating both sides.
- The desktop agent (`vm_server.py`) is launched as a detached background process by `start_agent_terminal.sh` (via `setsid ... &`), not inside a visible terminal — output goes to `/tmp/flowera-startup.log` inside the container. Don't reintroduce a foreground/`xfce4-terminal --hold` launch; it makes the agent visible as a window on the VNC desktop instead of running silently.
- Container/volume names are derived from user-supplied desktop names via `sanitizeName` (lowercase, `[a-z0-9_-]` only) — always route desktop IDs through it before using them in Docker API calls or shell commands.
- `manager.mu` guards `helpNotes` and `workerMemory`; hold the lock for the whole read-modify-write, not just parts of it.
