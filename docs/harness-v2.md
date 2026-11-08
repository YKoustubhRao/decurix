# Harness v2: evidence-carrying work

Status (2026-10-23): approved by the owner ("full freedom, build to your best"). Built so far:

- Phase 0: literals in a command are not a source of its output (`withoutCommandValues`).
- Phase 1, in the soul, where every typed value leaves (`agent/thebot/internal/skills/evidence.go`):
  - evidence units with their source;
  - an OCR text layer for document pictures;
  - picture-only values kept as uncertain;
  - record integrity at each click (advisory);
  - coverage gaps for the reviewer.
- Phase 2, first cut: the factored commit-gate verifier (`verifyCommit`). It fills the gated screen's blanked figures from the sources, and its disagreements become review points.
  - At a report it also runs (`verifyReport`), on every window the work typed into (its newest look) and on the screen the work leaves when it is of the same site. The screen is looked at again first if the work went on past its last look. At most three screens.
  - Review points found in work done after the round's one send-back go back to the worker again, at most twice more per goal, before the person is asked.

Built since (state on 2026-11-07):

- A second, independent read of pictures: a value read off a scan or photo that no text holds is set against a fresh transcription that never sees what the work wrote (soul `second_read.go`, `secondRead`). Agreement makes it traced; disagreement is a point to check.
- Record integrity on the host, per record: the record ledger (`gui record`, `computer/records.py`), with a field-by-field check before the save and a read-back after it ([harness-v3-plan.md](harness-v3-plan.md), item 2). Coverage counts a document read with no record declared from it (soul `coverageGaps`).
- Party resolution by identifier (`gui select --id`, `computer/resolve.py`).
- A TLA+ model of the protocol as the code has it, not a replacement for it ([spec/README.md](spec/README.md)).

Still to build:

- the shared evidence store in the host, with one lineage service for soul and host (`computer/evidence.py` holds pictures and notes of what a task looked at, not this store; the gate-owned store is O-1 in [follow-ups.md](follow-ups.md));
- `office extract` (span-checked structured extraction) and a `facts` CLI: neither exists;
- reconciliation of mapped quantities;
- the interaction FSM (Phase 3);
- staged contexts (Phase 4). Planned runs ([long-requests.md](long-requests.md)) carry a ledger between stages, but nothing makes a value typed late come from a store.

## Why

The harness passes most of the GST suite, but it still lets the model do the one thing a model is worst at: hold facts. The clearest case is GST suite T9 (2026-10-23, iteration 6):

- About 40 minutes into a long goal, the worker wrote the month's nine invoice lines **from memory** into a Python script.
- It shifted the invoice numbers by one: cancelled 455 went in, and 457 dropped out.
- It invented two HSN codes: 0910 where the invoices say 1905, and 2106 where they say 2001.
- The script printed the rows back. That printout counted as "seen", so the provenance check passed every invented value.
- Only the portal's own HSN master stopped it.

This is not one bug. Behind the 630 entries in `docs/gotchas.md` the same few gaps repeat, and patching each live failure with a rule is how those entries accumulated. Several of those rules were word lists or regexes that later judged correct work wrong (refusal words, redirect words, status headers, party columns).

The goal of v2: **the model proposes, the system disposes.** A fact never lives only in the model. Every value the work puts into the world carries its evidence. Code checks the evidence mechanically. Judgement goes to an independent reviewer and to the person, and nothing in the harness knows GST.

## What goes wrong, at the root

| # | Root cause | Live evidence |
|---|---|---|
| R1 | The model is the store of facts. Late in a long goal it recalls instead of reading, and recall decays with context length (Liu et al. 2024, "lost in the middle"; "context rot"). | T9 invoice lines from memory; T7/T9 figures misread late in a goal |
| R2 | There is no data model. Checks run on prose (reports, transcripts, screen text) with regexes and word lists, so they are brittle both ways. | is_refusal / REDIRECTS ended a task on "No - don't file yet"; COMMIT_CONTROL; _SKIPPED_ROW, _STATUS_HEADER, PARTY_COLUMN |
| R3 | Provenance is value-level and can be laundered. A value counts as "seen" if any text shows it, including the work's own echo. Nothing checks that the fields of one record came from one source record. | T9 self-echo; invoice numbers paired with another invoice's amounts |
| R4 | Nothing accounts for completeness or conservation. No ledger says every input record was used or excluded for a stated reason, or that the same total agrees across the workbook, the portal and the report. | T8 photo invoice never read; T9 cancelled invoice counted 0; B2C summary collapsed to one line |
| R5 | The interaction protocol is implicit: about 20 flags across the soul's loop and the host. Loops, deadlocks and double questions came from state combinations nobody enumerated. | T6 deadlock (both sides waiting); 12 repeated questions; refusal treated as call-off; a timeout ended a task |
| R6 | Verification is not independent. The reviewer reads the actor's own account (its ledger and citations), so it inherits the actor's framing. CoVe found that verification helps only when it cannot see the draft (Dhuliawala et al. 2024). | T9: the review passed a return that collapsed B2C lines; the HSN tab split was raised only after filing |

## Principles

1. **Facts live in an evidence store, not in the model.** Values are read through tools that record their source.
2. **Evidence-carrying values.** Every value written to the world (typed into a form, written to a file the person keeps, sent in a message) traces to one of four things:
   - a source fact;
   - the person's words;
   - the request;
   - a recorded derivation over those three.

   A command's output never vouches for a value that stood in the command itself.
3. **Record integrity.** The fields of one target record (a form submitted, a row written) come from one source record or from one derivation row, never mixed.
4. **Accounting.** Every input record in scope is accounted for: used, or excluded for a reason a source, a rule or the person gives. Every quantity reconciles across its representations: computed, entered, read back, reported.
5. **Independent verification.** Before an irreversible action, a verifier re-derives the key figures from the sources in a fresh context, without first seeing the actor's figures. Code then diffs the two.
6. **A specified protocol.** The person-worker interaction is one explicit state machine with checked properties: no deadlock, bounded asks per action, every doubt resolved, asked or carried, and a refusal scoped to its action.
7. **Code blocks only on mechanical invariants:** lineage, record integrity, accounting, reconciliation arithmetic, gates and secrets. Everything that needs meaning goes to a model, as structured output, or to the person. No word lists decide meaning.
8. **Nothing domain-specific in the harness.** Domain knowledge lives in the sources, the person's rules, taught tasks, vouched references and the person.

## Architecture

```
 sources (files, screens, chats, the person)            the world (portal, files, messages)
        |                                                         ^
   [ extractors ]  deterministic first; images read twice        |
        |           (vision + OCR / factored second read)  [ action boundary monitor ]
        v                                                   lineage + record integrity
  +-----------------+     [ derivations ]  scripts over the    |   (blocks values with no lineage)
  | evidence store  |<---- store, recorded, replayable -------+
  | facts, records, |                                          |
  | lineage, ledger |---> [ accounting ]  coverage + reconciliation
  +-----------------+                         |
        ^                                     v
        |                      [ commit gate ]  dossier before an irreversible action:
  [ worker (LLM) ]  proposes                    read-back, lineage gaps, coverage, reconciliation,
  staged contexts rehydrated                    independent re-derivation diff, open doubts
  from the store                                      |
        ^                                             v
        +------------ [ interaction FSM ] <------> the person / CA
                       one transition function, TLA+-specified, property-tested
```

### 1. Evidence store

There is one store per task (and per planned run), at `~/.decurix/evidence/<task>/`. It holds facts:

`{id, value-as-written, normalised (provenance.py), label, record, source{kind, ref, span}, method, agreement}`

- **`record`** groups the facts of one source record: an invoice, a sheet row, a chat message, a portal table row.
- **`source.ref`** is precise: `file#sheet!B12`, `file#page2`, a screen element and the URL, a chat message id.

How facts get in:

- **Deterministic extractors**, no model, exact. `office read` of xlsx/csv/docx/text PDFs emits cells and lines as facts, with records = rows. `gui observe` emits screen fields and table rows.
- **Model-proposed structured extraction** (`office extract FILE`) turns a document into records. The model proposes the fields, but every field value must occur in the document's text layer: a span check, in code. A value that isn't in the text is never a fact.
- **Scans and photos.** Two independent reads (vision, and OCR plus a factored second model read) must agree. Where they disagree the fact is uncertain: it gets re-read at higher zoom, and if still unclear it goes to the person. A single read is never enough for a value that will be committed.
- **The person's answers and the request** are facts of kind `person` or `request`, with their exact words.

The ledger the soul keeps today (commands and what they printed) becomes one view of this store, not the only memory.

### 2. Derivations

Computations (groupings, totals, splits) run as code over the store or the source files, never as values the model writes into a script. The harness records each derivation: its inputs, its code hash, and its output rows.

- A derived value's lineage is its derivation.
- At a commit gate the harness can re-run derivations to confirm them (reproducibility).
- The rule from principle 2 is enforced generically: literals in a command are not a source of its output. The soul has done this since 2026-10-23 (`withoutCommandValues`).

### 3. Action boundary monitor

This is the enforcement point the literature converges on: check at the last tool boundary, where the concrete action, its arguments and their provenance are all known (PACE, AgentFlow). It extends the soul's `groundingCheck` and the host's `provenance.py` into one service with three checks:

- **Lineage.** Every value about to be typed, written or sent traces to the store. A value with no lineage blocks, and the worker is told to read the source.
- **Record integrity.** On a record commit (a form saved or submitted, a row written), the values entered since the record opened must all trace to one source record or one derivation row.
  - Mixed lineage, such as invoice 455's number with invoice 456's amount, is a finding.
  - This closes the gap the provenance survey names: parameter lineage misses a wrong *interpretation*. Co-provenance catches the pairing even when every single value is "real".
- **Secrets and the approval gate:** unchanged.

### 4. Accounting

- **Coverage.** The scope of a task is a set of input records: the documents of the period folder, the attachments a chat offered, the serial range of numbered documents the facts show. The model declares the scope as structured output; code computes the serial range and set membership from facts. Every record in scope must end in one of three states:
  - *used* (it has lineage into an output);
  - *excluded* for a stated reason that is itself a fact (a "cancelled" stamp, the person's words, a rule);
  - *open* (blocks the commit gate).
- **Reconciliation.** The same quantity is compared across its representations: derivation output, values entered, the system's read-back (summary pages, a saved sheet), and the report. The model declares the mapping ("the portal's B2C summary for 29-Karnataka 5% equals derivation row X"); code checks the arithmetic within the normaliser's precision.

### 5. Commit gate (irreversible actions)

Before FILE, PAY or SEND, the harness assembles a dossier containing:

- what will be committed, read back from the screen as facts;
- the lineage of each value;
- the coverage state;
- the reconciliation table;
- the independent verifier's diff;
- the review's open points.

Mechanical failures (no lineage, open coverage, a broken reconciliation) block, and the worker is sent back with the specific gap. Judgement doubts go into the approval, as they already do. The person approves the dossier, not a bare click.

### 6. Independent verifier (factored)

At each commit gate a fresh model context, with no actor transcript and no actor figures, is given three things:

- the sources (file paths, read through the same extractors);
- the person's rules and the taught task;
- the list of quantities the dossier commits, with no values.

It produces its own figures. Code diffs the two tables; any disagreement is a recheck point with both lineages shown. This is CoVe's "factored" variant and N-version computation. It costs one extra model pass per irreversible action, which correctness-first accepts.

The existing reviewer stays for the questions only judgement can answer: not done, not taken in, a contradiction, missing knowledge.

### 7. Interaction FSM

Today's state is scattered: about 20 soul fields (`AwaitingJudge`, `Settling`, `PendingReason`, `JudgementQuestion`, `AskedPoints`, ...) plus host-side state (`guidance._refused`, `paused_skill`, confirm rounds, `long_task.pending_request`). v2 replaces it with:

- **One explicit state machine per layer**, each a single transition function `next(state, event) -> (state, effects)`. The states are working, awaiting-answer(question), awaiting-approval(action, dossier), reviewing(round), settling(points), retrying(cause), done and stopped.
- **A TLA+ spec** of the person-worker protocol, model-checked with TLC (the practice TraceFix and others report for agent coordination). Properties checked:
  - no state in which both sides wait;
  - at most one ask per action and per doubt;
  - every doubt ends resolved, asked or carried;
  - a refusal blocks only its own action;
  - bounded rounds;
  - a transient model failure never ends a goal.
- **Property-based tests** of the Go and Python transition functions: random event sequences, with the same invariants asserted.
- **Reply meaning** comes from structured model output (answer / change / new request / refusal-scope), never from word lists. Code handles structure only.

### 8. Rulebook

The person's rules, taught answers, corrections and vouched sources become explicit entries: a rule's text, its scope, and the source of the rule itself. They are retrieved at plan time and at every commit gate. The verifier and the reviewer check the applicable rules against the dossier. Domain knowledge stays outside the harness: in the rulebook, the taught tasks, vouched references, and the person or CA.

### 9. Staged contexts

Long goals are worked in bounded stages. Each stage rehydrates from the evidence store and the ledger, not from a long transcript; the planner and compaction already exist. Lineage enforcement makes this real: a value typed late in a goal must come from the store, so the worker has to re-read rather than recall.

## How each observed failure would have been stopped

| Failure | Stopped by |
|---|---|
| T9 invoice lines from memory: invented HSN 0910 and 2106, shifted numbers | Lineage: 0910 is in no source and no derivation input. Record integrity: 455's number paired with 456's values. Verifier diff. |
| T9 cancelled invoice counted 0 in documents issued | Coverage: 455 in scope with a "cancelled" fact, so its exclusion is visible. Rulebook (cancelled still counts) at the commit gate. Reconciliation of total/cancelled against the facts. |
| T9 B2C (Others) collapsed to one line; HSN B2B/B2C tabs | Derivation grouping by the declared key. Reconciliation of the portal's read-back per place of supply and rate against the derivation. Rulebook. Verifier diff. |
| T8 photo invoice never read | Coverage: an attachment in scope is still open at the commit gate. Double read of images. |
| T10 new block without a header row | Already a write-time note. In v2, the layout facts of the existing blocks are part of the dossier. |
| Report claims something the files contradict | Reconciliation: the report's figures against the store. The final gate stays. |
| Loops, deadlocks, double questions, refusal ending a task, a timeout ending a goal | Interaction FSM and its checked properties. |

## What changes in the code

- **New (not built):** `agent/decurix_agent/evidence/`, with the store, extractors (wrapping `office read`, `gui observe` and OCR), derivation records, accounting and the dossier. This is a Python service, because the host already owns files, screens and provenance. The soul calls it through the `office`/`gui` CLIs and a new `facts` CLI.
- **Merged (not yet):** the soul's `groundingCheck` and the host's `provenance.py` become one lineage service, so there is one definition of "traced". Today they are still two; what the host shares is one value comparator (`values.py`) for the screen's checks, the record ledger and replays.
- **Refactored:** the soul's judgement state machine (`operator.go`, `knowledge.go`, `judgements.go`) and the host's pause/approval/refusal state (`guidance.py`, `skill_task.py`, `long_task.py`) become explicit FSMs with a TLA+ spec. Not done: `docs/spec/Interaction.tla` and `PlannedRun.tla` model today's code, and no FSM replaces it yet.
- **Retired over time, each through the replay gate:** word-list and regex classifiers of meaning (`approval.REDIRECTS`, `REFUSAL_LEAD` for task scope, `_SKIPPED_ROW`, `_STATUS_HEADER`, `PARTY_COLUMN` and the like), as the structured facts they guess at become available.

## Phases, with exit criteria

Each phase is judged by the loop the owner set: a fresh build with `build_app.ps1 -Fresh`, then T9 first; only when T9 passes, then T1-T10. The final bar is 3-4 consecutive clean fresh iterations.

0. **Done today:** literals in a command are not a source of its output (soul).
1. **Lineage and record integrity at the action boundary, and coverage.** Build the evidence store fed by the deterministic extractors and the span-checked `office extract`; one lineage service; record integrity and coverage, *advisory* first (`CHECK_MATURITY`).
   - Exit: the T9 failure of 2026-10-23 replays as a finding, with no finding on the saved correct runs (replay gate).
   - Then lineage becomes blocking.
2. **Commit gate and independent verifier.** Build the dossier and the factored re-derivation diff at FILE/PAY/SEND.
   - Exit: T9 clean in 3 fresh runs.
3. **Interaction FSM.** One transition function per layer, the TLA+ spec checked with TLC, and property tests; delete the flag soup.
   - Exit: TLC passes, every existing test passes, and no regression in T1-T10.
4. **Staged contexts over the store, and retiring the word lists.**
   - Exit: 3-4 consecutive clean fresh iterations of T1-T10, with questions limited to necessary ones (approvals, OTPs, secrets, the client's own facts) plus real doubts.

## Costs and risks

- **Tokens:** one verifier pass per irreversible action, plus double reads of images. This is accepted under correctness > intelligence >> efficiency. The worker's own token use should fall, because it stops re-deriving from memory and stops repeating work after a wrong commit.
- **False findings from record integrity on legitimate aggregates.** Mitigation: aggregates must come from a recorded derivation; the check starts advisory and becomes blocking only after a clean live round, as the existing `CHECK_MATURITY` process does.
- **The model not using the fact tools.** That is handled by enforcement at the boundary: a value without lineage does not go out, whatever the model meant.
- **TLA+ needs Java and TLC on the dev machine.** If not, property-based tests carry the protocol checks alone, which is weaker but still exhaustive over bounded sequences.

## Decisions for the owner

1. The direction: evidence store with lineage, record integrity, accounting, an independent verifier at commits, and the interaction FSM.
2. Cost: a verifier pass at every irreversible action.
3. Blocking policy: lineage blocks at once; record integrity and coverage start advisory.
4. TLA+ with TLC, or property tests only.

## References

- Liu et al., *Lost in the Middle: How Language Models Use Long Contexts*, TACL 2024: https://preview.aclanthology.org/setup/2024.tacl-1.9
- Dhuliawala et al., *Chain-of-Verification Reduces Hallucination in Large Language Models*, ACL Findings 2024: https://arxiv.org/pdf/2309.11495
- *From Agent Traces to Trust: Evidence Tracing and Execution Provenance in LLM Agents* (survey): https://arxiv.org/html/2606.04990v4
- *AgentFlow: A Flow-Centric Policy Language and Framework for Securing LLM Agent Systems*: https://arxiv.org/html/2608.22868v1
- *PACE: Provenance-Aware Capability Enforcement for Tool-Using LLM Agents*: https://arxiv.org/html/2610.01349v1
- *Runtime Policy Enforcement for MCP-Based LLM Agents* (memory blind spot): https://www.mdpi.com/2079-9292/15/13/2829
- Nutrient, *How to catch hallucinated values in document extraction* (grounding each value to a source region): https://www.nutrient.io/blog/extraction-hallucination-detection/
- *TraceFix: Repairing Agent Coordination Protocols with TLA+ Counterexamples*: https://arxiv.org/pdf/2605.07935
- *Agentproof: Static Verification of Agent Workflow Graphs*: https://arxiv.org/html/2603.20356
- J. Phoenix, *Formal Verification for Agent Orchestration*: https://understandingdata.com/posts/formal-verification-for-agent-orchestration/
