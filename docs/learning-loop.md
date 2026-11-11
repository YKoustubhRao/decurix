# The learning loop: cases and methods

Status (2026-10-23): the design, written after eleven fresh-build iterations of one hard task did not converge. It builds on [harness-v2.md](harness-v2.md) (facts, lineage, verification) and adds what v2 left out: a model of the task itself, and a worker that gets better at a kind of task each time it does one. Nothing here knows any domain.

Built (first cut, not yet measured live):

- **A. Case matrix** at every commit gate: soul `case.go` (`caseMatrix`), beside the verifier. Sources are the documents, sheet rows and the screens of windows other than the one being committed (`otherScreens`; a screen's lines carry their window, `screenSource`), the request, the person's words and what earlier tasks kept (a `memory` unit). An explanation counts only when its quote of at least three words stands in one of those.
- **B. The plan**: soul `plan.go` (`planBeforeChange`), once per goal, at its first changing command (`gui type/select`, `office update/create/email`); put to the person when the plan commits something and no method is known (`methodMarker`). Their answer is applied before anything is entered, shown to the reviewer, and returned in the result's `plan`. Each commit is `{"what", "control"}` (the control's words as the screen shows them, or ""; a plain string is still read, its control not known: `parsePlan`). The question says "Then, each with your OK first: ...", and that word is kept in code (2026-11-08): once it is said, the plan is `promised` and every gui command of the goal gets `DECURIX_PLAN_ASK_FIRST` (a JSON list, one control per commit); `core.act` puts a committing press (what `_commit_check` finds it commits with) that one of them names, as a whole phrase either way, to the approval gate (`_plan_hold`, its `require`), so it goes only on the gate's yes for that one action, whether or not the gate would ask on its own (a local form's Save). A commit whose control is not known holds every committing press. A plan not put to the person (a known method, a stage of an agreed plan) promises nothing and holds nothing.
- **C. Methods**: host `methods.py`. The plan of a run checked done, with the person's words about it and every number of two digits or more taken out, is kept per kind of request (`playbooks.shape`, matched as playbooks are) and given to the next one (`MARKER`), so the plan is not asked again. It is kept from `skill_task._playbook` on a done verdict, only when the plan writes or commits something; at most `MAX_METHODS` (200), and the person's answers on the run - the plan's question's and every later one's, the last eight in all - each kept with the question it answered (`{asked, said}`, 2026-11-08: an answer kept alone - a "don't know" to the plan question - reached the next task as the person's word about it; it is now offered as "Asked on an earlier run: ... - the person answered: ..."). They are offered under "where an answer differs from the plan above - what it writes, how it is laid out - the answer holds": found live the same day, the plan's question got "I don't know", the layout came in answer to a later question, the method kept only the first with the plan's own idea of the output, and the next task of the kind wrote that file and asked nothing (a false done the check passed). Only TheBot on a hosted brain reads one (`thebot_harness`). Not built yet: the proof that a method reproduces the checked run's result, and maturity for methods (below).
- **Facts the sources stated**: host `kept_facts.py`, on a desktop with a hosted brain. At every task's end, up to five statements a later task would need (a record cancelled, corrected or paid; an instruction), each quote checked to stand in what the task read, are kept in the worker's memory (category `fact`) as `About <client>`, the client the request names (checked to stand in the request's words). A request that names no client keeps no facts, and a fact or place holding anything shaped like a secret is never kept (2026-11-02: memory is the desktop's, shared by every client's tasks). Up to four places the task read from (a chat's address, a folder) are kept too, category `place`. Lessons kept what the person said; this keeps what the world said.
- **Memory**: host `memory_context.py`. Memory is append-only and dated: a correction is a newer entry, nothing is deleted or rewritten. When everything fits in `MEMORY_CONTEXT_CHARS` the prompt shows everything, byte for byte as before. When it does not, the room goes to what this task needs: up to a third to the newest of what the person taught (`lesson`, `workflow`), then every entry about the party the request names (`About <party>`; a party with a name alike comes last), then entries sharing the request's words (BM25), then the newest. A `fact` is shown only to a task whose request names its party (`_scoped`): one client's facts never reach another client's task, and a fact kept with no party reaches none (a line says how many were left out; `gui recall` still finds them). A first line says how much is not shown, so an absence never reads as "never said", and `gui recall WORDS` searches all of it. No embedder and no model on the way: the choice is the same for the same request and memory, so a task's prefix stays byte-identical. Taken from Zep/Graphiti (validity over time, never overwrite), Letta (a small core plus a searchable archive) and Agent Zero Memory (provenance, abstain), built on the files already there. See [gotchas part-5](gotchas/part-5.md), "Newest-only memory".
- **The test protocol**: tasks that depend on each other run as a series on one worker with its memory kept (`gstrun.py T8 T9`): a task alone on a fresh build cannot know what the task before it read.

## Why eleven iterations did not converge

1. **The test measured the wrong thing.** Every iteration was a fresh build (empty memory) given the hardest task cold, answered by a person who does not hold the client's facts either. The task could pass only if the worker happened to look in a channel nothing pointed it to. No harness change makes that reliable; learning does. A real office learns once where its clients' messages are, and every later month starts from there.
2. **The worker has no model of the task.** Nothing lists the records the work covers (a serial range of documents, the lines of a statement), the places facts about them are kept, or what each place says about each record. The work reasons over one long transcript, so a discrepancy (a bill on file that the register leaves out, a payment made on figures without it) is noticed by chance and decided by whoever guesses.
3. **The person is the error detector.** Fifteen questions a run, most of them caused by 1 and 2: refusals, re-asks, "did you check".
4. **Fixes landed on the interaction surface** (how replies are read, what a prompt says). That surface is large and was never specified, so each run found a new edge. The fixes were right; they could not converge.

## The model: a task is a case, a kind of task is a method

**A case** is one task's working file, kept by the harness, not by the model:

| Part | What it is | Who fills it | Who checks it |
|---|---|---|---|
| Scope | the records the work covers, each by its key (document numbers, row ids, people) | the model proposes, from the sources | code: every record has an outcome |
| Sources | every place facts about those records are kept (files, sheets, chats, mail, portals, the person) | the model proposes; the person adds; a method lists them | code: each was read (coverage) |
| Facts | what each source says, with where it says it (v2 evidence units) | extractors, never the model's memory | code: lineage, span checks |
| Matrix | each record against each source: present, absent, or what it says | code, from the facts | code: a record the sources disagree about must have an explanation a source holds, or the person's decision |
| Decisions | the person's answers, scoped to records or to the kind of task | the person | kept word for word |
| Derivations | groupings and totals, run as code over the facts (v2) | the model writes, the harness runs and records | code: reproducible |
| Commits | the irreversible actions (file, pay, send), each with its dossier | the gate | the person, on the dossier |

**A method** is the reusable part of a case for one kind of task: how scope is found, which sources hold what, which reconciliations must hold, which rules apply (the person's, with where they said them), where the commits are and what goes in their dossiers. It is learned, never written into the harness:

- **First time (exploring).** The heavy model works the case with the person. Early, once, it shows its plan in the case's terms - the scope it will cover, the sources it will read, what it will commit - and asks what is missing ("is there anywhere else facts about this month are kept?"). That one question is worth more than the fifteen it replaces.
- **After a checked run (distilling).** The case becomes a method: the sources that turned out to matter, the checks that caught something, the person's corrections turned into rules, the derivations as scripts. A method is kept only if it reproduces the checked run's result on that run's own facts (the proof playbooks already use).
- **Next time (apprentice).** The method drives: its sources are read first (coverage enforced by code), its matrix and reconciliations run in code, and the person is asked only about records the sources disagree on and for approvals. The model still decides each step and every gate still applies.
- **Trusted** after N clean checked runs with no help (the maturity rule skills already have). One failure or rescue and it is learning again.

**Built: a field's unit is asked once.** A field whose label states no unit (a tax table's "Rate", no "%") made every save of "18%" against "18" wait on the person, once per task, every task. The save now asks whether that field takes its figures in the unit, as a numbered question, and the person's recorded reply is kept as a learned `field_unit` row for that field: the application (host), the form (the page path without the record's own last part) and the label's words, all equal (`agent/decurix_agent/unit_answers.py`; `GET /api/desktops/learned?kind=field_unit`). Later tasks compare by it and do not ask. Only the person's word backs it, as the control plane recorded it, and a control plane that cannot be read backs nothing. A host-less application (not a web page) is answered for its task only.

**Built: the person's words on earlier tasks back later ones** (2026-11-08, found live in learn-v1). A rule the person taught once ("from now on, region 07 goes to fund FND-3") backed nothing two tasks later: the value the worker derived from it was refused at `gui record` and at the final check as backed by nothing a person said, because only the current task's answers were read. Now the person's own answers on any task of the same worker are a source of their own (`records._free_text`, so `given_sources`, `given_text`, `option_backed` and `wordy_identifiers` read them): `person_record.worker_words`, read through `GET /api/desktops/judgements?scope=worker&kind=answer` (newest first, at most 100 by default and 200 at most; only the caller's own worker; only a person's or the scripted expert's own words, never a derived row, a worker's row or a secret one). It is read again when the task changes and fails closed when the control plane cannot be read. A value is backed when it stands in those words, exactly as for the current task's answers: a value derived by applying a taught rule stands in the rule's text; a value that stands nowhere is still not backed, and the worker's own copy of the rule in `~/work` backs nothing. What this does not decide: the words back every value they name (a rule that names four codes backs each of them), so whether the rule was applied right is the final check's model's, as it is for an answer given in the task itself.

On a practice or eval desktop the control plane is the practice broker. Until now it answered 404 to the judgement read, so the scripted expert's replies backed nothing at all, not even on the task they were given in. The eval runner now keeps each expert reply on the broker (`Broker.KeepReply`, actor kind `scripted`) and sends its judgement id with the reply, as the control plane sends a person's; the broker serves them on `GET /api/desktops/judgements` to the run's desktop only, and forgets them when the desktop is reset to its snapshot (a carry-state run keeps them).

A code among codes that share their letters ("FND-3", "FND-8", "FND-9") is picked out when the words write it whole (`records._code_written_whole`): its figure is joined to the letters, so the word rule alone never let any text back it.

Skills (from demonstrations), playbooks (paths and tools), lessons (the person's answers) and guides (how an app is worked) stay what they are. A method is the place they meet for one kind of task: it points at the skill for a portal, the guide for an app, the lessons that apply, and the tool distilled from a derivation.

## Code, model, person

- **Code** holds the facts, computes the matrix, runs derivations and reconciliations, enforces coverage and lineage, and blocks only on mechanical gaps: a source in the method never read, a record with no outcome, a disagreement with no explanation, a value with no lineage.
- **The model** reads screens and scans, proposes scope and sources, maps fields, writes derivations, handles what the method did not foresee, and explains disagreements from what it read.
- **The person** decides what the sources cannot (a record they disagree on), approves commits on a dossier, and corrects the plan. Every answer becomes part of the method.

## Where it is enforced

| Point | Check |
|---|---|
| Plan (early, first time per kind) | the plan in case terms is shown once; the person's additions become sources |
| Every read | facts into the store (v2) |
| Before a commit | the matrix: every record in scope has an outcome; every disagreement between sources has an explanation a source holds (quoted, span-checked) or the person's decision; then v2's verifier diff and the review |
| After the result is checked | distil the method; keep it only if it reproduces the result |

## Measuring it: learning curves, not cold starts

A cold start on the hardest task measures luck. The bar is a curve:

1. **A series per kind of task.** Instance 1 (learning, with the person), instance 2 (another client or month, memory kept), instance 3. Per instance: checks passed, questions (split into required - approvals, OTPs, secrets - and the rest), tokens, minutes.
2. **The bar.** Instance 2 correct with only required questions plus real disagreements; instance 3 cheaper than instance 2 at the same correctness.
3. **Fresh builds still run**, for robustness: a fresh build runs a whole series from an empty memory.
4. **Held out.** A kind of task the harness was never tuned on, learned from one teaching, must show the same curve.

## Phases, with exit criteria

- **A. The case matrix at the commit gate** (soul, advisory first, `CHECK_MATURITY`). Before a commit, a fresh model reads the sources and lists the records in scope with what each source says; code checks every disagreement has a quoted explanation found in a source, or the person's decision. Exit: the iteration-10 run replays as a finding (a bill on file the register leaves out, nothing read says why), with no finding on the saved correct runs.
- **B. The plan, once per kind.** Early in a goal with commits, the model proposes scope, sources and commits; with no method for this kind, the person sees it once. Exit: the first instance of a series asks it and adds the person's sources; the second instance does not ask.
- **C. Methods.** Distil after a checked run, prove on the run's facts, read at the start of the next instance; maturity as for skills. Exit: instance 2 of a series reads the method's sources first and asks only required questions plus disagreements.
- **D. Learning-curve runner.** The test suite runs series with memory kept, and reports the curve. Exit: the curve for the GST practice suite and for one held-out suite.
- **E. The interaction FSM** (v2 phase 3).

## Costs and risks

- One extra model pass per commit for the matrix (correctness first).
- A method that fits one client too closely: it is stated in terms of sources and checks, never values; values never enter a method (as playbooks do with `<value>`); and a method is kept only when it reproduces a checked result.
- A wrong lesson spreading: methods carry the person's words with their date, and a correction replaces, never appends silently.
## Kept facts and memory, since the review of 2026-11-03

- **Places are kept for the client the request was for** (`Where (for <client>): ...`) and shown only to that client's tasks. A place with no client is shown to no task's view.
- **A client is named only whole** (`memory_context.names_whole`): a capitalised word right before (unless at a sentence start) or right after continues the name, and the longest name wins, in any case. It fails closed: a fact may be hidden, never shown to another client. A line written wholly in capitals has no case to read, so there no capital continues a name (as on a line all in small letters): an all-capitals request sees its client's facts (2026-11-08).
- **Kept facts run after the report is sent**, not before it, still before the task lets go of the desktop, and from the whole request.
- **Lessons are the whole worker's** (every client's). They are context for the model, never grounding for a run's values (`request_reading`, E-28).
