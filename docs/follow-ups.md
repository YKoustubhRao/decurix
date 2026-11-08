# Follow-ups after the review of 2026-11-03

The deep review of 2026-11-03 fixed about 300 findings on the `review-fixes` branch. This file lists what it left open: the owner's decisions, the deferred items, and the live checks the writers could not run (no containers, no app builds, no paid APIs). Each line carries its review ID. Security items are written by input class, never as attack strings.

## Owner decisions

| ID | Decision | State |
|---|---|---|
| O-1 | Input broker and OS boundary: direct input around the gate (XTEST, VNC, CDP, AT-SPI, host-app and phone viewers), a gate-owned evidence store, `checked` set by the worker, the script sandbox's filesystem | **Broker on by default** (decided 2026-11-08): every new desktop is made in broker mode unless the control plane runs with `DECURIX_SCREEN_BROKER=0`; existing desktops keep their mode until recreated. The panel's window list is the screen user's now (`computer/windowlist.py`, lists and raises windows, starts nothing; [gate-boundary.md](gate-boundary.md), "The panel's window list"). The word guards are frozen. The agent token's half of path 6 is closed (2026-11-08: out of every environment, two non-dumpable holders, start scripts on the system's PATH, a session bus without the worker's services; gate-boundary.md, "The agent token's two holders"; checked live). **Open:** the default mode's boot race for the token files, `VNC_PW` out of a `docker exec`'s environment, the worker's home readable by the screen user by path, no desktop icons in broker mode, the gate-owned evidence store (it makes "observed" real), an agent-signed outcome callback; also a per-task `$HOME` snapshot, read-only binds outside `~/work`, an egress proxy |
| O-1.a | TheBot command guard: five structural fixes before the final freeze | **Done** 2026-11-07: all five landed and the guard is frozen (`safety.go`'s FROZEN note). Table below |
| O-2 | Per-desktop cache volume (B-02) | Done as recommended: a cache volume per desktop, the model cache shared read-only |
| O-3 | Brainiac appliance: model user, AppArmor, signed catalog, uploader list | **Open; required before any box ships.** Table below |
| O-4 | Legacy harnesses (Azure, `task_runner`, Claude Code) | **Done** 2026-11-07: deleted (`azure_harness`, `task_runner`, `claude_harness` with `claude_guard` and the MCP server, `internal/claudeauth`, the Claude Code CLI in the tools image). A desktop created with one runs TheBot; the `claude_session` table keeps its migration steps, emptied (harnesses.md) |
| O-5 | Public tunnel off by default (B-08) | Done: off by default, one switch in the Mobile menu, `DECURIX_PUBLIC_LINK=1` for the scripts. Veto if you disagree |
| O-6 | A typed "yes" in the desktop chat as an approval (W-07) | Kept, with `seen_at` frozen at the first keystroke and buttons bound to the note id. **Open:** buttons-only is safer if the UX is acceptable |
| O-7 | Org knowledge and skill writes from the gated listener (A-08) | Done: host-operator only |
| O-8 | Acquired skills, proven fixes and the coding harness inside Decurix (T-07, T-08, T-21) | Done: off unless `THEBOT_STANDALONE=1`. Veto keeps them |
| O-9 | taskRules audit to principles only (T-18) | Staged as commit `ab8cac8`. **Open:** bench it on hosted brains before keeping it |
| O-10 | God-file splits (`core.py`, `long_task.py`, `skill_task.py`, `operator.go`, `httpapi`) and the check-registry rewrite | First split done (2026-11-07): `long_task.py`'s stage checks and the cross-check's code part moved to `stage_checks.py`, no behaviour change. Second (2026-11-08): `computer/core.py` (5673 -> 4135 lines) gave select's machinery to `selecting.py`, sheet blocks and slide text to `sheets.py`, and the comparison of two looks to `changes.py`, no behaviour change. Third (2026-11-08): `skill_task.py` (4221 -> 2496 lines) gave the commands and reply words to `skill_commands.py`, `/learn` to `teaching.py`, an engine's mid-task question to `engine_questions.py`, and the final gate with its restore offer to `final_gate.py`, no behaviour change. **Open:** the other files, one at a time, each behind the replay gate |
| N-W4-8 | A Landlock file policy for Brainiac's headless browser (profile folder plus the system read set, network kept) | **Built** 2026-11-08 (`web/render.go` `browserPolicy`, `render_linux.go`): through `isolate.Wrap`, profile read-write, system read set read-only, `HOME`/`TMPDIR`/XDG moved into the profile; no Landlock = old behaviour plus one log line. Live in a `golang:1.27` container: a child under the policy wrote its profile, read `/usr`, and got EACCES for a file outside, `/proc/1/environ` and a write outside. **Open:** a real Chrome render under the policy (`BRAINIAC_LIVE_RENDER`) before Render may be turned on on an appliance; Chrome's children lack their own `/proc/self` |
| O-11 | The approval gate on a grounded click (found 2026-11-08, ERPNext bill 05, taught worker) | **Open.** (a) Clicking a "+ Add Purchase Invoice" button was put to the person as a buy: the gate reads the control's words, and the word guards are frozen (O-1), so changing that is yours. (b) `gui click --see` asked again after each yes: every new screenshot placed the control at a slightly different point, and a yes covers one action at one point, so the same click was asked three times until the person said no. One option that keeps "a yes is one action's": bind the yes to the control's identity (role and name in the observed tree) plus a small tolerance on the point, not to the exact point |
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
3. **A signed catalog.** **Built 2026-11-08, uncommitted:** `pull` refuses any HF source without `revision` and per-file `sha256`, and any GitHub asset or llama.cpp build without `sha256` (`install.CheckPins`, fail closed, every gap named); `brainiac catalog pin|sign|verify|keygen` (ed25519, detached `.sig`); an overlay is used only when its signature verifies, and on an appliance only a signed one (`scout adopt`/`scout pin`/updater adoptions refused there). `catalog pin` filled the shipped catalog from metadata (28 HF sources, 4 sherpa-onnx assets, 36 llama.cpp b11541 assets), left in the tree for review. **Open:** the owner makes the key (`catalog keygen`), compiles the public half into `internal/catalog/sign.go` (`releaseKeyHex`), reviews and signs; `whisper-turbo`, `whisper-small`, `whisper-tiny.en` have no GitHub digest and stay unpinned (refused) until someone hashes them; Ollama tags are not pinned (Ollama fetches them).
4. **A shorter trusted-uploader list.** **Done 2026-11-08, uncommitted:** `scout.TrustedUploader` holds first parties only (labs that make what they publish); quantisers (`ggml-org`, `unsloth`, `bartowski`, `mradermacher`, `lmstudio-community`), `openai-community` and `meta-models` left it, so their copies go through `brainiac scout adopt` (tests: the updater skips them, adopt still works and pins).
5. **Hash-locked worker requirements** (G-09). **Built 2026-11-08, uncommitted:** `brainiac/scripts/lock_worker.py` resolves `requirements-ocr.txt` and `requirements-asr.txt` together from PyPI's JSON index alone (Simple API sha256s, PEP 658 metadata, markers evaluated per target, wheels only) and wrote `workers/python/locks/<group>-cp3N-<target>.txt` for CPython 3.11-3.14 on `win_amd64`, `linux_x86_64`, `linux_aarch64`, `macos_arm64`: all 16 combinations, both groups. The installer takes only the lock for its Python and system (`install.WorkerLock`), refuses an unlocked file (`worker.CheckLocked`) and runs pip with `--require-hashes --only-binary=:all:`; no lock, no install. numpy is pinned (2.4.6 below 3.12, 2.5.4 from 3.12). **Open:** a live `brainiac pull` of the worker groups from a lock on Linux (no install was run here); macOS x86_64 and Windows arm64 have no lock (refused).
6. **Signed-release promotion.** `update.Promote` is the one place a staged plan becomes served. **Built 2026-11-08, uncommitted:** `Promote(st, now, PromoteOptions{ByHand})`: on an appliance only a `release.json` signed with the release key (ed25519, `catalog.Verify`; kind `brainiac.release/v1`, so a signed overlay cannot pose as one) naming the staged lock's sha256 promotes, and only with a `parity.json` of that lock, measured after staging, in which `evals.Results.Proven` holds for every changed model against the one it replaces (every served field for the brain, its own role for other roles); off an appliance a person may still promote by hand, but a present release is always checked. `brainiac update sign-release <lock> --key k` makes the release. Tests: unsigned, untrusted key, non-release, other lock, no/failed/stale/other-lock parity refused; signed and passed promoted. **Open:** the box-side parity run that writes `parity.json` from cases of the request shapes the box serves (trace shapes, `likeEvalCases`) on the staged plan's models; the owner's release key (shared with O-3.3).

## Deferred items

### Control plane, desktops, scripts, Electron (W1)

- **Digest pins** (B-24): `accetto/ubuntu-vnc-xfce-chromium-g3` (still `:latest`), `alpine:3.24` and `ollama/ollama:0.40.1` are pinned by tag only. Pin digests on the next build (needs a pull).
- **Dead bridge verbs** (B-27): removed 2026-11-08 (3,804 -> 2,258 lines). Gone: the native-worker screen verbs Go never sends (`open`, `openr`, `click`, `count`, `shortcuts`, `tasks`, `start`, `shell`, `win`, `over`, `screen`, `focus`, `windows`) with the painted wallpaper, taskbar, Start menu and window manager behind them, the AppContainer terminal (`Confined`), and the compiled-program mode (`-Build`, `-Resolve`, `Main`/`Starting`; Go stopped using it on 2026-10-06). The C# compiles, the script parses, `go test ./internal/winhost` and the real-helper check pass. Owed: one live host-app run on Windows (open, click, type, close).
- **Done: B-19** (2026-11-08, uncommitted). A deleted VM's private app profiles on the host go with it: `forgetHostApps` (delete only; a stop keeps them, they hold the worker's sign-ins) calls `winhost.Manager.DeleteDesktop`, which ends the apps and sends the bridge verb `dropprofiles` with the apps' private desktop names (shape-checked in Go); the bridge removes only folders directly under its own profile root whose name is exactly one of those, never follows a link, and counts files still held open (Go retries 3 times). Tests: the Go side with the stand-in, the real bridge against a temporary LOCALAPPDATA. Owed: one live delete on Windows.
- **W-14:** one aggregated endpoint per dashboard tab (performance only).
- **Done: C-25.** The practice broker knows its desktop by the address a callback comes from, never by `desktop_id` in the body: `Broker.Caller` = `desktop.Manager.PracticeDesktopAt` (labelled `practice-<run>` containers on `decurix-practice`, never the mock), answers kept per address (`Broker.fromRunDesktop`), a body naming another desktop refused, no `Caller` refuses all (simulation.md).
- **A-07:** no `knownHost` list on the gated listener (accepted: the gated listener is reached under changing host names; the session cookie and same-origin rule answer rebinding).
- **Done: B-37.** Each schedule and DAG cron has its own IANA zone (`schedule.Expr.NextIn`, `zone`/`cron_zone`, appended Migrate steps; old rows read UTC), picked on the forms from the browser's zone.
- **Done: B-29** (2026-11-08, uncommitted). `Watch-LanTunnel` (`Lan-Tunnel.ps1`, used by `build.ps1`/`rebuild.ps1`) and `supervise_lan_tunnel` (`lan-tunnel.sh`, Linux and Mac) start the tunnel again when it exits: 1 s doubling to 60 s, reset after 10 minutes up, the switch read again before each restart (off ends the loop), the new address reported. Tests: `scripts/testing/lan-tunnel.test.{sh,ps1}`.
- **Done: B-10.** Desktops reach the native Ollama only through an allow-listed proxy (`internal/httpapi/native_ollama.go`, `modelRequestPolicy`; d25d201).
- **Done: W-20.** The viewer's script is served at a path naming its own bytes (`noVNCPath`, `rfb-1.6.0-<sha256 12>.js`), so a changed file is never a stale cached copy. Also done: the in-flight guards (`loadNotes` and `pollResourceAlerts` go through `Decurix.poll`, one call at a time) and a bounded `/api/desktops/memory` (`?limit=`, default 200, max 1000, `truncated`).
- **Done: RW1-G-04 rest.** A start or a resume waits for a tools refresh in progress, at most 5 min and never past the request (`holdToolsForStart`, `ErrToolsRefreshing`).
- **Done: orphan home volume.** Create answers 409 `leftover_home`, and the form offers to delete the old volume (`POST /api/desktops/home-volume/delete`, primary listener, humansOnly; refused while any container mounts it or it is not that desktop's labelled home).
- **Done: RW1-D-01.** A desktop container carries its creation's id (`decurix.creation`, as its home does); after a `ContainerCreate` error, `Manager.removeHalfCreated` removes a container of exactly that name with that creation's id, never another's.
- **Decided: the default local model stays `devstral-small-2:24b`** (correctness before speed; `auto` still picks per machine). Soul, `start_agent_terminal.sh` and `config.py` now fall back to the same model.
- **Done: mail jobs.** A finished job with no request no longer shows "Run again" / "Send anyway".
- **Done: no-echo walker by type** (2026-11-08, uncommitted). `TestNoResponseEchoesAnErrorByType` type-checks the package (go list -export) and fails on any error value, error text, or result of a package helper derived from one reaching anything that implements `http.ResponseWriter`; sentinels and the named doors (`refusal`, `refusalLines`, `providerSaid`, `viewerReason`, `practice.Public`) pass. It found one real leak, fixed: `/api/host-bridge/exec` sent `cmd.Run`'s words (`hostExecReason` now). Two sentence conversions now go through `refusal`/`refusalLines`.
- **Done: two control planes on one data folder.** Start takes the data directory's exclusive lock (`sqlitedb.LockDataDir`, `decurix.lock`: flock, or LockFileEx on Windows) before the database, and a second start refuses with the holder's pid and start time (`ErrDataDirInUse`).
- **P3s not done:** B-02 (a panic path leaves only `last_error`), C-06 (a live test positive control; walk all packages), C-07 (a main-level Docker-down test; the watcher is tested), F-05 (owner's call), A-02 to A-06 (Electron details), F-08, F-10, F-11 (notes).

### Python agent (W2)

- **O-10:** the check-registry rewrite (step 1, the `_checked` wrapper, is done). The first god-file split is done (2026-11-07): the stage checks (`stage_code_findings`, `CHECK_MATURITY`, `_checked`, every check and the helpers only they use) and the cross-check's code part (`code_disagreements`, `model_disagreements`, `odd_one_out`) are `stage_checks.py`; long_task keeps the prompts and the run, imports what it calls and re-exports the names other modules and tests read through it. stage_checks never imports long_task: the run's ledger, runner and model call are handed over at import (`stage_checks.use_run`), looked up in long_task at each call. A test that patches a check helper patches it on `stage_checks`. The second split is done (2026-11-08): `computer/core.py` keeps observe/act/verify/open and what they share; `selecting.py` (select's machinery and "nothing else in its place"), `sheets.py` (a block of cells or slide text typed and read back, here or in a relayed viewer) and `changes.py` (`compare`, `summarize`, `text_change`, `expectations`) never import core: `observe`, `_task_now` and `_focus_window` are handed over at import (`use_core`), looked up in core at each call; core re-exports by name what others read through it, and the broker's `core.forget_failed_search` route still resolves there. Chosen as the parts with the fewest names tests patch through core: the downloads, settle, commit-check and record-scan groups were left because tests patch their helpers through core while code inside the group calls them, so a move would make those patches silently miss. The third split is done (2026-11-08): `skill_task.py` keeps perform/resume, the run and replay driver, the per-document part, the model's guided run and its judge; `skill_commands.py` (the `/learn`, `/skill`, `/restore` parsers and writers, the reply words, `LEARN_ENGINE`/`SKILL_ENGINE`, `_Refused`), `teaching.py` (`/learn`: `_learn`, `_learn_reply`, `_remember`), `engine_questions.py` (messaging's query gate `_question_gate`, carry-on, `learn_answer`) and `final_gate.py` (messaging's final gate `_gate`, `_retry_engine`, the "cannot confirm" reply `_confirm_reply`, the restore offer and `/restore`, `ledger_held`, `check_unchecked`) never import skill_task: what they call back there (`_judge`, `_record_verdict`, `_by_model`, `_engine`, `_engine_can_drive`, `write_final_reply`, `ask_for_query`, ...) is handed over at import (`use_run`), looked up in skill_task at each call, so the many tests that patch those through skill_task still reach the moved code. skill_task re-exports by name what others read through it and still sets messaging's two gates at its import. Chosen as the groups whose own names tests patch least: a test that patches `_did_screen_work`, `_retry_engine`, `_commands_text`, `_done_reading` (final_gate) or `_remember` (teaching) patches it there now (long_task's tests still patch `skill_task._did_screen_work`, which long_task reads); the run driver was left because tests patch its helpers through skill_task while code inside it calls them. Left: the rest of `core.py` (downloads/waits, the commit check), `operator.go`, `httpapi`.
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
- **Done 2026-11-08 (uncommitted): an all-capitals request** no longer hides the client's own facts: `memory_context.names_whole` reads a capital as continuing a name only on a line that writes small letters too (a line wholly in capitals has no case, like one all in small letters).
- **Done: an IBAN** with an unknown country and a valid mod-97 is reported "IBAN NOT CHECKED" among the unchecked payments (`payees._is_iban` answers None).
- **Done 2026-11-08 (uncommitted): a second note on the same value** no longer overwrites `OTHER_SIGN`: `binding._clean`
  keeps every harness note on a value whole (`OTHER_SIGN`, `NUMBERED`, `WORKED`), joined, beside the model's own words
  cut short, so no cut takes the end of a mark the final check looks for.
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

- **G-11:** carrying unchanged units into the new engine. **Built 2026-11-08, uncommitted:** one Residency per server (`engine.NewServerResidency`, `api.Server.Residency`, `engine.Options.Residency`); residents are known by key and recipe, model servers and the worker are shared by recipe, a new engine adopts the unchanged running units and the retired one stops only its own. Tests with the fake llama-server (kept process, changed one stopped, late load passes forward, held model waits for its request, shared worker). **Open:** a live plan change under `serve`.
- **G-17:** re-derive the workflow bench's results from artifacts (the name now says what is checked).
- **G-09:** hash-locked requirements (O-3.5). Built 2026-11-08; see O-3.5.
- **FIX-BR-1:** a managed (non-system) python for the worker. `isolate.ForProgram` (`brainiac/internal/isolate/elflibs.go`, called from `brainiac/internal/engine/engine.go` `New`) grants its binary and the shared libraries it links, but not its standard library (`../lib/python3.x`), so it may not start in the mount view. A system python is unaffected (`/usr` is read). Check it live, and grant that tree read-only if it fails. **Built 2026-11-08, uncommitted:** `isolate.ForProgram` grants a managed Python's `lib/python3.N` read-only, found structurally (`<prefix>/bin/python*` by real path, `os.py` landmark, not under the system read set, real path inside the prefix; a versioned name claims only its version). Tests with a fake tree. **Open:** the live start of a uv/pyenv Python worker in the mount view.

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
- **Done 2026-11-08** (see "Owed checks run on 2026-11-08" below): the sign-in refusal on a real gated listener and the
  field-unit question's round trip with a hosted brain. Still owed: every item below.

## Live checks done on 2026-11-08 (image rebuilt from this tree, a dev control plane beside the app's)

- **Benches**: `bench/skills/e2e.py` and `bench/recordings/e2e.py` pass on `decurix-desktop:next`, with and without
  `--broker`.
- **The screen broker's live round** on a control-plane desktop: the panel's browser icon, a taught skill opening a
  local page, a long request (two stages, done and checked), and a replay whose ladder asked the hosted model through
  the broker (`py.escalation.ground`); all calls tagged. Details and the broker-default verdict: gate-boundary.md,
  "Live pass on the rebuilt image".
- **The plan's "OK first"** on a gated default-mode desktop: one yes, one press; a no, no press and not done. It asked
  twice before two fixes made in this pass (soul adopts the gate's own request for an action it stopped on the screen;
  the agent's watcher no longer takes TheBot's yes from the gate) - gate-boundary.md, same section.
- **Not reached, still owed:**
  - The field-unit question with a hosted brain: **run on 2026-11-08, below.**
  - The sign-in refusal on the gated listener: **run on 2026-11-08, below.**
  - The review's "Still open" lines on a held press read screenshots as empty fields that `gui observe` showed filled
    (seen twice on the tax-code form). **Fixed (2026-11-08):** a chain held at its press (`gui type ... && gui click
    Save`) recorded nothing, so the review was shown the look and the screenshots from before the typing. Now the
    typing ahead of a held press is noted (`noteHeldScreen`), the window is looked at again before the review when
    typed since its last look (`lookBeforeCommit`, moved ahead of `review`), a look older than the typing is shown as
    "NOT READ", and only a screenshot of the screen as it is goes to the reviewer (`lastPictures`). Owed: a live held
    Save on that form.

## Owed checks run on 2026-11-08 (images built from HEAD 8586a01 only, broker mode by default, hosted Kimi-K2.6)

- **Gated sign-in: passes.** A user (`maya.finch`, role finance) made on the primary listener; on the gated listener
  with the access code only, `GET /api/auth/me` 401; a reply to the worker's plan question 401 "Sign in to reply: ...
  Nothing was sent." and no judgement made; signed in, the same reply 200, delivered, and the task's one judgement
  for it names `maya.finch` (`kind: person`, door `card`).
- **Field-unit round trip: passes after one fix.** On a fictional form whose "Rate" has no unit and a note saying
  "18%": the save stopped with "the agent asks them itself", but the person got the engine's own unnumbered question -
  in broker mode the screen side kept the unit question in its own `runs/<task>/context.json`. **Fixed:** that file
  is now sent with each command and handed back (`gui_client.TASK_STATE`, `gui_client.mirrored`; gate-boundary.md,
  "The screen side's own state"; `test_screen_state.TheTasksRunContextGoesBothWays`). The same applied to the
  date-order question and to the org's date order the save reads. Patched live: the numbered question came ("Reply 1
  if the field "Rate" on 127.0.0.1:8765/taxes takes its figures in %, or 2 ..."), "1" was kept as a `field_unit` row
  derived from it, and a later task (a 12% code) typed "12" against "12%" and saved with no question. On the way: the
  worker skipped `gui record` on its first run (no declaration, so no unit check) until told; and after the save
  asked, it retyped "18%" into the field despite "Do not retype the field".
- **Rule 3: passes.** Every `py.*` call of the three tasks (`py.study`, `py.judge`, `py.kept_facts`,
  `py.approval.read` read by the agent: 14) is a `caller: soul` row with its purpose and a line in soul's trace, one
  for one; the gate's two `py.approval.read` went straight to the proxy as `caller: gate` (with no task tag); no
  `caller: agent` row; `untagged: 0`.
- **Done 2026-11-08 (uncommitted), found here:** a one-digit percent could not be declared. `gui record ... "Rate": "5%"` from a note saying
  "rate 5%." is refused as not in the source, every time (the worker tried for 30 turns): `records.stands_in` gets None
  from `values.parse_number("5%")` and the text key "5" is under its two-character floor; "18%" passes only as an
  identifier. A fix belongs in `records.stands_in` (a figure with a unit stands where the text writes that figure
  with that unit), not yet made because that file was being changed in parallel. Now `records.stands_in` takes a
  figure written with its unit as standing where `values.units_beside` finds that figure with that same unit ("5%"
  in "rate 5%.", "5 kg" in "Weight: 5 kg"); `values.parse_amount` already read a percent of any length, and
  `parse_number` stays unit-less on purpose (a bare 5 would then stand for "5%"). A bare one-digit figure is still
  never found inside another figure.

## Two control planes on one Docker (found 2026-11-08, dev rig)

A second dev control plane started beside the first on the same Docker took the first's desktop for its own: its idle
pause (`idlepause`) sees only its own database's activity and paused a desktop mid-task, and each control plane's
`CleanupPractice` at start removes every practice desktop on the host. Not a product path today (one control plane per
host), but any rig that runs two (parallel eval lanes) needs both to scope their sweeps to desktops they created (a
control-plane instance label on the container, checked by idle pause and cleanup). **Done** 2026-11-08: every desktop and practice container and practice network carries `decurix.owner` = the control plane's instance id (`desktop.OwnerLabel`, `sqlitedb.InstanceID`); `List` leaves out another control plane's, and the idle pause, the rest/stop that makes room and the practice cleanup act only on their own (`Desktop.Owned`), logging once what they leave alone. A desktop made before the label is adopted when the agent token it was created with is the one this control plane issues for it (an HMAC under its own secret, `Manager.ownedLegacy`), so upgrading keeps idle pause on every existing desktop; one with no token is listed and left to no sweep until it is recreated.

## A refused outcome leaves its task running (found 2026-11-08, video lane)

When the control plane refuses a desktop's outcome with 403 (here an infra fault: a freed practice range still refused,
since fixed in `practice.SubnetCache`), the agent lets the kept outcome go for good after its short form is refused too
(`messaging.resend_unsent`), and the task stays `running` in the control plane, blocking the desktop until it is deleted
(the task then reads `abandoned`). A refusal the desktop cannot tell from an infra fault should be kept and retried with
backoff, or the task marked failed by the control plane, never left running. **Done** 2026-11-08: `messaging.NOT_YET_CODES` (401, 403, 404, 408, 425, 429) are tried again like an unreachable control plane, kept and resent for up to a day; 400, 409, 413 and 422 stay the control plane's no to the note itself.

## A pre-save refusal that needs the person waits on the model to ask (found 2026-11-08, ERPNext bill 11)

The ledger's pre-save comparison refused a save over a digit date it could not read ("which day 11-10-2026 is cannot be
told from here - this needs the person ... the agent puts the two readings to them itself when you next ask them
anything"). The model never asked: it read the refusal as a wrong format, retyped the date three ways (one of them,
"2026-10-11", the application took as 20-10-2011) and looped for 13 minutes. Code should put such a question to the
person itself at the refusal (the readings are code's), not wait for the model's HELP. Mitigated in the meantime: a
taught run no longer declares an ambiguous digit date when the application's order is unknown, and an organisation's
`DECURIX_DATE_ORDER` settles it. **Open.**

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
- **Done: the bundle limit is a setting** (2026-11-08, uncommitted): `DECURIX_RUN_BUNDLE_MB` (default 64, 8 to 1024, read
  once at start, every puller; running.md). Open: choose the default from real runs' sizes.
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
- **Python's model calls behind soul** (plan rule 3). **Phase 1, the transport, is done (2026-11-08):** every
  worker-side call goes through soul's `POST /api/llm/call` (`cmd/soul/llmcall.go`; `agent/decurix_agent/soul_call.py`)
  - `llm_client.chat`/`ask_once` (hosted) and `ollama_client.ask_once` (local) - so it has soul's trace line, tags,
  prompt-cache key and concurrency limit, with its `py.*` purpose recorded verbatim (`llm.AgentPurpose`). Straight to
  the proxy (or Ollama) only when soul cannot make the call (not running, an older image, a refusal, no token or key;
  logged once per process) and always in the approval gate (`soul_call.in_gate`: IN_GATE or `DECURIX_CALLER=gate`;
  `test_soul_call.TheGateNeverAsksSoul`). Usage is counted once, by Python (`usage.add`), never in a soul job. What
  remains is phase 2: moving each **decision** into soul, purpose by purpose, so Python holds tools and evidence only:
  `py.judge`, `py.judge.spot`, `py.skill.ask`, `py.skill.calls_off`, `py.stage`, `py.binding.fill`, `py.ground`,
  `py.escalation.ground`, `py.escalation.judge`, `py.escalation.detour`, `py.escalation.point`, `py.mimic`,
  `py.skill_engine.fits`, `py.kept_facts`, `py.playbook`, `py.study`, `py.tool.args`. `py.approval.read` and
  `py.approval.choice` stay with the gate by design (another user's process; never soul's).
  `agent/tests/test_purpose_tags.py` holds the set. Open with phase 1: a desktop's reasoning effort reaches a call
  through soul as soul read it at start (`OpenAIProvider.Effort`), where the direct path read the file per call; a
  `temperature` of 0 through soul is the provider's default (no caller passes one today). Live check done
  (2026-11-08, "Owed checks run on 2026-11-08"): `py.*` lines in soul's trace one for one with `caller: soul` rows,
  none with `caller: agent`. `py.approval.read` is also made by the agent when it reads a reply (`approval.reading`
  from `engine_questions`, `gated_command`, `final_gate`); those go through soul like any worker-side call.
  - **Phase 2, `py.judge`: built behind `DECURIX_SOUL_JUDGE=1` (off by default, read per call), parity proven
    offline (2026-11-08).** `verify.judge` hands soul the evidence as data (`verify.judge_evidence`: texts, values,
    observations, the files' facts, the documents, pictures as `data:` URLs; Python still reads the files, the screen
    and the closer look) at `POST /api/llm/judge` (`cmd/soul/judge.go`), and soul words the prompt and reads the answer
    (`agent/thebot/internal/judge`: `System`, `Messages`, `Decide` - Python's `JUDGE_SYSTEM`, `judge_content`,
    `_parse` and the not-sure-enough rule, with Python's `str()`/`float()`/`strip()`/`repr()` reproduced). Same call
    path as `/api/llm/call` (`makeCall`), purpose kept (`py.judge`, `py.judge.spot`), retries and the cut-off retry
    still `llm_client`'s (`chat_decided`), the error mapping still `verify.judge`'s (soul answers `{error, cut_off}`
    for an answer it cannot read; unsure, never done). Soul down, an older image (404) or a refusal: today's path.
    Proof: `agent/thebot/internal/judge/testdata/parity.json` (13 cases, 49+14 answers, fictional), recorded from the
    unmodified code; `agent/tests/test_judge_parity.py` (today's path still sends those messages and verdicts; the
    switch sends that evidence and maps soul's decision to the same verdict), `internal/judge` parity_test.go (soul's
    messages and decisions equal the recorded ones), `cmd/soul` judge_test.go (the provider is sent the same bytes from
    the evidence as from today's messages via `/api/llm/call`). One known difference: for JSON that does not decode,
    the `why` quotes Go's decoder, not Python's (still unsure). NaN/Infinity literals and Unicode digits in a
    confidence decode in Python, not in Go (stricter: unsure). A quirk kept for parity: a confidence of `"nan"` reads
    as 1.0 in Python's `max(0, min(1, x))`, so `done` with `"nan"` is done on both sides. **Owed before the switch
    defaults on:** a 5-repeat eval (rule 1) with `DECURIX_SOUL_JUDGE=1` against off on the same suite and image: the
    same verdict per case and repeat within the run-to-run noise of off vs off, no false done, the same judge tokens
    per call (prompt and cache share) and `py.judge` rows with `caller: soul`; then delete Python's copy (prompt,
    `judge_content`, `_parse`) so one copy remains.
- **Spot judgement of trusted replays** is built (`skill_task._spot`, purpose `py.judge.spot`; a `not_done` records a
  failure) and samples 1 in 5 task ids by default (`DECURIX_SPOT_JUDGE_EVERY`; 0 never, 1 always). Open: measure the
  judge's cost and its catch rate on the L3 matrix before changing it. `learn-v1` exists now (2026-11-08) and holds
  no case of its own for it: a spot judgement needs a trusted replay, which five cases of a kind may not reach.
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
- **Done: a delete no longer loses runs silently** (2026-11-08, uncommitted). After the 90 s flush, runs still not pulled
  refuse the delete (409 `held`, `unsaved_runs`, "N runs of this desktop are not saved to the run record yet; try again
  in a minute, or delete anyway"); the dashboard's confirm shows that reason and offers "Delete anyway" (`force=1`),
  recorded first as a `delete_anyway` judgement (actor, door, the runs given up); a desktop's own call cannot force it.
  Open: measure live how long a flush takes; `decurix --cli` shows the refusal but has no "delete anyway".
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
- **More suites:** `docs-v1` (documents becoming records) and `gst-v1` (a returns portal and a client chat mock,
  written by someone who does not tune the harness). **Done: `learn-v1`** (2026-11-08): ten interleaved cases, bills
  and bank reconciliations, a lesson taught once per kind and a missing input; `py -3 evals/run.py curve` reads it
  ([evals.md](evals.md), "The learning curve"). **Calibrated live 2026-11-08** (oracle 10/10, nothing and ask 0/10,
  twice; evals.md). Two runner fixes came of it: the desktop is settled between a carry-state run's cases (an open
  question no longer swallows the next request), and a restart does not count its own memory reservation. Owed:
  - **Done 2026-11-08: a bill case can pass a model arm.** Owner decision taken: approvals are case data. A case lists
    `approves` (a control and optional role), and the expert says a plain yes to a gate-signed question that words a
    click on a listed control, through the person's route (`practice.ApprovalGateNotice`); every other approval is
    refused and ends the case. Every `learn-bill-*` approves "Save bill" only ([evals.md](evals.md), "The scripted
    expert").
  - **Done 2026-11-08: the cost-centre lesson is no longer guessable.** The rule is a map from the supplier's GSTIN state
    code to the firm's own codes (29 CC-14, 30 CC-27, 33 CC-52, else CC-90), with suppliers in three states, and
    `TestLearnV1SuiteLoads` fails a bill whose code is written in its documents or in an earlier record. Calibrated
    again live: run `e9aa06b8d2a292e61`, oracle 10/10, nothing and ask 0/10.
  - **Worker findings from the same run** (harness, not suite): on bill 3 the worker tried to `curl` the bill straight
    into the app's form endpoint, with cost centre General for a freight bill (the gate held it); on bill 4 it declared
    "as per covering letter" as the bill number rather than asking; bill 1 took 1.36M tokens and 66 calls, near the
    case budget. **Fixed 2026-11-08** (bills 3 and 4): the bill-4 phrase did stand in the
    document, beside "Invoice number", so `declare` called it backed; a number, reference or id given as words (no
    digit, two or more words) is now refused at `gui record`, at the save and at the final check unless the people's own
    words give it (`records.wordy_identifiers`), with a question naming the field. A data-sending curl/wget or posting
    code is a final-check finding when the task declared a record or the command names a host where it opened or saved
    a record's form (`records.sent_around`). **Decided 2026-11-08:** a send to this machine is not held at the gate
    (the command guard stays frozen); such a save can never be reported done, and the real fix is the egress proxy in
    O-1's broker plan. `skill_task._ledger_why` names both new kinds. Owed: the next live learn-v1 run, to see bill 4
    end asking. The reconciliation half met the bar (674k tokens then about 110k, no question after the lesson).
  - **Bill 1's budget, diagnosed 2026-11-08 (`eeb05dd4a201cecda`):** the save was refused at every try because the form
    readers cut values at 80 characters and presave judged the part as the whole (a 126-character Notes field); the
    worker went round declaring, typing, curl and the dev tools until the budget ran out, and the Cost centre select,
    left at its empty first option, was never seen by presave. **Fixed:** values read whole up to 600 characters, a
    longer one marked cut and compared only as far as it was read (a gotcha, part 6); a choice left unset that the
    record does not declare is a point at the save; the operator ends a refusal repeated three times on an unchanged
    form with a question naming the field ([harnesses.md](harnesses.md)). **Owed:** `records.UNSET_CHOICE_BLOCKS` is
    ADVISORY. It becomes blocking only by its own entry here, after it passes the replay gate
    (`agent/tests/test_replay_gate.py`, a fixture with a correctly saved record whose form has an optional choice left
    unset must meet no blocking point) and a clean live learn-v1 round in which no correct save met it.
  - Runner notes: a run started within 90 s of another's desktops could be refused its desktop on memory reservations
    (fixed by the per-desktop reservation only for a restart; a new arm's desktop still counts the last arm's for 90 s),
    and once an arm's agent never became ready in 4 minutes while tests ran beside it on the host (a retry passed).
  - `default+expert --carry-state --repeats 5`: run `eeb05dd4a201cecda` started 2026-11-08 08:28 UTC on the dev control
    plane (image 158b3cdebb0b from 82f2966); its `curve`, and what it says against the bar, are owed here and in
    evals.md. **Cancelled 2026-11-08 09:30 UTC after 7 rows** (the "before"; rows in
    `evals/results/eeb05dd4a201cecda/rows.jsonl`): bill-1 budget (1.46M in, 73 calls), recon-1 pass (166k, the layout
    asked), bill-2 asked (too many questions; 582k), recon-2 pass (71k), bill-3 timeout (1.16M), recon-3 pass (67k),
    bill-4 timeout (1.11M; it asked the invoice number, required, and was answered). Bills 2, 3 and 4 had the right
    record saved (`mock_records` passed) and still did not end done. Two harness-looking points: on bill 2 the expert
    gave "don't know" to "Which cost centre should I select for this bill?" although the question names
    `cost-centre-rule` (trajectory `x0a15bc60469f8d16`); on bills 3 and 4 the final check said the cost centre was
    backed by nothing "a person said or taught", though the person taught the rule in an earlier case, so a lesson
    kept across tasks is not counted as provenance (`xf9654c7fe29c258b`, `xce7e9d6e97a9bcaf`).
    **Fixed 2026-11-08 (uncommitted), three causes found in those trajectories:** (1) only the current task's answers
    were read: the person's own answers on any task of the same worker are now a source
    (`person_record.worker_words`, `GET /api/desktops/judgements?scope=worker&kind=answer`, bounded, newest first, the
    caller's own worker only; [learning-loop.md](learning-loop.md)); (2) on an eval desktop nothing the expert said
    backed anything, even on its own task: the practice broker answered 404 to the judgement read and the runner sent
    no judgement id, so bill 3's own reply with the rule and bill 4's invoice number were refused too. The runner now
    keeps each expert reply on the broker (`Broker.KeepReply`, actor kind `scripted`, forgotten at a reset) and sends
    its id; (3) no text could back a code among codes sharing their letters (the figure is joined, so the word rule
    dropped it): a code written whole picks out its entry (`records._code_written_whole`). **The "2 records where 1
    was expected" (`ledger_readback`):** both bills had one record in the app (`mock_records` passed). Bill 3: the bill
    was saved under the key "bill" with the cost centre undeclared, then declared again under its own bill number to
    add that field; `declare` matched saved records by key only, so it was a new record (first stopped as "may be
    entered already" by the task's own saved page), and the edit's save marked it saved - a harness bug, fixed
    (`records.saved_by_number`: the field joins the saved record). Bill 4: its declarations were refused (cause 2 and
    3), the worker saved the bill outside the ledger with `--checked`, then declared it twice under two keys after the
    save; neither was saved, and the final check rightly held a record no declaration covers. No fix to that check;
    with the causes above the declaration passes before the save. **Owed:** the next live learn-v1 run, to see bills
    3 and 4 end done without asking the rule again. (Done 2026-11-08: the expert's replies are in the task records as
    answer judgements authored by the scripted expert, `eval.ExpertActor`, under the id the worker is sent, so the run
    record's `judgements` names them; `TestTheExpertsReplyIsRecordedWithItsAuthor`.)
  - **Rerun on today's fixes, in progress 2026-11-08:** images rebuilt from ac20e00 (`decurix-desktop:basic`, `:next`,
    `:developer`, `:ai` = a8edcd908964; `decurix-tools` unchanged, 28170e9f7e03); dev control plane `decurix-dev`
    restarted on a binary from ac20e00 (sha256 5939e84a...), volume `decurix-dev-data` kept, brain Kimi-K2.6 set.
    Calibration `e838100482d961956` (oracle + nothing, `--carry-state --repeats 1`, 09:34 UTC): **oracle 10/10,
    nothing 0/10** (false done 10/10), no infrastructure or invalid rows, no problems on an oracle row. The
    5-repeat `default+expert` run, the curve, and the cleanup (`cp-dev.sh stop`, the dev volume, the `before-1107`
    tags, the buildx builder `decurix-wsl-2048m`) are still owed.
  - **The "after" run, done as a baseline 2026-11-08:** HEAD 9038515 (the expert prompt, the broker serving the expert's
    replies, scope=worker provenance, the cut-read, unset-choice and refusal-loop fixes). Images rebuilt from a
    `git archive` of HEAD (`decurix-desktop:basic`, `:next`, `:developer`, `:ai` = e00bd82ce61e; `decurix-tools`
    unchanged, 28170e9f7e03); `decurix-dev` restarted on a binary from the same archive (sha256 68315fea4223...),
    volume `decurix-dev-data` kept. Brain Kimi-K2.6 / gpt-5.4-mini on `AZURE_KEY_1` (the before run's; key 3,
    `AZURE_KEY_2`, still answers 401 on every route). Run **`eaa791e6bd0cff5fa`** (`default+expert --carry-state
    --repeats 5`) started 10:30 UTC; watch with `py -3 evals/run.py --cp http://127.0.0.1:18080 table
    eaa791e6bd0cff5fa`. First six rows (repeat 1), all pass: bill-1 608k in (before: budget at 1.46M), recon-1 172k,
    bill-2 1.04M, recon-2 75k, bill-3 330k, recon-3 66k. **Paused for the owner at 11:20 UTC** on two harness points
    seen in bill-2 (`x8d5d51639a38fd33`), the run itself left going:
    - A "don't know" reply travels to later tasks with no question. The method learned from bill 1
      (`methods.py:158`, "The person said about this plan: ...") carries bill-1's reply to the plan question ("I don't
      know that ... Do not make it up: if the task needs it, say it is missing.") into bill 2's prompts and its
      independent review (call `c0e29be653fa6bbe6`, purpose `review`, input blob `492afed814b8...`). That input has no
      section for this task's own replies: the rule the person gave shows only inside a cited passage. The review
      then said "the person explicitly stated they do not know the cost centre" and called a value the person did
      give fabricated. The case passed only after the expert answered that review question with the rule again.
    - There is no way to declare a value from the person's reply. `gui record --from "human reply"` failed (not a
      file), so the worker wrote the person's rule to `~/work/cost-centre-rule.txt` and declared from that file, a
      source of its own making (the check caught it). It would have passed as `seen` with `--from bill.pdf`, since
      the reply holds "CC-27", but nothing says so.
    - The run record lacks the expert's replies. Every trajectory of the run has `questions` and `judgements`
      empty, and `GET /api/judgements?task=tfdc005bf26753cc6` is empty, though the broker serves both replies on
      that task (`/api/desktops/judgements?task=` from the desktop). `recordExpertReply` logged no error. Fixed in
      7aebc2a.
    **Ended by the owner after repeat 3** (cancelled 14:12 UTC; 30 rows): 22/30 pass (Wilson lower bound 0.56),
    against 3/7 before. Bills 11/15 (before 0/4); reconciliations 11/15 with 4 false done. `curve`: neither kind meets
    the bar (bills: 988k then 536k tokens, but questions that are not required rose from 0.7 to 0.9; recons: 100%
    then 67% with 4 false done). All 8 failures are the harness's and HEAD fixes each: recon-5 r2 `xcb425599da9f1c9e`
    (82495c2), bill-1 r3 `x643d67b7c0e709ac` (82495c2, b797a0f), bill-2 r3 `xccb1b2b6e6045634` (ae332a0), bill-3 r3
    `x3275c004d0ccfc5c` (82495c2, 135771b), recon-3 r3 `xc6b1e64d2526879f`, recon-4 r3 `x8aac58f7c6d4dfc2` and
    recon-5 r3 `xead09eed0df85a94` (be2ebfe, 82495c2), bill-5 r3 `xba5fe22d81ee9442` (b797a0f, 82495c2). Table and
    per-failure evidence: [evals.md](evals.md), "Baseline for 9038515". Rows in
    `evals/results/eaa791e6bd0cff5fa/rows.jsonl`. Next: the validation on HEAD, as two runs one after the other
    (`default+expert`, then `default+expert+soul`), never two practice desktops at once.
  - **Validation run 1 on fc3cd32: partial, host-contended** (`e53e9a9df7f70755a`, 14:24 to 15:35 UTC, cancelled by
    the owner after 6 rows while the owner's ERPNext work and its teaching desktop shared the WSL VM, at load 15-57 on
    6 CPUs). Rows, correctness and tags: [evals.md](evals.md), "Validation on fc3cd32". Every graded record and file
    was right and there was no false done; the three bills timed out. Owed:
    - The re-run on a quiet machine, `default+expert`, then the `+soul` run. Both wait for the owner's "go: rerun".
    - The fix the owner is making: a value set from the person's answer but not declared is adopted into the
      declaration when it stands in the record's document or the person's confirmed words. Bills 2 and 3 were sent
      back for it, and the sent-back job started again from the request (`x19d1e3554629eed8`, `x9a02e6064b9e3ded`).
    - The dev stack is still up (decurix-dev and the dev volume; images a8f4ebfcc2c5 on `:basic`/`:next`). The
      cleanup list (`cp-dev.sh stop`, the dev volume, the app's `before-1107` tags) is owed after the runs; the buildx
      builder is stopped.
  - **Fixed 2026-11-08: the expert's "don't know" to bill 2's cost-centre question.** Matching was right (the question
    named `cost-centre-rule` by `refers_to`); the expert's model chose null because the fact gives a rule, not "which"
    ("it explains how, not which"; the question held no GSTIN). `ExpertPrompt` now says a fact gives what is asked when
    it states it or the rule the person decides it by, a same-subject fact that does not gives nothing, and no fact
    gives leave to act. Probed on the deployment: that question 1/12 before, the new prompt 93/93 on 22 positives and
    near misses ([evals.md](evals.md), "The scripted expert"). Suite data unchanged (SHA `c6722c894ced`).
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
  the table's "State:" line say which mode ran. Rows keep `calls` beside the tokens (2026-11-08), so the curve shows
  model calls per case.
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
  Since 2026-11-08 also `?scope=worker`: the person's answers on the worker's earlier tasks back later ones
  (`person_record.worker_words`; the learn-v1 bullet under EVAL).
  - Not backing anything any more: `<task>.said`, the gone `field-answers.json`, and the skill file.
  - Org knowledge still counts.
- **Accepted residual: a word of the option in an unrelated phrase of the request.** "the general manager approved
  it" backs a "General" entry when no other offered entry has that word (`records.option_backed`). A file is held to
  the field's own label (`option_beside_label`), so a bill's "General terms" footer backs nothing. The request and
  the person's replies are not, since they rarely name a field.
- **Done: the module-order leak.** A secret reply left `state.reply_secret` set, so a later correction was kept by nothing; `state.begin` now clears it (and `reply_judgement`) for each task, and the teaching tests reset it.

## Phase 2: Python agent, review round 2

- **Done 2026-11-08 (uncommitted): the comparators outside `values.py`** ask it now.
  - Dates: `stage_checks._scalar` (a kept cell) and `provenance.fact_value` read dates by `values.parse_date` under
    the request's order, so a first part over 12 is no date under MM/DD (not decided, so a kept cell is "changed"
    and a fact is compared as written), and an ambiguous date with no order is the same only as written.
  - Numbers: one precision rule, `values.within_precision` (half the coarser written place, a float's 1e-6 noise
    aside), reached as `values.compare(..., precision=True)` and `values.same_figure(a, b, precision, a_computed)`.
    A kept cell against its computed value and a fact against a rounded figure keep it (deliberate); a computed
    value (`a_computed`: a product, a sum, a formula's result) claims no precision, so only the written side's
    counts (a result written 1392 is 1392.4 rounded, one written 1392.04 is not); two computed values, or two
    figures each read as written in `stage_facts` (a row's stated amount against its working's cells, a message's
    amounts, a source's price), are equal only (noise aside). Gone: provenance's extra 0.005 (10.00 and 10.01 were
    one figure) and stage_facts' 0.011 and float rounding (`removed_residue` sums in Decimal, compared noise aside).
  - Separators that read two ways ("12,5") are no figure in `provenance.fact_value` either (it read 12).
  - Left as is: provenance's scale words (lakh, crore, a declared unit), which `values` does not read; `sources.
    _same_figure`, not in this list.
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
  - Done 2026-11-08 (uncommitted): `resend_learned` is single-flight (a call while one runs asks it for one more pass
    and returns), and each learned line carries its own `client_id` (32 hex), the same on every try; the control plane
    answers a line already kept under that id with that row (200), under a mutex with its write (`learned.go`
    `keptLearned`). A line with no id (an older desktop) is kept as before.
  - Done 2026-11-08 (uncommitted): the gate's nonces are kept in its own directory (`gate-nonces.json`, 0600, written
    whole, loaded at start, pruned on expiry; `gate_server.use_nonce_store`), so a signed body seen before a gate
    restart is refused after it. Said in gate-boundary.md.
  - Done: soul's step text (`Running: gui type e3 "<value>"`) withholds a job's secret values in every written form
    the agent's `approval.written_forms` gives (H-PY-9, 99d7962).
