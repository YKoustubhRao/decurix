# The approval gate as a boundary (design, round 3)

Status (2026-11-03, after the round-3 recheck): built, and checked live on a layered image of this checkout with
`bench/security/boundary.sh`. Not done: a full image build and a real task end to end; existing desktops keep the old
setup until recreated.

Found while checking step 1: the worker could already become root. `headless` had `sudo` for everything and the base image's
default password (`headless`) works (`echo headless | sudo -S id -u` printed 0). Step 1 removes that: see
`agent/decurix-entrypoint.sh` and the root-removal block of `docker/desktop.basic.Dockerfile`.

Original statement: nothing here was built. Round 2 ended with every approval path still self-approvable by the
worker, because the gate's state, the reply channel and the agent token all belong to the same Linux user as the
worker's shell. Filters on words cannot fix that; a separate identity can.

## The rule

A yes can only be created by something the worker cannot write to or read the secrets of. The worker may ask the
gate (`check`), never grant.

## Pieces

1. **Gate daemon**: a small process run as its own Linux user (`decurixgate`), not the worker's `headless`.
   - Owns the approval state (today `~/.decurix/computer/approval.json`) in a directory only it can read or write
     (mode 700, outside `$HOME`, e.g. `/var/lib/decurix-gate`).
   - Listens for the control plane's person replies on its own port (not :8081, not soul's :4900), checked with an
     **approval key** that only the gate and the control plane hold. The key is derived like the agent token
     (`internal/desktop/agenttoken.go`) with a different label, and is never in the worker's, the agent's or soul's
     environment.
   - Offers the agent and `gui` one call over a unix socket: `check(fingerprint, action)` -> allow / ask. A grant is
     consumed inside the daemon. There is no call that creates a grant.
   - Reads a reply (yes / no / if) itself, through the control plane's LLM proxy with its own credential, so a
     lying caller cannot decide what a reply meant.
2. **Replies bypass the worker's files.** Today a reply travels control plane -> agent :8081 -> `command/*-reply.txt`
   -> watcher. The control plane sends an approval reply to the gate daemon as well (signed with the approval key,
   bound to the task id and the pending request id). A file in the command folder, or a POST to :8081 with the
   worker-readable agent token, can no longer produce a grant.
3. **The worker cannot become the gate user.** Precondition to verify in the image before any of this counts:
   - `headless` must not have passwordless `sudo` once startup is done (the base image uses sudo once in
     `user_generator.rc`); drop the sudoers entry after that step, or limit it to that one command.
   - `/etc/passwd` and `/etc/group` stay 666 today only because that startup step tests `-w /etc/passwd`. Make them
     644 after startup finishes (a root step at the end of the entrypoint), so the worker cannot add a uid-0 user.
   - Drop `NET_RAW` (no ARP spoofing of the control plane's identity checks) and keep `no-new-privileges` where the
     startup allows it.
4. **soul and Ollama harnesses** go through the same `check` over the socket; soul's localhost exemption in its
   safety check must not let the worker reach the gate's port.

## What stays a filter (and is said so)

`claude_guard.py` stays as a guard rail for the Claude harness. It is not part of the boundary.

## Open questions to settle before building

- (answered) `headless` had sudo with the public default password; now removed.
- Can the entrypoint run a root step after startup (tini -> startup.sh), or must the image set its own entrypoint?
- Where does the gate daemon get LLM access without a key the worker can read? (control plane proxy, per-desktop
  token held only by the gate)
- Existing desktops keep the old gate until recreated (desktop state is fixed at creation).

## Order of work

1. Image: gate user, sudoers trimmed, `/etc/passwd` 644 after startup, drop NET_RAW. Verify with a live desktop.
2. Gate daemon with the unix-socket `check`; move `approval.py`'s state behind it.
3. Control plane: approval key, send approval replies to the gate port.
4. Remove the reply-file and :8081 paths as grant routes; keep them for ordinary requests.
5. Live attack run: every self-approval path from the round-2 report must fail.


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

Existing desktops from before round 3 keep their old agent and image until recreated; the control plane does not
refuse to start them.

**A reply is bound to the approval it answers** (`httpapi.replyBinding`). A reply is signed with the gate's request id
only when it is bound to that approval:
- typed under that approval's card on the dashboard (`note_id`, held to the newest open question as before), or
- typed in the desktop's chat with that approval on screen (the chat sends `seen_at`, when its newest entry was
  posted) and no other question open.
A chat reply that would land on an approval posted after what the person saw, or on one of several open questions,
is refused with a reason. Since the review of 2026-11-03 the chat's `seen_at` is frozen at the first keystroke of a
draft, approval bubbles carry Approve and Deny bound to the note id, and the concierge binds a reply by `note_id` only
when the newest open question is a gate-worded approval (a bare ok, sure, go or yes never answers one there). Every
other door (the requests page, an API call) delivers the reply as a message bound to no request: it grants nothing.

**The gate's question is the gate's words only.** With no reason of its own the gate used the caller's reason in its
signed question; it now uses a fixed one (`gate_server.GATE_REASON`). The worker's text reaches the person only as
the worker's note.

## Round 5 (2026-11-03, after the round-5 recheck)

Guard rails, not the boundary, but each closed a hole the recheck found:
- **The shell checks read words, not text.** `commands._rm_wipe_reason` (agent) and `safety.wipeConcern` (TheBot) split
  a command as the shell does, unquote it, follow `sh -c`, `env -S` and shell-fed here-documents, track `cd`, and resolve
  each path rm is given. The Python side also steps over wrappers (sudo, env, nohup, timeout N, xargs...) to the program.
- **A request is local only when every target is.** `safety.onlyLocalTargets` reads each curl and wget's targets, and a
  proxy, `--resolve`, `--connect-to`, `-K`, a proxy variable or a curl/wget rc file in the home means not local.
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
(`desktop.GateText`). Values are signed over the SHA-256 of each value's SHA-256 followed by a NUL.
- **`/request`**: the gate's own copy of each new request, never of a reply. When anything of it was withheld the copy
  is the withheld form: a site's name is kept, a value never is, and a request marked secret is withheld whole, so it
  names no site. Whether a password may go to a site (`approval.site_known`) is read from this copy, never from the
  worker's task file, and only for the task the action belongs to: a copy of another task names nothing
  (`approval.gate_request_text`).
- **`/secrets`**: the values the records withheld from a request or a reply (`httpapi.markWithheld`, the one C14
  decision), and the secret parameters of a replay or a skill run. The gate withholds them from the questions it words.
- **`/reply`**: the person's reply, signed with the gate's request id when it is bound to that approval. It is the
  only way a yes is made (above).
- **`/task-ended`**: sent on every task close (done, error, stop, restart, delete, abandon; `tasks.Store.OnTaskEnded`
  calls `Manager.GateTaskEnded`). The gate then drops that task's values, and its request copy when the copy is that
  task's (below).

**Sent in order, failing closed.**
- **A request** (`httpapi.deliverRequest`, once the desktop answers, on every try): `/secrets`, then `/request`, then
  the agent. If the gate cannot be told (no answer, or not 2xx), nothing reaches the agent and the sender gets 503. A
  desktop with no `decurix.gate` label whose gate does not answer gets "recreate this desktop" (`ErrGateMaybeMissing`).
- **Teach, replay and run-skill** (`httpapi.sendTaught`) do the same before calling their own agent endpoints. The copy
  is their request line, withheld. It names no site, so the gate asks about every password a taught skill types. A
  replay's parameters are held when its recording's program marks them secret or does not list them; when the
  program cannot be read, every value given is held.
- **A reply** (`httpapi.deliverRequest` too, once the desktop answers): the reply's withheld values by `/secrets`,
  then `/reply`, then the agent, so the gate already holds the reply's values when it settles it. `/reply` is given
  once, on the first try whose `/secrets` the gate took. A refused `/secrets` stops the reply (503) and nothing
  reaches the gate's `/reply` or the agent. A `/reply` the gate refuses or does not answer is only logged, and **the
  reply is still delivered** to the agent: it grants nothing there (only the gate makes a yes, from a `/reply` it
  verified), so an approval fails closed, while an answer to a question still reaches the worker as the person's text.
- **A task's end** never holds up the close. The notice is sent in the background. One the gate did not take is kept
  (at most 256 per desktop, oldest dropped) and sent, oldest first, before the next `/request`, `/secrets` or `/reply`
  to that gate, without failing that delivery. Kept notices are in memory: a control-plane restart loses the ones not
  yet taken, and the gate's own 24 h and 4 MB bounds cover them.

**The gate's own state.** The request copy (`gate-request.json`, the latest request with its task) and the values
(`gate-secrets.json`, per task) are 0600 files in the gate's directory. The nonces it has seen are kept in memory only,
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

## Next: the input broker (proposed, not built)

The gate decides whether a yes exists; nothing yet stands between the worker and the screen. The worker's Linux user
owns everything that can act on the screen or as the person, so each of these is a click on Pay without asking:
1. XTEST from any X client of the worker (`xdotool`, python-xlib), and XSendEvent to the browser's windows.
2. The VNC server itself: the worker can connect on localhost with the password in `VNC_PW` and send RFB input.
3. Chrome's DevTools port (9222), which `computer/cdp.py` uses.
4. AT-SPI: the accessibility bus offers `doAction` on any button of any app.
5. The browser's profile: its cookies are the worker's files, so `curl` can act with the person's session directly.
6. `ptrace` and `/proc` of the browser and the agent (same user): the agent token is one such read.

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

## What this does not cover (said plainly)

- **Direct input.** The gate decides whether a *yes* exists. It does not stand between the worker and the screen: a
  worker that runs `xdotool` or drives Chrome's DevTools port can click Pay without asking anyone. Closing that needs the
  X server and the browser behind the gate's user, with `gui` going through a broker. Until then the guard rails
  (`claude_guard.py` for Claude, the shell checks for the others) are filters.
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
- TheBot's `internal/safety` carries a FROZEN note. Its last five structural fixes are written by input class in
  [follow-ups.md](follow-ups.md), O-1.a.
- The Claude harness's PreToolUse guard runs for `mcp__computer__.*` too. `open` may not name an input program, a local
  port or the gate's state, and a call the guard cannot read is refused.
- Commands soul runs start without `DECURIX_AGENT_TOKEN`, `*_TOKEN` and `*_API_KEY` (`OPENAI_API_KEY` and
  `OLLAMA_API_KEY` stay, because the gui tool reads them), so a publish or push the guard misses has no credential.

**X.** Xvnc is told `-nolisten tcp`, and the agent terminal no longer runs `xhost +`: X access control stays on, with
only the worker's local user (`+SI:localuser:headless`). Recreate desktops to pick both up.

**Checks the owner runs.** `bench/security/boundary.sh` and `bench/security/gate_flow.py` (the keep-it confirmation
through the gate) on the next image; see [follow-ups.md](follow-ups.md).
