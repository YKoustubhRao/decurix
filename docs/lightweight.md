# A lightweight Decurix: many desktops on one ordinary computer

The goal: a person with an 8 GB laptop runs several workers at once and every one of them feels quick. Correctness
still comes first (CLAUDE.md): nothing here trades judgement for speed, and every change is measured before it is
kept.

## Where the time and memory go (baseline, 2026-11-05, this host)

Measured by the resource bench (`bench/resources/run.py`) on the 8 GB Windows host, WSL capped at 5 GB, image
`decurix-desktop:basic` at e1349dd.

| What | Cost |
|---|---|
| An idle desktop, no browser | 404 MB in its cgroup, 187 MB of it anonymous (agent 40, other 35, panel 25, X 23, websockify 22, xfdesktop 20, gate 14, soul 9) |
| The same with Chrome open | 627 MB (Chrome 206-240 MB) |
| A paused desktop | the same memory as a running one: pausing frees CPU, not RAM |
| One `gui` command | p50 2.3 s, p95 4.1 s; about 0.9 s of its own CPU, most of it starting Python and importing the screen stack again |
| `gui observe` | p50 4.1 s |
| The dashboard, one desktop | 2 requests a second while visible (7 separate polls a card) |
| Room on the docker host | 4.3 GB free with no desktop: about 6 idle desktops, 4 with a browser each, before Docker refuses |

So two things make Decurix feel heavy: every screen action pays for a fresh Python, and every desktop holds its
memory whether anyone uses it or not.

## Targets

- A `gui` command: p50 under 0.8 s, and its CPU cut by half or more.
- An idle desktop: 60 MB less anonymous memory.
- **Density:** as many desktops as a person asks for, with no more resident than the memory holds. Idle ones give
  their memory back and come back on their own when they are given work.
- The dashboard: one request per sweep for all cards, however many desktops there are.

## The changes

Each is its own commit, measured on the bench before and after. Image changes reach only new or recreated desktops.

| # | Change | Expected | Where |
|---|---|---|---|
| L1 | **The `gui` daemon.** One long-lived process per desktop, in the worker's user, keeps the screen stack imported and its connections open; the `gui` command hands it the arguments over a Unix socket and prints its answer. Arguments, exit codes and output stay byte-identical; with no daemon, `gui` runs in-process as today. Every consequential action still asks the gate through the gate's own socket, so the daemon holds no yes. | `gui` p50 2.3 s to under 0.8 s; most of the 0.9 s CPU a call gone | `agent/gui`, `decurix_agent/computer/` |
| L2 | **Memory goes back from idle desktops.** A desktop paused past a longer idle time, or the longest-idle paused desktop when a start or wake would not fit, is stopped (its home volume and everything on disk stay). It starts again by itself when it is given work, as today. A desktop busy with a task, recording, viewed, or holding an open question is never stopped. | Room for any number of workers; resident ones fit in memory | `internal/idlepause`, `internal/resourcemon`, `internal/desktop` start path |
| L3 | **No websockify in the desktop.** The control plane bridges the screen's WebSocket to VNC itself (Phase 2 C); the desktop's own websockify is now unused. | about 22 MB a desktop | entrypoint, image |
| L4 | **One dashboard poll for all cards** (follow-up W-14): `/api/desktops/overview` returns what the seven card polls read, for every desktop, in one answer. | requests a sweep from 7×N to 1 | `internal/httpapi`, `web/templates/index.html` |
| L5 | **The browser keeps less in memory**: Chrome's Memory Saver discards tabs not used for a while (never the active one), by policy. | tens of MB with several tabs | `agent/chrome-policies.json` |
| L6 | **No Claude Code CLI in the tools volume** once the legacy harnesses are gone (O-4): new desktops run TheBot only. | the CLI's install out of every tools volume | `docker/tools.Dockerfile` |
| L7 | **A density bench**: start desktops until the host's floor and report how many fit, idle and with a browser, and how long each start and wake took. It is the number L2 and L3 must move. | the measure | `bench/resources/run.py --only density` |

## Not done, on purpose

These save memory or tokens but cost correctness or safety, and stay rejected (the 2026-11-04 resource rules):
smaller or low-detail screenshots, fewer judge pictures, the fast model for judging, reviewing or filling, merging or
dropping the reviewer or the final check, summarising instead of masking, 16-bit colour, one soul for every desktop,
and KSM (shared pages between desktops are a side channel).

Swapping desktops stays off (`MemorySwap` equals `Memory`): a swapped desktop is slow when a person looks at it. L2
stops instead, which is honest about the cost: a stopped desktop takes a few seconds to come back and its open windows
are gone, so it is only done to a desktop nobody is using.

## Result after L8 (measured 2026-11-07 16:00, same host, the final image of this tree)

| | before (image of 2026-11-05) | after |
|---|---|---|
| idle desktop, no browser: anonymous MB | 162 | **140** (-14%: no websockify, no gui daemon while idle) |
| idle desktop, Chrome open: anonymous MB | 282 | 291 (the gui daemon was running: the bench had just used gui) |
| gui observe p50 | 3477 ms | **1553 ms** (-55%) |
| gui click p50 | 2235 ms | **1601 ms** (-28%) |
| gui read / verify p50 | 350 / 345 ms | **234 / 210 ms** |
| gui type p50 | 1683 ms | 1513 ms |
| gui p50 / p95, all 21 commands | 1621 / 3488 ms | 1496 / **1617 ms** (p95 -54%) |
| gui CPU per command, container p50 | 476 ms | 577 ms (observe now looks every 0.4 s instead of sleeping 3 s: faster, a little more CPU) |
| density, idle, cap 8 | 8 fit | 8 fit (the cap, not the memory, stopped both runs) |

Not reached: the 0.8 s p50 target. What remains in a click or a type is the settle wait (screen unchanged for 0.35 s,
up to SETTLE_SECONDS) and two looks; making those faster needs a cheaper "has the screen changed" signal than a full
screenshot, which is the next item if speed is still the complaint.

## Status (first measurement 2026-11-07, before L8)

| | before | after |
|---|---|---|
| idle desktop, no browser: cgroup / anonymous MB | 173 / 162 | 375 / 175 (the cgroup figure includes page cache from the fresh image build; anonymous memory is the measure) |
| idle desktop, Chrome open: anonymous MB | 282 | 293 |
| websockify | 22 MB | gone (L3, checked live: no process, the screen reads through the control plane) |
| gui daemon | none | +30 MB per desktop while it runs (L1) |
| gui p50 / p95 over 21 commands | 1621 / 3488 ms | 1507 / 3352 ms |
| gui read, verify p50 | 350, 345 ms | 225, 217 ms |
| gui own CPU / container CPU p50 | 361 / 476 ms | 17 / 378 ms |
| density, idle, cap 8 | 8 fit, 188 MB of host memory each | 8 fit, 196 MB each |
| desktop ready / wake from paused | 5.0 s / 0.23 s | 5.0 s / 0.23 s |

What this says, plainly:
- **L1 did less than planned.** The daemon saves the Python start (about 130 ms and a fifth of the CPU of each call)
  but costs about 30 MB per desktop, so idle memory went up, not down. A profile of one call on a live desktop showed
  where the time really is: `gui observe` on a small page spent 3.0 s of its 3.7 s in one fixed sleep (the "page still
  drawing" second look), and `gui click` 1.0 s waiting to see whether a download began after every action, even
  outside the browser. The real work is about 0.5 s. L8 below replaces those waits with signals, and the daemon now
  starts on demand and exits when idle, so an idle desktop does not carry it.
- **Density was already good**: the earlier resource work (one Python, no preloaded model, policies) brought an idle
  desktop from about 404 MB (2026-11-05 baseline) to about 175 MB, so 8 idle desktops fit in the 5 GB VM with 2.6 GB
  to spare. L2 (memory given back by stopping long-idle paused desktops, and making room on a start) is what lets a
  person keep more workers than fit; it was checked live (table below).
- **L6 saves nothing**: the old tools image's `claude` was a dangling link (the installer left the binary in /tmp), so
  removing the install changed only the build.
- **L4** (one dashboard poll) cut the visible tab from about 93 requests a minute to about 24; it is not visible in
  these rows (the viewer section was not rerun).

| # | Change | State |
|---|---|---|
| L1 | gui daemon | built; on demand with idle exit (L8) |
| L2 | memory back from idle desktops | built; checked live: a desktop paused 30 min was stopped (`rest_reason: idle`); with memory held by another program, a start stopped the longest-idle paused desktop first (`rest_reason: memory`, "stopping prof to make room for l2new (839 MB free, 1212 MB wanted)"); with nothing to stop, the start was refused in the person's words |
| L3 | no websockify | built, checked live |
| L4 | one dashboard poll | built, tested |
| L5 | Chrome Memory Saver | built; policy checked live in the image |
| L6 | no Claude CLI | built; saved nothing (see above) |
| L7 | density bench | built, run (rows above) |
| L8 | no fixed waits in observe and click; daemon on demand with idle exit | built; measured above; daemon and broker tests pass on Linux |
