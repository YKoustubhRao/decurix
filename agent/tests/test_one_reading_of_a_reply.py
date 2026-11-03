"""A reply to an approval is read once, by one reader, and every part of the worker acts on that reading (2026-10-23).

A person's "haan fine" was a no to every word list; a "yes, provided ..." was a yes to the engine's own word list while
the gate read it as a condition. Code settles only a reply that plainly says yes or no; any other is the model's
reading; with no model to ask it is "unclear" - never guessed.
"""
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import llm_client
from decurix_agent.computer import approval


def model_reads(decision, condition=""):
    """Patches for a model that reads every reply as decision; the list of calls it got."""
    calls = []

    def chat(messages, **kw):
        calls.append(messages[-1]["content"])
        return {"content": json.dumps({"decision": decision, "condition": condition})}, {}
    return [mock.patch.object(llm_client, "available", lambda: True), mock.patch.object(llm_client, "chat", chat)], calls


class Base(unittest.TestCase):
    def setUp(self):
        approval._READ.clear()
        self.addCleanup(approval._READ.clear)

    def use(self, patches):
        for p in patches:
            p.start()
            self.addCleanup(p.stop)


class PlainRepliesNeedNoModel(Base):
    def test_plain_yes_and_plain_no(self):
        self.use([mock.patch.object(llm_client, "chat", side_effect=AssertionError("asked"))])
        for reply in ("yes", "ok", "k", "Yes please!", "ok go ahead", "fine, do it", "go for it"):
            self.assertEqual(approval.read_reply(reply, "file"), "yes", reply)
        for reply in ("no", "no, not yet", "hold on", "wait", "don't", "not now", "stop"):
            self.assertEqual(approval.read_reply(reply, "file"), "no", reply)


class EveryOtherReplyIsTheModels(Base):
    def test_a_yes_in_the_persons_own_words(self):
        patches, calls = model_reads("yes")
        self.use(patches)
        self.assertEqual(approval.read_reply("haan fine", 'click button "Submit"'), "yes")
        self.assertEqual(len(calls), 1)

    def test_a_short_yes_with_a_condition_is_not_a_yes(self):
        patches, calls = model_reads("if", "paid first")
        self.use(patches)
        self.assertEqual(approval.reading("yes if paid", "pay"), {"decision": "if", "condition": "paid first"})

    def test_a_question_back_decides_nothing(self):
        patches, _ = model_reads("unclear")
        self.use(patches)
        self.assertEqual(approval.read_reply("did you check the chat?", "file"), "unclear")
        self.assertFalse(approval.says_yes("did you check the chat?", "file"))

    def test_what_they_ask_of_the_worker_goes_with_the_reading(self):
        answer = {"content": json.dumps({"decision": "no", "condition": "", "wants": "which bill was counted as cancelled"})}
        self.use([mock.patch.object(llm_client, "available", lambda: True),
                  mock.patch.object(llm_client, "chat", lambda *a, **k: (answer, {}))])
        self.assertEqual(approval.reading("not yet - tell me which inv u counted as cancelled", "file"),
                         {"decision": "no", "condition": "", "wants": "which bill was counted as cancelled"})

    def test_one_reply_is_read_once(self):
        patches, calls = model_reads("yes")
        self.use(patches)
        for _ in range(3):
            approval.read_reply("theek hai, kar do", "file")
        self.assertEqual(len(calls), 1, "the parts that act on one reply read it once")

    def test_one_reply_is_read_again_for_another_action(self):
        # "yes, if the totals match" read for one action is not the reading of the same words for another (review
        # 2026-11-02: kept by the reply's text alone, a reading made for one action was reused for the next).
        patches, calls = model_reads("if", "totals match")
        self.use(patches)
        approval.reading("yes, once the totals match", "file the return")
        approval.reading("yes, once the totals match", "pay the challan")
        self.assertEqual(len(calls), 2)
        self.assertIn("pay the challan", calls[-1])


class WithNoModel(Base):
    def test_what_the_lists_cannot_settle_is_unclear(self):
        self.use([mock.patch.object(llm_client, "available", lambda: False)])
        self.assertEqual(approval.read_reply("haan fine", "file"), "unclear")
        self.assertEqual(approval.read_reply("yes if the client paid", "pay"), "unclear", "a yes that may carry a condition")
        # Remarks may change the action (review F-02): beyond a plain yes, no model means it is asked again.
        self.assertEqual(approval.read_reply("yes - the client has paid the challan; offset and file.", "x"), "unclear")
        self.assertEqual(approval.read_reply("no, do not file it yet please", "x"), "no")


class TheGateKeepsItsReadingForTheEngine(Base):
    def setUp(self):
        super().setUp()
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.use([mock.patch.object(approval, "STATE_DIR", Path(self.tmp.name)),
                  mock.patch.object(approval, "STATE_FILE", Path(self.tmp.name) / "approval.json")])

    def element(self):
        return {"role": "button", "name": "Send newsletter", "ref": "e1", "path": "main"}

    def test_a_yes_in_own_words_grants_and_the_reading_is_kept(self):
        patches, calls = model_reads("yes")
        self.use(patches)
        approval.check("click", self.element())
        self.assertEqual(approval.resolve_reply("haan fine", asked=approval.request_text(approval.pending())), "granted")
        self.assertEqual(approval.last_reading("haan  fine"), {"decision": "yes", "condition": ""})
        self.assertEqual(len(calls), 1)

    def test_the_reply_is_read_against_the_question_the_person_was_shown(self):
        # Found live (2026-10-24): a yes that answered the points the question listed, read against the bare action,
        # was "if" two times in three and the filing was asked for again.
        patches, calls = model_reads("yes")
        self.use(patches)
        approval.check("click", self.element())
        action = approval.pending()["action"]
        shown = f"I need your OK before I run this, because {action}\n\nStill open: - file sizes on the status bar"
        self.assertEqual(approval.resolve_reply("yes go. file sizes irrelevant", asked=shown), "granted")
        self.assertIn("Still open: - file sizes on the status bar", calls[-1])
        # A reply to a question about something else is not read as this action's: it settles nothing.
        approval.check("click", self.element())
        before = len(calls)
        self.assertIsNone(approval.resolve_reply("yes, fine by me", asked="Which client is this for?"))
        self.assertEqual(len(calls), before)
        self.assertFalse(approval.has_grant())

    def test_a_condition_grants_nothing_and_is_kept(self):
        patches, _ = model_reads("if", "the chat holds nothing")
        self.use(patches)
        approval.check("click", self.element())
        self.assertEqual(approval.resolve_reply("ok provided the chat has nothing",
                                                asked=approval.request_text(approval.pending())), "declined")
        self.assertFalse(approval.has_grant())
        self.assertEqual(approval.last_reading("ok provided the chat has nothing")["decision"], "if")
        self.assertIsNone(approval.last_reading("some other reply"))


class TheEngineIsSentTheReading(Base):
    def test_a_reply_to_an_approval_goes_to_soul_with_its_reading(self):
        from decurix_agent import state, thebot_harness
        patches, _ = model_reads("if", "check the chat")
        self.use(patches)
        sent = []
        self.use([mock.patch.object(state, "current_task", "file the return"),
                  mock.patch.object(thebot_harness, "_approval_asked", "Approval needed before I click FILE"),
                  mock.patch.object(thebot_harness, "_written_pause", None),
                  mock.patch.object(thebot_harness.approval, "last_reading", lambda reply: None),
                  mock.patch.object(thebot_harness, "_run_thebot",
                                    lambda request, reply, extra_context="", approval_reading=None:
                                    sent.append(approval_reading) or (None, "stop")),
                  mock.patch.object(thebot_harness, "_handle_job", lambda *a, **k: None)])
        thebot_harness.continue_after_query("ok, provided you checked the chat")
        self.assertEqual(sent, [{"decision": "if", "condition": "check the chat"}])


if __name__ == "__main__":
    unittest.main()
