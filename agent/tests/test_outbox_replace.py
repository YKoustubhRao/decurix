import io
import json
import tempfile
import unittest
import urllib.error
from pathlib import Path
from unittest import mock

from decurix_agent.tools import outbox


class FakeOutbox:
    """The control plane's POST /api/outbox: ids in order, `replaces` honoured for drafts it still holds."""

    def __init__(self):
        self.drafts, self.n = {}, 0

    def urlopen(self, request, timeout=None):
        # The patch is on urllib.request itself: a background sender left by another test (progress events) posts
        # through it too, and must not be taken for a draft.
        if not getattr(request, "full_url", "").endswith("/api/outbox"):
            return mock.MagicMock(__enter__=lambda s: io.BytesIO(b"{}"), __exit__=lambda *a: False)
        body = json.loads(request.data)
        old = body.get("replaces")
        if old and (old not in self.drafts or self.drafts[old]["status"] != "draft"):
            raise urllib.error.HTTPError(request.full_url, 409, "conflict", {}, io.BytesIO(b'{"error":"that email was already sent or discarded"}'))
        self.n += 1
        new = f"mail_{self.n}"
        self.drafts[new] = dict(body, status="draft")
        if old:
            self.drafts[old]["status"] = "discarded"
        answer = json.dumps({"id": new, "status": "draft", "replaced": old or ""}).encode()
        return mock.MagicMock(__enter__=lambda s: io.BytesIO(answer), __exit__=lambda *a: False)

    def live(self):
        return sorted(k for k, v in self.drafts.items() if v["status"] == "draft")


class ADraftAgainReplacesTheTasksEarlierOne(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.patches = [mock.patch.object(outbox, "DRAFTS_DIR", Path(self.tmp.name)),
                        mock.patch.object(outbox.task_context, "current", return_value="t1"),
                        mock.patch.object(outbox.progress, "emit")]
        for p in self.patches:
            p.start()
        self.cp = FakeOutbox()
        self.net = mock.patch.object(outbox.urllib.request, "urlopen", self.cp.urlopen)
        self.net.start()

    def tearDown(self):
        self.net.stop()
        for p in reversed(self.patches):
            p.stop()
        self.tmp.cleanup()

    def test_the_same_letter_drafted_again_replaces_the_first(self):
        outbox.draft(["ap@globex.example"], "Payment reminder - overdue", "v1")
        outbox.draft(["billing@hooli.example"], "Final notice", "v1")
        said = outbox.draft(["AP@globex.example"], "Payment reminder: overdue", "v2 with the corrected total")
        self.assertIn("replaces the earlier draft mail_1", said)
        self.assertEqual(self.cp.live(), ["mail_2", "mail_3"])
        # What the task made, as the final check reads it: one letter each.
        self.assertEqual([d["body"] for d in outbox.drafted()], ["v1", "v2 with the corrected total"])

    def test_a_stage_run_again_replaces_its_earlier_draft_to_the_same_recipient(self):
        # Round 5 P1 (2026-10-20), Finding BC: a re-check drafted a second reminder to one customer, another subject.
        attempt = {"n": 1}
        with mock.patch.object(outbox, "_stage", lambda: ("run1:s2-reminders", attempt["n"])):
            outbox.draft(["accounts@coromandelclin.example.in"], "Overdue invoices - Coromandel Clinics", "v1")
            outbox.draft(["accounts@konkanreso.example.in"], "Overdue invoices - Konkan Resorts", "v1")
            attempt["n"] = 2
            said = outbox.draft(["accounts@coromandelclin.example.in"], "Payment reminder", "v2")
        self.assertIn("replaces the earlier draft mail_1", said)
        self.assertEqual(self.cp.live(), ["mail_2", "mail_3"])
        # Within one attempt, two letters to one address stay two (separate subjects).
        with mock.patch.object(outbox, "_stage", lambda: ("run1:s2-reminders", 2)):
            said = outbox.draft(["accounts@konkanreso.example.in"], "Statement of account", "v1")
        self.assertIn("replaces the earlier draft mail_2", said)  # an earlier attempt's letter to it
        with mock.patch.object(outbox, "_stage", lambda: ("run1:s2-reminders", 2)):
            said = outbox.draft(["accounts@konkanreso.example.in"], "Another matter", "v1")
        self.assertNotIn("replaces", said)

    def test_separate_letters_with_other_files_are_not_taken_for_one(self):
        a, b = Path(self.tmp.name) / "INV-1.pdf", Path(self.tmp.name) / "INV-2.pdf"
        a.write_bytes(b"%PDF-1")
        b.write_bytes(b"%PDF-2")
        outbox.draft(["billing@globex.example"], "Invoice", "first", attachments=[str(a)])
        said = outbox.draft(["billing@globex.example"], "Invoice", "second", attachments=[str(b)])
        self.assertNotIn("replaces", said)
        self.assertEqual(self.cp.live(), ["mail_1", "mail_2"])
        # The same letter with the same file drafted again still replaces it.
        self.assertIn("replaces the earlier draft mail_2",
                      outbox.draft(["billing@globex.example"], "Invoice", "second, fixed", attachments=[str(b)]))

    def test_an_earlier_draft_a_person_already_discarded_leaves_a_new_draft(self):
        outbox.draft(["ap@globex.example"], "Payment reminder", "v1")
        self.cp.drafts["mail_1"]["status"] = "discarded"
        self.assertIn("Drafted (id mail_2)", outbox.draft(["ap@globex.example"], "Payment reminder", "v2"))
        with self.assertRaises(outbox.DraftError):
            outbox.draft(["ap@globex.example"], "Other", "v3", replaces="mail_1")


    def test_a_control_plane_that_ignores_replaces_is_said_to(self):
        outbox.draft(["ap@globex.example"], "Payment reminder", "v1")
        def old_control_plane(request, timeout=None):
            # Files the draft and answers as a control plane from before `replaces` existed: no "replaced" at all.
            self.cp.n += 1
            answer = json.dumps({"id": f"mail_{self.cp.n}", "status": "draft"}).encode()
            return mock.MagicMock(__enter__=lambda s: io.BytesIO(answer), __exit__=lambda *a: False)

        with mock.patch.object(outbox.urllib.request, "urlopen", old_control_plane):
            said = outbox.draft(["ap@globex.example"], "Payment reminder", "v2")
        self.assertIn("did not take it as a replacement", said)
        self.assertIn("mail_1", said)


if __name__ == "__main__":
    unittest.main()


class ARecipientIsAnAddress(unittest.TestCase):
    """Found live (2026-10-14): drafts went to "grant", "holt" and "wei" - surnames where addresses were meant."""

    def test_a_name_that_is_not_an_address_is_refused_before_anything_is_drafted(self):
        with mock.patch.object(outbox.urllib.request, "urlopen") as sent:
            with self.assertRaises(outbox.DraftError) as refused:
                outbox.draft(["grant"], "Commission", "body")
        sent.assert_not_called()
        self.assertIn("'grant' is not an email address", str(refused.exception))

    def test_an_address_written_with_its_name_is_an_address(self):
        self.assertEqual(outbox._address_of("Alicia Grant <alicia@example.com>"), "alicia@example.com")
        self.assertTrue(outbox.ADDRESS.fullmatch(outbox._address_of("Alicia Grant <alicia@example.com>")))


class AFollowUpReplacesTheReportedTasksDrafts(unittest.TestCase):
    """Series 16 H3: "leave Ishaan out" came after the fee run was reported; the follow-up task redid the drafts and the
    first run's Rao draft, still billing him, stayed beside the new one."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.task = "t1"
        self.patches = [mock.patch.object(outbox, "DRAFTS_DIR", Path(self.tmp.name)),
                        mock.patch.object(outbox.task_context, "current", lambda: self.task),
                        mock.patch.object(outbox.progress, "emit")]
        for p in self.patches:
            p.start()
        self.cp = FakeOutbox()
        self.net = mock.patch.object(outbox.urllib.request, "urlopen", self.cp.urlopen)
        self.net.start()

    def tearDown(self):
        self.net.stop()
        for p in reversed(self.patches):
            p.stop()
        self.tmp.cleanup()

    def test_the_new_letter_to_the_same_family_replaces_the_reported_tasks(self):
        outbox.draft(["rao@x.example"], "Term 2 fee demand", "Ishaan and Tara: Rs 89,300")
        outbox.draft(["pillai@x.example"], "Term 2 fee demand", "Rs 44,000")
        self.task = "t2"
        with mock.patch.object(outbox.task_context, "continues", lambda: "t1"):
            outbox.draft(["rao@x.example"], "Term 2 fee due by 15/11/2026", "Tara: Rs 41,200")
        live = {k: v for k, v in self.cp.drafts.items() if v["status"] == "draft"}
        self.assertEqual(sorted(v["body"] for v in live.values()), ["Rs 44,000", "Tara: Rs 41,200"])
        self.assertEqual(next(v for v in live.values() if v["body"].startswith("Tara"))["continues"], "t1")

    def test_a_task_that_continues_nothing_never_touches_another_tasks_drafts(self):
        outbox.draft(["rao@x.example"], "Term 2 fee demand", "v1")
        self.task = "t2"
        with mock.patch.object(outbox.task_context, "continues", lambda: ""):
            outbox.draft(["rao@x.example"], "Term 2 fee demand", "a new, separate letter")
        self.assertEqual(len(self.cp.live()), 2)
