# Follow-ups after the review of 2026-11-03

The deep review of 2026-11-03 fixed about 300 findings on the `review-fixes` branch. This file lists what it left open: the owner's decisions, the deferred items, and the live checks the writers could not run (no containers, no app builds, no paid APIs). Each line carries its review ID. Security items are written by input class, never as attack strings.

## Owner decisions

| ID | Decision | State |
|---|---|---|
| O-1 | Input broker and OS boundary: direct input around the gate (XTEST, VNC, CDP, AT-SPI, host-app and phone viewers), a gate-owned evidence store, `checked` set by the worker, the script sandbox's filesystem | **Open, do next.** The word guards are frozen. Order: the evidence store under the gate user first (it makes "observed" real), then the input broker ([gate-boundary.md](gate-boundary.md), "Next: the input broker"). Also: a per-task `$HOME` snapshot, read-only binds outside `~/work`, an egress proxy |
| O-1.a | TheBot command guard: five structural fixes before the final freeze | **Done** 2026-11-07: all five landed and the guard is frozen (`safety.go`'s FROZEN note). Table below |
| O-2 | Per-desktop cache volume (B-02) | Done as recommended: a cache volume per desktop, the model cache shared read-only |
| O-3 | Brainiac appliance: model user, AppArmor, signed catalog, uploader list | **Open; required before any box ships.** Table below |
| O-4 | Legacy harnesses (Azure, `task_runner`, Claude Code) | **Done** 2026-11-07: deleted (`azure_harness`, `task_runner`, `claude_harness` with `claude_guard` and the MCP server, `internal/claudeauth`, the Claude Code CLI in the tools image). A desktop created with one runs TheBot; the `claude_session` table keeps its migration steps, emptied (harnesses.md) |
| O-5 | Public tunnel off by default (B-08) | Done: off by default, one switch in the Mobile menu, `DECURIX_PUBLIC_LINK=1` for the scripts. Veto if you disagree |
| O-6 | A typed "yes" in the desktop chat as an approval (W-07) | Kept, with `seen_at` frozen at the first keystroke and buttons bound to the note id. **Open:** buttons-only is safer if the UX is acceptable |
| O-7 | Org knowledge and skill writes from the gated listener (A-08) | Done: host-operator only |
| O-8 | Acquired skills, proven fixes and the coding harness inside Decurix (T-07, T-08, T-21) | Done: off unless `THEBOT_STANDALONE=1`. Veto keeps them |
| O-9 | taskRules audit to principles only (T-18) | Staged as commit `ab8cac8`. **Open:** bench it on hosted brains before keeping it |
| O-10 | God-file splits (`core.py`, `long_task.py`, `skill_task.py`, `operator.go`, `httpapi`) and the check-registry rewrite | First split done (2026-11-07): `long_task.py`'s stage checks and the cross-check's code part moved to `stage_checks.py`, no behaviour change. **Open:** the other files, one at a time, each behind the replay gate |
| N-W4-8 | A Landlock file policy for Brainiac's headless browser (profile folder plus the system read set, network kept) | **Open.** Prove it with a live Linux run before Render may be turned on on an appliance |
| N-W4-8 note | Render stays off the automatic path everywhere, off by default on an appliance, behind the guard proxy, page read bounded | Done |

### O-1.a: the guard's last structural fixes

**Who:** a human-authored commit, or a fresh session whose brief contains no command strings. Do not have a model analyse evasion shapes; a safety classifier stopped two agents three times on this. **Paths:** `agent/thebot/internal/safety/` and the callers of `AssessCommand`. Do the two with no command shape first.

| # | Input class the guard mishandles | Structural change | Test, built mechanically from the guard's own flagged examples |
|---|---|---|---|
| O-1.a.3 (**done** 2026-11-07: `AssessCommand(command, dir)`, the operator and terminal pass `rc.Dir()`) | (no shape) The folder a command runs in | Pass the real working folder from the operator, terminal and coding callers into `AssessCommand`, and resolve paths against it instead of assuming `~/work` | A relative recursive delete with a working folder of `$HOME` is flagged |
| O-1.a.4 (**done** 2026-11-07: `localTarget()` judges `url.Parse(...).Hostname()`, refusing what it cannot parse) | (no shape) A URL whose authority carries a user-info part | The fetch tools' "local only" exemption judges each target by `net/url`'s `Hostname()`, not a regex over the raw text | An existing remote target, rewritten with a user-info part, is not exempted |
| O-1.a.1 (**done** 2026-11-07: `programLines()` reads each command word's base name past `commandWords()`'s wrappers, the ones `wipeConcern`'s examples name, and every rule list is matched on those lines too; `blankHeredocs` finds the program a body is fed to the same way; the word reader is `eachCommand()`, shared with `wipeConcern`) | A program named by its absolute path, or behind a wrapper program | Every program rule identifies the program by the base name of the command word, after skipping the wrappers `wipeConcern` already models (reuse its parser) | Every existing flagged example, with its program given by absolute path or behind a wrapper, is still flagged |
| O-1.a.2 (**done** 2026-11-07: `beforeSubcommand()` in the git, npm/yarn/pnpm/cargo/twine/gem and docker rules reads past folder and config options, with their values, and other lone options) | Global options placed before a subcommand | The version-control and publish rules accept the folder and config options before the subcommand | Every existing flagged version-control or publish example, with such an option inserted, is still flagged |
| O-1.a.5 (**done** 2026-11-07: `deleteConcern()` with `runTimeName()` for rm and find (`recursiveDeleteConcern()`), a cd to a run-time folder makes it `runTimeDir`, `splitCommands()` keeps a substitution's place in its word, and `localTarget()` refuses a run-time target) | An operand decided at run time (a variable or a command substitution) | A recursive delete's operand, or a fetch tool's target, that is decided at run time waits for a person | An existing flagged recursive delete with its path replaced by a variable is flagged |

**Not to be done** (the freeze): new verbs, new power-management verbs, heredoc body scanning, a shared Go/Python corpus. After O-1.a, `safety.go`'s FROZEN note says these five were the last changes. Related: the Python guard's "a folder it cannot follow waits" (F-10) is done in `commands.py`; anything further there belongs in this list too.

### O-3: before a Brainiac box ships

The mount namespace, `sandbox: required` and stage-only updates landed. These need root, an image or a release pipeline:
1. **A dedicated model user.** A `brainiac-model` system user with no supplementary groups, no login shell and no home. Brainiac starts model processes as that user through a small setuid-free launcher (a systemd service, or a root helper that only accepts "run this lock's binary with this policy"). It covers hosts that refuse user namespaces, where a model process today keeps the operator's groups (docker included). Store layout: models readable by group `brainiac-model`; the socket folder owned by `brainiac-model`, with a group the serving user is in.
2. **An AppArmor profile** for Ubuntu 24.04+ (`apparmor_restrict_unprivileged_userns=1`) that permits the user namespace for the Brainiac binary only, so the mount view works there instead of falling back.
3. **A signed catalog.** Every catalog file pins `revision` (a Hugging Face commit) and `sha256`; every GitHub asset pins `sha256` (`source.sha256` and `runtime.llamacpp_sha256` are ready). The catalog ships as a signed release, and the appliance checks the signature before using an overlay. Several sherpa-onnx whisper assets (`whisper-turbo`, `whisper-small`, `whisper-tiny.en`) have no GitHub digest and cannot be installed until the catalog carries their sha256 (they fail closed).
4. **A shorter trusted-uploader list** for automatic candidates (`scout.go` holds about 55 orgs plus individuals): first parties only, everything else through `brainiac scout adopt`.
5. **Hash-locked worker requirements** (G-09). On a networked machine, `pip-compile --generate-hashes` for Python 3.11-3.14 on each OS, for `requirements-ocr.txt` and `requirements-asr.txt`. numpy is still a range.
6. **Signed-release promotion.** `update.Promote` is the one place a staged plan becomes served. A signed release can call it once there is a local parity eval: the corrected `Proven` on the request shapes the box serves.

## Deferred items

### Control plane, desktops, scripts, Electron (W1)

- **Digest pins** (B-24): `accetto/ubuntu-vnc-xfce-chromium-g3` (still `:latest`), `alpine:3.24` and `ollama/ollama:0.40.1` are pinned by tag only. Pin digests on the next build (needs a pull).
- **Dead bridge verbs** (B-27): about 1,500 lines of `bridge.ps1` verbs Go never sends. First item for the next loop with a live Windows run.
- **B-19:** clean up host browser profiles when a VM is forgotten (needs a new bridge verb). The launch-time quarantine covers the dangerous case.
- **W-14:** one aggregated endpoint per dashboard tab (performance only).
- **Done: C-25.** The practice broker knows its desktop by the address a callback comes from, never by `desktop_id` in the body: `Broker.Caller` = `desktop.Manager.PracticeDesktopAt` (labelled `practice-<run>` containers on `decurix-practice`, never the mock), answers kept per address (`Broker.fromRunDesktop`), a body naming another desktop refused, no `Caller` refuses all (simulation.md).
- **A-07:** no `knownHost` list on the gated listener (accepted: the gated listener is reached under changing host names; the session cookie and same-origin rule answer rebinding).
- **Done: B-37.** Each schedule and DAG cron has its own IANA zone (`schedule.Expr.NextIn`, `zone`/`cron_zone`, appended Migrate steps; old rows read UTC), picked on the forms from the browser's zone.
- **B-29:** a restart loop for the Windows tunnel (`Lan-Tunnel.ps1`); the tunnel is opt-in now.
- **Done: B-10.** Desktops reach the native Ollama only through an allow-listed proxy (`internal/httpapi/native_ollama.go`, `modelRequestPolicy`; d25d201).
- **Done: W-20.** The viewer's script is served at a path naming its own bytes (`noVNCPath`, `rfb-1.6.0-<sha256 12>.js`), so a changed file is never a stale cached copy. Also done: the in-flight guards (`loadNotes` and `pollResourceAlerts` go through `Decurix.poll`, one call at a time) and a bounded `/api/desktops/memory` (`?limit=`, default 200, max 1000, `truncated`).
- **Done: RW1-G-04 rest.** A start or a resume waits for a tools refresh in progress, at most 5 min and never past the request (`holdToolsForStart`, `ErrToolsRefreshing`).
- **Done: orphan home volume.** Create answers 409 `leftover_home`, and the form offers to delete the old volume (`POST /api/desktops/home-volume/delete`, primary listener, humansOnly; refused while any container mounts it or it is not that desktop's labelled home).
- **Done: RW1-D-01.** A desktop container carries its creation's id (`decurix.creation`, as its home does); after a `ContainerCreate` error, `Manager.removeHalfCreated` removes a container of exactly that name with that creation's id, never another's.
- **Decided: the default local model stays `devstral-small-2:24b`** (correctness before speed; `auto` still picks per machine). Soul, `start_agent_terminal.sh` and `config.py` now fall back to the same model.
- **Done: mail jobs.** A finished job with no request no longer shows "Run again" / "Send anyway".
- **No-echo walker limits** (RW1-G-06 is done): an error variable not named like one, or a helper that returns the text, still gets past it.
- **Done: two control planes on one data folder.** Start takes the data directory's exclusive lock (`sqlitedb.LockDataDir`, `decurix.lock`: flock, or LockFileEx on Windows) before the database, and a second start refuses with the holder's pid and start time (`ErrDataDirInUse`).
- **P3s not done:** B-02 (a panic path leaves only `last_error`), C-06 (a live test positive control; walk all packages), C-07 (a main-level Docker-down test; the watcher is tested), F-05 (owner's call), A-02 to A-06 (Electron details), F-08, F-10, F-11 (notes).

### Python agent (W2)

- **O-10:** the check-registry rewrite (step 1, the `_checked` wrapper, is done). The first god-file split is done (2026-11-07): the stage checks (`stage_code_findings`, `CHECK_MATURITY`, `_checked`, every check and the helpers only they use) and the cross-check's code part (`code_disagreements`, `model_disagreements`, `odd_one_out`) are `stage_checks.py`; long_task keeps the prompts and the run, imports what it calls and re-exports the names other modules and tests read through it. stage_checks never imports long_task: the run's ledger, runner and model call are handed over at import (`stage_checks.use_run`), looked up in long_task at each call. A test that patches a check helper patches it on `stage_checks`. Left: `core.py`, `skill_task.py`, `operator.go`, `httpapi`.
- **E-41:** the "PAYMENT TO " string branch only adds a prompt hint and a warning; make it a typed field when `FileFacts` reaches `code_problems`.
- **E-42:** `claims_done` stays (its only effect is a second judge call, whose "cannot" ends "not checked"; widening its regex would be a word list).
- **E-43:** run-folder retention is the owner's call (run folders hold the person's outputs; locks, temp files and host copies are pruned).
- **E-30:** "only other parties' messages": nothing structural marks another party's message, so it is not chosen by words.
- **D-32** (partly done), **D-34** (harmless for allow decisions).
- **D-36 rest:** OpenHands import URLs; `skill_engine`'s no-model right-system check; the grounder's one-probe disable; `ocr._pgm` speed. (`bind_sources`' substring match is done, `98bb9d8`.)
- **Replay corpus breadth:** a second synthetic run with an Office output and a payments table, so `system_entry`, `payee_approval` and `files_open` reach their cases, not only get called. Or word the assertion as "called".
- **The cancelled-row test** checks maturity only, not `stage_code_findings` end to end.
- **The X-6 test** calls `_pause` directly; it should drive `_prepare`/`_execute`.
- **The ledger:** withholding runs on every save rather than once (fails closed); done: only a parse error moves the ledger aside; an `OSError` is retried four times with backoff, then the save fails loudly and leaves the file (`long_task._ledger_io`).
- **A stale "filled from" note** could add a block for a reused output path (fc0849c).
- **An all-capitals request** hides the client's own facts (fails closed).
- **Done: an IBAN** with an unknown country and a valid mod-97 is reported "IBAN NOT CHECKED" among the unchecked payments (`payees._is_iban` answers None).
- **A second note on the same value** overwrites `OTHER_SIGN`.
- **Done: `false_sums`** skips a range of numbered things and its count ("Rows 2 - 10 = 9.", `values.range_count`, by structure); "200 - 350 = 150" is still a wrong sum.
- **X-6 for TheBot:** an optional structured secret marker on HELP.
- **Done 2026-11-07: the legacy harnesses** are deleted (O-4).

### TheBot (W3)

- **O-1.a** (above).
- **Closed 2026-11-08: N-W3-3.** `asksAppNamesRe` is gone. A HELP is held once for names the application lists only on a
  fact of the listing (`listOnScreen`, `screen_answer.go`): it names a list a listed window shows (a field's options,
  or a column of a table's first row) whose entries hold letters; the lines go back with the nudge (`NamesNudged`).
  With no listed screen showing that list it goes to the person (a names question for a page not yet listed is no
  longer held). No T-20 gate table exists in the repository; the fact is recorded where the gate is
  (`screen_answer.go`) and in harness-v3-plan.md item 4.
- **Closed: T-19.** A recall score floor was measured and decided against (`agent/thebot/internal/memory/l1.go`, the note at ~318).
- **T-27:** the embedded UI is the owner's decision.
- **A planted row in `l1.db`** (worker-writable) only shows "you approved this on <date>" in a standalone coding question. Word it as "a record on this machine says ...".

### Brainiac and bench (W4)

- **G-11:** carrying unchanged units into the new engine. The right shape is one Residency per Server, outliving engines.
- **G-17:** re-derive the workflow bench's results from artifacts (the name now says what is checked).
- **G-09:** hash-locked requirements (O-3.5).
- **FIX-BR-1:** a managed (non-system) python for the worker. `isolate.ForProgram` (`brainiac/internal/isolate/elflibs.go`, called from `brainiac/internal/engine/engine.go` `New`) grants its binary and the shared libraries it links, but not its standard library (`../lib/python3.x`), so it may not start in the mount view. A system python is unaffected (`/usr` is read). Check it live, and grant that tree read-only if it fails.

## Live checks done on 2026-11-07 (image built from this tree, a dev control plane beside the app's)

- **The gate starts with a cleared environment:** its process holds only `CONTROL_PLANE_URL`, `DECURIX_DIR`,
  `DECURIX_WORK_DIR`, `HOME`, `LANG`, `PATH` (read as the gate's own user; root in the container cannot, by design).
- **No websockify** runs in a new desktop; the screen reads through the control plane's own bridge.
- **Chrome's Memory Saver policy** is installed; the switched-off session services do not run; soul starts.
- **Memory given back** (docs/lightweight.md L2): a desktop paused 30 minutes is stopped (`rest_reason: idle`); with
  the memory held elsewhere, a start stops the longest-idle paused desktop first (`rest_reason: memory`); with nothing
  to stop, the start is refused in the person's words (the wording was fixed after this check: it blamed "0 busy
  desktops" for memory another program held).
- **The resource bench, before and after** (docs/lightweight.md): idle anonymous memory 162 to 140 MB, gui observe p50
  3.5 to 1.6 s, p95 of all gui commands 3.5 to 1.6 s.
- **`bench/skills/e2e.py` and `bench/recordings/e2e.py`** pass on that image with this tree's agent.
- **The gui daemon and screen broker tests** pass on Linux (74 tests; two code bugs fixed: the idle clock started
  before the daemon served, and a socket's inode compared with sockfs's).
- **Still owed:** the sign-in refusal on a real gated listener (needs a worker with an open question; the integration
  test covers it with the real stores and routes), the field-unit question's round trip with a hosted brain, the
  screen broker's live round (its entrypoint split is not built), and every item below.

## Live checks after the next build

The writers could not run these. Run them on the next build, before calling this review done.

**Rebuild and recreate.**
- Rebuild the desktop and tools images, then **recreate every desktop**. Desktop state is fixed at creation; these changes reach a desktop only then: no-new-privileges, the setuid and setgid strip, per-desktop caches, new host session ids, the tools populator, `-nolisten tcp`, no `xhost +`, the browser-agent removal.
- On the first start, remove this instance's own old-tagged helpers and hops by hand ([running.md](running.md), "Upgrading to the random instance tag").
- The practice reset on the new image: `DECURIX_PRACTICE_IMAGE=decurix-desktop:basic go test ./internal/desktop -run TestPracticeResetLive -count=1 -v` in the Decurix distro ([running.md](running.md)). It skips without the variable.

**Security and the gate.**
- `sh bench/security/boundary.sh --layer` on the new image (as root in the Decurix distro).
- `bench/security/gate_flow.py`: the keep-it confirmation through the gate.
- The live X11 TCP test on a running desktop: `DECURIX_X11_LIVE_DESKTOP=desktop_<id> go test ./internal/desktop -run XServerHasNoTCPLive -v`.
- `TestHomeCopiesCannotReachPastTheWorkerLive` and `TestImageHasNoSetuidFilesLive`.
- Check that Chromium's namespace sandbox works with no-new-privileges and no setuid files.

**Agent benches.**
- `python3 bench/skills/e2e.py` on a hosted brain and on the local brain (the CPU host), and with the Claude harness (the MCP door). Triage now sends questions to the operator, and build requests go to the operator.
- The long-requests bench (`planAgreed` from both sides, amendments, declined stages).
- `python3 bench/backoffice/e2e.py <desktop> --only wf3,wf4,wf5,wf6` (G-16).
- `python3 bench/recordings/e2e.py` (replay, identity, healing, read-back).
- `bench/workflows/transfer.py` (each run has its own task id).
- The GST bench (review points changed: T-03, T-18, R-2).
- `python3 bench/screen/e2e.py` (G-32).
- `python3 bench/gui/run.py --engine oracle --container desktop_<id>` first (it creates the `benchpanel` user), then the engines (G-15 moved the answer key).
- The handoff bench (receivers save to `~/handoffs/<id>/out/`).
- **O-9:** bench commit `ab8cac8` on the hosted benches before keeping it.
- The replay gate with `DECURIX_REQUIRE_FIXTURES=1` on the machine that has the private corpus.

**Brainiac.**
- `brainiac eval` on each box. Old `evals.json` scores have no decided counts and no `routed:` scores, so no field stays earned until it is measured again.
- The llama-under-sandbox run on Linux: `cd brainiac && BRAINIAC_LIVE_LLAMA=<llama-server> BRAINIAC_LIVE_GGUF=<small gguf> go test ./internal/engine -run Live` (`TestLiveServerRunsWithNoNetworkAndNoSecrets`), to confirm the minimal `/dev` and partial `/proc` are enough. Include a GPU build on a box that has a GPU (WSL2 `/dev/dxg` or native NVIDIA).
- On Windows, stop `serve` before the first `brainiac pull` after this change: it installs the llama.cpp build again once to make its install record, and Windows will not remove a running exe.

**Windows and the phone.**
- B-09 and the "Public link" switch in the Windows app; the Private-profile LAN firewall rule.
- Phone setup over Wireless debugging on a real phone, including a Wi-Fi change.

## Phase 2: soul and run record

What the SOUL lane left for later (PLAN-P2 §9; schema and limits in [trajectory.md](trajectory.md)).

- **`ledger_delta` is `[]` on every step.** The ledger does not name the recording event of its save. To fill it,
  `computer/records.py` would stamp the save's record id (the event's `id`) on the record it marks saved.
- **A local brain's calls have no bodies.** Soul's trace keeps their model, prompt hash and usage only. Bodies need a
  recording proxy in front of Ollama or soul writing them; measure the size and the `num_ctx`/prefix effect first.
- **The bundle limit is a fixed 64 MB a run** (`runs.DefaultLimit`); past it frames go, oldest first, and are named
  in `integrity.missing`. Make it a setting once real runs show their sizes.
- **Done: bundles of practice and eval desktops are pulled** through `desktop.PracticeHome` before each reset: the eval
  runner's `pullBundle` and Simulate's `practice.Service.Bundles`. A failed pull is a problem on the finding, never a
  grade. `labels.eval` is filled from the eval row (`eval.Store.EvalLabel`).
- **Done: soul's traces are pruned** by `retention.prune_traces` at each run's end: only an exported, not re-held run's
  `.jsonl`/`.rules`, older than 3 days. Open: soul counts a pruned trace's old size against its 32 MB cap until it
  restarts.
- **`request.files` and `worker.image` are empty** in v1: neither is kept per execution. `worker.rules_sha` is soul's
  operator rules hash (`~/.decurix/ai/traces/<task>.rules`, copied into `run.json`); it is empty for a local brain.
- **Done: Build reads an execution's verdict, origin, skill and resolved model from the task records' own fields**
  (CP-4/CP-5 landed, 7f2c26d, 22e3c3e; `TestViewReadsTheExecutionsFields`, 515091f). An execution with no bundle
  builds through `tasks.Store.TaskOfExecution` (7f2c26d).
- **Live check (L0):** `/api/llm/calls/stats` shows no `untagged` from `caller=soul`; a task's
  `~/.decurix/ai/traces/<task>.jsonl` exists, and its call lines' turns are the turns on that task's `llm_calls` rows.

## Phase 2: Python agent

What the PY lane left for later (lane-PY.md; PLAN-P2 §9).

- **Done: legacy verdicts.** `tool_task.perform` sends `tool_task._verdict` (by `verify`: exit code and file facts in
  code, the judge by model) and `workflow_task._report` sends `workflow_task._run_verdict` (by `workflow`: stage file
  checks in code, the judgement against the request not made, so never done). `verdict.derive` never makes a done
  (checked=True alone goes out unchecked), `test_verdict.NoReportReliesOnADerivedDone` keeps the list empty, and
  `legacy` is no gate on either side (`verdict.BY`, `tasks.verdictBy`); stored legacy rows are still read.
- **Python's model calls, still to move behind soul** (plan rule 3; every one is tagged `py.*` or `legacy.*`, so the
  proxy's stats show the share): `py.judge`, `py.judge.spot`, `py.skill.ask`, `py.skill.calls_off`, `py.stage`,
  `py.binding.fill`, `py.approval.read`, `py.approval.choice`, `py.ground`, `py.escalation.ground`, `.judge`,
  `.detour`, `.point`, `py.mimic`, `py.skill_engine.fits`, `py.kept_facts`, `py.playbook`, `py.study`,
  `py.tool.args`. `agent/tests/test_purpose_tags.py` holds the set (`legacy.azure.turn` went with the azure harness, O-4).
- **Spot judgement of trusted replays** is built (`skill_task._spot`, purpose `py.judge.spot`; a `not_done` records a
  failure) and samples 1 in 5 task ids by default (`DECURIX_SPOT_JUDGE_EVERY`; 0 never, 1 always). Open: measure the
  judge's cost and its catch rate on the L3 matrix before changing it; its eval case waits for `learn-v1`.
- **Dates.** With no order known (no `DECURIX_DATE_ORDER` on the control plane, no DD/MM marker, no unambiguous date
  in the request or its files' text layer), an ambiguous digit date never matches an ISO one: the save waits and asks
  which order. Set `DECURIX_DATE_ORDER` for each org (read in `cmd/decurix/main.go`). A digit date against the known
  order (04/13/2026 under `day`) is kind `unreadable`, and `values.compare` answers undecided (`date_order:a|b`), so the
  save asks; with no order known, "p2 > 12 is month-first".
- **What the worker could write itself** (O-1): the run folder's `context.json` (the date order, the reply's
  judgement id), `LABELLED` and the ledger are all in the worker's user. A worker that flipped its own date order
  could pass a wrong date. Moving them under the gate user is part of O-1.
- **Live checks:** L0 (every recording event of a hosted task carries `turn`, actions carry `obs`, `run.json` lists
  the parts; every Python call row has a `py.*` purpose), L5 (retention), L1 (no secret in any kept file:
  `.done`, `.request`, `active-task.json`; soul gets `secrets` on the job, H-PY-4).

## Phase 2: fixture corpus lost

`C:\Users\Administrator\decurix-tests\fixtures` (and the round folders beside it) are gone (W0.1). These tests skip,
counted, for what is no longer on this machine. `DECURIX_REQUIRE_FIXTURES=1` is not set on this host: every one would
fail on an absent corpus, which proves nothing. "Synthetic" says whether a fictional fixture could stand in.

| Test | Live regression it pinned | Synthetic? |
|---|---|---|
| `test_coverage_condition.ConditionalCoverage` (4 tests) | CW, round 5 P6: a stage that selects by a condition (refunds over Rs 5,000) was checked against every record of the tracker | Yes: a small tracker and refund list with a threshold reproduce it |
| `test_cross_check_corpus.TheCorpus.test_every_case` | Every live line the cross-check got wrong or right (dates, lakh/crore, rounding, signs, ids, percentages) | Partly: the forms can be written fresh; the corpus's value was that each line was a real miss |
| `test_draft_facts.TheLiveDrafts` | DE, round 5 P4: welcome drafts naming managers and dates no source held | Yes: a tracker and drafts with planted wrong facts |
| `test_duplicate_entry.AnotherRecordsValues.test_the_live_saves`, `TheSaveGuard.test_the_live_record` | CX/CY, round 5 P4: a joiner saved with another joiner's department, then entered twice | Yes: the synthetic table tests beside them already cover the rule; the live event log only replays it |
| `test_files_open.OfficeFilesOpen.test_the_live_spec_round_trips_to_a_workbook_that_opens` | Series 14 P3: script-written workbooks that no app could open | Yes: a spec of the same shape |
| `test_kept_sheets.AdditionsAreNoChange` (4), `AfterARecordWasRemoved` (3), `CellsByMeaning` (1) | CO, CP2, round 5 P1/P2: a later stage's added column or a cancelled bill read as changing earlier stages' sheets; LibreOffice's re-save | Yes for the first two (small workbooks); the round trip needs a LibreOffice re-save, which a desktop image can make again |
| `test_no_suite_terms.NoSuiteTerms` | The harness names no suite (needs the private `leak_terms.txt`) | No: the terms list is the private suites' own words; rebuild it from `evals/` ids (the EVAL lane's boundary test covers the dev suites) |
| `test_order_totals.OrderTotals.test_the_live_sheet_has_no_finding` | CK, round 5 P2: an order's total compared with its line rows | Yes |
| `test_payees.TheRoundsOwnFiles` | Round 5 P3: a payment run paid to a lookalike "change of bank account" email | Yes: a vendor master, bills and one lookalike email |
| `test_replay_gate.ReplayGate` | The replay gate: every code check run on saved right runs (CHECK_MATURITY) | No: its point is real right runs. Rebuild from the core-v1 oracle arm's runs once they are kept (R9 bundles) |
| `test_requests_corpus.LiveRequests` | CL, round 5 P1: each deliverable bound to its own app | Partly: the six requests can be rewritten fictionally; they are no longer the live ones |
| `test_stage_app.StageApp` (3) | CL: a named app binds only to its deliverable | Yes: the request text is the fixture |
| `test_stage_facts` (5 classes) | BG, CF, CC, round 5 P1/P6: a re-check losing a flag, a computed refund with no working, an auto-reply with another email's order | Yes: small registers and inboxes |
| `test_stage_question_corpus.TheBounceCap`, `TheCorpus` | A stage's question routing (person_keeps, route_question) over every live question | Partly, as the cross-check corpus |
| `test_stated_amounts.TheLiveInbox` | CI, round 5 P6: amounts stated in a customer's own email called "no working" | Yes: a few .eml files |
| `test_summary_totals.TheLiveWorkbooks` (2) | DC, round 5 P1: a GSTR-3B sheet's ITC of 0 against its own register | Yes: two small workbooks (tax figures are fictional) |
| `test_system_entry_readback.TheLiveReadBack` (2) | DA, round 5 P2: orders the portal held read as not shown | Yes: an event log of the same shape |
| `test_table_quality.TheLiveP5Exports` | BK, round 5 P5: a sales export repeating a week, inflating every total | Yes: CSVs with one repeated week |
| `test_working_factors.TheLiveExceptions`, `TheLiveRegister` (3) | CH/CQ, round 5 P3: the working check took the first quantity x the first rate | Yes: registers with GST on top and partial receipts |

None holds customer data that must be recreated: every fixture was a fictional company's run. Rebuild each on a
synthetic fixture as its area is next touched (PLAN-P2 §9), never in bulk.

## Phase 2: control plane

- **Done: untagged model calls are refused by default** (D3). The live round of 2026-11-08 (a broker-mode desktop with a
  hosted brain: tasks, a demonstration, teaching, a replay) showed 58 calls, 0 untagged and 0 invalid (soul 41, the agent
  17). `DECURIX_LLM_REQUIRE_PURPOSE=0` lets them through for desktops made before.
- **The delete flush can time out** (90 s). A run not pulled by then is named in the log ("... that run's recording,
  ledger and notes are lost") and the delete goes on: the one case where a label can be lost. Measure live how long a
  flush takes; consider refusing the delete instead while unpulled runs remain.
- **`resolved_model` for a local brain** is `"<engine>:<model the desktop said>"`, not a deployment the proxy saw
  (Ollama calls never pass it). Local-brain call bodies are a later phase (PLAN-P2 §9).
- **Pass rates** (`/api/tasks/pass-rates`) count every ended attempt under its own skill version x model x origin
  class (production, practice, eval); the headline is the first-attempt rate, `checked_independent` counts only code,
  ledger or person bases, and a person's verdict joins only the attempt it judged. Wilson bounds are the reader's.
- **The trust statement** (what a recording's program can and cannot do) is in [gate-boundary.md](gate-boundary.md),
  "What the control plane tells the gate".
- **The `decurix.gate` label's "0"** is a branch nothing takes today: no code makes a desktop without a gate. It stays
  as the one explicit "no" HasGate honours. A desktop with no label (made before 718220f) is gated; if its gate does
  not answer, the person is told to recreate it.
- **Teach, replay and run-skill give the gate only their request line as its copy** (a `/replay ...` or `/skill ...`
  line, withheld), plus their secret parameters as values, before the agent has anything; a gate that cannot be told
  stops them. A replay's secret parameters are those its recording's program marks secret (structure); when the
  program cannot be read, every value given is held. The line names no site, so the gate asks about every password a taught skill types: fail closed, but a
  taught skill asks each time. Proposal: the control plane sends the gate the taught skill's named sites, signed the
  same way as `/request` (its own label, the task id, the time, a one-use nonce, the gate key), from the skill as the
  control plane holds it, never from the worker's files. What makes any door that leaves out the copy safe is the
  gate binding its copy to its own task (H-CP-5): done in 1890a94, a copy of another task names nothing.
- **`withhold.Reply` takes a username for a secret.** "sure, the password is Fern42Kite and the user is mira.k" withholds
  `mira.k` too (any word with a digit or a mark), and since round 2 that value is held for 24 h by the call recorder
  and given to the gate, so the username is withheld from that desktop's calls and the gate's questions. Narrow the
  rule by the question's structure (the field it asks for), never by a word list.
- **A secret reply whose question changes between Plan and commit** (a faster reply resumed the task and it asked
  again) can be told "secret" by one intervention and stored by another (C14.1 decides before delivery, the store at
  commit). The window is the length of a delivery. The late remember in `recordDelivered` feeds the call recorder
  but not the gate, so for that window the gate's question may also lack the value.
- **A gated reply without a sign-in** is authored by "someone with the access code" (`access-code`, no roles): rule 5
  holds in that the input says who it could be, not who it was. Nothing is kept as a lesson or correction from such
  an input (`/api/desktops/learned` answers 409). **Done:** once the access store holds a user, an unsigned reply on
  the gated listener (chat, Queries, the recording door) is refused with 401 "Sign in to reply" (503 when the store
  cannot be read); with no users the access-code holder may still reply.
- **Done: purging a leaked secret** (decided 2026-11-07: a secret is not a label). `internal/purge`, `POST /api/privacy/purge`
  (primary listener, humansOnly, owner role), `decurix --purge-secret` and a control on the Tasks page rewrite every
  copy to `[purged]` in place, keeping blob names and lengths so rows and prefixes still join, and record a `purge`
  judgement. Not reached: backups already downloaded, gzip-compressed blobs, image pixels, anything still on a
  desktop's disk.
- **No control-plane page links to soul at `/agent/<id>/`.** The gated listener forwards only its health; the primary
  one forwards all of soul. If soul's own page is to be shown on a phone, add each route it needs to
  `gatedAgentRoutes` with a reason, never `/api/chat` (it would drive the worker with no task record or author).
- **The tools image is re-checked by its ID**, not its sentinel: a tools image rebuilt with the same tree but a new
  ID is applied again (a cheap no-op copy, since the tools CMD compares sentinels).
- **Done: a checked-only final with no verdict fails closed.** `tasks.checkedFrom` stores checked=True with no
  verdict as not checked, logged as a disagreement.
- **A judgement id is chosen before delivery** (C6). If delivery errs after the desktop accepted the request, the
  desktop holds an id never committed, and what it keeps from that reply answers 409 (fails closed).
- **Done: tools not ready.** A failed populate sets `Manager.ToolsNotReady()` (`volume`, `start` or `copy`); `/api/docker/status` returns `tools_not_ready`, and the dashboard shows a banner until a fill succeeds.

## Phase 2: soul and run record, review round 3

- **Existing desktops keep values written before the C14 fixes (review M4).** L2 lines (`~/.decurix/ai/.memory/
  l2.jsonl`) written before FIX-SOUL-1 hold raw values, and after a soul restart `/api/memory`'s second wall has only
  the shape filter for them. Every desktop made before this build must be recreated (or its `l2.jsonl` cleared).
  LIVE-1's `live1` (4 copies) is deleted after the L1 recheck.
- **Full command outputs in `/tmp/thebot-outputs-*`** (`internal/skills/screen_digest.go`, `keepOutput`, 0600) are
  kept as printed, not withheld: the model reads them back. They never leave the desktop, but they are not withheld.
- **Done: a value written in shell-escaped form** (`'\''`, backslashes) is matched by soul's withholding of the job's
  secret values, in any command (99d7962, `TestAShellQuotedValueLeavesTheStepLineWithheld`).
- **The coding harness's log lines** (`internal/coding`, standalone soul only) cut commands and outputs without
  withholding; it is off inside Decurix (O-8).
- **`GuardRefused`** is never written by soul: the trace's guard is worked out again from `AssessCommand` after the
  command runs, not recorded from the decision taken.
- **A trace at its cap** gets one more `truncated` line after each soul restart.

## Phase 2: evals

What the EVAL lane (`internal/eval`, `evals/`, docs/evals.md) left for later:

- **Concurrency 2.** The runner is sequential, one practice desktop at a time (the broker serves one run; the host has
  room for one desktop). Two arms at once need a broker run per desktop and measured headroom of 5 GB or more; it would
  halve L3. Key 3 matters only then.
- **E5: local brains.** Practice and evals refuse Ollama and Brainiac brains (`practice.ErrHostedOnly`); the broker would
  proxy an OpenAI-compatible local endpoint, and soul's trace is then the only record of a call.
- **Bundles from practice and eval desktops:** see "Phase 2: soul and run record" above.
- **More suites:** `docs-v1` (documents becoming records), `gst-v1` (a returns portal and a client chat mock, written by
  someone who does not tune the harness) and `learn-v1` (memory kept between cases; its runner mode is below).
- **Done: D7.** CLAUDE.md now says dev eval suites with fictional data live in `evals/` and the sealed held-out set stays
  outside the repo.
- **Calibration L2 needs the live stack.** Offline, `TestCoreSuiteLoads` passes the oracle on 10 of 12 core-v1 cases,
  and the nothing arm (false-done 12/12) and the ask arm on none. The offline oracle interprets the oracle's shell itself
  (curl posts become form posts; its read assertions are skipped), so the real lines are proved only at L2. `web-pay-approval` (the gate must stop `gui click Pay` with exit 3) and
  `calc-sum` (`office update` must recalculate so the SUM cell has a saved value) are proved only by
  `py -3 evals/run.py run --suite evals/suites/core-v1 --arm oracle --arm nothing --arm ask --repeats 1`.
- **The duplicate case still reads words.** `bill-already-entered` passes a final that names the bill and says it is
  already there by one of a few words, or the right question (the expert matches it to the fact that the bill was
  entered). A worker that says it in other words fails it; widen the case, never the grader library.
- **The GUI bench's approve-then-done tasks** (`web-pay-approved`, `web-delete-user`) are skipped by its oracle: a yes is
  the gate's, from a reply the control plane signed for an approval on a task it opened, and the oracle opens none.
  Engines cover them.
- **Learning curves (learn-v1) need a runner mode that keeps state.** By default every case starts from the run's
  snapshot (`ResetPractice` clears all of `~/.decurix`, the work folder, the browser profiles and `/tmp/decurix-*`). A
  suite that carries state between cases must say so explicitly. **Done:** suite `carry_state` and `evals/run.py
  --carry-state` must agree (else 400); the desktop is reset only before each repeat's first case, and the run row and
  the table's "State:" line say which mode ran.
- **Practice and eval desktops are reset by stopping the worker's processes and clearing its home's agent state**, then
  restarting ([evals.md](evals.md); `TestPracticeResetLive` passed on the real image, a9609b3). L2 should check `ls -A ~/.decurix` after a reset holds only the snapshot.
- **Done: `approval.site_known` no longer reads the worker's task file.** On a gated desktop it reads the gate's own
  signed copy of the request, for the action's task only (e343065, 1890a94; [gate-boundary.md](gate-boundary.md)).

## Phase 2: Python agent, false successes in eval run eb5fdd477d1eb6744 (round 9)

- **Presave's block of an undeclared field this task set is ADVISORY** (`records.UNDECLARED_FIELD_BLOCKS = False`).
  The final gate already refuses done over it (`records.gate_faults`). Make it blocking only after it passes the
  replay gate (`agent/tests/test_replay_gate.py`) and a clean live round.
- **An option the worker picked from an application's list is backed only by a source value:** a party resolved by
  identifier, or the option's text in the request, its files or the person's answers (`records.given_text`). An
  inference from the document, such as "Sales@18%" because the bill charges 18% GST, is now the person's to
  confirm. Watch the next run for extra questions on ledger and account picks.
- **The ledger basis needs the records this task's saves made to equal its declared, saved records**
  (`core.records_created`, `records.gate_faults`). A party record created during the task without a `gui record`
  declaration now holds done back too. Declare every record a task saves.
- **A declared key already shown on an application page this task read** (a page's words, not the source document)
  stops the declaration once and asks the person. A record deleted and entered again in the same task is refused as
  "saved in this task already". It has to be corrected in place, or the person removes it.
- **Superseded:** the task's `<task>.said` notes no longer back a pick (rounds 10 and 11); only the person's word as
  the control plane recorded it does (`person_record.answers`).

## Phase 2: Python agent, the person's word and what picks an option out (rounds 10 and 11)

- **Done: H-PY-18, desktop-scoped reads of the person's judgements.** The agent backs a pick with the person's word only
  as the control plane recorded it (`person_record.py`), through `GET /api/desktops/judgements?task=` and `?skill=` and
  `GET /api/desktops/learned?kind=field_answer` (`internal/httpapi/desktop_judgements.go`, `tasks.JudgementFieldAnswer`;
  d63f89b, a4ce49c). When the control plane cannot be read, a pick only the person's word backs is not backed (fail closed).
  - Not backing anything any more: `<task>.said`, the gone `field-answers.json`, and the skill file.
  - Org knowledge still counts.
- **Accepted residual: a word of the option in an unrelated phrase of the request.** "the general manager approved
  it" backs a "General" entry when no other offered entry has that word (`records.option_backed`). A file is held to
  the field's own label (`option_beside_label`), so a bill's "General terms" footer backs nothing. The request and
  the person's replies are not, since they rarely name a field.
- **Done: the module-order leak.** A secret reply left `state.reply_secret` set, so a later correction was kept by nothing; `state.begin` now clears it (and `reply_judgement`) for each task, and the teaching tests reset it.

## Phase 2: Python agent, review round 2

- **Comparators still outside `values.py`** (known divergences, each to move as its area is next touched):
  - `long_task._same_value` / `_scalar` (`long_task.py`, a kept cell's value): its own date logic reads `a > 12` as day
    first even under MM/DD, and compares numbers within their written precision (a deliberate tolerance for a cell and
    its computed value).
  - `provenance.fact_value` / `same_figure` (`provenance.py`): figures within the coarser precision, and an ambiguous
    date with no convention compared as written.
  - `stage_facts`: amounts as floats (`round(..., 2)`, `abs(...) <= 0.011`).
- **Done: every request and reply names its execution** (`execution_id`, H-PY-7: 0f82aad). Teach, replay and
  run-skill send it too (a498b96), and the agent keeps it on those doors as on `/request` (H-CP-10, cc96751).
- **A run open or paused for days is not held** before it has a label: `evidence._prune` can take its notes after 2
  days (review round 2, non-blocking 10). Hold an open task's run as well.
- **Done: the person's delete of a held run's recording** answers 409 `held: true` unless forced; the pages ask "This run is not saved to the run record yet; delete it anyway?" (`recording.held`).
- **The skill a run noted on its task** (`task_context.set_skill`) stays when the skill run hands the task to the
  general harness; the outcome then names a skill that did not drive its end.
- **The `code` basis of trust** now comes only from a comparison that could have failed (review round 4, R2): a
  figure a made workbook presents as a source's own, compared with what that source states (`sources.mismatches`),
  at least one compared and none different. Most file work names no such figure and earns no `code` basis, however
  clean its files read, so watch the L3 matrix for skills that can no longer earn trust by code. Owed: a check that
  recomputes a total from source data, which would count too.
- **Done: a person's answer about a field's unit is kept for that field.** A save that waits on a unit written on one side only asks its own numbered question ("Reply 1 if the field ... takes its figures in %, or 2 if it does not"); only the person's recorded reply backs it, in the task by its judgement and later by a learned `field_unit` row (`tasks.JudgementFieldUnit`, `unit_answers.py`), scoped to host + form + label. Open: a live check that the reply comes back with the question's text unchanged.
- **A date the save waits on** goes out as a question of its own at the engine's next question, in place of it
  (round 3, R3). An engine that reports instead ends the task unchecked, with the question in the report. Asked
  during a taught skill's lesson, the engine's lesson question is not put to the person, and the lesson gets the
  "not your question" note as its reply.
- **A unit on one side only** is compared by the field's label (`values.field_units`, round 3, R5): "18%" against a
  field labelled "Rate (%)" is one figure; with no such label it still waits on the person. Owed: an eval case that
  counts the questions unit fields cost (GST rates, quantities in Nos), in a future suite.
- **Done: EXPORTED carries LABELLED's two lines** (execution id, label number), so it is four lines, not three: the
  agent reads everything after the second line as the copy (round 3, R1). docs/trajectory.md, `internal/runs/puller.go`
  and its test say four lines.
- **Secrets, round 2 leftovers** (the security review of 2026-11-04):
  - The task's secret values as they are live in the task file's `.secrets` (tmpfs, 0600) until the task's final
    report; a container restart loses them (the interrupted task is then reported, not resumed). The word hashes stay
    desktop-wide.
  - Every write of desktop state now goes through `privacy.write_scrubbed` (round 3). An AST test forbids any other
    way of writing a file in the agent's package except a listed few: bytes, the person's own files, lock files, the
    secret stores, and the command-file transport. Stores whose values are compared later (the record ledger, a
    stage's ledger and cells, context.json) withhold the literal secrets only, with no shape matching.
  - A planned run's ledger and a workflow checkpoint hold the request withheld on disk. The request as it was, and
    a workflow's inputs, are kept in the task's resume copy (tmpfs, beside its secret values, let go at the final
    report), so an agent restart resumes with the value (round 4). Only a container restart loses it, and that
    already reports the task as interrupted. `core.act` never types a `[withheld]` mark: it says to ask the person.
  - A wholly marked message keeps three kinds of value (rounds 4 to 6):
    - what its structure names, withheld wherever it stands;
    - the message whole, always, so every kept and emitted copy of it is withheld (round 6, R5-B1). The task's own
      checks read the request from a raw copy in the task's secret store (`task_context.current_request`: tmpfs,
      0600, let go at the final report);
    - each word shaped like a value (4+ characters with a digit or a mark), withheld as a whole token only, and only
      when it is not data by structure: a number, amount, date or time, an e-mail address, URL or host name, a
      contraction, or a word the files it brings also hold.

    A step line never quotes what `gui type` types (the whole rest of the line).
    Residual costs:
    - An all-letter secret word of a marked message, typed alone in the task's own new output, is not withheld. It
      can surface in:
      - the model's own replies;
      - step lines of commands other than `gui type` (`sshpass -p`, `curl -u`, `echo ... |`);
      - a non-password field's "Output" line;
      - the recorder's later model turns.

      The long-term fix is O-1's input broker, not more rules.
    - No end-of-task facts are kept from a task any message of which was marked or withheld as secret (round 7,
      R6-B1). For other tasks the facts thread checks every fact against the task's values captured when it starts,
      and the values are let go only after it ends.
    - A value-shaped code that is data but has no structure the rules read, like a period code `Q3-2024` in the
      message and in no input file, is withheld as a whole token in that task's later lines.
  - Not done from REVIEW-PY-A-2's nits: `core._shown_by_app` still takes the application's value from any observation,
    not only one before the worker's first input on that form; `skill_task._correct` still reads the reply's
    judgement and secret flag from globals.
  - `resend_learned` can post a line twice when two threads read it at once (no client id for the control plane to
    dedupe on).
  - The gate's nonces are in memory: after a gate restart a signed body still within its 120 s window replays (the
    worker cannot capture the traffic: NET_RAW is dropped). Said in gate-boundary.md.
  - Done: soul's step text (`Running: gui type e3 "<value>"`) withholds a job's secret values in every written form
    the agent's `approval.written_forms` gives (H-PY-9, 99d7962).
