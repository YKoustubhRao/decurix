# Computer use: one action layer, every engine

Every desktop can see and act on its own screen through `decurix_agent/computer`. The layer lives in the desktop, below the engines, so the approval gate and the audit trail cannot be skipped by any of them:

```
observe()  screenshot (python3-xlib) + windows + elements: AT-SPI for GTK/XFCE/LibreOffice,
           the Chrome DevTools Protocol for Chromium (its AX tree + one DOMSnapshot for boxes)
act()      observe -> resolve the target (ref | role+name | path | x,y) -> approval.check
           -> do it (CDP input / AT-SPI action or caret / xdotool) -> wait for the screen to
           settle -> observe -> compare -> audit.record
verify()   observe again, compare with the last observation, optionally check text/element
```

How each engine reaches it:

- **Claude** gets them as MCP tools (`mcp__computer__observe/act/verify/open`) via `claude_harness.MCP_CONFIG` (verified: the stream's init event lists the server `connected` with all four tools). Every result carries the screenshot, because Claude can read one.
- **Ollama** and **TheBot** use the `gui` shell command through their existing `COMMAND:` loops (no contract change) and read the element list **as text only**. A 3B model on a CPU cannot read pixels; `elements.render` is budgeted to `DECURIX_GUI_TEXT_CHARS` (3500) because `task_runner` shows a model at most 4000 characters of any command's output. TheBot's `atomizer.Triage` routes screen work (`wantsScreen`: a surface *and* an action) to the operator, ahead of the web skill that would otherwise fetch any URL itself.

`gui` exit codes are a contract harnesses read: 0 done, 1 could not, **3 NEEDS_APPROVAL (nothing happened)**, 4 verify failed.

Approval travels the same way for all three: `approval.check` stops a click on a Send/Pay/Buy/Publish-like control (or Enter in a messaging page), records a *pending* approval and returns NEEDS_APPROVAL. The harness pauses deterministically — `task_runner.pause_for_screen_approval` on exit 3, `operator.screenApprovalReason` → `NeedsApproval` checkpoint, `claude_harness._handle_result` if a pending approval exists whatever Claude wrote. The human's reply reaches `watchers.command_watcher`, which calls `approval.resolve_reply` **before** the harness sees it: a yes (`is_approval`, the same regexes as soul's `safety.IsApproval`) becomes a one-shot grant fingerprinted to that action; anything else drops it. The repeated action consumes the grant. A new task calls `approval.reset()`.

Every action — done, failed, or paused — is recorded twice: as an event of the task's recording on the desktop's persistent volume (see "Recordings" below), and as a chat step via `progress.emit_action` → `POST /api/agent/events` with `action` + half-size `before_png`/`after_png` → `conversation.AppendActionStep` + `stepimages.Store`. The chat renders those steps with Before/After thumbnails. Both copies go through `privacy.py` first.
