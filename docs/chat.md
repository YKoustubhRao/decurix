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

Replying to a blocked agent is just sending it another request: `watchers.command_watcher` routes a request arriving while `state.waiting_for_query` is set to `continue_after_query` instead of starting a new task. So the chat, the Query rail's reply box and the Requests page are three doors into one conversation. **A reply names the question it answers:** `messaging.save_request` marks a request as the reply only when its `task_id` is the waiting task's (the control plane sends the paused task's ID, after checking the note is the desktop's newest open one); a request with no ID or another task's is no answer, and `watchers.answer_waiting` refuses to settle a question - an approval above all - from a file not marked as its reply. And `:8081` itself serves only the control plane: on a desktop created with `DECURIX_AGENT_TOKEN` every route but `GET /health` needs `Authorization: Bearer <token>`, and any request a browser made (an `Origin` or `Sec-Fetch-*` header, a DNS-name Host) or a write whose body is not JSON is refused even without one (`http_api.refusal`). The worker's own shell can read the token, so this keeps out other desktops and web pages, not the worker.

While a long request runs as a planned run ([long-requests.md](long-requests.md)), every message that arrives - a reply or a new request - is read by `long_task.take_message` first: an approval's yes or no still goes straight to the gate, an answer resumes only the stage that asked, a change to the task is receipted and folded in, and an unrelated request waits for the run as before.
