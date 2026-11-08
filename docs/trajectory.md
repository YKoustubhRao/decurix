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
| labels.eval | `eval_rows` (`eval.Store.EvalLabel`) | the eval runner |
| worker.rules_sha | `run.json` (bundle), from soul's `traces/<task>.rules` | soul, then the desktop's manifest |

### The bundle (R9)

What `runs.Puller` reads off the desktop, as the worker (`desktop.Manager.ReadTar`, never Docker as root), when an
execution ends and before a desktop is deleted. When (`internal/httpapi/runpull.go`): the final or error callback
queues a pull (128 deep; a full queue drops it, and says so), tried 5 s later and up to 3 times with a growing wait; a
delete first pulls every run of that desktop not yet pulled, for at most 90 s, and while any is still not pulled the
delete is refused (409, `held`, `unsaved_runs`: "N runs of this desktop are not saved to the run record yet; try again
in a minute, or delete anyway"); the dashboard then offers "Delete anyway", which sends it again with `force=1`. A forced
delete is a person's choice, recorded before the desktop goes as a judgement of kind `delete_anyway` (rule 5: the
actor, the door `card` or `phone`, the worker, and in its payload `runs: [{execution_id, task_id}]` given up and
`flush_seconds`); no actor (a desktop's own call) cannot force it (403), and each run given up is logged; a restart catches up on the last 7 days' runs. An eval case's bundle, and a Simulate case's, is pulled off its practice
desktop before the next case's reset (`desktop.PracticeHome`, [evals.md](evals.md), [simulation.md](simulation.md)).
Paths are under the worker's home:

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
- **The recording's observations, `program.json` and frames share one limit** (64 MB a run by default;
  `DECURIX_RUN_BUNDLE_MB`, 8 to 1024, read once at start: [running.md](running.md)), each folder read as its
  own bounded tar. Over the limit, frames are dropped first, then observations, oldest first by the events, each
  named in `missing`. A tar cut at its read bound keeps what was read before the cut, and says so.
- **At most 2000 files a label part and 20000 of the recording** are kept, each part counted on its own; `missing`
  stops at 200 lines and a count.
- **A later pull of the same execution merges with the earlier one by path**: a file the desktop has pruned since
  stays referenced (its `not present` line says it was kept from an earlier pull), and a path whose content changed
  keeps the new file, with the old one in the bundle's `history`. Rows and blobs are never deleted (plan rule 4).
- A desktop that is not there saves nothing (`runs.ErrDesktopGone`), as a pull that finds no part at all does
  (`runs.ErrNothingFound`).
- After the bundle is saved, the control plane writes `~/.decurix/runs/<task>/EXPORTED` (`runs.writeExported`): the
  execution id, the manifest SHA-256, then the run folder's `LABELLED` as that pull read it, trailing newlines
  trimmed. `LABELLED` is two lines of its own (the execution id and the label's sequence number), so `EXPORTED` is
  four lines; with no `LABELLED` the third line is empty. The agent (`retention.held`) compares everything after the
  second line with `LABELLED` now, since a tar copy rounds file times to the second. An `EXPORTED` of two lines (from
  before the copy was added) is compared by execution id and time, with two seconds allowed. The desktop's prune keeps
  a labelled run until an export covers its latest label (W0.8).
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
             // eval: {run_id, suite, suite_sha, case, arm, repeat, observed, passed, false_done, invalid, infra,
             //        carry_state} for a run an eval case made, else null
  "integrity": {"bundle_sha256", "parts": {"recording": "sha256:..", ...}, "missing": [...], "worker_written": [...]}
}
```

- **calls**: each proxy row (`source: "proxy"`) with its id, time, provider, deployment, purpose, turn, caller, status,
  ms, the number of messages, `prompt_sha256` and `prefix_sha256` (over the stored message hashes, so two calls that share a prefix share
  `prefix_sha256`), `response_sha256`, usage and problems. Only rows whose desktop is the execution's worker, made
  between its start and a minute after its end, are joined: the task id in a header is the desktop's word.
  With no proxy rows (a local brain the proxy never sees), the calls are soul's trace's call lines
  (`source: "soul_trace"`, no bodies, `deployment` = the model soul asked for, `caller: "soul"`, a failed call's
  problems `["failed"]`).
- **models** and **usage** are summed from `calls`.
- **labels.eval** is read back from the eval rows (`runs.Sources.Evals`, `eval.Store.EvalLabel`): the row whose
  `trajectory_id` is this execution, else a row of this task that names no trajectory (its bundle could not be
  pulled; the case's run is still the eval's). Invalid and infrastructure rows are labelled as such, never left out.
  A read that fails leaves it `null` and says `labels.eval: the eval rows could not be read` in `integrity.missing`.
- **worker.rules_sha** is the SHA-256 (`sha256:<hex>`) of the operator's rules text soul gave the task's latest job
  (`rulesFor` in `skills/operator.go`, hosted brains only): soul writes it to `~/.decurix/ai/traces/<task>.rules`
  (`trace.Rules`, the hash only), and the desktop's manifest copies it (`retention.rules_sha`). `""` when soul gave no
  rules (a local brain, another harness) or ran before this was built.
- **Only this execution's own part of the task's files.** The recording, soul's trace and the ledger are kept per
  task and grow over its attempts (a retry is a new execution of the same task). A trajectory keeps the events and
  trace lines from its execution's start (less 2 s) to a minute after its end, and the ledger records declared in
  that window, but never into a neighbouring attempt's time on the same worker: the window stops at that worker's
  next attempt's start and begins where its attempt before stopped, so a retry started right after a failure is the
  retry's from its first second. An event's time is the recording's start plus its `t` (to the millisecond), not its
  whole-second `at`. The proxy's calls are clipped to the same window. `integrity.missing` says how many records it
  left out as "outside this execution's window"; an event or record with no time is left out too.
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
 "by": "verify",                  // verify | skill_gate | long_task | replay | workflow | worker | none,
                                  // or the control plane's own graders: readiness | practice | eval
 "why": "...",                    // at most 600 characters, withheld
 "basis": ["code", "model"],      // the kinds of check that passed: code | ledger | screen | model | person
 "checks": [{"id", "result": "pass | fail | not_made", "basis", "detail", "refs": []}],
 "evidence": ["Checked on the screen: ..."]}
```

`done` needs at least one check that passed and none that failed or was not made. `stopped` (the person said no and
the work stopped) is never done. At most 64 checks, 16 refs a check, 64 evidence lines, 600 characters a string
(counted in characters, never bytes), 64 KB in all; the desktop keeps its own under 12,000 bytes (`verdict.bound`),
since a callback body is read up to 32 KB. Every path that reports checked says what it checked: a learned tool's run
(`tool_task._verdict`, by `verify`: the tool's exit code and the code-decided file facts as code checks, the judge's
reading as the model's) and a workflow's report (`workflow_task._run_verdict`, by `workflow`: each completed stage's
spec file checks passed in code, each failed stage a failed check, and `request.judged` not made, so a completed run is
`unchecked`, a failed one `not_done`). A report sent with no verdict is given one on the desktop (`verdict.derive`),
never a done: checked=True alone goes out `unchecked` (`test_verdict.NoReportReliesOnADerivedDone` keeps every caller
giving its own). The `legacy` gate (a done with no checks, H-PY-1) is gone: the control plane refuses it as an unknown
gate, and rows stored with one are still read. The control plane stores `checked` as the verdict says (done); a
callback whose own flag disagrees is stored not checked. A verdict that does not validate is stored as `unchecked` by
`none`. A final with no verdict (an image from before R6) that says checked is stored **not checked** (fail closed,
`tasks.checkedFrom`, logged as a disagreement); one that says not checked, or nothing, is stored as it says.

The control plane's graders write the same document: a readiness run, a Simulate case and an eval case each record
their grade on the case's task record (`by: readiness | practice | eval`, one check, basis `code`; `not_made` when the
evidence could not be read).

## decurix.run-manifest/v1

`~/.decurix/runs/<task>/run.json`, written by the desktop at a run's end (C8):

```jsonc
{"schema": "decurix.run-manifest/v1", "task_id", "desktop_id",
 "agent_sha": "sha256:<tree hash of the agent code>", "soul_sha": "sha256:<soul binary>",
 "rules_sha": "sha256:<the operator's rules text soul gave the run>",   // "" when it gave none
 "harness": "thebot", "brain": "azure", "started_at": 1762240931, "ended_at": 1762241380,
 "parts": {"recording", "notes", "ledger", "soul_trace"}, "labelled_by": ["final"]}   // final | reply
```

## decurix.soul-turn/v1 (soul's trace)

`~/.decurix/ai/traces/<task>.jsonl`, one line per model call and per command the operator ran (`agent/thebot/internal/trace`).
Soul never reads it back. It goes through soul's secrets filter (the task's own secrets, then `memory.RedactSecrets`)
before it is written; files are 0600 and capped at 32 MB, past which one last line `{"kind":"truncated"}` is written.
A file that already ends with that line is stopped when soul restarts, so a restart never adds another. Beside it,
`<task>.rules` holds `sha256:<hex>` of the rules text the task's latest operator job was given (above).

**Pruned once exported.** At each run's end the desktop (`retention.prune_traces`, from `write_manifest`) deletes a
task's trace and rules note when its run was exported (`EXPORTED` in its run folder), no later label holds it again
(`retention.held`), and the file was last written more than 3 days ago (`TRACE_KEEP_SECONDS`). A trace whose run was
never exported is never deleted.

```jsonc
{"schema": "decurix.soul-turn/v1", "at", "task", "job": "j42", "turn": "j42:7", "kind": "call",
 "purpose": "operator.turn", "model": "default", "provider": "openai",
 "prompt_sha256": "<SHA-256 of the message list's JSON as soul holds it>", "messages": 14,
 "usage": {"in": 30122, "cached": 28800, "out": 211}, "ms": 2140, "command": null}   // "failed": true on a call that failed
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

## Purging a leaked secret

Nothing that labels a run is deleted (plan rule 4), but a secret is not a label. When a value slipped past
withholding into the records, the owner purges it by value (`internal/purge`): `POST /api/privacy/purge` on the
primary listener (humansOnly, owner role; never on the gated listener, refused to a desktop), the "Purge a leaked
secret" control at the foot of the Tasks page, or `decurix --purge-secret --reason "..."`, which reads the value from
stdin and posts it to that route. It refuses a value shorter than 6 characters (too likely to match ordinary text) and
a purge without a reason.

It looks for the value as written, in each JSON-string escaping the recorder withholds (`llmrec.SecretForms`), and as
base64 of the value alone, in: every blob (model-call messages, answers and tool lists, run-bundle files, eval suites),
the `llm_calls` rows (`params`, `problems`), the judgements (`text`, `payload`, `changed`), the task records (request,
verdict note, final reply, verdict, steps, interventions), the chat (entries and steps, in the database and in memory)
and the concierge's messages, the query notes, and the run-bundle manifests (paths, missing reasons). Every copy is
overwritten by `[purged]` in place; nothing is deleted. Tables are rewritten one `UPDATE` each (SQLite scans), the
tables a purge must also read are paged, and blobs are streamed, so the work is bounded. One purge runs at a time.

**Joins.** A blob keeps its name, the SHA-256 it was stored under, so every row still opens it: `llm_calls.messages`,
`prefix_sha`, `prompt_sha` and `response_sha`, a bundle's `files`, a trajectory's `prompt_sha256`, `prefix_sha256` and
`integrity.parts`. In a blob the mark is padded or cut to the value's length (`[purged]____`), so a bundle's `bytes`
and a tar's entry sizes stay true. Beside the blob, `<sha>.purged` records the SHA-256 of its new bytes and the
purge's judgement (`blobstore.Purged`), so a check that hashes blobs tells a purged one from a corrupted one. A
bundle's `manifest_sha` stays the one computed at the pull. A later `Put` of the original bytes finds the name taken
and keeps the purged ones.

**The record of it.** The purge is a judgement of kind `purge` (rule 5): the person, the door `privacy`, the reason
they typed (with the value purged from it too), and the counts per store in its payload; never the value or a hash of
it. The answer is the same counts and the judgement's id. Afterwards the database is rebuilt (`VACUUM`, then a WAL
checkpoint) so no free page keeps an old copy; `compacted: false` says it could not be (a busy database), and the rows
are purged either way. **Not reached:** backups already downloaded, a gzip-compressed blob, an image's pixels, and
anything still on a desktop's own disk.

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
- **`worker.rules_sha` covers soul's operator rules only**: the worker's business rules (its memory entries, which
  Simulate pins as `Pins.RulesSHA`) and the organisation's knowledge are not in it.
- **The bundle limit is one setting for every run** (`DECURIX_RUN_BUNDLE_MB`, default 64; `runs.BundleLimit`), not per
  kind of work; past it frames are dropped, oldest first, and said.
- **A pruned trace's task that runs again** (a reply days after the export) starts a new trace file, but soul, if it
  did not restart, still counts the old file's size against the 32 MB cap.
