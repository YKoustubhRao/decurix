# One chat, every harness

`/view/<id>` (the dashboard's **Chat** button) renders `web/templates/desktop.html`, which is the chat and nothing else — no embedded screen, identical for all three harnesses. A desktop's screen is a separate thing, opened in its own tab from the dashboard's open-in-a-new-tab button (the only emitter of `?ui=touch`; see the `injectMobileEnhancements` note below - the ticket toolbar described next is unconditional, like the branding rewrite, since noVNC's page loads for no other reason). Nothing in the chat page branches on which engine is running, because every harness is addressed and reports back the same way:

```
human types in the chat
  → POST /api/desktops/send        → tasks.Plan (new task / reply / retry → a task ID)
                                   → httpapi.deliverRequest → desktop.Manager.SendRequest(content, task_id) → :8081
                                     → COMMAND_DIR/<stem>.t<id>.txt → watchers.py
                                     → task_context.start(id) → harness.py → engine
                                   → tasks.Commit + conversation.AppendUserRequest

the agent works
  → progress.emit(line) → POST /api/agent/events {task_id} → conversation.AppendStep
                                                           → tasks.AddStep (the record)

the agent finishes, or needs a human
  → messaging.write_final_reply / ask_for_query(kind) → POST /api/querytext
        {task_id, kind: final|error|query|approval, engine, model, tokens, cost_usd}
      → querynotes.Add          (the Query rail keeps the whole formatted note)
      → tasks.RecordOutcome     (done/error closes the execution; query/approval opens
                                 an intervention the next human request replies to)
      → conversation.AppendAgentReply (the chat shows only the agent's own words)

the human judges the answer
  → POST /api/tasks/verdict (chat's Correct/Wrong, or the task page) → tasks.SetVerdict
                                                                    → conversation.SetVerdict
```

**Reading a task back.** Every dashboard card has a task dropdown (the desktop's tasks newest first, polled from `GET /api/desktops/tasks?ids=` every few seconds like the rest of the card, so a task just sent appears without a reload) and a **Trace** button opening `/tasks/<id>/trace`: the full record in the site chrome - the request, each attempt with its engine, model, timing, tokens and cost, and every step in order with its timestamp and kind (command, output, screen, note), the questions asked and the answers given, and the final report. Long steps fold to their first line. `/tasks/<id>/trace.txt` is the same as plain text (`webui.TraceText`), behind the page's Copy and Download buttons, so a run can be shared as one file. Both routes read the task store only and are served on both listeners, like `/tasks/<id>`. What they show is what the control plane recorded: a step's text is cut at `maxStepTextBytes` (2 KB) when it arrives at `/api/agent/events`, so a command's long output is in the trace only as far as the agent reported it.

The chat page holds no transcript of its own: it renders whatever snapshot the control plane last sent over `GET /api/desktops/chat/stream` (SSE, whole snapshots rather than deltas, so a reconnect is self-healing), falling back to polling `GET /api/desktops/chat`. That is what makes one desktop's conversation the same on a laptop, on a phone through the tunnel, and in a second window — and why a desktop's agent is genuinely *central* to it, rather than one chat box per browser.

Replying to a blocked agent is just sending it another request: `watchers.command_watcher` routes a request arriving while `state.waiting_for_query` is set to `continue_after_query` instead of starting a new task. So the chat, the Query rail's reply box and the Requests page are three doors into one conversation. **A reply names the question it answers:** `messaging.save_request` marks a request as the reply only when its `task_id` is the waiting task's (the control plane sends the paused task's ID, after checking the note is the desktop's newest open one); a request with no ID or another task's is no answer, and `watchers.answer_waiting` refuses to settle a question - an approval above all - from a file not marked as its reply. Such a message is never dropped in silence (a long request may have read it as the answer): the person is told it was not taken as the answer, and the open question is put to them again in its own words, so their answer comes back as its reply (`watchers._not_the_reply`). And `:8081` itself serves only the control plane: on a desktop created with `DECURIX_AGENT_TOKEN` every route but `GET /health` needs `Authorization: Bearer <token>`, and any request a browser made (an `Origin` or `Sec-Fetch-*` header, a DNS-name Host) or a write whose body is not JSON is refused even without one (`http_api.refusal`). The worker's own shell can read the token, so this keeps out other desktops and web pages, not the worker.

While a long request runs as a planned run ([long-requests.md](long-requests.md)), every message that arrives - a reply or a new request - is read by `long_task.take_message` first: an approval's yes or no still goes straight to the gate, an answer resumes only the stage that asked, a change to the task is receipted and folded in, and an unrelated request waits for the run as before.
## Outcomes, secrets and approvals (review 2026-11-03)

**One outcome model.** Every page shows a task's outcome from one function, `tasks.DisplayOutcome(status, checked)`: `done_checked` ("Done"), `done_unchecked` ("Done, not checked", amber), `needs_you`, `failed`, `stopped`, `running` and `queued`. A nil `checked` (the desktop said nothing) is not checked. A status this build does not know is shown as it is, never as a success. The finished feed says "finished its task, not checked" for an unchecked done (`tasks.FinishedPhrase`). The task CSV has a new last column, `outcome`.

**An outcome names the task it ends.** A report for a task that is not open on that desktop (it ended, or is not its) gets **409** from `/api/querytext` and is shown nowhere. The agent's resend loop treats any 4xx as refused and lets the kept outcome go; a 5xx is tried again. Only a report with no task ID uses the "oldest live execution" fallback. A late step stays on its own task inside the 10 s window, then is dropped.

**How the agent reports.**
- A stop is reported `kind="error"`, `checked=False`, with text starting "Stopped, as you said". It is never `kind="final"`, which the control plane records as completed. This holds for replay stops, TheBot's needs-input after a stop, a task called off at its question (`watchers._called_off`) and `skill_task`'s stops.
- When the final gate itself raises, the report goes out `checked=False` with "Not checked - the final check itself failed ..." as its first line (`messaging.write_final_reply`).
- `/workflow list` is `gated=True` (not checked).

**Secrets rest on data, not a word list.** A query sent with `secret: true` has its reply kept as `[secret withheld]` in the task store, the chat, the send response and the concierge; the reply still reaches the desktop as typed. The agent sets the flag only from structure: a replay stopped on a step the recorder kept private, a taught run asking for a parameter the skill marks secret, or a secret-field target (`messaging.send_query`/`ask_for_query` `secret=`). It then remembers the reply as a secret and never keeps it as a lesson. A person's "This is a secret" toggle (desktop chat, dashboard reply box) does the same. `withhold.Text` is a fail-safe backstop only, and may grow only where withholding too much is harmless. Model API keys and Cloudflare tokens are never stored unsealed: without the key file, saving refuses with a reason.

**What the desktop keeps of a secret (C14, phase 2).** The control plane decides once, before delivery, whether the records keep a message withheld (`tasks.StoredForm`), and tells the desktop `secret: true` exactly then. On the desktop, the command file's handled copy (`.done`), the task file's `.request` copy and `active-task.json` hold the withheld form; a message marked secret is withheld whole. A job handed to soul carries the task's secret values as `secrets`, data beside the prompt and never in it, and soul withholds them from what it writes. The approval gate gets its own signed copy of the values ([gate-boundary.md](gate-boundary.md)).

**Approvals in the chat.** The desktop chat's `seen_at` is frozen at the first keystroke of a draft (`draftSeen` in `desktop.html`), so a "yes" typed while a newer question arrives does not answer it. Approval bubbles have Approve and Deny buttons bound to the note id. The concierge shows Approve/Deny only when the newest open question is a gate-worded approval (`/api/concierge/answer` with `note_id`); otherwise it says "Answer on its card". A bare ok, sure, go or yes never answers an approval in the concierge. The workflow page has no approve control: a stage's go-ahead is the approval question in chat, which the gate decides, and the proposed-stage box says "It asks in chat before it starts this stage." (owner decision O-6 kept the typed yes, with these limits).
