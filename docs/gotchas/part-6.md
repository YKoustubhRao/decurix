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

## TheBot

- **Raw nomic-embed-text cosine does not separate relevant from unrelated facts** (T-19), as soul calls it, with no query or document prefix. Measured on 2026-11-03: relevant facts scored 0.44-0.69 and unrelated ones 0.28-0.56. A 0.6 floor dropped a real fact. No fixed floor works, so frames say "may be relevant". A real fix needs nomic's prefixes (re-embedding the stores) or a judge.
- **Tests can pin a fail-open as a feature.** Two TheBot tests asserted that a decomposed recovery with a failed step was verified, and two more pinned an unread vouch as a yes. Read what a test asserts before keeping it green.
- **A token estimate must never under-count** (N-W3-6). A byte-based estimate under-counted non-Latin scripts. Calibrate from `prompt_eval_count`, and keep the highest measured rate: a call served partly from the prompt cache reports fewer tokens than the prompt holds.
- **A file is the work's own code only when it was written from the command's own text and then run as the program** (N-W3-9). Any word equal to a written file counted, so a downloaded file read with `cat` passed off its figures as "values only the work's own code printed".

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
- **Do not have a model analyse evasion shapes for a guard.** A safety classifier stopped two agents three times while they prepared guard fixes from attack shapes. Write such fixes by input class and build their tests mechanically from the guard's own existing examples (O-1.a), or leave them to a person.
- **A test request from `httptest.NewRequest` comes from 192.0.2.1, which is not the host once the server has a
  desktop manager.** `fromContainer` then holds and `ActorContext` carries no actor: a reply is refused as "a desktop
  cannot answer its own question". Set `req.RemoteAddr = "127.0.0.1:..."` in a test that plays a person.
