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
--broker --screen-only`: every screen row PASS; the same rows in the default mode say OPEN), and again on a desktop the
real control plane made with `DECURIX_SCREEN_BROKER=1`, under its own seccomp profile and security options (the five
screen rows PASS there too):
1. **Closed.** The worker's X clients have an untrusted cookie: the display offers them BIG-REQUESTS and XC-MISC only
   (no XTEST, no RECORD, no XInput), and the SECURITY extension denies them events to and reads of the screen
   user's windows (not probed: a probe never sends input).
2. **Closed for files and processes.** The password file is the screen user's (0600 in `/var/lib/decurix-screen`),
   no process the worker can read holds it, and the worker's session starts without it. Still open: a `docker exec`
   without `env -i` inherits `VNC_PW` from the container's config.
3. **Closed.** No port: the browser runs with `--remote-debugging-pipe`, and the relay's socket is in a folder only the
   screen user may enter. Nothing of the screen user listens but VNC (5901).
4. **Closed for the screen side, not probed.** The screen user's accessibility bus is its own (D-Bus checks the uid);
   the worker's apps and their bus are the worker's and not part of the boundary. The screen side is the more
   privileged reader of the worker's bus (to see the worker's apps' widgets), never the other way, and acts on a widget
   read there only over a window of the worker's (below, "the worker's apps as untrusted clients").
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

Built so far (2026-11-07, behind `DECURIX_SCREEN_BROKER`, off; code, unit tests, live runs on throwaway containers,
and a desktop the real control plane made - see "Checked on a desktop the control plane made" below):
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
    `xfce4-panel`, `Thunar --daemon` (no longer `xfdesktop`, below), and `start_agent_terminal.sh` directly (no XFCE
    session, no autostart). `start_agent_terminal.sh` no longer runs `xhost` or pre-warms the browser in broker mode.
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

- **The screen side's own state** (`computer/broker.py`, `gui_client.py`; the route chosen: the worker's state stays
  the worker's, and the screen side never opens a path of the worker's). Two ways were weighed: a folder the screen
  user writes and the worker reads, and the writes handed back through the caller. A shared folder in the worker's
  home is a folder the worker can swap or fill with links, and a reader as the screen user that follows one reads the
  screen user's own files (the trusted cookie, the profile) on the worker's behalf; so the caller route was built.
  - *In*: with every command the client sends, as data it read as the worker, what the screen side must know of the
    worker's state (`gui_client.caller_context`): the task file's parts (the task, its stage, request, files, the task's
    secret values and resume copies), `withheld-secrets`, a few state files a screen command reads (`active-task.json`,
    `filled-documents.json`, `opened-files.json`, the worker mode, the guided run), the current task's evidence so far
    (`computer/evidence/<task>.*`) and its recording's `meta.json`, and the worker's launchers (its desktop's and menu's
    `.desktop` files). The broker's child writes them into the screen user's own folders before the command runs
    (`broker.take_context`: the task file's parts in `/run/decurix-screen/caller`, 0700, tmpfs like the worker's own;
    the rest under its `DECURIX_DIR`; the launchers under `~/caller-launchers`, the screen side's `launchers.PLACES`),
    and removes a part the worker no longer has. So the screen side knows the task (evidence is keyed by it; the
    recording is `task-<id>`) and withholds the task's own secret values: checked live, a typed secret came back as
    `[withheld]` in the recording and the evidence.
  - *Out*: every write the child makes under `recordings/` or `computer/evidence/` (through `privacy.write_scrubbed`,
    so already withheld; a recording's frames and an observation's blob) is written in its own folder as before and
    also handed back over the command's connection (`broker.mirror`, an O frame flagged STATE, appends as appends);
    the client writes it as the worker under the worker's own `DECURIX_DIR` (`gui_client.save_state`: a relative path
    of plain parts, in those two roots only, 0644/0755 at most whatever the caller's umask). A demonstration's recorder
    has no caller of its own, so at its stop (or `recover`) the whole recording is handed back (`broker.mirror_tree`).
    Every reader (the agent's checks, the run record, the control plane's puller) finds them where it does today.
    Checked live: a task's recording (meta, events, frames, observations), its evidence notes and a demonstration
    recorded through the agent's routed calls all appeared in the worker's `~/.decurix`.
  - The commands that only read and write the worker's own stores run on the worker's side, as the worker
    (`gui_client.WORKER_COMMANDS`: `skills`, `skill`, `do`, `replay`, `recordings`, `recording`, `record`, `records`,
    `recall`, `guide`), and reach the screen only through the broker's screen calls (`replay.run`, `core.observe`,
    and now `screen.grab`, routed for a vision engine's picture after `replay`/`do` and the final check's look,
    `evidence.live`). `record` reads its `--from` and `--fields` files as the worker; its ledger is then told to the
    screen side with the next command, where `core.act`'s pre-save checks read it.
  - `core.forget_failed_search` is routed (the agent cleared its own copy while the screen side kept the stale one).
  - At boot the agent's `demonstration.recover` waits for the broker to serve (`screen_call.wait_for_broker`, up to 2
    minutes) instead of failing before it is up; checked live: no "not reached" at boot.
- **The worker's apps as untrusted clients.** Found: the SECURITY extension gives an untrusted client no write to the
  root window and none of its events. So xfdesktop exits (BadAccess on its first root property), the panel's window
  list never fills (it never hears `_NET_CLIENT_LIST` change), and the worker's accessibility bus launcher died the
  same way (it names its bus on the root window when it has a display), so none of the worker's apps had an
  accessibility bus at all.
  - xfdesktop is **not** run in broker mode, on either side: as the screen user its menus ("Open With", "Open
    Terminal Here", the applications menu) start any program as that user, past the gate. The screen user draws the
    wallpaper on the root window instead (`computer/wallpaper.py`, redrawn when the screen's size changes); there are
    no desktop icons in this mode. The worker's launchers are still opened by name (`gui open "Excel (Host)"`, a replay
    bringing a window forward) from the copies the screen side was sent, and started by the launch helper as the
    worker. Checked live.
  - The accessibility bus: the worker's session starts its bus without DISPLAY (the launcher then names nothing on the
    display; apps ask the session bus for the address) and with the launch helper's folder (0711) as its runtime
    folder, so the bus socket can be reached; it publishes the address there (`a11y-bus`). The image's
    `accessibility.conf` admits `decurixscreen` besides root and the bus's own user, so the screen broker reads the
    worker's bus (`atspi._broker_bus`: only a `unix:path=`/`unix:abstract=` address from a regular file of the
    worker's in a folder nobody else writes; a `unixexec:` address would run a program). The screen user's own bus
    admits root and itself only. The browser is never given the worker's bus address (`session.launch`,
    `cdp_relay.main`): its widgets on it would be the worker's to press. Checked live: `gui observe` lists mousepad's
    and LibreOffice Writer's menus, toolbars and document, and the panel's buttons; a click and typing in Writer work,
    including on Writer behind the browser (its window is brought forward first).
  - What the worker's bus says is the worker's word, so a widget read there is acted on only where a window of the
    worker's own apps is shown (`core._worker_point`, `screen.owner_uid_at`: the client window at the point, its
    `_NET_WM_PID`, which only that window's own client can set, and that process's uid). Otherwise a widget the worker
    made up and laid over the browser would have the gate asked about "Cancel" while the press landed on "Pay".
    Checked live: a made-up widget over the browser was refused, one over the panel was not. A window the worker maps
    and unmaps between the check and the press is a race this does not close (a few milliseconds).
  - LibreOffice (soffice, GTK3) and mousepad open, draw and take input as untrusted clients; the panel draws and its
    launchers work (a person's click on its Web Browser icon), but its window list stays empty (open, below).
- **The browser launcher run as the worker** (`agent/chromium-browser`): started as the worker in broker mode (the
  panel's Web Browser icon through `exo-open`, `xdg-open`, a link in an app, a host app's script), it hands each page
  to the broker as `gui open` (a local file handed over and opened as a copy) and with none brings the browser up; it
  never starts a browser itself. Checked live: a person's click on the panel's icon and `xdg-open` of a local page
  brought up the screen user's browser, and no browser process of the worker's existed.
- **Input files** (`gui_client.input_files`, `broker.read_input`, `broker.local_copy`): the files a command names (its
  arguments, `--option=value` values, `file://` URLs; regular files the worker can read, at most 16) are opened by the
  client as the worker and handed over as descriptors with the request, like its stdio; the agent's `open_target`
  hands its target the same way (`screen_call.FILE_PARAMETERS`); the current task's documents (the task file's
  `.files`, at most 4) go with every command for `core._in_documents`. The screen side reads them only from those
  descriptors, and gives the browser a copy (same name, 0600, in `~/inputs`, kept a day). A file the caller did not
  hand over is never opened by its name in a broker's child (`broker.NotHanded`): checked live, a link in `~/work` to
  the screen user's `vnc.passwd` was refused without being read. Which commands need it: `upload` (checked live: a
  0600 file of the worker's reached the page, name and size), `open` of a local page or `file://` URL (checked live),
  the agent's `core.open_target`, and the task's documents for the search guard. `record --from/--fields` and `do
  --source-file` need none: they run as the worker. An upload's "may this leave the desktop" check now knows the
  worker's work folders (`privacy._send_roots`) and the worker's state as never leaving.

Checked on a desktop the control plane made (2026-11-07): the control plane binary built from this tree (with a build
overlay pointing its basic image at `decurix-desktop:broker`, so no product tag was touched), run beside the app's
with `DECURIX_SCREEN_BROKER=1`; a desktop created through `/api/desktops/create`, then deleted. Under its seccomp
profile and security options: Chrome runs as `decurixscreen` with its sandbox (no `--no-sandbox`, the namespace sandbox
probe passes; renderers in their own pid namespaces) and the DevTools pipe; the control plane's thumbnail
(`/api/desktops/screenshot`, full RFB authentication against Xtigervnc) and its screen bridge (`/desktop/<id>/websockify`:
the RFB banner, security types and the VncAuth challenge relayed both ways) work; `gui` through the broker (open,
observe, click, type, an upload, LibreOffice's widgets, the agent's routed calls); the worker's state as above; and the
five screen probes PASS (`bench/security/screen_probes.sh` against that desktop, with its own screen password).

What remains, in order:
1. **The panel's window list.** As an untrusted client the panel never hears the root window change, so its
   tasklist stays empty; windows are switched with the window manager's keys (alt-tab) or `gui front`. A tasklist
   drawn by the screen user would have to be one with no way to start a program (the panel's own menus and
   preferences start programs); not built.
2. **What the screen side still does not see of the worker's state**: the worker's learned app guides
   (`_guide_hint` after `gui open` names only the built-in ones), and anything a screen command reads that is not in
   the context it is sent (it reads its own, older or empty copy). A new reader of worker state on the screen side
   must be added to the context (`gui_client.caller_context`) or run on the worker's side.
3. **The browser's own dialogs over accessibility**: the screen broker reads the worker's bus, so the browser's own
   GTK dialogs (a native file chooser) are not listed; page content comes through DevTools as before.
4. **Load and long runs**: a whole skill replay with ladder questions on a broker-mode desktop, Chrome's pipe mode
   under load (target order, events), and the per-command cost of the context (a few small files read and written;
   observe stayed well under a second live).
5. **The agent's own user** (path 6's open half), and `VNC_PW` out of a `docker exec`'s environment.

**Making broker mode the default** (decision for the owner, O-1): every path of the six that the screen side holds is
closed and checked on a control-plane desktop, the worker's state reaches every reader, and the worker's apps work.
What a person loses in this mode is the desktop's icons and the panel's window list; what is still open is path 6's
agent half. The recommendation is to turn it on for new desktops after (4) - one live series of real tasks (a taught
skill with a ladder question, a long request, a demonstration recorded by a person) on a broker-mode desktop - since
until then the gate's boundary is only as good as the filters in front of it. A desktop keeps the mode it was created
with, so turning it on changes nothing for existing desktops.

### Live trial (2026-11-07, finished 2026-11-08)

What ran:
- **The deterministic benches in broker mode.** `bench/skills/e2e.py` and `bench/recordings/e2e.py` take `--broker`
  (`bench/brain.py`, `screen_env`: exactly what `desktop.screenBrokerEnv` adds, `DECURIX_SCREEN_USER=1`). The
  recordings bench now reads `DECURIX_TEST_IMAGE` like the skills bench (it was fixed to `:basic`), and in broker mode
  takes its own viewport measurement as the screen user (the DevTools relay is that user's alone). On
  `decurix-desktop:next` with this tree's agent, all four runs pass (each bench with and without `--broker`). The
  skills bench's output is the same in both modes apart from element refs, including the heal question D-09 puts to
  the person and the "is not on the screen" question.
- **A desktop the real control plane made**: this tree's control plane with `DECURIX_SCREEN_BROKER=1` (a build overlay
  pointing its image at `:broker`; no product tag touched), a hosted brain (Azure, Kimi-K2.6 with vision, gpt-5.4-mini
  fast), `/api/desktops/create`, under its seccomp profile. The trial's agent fixes (below) were copied into the
  desktop, owned by root and not writable by others, then the desktop restarted. Everything went through the routes a
  person uses: `/api/desktops/send` (replies with `note_id`), `/api/desktops/record`, `/api/desktops/ticket/action`,
  `/api/desktops/tasks`, `/tasks/<id>/trace.txt`. All data fictional.
  1. "Open invoice.pdf in ~/work and enter its total into totals.xlsx cell B2, then save": the plan was put to the
     person, agreed, done with `office`, and checked (B2 = 18880). Done, checked, on both days.
  2. "Open the bill entry page ~/work/bills.html in the browser and enter the bill from ~/work/invoice.pdf ...":
     `gui open` of the worker's page (handed over, opened as a copy), observe, three fields typed, the pre-save check
     refused the first press and showed the values, `--checked` pressed it. The final check looked at the screen
     through the broker. Done, checked.
  3. **A demonstration recorded by a person** through a ticket: VNC input (`bench/screen/person.py`, the input a
     viewer sends): a click on the panel's Web Browser icon, ctrl+L, the page's address, three fields, Save bill. All
     11 events were recorded on the screen side, handed back to the worker's store, and listed by the control plane.
     `/learn` through the chat asked 8 questions, each answered as a reply, and saved the skill.
  4. **The taught skill with its button renamed**, twice ("Record bill" through a plain request, "Go" through
     `/skill`): the replay ran through the broker (the browser brought forward for the icon step, the address bar,
     three fields) and stopped at step 11, "button "Save bill" is not on the screen", naming the worker's page. The
     skill is still learning, so it runs model-guided, and the engine settled the renamed control itself (it observed
     the page, once it read the page's source) and pressed it. The final check confirmed on the screen. Done, checked.

Failures found and fixed (with tests; the benches pass with them in both modes):
1. **A replay through the broker could not open a local page.** A taught skill whose first step opens
   `~/work/form.html` stopped there ("not handed over by the command that named it"): `replay.run` went to the
   broker without the files its steps name. Now `screen_call.program_files` hands over each open step's target and
   each upload's files (this run's value for an upload that takes one) from the starting step on, the way `gui` hands
   over the files its command names.
2. **The screen side's copy's address leaked into everything it reported.** The browser shows a handed-over page as
   `/var/lib/decurix-screen/inputs/<random>/name`. So an imported recording's check "the page
   file:///home/headless/work/form.html" never matched, and a recording's addresses held the random folder. They
   matched again only because `url_shape` stars a segment with two digits. Now `broker.local_copy` keeps the worker's
   path beside each copy (`.origin`), and `broker.shown_url` gives it back wherever a page's address leaves the screen
   side: the DevTools group and context (`cdp.py`), and `gui open`'s message. Outside broker mode an address is itself.
3. **A person's click on the panel's Web Browser icon showed nothing.** A task had warmed the browser windowless
   (`--no-startup-window`). The launcher's `gui open browser` answered "Brought Chromium to the front", with no window
   on the screen, so the person's typing went nowhere. Now a running browser with no window gets one
   (`core._open`). This was latent in the default mode too, for `gui open browser`.

Seen, not fixed (none is specific to broker mode; they belong in follow-ups):
- **The hosted brain never reached the replay's own question.** The heal question to the person, and the ladder's
  model questions sent back through `screen_call` as `ask` messages, were covered only by the model-free bench and the
  unit tests. The live run made no `py.escalation` call (the operator's `gui do` runs its ladder without a model).
- **A plan's "each with your OK first: Press Save bill" was not kept.** The press went through on the pre-save self
  check, without the person. The review raised it, and the final gate took the worker's screen evidence. The approval
  gate does not count a local form's save as consequential; the same holds in the default mode. *Fixed in the working
  tree (2026-11-08):* the plan's commits carry their control, a plan put to the person sets `DECURIX_PLAN_ASK_FIRST` on
  the goal's gui commands, and `core.act` puts such a press to the gate (`_plan_hold`); the broker passes the variable
  (read by its literal name). It is the worker's own promise, not part of the boundary: a worker that drops the
  variable presses unasked, as it can with direct input.
- **soul's coverage check does not credit a halted `gui do`** for the values its earlier steps typed (it credits a
  failed command only for `Typed "..."` in its output). So "invoice2.pdf (read; none of its figures went into what was
  entered)" was left open for the person on a run that had entered them. *Fixed in the working tree (2026-11-08):* a
  replay's result carries `entered` (its steps' typed and chosen values, never a secret parameter's), `gui do` /
  `gui replay` print it as one `ENTERED_BY_REPLAY: [...]` line however the run ended, and soul credits it from a
  command that is one `gui do` or `gui replay` (`noteReplayEntered`).
- **The screen user can read the worker's home by path** (`/home/headless` is 0755). A person typing
  `file:///home/headless/work/bills.html` into the browser opened it directly. This is no way in for the worker:
  handed files are still read only from descriptors, and nothing the worker names is opened by its name. But the
  proposed shape ("the browser cannot read the rest of `$HOME`") does not hold.
- A ticket opened about 13 s after `/record` while the previous task finished its post-run work; a `begin` sent
  before then got 409 "no ticket is open". A person's screen bar polls the ticket, so nobody is stranded.
- **No long request was run.**

**Verdict: not yet.** Broker mode held up for real work: a hosted brain's tasks on files and on the screen, a
person's demonstration, teaching, and a taught skill's replay, with no unseen success. But the trial found three
failures that would have broken real use: a taught skill that opens a local page, an imported recording, and the
browser icon. They are fixed in the working tree, and `decurix-desktop:broker`/`:next` predate the fixes. To turn it on
for new desktops:
1. Rebuild the desktop image with these fixes.
2. Run one more short live pass on a desktop made from that image (the browser icon, a taught skill opening a local
   page).
3. Run the part of the series this trial did not reach: a long request, and a run where a replay's ladder asks the
   hosted model through the broker (a skill past learning, or the healing ladder on a recorded replay).

The losses already known stay as stated above: no desktop icons, an empty panel window list, and path 6's agent half.

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
