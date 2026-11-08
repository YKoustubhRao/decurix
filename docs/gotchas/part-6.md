# Gotchas, part 6

Lessons from the deep review and fix loop of 2026-11-03 (review IDs in brackets). The open items are in [follow-ups.md](../follow-ups.md).

## Desktops, files and processes

- **Docker's copy API is root inside the desktop** (`internal/desktop/homefs.go`, `rootcopy_test.go`; B-01). `CopyFromContainer` and `CopyToContainer` run in the daemon as root and resolve a path's parent folders through links inside the container. The worker owns its home and can make any folder there a link, so a lexical "is under /home/headless" check is not a boundary. Copy as the worker: tar inside the container as 1001:1001 with an empty environment. The root-side copies left are listed in `rootcopy_test.go`, each with its reason.
- **A heredoc built from text is not a safe way to write a file into a desktop.** The text can end the heredoc early. Send the bytes as a tar on stdin, with a constant script.
- **Killing processes by command-line pattern on a shared machine needs an owner tag** (W-04). A second control plane on the same PC (a dev one, a test run) ended the owner's live host-app helper, because every helper looked the same. Helpers now carry `-DecurixInstance <tag>`, and sweeps end only their own.
- **A path hash is not an instance id when every container has the same path** (RW1-C-03). Every containerised control plane has `/data`, so they all shared one tag. The tag is a random `instance-id` file now, made with an exclusive create.
- **An empty `instance-id` older than 5 s is a crash's leftover** (RW1-G-05). A crash between the create and the write left an empty file, and every later start fell back to the shared hash. It is now removed and made again; a fresh empty file is left alone, since another start may be mid-write.
- **A creation that fails must remove only what it made.** "Any inspect error means absent" deleted a live desktop. Volumes carry `decurix.creation=<id>`, and a creation refuses a home volume another creation made.
- **Docker's one-shot stats have no `PreCPUStats`.** A CPU % needs the previous sample.
- **A full stderr sink that returns an error ends `stdcopy.StdCopy`** (RW1-C-01). The archive on stdout is then cut, and a cut archive looks whole. Give stderr a sink that never fails, and check the archive's end.
- **Copies into one paused desktop decide one at a time.** Two copies each unpaused and re-paused it, and the second failed on "not paused". A per-desktop lock, also held while re-pausing, makes the second join the first's unpause. A desktop resumed from outside meanwhile is re-read, copied into, and not paused afterwards.
- **A paused desktop is a running desktop for the tools refresh** (RW1-G-02). The check listed only `status=running`, and idle-paused is the commonest state, so a refresh emptied `/opt/tools` under desktops that later resumed mid-task. Any state but exited, created or dead counts, and a failed listing counts as running.
- **Docker's `--filter name=X` matches substrings (and regex).** Select Decurix resources by label, or anchor the name (`^desktop_`).
- **A whole-`/dev` bind carries the host's `/dev/shm` and `/dev/mqueue` into a sandbox.** Bind device nodes one by one, and give the view its own `/dev/shm`.
- **The base image's Xvnc has no SECURITY extension, so it cannot give out an untrusted X cookie** (broker mode,
  2026-11-07). `xauth generate :1 . untrusted` said "couldn't query Security extension" against accetto's TigerVNC
  1.16.2 `/usr/bin/Xvnc`; Ubuntu's `Xtigervnc` 1.13.1 (`tigervnc-standalone-server`) has it, and an untrusted client
  of it sees only BIG-REQUESTS and XC-MISC. Installing that package stops at a dpkg conffile prompt (the base image's
  `/etc/tigervnc` differs; `--force-confold` keeps it) and takes over `/usr/bin/vncserver` by alternatives, which the
  default mode's startup runs: the link is put back (`/usr/libexec/vncserver`). As untrusted clients, `xfdesktop`
  exits (BadAccess on a root window property) and the panel's window list is empty; mousepad and the panel draw.

## Scripts and the Windows app

- **A fallback written to `.env` is a permanent downgrade.** One failed probe pinned every worker to a 3B model for good. Fallbacks are for one run only (a compose override with a "THIS RUN ONLY" warning); `.env` is the person's.
- **PowerShell 5.1's `Get-Content` reads `.env` in the ANSI code page.** Read and write it with `[IO.File]` and UTF-8 without a BOM.
- **A partial inventory is worse than none** (A-04). A Docker listing that failed half-way, saved as `preexisting`, would have turned into deleting another project's containers at uninstall. `maintain-existing.sh inventory` fails when any listing fails, and a record without `preexistingComplete: true` removes nothing.
- **A setting that only exists for an opt-in feature must not be required without it.** The build scripts refused to run without `cloudflared` even with the public link off. The check now follows `DECURIX_PUBLIC_LINK`.
- **GNU `sed` replacement text with `\L`-like sequences lowercases the rest of the line** (a path such as `lib/Lan-Tunnel`). Use a Python edit for lines with backslashes or such paths.
- **Git for Windows' grep does not see `\r` in CRLF files** (`grep -c $'\r'` gives 0). Run CRLF checks on Linux.

## Mail, schedules, phones and devices

- **A verified-only mail rule must write out the sender's domain** (B-15). Provider verification proves only that an email comes from its own From domain. An unanchored sender pattern also matches a look-alike domain that contains the pattern's text, whose owner can sign its own mail. The verified domain must equal the rule's named domain or be a subdomain of it (`billing@acme.com` or `@acme.com`).
- **Whether a mail rule's request is a slash command is decided from the rule's text, before the email's values go in** (B-16). In a command each value is one quoted token, or the job is skipped with nothing to send (a quote, `=`, a line break, or a space outside an option's quotes). A non-command rule whose filled text starts with `/` gets "Request from an email: " in front.
- **Outgoing mail headers are rendered from parsed addresses and parsed media types** (B-16). `net/mail` accepts a line break inside an address comment, so never paste the raw string into a header.
- **A mail poll commits each batch** (B-17), and mid-batch past 32 MB of files. Job files of jobs that no longer exist are deleted in prune and on every delete path.
- **Only a fault in the email itself counts against its three tries.** A NO or BAD answer, or a reply that cannot be read, counts. A reset link, a timeout or a cancelled poll fails the poll, which is retried with its backoff. Counting only server refusals would stall the mailbox on an email the parser cannot read.
- **A link fault that one email causes recurs forever at that UID** (RW1-G-01). A 20 MB scan on a slow office link failed the 90 s session every poll, was never counted, and stalled every email behind it. Link faults at the same UID on 10 polls in a row now pass it over with a failed job ("may be too large for this link").
- **An SMTP QUIT failure after the server accepted DATA is a sent email** (B-28). Returning it as an error sent the mail twice.
- **Attachments an email carried but that were not kept are named** (B-28): over 20 files, cut short, empty, or over 25 MB. They are listed in the request under "NOT on this desktop".
- **A schedule's failed run is consumed but keeps `last_error`** (B-28). TIME is the occurrence the run is for, not the tick. Cron (B-37): a day field starting with `*` is unrestricted for the day-of-month/weekday rule (vixie), `N/S` runs from N to the field's end, and 7 is Sunday.
- **An extra device token is valid only while its device row exists** (B-18). `AddToken` is one `INSERT ... WHERE EXISTS`. `Revoke` is one transaction and calls `OnRevoke` hooks, which close the phone's live link (`phonehost.Hub.Drop`).
- **`decurix --phone` never sends a saved token to another control plane address** (B-18). A new address pairs again with the link's code.
- **Phone viewers pass only ordinary typing and navigation keycodes** (B-05; `phonehost.OrdinaryKey`, an allowlist). System keys are refused at the control plane and on the phone. The approval gate sees an agent's clicks on the viewer in the VM, not input sent to the phone link directly; the input broker (O-1) closes that.
- **Phone setup never turns on unencrypted network debugging** (B-26). It is open on every network until reboot. Setup stays on Wireless debugging, whose port can change with the network, and keeps the new serial.
- **A setup one-liner checks what it runs** (W-13). The script's SHA-256 travels with the setup token, and the script checks the helper binary's. Over plain http this does not protect the page itself, and the page says so.
- **Person-facing validation errors are typed** (B-37). Mail-trigger and schedule mark them `InputError`; httpapi shows only those in their own words.

## The control plane's HTTP surface

- **A name-based walker is a net, not a proof** (RW1-G-03, RW1-G-06). The no-echo test missed error text kept in a variable, `Sprintf` of an error, writes to a differently named `ResponseWriter` and `json.NewEncoder(w).Encode`. Its sentinel exemption also let a struct field such as `p.Err.Error()` through, until it was limited to qualifiers that are the file's imports. An error variable not named like one, or a helper that returns the text, still gets past it.
- **An identifier-labelled figure larger than every money figure makes the amount unknown.** The amount policy then applies, so a payment that names a larger account or invoice number meets its amount policy, and the card says the amount could not be read for certain.

## The agent: checks, gates and evidence

- **A check's own `except: return []` is a pass** (E-01). Run every check through the one wrapper (`long_task._checked`), so a raise is NOT_MADE.
- **"Could not read" is not "none"** (R-1). A typed-values record or an Outbox that could not be read returned an empty list, which read as "nothing typed" or "no drafts", and the check passed. Raise, or return None, and treat it as NOT_MADE.
- **A word list that unblocks or refuses is a bug that comes back** (X-8). The source guard's verb list, removed records, sources, missing files and placeholders each came back in a new spelling. Keep mechanical fact in code, give meaning to the judge as evidence, and put disagreement to the person.
- **A word list that un-gates a committing verb is a hole** (D-04). Exemptions come from role and container: only a link, a tab or a navigation item can be a name.
- **A fallback that turns an unread reply into a yes grants changed actions** (F-02). With no reading, the reply is unclear and is asked again.
- **A retry after a stale node is a second action** (D-06). Judge it again on the page as it is now.
- **Healing onto another name is acting on another record** (D-09). VERIFIED skips the judge, so a repaired run is unverified.
- **A value with a slash is not a path** (R-2). Replay read-back treated any value with `/` or `\` as a file and read back only its last part, so a date read back from any year on the page, and a run could end VERIFIED. Read the whole value; use a file name only for a parameter given to an upload step or a file field.
- **A digit string compared as a float is not an identifier** (R-3). A different 18-digit account number passed, and a leading-zero reference passed on the number without it. A number that would lose information as an amount (a leading zero, a leading `+`, more than 15 digits) is compared digit for digit as a whole token; amounts compare as `Decimal`.
- **A substring replace of a file name rewrites other files** (D-36). Binding one file renamed another whose name contained it. Rename a file only where its name stands whole.
- **Never decide what a table is (a payment file or not) from its headers** (F-01). Decide each identifier by its provenance.
- **A reply file is a channel, not a person.** The command folder is the worker's, so an approval-class answer needs the gate's grant too (`gated_command.granted`).
- **`engine_returned` runs in a `finally` and on the reply thread.** An exception there reached nobody and left a workflow stage pointed at, swallowing the next task's report. It now never raises.
- **`os.scandir` order is the filesystem's.** Sort before a cap, or Linux and NTFS see different subsets.
- **`binding.held_by` compared amounts sign-blind.** The minus was dropped only from the texts, so negative values were never held.
- **A quoted-name regex must not open on an apostrophe.** "today's payments in ledger.xlsx" swallowed the file name, and the source guard never saw the file (`source_guard._REQUESTED`).
- **A sum that ends a sentence was never checked.** `verify.false_sums` decided nothing when the total ended with a full stop ("= 1143.00."), the commonest way to write a sum. Indian digit grouping (1,00,000) and ₹ are notation, and it reads them too (E-24).
- **Scientific notation counts as a program's number only with its sign** (3E+1, E-25). A bare 3E2 is an address or a part number.
- **Identifiers in sources are matched as one run of digits, never digits joined across cells** (E-28, provenance). A prefix is cut only after `+` or as a trunk `0`.
- **Lessons are the whole worker's (every client's)** (E-28). They are context for the model, never grounding for a run's values (`request_reading`).
- **Check copies of host files go in a folder per task and per full path** (E-31). Two files with the same name in two folders were one copy.
- **Every write of desktop state goes through `privacy.write_scrubbed`** (phase 2, round 3). `test_writers_withhold` forbids every other way of writing a file in the agent's package outside a short list. A new writer either calls `write_scrubbed` (`shapes=False` for a store whose values are compared later) or is added to the list with its reason and its forms.
- **Withholding by shape breaks machine state** (phase 2, round 3). When the writers first went through `write_scrubbed`, 185 tests failed: shape withholding read a workflow checkpoint's `"claim_token": "..."` as a token and withheld it, so every stage failed "requires its current claim token". A text pass over dumped JSON also took a quote along with a `"password": "..."` value and broke the line. Write JSON as an object (withheld string by string), and use `shapes=False` for stores of ids, tokens and compared values.
- **In broker mode the save path's question stayed with the screen user** (2026-11-08, live). The save runs in the
  screen broker as `decurixscreen`, whose `DECURIX_DIR` is its own: `unit_answers.ask` kept the unit question in that
  side's `runs/<task>/context.json`, the agent (the worker) never saw it, and the person got the engine's own
  unnumbered question, whose reply backs nothing. Any state the screen side writes for the worker must be in the
  caller route both ways (`gui_client.TASK_STATE`, `gui_client.mirrored`); a new kind of it needs its entry there.

- **A demonstration's corrections were learned as part of the procedure** (2026-11-08, ERPNext bill taught once).
  The person typed the bill number and date, then typed both again, and typed a line's item, then chose it again
  after Ctrl+A. Learned as recorded, the retyped pair read as two "rows" of a repeated part (`skills.repeated_rows`)
  and the line rows were not found, so a replay would enter every bill with the demonstration's lines. An entry
  superseded by a later one into the same field instance (role, name, page, table row, recorded box within 8 px) is
  dropped before generalising, and the later entry takes its place (`learning._drop_corrections`). The trap the other
  way: a loop types into the same field once per item. So only field presses and keys may come between the two (a
  button, link, commit or page change makes it a repetition), the later entry must overwrite (select-all) or repeat
  the value, and another field re-entered with another value in between marks a register's next line.
- **A demonstration's empty row, added and deleted, would delete a real line** (2026-11-08, the same demo). A Tab
  out of the last line made an empty third row; the save refused it and the person ticked row 3 and deleted it. On a
  three-line bill the replay writes out row 3 and those steps then tick it and delete it, and the ledger reads back
  header fields only. A `row N` no entry went into, selected and then followed by a non-field press, is taken out
  with the step that made it (`learning._drop_empty_rows`). Still left: the save that failed while the row was there,
  the press that closed its notice, and the save again.

## TheBot

- **Raw nomic-embed-text cosine does not separate relevant from unrelated facts** (T-19), as soul calls it, with no query or document prefix. Measured on 2026-11-03: relevant facts scored 0.44-0.69 and unrelated ones 0.28-0.56. A 0.6 floor dropped a real fact. No fixed floor works, so frames say "may be relevant". A real fix needs nomic's prefixes (re-embedding the stores) or a judge.
- **Tests can pin a fail-open as a feature.** Two TheBot tests asserted that a decomposed recovery with a failed step was verified, and two more pinned an unread vouch as a yes. Read what a test asserts before keeping it green.
- **A token estimate must never under-count** (N-W3-6). A byte-based estimate under-counted non-Latin scripts. Calibrate from `prompt_eval_count`, and keep the highest measured rate: a call served partly from the prompt cache reports fewer tokens than the prompt holds.
- **A file is the work's own code only when it was written from the command's own text and then run as the program** (N-W3-9). Any word equal to a written file counted, so a downloaded file read with `cat` passed off its figures as "values only the work's own code printed".
- **The trace's withholding and L1's secret test are one rule set** (phase 2). `memory.RedactSecrets` replaces exactly what `memory.LooksSecret` finds, and a test holds both to the same samples. A new secret shape goes into `secrets.go` once.
- **A turn id is the job's, not the operator run's** (phase 2). Counted per run, a plan with two operator steps in one job numbered both "<job>:1", and the run record joined one step's commands to the other's calls (`TestTwoOperatorRunsInOneJobShareTheTurnCount`). `llm.NextTurn` counts on the job's context.

## Brainiac

- **A stream that ends is not an answer that finished.** `bufio.Scanner` treats EOF as success, and a cut llama-server or Ollama stream came back as `finish_reason: "stop"`. Every backend now needs an explicit finish (`finish_reason`/`[DONE]`, Ollama's `done`), and `Complete` is checked again in the engine and the API.
- **`WaitGroup.Add` after the pointer was handed out races `Wait`.** An engine being retired must refuse new holds under the same lock that counts them (`Engine.Enter`/`Leave`/`Retire`). Drain never has a deadline: a CPU answer takes 20 minutes.
- **Landlock has no right for connecting to a pathname Unix socket, and a network namespace does not cover one.** Only a mount view without the socket hides it. The helper needs `CAP_SYS_ADMIN` in its own user namespace (`AmbientCaps`), and must clear it before it execs the model.
- **`pivot_root` from a tmpfs on /tmp keeps /tmp's real contents reachable at /oldroot/tmp**, so binds of paths under /tmp work. Read-only remounts of binds in a user namespace must keep the locked flags (nosuid, nodev, noexec, atime), or they fail with EPERM.
- **Pdeathsig fires when the creating thread exits.** Start children from a locked OS thread (`isolate.StartLocked`).
- **Run `go test -c` binaries for Linux sandbox tests from a private directory, not /tmp.** Before `ForProgram` granted only the program file and its libraries, the whole binary directory was granted.
- **A mount view hides `/proc/self/exe`, and with it the program's `$ORIGIN` and ggml's backends.** glibc expands the main program's `$ORIGIN` from `/proc/self/exe`, so llama-server's own `lib*.so` were in the view and still "cannot open shared object file". ggml lists a directory to find `libggml-cpu-*.so` (the program's, falling back to the working directory) and opens them itself, so no `DT_NEEDED` names them, and Landlock needs ReadDir there. A sandbox test whose program is the static Go test binary proves neither; run the live llama test (FIX-BR-1).
- **Margin 0 makes "B proven as good as A, and A proven as good as B" impossible.** The local "brain is as good and faster" branch was dead, and is gone.
- **A strict checker is not neutral in a comparison.** Misgrading the remote's verbose correct answers lowers its score and makes a cheap path look as good. Case prompts should ask for a final `Answer: <value>` line.
- **Chrome's own networking bypasses a Go dialer guard.** Use `--proxy-server` to an in-process guard proxy plus `--proxy-bypass-list=<-loopback>`. Checked on this host: without it, Chrome read a loopback page.
- **A path proven by name is not proven when routed** (R-3, W4). A specialist scored 99/100 on cases named for its field but would have received misrouted requests. Routed requests earn on `routed:<model>` scores, where a misrouted case is a miss.

## Tooling for the people and agents working here

- **Shell heredocs on this host collapse backslashes.** Git Bash and the Bash tool turned `\\n` into a real newline, `\` line continuations into nothing, and Python string literals' `\b` into a backspace, splitting Go and Python source. Write patch scripts with the Write tool (or a script file with raw strings), never through a heredoc.
- **`go test -overlay` with Windows paths failed** ("open :"). Swapping a file in briefly worked.
- **`go:embed` can fail with "unexpected length"** while another writer is editing an embedded agent file. Retry.
- **Run the module's suite before each commit, not only before the report.** One commit landed with a failing test and the next fixed it.
- **Patching one module through two names, then stopping in start order, leaks the mock.** `test_launchers` patched `launchers.time.sleep` and `replay.time.sleep`, which are the one `time` module. The second patch saved the first mock as the original, so the in-order stop put the mock back, and `time.sleep` stayed a mock for the rest of the run: a later test's sleep-based wait took no time. Patch a shared module once, and stop patches in reverse.
- **Do not have a model analyse evasion shapes for a guard.** A safety classifier stopped two agents three times while they prepared guard fixes from attack shapes. Write such fixes by input class and build their tests mechanically from the guard's own existing examples (O-1.a), or leave them to a person.
- **A test request from `httptest.NewRequest` comes from 192.0.2.1, which is not the host once the server has a
  desktop manager.** `fromContainer` then holds and `ActorContext` carries no actor: a reply is refused as "a desktop
  cannot answer its own question". Set `req.RemoteAddr = "127.0.0.1:..."` in a test that plays a person.
- **A test that times a handler or a subprocess against a wall-clock number fails under load with nothing wrong.**
  The async-create test held 500ms, and the icon test ran PowerShell (it compiles C# per call) under the 20s a
  creation allows. Assert the behaviour: hold the stand-in daemon until the test lets go and check the 202 came
  back meanwhile; give a test of output the test's own deadline and pin the production bound in its own test.
- **A download in a Dockerfile with no timeout can hang a first start for good.** An `apk add` held an ESTABLISHED
  connection to a mirror that stopped sending and waited 18 minutes, the network otherwise fine; the app sat at the
  step. Every download step now bounds itself (apt `Acquire::*::Timeout`/`Retries`, pip `--timeout --retries`,
  curl `--speed-limit/--speed-time --retry`, wget `--timeout --tries`, npm `--fetch-timeout --fetch-retries`, and
  `timeout N` with three tries for apk, `go mod download` and piped installers), pinned by
  `TestEveryDockerfileDownloadIsBounded`; the app cancels a build silent for 5 minutes (15 for the image builds,
  whose quiet compile steps print nothing) and tries once more (`desktop/electron/build-watchdog.js`).
- **One Alpine mirror can stall for one repository only, every time.** On the owner's network dl-cdn served
  main/APKINDEX in a second while community/APKINDEX opened and sent nothing; `apk update` in the WSL setup waited
  for good. A timeout alone only fails the start: every apk use now loops over Alpine's official mirrors under a
  timeout (`desktop/electron/wsl/apk-safe.sh`, the same list inlined in the Alpine Dockerfiles), and the app's
  watchdog covers setup.sh too (`TestEveryApkUseFallsBackAcrossMirrors`, `apk-safe.test.js`).
- **A timeout is not a retry, and a status sent is not a status seen.** A pip ReadTimeout from PyPI failed the
  desktop image (pip's own retries spent), and the start screen stayed frozen on a line from the middle of the build
  with no error and no Retry. Every download is now tried three times as a whole (10 s, then 30 s apart), the app
  tries a failed build once more, and the screen acknowledges each status: a failure it does not show within 10 s
  reloads it and opens a dialog with Retry; start failures are logged as `Start failed:` in desktop.log.
- **An action's summary is words, not who chose the entry.** A plain pick on a dropdown the page drew itself wrote
  `took the entry "X"`, the same words an identifier's match wrote, and the ledger read them as a resolution: the
  worker's guess backed itself (review round 9, R9-B1). Who chose an entry is now a field of the action's record
  (`resolved_by: "identifier"`, set only where resolve matched), with the entry taken and what the list offered;
  `records.resolved_names` reads that and `resolve.facts`, never a summary.
- **A select is a record's value only when its form was saved.** Counting every `gui select` of a task as a pick
  refused done to any task that filtered, paged or sorted a list first (R9-B2). `records.picks` counts the selects
  from the form's page until its save is answered as saved; a page left, or a select that moved the page, drops them.
- **A check that only the general gate makes is a check a planned run skips.** The ledger's checks ran in
  `skill_task._gate` only, so a planned run, a taught skill's read-back and a replay could still say done over a value
  no source gives (R9-B3). They are one function, `ledger_checks.findings`, and every route that can say done calls it.
- **A bounded recheck that ends in a re-run is a re-run postponed.** After two rechecks of a check not made, the third
  "I don't know that" ran the task again, and a skill run kept it as a lesson (R9-B4). It now ends not checked,
  quoting the question; the watcher leaves such a reply to the final check's reading, which keeps a lesson only from
  a reply read as a correction.
- **A form's value for a field is its latest, not every value it ever held.** `core._current_form` kept each value
  typed since the form opened, so an account typed wrong and corrected in place was still held against every save,
  and a worker doing what the refusal said was stuck (W0.3 slip probe, 2026-11-05). A field is one field by its path
  (else its label) and holds what was typed last.
- **A refusal nobody records is a refusal nobody learns from.** The pre-save guards raised before the action was
  recorded, so the task's events, its trajectory and its bundle never showed which guard stopped which save, or why.
  `core.act` now records a refused action (`outcome: "refused"`, `guard`, `reason`) before raising; the checks that
  read events read only done ones.
- **A file the worker's user can write is not the person's word.** Round 10 let `<task>.said`, a local
  `field-answers.json` and the skill file back a pick, and a forged line or row in any of them backed the worker's
  own guess (review round 10, R10-B1). The person's word now counts only as the control plane recorded it, read from
  the control plane (`person_record.py`). With no such read, it backs nothing.
- **"Some word of the option" is not "the words that pick it out".** "to" backed "Transfer to Reserve", and a bill's
  "General terms and conditions" backed a guessed "General" cost centre (R10-B2). Every word that tells the option
  apart from the other entries must be given, and a file counts only where it states that field.
- **A daemon started as plain `python3` reads the worker's home.** Unlike the gate, the agent was not started with
  `-I`, so a `usercustomize.py`, a `.pth` file in user site-packages, or a `PYTHON*` variable could load code into
  it. The agent and the recorder it starts now run `python3 -I`, and they put their own folder on `sys.path`
  themselves: from Python 3.11, `-I` puts no script folder there (review round 11).
- **A label's value is its cell, not its line.** "Cost centre: to be decided. General terms apply." held "General"
  on the label's line. A file now backs a pick only with the text after the label, up to the next sentence or column
  break (`records._value_cell`).
- **A cron in UTC is a conversion the person does in their head** (B-37). An IST user set `0 9 * * *` and got 14:30.
  A schedule and a DAG cron now keep an IANA zone (`zone`, `cron_zone`; old rows migrated to `UTC` so nothing moved),
  matched on that zone's wall clock (`Expr.NextIn`); the binary embeds `time/tzdata`, since Windows and a minimal
  image have no zoneinfo. Daylight saving goes the vixie way: a fixed-time job in the skipped hour runs at the jump, and
  once in the repeated hour; a wildcard job follows the clock. TIME:{...} carries the zone's offset, so a 00:30 IST
  run says that day's date, not yesterday's UTC one.
- **A page that lists desktops must render when Docker is down** (2026-11-07). The dashboard, Requests, Triggers and
  DAG pages answered "internal server error" while Docker was unreachable (the Windows app's distro still starting), so
  the Docker banner that explains it never showed. They now render with no desktops (`httpapi.pageDesktops`,
  `TestPagesRenderWhenDockerIsDown`); a JSON route may still fail, a page may not. Found only by running a freshly
  built control plane with no Docker, which unit tests with a fake daemon never do.
- **A process that imports once reads its settings once** (gui daemon, 2026-11-07). `cli.TEXT_BUDGET` was read from
  `DECURIX_GUI_TEXT_CHARS` at import, and TheBot sets it on every command: a daemon that imported `cli` once would
  have printed every command at the daemon's own budget. It is read per command now (`cli.text_budget`), and the daemon
  finds every other import-time read in the imported sources (`daemon.import_time_reads`) and re-imports the agent's
  modules in a child whose command differs. Also: a daemon started with `&` from a non-interactive shell has SIGINT
  ignored, and Python then installs no KeyboardInterrupt handler, so each forked child sets
  `signal.default_int_handler` back, as a `gui` process has it.
- **A Unix socket's file is not the socket's inode** (gui daemon, DevTools relay, 2026-11-07). "Unlink the socket only
  if it is still mine" compared `os.stat(path).st_ino` with `os.fstat(listener.fileno()).st_ino`; on Linux the fstat is
  the socket's inode in sockfs, never the file's, so the two never matched and an idle daemon left its `gui.sock`
  behind (Windows, where these tests skip, never showed it). The file's (st_dev, st_ino) is kept at bind now
  (`Server.bound`, `cdp_relay._BOUND`). Found with it, also Linux only: the daemon's idle time counted from before its
  imports (`last_call` now starts in `serve`); a test that wrote 300 KB into a socketpair before reading it blocked
  forever (Linux buffers about 200 KB); and a send on a DevTools connection the relay had closed raised a raw
  BrokenPipeError instead of the ComputerError a read gives (`cdp._Socket.send`).
- **The browser's download bubble moves the next action's time** (typing and download waits, 2026-11-07). A bench that
  clicked a download link and then, at once, typed and clicked Save measured Save slower with a change that took 0.25 s
  out of it: after a download Chrome's toolbar animates and its bubble closes by itself seconds later, and wherever that
  lands the settle wait (screen unchanged for 0.35 s) is longer. Making the earlier actions faster moved the animation
  into a later one. Time actions after a download with an untimed pause of several seconds, or in their own block.
- **An untrusted X client gets nothing of the root window: no property write, no event** (broker mode, 2026-11-07).
  The SECURITY extension lets an untrusted client read the root's properties but not change them (BadAccess) and
  delivers it no event on a window of a trusted client, the root included. So xfdesktop exits at its first root
  property, the panel's window list never fills (it reads `_NET_CLIENT_LIST` once and never hears it change), and
  at-spi-bus-launcher, which names its bus on the root window when it has a DISPLAY, died - leaving none of the
  worker's apps an accessibility bus, with only a "NoReply" warning per app in the log. Start the worker's session bus
  without DISPLAY and start the accessibility bus on it before giving DISPLAY to its activations; do not run a desktop
  manager as the screen user instead (its menus start programs as that user, past the gate).
- **A plan's "with your OK first" lived only in the question's prose** (live trial, 2026-11-08). The operator's plan
  question said "Then, each with your OK first: Press Save bill"; the gate asks only for Send/Pay-like controls, so the
  worker pressed Save bill after its own pre-save check and the person was never asked. A promise made to a person must
  be data the executor reads: the plan's commits now carry their control and the gui command holds that press for the
  gate (`DECURIX_PLAN_ASK_FIRST`, `core._plan_hold`). And a variable the screen side must see is read by its literal
  name (`os.environ.get("NAME")`): the broker passes only names the agent's code names that way, so a constant would
  have dropped it in broker mode. In the same trial a halted `gui do` was not credited with the figures it had typed
  (soul read only "Typed ..." lines): a replay now gives back its own `entered` list however it ends.
- **A widget on the worker's accessibility bus is the worker's word, box included** (broker mode, 2026-11-07). Once
  the screen broker reads the worker's bus, the worker can lay a made-up "Cancel" over the browser's "Pay"; a press by
  its box lands on the browser while the gate is asked about "Cancel". Act on such a widget only where the window shown
  at that point is the worker's own (its `_NET_WM_PID`, which only the window's client can set, and that process's uid).
- **A command the gate held still did what came before the held press** (2026-11-08). `gui type ... && gui click
  Save` held at Save had typed, but a held command recorded nothing: the review read the form off the look and the
  screenshots from before the typing and said filled fields were empty. Note a held chain's typing, order looks and
  typing by commands run (held ones included), and give a review only the latest look of a window, or "not read".
- **A bare figure declared from a source that writes it with a unit settles the unit silently** (2026-11-08). "18" from
  "18%" matched a unit-less field's 18, and the unit question never came. A declaration keeps the unit its backing
  writes (`records._source_units`).
- **A carry-state eval sent the next case's request into the earlier case's open question** (2026-11-08, first
  learn-v1 run). A case that ended at an approval left it pending; the next request, saved a second later, was neither
  its reply nor "after" it by the agent's one-second stale-question rule, so it queued until its case timed out. Between
  a learning curve's cases the runner now settles the desktop (`desktop.SettlePractice`: the open task cleared, the
  learned state kept, restarted), and a desktop's restart no longer counts its own earlier memory reservation.
- **A locked xfce4-panel still starts programs** (2026-11-08, broker mode's window list). Run as the screen user, a
  second panel holding only the window list looked safe once locked, but xfce4-panel 4.18 keeps Log Out, Help and
  About in its menu whatever is locked, and the window list's own button menu has "Launch New Instance", which runs
  `/proc/<pid>/exe` of the window's `_NET_WM_PID` - a property the window's own client (the worker) sets. The screen
  side's window list is its own small program instead (`computer/windowlist.py`): no toolkit (GTK loads the modules
  the XSETTINGS manager names, and that is the worker's xfsettingsd), no menu, no keyboard, nothing it can start. Its
  window must select button presses too, or the window manager's grab takes the release and a click does nothing.
- **Words in a record's number field were "backed" because the document printed them there** (2026-11-08, first
  learn-v1 run, bill 4). "Invoice number: as per covering letter" made the phrase stand in the source, so `gui record`
  kept it as the bill's number and nothing asked. A number, reference or id field holds a code: words with no digit
  there are refused at the declaration, the save and the final check unless the people's own words give them
  (`records.wordy_identifiers`). A record curl'd to the application's endpoint is never declared or read back: the
  final check holds such a send against the done (`records.sent_around`).
- **A cut read was judged as the whole value** (2026-11-08, learn-v1 bill 1). The form readers kept a value's first 80
  characters with no mark (`cdp.FORM_FIELDS`'s `v.slice(0, 80)`, `core._page_fields`, the Enter path), and
  `records.presave` compared that part with the declared value, so a declared 126-character Notes field was refused at
  every save ("Notes holds '... Taxable val' where the record says '... Taxable value: 3,600.00; ...'") until the token
  budget ran out. The same bug as the 80-character cuts of 2026-10-25 (part 5): a reader that cuts must say so. Values
  are now read whole up to `cdp.FORM_VALUE_CHARS` (600, what approvals quote); a longer one is cut at a word and marked
  (`found["cut"]`), and presave compares a cut value only as far as it was read (`records._read_in_part`): a part that
  agrees is undecided and left to the read-back, never "differs". A declared value is never kept cut either
  (`records.MAX_VALUE`, 4000: longer is refused, not silently cut as at 400).
- **A rule the person taught backed nothing in the next task, and an eval expert's words backed nothing at all**
  (2026-11-08, learn-v1 bills 3 and 4). Three gaps stacked: the person's words were read for the current task only
  (`person_record.answers`), so a value derived from a rule taught two tasks before was "backed by nothing a person
  said"; on a practice desktop the control plane is the broker, which answered 404 to the judgement read while the
  runner sent each expert reply with no judgement id, so even a reply on the same task backed nothing; and a code among
  codes sharing their letters ("FND-3" beside "FND-8") could never be picked out by any text, since its only telling
  part, the figure, is joined to the letters and was dropped as the code's own part. Now: the person's answers on any
  task of the worker are a source (`person_record.worker_words`, `?scope=worker`), the broker keeps and serves the
  expert's replies (`Broker.KeepReply`, actor kind `scripted`), and a code written whole picks out its entry
  (`records._code_written_whole`). Check a provenance rule on the eval's own route too: a source the broker does not
  serve is silently empty there, and fail-closed hides it.
- **A saved record declared again under its own number became a second record** (2026-11-08, learn-v1 bill 3). The
  worker keyed the bill "bill", saved it with a field undeclared, then declared that field under the bill's number;
  `declare` found saved records by key only, so the ledger held two records for the application's one and the eval's
  `ledger_readback` failed. A key that is a saved record's own number (an identifier field's value) is that record
  (`records.saved_by_number`).
- **The agent's start scripts ran the worker's own `python3` in the agent's place, and every program had the token**
  (2026-11-08, probe on a broker-mode desktop). The container's PATH puts `~/.local/bin` first, and
  `start_agent_terminal.sh` and the worker's session looked `python3`, `nice` and `setsid` up on it: a `python3` left
  there started as the agent at the next boot. The token was in the environment of 14 processes of the worker's user,
  each readable at `/proc/<pid>/environ`, and a session-bus service file in the worker's home ran at boot. A script
  that starts a holder of a secret looks its commands up on the system's PATH and starts the holder by absolute path;
  a secret reaches its holder in a file it reads once it is non-dumpable, never in an environment
  (gate-boundary.md, "The agent token's two holders").
- **A reply carried without its question read as the person's standing word, and a sent-back job lost every reply**
  (2026-11-08, learn-v1 bill 2). A learned method kept the person's answer to bill 1's plan question alone ("I don't
  know that ... Do not make it up"), and bill 2's worker and review read it as said about bill 2; the review called a
  cost centre the person gave in bill 2 made up. Bill 2's final check had sent it back, and the new job started a goal
  of its own in soul, so its review had no "what the person said" at all. A person's word travels with the question it
  answered (`methods.keep` keeps `{asked, said}`), and every job carries the task's confirmed replies (`earlierSaid`).
  The worker also had no way to back a typed value with a reply: it wrote the reply into a file of its own to declare
  from. The person's confirmed words are now among what a declared value may stand in (`records._elsewhere_text`).
- **The final check imposed its own idea of the work over the person's taught rule, and the worker obeyed it: a false
  done** (2026-11-08, learn-v1 recon 5, repeat 2, trajectory `xcb425599da9f1c9e`). The person had taught "one row per
  statement line" on an earlier task; the judge was shown only this task's words, said not_done for a book entry with
  no row, the worker added that row citing the check, and the second judge - shown the rule only through the worker's
  citation - passed the seven-row file. The check is shown the person's words on the worker's earlier tasks with
  their questions, and the worker sent back is told the person's words hold over the check's finding.
- **A host app's viewer count could reach its helper out of order** (2026-11-08, found reading the code while chasing
  the CI flake 1,0,2,1,0 in `TestTheHelpersCaptureFollowsTheViewers` - which was itself the test's log shared by two
  fake helpers, the warm one's readiness ping between the viewers; the test now reads one helper's lines). `winhost.syncViewers` counted under the session's lock and sent after releasing it, so two changes at
  once sent their counts in either order and the helper could be left told 0 with a person watching: its capture stops
  and their screen freezes. The count and its send are one step now (`viewersSend`). A count, a state or a version sent
  to another process after its lock is released can arrive stale; send it under a lock of its own.
- **A learned method kept the plan's first idea of the output and only the plan question's answer** (2026-11-08, an eval
  run's third repeat of a reconciliation). The plan's question got "I don't know"; the layout (a CSV, one row per
  statement line) came in answer to a later question; the method kept the plan's workbook and the "don't know", and
  the next task of the kind wrote the workbook, asked nothing, and was passed. A method keeps every answer of the run
  with its question, and offers them as holding over its plan where they differ (`methods.keep`,
  `guidance.exchanges_this_task`).
- **A formality the worker skipped cost the whole case** (2026-11-08, validation run e53e9a9df7f70755a, bills 2 and 3).
  The worker saved the cost centre the person's answer gave without declaring it on the record; the final check
  rightly held a value no declaration covers and sent the task back, and the attempt sent back started over and ran
  out of time. Where code can establish what a declaration would (the value stands in the record's document or the
  person's confirmed words), it takes the value onto the record itself (`records.adopt_backed`); only what nothing
  backs is sent back.
- **The offline oracle could not drive a mock that serves several apps by host name** (2026-11-08, the office
  suites). It posted to the mock's address with no Host, kept no cookie between steps and set one value per field,
  while live `curl $MOCK_ROUTE` keeps the URL's host, `-b/-c` keep a sign-in and a repeated `--data-urlencode` sends the
  field twice; and a `curl` line with no data was skipped offline as a read, though live it is a GET that a
  button's form refuses. `offlineOracle` now sends the URL's host, keeps a cookie jar and adds repeated fields, and a
  scripted solution's bare button post sends one field (`make_office_suites.curl`).

### A taught task could never be trusted, so it never got cheaper

Since independent checks became the only ones that count towards trust (2026-11-03), a taught form task needed its
record read back from the application - and only the model declared records (`gui record`). On four ERPNext bills
Kimi never declared one, so every verdict rested on the model, no run counted, and the worker stayed an apprentice
doing every field by hand (48-98 turns, 1.3-4.2M tokens a bill). An instruction in the prompt is not a guarantee:
what the harness needs for correctness, code does (`skill_task._declare_taught_record`). And a PDF table cell wrapped
over lines is still the document's value (`binding.wrapped_in`); the exact-text check dropped every bill's item.

### A save the person had to repeat was learned as two saves and a press on a notice

The demonstration's first Ctrl+S was refused (a "missing fields" notice), the person closed the notice with an unnamed
button and saved again. Learned as steps, a replay's first save holds, nothing appears, and the replay stops at the press
meant for the notice. The same commit pressed twice with only that dismissal between is now kept once, the last
(`learning._drop_retried_commits`).

### A bill's table lines were folded as a loop, and its dates were never the document's

Two blockers found by dry-running the new taught-task route on the real ERPNext demonstration before a live run spent
30 minutes on them. (1) Line 2 began with "Add row" and line 1 did not, so the loop finder took line 1 for the shorter
first copy of a per-document loop; `each` dropped line 1's quantity and rate and fixed every quantity at the demo's 1.
Repetitions whose entries each go into one table row, no two into the same row, are now no loop
(`learning._rows_of_a_table`): the form's rows (`skills.repeated_rows`, `fit`) write out one per document line. (2) The
bill writes "09-Oct-2026", the form takes "09-10-2026": the fill's strict check dropped every bill's date, so every run
went to the model. A digit date is now held when one of its readings (the application's order when known) is a date the
document writes, and the record declares the document's own token (`binding.date_in`).
