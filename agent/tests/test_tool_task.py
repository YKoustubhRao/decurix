import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import playbooks, tool_task, usage, verify

REQUEST = "Total the figures in figures_jan.txt into total_jan.txt for the weekly sheet"
SCRIPT = '''import sys
total = sum(float(x) for x in open(sys.argv[1]).read().split())
open(sys.argv[2], "w").write("total %s\\n" % total)
print("wrote", sys.argv[2], total)
'''


class _Worker(unittest.TestCase):
    """A worker with one proven tool, a work folder, a model that reads requests and a check."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.work = root / "work"
        self.work.mkdir()
        (root / "tools").mkdir()
        for month, figures in (("jan", "1 2"), ("feb", "10 20.5 3")):
            (self.work / f"figures_{month}.txt").write_text(figures, encoding="utf-8")

        self.said, self.reports, self.engine, self.asked = [], [], [], []
        self.reads = {"fits": True, "args": ["figures_feb.txt", "total_feb.txt"], "why": "the same work", "confidence": 0.95}
        self.verdict = {"verdict": verify.DONE, "confidence": 0.99, "seen": "total_feb.txt holds total 33.5", "why": ""}

        def chat(messages, **kw):
            self.asked.append((messages, kw))
            usage.add(input_tokens=600, output_tokens=40)
            return {"content": self.reads if isinstance(self.reads, str) else json.dumps(self.reads)}, {}

        def judge(task, **kw):
            self.judged = kw
            return self.verdict

        def engine(content, task_file):
            self.engine.append(content)

        for p in (patch.object(playbooks, "ROOT", root / "playbooks"),
                  patch.object(playbooks, "TOOLS", root / "tools"),
                  patch.object(tool_task.config, "WORK_DIR", self.work),
                  patch.object(tool_task.llm_client, "available", lambda: True),
                  patch.object(tool_task.llm_client, "chat", chat),
                  patch.object(tool_task.verify, "judge", judge),
                  patch.object(tool_task.produced, "files", lambda report="", **kw: [str(self.work / "total_feb.txt")]),
                  patch.object(tool_task.produced, "text", lambda paths: "total_feb.txt: total 33.5"),
                  patch.object(tool_task, "write_final_reply", lambda reply, **kw: self.reports.append((reply, kw))),
                  patch.object(tool_task.progress, "emit", lambda line, *a, **k: self.said.append(line)),
                  patch("decurix_agent.harness.perform_task", engine),
                  patch.object(tool_task, "DIRECT_AFTER", 1)):
            p.start()
            self.addCleanup(p.stop)

        path = root / "tools" / "total-figures.py"
        path.write_text(SCRIPT, encoding="utf-8")
        self.book = {
            "name": "total-figures", "request": REQUEST, "shape": playbooks.shape(REQUEST), "skill": None,
            "commands": ["cat figures_jan.txt", "python3 - <<'PY'\nprint(3)\nPY"],
            "made": {"at": 1, "tokens": 15000, "turns": 4, "commands": 2}, "misses": 0,
            "uses": [{"at": 2, "ok": True, "tokens": 8300, "turns": 2, "commands": 0, "tool": True}],
            "tool": {"path": str(path), "run": f"python3 {path.as_posix()}", "usage": "IN OUT",
                     "example": "figures_jan.txt total_jan.txt", "does": "Totals the figures in a file.", "unused": 0},
        }
        playbooks.save(self.book)
        usage.reset()
        playbooks.begin()

    def ask(self, request=REQUEST.replace("jan", "feb")):
        self.request = request
        if tool_task.handles(request):
            tool_task.perform(request)
            return True
        return False


class DoneWithNoEngine(_Worker):
    def test_the_request_is_read_into_arguments_the_tool_runs_and_the_result_is_checked(self):
        self.assertTrue(self.ask())

        self.assertEqual((self.work / "total_feb.txt").read_text(encoding="utf-8"), "total 33.5\n")
        self.assertEqual(self.engine, [])
        (reply, kw), = self.reports
        self.assertIn("wrote total_feb.txt 33.5", reply)
        self.assertIn("Checked against the files it wrote: total_feb.txt holds total 33.5", reply)
        self.assertEqual((kw["kind"], kw["engine"], kw["checked"]), ("final", "tool", True))

        # One small call read the request; the check read the file back, not the screen.
        self.assertEqual(len(self.asked), 1)
        # ...on the worker's own model: whether the request is what the script does is a judgement.
        self.assertNotIn("model", self.asked[0][1])
        self.assertIn("figures_jan.txt", self.asked[0][0][1]["content"])
        self.assertEqual((self.judged["screen"], self.judged["produced"]), (False, "total_feb.txt: total 33.5"))

        last = playbooks.load("total-figures")["uses"][-1]
        self.assertEqual((last["ok"], last["direct"], last["turns"], last["tokens"]), (True, True, 0, 640))

    def test_a_tool_is_run_directly_only_once_it_has_done_a_request_under_the_engines_eye(self):
        self.book["uses"] = []
        playbooks.save(self.book)
        self.assertFalse(self.ask())
        # ...unless told that a proven tool is enough.
        with patch.object(tool_task, "DIRECT_AFTER", 0):
            self.assertTrue(self.ask())

    def test_another_kind_of_request_or_no_model_is_not_this(self):
        self.assertFalse(tool_task.handles("Delete the figures in figures_feb.txt for the weekly sheet"))
        self.assertFalse(tool_task.handles("hello"))
        with patch.object(tool_task.llm_client, "available", lambda: False):
            self.assertFalse(tool_task.handles(self.book["request"]))


class TheEngineWhenTheToolCannot(_Worker):
    def nothing_ran(self):
        self.assertFalse((self.work / "total_feb.txt").exists())
        self.assertEqual(self.engine, [self.request])
        self.assertEqual(self.reports, [])

    def test_the_model_says_the_request_asks_for_more(self):
        self.reads = {"fits": False, "why": "it also asks for an email", "confidence": 0.9}
        self.ask(REQUEST.replace("jan", "feb") + " and email it to the team")
        self.nothing_ran()
        # The engine is not handed the tool for a request it does not fit.
        self.assertEqual(playbooks.context(self.request), "")

    def test_an_argument_that_is_not_in_the_request_is_a_guess(self):
        for args in (["figures_jan.txt", "total_feb.txt"],          # the example's own value
                     ["figures_feb.txt", "/etc/passwd"],            # nowhere in the request
                     ["figures_feb.txt"],                           # one too few
                     ["figures_feb.txt", "total_feb.txt", "x"]):    # one too many
            self.engine, self.reads = [], dict(self.reads, args=args)
            self.ask()
            self.nothing_ran()

    def test_a_reading_that_is_unsure_or_unreadable(self):
        for reads in (dict(self.reads, confidence=0.5), "not json", {"fits": True, "args": "figures_feb.txt"}):
            self.engine, self.reads = [], reads
            self.ask()
            self.nothing_ran()

    def test_a_path_is_the_requests_own_when_the_request_names_its_file(self):
        self.reads = dict(self.reads, args=[str(self.work / "figures_feb.txt"), "total_feb.txt"])
        self.ask()
        self.assertEqual(self.engine, [])
        self.assertTrue((self.work / "total_feb.txt").exists())

    def test_a_tool_that_fails_hands_over_and_twice_running_is_not_run_directly_again(self):
        (self.work / "figures_feb.txt").write_text("ten twenty", encoding="utf-8")

        self.ask()
        self.assertEqual(self.engine, [self.request])
        self.assertEqual(self.reports, [])
        self.assertIn("My tool did not work this time, so I am doing it by hand.", self.said)
        from decurix_agent import guidance
        self.assertIn("failed (exit 1)", guidance.text())
        guidance.end()
        self.assertEqual(playbooks.load("total-figures")["tool"]["direct_failures"], 1)

        playbooks.begin()
        self.ask()
        guidance.end()
        self.assertFalse(tool_task.handles(self.request))

    def test_a_result_the_check_finds_wrong_is_the_engines_to_correct_and_never_reported_done(self):
        self.verdict = {"verdict": verify.NOT_DONE, "confidence": 0.95, "seen": "total_feb.txt says 33.5", "why": "the request asked for an average"}
        self.ask()
        self.assertEqual(self.reports, [])
        self.assertEqual(self.engine, [self.request])
        from decurix_agent import guidance
        self.assertIn("the request asked for an average", guidance.text())
        guidance.end()
        book = playbooks.load("total-figures")
        self.assertFalse(book["uses"][-1]["ok"])
        # One wrong result ends the tool, and what was wrong is kept for the next one written.
        self.assertNotIn("tool", book)
        self.assertIn("the request asked for an average", book["tool_faults"][0])
        self.assertFalse(tool_task.handles(self.request))

    def test_a_result_the_check_cannot_settle_is_looked_at_by_the_engine_not_reported(self):
        self.verdict = {"verdict": verify.UNSURE, "confidence": 0.3, "seen": "", "why": "the total could not be read"}
        self.ask()
        self.assertEqual(self.reports, [])
        self.assertEqual(self.engine, [self.request])
        from decurix_agent import guidance
        told = guidance.text()
        guidance.end()
        self.assertIn("could not confirm", told)
        self.assertIn("do not make a second copy", told)
        self.assertIn("wrote total_feb.txt 33.5", told)
        # Not counted as a good run of the tool, and not as a fault of it either.
        book = playbooks.load("total-figures")
        self.assertFalse(book["uses"][-1]["ok"])
        self.assertIn("tool", book)


class ItCountsTowardsDirectRuns(unittest.TestCase):
    def test_a_checked_run_in_which_the_engine_ran_the_tool_is_marked_as_one(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(playbooks, "ROOT", Path(tmp) / "p"), \
                patch.object(playbooks, "TOOLS", Path(tmp) / "t"), patch("decurix_agent.progress.emit", lambda *a, **k: None):
            (Path(tmp) / "t").mkdir()
            path = Path(tmp) / "t" / "total-figures.py"
            path.write_text(SCRIPT, encoding="utf-8")
            playbooks.save({"name": "total-figures", "request": REQUEST, "shape": playbooks.shape(REQUEST), "skill": None,
                            "commands": ["cat figures_jan.txt"], "made": {"tokens": 1}, "uses": [], "misses": 0,
                            "tool": {"path": str(path), "run": f"python3 {path.as_posix()}", "usage": "IN OUT",
                                     "example": "a b", "does": "", "unused": 0}})
            usage.reset()
            playbooks.begin()
            self.assertIn(path.as_posix(), playbooks.context(REQUEST.replace("jan", "feb")))
            playbooks.note_job({"results": [{"SkillName": "operator", "Meta": {
                "worked": [f"python3 {path.as_posix()} figures_feb.txt total_feb.txt"], "turns": 2}}]})
            playbooks.keep(REQUEST.replace("jan", "feb"))

            book = playbooks.load("total-figures")
            self.assertTrue(book["uses"][-1]["tool"])
            self.assertTrue(tool_task._ready(book))


if __name__ == "__main__":
    unittest.main()
