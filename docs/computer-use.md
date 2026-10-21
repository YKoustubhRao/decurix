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

**Rows into a spreadsheet, verbatim text, windows from before (2026-10-19).**
- `gui type CELLREF "a\tb\nc\td"` - text with Tabs or new lines aimed at a sheet's cell (or typed with no target while the active window's sheet has the focused cell) is a block: `core._type_sheet_block` enters it row by row (the Name Box to each row's first cell, Tab between cells, Enter), and `_read_back_block` reads every cell back through `atspi.sheet_cell_text`. A cell that does not show what was typed (AutoInput's completion, a number read as a date) or a formula showing an error (`#NAME?`, `Err:502`) is listed and the command exits 4. Anywhere else a Tab is still refused (one field per typing: a Tab carries the rest into the next form field). A host app in a viewer is not covered.
- `gui type [TARGET] -` reads the text verbatim from stdin (a heredoc), for quotes the shell would otherwise eat (`"=IF(D2=""x"",1,0)"` is bash concatenation).
- `core._task_windows` records the windows open at a task's first look. The engine's `gui type`/`gui key` with no target is refused while the window in front is one of those that the task has not worked in (`_adopt_front`: an action by ref or point, `gui open`, `gui front`). Replays and a person's actions are not asked. Leftover documents are not closed.
- Why an action is slow: each `gui` command is a new process that looks twice (AT-SPI walk of the app plus up to 60x26 sheet cells read one D-Bus call at a time, `WALK_SECONDS` 6 each), waits for the screen to settle (up to 4 s), saves screenshots and posts an audit and chat step - 10-20 s on the 8 GB host. Bulk entry is one command for that reason; the per-action cost itself is unchanged.
- In a host-app viewer (real Excel), a block typed with no target goes where the app's cursor is - each cell's text, Tab, Enter after each row - and is read back from the viewer's cell list (`_type_relayed_block`, `_read_back_relayed`); the host clipboard is never used. Keys with no target and no window in front are refused (`_nothing_in_front`: Alt+F4 there asks to log out). Not verified live.
- `gui type D1 ...`: an A1-style coordinate is a cell target (refs are `e12`). In a sheet read over AT-SPI it is made from a listed cell (`_sheet_cell_named`); in a host-app viewer the cursor is put there by the viewer's Name Box and Enter (`_goto_relayed_cell`), and the read-back is anchored at it.

## App guides: shipped and learned

`app_skills/<name>/SKILL.md` ships guides to well-known apps (front matter `applies-to` names the app; a task that names it, or `gui open`s it, gets the guide; `gui guide NAME` prints one). A desktop also learns guides to apps nobody wrote one for: the engine works the app out (the screen, `lookup "APP how to ..."` on the web) and, once its work there is right, writes what it found with `gui guide --learn NAME --applies-to "words" --description "..."`, the guide on stdin. `app_skills.learn` holds it under `~/.decurix/app_skills/.pending/<task>/<stage>/`; `app_skills.promote` makes it a guide only when the work it came from is checked done: the stage of a planned run that wrote it (`long_task` when that stage completes), or the whole task (`skill_task._playbook` on a DONE verdict; `long_task.final_report` when every stage passed). Pending guides of tasks that never ended done are dropped after 7 days. A guide is refused when it holds a credential, an e-mail address or a run of 8+ digits (it is how the app works, never a value), and a learned guide never replaces a shipped one (the same name is kept as `<name>-notes`, shown beside it). A learned guide is rendered as "learned on this desktop ... where the screen differs, the screen is right"; re-learning keeps the previous version as `SKILL.prev.md`.

## Enter in a web form

`gui type REF "...\n"` and `gui key enter` hold the Enter back when it would submit a form that still has empty fields (an `<input>`, not a search box, in a `<form>` with a submit control): the text is typed, nothing is submitted, and the operator is told to move on with Tab or a click and to press the form's own submit button when everything is filled (`cdp.ENTER_SUBMITS`).

## Number fields and saving a form

A number field (`<input type=number>`) is set as a whole value and read back. A click on its up/down arrows, Up/Down while it has the keyboard, and the wheel over it would step its value: the first two press nothing, the wheel scrolls the page from beside it. Before a form with three or more filled fields is sent by a click on its submit control, `gui click` lists what it holds and presses nothing; `gui click REF --checked` sends it when nothing changed since the list.

## Filling a Word document

`office update DOC.docx SPEC` with `{"replace": {"{{FULL_NAME}}": "Ananya Deshpande", ...}, "output_path": "letter.docx"}` fills a document or template in one command - body, tables, text boxes, headers and footers, also where Word split a placeholder across runs, keeping the formatting - and lists any placeholder still left. A filled document that still holds a placeholder fails the final check. It is never done by clicking Find & Replace on the screen.
