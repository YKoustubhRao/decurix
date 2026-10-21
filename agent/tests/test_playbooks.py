import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import llm_client, playbooks, usage
from decurix_agent.computer import experience


def job(worked, turns=10, resumed=False, prompt_tokens=0, response_tokens=0):
    """A finished soul job, as its snapshot reports it."""

    return {
        "outcome": "answered", "resumed": resumed,
        "results": [{"SkillName": "operator", "Meta": {"worked": worked, "turns": turns}}],
        "usage": {"promptTokens": prompt_tokens, "responseTokens": response_tokens, "calls": turns},
    }


ORDER = 'Create a sales order for "Acme Corp" from the attached purchase order'
ORDER_WAY = [
    "gui observe",
    "office read /home/headless/work/inbox/up-1/po.pdf",
    'gui open "Orders"',
    "gui observe",
    "gui click e12",
    'gui select e20 "Acme Corp"',
    'gui type e31 "04-10-2026"',
    "gui key ctrl+s",
    "gui observe",
]


class _Store(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.said = []

        for p in (patch.object(playbooks, "ROOT", Path(self.tmp.name)),
                  patch("decurix_agent.progress.emit", lambda line, *a, **k: self.said.append(line))):
            p.start()
            self.addCleanup(p.stop)

        usage.reset()
        playbooks.begin()

    def do(self, request, worked, tokens, turns=10, done=True, **kw):
        """One task: the engine is given its context, works, and the check says done or not."""

        usage.reset()
        playbooks.begin()
        offered = playbooks.context(request)
        playbooks.note_job(job(worked, turns=turns, **kw))
        usage.add(input_tokens=tokens, output_tokens=0)
        if done:
            return offered, playbooks.keep(request)
        playbooks.missed()
        return offered, None


class KeepingTheWay(_Store):
    def test_a_checked_task_leaves_the_way_it_took_and_the_next_one_like_it_is_given_it(self):
        offered, name = self.do(ORDER, ORDER_WAY, tokens=180000, turns=34)
        self.assertEqual(offered, "")
        self.assertTrue(name)

        book = playbooks.load(name)
        # Looks are not the way; the customer was in the request; the date came from that run's document.
        self.assertEqual(book["commands"], [
            "office read /home/headless/work/inbox/up-1/po.pdf",
            'gui open "Orders"',
            "gui click e12",
            'gui select e20 "Acme Corp"',
            "gui type e31 <value>",
            "gui key ctrl+s",
        ])
        self.assertEqual(book["made"]["tokens"], 180000)

        # Kept, and not handed back to the engine: measured live, a list of earlier commands did no good
        # on the screen (its refs were stale on the next run) and cost more than it saved everywhere.
        self.assertEqual(playbooks.context('Create a sales order for "Globex Inc" from the attached purchase order'), "")
        self.assertEqual(self.said, [])

    def test_another_kind_of_request_is_given_nothing(self):
        self.do(ORDER, ORDER_WAY, tokens=1000)

        for other in ('Delete the sales order for "Acme Corp" from the attached purchase order',
                      "what is the kernel version?",
                      "Create a workbook of this month's invoices and draft an email with it",
                      ""):
            self.assertEqual(playbooks.context(other), "", other)

    def test_a_task_with_nothing_to_find_out_keeps_nothing(self):
        _, name = self.do("what is the kernel version of this machine", ["uname -r"], tokens=900, turns=2)
        self.assertIsNone(name)
        self.assertEqual(playbooks.all_books(), [])

    def test_what_counts_is_the_turns_it_took_not_how_many_commands_worked(self):
        # Found live: a look, a script that failed, the script that worked, the answer.
        script = "python3 - <<'PY' >/tmp/spec.json\nimport csv\nPY\noffice create summary_jan.xlsx - </tmp/spec.json"
        _, name = self.do("Summarize sales_jan.csv from the work folder into the workbook summary_jan.xlsx",
                          ["ls -l /home/headless/work", script], tokens=15532, turns=4)
        self.assertEqual(playbooks.load(name)["commands"], ["ls -l /home/headless/work", script])

        # Measured live: handed back as a list, those commands were run again one by one, in as many turns,
        # with the list carried in each (17,088 tokens without it; 20,240 and 20,961 with). Not offered.
        offered = playbooks.context("Summarize sales_feb.csv from the work folder into the workbook summary_feb.xlsx")
        self.assertEqual(offered, "")


SUMMARIZE = "Summarize sales_jan.csv from the work folder into the workbook summary_jan.xlsx"
BY_HAND = ["ls -l /home/headless/work", "python3 - <<'PY'\nimport csv\nPY\noffice create summary_jan.xlsx - </tmp/s.json"]
TOOL = {"worth": True, "language": "python", "script": "import sys\nprint('wrote', sys.argv[2])\n",
        "usage": "INPUT.csv OUTPUT.xlsx - the sales file and the workbook to write",
        "example": "sales_jan.csv summary_jan.xlsx", "does": "Totals revenue per region into a workbook."}


class AToolOfItsOwn(_Store):
    """Work done without the screen is written out once as a script, and run in one command after that."""

    def setUp(self):
        super().setUp()
        self.tools = Path(self.tmp.name) / "tools"
        self.answer, self.asked, self.proof = dict(TOOL), [], ""

        def chat(messages, **kw):
            import json
            self.asked.append(messages)
            usage.add(input_tokens=2000, output_tokens=500)
            return {"content": json.dumps(self.answer)}, {}

        for p in (patch.object(playbooks, "TOOLS", self.tools),
                  patch.object(playbooks, "_prove", lambda tool: self.proof),
                  patch.object(playbooks.llm_client, "available", lambda: True),
                  patch.object(playbooks.llm_client, "chat", chat)):
            p.start()
            self.addCleanup(p.stop)

    def test_the_commands_that_worked_become_one_script_and_the_next_request_is_given_it(self):
        _, name = self.do(SUMMARIZE, BY_HAND, tokens=15532, turns=4)

        book = playbooks.load(name)
        script = self.tools / f"{name}.py"
        self.assertEqual(script.read_text(encoding="utf-8"), TOOL["script"])
        self.assertEqual(book["tool"]["run"], f"python3 {script.as_posix()}")
        self.assertEqual(book["made"]["tokens"], 15532 + 2500)        # writing the tool is part of what learning cost
        self.assertIn("office create summary_jan.xlsx", self.asked[0][1]["content"])

        playbooks.begin()
        offered = playbooks.context(SUMMARIZE.replace("jan", "feb"))
        self.assertIn(f"python3 {script.as_posix()} INPUT.csv OUTPUT.xlsx", offered)
        self.assertIn("sales_jan.csv summary_jan.xlsx", offered)
        self.assertNotIn("import csv", offered)                        # the tool, not the old commands
        self.assertIn("I have a tool I wrote for a request like this: running it.", self.said)

    def test_a_run_that_used_the_tool_keeps_the_tool_and_shows_what_it_saved(self):
        _, name = self.do(SUMMARIZE, BY_HAND, tokens=15532, turns=4)
        run = playbooks.load(name)["tool"]["run"]

        for month in ("feb", "mar"):
            self.do(SUMMARIZE.replace("jan", month), [f"{run} sales_{month}.csv summary_{month}.xlsx"], tokens=6000, turns=2)

        book = playbooks.load(name)
        self.assertEqual(book["commands"], BY_HAND)                    # still what it was written from
        self.assertEqual(book["tool"]["unused"], 0)
        self.assertEqual(len(self.asked), 1)                           # written once
        self.assertEqual(playbooks.economy(book)["saved_percent"], 67)

    def test_a_tool_the_engine_keeps_passing_over_is_dropped(self):
        _, name = self.do(SUMMARIZE, BY_HAND, tokens=1000, turns=4)
        script = Path(playbooks.load(name)["tool"]["path"])

        self.do(SUMMARIZE, BY_HAND, tokens=1000, turns=4)
        self.assertTrue(script.exists())
        self.do(SUMMARIZE, BY_HAND, tokens=1000, turns=4)
        self.assertFalse(script.exists())
        self.assertNotIn("tool", playbooks.load(name))
        playbooks.begin()
        self.assertEqual(playbooks.context(SUMMARIZE), "")

    def test_a_tool_that_fails_its_check_twice_goes_with_its_playbook(self):
        _, name = self.do(SUMMARIZE, BY_HAND, tokens=1000, turns=4)
        script = Path(playbooks.load(name)["tool"]["path"])

        self.do(SUMMARIZE, [], tokens=1000, done=False)
        # The second attempt of that task is not handed the tool again.
        self.assertEqual(playbooks.context(SUMMARIZE), "")
        self.do(SUMMARIZE, [], tokens=1000, done=False)
        self.assertFalse(script.exists())
        self.assertIsNone(playbooks.load(name))

    def test_no_tool_is_kept_from_a_script_that_is_not_sound_or_not_worth_it(self):
        for answer in ({"worth": False},
                       dict(TOOL, script="def broken(:\n"),
                       dict(TOOL, script="import os\nKEY = 'sk-live-abcdefghijklmnopqrstuvwxyz0123456789'\n"),
                       dict(TOOL, language="bash", script="rm -rf /\n"),
                       "not json at all"):
            self.answer = answer
            _, name = self.do(SUMMARIZE, BY_HAND, tokens=1000, turns=4)
            self.assertNotIn("tool", playbooks.load(name), answer)
            self.assertEqual(list(self.tools.glob("*")) if self.tools.exists() else [], [], answer)
            playbooks.delete(name)

    def test_work_on_the_screen_and_work_that_stopped_for_a_yes_get_no_tool(self):
        self.do(ORDER, ORDER_WAY, tokens=1000)
        self.assertEqual(self.asked, [])

        usage.reset()
        playbooks.begin()
        playbooks.note_job(job(BY_HAND[:1], turns=2))                       # stopped for an approval
        playbooks.note_job(job(BY_HAND, turns=2, resumed=True))
        name = playbooks.keep(SUMMARIZE)
        self.assertEqual(self.asked, [])
        self.assertNotIn("tool", playbooks.load(name))

    def test_a_run_the_check_sent_back_is_not_kept_as_the_way(self):
        usage.reset()
        playbooks.begin()
        playbooks.note_job(job(ORDER_WAY))
        playbooks.missed()                       # the check said not done
        playbooks.note_job(job(["gui click e40", "gui type e41 \"7\"", "gui key ctrl+s"]))
        self.assertIsNone(playbooks.keep(ORDER))
        self.assertEqual(playbooks.all_books(), [])

    def test_a_job_that_resumed_a_paused_one_is_the_same_run(self):
        usage.reset()
        playbooks.begin()
        playbooks.note_job(job(ORDER_WAY[:5]))                 # paused for an approval
        playbooks.note_job(job(ORDER_WAY, resumed=True))       # soul's checkpoint carries the whole run
        name = playbooks.keep(ORDER)
        self.assertEqual(len(playbooks.load(name)["commands"]), 6)

    def test_a_password_is_never_kept(self):
        way = ['gui open "Portal"', 'gui type e3 "admin"', 'gui type e4 "hunter2-S3cret!"', "gui click e5",
               "export API_KEY=sk-live-abcdefghijklmnopqrstuvwxyz0123456789 && ./sync.sh", "gui click e9"]
        _, name = self.do("Sign in to the portal as admin and download the monthly statement", way, tokens=5000)

        kept = "\n".join(playbooks.load(name)["commands"])
        self.assertNotIn("hunter2", kept)
        self.assertNotIn("sk-live", kept)
        self.assertIn('gui type e3 "admin"', kept)          # in the request, so it is the request's to show
        self.assertIn("gui type e4 <value>", kept)

    def test_a_script_the_worker_wrote_is_kept_whole(self):
        script = "cat > totals.py <<'EOF'\n" + "\n".join(f"print({n})" for n in range(40)) + "\nEOF"
        _, name = self.do("Total the invoices in the inbox folder into a workbook",
                          [script, "python3 totals.py", "office read totals.xlsx"], tokens=4000)
        self.assertIn(script, playbooks.load(name)["commands"])


class GettingCheaper(_Store):
    def test_each_use_is_counted_and_the_shorter_way_is_the_one_kept(self):
        _, name = self.do(ORDER, ORDER_WAY, tokens=180000, turns=34)

        shorter = [c for c in ORDER_WAY if c != "gui click e12"]
        offered, again = self.do(ORDER.replace("Acme Corp", "Globex Inc"),
                                 [c.replace("Acme Corp", "Globex Inc") for c in shorter], tokens=60000, turns=9)
        self.assertEqual(offered, "")
        self.assertEqual(again, name)

        book = playbooks.load(name)
        self.assertEqual(len(book["commands"]), 5)
        self.assertEqual(playbooks.economy(book),
                         {"first": 180000, "now": 60000, "uses": 1, "ok": 1, "saved_percent": 67})

    def test_a_longer_way_replaces_the_kept_one_only_when_it_keeps_being_needed(self):
        _, name = self.do(ORDER, ORDER_WAY, tokens=1000)
        longer = ORDER_WAY + ["gui click e50", "gui click e51"]

        self.do(ORDER, longer, tokens=1000)
        self.assertEqual(len(playbooks.load(name)["commands"]), 6)
        self.do(ORDER, longer, tokens=1000)
        self.assertEqual(len(playbooks.load(name)["commands"]), 8)

    def test_a_run_that_was_given_nothing_counts_against_nothing(self):
        _, name = self.do(ORDER, ORDER_WAY, tokens=1000)

        self.do(ORDER, [], tokens=1000, done=False)
        self.do(ORDER, [], tokens=1000, done=False)
        self.assertEqual(playbooks.load(name)["misses"], 0)

    def test_one_miss_is_forgiven_by_a_run_that_then_works(self):
        _, name = self.do(ORDER, ORDER_WAY, tokens=1000)
        self.do(ORDER, [], tokens=1000, done=False)
        self.do(ORDER, ORDER_WAY, tokens=1000)
        self.assertEqual(playbooks.load(name)["misses"], 0)

    def test_switched_off_nothing_is_kept_or_offered(self):
        with patch.object(playbooks, "ENABLED", False):
            offered, name = self.do(ORDER, ORDER_WAY, tokens=1000)
        self.assertEqual((offered, name), ("", None))
        self.assertEqual(playbooks.all_books(), [])


GOOD = '''import sys
lines = open(sys.argv[1]).read().split()
total = sum(float(x) for x in lines)
open(sys.argv[2], "w").write("total %s\\n" % total)
print("wrote", sys.argv[2], total)
'''
# What the first tool written live did: it only worked when the file it writes was already there.
NEEDS_ITS_OUTPUT = GOOD.replace("lines = ", "import os\nif not os.path.isfile(sys.argv[2]): sys.exit('output not found')\nlines = ")
WRONG_SUM = GOOD.replace("sum(float(x) for x in lines)", "sum(float(x) for x in lines[:-1])")


class AToolIsProvenBeforeItIsKept(_Store):
    """The script is run in a copy of the work folder and must write what the checked task left - twice."""

    def setUp(self):
        super().setUp()
        self.tools = Path(self.tmp.name) / "tools"
        self.work = Path(self.tmp.name) / "work"
        self.work.mkdir()
        (self.work / "figures.txt").write_text("10\n20.5\n3\n", encoding="utf-8")
        # The input was there before the task began; out.txt is what the task left, as it printed it.
        import os
        import time
        began = time.time() - 60
        os.utime(self.work / "figures.txt", (began - 3600, began - 3600))
        (self.work / "out.txt").write_text("total 33.50\n", encoding="utf-8")
        started = patch.object(playbooks.produced, "since", lambda: began)
        started.start()
        self.addCleanup(started.stop)
        self.scripts, self.asked = [], []

        def chat(messages, **kw):
            import json
            self.asked.append(messages)
            return {"content": json.dumps({"worth": True, "language": "python", "script": self.scripts.pop(0),
                                           "usage": "IN OUT", "example": "figures.txt ~/work/out.txt",
                                           "does": "Totals the figures."})}, {}

        for p in (patch.object(playbooks, "TOOLS", self.tools),
                  patch.object(playbooks.config, "WORK_DIR", self.work),
                  patch.object(playbooks.llm_client, "available", lambda: True),
                  patch.object(playbooks.llm_client, "chat", chat)):
            p.start()
            self.addCleanup(p.stop)

    def made(self):
        _, name = self.do("Total the figures in figures.txt into out.txt for the weekly sheet",
                          ["cat figures.txt", "python3 - <<'PY'\nprint(1)\nPY"], tokens=1000, turns=4)
        return playbooks.load(name)

    def untouched(self):
        self.assertEqual((self.work / "out.txt").read_text(encoding="utf-8"), "total 33.50\n")
        self.assertEqual(sorted(p.name for p in self.work.iterdir()), ["figures.txt", "out.txt"])
        self.assertEqual([p for p in self.tools.glob(".proof-*")], [])

    def test_a_script_that_does_the_work_again_is_kept_and_the_real_folder_is_not_touched(self):
        self.scripts = [GOOD]
        book = self.made()
        self.assertTrue(Path(book["tool"]["path"]).exists())
        self.assertEqual(len(self.asked), 1)
        self.untouched()          # 33.5 and 33.50 read the same; the task's own file is as it was

    def test_one_that_fails_is_sent_back_once_with_what_went_wrong(self):
        self.scripts = [NEEDS_ITS_OUTPUT, GOOD]
        book = self.made()
        self.assertTrue(Path(book["tool"]["path"]).exists())
        self.assertEqual(len(self.asked), 2)
        told = self.asked[1][-1]["content"]
        self.assertIn("what it writes not yet there", told)
        self.assertIn("output not found", told)
        self.untouched()

    def test_one_that_writes_something_else_twice_is_not_kept(self):
        self.scripts = [WRONG_SUM, WRONG_SUM, WRONG_SUM]
        book = self.made()
        self.assertNotIn("tool", book)
        self.assertIn("does not read the same", self.asked[1][-1]["content"])
        self.assertEqual(list(self.tools.glob("*.py")), [])
        self.untouched()
        # ...and with no tool, shell work is offered nothing.
        playbooks.begin()
        self.assertEqual(playbooks.context("Total the figures in figures.txt into out.txt for the weekly sheet"), "")

    def test_one_that_fails_or_writes_nowhere_is_not_kept(self):
        self.scripts = ["import sys\nsys.exit('cannot do this')\n", "import sys\nprint('nothing written')\n",
                        "import sys\nprint('nothing written')\n"]
        book = self.made()
        self.assertNotIn("tool", book)
        told = [m["content"] for m in self.asked[-1] if m["role"] == "user"]
        self.assertIn("it exited 1: cannot do this", told[1])
        self.assertIn("It wrote no file", told[2])
        self.assertEqual(len(self.asked), 3)
        self.untouched()


class ATaughtSkillsOwnPlaybook(_Store):
    def test_it_is_found_by_the_skill_not_by_the_words(self):
        usage.reset()
        playbooks.begin()
        playbooks.note_job(job(ORDER_WAY))
        name = playbooks.keep('Do the task you were taught, "Enter a bill" (skill enter-a-bill) with ...', skill="enter-a-bill")
        self.assertEqual(playbooks.load(name)["skill"], "enter-a-bill")

        self.assertEqual(playbooks.find("anything at all", skill="enter-a-bill")["name"], name)
        # ...and never matched to an untaught request that happens to read like it.
        self.assertIsNone(playbooks.find('Do the task you were taught, "Enter a bill" (skill enter-a-bill) with ...'))


class EveryModelCallIsCounted(unittest.TestCase):
    def test_a_hosted_call_adds_to_the_tasks_totals_whoever_made_it(self):
        usage.reset()

        class Answer:
            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

            def read(self):
                return (b'{"choices":[{"message":{"content":"ok"}}],'
                        b'"usage":{"prompt_tokens":1200,"completion_tokens":30}}')

        with patch.object(llm_client.urllib.request, "urlopen", lambda *a, **k: Answer()):
            llm_client.chat([{"role": "user", "content": "one"}])
            llm_client.ask_once("system", "two")

        self.assertEqual(usage.snapshot(), {"input_tokens": 2400, "output_tokens": 60})
        usage.reset()

    def test_a_soul_job_reports_what_its_calls_used(self):
        from decurix_agent import thebot_harness

        usage.reset()
        playbooks.begin()
        thebot_harness._count(job(ORDER_WAY, prompt_tokens=90000, response_tokens=2100))
        thebot_harness._count({"outcome": "answered", "results": []})     # a soul from an older image
        self.assertEqual(usage.snapshot(), {"input_tokens": 90000, "output_tokens": 2100})
        usage.reset()


class ASkillsCost(unittest.TestCase):
    def test_a_skill_shows_what_it_costs_now_against_its_first_good_run(self):
        skill = {"name": "enter-a-bill", "steps": []}
        self.assertEqual(experience.economy(skill), {"first": None, "now": None, "saved_percent": None})

        experience.note_verdict(skill, "model", False, why="not done", tokens=250000)
        experience.note_verdict(skill, "model", True, checked_by="model", tokens=200000)
        self.assertEqual(experience.economy(skill), {"first": 200000, "now": None, "saved_percent": None})

        for tokens in (120000, 90000, 6000, 6000, 6000):
            experience.note_verdict(skill, "replay", True, checked_by="screen", tokens=tokens)

        self.assertEqual(experience.economy(skill), {"first": 200000, "now": 6000, "saved_percent": 97})
        self.assertEqual(experience.health(skill)["tokens"]["saved_percent"], 97)

    def test_a_run_nobody_counted_is_not_a_run_that_cost_nothing(self):
        skill = {"name": "x", "steps": []}
        experience.note_verdict(skill, "replay", True, checked_by="screen")
        self.assertNotIn("tokens", skill["experience"]["verdicts"][0])
        self.assertEqual(experience.economy(skill)["first"], None)


if __name__ == "__main__":
    unittest.main()


class ARunAPersonSteered(_Store):
    def test_what_was_done_after_a_persons_answer_is_not_kept_as_the_requests_way(self):
        # Found live: "open ... and sign in" stopped on a question; the answer asked for a supplier to be made.
        usage.reset()
        playbooks.begin()
        playbooks.note_job(job(['gui open "http://erp"', 'gui type e3 "Administrator"'], turns=6))
        playbooks.answered()
        playbooks.note_job(job(ORDER_WAY, turns=20, resumed=True))
        self.assertIsNone(playbooks.keep("Open http://erp in the browser and sign in with the username Administrator"))
        self.assertEqual(playbooks.all_books(), [])

    def test_a_yes_to_an_approval_is_not_steering(self):
        usage.reset()
        playbooks.begin()
        playbooks.note_job(job(ORDER_WAY[:5], turns=5))
        playbooks.note_job(job(ORDER_WAY, turns=9, resumed=True))
        self.assertTrue(playbooks.keep(ORDER))


class AToolsArgumentsAreWhatARequestStates(AToolOfItsOwn):
    def test_an_argument_the_request_never_states_sends_the_script_back(self):
        # Found live: PERIOD_LABEL "January" for a request that only names sales_jan.csv.
        self.answer = dict(TOOL, example="sales_jan.csv summary_jan.xlsx January")
        _, name = self.do(SUMMARIZE, BY_HAND, tokens=1000, turns=4)

        self.assertNotIn("tool", playbooks.load(name))
        self.assertEqual(len(self.asked), 3)
        told = [m["content"] for m in self.asked[-1] if m["role"] == "user"][1]
        self.assertIn("'January' is not in the request's own words", told)

    def test_a_file_by_its_path_or_a_quoted_phrase_is_stated(self):
        request = 'Count the units in sales_jan.csv and draft an email to ops@example.com with the subject "Units sold jan"'
        for arg in ("sales_jan.csv", "/home/headless/work/sales_jan.csv", "ops@example.com", "Units sold jan"):
            self.assertTrue(playbooks.in_request(arg, request), arg)
        for arg in ("January", "sales_feb.csv", "", "/etc/passwd"):
            self.assertFalse(playbooks.in_request(arg, request), arg)


class WhatARequestQuotesIsAnArgument(unittest.TestCase):
    def test_a_quoted_phrase_the_work_used_must_be_taken_not_rebuilt(self):
        # Found live: the subject was rebuilt from the file's name, and a request stating its own could not use the tool.
        request = 'Count the units in sales_jan.csv and draft an email to ops@example.com with the subject "Units sold jan"'
        used = ['office email --to ops@example.com --subject "Units sold jan" --attach sales_jan.csv']

        self.assertIn('quotes "Units sold jan"', playbooks._stated({"example": "sales_jan.csv ops@example.com"}, request, used))
        self.assertEqual(playbooks._stated({"example": "sales_jan.csv ops@example.com 'Units sold jan'"}, request, used), "")
        # A quoted phrase the work never used is not the script's to take.
        self.assertEqual(playbooks._stated({"example": "sales_jan.csv ops@example.com"}, request, ["cat sales_jan.csv"]), "")


class AToolThatWroteAWrongResult(AToolOfItsOwn):
    """Found live: proven on January's request, it drafted "January" for February."""

    def wrong_run(self, month):
        run = playbooks.load(self.name)["tool"]["run"]
        usage.reset()
        playbooks.begin()
        self.assertIn(run, playbooks.context(SUMMARIZE.replace("jan", month)))
        playbooks.note_job(job([f"{run} sales_{month}.csv summary_{month}.xlsx"], turns=2))
        playbooks.missed("The workbook says January where the request is for " + month)

    def test_it_is_dropped_at_once_and_the_next_one_is_told_what_was_wrong(self):
        _, self.name = self.do(SUMMARIZE, BY_HAND, tokens=1000, turns=4)
        script = Path(playbooks.load(self.name)["tool"]["path"])

        self.wrong_run("feb")
        book = playbooks.load(self.name)
        self.assertNotIn("tool", book)
        self.assertFalse(script.exists())
        self.assertEqual(book["tool_faults"], ["The workbook says January where the request is for feb"])
        # The second attempt of that task, and the next request, are the engine's own work.
        self.assertEqual(playbooks.context(SUMMARIZE.replace("jan", "feb")), "")

        # A clean run by hand gets a script written again - told what the last one got wrong.
        self.do(SUMMARIZE.replace("jan", "mar"), BY_HAND, tokens=1000, turns=4)
        self.assertIn("tool", playbooks.load(self.name))
        self.assertIn("The workbook says January where the request is for feb", self.asked[-1][1]["content"])
        self.assertIn("true only of the request above", self.asked[-1][1]["content"])

    def test_a_kind_of_request_whose_scripts_keep_going_wrong_is_left_to_the_engine(self):
        _, self.name = self.do(SUMMARIZE, BY_HAND, tokens=1000, turns=4)
        self.wrong_run("feb")
        self.do(SUMMARIZE.replace("jan", "mar"), BY_HAND, tokens=1000, turns=4)
        self.wrong_run("apr")

        written = len(self.asked)
        self.do(SUMMARIZE.replace("jan", "may"), BY_HAND, tokens=1000, turns=4)
        self.assertEqual(len(self.asked), written)
        self.assertNotIn("tool", playbooks.load(self.name))
