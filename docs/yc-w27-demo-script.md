# Decurix — YC Winter 2027 product demo: voiceover cue sheet

Companion to [yc-w27-demo-plan.md](yc-w27-demo-plan.md). The silent cut is recorded and edited (2:59.1, 1920x1080). This file is what Koustubh reads over it: twelve cues, each with its start time and the time it has. The lines describe only what the footage shows.

## How to record

- Play `decurix-demo-silent.mp4` with `decurix-demo-cues.srt` loaded as subtitles. Each cue then appears on screen at the moment it starts.
- Record **one audio file per cue**, named `cue01` … `cue12` (any format: phone voice memo, WhatsApp audio, .m4a, .wav). Leave a breath of silence at the start and end; the edit trims it.
- Speak at a normal pace. Each line is written to fit its time with room to spare. If a take runs long, send it anyway: the edit can hold a frame or slow a sped-up segment slightly, but it never speeds up your voice.
- Quiet room, phone 20–30 cm from your mouth, the same place for every cue.

## Cues

| Cue | Starts | Time | On screen |
|---|---|---|---|
| 01 | 0:00.0 | 13.8 s | Dashboard: two workers, each with its learned memory; "New desktop" with the model choice |
| 02 | 0:13.8 | 23.6 s | A person enters bill 9 in ERPNext on the worker's screen, recording on (6.5×) |
| 03 | 0:37.4 | 14.9 s | The worker studies the recording; its read-back of steps and rules, each marked seen or guessed |
| 04 | 0:52.4 | 11.5 s | Its questions in the chat and the person's short answers (10×) |
| 05 | 1:03.9 | 23.7 s | Bill 16 (another state, three lines) attached in chat; it fills ERPNext (10×), then stops before saving to ask about one line |
| 06 | 1:27.5 | 5.1 s | The person's answer; a caption says the rest of this run is cut |
| 07 | 1:32.7 | 14.7 s | Bill 17, worded differently again: it enters all three lines and asks nothing (12×) |
| 08 | 1:47.4 | 24.6 s | The review finds it skipped the taught duplicate check; it checks, reports, the person confirms |
| 09 | 2:12.0 | 20.8 s | Bill 7, an unknown supplier: it searches, stops, asks, and leaves the bill (18×, then 1× and 8×) |
| 10 | 2:32.7 | 8.2 s | ERPNext: the saved invoice PINV-26-00007, its lines, IGST and total; then bill 17 itself |
| 11 | 2:40.9 | 8.2 s | Workforce page: the skill published to the team registry |
| 12 | 2:49.1 | 10.0 s | End card (held to fit the line; the demo runs 2:59) |

### 01 · 0:00.0 (13.8 s)

> This is Decurix, an operating system for AI workers. Each worker gets its own computer, its own memory, and the model you choose: local, Claude, or Azure.

### 02 · 0:13.8 (23.6 s)

> I don't write a script. A person does the job once, on the worker's own screen: opens a vendor bill, starts a purchase invoice in the ERP, picks the supplier, enters each line, and saves a draft. Every step is recorded.

### 03 · 0:37.4 (14.9 s)

> Then the worker studies the recording. It doesn't just keep the clicks. It writes down the rules of the job, and marks each one: what it saw, and what is only its guess.

### 04 · 0:52.4 (11.5 s)

> Then it asks what it couldn't see. I answer once, in plain words: the tax for another state, duplicates, unknown suppliers.

### 05 · 1:03.9 (23.7 s)

> Now a new bill. Another supplier, another state, so a different tax, and three lines instead of two. I just attach it in the chat. It fills the invoice on its own screen. Before saving, it stops: one line has no clear match in the ERP. It proposes one, and asks.

### 06 · 1:27.5 (5.1 s)

> I confirm it.

### 07 · 1:32.7 (14.7 s)

> The next bill is worded differently again. This time it asks nothing, and every match is right.

### 08 · 1:47.4 (24.6 s)

> Before anything is called done, a review checks the work against the rules. Here it caught that the worker skipped the duplicate check it was taught. So it was sent back. It checked, found only its own invoice, and said exactly what it had missed.

### 09 · 2:12.0 (20.8 s)

> Next, a supplier the ERP has never seen. My rule: never create one. It searched, stopped, and asked. Then it leaves the bill, and says exactly what it didn't do.

### 10 · 2:32.7 (8.2 s)

> Here's the saved invoice, next to the bill. Every line, the tax, and the total match.

### 11 · 2:40.9 (8.2 s)

> The skill goes into the team's registry. Any worker can install it.

### 12 · 2:49.1 (10.0 s)

> Twenty people at an accounting firm use it every day. Next, experts sell the workers they teach, and buyers run them on their own model.

## What the footage is, honestly

- **Real product, real runs.** Recorded on 30 October 2026 on the local Decurix build (worker `nova`, Azure GPT-5.4) against a local ERPNext with fictional bills. Nothing is staged or re-enacted. Waiting is sped up and labelled on screen; the one cut inside a run is captioned on screen (cue 06) and explained below.
- **The worker ran a fixed soul.** The first shoot showed the final check misreading an ERPNext item table (it set line amounts against the grand total). The cause was in the check itself: it read a screen with only the lines holding figures, so a grid listed one cell per line lost its column and row names, and it rechecked an older half-filled look at the form after the record was saved under a new title. Fixed in `agent/thebot/internal/skills/evidence.go` (the whole screen goes to the verifier in order, the report's own look carries element positions, and doubts from a window no longer open become the worker's to recheck rather than the person's), with tests. Bill 17 ran on the fix: no false doubts.
- **Bill 16's run is cut after the person's answer.** It went on to finish the entry correctly (PINV-26-00006, ₹3,09,160), but that run still had an earlier version of the check, which raised false doubts; the person answered "as you did". The run that shows the finished check is bill 17.
- **The person in the teaching scene** is the scripted test operator (`bench/screen/person.py`, VNC input), not Koustubh. That is why cue 02 says "a person does the job once".
- **The people's answers in the runs:** the item for bill 16's laptop line (it proposed SKU002 Laptop); "done" on bill 17 after its report; and on bill 7, "leave this bill" and "stop". On bill 17 it asked nothing.
- **Bills 16 and 17 were made for this demo** with the same generator and layout as the other fictional bills (`erpnext-demo/bills/extra/make_demo_bill.py`, outside the repo), because every unused inter-state bill was already in ERPNext from earlier takes.
- **What it skipped, and how it was caught:** on bill 17 it created the invoice before running the duplicate check it was taught. The review caught it and sent it back; it then checked and found no other invoice with that number. That is shown, not cut.
- **Saved results, checked through ERPNext's API:** PINV-26-00004 (bill 9: ₹92,000 + CGST/SGST ₹16,560 = ₹1,08,560) and PINV-26-00007 (bill 17: ₹2,39,000 + IGST ₹43,020 = ₹2,82,020), both drafts. Nothing was created for bill 7.
- **Not shown:** the dashboard's model setting is the New desktop dialog; the model used for the runs is Azure GPT-5.4, set before filming.
