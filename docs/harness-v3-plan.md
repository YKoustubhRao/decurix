# Harness v3 plan: the record is the unit (2026-10-26)

Written after four days and ~220 runs of two suites without one clean iteration. Input: a failure taxonomy over every
recorded run, a structural review of the harness, and an audit of the test infrastructure (scratch reports of
2026-10-26). This file states what is built today and why; harness-v2.md and learning-loop.md stay the long-term design.

## What the data says

- GST suite: 216 runs, 62% pass every check, 57% clean by the owner's bar. Last-10 clean rates per task: 6, 10, 8, 10,
  6, 6, 6, 4, 6, 9 of 10. The chance that ten such tasks are all clean on one build is about 2%. No harness change
  short of raising every task to ~98% makes "16 tasks clean, 2-3 iterations running" likely; the per-task rates are
  the thing to raise, and the runner must never lose a run to anything but the worker.
- 44% of failed runs are the worker's judgement, concentrated on four documents (a two-rate photo bill entered as one
  line, a cancelled bill treated as absent, a service code folded into goods rows, another month's closing figure read
  off a 60-row listing). 11% are "reported unseen". 22% were harness flow bugs (since fixed, each by a rule or a
  check). 10% infra, 7% checker bugs.
- 46% of the 1,152 questions were harness templates (review rounds, "I cannot confirm", plan, judgement calls,
  grounding refusals); gated clicks were asked twice per click; 53 password prompts asked for a value a named file
  held. A review round costs 2-5 minutes; three rounds alone fail a short task's time budget.
- The eval (bills into books): the only two scored runs failed on the same thing - a second customer account created
  for a customer the books already held under another spelling (same tax number). A prose rule for it did not land
  twice. The two-screen loop and "accept" never being a save word were found in the same run.
- Structural: every business invariant lives in prompts (five of them repeat the same rules); code checks are
  regex-shaped symptoms; evidence is value-level, never "this record's field came from that document's field and the
  saved record shows it"; three model judges decide "done" over prose; nothing re-reads a saved record in code.

## What is built today (in order; each generic, nothing names a product or a suite)

1. **Party resolution by identifier** - `gui select REF "<name>" --id "<identifier>"`. The list is searched by the
   identifier first (whole, then its tail, then digits), then by name; an entry whose text holds the identifier is the
   match whatever its name; the result is printed and noted as a *resolution fact* ("matched by identifier", "no record
   holds it (searched: ...)", "ambiguous"). A new party record holding an identifier is refused in code unless this
   task holds a "no record" resolution for that identifier; when a resolution found one, the refusal names it.
   Replaces the prose rule and the shape heuristics of `unsearched_identifier`.
2. **Record Intent Ledger** - `gui record "<key>" --from FILE|screen --fields -` declares, before a record is typed,
   each field's intended value; code span-checks every value against the source's own text (a value not in the source
   is refused with its field named). Before a save, the form's fields are matched to the intent by label words and
   compared (`_same_value`): a typed field that differs blocks the click; a missing or extra field is advisory; a clean
   diff skips the form listing (one click, not two). After a save the page is read back and each field marked
   confirmed / differs / absent; the ledger is a file per task (`evidence/<task>.records.jsonl`), printed by
   `gui records`, shown to the reviewer as a table, and read by the host's final gate as code facts (a typed field that
   differs is NOT_DONE with no model asked). Coverage becomes "a document with no record declared from it".
3. **Scope on the record** - the intent's `scope` words (the entity/book/period the request names) must appear in the
   window title or page words at the save and at the read-back; absent, the save is refused once with what the page
   shows instead.
4. **Questions that the screen answers** - a HELP whose quoted words or values stand in the newest listing of an open
   window, a document read, or the ledger is sent back once with the passage.
5. **Runner that never loses a run to itself** - preflight (ports, worker ready, mock state, folder manifest, memory,
   no other runner, code hash in the worker), infra failures classified and retried once on a recreated worker, result
   rows tagged with iteration/build/code hash, one log per iteration, leak checks between tasks (no lesson, skill or
   method from a run the checks failed; mock DB and folder reset verified).

## Not built today, deliberately

The interaction FSM, the shared evidence store, retiring the word lists, cutting the host judge. Each is right and each
is a day. What is built today is the smallest set that turns the three recurrent outcome errors (wrong party, wrong
scope, wrong field value) into facts code checks before and after the save, and stops the loop paying for its own
infrastructure.

## How it is judged

The owner's bar is unchanged: an iteration = eval F1-F6 then GST hardest-first, each task clean (all checks, no repeated
question, under the cap, within time), 2-3 iterations running. Reported beside it: per-task clean rate over the last
runs, questions split into required (approvals, OTPs) and the rest, and minutes - because that curve is what tells
whether the harness is converging when one iteration is not clean.

## The night of 2026-10-26/27: what the live loop added

Every item below came from a run that failed or burnt questions on the day's harness, was decided in code the same
night, has a test, and names no product or suite. Lessons in `docs/gotchas/part-5.md`.

**Work is the worker's, judgement is the person's.** A review point that is work (a record declared and never saved,
a document read and never named, a listed message never opened, a point whose words say "not entered / not
completed", a point the person already answered) goes back to the worker up to three times, then is carried open -
never put to the person as "a choice I made" (soul `workPoints`, `saysNotDone`, `AnsweredKeys`). Verifier
disagreements on a screen the work did not type into are advisory, told once; the "typed but not looked at" point is
told once a goal; a window the work typed only prose into (a chat message) is not verified at all, before a report or
before a gated action (`Advisory`, `UnseenTold`, `typedFigureIn`).

**The declaration is where a value is decided.** A code of digits is the document's only whole (`3306` for `ABC-3306`
is refused naming the whole token), so the pre-save check cannot be walked around by declaring the record again
(`records.written_whole`). A code with letters joined to its digits is never a "decorated number"
(`_decorated_number`).

**The office tool says what it did to what was there.** An update names every filled cell it writes over, and the
soul forgets a value written over (`noteReplaced`), so the document whose figures are gone is a gap again; a block
copied short of its total row is refused; a date cell takes a year-first text date and refuses a serial; emptying a
label cell needs `clear_text`.

**Messages and inputs.** A row of a message list from a sender whose other message was opened, or whose words say
revised/cancelled about a number of this task, is a Work point until opened or named in the report (messages.go).

**The person's yes and no.** The latest reply that plainly says yes to the very action the gate stops next - fresh,
unused, naming the action, read as a plain yes by the same reading every approval gets - is that action's yes, spent
at once (`approval.note_reply`, `_reply_grants`); the model never touches it. A refusal that asks for something first
("do X, then ask again") stops that action, never the task (`guidance.not_yet`); after any refusal, what the rest of
the task needs is asked for directly (`declinedAdvice`). A reply that opens with "it was my answer" is the answer
(`long_task._choice`). The confirmed press after "Not pressed yet" is not a repeat (`stoppedBeforeActing`); Enter is
held while a listed form is unconfirmed; an approved click whose ref is gone finds its element again by role and
name (`_same_named`).

**Questions the harness answers itself.** A question for names the application lists, and a HELP asking to be
allowed to continue, are pushed back (`asksAppNamesRe`, `asksToContinueRe`). A written-out sum is checked only when
its operator stands on the terms' line - a bullet dash is not a minus (`verify.false_sums`).

**The trace.** Each command's output is kept bounded, a failed command's head and tail; a review's points are listed.
Every VM card has a task dropdown and a Trace page (needs an app rebuild).

Measured on the night's last GST series (soul 06a59c29): T8 46/46 in 10.6 min, T7 16/16, T1-T5 all clean on the first
attempt, T6 13/13; T9 and T10 filed GSTR-1 right and lost GSTR-3B to a refusal path fixed since. The eval set: F1-F3
clean on the deployed harness, F4 fixed in code (the unopened message), F5-F6 not yet rerun on it.
