"""
Tests for run_search (`gui recall --runs`): the query it sends (this desktop, this task left out), the client scope of
memory applied to what comes back, and the printed blocks.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import config, run_search


def _fact(folder, name, party):
    (Path(folder) / name).write_text(f"Title: About {party}\nCategory: fact\n\n{party} pays by bank transfer.\n",
                                     encoding="utf-8")


class RunSearchTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        patches = [mock.patch.object(config, "MEMORY_DIR", Path(self.tmp.name)),
                   mock.patch.object(config, "DESKTOP_ID", "box"),
                   mock.patch.object(run_search, "_current", return_value=("t_now", "file the return for Kiran Stores"))]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        _fact(self.tmp.name, "001.txt", "Kiran Stores")
        _fact(self.tmp.name, "002.txt", "Asha Mills")

    def test_search_sends_its_own_scope_and_hides_another_partys_run(self):
        hits = {"hits": [
            {"task_id": "t_1", "snippet": "filed the [[return]] for Kiran Stores", "seq": 3, "kind": "step",
             "status": "done", "outcome": "done_checked", "messages": [
                 {"seq": 0, "kind": "request", "text": "file the return for Kiran Stores", "cut": 0}]},
            {"task_id": "t_2", "snippet": "filed the [[return]] for Asha Mills", "seq": 2, "kind": "step"},
            {"task_id": "t_3", "snippet": "the [[return]] form moved", "seq": 1, "kind": "step", "automated": True,
             "origin": "schedule"},
        ]}
        with mock.patch.object(run_search, "_call", return_value=hits) as call:
            found, hidden, _ = run_search.search("return", since="7d")
        query = call.call_args[0][0]
        self.assertEqual(query["desktop_id"], "box")
        self.assertEqual(query["exclude"], "t_now")
        self.assertEqual(query["after"], "7d")
        self.assertEqual([h["task_id"] for h in found], ["t_1", "t_3"])
        self.assertEqual(hidden, 1)
        text = run_search.render_search(found, hidden)
        self.assertTrue(text.startswith("=== PAST RUNS ==="))
        self.assertTrue(text.endswith("=== END PAST RUNS ==="))
        self.assertIn("#0 request", text)
        self.assertIn("automated (schedule)", text)
        self.assertIn("not proof", text)
        self.assertNotIn("Asha", text)

    def test_reading_another_partys_run_shows_nothing_of_it(self):
        run = {"task_id": "t_2", "total": 2, "messages": [
            {"seq": 0, "kind": "request", "text": "file the return for Asha Mills"},
            {"seq": 1, "kind": "outcome", "text": "filed", "cut": 0}]}
        with mock.patch.object(run_search, "_call", return_value={"run": run}) as call:
            self.assertIsNone(run_search.read("t_2", around=1, window=1))
        self.assertEqual(call.call_args[0][0], {"desktop_id": "box", "task": "t_2", "around": 1, "window": 1})
        self.assertNotIn("Asha", run_search.render_run(None))

    def test_a_read_prints_the_stored_text_and_its_cut(self):
        run = {"task_id": "t_1", "total": 1, "outcome": "done_checked", "messages": [
            {"seq": 0, "kind": "request", "text": "file the return for Kiran Stores", "cut": 40}]}
        with mock.patch.object(run_search, "_call", return_value={"run": run}):
            text = run_search.main("", task="t_1")
        self.assertIn("=== PAST RUN t_1 ===", text)
        self.assertIn("[40 more characters in the task record]", text)

    def test_gui_recall_takes_the_runs_flags(self):
        from decurix_agent.computer import cli

        args = cli.build_parser().parse_args(["recall", "--runs", "march", "invoice", "--since", "7d"])
        self.assertTrue(args.runs)
        self.assertEqual((args.words, args.since), (["march", "invoice"], "7d"))
        args = cli.build_parser().parse_args(["recall", "--task", "t_1", "--around", "4"])
        self.assertEqual((args.task, args.around, args.words), ("t_1", 4, []))
        args = cli.build_parser().parse_args(["recall", "ledger"])
        self.assertFalse(args.runs)

    def test_no_words_and_no_task_is_refused(self):
        with self.assertRaises(run_search.SearchError):
            run_search.main("  ")


if __name__ == "__main__":
    unittest.main()
