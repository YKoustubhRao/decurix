# Product manager acceptance review

Reviewed 14 October 2026. This review evaluates a generic computer-workflow foundation and demonstrated-task transfer. It does **not** establish that the product can perform every computer task.

## Independent acceptance result

`PYTHONPATH=agent python -m unittest discover -s agent/tests -p test_workflow_acceptance.py`

**26 tests passed.** Tests use independent callbacks, temporary files and deliberately invalid Office packages; they do not open Excel, PowerPoint, Word, a browser or a mail application.

The production chat-dispatch integration suite initially passed **6 tests** using a fake harness, covering stage sequencing, one aggregate final report, evidence-gated completion, resume, engine cleanup, stop and continuation after a paused query. The final full agent regression run completed **1,284 tests: 1,275 passed and 9 skipped**, in **173.050 seconds**, after the replay and native-label fixes. Focused checks also passed, including **72 checks** for the resumed-parameter correction. The offline soak replaces Office outputs with simulated JSON receipts; those results establish orchestration resilience rather than Office application correctness.

Coverage:

- An unseen sixth workflow and unknown application label execute without catalog-specific branches. Checked output paths, hashes and original inputs reach downstream stages.
- A 100-stage workflow crosses five application labels and resumes through ten runner reconstructions without repeating completed stages. This is a persistence stress test, not evidence of 100 actual app operations or a multi-hour live run.
- Missing required inputs, missing checks, duplicate IDs, invalid dependencies, cycles and escaping artifact paths are rejected.
- Run/step-scoped approval tokens, cancellation, bounded safe retries, non-retry of failed external actions and uncertain-state handling after an interrupted non-idempotent stage are exercised.
- Self-reported success, absent output, wrong output content, a rejecting business verifier, changed checked files and corrupted checkpoints cannot produce normal completion.
- Separate runs have isolated artifact folders; an existing run ID cannot silently replace a business case.
- Malformed/empty PowerPoint files, markers hidden in metadata or Word comments, unreferenced spreadsheet shared strings and a fake PDF magic header cannot substitute for actual artifact content.

The adversarial artifact cases initially exposed false completion. Engineering changed validation to parse Office XML, inspect document/slide/cell content, resolve referenced shared strings and invoke a PDF extractor. The required-input case also caused an implementation correction.

## Workflow breadth

The catalog now loads editable workflow specifications as data rather than embedding task-specific execution branches. Custom JSON definitions accept other applications and stage sequences.

The separate learning-transfer benchmark defines **25 distinct task families**: reconciliation, deduplication, grouped sales summaries, budget variance, message prioritization, personalized letters, calendar availability, document templates, redaction, ordered page bundles, clause extraction, slide outlines, chart percentages, CRM identity lookup, onboarding completeness, ticket SLA escalation, quote comparison, rename manifests, archive inventories, CSV conversion, database selection, inventory reorder, terminology review, payroll controls and FAQ drafting. These use more than fifteen different sequences of open, click, type, select and keyboard actions. Their independently specified holdout answers differ from recorded examples.

`python bench/workflows/transfer.py --output bench/workflows/results/transfer.json` passed **150 evaluations**: one persisted demonstration and one ready skill per family, each evaluated on six variants. The benchmark uses the production recording store, learning questions, skill persistence and `skill_engine.run`; only the application observation/action adapter and model-based fit check are replaced. Variants cover unseen inputs, moved controls, renamed controls, ambiguous controls, missing parameters and deliberately incorrect business results. Independent business assertions reject wrong answers even when the scripted UI reports completion. All **6 learning-transfer acceptance tests** passed in **22.139 seconds**. (Correction, 2026-11-02: the "incorrect business result" variant passed whatever the worker did - its scripted app always answered wrongly and the row passed on the bench's own check of that answer - so 25 of the 150 measured nothing. It was removed; the suite is now 125 evaluations on five variants.)

The rename variants exposed a production defect: a control renamed from `Source data` to `Source data:` was safely healed for the action, but its value postcondition retained the obsolete label and falsely stopped the run. Engineering now applies an ephemeral same-step alias after deterministic healing, preserving the expected value and original recording. All twenty-five rename holdouts pass after the fix.

These are **scripted-application tests**, not recordings captured from real mouse/keyboard input or live Office automation. They validate the existing demonstration format, learning and transfer pipeline without claiming actual application operation. The JSON report explicitly records `live_apps_tested: false`.

## Live browser qualification, checked for consistency

After provisioning isolated desktops, the actual Chromium fixture run passed **25 task families x 3 variants = 75 live evaluations**, taking **660.456 seconds**. Each family was recorded once through actual `core.open_target` / `core.act` browser operations, learned, saved and reloaded before executing unseen-input, moved-layout and renamed-control variants. All seventy-five returned `verified`, passed a fresh UI check and produced the separately specified expected business answer from the exact holdout input.

`python bench/workflows/audit_evidence.py` checked for consistency: the runner's exported report against the scenario table and the files on disk (recordings, skills, PNG headers). The pass/fail results are the runner's own and are not re-derived. It found **25 original recordings**, **25 matching ready skills**, **107 recording frames** and **75 result screenshots**. Every skill's source matches its reported teacher recording; each teacher contains exactly three completed agent actions (open/type/click), and each saved skill retains its original control label rather than baking in the healed label. Selected moved/renamed screenshots were visually inspected and show the expected input/result and actual position/label changes. The audit result is saved in `results/pm-browser-audit.json`.

This is real Chromium/CDP execution against **local deterministic fixtures sharing one three-action browser form**, with twenty-five different transformations. It is not twenty-five unrelated enterprise applications, human mouse/keyboard demonstration capture or native Microsoft Office qualification. The actual audit metadata identifies the demonstrator as `agent`. Full-run artifacts are coherent; earlier smoke reports used shared skill/screenshot names, which later runs overwrote. Future runs now use teacher-specific artifact names to preserve snapshots.

Live/API review also exposed two additional issues. Explicit workflow cancel/status commands at a paused question were being treated as model answers; they now take the deterministic control path before approval or lesson handling. A resumed replay could drop values typed in earlier steps, causing a correct UI result to fail a later parameterized check; explicit earlier parameters are now retained, with focused regression coverage. Initial native-office failures showed a real LibreOffice recovery modal caused by the benchmark's force-close reset; clearing only the isolated fixture recovery state removed that setup obstruction. That fixture correction does not establish automatic recovery from real production crashes.

## Full teaching and correction through the worker API

The real API end-to-end test also passed from **one audited demonstration** through teaching questions, a saved ready skill and three subsequent runs. It entered `Initech`, healed `Save` to `Save changes` for `Umbrella`, then safely stopped when the button became the unclear `Go`. An explicit correction resumed **only step 3**, producing `Saved Wayne` and persisting the correction. The final skill is version **3**, has **3 successful runs** and targets `Go`; the original three-action teacher recording still targets `Save` and types `Acme`.

The PM independently checked the exported source/skill lineage, recorded original actions, three run recordings, exactly one `Wayne` typing action, the completed `Go` click and recorded `Saved Wayne` postcondition. The passing audit is saved in `results/pm-api-audit.json` and is reproducible with `python bench/workflows/audit_api.py`; the full question/answer and UI verification log is `results/skill-api-verified.log`. Capture remains agent-driven, and this local form does not establish human input capture or an enterprise CRM integration.

## Native applications and scripted GUI qualification

**Seven native recorded-task transfers passed** across LibreOffice Calc, Writer, Impress and Thunar: cell values, a recalculated formula, sheet renaming, a document table, document replacement, presentation replacement and file renaming. Six document variants explicitly rebound the recorded source file to an unseen source. The PM independently reopened the exported ODS/ODT/PPTX packages and checked original teacher values alongside the distinct holdout values, including the saved formula and cached result **120**. Source-recording/ready-skill lineage, result screenshots and hashes of prior failed attempt reports also match. Selected Calc, Writer and Impress screenshots visibly show the expected final values and preserved source content.

These runs returned **`completed_unverified`** because some GUI steps leave no directly checkable screen condition. They were accepted only because separate saved-artifact checks succeeded; this distinction is preserved in the reports. `native-consolidated.json` retains the failed attempt history rather than replacing it with an uninterrupted success narrative. Actual native failures led to a generic accessibility correction: an unnamed editable field can acquire its single visible label only within a bounded container containing exactly one field and one label; ambiguous or hidden candidates remain unnamed. An overlength generated benchmark skill name was also corrected without weakening the product's name validation.

The separate **20-task GUI oracle suite passed all 20 tasks**, covering browser administration, Calc, Writer, Mousepad and Thunar, including approval-gate cases. The PM checked every per-task business verdict instead of trusting the process exit code. The benchmark initially could return a successful process status despite failed rows; its exit semantics were corrected and **two integration tests** now distinguish all-pass from mixed pass/fail results. These oracle results establish scripted GUI solvability and outcome checking, **not model autonomy**.

The passing independent native/GUI audit is `results/pm-native-gui-audit.json`, reproducible with `python bench/workflows/audit_native.py`. Native Office evidence here uses LibreOffice and compatible files, not Microsoft Excel/Word/PowerPoint installations.

## Nine-stage native chain with a resumed worker

The learned Calc, Writer and Impress procedures also passed a **nine-stage chain**, cycling through the three actual applications three times. Each stage reads the preceding saved artifact: spreadsheet rows flow to the document's amount/summary and then to the presentation; the next spreadsheet cycle reads the presentation values and increments them. Independently checked totals were **606, 636 and 666**.

The first worker deliberately stopped at a durable checkpoint after three completed stages. A fresh worker process resumed the remaining six. The PM independently compared that pre-resume snapshot with the final checkpoint: the same run completed **9/9 stages**, every stage has **one attempt**, and the first three artifact hashes remained unchanged. All nine exported artifact hashes/byte counts were recomputed, every spreadsheet/document/presentation was reopened and its expected values checked, and the resume log contains exactly the six downstream starts with no repeated first-cycle stage. Nine result screenshots are preserved; the final Writer and Impress screenshots visibly show the **666** total and preserved source content.

Active execution took **303.644 seconds** across the two worker invocations. This is deliberate process-level checkpoint/resume, not a VM crash or automatic recovery of an arbitrary application session. `python bench/workflows/audit_chain.py` reproduces the passing independent audit saved in `results/pm-chain-audit.json`.

These examples prepare reviewable artifacts. CRM/HR/ERP changes, payments, order placement and email delivery are **not** demonstrated by producing local proposals or drafts. The harness dispatches work through the selected existing engine; app labels themselves are not connectors or application adapters.

## Concrete remaining limits

1. **Enterprise breadth remains unproven.** Browser fixtures, seven native transfers and twenty scripted GUI tasks are qualified separately above. These results do not establish production ERP/CRM/HR connectors or success on arbitrary enterprise apps. `bench/backoffice/e2e.py` remains an additional live-output benchmark requiring its own desktop/control-plane setup.
2. **Business correctness needs explicit assertions.** Structural/content checks and hashes alone do not prove financial totals, formula recalculation, forecast accuracy, HR identity matching, SLA policy correctness or consistency between a workbook and presentation. Generic JSON equality, tabular column/row checks and an optional separate business verifier can enforce additional requirements when the workflow author supplies them. The transfer benchmark independently asserts known fixture answers; this does not establish arbitrary real-world correctness.
3. **Visual quality is not assessed by artifact parsing.** Clipped slides, poor charts, unreadable document layout and spreadsheet usability require rendered or live-app inspection.
4. **Concurrent ownership is bounded, not a distributed execution guarantee.** The runner now uses per-run OS locks, ownership leases and claim tokens to prevent duplicate active claims and stale completion. Recovery still requires the old executor to stop; this does not establish exactly-once side effects in an external application or a distributed worker scheduler.
5. **Restart recovery is explicit.** Checkpoints persist and `/workflow resume RUN_ID` continues completed work. This is not automatic crash recovery of an in-flight application session. An interrupted non-idempotent step requires external reconciliation.
6. **Cancellation is cooperative.** Stopping at a question prevents subsequent workflow stages. It does not undo completed changes or forcibly interrupt an arbitrary currently running app operation.
7. **Dependencies are ordered.** Specs must list prerequisite stages first. Execution is sequential; arbitrary parallel DAG scheduling is outside this runner.

Release assessment: the generic workflow foundation has meaningful adversarial coverage, with live browser transfer checked for consistency against its runner's report, native application transfer, API teaching/correction, scripted GUI outcomes and a nine-stage native chain resumed without repeating completed work. Production enterprise breadth, real human demonstration capture, Microsoft Office installations and arbitrary domain-level outcomes require separate evidence before making a universal capability claim.
