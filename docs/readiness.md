# Readiness before autonomy

Readiness belongs to one responsibility and executable revision. It is separate from a skill's `trusted` experience flag: that flag chooses between replay and the model, and never grants permission. A practice pass is evidence for the owner's reviewed suite, not a claim that all possible inputs will be correct.

The grant that is in force covers **offline local artifact tools**: saved Python or Bash scripts in a worker's playbooks. Taught skills and recordings can be practised with Simulate ([below](#beyond-local-artifact-tools)), but no such grant starts work yet. Live websites, mailboxes, host apps, phone apps and other model-driven workflows are not qualified. Those routes retain their existing supervision and approval gates. Sending, paying and publishing still need one-action approval. A readiness grant cannot bypass an existing gate.

## Use it

1. Open the worker's Skills page and choose **Readiness** (`/readiness/<worker>`), or **Simulate** on its dashboard card (`/readiness/<worker>#simulate`). Start the worker so its saved tools can be read. New agents expose the exact executable at `GET /playbooks/<name>/qualification-source`; older agents cannot qualify from the truncated display preview.
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

Only script bytes, case inputs and argv enter the container, through an exec'd `tar` (`exectar.go`). Expected answers and verifier code remain in the control plane. After the program exits, every process of its user is stopped (`SIGSTOP`) before saved files are collected the same way. The container is then removed. Practice and production use exactly this boundary. No model, mutable experience history, browser snapshot or `checked=true` report can qualify a run.

Canonical definitions, runs, findings, artifacts and audit events live in the control plane's SQLite database (`internal/qualification`), included in the database snapshot backup. Findings and files are persisted after each case, so a restart preserves completed evidence. Agent callback routes refuse the reserved `readiness-` worker identities. Each evaluation also has a normal task record; practice tasks are explicitly excluded from production autonomy reports. A task marked wrong revokes its linked responsibility; retracting the verdict does not restore a grant.

Readiness states are `untested`, `testing`, `qualified`, `needs_retesting`. Authorization states are `supervised`, `enabled_within_scope`, `suspended`. All cases must pass, and a person must enable the current revision. Qualification expires after 30 days or an offline policy change. An enabled run failing verification suspends permission immediately. Cancellation is durable before the process is stopped, so a late success cannot undo it. Interrupted production needs reconciliation and retesting, and is never automatically replayed. Startup removes only tagged qualification containers, reconciles task records, and returns previously enabled grants to supervision for owner review. Restore preserves evidence, not active permission.

## Tests

The unit tests cover revision binding, incomplete suites, the independent JSON/CSV/workbook checks, expected stops, cancellation, restart, expiry, concurrency, forged agent callbacks, owner sessions and CSRF, primary-only routes, backup and host-file exclusion of the owner key, and exact source export (`internal/qualification`, `internal/httpapi/qualification_test.go`, `agent/tests` for `playbooks.qualification_source`). `TestQualificationDockerAdapter` drives the whole container sequence against a stand-in daemon: create with the restricted configuration, copy in only the program and inputs, execute with a cleared environment, stop every process before collecting, extract the saved files, and remove the container.

Unit coverage is not a live exam. The real-daemon test needs an installed Linux desktop image and never builds or pulls one:

```bash
DECURIX_QUALIFICATION_IMAGE=<image ID> go test ./internal/desktop -run TestQualificationLive -count=1 -v
```

It checks that the program runs as UID 1001 with an empty home, no Docker socket, no desktop environment values, a read-only root and no network; that a symlink cannot carry another file out; and that a detached child is frozen before collection. Without the variable it skips.

## Beyond local artifact tools

Practice for taught skills and recordings is **Simulate**, on the same page ([simulation.md](simulation.md)): the hosted heavy model drafts a suite, the owner reviews and approves it, the worker runs it in a disposable practice desktop against stand-ins, and code decides every case. Its routes (`/api/readiness/simulation/...`) need the same owner session. A passed run can be enabled by the owner, but nothing yet starts a taught skill unattended on that grant (`qualification.Store.PracticeGrant` has no caller); by the owner's rule that waits until practice runs are proven live.

Live websites, mailboxes, host apps and phone apps stay out of scope. Qualifying them would need a staging network, independent application-state verifiers, owner-approved fixture provisioning, and structured grants enforced at every ingress. No live-site grant is offered until those boundaries exist. The local artifact grant cannot flow into those routes.
