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
- The worker cannot become root: no sudo, no setuid helpers but chrome-sandbox, `/etc/passwd` 644, `NET_RAW` dropped,
  and every desktop process runs with no capabilities and no-new-privileges.
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

## What this does not cover (said plainly)

- **Direct input.** The gate decides whether a *yes* exists. It does not stand between the worker and the screen: a
  worker that runs `xdotool` or drives Chrome's DevTools port can click Pay without asking anyone. Closing that needs the
  X server and the browser behind the gate's user, with `gui` going through a broker. Until then the guard rails
  (`claude_guard.py` for Claude, the shell checks for the others) are filters.
- **What the gate is told.** The gate does not see the screen: the action's description comes from the worker's side.
  A lying caller can only get a grant for the action it described, but with direct input it needs none.
- **The agent token** is still in the agent's environment and readable by the worker (`/proc/<agent>/environ`). With it
  the worker can start tasks as a "person" and call the agent's API. It can no longer make a yes.
- **The browser extension server** (:8092, off unless `THEBOT_BROWSER_EXTENSION=1`) still takes the approvals it
  stopped on from any caller with the agent token.
- **A root `docker exec` by a person.** `docker exec -u 0 <desktop> sh` finds `sh` through the container's PATH, which
  puts the worker's `~/.local/bin` first: it runs the worker's code as root. The control plane never execs as root.
  Anyone else uses absolute paths and `env -i` (the bench caught itself doing this).
- **Practice desktops** get no key, so they have no gate (unchanged).
- **A fixed `hostgui.key`** change (e.g. after a restore onto another machine) changes every derived key: recreate desktops.
