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
- **W-19:** the website says "it never asks the same thing twice", and its app replica shows an Approve button on the rail that the real rail does not have. Fix the website copy.
- **W-20 rest:** in-flight guards for `loadNotes`/`pollResourceAlerts`, `/api/desktops/memory` unbounded, the `rfb.js` hash.
- **RW1-G-04 rest:** starting an existing desktop does not wait for a tools refresh in progress.
- **Instance tag (b):** the dead "exceed the copy limit" branch (`internal/desktop/exectar.go`).
- **Orphan home volume:** after a Delete whose volume removal failed, a new desktop of that name is refused with "delete that volume", and no UI does that.
- **RW1-D-01:** a container the daemon made while `ContainerCreate` returned an error is leaked until a later Delete of that name; at most a log line.
- **`internal/ollama/ollama.go`:** the default model (`devstral-small-2:24b`) and its comment about the deleted browser-agent work. Owner's call: restore the 3B default the comment defends, or rewrite the comment.
- **Mail jobs:** the page may hide "Run again" on a skipped or failed job with no request (the server already answers 409).
- **No-echo walker limits** (RW1-G-06 is done): an error variable not named like one, or a helper that returns the text, still gets past it.
- **Two control planes on one data folder** are unsupported; after a crash, two starting at once could each remove the other's fresh `instance-id`.
- **P3s not done:** B-02 (a panic path leaves only `last_error`), C-06 (a live test positive control; walk all packages), C-07 (a main-level Docker-down test; the watcher is tested), F-05 (owner's call), A-02 to A-06 (Electron details), F-08, F-10, F-11 (notes).
- **`bench/workflows/README.md` and `bench/workflows/PM_REVIEW.md`:** change "independently verified" to "checked for consistency: the runner's exported report against the scenario table and the files on disk (recordings, skills, PNG headers). The pass/fail results are the runner's own and are not re-derived." (Not applied: outside the docs paths.)

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
- **Soul's browser skill:** `internal/skills/browser.go` and `THEBOT_BROWSER_EXTENSION` remain after `agent/browser-agent` was deleted; it is off by default and has nothing to talk to. Delete it and its flag.
- **N-W3-3:** `asksAppNamesRe` suppresses a HELP to the person once, by words. It fails toward a delay, never a false success; move it into T-20's gate table as a fact.
- **T-19:** a recall score floor needs nomic's query/document prefixes (re-embedding the stores) or a judge.
- **T-27:** the embedded UI is the owner's decision.
- **A planted row in `l1.db`** (worker-writable) only shows "you approved this on <date>" in a standalone coding question. Word it as "a record on this machine says ...".

### Brainiac and bench (W4)

- **G-11:** carrying unchanged units into the new engine. The right shape is one Residency per Server, outliving engines.
- **G-17:** re-derive the workflow bench's results from artifacts (the name now says what is checked).
- **G-09:** hash-locked requirements (O-3.5).

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
