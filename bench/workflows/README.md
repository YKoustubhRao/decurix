# Generic workflows and learning transfer

`workflow_runner.py` executes arbitrary ordered stage specifications with checkpoints, isolated outputs, ownership leases, bounded retries, approval boundaries and independent evidence checks. `workflow_catalog.py` is a data loader for 25 editable examples in `workflow_examples.json`; no task-specific execution logic lives in the runner or loader.

Examples cover table reconciliation/deduplication/pivots/model updates, inbox triage, draft personalization, scheduling, document templates/redaction, PDF assembly/extraction, presentation creation/chart refresh, record imports, completeness checklists, escalation, quotation comparison, file renaming, archives, CSV conversion, database reports, inventory calculations, translation review, knowledge articles and release verification. Routes vary between two and three stages and produce different artifacts. Examples are instructions, not claims of tested application adapters.

```text
/workflow list
/workflow start table-reconciliation {"source_files":["/home/headless/work/first.xlsx","/home/headless/work/second.csv"],"instructions":"Match by record ID, retain unmatched rows and check the control total."}
/workflow status RUN_ID
/workflow resume RUN_ID
/workflow cancel RUN_ID
/workflow approve RUN_ID STEP_ID
```

Use `/workflow start-json {"spec": {...}, "inputs": {...}}` for any other workflow. Stages name `id`, optional `app`, `instruction`, `depends_on`, `checks`, `retry_safe`, `max_attempts` and optional `approval`. A learned stage can provide `skill: {"name":"saved-skill","params":{"customer":{"input":"customer"}}}`; it runs the actual saved skill through the existing teaching/replay system, followed by the workflow's artifact checks.

Checks support text, JSON, CSV/TSV, HTML/XML, Office files, PDF, EML, images and exact-hash binary artifacts. Generic `json_equals` maps RFC 6901 pointers to expected values; CSV checks can require `columns` and `min_rows`. Example:

```json
{"path":"result.json","format":"json","json_equals":{"/totals/count":3,"/totals/amount":110}}
```

Local file locks protect checkpoint mutations. A running stage has a process-identity lease and token; another caller cannot execute or complete it. Safe stages whose owner died can resume; interrupted non-idempotent work becomes `uncertain` and needs external reconciliation. `/workflow recover RUN_ID` explicitly recovers an expired remote-owner lease after the executor has stopped. Recovery never establishes exactly-once effects in an external application. Stop/cancel prevents subsequent stages, but does not undo work or forcibly interrupt arbitrary app operations.

## Tests and qualification

```powershell
$env:PYTHONPATH='agent;agent/tests'
python -m unittest agent.tests.test_workflow_acceptance agent.tests.test_workflow_hardening agent.tests.test_workflow_task agent.tests.test_watcher_restart agent.tests.test_learning_transfer agent.tests.test_office_presentations agent.tests.test_office_cli -v
```

The offline transfer suite persists one demonstration and one learned skill per task family, then exercises unseen values, moved controls, renamed controls, ambiguous controls, missing parameters and wrong application results. It uses production recording, learning and skill execution with a scripted screen adapter, and labels this explicitly:

```text
python bench/workflows/transfer.py --output bench/workflows/results/transfer.json
python bench/workflows/soak.py --cycles 4 --output bench/workflows/results/soak.json
```

For actual desktop actions, build the production image and provision isolated test desktops. Provisioning never reuses or removes an existing desktop. `--keep` preserves the new VM; reports, screenshots, recordings and learned skills are copied back even after failures:

```text
docker build -f docker/desktop.basic.Dockerfile -t decurix-desktop:basic .
python bench/workflows/provision.py --suite transfer --keep
python bench/workflows/provision.py --suite native --keep
python bench/workflows/provision.py --suite gui --keep
```

The live transfer suite captures real Chromium actions through the production audit recorder, learns from one recorded example, then checks holdout values under unchanged/moved/renamed controls. Its 25 deterministic local-fixture operations are not native Office/enterprise-app qualification. The GUI suite independently checks outcomes in Chromium, Calc, Writer, Mousepad and Thunar using scripted actions through the same production GUI layer. `native_transfer.py` captures seven native-app lessons and replays unseen values into new source files. `native_chain.py` combines saved lessons in a nine-stage Calc/Writer/Impress workflow with actual disk handoffs and independent numeric checks.

For explicit local source-file transfer, the skill API accepts `source_files={"/absolute/demo.ods":"/absolute/holdout.ods"}` and the CLI accepts `gui do NAME --source-file /absolute/demo.ods=/absolute/holdout.ods`. The target must be an existing local file of the same type. This rebinds explicit input opens on a run copy, preserving the learned procedure and typed save destinations; it never guesses a file by basename.

For model-driven multi-app examples, use an idle connected desktop with a configured model and an inputs JSON object keyed by selected example IDs:

```text
python bench/workflows/live.py --desktop office1 --inputs inputs.json --timeout 7200
```

The runner uses chat and reopens every saved artifact from a separate process before accepting completion. Structural parsing and hashes do not prove numerical accuracy, formula recalculation, visual layout or cross-artifact consistency. Add task-specific semantic checks and rendered/live inspection for those guarantees. Reports distinguish simulated adapters, real fixture applications, scripted native GUI tasks and model-driven tasks; do not combine them into a single application-success claim.

## Qualification recorded on 14 October 2026

| Check | Result | Evidence and scope |
|---|---|---|
| Agent regression suite | 1,284 tests, 9 skipped | `results/agent-tests-final.log`; includes the production fixes found during live testing |
| Offline learning transfer | 150/150 | 25 families, six variants each; simulated application adapter |
| Live browser learning transfer | 75/75 | `results/live-transfer-full.json`; 25 transformations sharing one local browser form, three holdout variants each |
| Native learning transfer | 7/7 | `results/native-consolidated.json`; Calc, Writer, Impress and Thunar, one agent-recorded lesson per case, unseen values and independently reopened saved artifacts |
| Native multi-app chain | 9/9 | `results/native-chain-final.json`; three Calc/Writer/Impress cycles, real file handoffs, fresh-worker resume after stage three, one attempt per stage |
| Full teaching/correction API flow | Passed | `results/skill-api-verified.log`; one demonstration, teaching questions, three runs, safe pause and correction that resumes only the final step |
| Scripted GUI outcomes | 20/20 | `results/gui-oracle/20261014-193022.json`; Chromium, Calc, Writer, Mousepad and Thunar |
| Benchmark failure reporting | 2/2 | `results/gui-exit-tests.log`; a rejected business result produces a failing process exit even when actions report success |

The independent [PM review](PM_REVIEW.md) records the evidence checks and remaining limits. Live runs use isolated Linux desktop containers, Chromium fixtures and LibreOffice; these results do not establish native Microsoft Office, enterprise account integration, human demonstration capture or unrestricted model autonomy. Results and screenshots are local, ignored artifacts preserved under `results/`.

Live testing exposed missing labels on LibreOffice editable fields and loss of earlier supplied parameters when replay resumed at a later step. Both are fixed in production with regressions. The native report preserves failed attempts as well as passing reruns; fixture recovery-state and dialog-scope corrections are identified separately.

Recheck exported evidence from the repository root:

```text
python bench/workflows/audit_evidence.py
python bench/workflows/audit_native.py
python bench/workflows/audit_api.py
python bench/workflows/audit_chain.py
python -m unittest discover -s bench/gui -p test_run.py -v
```

After teaching native lessons, run the chain inside that same isolated desktop (`DISPLAY=:1`, `PYTHONPATH=/opt`). Pass the native report containing those exact saved skills. The first process intentionally stops at a durable checkpoint; copy its `run_id` into the second command:

```text
python3 /home/headless/bench-workflows/native_chain.py --reports /home/headless/work/native-transfer.json --max-stages 3 --output /home/headless/work/native-chain-checkpoint.json
python3 /home/headless/bench-workflows/native_chain.py --reports /home/headless/work/native-transfer.json --resume RUN_ID --output /home/headless/work/native-chain.json
```
