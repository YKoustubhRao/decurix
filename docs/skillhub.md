# Org skills and knowledge

A worker learns a skill on its own desktop (`~/.decurix/skills/<name>.json`, `agent/decurix_agent/computer/skills.py`), and the people who run workers tell them things. Before this, both stayed where they were made. Two control-plane packages share them:

- **`internal/skillhub`**: a versioned registry of skills. You publish a skill from the desktop that learned it and install it on others, by hand, by role, or with a sync.
- **`internal/knowledge`**: rules, facts, corrections and lessons, each scoped to the org, a role, a worker or a task, and each carrying its source. Nothing is edited in place. A change supersedes the old item, so "why was the worker told this?" always has an answer.

Both depend on `sqlitedb` only. Each has a `Store` (`NewStore(db)`, schema via `sqlitedb.Migrate`, append-only steps) and a `Handler` with `Register(mux)`.

## Skill registry

### Data

| Table | Row |
|---|---|
| `skillhub_versions` | name, version (1, 2, ...), sha, bundle (the JSON as the desktop exported it), published_from (worker id), note (at most 2000 bytes), published_ns |
| `skillhub_installs` | worker, name, version, installed_ns (one row per worker and skill) |
| `skillhub_role_skills` | role, name: the skill belongs to the role |
| `skillhub_worker_roles` | worker, role: the worker carries the role |

A role is a plain label (`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`, at most 32 per worker). Skill names use the desktop's own rule, `^[a-z0-9][a-z0-9-]{0,47}$`.

The rest of the workforce goes by these roles and installs: scoped knowledge, the queue's `required_skill`/`required_role`, and a handoff's `--role`/`--skill`. A worker "holds" a skill when the registry records an install of it (publishing counts). A skill taught on a desktop and never published is invisible to them. The access store keeps a second set of worker roles for approval policies (docs/access.md); a policy matches the union of both.

### The bundle

The desktop's `skill_bundle.export(name)` produces `{"format": "decurix-skill/1", "skill": <the skill file, whole>, "exported_at": <unix>}`.

The **sha** is sha256 over the canonical JSON of `skill` (keys sorted at every depth), leaving out the fields that change only through use: `runs`, `experience` and `installed_from`. Exporting the same skill again after a few runs therefore produces the same sha. **Publish creates a new version only when the sha differs from the latest one.**

Bundles are capped at 1000 KiB (`MaxBundleBytes`). The desktop agent refuses request bodies over 1 MiB (`http_api.MAX_BODY`), and a bundle the registry accepted but no desktop could receive would be useless. A skill carries its steps and never its recordings, so real skills are far smaller.

### Flows

- **Publish** (`POST /api/skillhub/publish {worker, name, note}`): the handler reads `GET /skills/<name>/bundle` from the worker's agent. It refuses any skill whose status is not `ready`, because a draft has unanswered questions. It stores the bundle as a new version if it changed, and records the publisher as holding that version.
- **Install** (`POST /api/skillhub/install {worker, name, version?, replace?}`): version 0 or absent means the latest. The handler sends `POST /skills/install {bundle, version, sha, replace}` to the worker's agent, and records the install only when the agent returns 2xx.
- **Roles** (`POST /api/skillhub/roles {worker, roles[]}`) replace the roles a worker carries. **Assign** (`POST /api/skillhub/assign {name, role, remove?}`) adds a published skill to a role, or takes it off.
- **Outdated** (`GET /api/skillhub/outdated?worker=`) lists the skills where the worker's installed version is behind the latest (`behind`), plus the skills one of its roles carries that it lacks (`missing`).
- **Sync** (`POST /api/skillhub/sync {worker}`) installs everything Outdated reports, one skill at a time, without `replace`. It returns `installed` and `failed`, each failure with the desktop's reason.
- **List** (`GET /api/skillhub`) returns every skill with its versions (newest first, no bundles), roles and installs, plus every worker's roles.

POST routes take JSON, or a plain form (`roles` comma-separated). Errors are `{"error": ...}`. A 4xx from a desktop carries words the agent wrote for a person and is passed on. A 5xx or a transport error is logged and replaced, following the `jsonError` rule.

**Listeners.** The routes are on both listeners (`RegisterRoutes`), wrapped by `readsForAll`: anyone the listener lets in may read; a write comes only from a person at this computer, never from a desktop and never through the gated listener (owner decision O-7). A desktop may call only `GET /api/skillhub` (`containerRoutes` in `internal/httpapi/guard.go`); the worker's `handoff who` reads it to list the other workers, their roles and the skills the registry records them holding (docs/handoff.md).

### What the desktop guarantees on install (`skill_bundle.install`)

- The format, name, steps and version are checked. `ValueError` becomes a 400.
- **No typed secret, in either direction.** A secret step must be `manual` and carry no value. A secret parameter may have no default or example. Any typed value (a step's `text`/`samples`/`value`, a parameter's `default`/`example`) that `privacy.py` recognises as a credential is refused, including one written after its name (`password: ...`). Export refuses the same skill, so it never leaves the desktop.
- **Local work is never overwritten silently.** If a skill of the same name was taught on this desktop (it has no `installed_from`), or was taught, corrected or relearned here after it was installed, the install is refused with a 409 unless `replace` is set. `installed_from` records the version, the length of `corrections`, `run_answers`, `learned` and `notes` at install time, and the skill's own `version`. If any of them grew, the skill counts as changed here.
- The skill keeps its status. It drops `runs` and `experience`, so a cheaper path earned on another desktop's screen has to be earned again on this one (correctness first: maturity is per screen). It gains `installed_from {version, sha, at, ...marks}`.

### Limits and not done

- An install does not check whether the worker is busy. The skill file is written atomically, but a run already in progress keeps the copy it loaded.
- The registry never deletes a version. Unassigning a role does not uninstall the skill anywhere.
- Publishing from a desktop that installed the skill and changed it locally creates a new version. Nothing merges two desktops' divergent corrections.
- Deleting a desktop calls `Store.ForgetWorker`: its installs and roles go; the versions it published stay.

## Scoped knowledge

### Item

`{id, scope: org|role|worker|task, scope_key, kind: rule|fact|correction|lesson, text (≤4000 bytes), source: {kind: person|demonstration|run|import, ref, worker, task_id}, created_at, superseded_by}`

An org item has no scope key. Every other scope's key is the role, worker or task id.

### Operations

- `Add(item)`.
- `Supersede(oldID, next)`: `next` keeps the old item's scope, and its kind unless it names one. Only a live item can be superseded. A replaced item points to its successor.
- `Retire(id)`: stops giving an item out without replacing it (`superseded_by = "-"`).
- `ForWorker(worker, roles)`: the live items, **org, then each role in order, then the worker's own**, oldest first within each group. Narrower lines come later, so they read as refinements of broader ones. `ForTask(task)` covers task scope.
- `Why(id)`: the item, plus the chain of items it replaced, newest first, each with its source.

`Item.Line()` produces what a worker is told: `Rule (org): ...`, `Fact (role clerk): ...`, `Lesson (worker): ...`.

### Routes

- `GET /api/knowledge?worker=&task=` returns `{worker, roles, items, lines}`. Roles come from `Handler.Roles`, a `RoleSource` that `*skillhub.Store` satisfies. Without one, the worker still gets org items and its own. With no worker, only org items (and the task's). A desktop is bound to itself: whatever worker it names, it gets its own scope, and a task's items only for one of its own tasks (403 otherwise; `Handler.Caller`, `Handler.TaskOf`).
- `POST /api/knowledge {scope, scope_key, kind, text, source}`.
- `POST /api/knowledge/supersede {id, text, kind?, source}` or `{id, retire: true}`.
- `GET /api/knowledge/why?id=`.

Bodies are JSON only; unknown fields are refused. The routes sit behind `readsForAll` like the registry's: a desktop may only `GET /api/knowledge`, and writes come only from a person at this computer (O-7).

### Agent side

`org_knowledge.fetch(worker_id=None, task_id=None)` calls `GET CONTROL_PLANE_URL/api/knowledge?worker=<DESKTOP_ID>` (and `&task=` when given) with a 5-second timeout and returns the lines, cleaned and bounded (200 lines, 4000 characters each, 2 MiB answer). It is best-effort: on any failure it returns `[]` and never raises. `as_text()` joins the lines. `cached(worker_id=None, max_age=300, task_id=None)` reuses one answer per worker and task for five minutes, but never keeps a failed fetch (entries past five minutes are dropped as new ones are kept). Within that window every call returns the same lines, so a run's prompt prefix stays byte-identical and a check that runs many times makes one request. With a task, the task's own items come after the org's, the roles' and the worker's.

Three places read it, each for the task it works on (`task_context.current()`), so task-scoped items reach the worker:

- a skill run's taught context (`skill_task._taught`: org lines first, the task's last among them, the skill's own after);
- the record ledger's given sources (`computer/records.given_sources(task_id)`, which a `gui record` pick may rest on; `_org_text` keeps one task's text for five minutes);
- every TheBot request's standing context (`thebot_harness.org_context`, `org_knowledge.standing_context`), so an org rule such as "never pay without a purchase order" reaches a request no skill covers. It goes in the job's `context` field with the other standing context, never in the operator's fixed prompt. The lines are read once per task and kept for its later jobs (a reply, the final check sending it back), so every job of a task sends the same bytes. Each line passes `privacy.withhold_known` first (a line that cannot be checked is left out), lines a taught task's procedure already carries are not repeated, and the block is bounded at 6000 characters (`MAX_CONTEXT_CHARS`) with a closing note naming how many lines were left out. The Claude and Ollama harnesses do not send it.

### Limits

- Text is free prose from people and runs. It is a principle the model reads, never code the harness obeys. Like `taskRules`, it should state principles, not stories.
- The routes cannot verify a source. A `person` source and its `ref` are whatever the caller says. Only a person at this computer can write, so every item today is a person's. Nothing in the code writes a `demonstration`, `run` or `import` item, and a worker's own lessons stay on its desktop.
- There is no deduplication or contradiction check between scopes. Precedence is order only.

## The Workforce page

`GET /workforce` (`web/templates/workforce.html`, `internal/httpapi/workforce_page.go`, `humansOnly`) puts these services and the next three docs on one page, in tabs:

- **Workers & roles**: each desktop's registry roles, its installed skills, what Outdated reports, and a Sync button.
- **Skills**: the registry, with Publish, Install and "Give a skill to a role" forms.
- **Queue** (docs/workqueue.md), **Handoffs** (docs/handoff.md) and **Access** (docs/access.md).
- **Knowledge**: items by scope with Why, Replace and Retire; an Add form; a lookup by task id.

The open tab polls every 4 s while the page is visible. On the gated listener the page shows only Workers & roles, Skills and Knowledge, since the queue, handoff and access routes are on the primary listener only. Their forms still show there, but every write is refused (O-7).

The Knowledge tab lists org items and, for each desktop, its role and worker items. An item scoped to a role no desktop carries, or to a deleted desktop, is not listed. Task items show only through the task lookup.

The Skills tab's Publish form offers the chosen worker's confirmed (`ready`) skills, read from its desktop (`GET /api/desktops/skills?id=`) when the worker changes and at most every 30 s, instead of a typed name; a stopped desktop shows that its skills could not be read.
