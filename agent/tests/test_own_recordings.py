import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import playbooks, usage
from decurix_agent.computer import mimic, recording, replay

REQUEST = 'In the ERP application open in the browser, create a new Supplier named "Deccan Pharma" with the supplier group "Pharmaceutical" and save it'
TASK = "t0123456789abcdef"
WAY = ["gui open http://erp:8000", "gui click e882", 'gui type e1055 "Deccan Pharma"', 'gui select e1058 "Pharmaceutical"',
       "gui click e1043"]


def element(role, name):
    return {"role": role, "name": name, "path": f"Suppliers/{role}:{name}"}


class _Worker(unittest.TestCase):
    """A worker with a recordings folder, in which an engine has just done a request on the screen."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)

        for p in (patch.object(recording, "ROOT", root / "recordings"),
                  patch.object(playbooks, "ROOT", root / "playbooks"),
                  patch.object(playbooks, "TOOLS", root / "tools"),
                  patch("decurix_agent.task_context.current", lambda: self.task),
                  patch("decurix_agent.progress.emit", lambda *a, **k: None)):
            p.start()
            self.addCleanup(p.stop)

        self.task = TASK
        self.record(TASK, REQUEST, "Deccan Pharma")

    def record(self, task, request, name, wandering=False):
        """What core.act's audit leaves of an engine's run: its actions, each with what the screen then showed."""

        session = recording.session_for_task(task)
        detour = [{"kind": "click", "actor": "agent", "element": element("button", "Edit Full Form")}] if wandering else []
        events = detour + [
            {"kind": "open", "actor": "agent", "text": "http://erp:8000",
             "expect": {"active_window": "Untitled - Chromium"}},
            {"kind": "click", "actor": "agent", "element": element("link", "Supplier"),
             "expect": {"appeared": [{"role": "link", "name": "Contacts"}]}},
            {"kind": "type", "actor": "agent", "element": element("textbox", "Supplier Name"), "text": name},
            {"kind": "click", "actor": "agent", "element": element("button", "Save"),
             "expect": {"appeared": [{"role": "alert", "name": "Saved"}]}},
        ]
        for n, event in enumerate(events):
            recording.append(session, dict(event), at=1000.0 + n, defaults={"title": request})
        return session

    def finish(self, request=REQUEST, way=WAY, **kw):
        """The engine's job ends and its result is checked done."""

        usage.reset()
        playbooks.begin()
        playbooks.note_job({"results": [{"SkillName": "operator", "Meta": {"worked": way, "turns": 12}}], **kw})
        return playbooks.keep(request)

    def prove(self):
        """Two requests of the kind, each done cleanly by the engine along the same path, each checked."""

        self.finish(REQUEST.replace("Deccan Pharma", "Bluewave Logistics"),
                    [c.replace("Deccan Pharma", "Bluewave Logistics") for c in WAY])
        return self.finish()

    def recordings_offered(self):
        return [c["id"] for c in mimic._candidates() if c["kind"] == "recording"]


class AWorkersOwnRunIsReplayedNextTime(_Worker):
    def test_a_task_recording_is_not_a_demonstration_until_its_result_was_checked(self):
        self.assertEqual(self.recordings_offered(), [])

        self.prove()
        self.assertEqual(self.recordings_offered(), [f"task-{TASK}"])
        meta = recording.meta(f"task-{TASK}")
        self.assertTrue(meta["proven"])
        self.assertEqual(meta["task"], REQUEST)

    def test_one_run_is_a_way_that_worked_and_two_that_agree_are_the_way(self):
        # The first runs of anything are the model's: nothing is replayed on the strength of one.
        self.finish()
        self.assertEqual(self.recordings_offered(), [])
        self.assertEqual(playbooks.context(REQUEST.replace("Deccan Pharma", "Granite")), "")   # and nothing is handed back

        # A second run that wandered does not agree with the first...
        self.task = "t1111111111111111"
        self.record(self.task, REQUEST.replace("Deccan Pharma", "Granite"), "Granite", wandering=True)
        self.finish(REQUEST.replace("Deccan Pharma", "Granite"))
        self.assertEqual(self.recordings_offered(), [])

        # ...and a third that pressed what the first pressed does, though the refs it used were other ones.
        self.task = "t2222222222222222"
        third = self.record(self.task, REQUEST.replace("Deccan Pharma", "Harbor"), "Harbor")
        self.finish(REQUEST.replace("Deccan Pharma", "Harbor"),
                    [c.replace("e8", "e47").replace("e10", "e52").replace("Deccan Pharma", "Harbor") for c in WAY])
        self.assertEqual(self.recordings_offered(), [third])

    def test_the_path_is_what_was_pressed_not_its_ref_and_not_what_was_typed(self):
        one = playbooks.skeleton(recording.load(f"task-{TASK}")[1])
        self.assertEqual(one, [["open", "", ""], ["click", "link", "Supplier"], ["type", "textbox", "Supplier Name"],
                               ["click", "button", "Save"]])
        self.task = "t3333333333333333"
        other = self.record(self.task, REQUEST, "Another Name Altogether")
        self.assertEqual(playbooks.skeleton(recording.load(other)[1]), one)

    def test_the_next_request_like_it_matches_it_with_its_own_values(self):
        self.prove()
        found = mimic.plan(REQUEST.replace("Deccan Pharma", "Granite Local Supplies"))
        self.assertEqual((found["kind"], found["id"]), ("recording", f"task-{TASK}"))

        skill, one_off = mimic.load_skill(found)
        self.assertTrue(one_off)
        values, _, _ = mimic.values_for(skill, found)
        self.assertIn("Granite Local Supplies", values.values())

    def test_its_steps_are_held_to_their_controls_not_to_what_the_engine_happened_to_see(self):
        self.prove()
        skill, _ = mimic.load_skill({"kind": "recording", "id": f"task-{TASK}", "text": REQUEST, "spans": []})
        self.assertTrue(skill["own"])
        self.assertEqual([s["kind"] for s in skill["steps"]], ["open", "click", "type", "click"])
        self.assertEqual([s.get("expect") or [] for s in skill["steps"]], [[], [], [], []])
        self.assertEqual(skill["steps"][1]["target"]["name"], "Supplier")

    def test_a_persons_demonstration_keeps_its_results(self):
        session = "demo-20261012-abcdef"
        recording.append(session, {"kind": "click", "actor": "human", "element": element("button", "Save"),
                                   "expect": {"appeared": [{"role": "alert", "name": "Saved"}]}}, at=1000.0,
                         defaults={"title": "save it"})
        recording.update(session, status="complete")
        skill, _ = mimic.load_skill({"kind": "recording", "id": session, "text": "save it", "spans": []})
        self.assertNotIn("own", skill)
        self.assertTrue(skill["steps"][0]["expect"])

    def test_only_a_clean_unhelped_run_on_the_screen_is_kept_this_way(self):
        # Shell work has a tool instead; a run a person answered, or one the check sent back, is not the way.
        self.finish()
        self.finish(way=["ls", "python3 x.py", "office read out.xlsx"])
        self.assertEqual(self.recordings_offered(), [])

        usage.reset()
        playbooks.begin()
        playbooks.answered()
        playbooks.note_job({"results": [{"SkillName": "operator", "Meta": {"worked": WAY, "turns": 12}}]})
        playbooks.keep(REQUEST)
        self.assertEqual(self.recordings_offered(), [])

        usage.reset()
        playbooks.begin()
        playbooks.note_job({"results": [{"SkillName": "operator", "Meta": {"worked": WAY[:2], "turns": 5}}]})
        playbooks.note_job({"results": [{"SkillName": "operator", "Meta": {"worked": WAY, "turns": 9}}]})
        playbooks.keep(REQUEST)
        self.assertEqual(self.recordings_offered(), [])

    def test_a_newer_run_of_the_same_kind_takes_the_older_ones_place_and_neither_is_pruned(self):
        self.prove()
        self.task = "tfedcba9876543210"
        newer = self.record(self.task, REQUEST.replace("Deccan Pharma", "Harbor Distribution"), "Harbor Distribution")
        self.finish(REQUEST.replace("Deccan Pharma", "Harbor Distribution"),
                    [c.replace("Deccan Pharma", "Harbor Distribution") for c in WAY])
        self.assertEqual(self.recordings_offered(), [newer])

        with patch.object(recording, "MAX_BYTES", 1):
            recording.prune()
        self.assertIsNotNone(recording.meta(newer))                 # kept like a demonstration
        self.assertIsNone(recording.meta(f"task-{TASK}"))           # the one it replaced is an ordinary task recording again


class APageThatHasNotLoadedIsNotAResult(unittest.TestCase):
    def test_a_title_that_only_says_loading_is_never_waited_for(self):
        for title, kept in (("Untitled - Chromium", "Chromium"),
                            ("New Tab - Chromium", "Chromium"),
                            ("host.docker.internal_8000/desk - Chromium", "Chromium"),
                            ("Suppliers - Chromium", "Suppliers - Chromium"),
                            ("notes.txt - Mousepad", "notes.txt - Mousepad"),
                            ("Untitled 2 - LibreOffice Calc", "LibreOffice Calc")):
            self.assertEqual(replay._stable_title(title), kept, title)


if __name__ == "__main__":
    unittest.main()


class ARunThatStopsWorking(_Worker):
    def test_two_replays_running_that_the_engine_had_to_take_over_retire_it(self):
        self.prove()
        session = f"task-{TASK}"

        playbooks.own_replayed(session, False)
        playbooks.own_replayed(session, True)        # one stop is forgiven by a run that then works
        playbooks.own_replayed(session, False)
        self.assertEqual(self.recordings_offered(), [session])
        playbooks.own_replayed(session, False)
        self.assertEqual(self.recordings_offered(), [])


class WhatAPersonSeesOfIt(_Worker):
    def test_one_entry_per_kind_of_request_saying_how_it_is_done_and_what_it_saved(self):
        self.prove()
        usage.add(input_tokens=100000)
        book = playbooks.find(REQUEST)
        book["made"]["tokens"] = 100000
        playbooks.save(book)

        usage.reset()
        usage.add(input_tokens=6000, output_tokens=100)
        playbooks.own_replayed(f"task-{TASK}", True)

        entry, = playbooks.listing()
        self.assertEqual((entry["how"], entry["recording"], entry["tool"]), ("replay", f"task-{TASK}", None))
        self.assertEqual((entry["first"], entry["now"], entry["saved_percent"], entry["uses"]), (100000, 3050, 97, 2))     # the second engine run (uncounted here) and the replay

        kept = playbooks.detail(entry["name"])
        self.assertEqual(kept["steps"][0], "gui open http://erp:8000")
        self.assertIsNone(playbooks.detail("no-such-thing"))

    def test_forgetting_it_sends_the_next_request_back_to_being_worked_out(self):
        self.prove()
        name = playbooks.listing()[0]["name"]

        self.assertTrue(playbooks.forget(name))
        self.assertEqual(playbooks.listing(), [])
        self.assertEqual(self.recordings_offered(), [])
        self.assertIsNotNone(recording.meta(f"task-{TASK}"))        # the task's own record is not deleted
        self.assertIsNone(mimic.plan(REQUEST.replace("Deccan Pharma", "Granite Local")))
        self.assertFalse(playbooks.forget(name))

    def test_a_tool_is_listed_with_its_script_and_goes_with_its_entry(self):
        tools = playbooks.TOOLS
        tools.mkdir(parents=True)
        (tools / "total-figures.py").write_text("print('wrote')\n", encoding="utf-8")
        playbooks.save({"name": "total-figures", "request": "Total figures.txt into out.txt weekly", "skill": None,
                        "shape": ["total", "figures", "txt", "out", "weekly"], "commands": ["cat figures.txt"],
                        "made": {"at": 5, "tokens": 15000}, "uses": [{"ok": True, "tokens": 2200}], "misses": 0,
                        "tool": {"path": str(tools / "total-figures.py"), "run": "python3 x", "usage": "IN OUT", "does": "Totals."}})

        entry, = playbooks.listing()
        self.assertEqual((entry["how"], entry["saved_percent"]), ("tool", 85))
        self.assertEqual(playbooks.detail("total-figures")["script"], "print('wrote')\n")
        playbooks.forget("total-figures")
        self.assertFalse((tools / "total-figures.py").exists())
