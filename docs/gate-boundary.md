# The approval gate as a boundary (design, round 3)

Status (2026-11-03): built and checked live on a layered test image: the image lockdown (step 1), the gate daemon, the
signed reply from the control plane, the agent's client, and soul taking its reading from the gate. Not done: a full
image build and a real task end to end; existing desktops keep the old setup until recreated.

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


## What is built (2026-11-03)

- `agent/decurix-entrypoint.sh` starts as root, hands `/dockerstartup` and `$HOME` over, locks `/etc/passwd`, starts the
  gate as `decurixgate` with the key (`DECURIX_GATE_KEY`, derived by `desktop.Manager.GateKey`), **removes the key from
  the environment**, and drops everything else to `headless` with no capabilities and no-new-privileges.
- `agent/decurix_agent/gate_server.py` is the gate: TCP :8083 `POST /reply` (signed by the control plane, fresh, one
  use) and a unix socket `/run/decurix-gate/gate.sock` with calls that make no grant (`check`, `pending`, `has_grant`,
  `reset`, `outcome`, `reading`, `last_reading`). Its state is in `/var/lib/decurix-gate` (0700).
- `computer/approval.py`: with `/run/decurix-gate` present every decision goes to the gate; a gate that does not answer
  allows nothing; the old local store is used only on a desktop without a gate.
- The control plane signs every reply to an open question (`httpapi.signReplyToGate`, `desktop.SendGateReply`) before it
  gives the agent the same text. The agent asks the gate what it did with that reply (`resolve_reply`).
- TheBot (`cmd/soul/gate.go`): with a gate, a paused approval is resumed only on the gate's reading; the reading in the
  request is ignored.

Checked live (layered image): the worker could not read the gate's directory, its `/proc/<pid>/environ` or the key; a
forged `approval.json`, an unsigned reply, a reply signed with the agent token, a replayed nonce, and the calls
`grant`/`note_reply`/`resolve_reply` on the socket all failed; a reply signed with the real key granted that one action
once.

## What this does not cover (said plainly)

- **Direct input.** The gate decides whether a *yes* exists. It does not stand between the worker and the screen: a
  worker that runs `xdotool` or drives Chrome's DevTools port can click Pay without asking anyone. Closing that needs the
  X server and the browser behind the gate's user, with `gui` going through a broker. Until then the guard rails
  (`claude_guard.py` for Claude, the shell checks for the others) are filters.
- **The agent token** is still in the agent's environment and readable by the worker (`/proc/<agent>/environ`). With it
  the worker can start tasks as a "person" and call the agent's API. It can no longer make a yes.
- **The browser extension server** (:8092, off unless `THEBOT_BROWSER_EXTENSION=1`) still takes the approvals it
  stopped on from any caller with the agent token.
- **Records.** A long run still writes "You approved" from a file in the command folder; that is a record, not a grant.
- **Old desktops** (no `/run/decurix-gate`) keep the in-process store and the old image's root access until recreated.
- **A fixed `hostgui.key`** change (e.g. after a restore onto another machine) changes every derived key: recreate desktops.
