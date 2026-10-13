# Product manager acceptance review

Reviewed 14 October 2026. This review evaluates a generic computer-workflow foundation and demonstrated-task transfer. It does **not** establish that the product can perform every computer task.

## Independent acceptance result

`PYTHONPATH=agent python -m unittest discover -s agent/tests -p test_workflow_acceptance.py`

**26 tests passed.** Tests use independent callbacks, temporary files and deliberately invalid Office packages; they do not open Excel, PowerPoint, Word, a browser or a mail application.

The production chat-dispatch integration suite also passed **6 tests** using a fake harness. It covers stage sequencing, one aggregate final report, evidence-gated completion, resume, engine cleanup before dispatching the next stage, explicit stop and continuation after a paused query. The root agent's combined regression suite passed **61 tests**; its offline soak passed **100 runs / 1,000 stage attempts** in **95.015 seconds**, with transient failure injection and runner reconstruction. The soak explicitly replaces Office outputs with simulated JSON receipts, so those results establish orchestration resilience rather than Office application correctness.

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

`python bench/workflows/transfer.py --output bench/workflows/results/transfer.json` passed **150 evaluations**: one persisted demonstration and one ready skill per family, each evaluated on six variants. The benchmark uses the production recording store, learning questions, skill persistence and `skill_engine.run`; only the application observation/action adapter and model-based fit check are replaced. Variants cover unseen inputs, moved controls, renamed controls, ambiguous controls, missing parameters and deliberately incorrect business results. Independent business assertions reject wrong answers even when the scripted UI reports completion. All **6 learning-transfer acceptance tests** passed in **22.139 seconds**.

The rename variants exposed a production defect: a control renamed from `Source data` to `Source data:` was safely healed for the action, but its value postcondition retained the obsolete label and falsely stopped the run. Engineering now applies an ephemeral same-step alias after deterministic healing, preserving the expected value and original recording. All twenty-five rename holdouts pass after the fix.

These are **scripted-application tests**, not recordings captured from real mouse/keyboard input or live Office automation. They validate the existing demonstration format, learning and transfer pipeline without claiming actual application operation. The JSON report explicitly records `live_apps_tested: false`.

These examples prepare reviewable artifacts. CRM/HR/ERP changes, payments, order placement and email delivery are **not** demonstrated by producing local proposals or drafts. The harness dispatches work through the selected existing engine; app labels themselves are not connectors or application adapters.

## Concrete remaining limits

1. **Live multi-app qualification remains open.** Docker's desktop engine was unavailable in this environment. No live Office or enterprise-app success should be inferred from unit tests or the offline soak. `bench/backoffice/e2e.py` remains a separate live-output benchmark requiring a running desktop/control plane.
2. **Business correctness needs explicit assertions.** Structural/content checks and hashes alone do not prove financial totals, formula recalculation, forecast accuracy, HR identity matching, SLA policy correctness or consistency between a workbook and presentation. Generic JSON equality, tabular column/row checks and an optional separate business verifier can enforce additional requirements when the workflow author supplies them. The transfer benchmark independently asserts known fixture answers; this does not establish arbitrary real-world correctness.
3. **Visual quality is not assessed by artifact parsing.** Clipped slides, poor charts, unreadable document layout and spreadsheet usability require rendered or live-app inspection.
4. **Concurrent ownership is bounded, not a distributed execution guarantee.** The runner now uses per-run OS locks, ownership leases and claim tokens to prevent duplicate active claims and stale completion. Recovery still requires the old executor to stop; this does not establish exactly-once side effects in an external application or a distributed worker scheduler.
5. **Restart recovery is explicit.** Checkpoints persist and `/workflow resume RUN_ID` continues completed work. This is not automatic crash recovery of an in-flight application session. An interrupted non-idempotent step requires external reconciliation.
6. **Cancellation is cooperative.** Stopping at a question prevents subsequent workflow stages. It does not undo completed changes or forcibly interrupt an arbitrary currently running app operation.
7. **Dependencies are ordered.** Specs must list prerequisite stages first. Execution is sequential; arbitrary parallel DAG scheduling is outside this runner.

Release assessment: the generic workflow foundation and scripted learning transfer across twenty-five task families have meaningful adversarial coverage. Live app operation, real user demonstration capture and domain-level business outcomes require further qualification before making a universal capability claim.
