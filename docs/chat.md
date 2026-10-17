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

The chat page holds no transcript of its own: it renders whatever snapshot the control plane last sent over `GET /api/desktops/chat/stream` (SSE, whole snapshots rather than deltas, so a reconnect is self-healing), falling back to polling `GET /api/desktops/chat`. That is what makes one desktop's conversation the same on a laptop, on a phone through the tunnel, and in a second window — and why a desktop's agent is genuinely *central* to it, rather than one chat box per browser.

Replying to a blocked agent is just sending it another request: `watchers.command_watcher` routes any request arriving while `state.waiting_for_query` is set to `continue_after_query` instead of starting a new task. So the chat, the Query rail's reply box and the Requests page are three doors into one conversation.
