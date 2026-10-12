"""
Unit tests for the parts of running a skill that go beyond exact replay:
healing a control that has been renamed or moved, the escalation ladder (heal,
ask the engine, ask a person), and what a skill learns from being run. None of
it needs a display or a model - the engine is a function the test writes.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import unittest
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
import test_skills as ts
from test_recordings import FakeScreen, META, EVENTS, event, field

from decurix_agent import skill_task, state
from decurix_agent.computer import escalation, experience, healing, learning, replay, skills

PATH = 'site x > form "Order" > button "{}"'


def button(name, box, **extra):
    return field("button", name, box=box, path=PATH.format(name), **extra)


def save_step(**extra):
    step = {"n": 1, "id": "s1", "kind": "click", "summary": 'click button "Save"',
            "target": {"role": "button", "name": "Save", "path": PATH.format("Save"), "window_title": "Shop - Chromium"},
            "locate": {"box": [500, 400, 80, 30], "screen": [1280, 720], "anchors": ["cancel"]}}
    step.update(extra)
    return step


def screen(*elements):
    return {"elements": list(elements), "size": [1280, 720], "active_title": "Shop - Chromium"}


CANCEL = button("Cancel", [400, 400, 80, 30])


class Names(unittest.TestCase):

    def test_a_typo_or_a_plural_is_close_and_an_unrelated_word_is_not(self):
        self.assertGreater(healing.name_similarity("Add row", "Add rows"), 0.85)
        self.assertGreater(healing.name_similarity("Recieve", "Receive"), 0.8)
        self.assertLess(healing.name_similarity("Save", "Archive"), 0.4)

    def test_a_synonym_is_close_but_never_the_same(self):
        self.assertEqual(healing.name_similarity("Save", "Submit"), 0.8)
        self.assertEqual(healing.name_similarity("Save", "Save"), 1.0)

    def test_a_longer_label_is_not_taken_for_the_shorter_one_on_its_name_alone(self):
        # "Delete" and "Delete all" are different actions.
        self.assertEqual(healing.name_similarity("Delete", "Delete all"), 0.75)

    def test_counters_in_a_label_do_not_count(self):
        self.assertEqual(healing.name_similarity("Inbox (3)", "Inbox (12)"), 1.0)


class Healing(unittest.TestCase):

    def test_a_renamed_control_in_the_same_place_is_found(self):
        renamed = button("Save changes", [502, 401, 110, 30], group="g")
        ranked = healing.rank(save_step(), screen(renamed, CANCEL))

        verdict, top = healing.decide(save_step(), ranked)
        self.assertEqual(verdict, "confident")
        self.assertEqual(top["element"]["name"], "Save changes")

    def test_a_similar_name_somewhere_else_is_doubt_not_a_match(self):
        far = field("button", "Delete all", box=[50, 600, 90, 30], path='site x > table "Rows" > button "Delete all"', group="g")
        step = save_step(target={"role": "button", "name": "Delete", "path": PATH.format("Delete"), "window_title": "Shop - Chromium"},
                         locate={"box": [500, 400, 80, 30], "screen": [1280, 720], "anchors": ["cancel"]})

        verdict, top = healing.decide(step, healing.rank(step, screen(far, CANCEL)))
        self.assertEqual(verdict, "doubt")

    def test_two_identical_controls_are_told_apart_by_where_the_recording_had_it(self):
        near = button("Delete", [500, 400, 80, 30], group="g")
        far = field("button", "Delete", box=[900, 100, 80, 30], path=PATH.format("Delete") + "[2]", group="g")
        step = save_step(target={"role": "button", "name": "Delete", "path": PATH.format("Delete"), "window_title": "Shop - Chromium"})

        verdict, top = healing.decide(step, healing.rank(step, screen(far, near)))
        self.assertEqual(verdict, "confident")
        self.assertIs(top["element"], near)

    def test_two_identical_controls_with_nothing_to_tell_them_apart_are_left_alone(self):
        a = field("button", "Delete", box=[10, 10, 80, 30], path='site x > button "Delete"')
        b = field("button", "Delete", box=[10, 10, 80, 30], path='site x > button "Delete"[2]')
        step = {"n": 1, "kind": "click", "target": {"role": "button", "name": "Delete"}}

        verdict, _ = healing.decide(step, healing.rank(step, screen(a, b)))
        self.assertNotEqual(verdict, "confident")

    def test_a_step_the_gate_guards_is_never_healed_by_a_rename(self):
        renamed = button("Submit", [500, 400, 80, 30], group="g")
        step = save_step(consequential=True)

        verdict, top = healing.decide(step, healing.rank(step, screen(renamed, CANCEL)))
        self.assertEqual(verdict, "doubt")

        # ... while the same screen heals an ordinary step.
        self.assertEqual(healing.decide(save_step(), healing.rank(save_step(), screen(renamed, CANCEL)))[0], "confident")

    def test_a_different_kind_of_control_is_not_a_candidate(self):
        ranked = healing.rank(save_step(), screen(field("checkbox", "Save", box=[500, 400, 80, 30])))
        self.assertEqual(ranked, [])

    def test_a_name_a_run_has_proven_is_found_exactly(self):
        step = save_step(alternates=[{"role": "button", "name": "Store it"}])
        ranked = healing.rank(step, screen(button("Store it", [500, 400, 80, 30], group="g"), CANCEL))
        self.assertEqual(ranked[0]["name"], 1.0)
        self.assertEqual(healing.decide(step, ranked)[0], "confident")

    def test_a_disabled_control_ranks_below_an_enabled_one(self):
        on = button("Save changes", [500, 400, 80, 30], group="g")
        off = button("Save changes", [500, 400, 80, 30], group="g", states=["disabled"])
        off["path"] += "[2]"
        ranked = healing.rank(save_step(), screen(off, on))
        self.assertIs(ranked[0]["element"], on)


def asks(*replies):
    """An engine that gives these replies in turn (an Exception is raised) and remembers what it was asked."""

    calls = []
    queue = list(replies)

    def ask(system, prompt):
        calls.append((system, prompt))
        reply = queue.pop(0)
        if isinstance(reply, Exception):
            raise reply
        return reply

    ask.calls = calls
    return ask


def doubtful():
    """A screen where healing is unsure: nothing named like Save, two controls that might be it."""

    return screen(button("Store", [500, 400, 80, 30], group="g"), CANCEL)


class Ladder(unittest.TestCase):

    def recover(self, ask, step=None, **kwargs):
        ladder = escalation.Ladder(ask_model=ask, goal="Save the order", total=4, **kwargs)
        return ladder, ladder.recover(step or save_step(), doubtful(), "button \"Save\" is not on the screen")

    def test_the_engine_may_choose_among_the_candidates(self):
        ask = asks('Sure: {"choice": 1, "confidence": 0.9, "reason": "same place"}')
        ladder, found = self.recover(ask)

        self.assertEqual(found["how"], "grounded")
        self.assertEqual(found["element"]["name"], "Store")
        self.assertEqual(ladder.model_calls, 1)
        # It is shown the recorded control and the numbered candidates, never asked to invent one.
        self.assertIn('Recorded control: button "Save"', ask.calls[0][1])
        self.assertIn('1. button "Store"', ask.calls[0][1])

    def test_an_engine_that_is_not_sure_leaves_it_to_a_person(self):
        for reply in ('{"choice": 0, "confidence": 0.9}',                    # none of them
                      '{"choice": 1, "confidence": NaN}',
                      '{"choice": 1, "confidence": Infinity}',
                      '{"choice": 1, "confidence": 0.5}',                    # leaning, not sure
                      '{"choice": 7, "confidence": 0.99}',                   # outside the list
                      "I would probably click Store.",                       # prose
                      '{"choice": "one", "confidence": "high"}'):
            _, found = self.recover(asks(reply))
            self.assertIsNone(found["element"], reply)
            self.assertEqual(found["options"][0]["name"], "Store")

    def test_an_engine_that_cannot_be_reached_is_not_a_yes(self):
        ladder, found = self.recover(asks(RuntimeError("Ollama is down")))
        self.assertIsNone(found["element"])
        self.assertTrue(any("could not be asked" in line for line in ladder.trail))

    def test_no_engine_means_healing_then_a_person(self):
        _, found = self.recover(None)
        self.assertIsNone(found["element"])
        self.assertEqual([o["n"] for o in found["options"]], [1, 2])

    def test_a_strict_step_never_reaches_the_engine(self):
        for step in (save_step(consequential=True), save_step(optional=True)):
            ask = asks('{"choice": 1, "confidence": 1}')
            _, found = self.recover(ask, step)
            self.assertIsNone(found["element"])
            self.assertEqual(ask.calls, [])

    def test_the_engine_is_asked_a_bounded_number_of_times(self):
        ask = asks('{"choice": 0}', '{"choice": 0}', '{"choice": 0}')
        ladder = escalation.Ladder(ask_model=ask, model_calls=2)

        for _ in range(3):
            ladder.recover(save_step(), doubtful(), "not found")

        self.assertEqual(len(ask.calls), 2)

    def test_a_step_with_no_target_has_nothing_to_look_for(self):
        ladder = escalation.Ladder(ask_model=asks())
        found = ladder.recover({"n": 1, "kind": "click", "point": [5, 5]}, doubtful(), "recorded as a point")
        self.assertIsNone(found["element"])
        self.assertEqual(found["options"], [])

    def test_the_engine_sees_what_is_in_front(self):
        ask = asks('{"choice": 0}')
        observation = screen(button("Store", [500, 400, 80, 30]), field("dialog", "Session expired"))
        escalation.Ladder(ask_model=ask).recover(save_step(), observation, "not found")
        self.assertIn('In front: "Session expired"', ask.calls[0][1])


class Judging(unittest.TestCase):

    def judge(self, reply):
        ladder = escalation.Ladder(ask_model=asks(reply))
        observation = {"elements": [field("heading", "Orders")], "windows": [{"title": "Shop"}], "size": [1280, 720],
                       "active_title": "Shop", "page_url": "http://shop.test/"}
        return ladder, ladder.judge(save_step(), observation, ['an alert "Saved"'])

    def test_a_confident_yes_is_taken(self):
        ladder, ok = self.judge('{"achieved": true, "confidence": 0.95}')
        self.assertTrue(ok)
        self.assertEqual(ladder.judged, [1])

    def test_anything_less_is_not(self):
        for reply in ('{"achieved": true, "confidence": 0.6}', '{"achieved": null, "confidence": 0.9}',
                      '{"achieved": true, "confidence": NaN}',
                      '{"achieved": true, "confidence": Infinity}',
                      '{"achieved": false, "confidence": 0.99}', "looks fine to me"):
            self.assertFalse(self.judge(reply)[1], reply)

    def test_a_step_that_could_do_harm_is_never_judged_into_success(self):
        ask = asks('{"achieved": true, "confidence": 1}')
        self.assertFalse(escalation.Ladder(ask_model=ask).judge(save_step(consequential=True), {"elements": []}, ["x"]))
        self.assertEqual(ask.calls, [])


class Running(unittest.TestCase):
    """replay.run with a ladder."""

    def setUp(self):
        self.saved = replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0

    def tearDown(self):
        replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = self.saved

    def program(self, **step):
        return {"session": "s", "title": "Save the order", "params": [], "steps": [save_step(**step)]}

    def run_on(self, elements, ladder=None, **step):
        fake = FakeScreen(elements, after={"adds": [field("alert", "Saved")]})
        replay.core = fake
        return fake, replay.run(self.program(**step), report=lambda line: None, ladder=ladder)

    def test_without_a_ladder_a_renamed_control_still_halts(self):
        fake, result = self.run_on([button("Save changes", [500, 400, 80, 30], group="g")])
        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertEqual(fake.acted, [])

    def test_with_a_ladder_it_is_found_and_the_run_says_so(self):
        renamed = button("Save changes", [500, 400, 80, 30], group="g")
        ladder = escalation.Ladder()
        fake, result = self.run_on([renamed, CANCEL], ladder, expect=[{"type": "element", "role": "alert", "name": "Saved"}])

        self.assertEqual(result["outcome"], replay.VERIFIED)
        self.assertEqual(fake.acted[0][1]["path"], renamed["path"])
        self.assertEqual(result["healed"], 1)
        self.assertIn('("Save" is now "Save changes")', result["text"])
        self.assertIn("No model was used", result["text"])
        self.assertIn("s1", ladder.confirmed)

    def test_a_control_nothing_is_like_still_halts_and_offers_what_is_closest(self):
        ladder = escalation.Ladder(ask_model=None)
        fake, result = self.run_on([button("Store", [500, 400, 80, 30], group="g"), CANCEL], ladder)

        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertEqual(fake.acted, [])
        self.assertEqual(result["options"][0]["name"], "Store")
        self.assertTrue(result["trail"])

    def test_a_result_that_is_not_there_exactly_can_be_judged_but_is_never_called_verified(self):
        ladder = escalation.Ladder(ask_model=asks('{"achieved": true, "confidence": 0.95}'))
        elements = [button("Save", [500, 400, 80, 30], group="g")]
        expect = [{"type": "element", "role": "alert", "name": "Saved"}]

        fake = FakeScreen(elements)               # nothing appears after the click
        replay.core = fake
        fake.observation = lambda: {"elements": elements, "windows": [{"title": "Shop"}], "size": [1280, 720],
                                    "active_title": "Shop - Chromium", "page_url": "http://shop.test/"}
        result = replay.run(self.program(expect=expect), report=lambda line: None, ladder=ladder)

        self.assertEqual(result["outcome"], replay.COMPLETED_UNVERIFIED)
        self.assertEqual(result["judged"], 1)
        self.assertIn("took the screen's word", result["text"])

    def test_a_result_the_engine_will_not_vouch_for_halts_as_before(self):
        ladder = escalation.Ladder(ask_model=asks('{"achieved": null, "confidence": 0.9}'))
        elements = [button("Save", [500, 400, 80, 30], group="g")]
        replay.core = fake = FakeScreen(elements)
        fake.observation = lambda: {"elements": elements, "windows": [], "size": [1280, 720],
                                    "active_title": "Shop - Chromium", "page_url": "http://shop.test/"}
        result = replay.run(self.program(expect=[{"type": "element", "role": "alert", "name": "Saved"}]),
                            report=lambda line: None, ladder=ladder)
        self.assertEqual(result["outcome"], replay.HALTED)

    def test_an_optional_step_is_not_healed_into_a_different_control(self):
        # The banner is not there today; a similar-looking control must not stand in for it.
        other = button("Dismiss all notifications", [500, 400, 80, 30], group="g")
        fake, result = self.run_on([other], escalation.Ladder(), optional=True)
        self.assertEqual(fake.acted, [])
        self.assertEqual(result["skipped"], 1)


class Ingestion(unittest.TestCase):

    def test_a_program_keeps_where_each_control_was_and_which_part_of_the_task_it_is_in(self):
        program = replay.compile_program(META, EVENTS)

        self.assertEqual(program["steps"][0]["locate"]["box"], [10, 10, 100, 20])
        self.assertEqual(program["steps"][0]["locate"]["screen"], [1280, 720])
        self.assertNotIn("box", program["steps"][0]["target"])
        self.assertTrue(all("phase" in step and step["phase_label"] for step in program["steps"]))

    def test_a_change_of_window_starts_a_new_phase(self):
        events = [event(0, "click", field("link", "Customers")),
                  event(1, "click", field("button", "Open"), window="Files - Thunar"),
                  event(2, "click", field("button", "Save"), window="Files - Thunar")]
        steps = replay.compile_program(META, events)["steps"]
        self.assertEqual([s["phase"] for s in steps], [1, 2, 2])
        self.assertEqual(steps[1]["phase_label"], "Files - Thunar")


def ladder_with(healed=(), seen=None, confirmed=()):
    ladder = escalation.Ladder()
    ladder.healed = list(healed)
    ladder.seen = seen or {}
    ladder.confirmed = set(confirmed)
    return ladder


HEAL = {"id": "s1", "n": 1, "how": "healed", "confidence": 0.8, "was": "Save", "now": "Save changes", "role": "button"}
SEEN = {"s1": {"box": [502, 401, 110, 30], "screen": [1280, 720], "anchors": ["cancel"]}}


class Learning(unittest.TestCase):

    def skill(self):
        return {"name": "k", "steps": [save_step()], "version": 1}

    def test_a_repair_the_screen_confirmed_is_kept_at_once(self):
        skill = self.skill()
        learned = experience.commit(skill, ladder_with([HEAL], SEEN, confirmed=["s1"]), "verified", True)

        self.assertEqual(skill["steps"][0]["alternates"], [{"role": "button", "name": "Save changes"}])
        self.assertEqual(skill["steps"][0]["locate"]["box"], [502, 401, 110, 30])
        self.assertIn("confirmed on screen", learned[0])
        self.assertEqual(skill["version"], 2)

    def test_a_repair_nothing_confirmed_has_to_happen_twice(self):
        skill = self.skill()

        experience.commit(skill, ladder_with([HEAL], SEEN), "completed_unverified", True)
        self.assertNotIn("alternates", skill["steps"][0])
        self.assertEqual(skill["steps"][0]["locate"]["box"], [500, 400, 80, 30])      # the moved box is not trusted yet

        experience.commit(skill, ladder_with([HEAL], SEEN), "completed_unverified", True)
        self.assertEqual(skill["steps"][0]["alternates"][0]["name"], "Save changes")

    def test_a_run_that_did_not_succeed_teaches_nothing(self):
        skill = self.skill()
        learned = experience.commit(skill, ladder_with([HEAL], SEEN, confirmed=["s1"]), "halted", False)

        self.assertEqual(learned, [])
        self.assertNotIn("alternates", skill["steps"][0])
        self.assertEqual(skill["version"], 1)
        self.assertEqual(skill["experience"]["runs"][-1]["outcome"], "halted")

    def test_a_step_that_keeps_needing_help_is_reported_before_it_breaks(self):
        skill = self.skill()

        for _ in range(3):
            experience.commit(skill, ladder_with([HEAL], SEEN), "completed_unverified", True)

        health = experience.health(skill)
        self.assertEqual([(e["n"], e["needed"], e["of"]) for e in health["fragile"]], [(1, 3, 3)])
        self.assertIn("record it again", experience.advice(skill))

    def test_a_step_that_ran_clean_since_is_no_longer_fragile(self):
        skill = self.skill()
        for _ in range(3):
            experience.commit(skill, ladder_with([HEAL], SEEN), "completed_unverified", True)
        for _ in range(3):
            experience.commit(skill, ladder_with([], SEEN), "verified", True)

        self.assertEqual(experience.health(skill)["fragile"], [])
        self.assertEqual(experience.advice(skill), "")

    def test_the_names_a_step_knows_are_bounded(self):
        step = save_step()
        for n in range(9):
            experience._promote(step, "button", f"Save {n}")
        self.assertEqual(len(step["alternates"]), experience.ALIAS_LIMIT)
        self.assertEqual(step["alternates"][-1]["name"], "Save 8")


class Flow(ts.TeachingBase):
    """A skill run end to end, with the engine and the person as tests."""

    def setUp(self):
        super().setUp()
        self.backend = state.backend_ready
        state.backend_ready = True
        self.addCleanup(lambda: setattr(state, "backend_ready", self.backend))

    def taught(self, expect=None):
        extra = {"expect": {"appeared": [{"role": "alert", "name": "Saved"}]}} if expect else {}
        meta = {"id": self.SESSION, "title": "Save the form", "kind": "demonstration", "status": "complete",
                "started_at": 1789380000, "screen": [1280, 720]}
        skill = learning.build([(meta, [event(0, "click", field("button", "Save"), **extra)])])
        skill["status"] = skills.READY
        skills.save(skill)
        return skill["name"]

    def engine(self, *replies):
        ask = asks(*replies)
        patch = mock.patch.object(skill_task, "_ask_model", ask)
        patch.start()
        self.addCleanup(patch.stop)
        return ask

    def test_a_renamed_control_is_repaired_and_learned_when_the_screen_confirms_it(self):
        name = self.taught(expect=True)
        fake = self.screen()
        fake.elements[:] = [field("button", "Save changes")]
        fake.after = {"adds": [field("alert", "Saved")]}
        self.asked.clear()

        skill_task.perform(f"/skill {name}")

        self.assertEqual(self.asked, [])
        self.assertEqual(self.finals[-1][1], "final", self.finals[-1][0])
        self.assertIn('I repaired step 1 ("Save" is now "Save changes")', self.finals[-1][0])
        self.assertIn("learned from this run", self.finals[-1][0])
        self.assertEqual(skills.load(name)["steps"][0]["alternates"], [{"role": "button", "name": "Save changes"}])

    def test_a_repair_nothing_confirmed_is_learned_the_second_time(self):
        name = self.taught()
        for _ in range(2):
            fake = self.screen()
            fake.elements[:] = [field("button", "Save changes")]
            skill_task.perform(f"/skill {name}")
            self.confirm_done()

        self.assertEqual([f[1] for f in self.finals], ["final", "final"])
        self.assertNotIn("learned from this run", self.finals[0][0])
        self.assertIn("learned from this run", self.finals[1][0])

    def test_the_engine_grounds_a_control_healing_was_unsure_of(self):
        name = self.taught()
        ask = self.engine('{"choice": 1, "confidence": 0.9, "reason": "same button"}')
        fake = self.screen()
        fake.elements[:] = [field("button", "Store"), field("button", "Cancel", path="site x > button Cancel", box=[300, 300, 50, 20])]

        skill_task.perform(f"/skill {name}")
        self.confirm_done()

        self.assertEqual(len(ask.calls), 1)
        self.assertEqual(self.finals[-1][1], "final", self.finals[-1][0])
        self.assertEqual(fake.acted[0][1]["path"], fake.elements[0]["path"])

    def test_without_a_ready_engine_a_person_is_asked_with_the_candidates_numbered(self):
        state.backend_ready = False
        name = self.taught()
        ask = self.engine()
        fake = self.screen()
        fake.elements[:] = [field("button", "Archive")]
        self.asked.clear()

        skill_task.perform(f"/skill {name}")

        self.assertEqual(ask.calls, [])
        question = self.asked[-1][0]
        self.assertIn('Closest on the screen: 1) button "Archive"', question)
        self.assertIn("Reply the number", question)
        self.assertIn("I tried:", question)

        skill_task.resume("1")
        self.confirm_done()

        self.assertEqual(self.finals[-1][1], "final", self.finals[-1][0])
        learned = skills.load(name)["steps"][0]
        self.assertEqual(learned["target"]["name"], "Archive")
        # The name it had is remembered too, in case the screen goes back.
        self.assertEqual(learned["alternates"], [{"role": "button", "name": "Save"}])

    def test_a_number_that_was_not_offered_is_not_taken(self):
        state.backend_ready = False
        name = self.taught()
        fake = self.screen()
        fake.elements[:] = [field("button", "Archive")]

        skill_task.perform(f"/skill {name}")
        skill_task.resume("9")

        self.assertIn("I did not catch that", self.asked[-1][0])
        self.assertEqual(fake.acted, [])

    def test_a_run_that_stopped_is_remembered_as_a_step_that_needed_help_but_teaches_nothing(self):
        state.backend_ready = False
        name = self.taught()
        fake = self.screen()
        fake.elements[:] = [field("button", "Archive")]

        skill_task.perform(f"/skill {name}")
        skill_task.resume("stop")

        saved = skills.load(name)
        self.assertEqual(saved["steps"][0]["target"]["name"], "Save")
        self.assertEqual(saved["experience"]["runs"][-1]["outcome"], "stopped")
        self.assertEqual(saved["experience"]["steps"]["%s" % saved["steps"][0]["id"]]["needed"], [1])

    def test_an_approval_is_asked_with_what_the_step_does_and_never_bypassed(self):
        self.teach("skip", "yes", "changes", "yes")
        save = field("button", "Save")
        fake = self.screen(approval_on=save["path"])
        self.asked.clear()

        skill_task.perform('/skill add-a-customer customer_name="Initech"')
        skill_task.resume("done")                              # the password step, done by hand

        text, kind = self.asked[-1]
        self.assertEqual(kind, "approval")
        self.assertIn("This is step", text)
        self.assertIn("Reply `yes` to allow this one action", text)
        self.assertEqual(fake.acted[-1][0], "type")           # nothing ran past the gate

    def test_skills_report_how_they_are_holding_up(self):
        name = self.taught()
        shown = skills.summary(skills.load(name))
        self.assertEqual(shown["health"], {"fragile": [], "drifting": [], "recent": [],
                                           "stage": "apprentice", "clean_runs": 0, "trust_after": 3,
                                           "tokens": {"first": None, "now": None, "saved_percent": None}})


if __name__ == "__main__":
    unittest.main()
