# Decurix

Decurix is a control plane for spinning up disposable "desktop" VMs (Docker containers running a full XFCE desktop over VNC) and driving them with an autonomous AI agent. A Go server manages desktops through the Docker API; each desktop runs a Python agent that watches for tasks, does them with shell commands and the screen, and asks a human through the control plane when it is stuck.

This file holds only what must be in your head for every change. The detail lives in `docs/`; read the topic file before touching that area.

## Priorities

**Correctness > intelligence >> efficiency.** A worker may ask or say it failed, but must never report success for something it did not see. Use the heavy model while a kind of work is new; take a cheaper path only when checked runs have earned it; never hand a judgement to a smaller model to save tokens.

## Where things are

| Area | Read |
|---|---|
| Go packages, desktop container, the two listeners, package dependency direction | [docs/architecture.md](docs/architecture.md) |
| TheBot, the one agent harness, its brains (Ollama, Claude, Azure, OpenAI-compatible) and how it adapts to each | [docs/harnesses.md](docs/harnesses.md) |
| Chat page, task records, query rail | [docs/chat.md](docs/chat.md) |
| `gui`, observe/act/verify, approval gate | [docs/computer-use.md](docs/computer-use.md) |
| Recordings, replay, visual anchors, imports | [docs/recordings.md](docs/recordings.md) |
| Skills: learn, ask, run | [docs/skills.md](docs/skills.md) |
| Maturity, final check, "no unseen success", token savings | [docs/learning-and-verification.md](docs/learning-and-verification.md) |
| Healing ladder, experience | [docs/healing.md](docs/healing.md) |
| Making Decurix light: what a desktop and a `gui` command cost, the gui daemon, memory given back from idle desktops, measured before/after | [docs/lightweight.md](docs/lightweight.md) |
| What a run costs, where the tokens go, why a worker gets cheaper with practice (cache, turns, replay per document) | [docs/token-economy.md](docs/token-economy.md) |
| Long requests: planned runs, ledger, amendments, parked questions, cross-check | [docs/long-requests.md](docs/long-requests.md) |
| Build, run, env vars, `--cli`, releases | [docs/running.md](docs/running.md) |
| Harness v2: evidence store, lineage, record integrity, accounting, commit-gate verifier, interaction FSM | [docs/harness-v2.md](docs/harness-v2.md) |
| The learning loop: a task is a case (scope, sources, matrix), a kind of task is a learned method; learning curves | [docs/learning-loop.md](docs/learning-loop.md) |
| Harness v3: the record ledger (`gui record`), party resolution by identifier (`gui select --id`), the scope on the window, questions the screen answers; why four days of rules did not converge | [docs/harness-v3-plan.md](docs/harness-v3-plan.md) |
| The workforce: org skill registry and scoped knowledge with provenance; typed handoff between workers and asking a taught worker before a person; users, roles and approval policy as data; a work queue with capacity and spend limits; the Workforce page (`/workforce`) | [docs/skillhub.md](docs/skillhub.md), [docs/handoff.md](docs/handoff.md), [docs/access.md](docs/access.md), [docs/workqueue.md](docs/workqueue.md) |
| The run record: `decurix.trajectory/v1` (steps, calls, judgements, verdict, bundle) and how a run is assembled from what the control plane and the desktop kept | [docs/trajectory.md](docs/trajectory.md) |
| Evals: the case format, graders, the matrix runner with Wilson bounds, the scripted expert, suites in `evals/` | [docs/evals.md](docs/evals.md) |
| Brainiac: many open models behind one OpenAI-compatible endpoint, chosen per machine (own Go module, imports nothing from Decurix) | [brainiac/README.md](brainiac/README.md), [brainiac/DESIGN.md](brainiac/DESIGN.md) |
| Every lesson learned the hard way (about 950 entries) | [docs/gotchas.md](docs/gotchas.md): one line each, grep it, then open the named part file |
| Open owner decisions, deferred review items, and the live checks owed after the next build | [docs/follow-ups.md](docs/follow-ups.md) |

## Commands

```bash
go build ./... && go vet ./... && go test ./...
PYTHONPATH=agent python3 -m unittest discover -s agent/tests
docker compose up -d                       # just the control plane
./scripts/linux/build.sh                   # open the app (mac/, windows/ twins exist)
```

Live benches (Docker, minutes each) are listed in [docs/running.md](docs/running.md). A change to the agent needs `python3 bench/skills/e2e.py` or the relevant bench, not only unit tests.

## Invariants (breaking one is a bug, not a style choice)

- **Two listeners.** The primary `:8080` has no login and host-level power (Docker socket, host bridge). It is never exposed beyond this machine and its desktops. The gated `:8090` (`internal/lanaccess`) is the only thing tunnelled. Host-running routes are registered on the primary mux only (`RegisterLocalRoutes`). Details: architecture.md.
- **`harness.py` is the only file that branches on `AGENT_HARNESS`.** Everything upstream stays harness-agnostic.
- **No unseen success.** A report goes through the final gate (`verify.py`, `skill_task._gate`); only a confident `done` is done. Facts a program can decide (sums, files, paths, recipients) are checked in code before any model.
- **The approval gate is not the skill's or the engine's to switch off.** A yes is one action's. No "always allow". A yes is made only by the gate process (separate Linux user) from a reply the control plane signed; nothing in the worker's user (files, env, `:8081`, `:4900`) may create one. Details and limits: [docs/gate-boundary.md](docs/gate-boundary.md).
- **Generic tools only.** No per-product or per-workflow helpers; add capabilities (`office read/update/create`, `gui ...`), and put business rules in the operator's `taskRules` as a principle, not a story.
- **Secrets never reach the chat, the command file, the task record or a recording.** `privacy.py` withholds at the source, and every writer of desktop state ends with `privacy.scrub`. A secret reply rests on data (`secret: true`, set from structure), not on a word list.
- **SQLite.** A schema change is a new appended step in that component's `Migrate` slice, never an edit. No query while a `*sql.Rows` is open. A read-then-write needs its own mutex.
- **`go:embed`.** Anything a desktop image needs at docker-build time must be embedded (`embed.go`); it skips `_`-named files and `agent/thebot` (packed by `go generate .`). Use `path.Join` for embed paths.
- **Scripts.** `.sh` and `.ps1` twins are hand-maintained: change both. Scripts that run in Linux must have LF endings (`.gitattributes`; the build refuses CRLF).
- **A shared folder is one folder, opt-in, chosen at the person's own screen.** `shared_folder` at creation is validated (`desktop.ParseSharedFolder`), bound read-write outside `$HOME`, never accepted on the gated listener, and nothing else of the PC is reachable. Details: architecture.md.
- **Desktop state in `$HOME` is fixed at creation.** Seccomp, wallpaper, panel, launchers, ENV: an existing desktop needs recreating to pick up a change.
- **Keep the prompt prefix byte-identical** across model calls and `num_ctx` fixed at 8192, or Ollama reloads (minutes on the target CPU host).
- **`progress.emit` is decoration.** It never blocks, raises or fails a task.

## Conventions

- `CODE_MAP.md` is generated (`go run codemap_gen.go`, also run by `go generate .`); never hand-edit it. Give every new file a header comment; `TestCodeMapIsCurrent` fails until the map is regenerated.
- Every exported or significant Go func gets a one-line `// Name does X` comment.
- `jsonError` never echoes `err.Error()` to a client.
- Tests that need bash or a real browser skip on Windows or use a stand-in; never launch the real browser in a test.
- A new page template goes into `internal/webui/render_test.go`.
- A new stage code check goes into `long_task.stage_code_findings` as ADVISORY (`CHECK_MATURITY`) and must pass the replay gate (`agent/tests/test_replay_gate.py`); it becomes blocking only by its own entry after a clean live round (docs/long-requests.md).
- Add a new lesson to the matching `docs/` file (gotchas go in the newest `docs/gotchas/part-N.md` and a line in `docs/gotchas.md`), not here. Keep this file short.

## Working agreements with the person who runs this

- **Never run `build_app.ps1`**: they run builds (two at once collide). Fix the code; they build.
- Azure GPT may be used for tests through a dev control plane (key in the repo `.env`).
- Use the headless dev loop for live checks (start the Decurix distro's Docker, `docker cp` agent code, build soul in a Go container).
- Dev eval suites, with fictional data only, live in `evals/`; a boundary test keeps harness code from naming them. The sealed held-out set stays outside the repo and is backed up off-host.
