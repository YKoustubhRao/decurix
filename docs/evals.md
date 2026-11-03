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
- **Harness code never names a suite.** `internal/eval/boundary_test.go` walks `agent/` and `internal/` (all but
  `internal/eval` and `internal/httpapi/eval_routes*`) and fails on the path `evals/`, and on any suite name, case id,
  mock name or mock host found under `evals/suites`. The one test that reads a suite from the repo is
  `TestCoreSuiteLoads`.
- **Single runs are not results.** A table with one repeat says "n=1: not a result".

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

In the repo the mocks are shared: `evals/mocks/<mock>/mock_app.py`, packed into every suite that names them.

### suite.json

```json
{"schema": "decurix.eval-suite/v1", "name": "<suite>", "description": "...",
 "mocks": {"<mock>": {"hosts": ["app.example.test"]}},
 "budget": {"max_tokens": 1500000, "max_minutes": 15},
 "cases": ["<case-id>", "..."]}
```

`budget` is per case; a case may lower it, never raise it. `cases` is the run order. `hosts` are what the practice
browser maps to the mock (`practice-mock:8000`).

### case.json

```json
{"schema": "decurix.eval-case/v1", "id": "<case-id>",
 "category": "varied",
 "kind": "doc_to_record",
 "request": "Enter the bill in ~/work/practice/<case-id>/bill.pdf into ...",
 "start": {"files": ["bill.pdf"], "mock": "<mock>", "fixture": "fixture.json"},
 "facts": [{"id": "ledger", "about": "which ledger a bill of this kind goes to",
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
- `facts` are what the person who gave the task knows. `required` is explicit on every fact.
- `expected.outcome` is `complete`, `asked` or `approval`. `accept: ["asked"]` lets a complete case also pass by asking
  (a duplicate, where asking is a right answer); only the graders scoped to that outcome count then.
- `oracle` is a scripted solution, run as the worker in the practice desktop. A step is a shell line (exit 0
  expected), `{"run": line, "exit": n}`, `{"answer": text}` (the final report's text), `{"approval": line}` (the
  approval gate must stop it: exit 3, observed as an approval with the line's output as the gate's question) or
  `{"sleep": seconds}`. `oracle_secrets` lists a mock's demo password the oracle types; it is the one credential a case
  may hold, and it is withheld from every stored row.

`Load` validates all of it and returns every problem, not the first: schema names, ids
(`^[a-z0-9][a-z0-9-]{0,63}$`), categories, every named file present with a flat name, sizes (8 MB a file, 16 MB a
case, 64 MB a suite), no credential-shaped text in any input (request, files, fixture, fact answers, oracle lines,
with the oracle's own secrets taken out first), grader kinds and fields, the outcome against the graders, facts, the
budget, and every tar entry belonging to the layout (no links, no paths out, nothing twice).

`Suite.SHA` is the case-set hash: SHA-256 over every entry's path and its content hash (JSON documents canonicalised
first), in sorted order. The same suite gives the same SHA whatever order or spacing the tar has.

## Graders and trust (D8)

| Kind | Trust | Counts under | Decides |
|---|---|---|---|
| `mock_records` | independent | complete | The mock's collection holds every expected record (typed values below). With `exact_count`, the records that differ from the fixture are exactly the expected ones, and no fixture record is removed (an edit keeps its `id`). |
| `absent` | independent | every outcome | Not there: `file`; one record (`collection` + one record); any change to `collection` (no records given: nothing new, edited or removed); a draft to `to`; any draft (`drafts: true`). |
| `file_text_equals`, `json_equals`, `csv_equals`, `file_cells` | independent | complete | Readiness's verifier (`qualification.VerifyFileCheck`); `file_cells` is readiness's `xlsx_cells`, formula text and cached value both. |
| `file_text_contains` | independent | complete | A saved file's text (plain text, `.docx`, `.odt`) holds every one of `words` and one of `any`, whitespace collapsed and case folded. |
| `outbox` | independent | complete | Exactly one practice draft, to `to`/`cc`, with `attachments` and `contains`. |
| `final_text_number`, `final_text_contains` | output | complete | The final report gives the number (every digit, separators aside, not inside a longer number) or the words. Only for a case whose work is the answer. |
| `asked` | independent | asked | The case ended with a question, and the expert matched one of `facts` to it, or it mentions every one of `mentions`. |
| `stopped_at_approval` | independent | approval | The case ended at the approval gate, and the gate's question mentions `action_contains`. |
| `ledger_readback` | worker_written | complete | The worker's record ledger has `records` records, each saved and read back with nothing absent. Reported, never a pass. |

`when` moves a grader to another outcome (an `absent` that only matters when the worker asks, say).

**Typed values** in expected records: a plain string compares trimmed and exact; `{"id": s}` trimmed and case folded;
`{"number": s}` with `big.Rat` equality on every digit, separators (`,`, `_`, `'`, spaces) and a currency sign
removed; `{"date": "YYYY-MM-DD"}` against an ISO value, or a digit date read in the grader's declared `"written": "dmy"
| "mdy"` (never guessed); `{"text": s}` whitespace collapsed and case folded. These rules belong to the grader and stay
independent of the agent's own value comparator on purpose: a bug in one must not hide the same bug in the other.

**The outcome.** The observation maps `final` → complete, `query` → asked, `approval` → approval; `error`, `timeout`
and `budget` are their own. A case with a `required` fact expects `asked` when the arm has no expert; then only the
`asked` and `absent` graders count. A case passes only when the observed outcome is accepted **and** every
independent and output grader counted under it passes, with no runner error, no collection problem and no changed
input (work is saved under a new name). A saved file no grader names is reported, not failed.

**Accounting.** `false_done` is a final the worker marked checked that did not pass; `done_unchecked_wrong` is a
final it did not check that did not pass. Questions are `required` when the expert's chosen fact is required, and
approvals and secret questions count as required too; every other question is `other`.

## Arms (D10)

An arm pins everything that changes behaviour: `provider` (`azure` or `claude`), `model` (`default` or `fast`, the hub's
two deployments), `expert` (on or off), and the agent tree (the image's, or a `git archive` of
`agent/decurix_agent` at a rev, copied into the practice desktop as Simulate copies a worker's). The runner records
the deployment that served every call; a row served by more than one deployment, or by another than the arm's at its
start, is marked "model changed during the run" and the run's note says `invalid`.

Two calibration arms run no model: `oracle` runs each case's scripted solution (graded as if the facts were given,
since the oracle knows them), and `nothing` claims "done", checked, without doing anything. A new suite must give
`oracle` every case and `nothing` none before any model arm means anything.

## The scripted expert (D9)

On a question, the expert (the hub's Azure `default` model, pinned for the run, temperature 0, JSON reply, purpose
`eval.expert`) is shown only the question and each fact's `id`, `about` and `answer`. It replies with one fact id or
none. The worker is then sent that fact's `answer` bytes from the case, or the fixed sentence "I don't know that. Use
what you have, or stop and say what is missing." The expert never sees a grader, an expected record or the oracle; a
test asserts on the captured request body. It never answers a secret question or an approval: an approval ends the
case as observed. At most 5 questions a case; the 6th ends it with "too many questions". Without the expert, a
question ends the case; the expert still classifies it (no reply is sent), so `asked facts=[...]` and the
required/other split are decided the same way in both modes.

## Running

```
py -3 evals/run.py run --suite evals/suites/<suite> --arm oracle --arm nothing --repeats 1
py -3 evals/run.py run --suite evals/suites/<suite> --arm default --arm fast+expert --repeats 5
py -3 evals/run.py run --suite evals/suites/<suite> --arm default@<rev> --arm default --repeats 5 --cases a,b
py -3 evals/run.py status|table|rows <run-id>
```

One practice run at a time across Simulate and evals; the runner refuses below 2.5 GB free memory, and removes the
practice desktop and the mock at the end, on cancel and on error. Results are stored in the control plane
(`eval_runs`, `eval_rows`; insert only) and written to `evals/results/<run-id>/` (git-ignored).

## Adding a case (fails before, passes after)

Rule 1: a behaviour change ships with a case that fails before it and passes after, plus a matrix with at least 5
repeats showing no regression elsewhere.

1. Write the case under `evals/suites/<suite>/cases/<id>/`, add its id to `suite.json`, and make its inputs with
   `evals/tools/` (stdlib only; commit the tool and its output). Fictional names, amounts and numbers only.
2. Write its oracle, then calibrate: `--arm oracle --arm nothing --repeats 1` must pass the new case for `oracle` and
   fail it for `nothing`. `go test ./internal/eval -run TestCoreSuiteLoads` (or the suite's own load test) must pass.
3. Run before and after in one command, the old agent tree against the new:
   `py -3 evals/run.py run --suite ... --arm default@<old-rev> --arm default --repeats 5`.
   For example, a bill that writes its date `03/04/2026` and is made day-first by another date it writes
   (`28/09/2026`): the old comparator reads the save as a mismatch or the wrong month, so the case fails at the old
   rev, and passes after the fix in the value comparator, with every other case's Wilson lower bound no lower than
   before.
