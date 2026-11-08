# Work queue: a workforce, not one desktop

Before the queue, work went to a desktop a person named, one request at a time. The queue holds work for the whole
workforce: an item says what must be done and, optionally, what kind of worker may do it; a dispatcher hands it to an
idle worker that qualifies, within that worker's capacity and spend limits, and follows the task to its end.

Code: `internal/workqueue` (store, dispatcher, HTTP handler, `TaskSpend`). It imports `internal/sqlitedb` only; the
adapters that connect it to desktops, task records and the skill registry live in `internal/httpapi`.

## The model

**Item** (`workqueue_items`, files in `workqueue_files`; component `workqueue`, append-only `Migrate` steps)

| Field | Meaning |
|---|---|
| `id` | `q` + 16 hex |
| `request`, `files[]` | what to do (at most 64 KiB), and files written into `/home/headless/work/inbox/<id>` on the worker before the request is sent (the request then names them). At most 20 files, 25 MB together |
| `required_skill`, `required_role` | optional: the skill registry must record the skill as installed on the worker / the worker must carry the registry role (both, when both are given; case-insensitive). Access-store roles (docs/access.md) do not count |
| `worker` | optional pin: only this worker may take it |
| `priority` | higher first; equal priority goes oldest first |
| `not_before` | not sent before this moment |
| `status` | `queued`, `assigned`, `running`, `done`, `not_done`, `failed`, `cancelled`, `held` |
| `hold` | why it is held: `budget` (released automatically) or `interrupted` (released by a person) |
| `assigned_worker`, `task_id` | where it went and the ordinary task record it landed under |
| `attempts`, `max_attempts` | sends that reached the submission path (default 3, at most 10) |
| `reason` | why it is where it is, in words a person can act on |
| `source` | free text: who enqueued it (a person, another system) |

**Capacity** (`workqueue_capacity`): per worker `max_concurrent` (default 1, at most 16) and `paused`. A worker
with no row takes one item at a time and is not paused.

**Spend limit** (`workqueue_limits`): per worker, or `*` for every worker without its own row, per `day` or `month`:
`max_usd` and/or `max_tokens` (0 leaves that measure uncapped; a row with neither is removed). `*` is a default for
each worker, not a total for the fleet. Days and months start at midnight in the dispatcher's `Location` (the host's
local time by default).

## Dispatch rules (one `Tick`)

Ticks never overlap. Each tick:

1. **Follows in-flight items.** For each `running` item it reads its task (`TaskStatus`):
   - still `queued`/`running`, or `waiting` on a person: stays `running` (the reason says when a person is needed);
   - `done` **and** the desktop's final check judged it (`checked`) - or a person marked the task correct: `done`;
   - `done` without a checked final: `not_done`, "a person should look at it". **No unseen success**: a final the
     gate did not judge never counts as done, however confident it reads;
   - a person marked the task wrong: `not_done`; `error`: `failed` (with the worker's first line);
     `query`/`abandoned` (ended while waiting on a person): `not_done`; task record missing: `failed`.
   An item still `assigned` at the start of a tick was being sent when the control plane stopped: whether the desktop
   got it is unknown, so it is `held` (`interrupted`) for a person to check that desktop's chat and retry or cancel.
   It is never sent again on its own.
2. **Releases budget-held items** for which some qualifying worker is under its limits again (a new day or month, a
   raised limit).
3. **Assigns queued items**, highest priority first, skipping those whose `not_before` has not come:
   - candidates: the pinned worker, else every desktop but practice and readiness ones; minus those lacking the skill
     or role, minus paused ones. None
     left: stays `queued`, reason says why ("no worker holds skill x", "worker w is not available").
   - minus those at or over a spend limit. None left: **`held` (`budget`)** with each worker's spend and limit in the
     reason. A busy worker under budget keeps the item `queued` instead - held means "money", not "wait".
   - the least-loaded candidate under its `max_concurrent` whose `Busy` is empty (work from a person, a schedule or a
     handoff counts) gets it. None free: stays `queued` with the reason.
   - the item is claimed (`assigned`, attempt counted), then sent through the ordinary submission path. A task ID
     back: `running`. A busy refusal (the error carries `DesktopBusy()`): back to `queued`, the attempt given back,
     nothing more to that worker this tick. Any other error: back to `queued` after a backoff (a minute), or `failed`
     once `max_attempts` sends have failed.

Items that cannot be placed do not block lower-priority items that can go elsewhere.

## Over budget

Spend is what the task records already hold: each execution's `cost_usd` and `input_tokens + output_tokens`
(`TaskSpend` sums them per worker since the period's start). **Tokens are metered by the control plane; dollars are
not** (review 2026-11-03). The model proxy meters tokens per worker per day (`llm_usage`, `llm.Hub.Metered`); a
worker's tokens are the larger of that count and its tasks' own reports. The proxy refuses a desktop's hosted call
while `Dispatcher.OverBudget` names a limit it is at or over, either kind: 402 over a limit, 503 when spend cannot be
read. Calls that do not pass the proxy (a Claude-harness desktop on the owner's Claude account, a local model) are
counted only by the desktop's own report. A **dollar limit is only the desktops' self-reported cost**
(no price is in code); the API (`enforcement` in `GET /api/queue/limits`), the form and the hold reason say so.
Done items' files are dropped after 7 days. Over budget, an item is **held with a reason, never
dropped**; it goes back to the queue by itself when a qualifying worker is under its limits again, or a person can
retry or cancel it. Spend is checked before a send, not during a task: a task that is already running may take a
worker past its limit; the next item then waits. Spend a worker made outside the queue (a person's chat) counts too,
since the limit is on the worker.

## HTTP API

Primary listener only (`:8080`, `RegisterLocalRoutes`). A desktop may not call it at all, not even to read (it is not
among the callback routes in `internal/httpapi/guard.go`), and `readsForAll` refuses a write from any container. The
queue starts work on desktops and spends the owner's model budget: a leaked access code on the tunnelled `:8090`
must not become a way to run work or raise limits, and a desktop's agent (which may be reading untrusted pages) must
not queue work for others or change limits. Programs on the host (another system integrating with Decurix) reach
`:8080` directly. Worker-to-worker work goes through handoffs (docs/handoff.md), not the queue.

| Route | |
|---|---|
| `POST /api/queue` | enqueue `{request, files:[{name, data(base64)}], required_skill, required_role, worker, priority, not_before, max_attempts, source}` -> 201 item |
| `GET /api/queue?status=&worker=&limit=` | items, newest first (`worker` matches pinned or assigned) |
| `POST /api/queue/cancel` | `{id, reason}`; a running item's task keeps running on its desktop, the queue stops following it |
| `POST /api/queue/retry` | `{id}`: `failed`, `not_done`, `cancelled` or `held` back to `queued` (attempts kept); never `done` |
| `GET/POST /api/queue/capacity` | list (`{capacity, default_max_concurrent}`) / set `{worker, max_concurrent, paused}` |
| `GET/POST /api/queue/limits` | list (`{limits, enforcement}`) / set `{worker or "*", period, max_usd, max_tokens}` |
| `GET /api/queue/spend?worker=` | `{day:{since, usd, tokens, limit}, month:{...}}` |

Bodies are JSON, unknown fields refused. Errors are curated reasons (`{"error": ...}`), never an internal error's
text. An enqueue, retry, capacity or limit change wakes the dispatcher at once; otherwise it ticks every 10 s.

## Limits of this version

- **Single host.** One control plane, one SQLite file, one dispatcher. Workers are this host's desktops.
- **One task per desktop at a time.** The submission path (`submitWhenFree`) refuses a desktop with any live task, so
  `max_concurrent` above 1 has no effect yet beyond a busy refusal; it is in the model for when a desktop can run more.
- **Cancel does not stop a running task**; stop it in the worker's chat.
- **Spend is per worker**, not per item or per fleet; there is no forecast of what a task will cost.
- **Eligibility is exact skill/role names** from the skill registry; no matching on what a request needs.
- **Not done is final until a person acts**: no automatic retry of a task that ended `failed` or `not_done` (a second
  run could double an action the first one half did).
- **The Workforce page's Queue tab** (docs/skillhub.md), primary listener only, lists the latest 100 items with
  Cancel and Retry, adds an item (request, skill, role, pinned worker, priority), sets each worker's capacity and
  pause, shows each worker's spend today and this month, and sets or removes limits. A worker whose model calls skip
  the proxy (a legacy Claude or Ollama desktop, a TheBot desktop on the local model: read from the desktop's
  `harness` and `brain`) shows "not metered", with the tokens its own reports gave, instead of $0.00. Files, `not_before` and
  `max_attempts` are API only.
