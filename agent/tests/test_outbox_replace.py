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
