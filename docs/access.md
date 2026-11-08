# Users, roles and approval policy

`internal/access` says **who** is acting and **what they may approve**. The
approval gate on a desktop (`agent/decurix_agent/computer/approval.py`) stops
a consequential on-screen action and asks; this package decides whether the
person who replies may settle that request, and logs who did.

## Identity per listener

| Listener | Who is acting | How |
|---|---|---|
| Primary `:8080` (never exposed) | The built-in **local owner** (`local-owner`, role `owner`) | No login, ever. Whoever reaches it from this computer is the owner. A request from anything but this computer itself (`fromContainer`: loopback or a known Docker gateway is this computer; a desktop or any other container is not) has **no** actor: an agent can never approve its own action. |
| Gated `:8090` (`internal/lanaccess`, tunnelled) | The **signed-in user**, or nobody | The access code (or a paired device) still decides who gets in at all. Inside, a person signs in with name + password (`POST /api/auth/login`); the session rides in the `decurix_user` cookie (HttpOnly, SameSite=Strict, 12 h). |

`access.Middleware.Actor(r)` returns the actor; `Server.ActorContext(r)` puts
it on the request context for the code that delivers a reply.

**A reply on the gated listener needs a sign-in once a user account exists.**
From the first account in the access store on (`Store.HasUsers`, enabled or
not), a reply sent through the gated listener by someone not signed in is
refused before anything reaches the desktop or the records: 401, "Sign in to
reply: ..." (`Server.signInRefusal`, called where every reply door checks its
replier: `submitStored`, and the recording door). A store that cannot be read
refuses too (503). While there is no account, such a person can still reply:
the records name them `access-code` ("Someone with the access code", no roles),
the approvals log `anonymous` ("Not signed in"), and no policy lets them
approve; where no policy applies, their reply is delivered. The primary
listener is the local owner's and never asks for a sign-in.

Passwords: PBKDF2-SHA256 (Go stdlib `crypto/pbkdf2`), 600 000 iterations, 16-byte
salt, stored as `pbkdf2-sha256$<iter>$<salt>$<key>`. Session tokens are stored
only as their SHA-256. Five failed sign-ins for one name lock it for 15 minutes.
Disabling a user or changing their password ends their sessions.

## Roles

Roles are short lowercase words (`[a-z0-9_-]{1,32}`), assigned freely by the
owner: `finance`, `lead`, `partner`. Role `owner` satisfies every approver role.
Workers (desktops) can hold roles too (`/api/access/workers/roles`), which a
policy's `worker_role` matches (ignoring case). The gate matches the union of
these and the worker's skill-registry roles (`Server.workerRoles`,
docs/skillhub.md), so a role means the same in both places.

## Policies

A policy is data, not code:

| Field | Meaning |
|---|---|
| `worker` | A desktop id, or `*` for all |
| `worker_role` | Only workers holding this role; empty = any |
| `action_match` | Case-insensitive regular expression over the approval's action text; empty = every approval |
| `amount_over` | Optional. Applies only when the largest money figure in the text is over it (`access.LargestAmount`, which errs high: glued currency and scales "Rs450000", "$1.5M", "3Cr", "5L"; Western, Indian, European and apostrophe grouping; once any figure is marked as money every standalone figure counts; a figure read two ways takes the larger; no money mark at all means unknown, and the policy applies) |
| `approver_roles` | The approver must hold one of these (or `owner`) |
| `min_approvers` | 1, or 2 distinct approvers |
| `note` | Free text for people |

Every policy that matches an approval applies, and each approver must satisfy
all of them: two matching policies naming `finance` and `lead` need an approver
holding both (or `owner`). The approval needs the largest `min_approvers` among them.

Examples:

```json
{"action_match": "\\b(pay|transfer)\\b", "amount_over": 1000, "approver_roles": ["finance"], "note": "money out over 1000"}
{"action_match": "\\b(send|submit|file)\\b", "worker_role": "filing", "approver_roles": ["partner"], "min_approvers": 2}
{"worker": "desk-7", "action_match": "\\bdelete\\b", "approver_roles": ["lead"]}
```

Use `\b` for whole words: `file` alone also matches "profile".

**Money figures.** A figure counts when a currency symbol (any Unicode `Sc`),
an uppercase three-letter code (`USD 75`, `2500 EUR`), or `Rs`/`amount`/`total`/`sum`
stands next to it. Commas are grouping (`1,00,000` = 100000). Text with **no**
money figure is treated as over the threshold: the stricter reading. When a policy applies only because no amount
could be read for certain, the refusal, the held message and the Workforce check say "the amount could not be read
for certain, so the policy for larger amounts applied" (`Requirement.AmountUnknown`, JSON `amount_unknown`). An
identifier-labelled figure larger than every money figure also makes the amount unknown, so a payment that names a
larger account or invoice number meets its amount policy.

## Enforcement

Every door that sends a reply passes `submitStored` (`internal/httpapi/requests.go`),
which calls `Server.decideApprovalReply` (`internal/httpapi/access_routes.go`)
before the request reaches a desktop as the reply to its pending approval. It
reads what the desktop waits on from the task's own record first: the latest
attempt's open intervention of kind `approval`, recorded whether or not a note
was shown. The desktop's latest query note is the fallback (an approval not yet
answered; closing the note by hand does not count as answered). It then calls
`Store.Decide(actor, worker, workerRoles, taskID, actionText, replyText)`:

1. No policy applies → delivered (today's behaviour), logged with who replied.
2. A plain no ("no", "stop", "cancel") → delivered from anyone. Stopping is never the risky direction.
3. The actor cannot approve → **not delivered**; the person sees
   "This needs approval from a finance approver." (plus "Sign in…" when not signed in).
4. The local owner, or a one-approver policy → delivered.
5. Two approvers: the first yes is **held** (logged, not delivered, HTTP 409
   "Your yes is recorded…"); the same person again is refused; a second,
   different approver's yes is delivered. An instruction other than yes/no is
   refused under a two-approver policy ("reply no, then send the new instruction").
   The round ends when a reply is delivered; the next request of the same action starts over.

A reply other than yes or no ("no, pay B instead") is an instruction and needs
the approver role. Every decision — delivered, held or refused — is a row in
the approvals log (`/api/access/approvals`) with the user's id and name
(`anonymous` when nobody was signed in). A failure to read the policies refuses
the reply (fails closed, 503).

The gate does not change the agent: a yes is still one action's, there is no
"always allow", and the desktop still asks for every consequential action.

## Routes

| Route | Listener | Notes |
|---|---|---|
| `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me` | both | On the primary listener they report the local owner; login sets nothing |
| `GET /api/access/users`, `POST …/users/create`, `…/users/update`, `…/users/delete` | primary, humansOnly | update takes any of `roles`, `disabled`, `password` |
| `GET /api/access/policies`, `POST …/policies/save`, `…/policies/delete`, `…/policies/check` | primary, humansOnly | save creates (no id) or replaces; check `{worker, action, worker_roles?}` is a dry run that records nothing. Without `worker_roles` it reads the worker's roles as the gate does: the union of the registry's and the access store's (`Handler.WorkerRoles`, set by `SetAccess` to `Server.workerRoles`) |
| `GET /api/access/workers`, `POST …/workers/roles` | primary, humansOnly | |
| `GET /api/access/approvals?worker=&limit=` | primary, humansOnly | newest first |

POST bodies are JSON only (a cross-site form cannot send JSON without a
preflight). Admin handlers also refuse a gated request on their own, even if
registered on the wrong mux. Errors are fixed messages, never `err.Error()`.

The Workforce page's Access tab (docs/skillhub.md), primary listener only, has
the people (add; edit roles; set a new password, which ends their sessions;
disable or enable; delete), the policies (add; Edit fills the form with a
policy and Save changes replaces it by id; delete), a "Would this need
approval?" dry run, the worker roles for policies, and the latest 50 entries of
the approvals log.

## Limits

- **Other ways to the screen.** A gated visitor who passes the access code can
  still reach the desktop's own screen (`/desktop/`, noVNC) and the agent proxy
  (`/agent/`). The policy gate covers replies through the control plane, not
  someone clicking the button themselves on the live screen.
- **What is pending comes from the records.** An approval is recognised from
  the task's open intervention of kind `approval`, or else the desktop's latest
  approval note. A reply with no task, to a desktop whose intervention records
  no kind and whose note is not an approval, is not gated.
- Files attached to a refused reply are already copied into the desktop (the
  gate runs inside `submitLocked`, after attachments are written).
- **The local owner satisfies everything**, including two-approver policies.
  The two-person rule binds gated users only.
- A user holding several roles counts as one approver.
- Decimal commas (`1.234,56`) are read as `1.234`.
- One process: the sign-in lockout is in memory and resets on restart.
