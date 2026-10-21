import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import azure_harness, config, state


def _call(cid, name, **args):
    return {"id": cid, "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}


def _msg(*calls, content=""):
    m = {"content": content}
    if calls:
        m["tool_calls"] = list(calls)
    return m


class TestAzureHarnessLoop(unittest.TestCase):
    """The loop against a scripted model: every request it sends is kept."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.work = Path(self.tmp.name)
        self.replies = []
        self.sent = []
        self.finals = []
        self.queries = []

        def fake_chat(messages, tools=None, **kw):
            self.sent.append(json.loads(json.dumps(messages)))
            return self.replies.pop(0), {"prompt_tokens": 10, "completion_tokens": 5}

        self.patches = [
            patch.object(azure_harness.llm_client, "chat", side_effect=fake_chat),
            patch.object(azure_harness, "write_final_reply", side_effect=lambda t, **k: self.finals.append((t, k.get("kind", "final")))),
            patch.object(azure_harness, "ask_for_query", side_effect=lambda t, **k: self.queries.append((t, k.get("kind")))),
            patch.object(azure_harness, "get_memory_context", return_value=""),
            patch.object(azure_harness.progress, "emit"),
            patch.object(azure_harness.usage, "add"),
            patch.object(azure_harness.usage, "set_model"),
            patch.object(config, "WORK_DIR", self.work),
        ]
        for p in self.patches:
            p.start()

    def tearDown(self):
        for p in self.patches:
            p.stop()
        self.tmp.cleanup()

    def test_every_tool_result_comes_before_the_pictures_of_that_turn(self):
        # Two reads in one turn, one of them a picture: the API refuses a
        # message between two tool results, so the picture goes after both.
        img = self.work / "scan.png"
        img.write_bytes(b"\x89PNG\r\n\x1a\nfake")
        (self.work / "a.txt").write_text("hello")

        self.replies = [
            _msg(_call("c1", "read_document", path=str(img)), _call("c2", "read_document", path=str(self.work / "a.txt"))),
            _msg(_call("c3", "finish", report="done", checklist=[{"item": "read both files", "done": True}])),
        ]

        azure_harness.perform_task("read them", None)

        second = self.sent[1]
        roles = [m["role"] for m in second]
        self.assertEqual(roles[-3:], ["tool", "tool", "user"])
        self.assertEqual([m["tool_call_id"] for m in second[-3:-1]], ["c1", "c2"])
        self.assertTrue(any(p.get("type") == "image_url" for p in second[-1]["content"]))
        self.assertEqual(self.finals[0][1], "final")
        self.assertTrue(self.finals[0][0].startswith("done"))

    def test_a_question_pauses_and_the_reply_answers_that_call(self):
        self.replies = [
            _msg(_call("q1", "ask_human", question="Which customer?"), _call("q2", "shell", command="echo never")),
        ]
        state.current_task = "x"
        azure_harness.perform_task("x", None)

        self.assertEqual(self.queries, [("Which customer?", "query")])
        self.assertEqual(self.finals, [])

        self.replies = [_msg(content="Created for Acme.")]
        azure_harness.continue_after_query("Acme")

        resumed = self.sent[-1]
        answers = {m["tool_call_id"]: m["content"] for m in resumed if m["role"] == "tool"}
        self.assertIn("Acme", answers["q1"])
        self.assertIn("Not run", answers["q2"])  # the call after the pause never ran
        self.assertEqual(self.finals[-1], ("Created for Acme.", "final"))

    def test_a_dangerous_command_waits_for_a_clean_yes(self):
        self.replies = [_msg(_call("d1", "shell", command="rm -rf /etc"))]
        state.current_task = "x"
        azure_harness.perform_task("clean up", None)
        self.assertEqual(self.queries[0][1], "approval")

        with patch.object(azure_harness, "_shell", return_value="exit code 0") as shell:
            self.replies = [_msg(content="stopped")]
            azure_harness.continue_after_query("yes but not /etc")
            shell.assert_not_called()

        self.assertIn("did not approve", [m for m in self.sent[-1] if m.get("tool_call_id") == "d1"][0]["content"])

    def test_a_failed_finish_is_reported_as_an_error(self):
        self.replies = [_msg(_call("f", "finish", report="App was down", status="failed", checklist=[{"item": "enter the order", "done": False, "note": "App was down"}]))]
        azure_harness.perform_task("x", None)
        self.assertEqual(self.finals[0][1], "error")
        self.assertIn("NOT DONE: enter the order", self.finals[0][0])

    def test_a_finish_that_leaves_parts_undone_is_sent_back_once(self):
        undone = [{"item": "fill date of birth", "done": False, "note": "not visible"}, {"item": "add employee", "done": True}]
        self.replies = [
            _msg(_call("f1", "finish", report="all set", checklist=undone)),
            _msg(_call("f2", "finish", report="all set", checklist=[dict(undone[0], done=True, note="scrolled, filled"), undone[1]])),
        ]
        azure_harness.perform_task("onboard her", None)

        sent_back = [m for m in self.sent[1] if m.get("tool_call_id") == "f1"][0]["content"]
        self.assertIn("fill date of birth", sent_back)
        self.assertIn("scroll", sent_back)
        self.assertEqual(len(self.finals), 1)
        self.assertIn("done: fill date of birth - scrolled, filled", self.finals[0][0])

    def test_a_finish_without_a_checklist_is_sent_back_then_taken(self):
        self.replies = [_msg(_call("f1", "finish", report="ok")), _msg(_call("f2", "finish", report="ok")),
                        _msg(_call("f3", "finish", report="ok"))]
        azure_harness.perform_task("x", None)
        self.assertEqual(len(self.finals), 1)  # sent back at most twice, never forever

    def test_a_draft_email_needs_its_attachment_to_exist(self):
        out = azure_harness._draft_email({"to": ["a@b.c"], "subject": "s", "body": "b", "attachments": ["missing.pdf"]})
        self.assertIn("does not exist", out)

    def test_document_pages_outlive_screenshots(self):
        doc = {"role": "user", "content": [{"type": "text", "text": "Pictures from read_document (/w/inv.png):"},
                                            {"type": "image_url", "image_url": {"url": "doc"}}]}
        screens = [{"role": "user", "content": [{"type": "text", "text": azure_harness.SCREEN_LABEL},
                                                 {"type": "image_url", "image_url": {"url": f"s{i}"}}]} for i in range(6)]
        msgs = [doc] + screens
        azure_harness._trim_images(msgs)

        self.assertEqual(doc["content"][1]["type"], "image_url", "the scanned invoice was dropped for screenshots")
        kept = [m["content"][1]["type"] for m in screens]
        self.assertEqual(kept.count("image_url"), azure_harness.KEEP_SCREENS)
        self.assertEqual(kept[-1], "image_url")
        self.assertIn("observe", screens[0]["content"][1]["text"])


class TestOfficeTools(unittest.TestCase):
    def setUp(self):
        try:
            import openpyxl  # noqa: F401
        except ImportError:
            self.skipTest("openpyxl is only in the desktop images")
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_update_keeps_charts_and_extends_their_ranges(self):
        import openpyxl
        import zipfile
        from openpyxl.chart import BarChart, Reference
        from decurix_agent.tools import office

        wb = openpyxl.Workbook()
        ws = wb.active
        ws.title = "Data"
        ws.append(["Month", "Sales"])
        for m, v in [("Jan", 10), ("Feb", 20)]:
            ws.append([m, v])
        chart = BarChart()
        chart.add_data(Reference(ws, min_col=2, min_row=1, max_row=3), titles_from_data=True)
        ws.add_chart(chart, "E2")
        path = self.dir / "m.xlsx"
        wb.save(path)

        office.update_workbook(path, edits=[{"sheet": "Data", "cell": "A4", "value": "Mar"},
                                            {"sheet": "Data", "cell": "B4", "value": 30}],
                               replace_refs=[{"from": "'Data'!$B$2:$B$3", "to": "'Data'!$B$2:$B$4"}])

        with zipfile.ZipFile(path) as z:
            chart_xml = z.read("xl/charts/chart1.xml").decode()
        self.assertIn("$B$2:$B$4", chart_xml)
        rows = [[c.value for c in r] for r in openpyxl.load_workbook(path)["Data"].iter_rows()]
        self.assertEqual(rows[-1][:2], ["Mar", 30])
        self.assertTrue((self.dir / "m.orig.xlsx").exists())

    def test_create_refuses_to_overwrite(self):
        from decurix_agent.tools import office

        p = self.dir / "x.xlsx"
        office.create_workbook(p, [{"name": "S", "rows": [["a"], [1]]}])
        with self.assertRaises(office.OfficeError):
            office.create_workbook(p, [{"name": "S", "rows": [["a"]]}])


if __name__ == "__main__":
    unittest.main()


class CombiningOfficeDocuments(unittest.TestCase):
    def setUp(self):
        import shutil as _sh
        try:
            import docx  # noqa: F401
            import pypdf  # noqa: F401
        except ImportError:
            self.skipTest("python-docx and pypdf are only in the desktop images")
        if not (_sh.which("soffice") or _sh.which("libreoffice")):
            self.skipTest("LibreOffice is only in the desktop images")
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_a_word_form_is_laid_out_as_a_page_of_the_combined_pdf(self):
        import docx
        from pypdf import PdfReader
        from decurix_agent.tools import office

        d = docx.Document()
        d.add_heading("Customer Account Opening Form", 1)
        table = d.add_table(rows=2, cols=2)
        table.cell(0, 0).text, table.cell(0, 1).text = "Company", "Rao Precision Tools LLP"
        table.cell(1, 0).text, table.cell(1, 1).text = "PAN", "AAKFR7412Q"
        d.save(self.dir / "form.docx")
        from PIL import Image
        Image.new("RGB", (400, 250), (220, 230, 240)).save(self.dir / "pan.png")

        out = self.dir / "file.pdf"
        office.combine_pdf(out, [self.dir / "form.docx", self.dir / "pan.png"])

        reader = PdfReader(str(out))
        self.assertEqual(len(reader.pages), 2)
        text = reader.pages[0].extract_text()
        self.assertIn("Customer Account Opening Form", text)
        self.assertIn("AAKFR7412Q", text)  # the table's contents, not just its paragraphs
