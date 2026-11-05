# Handoff: workers that hand work to each other

Before handoffs, workers only cooperated through a fixed pipeline (`internal/dag`: one node's final text pasted into
the next node's request), and a stuck worker could only ask a person. A handoff is a typed request from one worker to
another, sent while the first worker's own task is still running, whose result comes back with the receiving
worker's final-gate verdict attached.

Code: `internal/handoff` (store, service, HTTP handler), `agent/decurix_agent/tools/handoff.py` (the `handoff` shell
command, wrapper `agent/handoff`), `agent/thebot/internal/skills/handoff_result.go` (the soul reads printed results).

## The record

| Field | Meaning |
|---|---|
| `id` | `ho_` + 16 hex |
| `kind` | `delegate` (work) or `ask` (a question) |
| `from_worker`, `from_task` | who sent it, and the task it was sent from |
| `to_worker` / `to_role` / `to_skill` | exactly one is given; a role or skill is resolved to a worker when it is sent |
| `request`, `inputs` (name -> value), `files` (paths), `expected` | what the receiver must do, with what, and what done must show |
| `depth` | how many handoffs led to this one (`MaxDepth` 3) |
| `status` | `queued`, `running`, `done`, `not_done`, `failed`, `cancelled` |
| `reason` | why it is where it is, in words a worker can act on |
| `task_id` | the receiving task, in the ordinary task records |
| `result` | `report`, `verified`, `evidence[]`, `files[]`, `open_questions[]` |

Stored in SQLite (`handoffs`, component `handoff`, append-only `Migrate` steps).

## How a handoff runs

1. `POST /api/handoffs` from the sending desktop (`desktop_id` in the body, like every agent callback). Validated:
   one target, a request, at most 40 inputs and 20 files, every file under `/home/headless` (the sending `handoff`
   command refuses, before anything is sent, a file outside the work folder, the shared folder and `~/Downloads`, any
   file under a hidden folder such as `~/.claude` or `~/.ssh`, and the worker's own `~/.decurix`:
   `privacy.file_may_leave`), a known sender and
   receiver, not the sender itself, and not a worker already waiting up the same chain (A -> B -> A would never finish,
   since A is busy waiting on B).
2. Sending. A named worker is used as named; a role or skill is resolved through `WorkerFinder.Holders` to the first
   holder that is not the sender, not up the chain, and free (`tasks.Store.Busy`). The files are read out of the
   sender's desktop and written into `/home/headless/handoffs/<id>/` on the receiver, and the request goes through the
   same automated door every other sender uses (`SubmitTracked` / `SubmitTrackedWithFiles`: the per-desktop send lock
   and busy check). A busy receiver leaves the handoff `queued`; `Service.Run` tries again every 15 s, and one that no
   worker takes in 30 minutes fails. No holder at all fails it at once.
3. The receiver works the request like any other request. Its own final gate (`verify.py`, `skill_task._gate`) judges
   it, and its final reply reaches the task records with `checked`.
4. `GET /api/handoffs/get?id=` (and the runner) read the receiving task and turn it into the result.

## No unseen success across a handoff

The sender cannot see the receiver's screen, so the only evidence that the work is done is the receiver's own gate.

- `done` with `verified: true` only when the receiving task ended `done` **and** its execution's `checked` is true.
- A task that ended `done` without `checked` (no gate ran, the check could not settle it, an older image that does not
  say) is `not_done`, `verified: false`. The report is kept so the sender can see what was claimed, never counted.
- A receiving task that ended `error` or `abandoned` is `failed`.
- A receiver waiting on a person stays `running`; its unanswered questions are in `open_questions`.
- `evidence` is what the receiver's gate wrote it saw (its `Checked ...:` lines and its "Values read from" block).
  `files` are the files the receiver saved in `~/handoffs/<id>/out/`, copied back to the sender under
  `/home/headless/handoffs/<id>/returned/`. The request tells the receiver to save there. Only a verified result
  returns files, and only from that folder: never a file a report merely names (C-06, review 2026-11-03).

On the sender, the `handoff` command prints one fixed block (`=== HANDOFF <id>: DONE (verified) ===` ... `=== END
HANDOFF <id> ===`). Anything but `DONE (verified)` tells the model not to report that part as done. The soul's report
review (`handoffIssues`) raises a Work point for every handed-back result that was not verified unless the report names
the handoff and says it is not done. The sender's own final gate still judges the sender's whole report as before.

## Asking a worker before a person

`POST /api/handoffs/ask {desktop_id|from_worker, from_task, skill, question, wait}` puts a question to a free worker
that holds the skill, as a short task: answer from what it was taught, change nothing, say "I do not know" otherwise.
No free holder fails it at once, so the asker goes to a person without waiting. The route holds the request up to 60 s
for the answer and returns `{handoff, answer, verified, answered_by}`; the command polls after that (default 300 s).
An answer from what a worker was taught is not something anyone saw: it nearly always comes back `verified: false`,
and the command prints it as advice. It can save a person a question; it never stands in for checking the work.

## Routes and listener

`POST /api/handoffs`, `GET /api/handoffs?worker=`, `GET /api/handoffs/get?id=`, `POST /api/handoffs/cancel`,
`POST /api/handoffs/ask`. They go on the primary listener only: the callers are desktop agents, a worker is
identified by the `desktop_id` it sends (trusted only on the listener the desktops reach and nothing outside this
machine does), and creating a handoff starts work on another desktop and moves files between desktops.

## Limits

- Cancelling stops collecting the result; a receiving task already running is not stopped (it says so in `reason`).
- A question times out after 15 minutes; a delegated task has no running time limit (the receiver may be waiting on a
  person), only the sender's own `--wait`.
- Chains are at most 3 deep and never hand back to a worker waiting up the chain.
- Files: at most 20 paths under `/home/headless`, 64 MB in all; folders and links are not carried.
- A stop ("Stopped, as you said") is reported `kind="error"`, not checked, since the review of 2026-11-03, so a
  receiver that stopped at a person's no fails the handoff instead of reading as verified.
- A receiving task that ended `query` fails the handoff. Delegated work has a 24 h ceiling.
- A queued handoff is claimed `sending` (shown as queued in JSON) and submitted without the service lock. A stale
  claim fails after 15 minutes.
- Knowledge GETs from a desktop are bound to the caller: its own worker scope, its own tasks only. Org skill and
  knowledge writes are host-operator only (owner decision O-7).
- One control-plane process: the service's mutex serialises the read-then-write of a handoff row.
