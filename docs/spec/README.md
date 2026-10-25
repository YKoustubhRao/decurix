# The person-worker interaction protocol, in TLA+

[docs/harness-v2.md](../harness-v2.md) (section 7, principle 6) asks for one explicit, checked state machine for the
person-worker protocol. This folder is the first step: a TLA+ model of the protocol **as it is implemented today**
(the working tree of 2026-10-23, including the 16:34 changes to `computer/approval.py` and the soul), model-checked
with TLC. It is a description of the code, not of the v2 design: where the code has a bug, the spec has it too, and TLC
finds it.

| File | What it is |
|---|---|
| `Interaction.tla` | One ordinary task run by TheBot's soul: the operator loop, the scheduler, the host (thebot_harness, watchers, guidance, approval.json, the final gate), the person and the worker model. |
| `Interaction.cfg` | Safety run: every invariant, the constants used. |
| `InteractionLive.cfg` | Liveness run with a cooperative person. |
| `PlannedRun.tla`, `PlannedRun.cfg`, `PlannedRunLive.cfg` | A planned run (long request) waiting on the person's answer: `long_task._take_message`, `classify`, held requests, `confirming`, `last_heard`. |
| `check.sh` | Runs TLC once per property (and per variant), so every violation gets its own shortest trace. |

RESULTS_GO_HERE

## What is modelled, and from where

**The soul operator** (`agent/thebot/internal/skills`). One record `s` holds the control fields of `operatorState`:
`AwaitingJudge`, `Settling`, `Pending`, `JudgementQuestion`, `AnswerUnreviewed`, `JudgementRounds > 0`, `DoneRounds`,
`ApprovalRounds`, `SentBackRound = JudgementRounds`, `PersonAsks`, `CommandsSucceeded` (up to `minJudgementCommands`),
`CommandsSucceeded > JudgedAt`, `LastIssues` (empty or not), `ScreenConfirmed`, `UnstartedNudged`, `UntracedNudges`,
`AskedUntraced`, the loop's local `repeats` counter and the try count of the model call.

- One step of the spec is one model reply, handled as `loop()` handles it: first the `AwaitingJudge == "approval"` block
  (`operator.go` 1293-1321), then the switch on DONE / HELP / COMMAND.
- Also modelled: `judgementsDue` with both caps; `weighJudgements` with the send-back once per round, the `PersonAsks`
  cap and approval folding (no question at an approval); `openPointsNote`; `reviewInProgress`; `review()`'s two tries; and
  `verifyCommit`, folded into the review's outcome.
- `pause()` and `ask()`, `resume()` with its early return on `Pending`, `ScreenConfirmed`, `AskedUntraced` and
  `JudgementQuestion`, the `transientModelError` retry (`maxModelTries`), `repeatedCommandNudge`, `unstartedScreenNudge`
  and `groundingCheck` (the untraced-values question).

**The soul scheduler** (`internal/scheduler/graph.go`). `restore()`: `safety.IsApproval(reply)` puts the paused action
into `e.approved`, which lasts for the job. `complete()`: needsApproval, then needs_input (a question); `fatal`, then
failed.

**The host** (`agent/decurix_agent`):
- `thebot_harness._handle_job`: needs_approval asks an approval; needs_input is "Stopped, as you said" when
  `guidance.refused()`, else `_escalate`; failed is an error report; answered goes to the final gate.
- `watchers.answer_waiting`: `approval.resolve_reply` settles approval.json's pending approval with *any* reply, then
  `guidance.note_reply`, then STOP_ONLY ends the task (`_called_off`). A paused confirm question goes to
  `skill_task.resume`. A "stop" sent while the worker works is not a reply file and stays queued (`watchers._pass`).
- `computer/approval.py`: `check()` sets the pending approval or uses a grant; `resolve_reply` uses `read_reply`, where
  three words or fewer go to the word list and anything longer to a model that says "yes", "if" or "no".
  `is_approval`, `is_refusal`, `REDIRECTS`.
- `guidance.refused()`: a no ends the task only if no recorded work (screen events or command outputs) followed it. A
  click the gate stopped is a recorded screen event too (`core.py` calls `audit.record` with `outcome
  "needs_approval"`).
- `skill_task._gate`: "Stopped there" on a refusal; the verdicts DONE, NOT_DONE, UNSURE and CANNOT; `_retry_engine`
  while `attempt < MAX_MODEL_ATTEMPTS`; the confirm question, capped by `MAX_CONFIRM_ROUNDS`; `_confirm_reply` (done,
  a vague yes, no/stop, or anything else, which retries); `_question_gate`, which answers a question from the person's
  earlier words up to `MAX_SELF_ANSWERS` times.

**The person** is nondeterministic. They answer an approval with yes, "yes, provided ...", a bare no, "No - don't file
yet, do X first" (no redirect word), "No, use Y instead" (a redirect word), stop, or something else. They answer a
question with an answer, "yes, ...", "no, ...", "use your judgement", stop, or something else. They answer the confirm
question with done, yes, no, stop, or something else. They may send stop while the worker works, and they may never
answer.

**The worker model** is nondeterministic. Each turn it may run a command, type values it cannot trace, repeat its last
command, send DONE (a list at an approval), send HELP, ask in a HELP to approve the click, or attempt one of the
irreversible actions. Any model call may fail transiently. Each review is none, points, web-only or failed (both tries).

**The planned run** (`PlannedRun.tla`) is modelled at the level of messages: the run is parked on one question
(`conclude`, `awaiting_answer`, `waiting_for_query`). The person sends their answer, a new request, a status question,
stop, or a reply to the held-request or unclear question. `classify` reads each message nondeterministically. The
transitions are `_take_message`, `_pending_request_reply` with `_choice`, `_take_new_request`, `confirming`,
`last_heard` / `HEARD_AGAIN`, and `_ask_again` with `state.last_query_text`.

## What is abstracted (assumptions)

- **Values and text are abstracted to kinds.** A reply is one of the kinds above, and a review is "points" or not.
  Word lists are modelled by which kinds they accept: `is_refusal` accepts "No - don't file yet ..." and rejects "No,
  use ... instead".
- **One task, at most two irreversible actions:** `G`, a click the on-screen gate stops, and `S`, a shell command
  `internal/safety` stops. A grant always matches a re-click of the same button (the fingerprint is stable, and the
  15-minute expiry is not modelled).
- **The worker model's budget** (`MaxCmds`, `MaxHelps`) stands in for `Turns`, continuations and the job's 25-minute
  limit, none of which are modelled. It is an assumption about the environment, not the code.
- **Caps are smaller than in the code** in some configs (they are listed in each `.cfg`). This removes no path, it
  only makes counterexamples shorter.
- `minJudgementCommands` is 1 instead of 4; `Hosted` is assumed (the judgement machinery runs only for a hosted model).
- **Not modelled:**
  - the once-only push-backs that only delay a turn (ungrounded DONE, early DONE, pictures, human check, lookup,
    premature, success and failure nudges, slips, quotes);
  - the shared-folder app nudges and the out-of-turns question;
  - `_carries_on` (OUT_OF_ROOM), the model writing `NEEDS_APPROVAL:` itself (`_written_pause`) and taught-skill runs
    (`paused_skill` modes other than confirm);
  - `&&` chains (`pausedRest`), recovery and replanning in the scheduler, and multi-step plans.
- **The final check** never calls a task DONE whose irreversible action did not happen. Otherwise its verdict is free.
- **The obedient-model variant** (`ObedientModel = TRUE`) assumes the worker never re-sends an action the person
  declined in its job, and never asks in a HELP to approve a click. A violation that survives it is the harness's own.
- **PlannedRun**: given the "you sent this same message before" hint, `classify` reads the answer as the answer. This
  is an optimistic assumption in the code's favour. Without the hint it may misread an answer as unclear, a change,
  unrelated or a new request. `new_request_reason` (files, the same request again) is not exercised.

## How to run

TLC needs Java 11 or later and `tla2tools.jar` in this folder (it is git-ignored):

```sh
curl -fsSL -o docs/spec/tla2tools.jar https://github.com/tlaplus/tlaplus/releases/download/v1.8.0/tla2tools.jar
```

On the Windows dev host (no Java 11 there), in the Decurix distro's Docker:

```sh
wsl.exe -d Decurix -u root -e sh -c 'docker run --rm -v /mnt/c/Users/Administrator/work/decurix/docs/spec:/spec -w /spec eclipse-temurin:21-jre sh check.sh'
```

Run a single TLC check directly with `java -cp tla2tools.jar tlc2.TLC -workers 2 -deadlock Interaction.tla`. This
checks every invariant in `Interaction.cfg` and stops at the first violation. `check.sh` runs one property at a time
(`sh check.sh Interaction ApprovalIntegrity`) and writes `out/<run>.txt`. Each Interaction run uses a VIEW: the
control state plus the ghost fields that property reads. No step reads a ghost field except `g.committed`, which the
view keeps, so this is sound for each invariant and cuts the state space by orders of magnitude.
