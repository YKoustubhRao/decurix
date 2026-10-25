------------------------------ MODULE PlannedRun ------------------------------
(***************************************************************************)
(* Messages that reach a planned run (a long request, long_task.py) while  *)
(* it waits on the person's answer to a question Q, AS IMPLEMENTED on      *)
(* 2026-10-23: _take_message, classify (kinds incl. new_request),          *)
(* _pending_request_reply / _choice (held requests), confirming (unclear), *)
(* last_heard / HEARD_AGAIN, _ask_again and state.last_query_text.         *)
(*                                                                         *)
(* The run is parked on Q (conclude(): status awaiting_answer, nothing     *)
(* else can run, waiting_for_query).  The person sends messages; the       *)
(* model's reading of each (classify) is nondeterministic within what it   *)
(* can plausibly answer.  Message identity stands for its text: the person *)
(* sending "ans" again sends the same words (the same last_heard hash).    *)
(***************************************************************************)
EXTENDS Naturals

CONSTANTS
    MaxMsgs,      \* safety runs: messages the person sends in all
    PersonMsgs,   \* what the person may send
    Cooperative   \* liveness runs: the person answers each open card as someone whose message WAS the answer would

Msgs == {"ans",       \* their answer to Q (e.g. the practice's eight numbered rules)
         "req",       \* a request of its own
         "status",    \* "how is it going?"
         "stop",
         "after",     \* to the held-request question: "finish that one first"
         "replace",   \* ... "drop it and do this instead"
         "itsAnswer", \* "that was my answer to your question"
         "vague"}     \* a reply _choice cannot read
Kinds == {"answer", "amendment", "status", "stop", "unrelated", "new_request", "unclear"}
HEARD_AGAIN == {"amendment", "unclear", "new_request"}      \* long_task.HEARD_AGAIN
Cards == {"none", "Q", "meta", "unclear"}

VARIABLES
    run,        \* "waiting" (parked on Q) | "answered" | "stopped"
    wq,         \* state.waiting_for_query
    shown,      \* the card open to the person, by its text: "Q" | "meta" (the held-request question) | "unclear" | "none"
    lastQ,      \* state.last_query_text, by its text
    pend,       \* data["pending_request"] is set
    pendAsks,   \* ... its "asks"
    held,       \* the message held as a request of its own
    confirming, \* data["confirming"]["message"]: the message kept aside by an unclear reading
    heard,      \* data["last_heard"]: [msg, kind]
    sent,
    queued,     \* messages released to run as requests after this run (_release)
    g           \* ghost

vars == <<run, wq, shown, lastQ, pend, pendAsks, held, confirming, heard, sent, queued, g>>

Init == /\ run = "waiting" /\ wq = TRUE /\ shown = "Q" /\ lastQ = "Q"
        /\ pend = FALSE /\ pendAsks = 0 /\ held = "none" /\ confirming = "none"
        /\ heard = [msg |-> "none", kind |-> "none"] /\ sent = 0 /\ queued = {}
        /\ g = [dropped |-> FALSE, misapplied |-> FALSE, wrongCard |-> FALSE, stoppedWrong |-> FALSE]

\* What the reader is shown as the message: a reply to "is this your answer?" is read for the EARLIER message.
Content(m) == IF m = "itsAnswer" /\ confirming # "none" THEN confirming ELSE m

\* classify() is told when the same message came again after a reading the question was asked again after.
Hint(m) == heard.kind \in HEARD_AGAIN /\ heard.msg = m

\* ASSUMPTION: with that hint the reader takes the answer as the answer (optimistic); without it, it may misread.
Readings(c, hint) ==
    CASE c = "ans"       -> IF hint THEN {"answer"} ELSE {"answer", "unclear", "amendment", "unrelated", "new_request"}
      [] c = "req"       -> {"new_request", "unrelated"}
      [] c = "status"    -> {"status"}
      [] c = "itsAnswer" -> {"unclear", "unrelated", "answer"}
      [] OTHER           -> {"unclear", "unrelated"}

\* _choice(): a word list over the reply to the held-request question.
Choices(m) ==
    CASE m = "after"     -> {"after"}
      [] m = "replace"   -> {"replace"}
      [] m = "itsAnswer" -> {"answer"}
      [] m = "ans"       -> {"", "after", "replace", "answer"}  \* an answer's own words: "first", "then", "instead", "question"
      [] m = "req"       -> {"", "after", "replace"}
      [] OTHER           -> {""}

\* _ask_again(text): ask_for_query shows it and makes it state.last_query_text.
\* A re-ask shows last_query_text; it is stale when what it asks is no longer open.
Stale(card, pend1, conf1) == (card = "meta" /\ ~pend1) \/ (card = "unclear" /\ conf1 = "none")

\* The body of _take_message after classify, for reading k of message m.
Process(m, k) ==
    LET c     == Content(m)
        conf1 == IF confirming # "none" /\ k # "unclear" THEN "none" ELSE confirming
    IN /\ heard' = [msg |-> m, kind |-> k]
       /\ CASE k = "new_request" ->
                 \* _take_new_request: held (the EARLIER message when one was kept aside), asked once what to do
                 /\ held' = IF confirming # "none" THEN confirming ELSE m
                 /\ pend' = TRUE /\ pendAsks' = 1
                 /\ shown' = "meta" /\ lastQ' = "meta" /\ wq' = TRUE
                 /\ confirming' = "none"
                 /\ UNCHANGED <<run, queued, g>>
            [] k = "stop" ->
                 /\ run' = "stopped" /\ wq' = FALSE /\ shown' = "none"
                 /\ g' = [g EXCEPT !.stoppedWrong = @ \/ (m # "stop")]
                 /\ confirming' = conf1
                 /\ UNCHANGED <<lastQ, pend, pendAsks, held, queued>>
            [] k \in {"unrelated", "status", "amendment"} ->
                 \* unrelated: marked handled, "send it again when this one is finished" + last_query_text;
                 \* status: plan + "Still waiting on you:" + last_query_text; amendment: applied as a change to
                 \* the task, then "Still waiting on your answer:" + last_query_text.
                 /\ shown' = lastQ
                 /\ g' = [g EXCEPT !.dropped = @ \/ (k \in {"unrelated", "status"} /\ c = "ans"),
                                   !.misapplied = @ \/ (k = "amendment" /\ c = "ans"),
                                   !.wrongCard = @ \/ Stale(lastQ, pend, conf1)]
                 /\ confirming' = conf1
                 /\ UNCHANGED <<run, wq, lastQ, pend, pendAsks, held, queued>>
            [] k = "unclear" ->
                 \* nothing applied; the message kept aside (confirming = this message); one short question
                 \* with "(Still open: " + last_query_text[:600] + ")"
                 /\ confirming' = m
                 /\ shown' = "unclear" /\ lastQ' = "unclear"
                 /\ g' = [g EXCEPT !.dropped = @ \/ (confirming = "ans" /\ m # "ans")]
                 /\ UNCHANGED <<run, wq, pend, pendAsks, held, queued>>
            [] k = "answer" ->
                 /\ run' = "answered" /\ wq' = FALSE /\ shown' = "none"
                 /\ g' = [g EXCEPT !.misapplied = @ \/ (c # "ans")]
                 /\ confirming' = conf1
                 /\ UNCHANGED <<lastQ, pend, pendAsks, held, queued>>

\* Process for the re-read of a held message: the reply m that asked for it was consumed by _choice.
Process2(hm, k, m) ==
    LET c     == hm
        conf1 == IF confirming # "none" /\ k # "unclear" THEN "none" ELSE confirming
        lost  == m = "ans" /\ hm # "ans"
    IN /\ heard' = [msg |-> hm, kind |-> k]
       /\ UNCHANGED <<pendAsks, held, queued>>
       /\ CASE k = "amendment" ->
                 /\ shown' = lastQ
                 /\ g' = [g EXCEPT !.misapplied = @ \/ (c = "ans"), !.dropped = @ \/ lost,
                                   !.wrongCard = @ \/ Stale(lastQ, FALSE, conf1)]
                 /\ confirming' = conf1
                 /\ UNCHANGED <<run, wq, lastQ>>
            [] k = "unclear" ->
                 /\ confirming' = hm
                 /\ shown' = "unclear" /\ lastQ' = "unclear"
                 /\ g' = [g EXCEPT !.dropped = @ \/ lost]
                 /\ UNCHANGED <<run, wq>>
            [] k = "answer" ->
                 /\ run' = "answered" /\ wq' = FALSE /\ shown' = "none"
                 /\ g' = [g EXCEPT !.misapplied = @ \/ (c # "ans"), !.dropped = @ \/ lost]
                 /\ confirming' = conf1
                 /\ UNCHANGED lastQ

\* _pending_request_reply: the reply to "finish that one first, drop it, or is this your answer?"
PendingReply(m) ==
    \E ch \in Choices(m) :
      IF ch = "" /\ pendAsks < 2
      THEN \* asked again
           /\ pendAsks' = pendAsks + 1
           /\ shown' = "meta" /\ lastQ' = "meta"
           /\ g' = [g EXCEPT !.dropped = @ \/ (m = "ans")]
           /\ UNCHANGED <<run, wq, pend, held, confirming, heard, queued>>
      ELSE IF ch = "answer"
      THEN \* the held message is read again "about this" (said_answer): read as anything but an answer or a
           \* change, it is asked about plainly (unclear)
           /\ pend' = FALSE
           /\ \E k \in Readings(held, Hint(held)) :
                LET kk == IF k \in {"answer", "amendment"} THEN k ELSE "unclear"
                IN Process2(held, kk, m)
      ELSE IF ch = "replace"
      THEN /\ pend' = FALSE /\ queued' = queued \cup {held}
           /\ run' = "stopped" /\ wq' = FALSE /\ shown' = "none"
           /\ g' = [g EXCEPT !.stoppedWrong = @ \/ (m # "replace"),
                             !.misapplied = @ \/ (held = "ans"),
                             !.dropped = @ \/ (m = "ans" /\ held # "ans")]
           /\ UNCHANGED <<lastQ, pendAsks, held, confirming, heard>>
      ELSE \* "after" - or asked twice without a clear choice: released to run after this run, and
           \* "if waiting: state.waiting_for_query = True  # the question in hand still stands" - nothing is shown
           /\ pend' = FALSE /\ queued' = queued \cup {held}
           /\ wq' = wq /\ shown' = "none"
           /\ g' = [g EXCEPT !.misapplied = @ \/ (held = "ans"),
                             !.dropped = @ \/ (m = "ans" /\ held # "ans")]
           /\ UNCHANGED <<run, lastQ, pendAsks, held, confirming, heard>>

\* _take_message.  A message closes the card it answers; the rest of the step decides what is shown next.
Take(m) ==
    IF m = "stop"
    THEN \* STOP_ONLY skips the held-request reading; classify returns "stop"
         /\ run' = "stopped" /\ wq' = FALSE /\ shown' = "none"
         /\ heard' = [msg |-> m, kind |-> "stop"]
         /\ confirming' = "none"
         /\ UNCHANGED <<lastQ, pend, pendAsks, held, queued, g>>
    ELSE IF pend
    THEN PendingReply(m)
    ELSE \E k \in Readings(Content(m), Hint(m)) : Process(m, k)

\* A cooperative person whose message was the answer: answers Q, says "it was my answer" to anything else open,
\* and waits when nothing is open.
CoopMsg(card) == IF card = "Q" THEN "ans" ELSE "itsAnswer"

Send ==
    /\ run = "waiting"
    /\ \E m \in PersonMsgs :
         /\ IF Cooperative THEN shown # "none" /\ m = CoopMsg(shown) ELSE sent < MaxMsgs
         /\ sent' = IF Cooperative THEN sent ELSE sent + 1
         /\ Take(m)

Terminated == run # "waiting" /\ UNCHANGED vars

Next == Send \/ Terminated

Spec == Init /\ [][Next]_vars
LiveSpec == Spec /\ WF_vars(Send)

TypeOK == /\ run \in {"waiting", "answered", "stopped"} /\ shown \in Cards /\ lastQ \in Cards \ {"none"}
          /\ held \in Msgs \cup {"none"} /\ confirming \in Msgs \cup {"none"} /\ heard.kind \in Kinds \cup {"none"}

\* The run waits on the person only while a question is open on their screen.
NoDeadlock == run = "waiting" => shown # "none"
\* The person's answer is applied to the question: never discarded, never applied as something else.
NoAnswerLost == ~g.dropped /\ ~g.misapplied
\* What a re-ask shows is still open.
RightQuestionShown == ~g.wrongCard
\* Only the person's stop or "drop it" stops the run.
StopOnlyByPerson == ~g.stoppedWrong
\* Liveness (LiveSpec, Cooperative): the answer is eventually applied.
AnswerApplied == <>(run = "answered")
=============================================================================
