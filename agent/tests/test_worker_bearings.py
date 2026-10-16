import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import identity, skill_task
from decurix_agent.computer import core, launchers


class AReplyThatExplainsThenConfirms(unittest.TestCase):
    def test_it_is_a_confirmation_when_its_last_sentence_says_so(self):
        for reply in ("done", "That is correct", "Looks good", "The entry looks fine.", "Use Partnership. It is done.",
                      "An LLP is a partnership firm, not a company - keep Supplier Type as Partnership for an LLP "
                      "even when a form says Company. The record is done."):
            self.assertTrue(skill_task._confirms(reply), reply)

    def test_a_correction_a_question_or_a_bare_yes_is_not(self):
        for reply in ("The supplier type is wrong. Fix it.", "done but the date is wrong", "It is not done.",
                      "Keep it. The rate is not right.", "the total is fine but the date is not. it is done?",
                      "Set the type to Company. Then it is done.", "ok", "yes", "good"):
            self.assertFalse(skill_task._confirms(reply), reply)


class WhereThingsAreFromInsideTheDesktop(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        desktop, bin_ = root / "Desktop", root / "bin"
        desktop.mkdir()
        bin_.mkdir()
        wrapper = bin_ / "erp-host"
        wrapper.write_text("#!/usr/bin/env bash\nAPP='http://host.docker.internal:8000/login'\nexec chromium-browser --app=\"$APP\"\n")
        (desktop / "erp-host.desktop").write_text(
            f"[Desktop Entry]\nType=Application\nName=ERP (Host)\nExec={wrapper.as_posix()} %U\n")
        (desktop / "notes.desktop").write_text("[Desktop Entry]\nType=Application\nName=Notes\nExec=mousepad\n")
        patcher = mock.patch.object(launchers, "PLACES", [desktop])
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_the_network_and_the_apps_with_their_addresses_are_facts_it_is_given(self):
        facts = identity.get_identity_context("TheBot (soul)")
        self.assertIn("localhost and 127.0.0.1 are THIS desktop", facts)
        self.assertIn("host.docker.internal", facts)
        self.assertIn('"ERP (Host)" (a web app at http://host.docker.internal:8000/login)', facts)
        self.assertIn('"Notes"', facts)


class ALauncherThatBringsItsAppForward(unittest.TestCase):
    def test_raising_an_open_window_is_opening_it(self):
        entry = {"name": "ERP (Host)", "exec": "/bin/erp-host %U", "path": "x", "wm_class": ""}
        browser = {"id": 1, "title": "Search - Chromium", "active": True, "box": (0, 0, 10, 10)}
        erp = {"id": 2, "title": "Desktop", "active": False, "box": (0, 0, 10, 10)}
        looks = iter([[browser, erp]] + [[dict(browser, active=False), dict(erp, active=True)]] * 50)
        with mock.patch.object(core.screen, "windows", lambda: next(looks)), \
                mock.patch.object(core.launchers, "by_name", lambda name, places=None: entry if "erp" in name.lower() else None), \
                mock.patch.object(core.session, "launch", lambda argv: None), \
                mock.patch.object(core.session, "ensure", lambda: None), \
                mock.patch.object(core.screen, "grab", lambda *a, **k: b""), \
                mock.patch.object(core, "LOAD_SECONDS", 0), mock.patch.object(core.time, "sleep", lambda s: None):
            said = core._open("ERP (Host)", False)
        self.assertNotIn("no new window", said)


class SearchingAListForWhatADocumentDescribes(unittest.TestCase):
    def test_a_plural_is_searched_in_the_singular_with_its_own_punctuation_and_in_its_phrase(self):
        # An order's lines, against the application's own items "T-shirt" and "Coffee Mug" (found live, 2026-10-13).
        self.assertIn("T-shirt", core._searches("Cotton T-shirts, round neck, assorted sizes"))
        self.assertNotIn("Tshirt", core._searches("Cotton T-shirts, round neck, assorted sizes"))
        self.assertIn("coffee mug", core._searches("Ceramic coffee mugs 300 ml"))
        self.assertIn("Internet Leased Line", core._searches("Dedicated Internet Leased Line (ILL) 1 Gbps"))


class AnEntryTheDocumentsNameIsNotAStandIn(unittest.TestCase):
    """After a search that found nothing, the payer the remittance itself names may still be chosen (found live)."""

    def test_the_guard_lets_through_what_the_task_documents_name(self):
        import json
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        doc = Path(tmp.name) / "REMIT.txt"
        doc.write_text("REMITTANCE ADVICE  West View Software Ltd.\nTo: Example Inc - Accounts Receivable\n229,000.00")
        kept = {"at": 10 ** 12, "task": "", "field": "Party", "role": "combobox", "wanted": "Example Inc - Accounts Receivable"}
        element = {"name": "Party", "role": "combobox"}
        core._DOCUMENT_WORDS.clear()
        failed = Path(tmp.name) / "failed.json"
        failed.write_text(json.dumps(kept), encoding="utf-8")
        with mock.patch.object(core, "FAILED_SEARCH_FILE", failed), \
                mock.patch.object(core.time, "time", lambda: 10 ** 12), mock.patch.object(core, "_task_now", lambda: ""), \
                mock.patch("decurix_agent.guidance.persons_words", lambda: ""), \
                mock.patch("decurix_agent.task_context.current_files", lambda: [str(doc)]):
            self.assertIsNone(core._in_place_of(element, "West View Software Ltd."))
            # Another customer the document does not name is still refused.
            self.assertEqual(core._in_place_of(element, "Grant Plastics Ltd."), "Example Inc - Accounts Receivable")
        core._DOCUMENT_WORDS.clear()


class AWordDocumentIsMadeByTheTool(unittest.TestCase):
    """office create DOC.docx: a valid document from headings, paragraphs, bullets and tables (found live: hand-written XML)."""

    def test_it_reads_back_with_its_table(self):
        from decurix_agent.tools import office
        with tempfile.TemporaryDirectory() as tmp:
            path = str(Path(tmp) / "changes.docx")
            said = office.create_document(path, [{"heading": "Contract changes", "level": 1},
                                                 {"table": [["Clause", "Version 1", "Version 2"], ["2", "45 days", "30 days"]]},
                                                 {"bullets": ["one", "two"]}])
            self.assertIn("1 table(s)", said)
            text = office.read_document(path)["text"]
            self.assertIn("2 | 45 days | 30 days", text)
            with self.assertRaises(office.OfficeError):
                office.create_document(path, [{"paragraph": "again"}])          # never overwrites
            with self.assertRaises(office.OfficeError):
                office.create_document(str(Path(tmp) / "x.docx"), [{"tabel": []}])  # a block it does not read


class TheResultIsWhatTheRequestNamed(unittest.TestCase):
    """A working file (a JSON spec fed to a tool) is not judged as the result when the request names its output."""

    def test_only_the_named_output_and_what_the_report_names(self):
        import time
        from decurix_agent import produced
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("contract_changes.docx", "spec.json", "notes.txt"):
                (root / name).write_text("x", encoding="utf-8")
            request = "Compare the two versions and make contract_changes.docx in my work folder"
            got = [Path(p).name for p in produced.files("Created " + str(root / "notes.txt"), started=time.time() - 60,
                                                        root=root, request=request)]
            self.assertEqual(sorted(got), ["contract_changes.docx", "notes.txt"])
            # A request that names no file: everything written is looked at, as before.
            self.assertEqual(len(produced.files("", started=time.time() - 60, root=root, request="tidy up")), 3)


class AFileSavedOverTheAttachmentIsNotInTheWorkFolder(unittest.TestCase):
    def test_it_is_found_with_no_model(self):
        from decurix_agent import produced
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "inbox" / "up-1").mkdir(parents=True)
            (root / "inbox" / "up-1" / "grades.xlsx").write_text("x")
            request = "Open grades.xlsx in LibreOffice Calc, sort it and save it as grades.xlsx in my work folder"
            self.assertIn("not in the work folder", produced.misplaced(request, root=root))
            (root / "grades.xlsx").write_text("x")
            self.assertEqual(produced.misplaced(request, root=root), "")
            # A request that does not say where says nothing about this.
            (root / "grades.xlsx").unlink()
            self.assertEqual(produced.misplaced("sort grades.xlsx", root=root), "")

    def test_an_input_the_request_only_names_is_not_a_misplaced_result(self):
        import os
        import time
        from unittest import mock
        from decurix_agent import produced
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "inbox" / "up-1").mkdir(parents=True)
            paid = root / "inbox" / "up-1" / "already_paid.csv"
            paid.write_text("vendor,invoice_no\n")
            old = time.time() - 600
            os.utime(paid, (old, old))
            request = ("Build ap_register.xlsx in my work folder; DUPLICATE if the invoice is already in already_paid.csv")
            (root / "ap_register.xlsx").write_text("x")
            with mock.patch.object(produced, "since", return_value=time.time() - 60):
                self.assertEqual(produced.misplaced(request, root=root), "")
                paid.write_text("saved over during the task")
                self.assertIn("already_paid.csv", produced.misplaced(request, root=root))


class AChartIsMadeByTheTool(unittest.TestCase):
    def test_a_workbook_with_a_chart_reads_back_with_it(self):
        from decurix_agent.tools import office
        with tempfile.TemporaryDirectory() as tmp:
            path = str(Path(tmp) / "r.xlsx")
            said = office.create_workbook(path, [{"name": "Monthly", "rows": [["Month", "Revenue"], ["Jul", 100], ["Aug", 120]],
                                                  "charts": [{"type": "column", "title": "Revenue", "data": "B1:B3", "categories": "A2:A3"}]}])
            self.assertIn("1 chart(s)", said)
            self.assertIn("chart", office.read_workbook(path))
            with self.assertRaises(office.OfficeError):  # a range past the data
                office.create_workbook(str(Path(tmp) / "x.xlsx"), [{"name": "S", "rows": [["a", 1]], "charts": [{"data": "B1:B9"}]}])


if __name__ == "__main__":
    unittest.main()
