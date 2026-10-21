# Harness audit (2026-10-21)

The person's direction: the harness is generic. Tools make correct work easy; code blocks only on domain-free mechanical invariants; judgement goes to the model judge with evidence, and to the person; business rules live only in the request and the operator's taskRules - never in our prompts or code.

## Code checks

Verdicts: **KEEP** = blocking, a domain-free mechanical invariant; **GEN** = rewrite as one of the generic primitives below; **ADV** = evidence for the judge and "Please check", never a verdict; **DEL** = a business rule or word list, it belongs in the request/taskRules. FP = false positives seen on correct work this session.

### Stage checks (`long_task.stage_code_findings`, CHECK_MATURITY)

| Check | Decides | Nature | FP this session | Verdict |
|---|---|---|---|---|
| `file_facts` (bundle, see next table) | file facts below | mixed | via sub-facts | split per sub-fact |
| `repeated_rows` (BK3, `table_quality.inflated_totals`) | an output figure equals a total incl. rows a source repeats | heuristic (a repeat may be real); "said it kept them" word list | series 10/11: switched off by "removed the", then by "excluded" | ADV, loud (amended): domain-free fact (exact duplicate input rows inside an output total). Shown under the report's top "Needs you"; headline "Done - but N figures include rows the input repeats"; no word-list switch-off, the finding stays whenever the figure equals the with-repeats total. office read's Data check stays as a TOOL |
| `coverage` (+`covers_every`, `SELECTING`, `_condition_test`, `LEFT_OUT`/`NOW_COLUMN`) | every record of an earlier table is in the stage's output | business: reads "every/each", thresholds, held/backorder words | P6 refunds >5,000 (3 of 12), P3 proposal, P2 held vs unstatused sheet | ADV |
| `removed_residue` (BB) | a record the person removed still counted in totals | invariant: the id comes from the person's own change | none | KEEP (GEN: "person's removal honoured") |
| `removed_still_there` | the removed record still a full row | invariant (as above) | none | KEEP (same primitive) |
| `deferred_actions` (CC1) | a status says "calculate/check later" | word list | none seen | ADV |
| `unsupported_references` (CC2) | a reference the item's own message does not give | provenance | auto-replies only | GEN: provenance |
| `working` (CF/CH/CJ/CK: `working_problems`, `reproduces`, `_stated_against_working`) | a computed amount its own working does not give | own arithmetic + factor-name guessing (qty/price/tax column words) | CH: GST on top, received qty; CK: order total vs line; CQ: formula working refused | GEN: own arithmetic - only formula evaluation / an explicit "a x b = c" row; factor guessing -> ADV |
| `unworked_amounts` (CI) | an amount with no working | provenance | CI: customer-stated refunds refused | GEN: provenance (amount in no source and no recorded computation) |
| `self_contradictions` (CF2) | a category its own row denies ("no refund requested") | word negation | none | ADV |
| `filler_values` (BR, `invented_typing`) | a typed value that looks like filler and is in no source | provenance + filler-shape list | none | GEN: provenance (drop the filler-shape list) |
| `lost_findings` (BG) | a re-check dropped an earlier flag | flag word list | none | ADV |
| `reply_coverage` (BX) | each input email drafted or explained | judgement (which need a reply) | none | ADV |
| `pending_lookups` (CB) | a row "to be looked up", TBD in a system column | word list + "portal/system" words | none | DEL (taskRules: find the record) |
| `app_emptied` (CS) | an app's save emptied what the tools wrote | invariant: tools' own copy vs saved file | none | KEEP |
| `system_entry` (CR/CT/DA/CX, `entry_clauses`, `_claimed_records`, `_shown`) | claimed entries visible in the system's own pages; duplicates in the list | invariant core; entry-stage and claim detection by verbs/status words | CT: stage 1 ("do not enter") refused; DA: five orders the portal held | KEEP core, GEN detection: entry = the tool record shows saves on a system; claim = the stage's own log rows; read = the system's pages |
| `allocation_order` (CV) | scarce stock served by request time; times vs messages | business (first come) | none, but it is P2's rule in code | DEL (time parsing may stay as a TOOL for the working) |
| `record_prices` (CJ) | a record's price from a catalogue / not its saved record | business (catalogue/price list vocabulary) | CQ: the bill's own PDF; MUG-12 as a record; price list legitimately named | DEL |
| `status_conflicts` (BZ1) | outputs disagree on an item's status | status word kinds | none | ADV |
| `lost_sheets` (CO/CP2) | an earlier stage's sheet changed | mechanical diff + ownership/amendment rules | CO: added column; CP2: wrong owner, amendment | ADV (diff as evidence) |
| `rate_check` (CE) | a bill line's rate vs an item-master rate | business (GST rates, codes) | P1: note read as flag (fixed) | DEL |
| `draft_coverage` (CP1) | drafts to every party a condition picks | business (thresholds) | CP1 dates/ranks as parties | DEL |
| `summary_totals` (DC) | a summary row vs a detail column over counted rows | own arithmetic, but counted-row status words | none | GEN: own arithmetic only where the workbook links them (a formula, a "total of" row); else ADV |
| `draft_facts` (DE) | a draft's date/manager/amount vs its recipient's row | provenance + field words (joining/manager/CTC) | none | GEN: provenance (a name/date/amount in a draft that is in no source) blocks; the per-field record comparison -> ADV |

### File facts (`produced.file_facts`), verify, gui, sources

| Check | Decides | Nature | FP | Verdict |
|---|---|---|---|---|
| `store_results` / `office calculate` | formula results stored | tool | - | TOOL |
| `unstored` | formulas with no stored result | invariant | none | KEEP |
| `typed_totals` / `layout_faults` / `formulas_asked` | typed totals where formulas were asked | request reading | none | ADV |
| `coverage_gaps` (Total row sums less than its column) | own arithmetic | invariant | none | KEEP (own arithmetic) |
| `missing_requested` / `misplaced` | a file the request names not made / not where named | invariant when the request names it | none | KEEP |
| `document_findings` placeholders | `{{X}}` left in a document | invariant | none | KEEP |
| `image_findings`, page-break facts | size / breaks the request asks for | request reading | none | ADV |
| `held_backorders` (AZ) + `HOLD_RULE`/`BACKORDER_RULE` | a held line backordered | business | CN: judge vs code conflict | DEL |
| `_source_mismatch` (`sources.mismatches`) | a figure "per X" that X contradicts | provenance-ish | none | GEN: provenance |
| `draft_disclosures` | a draft names another party with figures | heuristic | none | ADV |
| `payees.mismatches` | a payment account not on record | provenance | none | KEEP (as provenance) |
| `unknown_recipients` / `typed_recipients_refused` | a recipient in no input | invariant | none | KEEP |
| `payees.suspicious_messages` | messages asking to change bank details | heuristic | none | ADV |
| `verify.false_sums` / `false_item_totals` | stated total contradicts its own stated terms | invariant (own arithmetic) | P2 "Rs. 35" parsing (fixed) | KEEP |
| `verify.program_numbers` (+ office float clean-up CZ) | 7640.220000000001 written | invariant | none | KEEP (+TOOL) |
| `verify.left_undone` | report says something is left | word list | - | ADV |
| cross-check (`code_disagreements`, `model_disagreements`, `fact_value`, `identifier_like`, `figure_sign`, percent) | the same figure differs between outputs | normalisation is generic; verdict was code | BU/BU2/BU3/BU4: dates, lakh, signs, ids, % | TOOL (normaliser) + ADV (to the person), never reopen on code alone |
| `filler.invented` (gui) | a form value in no source | provenance | none | KEEP (drop filler-shape list) |
| `crossed_values` (CY, gui) | a form value is another record's, not its own | provenance vs the tied row | none | KEEP narrowly (exact equality to another row, differs from own); else ADV |
| `already_entered` / `_duplicate_record` (CX, gui) | saving a record already saved as new | invariant | none | KEEP |
| `placeholder_fields`, number-arrow/unsafe-save refusals, form listing | probe records, mechanics | invariant / tool | none | KEEP / TOOL |
| `source_guard` | a person's file altered | invariant | AB re-save (fixed) | KEEP |
| approval gate, `privacy` | send/pay without approval; secrets | invariant | none | KEEP |

### Judge and answer handling (code deciding what words mean)

| Mechanism | Nature | FP | Verdict |
|---|---|---|---|
| `code_rules` / `CODE_RULE_GUARDS` / `HELD_LINE_RULE` | business rule in code | CN | DEL |
| `_decision_refusal` + `_GO_WORDS`/`_STOP_WORDS`, `decided_directions` | word lists over the judge | BL | DEL: the person's answer goes to the judge as binding evidence |
| `KEEP_CLAUSES`/`affirmative_change` (CD) | word lists for "change vs answer" | CD paraphrase | DEL: one model classification with the person's words verbatim |
| `RESERVED`/`BLOCKING`/`_reserved`, `DELEGATES` | approval/delegation word lists | BI/BY | GEN: model reading; code keeps only "a yes is one action's" |
| question routing: `ABOUT_OWN_TOOLS`, `OWN_MECHANICS`, `OTHER_MEANS`, `PERSON_ONLY`, `fields_in_inputs`, `source_asked_for`, `request_settles` | phrase lists + one model call | AK/BM/BP/BW layers conflicted | GEN: one model call "who is this for" with evidence; keep `person_keeps` (secrets/captcha/approval) and the 2-bounce cap |
| `PROBE_RECORD`, `_wrong_in_app` | worker's own declarations | none | KEEP (`WRONG IN THE APPLICATION` is the worker's own line); probe regex -> ADV |
| `decision_problem`, `_unchanged_after_retry` | structural | none | KEEP |

## Target architecture (code checks)

Code blocks only on these primitives, each domain-free, each fed by tool records, never by business words:

1. **Seen in the system** - a record the stage claims to have entered is shown by the system's own pages in the tool record (number, else name + reference); no action on a system = nothing entered. Plus **no duplicate**: a save repeating a record already saved in this task is refused.
2. **Provenance** - every value typed into a form or written into a draft/output that is a name, number, date, account, address or reference traces to a source the request gives, a person's message, the system's own pages, or a recorded computation. (Absorbs filler, crossed values, accounts, recipients, unsupported references, unworked amounts, invented names in drafts.) One normaliser (`fact_value`: dates, units, signs, %, grouping, currency) for all comparisons.
3. **Own arithmetic** - a figure contradicts its own formula, its own written working ("a x b = c"), its own Total row, or terms it states beside it.
4. **The person's files and changes** - a file the person gave is unchanged; a record the person removed is gone (rows and totals); an app did not empty a file the tools wrote.
5. **Hygiene** - placeholders, secrets, program-printed numbers, unstored formulas, a named file missing.
6. **Gates** - sending/paying without approval; unknown recipients.

Everything else becomes **evidence** for the judge (diffs, office read's Data check, cross-check normalised pairs, kept-sheet diffs, status disagreements) and goes to the person under "Please check" - never a code verdict. One loud advisory: a figure equal to a total that includes exact duplicate rows of an input goes under "Needs you" at the top and into the headline ("Done - but N figures include rows the input repeats"), with no word-list switch-off. Business rules (holds, refunds, rates, first-come, backorders, units) live only in the request and taskRules. The replay gate stays as the regression net for the primitives, run on every change.

**Implemented (2026-10-21).** `long_task.CHECK_MATURITY` holds exactly the blocking set above (`file_facts`, `removed_residue`, `removed_still_there`, `app_emptied`, `system_entry`, `filler_values` and `draft_facts` as provenance, `working` as own arithmetic); every other check is ADVISORY, passed to `_judge` as evidence and listed under "Please check"; `LOUD` = `repeated_rows`. Provenance and the one normaliser are `provenance.py`. Question routing is `route_question` (`ROUTE_SYSTEM`); hand-backs, "keeps what is done" and "for approval" are model readings (`hands_back`, `keeps_done`, the planner's `for_approval`). Deleted: `rate_check.py`, `allocation_order`, `record_prices`, `pending_lookups`, `draft_coverage`, `held_backorders`, `code_rules`/`CODE_RULE_GUARDS`/`HELD_LINE_RULE`, `_decision_refusal`, the question phrase lists, `KEEP_CLAUSES`, `RESERVED`/`BLOCKING`/`DELEGATES`.

**Independent review (2026-10-21) - narrowed further.** Provenance blocks only amounts, identifiers (by digits) and addresses that no source holds and no single arithmetic step explains; dates and letter-and-digit codes are evidence; `own_work_only` tells the judge what only the work's own files explain. `source_guard` reads the whole request (attachment block, "it", "the attached form") and never discards the worker's version (kept beside, "(changed by worker)"). `file_facts` blocks formula errors, not short fills (`gap_notes`, evidence). `open_status` never calls a protected, Strict-OOXML or template file broken; the run-wide re-check flags only a file that broke after it opened. Repeated rows are LOUD only with an identifier or date column or a copied block. Regression net for "correct work is never refused": `agent/tests/test_correct_work_never_refused.py`.

**Independent review (2026-10-21) - intent from the model's structured output, enforced by code; the person overrules.** Who or what is in or out, which records a change removes, which stage enters records in a system and whether messages may be a source of payment details are the model's structured readings - never a regex over the person's words - and code enforces them, keeping only names and references the person's message (or the question) writes:
- **Removals** (`stage_facts.removed_records`): the classifier's `removed` / `removed_lines` / `restored` for an amendment. Deleted: `REMOVES` ("Don't remove INV-2026-001 after all" and "Exclude the freight line from PO-7781's total" were removals). A line of a record is never the record; a later "restored" clears it. An amendment with no reading removes nothing.
- **Exclusions** (`excluded_names`, `excluded_note`, `refused_for_excluded`, `_recheck_after_decision`, `_recheck_refusal`): each answer's `excluded` / `restored` / `names` / `undecided`. Deleted: `EXCLUDES`, `decided_names`, `_NAMED` (series 15 H1: "Approve Neha's airport taxi without the receipt, but pay only Rs 1,200" left Neha out, and her correct draft was refused). With no reading of what a decision names, a re-check's refusal stands.
- **Binding text** (`binding_text`): the whole question (up to 2,500 characters, never 300), the reading first ("The answer addresses: ... It does not decide: ... - nothing about them is the person's decision"); "correctly missing" only for what the reading or a removal lists. An answer that leaves listed items undecided is followed by ONE short question naming only those (`_rest_to_ask`, `_ask_rest`); the stage waits, the rest goes on; "use your judgement" to it is recorded as assumptions (`assumed_for_person`) listed under "Needs you" with the stage's Assumed lines.
- **Entry stages** (`enters_records`, `enters_into_system`, `system_entry_findings`): the planner's per-stage `enters` (the system, or null) drives the read-back rule, the judge's entry line, `covers_every` and the system-entry check; the verb test (`entry_clauses`, `APP_ENTRY`) is only a fallback for a plan without the field, and its findings are `system_entry_text` (ADVISORY). A claim is an entry word or the record's number in a reference column - never OK/Yes/Done/a tick. Record numbers of any shape (`_system_numbers`, `_id_shape`); names in any order, case or punctuation (`_name_seen`: "SHARMA/RAHUL" is Rahul Sharma). A duplicate blocks only when two list rows of one claimed record are the same in every field but their numbers; otherwise it is evidence.
- **Payment details from messages** (`payees.declare_message_details`, `message_payments`): when the planner says the request itself gives messages as the source (`payment_details_from_messages`) and no record holds an account for that payee, the payment is put to the person as an approval (`payee_approval`, ASK_PERSON) showing the account and the message; a message that would change an account the records hold is a change request, always. `known_addresses` includes the system's own pages read during the task.
- **Draft names** (`draft_checks`): a capitalised "name in no source" is evidence (`draft_fields`), never blocking ("our Engineering Lead, Priya Nair, at the Head Office").
- **A wrong blocking finding is the person's to overrule** (`without_overrides`, `_ask_about_findings`, `_keep_findings`): the SAME blocking finding back on a retry, with the worker's "Kept <item> as it is: <reason>" line, stops the retries and parks a "finding" question - the finding, the worker's reason and the model's view (`FINDING_VIEW_SYSTEM`, evidence) - while independent stages go on. Only the person's "keep it" (classify's `keep`) clears it, for that stage and that finding (ledger `overrides`; the judge is told, as binding). Never code or the judge alone; "use your judgement" clears nothing. `source_guard` is its own blocking check and goes through the same path.
- Maturity now: BLOCKING `file_facts`, `source_guard`, `removed_residue`, `removed_still_there`, `filler_values`, `draft_facts`, `app_emptied`, `files_open`, `system_entry`, `working`; ASK_PERSON `payee_approval`; everything else ADVISORY (new: `system_entry_text`, `typed_doubtful`, `repeated_rows_quiet`). `agent/tests/stage_check_harness.py` (`run_checks`) runs every stage check on a synthetic stage.

## Prompts and rule texts

(engineer 2.)

### A. long_task.py - the planner, the stage prompt, the judge's task lines

Verdicts: **KEEP** - a generic principle already; **GENERALIZE** - keep the principle, reword without the domain or the benchmark (proposed wording given); **DELETE** - a business rule or a benchmark fix that belongs in the request / the operator's taskRules, or a duplicate.

| Location | Text (short) | Verdict | Proposed / why |
|---|---|---|---|
| PLAN_SYSTEM (l.110) | "plan a person's long **back-office** request" | GENERALIZE | "plan a person's long request" - the planner is not for one kind of office. |
| PLAN_SYSTEM | "A file with several independent parts ... (a bank reconciliation, a register, a tax return, an ageing) is several pieces of work" | GENERALIZE | Keep the rule; examples domain-free: "(sheets or sections that each have their own sources and rules)". |
| PLAN_SYSTEM | "Each stage's does says exactly what to make and every rule, threshold, exception and format from the request ... Never invent a rule or a figure." | KEEP | The core principle. |
| PLAN_SYSTEM | drafts: "... ('one reminder to each customer over 30 days' after the stage that lists them); for 'a reply for every email that needs one' ... drafts 1" | GENERALIZE | "A count of drafts only when the request states it or a rule fixes it; when the request lets the worker decide which items get one, drafts 1 (the stage decides)". Drop the P1/P6 quotes. |
| PLAN_SYSTEM | "A stage that enters records in an application also writes a short CSV log ... (one row per record, with the reference)" | KEEP | Generic: evidence of what was entered. |
| PLAN_SYSTEM | "'List them for my approval' ... is NOT a decision stage" | GENERALIZE | "A list the person will review later is a deliverable, not a question: only a request that says to wait for the person makes a decision stage." |
| PLAN_SYSTEM | decision paragraph: "...which items, orders or records the decision lets through changes any allocation, totals, dispatch, invoices, replies, requisitions or entries built on them ... Allocating stock or a budget, assigning, scheduling and backorders are such work ... allocation is its own stage after it." | GENERALIZE | Keep the principle, drop the P2 vocabulary: "Every stage whose result can change with the person's answer runs after the decision stage; before it runs only work that is the same whatever the answer (reading the sources, working out what needs deciding)." (~8 lines -> 2) |
| PLAN_SYSTEM | "A rule the request states outright ... ('approve refunds up to Rs 5,000 as per the policy', 'hold orders over the limit') is applied in the stage that first records the item ... ('Approve refund (policy, up to Rs 5,000)'), never 'assess'" | GENERALIZE | "A rule the request states about what to do with an item is applied where the item is first recorded: its status says what the rule decided, never 'to assess'." No refund/limit example. |
| PLAN_SYSTEM | "Copy each rule ... (a threshold, an order of priority such as first come first served, a hold condition), and say what follows where two rules meet: something held ... no longer takes what it would have taken (stock, budget, a slot)" | GENERALIZE | "Copy each rule of the request word for word into every stage it applies to." The "where two rules meet" clause is P2's held-vs-allocation case: DELETE it - the request's own rules say that, or the model reasons it. |
| PLAN_SYSTEM | "When a rule orders or ranks records by an attribute (first come by the time each item was asked for ...) the stage that reads the sources records that attribute for every line ... (the time of the message that asked for that very line - an item added later in a chat has its own time ...)" | GENERALIZE | "When a rule orders records by an attribute, the stage that reads the sources records that attribute for every line, as the source gives it for that line." Drop the chat/time specifics. |
| PLAN_SYSTEM | "Copy each deliverable's scope words ... never narrow a deliverable to a subset ... (a held line with 0 allocated and 'on hold'; a reply telling a held customer so)" | GENERALIZE | Keep the scope rule; drop the held-line example. |
| PLAN_SYSTEM | "one file per item (an invoice per order) ... 'folder/invoice_*.pdf'" | KEEP | Neutral example of a pattern; could read "item_*.pdf". |
| PLAN_SYSTEM | must_contain / inputs / 2-9 stages / JSON shape | KEEP | Mechanical. |
| stage_prompt - "It hands in" lines | save under exactly this name; "an earlier stage made it ... add or update only this stage's sheet(s)" | KEEP | Mechanical file handling. |
| stage_prompt - drafts_as_needed line | "for each one that needs a reply - you decide which ... name each one you drafted nothing for and why: an auto-reply, a suspicious or phishing sender ..., a duplicate ..." | GENERALIZE | "You decide which need a draft; in your report name each you drafted nothing for and why." The list of reasons is P6's inbox - DELETE it (phishing is the operator's safety rule, it is in the soul already). |
| stage_prompt - every/each drafts line | "one email draft ... for EACH one the stage's rule covers ... replaces its earlier draft (office email --replaces <its id>)" | KEEP | Generic + a tool hint. |
| stage_prompt - "Work in app: X" / "No app is named ..." | machine lines for AppsFor | KEEP | Generic binding of an app to its deliverable. |
| stage_prompt - "PAYMENT TO" retry | "Fill bank accounts, IFSC/SWIFT/IBAN and UPI ids ONLY by looking them up in <master> ... leave them EMPTY for rows not paid in this run" | GENERALIZE | Only after the payee check fired: "Values of the records the request gives (accounts, identifiers) come from those records by lookup in a script, never typed; leave them empty where the row has none." |
| stage_prompt - stuck/stopped retry, "The person decided", "This decides it. Apply it ..." | | KEEP | Generic answer handling. |
| stage_prompt - decision stage answered | "a Decision column: **Released / Held / Approved / Declined** ..." | GENERALIZE | "Record the decision in this stage's file: each item it listed for the person shows what they decided, in their words." |
| stage_prompt - before_decision redo | "Redo its computation ... (allocation, totals, what goes to whom) ... something held, cancelled or left out no longer takes what it would have taken (stock, budget, a slot); that goes to the next in line by the request's own order." + `interacting_rules` | GENERALIZE | "This stage was made before the decision: redo it from the sources with the decision applied - never patch the old result." Drop the allocation sentence (P2); the request's rules are already in the prompt. |
| stage_prompt - "Apply the request's rules to every item as soon as its figures are known ... ('Approve refund Rs 2,100 (policy, up to Rs 5,000)') ... Record a reference ... only when its own content gives it ... never one carried over from another item of the same sender" | | GENERALIZE | Two principles, no P6: "Apply the request's rules to each item as soon as you have its figures and record the outcome, never 'to calculate later'. Record a reference for an item only when its own source gives it." |
| stage_prompt - working paragraph (l.966) | "For every amount you compute ... a Working sheet ... ('Payable qty') ... unit price, a tax %, a discount % ... comes from that record: open it in the system ... (working/SO-5103.csv ...) ('SO-5103 line LAN-S') - never a catalogue, price list or new-order page ... (=L2*O2*(1+P2/100)) ... File every item in the category its own content gives ... no refund row or 'refund amount'" | GENERALIZE (split) | The longest line of the prompt, five rules from four findings. Keep: "Record how you computed each figure (its inputs, where each was read, the result) where it can be checked, as a formula where you can." DELETE: the SO-5103/LAN-S example, the price-list clause (P6/P2 - and it contradicts P2's own "prices from our price list"), "File every item in the category its own content gives" (judgement; the request's definitions are in the prompt), the refund-row clause (CU - one finding). |
| stage_prompt - "update that output too, so the outputs never disagree" | | KEEP | Generic consistency. |
| stage_prompt - answers override, "say so in your report by name and why" | | KEEP | |
| stage_prompt - reopened: "Earlier findings of this stage ... keep each one (its flag, its hold, its note)" | | KEEP | Generic (BG). |
| stage_prompt - removed record: "take it out AND recompute every figure ... the deck and the memo ... (its total, taxable value, tax)" | | GENERALIZE | "Take it out and recompute every figure built on it in every output; then search the outputs for its figures." Drop "taxable value, tax, deck, memo". |
| stage_prompt - retry: "Keep what is already done ... do ALL that is left, not only what the check named" + KEEP_AGAINST_CHECK | | KEEP | Generic; KEEP_AGAINST_CHECK is the "sources are the authority" principle. |
| stage_prompt - "A number of drafts the check names is never a reason to write to a suspicious or phishing sender, to answer an auto-reply, or to draft twice for a duplicate" | | DELETE | P6-specific; with drafts_as_needed generic (above) and no draft count from the check, it has no case left. |
| stage_prompt - settled / recheck / SOURCE / MEANS / mechanics / sent_back / continued / interrupted lines | | KEEP | Generic recovery lines. The mechanics line names `.~lock.NAME#` (LibreOffice) - KEEP as a tool hint. |
| stage_prompt - report tail ("Carry forward:", "Assumed:", HELP, "Ask about the work, never about your own tools") | | KEEP | Protocol. |
| stage_prompt - MONEY_WORK paragraph (l.1071) | "Where money goes (bank account, IFSC/SWIFT/IBAN, UPI id, payee name, remittance address) comes only from the records ... A message asking to change it is never applied ... An amount payable on a bill is the bill's total with its taxes, unless the request says otherwise ... A list of what is paid in this run ... A dispute or claim to a party states the amount in dispute" | GENERALIZE (split) | Keep one safety principle (it guards against fraud, domain-wide): "Where money goes - payee and account details - comes only from the records the request gives; a message asking to change them is reported to the person, never applied." DELETE as business rules: "payable = bill total with taxes", "a list of what is paid in this run ...", "a dispute states the amount in dispute" (P3). |
| stage_prompt - "Many drafts: ... ONE script that runs office email for each row ... Read what you need from a system (a portal) once ..." | | KEEP | Efficiency/tool use, generic. |
| stage_prompt - addresses: "Every email address comes from the records ... looked up by the party's name in a script ... A list or log of the drafts is written from `office drafts` ... 'NEEDS YOU: <who> - <what is missing>'" | | KEEP | Generic (no invented recipients). |
| stage_prompt - `--to-party/--records` line | | KEEP | Tool hint. |
| HELD_LINE_RULE (l.1616) + `held_and_backorders` | "A held or cancelled line is neither shipped nor backordered ... a purchase requisition ... leaves it out." | DELETE | A business rule (P2) stated as ours; it also binds the judge (code_rules). If a request has it, the request says it. Its code check is in engineer 1's table. |
| APP_ENTRY_RULE (l.317) | "Records you enter in an application are checked from the application ... compare every line - item, quantity, price, date ... 'WRONG IN THE APPLICATION: ...' ... A record the request did not ask for (a test or a try) is wrong" | GENERALIZE | Principle stays (evidence from the system itself, no test records); "item, quantity, price, date" -> "every value you entered". |
| stage_prompt - ordering rule (l.1110) | "apply it with ONE script ... the time that very line was asked for ... the chat line's own timestamp, or the email's Date header - in one time zone (India time, +05:30) ... an item added to yesterday's order today was asked for today" | GENERALIZE | "When a rule of the request orders the items, apply it in one script over a table that records, per item, the attribute it orders by as its source gives it; show that attribute and the order served." DELETE the chat/email/IST specifics (P2). |
| stage_prompt - "Read each document with `office read FILE` ... never a hand-written zip/XML script" | | KEEP | Tool use. |
| stage_prompt - "Before totalling an input table ... 'Data check' ... Never total repeated rows silently" | | KEEP | Tool hint + generic data hygiene. |
| stage_prompt - systems: "Systems the request gives ... When a record must be found ... find it by what the message does give - the sender's company or address, the product, the date ... never leave a row 'to be looked up'" | | GENERALIZE | Keep "the request's systems and inputs are usable sources"; the lookup sentence -> "find a record by what its source gives; ask only when several or none match". |
| stage_prompt - "The request's own input files ... (look in them before asking the person anything)" | | KEEP | |
| _judge - "THE STAGE TO CHECK ... (One stage of a longer request; the rest is done in other stages and is not checked here.)" | | KEEP | |
| _judge - decision answered binding line; binding_text ("override the source files ... a phrase like 'everyone whose papers are complete' means everyone the person has not excluded") | | GENERALIZE | Keep binding; drop the P4 quote: "a scope phrase of the request covers what the person has not left out". |
| _judge - kept-answer (CU) binding | | KEEP | Generic: the source decides. |
| _judge - code_rules binding ("Decided in code ... HELD_LINE_RULE") | | DELETE (with HELD_LINE_RULE) | No business rule in our code; a mechanical code fact needs no binding line (it already passed). |
| _judge - app line ("This stage's own output (x) is to be made in App" / "names no app") | | KEEP | |
| _judge - system-entry line (CR) | "A record counts as entered only where the system's own pages ... show it" | KEEP | Generic evidence rule. |
| _judge - "Look for rows that contradict themselves (a category or status their own action or notes deny) and for amounts whose working is missing or does not give them: either is not done." | | GENERALIZE | Its first half pushes the judge towards CU-like refusals; its second makes "no working" a failure, a rule the request never states. -> "Check the result against its sources; a working sheet is evidence, not a requirement." |
| _judge - APP_ENTRY judge line ("compared line by line ... Every record the stage asks for must be there") | | KEEP | Duplicate of the system-entry line above - merge (see bloat). |
| excluded_note / refused_for_excluded | "X was left out by the person's own answer ... do not count them as missing" | KEEP | Generic. |
| RESCOPE_SYSTEM (l.2524) | "for example '... - except Priya Nair (you said: skip her until her ID arrives)'" ... "'hold Harbour until they clear their dues' says Harbour is held, nothing about its quantities" | GENERALIZE | Same rules, neutral examples: "(for example '... - except <name> (you said: <their words>)')"; drop the Harbour sentence (the rule "never add a handling rule" stands on its own). |
| OWN_OR_PERSON_SYSTEM | PERSON/OWN definitions | KEEP | Generic. |
| SETTLE_SYSTEM | | KEEP | |
| CROSS_SYSTEM | "('value': '02/11/2026', 'Rs 480,000')" | GENERALIZE | Neutral examples ("'12/03/2026', '480,000'"); the rupee example is P4's. |
| CLASSIFY_SYSTEM | "(a decision about an account or an order, when the question asks where a value comes from)", "('your answer about the order quantities')" | GENERALIZE | Mild: neutral nouns ("a record", "your answer about the quantities") - the rules are generic. |
| KEEP_AGAINST_CHECK (CU) | "The check is not the authority - the sources are ..." | KEEP | The principle this audit is for. |
| _send_back_unchecked (DD) text; _send_back_false_figures; found_in_inputs; settled_by; own-tools texts | | KEEP | Generic "check it yourself / re-read your source / the request says" send-backs. |
| delegated_answers / stuck answer texts | "...the request keeps this for your approval: I keep it pending (listed for you, nothing promised)" | KEEP | Generic reserved-decision handling. |
| claims_done note to the judge (DB) | "The worker's report says this stage is DONE and names no obstacle: answer done, not_done or unsure" | KEEP | |

### B. verify.JUDGE_SYSTEM (the final judge)

Its rules are generic; the problem is fifteen "found live" stories inside it, many from one domain, which teach the model the benchmarks' facts as if they were rules.

| Part | Verdict | Proposed / why |
|---|---|---|
| Opening (done only when proven for THIS request; evidence = screen now + what tools observed; info answers must match what was read) | KEEP | |
| "a Word letter reported saved was an empty file" | KEEP | Neutral illustration. |
| demonstration details are examples ("Udupi, Karnataka") | GENERALIZE | Keep the rule; drop the place name. |
| "a figure the report or files give contradicts what the request asks ... resized to 50% ... 1200 x 600" | KEEP | Neutral. |
| named-app paragraph (format vs option vs app; tools-then-app is done in that app) | KEEP | Generic, with the format/option distinction. |
| "Go through the request part by part ... ANOTHER value ... is not_done" | KEEP | |
| "A report that says a value ... was left out ... because the application refused it ('it rejected the GSTIN ... Unregistered')" | GENERALIZE | Keep; example -> "('it rejected the tax number, so I saved without it')". |
| "A record made from a document must carry that document's own reference ... (an order without the customer's order number, a receipt without the challan number)" | GENERALIZE | "...must carry the document's own reference where the record has a field for it." Drop challan/PO. |
| "A period the document gives (valid for 30 days ...) ... a quote asked to be valid 30 days was saved valid for 31" | KEEP | Generic date arithmetic. |
| "A result must be where the request said to put it ... (C:\\...)" | KEEP | |
| "Read what the report itself says was left undone ... (found live: a sales order passed without the customer's own order number ...)" | GENERALIZE | Drop the sales-order story; the rule is generic. |
| "the values entered must be the document's ... a thing the document describes is entered as the application's own record ... ('Executive A4 Printing Paper 75 GSM (Box of 10 Reams)' is the record 'A4 Printing Paper')" | KEEP | Generic matching rule with a neutral example. |
| date format paragraph (yyyy-dd-mm) | KEEP | |
| "A rule about HOW to work ... is a step of the method, not part of the result ... (found live: a correct draft ... duplicate check)" | KEEP | Generic; the story can go. |
| "Answer cannot when the worker's own report does not claim the task was done ..." | KEEP | Generic. |
| files paragraph: "Work out yourself every total ... recompute at least two rows ... (a depreciation schedule ...) ... A figure the result presents as a source's own (a balance ... 'per' the bank statement ...) ... (-14,140.00 against 398,360.00) ... When rules sort the source's rows into places (kept/rejected, matched/unmatched, OK/exception, duplicate - keep the first) ... (a copy shown as its own row marked as the duplicate ... counted once) ... Every rule applies to every entry it covers ... (a meal approved at 60.50 over a 60.00 cap, after its wine was deducted) ... 'every credit on the statement is a customer receipt' ... A record the worker made during this task is the task's result ... &amp; ... '2 Units'" | GENERALIZE | One paragraph of ~30 lines with eight domain stories (depreciation, bank reconciliation, expenses, receipts, duplicates). Keep the principles as five short sentences: recompute from the sources by the request's rules; a figure attributed to a source equals that source; every source row ends where the rules put it, once; every rule applies to every entry it covers; a record the worker made is this task's result. DELETE the stories. |
| classification paragraph (CU): "must quote the source's own words ... The worker's working notes are not a source ... (found live: an email asking for an invoice to be corrected ... filed as a complaint ...)" | KEEP (trim) | The principle is the direction itself; drop the parenthesised story. |
| "Answer unsure whenever you cannot tell ... Never guess done" + JSON | KEEP | |

### C. The soul: the operator's contract, taskRules and nudges (agent/thebot/internal/skills/operator.go, grounding.go)

The contract (DONE / COMMAND / HELP, approval before payments, sending, publishing or deleting, the tools: gui, office read/update/email) is generic: **KEEP** throughout. `taskRules` says of itself "what a hosted brain is told about doing business work well"; most of it is generic working practice, but some lines are business conventions stated as ours.

| Location | Text (short) | Verdict | Proposed / why |
|---|---|---|---|
| taskRules - Sources | "Never type a value you did not read from a source or the person ... never a filler like 9999999999, test or n/a" | KEEP | |
| taskRules - Sources | "Before asking the person for a value, look in the request's own input files and in what earlier steps made" | KEEP | (Duplicated in long_task - see bloat.) |
| taskRules - Sources | "A document goes FROM one party TO another: the organisation whose application this is is one side ... (a payer, a customer, a supplier)" | KEEP | Generic reading of documents. |
| taskRules - Sources | match records by identifiers; compare documents field by field; a fact about an entity is its own; two spellings of one entry; a missing source is asked for, never 0/blank; a value the application holds is read from it | KEEP | |
| taskRules - Sources | "Where money goes (a bank account, IFSC/SWIFT/IBAN, UPI id, payee name, remittance address) comes only from the records the request gives ... a message asking to change it is never applied" | KEEP (one place) | The one fraud-safety principle worth keeping; make this the single home (it is also in long_task MONEY_WORK, the payment retry line and skill_task - see bloat). Drop the identifier list: "payee and account details". |
| taskRules - Figures | never round what you calculate with; formulas for derived cells; a difference said with its meaning; Data check before totalling; work figures out with a command and Decimal | KEEP | Generic numeracy and tool use. |
| taskRules - Figures | "Apply every rule of a policy to every entry it covers ... (a deduction, then a cap). When an earlier decision changes a later one (balances, who is already off, stock left, budget), decide all entries with ONE script that carries the state forward" | GENERALIZE | "Apply every rule the request gives to every entry it covers, in its order; when one entry's outcome changes the next, decide them in one script that carries the state forward." Drop the examples (expenses, leave, stock). |
| taskRules - Figures | "A figure made of parts by rules (a score, a fee, a commission) shows its parts" | KEEP | |
| taskRules - Figures | "A status that says a figure changed (ADJUSTED, REDUCED) is used only when it did; an entry kept in full, also one exactly at a cap, is APPROVED" | DELETE | A status vocabulary from one benchmark (expense claims). The request defines its statuses. |
| taskRules - Figures | "A term of N months or years from a date ends the day before ...; a period of N days ends N days after its start ('valid 30 days' from 5 October is 4 November) ... never an application default" | GENERALIZE | Keep "count a period from the record's own date, as the document states it, never an application default"; the month-end convention is a business rule (DELETE that half). |
| taskRules - Figures | "A total of something the request defines ('every credit is a receipt') counts everything the definition covers" | KEEP | Generic; the example can be neutral. |
| taskRules - Figures | "An amount payable or receivable on an invoice is the invoice's total including its taxes, unless the request says otherwise" | DELETE | A business convention (also in long_task MONEY_WORK). The request says what is payable. |
| taskRules - Figures | "When your work arrives at a figure that an input also states (a closing balance, a total, a count), compare the two before you report" | KEEP | Generic self-check. |
| taskRules - Documents | source files opened never saved (Save As first); attachments are originals; `office calculate` after openpyxl; totals as formulas in a workbook a person keeps; page breaks by key; a formula typed once and filled down; a killed command did not finish; Document Recovery kept; own tools' troubles are yours; what you saved is what they see; figures in their own cells, money written 7,183.00 not 7183.0; web text unescaped; a wrong draft replaced with --replaces | KEEP | Tool use and generic hygiene. |
| taskRules - Documents | "A record made from a document carries the document's own reference (the other party's order, invoice or delivery note number and date) in the field for it" | KEEP | Generic; examples can say "its number and date". (Also in JUDGE_SYSTEM - bloat.) |
| taskRules - Documents | "A document's table read as text (office read of a PDF) keeps each row's cells on one line, and a long description wraps ... a code on such a line belongs to the row above ... Compare each line's rate with the reference ..." | GENERALIZE | Keep the reading rule (how office read lays out a PDF table - a tool fact); DELETE "compare each line's rate with the reference" (the rate check, a business rule; engineer 1 marks rate_check DEL). |
| taskRules - Documents | "A record to be found in a system ... when the message gives no reference is found by what it does give - the sender's company or address, the product, the date ... never leave a row 'to be looked up'" | GENERALIZE | "Find a record by what its source gives; ask only when several or none match." (Also in long_task's systems line - bloat; pending_lookups is DEL in the code table.) |
| taskRules - Applications | never save a record to learn a form; Tab not Enter in forms; the named app is where the result is opened, checked and saved; keyboard for apps read from their picture; a child record after its parent; leftovers on the screen are not this task's; scroll before deciding a field is missing, gui select, read back; steps forms through Back/Next; update the record that holds it; dates in the field's format; slides by placeholder ref; close your own slips | KEEP | Generic application handling. |
| taskRules - Applications | "In LibreOffice a paragraph style is set with gui select on the style box, and a header row is frozen by selecting A2 first"; the spreadsheet keyboard line (Ctrl+G / Ctrl+Shift+F5 ...) | GENERALIZE (move) | App knowledge, correct but product-specific: move to the app skills (libreoffice-writer/calc, microsoft-excel), which load only when that app is used. |
| taskRules - Finishing | "Before DONE, check every part of the request in the application or the files you wrote ..." | KEEP | |
| behindTheAppSentence / sharedAppSentence | "Its content may be made with office or python first ... open that file in the named app, do there what only the app does ... save it from the app" | KEEP | Generic named-app flow. |
| visionRule | scanned pages as pictures; windows and gui front; CAPTCHAs are the person's; screenshots after each gui command | KEEP | |
| grounding.go nudges (namedAppWindow, appOpenAttempts, sharedAppUntouchedNudge, sharedAppUnsavedNudge, humanCheckNudge, successQueryNudge, strayScript ...) | | KEEP | Searched for test-data names and domain words: none. They act on the named-app binding and the screen, not on a business. |
| atomizer (triage, WorkApps, plan prompt) | | KEEP | Searched: the only domain-looking text is a regex of header words (`product|item|supplier|vendor|customer...`) used to tell a data request from a coding one - mechanical. |

### C2. The soul, line by line (operator.go, grounding.go, shell.go, memory/l1.go)

A full read of every model-facing Go string (not only a search). No test-data names (Harbour, Priya, NS-0457, WA-1810, SO-51xx, MUG-12 ...) and no GST, refund, credit-limit, backorder or lakh text reach the model from the soul; they are only in tests and comments. Beyond C above:

| Location | Text (short) | Verdict | Proposed / why |
|---|---|---|---|
| operator.go:90 (contract) | "Drafting an email with office email is not sending it ... do that without asking" | DELETE | Said again at :95. |
| operator.go:92 | "gui guide NAME ... (Excel, Calc, TallyPrime, Word, PowerPoint, Impress, GIMP ...)" | GENERALIZE | "gui guide NAME prints the guide for an app this desktop has one for (gui guide lists them)" - the list goes stale and names one benchmark app. |
| operator.go:92 | "'replace every X' is done when no X is left ... Find and Replace with its replace-all option" | GENERALIZE | "A change asked for everywhere is done when reading back shows none left; use the app's bulk operation, not one edit at a time." |
| operator.go:94 | office API reference (~3k chars) with examples "Exceptions"/"Dept"/"IT", "Invoices"/"INV-1", "(an invoice, a quote, a letter, a payslip)", "keep a Total row last" | GENERALIZE (bloat) | Neutral example names; keep one line per verb in the contract and put the full reference behind `office help` (re-read every turn today). |
| taskRules:142 | "... (a phone's digits without the country code)" | GENERALIZE | Drop the phone aside (P4). |
| taskRules:144 | "A document goes FROM one party TO another ... (a payer, a customer, a supplier)" | GENERALIZE | (C said KEEP.) Fine principle; the AR/AP framing can go: "the counterparty is the other party the document names, never your own organisation." |
| taskRules:145 | "the item 'T-shirt' for 'Cotton T-shirts, round neck'" | GENERALIZE | Neutral example. |
| taskRules:147 | "A fact looked up about an entity is that entity's own (its founding, not a predecessor's ...) ... the infobox of its own article" | GENERALIZE | One research task's story; keep "a fact about the exact entity asked, from its own record". |
| taskRules:155 | "0.4% below target (or short of it), 2% above (or over)" | GENERALIZE | Example figures from one report. |
| taskRules:157 vs :154 | "Money uses decimal.Decimal ... each amount rounded to the cent first" vs "Never round a figure you go on to calculate with" | GENERALIZE | The two conflict: "Decimal from the source text, at the source's own precision". |
| taskRules:159 | "(a score, a fee, a commission) ... '8 + 15 + 2 + 3 = 28'" | GENERALIZE | Keep "show the parts of a rule-derived figure, each traced to its source". |
| taskRules:161 | term of N months / days | (C: GENERALIZE) | The full read keeps the date-arithmetic part as generic convention; agree to keep the principle, trim the worked examples. |
| taskRules:162 | "('every credit is a receipt')" | GENERALIZE | Drop the banking example. |
| taskRules:168 | "To change a workbook use office update ... add_rows with 'before' keeps a Total row last ... A PDF is made with ..." | DELETE (dup) | Duplicates the office reference at :94; keep only "attached files are originals: save results under a new name". |
| taskRules:171 | page break by ctrl+Return | GENERALIZE (move) | To the word-processor guides. |
| taskRules:172 | one formula filled down via Name Box + Ctrl+D | GENERALIZE (move) | Keep "fill a formula down in one action and read back the last row"; key sequence to the spreadsheet guides. |
| taskRules:173 | "A command that was killed (exit 124) did not finish" | DELETE (dup) | shell.go appends the same warning to every killed command. |
| taskRules:179 | "An email you drafted that turns out wrong is replaced with --replaces" | DELETE (dup) | Contract :95 says it. |
| taskRules:180 | document's own reference "(the other party's order, invoice or delivery note number and date)" | GENERALIZE | "its own reference number and date". |
| taskRules:187 | the named-app paragraph (~2.5k chars: Host ribbon "Sort Largest to Smallest", Windows Open/Save As steps, `gui type CELL "Item\tQty\nTea\t4"`) | GENERALIZE (split) | Keep the principle (behindTheAppSentence); move the app mechanics to the app guides; the Sort example is one benchmark task. |
| taskRules:188 | "an app whose controls are read from its picture ... ('A: Accept') ... (often Ctrl+A ...) ... asks to quit: answer No" | GENERALIZE | Keep the keyboard principle; the TallyPrime specifics to its guide. |
| taskRules:192 | "A form in steps (1. Personal, 2. Job, 3. Review)" | GENERALIZE | Neutral step names (onboarding portal). |
| taskRules:194-196 | LibreOffice paragraph style / freeze row; spreadsheet keys; slide placeholders | GENERALIZE (move) | To the Writer/Calc/Excel/Impress/PowerPoint guides (C said the same). |
| taskRules:197 vs :175 | "Close a dialog you opened by mistake ... never handed to the person" vs "own tools' troubles are yours" | GENERALIZE (merge) | One rule. |
| grounding.go:630-632 (quotes nudge) | "Prices!$A$2:$C$13 ... =VLOOKUP(B2,Prices!$A$2:$C$13,3,FALSE)" | GENERALIZE | Neutral formula (the "Prices" sheet is one benchmark's). |
| grounding.go:723-726 (filler nudge) | "(a phone: digits only, without the country code)" | GENERALIZE | Drop the phone aside. |
| grounding.go:869-878 `sharedRule` | the named-app procedure and the source-file rule, again | GENERALIZE (dedupe) | When `rulesFor` has swapped in sharedAppSentence, sharedRule only needs the path mapping; a shared-folder desktop reads both rules twice today. |
| memory/l1.go:101 | "e.g. 'The user's name is Alex Doe.'" | GENERALIZE | A real person's name as the example: "The user's name is Alex Doe." |
| shell.go:116-118 | the killed-command note | KEEP | The single home for that rule. |
| scheduler (recovery, compose), prompt/frame.go, skills general/filewrite/web/ragquery/terminal, atomizer plan prompts, coding prompts | | KEEP | Generic; nothing domain-fitted. |

Bloat added from this read (to E): the office API reference in the contract and again in taskRules:168; the draft-replace rule (contract :95, taskRules :179, stage prompt); the killed-command rule (taskRules :173, shell.go); the named-app procedure and the source-file rule (behindTheApp/sharedAppSentence, taskRules :167, sharedRule); "own slips / own troubles are yours" (taskRules :175, :197, the stage prompt's mechanics line, OWN_OR_PERSON); "look everywhere before asking" (taskRules :143, :149, the stage prompt, the found-in-inputs send-back).

### D. Agent texts outside long_task: guidance, app skills, harness prompts, tool and refusal texts

| Location | Text (short) | Verdict | Proposed / why |
|---|---|---|---|
| guidance.py | operator guidance on recordings and taught skills | KEEP | Searched for test-data names and business words: none in the text the model reads (one "Globex" story is a code comment). |
| thebot_harness.py, azure_harness.py, harness.py | harness prompts (shell, tools, work folder) | KEEP | No business rules found. |
| app_skills/*/SKILL.md (gimp, libreoffice-calc/impress/writer, microsoft-excel/powerpoint/word) | how each app is worked here (dialogs, keys, saving) | KEEP | App knowledge, loaded only for that app: the right home for app-specific hints (see C, "move"). |
| app_skills/tallyprime/SKILL.md | how TallyPrime is worked (keyboard only, Educational Mode, licence box) | KEEP | App knowledge. |
| app_skills/tallyprime/SKILL.md "Create a ledger - exactly these steps"; "Record a purchase - ... F9 for Purchase ... Accounting Invoice ... Supplier Invoice No. ... Party A/c ... Narration" | | GENERALIZE | Workflow recipes for two accounting tasks. Keep the app facts they rely on (Alt+G "Go To", Ctrl+A accepts, Ctrl+H change mode, how a list field is chosen, "Name already exists"); the business recipes belong to a taught skill or the request. |
| skill_task.py (final gate payee "why") | "Bank accounts, IFSC/SWIFT/IBAN and UPI ids come only from the records the request gives ..." | KEEP (dedupe) | Same rule as taskRules - one wording, shared (bloat). |
| computer/core.py refusal and note texts (unsafe save, number arrows, taskbar, shared-folder save note, form check ...) | | KEEP | Mechanical facts about the screen and the person's files. |
| tools/office.py help and error texts | | KEEP | Tool usage. |
| produced.py held_backorders finding ("a held line is neither shipped nor backordered ... anything bought for the backorders must leave held lines out") | | DELETE | The P2 business rule as a finding; engineer 1's table marks held_backorders DEL - consistent with HELD_LINE_RULE (A). |
| Finding sentences of the other code checks (stage_facts, payees, rate_check) | | see "Code checks" | Their wording goes with each check's verdict in engineer 1's table; the sentences that state business rules ("must come from SO-5103's own lines", "Serve each item's lines earliest first") go when their checks go. |

### E. Prompt bloat - the same rule said in several places

| Rule | Said in | Proposed single home |
|---|---|---|
| Where money goes comes only from the records; a change request is reported, never applied | taskRules (Sources); long_task MONEY_WORK paragraph; long_task payment-retry IFSC line; code_problems payee finding; skill_task gate "why" | taskRules (one line). The code fact (payee mismatch) keeps its own one-line finding. |
| An invoice's payable amount includes its taxes | taskRules (Figures); long_task MONEY_WORK | Neither (DELETE - business convention). |
| Never total repeated rows silently; read the Data check line | taskRules (Figures); stage_prompt (input tables line); repeated_rows finding | taskRules; the stage-prompt line goes. |
| Look in the inputs / earlier outputs before asking the person | taskRules (Sources); stage_prompt ("The request's own input files ... look in them before asking"); stage_prompt report tail; found_in_inputs send-back | taskRules + the send-back (it acts); the two stage-prompt lines go. |
| A record without a reference is found in the system by what the message gives; never "to be looked up" | taskRules (Documents); stage_prompt systems line; pending_lookups finding | taskRules (generalized); the stage-prompt line keeps only "the request's systems are sources". |
| Records entered in an application are checked from the application, not from a log | APP_ENTRY_RULE (stage prompt); _judge APP_ENTRY line; _judge system-entry line; system_entry check | APP_ENTRY_RULE for the worker; one judge line (the system-entry line, which carries the evidence). |
| Your own tools' troubles are yours to recover from | taskRules (Documents); stage_prompt report tail ("never about your own tools"); mechanics retry line; OWN_OR_PERSON_SYSTEM | taskRules + the retry line when it fires; the report-tail sentence goes. |
| A record carries its document's own reference | taskRules (Documents); JUDGE_SYSTEM | Both (worker rule + judge rule) - keep, but one wording. |
| Record how each figure was computed; formulas; never round | taskRules (Figures); stage_prompt working paragraph; stage_prompt "apply the request's rules as soon as figures are known" | taskRules; the stage-prompt working paragraph shrinks to its generic sentence (see A). |
| Order by an attribute per line; one script carrying state forward | PLAN_SYSTEM (ordering rule); stage_prompt ordering line; taskRules (one script carries the state) | PLAN_SYSTEM (record the attribute per line) + taskRules (one script); the stage-prompt line goes. |
| A wrong draft is replaced with --replaces, never a second | taskRules; stage_prompt every/each drafts line | taskRules; keep the flag in the drafts line only as a tool hint. |
| Removed record: take it out and recompute everything built on it | stage_prompt reopened line ("Removing X: ..."); removed_residue / removed_still_there findings | stage_prompt line (generalized); the findings keep their own sentence. |
| Update an earlier output when this stage changes what it records | stage_prompt ("update that output too"); PLAN_SYSTEM decision paragraph (in part) | stage_prompt. |
| Decision stages: ask only the person, never by email | PLAN_SYSTEM decision paragraph; stage_prompt decision lines; decision_problem finding | PLAN_SYSTEM + the stage line; the finding stays as the code fact. |

### Consistency with the code-checks table

Where engineer 1 marks a check DEL (move to the request or taskRules), the prompt line that teaches the same business rule goes with it here: HELD_LINE_RULE and its judge binding (A), the held-backorders finding (D), the rate comparison in taskRules (C), the allocation/ordering lines (A: PLAN_SYSTEM, stage_prompt ordering line), the record-price clause of the working paragraph (A), the "to be looked up" wording (A, C), and the draft-coverage reasons list (A). Question routing becoming one model call with evidence (engineer 1) matches keeping OWN_OR_PERSON/SETTLE as the prompts that call uses.
