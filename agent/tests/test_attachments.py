import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import attachments, record_task, skill_task, study
from decurix_agent.computer import learning


def request(text, *paths):
    """A request as the control plane sends it when files were attached (httpapi.withAttachments)."""

    return text + "\n\n" + attachments.MARK + "".join(f"\n  {p}" for p in paths)


class SplitTest(unittest.TestCase):
    def test_a_request_is_its_words_and_the_files_it_brought(self):
        linux = "/home/headless/work/inbox/up-1/PO.pdf"
        windows = r"C:\Users\ana\AppData\Local\Decurix\workers\w1\home\work\inbox\up-1\scan.png"
        content = request("Enter this purchase order", linux, windows)

        self.assertEqual(attachments.split(content), ("Enter this purchase order", [linux, windows]))
        self.assertEqual(attachments.text(content), "Enter this purchase order")
        self.assertEqual(attachments.files(content), [linux, windows])
        self.assertEqual(attachments.names([linux, windows]), "PO.pdf, scan.png")

    def test_a_request_with_no_files_is_left_as_it_is(self):
        self.assertEqual(attachments.split("what is the kernel version?"), ("what is the kernel version?", []))
        self.assertEqual(attachments.split(None), ("", []))

    def test_the_block_is_what_split_takes_apart(self):
        paths = ["/home/headless/work/inbox/up-2/a.pdf"]
        self.assertEqual(attachments.split("do it\n\n" + attachments.block(paths)), ("do it", paths))
        self.assertEqual(attachments.block([]), "")


class TicketTest(unittest.TestCase):
    def test_a_tickets_name_is_its_task_not_its_files(self):
        content = request("/record enter the purchase order", "/home/headless/work/inbox/up-1/PO.pdf")

        self.assertEqual(record_task.parse(content), "enter the purchase order")
        self.assertEqual(record_task.files(content), ["/home/headless/work/inbox/up-1/PO.pdf"])

        self.assertEqual(record_task.parse("/record fill in the form"), "fill in the form")
        self.assertEqual(record_task.files("/record fill in the form"), [])
        self.assertIsNone(record_task.parse(request("enter the purchase order", "/tmp/PO.pdf")))

    def test_files_attached_to_an_open_ticket_are_kept_with_its_recording(self):
        kept = {}

        def update(session_id, **fields):
            kept[session_id] = fields

        ticket = {"task_id": "t1", "title": "enter the PO", "session_id": "demo-1", "files": ["/a/PO.pdf"]}
        with mock.patch.object(record_task, "_ticket", ticket), \
                mock.patch.object(record_task.recording, "update", update), \
                mock.patch.object(record_task.demonstration, "status", return_value={"paused": False, "events": 2}), \
                mock.patch.object(record_task.progress, "emit"):
            status = record_task.attach(["/a/terms.pdf", "/a/PO.pdf"])

        self.assertEqual(status["files"], ["/a/PO.pdf", "/a/terms.pdf"])
        self.assertEqual(kept, {"demo-1": {"files": ["/a/PO.pdf", "/a/terms.pdf"]}})

    def test_attaching_with_no_ticket_open_is_refused(self):
        with mock.patch.object(record_task, "_ticket", None):
            with self.assertRaises(record_task.ComputerError):
                record_task.attach(["/a/PO.pdf"])


class MatchingTest(unittest.TestCase):
    def test_a_recorded_task_is_matched_on_the_requests_words_only(self):
        seen = []

        def plan(text):
            seen.append(text)
            return None

        with mock.patch.object(skill_task.mimic, "plan", plan):
            skill_task.mimics(request("enter the purchase order", "/home/headless/work/inbox/up-1/PO.pdf"))

        self.assertEqual(seen, ["enter the purchase order"])


class LessonTest(unittest.TestCase):
    def test_a_lesson_keeps_the_documents_its_demonstrations_were_given(self):
        programs = [({"files": ["/a/PO.pdf", "/a/terms.pdf"]}, [], {}), ({"files": ["/a/PO.pdf", "/b/PO-2.pdf"]}, [], {}),
                    ({}, [], {})]

        self.assertEqual(learning._documents(programs), ["/a/PO.pdf", "/a/terms.pdf", "/b/PO-2.pdf"])

    def test_the_model_studying_a_lesson_is_shown_its_documents(self):
        with tempfile.TemporaryDirectory() as tmp:
            po = Path(tmp) / "PO.txt"
            po.write_text("Purchase order GF-5522\nCustomer: Gemini Furniture\nQty 4  Office Chair", encoding="utf-8")
            gone = str(Path(tmp) / "missing.pdf")

            parts = study._documents({"documents": [str(po), gone]})

        text = "\n".join(part["text"] for part in parts if part["type"] == "text")
        self.assertIn("worked from in this demonstration", parts[0]["text"])
        self.assertIn("Customer: Gemini Furniture", text)
        self.assertIn("no longer on this desktop", text)

    def test_a_lesson_with_no_documents_shows_none(self):
        self.assertEqual(study._documents({}), [])
        self.assertNotIn("Documents attached", study._described({"steps": [], "title": "x"}))
        self.assertIn("Documents attached to the ticket (shown below): PO.pdf",
                      study._described({"steps": [], "title": "x", "documents": ["/a/PO.pdf"]}))


if __name__ == "__main__":
    unittest.main()
