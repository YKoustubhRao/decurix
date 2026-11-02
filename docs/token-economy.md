# Token economy: why a worker gets cheaper with practice, and what it costs today

Measured on the monthly-bills and GST workflows with a hosted reasoning model (gpt-6 family), 2026-10-31 to 11-01.
The learning loop (learning-loop.md) says what a worker learns; this file says what each run costs, where the tokens
go, and the architecture that makes the fourth month cheaper than the first. Research behind it (two surveys, ~60
sources): `decurix-tests/research/reports/` (outside the repo).

## What a task costs, and where it goes

A task is a loop: the model is called once per turn with the whole transcript (system frame, request, every command and
its output, screenshots). Cost = turns x context, less what the provider's prompt cache serves (billed at ~10%).

One 64-minute run, probed call by call (189 calls, 6.7M input tokens, 2.65M of them not served from the cache):

| Where the full-price input went | Share | Why |
|---|---|---|
| Calls that rewrote an earlier message (47 of 189) | 64% | On these models a change anywhere after the system frame loses the whole cache; the soul's own context maintenance (screenshots taken off, listings digested, outputs masked) fired every 3-4 turns |
| New content on append-only calls (124) | 22% | The real information: a screenshot (~1,100 tokens at 1280x720), the listing, the command's output |
| The final check, the figures check, the lesson (17 calls, their own prefixes) | 14% | Each re-reads the evidence under its own system prompt; nothing cached |
| Output (30k, 3k of it reasoning) | - | Not where the cost is |

Per turn the model read 37k tokens on average (56k max) for ~600 tokens of new text; 172 turns for one task. The
first lever is therefore the cache (keep the transcript append-only; a maintenance pass only when it takes off >= 40%
of the transcript, `skills/context.go`), the second is turns.

## Why month four costs what month one did

Measured learning curve, one worker per arm, months F1->F4 of the same taught task: every check passed, questions the
same (7 and 7), tokens flat (final 1.3-2.1M full-price a month, baseline 1.3-3.5M). Nothing got cheaper because:

1. The taught skill never ran as a skill: reading the request against the taught tasks got an empty model answer
   (800-token budget, all of it reasoning) and the request went to the general engine. Fixed (`llm_client`: an empty
   answer cut off by its limit is asked once more with room).
2. With that fixed, the apprentice run cost MORE than the engine (4.4M vs 3.2M): the model is told the replay command
   is "the quickest way" and works every step by hand anyway, with the skill's text on top of the engine's.
3. It can never graduate: a run that asked the person anything counts as helped (`experience.clean_streak`), every
   run asks 2-4 real questions about the data (is this bill a duplicate, which customer), so three clean runs in a
   row never happen and the replay never runs.

So the worker is an apprentice for life, and an apprentice is the engine plus overhead.

## What the research says (short)

- Memories of lessons and workflows in text do not save tokens. Every system that measured it saw tokens go up
  (ExpeL 1.4-3.3x, Dynamic Cheatsheet 1.3-5x, AWM +17%) for modestly fewer steps. A budget-matched vanilla agent
  matched them (arXiv 2606.15017).
- The measured savings come from replacing model steps with code that replays: -82 to -96% tokens at flat or better
  success (AutoRPA, arXiv 2605.21082), tokens halving by run 2-3 of the same task (GenericAgent, 2604.17091), -58%
  tokens and +13 points (PANDO, 2605.24785). The shape is always: model once, record, replay with no model, model only
  at the step that fails.
- Trust is per step, by counters in code, with a ladder down (compiled -> guided -> model) and up again only after N
  clean verified runs (MACLA, 2512.18950; Stagehand's threshold; AFTER's held-out promotion rule, 2606.23127).
- On-screen "Saved" is not verification: in a fault-injection test it accepted 54 of 72 bad saves; reading the record
  back from the application caught them all (OpenAdapt). Model judges over-approve (no judge above 70% precision,
  AgentRewardBench) and the agent's own claim is the main poison - every strong judge removes it.
- Context: mask old observations, do not summarize them (-50% cost at equal success, "The Complexity Trap", 2508.21433;
  LLM condensers cost +24-94%, 2605.18854); prune in batches, rarely (Anthropic's own loop: "never sensible to break
  the cache by truncating images" at the 10% read price).
- Fewer, larger calls beat many small ones on cost AND accuracy: Agent S3 cut calls 52% and gained 14 points by
  dropping per-step hierarchy (2510.02250); batching several actions per call cut steps 40%+ (UFO2, DMI).

## The architecture: the harness drives the loop, the model handles the exceptions

A taught task that works from documents has three parts, and only the middle one repeats:

1. **Gather** (the model, once): the person's steps before the first document-driven value - open the mail, save the
   attachments, make the month folder. Variable in count and names; the model does it with the steps as its guide.
   This run's documents are the files the gather saved plus any the request names.
2. **The unit, per document** (the replayer, the model on call): from the first step that types a value read from a
   document, to the save. For each document: one small model call fills the unit's values from THAT document under the
   person's rules (`binding.fill`; a document the rules say not to enter comes back as a skip with why); the intent is
   declared (`gui record`: fields and source, span-checked in code); the steps replay (`replay.run`, every step through
   `core.act`: the approval gate, the pre-save field comparison and the read-back apply as to any engine); a halt hands
   THAT document to the model from THAT step (`_by_model(from_step)`), bounded, and the loop goes on.
3. **Finish** (code, then the judge with evidence): the per-document table from the ledger, the figures checked in
   code, the judge given the read-back facts - never the worker's claim.

Trust accrues per step (`experience.steps[key]`): a step replayed with its expectation met counts a verified run; a
heal, a halt or a takeover at it counts against. A question about the data (which customer, is this a duplicate) is
not evidence about a step and does not reset it. Rungs per step: model (the engine does it) -> guided (the replayer
tries it, a miss goes to the model with the step's intent and the before/after diff only) -> compiled (replayed, a miss
drops a rung). Promotion only after N clean verified runs on different documents than the recording's; a revised step
starts over.

What this makes cheaper, and when:
- From the first run: the unit's ~15 model turns per document become one small call (fill) plus the replayer; a
  six-document month goes from ~170 turns to ~40-50 (gather, exceptions, finish).
- With practice: the plan question before changes goes once the unit is compiled (fewer questions - the intelligence
  measure), the judge reads the ledger instead of screenshots, takeovers get rarer as heals promote alternates.
- What does not get cheaper: gather (genuinely variable), a new customer, a cancelled bill, a disagreement with the
  person's rules - these are the model's, and the right place to spend.

## What is built (2026-11-01)

- The soul's maintenance pass is due by share, never by count (`skills/context.go`), and every call carries a
  `prompt_cache_key`; the transcript is append-only between passes.
- An empty model answer cut off by its budget is asked once more with room (`llm_client`), so a taught task is read as
  one.
- The learner finds the per-document block in a demonstration with steps between repetitions (`learning._find_loops`,
  `_apply_loop`; docs/skills.md "Loops").
- A run whose documents are not in hand goes gather -> fill -> replay -> the rest by hand (`skill_task` "The
  per-document part"; docs/skills.md): the model does what varies (collecting, a document of another shape, a halt),
  the replayer the block per document.
- Found on the way (2026-11-01 night, before any live replay): the learner took a 4-step address repeat for the loop
  (fixed: the loop that covers the most steps, never a block that begins with an address typed differently each
  time); the rig's lesson poll limit (~9 min) cut the lesson short twice (its study asks 8+ questions at ~90 s each).
- A value filled for a taught step that carries a number in its example and none in the value is left for the work to
  read (`binding.drops_number`): "December" is not a folder named "9. DECEMBER".
- First clean live run of the path (2026-11-02 02:46, one month of six bills): every check passed; 166 steps replayed
  with no model call for four bills, two bills of another shape entered by the model, 3 maintenance passes where the
  engine had 29-39; 7.50M input of which 6.07M (81%) from the cache, 1.48M billed in full. Not yet cheaper than the
  engine on month one (1.3M): the model's parts - collecting the documents (~50 turns), the two bills by hand (~40),
  and the soul's own review sending the collecting phase back three times for "documents read, nothing entered from
  them", which is that phase's job - cost what the replay saved. The learning curve F1->F4 on this path is the next
  measurement.
- First learning curve on the path (2026-11-02 03:21-06:00, taught afresh, months F1-F4 of the same workflow, one
  worker): every check passed (28/28, 25/25, 23/23, 24/24), questions 3 / 3 / 2 / 4, billed in full 1.34 / 1.85 /
  0.92 / 1.62M (5.73M for the four) against the engine on the same months 1.32 / 2.12 / 1.98 / 1.31M (6.73M) and
  1.28 / 2.11 / 1.65 / 1.34M (6.38M); cache 78-84% against 53-74%; slower (25-49 min against 16-42). The month with no
  question after the plan cost half the engine's; a month with a "cannot confirm" question cost the same or more. The
  three wastes found and fixed from it: the final check was never shown the files the gather phase saved and asked the
  person in three months of four (`_documents_note`: each file's presence and size, a fact code decides, goes to the
  check and into the report); the rest phase read the gather's "enter nothing" as a rule over the task and asked whether
  to undo the replay's records (the phases are told in order, with what each did); the soul's coverage review sent the
  gather report back three times for entering nothing (its report now says "saved, not entered in this phase").
- Second curve (2026-11-02 06:27-09:53, those fixes in): every check passed again, questions 3 / 3 / 1 / 4, but 1.84 /
  1.42 / 1.05 / 2.37M (6.68M), more than the first. The phases misreading and the cross-check question were gone; the
  cost came from three things still left to the model's word. The documents note reached the check among the person's
  words and read as a claim (`verify.judge(found=)` is now a slot of its own for what code checked, and `_judge` adds it
  on every ending). A folder name the fill numbered after the taught example was held over the one the work rightly took
  from the screen (`binding.NUMBERED` marks it; `_assumed_note` tells the check every worked-out value is an
  assumption). One month never replayed: the fill set every document aside for a party it could not see in the
  books (the fill is told a thing chosen from the application's lists is the procedure's to find). Lesson for the
  measure: one curve's month-to-month swing (0.9M to 2.4M) is as large as any effect yet seen, so a curve is now run
  twice at once (a replicate on a second worker) before a fix is believed.
- Not yet: trust per step (today a run's clean streak, `experience.clean_streak`), the judge reading the ledger instead
  of screenshots, each document's extraction kept by content hash.

## Measuring it

Per skill, per run (`experience.note_verdict` tokens, the task record's executions): tokens by phase (gather / fill /
replay / takeovers / finish), model calls, steps replayed vs recorded, heals, halts, cache share, the read-back
verdicts. A curve is only believed against the engine on the same months (the `ab-base` arm) and on documents the
recording never saw. The rig: `decurix-tests/wide/erp/scorecard2.py` (tok and uncached columns), `probe_proxy.py`
(where each call's tokens go).

## Handing work to a local model: earned, never assumed

The owner's idea (2026-11-01): the brain stays Claude / OpenAI / Azure / Ollama, but hands some of its work to local
open models (Brainiac: many open models behind one OpenAI-compatible endpoint, chosen per machine) to save money.
Hermes Agent does a version of this with an `auxiliary` block: compression, vision, web-page summaries and titles can
each go to a cheaper model; the default is the main model, and nothing measures whether the cheaper one was right.
Brainiac's own design already states the right rule for us (its "recipes, earned like Decurix methods"): a cheaper
path for a kind of work becomes the default only after N checked runs agree with the heavy path, is tried in shadow
first (the person always gets the heavy path's answer while it is tested), and goes back to learning on one miss.

What that means here, in order of safety:

1. **Never a judgement.** The final check, the figures check, the grounder's choice among controls, reading a request
   against the taught tasks, filling values from documents: each decides something a wrong answer costs real work.
   CLAUDE.md already says so; a small model saving tokens on these is the one trade the product does not make.
2. **Work whose output code checks is where a handover can start.** OCR of a scanned page (checked by the ledger's
   span check and the figures check), reading a document into text, a screen listing's digest (today done in code
   with no model at all - the cheapest handover is to no model), speech to text. Brainiac serves these as roles with
   their own specialist models, and `brainiac eval` scores them against checked cases.
3. **The brain itself only when earned.** A kind of task that replays (above) spends its model turns on the gather and
   the exceptions. Those turns could go to a local brain only after the parity suite (Brainiac DESIGN "what comes
   next" 2) shows the same checks passing over N runs, with the heavy path still answering in shadow. The lower
   confidence bound of the pass rate, not the mean, decides (20/20 does not beat 19/20).
4. **Plumbing first, before any routing.** The control plane now has an `openai` provider (any OpenAI-compatible
   endpoint, Bearer auth, plain http allowed on the own network): a desktop can be created on a Brainiac box as its
   brain today, which is how the parity runs are made. Routing *within* a task (this call to the local model, that one
   to the hosted) needs the per-call kind the probe already logs and a place to record each kind's checked results;
   that is the next piece, and it is a measurement before it is a feature.

Money is the third priority. Nothing here moves until the per-document replay is clean and measured, because the
replay removes more model turns than any handover could cheapen.

## What not to do (each was tried or is contradicted by the evidence)

- A maintenance pass by count (every N turns, N screenshots): it costs more cache than it saves (gotchas part-5).
- Summarizing the transcript with the model as the default: longer trajectories, invented state (83% of failed runs in
  one study acted on a summary entry no observation supported, 2610.20927). Compaction stays the last resort.
- Lessons in prose as the lever for cost: they help correctness, not tokens.
- A smaller model for a judgement to save tokens (CLAUDE.md): checks in code first, the heavy model for what is left.
- A "trusted" skill that skips the per-step expectation checks or the pre-save comparison: trust changes who is asked
  and how often, never what is checked.
