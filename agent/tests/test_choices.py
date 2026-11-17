"""
A pick this run has no value for is chosen at its step (computer/choices.py): remembered for the party, the only entry,
by analogy with the demonstration, or asked once - never a hole for the model. Found in the learning curves
(2026-11-17): two such picks a case (the exported file, the signatory) were a third of a filing's cost, every month.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import choices  # noqa: E402

DEMO_GOAL = "Demonstration: File the October 2026 GSTR-1 of Sahyadri Lift Services (GSTIN 29AALFS8842N1Z6) from its books"
DEMO_FILE = "29AALFS8842N1Z6_GSTR1_OCT-2026_05112026_101500.json"
FILES = ["29AAGFK7314L1ZY_GSTR1_OCT-2026_04112026_101010.json",
         "29AAGFK7314L1ZY_GSTR1_NOV-2026_04122026_172107.json",
         "29AALFS8842N1Z6_GSTR1_NOV-2026_03122026_090000.json"]
REQUEST = "File the November 2026 GSTR-1 of Konkan Coast Interiors (GSTIN 29AAGFK7314L1ZY) from its books."


class Store(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        for p in (mock.patch.object(choices, "PATH", Path(tmp.name) / "c.json"),
                  mock.patch.object(choices, "PENDING", Path(tmp.name) / "p.json"),
                  mock.patch.object(choices, "ask", return_value=(None, ""))):
            p.start()
            self.addCleanup(p.stop)


class Choose(Store):
    def test_by_analogy_with_the_demonstration(self):
        # The person chose their GSTIN's file for their month: this run's file holds this GSTIN and this month.
        got, how = choices.choose("s", "exported_json_file", "radio", DEMO_FILE, DEMO_GOAL, FILES, REQUEST)
        self.assertEqual(got, FILES[1])
        self.assertIn("this request.s words", how.replace("'", "."))

    def test_the_only_entry(self):
        got, _ = choices.choose("s", "signatory", "Authorised signatory", "Nandini Kamath", DEMO_GOAL,
                                ["Select", "Prakash Hegde"], REQUEST)
        self.assertEqual(got, "Prakash Hegde")

    def test_nothing_settles_it_and_nothing_is_guessed(self):
        got, how = choices.choose("s", "signatory", "Authorised signatory", "Nandini Kamath", DEMO_GOAL,
                                  ["Select", "Prakash Hegde", "Ravi Kamath"], REQUEST)
        self.assertIsNone(got)
        self.assertIn("nothing settles", how)

    def test_two_entries_holding_the_words_choose_neither(self):
        twice = FILES + ["29AAGFK7314L1ZY_GSTR1_NOV-2026_05122026_080000.json"]
        got, _ = choices.choose("s", "exported_json_file", "radio", DEMO_FILE, DEMO_GOAL, twice, REQUEST)
        self.assertIsNone(got)

    def test_an_answer_not_one_of_the_entries_chooses_nothing(self):
        with mock.patch.object(choices, "ask", wraps=None) as asked:
            asked.return_value = (None, "")
            got, _ = choices.choose("s", "q", "Quarter", "x", "", ["A b", "C d"], REQUEST)
        self.assertIsNone(got)


class Remembered(Store):
    def test_a_choice_is_remembered_for_its_party_only_from_a_task_checked_done(self):
        entries = ["Select", "Prakash Hegde", "Ravi Kamath"]
        choices.note("t1", "s", "signatory", "Prakash Hegde", REQUEST)
        choices.settle("t1", False)
        self.assertIsNone(choices.choose("s", "signatory", "Signatory", "", "", entries, REQUEST)[0])
        choices.note("t2", "s", "signatory", "Prakash Hegde", REQUEST)
        choices.settle("t2", True)
        got, how = choices.choose("s", "signatory", "Signatory", "", "", entries, REQUEST)
        self.assertEqual(got, "Prakash Hegde")
        self.assertIn("this party", how)
        # Another party's request is not this one's.
        other = REQUEST.replace("29AAGFK7314L1ZY", "29AALFS8842N1Z6").replace("Konkan Coast Interiors", "Sahyadri")
        self.assertIsNone(choices.choose("s", "signatory", "Signatory", "", "", entries, other)[0])


class TheReplayChoosesAtTheStep(unittest.TestCase):
    def test_a_radio_named_for_the_demonstrations_file_is_chosen_among_its_kind(self):
        from decurix_agent.computer import replay
        elements = [{"role": "radio", "name": n, "ref": f"e{i}"} for i, n in enumerate(FILES)]
        elements += [{"role": "radio", "name": "Overwrite", "ref": "e9"}, {"role": "radio", "name": "Append", "ref": "e8"}]
        step = {"n": 29, "kind": "select", "param": "exported_json_file",
                "target": {"role": "radio", "name": DEMO_FILE, "path": "x"}}
        values = {"exported_json_file": "", "__skill__": "file-it"}
        with mock.patch.object(replay.core, "observe", return_value={"elements": elements}), \
                mock.patch("decurix_agent.task_context.current_request", return_value=REQUEST), \
                mock.patch("decurix_agent.task_context.current", return_value="t9"), \
                mock.patch("decurix_agent.computer.skills.load", return_value={"goal": DEMO_GOAL, "params": []}), \
                mock.patch.object(choices, "note") as noted, mock.patch.object(choices, "ask", return_value=(None, "")):
            out = replay._choose_at_step(step, values, report=lambda *_: None)
        self.assertEqual(values["exported_json_file"], FILES[1])
        self.assertEqual(out["target"]["name"], FILES[1])
        self.assertNotIn("path", out["target"])
        noted.assert_called_once()


class AListThatFollowsAnEarlierChoice(unittest.TestCase):
    """Found in the learning curves (2026-11-17): the period list follows the quarter only once the form is sent; a
    month in another quarter stopped at its period on every case. The person's next press (Search) is made first."""

    def test_the_next_press_on_the_page_refreshes_the_list(self):
        from decurix_agent.computer import replay
        step = {"n": 50, "kind": "select", "url": "http://portal.test/returns", "target": {"role": "combobox", "name": "Period"}}
        search = {"n": 51, "kind": "click", "url": "http://portal.test/returns",
                  "target": {"role": "button", "name": "Search"}}
        self.assertTrue(replay._refreshes_lists(step, search))
        self.assertFalse(replay._refreshes_lists(step, dict(search, target={"role": "button", "name": "File with EVC"},
                                                            consequential=True)))
        self.assertFalse(replay._refreshes_lists(step, dict(search, url="http://portal.test/other")))
        self.assertFalse(replay._refreshes_lists(step, dict(search, kind="type")))

    def test_the_choice_is_made_again_after_the_press(self):
        from decurix_agent.computer import core, replay
        step = {"n": 50, "kind": "select", "param": "month", "option": "Select", "url": "http://p.test/r",
                "target": {"role": "combobox", "name": "Period", "path": "x"}, "expect": []}
        search = {"n": 51, "kind": "click", "url": "http://p.test/r", "target": {"role": "button", "name": "Search"}}
        calls = []

        def act(kind, **kw):
            calls.append((kind, kw.get("option") or kw.get("path")))
            if kind == "select" and len([c for c in calls if c[0] == "select"]) == 1:
                raise replay.ComputerError("combobox \"Period\" has no option 'March'. Options: Select, January")
            return {"status": "done", "observation": None}

        element = {"role": "combobox", "name": "Period", "path": "x", "ref": "e1"}
        with mock.patch.object(core, "act", side_effect=act),                 mock.patch.object(replay, "_find", side_effect=lambda s, *a, **k: (dict(element, name=s["target"]["name"],
                                                                                   path=s["target"]["name"]),
                                                                              (1, 1), {}, None)):
            replay._run_step(step, {"month": "March"}, next_step=search, report=lambda *_: None)
        self.assertEqual([c[0] for c in calls], ["select", "click", "select"])
        self.assertEqual(calls[1][1], "Search")


if __name__ == "__main__":
    unittest.main()
