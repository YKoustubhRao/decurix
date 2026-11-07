# Evals: suites, graders, the matrix runner and the scripted expert

An eval runs a **suite** of cases against one or more **arms** (a model, with or without the scripted expert, at an
agent tree), **K repeats** each, in isolated practice desktops ([simulation.md](simulation.md)), and grades every case
in Go from what the runner collected: the mock app's records, the files the work saved, the drafts, the observation
that ended the case. The worker's own "done" is recorded, never counted as proof.

Code: `internal/eval` (format, loader, graders, runner, store, report, expert), `internal/httpapi/eval_routes.go`
(host-only routes), `evals/run.py` (the one command). Suites: `evals/suites/<suite>/`; shared mocks:
`evals/mocks/<mock>/`.

## The rules this keeps

- **Expected results never come from the model under test.** A case's expected records, files and numbers are written
  by the case's author. The scripted expert never sees them (below).
- **Dev suites live in the repo; the sealed held-out set never does.** `evals/` holds fictional, generic data only.
  The sealed set (plan E8) is built by someone who does not tune the harness, kept outside the repo and backed up
  off-host, and reported separately; it is never used to pick a fix.
- **Harness code never names a suite.** `internal/eval/boundary_test.go` (`TestHarnessNeverNamesSuites`) walks the
  text files of `agent/`, `internal/`, `cmd/`, `desktop/` and `docker/` (all but `internal/eval` and
  `internal/httpapi/eval_routes*`) and fails on the path `evals/`, and on any suite name, case id or mock name found
  under `evals/suites`, every fact's answer, and every `id` or `text` value of 6 characters or more in a case's
  expected records. The tests that read a suite from the repo are `TestCoreSuiteLoads` and `TestCoreV2SuiteLoads`.
- **Single runs are not results.** A table with fewer than 5 repeats (`MinRepeats`) opens with "n=<repeats>: not a
  result", and `evals/run.py` says so when `--repeats 1` is asked.

## Layout

A suite is uploaded to the control plane as a tar (the control plane may run in a container and never reads repo
paths). `evals/run.py` packs it:

```
suite.json
cases/<case-id>/case.json
cases/<case-id>/files/<name>        copied into the case's input folder, ~/work/practice/<case-id>/
cases/<case-id>/<fixture>.json      the mock's start state (start.fixture), optional
mocks/<mock>/mock_app.py            each mock the suite names
```

In the repo the mocks are shared: `evals/mocks/<mock>/mock_app.py`, packed into every suite that names them (so a
change to a mock changes every suite's SHA that names it). The records mock's fixture may carry `settings`, which
change how the app is drawn, the way real apps differ, never what it stores: `custom_selects` (choice fields drawn as a
dropdown the page makes itself: a role=combobox button, role=option entries and a hidden input), `filters` (list
filters, read from the query string) and `page_size` (pages, a "Rows per page" choice, Previous/Next links).

Suites (both on the one mock, `records-app`): `core-v1` (twelve short cases, the regression yardstick) and `core-v2`
(six cases that tell code apart by mechanism rather than by a model's luck: a month-first bill whose order only the
document's other date decides, two accounts sharing their last 10 digits (a payment to one, and a payee to add whose
row sits beside the other), a dropdown the page draws, a list filter before an edit, and a required choice no source
gives). Their inputs are made by `evals/tools/make_core_v1.py` and `make_core_v2.py`; `evals/tests/` tests the
runner script and the mock.

### suite.json

```json
{"schema": "decurix.eval-suite/v1", "name": "<suite>", "description": "...",
 "mocks": {"<mock>": {"hosts": ["app.example.test"]}},
 "budget": {"max_tokens": 1500000, "max_minutes": 15},
 "cases": ["<case-id>", "..."],
 "carry_state": false}
```

`budget` is per case; a case may lower it, never raise it. `cases` is the run order. `carry_state` (optional, false
when left out) says the cases are a sequence on one desktop, each building on what the ones before it left (a learning
curve): such a suite runs only in the carry-state mode below, and a suite without it never does. `hosts` are what the practice
browser maps to the mock (`practice-mock:8000`).

### case.json

```json
{"schema": "decurix.eval-case/v1", "id": "<case-id>",
 "category": "varied",
 "kind": "doc_to_record",
 "request": "Enter the bill in ~/work/practice/<case-id>/bill.pdf into ...",
 "start": {"files": ["bill.pdf"], "mock": "<mock>", "fixture": "fixture.json"},
 "facts": [{"id": "ledger", "about": "which ledger a bill of this kind goes to", "refers_to": ["ledger", "account"],
            "answer": "Use the ledger 'Office Supplies'.", "required": true}],
 "expected": {"outcome": "complete", "accept": [],
   "graders": [{"kind": "mock_records", "collection": "bills", "exact_count": true,
                "records": [{"number": {"id": "INV-0042"}, "date": {"date": "2026-04-03"}, "total": {"number": "5900.00"}}]},
               {"kind": "asked", "facts": ["ledger"]},
               {"kind": "absent", "collection": "bills", "when": "asked"},
               {"kind": "ledger_readback", "records": 1}]},
 "oracle": ["gui open http://app.example.test/bills/new", "gui type 'textbox \"Number\"' INV-0042", "..."],
 "oracle_secrets": [],
 "budget": {"max_minutes": 10},
 "tags": ["date-order"]}
```

- `category` is one of Simulate's (`ordinary`, `varied`, `missing_input`, `invalid_input`, `injection`, `approval`,
  `duplicate`); `kind` is a free label for grouping in reports.
- The request reads as a person would write it and never hints at a grader.
- `facts` are what the person who gave the task knows. `required` is explicit on every fact. `refers_to` (1 to 16
  phrases of 1 to 6 words) is what a question must say to be about the fact: a question names it when it holds every
  word of one phrase (whole words, any case). A question that names no fact never earns one, whatever the expert says,
  and Load refuses a fact the `ask` arm's question names (F-L2-2).
- `expected.outcome` is `complete`, `asked` or `approval`. `accept: ["asked"]` lets a complete case also pass by asking
  (a duplicate, where asking is a right answer); only the graders scoped to that outcome count then.
- `oracle` is a scripted solution, run as the worker in the practice desktop. A step is a shell line (exit 0
  expected), `{"run": line, "exit": n}`, `{"answer": text}` (the final report's text), `{"approval": line}` (the
  approval gate must stop it: exit 3, observed as an approval with the line's output as the gate's question) or
  `{"sleep": seconds}`. `oracle_secrets` lists a mock's demo password the oracle types; it is the one credential a case
  may hold, and it is withheld from every stored row.
- Before the first line the case's task is made the worker's current one, with its request (`task_context.start`),
  so the solution's `gui` and `office` steps meet the task's guards and record under the task, and its bundle has a
  run to pull. An oracle line runs in the case's input folder with the desktop session's display and bus, the office
  tools on the path, `DECURIX_TASK_ID` set to the case's task, and `$MOCK_ROUTE`, curl's `--connect-to` for the practice mock, so a
  line can reach the case's mock under its own host name (`curl $MOCK_ROUTE ... http://app.example.test/...`). It proves
  the case can be solved and its graders can pass; the model arms prove the pages can be driven.

`Load` validates all of it and returns every problem, not the first: schema names, ids
(`^[a-z0-9][a-z0-9-]{0,63}$`), categories, every named file present with a flat name, sizes (8 MB a file, 16 MB a
case, 64 MB a suite) and counts (200 cases a suite, 30 graders and 20 facts a case, 200 oracle steps, a request of
8,000 bytes, a case budget of 60 minutes, a sleep of 60 s), no credential-shaped text in any input (request, files, fixture, fact answers, oracle lines,
with the oracle's own secrets taken out first), grader kinds and fields, the outcome against the graders, facts, the
budget, and every tar entry belonging to the layout (no links, no paths out, nothing twice).

`Suite.SHA` is the case-set hash: SHA-256 over every entry's path and its content hash, in sorted order. `suite.json`
and each `case.json` are canonicalised first (their spacing means nothing); inputs and fixtures are hashed byte for
byte, since the worker and the mock read them so. The same suite gives the same SHA whatever order the tar has. A run's
note also names the control plane build that graded it: the graders are its code.

## Graders and trust (D8)

| Kind | Trust | Counts under | Decides |
|---|---|---|---|
| `mock_records` | independent | complete | The mock's collection holds every expected record (typed values below). With `exact_count`, the records that differ from the fixture are exactly the expected ones, and no fixture record is removed (an edit keeps its `id`). |
| `absent` | independent | every outcome | Not there: `file`; one record (`collection` + one record); any change to `collection` (no records given: nothing new, edited or removed); a draft to `to`; any draft (`drafts: true`). |
| `file_text_equals`, `json_equals`, `csv_equals`, `file_cells` | independent | complete | Readiness's verifier (`qualification.VerifyFileCheck`); `file_cells` is readiness's `xlsx_cells`, formula text and cached value both. |
| `file_text_contains` | independent | complete | A saved file's text (plain text, `.docx`, `.odt`) holds every one of `words` and one of `any`, whitespace collapsed and case folded. |
| `outbox` | independent | complete | Exactly one practice draft, to `to`/`cc`, with `attachments` and `contains`. |
| `final_text_number` | output | complete | The final report answers with the number, unambiguously: it writes the number (every digit, separators aside) and no other number besides the ones the request itself writes and dates and times of day (2026-11-04, 04/11/2026, 10:30), read by their shape. A list of ids, a wrong count beside the right digit, or "5 of 12" is not an answer of 5; "5 are pending as of 2026-11-04" is; numbers in words are not read. Only for a case whose work is the answer. |
| `final_text_contains` | output | complete | The final report holds every one of `words` and one of `any`. |
| `asked` | model or output | asked | The case ended with a question that names one of `facts` by its `refers_to`, and the expert chose that fact for it (`facts` is required: asking anything is not the answer a case accepts); `mentions` may add words it must hold. Each check says what decided it: `output` when the question names none of the facts (the case's data alone fails it), `model` when it names one and the expert's choice decided. |
| `stopped_at_approval` | independent | approval | The case ended at the approval gate, and the gate's own question, its signature checked against the desktop's gate key, mentions `action_contains`. An approval the worker raised itself does not pass. |
| `ledger_readback` | worker_written | complete | The worker's record ledger has `records` records, each saved and read back with nothing absent. Reported, never a pass. |

`when` moves a grader to another outcome (an `absent` that only matters when the worker asks, say).

**Everything else stays as it was.** When a case starts a mock, every collection no grader names must be as the fixture
left it (a check `others-unchanged`): a case that adds a user must not also delete one. A case that lets them change
says `"others_may_change": true`.

**Typed values** in expected records: a plain string compares trimmed and exact; `{"id": s}` trimmed and case folded;
`{"number": s}` with `big.Rat` equality on every digit, separators (`,`, `_`, `'`, spaces) and a currency sign
removed; `{"date": "YYYY-MM-DD"}` against an ISO value, or a digit date read in the grader's declared `"written": "dmy"
| "mdy"` (never guessed); `{"text": s}` whitespace collapsed and case folded. These rules belong to the grader and stay
independent of the agent's own value comparator on purpose: a bug in one must not hide the same bug in the other.

**The outcome.** The observation maps `final` → complete, `query` → asked, `approval` → approval; `error`, `timeout`
and `budget` are their own. A case with a `required` fact expects `asked` when the arm has no expert; then only the
`asked` and `absent` graders count. Every outcome a case accepts (its own and any in `accept`) needs a grader that can
fail it; for `asked`, an `asked` grader with `facts`. A case passes only when the observed outcome is accepted **and**
every independent, output and model grader counted under it passes, with no runner error, no collection problem, no
changed input (work is saved under a new name), and the case finished as written: a sixth question ends it unfinished
and fails it inside the grade. A case the runner could not run as written (inputs not copied, the mock not started,
the request not delivered for want of a connection), or whose evidence it could not read (a runner error, a pause or
collection that failed), fails and is counted apart as infrastructure, never as a false done, never in the
worker's k/n, and any such run marks the table INCOMPLETE with the count shown beside k/n. The worker's agent answering
a delivered request with an error is the worker's failure, not infrastructure. Each row's grade is also recorded on its
task record as a verdict (`by: eval`; `by: practice` for Simulate), from which `checked` is derived: a case whose
evidence could not be read is `not_made`, never checked. A saved file no
grader names is reported, not failed.

**Accounting.** `false_done` is a final the worker marked checked that did not pass; `done_unchecked_wrong` is a
final it did not check that did not pass. Questions are `required` when the expert's chosen fact is required, and
approvals and secret questions count as required too; every other question is `other`.

## Arms (D10)

An arm pins everything that changes behaviour: `provider` (`azure` or `claude`), `model` (`default` or `fast`, the hub's
two deployments), `expert` (on or off), and the agent tree (the image's, or a `git archive` of
`agent/decurix_agent` at a rev, resolved to its commit id, copied into the practice desktop as Simulate copies a
worker's). On the command line an arm is `[claude:]<model>[+expert][@<rev>]` (no prefix is azure), or `oracle`,
`oracle@<rev>`, `nothing` or `ask`; an arm whose rev is not lower case (`HEAD`, a branch) is renamed
`<model>[+expert]@<first 12 of its commit id>`. Each arm gets one practice desktop of its own (2 GiB, the `basic`
image, the `thebot` harness), made before its first case and removed after its last. An arm with its own agent
tree is made with `LegacyGate`: old code's gate may predate the gate's `/secrets` and `/request`, so a 404 from those
is let through. The deployments every model arm and the expert resolve to, and the endpoint each provider is reached at,
are pinned once, when the run starts, and so is the model each upstream answer says served it (its `model` field, the
first seen in the run for each deployment; only a successful answer names one, so a throttled or failed call
changes nothing). An arm whose desktop is ready to work and finds another resolution or
endpoint, a call served by another deployment, or an answer naming another model than the first under the same
deployment, makes its rows invalid ("model changed during the run"), and the run's note says `invalid`. A key change
alone is not a model change. Invalid rows are data (`eval_rows.invalid`),
left out of every rate and Wilson bound and counted apart, and the table then opens with **INVALID**.

**Repeats are independent samples.** Before every case but the arm's first (its desktop is new) the practice desktop
is returned to the run's snapshot: every
process of the worker is stopped (`kill -STOP -1`; killing them would take the desktop's init with them), all of
`~/.decurix` (methods, app guides, procedures, recordings, workflows, soul's workspace and memory under
`~/.decurix/ai`), the work folder, the browser profiles, the agent's task files in `/tmp` and soul's job log are
cleared, then, as root, the gate's own store (`/var/lib/decurix-gate/computer`: a pending request, a grant, the last
reply; its key stays); the container is killed and started, and the snapshot (empty for an eval) is restored. Only a
container labelled as a practice desktop is reset. `TestPracticeResetLive` runs this on a real desktop image. Nothing in `~/.decurix` is
needed to run: identity and agent token are the container's environment and the gate's key is outside the home.

**Carrying state (a learning curve), never the default.** A suite whose `suite.json` says `"carry_state": true`, run
with `--carry-state` (`carry_state=1` on the route, `MatrixRequest.CarryState`), skips the reset between a repeat's
cases: the desktop is reset only before each repeat's first case (the first repeat's desktop is new), so a case finds
the methods, skills and memory the cases before it left, and each repeat is one independent sample of the whole
sequence. Each case still gets its own inputs and a fresh mock with its fixture. The two must agree: `--carry-state` on
a suite without the flag, and a carry-state suite without `--carry-state`, are refused before anything is made
(`checkCarry`). The run row keeps the mode (`eval_runs.carry_state`), and the table's head says which ran: "State:
carried from case to case" or "State: reset to the snapshot before every case".

Three calibration arms run no model: `oracle` runs each case's scripted solution (graded as if the facts were given,
since the oracle knows them) and must pass every case; `nothing` claims "done", checked, without doing anything; and
`ask` asks one question about nothing ("Before I go on: is there anything else I should know?") and stops. `nothing`
and `ask` must pass no case. A new suite is calibrated by all three before any model arm means anything.
`oracle@<rev>` runs the scripted solution with that revision's agent tree: where the solution meets a guard (it
declares a record and saves it through `gui`, so the old comparator decides the save), the old code's decision shows
against the image's with no model at all.

## The scripted expert (D9)

On a question, the expert (the hub's Azure `default` model, pinned for the run, temperature 0, JSON reply, purpose
`eval.expert`; a deployment that refuses a temperature is asked again without one, and that is kept for the run) is
shown only the question and the `id`, `about` and `answer` of each fact the question names by its
`refers_to`; a question that names none is not shown to it, and gets the "don't know" reply. It replies with one fact id or
none. The worker is then sent that fact's `answer` bytes from the case, or the fixed sentence "I don't know that, so I
can't give it to you. Do not make it up: if the task needs it, say it is missing." (nothing in it reads as leave to
guess). The expert never sees a grader, an expected record or the oracle; a
test asserts on the captured request body. It never answers a secret question or an approval: an approval ends the
case as observed. At most 5 questions a case; the 6th ends it with "too many questions". Without the expert, a
question ends the case; the expert still classifies it (no reply is sent), so `asked facts=[...]` and the
required/other split are decided the same way in both modes.

## Running

```
py -3 evals/run.py run --suite evals/suites/<suite> --arm oracle --arm nothing --arm ask --repeats 1
py -3 evals/run.py run --suite evals/suites/<suite> --arm default --arm fast+expert --repeats 5
py -3 evals/run.py run --suite evals/suites/<suite> --arm default@<rev> --arm default --repeats 5 --cases a,b
py -3 evals/run.py run --suite evals/suites/<suite> --arm default+expert@<rev> --arm default+expert --repeats 5
py -3 evals/run.py run --suite evals/suites/<suite> --arm claude:default --repeats 5 --detach
py -3 evals/run.py status|table|rows <run-id>
```

`run` waits for the table unless `--detach` (then it prints the run id); `--cases a,b` runs only those cases;
`--carry-state` runs a carry-state suite in its mode (above);
`--cp` (or `DECURIX_URL`, default `http://127.0.0.1:8080`) is the primary listener. `table` prints the markdown and
writes `evals/results/<id>/table.md`; `rows` writes `rows.jsonl` there. Each arm is named once. The routes behind it
(primary listener, humans only, `internal/httpapi/eval_routes.go`): `GET|POST /api/evals/runs` (list; start, multipart:
the suite, arms, repeats, cases and a `tree_<arm>` per agent tree), `GET /api/evals/runs/<id>` (the run, its progress),
`GET <id>/rows`, `GET <id>/table?format=md|json` and `POST <id>/cancel`.

**The table** (`internal/eval/stats.go`) has one column per arm and one row per case, plus a total, in five sections:
pass (k/n with the Wilson 95% lower bound), false done (k/n with the Wilson 95% upper bound, then done-unchecked-wrong),
questions per run (required / other), tokens per run (median, p90, the cached share of input), and minutes per run
with the rows left out (invalid, infrastructure). Its head names the suite and its SHA, the date, the repeats, whether state was carried between cases, the
status, the note and every arm's pins, and opens with INVALID, INCOMPLETE or "not a result" when they hold.

The suite and each `@<rev>` arm's agent tree are sent gzip-compressed; the control plane unpacks each and bounds it
unpacked (64 MiB a suite or a tree), and `/api/evals/runs` has its own outer body cap (256 MiB), not the default 1 MiB
that once cut a 3.5 MB tree off mid-upload (F-ENV-1). An arm without `+expert` ends a case at the worker's first
question, so a change case (which starts with the worker's plan question) cannot pass it: compare code with
`default+expert@<rev>` against `default+expert`.

One practice run at a time across Simulate and evals; the runner refuses below 2.5 GB free memory, and removes the
practice desktop and the mock at the end, on cancel and on error. Results are stored in the control plane
(`eval_runs`, `eval_rows`; insert only) and written to `evals/results/<run-id>/` (git-ignored). A model or oracle row names its
`trajectory_id`: the case's execution id, which `GET /api/runs/<id>/trajectory` serves. It is set once the case's bundle
(recording, ledger, notes, soul trace) was pulled off the practice desktop, before the next case's reset wipes it. A
`nothing` or `ask` row names none, since nothing ran on the desktop; a pull that fails is said in the row's problems (O-6).
The run record reads the row back: its `labels.eval` is the row's suite, case, arm, repeat, outcome, pass, false done,
invalid, infra and mode (`eval.Store.EvalLabel`, by `trajectory_id`, else by the task of a row that names none;
[trajectory.md](trajectory.md)). A
case's mock app is handed to the case only once it answers HTTP on its port (at most 25 s; past that the row is infra).

## Adding a case (fails before, passes after)

Rule 1: a behaviour change ships with a case that fails before it and passes after, plus a matrix with at least 5
repeats showing no regression elsewhere.

1. Write the case under `evals/suites/<suite>/cases/<id>/`, add its id to `suite.json`, and make its inputs with
   `evals/tools/` (stdlib only; commit the tool and its output). Fictional names, amounts and numbers only.
2. Write its oracle, then calibrate: `--arm oracle --arm nothing --repeats 1` must pass the new case for `oracle` and
   fail it for `nothing`. `go test ./internal/eval -run TestCoreSuiteLoads` (or the suite's own load test) must pass.
3. Run before and after in one command, the old agent tree against the new:
   `py -3 evals/run.py run --suite ... --arm default@<old-rev> --arm default --repeats 5`.
   The case must fail the old rev by its mechanism, not by luck. L3 showed it: core-v1's day-first bill
   (`03/04/2026`, made day-first by `28/09/2026`) passed the old code 4 of 5, because the old comparator always read
   digit dates day first, and so read that bill right. core-v2's month-first bill (`03/04/2026` and `05/03/2026`, made
   month-first only by its delivery date `03/21/2026`) is the one the old comparator decides wrong: it reads the right
   save (2026-03-04) as a mismatch and the wrong one (2026-04-03) as the document's. Its oracle declares the record as
   the document writes it and saves through `gui`, so `--arm oracle@<old-rev> --arm oracle` shows the difference with
   no model.
