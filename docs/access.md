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

Passwords: PBKDF2-SHA256 (Go stdlib `crypto/pbkdf2`), 600 000 iterations, 16-byte
salt, stored as `pbkdf2-sha256$<iter>$<salt>$<key>`. Session tokens are stored
only as their SHA-256. Five failed sign-ins for one name lock it for 15 minutes.
Disabling a user or changing their password ends their sessions.

## Roles

Roles are short lowercase words (`[a-z0-9_-]{1,32}`), assigned freely by the
owner: `finance`, `lead`, `partner`. Role `owner` satisfies every approver role.
Workers (desktops) can hold roles too (`/api/access/workers/roles`), which a
policy's `worker_role` matches.

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

The reply route calls `Server.gateApprovalReply` (in `internal/httpapi/access_routes.go`)
before a request reaches a desktop as the reply to its pending approval. It
reads what the desktop waits on from its latest query note (an approval not yet
answered; closing the note by hand does not count as answered), and calls
`Store.Decide(actor, worker, nil, taskID, actionText, replyText)`:

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
the reply (fails closed).

The gate does not change the agent: a yes is still one action's, there is no
"always allow", and the desktop still asks for every consequential action.

## Routes

| Route | Listener | Notes |
|---|---|---|
| `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me` | both | On the primary listener they report the local owner; login sets nothing |
| `GET /api/access/users`, `POST …/users/create`, `…/users/update`, `…/users/delete` | primary, humansOnly | update takes any of `roles`, `disabled`, `password` |
| `GET /api/access/policies`, `POST …/policies/save`, `…/policies/delete`, `…/policies/check` | primary, humansOnly | save creates (no id) or replaces; check is a dry run |
| `GET /api/access/workers`, `POST …/workers/roles` | primary, humansOnly | |
| `GET /api/access/approvals?worker=&limit=` | primary, humansOnly | newest first |

POST bodies are JSON only (a cross-site form cannot send JSON without a
preflight). Admin handlers also refuse a gated request on their own, even if
registered on the wrong mux. Errors are fixed messages, never `err.Error()`.

## Limits

- **Other ways to the screen.** A gated visitor who passes the access code can
  still reach the desktop's own screen (`/desktop/`, noVNC) and the agent proxy
  (`/agent/`). The policy gate covers replies through the control plane, not
  someone clicking the button themselves on the live screen.
- **Pending text comes from the query note.** A desktop that reports an
  approval with `quiet` (no Query rail entry) or an older image that reports no
  kind is not recognised as an approval, and its reply is not gated. Reading the
  open intervention's kind and request from the task records (`interventions`)
  would close this.
- Files attached to a refused reply are already copied into the desktop (the
  gate runs inside `submitLocked`, after attachments are written).
- **The local owner satisfies everything**, including two-approver policies.
  The two-person rule binds gated users only.
- A user holding several roles counts as one approver.
- Decimal commas (`1.234,56`) are read as `1.234`.
- One process: the sign-in lockout is in memory and resets on restart.
