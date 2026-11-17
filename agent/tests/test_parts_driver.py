"""
A skill still being learned is driven part by part (skill_task "Driven part by part"): code replays each part with this
run's values, the model is handed only the holes - a part done by hand or pressing the demonstration's own case, a part
whose value is not in hand, the rest of a part the repairs could not get through - and, before the part that commits to
the person, a check of the work so far. Owner (2026-11-16): "subparts of it are repeatable ... repeated sub parts logic
can be codified".
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import skill_task  # noqa: E402
from decurix_agent.computer import escalation, experience, part_record, replay, skills  # noqa: E402
from test_skill_parts import ALL_VALUES, SKILL  # noqa: E402


class Driver(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.replayed, self.holes, self.finished, self.paused = [], [], [], []
        outcome = {"value": replay.VERIFIED}

        def run_replay(program, params=None, start=1, **_):
            steps = [s["n"] for s in program["steps"] if s["n"] >= start]
            self.replayed.append((steps[0], steps[-1]))
            if callable(outcome["value"]):
                return outcome["value"](steps)
            return {"outcome": outcome["value"], "text": f"replayed {steps[0]}-{steps[-1]}", "resume_from": None,
                    "step": None}

        self.outcome = outcome
        for p in (mock.patch.object(part_record, "PATH", Path(tmp.name) / "parts.json"),
                  mock.patch.object(skill_task, "_engine_can_drive", return_value=True),
                  mock.patch.object(skill_task, "PART_REVIEW", "1"),
                  mock.patch.object(skill_task, "_stage", return_value=experience.APPRENTICE),
                  mock.patch.object(replay, "run", side_effect=run_replay),
                  mock.patch.object(skill_task, "_by_model",
                                    side_effect=lambda run, **kw: self.holes.append((dict(run.get("hole") or {}), kw))),
                  mock.patch.object(skill_task, "_finish", side_effect=lambda run, result: self.finished.append(result)),
                  mock.patch.object(skill_task, "_pause",
                                    side_effect=lambda run, kind, text, *a, **kw: self.paused.append((kind, text))),
                  mock.patch.object(skill_task.repairs, "on_halt", return_value=False),
                  mock.patch.object(skill_task.repairs, "settle"),
                  mock.patch.object(skill_task, "_lookup_ids", return_value={}),
                  mock.patch.object(skill_task.phase_note, "write"),
                  mock.patch("decurix_agent.task_context.current", return_value="t00000000000000a9")):
            p.start()
            self.addCleanup(p.stop)
        self.run_ = {"skill": SKILL, "values": dict(ALL_VALUES), "start": 1, "stuck": 0, "checked": True,
                     "ladder": escalation.Ladder(ask_model=None, goal="x", total=0)}

    def hole_reply(self, text="done"):
        skill_task._after_phase(self.run_, skill_task.PART_PHASE, text, "final")

    def test_the_plan_is_asked_once_then_the_parts_run(self):
        skill_task._drive_parts(self.run_)
        self.assertEqual(self.paused[0][0], "plan")
        self.assertIn("I do it myself for this case", self.paused[0][1])          # the demonstration's own case
        self.assertIn("your taught steps, replayed", self.paused[0][1])
        self.run_["plan_ok"] = True
        skill_task._drive_parts(self.run_)
        found = skills.parts(SKILL)
        self.assertEqual(self.replayed[0], (found[0]["first"], found[0]["last"]))  # code ran the first part
        self.assertEqual(self.replayed[1], (found[1]["first"], found[1]["last"]))  # and the next, no model turn

    def walk(self):
        self.run_["plan_ok"] = True
        skill_task._drive_parts(self.run_)
        for _ in range(12):
            if self.finished:
                break
            self.assertTrue(self.holes, "the driver stopped without a hole or a finish")
            self.hole_reply()

    def test_holes_are_the_by_hand_parts_and_the_review_before_the_commit(self):
        with mock.patch.object(skill_task, "_case_extras", return_value=["the B2B rows of table 12, from KCI-045"]):
            self.walk()
        found = skills.parts(SKILL)
        holes = [h for h, _ in self.holes]
        case = next(p for p in found if p["by_hand"] and "29AAQFC6610D1ZG" in p["by_hand"])
        self.assertIn(case["k"], [h["k"] for h in holes if not h["review"]])
        review = [h for h in holes if h["review"]]
        self.assertEqual(len(review), 1)
        commit = next(p for p in found if skill_task._commits_to_the_person(SKILL, p))
        self.assertEqual(review[0]["k"], commit["k"])
        replayed_firsts = [a for a, _ in self.replayed]
        self.assertNotIn(case["first"], replayed_firsts)                          # never replayed: the model's
        self.assertIn(commit["first"], replayed_firsts)                           # the commit itself: the code's
        self.assertTrue(all(kw.get("phase") == skill_task.PART_PHASE for _, kw in self.holes))
        # A run with holes is never finished as verified on the screen.
        self.assertEqual(self.finished[0]["outcome"], replay.COMPLETED_UNVERIFIED)
        self.assertIn("The task ran part by part", self.finished[0]["text"])

    def test_no_answer_from_the_check_still_reviews_within_a_budget(self):
        # Correctness first: a check that gave no answer is no reason to commit unchecked - the review runs, within its
        # action budget (measured 2026-11-17: an unbounded one ran 197 turns and the case timed out).
        from decurix_agent import repairs
        from decurix_agent.computer import phase
        noted = []
        with mock.patch.object(skill_task, "_case_extras", return_value=None),                 mock.patch.object(skill_task.phase_note, "write", side_effect=lambda *a, **k: noted.append((a, k))):
            self.walk()
        self.assertEqual(len([h for h, _ in self.holes if h["review"]]), 1)
        self.assertTrue(self.finished)

    def test_a_hole_that_finds_the_work_cannot_go_on_ends_the_driving(self):
        # Measured (2026-11-17): after a stop at a cancelled GSTIN the driver handed parts 5-8 to the model one by one.
        with mock.patch.object(skill_task, "_case_extras", return_value=[]):
            self.run_["plan_ok"] = True
            skill_task._drive_parts(self.run_)
            first = [h for h, _ in self.holes if not h["review"]]
            self.assertTrue(first)
            with mock.patch.object(skill_task, "_says_it_could_not", return_value=True):
                self.hole_reply("Stopped: the receiver's GSTIN is cancelled; nothing can be filed.")
        self.assertEqual(len([h for h, _ in self.holes if not h["review"]]), 1)
        self.assertEqual(len(self.finished), 1)
        self.assertIn("cannot go on", self.finished[0]["text"])

    def test_the_check_before_the_commit_reads_the_documents_and_asks_once(self):
        # Found live (2026-11-16): a wrong import made the check fail every time, and the full audit ran instead.
        seen = {}

        def chat(messages, **kw):
            seen["content"] = messages[1]["content"]
            return {"content": '{"missing": []}'}, {}

        with mock.patch.object(skill_task.llm_client, "available", return_value=True), \
                mock.patch.object(skill_task.llm_client, "chat", side_effect=chat), \
                mock.patch("decurix_agent.computer.records._request_folder_files", return_value=["/x/bill.docx"]), \
                mock.patch("decurix_agent.tools.office.read_document", return_value={"text": "Bill 7, 100.00", "images": []}):
            self.run_["parts_done"] = ["Part 1: replayed"]
            self.assertEqual(skill_task._case_extras(self.run_), [])
        self.assertIn("Bill 7, 100.00", seen["content"])
        self.assertIn("The values entered", seen["content"])

    def test_switched_off_there_is_no_review_before_the_commit(self):
        # Measured live (2026-11-16): the review cost ~3M tokens a case, or named given invoices as missing.
        with mock.patch.object(skill_task, "PART_REVIEW", "0"), \
                mock.patch.object(skill_task, "_case_extras", side_effect=AssertionError("not asked")):
            self.walk()
        self.assertFalse([h for h, _ in self.holes if h["review"]])
        self.assertTrue(self.finished)

    def test_by_default_only_a_run_typing_its_documents_values_is_checked(self):
        # Found live (2026-11-16): a month with a B2B invoice, taught on B2C only, filed without its B2B row; and a
        # filing whose invoices only check an import had them named "missing".
        with mock.patch.object(skill_task, "PART_REVIEW", "auto"):
            self.assertFalse(skill_task._part_review(self.run_))
            self.assertFalse(skill_task._part_review(dict(self.run_, evidence={"month": "the request says December"})))
            self.assertTrue(skill_task._part_review(dict(self.run_, evidence={"taxable": "204_invoice.pdf, Taxable"})))

    def test_nothing_missing_before_the_commit_means_no_review_hole(self):
        # Found (2026-11-16): an open audit before the filing cost ~3M tokens of a 4.3M case.
        with mock.patch.object(skill_task, "_case_extras", return_value=[]):
            self.walk()
        self.assertFalse([h for h, _ in self.holes if h["review"]])
        self.assertIn("hold nothing the entered values", self.finished[0]["text"])

    def test_what_the_documents_need_beyond_the_steps_is_the_only_review(self):
        with mock.patch.object(skill_task, "_case_extras", return_value=["the B2B rows of table 12, from KCI-045"]):
            self.run_["plan_ok"] = True
            skill_task._drive_parts(self.run_)
            for _ in range(12):
                review = [h for h, _ in self.holes if h["review"]]
                if review:
                    break
                self.hole_reply()
        self.run_["hole"] = review[0]
        text = skill_task._part_procedure(self.run_)
        self.assertIn("the B2B rows of table 12, from KCI-045", text)
        self.assertIn("do ONLY these", text)

    def test_a_part_whose_typed_value_is_not_in_hand_is_a_hole_with_its_command(self):
        del self.run_["values"]["client"]
        found = skills.parts(SKILL)
        part = next(p for p in found if "client" in p["params"])
        self.assertIn("client", skill_task._part_hole(self.run_, part))
        self.walk()
        hole = next(h for h, _ in self.holes if h["k"] == part["k"])
        self.run_["hole"] = hole
        text = skill_task._part_procedure(self.run_)
        self.assertIn(f"--from {part['first']} --to {part['last']}", text)
        self.assertIn('client="<Search, as the screen shows it by then>"', text)

    def test_a_pick_not_in_hand_is_no_hole_the_replay_chooses_it(self):
        # (computer/choices.py: chosen at its step from what the screen lists.)
        del self.run_["values"]["export_file"]
        part = next(p for p in skills.parts(SKILL) if "export_file" in p["params"])
        self.assertEqual(skill_task._part_hole(self.run_, part), "")

    def test_a_stop_the_repairs_cannot_fix_is_a_hole_for_the_rest_of_that_part(self):
        found = skills.parts(SKILL)
        stop = found[0]["first"] + 2

        def halt(steps):
            if steps[0] == found[0]["first"]:
                return {"outcome": replay.HALTED, "text": "Stopped: not on the screen", "resume_from": stop + 1,
                        "step": stop}
            return {"outcome": replay.VERIFIED, "text": "ok", "resume_from": None, "step": None}

        self.outcome["value"] = halt
        self.run_["plan_ok"] = True
        skill_task._drive_parts(self.run_)
        self.assertEqual(self.holes[0][0]["k"], found[0]["k"])
        self.assertEqual(self.holes[0][0]["from"], stop)
        self.hole_reply()
        self.assertEqual(self.replayed[-1][0], found[1]["first"])                 # the driver goes on after it

    def test_a_hole_the_model_cannot_finish_hands_it_the_rest(self):
        self.run_["plan_ok"] = True
        self.walk_to_first_hole()
        skill_task._after_phase(self.run_, skill_task.PART_PHASE, "I could not", "query")
        self.assertFalse(self.run_.get("parts"))
        self.assertTrue(self.run_.get("parts_off"))
        self.assertIsNone(self.holes[-1][1].get("phase"))                        # the model's, whole, from there

    def walk_to_first_hole(self):
        skill_task._drive_parts(self.run_)
        self.assertTrue(self.holes)

    def test_parts_replayed_are_noted_for_the_record(self):
        self.walk()
        found = skills.parts(SKILL)
        self.assertTrue(any(part_record.key(SKILL, found[0]) in v for v in [part_record._load().get(SKILL["name"], {})]))

    def test_off_switch_and_one_offs(self):
        with mock.patch.object(skill_task, "DRIVE_PARTS", False):
            self.assertFalse(skill_task._drives_parts(self.run_))
        self.assertFalse(skill_task._drives_parts(dict(self.run_, one_off=True)))
        self.assertFalse(skill_task._drives_parts(dict(self.run_, start=5)))
        self.assertTrue(skill_task._drives_parts(self.run_))


if __name__ == "__main__":
    unittest.main()
