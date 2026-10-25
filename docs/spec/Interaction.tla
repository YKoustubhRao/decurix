------------------------------ MODULE Interaction ------------------------------
(***************************************************************************)
(* The person <-> worker interaction protocol of the Decurix agent harness *)
(* AS IMPLEMENTED on 2026-10-23 (branch koustubh/gst-sample-workflow), for *)
(* one ordinary (not planned) task run by TheBot's soul.                   *)
(*                                                                         *)
(* Layers, each modelled from its source:                                  *)
(*   soul operator   agent/thebot/internal/skills/operator.go (loop,       *)
(*                   resume, execute, pause, ask), knowledge.go            *)
(*                   (weighJudgements, reviewInProgress), judgements.go    *)
(*                   (judgementsDue), reviewer.go (review: two tries),     *)
(*                   evidence.go (verifyCommit, folded into the review)    *)
(*   soul scheduler  agent/thebot/internal/scheduler/graph.go (restore,    *)
(*                   complete, e.approved)                                 *)
(*   host            agent/decurix_agent/thebot_harness.py (_handle_job),  *)
(*                   watchers.py (answer_waiting, _pass), guidance.py      *)
(*                   (note_reply, refused), computer/approval.py (check,   *)
(*                   resolve_reply, is_approval, is_refusal), skill_task.py*)
(*                   (_gate, _retry_engine, _confirm_reply,                *)
(*                   _question_gate)                                       *)
(*   person          a nondeterministic environment                        *)
(*   worker model    a nondeterministic environment (its replies), with a  *)
(*                   finite command/question budget (an assumption)        *)
(*                                                                         *)
(* Values are abstracted away; control state is kept.  Ghost fields (in    *)
(* the record g) record what the properties talk about.  README.md says    *)
(* what is abstracted and what TLC found.                                  *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Actions,            \* irreversible actions the task needs: "G" = a click the on-screen gate stops (FILE WITH EVC),
                        \*   "S" = a shell command internal/safety stops (a script that sends)
    MaxModelTries,      \* operator.go maxModelTries (3)
    MaxReviewQuestions, \* knowledge.go maxReviewQuestions (2)
    MaxDoneRounds,      \* judgements.go maxDoneRounds (4)
    MaxApprovalRounds,  \* judgements.go maxApprovalRounds (6)
    MinJ,               \* judgements.go minJudgementCommands (4)
    MaxUntracedNudges,  \* grounding.go maxUntracedNudges (2)
    MaxEngineAttempts,  \* skill_task.py MAX_MODEL_ATTEMPTS (2)
    MaxConfirmRounds,   \* skill_task.py MAX_CONFIRM_ROUNDS (3)
    MaxSelfAnswers,     \* skill_task.py MAX_SELF_ANSWERS (2)
    MaxCmds,            \* ASSUMPTION: commands the worker model sends in the whole task
    MaxHelps,           \* ASSUMPTION: questions (HELP) the worker model asks in the whole task
    OnScreen,           \* the task worked on the screen, so the final check (_gate) judges it
    ScreenRequest,      \* the request wants the screen or names file work (atomizer.WantsScreen || namesFileWork):
                        \*   unstartedScreenNudge applies to it
    ObedientModel,      \* ASSUMPTION switch: the model never re-sends an action the person declined in this job
    ApprovalReplies,    \* what the person may answer to an approval
    QuestionReplies,    \* ... to a question
    ConfirmReplies      \* ... to the host's "I cannot confirm this is done" / "I could not finish this"

VARIABLES
    ctl,  \* who holds the turn: "soul" (operator loop running), "host" (handling a soul outcome),
          \*   "person" (a question is on their screen), "end"
    s,    \* the soul's operatorState (one goal = one soul job; reset when the host retries the engine)
    h,    \* host state: approval.json, guidance._refused, the final gate's counters
    p,    \* the channel: what the person is shown, and the outcome the soul handed the host
    e,    \* the worker model's remaining budget (environment assumption)
    g,    \* ghost state for the properties
    ev    \* the last step, to read a trace by (no property and no step reads it)

vars == <<ctl, s, h, p, e, g, ev>>

\* --------------------------------------------------------------------------
\* Reply kinds (abstracting the text)
\* --------------------------------------------------------------------------
\* Person:  "yes"        an unconditional yes ("yes", "yes - the client has paid; go ahead")
\*          "yesIf"      a yes on a condition ("yes - provided the HSN tab now shows 600 kg, file it")
\*          "yesAnswer"  an answer to a question that starts with yes ("yes, include the cancelled invoice")
\*          "no"         a bare no
\*          "noDoX"      "No - don't file yet. Open invoice 455 first ..."  (no redirect word)
\*          "noUse"      "No, use the other account instead"               (a redirect word)
\*          "noAnswer"   an answer that starts with no ("No, a cancelled invoice still counts")
\*          "stop"       stop / cancel / never mind
\*          "answer"     an answer
\*          "handback"   "use your judgement"
\*          "done"       "done" / "it is done"
\*          "other"      anything else (an unrelated message, a question back, what is wrong)
\* internal/safety IsApproval (scheduler restore) and computer/approval.py is_approval: the same word lists - a reply
\* that starts with yes and holds no refusal word.
IsApproval(k) == k \in {"yes", "yesAnswer", "yesIf"}
\* computer/approval.py resolve_reply -> read_reply (since 16:34 on 2026-10-23): a reply of up to three words by the
\* word list; a longer one by a model told "a person was asked to approve one action" - "yes" | "if" | "no"; only
\* "yes" grants. Told that even when the person was asked something else, it may read an answer's yes as the grant.
GrantReadings(k) == CASE k = "yes"       -> {"yes"}
                      [] k = "yesAnswer" -> {"yes", "no"}
                      [] k = "yesIf"     -> {"if"}
                      [] OTHER           -> {"no"}
\* computer/approval.py is_refusal: REFUSAL_LEAD and no REDIRECTS word
IsRefusal(k)  == k \in {"no", "noDoX", "noAnswer", "stop"}
\* guidance.py STOP_ONLY
StopOnly(k)   == k = "stop"
\* operator.go screenConfirmRe (anchored: a bare done/yes/ok only)
IsConfirm(k)  == k \in {"yes", "done"}
\* The person's intent, for the ghost
Word(k) == CASE k = "no" -> "bare"
             [] k \in {"noDoX", "noUse"} -> "redirect"
             [] k = "stop" -> "stop"
             [] OTHER -> "other"

RepliesFor(shown) == CASE shown = "approval" -> ApprovalReplies
                       [] shown = "question" -> QuestionReplies
                       [] shown = "confirm"  -> ConfirmReplies
                       [] OTHER -> {}

\* The worker model's reply to one turn (parseControl's marker, abstracted):
\*   "cmd"       a command that runs and succeeds (reads, fixes, types traced values)
\*   "untraced"  a command typing values grounding.go cannot trace (groundingCheck)
\*   "repeat"    the command it ran last, again (the repeats counter of loop())
\*   "done"      DONE (with or without the JUDGEMENTS list; also an unmarked list at an approval)
\*   "help"      HELP (also a DONE that only asks the person, doneAsksThePerson)
\*   "G", "S"    the irreversible action
\*   "helpG"     HELP asking the person to approve the click G before making it (the contract says not to:
\*               "a click in an app or a web page is made with gui, which asks them itself"; seen live in GST T9
\*               on 2026-10-23, Q13 then Q14 for one PROCEED TO PAYMENT)
Replies == {"cmd", "untraced", "repeat", "done", "help"} \cup Actions \cup (IF "G" \in Actions THEN {"helpG"} ELSE {})
IsCmd(r) == r \in {"cmd", "untraced", "repeat"} \cup Actions

\* What one review (reviewer.go review, tried twice, plus evidence.go verifyCommit at an approval) gives:
\*   "none" no points; "points" review/verifier points; "web" only calls resting on a web page; "failed" both tries failed
ReviewOutcomes == {"none", "points", "web", "failed"}

HarnessQCap == MaxReviewQuestions + 1   \* review questions + the one "told once; said again, a person is asked" doubt

Sat(n, cap) == IF n > cap THEN cap ELSE n

\* --------------------------------------------------------------------------
\* Initial state
\* --------------------------------------------------------------------------
SoulInit == [aw |-> "none",        \* AwaitingJudge: "none" | "done" | "approval"
             settling |-> FALSE,   \* Settling
             pending |-> "none",   \* Pending: the gated action ("G" | "S") or "none"
             jq |-> FALSE,         \* JudgementQuestion
             unrev |-> FALSE,      \* AnswerUnreviewed
             anyRound |-> FALSE,   \* JudgementRounds > 0
             dr |-> 0,             \* DoneRounds
             ar |-> 0,             \* ApprovalRounds
             sentBack |-> FALSE,   \* SentBackRound = JudgementRounds
             pa |-> 0,             \* PersonAsks
             succ |-> 0,           \* CommandsSucceeded, saturating at MinJ
             workSince |-> FALSE,  \* CommandsSucceeded > JudgedAt
             lastIssues |-> FALSE, \* LastIssues non-empty
             scrConf |-> FALSE,    \* ScreenConfirmed
             unstNudged |-> FALSE, \* UnstartedNudged
             untr |-> 0,           \* UntracedNudges
             askedUntr |-> FALSE,  \* AskedUntraced non-empty
             degen |-> 0,          \* loop()'s local repeats counter
             tries |-> 0,          \* tries of this turn's model call that failed transiently
             appr |-> {},          \* scheduler e.approved (per job)
             declined |-> {},      \* actions the person declined in this job (only for ObedientModel)
             sched |-> "none"]     \* the scheduler pause kind of the checkpoint: "approval" | "input" | "none"

HostInit == [guiPending |-> FALSE, \* approval.json "pending" (the stopped click G)
             guiGrant |-> FALSE,   \* approval.json "grants" holds one for G
             refused |-> FALSE,    \* guidance._refused is set (for this task)
             workAfter |-> FALSE,  \* work recorded (screen events or command outputs) since it was set
             attempt |-> 1,        \* guidance attempt (skill_task _retry_engine)
             confirms |-> 0,       \* skill_task _confirm_rounds for this task
             selfAns |-> 0]        \* skill_task _self_answered count

ChanInit == [shown |-> "none",     \* the question card open to the person: "none" | "approval" | "question" | "confirm"
             asked |-> "none",     \* what it is about: "G" | "S" | "model" | "review" | "doubt" | "untraced" | "host"
             out |-> "none",       \* the soul's outcome for the host: "approval" | "question" | "answered" | "failed"
             qo |-> "none",        \* ... about what
             settle |-> FALSE,     \* the question is about the action being settled (a HELP sent while settling
                                   \*   review points at its approval, or asking to approve it): an ask in that attempt
             stopQueued |-> FALSE] \* the person sent "stop" while the worker worked (no question open)

EnvInit == [cmds |-> MaxCmds, helps |-> MaxHelps]

GhostInit == [committed |-> {},     \* irreversible actions that happened
              yes |-> {},           \* the person said yes to an approval naming it, not yet used
              badCommit |-> FALSE,  \* an irreversible action ran without its own fresh yes
              inEp |-> "none",      \* the action whose gate stop is being settled (an "attempt")
              epAsks |-> 0,         \* asks to the person in that attempt
              unacc |-> FALSE,      \* review points sent back, not yet re-judged, asked or carried
              dropped |-> FALSE,    \* a verified result or an approval went out while unacc
              lastWord |-> "none",  \* the person's last reply: "bare" | "redirect" | "stop" | "other"
              workSinceWord |-> FALSE, \* real work (a command run, an action made) since that reply
              noed |-> {},          \* actions the person answered with a bare no
              reask |-> FALSE,      \* an approval was asked again for an action the person bare-no'd
              bareNoIgnored |-> FALSE, \* after a bare no and no work, the task went on instead of ending
              stoppedWrong |-> FALSE,  \* the task was ended as "stopped, as you said" without a bare no or stop
              misrouted |-> FALSE,  \* a reply was applied to something other than what the person was asked
              rqJob |-> 0,          \* review questions shown in this soul job
              hqJob |-> 0,          \* harness-originated questions (review + doubt) in this soul job
              confirmAsks |-> 0,    \* host "cannot confirm" questions in this task
              endKind |-> "none",
              fatalTries |-> 0,     \* consecutive failed tries of the model call that ended the task
              workAfterStop |-> FALSE] \* work done after the person sent "stop"

Init == /\ ctl = "soul" /\ s = SoulInit /\ h = HostInit /\ p = ChanInit
        /\ e = EnvInit /\ g = GhostInit /\ ev = <<"task starts">>

\* --------------------------------------------------------------------------
\* Building blocks.  A step's result is a record [ctl, s, h, g, p].
\* --------------------------------------------------------------------------
R(c, ss, hh, gg, pp) == [ctl |-> c, s |-> ss, h |-> hh, g |-> gg, p |-> pp]
Apply(r) == /\ ctl' = r.ctl /\ s' = r.s /\ h' = r.h /\ g' = r.g /\ p' = r.p

Continue(ss, hh, gg, pp) == R("soul", ss, hh, gg, pp)

\* The soul's loop returns: pause (needs_approval), ask (needs_input), finish (answered) or a fatal failure.
\* A verified result or an approval that goes out while review points are unaccounted for is a dropped doubt.
ToHost(o, q, settle, ss, hh, gg, pp) ==
    R("host",
      [ss EXCEPT !.sched = IF o = "approval" THEN "approval" ELSE IF o = "question" THEN "input" ELSE "none",
                 !.degen = 0, !.tries = 0],
      hh,
      IF o \in {"approval", "answered"} THEN [gg EXCEPT !.dropped = @ \/ gg.unacc] ELSE gg,
      [pp EXCEPT !.out = o, !.qo = q, !.settle = settle])

\* What counts as work for guidance.refused(): screen events (a click stopped by the gate is recorded too,
\* computer/core.py audit.record) and command outputs. The ghost counts only real work.
HWork(hh) == [hh EXCEPT !.workAfter = TRUE]
GWork(gg, pp) == [gg EXCEPT !.workSinceWord = TRUE, !.workAfterStop = @ \/ pp.stopQueued]
SWork(ss) == [ss EXCEPT !.succ = Sat(@ + 1, MinJ), !.workSince = TRUE]

Commit(act, gg) == [gg EXCEPT !.committed = @ \cup {act},
                              !.badCommit = @ \/ (act \notin gg.yes),
                              !.yes = @ \ {act},
                              !.inEp = IF gg.inEp = act THEN "none" ELSE @,
                              !.epAsks = IF gg.inEp = act THEN 0 ELSE @]
StartEp(act, gg) == IF gg.inEp = act THEN gg ELSE [gg EXCEPT !.inEp = act, !.epAsks = 0]

\* judgements.go judgementsDue (Hosted assumed: the judgement machinery runs only for a hosted model)
JudgementsDue(ss, kind) ==
    /\ ss.aw = "none"
    /\ ss.succ >= MinJ
    /\ IF kind = "approval"
         THEN (ss.unrev \/ ~ss.anyRound \/ ss.workSince) /\ ss.ar < MaxApprovalRounds
         ELSE ~(ss.anyRound /\ ~ss.workSince) /\ ss.dr < MaxDoneRounds

StartRound(ss, kind) ==
    [ss EXCEPT !.aw = kind, !.anyRound = TRUE,
               !.dr = IF kind = "done" THEN @ + 1 ELSE @,
               !.ar = IF kind = "approval" THEN @ + 1 ELSE @,
               !.unrev = FALSE, !.sentBack = FALSE]

\* knowledge.go weighJudgements.  isAction: at an approval (the question branch needs action == "").
\* Result kind: "nudge" (sent back to the operator), "question" (asked), "carry" (open points with the result/approval).
Weigh(ss, gg, isAction, rv) ==
    LET issues   == rv = "points"
        web      == rv = "web"
        reviewed == rv # "failed"
    IN IF issues /\ ~ss.sentBack
       THEN [kind |-> "nudge",
             s |-> [ss EXCEPT !.sentBack = TRUE, !.lastIssues = TRUE],
             g |-> [gg EXCEPT !.unacc = TRUE]]
       ELSE IF (issues \/ web) /\ ~isAction /\ ss.pa < MaxReviewQuestions
       THEN [kind |-> "question",
             s |-> [ss EXCEPT !.lastIssues = issues, !.pa = @ + 1, !.jq = TRUE],
             g |-> [gg EXCEPT !.unacc = FALSE]]
       ELSE [kind |-> "carry",
             s |-> [ss EXCEPT !.lastIssues = issues],
             \* a review that was made re-judged the points sent back (and its own are carried); one that
             \* could not be made carries only "an independent review could not be made"
             g |-> [gg EXCEPT !.unacc = IF reviewed THEN FALSE ELSE @]]

\* knowledge.go reviewInProgress: an approval asked while a round is open carries LastIssues.
ReviewInProgress(ss, gg) ==
    IF ss.aw # "none"
    THEN [s |-> [ss EXCEPT !.aw = "none", !.settling = FALSE],
          g |-> [gg EXCEPT !.unacc = @ /\ ~ss.lastIssues]]
    ELSE [s |-> ss, g |-> gg]

\* execute() of the click G: the gui gate (computer/approval.py check) lets it through on a grant, else stops it,
\* records a pending approval, and the operator either opens an approval round or pauses (operator.go 1581-1597).
GateBlockedG(ss, hh, gg, pp) ==
    LET hh1 == [HWork(hh) EXCEPT !.guiPending = TRUE]
        gg1 == StartEp("G", gg)
        ss1 == [ss EXCEPT !.pending = "G"]
    IN IF JudgementsDue(ss1, "approval")
       THEN Continue(StartRound(ss1, "approval"), hh1, gg1, pp)
       ELSE LET rp == ReviewInProgress(ss1, gg1)
            IN ToHost("approval", "G", FALSE, rp.s, hh1, rp.g, pp)

ExecG(ss, hh, gg, pp) ==
    IF hh.guiGrant
    THEN Continue(SWork(ss), [HWork(hh) EXCEPT !.guiGrant = FALSE], Commit("G", GWork(gg, pp)), pp)
    ELSE GateBlockedG(ss, hh, gg, pp)

\* --------------------------------------------------------------------------
\* The soul operator: one model reply, handled as loop() handles it
\* --------------------------------------------------------------------------
DoneTurn(rv, ss, hh, gg, pp) ==
    IF ScreenRequest /\ ss.succ = 0 /\ ~ss.scrConf
    THEN \* unstartedScreenNudge: nothing run for a request that wants the screen or files
         IF ~ss.unstNudged
         THEN Continue([ss EXCEPT !.unstNudged = TRUE], hh, gg, pp)
         ELSE ToHost("question", "doubt", FALSE, [ss EXCEPT !.scrConf = TRUE], hh, gg, pp)
    ELSE IF JudgementsDue(ss, "done")
    THEN Continue(StartRound(ss, "done"), hh, gg, pp)
    ELSE IF ss.aw = "done"
    THEN LET w == Weigh(ss, gg, FALSE, rv)
         IN CASE w.kind = "nudge" -> Continue(w.s, hh, w.g, pp)
              [] w.kind = "question" ->
                   ToHost("question", "review", FALSE, [w.s EXCEPT !.aw = "none", !.workSince = FALSE], hh, w.g, pp)
              [] OTHER ->
                   ToHost("answered", "none", FALSE, [w.s EXCEPT !.aw = "none", !.workSince = FALSE], hh, w.g, pp)
    ELSE ToHost("answered", "none", FALSE, ss, hh, gg, pp)

Switch(r, rv, ss, hh, gg, pp, wasSettling) ==
    CASE r = "done" -> DoneTurn(rv, ss, hh, gg, pp)
      [] r = "help" -> ToHost("question", "model", wasSettling, ss, hh, gg, pp)
      [] r = "helpG" -> ToHost("question", "model", TRUE, ss, hh, StartEp("G", gg), pp)
      [] r = "cmd"  -> Continue([SWork(ss) EXCEPT !.degen = 0], HWork(hh), GWork(gg, pp), pp)
      [] r = "untraced" ->
           IF ss.untr < MaxUntracedNudges
           THEN Continue([ss EXCEPT !.degen = 0, !.untr = @ + 1], hh, gg, pp)
           ELSE ToHost("question", "untraced", FALSE, [ss EXCEPT !.degen = 0, !.askedUntr = TRUE], hh, gg, pp)
      [] r = "repeat" ->
           \* repeatedCommandNudge once, then finish(answerFromCommand, verified)
           IF ss.degen = 0 THEN Continue([ss EXCEPT !.degen = 1], hh, gg, pp)
           ELSE ToHost("answered", "none", FALSE, ss, hh, gg, pp)
      [] r = "G" -> ExecG([ss EXCEPT !.degen = 0], hh, gg, pp)
      [] r = "S" ->
           IF "S" \notin ss.appr
           THEN \* safety.AssessCommand stops it: st.Pending = value; pause(reason + reviewInProgress())
                LET rp == ReviewInProgress([ss EXCEPT !.degen = 0], StartEp("S", gg))
                IN ToHost("approval", "S", FALSE, [rp.s EXCEPT !.pending = "S"], hh, rp.g, pp)
           ELSE Continue([SWork(ss) EXCEPT !.degen = 0], HWork(hh), Commit("S", GWork(gg, pp)), pp)

\* loop(): the AwaitingJudge == "approval" block before the switch (operator.go 1293-1321), then the switch.
Turn(r, rv) ==
    LET s0 == [s EXCEPT !.tries = 0]
        wasSettling == s0.aw = "approval" /\ s0.settling
    IN IF s0.aw = "approval" /\ s0.settling /\ IsCmd(r)
       THEN Switch(r, rv, s0, h, g, p, wasSettling)
       ELSE IF s0.aw = "approval"
       THEN LET s1 == [s0 EXCEPT !.aw = "none", !.settling = FALSE]
            IN IF ~IsCmd(r) /\ r \notin {"help", "helpG"} /\ s1.pending # "none"
               THEN \* its list: weighed, then sent back once or the action put to the person with it
                    LET w == Weigh(s1, g, TRUE, rv)
                    IN IF w.kind = "nudge"
                       THEN Continue([w.s EXCEPT !.aw = "approval", !.settling = TRUE], h, w.g, p)
                       ELSE ToHost("approval", s1.pending, FALSE, [w.s EXCEPT !.workSince = FALSE], h, w.g, p)
               ELSE \* a command or a question changes course: st.Pending = ""
                    Switch(r, rv, [s1 EXCEPT !.pending = "none"], h, g, p, wasSettling)
       ELSE Switch(r, rv, s0, h, g, p, wasSettling)

EnvOK(r) == /\ (r \in {"cmd", "untraced"} \cup Actions) => e.cmds > 0
            /\ r \in {"help", "helpG"} => e.helps > 0
            /\ r = "repeat" => s.succ > 0
            /\ (ObedientModel /\ r \in Actions) => r \notin s.declined
            /\ ObedientModel => r # "helpG"

EnvAfter(r) == [e EXCEPT !.cmds = IF r \in {"cmd", "untraced"} \cup Actions THEN @ - 1 ELSE @,
                         !.helps = IF r \in {"help", "helpG"} THEN @ - 1 ELSE @]

SoulReply ==
    /\ ctl = "soul"
    /\ \E r \in Replies, rv \in ReviewOutcomes :
         /\ EnvOK(r)
         /\ Apply(Turn(r, rv))
         /\ e' = EnvAfter(r)
         /\ ev' = <<"model", r, IF r = "done" THEN rv ELSE "-">>

\* rc.Provider.Chat failed transiently (transientModelError): asked again up to maxModelTries; then fatal.
SoulTimeout ==
    /\ ctl = "soul"
    /\ ev' = <<"model call failed (transient)", IF s.tries + 1 < MaxModelTries THEN "asked again" ELSE "last try">>
    /\ IF s.tries + 1 < MaxModelTries
       THEN /\ s' = [s EXCEPT !.tries = @ + 1]
            /\ UNCHANGED <<ctl, h, p, e, g>>
       ELSE /\ Apply(ToHost("failed", "none", FALSE, s, h, [g EXCEPT !.fatalTries = s.tries + 1], p))
            /\ UNCHANGED e

\* --------------------------------------------------------------------------
\* Resuming the soul with the person's reply (scheduler restore + operator resume)
\* --------------------------------------------------------------------------
Resume(k, hh, gg, pp) ==
    LET approvedNow == IsApproval(k)
        s0 == [s EXCEPT !.appr = IF s.sched = "approval" /\ approvedNow /\ s.pending # "none"
                                  THEN @ \cup {s.pending} ELSE @,
                        !.sched = "none", !.tries = 0, !.degen = 0]
    IN IF s0.pending # "none"
       THEN \* operator.go resume: the pending action is run if approved, else "the human did not approve"
            LET act == s0.pending
                s1  == [s0 EXCEPT !.pending = "none"]
                \* a question asked while st.Pending was still set is answered as a refusal of that action; a yes on
                \* a condition is taken by IsApproval as a yes: the action is run (or asked again) at once, and the
                \* condition never reaches the model
                g1  == [gg EXCEPT !.misrouted = @ \/ (s.sched = "input") \/ (k = "yesIf")]
            IN IF act \in s1.appr
               THEN IF act = "S"
                    THEN Continue(SWork(s1), HWork(hh), Commit("S", GWork(g1, pp)), pp)
                    ELSE IF hh.guiGrant
                         THEN Continue(SWork(s1), [HWork(hh) EXCEPT !.guiGrant = FALSE], Commit("G", GWork(g1, pp)), pp)
                         ELSE \* stopped again on the screen: o.pause, no round
                              ToHost("approval", "G", FALSE, [s1 EXCEPT !.pending = "G"],
                                     [HWork(hh) EXCEPT !.guiPending = TRUE], StartEp("G", g1), pp)
               ELSE Continue([s1 EXCEPT !.declined = @ \cup {act}], hh, g1, pp)
       ELSE IF s0.scrConf /\ IsConfirm(k)
       THEN Continue(s0, hh, gg, pp)
       ELSE Continue([s0 EXCEPT !.scrConf = FALSE, !.askedUntr = FALSE,
                                !.untr = IF s0.askedUntr THEN 0 ELSE @,
                                !.jq = FALSE, !.unrev = @ \/ s0.jq], hh, gg, pp)

\* --------------------------------------------------------------------------
\* The host
\* --------------------------------------------------------------------------
Refused == h.refused /\ ~h.workAfter                 \* guidance.refused() returns their words
BareNoPending == g.lastWord = "bare" /\ ~g.workSinceWord

End(kind, hh, gg, pp) ==
    /\ ctl' = "end"
    /\ h' = hh
    /\ g' = [gg EXCEPT !.endKind = kind]
    /\ p' = [pp EXCEPT !.shown = "none", !.asked = "none", !.out = "none"]
    /\ UNCHANGED s

\* skill_task._retry_engine / _confirm_reply: the engine gets the task again - a new soul job (reply=False drops
\* the paused one); approval.json and guidance._refused are per task and stay.
RetryEngine(att, hh, gg, pp) ==
    /\ ctl' = "soul"
    /\ s' = SoulInit
    /\ h' = [hh EXCEPT !.attempt = att]
    /\ g' = [gg EXCEPT !.inEp = "none", !.epAsks = 0, !.rqJob = 0, !.hqJob = 0, !.unacc = FALSE]
    /\ p' = [pp EXCEPT !.out = "none", !.shown = "none", !.asked = "none"]

ConfirmAsk(hh, gg) ==
    LET n == hh.confirms + 1
    IN IF n > MaxConfirmRounds
       THEN End("notdone", hh, gg, p)
       ELSE /\ ctl' = "person"
            /\ h' = [hh EXCEPT !.confirms = n]
            /\ p' = [p EXCEPT !.shown = "confirm", !.asked = "host", !.out = "none"]
            /\ g' = [gg EXCEPT !.confirmAsks = Sat(@ + 1, MaxConfirmRounds + 1)]
            /\ UNCHANGED s

\* thebot_harness._handle_job, outcome needs_approval: ask_for_query(kind="approval") - never self-answered
HostApproval ==
    /\ ctl = "host" /\ p.out = "approval"
    /\ ctl' = "person"
    /\ p' = [p EXCEPT !.shown = "approval", !.asked = p.qo, !.out = "none"]
    /\ g' = [g EXCEPT !.epAsks = IF g.inEp = p.qo THEN Sat(@ + 1, 2) ELSE @,
                      \* (an approval for ANOTHER action after a bare no is the refusal scoped to its action)
                      !.reask = @ \/ (p.qo \in g.noed)]
    /\ ev' = <<"host shows approval", p.qo>>
    /\ UNCHANGED <<s, h, e>>

\* outcome needs_input: "Stopped, as you said" when guidance.refused(); else _escalate -> ask_for_query(kind="query"),
\* where skill_task._question_gate may answer it from what the person said before (a thread resumes the engine).
HostQuestion ==
    /\ ctl = "host" /\ p.out = "question"
    /\ IF Refused
       THEN /\ End("stopped", h, [g EXCEPT !.stoppedWrong = @ \/ (g.lastWord \notin {"bare", "stop"})], p)
            /\ ev' = <<"host: Stopped, as you said (instead of asking)", p.qo>>
       ELSE LET g1 == [g EXCEPT !.bareNoIgnored = @ \/ BareNoPending]
            IN \/ /\ h.selfAns < MaxSelfAnswers
                  /\ Apply(Resume("selfanswer", [h EXCEPT !.selfAns = @ + 1], g1, [p EXCEPT !.out = "none"]))
                  /\ ev' = <<"host answers it from the person's earlier words", p.qo>>
               \/ /\ ev' = <<"host shows question", p.qo>>
                  /\ ctl' = "person"
                  /\ p' = [p EXCEPT !.shown = "question", !.asked = p.qo, !.out = "none"]
                  /\ g' = [g1 EXCEPT !.rqJob = IF p.qo = "review" THEN Sat(@ + 1, MaxReviewQuestions + 1) ELSE @,
                                     !.hqJob = IF p.qo \in {"review", "doubt"} THEN Sat(@ + 1, HarnessQCap + 1) ELSE @,
                                     !.epAsks = IF p.settle /\ g.inEp # "none" THEN Sat(@ + 1, 2) ELSE @]
                  /\ UNCHANGED <<s, h>>
    /\ UNCHANGED e

\* The final check's verdict (verify.judge): it never calls a task done whose irreversible action did not happen.
Verdicts == IF Actions \subseteq g.committed THEN {"DONE", "NOT_DONE", "UNSURE", "CANNOT"}
                                            ELSE {"NOT_DONE", "UNSURE", "CANNOT"}

\* outcome answered: write_final_reply -> messaging's final gate, skill_task._gate
HostAnswered ==
    /\ ctl = "host" /\ p.out = "answered"
    /\ LET g1 == [g EXCEPT !.bareNoIgnored = @ \/ (BareNoPending /\ ~Refused)]
       IN IF ~OnScreen
          THEN End("unchecked", h, g1, p) /\ ev' = <<"host: report goes out unchecked">>
          ELSE IF Refused
          THEN /\ End("stopped", h, [g1 EXCEPT !.stoppedWrong = @ \/ (g.lastWord \notin {"bare", "stop"})], p)
               /\ ev' = <<"host: Stopped there, as you said (final check skipped)">>
          ELSE \E v \in Verdicts :
                 /\ ev' = <<"final check", v>>
                 /\ CASE v = "DONE" -> End("checked", h, g1, p)
                      [] v = "NOT_DONE" /\ h.attempt < MaxEngineAttempts -> RetryEngine(h.attempt + 1, h, g1, p)
                      [] OTHER -> ConfirmAsk(h, g1)
    /\ UNCHANGED e

\* outcome failed (the model call failed maxModelTries times): _fail_on_backend -> an error report
HostFailed ==
    /\ ctl = "host" /\ p.out = "failed"
    /\ End("failed", h, g, p)
    /\ ev' = <<"host: the task ends as failed (model did not answer)">>
    /\ UNCHANGED e

\* --------------------------------------------------------------------------
\* The person
\* --------------------------------------------------------------------------
\* skill_task._confirm_reply
ConfirmReply(k, hh, gg, pp) ==
    CASE k = "done" -> End("confirmed", hh, gg, pp)
      [] k = "yes" -> \* VAGUE_YES: "a plain yes is not enough here" - asked again
                      /\ ctl' = "person" /\ h' = hh /\ g' = gg
                      /\ p' = [pp EXCEPT !.shown = "confirm", !.asked = "host"]
                      /\ UNCHANGED s
      [] k \in {"no", "stop"} -> End("notdone", hh, gg, pp)
      [] OTHER -> RetryEngine(1, hh, gg, pp)

\* watchers.answer_waiting: approval.resolve_reply settles approval.json's pending with ANY reply, then
\* guidance.note_reply, then STOP_ONLY ends the task (_called_off), else continue_after_query resumes the soul.
PersonReply ==
    /\ ctl = "person"
    /\ \E k \in RepliesFor(p.shown) :
       \* read_reply is consulted only when an on-screen approval is pending
       \E rr \in (IF h.guiPending THEN GrantReadings(k) ELSE {"-"}) :
         \* (third element: what read_reply made of it for approval.json's pending approval, "-" when none was pending)
         /\ ev' = <<"person", k, rr>>
         /\ LET outcome == IF h.guiPending THEN (IF rr = "yes" THEN "granted" ELSE "declined") ELSE "none"
                h1 == [h EXCEPT !.guiPending = FALSE, !.guiGrant = @ \/ (outcome = "granted")]
                onApproval == p.shown = "approval"
                \* the attempt goes on after a yes (on a condition too); any other reply declines it
                goesOn == onApproval /\ k \in {"yes", "yesIf"}
                g1 == [g EXCEPT !.misrouted = @ \/ (h.guiPending /\ ~(onApproval /\ p.asked = "G")),
                                !.yes = IF onApproval /\ k = "yes" THEN @ \cup {p.asked} ELSE @,
                                !.noed = IF onApproval /\ k = "no" THEN @ \cup {p.asked} ELSE @,
                                !.lastWord = Word(k),
                                !.workSinceWord = FALSE,
                                !.inEp = IF onApproval /\ ~goesOn THEN "none" ELSE @,
                                !.epAsks = IF onApproval /\ ~goesOn THEN 0 ELSE @]
                p1 == [p EXCEPT !.shown = "none", !.asked = "none"]
            IN IF p.shown = "confirm"
               THEN ConfirmReply(k, h1, g1, p1)
               ELSE LET h2 == [h1 EXCEPT !.refused = (outcome = "declined" /\ IsRefusal(k)) \/ StopOnly(k),
                                          !.workAfter = FALSE]
                    IN IF StopOnly(k)
                       THEN End("stopped", h2, g1, p1)
                       ELSE Apply(Resume(k, h2, g1, p1))
    /\ UNCHANGED e

\* A "stop" sent while the worker works and no question is open: not a reply file, so watchers._pass leaves it
\* queued behind the task (it runs afterwards as a request of its own: "nothing to stop").
PersonStopMidWork ==
    /\ ctl \in {"soul", "host"}
    /\ ~p.stopQueued
    /\ p' = [p EXCEPT !.stopQueued = TRUE]
    /\ ev' = <<"person sends stop while the worker works (queued)">>
    /\ UNCHANGED <<ctl, s, h, e, g>>

Terminated == ctl = "end" /\ UNCHANGED vars

Next == \/ SoulReply \/ SoulTimeout
        \/ HostApproval \/ HostQuestion \/ HostAnswered \/ HostFailed
        \/ PersonReply \/ PersonStopMidWork
        \/ Terminated

Spec == Init /\ [][Next]_vars

\* Liveness: the soul, the host and the person each eventually take an enabled step (the person eventually
\* answers what is on their screen); the worker model's commands and questions are finite (MaxCmds, MaxHelps).
LiveSpec == /\ Spec
            /\ WF_vars(SoulReply \/ SoulTimeout)
            /\ WF_vars(HostApproval \/ HostQuestion \/ HostAnswered \/ HostFailed)
            /\ WF_vars(PersonReply)

\* --------------------------------------------------------------------------
\* Properties
\* --------------------------------------------------------------------------
TypeOK ==
    /\ ctl \in {"soul", "host", "person", "end"}
    /\ s.aw \in {"none", "done", "approval"}
    /\ s.pending \in {"none"} \cup Actions
    /\ s.dr \in 0..MaxDoneRounds /\ s.ar \in 0..MaxApprovalRounds /\ s.pa \in 0..MaxReviewQuestions
    /\ s.succ \in 0..MinJ /\ s.tries \in 0..(MaxModelTries - 1)
    /\ p.shown \in {"none", "approval", "question", "confirm"}
    /\ e.cmds \in 0..MaxCmds /\ e.helps \in 0..MaxHelps

\* No state where the worker waits for the person while the person has nothing to answer.
NoDeadlock == ctl = "person" => p.shown # "none"

\* Every irreversible action runs on a yes the person gave to an approval that named it, and that yes is used once.
ApprovalIntegrity == ~g.badCommit

\* One gate stop of an irreversible action is put to the person at most once (approval, or a review question
\* raised while settling at that approval) until it runs or the person declines it.
AtMostOneAskPerAction == g.epAsks <= 1

\* Every review point ends resolved (a later review that was made), asked, or carried into the result/approval.
DoubtsAccountedFor == ~g.dropped

\* A redirect ("no, do X first") never ends the task as "stopped, as you said".
RefusalScopedToAction == ~g.stoppedWrong

\* A bare no (or stop) with no work after it ends the task; it is not followed by more asking or a retry.
BareRefusalEndsTask == ~g.bareNoIgnored

\* An action the person said a bare no to is not put to them again in the same task.
RefusedActionNotReasked == ~g.reask

\* Harness questions per soul job are within the caps; the host's confirm questions within MAX_CONFIRM_ROUNDS.
BoundedQuestions == /\ g.rqJob <= MaxReviewQuestions
                    /\ g.hqJob <= HarnessQCap
                    /\ g.confirmAsks <= MaxConfirmRounds

\* A reply is applied to what the person was asked.
NoAnswerLost == ~g.misrouted

\* A transient model failure is retried: only MaxModelTries failures in a row end the task.
TransientNotFatal == g.endKind = "failed" => g.fatalTries = MaxModelTries

\* After the person says stop, no more work is done.
StopHonoured == ~g.workAfterStop

\* Liveness (LiveSpec): the worker side never loops without the person; and every task ends.
NoLivelock == []<>(ctl \in {"person", "end"})
Termination == <>(ctl = "end")

\* --------------------------------------------------------------------------
\* VIEWs (check.sh): no step reads a ghost field to decide what happens, except g.committed (Verdicts). So two states
\* that agree on the control state and on the ghost fields one property reads are equivalent for that property, and
\* TLC may keep one of them. (s.declined steers only an obedient model.)
\* --------------------------------------------------------------------------
SoulView == IF ObedientModel THEN s ELSE [s EXCEPT !.declined = {}]
Control == <<ctl, SoulView, h, p, e, g.committed>>
V_TypeOK                  == Control
V_NoDeadlock              == Control
V_ApprovalIntegrity       == <<Control, g.yes, g.badCommit>>
V_AtMostOneAskPerAction   == <<Control, g.inEp, g.epAsks>>
V_DoubtsAccountedFor      == <<Control, g.unacc, g.dropped>>
V_RefusalScopedToAction   == <<Control, g.lastWord, g.stoppedWrong>>
V_BareRefusalEndsTask     == <<Control, g.lastWord, g.workSinceWord, g.bareNoIgnored>>
V_RefusedActionNotReasked == <<Control, g.noed, g.reask>>
V_BoundedQuestions        == <<Control, g.rqJob, g.hqJob, g.confirmAsks>>
V_NoAnswerLost            == <<Control, g.misrouted>>
V_TransientNotFatal       == <<Control, g.endKind, g.fatalTries>>
V_StopHonoured            == <<Control, g.workAfterStop>>
=============================================================================
