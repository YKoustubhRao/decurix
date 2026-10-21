"""
A path is typed as written. Found live (2026-10-19, word_letters): `gui type e960 "C:\\Users\\Administrator\\Documents\\
Decurix demo\\receivables.xlsx"` was refused every time - the path was cut at its first space, and the rest,
"demo\\receivables.xlsx", began with \\r, which the escape check took for a carriage return. Any folder starting with
r, f, v, b or a (\\reports, \\forms, \\vendors, \\backup, \\archive) was untypeable, and \\n and \\t inside a path
(C:\\new\\temp\\notes.txt) were read as Enter and Tab. Precision over recall at typing time; the final check catches
the run-on prose case in the saved file (produced.document_findings).
"""

import io
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

from decurix_agent import produced
from decurix_agent.computer import cli
from decurix_agent.computer.errors import ComputerError

PATHS = [
    "C:\\Users\\Administrator\\Documents\\Decurix demo\\receivables.xlsx",
    "C:\\Users\\Administrator\\Documents\\Decurix demo\\orders_oct.xlsx",
    "C:\\Users\\Administrator\\Documents\\Decurix demo\\forms\\vendors\\backup\\archive\\reports.csv",
    "D:\\receivables\\fy26\\audit.xlsx",
    "\\\\fileserver\\share\\reports\\receivables.xlsx",
    "%USERPROFILE%\\Documents\\forms\\a.txt",
    "C:\\new\\temp\\notes.txt",
    "C:\\Users\\me\\Documents\\Decurix demo",  # a folder, no file
    "C:\\work\\notes\\t1",
]


class APathIsTypedAsWritten(unittest.TestCase):
    def test_no_path_is_refused_or_changed(self):
        for path in PATHS:
            with self.subTest(path=path):
                self.assertEqual(cli._unsupported_escape(path), "")
                self.assertEqual(cli._unescape(path), path)

    def test_a_path_inside_a_sentence_or_quotes_is_still_a_path(self):
        for text in ("Open C:\\Users\\Administrator\\Documents\\Decurix demo\\receivables.xlsx now",
                     "file name: C:\\Users\\me\\Documents\\Decurix demo\\forms\\receivables.xlsx.",
                     "'C:\\Reports\\Q3 numbers\\vendors.xlsx'"):
            with self.subTest(text=text):
                self.assertEqual(cli._unsupported_escape(text), "")
                self.assertEqual(cli._unescape(text), text)

    def test_a_relative_path_with_a_word_after_the_slash_is_not_a_control_escape(self):
        for text in ("reports\\receivables.xlsx", "forms\\vendors\\backup.txt", "docs\\archive", "a\\bc"):
            self.assertEqual(cli._unsupported_escape(text), "", text)

    def test_the_keys_still_mean_enter_and_tab_outside_a_path(self):
        self.assertEqual(cli._unescape("Item\\tQty\\nTea\\t4"), "Item\tQty\nTea\t4")
        self.assertEqual(cli._unescape("Region\\tRevenue\\nNorth\\t812"), "Region\tRevenue\nNorth\t812")

    def test_a_path_then_a_real_enter_or_tab_keeps_both_meanings(self):
        # A file (a known extension) ends the path: what follows an escape after it is a key again.
        self.assertEqual(cli._unescape("C:\\data\\report.csv\\nTotal"), "C:\\data\\report.csv\nTotal")
        self.assertEqual(cli._unescape("C:\\data\\report.xlsx\\tC:\\data\\other.xlsx"),
                         "C:\\data\\report.xlsx\tC:\\data\\other.xlsx")
        # ...and a path on each line of a block still splits there.
        self.assertEqual(cli._unescape("C:\\a\\b.txt\\nC:\\new\\temp\\c.txt"), "C:\\a\\b.txt\nC:\\new\\temp\\c.txt")

    def test_a_folder_that_only_looks_like_a_file_does_not_end_the_path(self):
        self.assertEqual(cli._unescape("C:\\data\\v1.2\\notes\\todo.txt"), "C:\\data\\v1.2\\notes\\todo.txt")
        self.assertEqual(cli._unsupported_escape("C:\\data\\v1.2\\reports\\q3.txt"), "")

    def test_path_spans_say_where_the_path_ends(self):
        text = "see C:\\Users\\me\\Documents\\Decurix demo\\receivables.xlsx please"
        (begin, end), = cli.path_spans(text)
        self.assertTrue(text[begin:end].startswith("C:\\Users") and "receivables.xlsx" in text[begin:end])
        self.assertEqual(cli.path_spans("Note:\\nTotal and ratio 3:\\n"), [])
        self.assertEqual(cli.path_spans("no path here"), [])


class OnlyALoneEscapeIsRefused(unittest.TestCase):
    def test_a_lone_token_is_refused_with_how_to_press_the_key(self):
        for text in ("one letter \\f next letter", "end of page \\f", "\\f", "a \\r b", "x \\v"):
            with self.subTest(text=text):
                self.assertNotEqual(cli._unsupported_escape(text), "")
        with mock.patch.object(cli.core, "act") as act, mock.patch("sys.stdout", new_callable=io.StringIO):
            with self.assertRaises(ComputerError) as caught:
                cli.run(["type", "e12", "Dear Sir, \\f Dear Madam"])
        act.assert_not_called()
        self.assertIn("gui key ctrl+Return", str(caught.exception))

    def test_a_path_goes_through_the_command_with_its_backslashes(self):
        for path in PATHS[:3]:
            with self.subTest(path=path), mock.patch.object(cli.shown, "unseen", lambda ref: False), \
                    mock.patch.object(cli.core, "act", return_value={"status": "done", "text": "ok"}) as act, \
                    mock.patch("sys.stdout", new_callable=io.StringIO):
                self.assertEqual(cli.run(["type", "e960", path]), cli.EXIT_OK)
                self.assertEqual(act.call_args.kwargs["text"], path)

    def test_a_path_on_stdin_is_typed_verbatim(self):
        path = PATHS[0]
        with mock.patch.object(cli.shown, "unseen", lambda ref: False), \
                mock.patch.object(cli.core, "act", return_value={"status": "done", "text": "ok"}) as act, \
                mock.patch("sys.stdin", io.StringIO(path + "\n")), mock.patch("sys.stdout", new_callable=io.StringIO):
            self.assertEqual(cli.run(["type", "e960", "-"]), cli.EXIT_OK)
        self.assertEqual(act.call_args.kwargs["text"], path)

    def test_a_do_parameter_that_is_a_path_keeps_its_backslashes(self):
        self.assertEqual(cli._unescape(PATHS[0]), PATHS[0])
        self.assertEqual(cli._unescape("C:\\new\\folder\\a.txt"), "C:\\new\\folder\\a.txt")


class TheGateStillCatchesRunOnProse(unittest.TestCase):
    def test_a_typed_backslash_f_between_letters_is_not_done_in_the_saved_file(self):
        from test_unseen_success import docx, para

        with tempfile.TemporaryDirectory() as tmp:
            path = docx(Path(tmp) / "r.docx", para("Please pay.\\fKaveri Electricals"))
            found = produced.document_findings("Each letter on its own page.", [path])
        self.assertEqual(len(found), 1)  # (the page breaks asked for are page_break_findings: evidence)
        self.assertIn('holds the characters "\\f" typed as text', found[0])


if __name__ == "__main__":
    unittest.main()
