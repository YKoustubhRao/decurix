# The approval gate as a boundary (design, round 3)

Status (2026-11-07): the gate, its key, the image's root removal, the signed inputs of phase 2 and the network per
desktop are built. Round 3 was checked live on a layered image of this checkout with `bench/security/boundary.sh`; the
live checks owed on the next image are listed in [follow-ups.md](follow-ups.md). Existing desktops keep the old setup
until recreated. The input broker ("Next" below) is not built.

Found while checking step 1: the worker could already become root. `headless` had `sudo` for everything and the base image's
default password (`headless`) works (`echo headless | sudo -S id -u` printed 0). Step 1 removes that: see
`agent/decurix-entrypoint.sh` and the root-removal block of `docker/desktop.basic.Dockerfile`.

Original statement: nothing here was built. Round 2 ended with every approval path still self-approvable by the
worker, because the gate's state, the reply channel and the agent token all belong to the same Linux user as the
worker's shell. Filters on words cannot fix that; a separate identity can.

## The rule

A yes can only be created by something the worker cannot write to or read the secrets of. The worker may ask the
gate (`check`), never grant.

## Pieces (as built)

1. **Gate daemon** (`agent/decurix_agent/gate_server.py`): a small process run as its own Linux user (`decurixgate`),
   not the worker's `headless`.
   - Owns the approval state (`computer/approval.json`: the pending request, the one-shot grants, the last reading) in
     its own directory, `/var/lib/decurix-gate` (mode 700, outside `$HOME`).
   - Listens for the control plane on its own TCP port, :8083 (not :8081, not soul's :4900). Each body is checked with
     the desktop's **gate key**, which only the gate and the control plane hold. The key is derived like the agent
     token (`desktop.GateKey`, label `decurix-gate-key`), and is never in the worker's, the agent's or soul's
     environment.
   - Offers the agent, `gui` and TheBot a unix socket (`/run/decurix-gate/gate.sock`, mode 666). Its calls are
     `check` (with the action's `material`), `pending`, `has_grant`, `reset`, `outcome`, `settled`, `last_reading` and
     `site_known`. A grant is consumed inside the daemon. No call creates a grant.
   - Reads a reply (yes / if / no / unclear) itself, through the control plane's LLM proxy, so a lying caller cannot
     decide what a reply meant. The proxy holds the model key; the gate's calls are tagged `gate` (`DECURIX_CALLER`),
     which is a label for the records, not a credential.
2. **Replies bypass the worker's files.** A reply still travels control plane -> agent :8081 -> `command/*-reply.txt`
   -> watcher, as text. The control plane sends an approval reply to the gate as well, signed with the gate key and
   bound to the task id and the gate's request id. A file in the command folder, or a POST to :8081 with the
   worker-readable agent token, cannot produce a grant.
3. **The worker cannot become the gate user or root.** The image has no sudo, no setuid or setgid bit, `/etc/passwd`
   and `/etc/group` at 644, and every desktop runs without `NET_RAW` and with `no-new-privileges` (below).
4. **soul and the legacy harnesses** go through the same socket: soul's `gateAsk` (`cmd/soul/gate.go`) and the Ollama
   and Azure runners' `gated_command`. The gate's port takes only signed bodies, so reaching it from the worker grants
   nothing.

## What stays a filter (and is said so)

`claude_guard.py` was a guard rail for the Claude harness, removed with it on 2026-11-07. It was never part of the boundary.

## Questions settled while building

- `headless` had sudo with the public default password: removed.
- The root step: the image sets its own entrypoint (`tini -> /opt/decurix-entrypoint.sh`), which does the root work
  first and then runs the base image's `startup.sh` as the worker.
- The gate's model access: the control plane's LLM proxy, which holds the key. No key is in the desktop.
- Existing desktops keep the old gate until recreated (desktop state is fixed at creation).

## Order of work (done)

1. Image: gate user, no sudo, `/etc/passwd` 644, NET_RAW dropped.
2. Gate daemon with the unix socket; `approval.py`'s state behind it.
3. Control plane: the gate key, signed replies to the gate's port.
4. The reply file and :8081 are no grant routes; they still carry ordinary requests and replies as text.
5. Live attack run: `bench/security/boundary.sh` replays the self-approval paths of the reviews.


## What is built (2026-11-03, after the round-3 recheck)

The round-3 recheck got root twice on the fresh image and found that a yes was tied to wording, not to the action.
Both are closed, and `bench/security/boundary.sh` replays the attacks against an image (22 rows, all passing on a
layered image of this checkout).

**The image and the entrypoint** (`agent/decurix-entrypoint.sh`, `docker/desktop.basic.Dockerfile`):
- The worker cannot become root: no sudo, no setuid or setgid bit anywhere in the image (chrome-sandbox included, since
  the review of 2026-11-03), `/etc/passwd` 644, `NET_RAW` dropped, and every desktop process runs with no capabilities
  and no-new-privileges.
- The entrypoint runs as root on every start, so it trusts nothing the worker can write. PATH is the system's only
  and every command is named by absolute path: the container's PATH puts `~/.local/bin` first, and a planted `id` there
  ran as root. Root never runs a recursive chown on a folder the worker can write into. Hard-link protection is off in
  the WSL kernel, and `chown -R /dockerstartup` handed the worker the entrypoint, the gate's code and `/etc/passwd`
  through links it had made. `/dockerstartup` becomes the worker's at build time. A new `$HOME` is handed over without
  following links and without files that have a second link.
- The gate starts as `decurixgate` from its own directory with `python3 -I` (no cwd and no user site on `sys.path`). Its
  code is root's (`/opt/decurix_agent`, not writable by the worker).
- The gate starts from a cleared environment (`env -i` in `decurix-entrypoint.sh`), not the container's: it never holds
  the worker's agent token, the VNC password or a brain's settings. It gets PATH, LANG, and HOME, `DECURIX_DIR` and
  `DECURIX_WORK_DIR` set to its own directory, and, when the container sets them, only the variables its own code path
  reads (`GATE_ENV_PASS`): `CONTROL_PLANE_URL` and `DECURIX_LLM` for reading replies through the proxy, the model
  call's two time limits, the worker stage, the task file and the log folder. `agent/tests/test_gate_env.py` fails
  when the gate's modules read a variable the list does not pass (2026-11-07).

**The key** (`desktop.putGateKey`, `GateKeyFile`):
- The key is never in an environment. Every `docker exec` inherits the container's environment, and a desktop could get
  the control plane to exec as the worker (the memory route), so a `~/.profile` line could capture it.
- The control plane copies the key into the created container before its first start, as `/etc/decurix-gate/key`
  (root, 0400, in a 0700 directory). The entrypoint moves it to `/var/lib/decurix-gate/key` (decurixgate, 0400), where
  it survives restarts.
- Every exec the control plane runs in a desktop starts from an empty environment (`workerCmd`).
- A desktop whose key could not be placed is not started.

**A yes is one action's** (`gate_server.py`, `approval.from_material`, `desktop.VerifyGateQuestion`):
- A caller describes the action (`material`: kind, control, window, address, fields sent). The gate makes both its
  fingerprint and its words from that one description, so Pay's grant cannot be asked for in Save draft's words.
  A pending action keeps the words it was first asked in.
- The gate words its own question (`request_text`) and signs it with the request id. The agent sends that signed
  question with the approval. The control plane verifies it, shows the person the gate's words first (the worker's text
  follows, marked as the worker's note), and signs their reply with that request id.
- The gate settles only the request a reply was signed for, and only when the question signed with it is the gate's own
  words. It reads the reply against those words, never against text the worker wrote. An approval the gate did not word
  (a model's own "may I?") is answered as before, but its reply grants nothing.
- What one reply settled is taken once, whether by the agent's watcher (by its text, newest first) or by TheBot (by
  request id). One yes is not both the gate's decision and TheBot's reading.
- TheBot's paused command is a gate request too (`gateAsk`). Its resume takes that request's settlement, waited for in
  the job, not in the HTTP call (the agent gives up on that call after 15 s).
- In the gate, a reply to a question the worker wrote never stands as the yes for the next gated action. Outside a
  gate that convenience stays. Its cost: the "Yes, file the return" flow of 2026-10-26 asks once more.
- No environment variable turns the gate off (`DECURIX_GATE_LOCAL` is gone); only the gate's own process sets
  `approval.IN_GATE`. The socket and the reply port each serve at most 32 calls at once.

Desktops made from the round-3 image have a gate that checks the round-3 signature, so the control plane's replies no
longer verify there: nothing is approved on them until they are recreated. Desktops from before round 3 have no gate.

## Round 4 (2026-11-03, after the round-4 recheck)

**No silent fallback.** "This desktop has a gate" was read only from `/run/decurix-gate`, which existed only when a key
arrived, so a desktop without one kept approvals in the worker's own store.
- The entrypoint makes `/run/decurix-gate` on every image that has the `decurixgate` user, key or not. A gate that never
  started (no key) is then a gate that answers nothing, and nothing gated is allowed.
- The control plane sets `DECURIX_GATE_REQUIRED=1` in every desktop it makes (`desktop.GateRequiredEnv`, not a secret).
  The agent (`approval.remote`) and TheBot (`gatePresent`) read it as "a gate is required": it can only turn the gate
  on, never off.
- No desktop or practice desktop is made from an image without the gate's user (`desktop.prepareGate`, `ErrNoGate`):
  the control plane reads the created container's `/etc/passwd` before the first start.
- Practice desktops get a key and a gate of their own, like any desktop. The practice broker never grants an approval.
- `boundary.sh` has a row for it: a second desktop started with no key has the directory, and a gated click is refused.
  It also has a network row: one desktop's worker cannot reach another desktop's 8081, 5901 or 6901.

Existing desktops from before round 3 keep their old agent and image until recreated; the control plane does not
refuse to start them.

**A reply is bound to the approval it answers** (`httpapi.replyBinding`). A reply is signed with the gate's request id
only when it is bound to that approval:
- typed under that approval's card on the dashboard (`note_id`, held to the newest open question as before), or
- typed in the desktop's chat with that approval on screen (the chat sends `seen_at`, when its newest entry was
  posted) and no other question open.
A chat reply that would land on an approval posted after what the person saw, or on one of several open questions,
is refused with a reason. Since the review of 2026-11-03 the chat's `seen_at` is frozen at the first keystroke of a
draft, and approval bubbles carry Approve and Deny bound to the note id. In the concierge, words typed to a job never
answer its approval: they go as a message and approve nothing. Only the job's Approve and Deny buttons answer it, bound
by `note_id` to the desktop's newest open question when that is a gate-worded approval (`boundApprovalNote`). Every
other door (the requests page, an API call) delivers the reply as a message bound to no request: it grants nothing.

**The gate's question is the gate's words only.** With no reason of its own the gate used the caller's reason in its
signed question; it now uses a fixed one (`gate_server.GATE_REASON`). The worker's text reaches the person only as
the worker's note.

## Round 5 (2026-11-03, after the round-5 recheck)

Guard rails, not the boundary, but each closed a hole the recheck found:
- **The shell checks read words, not text.** `commands._rm_wipe_reason` (agent) and `safety.wipeConcern` (TheBot) split
  a command as the shell does, unquote it, follow `sh -c`, `env -S` and shell-fed here-documents, track `cd`, and resolve
  each path rm is given. The Python side also steps over wrappers (sudo, env, nohup, timeout N, xargs...) to the program;
  TheBot's program rules read each command word's base name past the wrappers `wipeConcern`'s examples name (O-1.a.1),
  read past the folder and config options before a git, publish or docker subcommand (O-1.a.2), and a recursive
  delete's path or a request's target decided at run time waits for a person (O-1.a.5, all 2026-11-07).
- **A request is local only when every target is.** `safety.onlyLocalTargets` reads each curl and wget's targets, and a
  proxy, `--resolve`, `--connect-to`, `-K`, a proxy variable or a curl/wget rc file in the home means not local.
  Each target is judged by the host `net/url` reads from it (`Hostname()`), not a pattern over its text (O-1.a.4,
  2026-11-07).
- **A command is read from the folder it runs in** (O-1.a.3, 2026-11-07): the operator and terminal skills pass their
  working folder to `safety.AssessCommand`, and a relative path is resolved against it, not against an assumed
  `~/work`. With no folder given, only a relative path that climbs out is read, as before.
- **The local control ports** the Claude guard and a kept script may not name are DevTools (9222), the control plane's
  and agent's (8080, 8081), soul's (4900) and the gate's (8083); the deleted extension's 8092 is no longer listed.
- **The Claude guard fails closed when it cannot start**: `/usr/bin/python3 -I <guard> || exit 2`.
- **The browser extension (:8092) took no approval on a gated desktop** (`guard.gate_required`). It has since been
  deleted with `agent/browser-agent` (review 2026-11-03); soul's browser skill remains, off by default.
- **Question files, the final-reply file, the log banner and the saved run report** go through `privacy.withhold_known`.
- **A proven fix stands in the build's own folder**, not the workspace that holds every project.

## What the control plane tells the gate (phase 2, 2026-11-04)

**Four signed inputs.** The gate's TCP port (:8083) takes four POSTs from the control plane, and answers `GET /health`;
a POST that carries an `Origin` is refused. Each body
carries the task id, the time and a fresh nonce, signed with the desktop's gate key under a label of its own
(`request`, `secrets`, `reply`, `task-ended`), so no one of them can stand for another (`desktop.SignGate*`,
`gate_server.sign_*`; vectors signed by the Python gate in `internal/desktop/testdata/gate-signatures.json`, which Go
matches byte for byte). Text is signed as it is sent: a byte that is not valid UTF-8 becomes U+FFFD
(`desktop.GateText`). Values are signed as one SHA-256 over the list of each value's SHA-256, each followed by a NUL,
in order. A `/reply` is checked as it arrives (204) and settled just after, on a thread of its own; the agent's
watcher waits up to 45 s for that settlement (`approval.resolve_reply`, the gate's `outcome` call).
- **`/request`**: the gate's own copy of each new request, never of a reply. When anything of it was withheld the copy
  is the withheld form: a site's name is kept, a value never is, and a request marked secret is withheld whole, so it
  names no site. Whether a password may go to a site (`approval.site_known`) is read from this copy, never from the
  worker's task file, and only for the task the action belongs to: a copy of another task names nothing
  (`approval.gate_request_text`).
- **`/secrets`**: the values the records withheld from a request or a reply (`httpapi.markWithheld`, the one C14
  decision), the secret parameters of a replay or a skill run, and the desktop's screen password (`desktop.ScreenHeld`,
  added to every notice). The gate withholds them from the questions it words.
- **`/reply`**: the person's reply, signed with the gate's request id when it is bound to that approval. It is the
  only way a yes is made (above).
- **`/task-ended`**: sent on every task close (done, error, stop, restart, delete, abandon; `tasks.Store.OnTaskEnded`
  calls `Manager.GateTaskEnded`). The gate then drops that task's values, and its request copy when the copy is that
  task's (below).

**Sent in order, failing closed.**
- **A request** (`httpapi.deliverRequest`, once the desktop answers, on every try): `/secrets`, then `/request`, then
  the agent. If the gate cannot be told (no answer, or not 2xx), nothing reaches the agent and the sender gets 503. A
  desktop with no `decurix.gate` label whose gate does not answer gets "recreate this desktop" (`ErrGateMaybeMissing`).
  One exception: when the only value is the screen password (the task has none of its own), a `/secrets` the gate
  does not take is logged and the delivery goes on; the password is then a second wall only.
- **Teach, replay and run-skill** (`httpapi.sendTaught`) do the same before calling their own agent endpoints. The copy
  is their request line, withheld. It names no site, so the gate asks about every password a taught skill types. A
  replay's parameters are held when its recording's program marks them secret or does not list them; when the
  program cannot be read, every value given is held.
- **A reply** (`httpapi.deliverRequest` too, once the desktop answers): the reply's withheld values by `/secrets`,
  then `/reply`, then the agent, so the gate already holds the reply's values when it settles it. `/reply` is given
  once, on the first try whose `/secrets` the gate took. A refused `/secrets` (of the reply's own values) stops the
  reply (503) and nothing
  reaches the gate's `/reply` or the agent. A `/reply` the gate refuses or does not answer is only logged, and **the
  reply is still delivered** to the agent: it grants nothing there (only the gate makes a yes, from a `/reply` it
  verified), so an approval fails closed, while an answer to a question still reaches the worker as the person's text.
- **Practice and eval** give a practice desktop's agent its request (and an eval expert's reply) through
  `desktop.Manager.PracticeAgentRequest`, which tells the gate first in the same order, by the same function
  (`desktop.TellGate`) and from the same C14 decision (`practice.RequestGateNotice` / `ReplyGateNotice`, which apply
  `tasks.RequestForm` / `tasks.ReplyForm`, the rules `tasks.Store.StoredForm` applies). A practice `/request` with no
  notice, or with a notice for another task, is refused, and the agent is told "secret" exactly when production would
  tell it. So an eval measures the gate production runs.
  - The practice caller opts into its own container (`PracticeAgentRequest`, and a practice task's end by the container
    `CreatePractice` recorded); nothing finds a practice gate by a desktop's name, and a production door never reaches
    one. The container must still carry `PracticeLabel`, be no mock app and bear that name.
  - Names starting with `practice-` or `readiness-` are refused at every create (`desktop.ReservedName`, in
    `BeginProvisioning`), so a person's desktop never shares a practice desktop's id, gate key or gate.
  - A practice calibration replay or skill run tells the gate nothing yet, and that fails closed: the gate binds its
    request copy to the task (H-CP-5, `approval.gate_request_text(task_id)`), so another task's copy names no site
    and the gate asks about every password.
- **The one exception: pinned old code in practice.** An eval arm that runs its own agent tree (`agent_tree`, for
  example eb15981 to measure old code) runs that tree's gate, which may predate `/secrets` and `/request`. Only such
  an arm creates its practice desktop with `PracticeSpec.LegacyGate`; `CreatePractice` records that container, and
  for it alone a 404 from `/secrets` or `/request` is logged and the request still delivered. Any other failure there
  fails closed, the practice runner never sets it, and no desktop of a person can have it: it is a control-plane
  record of a practice container, not a label or a file. The record is in memory only: a control-plane restart
  clears it, and a pinned old arm then fails closed (its requests are not delivered), so an eval in flight across a
  restart loses its old arm. A practice desktop holds only fictional case data, so what
  the old gate does not withhold is fictional; this is an accommodation for measuring old code, never a production
  path.
- **A task's end** never holds up the close. The notice is sent in the background. One the gate did not take is kept
  (at most 256 per desktop, oldest dropped) and sent, oldest first, before the next `/request`, `/secrets` or `/reply`
  to that gate, without failing that delivery. Kept notices are in memory: a control-plane restart loses the ones not
  yet taken, and the gate's own 24 h and 4 MB bounds cover them.

**The gate's own state.** The request copy (`gate-request.json`, the latest request with its task), the values
(`gate-secrets.json`, per task) and `computer/approval.json` are 0600 files in the gate's directory, beside `key` and
`gate.log`, each written whole (a temporary file of the writer's, flushed, then put in place; approval.json since
2026-11-07, before which it was written with the process's umask). A grant there lasts at most 15 minutes. What a reply settled is kept in memory for 10
minutes, for the agent or TheBot to take once. The nonces it has seen are kept in memory only,
for twice the 120 s window a signed body is accepted in. So after a gate restart, a body signed less than 120 s before
can be replayed once more. The worker cannot capture one: `NET_RAW` is dropped.

**The gated listener** forwards no soul route but `GET /agent/<id>/api/health` (`httpapi.gatedAgentRoutes`). A
phone or the tunnel cannot drive the worker through soul.

**What a recording's program can and cannot do (the trust statement).**
- The program (its `params[].secret`) can only *add* withholding. A parameter it does not list, or a program that
  cannot be read, is held. A worker that under-flags a parameter leaks only what it already holds.
- The call recorder's withholding and the gate's wording of its questions are walls for an honest worker.
- Against a dishonest worker, what holds is the gate's site check (from its own signed copy of the request) and the
  yes the gate alone makes from a signed reply. Neither reads the program.

## The gate's secret values and its refusals (2026-11-04, secrets review round 3)

- **Kept until the task ends.** The gate keeps each task's signed secret values (`/secrets`) until the control plane
  signs `POST /task-ended` for that task (label `task-ended`, the same window and one-use nonce as `/secrets`). The
  notice drops the task's values, and its request copy too when the copy is that task's. Counting requests did not
  work: a task waiting on a person lost its values when two newer requests came, and its command reached the
  question in the clear. As a backstop, values are dropped 24 h after they last came, and the store is held under
  4 MB with the oldest tasks dropped first, because the control plane keeps undelivered notices in memory only.
- **A store that cannot be read is kept**, never written over. While it cannot be read, every question the gate
  would word is held back, so the check is refused.
- **Any part that cannot be checked refuses the question.** If the command, the typed text or what the action acts
  on cannot be checked for secrets, the gate asks nobody. It answers
  `{"allowed": false, "request": null, "refused": true, "reason": <fixed text>}`. `gui` raises that reason as an
  error, and soul takes it as a no to that one action and resumes (H-SOUL-8). Nothing waits on a question no reply
  could approve.

- **A value that leaked anyway is purged by value.** Withholding decides at the source; when a value got past it into
  the never-delete stores (blobs, `llm_calls`, judgements, task records, chat, query notes, run bundles), the owner
  purges it at this machine: `POST /api/privacy/purge` (primary listener only, humansOnly, owner role), the Tasks
  page's "Purge a leaked secret" control, or `decurix --purge-secret` reading the value from stdin. Every stored copy
  becomes `[purged]` in place, names and ids kept so the records still join, and the purge is a judgement with the
  person, their reason and the counts, never the value; the value is not logged or echoed, and one shorter than 6
  characters is refused. A desktop cannot call it, so a worker cannot erase what it was seen doing.
  (docs/trajectory.md, "Purging a leaked secret".)

## Next: the input broker (built behind a switch that is off; checked live on a throwaway container)

The gate decides whether a yes exists; nothing yet stands between the worker and the screen. The worker's Linux user
owns everything that can act on the screen or as the person, so each of these is a click on Pay without asking:
1. XTEST from any X client of the worker (`xdotool`, python-xlib), and XSendEvent to the browser's windows.
2. The VNC server itself: the worker can connect on localhost with the password in `VNC_PW` and send RFB input.
3. Chrome's DevTools port (9222), which `computer/cdp.py` uses.
4. AT-SPI: the accessibility bus offers `doAction` on any button of any app.
5. The browser's profile: its cookies are the worker's files, so `curl` can act with the person's session directly.
6. `ptrace` and `/proc` of the browser and the agent (same user): the agent token is one such read.

In broker mode (DECURIX_SCREEN_USER=1), as of 2026-11-07, checked live on a throwaway container (`boundary.sh --layer
--broker --screen-only`: every screen row PASS; the same rows in the default mode say OPEN):
1. **Closed.** The worker's X clients have an untrusted cookie: the display offers them BIG-REQUESTS and XC-MISC only
   (no XTEST, no RECORD, no XInput), and the SECURITY extension denies them events to and reads of the screen
   user's windows (not probed: a probe never sends input).
2. **Closed for files and processes.** The password file is the screen user's (0600 in `/var/lib/decurix-screen`),
   no process the worker can read holds it, and the worker's session starts without it. Still open: a `docker exec`
   without `env -i` inherits `VNC_PW` from the container's config.
3. **Closed.** No port: the browser runs with `--remote-debugging-pipe`, and the relay's socket is in a folder only the
   screen user may enter. Nothing of the screen user listens but VNC (5901).
4. **Closed for the screen side, not probed.** The screen user's accessibility bus is its own (D-Bus checks the uid);
   the worker's apps and their bus are the worker's and not part of the boundary.
5. **Closed.** The browser's profile is `/var/lib/decurix-screen/.config/chromium`, the screen user's alone.
6. **Closed for the screen side**: the X server, the window manager, the browser and the broker are the screen user's,
   and their `/proc` environments cannot be read by the worker. **Open for the agent**: it still runs as the worker,
   with its token in its environment (step 5 of the order below).

Proposed shape:
- A screen user (`decurixscreen`) runs Xvnc, the window manager, the browser and the accessibility bus. The VNC
  password is a file only that user and the control plane hold (the key is placed like the gate's).
- The worker's own X clients (terminal, office) get an *untrusted* X cookie (SECURITY extension): no XTEST, no events to
  and no reading of trusted windows. To check live: which apps misbehave as untrusted clients.
- `gui` becomes a client of a broker that runs as the screen user: it observes (screenshot, accessibility tree, CDP over
  `--remote-debugging-pipe`, no port), describes the action itself, asks the gate `check` with that description, and
  injects. That also closes "what the gate is told": the description comes from the screen side, not the worker's.
- Files reach the browser only through one shared folder (group `decurix-share`); the browser cannot read the rest of
  `$HOME`, and the worker cannot read the browser's profile.
- The agent process moves to its own user too (or reads the token from a file only it can read), so `/proc` gives the
  worker nothing.

Order: (1) a probe row in `boundary.sh` for each of the six paths, failing today; (2) the screen user and an untrusted
cookie, with the desktop's apps checked live; (3) the broker and `gui` through it; (4) CDP by pipe and the
accessibility bus under the screen user; (5) the agent's own user; every row passes.

What a reading of the code found (2026-11-07), which shapes the build:
- **Not only `gui` reaches the screen.** The agent process itself (`skill_task`, `replay_task`, `record_task`,
  `ask_task`, `host_documents`, `ledger_checks`, the browser warm in `watchers`) calls `computer.core`, `cdp` and
  `recording` in-process, as the worker. With an untrusted cookie and no CDP these stop working, so they must become
  clients of the broker too; routing only `gui` is not enough.
- **Only the browser may be launched as the screen user.** Every app launch goes through `session.launch`. A program
  the broker opened would run as `decurixscreen`, and a terminal opened that way would be a way round the gate (typing
  into a terminal is not a gated action). The broker starts the browser only; any other app is started by a helper on
  the worker's side that is given the program and its arguments, never the screen side's environment.
- **Which processes are the screen user's** (to be checked live): Xvnc, the window manager and the browser; the panel,
  the desktop, the terminal and office apps run as the worker with the untrusted cookie. The worker's own apps and its
  own accessibility bus are not part of the boundary. The XFCE autostart that starts the agent is then started directly
  as the worker.
- **CDP needs a relay, not a port.** `cdp.py` uses `/json/version`, `/json/list`, `PUT /json/new` and per-page
  WebSockets; a relay serving those over a Unix socket only the screen user reaches is contained. A loopback TCP port
  cannot be limited to one uid without NET_ADMIN.
- **The VNC password** stays in the container's config for the control plane's readers (`screenshot.go`, `viewer.go`,
  `HoldScreenPasswords`); the entrypoint moves it into a file only the screen user reads before anything else starts.
  A `docker exec` without `env -i` still inherits it.
- **In broker mode `gui` never falls back in-process**: a refused command fails with an error.

Build order from here: the creation switch (`DECURIX_SCREEN_BROKER` on the control plane gives a desktop
`DECURIX_SCREEN_USER=1`); the probe rows; the daemon's peer-uid check and the screen/worker split in the entrypoint;
the CDP relay; the worker-side launch helper; then the agent's in-process screen paths as broker clients.

Built so far (2026-11-07, behind `DECURIX_SCREEN_BROKER`, off; code, unit tests, and a live run on a throwaway
container of a layer over `decurix-desktop:next`, not yet a desktop the control plane made):
- **The creation switch.** `DECURIX_SCREEN_BROKER=1` on the control plane gives each new desktop
  `DECURIX_SCREEN_USER=1` (`internal/desktop/screenbroker.go`). The image has the `decurixscreen` system user (no
  login shell, no password, no sudo).
- **The broker's socket identity** (`computer/daemon.py`, `gui_client.py`, `broker.py`). The gui daemon serves as the
  broker when the switch is on and its uid is not the worker's (`DECURIX_SCREEN_WORKER_UID`, default 1001). A daemon
  in the worker's own uid on such a desktop exits 3. The socket is `/run/decurix-screen/gui.sock`, mode 0666. Every
  caller is checked by SO_PEERCRED, and only the worker's uid is served. The client trusts it only when the folder is
  the screen user's and not writable by others, and the peer is that user. The child takes the caller's argv, stdio,
  umask and niceness, and only those variables of the caller that the package's code reads (scanned from every
  module's source). Of those it refuses any that change the interpreter, the loader, identity, session, a path, an
  address, the gate, the stage or the caller name (`Broker.caller_may_set`). The caller's folder is never put on
  `sys.path`. A screenshot a command writes for its caller (`observe --screenshot`, `DECURIX_GUI_SCREENSHOT`,
  `skill --look`) goes back as bytes over the connection and the client writes it as the worker
  (`broker.deliver_to_caller`, `gui_client.save_output`), and only at a path the caller named. The client never
  runs a command in-process; a broker that is missing, refusing or untrusted is "cannot reach the screen", exit 1.
- **The DevTools relay** (`computer/cdp_relay.py`). It starts the browser with `--remote-debugging-pipe`
  (`agent/chromium-browser` drops the port when `DECURIX_BROWSER_DEBUG=pipe`). It serves `/json/version`,
  `/json/list`, `PUT /json/new`, `/json/activate|close` and per-page WebSockets on
  `/run/decurix-screen/cdp/cdp.sock` (folder 0700). Each WebSocket is a flattened `Target.attachToTarget` session,
  and the relay sets its `sessionId`. There is no browser-wide socket. In broker mode `cdp.py` uses this socket and
  nothing else.
- **The worker-side launch helper** (`computer/launch_helper.py`, started by `start_agent_terminal.sh` in broker
  mode). On the screen side, `session.launch` starts only the browser (through the relay, and with pages for its
  arguments; `--app=URL`, `--new-window` and `--no-startup-window` are the only flags allowed). Every other program
  goes to the helper as argv only. The helper serves the screen user's uid only and starts the program with its own
  (the worker's) environment. The broker checks that the helper's folder and peer are the worker's.
- **The startup split** (`agent/decurix-entrypoint.sh`'s broker branch; the default path after it is unchanged). As
  root: `/run/decurix-screen` (0711) and `/var/lib/decurix-screen` (0700, the screen user's home), `/tmp/.X11-unix`
  made root's and sticky before any user runs, and the VNC password made into `vnc.passwd` by the screen user from a
  pipe (never an argument or an environment of its), then unset. Then the worker's session in the background
  (`agent/decurix-worker-session.sh`, the container's environment less the password) and an exec into the screen
  user's (`agent/decurix-screen-session.sh`, a cleared environment: fixed identity and folders, and only `DECURIX_*`,
  `DESKTOP_*`, `CONTROL_PLANE_URL`, `AGENT_HARNESS`, the screen's size and a few display settings from the container,
  never a `*_TOKEN`, `*_KEY`, `*PASSWORD*`, `*SECRET*` or `VNC_PW`).
  - The screen session makes a new display cookie (given to `xauth` on stdin), starts **Ubuntu's `Xtigervnc`** (the
    base image's `/usr/bin/Xvnc` is built without the SECURITY extension: `xauth generate` failed with "couldn't query
    Security extension"; the image now installs `tigervnc-standalone-server` in its own step and keeps the base
    image's `vncserver` link and `Xvnc`, which the default mode runs), with `-nolisten tcp`, `-rfbauth` and
    `-SecurityTypes VncAuth`; then writes the worker's **untrusted** cookie (`xauth generate :1 . untrusted timeout
    0`) to `/run/decurix-screen/worker.xauth` (0644), starts its own session bus, `xfwm4` (its look from
    `/etc/xdg/decurix-screen`, without the accessibility bridge) and the broker loop, and waits on the X server.
  - The screen side's `DECURIX_DIR`, `DECURIX_WORK_DIR` and `DECURIX_LOG_DIR` are its own folder's (a command first
    failed live on `~/.decurix/computer`, which it may not write).
  - The worker session copies the cookie to `~/.Xauthority`, starts its own session bus, `xfsettingsd`,
    `xfce4-panel`, `xfdesktop`, `Thunar --daemon`, and `start_agent_terminal.sh` directly (no XFCE session, no
    autostart). `start_agent_terminal.sh` no longer runs `xhost` or pre-warms the browser in broker mode.
  - Seen live: Xtigervnc, xfwm4, the broker and Chrome (profile in `/var/lib/decurix-screen`) as `decurixscreen`;
    the panel, Thunar, the agent, soul and the launch helper as `headless`. `gui observe`, `gui open` of a
    `file:///tmp` page, `observe --screenshot` (written as the worker) and `gui open mousepad` (started by the launch
    helper, as the worker, drawn and managed as an untrusted client) work through the broker.
  - Untrusted apps, seen live: the panel draws, but its window list shows nothing (it cannot read the screen user's
    windows); **`xfdesktop` exits** (BadAccess setting a root window property), so there are no desktop icons;
    mousepad works.
- **The agent's in-process screen paths through the broker** (`computer/screen_call.py`). In broker mode, on the
  worker's side, a routed function sends its call as `gui __screen_call NAME` through the same thin client and broker
  `gui` uses; the broker's child runs it as the screen user and answers in JSON lines (a screenshot Frame, sets,
  tuples and non-text keys are tagged so they come back as themselves; the agent's own error classes come back as
  themselves). It never runs one in the worker's process. The broker takes only what each call site needs
  (`SERVED`): `core.observe(remember)`, `core.act` as a click on a ref or keys only, `core.open_target`,
  `cdp.page_list`, `cdp.activate_page(id)`, `browser.warm`, `launchers.close` (a recorded window, by the replay's
  title rule), `ask_task.read_service(key)` (the screen side's own script for that service, never the caller's),
  the demonstration recorder's start, pause, resume, note, stop, status and recover, and `replay.run` with a plain
  program: the escalation ladder's questions to the engine come back to the caller as `ask` messages and are answered
  there, its reports as `report` messages, and the ladder's state comes back onto the caller's ladder. Outside broker
  mode every one runs in its caller's process as before. Checked live from the worker's side: observe (0.17 s with
  its 1280x720 frame), page_list, activate_page, demonstration.status, `host_documents._viewers` and `_buttons`, a
  refused `core.act` "type", `ask_task.read_service`, and an empty-program replay.
- **The probe rows** (`bench/security/screen_probes.sh`, `.py`; `boundary.sh --broker`, `--screen-only`), read-only
  as the worker: XTEST on the display, the VNC password in a readable file or environment, a DevTools endpoint, the
  profile's cookie store, the screen side's `/proc` environ. All five PASS in broker mode; all five are OPEN in the
  default mode.

What remains, in order:
1. **The screen side's own state.** In broker mode the child keeps the screen side's `DECURIX_DIR`
   (`/var/lib/decurix-screen/.decurix`),
   `DECURIX_TASK_FILE` and log folder. The evidence, audit, records ledger, form-check notes and recordings that a
   command writes therefore do not reach the worker's `~/.decurix`, and the screen side does not know the task. One
   way, through the broker or a worker-side writer, has to be chosen. The same holds for the agent's routed calls:
   a demonstration recorded through the broker, its references for the next action, `core.forget_failed_search`
   (which the agent still clears in its own folder) and a warmed browser's "close if unused" check (its thread ends
   with the call) are the screen side's. At boot the agent's `demonstration.recover` ran before the broker served and
   was logged as not reached.
2. **Input files.** Files the browser or a command reads from the worker (`gui upload`, `record --from`,
   `do --source-file`, `file://` pages) must come through the shared folder (group `decurix-share`).
3. **The browser launcher run as the worker** (an icon, `exo-open`, a host app's script) must hand its URL to the
   broker instead of starting a browser of its own.
4. **The worker's apps as untrusted clients**: the desktop (`xfdesktop` exits), the panel's window list, and the
   office apps (LibreOffice, GIMP) are still to be checked; the broker reads AT-SPI on the screen user's bus only, so
   `gui observe` sees no widget of a worker's GTK or LibreOffice window (only its picture and title).
5. **A live check on a real desktop**: one made by the control plane with `DECURIX_SCREEN_BROKER=1` and its seccomp
   profile (the throwaway ran Chrome with `--no-sandbox`), the screen bridge and thumbnails against Xtigervnc 1.13.1,
   a whole skill replay with ladder questions, and Chrome's pipe mode under load (target order, events).
6. **The agent's own user** (path 6's open half), and `VNC_PW` out of a `docker exec`'s environment.

## What this does not cover (said plainly)

- **Direct input.** The gate decides whether a *yes* exists. It does not stand between the worker and the screen: a
  worker that runs `xdotool` or drives Chrome's DevTools port can click Pay without asking anyone. Closing that needs the
  X server and the browser behind the gate's user, with `gui` going through a broker. Until then the guard rails
  (TheBot's command guard and the shell checks) are filters.
- **What the gate is told.** The gate does not see the screen: the action's description comes from the worker's side.
  A lying caller can only get a grant for the action it described, but with direct input it needs none.
- **The agent token** is still in the agent's environment and readable by the worker (`/proc/<agent>/environ`). With it
  the worker can start tasks as a "person" and call the agent's API. It can no longer make a yes.
- **The script sandbox cuts the network and ends what a script started, but not its files.** It runs
  `unshare --user --map-root-user --net --ipc --pid --fork --kill-child`, so nothing a script starts outlives it. A
  pathname Unix socket stays reachable with `unshare` alone; `script_sandbox.status()` reports it. A kernel that
  refuses a pid namespace fails the probe, and then no learned script runs unattended. Offered tools run unsandboxed.
  Neither is part of the boundary.
- **Each desktop has its own cache volume** since the review of 2026-11-03 (`desktop_<id>_cache` at
  `/var/cache/decurix`), and the shared model cache is read-only, so one desktop can no longer leave a poisoned package
  for the next.
- **Input sent to a viewer is the viewer's.** The gate sees an agent's clicks on the host-app or phone viewer in the
  desktop. It does not see input sent to the phone link directly. Phone viewers pass only ordinary typing and
  navigation keycodes (`phonehost.OrdinaryKey`, an allowlist); system keys are refused at the control plane and on the
  phone. The input broker (owner decision O-1) is what closes this.
- **A root `docker exec` by a person.** `docker exec -u 0 <desktop> sh` finds `sh` through the container's PATH, which
  puts the worker's `~/.local/bin` first: it runs the worker's code as root. The control plane never execs as root.
  Anyone else uses absolute paths and `env -i` (the bench caught itself doing this).
- **The chat binds to what was on screen, not to what the person meant.** With one approval open and on screen, a chat
  "yes" is its yes, even if the person was answering an earlier question the worker had since closed. The approval's
  card is the unambiguous door.
- **What `gui` describes.** `about` (what an action acts on) and the control's name come from the worker's side of
  the screen; the gate words its question from them. A misleading description gets a grant only for that description.
- **A fixed `hostgui.key`** change (e.g. after a restore onto another machine) changes every derived key: recreate desktops.

## What the control plane takes from a desktop, by input class (2026-11-05)

- **A person's reply** comes from the control plane's own door, signed when it is an approval. Its boundary is
  built: the gate.
- **A request for a yes** (`check`) is the gate's to decide. It is never a grant.
- **A task's outcome** (`POST /api/querytext`: the final reply, `checked`, the structured verdict, the skill and the
  spend) is taken from **any process in the desktop**. The agent, the worker's shell and a learned script look the
  same.
- **What a desktop kept** (`POST /api/desktops/learned`) is held to a person's own judgement on that desktop's task
  (`derived_from`).
  - The judgement must be the person's own: with a role, not derived from another row, on that task.
  - For a `field_answer`, two more checks:
    - the option must stand in that judgement's text;
    - the row's label must name a field the question it answered names (each label word as a whole word, by the
      agent's own label rule).
  - A worker can post one, but only of what the person said, to what they were asked.
- **Reads** of its own judgements, learned rows and knowledge (`GET /api/desktops/judgements`,
  `/api/desktops/learned`, `/api/knowledge`) are reads only, of its own rows. A secret judgement's text is never sent.
- **Who the caller is.** The desktop doors (`containerRoutes`) are on the primary listener only, never the gated one.
  - The caller is identified by its address on its own network (`requestDesktop`, `callerDesktop`). Each desktop is
    on a network of its own, NET_RAW is dropped and the worker is not root, so one desktop cannot speak with
    another's address.
  - Every such route answers for the calling desktop only, whatever the request names (`desktopReads`, in
    `internal/httpapi/desktop_reads_test.go`, says why for each one, and fails for a route that has no reason).
  - The address names the desktop, not the process in it. No token is asked of the agent on these calls: the agent
    token is for the control plane's calls to desktops, and the worker can read it anyway (above).
- **The gap.** The outcome class has no boundary. A worker can report its own task "done, checked", and the queue, the
  DAG and the person's view take it as the final gate's verdict. The gate's yes cannot be forged; the gate's verdict
  can.
- **What closes it.** An agent-signed callback: an agent key the worker cannot read, bound to the task and a
  control-plane nonce (proposed by PY, not built).
- **Until then, a desktop's `checked` and `done` are a claim of that desktop, not proof.** Nothing downstream that
  moves money or chains work should rest on them alone, without a person or an independent read (the ledger read
  back, an independent basis).

## Between desktops: the network, the fence, the screen password (2026-11-05)

- **A network per desktop.** Each desktop is on a bridge network of its own (`decurix-vm-<id>`, a /28 of
  `DECURIX_DESKTOP_SUBNETS`), so none reaches another's 5901, 6901 or 8081.
  - The control plane joins each one with the lowest gateway priority, so its default route and published ports
    stay on decurix-network.
  - The model server is on no desktop network. Every desktop reaches the local model only through the control
    plane's allow-listed proxy (`/api/native-ollama`): no pull, delete or create, and no unload or reload of the
    shared model (keep_alive and num_ctx are the proxy's).
  - A desktop made before this is on the shared decurix-network until it is recreated, and its card says so.
- **The Windows app's WSL host runs Docker with iptables off**, so dockerd.sh keeps the desktops apart with two rules,
  put first on every start (a kept daemon included) and checked after adding:
  - **the fence**: `-s 10.213.0.0/16 -d 10.213.0.0/16 -m physdev ! --physdev-is-bridged -j DROP` drops traffic routed
    between desktop networks. A network's own bridged frames pass, br_netfilter on or off;
  - **the guard**: mangle `! -s 10.212.0.0/15 -d 10.212.0.0/15 -m conntrack --ctstate NEW -j DROP` drops new
    connections forwarded into Decurix's range from outside it. A sibling distro's container would otherwise come in
    masqueraded as decurix-network's gateway, the address the control plane takes for the person.
- **On that host, a desktop runs only while both rules hold.**
  - dockerd.sh writes what holds to `/tmp/decurix-wsl/fence`.
  - While it says anything but ok, no desktop is made, started or woken: the two ways a desktop runs,
    `startDesktopContainer` and `wakeDesktop`, both ask `requireFence`.
  - `/api/docker/status` says `fence_missing`, and every page shows a banner.
  - A `DECURIX_DESKTOP_SUBNETS` outside the fence's pool is refused.
- **Elsewhere** (Linux, a Mac's Docker Desktop), Docker's own per-bridge rules keep desktop networks apart. That
  holds unless another tool's FORWARD rule accepts the traffic first (libvirt, a second daemon).
- **The person is decurix-network's gateway alone**, never another network's gateway or a default route Docker did
  not name. An address is a weak signal on its own: the WSL guard is what keeps a routed container from wearing it.
- **The screen password is readable by its own worker.** Xvnc, the XFCE session and noVNC run as the worker's uid
  with VNC_PW in their environment, and the password file is the worker's (TigerVNC's obfuscation is a fixed key).
  - It is unset for the agent and everything it starts, and the control plane holds it as a value to withhold for
    the desktop's life.
  - It is withheld from every record the control plane keeps (`withhold.Text`, and the stores where a desktop writes),
    from recorded model calls, and in every task's values given to the gate.
  - **Not covered:**
    - handoffs, the outbox, runs and trajectory bundles, eval output, and log lines that echo desktop text;
    - what the desktop keeps on its own side;
    - images showing it;
    - any transformed form of it (case, splitting, encoding);
    - records written before start-up holds it again.
  - With a network per desktop no other desktop can reach a screen, so the password matters on the worker's own
    desktop only.
  - Running Xvnc as a uid of its own is the fix (an owner decision).

## The deep review (2026-11-03)

**One rule for an approval-class reply.** Keeping a parked finding, starting a workflow stage, and running a held
command are each one action's yes. Each goes through `gated_command.granted(held, reply, question)` plus
`watchers.is_reply_file`. The reply mark says which way a message came, not who wrote it: the command folder is the
worker's, so a reply file alone is a channel, never a person. On a gated desktop the gate's one-shot grant is needed
too.
- **A keep** of a parked finding is asked in the gate's words ("To keep the work of stage X as it is ... confirm it
  here"; `long_task._keep_allowed` / `_keep_confirmed`).
- **A workflow stage** that needs a go-ahead is asked as an approval question ("may I start stage X?"). Only the reply
  to it starts it, and on a gated desktop only the gate's grant (kind `command`, request "start stage X of workflow R:
  ..."). `/workflow approve` starts nothing, and the workflow page has no approve control. Starting a stage approves
  nothing inside it.
- **Legacy harnesses** (Azure, the Ollama task runner; owner decision O-4): on a gated desktop a held dangerous command
  is the gate's (kind `command` material, as soul sends). It runs only when a fresh `check` finds the one-shot grant
  the person's signed reply made. A gate that does not answer allows nothing, and nobody is asked. Deleting these
  harnesses is a follow-up.

**Guard rails, frozen.** The shell guards are filters, not the boundary (owner decision O-1). After this review:
- `commands.py`: a change of folder the guard cannot follow makes the folder unknown, and a recursive delete of a
  relative path or pattern then waits for a person. No new words are added after this.
- TheBot's `internal/safety` carries a FROZEN note. Its last five structural fixes, written by input class in
  [follow-ups.md](follow-ups.md), O-1.a, all landed on 2026-11-07; nothing more is added to it.
- The Claude harness's PreToolUse guard runs for `mcp__computer__.*` too. `open` may not name an input program, a local
  port or the gate's state, and a call the guard cannot read is refused.
- Commands soul runs start without `DECURIX_AGENT_TOKEN`, `*_TOKEN` and `*_API_KEY` (`OPENAI_API_KEY` and
  `OLLAMA_API_KEY` stay, because the gui tool reads them), so a publish or push the guard misses has no credential.

**X.** Xvnc is told `-nolisten tcp`, and the agent terminal no longer runs `xhost +`: X access control stays on, with
only the worker's local user (`+SI:localuser:headless`). Recreate desktops to pick both up.

**Checks the owner runs.** `bench/security/boundary.sh` and `bench/security/gate_flow.py` (the keep-it confirmation
through the gate) on the next image; see [follow-ups.md](follow-ups.md).
