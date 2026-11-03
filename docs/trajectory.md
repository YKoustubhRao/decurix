# The run record: decurix.trajectory/v1

A **run** is one execution of a task on a worker. Its **trajectory** is the one document that joins what each model
saw and said, what the worker did, and what the checks and people decided. It is the substrate the evals, the
expert loop and training-set exports read (plan R1).

- The control plane builds it (`internal/runs`, `runs.Build`); a desktop never writes it. Only the control plane
  outlives desktops and sees every input.
- The joins are by id (task, execution, turn, record id), never by recomputing a hash on two sides.
- Go types: `internal/runs/trajectory.go`. Python types and a validator: `agent/decurix_agent/trajectory.py`
  (`validate(doc) -> list[str]`). Three examples, made by `runs.Build` from fixtures in its golden tests:
  `internal/runs/testdata/{hosted,local,stopped}.json`; both languages validate them.
- Served by `GET /api/runs/<execution>/trajectory` and, with the bundle's files, `GET /api/runs/<execution>/bundle`
  (primary listener, humans only).
- A reader keeps fields it does not know: a v1 reader reads a v1.1 document.

## Where it comes from

| Part of the document | Source | Written by |
|---|---|---|
| run, task, attempt, status, outcome, request, questions | the task records (`tasks`) | control plane |
| origin, skill | the execution (R7) | control plane, from the door and the final callback |
| verdict | `executions.verdict` (R6, decurix.verdict/v1) | the desktop's final gate; the control plane stores it |
| judgements, each answer's actor | the judgements table (R5) | control plane, from the door's actor |
| calls, models, usage, prompt hashes | the proxy's call log, `llm_calls` (R2) | control plane |
| steps: observation, action, diff | the recording's `events.jsonl`, `frames/`, `obs/` (bundle) | the desktop's action layer |
| steps: command, guard, turn | soul's trace (bundle) | soul |
| ledger | `<task>.records.json` (bundle) | the desktop's record ledger |
| worker hashes, harness, brain | `run.json` (bundle) | the desktop |
| labels.eval | `eval_rows` | the eval runner |

### The bundle (R9)

What `runs.Puller` reads off the desktop, as the worker (`desktop.Manager.ReadTar`, never Docker as root), when an
execution ends and before a desktop is deleted. Paths are under the worker's home:

| Part | Path |
|---|---|
| recording | `.decurix/recordings/task-<task>/` (meta.json, events.jsonl, frames/, obs/) |
| notes | `.decurix/computer/evidence/<task>.jsonl` |
| ledger | `.decurix/computer/evidence/<task>.records.json` |
| files, outputs | `.decurix/computer/evidence/<task>.files`, `.outputs` |
| run | `.decurix/runs/<task>/` (run.json, LABELLED, learned.jsonl) |
| soul_trace | `.decurix/ai/traces/<task>.jsonl` |

- Every regular file becomes a blob, by SHA-256 (`internal/blobstore`); the manifest (`run_bundles`) lists each
  file's path, SHA and size. `manifest_sha` is the SHA-256 of the canonical JSON of that list, sorted by path.
- A part that is not there is listed in `missing` by name (`soul_trace: not present`); a read that fails, with a short
  reason. Never silently empty.
- Only regular files are kept. A tar entry outside its part, a link or a device is refused and listed.
- **The labels come first and are kept whole.** The run folder, ledger, notes, files, outputs and soul's trace, then
  the recording's `meta.json` and `events.jsonl`, are each read on their own (up to 64 MB a part; one past that is
  cut, and said). Nothing the recording holds can crowd them out.
- **The recording's observations, `program.json` and frames share one limit** (64 MB a run), each folder read as its
  own bounded tar. Over the limit, frames are dropped first, then observations, oldest first by the events, each
  named in `missing`. A tar cut at its read bound keeps what was read before the cut, and says so.
- **At most 2000 files a label part and 20000 of the recording** are kept, each part counted on its own; `missing`
  stops at 200 lines and a count.
- **A later pull of the same execution merges with the earlier one by path**: a file the desktop has pruned since
  stays referenced (its `not present` line says it was kept from an earlier pull), and a path whose content changed
  keeps the new file, with the old one in the bundle's `history`. Rows and blobs are never deleted (plan rule 4).
- A desktop that is not there saves nothing (`runs.ErrDesktopGone`), as a pull that finds no part at all does
  (`runs.ErrNothingFound`).
- After the bundle is saved, the control plane writes `~/.decurix/runs/<task>/EXPORTED` (the execution id, then the
  manifest SHA, one line each). The desktop's prune keeps a labelled run until that file is there (W0.8).
- An execution with no bundle still has a trajectory: no steps, and `integrity.missing` says "no bundle".

## The document

```jsonc
{
  "schema": "decurix.trajectory/v1",
  "run_id": "e_...",                     // the execution id
  "task_id": "t_...", "attempt": 1,
  "worker": {"id", "name", "harness", "brain", "image", "agent_sha", "soul_sha", "rules_sha"},
  "origin": {"kind": "person", "ref": "chat", "actor": {"id", "roles", "kind"} | null},
  "skill": {"name": "", "version": 0},   // empty when no taught skill drove the run
  "request": {"text": "(as the records keep it: withheld)", "sha256": "...", "files": []},
  "started_at": "RFC 3339", "ended_at": "RFC 3339 or empty",
  "status": "done",                      // the task records' status
  "outcome": "done_checked",             // tasks.DisplayOutcome's state
  "verdict": { /* decurix.verdict/v1 */ } | null,   // null for a run from before verdicts
  "models": [{"provider", "deployment", "calls", "purposes": {"operator.turn": 27, "review": 2}}],
  "usage": {"input_tokens", "cached_input_tokens", "output_tokens", "calls"},
  "steps": [ /* Step */ ],
  "calls": [ /* Call: every model call of the run, in order */ ],
  "questions": [{"intervention_id", "kind", "asked_at", "text", "secret",
                 "answer": {"judgement_id", "at", "text", "actor"} | null}],
  "judgements": [{"id", "kind", "actor", "at", "derived_from", "payload"}],
  "ledger": [{"key", "source", "status", "fields": [{"label", "intended", "at_save", "shown", "status"}]}],
  "labels": {"run": "done", "person_verdict": "", "eval": null},
             // run: the run's verdict (done | not_done | unchecked | stopped | "" before verdicts), never a step's;
             // eval: {suite, case, arm, repeat, passed, false_done}
  "integrity": {"bundle_sha256", "parts": {"recording": "sha256:..", ...}, "missing": [...], "worker_written": [...]}
}
```

- **calls**: each proxy row (`source: "proxy"`) with its id, time, deployment, purpose, turn, caller, status, ms,
  `prompt_sha256` and `prefix_sha256` (over the stored message hashes, so two calls that share a prefix share
  `prefix_sha256`), `response_sha256`, usage and problems. Only rows whose desktop is the execution's worker, made
  between its start and a minute after its end, are joined: the task id in a header is the desktop's word.
  With no proxy rows (a local brain the proxy never sees), the calls are soul's trace's call lines
  (`source: "soul_trace"`, no bodies, `deployment` = the model soul asked for).
- **models** and **usage** are summed from `calls`.
- **Only this execution's own part of the task's files.** The recording, soul's trace and the ledger are kept per
  task and grow over its attempts (a retry is a new execution of the same task). A trajectory keeps the events and
  trace lines from its execution's start (less 2 s) to a minute after its end, and the ledger records declared in
  that window; `integrity.missing` says how many of other attempts it left out. An event or record with no time is
  left out too.
- **ledger** status: `matched` (saved, every intended value read back), `differs` (saved, a value not shown),
  `saved` (not read back), `declared` (never saved). A field is `matched`, `differs` or `unchecked`.
- **integrity.parts**: a part's SHA (a one-file part: the file's; a folder: the manifest SHA of its files);
  `manifest` is the run folder. `worker_written` lists every part the worker's own Linux user could have written.

### Step

A step is one action the worker took (a recording event), or one command soul ran that left no event. Steps are in
time order; a tie is broken by turn.

```jsonc
{
  "i": 0, "at": "RFC 3339",
  "turn": "j42:7",                 // soul's turn ("<job>:<n>"); "" for a step no soul turn made
  "call": 6,                       // index into calls of the call that chose it; null when none is known
  "model": {"provider", "deployment"} | null,
  "prompt_sha256": "...", "prefix_sha256": "...",
  "observation": {"frame": "<before frame>", "ax": "<obs name>", "window", "url"} | null,
  "command": {"text", "tool", "exit", "ms", "output_sha256", "guard"} | null,  // guard: allowed | asked | refused
  "action": {"record_id", "kind", "target", "text", "withheld", "backend", "outcome", "actor"} | null,
  "gate": {"decision", "request_id", "judgement_id"},
  "diff": {"before", "after", "change", "expect"} | null,
  "ledger_delta": [],
  "label": "ok"                    // ok | failed | refused | corrected | overridden | unlabelled
}
```

How the parts are joined:

- **Command to event.** The recording event carries its turn (`DECURIX_TURN_ID`, C3). A soul command of the same
  turn is attached to the first event it made: the command whose run (its trace time, less its duration, with 2 s
  of slack for whole-second event times) holds the event, else the turn's first command. A command no event took is a
  step of its own with `action: null`.
- **Call.** The last call of the step's turn at or before the step's time. A step with no turn, or before its turn's
  first call, has `call: null`.
- **Gate.** An event whose outcome is `needs_approval`, or a command soul's guard asked about, is `asked`; the next
  approval intervention's reply judgement then makes it `granted` (`payload.decision` = yes) or `denied` (no). A
  command that ran after being asked is `granted`. An event or command the gate held with nobody asked is `refused`.
  Anything else is `not_needed`.

### Step labels

A step's label is **that step's own outcome**, derived by `runs.Build` and never written by the desktop. The run's
verdict is the run's label (`labels.run`); it is never copied onto steps. The first that holds:

- `overridden`: the event's actor is `human` (a person acted on the screen), or the approval for the step was
  denied;
- `corrected`: a `correction` judgement on the run names the step's record (its ledger key) or field (the label of
  the element the step acted on), or that ledger field ended `differs` and a person answered after the step;
- `refused`: the gate held it and nobody was asked;
- `unlabelled`: it was asked about and never answered, or nothing says how it ended;
- `failed`: its action failed, or its command exited non-zero (a `gui` command's exit 3, the gate holding it, is not a
  failure);
- `ok`: its action was done, or its command exited 0.

A training export takes steps by their own label and runs by `labels.run` (with the verdict's basis: W0.5 counts
`code`, `ledger` and `person`).

## decurix.verdict/v1

The final gate's structured verdict (PLAN-P2 §5.2; written by `agent/decurix_agent/verdict.py`, stored by the control
plane as `executions.verdict`).

```jsonc
{"schema": "decurix.verdict/v1",
 "verdict": "done",               // done | not_done | unchecked | stopped
 "by": "verify",                  // verify | skill_gate | long_task | replay | workflow | worker | none
 "why": "...",                    // at most 600 characters, withheld
 "basis": ["code", "model"],      // the kinds of check that passed: code | ledger | screen | model | person
 "checks": [{"id", "result": "pass | fail | not_made", "basis", "detail", "refs": []}],
 "evidence": ["Checked on the screen: ..."]}
```

`done` needs at least one check that passed and none that failed or was not made. `stopped` (the person said no and
the work stopped) is never done. At most 64 checks, 16 refs a check, 600 characters a string.

## decurix.run-manifest/v1

`~/.decurix/runs/<task>/run.json`, written by the desktop at a run's end (C8):

```jsonc
{"schema": "decurix.run-manifest/v1", "task_id", "desktop_id",
 "agent_sha": "sha256:<tree hash of the agent code>", "soul_sha": "sha256:<soul binary>",
 "harness": "thebot", "brain": "azure", "started_at": 1762240931, "ended_at": 1762241380,
 "parts": {"recording", "notes", "ledger", "soul_trace"}, "labelled_by": ["final"]}   // final | reply
```

## decurix.soul-turn/v1 (soul's trace)

`~/.decurix/ai/traces/<task>.jsonl`, one line per model call and per command the operator ran (`agent/thebot/internal/trace`).
Soul never reads it back. It goes through soul's secrets filter (the task's own secrets, then `memory.RedactSecrets`)
before it is written; files are 0600 and capped at 32 MB, past which one last line `{"kind":"truncated"}` is written.

```jsonc
{"schema": "decurix.soul-turn/v1", "at", "task", "job": "j42", "turn": "j42:7", "kind": "call",
 "purpose": "operator.turn", "model": "default", "provider": "openai",
 "prompt_sha256": "<SHA-256 of the message list's JSON as soul holds it>", "messages": 14,
 "usage": {"in": 30122, "cached": 28800, "out": 211}, "ms": 2140, "command": null}
{"schema": "decurix.soul-turn/v1", "at", "task", "job", "turn", "kind": "command",
 "command": {"text": "<redacted>", "guard": "allowed", "exit": 0, "ms": 830,
             "output_sha256": "<of the redacted output>", "output_head": "<first 2 KB, redacted>"}}
```

The turn is the same one soul sends the proxy (`X-Decurix-Turn`) and gives each command (`DECURIX_TURN_ID`), so the
three records join on it. A trace call's `prompt_sha256` is the SHA-256 of Go's JSON of soul's `[]llm.Message`, not of
the bytes the provider was sent; a proxy row's is over the stored message hashes. The two can never be compared: join
on the turn.

What soul writes or serves keeps no secret value (C14): a job's own values (`secrets` on `/api/chat`, plus those of
the paused job it resumes) and what soul knows by structure (a value given after a credential's name, a value typed
into a password field) are withheld before anything is shortened or quoted, by one filter for the step line, the trace,
the outputs file and an approval's quoted action. Another job's values never touch this job's text.

## v1 limits

- **`ledger_delta` is always `[]`.** The ledger does not name the event of its save, so no step can be said to have
  changed a field. The ledger itself is in `ledger`.
- **A ledger field's `shown` is `""`.** The ledger keeps whether each intended value was seen on the page after the
  save, not what the page showed.
- **A local brain's calls have no bodies.** The trace keeps their model, prompt hash and usage only; bodies for local
  calls are a later phase.
- **Every bundle part is `worker_written`** until the evidence store sits under the gate user (O-1): the worker's own
  user can write its recording, ledger, notes, trace and manifest.
- **`request.files` is `[]`** and **`worker.image` is `""`**: neither is kept per execution yet.
- **Bundles of practice and eval desktops are not pulled yet**: they have no home volume to read after a reset.
- **The bundle limit is fixed at 64 MB a run**; past it frames are dropped, oldest first, and said.
