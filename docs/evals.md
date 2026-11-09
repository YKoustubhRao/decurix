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
  expected records. The tests that read a suite from the repo are `TestCoreSuiteLoads`, `TestCoreV2SuiteLoads`,
  `TestLearnV1SuiteLoads` and the three office suites' (`internal/eval/office_suites_test.go`). A comment counts too: a test docstring under `agent/` that named `learn-v1` failed it.
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

Suites on the `records-app` mock: `core-v1` (twelve short cases, the regression yardstick), `core-v2`
(six cases that tell code apart by mechanism rather than by a model's luck: a month-first bill whose order only the
document's other date decides, two accounts sharing their last 10 digits (a payment to one, and a payee to add whose
row sits beside the other), a dropdown the page draws, a list filter before an edit, and a required choice no source
gives) and `learn-v1` (the learning curve, below). Their inputs are made by `evals/tools/make_core_v1.py`,
`make_core_v2.py` and `make_learn_v1.py`; `evals/tests/` tests the runner script and the mocks. On the `ca-office` mock,
three suites of a CA office's monthly GST work, each meant to be taught once by demonstration (below, "The office
suites" and "Teaching once"): `office-books-v1`, `office-gstr1-books-v1` and `office-gstr1-chat-v1`, made by
`evals/tools/make_office_suites.py`.

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
 "approves": [{"control": "Save bill", "role": "button"}],
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
- `approves` (optional, at most 10) lists the actions the person approves for this case when the approval gate stops
  one: a click on the control named `control` (1 to 79 characters, no double quote; the gate quotes a name with its
  own quotes turned to single ones and cuts it at 80), and, when given, of the role `role` (`button`, `link`). It is
  matched by structure, as a fact is by its `refers_to`, never by free words: the gate's own signed question must
  word a click on that control (`Approval needed before I click <role> "<name>" ...`, read by `eval.GateControl`),
  the name whole, whitespace collapsed, any case. The listed name typed into a field, pressed as a key, inside a
  command or in the window's title approves nothing. A case that expects `approval` approves nothing (Load refuses
  it). See "The scripted expert" for what a listed approval gets.
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
worker's). On the command line an arm is `[claude:]<model>[+expert][+soul][@<rev>]` (no prefix is azure), or `oracle`,
`oracle@<rev>`, `nothing` or `ask`; an arm whose rev is not lower case (`HEAD`, a branch) is renamed
`<model>[+expert]@<first 12 of its commit id>`. Each arm gets one practice desktop of its own (2 GiB, the `basic`
image, the `thebot` harness), made before its first case and removed after its last. An arm with its own agent
tree is made with `LegacyGate`: old code's gate may predate the gate's `/secrets` and `/request`, so a 404 from those
is let through. A model arm with `+soul` (`Arm.Soul`) runs its desktop with every decision soul words and reads in
place of Python turned on (`desktop.SoulDecisionSwitches`: `DECURIX_SOUL_JUDGE`, `DECURIX_SOUL_ESCALATION`; plan rule
3, phase 2): each is off by default until `default+expert` against `default+expert+soul`, 5 repeats on one image,
shows no regression (plan rule 1). A calibration arm never has it. The deployments every model arm and the expert resolve to, and the endpoint each provider is reached at,
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
needed to run: identity and agent token come from the container's environment (the token reaches the agent and soul as one-time files, gate-boundary.md) and the gate's key is outside the home.

**Carrying state (a learning curve), never the default.** A suite whose `suite.json` says `"carry_state": true`, run
with `--carry-state` (`carry_state=1` on the route, `MatrixRequest.CarryState`), skips the reset between a repeat's
cases: the desktop is reset only before each repeat's first case (the first repeat's desktop is new), so a case finds
the methods, skills and memory the cases before it left, and each repeat is one independent sample of the whole
sequence. Each case still gets its own inputs and a fresh mock with its fixture. Between a repeat's cases the desktop
is **settled** instead (`desktop.SettlePractice`): the worker's processes are stopped, the open task is cleared (the
agent's `active-task.json`, every request in `~/.decurix/command` not yet handled, its task files in `/tmp`, soul's job
log, the browser profiles, and as root the gate's store), and the container is restarted; everything else under
`~/.decurix` and the work folder stay. Found live in the first run: a case that ended at an approval left its question
open, and the next case's request, sent a second later, queued behind it (the agent drops a stale question only for a
request saved more than a second after it) until that case ran out of time. `TestPracticeSettleLive` proves it on the
image. A restart's start no longer counts its own earlier start's memory reservation (`idlepause` reserves per desktop
id): settling every few seconds was refused its desktop after a few cases. The two must agree: `--carry-state` on
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
test asserts on the captured request body. It never answers a secret question.

**Its words have an author** (plan rule 5). A reply is kept twice under one judgement id: by the broker, which serves
it to the practice desktop as a person's judgement (so it backs a value on that task and the worker's later ones), and
in the task records as an `answer` judgement by the scripted expert (`eval.ExpertActor`: kind `scripted`, role
`expert`, door `eval`, payload the question and the fact id), so the case's run record lists it among its
`judgements` with who gave it. `TestTheExpertsReplyIsRecordedWithItsAuthor`.

**What "gives" means** (`ExpertPrompt`, 2026-11-08). A fact gives what a question asks for when its words state it, or
the rule or method the person decides it by: the worker applies a rule itself, so the fact is chosen even when the
question lacks what applying it needs. A fact only on the same subject gives nothing, and no fact gives leave to act:
a question that only asks whether the worker may go ahead gets none. The model replies `{"asks": ..., "fact": ...}`;
only `fact` is read. Why: the first prompt ("the one fact that answers the question") had Kimi-K2.6 answer null to
learn-v1 bill 2's "The bill from Inkhaven Printers (IP-0782) does not specify a cost centre ... Which cost centre
should I select for this bill?" (run `eeb05dd4a201cecda`, trajectory `x0a15bc60469f8d16`; its reasoning: the rule
"explains how, not which", and the question held no GSTIN), so a lesson asked for again got "don't know". Matching by
`refers_to` was right there (the question named `cost-centre-rule`); the choice was not. Probed against the deployment
(22 questions, positives and near misses from learn-v1 and core's `bill-already-entered`): the first prompt chose that
question's fact 1 time in 12 and gave a fact to "may I click Save bill?" and to a gate-worded approval 9 times in 18;
the new one was right 93 of 93, and still gives none to a question on the same subject that the fact does not answer
(which ledger the IGST goes to, the IGST amount, the due date, the supplier's GSTIN, a date's reading).
`TestTheExpertCountsARuleAndNeverGivesLeave` holds the prompt's two rules.

**Confirmation questions are answered in code** (owner decision 2026-11-08, `confirm.go`). Two questions only ask the
person to confirm the worker's own reading, and before this both got "don't know", which the worker read as a change to
its plan (a taught run never replayed) or as a reason to try again (a correct ob-3 result went round for 50 minutes and
7.7M tokens). They never reach the expert's model:
- **A taught run's plan** (it opens with the agent's `PLAN_QUESTION`), only in a suite with `"confirms_plans": true`
  (the three office suites): each `Label = "value"` / `Label: "value"` the plan shows is set against the case's facts
  that name its field by their `refers_to`; a fact stating one figure the plan's value does not hold gets
  `No - <field> should be as I said: <the fact's words>`, else `Yes, go ahead.` (`eval.PlanYes`). A yes cannot fake a
  result: the graders still judge what was entered. Other suites answer a plan as any question.
- **"Is it done?"** (the agent's question that asks for a reply of done or retry, `eval.DoneQuestionMark`), in every suite:
  `I can't check that from here - report it as you see it, not confirmed.` (`eval.NotConfirmed`). The agent reads it,
  in code and with no model, as "leave it" (`final_gate.NOT_CONFIRMED_REPLY`): the run ends as reported, unchecked,
  with no verdict for or against the skill - never "done", never a retry - and the graders decide pass or fail.
Both are recorded like every expert reply, with `"confirm": "plan" | "done"` and `"by": "the scripted expert"` in the
judgement's payload, so a report says the plan was approved by the scripted expert, not by a person.
`TestAPlanIsConfirmedOrCorrectedByTheFacts`, `TestOnlyASuiteThatConfirmsPlansDoesSo`,
`TestTheConfirmationWordsAreTheAgents` (the words are the agent's own, kept the same on both sides),
`test_not_confirmed_reply`.

**Approvals are case data, decided in code** (owner decision 2026-11-08). The expert's model never sees an approval.
When the gate stops an action the case lists under `approves`, the arm has the expert, and the question is the gate's
own (its signature checked against the desktop's gate key, `VerifyGateQuestion`), the worker is answered
`Yes, go ahead.` (`eval.ApprovalYes`, a plain yes the gate settles without a model) by the route a person's yes
takes. The gate is told first, through the same `desktop.TellGate`, with the reply bound to its request id and its own
words, kind `approval` (`practice.ApprovalGateNotice`, as `httpapi.replyBinding` binds a reply typed under the
approval's card). Then the agent gets the reply. The gate settles that one request and grants that one action, and
nothing else is granted. Any other approval is refused: an action the case does not list, a question the gate did not
sign (the worker's own "may I?"), or any approval on an arm without the expert. Nothing is granted, and the case ends
at it as observed, as every approval did before (so `stopped_at_approval` cases are unchanged). A granted approval is
kept in the row's questions with its reply, counts as required, and counts toward the five a case may ask
(`TestTheExpertSaysYesToAListedApprovalThroughTheGate`, `TestTheExpertApprovesOnlyAListedControl`). Why: a worker's plan rightly
promises a bill's save "with your OK first", and the gate holds it. An eval that never answered approvals measured
nothing on such a case: every bill of the first learn-v1 model run ended at "Save bill". At most 5 questions a case; the 6th ends it with "too many questions". Without the expert, a
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
py -3 evals/run.py curve <run-id> --suite evals/suites/<suite> [--rows evals/results/<run-id>/rows.jsonl]
py -3 evals/run.py run --suite evals/suites/<suite> --arm default+expert --repeats 5 --from-worker <desktop id>
py -3 evals/run.py teach --suite evals/suites/<suite> --case <case-id> --desktop <desktop id>
py -3 evals/run.py teach-stop --desktop <desktop id>
```

`run` waits for the table unless `--detach` (then it prints the run id); `--cases a,b` runs only those cases;
`--carry-state` runs a carry-state suite in its mode (above); `--from-worker` starts every case from a taught worker's
state, and `teach` / `teach-stop` set a normal desktop up to demonstrate a case (both below, "Teaching once");
`--cp` (or `DECURIX_URL`, default `http://127.0.0.1:8080`) is the primary listener. `table` prints the markdown and
writes `evals/results/<id>/table.md`; `rows` writes `rows.jsonl` there; `curve` writes `curve.md` there (the
learning curve, below; `--rows` reads a saved `rows.jsonl` instead of the control plane). Each arm is named once. The routes behind it
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
A row also keeps `calls`, the model calls its case made through the broker (the expert's are not counted), beside its
tokens.

## The learning curve: learn-v1

The product's promise is that a worker gets cheaper with practice at the same correctness: fewer tokens per task, the
same intelligence. `learn-v1` measures it. It says `"carry_state": true`, so it runs only with `--carry-state`: a
repeat's ten cases are one sequence on one desktop, each finding the methods, skills and memory the cases before it
left, and each repeat is one independent sample of the whole sequence.

**The work.** Two kinds a small accounting firm repeats, five fresh documents each, all fictional
(`evals/tools/make_learn_v1.py`; the GSTINs carry a valid check character):

- `doc_to_record` (`learn-bill-1..5`): enter a supplier's tax invoice (PDF) into the records app's bill form. Five
  suppliers, two to four lines, CGST and SGST, different dates and totals; every case starts the app from the same
  fixture (two earlier bills, none of this sequence's), so what a case knows of the earlier ones it knows from its own
  memory. Graded by `mock_records` with `exact_count` (number, vendor, date, due date, total and cost centre, by value).
- `reconcile` (`learn-recon-1..5`): reconcile a client's bank statement (CSV) against their bank book (workbook, sheet
  `Bank book`) and save `recon.csv`. Two clients (Corvel Bakehouse three months, Pinecrest Dental Clinic two, the
  first of them at case 6), five to eight lines with the traps a real statement has: two receipts of one amount on one
  day from two parties (the party decides), a part payment, an amount that differs, bank charges, interest, a
  returned cheque, an entry not yet on the statement, last month's payment clearing. Graded by `csv_equals` on every
  row and cell.

**Interleaved, not blocked** (bill 1, recon 1, bill 2, ... recon 5): a firm's day mixes its work, so a method must be
found from the request, not from "the thing I did last"; a learned method has to survive a different task between two
uses; and each kind's curve has points spread over the whole sequence, so a drift late in the run shows in both.
`TestLearnV1SuiteLoads` holds the order.

**The person teaches once per kind, and the expert says the same words every time.** Neither rule is in any source:

- `learn-bill-1`: no bill says which cost centre it goes to. The fact `cost-centre-rule` (required in that case):
  "From now on, for every bill you enter, the cost centre is set by the first two digits of the supplier's GSTIN (its
  state code): 29 goes to CC-14, 30 to CC-27, 33 to CC-52, and every other state code to CC-90." Nothing in the
  documents implies it, so a correct guess cannot pass bill 1 without the lesson. The codes are the firm's own, the
  app offers five (CC-14, CC-27, CC-52, CC-61, CC-90), its two earlier bills sit in CC-52, which no bill of the
  sequence takes, and each bill also prints the client's GSTIN (29), which the rule does not read. The suppliers are in
  three states, so the rule is applied, not one answer remembered: bills 1, 3 and 4 are Karnataka's (CC-14), bill 2
  is from Goa (CC-27, IGST) and bill 5 from Maharashtra (no code of its own: CC-90, IGST). The first version taught
  "courier and freight to Logistics, the rest to General". That rule followed from the vendors' names, and the worker
  in run `ea1b8e583aab3317b` picked Logistics on bill 1 without asking. `TestLearnV1SuiteLoads` now fails a bill whose
  cost centre is written in its documents or in an earlier record. Bills 2 to 5 hold the same fact, not required: a
  worker that remembers asks nothing, and one that asks again gets the same answer and is counted under "other"
  questions.
- Every bill case approves the form's "Save bill" (button) and nothing else: the worker's plan puts the save to the
  person, and the person says yes to it (above, "The scripted expert").
- `learn-recon-1`: "our usual layout" is the firm's. The fact `recon-layout` (required in that case) gives the file
  name, the header `date,amount,ledger_ref`, one row per statement line in order, date and amount copied as the
  statement writes them, and the matching rule (equal amount and the description names the entry's party; each entry
  used once; else an empty `ledger_ref`). Recons 2 to 5 hold it, not required. Recon 3 is the second client's first:
  a dip there shows a firm-wide convention kept for one client only.
- `learn-bill-4` is the missing input: the copy prints no invoice number ("as per covering letter", and the letter is
  not in the folder). The fact `invoice-number` is required and is the only source of the bill number the grader
  holds, so a worker cannot get cheaper there by not asking: a guess fails the record by value.

The facts' answers are byte-identical across the cases that hold them (`TestLearnV1SuiteLoads` checks), so whatever a
worker learns, it learns from the same words.

**Run it** with a model arm that has the expert (without it a question ends the case, and a later case's lesson is
then never repeated), calibrated first as every suite is:

```
py -3 evals/run.py run --suite evals/suites/learn-v1 --arm oracle --arm nothing --arm ask --carry-state --repeats 1
py -3 evals/run.py run --suite evals/suites/learn-v1 --arm default+expert --carry-state --repeats 5
py -3 evals/run.py curve <run-id> --suite evals/suites/learn-v1
```

Run the whole sequence: `--cases` with a later case and not its kind's first runs a lesson nobody taught. Two arms in
one run (old code against new, `default+expert@<rev>` against `default+expert`) compare two curves on the same SHA.

**Read the curve.** `curve` prints, per arm, one line per case in run order: pass k/n, false done, questions (required /
other), tokens in and tokens out (medians over the repeats), model calls (median), the cached share of the input, and
the rows left out (invalid, infrastructure). Then, per kind, the later four cases against the first: their pass rate,
median tokens (in + out), questions that are not required, and false done. **Success** is all four at once: the later
cases pass at the first case's rate or better, with fewer tokens, no more questions that are not required, and no false
done; the line says "met" or which of them was not. Tokens alone never count: a later case that is cheaper and wrong,
or cheaper because it skipped `learn-bill-4`'s question, fails the bar by its pass rate. At fewer than 5 repeats the
curve says "not a result", and a run that did not carry state says it is not a learning curve.

Offline, `TestLearnV1SuiteLoads` passes the oracle on all ten cases (form posts to the real mock; the recon's
`python3 -c` writes the expected `recon.csv`); the `office read ... | grep -q` line before each is skipped there and is
proved only by the live oracle arm, with the carry-state run's own mechanics (nothing cleared between cases, a fresh
mock per case).

**Calibrated live** (2026-11-08, runs `ee0acbd1dcca561fb` and, after the settle between cases, `e889f624932ab3306`;
suite SHA `eb82d4819eac`, image `decurix-desktop:next` 5beb149276ff, `--carry-state --repeats 1`; both the same):
`oracle` 10/10 (every `office read ... | grep -q` line ran and matched),
`nothing` 0/10 (false done 10/10), `ask` 0/10 (its question names no fact, so `learn-bill-1`, `learn-recon-1` and
`learn-bill-4` fail their `asked` grader; the rest expect `complete`). No infrastructure rows, no problems on an oracle
row. Carry state held: in the middle of the oracle arm the practice desktop still had the earlier cases' folders under
`~/work/practice/` (with their `recon.csv`) and their tasks' files under `~/.decurix`, while each bill case's
`mock_records` with `exact_count` passed against its own fixture, so its mock did not hold the bill the case before it
saved. The runner removed the practice desktops, the mocks and the practice network at the end.

**Calibrated again** after the approvals and the GSTIN-state lesson (2026-11-08, run `e9aa06b8d2a292e61`, suite SHA
`c6722c894ced`, image `decurix-desktop:next` 158b3cdebb0b built from 82f2966, control plane 82f2966,
`--carry-state --repeats 1`): `oracle` 10/10, `nothing` 0/10 (false done 10/10), `ask` 0/10. No infrastructure rows,
no problems on an oracle row. The oracle posts the form itself, so the gate never stops it and the `approves` path
is proved only by a model arm (below).

**First curve** (2026-11-08, run `ea1b8e583aab3317b`, `default+expert` = azure Kimi-K2.6 on key 2, n=1: not a result):

| kind | case 1 | cases 2-5 | tokens in, case 1 then 2-5 | questions |
|---|---|---|---|---|
| reconcile | pass | 4/4 pass | 674k; 111k, 110k, 116k, 101k | recon 1 asked the layout (required), then none |
| doc_to_record | approval | 0/4 (all at the gate) | 1.36M; 175k, 808k, 238k, 123k | the plan question each time ("other") |

The reconciliation half shows the promise: the lesson taught once was kept across a different task between uses and
a second client, at a sixth of the first case's tokens, with no question. The bill half measures nothing yet: every
bill ended at the approval gate on "Save bill", which the plan of the work promised to put to the person first
(`DECURIX_PLAN_ASK_FIRST`) and which an eval never answers; on bill 1 the worker also picked Logistics without asking,
and on bill 4 it declared "as per covering letter" as the bill number instead of asking ([follow-ups.md](follow-ups.md),
"Phase 2: evals"). `curve` now says "not met" for a kind no later case passed (it said "met" for these bills: cheaper
failures).

**Baseline for 9038515** (2026-11-08, run `eaa791e6bd0cff5fa`, `default+expert --carry-state`, three repeats of the
five planned. The owner cancelled it after repeat 3 because the rest would only repeat failures already fixed on
HEAD. Image `decurix-desktop` e00bd82ce61e and the dev control plane, both from a `git archive` of 9038515.
Kimi-K2.6 / gpt-5.4-mini on `AZURE_KEY_1`, the before run's key; key 3 answered 401. Suite SHA `c6722c894ced`. The
before run is `eeb05dd4a201cecda` on 82f2966: one repeat, cancelled after 7 rows, with no approval answered as
asked):

| # | case | pass (Wilson lo) | false done | questions req / other | tokens in (median; r1, r2, r3) | before (r1) |
|---|---|---|---|---|---|---|
| 1 | bill-1 | 2/3 (0.21) | 0/3 | 2.3 / 0.7 | 868k; 608k, 868k, 903k | budget, 1.46M |
| 2 | recon-1 | 3/3 (0.44) | 0/3 | 1.0 / 0.3 | 172k; 172k, 121k, 349k | pass, 166k |
| 3 | bill-2 | 2/3 (0.21) | 0/3 | 0.7 / 1.3 | 887k; 1.04M, 367k, 887k | asked (6 questions), 582k |
| 4 | recon-2 | 3/3 (0.44) | 0/3 | 0.0 / 0.7 | 235k; 75k, 235k, 371k | pass, 71k |
| 5 | bill-3 | 2/3 (0.21) | 0/3 | 0.7 / 1.0 | 330k; 330k, 163k, 611k | timeout, 1.16M |
| 6 | recon-3 | 2/3 (0.21) | 1/3 | 0.0 / 0.0 | 69k; 66k, 69k, 252k | pass, 67k |
| 7 | bill-4 | 3/3 (0.44) | 0/3 | 1.7 / 0.7 | 547k; 326k, 735k, 547k | timeout, 1.11M |
| 8 | recon-4 | 2/3 (0.21) | 1/3 | 0.0 / 0.3 | 111k; 112k, 70k, 111k | - |
| 9 | bill-5 | 2/3 (0.21) | 0/3 | 0.0 / 0.7 | 368k; 368k, 226k, 959k | - |
| 10 | recon-5 | 1/3 (0.06) | 2/3 | 0.0 / 0.0 | 105k; 66k, 145k, 105k | - |

The total is 22/30 (Wilson lower bound 0.56), against 3/7 before.

**By kind (`curve`):**
- **Bills:** pass 67% on bill 1, then 75% on bills 2 to 5. Tokens fell from 988k to 536k (in plus out). Questions
  that are not required rose from 0.7 to 0.9, so the bar is not met. Every bill that failed had its record saved
  right (`mock_records` passed); it timed out before saying done.
- **Reconciliations:** pass 100% on recon 1, then 67%. Tokens fell from 198k to 140k. Four later rows were false
  done, so the bar is not met.

**Against the before run:** bills went from 0 passes in 4 to 11 in 15. Bill 4 now asks for the missing invoice number
and passes 3/3. Repeat 1 is the closest comparison: 10/10, bills 608k then 326k to 1.04M, against 3/7 before.

**Every failure is a harness failure, and HEAD fixes each one:**

| row | trajectory | what happened | fixed in |
|---|---|---|---|
| recon-5 r2 | `xcb425599da9f1c9e` | False done. The judge, not shown the layout the person taught, called the correct 6-row file not done; the worker added a 7th row and the next judge passed it. | 82495c2 |
| bill-1 r3 | `x643d67b7c0e709ac` | Timeout. Reviews demanded the cost centre be re-sourced from "the human reply", which `gui record` could not cite. | 82495c2, b797a0f |
| bill-2 r3 | `xccb1b2b6e6045634` | Timeout. The read-back hint did not say how; the worker tried a title and a URL as `--from`, then misreported its declaration, and four reviews held it. | ae332a0 |
| bill-3 r3 | `x3275c004d0ccfc5c` | Timeout. Reviews said the worker never asked for the cost centre (HELP), though it had asked and was answered. | 82495c2, 135771b |
| recon-3 r3 | `xc6b1e64d2526879f` | False done. The learned method held recon 1's plan answer ("don't know") and its first idea of the output (a balance workbook), not the layout given later. No call saw the layout; the judge passed the workbook. | be2ebfe, 82495c2 |
| recon-4 r3 | `x8aac58f7c6d4dfc2` | False done, as recon-3 r3. | be2ebfe, 82495c2 |
| bill-5 r3 | `xba5fe22d81ee9442` | Timeout. The worker wrote the person's rule to `~/work/gstin-rule.txt` to declare from it, and reviews held the source. | b797a0f, 82495c2 |
| recon-5 r3 | `xead09eed0df85a94` | False done, as recon-3 r3. | be2ebfe, 82495c2 |

**Costs that did not fail a row:**
- The plan question's "don't know", carried by the method as "said about this plan" (135771b): bill 2 in repeat 1
  (1.04M; its review called the person's rule made up). It was in bills 3 to 5 of repeat 1 too, which passed without
  a visible cost. Recon 2 in repeat 3 (371k) asked again because of it and was answered.
- No way to back a value with the reply (b797a0f): bill 1 in repeat 2 (868k; it wrote the rule to a file and declared
  from it).

**Validation on fc3cd32: partial, host-contended** (2026-11-08, run `e53e9a9df7f70755a`, `default+expert
--carry-state`, image a8f4ebfcc2c5 and a dev control plane from a `git archive` of fc3cd32, same brain). Started 14:24
UTC. Cancelled by the owner at 15:35 after 6 rows (repeat 1, cases 1-6). It ran beside the owner's ERPNext work in
the same WSL VM: ERPNext's start-up, then a teaching desktop (`desktop_erpbills`, 128% CPU and 688 MiB at 15:36,
against the practice desktop's 108%). Load averages on 6 CPUs, logged each minute: 20-25 at 14:37; the 1-minute
figure reached 57.3 at 15:02 and stayed at 15-37 from 15:25 to 15:33. Docker went unreachable for 10 s at 14:37, and
`wsl.exe` timed out at 15:06. These rows measure contention, not the harness:

| case | correctness (graders) | ended | min | tokens in | tag |
|---|---|---|---|---|---|
| bill-1 | right: `mock_records`, `ledger_readback`, others-unchanged pass | timeout | 16.7 | 297k | host. No trajectory (the bundle pull hit its deadline). Reviews took 35-55 s and calls had 1-2 min gaps. |
| recon-1 | right: `csv_equals` pass | pass | 11.0 | 282k | slow (baseline r1: 2.3 min) |
| bill-2 | record right (`mock_records` pass); `ledger_readback` fails | timeout | 15.6 | 519k | model, plus about 3 min of host (one 126 s call at load 57). Sent back for an undeclared CC-27; the new job typed into /bills/new against the hint and its plan (`x19d1e3554629eed8`). |
| recon-2 | right | pass | 5.8 | 86k | |
| bill-3 | record right (`mock_records` pass); `ledger_readback` fails | timeout | 15.7 | 356k | host. Calls took 460 of the 862 s; 13 gaps of 46-92 s. Sent back for an undeclared CC-14, and the new job ran out of time (`x9a02e6064b9e3ded`). |
| recon-3 | right | pass | 1.4 | 68k | |

No false done in the six rows, and every record and file the graders read was right. Bills 2 and 3 show one pattern.
A value set from the person's answer (the cost centre) was cited in JUDGEMENTS but never declared. The code check
sent the task back, and the sent-back job started again from the request. The owner is fixing it in code: an
undeclared value is adopted into the declaration when it stands in the record's document or in the person's
confirmed words. The run is to be repeated on a quiet machine.

**Read:** the lesson does carry across tasks. Later bills mostly asked nothing required, and the rule reached the
record. The cost and the misses came from how the person's words were kept and shown to the checks. The next run on
HEAD is the validation.

## The office suites (ca-office)

Three kinds of a CA office's monthly GST work, written from screen recordings of real office work (no name, GSTIN,
number or amount of those recordings is in the repo), each a suite whose case 1 is the instance a person demonstrates
once and whose later cases are new months with the traps a real month has. All data is fictional; GSTINs carry the
mod-36 check character. `evals/tools/make_office_suites.py` writes every case (documents, fixture, case.json, oracle)
from its instance tables, works the expected records out by its own arithmetic (apart from the mock's code) and checks
them against hand-written totals; `--check` fails when a committed file differs.

**The mock** (`evals/mocks/ca-office/mock_app.py`, standard library) serves five plain, unbranded apps, chosen by the
first label of the Host header (`chat.`, `mail.`, `books.`, `fileprep.`, `taxnet.` under `example.test`):

| App | What it has |
|---|---|
| ClientChat | a thread per client: messages, PDF attachments to download, a message box (its Send is gated as a send) |
| Mailbox | an inbox (sender, staff label, subject), a mail with its attachments |
| Bookwell | companies (it opens on the company used last), ledgers (Ledger creation defaults to Capital Account), Sales vouchers (the form opens on Payment; a short date like `3/10` takes the first such day in the company's period, so a company spanning two years can put it in the wrong one), ledger vouchers by month, a day book, the month's GSTR-1 from the books and its JSON export into the office's file share (the dialog starts at the folder used last) |
| FilePrep GST | clients (similar names), a period's GSTR-1: import JSON from a share folder (Overwrite or Append), summary, HSN, documents, receivers validated against TaxNet's register (Valid / Cancelled / Invalid, names from the register), a pre-upload check (an unvalidated or cancelled receiver, an invoice twice) and Save to portal; "Open TaxNet as <client>" opens the portal's sign-in with the client's login filled in |
| TaxNet | sign-in, returns calendar and credit ledger, File returns (financial year, quarter - it opens on the current one - and month; a period not yet ended is refused), GSTR-1 tables 4A, 5 (inter-state, over 1,00,000 only), 7 (one row per place of supply and rate, tax worked out), 12 (B2B and B2C tabs, codes from its HSN list) and 13, File Nil, Generate summary, the summary, declaration and signatory, File with EVC, then an OTP page (a plain text field: the code the person reads out) whose Verify files the return and shows an ARN |

The state is `{"collections": ..., "app": ...}`: the graders read the collections (vouchers, ledgers, share_files,
fp_receivers, fp_uploads, tn_returns, tn_b2b, tn_b2cl, tn_b2cs, tn_hsn, tn_docs; the rest is the fixture's and must stay
so); `app` is each app's own memory (last company, last folders, FilePrep's imported data, messages sent). One lock
around every request, the state written atomically on every change, a refused form drawn again with each problem in a
`role=alert` line (400), 413/415 for an oversized or non-form body, and any unexpected error a 500 page, never a stop.
`evals/tests/test_ca_office_mock.py` tests each app's pages, the committing actions, the state writes, the validation,
the OTP and the ARN, odd input and 20 concurrent saves.

| Suite | Cases (case 1 demonstrated) | Graded |
|---|---|---|
| `office-books-v1`: a client emails its month's sales bills (.docx); enter each as a Sales voucher in its company, a ledger for a new customer, then check the month's sales ledger | `ob-1-october` (the books open on another client's company with two of these bills keyed into it; a bill's header names the customer otherwise than its ledger; a new customer with a GSTIN); `ob-2-november` (an inter-state customer, IGST; a new unregistered customer; a bill at 12%; the books open on the client's 2023-24 company); `ob-3-december` (a bill sent twice; a bill already entered from an earlier mail; a bill whose words differ from its figures - the fact, not required: take the figures); `ob-4-january` (bill 1060 is missing from the mail: a required fact gives it, so only asking passes; January dates in a company spanning 2025-27) | `vouchers` exactly (company, number, date, party or a new party's GSTIN, sales ledger, every amount), `ledgers` exactly (group, state, registration, GSTIN) |
| `office-gstr1-books-v1`: file GSTR-1 from the books, through FilePrep, on TaxNet | `og-1-october` (the export dialog left on another client's folder; a new receiver never validated); `og-2-november` (B2B to Goa with IGST, B2C Large, B2C inter-state, three HSN codes, a prefixed series; the dialog left on last month's folder); `og-3-december` (a cancelled invoice in the series; the portal opening on January's quarter; an earlier partial import that Append would double); `og-4-cancelled-gstin` (a receiver the register shows cancelled: the required fact says do not file, say which invoice) | the export in the client's month folder and none in the folder left open; receivers validated (names from the register); the save; TaxNet's tables and the filed return (status, ARN, EVC, signatory, date) exactly; the ARN in the report. Case 4: nothing saved or filed, the invoice named in the report |
| `office-gstr1-chat-v1`: file GSTR-1 from the PDFs a client sent on chat (and its computation workbook), entering the tables by hand | `oc-1-october` (tax-inclusive round amounts, all B2C in the state: Table 7, 12 B2C, 13); `oc-2-november` (a buyer with a GSTIN: 4A and 12 B2B; invoice 205 cancelled in chat and stamped on its PDF: not in the tables, counted in 13); `oc-3-december` (Kerala buyers: B2C Large and Table 7 inter-state with IGST; the portal opening on January's quarter); `oc-4-nil-month` (another client says there were no bills: File Nil) | TaxNet's tables and the filed return exactly (a nil one with no table), the ARN in the report |

Every case approves its committing controls (`Save voucher` and `Save ledger`; `Save to portal`, `File with EVC` and
`Verify`; `File with EVC` and `Verify`), so the gate's yes is the person's and nothing else is granted. Every case that
files holds the OTP as a required fact (a different code each case, in no input): without the expert the case expects
the question, and `not-filed-before-the-otp` fails a return filed without it. The amounts of the chat suite are chosen
so the bills' own taxes add up to the tax the portal works out on Table 7's consolidated value (the generator asserts
it), so a case never holds two right answers.

**Why each case is its own instance (no `carry_state`).** The mock starts fresh from each case's fixture whatever the
mode, so a later case cannot rely on an earlier one's books through the mock; each fixture holds the books as the
months before it left them. `office-gstr1-books-v1` starts from its own fixtures (books already entered), not from
`office-books-v1`'s results, so each return's case is graded alone and a slip in the books suite cannot fail it.
Without carried state every case of a run starts from the same snapshot: empty for the untaught baseline, the taught
worker's with `--from-worker`, so each later case measures that one teaching.

**Calibrated offline** (`go test ./internal/eval -run TestOffice`): `nothing` and `ask` pass no case; every case's
oracle passes on the real mock (12 of 12: form posts under each app's own host, the portal's sign-in kept in a cookie
jar); and `TestOfficeTrapsFailTheirGraders` puts a real month's slip into six solutions (a bill into the company left
open, a short January date, a bill sent twice entered twice, the export into the folder left open, the HSN summary
without the IGST, the cancelled invoice not counted) and each fails its graders. The oracle posts the forms itself, so
the gate, the `office read`s and the approvals are proved only by the live arms.

**Not faithful, on purpose or by limit:** the office share is the mock's (folders are pages, not the desktop's file
system: a case's evidence is its flat case folder, so filing the bills into `<client>/<FY>/<n>. <MONTH>` is not graded;
the case folder stands for the client's month folder and holds them); the computation workbook of the chat suite is
read, not graded (where a block lands and a library save that drops formulas' cached values would make cell checks
judge layout, not work); TaxNet has no captcha and no DSC; 8, 9B, 11 and 14 are shown empty; the OTP is the person's
reply to a HELP (as `facts`), not a call to the client; Bookwell takes Sales vouchers only, and the GSTR-3B side (rent
under reverse charge, credit carried forward) is outside these suites.

## Teaching once, then the suite from the taught worker

The question is whether a worker taught a kind of work once, from one demonstration, does new instances of it. Two
generic pieces make that runnable for any suite; neither names a suite, an app or a workflow.

**`--from-worker <desktop id>`** (`MatrixRequest.FromWorker`, form field `from_worker`): the run reads that desktop's
agent tree and taught state once, before anything is made (`Service.Workers`, the same `PracticeAgentTree` and
`PracticeSnapshot` Simulate pins: `~/.decurix/{skills,experience,playbooks,memory}`), and every case of every arm starts
from that snapshot in place of the empty one: the practice desktop is made with it and every reset restores it
(between repeats too, and between cases unless the run carries state). An arm without its own `@rev` tree runs the
worker's tree (`agent_tree_rev` `worker`, no legacy gate); an `@rev` arm keeps its tree and still gets the snapshot. A
worker id that is a practice or readiness desktop, a worker that cannot be read, or a control plane without `Workers`
is refused with a sentence before the slot is taken. The run keeps `from_worker`, the snapshot's and the tree's
fingerprints (`eval_runs`, migration step 6); the table's head says "Started from worker `<id>` (agent tree <12>,
snapshot <12>)", and each case's run record's `labels.eval` carries `from_worker` and `snapshot_sha`, so a curve says
which teaching it measured. Run the same suite without it for the untaught baseline on the same SHA.

**`teach` / `teach-stop`** (`POST /api/evals/teach`, `/api/evals/teach/stop`; primary listener, humans only): put one
case of a suite on a normal, running desktop so a person can demonstrate it on the very addresses a practice run uses.
It copies the case's inputs to `~/work/practice/<case>/`, copies the case's mock and fixture to `~/work/teach-mock/` and
starts it as the worker on `127.0.0.1:18080` (an earlier one stopped first), and maps the suite's hosts to it in that
desktop's browser the way practice desktops map theirs (`desktop.MapBrowserHosts`: a marked
`--host-resolver-rules` block in the root-owned `/opt/decurix-chromium.init` and the worker's
`~/.chromium-browser.init`, replaced, never stacked). Only names under a reserved test top-level name (`.test`,
`.example`, `.invalid`, `.localhost`) may be mapped, and only to the desktop's own loopback: a real site can never be
redirected in a person's desktop. `teach-stop` stops the mock and removes the block.

**The teach, end to end** (one desktop per suite, or `teach-stop` between suites):

1. Make (or pick) a normal desktop with the hosted brain the eval arm will use, running.
2. `py -3 evals/run.py teach --suite evals/suites/<suite> --case <case 1> --desktop <id>`, then close the desktop's
   browser once so the next launch reads the mapping.
3. Record the demonstration as a person at the screen: `py -3 evals/tools/teach_office.py --suite
   evals/suites/<suite> --desktop <id>` opens a ticket, Records, and drives the pages through the desktop's own VNC
   server (`bench/screen/person.py`; the recorder takes it as a person's), finding each control by its visible label
   (`gui observe --coords`, a look and not an action) and typing the case's own values (from its oracle and facts);
   at each stage it adds the Reason a person would say (where the work comes from, which company or folder, the
   classification rules, why the OTP is asked). Then it Ends the ticket and prints the session id. `--dry-run` lists
   the steps. `evals/tests/test_teach_office.py` follows the same steps by label on the mock offline and checks the
   work ends done. Or demonstrate by hand through the screen viewer.
4. Teach it: in the desktop's chat, `/learn <session> name="<name>"`, answering the lesson's questions as the person
   would (what changes per run, what to check, which steps need the person's OK).
5. `py -3 evals/run.py teach-stop --desktop <id>`.
6. Calibrate, then measure from the taught worker and against the untaught baseline:
   `run --suite ... --arm oracle --arm nothing --arm ask --repeats 1`, then
   `run --suite ... --arm default+expert --repeats 5 --from-worker <id>` and the same without `--from-worker`.

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
