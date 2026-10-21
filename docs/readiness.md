# Readiness before autonomy

Readiness belongs to one responsibility and executable revision. It is separate from a skill's `trusted` experience flag: that flag chooses between replay and the model, and never grants permission. A practice pass is evidence for the owner's reviewed suite, not a claim that all possible inputs will be correct.

The first release supports **offline local artifact tools**: saved Python or Bash scripts in a worker's playbooks. It does not qualify arbitrary GUI skills, live websites, mailboxes, host apps, phone apps or model-driven workflows. Those routes retain their existing supervision and approval gates. Sending, paying and publishing still need one-action approval. A readiness grant cannot bypass an existing gate.

## Use it

1. Open the worker's Skills page and choose **Readiness** (`/readiness/<worker>`). Start the worker so its saved tools can be read. New agents expose the exact executable at `GET /playbooks/<name>/qualification-source`; older agents cannot qualify from the truncated display preview.
2. Unlock owner controls using `qualification-owner.token` in the control plane's `DECURIX_DATA_DIR`. A fresh key is written at each launch, never exposed by an HTTP route, included in a backup, or deliberately copied into a worker. For a compose deployment it lives inside the control plane's `/data`; an owner can read it with `docker compose exec desktop-manager cat /data/qualification-owner.token`. Treat it like a local administrative credential. The page exchanges it for an eight-hour HttpOnly, SameSite=Strict session and requires a CSRF token for mutations. The primary listener must remain local as before. The readiness console is not registered on the gated listener.
3. Define the responsibility's scope, company/workspace, saved tool and reviewed practice suite. The server pins the **actual image ID**, exact source bytes, language, timeout, offline runner version, scope, fixtures and expected results into a SHA-256 revision. Source, image, scope and suite changes require a new exam.
4. Provide at least five distinct holdouts, covering `ordinary`, `varied`, `missing_input`, `invalid_input` and `injection`. Expected outcomes are `complete`, `escalate` (exit 20) or `approval` (exit 21). Ordinary, varied and injection inputs must complete correctly; missing/invalid input must escalate without output artifacts. An optional approval-stop case must produce no artifacts. The owner, rather than the model being tested, supplies and reviews expected results. Category names alone do not establish test quality: use unseen values and adversarial content that actually challenge this responsibility.
5. Run practice. Every case starts fresh. Review independent findings, artifact hashes and downloadable files; run history retains prior revisions and production outcomes. **Enable this scope** is available only after the entire reviewed suite passes. The server rechecks source/image before enabling and before every run. There is no automatic enabling after a pass.
6. Run real work through **Run artifact task**, supplying inputs, arguments and explicit saved-file checks. This runs the exact frozen script under the same offline policy. Download and review its results here. Ordinary desktop chat, schedules and mail workflows do not consume this grant.

## Case format

The page accepts `input_text` for UTF-8 fixtures and converts it to the API's `inputs` map of base64-encoded bytes. Binary workbook fixtures can be supplied directly as base64. Inputs and outputs are flat file names under `/work`; outputs cannot overwrite inputs. Arguments are passed as an argv list, never interpolated into a shell command. Do not include secrets or real production credentials in practice fixtures.

```json
{
  "id": "ordinary-order",
  "category": "ordinary",
  "args": ["input.csv", "result.json"],
  "input_text": {"input.csv": "item,amount\nA,10\nB,20\n"},
  "expected": "complete",
  "checks": [{"file": "result.json", "kind": "json_equals", "json": {"total": 30}}]
}
```

Checks supported by the independent Go verifier:

| Kind | Evidence |
|---|---|
| `text_equals` | Exact saved text, including newlines. |
| `json_equals` | Full decoded structure, rejecting duplicate keys and trailing documents. Numeric representation is compared exactly. |
| `csv_equals` | All saved rows and columns against `rows`; extra or missing rows fail. |
| `xlsx_cells` | Exact saved `sheet`, `cell` and `value`. For every checked formula cell, specify `formula_text` as well as the expected cached value. `formula: true/false` additionally requires a formula or a typed value. |

Workbook checks read OOXML in the control plane, not through code running in the agent. A formula without a saved value, an error cell, a missing cell, a duplicate package part, or a formula differing from the independently supplied `formula_text` fails. This is a cell contract, not a complete spreadsheet calculation engine; include every important input row, calculated result and formula in the check. These checks do not establish formatting, macro safety, visual appearance or other unchecked workbook properties. Unsupported semantic criteria require supervision rather than a model-confidence substitution.

Any additional output file, modified/deleted input, nested output directory, symlink, duplicate tar entry, runner timeout or collection failure fails the case. Each file is limited to 8 MiB, each case's inputs and artifacts to 16 MiB, and collected evidence per run to 64 MiB. Exceeding a limit fails closed. Suites are bounded to 30 cases, each with a 1–300 second timeout. The HTTP runner permits two active runs overall and one per responsibility.

## Isolation and authority

`internal/desktop/qualification.go` creates a new container directly, without using normal desktop creation, migration, launchers or credential seeding. It runs the installed immutable local image as UID/GID 1001 with its entrypoint replaced. Images declaring volumes are refused. The root filesystem is read-only; home, work and `/tmp` are disposable tmpfs. There are no host binds, shared tool/cache mounts, ports, host-gateway entry, Docker socket, production sessions or network connectivity. All capabilities are dropped, `no-new-privileges` is set, and memory, CPU, process count and output size are bounded. Image ENV values are cleared too. This boundary relies on Docker and the trusted installed image/kernel, not on regexes or prompts.

Only script bytes, case inputs and argv enter the container. Expected answers and verifier code remain in the control plane. After the program exits, all its subprocesses are paused before saved files are collected. The container is then removed. Practice and production use exactly this boundary. No model, mutable experience history, browser snapshot or `checked=true` report can qualify a run.

Canonical definitions, runs, findings, artifacts and audit events live in the control plane's SQLite database (`internal/qualification`), included in the database snapshot backup. Findings and files are persisted after each case, so a restart preserves completed evidence. Agent callback routes refuse the reserved `readiness-` worker identities. Each evaluation also has a normal task record; practice tasks are explicitly excluded from production autonomy reports. A task marked wrong revokes its linked responsibility; retracting the verdict does not restore a grant.

Readiness states are `untested`, `testing`, `qualified`, `needs_retesting`. Authorization states are `supervised`, `enabled_within_scope`, `suspended`. All cases must pass, and a person must enable the current revision. Qualification expires after 30 days or an offline policy change. An enabled run failing verification suspends permission immediately. Cancellation is durable before the process is stopped, so a late success cannot undo it. Interrupted production needs reconciliation and retesting, and is never automatically replayed. Startup removes only tagged qualification containers, reconciles task records, and returns previously enabled grants to supervision for owner review. Restore preserves evidence, not active permission.

## Validation and remaining scope

Focused tests cover revision binding, incomplete suites, independent JSON/CSV/workbook checks, expected stops, cancellation, restart, expiry, concurrency, forged callbacks, owner sessions, CSRF, backup exclusion and exact source export. A live Docker test is still needed on the running deployment to validate the complete container execution/collection path. Do not present unit coverage as a successful live exam.

The Docker adapter integration test drives the complete API sequence against a stand-in daemon: create with the restricted serialized configuration, copy only executable/inputs, execute with a cleared environment, pause before collecting, extract saved files, and remove the container. For the real-daemon check, set `DECURIX_QUALIFICATION_IMAGE` to an existing installed Linux desktop image ID and run `go test ./internal/desktop -run TestQualificationLive -count=1 -v`. This test never builds or pulls an image. It checks root/network isolation, symlink rejection and detached-child freezing.

Validation on the implementation checkout: `go vet ./...` passed; the Go suite passed with the three pre-existing shared-folder tests excluded because their `filepath.EvalSymlinks(t.TempDir())` calls returned `Access is denied` in this sandbox. An unfiltered suite reports those three failures. The exact-source tests (2), playbook regression tests (55), and agent HTTP API tests (7) passed. Readiness JavaScript syntax and locked-page initialization passed. The live Docker test was skipped: Docker/WSL access was unavailable, and automatic approval review could not authorize the WSL inspection because its review deployment returned HTTP 404.

Practice for taught skills and recordings (generated, owner-reviewed suites run in a disposable practice desktop) is designed in [simulation.md](simulation.md); its types, verifier and store are built, and its runner is not yet. To qualify model-driven desktop workflows next, implement a dedicated staging network and constrained broker for model calls/fixtures, independent application state verifiers, owner-approved fixture provisioning, frozen skill/rules/memory snapshots, and structured grants enforced at every ingress. No live-site grant should be offered until those boundaries exist. The local artifact grant deliberately cannot flow into those routes.
