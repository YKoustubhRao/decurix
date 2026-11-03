# Follow-ups after the review of 2026-11-03

The deep review of 2026-11-03 fixed about 300 findings on the `review-fixes` branch. This file lists what it left open: the owner's decisions, the deferred items, and the live checks the writers could not run (no containers, no app builds, no paid APIs). Each line carries its review ID. Security items are written by input class, never as attack strings.

## Owner decisions

| ID | Decision | State |
|---|---|---|
| O-1 | Input broker and OS boundary: direct input around the gate (XTEST, VNC, CDP, AT-SPI, host-app and phone viewers), a gate-owned evidence store, `checked` set by the worker, the script sandbox's filesystem | **Open, do next.** The word guards are frozen. Order: the evidence store under the gate user first (it makes "observed" real), then the input broker ([gate-boundary.md](gate-boundary.md), "Next: the input broker"). Also: a per-task `$HOME` snapshot, read-only binds outside `~/work`, an egress proxy |
| O-1.a | TheBot command guard: five structural fixes before the final freeze | **Open, first, ahead of the broker.** Table below |
| O-2 | Per-desktop cache volume (B-02) | Done as recommended: a cache volume per desktop, the model cache shared read-only |
| O-3 | Brainiac appliance: model user, AppArmor, signed catalog, uploader list | **Open; required before any box ships.** Table below |
| O-4 | Legacy harnesses (Azure, `task_runner`) | This loop: their dangerous-command yes goes through the gate's `kind: "command"` or fails closed. **Open:** delete them |
| O-5 | Public tunnel off by default (B-08) | Done: off by default, one switch in the Mobile menu, `DECURIX_PUBLIC_LINK=1` for the scripts. Veto if you disagree |
| O-6 | A typed "yes" in the desktop chat as an approval (W-07) | Kept, with `seen_at` frozen at the first keystroke and buttons bound to the note id. **Open:** buttons-only is safer if the UX is acceptable |
| O-7 | Org knowledge and skill writes from the gated listener (A-08) | Done: host-operator only |
| O-8 | Acquired skills, proven fixes and the coding harness inside Decurix (T-07, T-08, T-21) | Done: off unless `THEBOT_STANDALONE=1`. Veto keeps them |
| O-9 | taskRules audit to principles only (T-18) | Staged as commit `ab8cac8`. **Open:** bench it on hosted brains before keeping it |
| O-10 | God-file splits (`core.py`, `long_task.py`, `skill_task.py`, `operator.go`, `httpapi`) and the check-registry rewrite | **Open.** One at a time, each behind the replay gate |
| N-W4-8 | A Landlock file policy for Brainiac's headless browser (profile folder plus the system read set, network kept) | **Open.** Prove it with a live Linux run before Render may be turned on on an appliance |
| N-W4-8 note | Render stays off the automatic path everywhere, off by default on an appliance, behind the guard proxy, page read bounded | Done |

### O-1.a: the guard's last structural fixes

**Who:** a human-authored commit, or a fresh session whose brief contains no command strings. Do not have a model analyse evasion shapes; a safety classifier stopped two agents three times on this. **Paths:** `agent/thebot/internal/safety/` and the callers of `AssessCommand`. Do the two with no command shape first.

| # | Input class the guard mishandles | Structural change | Test, built mechanically from the guard's own flagged examples |
|---|---|---|---|
| O-1.a.3 | (no shape) The folder a command runs in | Pass the real working folder from the operator, terminal and coding callers into `AssessCommand`, and resolve paths against it instead of assuming `~/work` | A relative recursive delete with a working folder of `$HOME` is flagged |
| O-1.a.4 | (no shape) A URL whose authority carries a user-info part | The fetch tools' "local only" exemption judges each target by `net/url`'s `Hostname()`, not a regex over the raw text | An existing remote target, rewritten with a user-info part, is not exempted |
| O-1.a.1 | A program named by its absolute path, or behind a wrapper program | Every program rule identifies the program by the base name of the command word, after skipping the wrappers `wipeConcern` already models (reuse its parser) | Every existing flagged example, with its program given by absolute path or behind a wrapper, is still flagged |
| O-1.a.2 | Global options placed before a subcommand | The version-control and publish rules accept the folder and config options before the subcommand | Every existing flagged version-control or publish example, with such an option inserted, is still flagged |
| O-1.a.5 | An operand decided at run time (a variable or a command substitution) | A recursive delete's operand, or a fetch tool's target, that is decided at run time waits for a person | An existing flagged recursive delete with its path replaced by a variable is flagged |

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
- **C-25:** the practice broker trusts `desktop_id` in the body. Fix it with the O-1 evidence store, identifying callers by container address from the practice network's map.
- **A-07:** no `knownHost` list on the gated listener (accepted: the gated listener is reached under changing host names; the session cookie and same-origin rule answer rebinding).
- **B-37:** cron is UTC only. A per-schedule zone needs a Migrate step and UI.
- **B-29:** a restart loop for the Windows tunnel (`Lan-Tunnel.ps1`); the tunnel is opt-in now.
- **B-10:** the native-Ollama admin API is reachable from desktops; an allowlist proxy for desktops is not built.
- **W-20 rest:** in-flight guards for `loadNotes`/`pollResourceAlerts`, `/api/desktops/memory` unbounded, the `rfb.js` hash.
- **RW1-G-04 rest:** starting an existing desktop does not wait for a tools refresh in progress.
- **Orphan home volume:** after a Delete whose volume removal failed, a new desktop of that name is refused with "delete that volume", and no UI does that.
- **RW1-D-01:** a container the daemon made while `ContainerCreate` returned an error is leaked until a later Delete of that name; at most a log line.
- **`internal/ollama/ollama.go`:** the default model is still `devstral-small-2:24b` (its comment is corrected). Owner's call: keep it or restore the 3B default.
- **Mail jobs:** the page may hide "Run again" on a skipped or failed job with no request (the server already answers 409).
- **No-echo walker limits** (RW1-G-06 is done): an error variable not named like one, or a helper that returns the text, still gets past it.
- **Two control planes on one data folder** are unsupported; after a crash, two starting at once could each remove the other's fresh `instance-id`.
- **P3s not done:** B-02 (a panic path leaves only `last_error`), C-06 (a live test positive control; walk all packages), C-07 (a main-level Docker-down test; the watcher is tested), F-05 (owner's call), A-02 to A-06 (Electron details), F-08, F-10, F-11 (notes).

### Python agent (W2)

- **O-10:** the check-registry rewrite (step 1, the `_checked` wrapper, is done).
- **E-41:** the "PAYMENT TO " string branch only adds a prompt hint and a warning; make it a typed field when `FileFacts` reaches `code_problems`.
- **E-42:** `claims_done` stays (its only effect is a second judge call, whose "cannot" ends "not checked"; widening its regex would be a word list).
- **E-43:** run-folder retention is the owner's call (run folders hold the person's outputs; locks, temp files and host copies are pruned).
- **E-30:** "only other parties' messages": nothing structural marks another party's message, so it is not chosen by words.
- **D-32** (partly done), **D-34** (harmless for allow decisions).
- **D-36 rest:** OpenHands import URLs; `skill_engine`'s no-model right-system check; the grounder's one-probe disable; `ocr._pgm` speed. (`bind_sources`' substring match is done, `98bb9d8`.)
- **Replay corpus breadth:** a second synthetic run with an Office output and a payments table, so `system_entry`, `payee_approval` and `files_open` reach their cases, not only get called. Or word the assertion as "called".
- **The cancelled-row test** checks maturity only, not `stage_code_findings` end to end.
- **The X-6 test** calls `_pause` directly; it should drive `_prepare`/`_execute`.
- **The ledger:** withholding runs on every save rather than once (fails closed); any `OSError`, even a transient one, moves a good ledger aside for good (prefer only a parse error).
- **A stale "filled from" note** could add a block for a reused output path (fc0849c).
- **An all-capitals request** hides the client's own facts (fails closed).
- **An IBAN** with an unknown country and a valid mod-97 is ignored; make it "not checked".
- **A second note on the same value** overwrites `OTHER_SIGN`.
- **`false_sums`:** "Rows 2 - 10 = 9." now reads as a wrong sum.
- **X-6 for TheBot:** an optional structured secret marker on HELP.
- **The legacy harnesses:** delete (O-4).

### TheBot (W3)

- **O-1.a** (above).
- **N-W3-3:** `asksAppNamesRe` suppresses a HELP to the person once, by words. It fails toward a delay, never a false success; move it into T-20's gate table as a fact.
- **T-19:** a recall score floor needs nomic's query/document prefixes (re-embedding the stores) or a judge.
- **T-27:** the embedded UI is the owner's decision.
- **A planted row in `l1.db`** (worker-writable) only shows "you approved this on <date>" in a standalone coding question. Word it as "a record on this machine says ...".

### Brainiac and bench (W4)

- **G-11:** carrying unchanged units into the new engine. The right shape is one Residency per Server, outliving engines.
- **G-17:** re-derive the workflow bench's results from artifacts (the name now says what is checked).
- **G-09:** hash-locked requirements (O-3.5).
- **FIX-BR-1:** a managed (non-system) python for the worker. `isolate.ForProgram` (`brainiac/internal/isolate/elflibs.go`, called from `brainiac/internal/engine/engine.go` `New`) grants its binary and the shared libraries it links, but not its standard library (`../lib/python3.x`), so it may not start in the mount view. A system python is unaffected (`/usr` is read). Check it live, and grant that tree read-only if it fails.

## Live checks after the next build

The writers could not run these. Run them on the next build, before calling this review done.

**Rebuild and recreate.**
- Rebuild the desktop and tools images, then **recreate every desktop**. Desktop state is fixed at creation; these changes reach a desktop only then: no-new-privileges, the setuid and setgid strip, per-desktop caches, new host session ids, the tools populator, `-nolisten tcp`, no `xhost +`, the browser-agent removal.
- On the first start, remove this instance's own old-tagged helpers and hops by hand ([running.md](running.md), "Upgrading to the random instance tag").

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
- **Bundles of practice and eval desktops are not pulled yet.** They have no home volume to read after a reset;
  collect them by exec tar before the reset.
- **Soul's traces are never pruned on the desktop** (`~/.decurix/ai/traces/`, at most 32 MB a task, any number of
  tasks). Prune an exported one by the W0.8 rule (`EXPORTED` present).
- **`request.files` and `worker.image` are empty** in v1: neither is kept per execution.
- **Build reads an execution's verdict, origin, skill and resolved model by their JSON names** until CP-4/CP-5 land;
  then a test with the real fields. An execution with no bundle builds only once `tasks.Store` can name its task
  (`TaskOfExecution`).
- **Live check (L0):** `/api/llm/calls/stats` shows no `untagged` from `caller=soul`; a task's
  `~/.decurix/ai/traces/<task>.jsonl` exists, and its call lines' turns are the turns on that task's `llm_calls` rows.

## Phase 2: Python agent

What the PY lane left for later (lane-PY.md; PLAN-P2 §9).

- **Legacy verdicts.** Two paths still send `checked=True` with no verdict of their own and so get a `legacy` done
  (no checks, no basis; it never counts towards trust): `tool_task.perform` (a learned tool's run) and
  `workflow_task._report` (a workflow stage's report). `agent/tests/test_verdict.py` (`LEGACY`) lists them; the list
  may only shrink. The control plane must accept `by: "legacy"` with no checks (handoff H-PY-1).
- **Python's model calls, still to move behind soul** (plan rule 3; every one is tagged `py.*` or `legacy.*`, so the
  proxy's stats show the share): `py.judge`, `py.judge.spot`, `py.skill.ask`, `py.skill.calls_off`, `py.stage`,
  `py.binding.fill`, `py.approval.read`, `py.approval.choice`, `py.ground`, `py.escalation.ground`, `.judge`,
  `.detour`, `.point`, `py.mimic`, `py.skill_engine.fits`, `py.kept_facts`, `py.playbook`, `py.study`,
  `py.tool.args`, `legacy.azure.turn`. `agent/tests/test_purpose_tags.py` holds the set.
- **Spot judgement of trusted replays** samples 1 in 5 task ids by default (`DECURIX_SPOT_JUDGE_EVERY`; 0 never, 1
  always). Measure the judge's cost and its catch rate on the L3 matrix before changing it; its eval case waits for
  `learn-v1`.
- **Dates.** With no order known (no `DECURIX_DATE_ORDER` on the control plane, no DD/MM marker, no unambiguous date
  in the request or its files' text layer), an ambiguous digit date never matches an ISO one: the save waits and asks
  which order. Set `DECURIX_DATE_ORDER` for each org. A digit date against the known order (04/13/2026 under `day`) is
  no date, by the lane's example; the lane's other sentence ("p2 > 12 is month-first, whatever the order") was read
  as applying when no order is known.
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

- **Untagged model calls are flagged, not refused** (D3). `DECURIX_LLM_REQUIRE_PURPOSE=1` refuses them; make it the
  default only after a clean live round shows 0 `untagged` on `/api/llm/calls/stats` (L0, L3).
- **The delete flush can time out** (90 s). A run not pulled by then is named in the log ("... that run's recording,
  ledger and notes are lost") and the delete goes on: the one case where a label can be lost. Measure live how long a
  flush takes; consider refusing the delete instead while unpulled runs remain.
- **`resolved_model` for a local brain** is `"<engine>:<model the desktop said>"`, not a deployment the proxy saw
  (Ollama calls never pass it). Local-brain call bodies are a later phase (PLAN-P2 §9).
- **Pass rates** (`/api/tasks/pass-rates`) count every ended attempt under its own skill version x model x origin
  class (production, practice, eval); the headline is the first-attempt rate, `checked_independent` counts only code,
  ledger or person bases, and a person's verdict joins only the attempt it judged. Wilson bounds are the reader's.
- **What a recording's program can and cannot do (the trust statement).** The program (its `params[].secret`) can only *add* withholding: a parameter it does not list, or a program that cannot be read, is held. A worker that under-flags a parameter leaks only what it already holds. The call recorder's withholding and the gate's wording of its questions are walls for an honest worker; against a dishonest one, what holds is the gate's site check (from its own signed copy of the request) and the yes the gate alone makes from a signed reply, neither of which reads the program.
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
  gate binding its copy to its own task (H-CP-5, PY); until then the gate may read an earlier task's copy.
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
  an input (`/api/desktops/learned` answers 409). **Trigger:** when the first user account is created in the access
  store, the gated listener must require a sign-in for replies.
- **Open owner decision: purging a leaked secret from the never-delete stores** (the blob store, `llm_calls`,
  `judgements`, run bundles). Rule 4 protects labels, and a secret is not a label. Recommendation: a purge-by-value
  tool (primary listener, humansOnly) that replaces each blob holding the value with a tombstone that keeps the hash
  (so rows and prefixes still join), and records a judgement of who purged it and why.
- **No control-plane page links to soul at `/agent/<id>/`.** The gated listener forwards only its health; the primary
  one forwards all of soul. If soul's own page is to be shown on a phone, add each route it needs to
  `gatedAgentRoutes` with a reason, never `/api/chat` (it would drive the worker with no task record or author).
- **The tools image is re-checked by its ID**, not its sentinel: a tools image rebuilt with the same tree but a new
  ID is applied again (a cheap no-op copy, since the tools CMD compares sentinels).

## Phase 2: soul and run record, review round 3

- **Existing desktops keep values written before the C14 fixes (review M4).** L2 lines (`~/.decurix/ai/.memory/
  l2.jsonl`) written before FIX-SOUL-1 hold raw values, and after a soul restart `/api/memory`'s second wall has only
  the shape filter for them. Every desktop made before this build must be recreated (or its `l2.jsonl` cleared).
  LIVE-1's `live1` (4 copies) is deleted after the L1 recheck.
- **Full command outputs in `/tmp/thebot-outputs-*`** (`internal/skills/screen_digest.go`, `keepOutput`, 0600) are
  kept as printed, not withheld: the model reads them back. They never leave the desktop, but they are not withheld.
- **A value written in shell-escaped form** (`'\''`, backslashes) is not matched by the withholding. The stdin rule
  covers `gui type`; other commands are not covered.
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
- **Bundles from practice and eval desktops.** They have no home volume, so R9's puller does not run there; collect the
  run folder by an exec tar before each reset, so an eval row links to its trajectory.
- **More suites:** `docs-v1` (documents becoming records), `gst-v1` (a returns portal and a client chat mock, written by
  someone who does not tune the harness) and `learn-v1` (memory kept between cases; needs a runner mode that skips the
  snapshot reset).
- **D7 waits on the owner.** Dev suites live in `evals/` with fictional data only. The CLAUDE.md working-agreement line
  ("Test data and checkers live outside the repo") still says otherwise: the writer did not edit CLAUDE.md, since only
  the owner may approve that. Proposed line: "Dev eval suites with fictional data live in `evals/`; the sealed held-out
  set stays outside the repo and is backed up off-host."
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
  suite that carries state between cases must say so explicitly (a suite or run flag that skips the clear), never by
  default; not built.
- **Practice and eval desktops are reset by ending the worker's processes and clearing its home's agent state**, then
  restarting. L2 should check `ls -A ~/.decurix` after a reset holds only the snapshot.
- **`approval.site_known` reads the request from a file the worker's user can write** (`/tmp/decurix-current-task.request`),
  so a worker's command could name a site there and skip the "password to an unnamed site" question. For PY / O-1.
- **A checked-only final with no verdict still records as checked** (an image from before R6, D5's one release).
  After this build every desktop sends a verdict: switch it to fail closed (checked-without-verdict stored unchecked)
  at the next release.
- **A judgement id is chosen before delivery** (C6). If delivery errs after the desktop accepted the request, the
  desktop holds an id never committed, and what it keeps from that reply answers 409 (fails closed).
- **`tools_waiting` says nothing after a failed populate**, though the tools CMD empties the volume first: report
  "tools not ready" too.

## Phase 2: Python agent, review round 2

- **Comparators still outside `values.py`** (known divergences, each to move as its area is next touched):
  - `long_task._same_value` / `_scalar` (`long_task.py`, a kept cell's value): its own date logic reads `a > 12` as day
    first even under MM/DD, and compares numbers within their written precision (a deliberate tolerance for a cell and
    its computed value).
  - `provenance.fact_value` / `same_figure` (`provenance.py`): figures within the coarser precision, and an ambiguous
    date with no convention compared as written.
  - `stage_facts`: amounts as floats (`round(..., 2)`, `abs(...) <= 0.011`).
- **The control plane does not yet send `execution_id` on `/request`** (handoff H-PY-7). Until it does, a run is held
  by time: a label written after the last EXPORTED is held again.
- **A run open or paused for days is not held** before it has a label: `evidence._prune` can take its notes after 2
  days (review round 2, non-blocking 10). Hold an open task's run as well.
- **The person's own delete of a recording** (`recording.delete`) removes a held run's recording before its export:
  their explicit act; say so in recordings.md or check `held` there.
- **The skill a run noted on its task** (`task_context.set_skill`) stays when the skill run hands the task to the
  general harness; the outcome then names a skill that did not drive its end.
- **The `code` basis of trust** now needs a fact the request states (a figure, a reference code) compared against the
  files made and held (`produced.compared_facts`); a request that states none earns no `code` basis however clean
  its files read. Watch the L3 matrix for skills that can no longer earn trust by code alone.
- **A date the save waits on** is asked through the next question the engine puts to the person
  (`messaging.ask_for_query`); an engine that reports instead of asking leaves the record unsaved (not done), and the
  question is not asked by itself.
- **Secrets, round 2 leftovers** (the security review of 2026-11-04):
  - The task's secret values as they are live in the task file's `.secrets` (tmpfs, 0600) until the task's final
    report; a container restart loses them (the interrupted task is then reported, not resumed). The word hashes stay
    desktop-wide.
  - Writers listed in `agent/tests/test_writers_withhold.py` with "follow-up" hold model- or document-derived text
    that is not yet withheld: app guides, methods, procedures, email-draft records, the record ledger (which needs a
    literal-only withholding, since shape matching would withhold a 16-digit account as a card number).
  - `resend_learned` can post a line twice when two threads read it at once (no client id for the control plane to
    dedupe on).
  - The gate's nonces are in memory: after a gate restart a signed body still within its 120 s window replays (the
    worker cannot capture the traffic: NET_RAW is dropped). Say so in gate-boundary.md.
  - Soul's step text (`Running: gui type e3 "<value>"`) has the same escaped-form exposure the gate had: relay to SOUL.
