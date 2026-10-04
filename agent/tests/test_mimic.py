import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import skill_task, state
from decurix_agent.computer import mimic, recording, replay, skills


def _event(kind, **fields):
    base = {"actor": "human", "kind": kind, "outcome": "done", "target": "", "element": None, "point": None,
            "text": None, "keys": [], "window": "Koustubh - Chrome", "url": "", "screen": [1280, 720]}
    base.update(fields)
    return base


GOOGLE = [
    _event("click", point=[400, 88], target="400,88"),
    _event("type", text="koustubh is great"),
    _event("key", keys=["Return"], expect={"active_window": "koustubh is great - Google Search - Chrome",
                                           "windows_opened": [], "windows_closed": [], "appeared": [],
                                           "disappeared": [], "values": []}),
]

INVOICE = [
    _event("click", window="Billing - Chromium", target='link "Invoice 1042"',
           element={"role": "link", "name": "Invoice 1042", "path": "Billing/link[3]"}),
]


class _Recordings(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT, skills.ROOT
        recording.ROOT = Path(self.tmp.name) / "recordings"
        skills.ROOT = Path(self.tmp.name) / "skills"
        mimic._cache.clear()

    def tearDown(self):
        recording.ROOT, skills.ROOT = self.saved
        self.tmp.cleanup()
        mimic._cache.clear()

    def record(self, title, events, stamp="20260923T152346"):
        session = f"demo-{stamp}-{len(list(recording.ROOT.glob('*'))) if recording.ROOT.exists() else 0:06d}"
        recording.ensure(session, title=title)
        for event in events:
            recording.append(session, dict(event))
        recording.update(session, status="complete")
        return session


class TestAlign(unittest.TestCase):
    """What a request changes about a recorded task - or that it is another task."""

    TYPED = [("typed", "koustubh is great")]

    def spans(self, request, recorded='google "koustubh is great"', places=None):
        found = mimic.align(recorded, request, self.TYPED if places is None else places)
        return None if found is None else [(s["old"], s["new"], s["placed"]) for s in found["spans"]]

    def test_the_value_that_differs_is_what_changes(self):
        self.assertEqual(self.spans('google "sanmayee is great"'), [("koustubh", "sanmayee", True)])
        self.assertEqual(self.spans('google "the weather in pune"'),
                         [("koustubh is great", "the weather in pune", True)])

    def test_the_same_task_changes_nothing(self):
        self.assertEqual(self.spans("google koustubh is great"), [])
        self.assertEqual(self.spans('please google "koustubh is great"'), [])

    def test_a_word_of_the_task_itself_is_never_swapped(self):
        # "google" is not something the person typed: "bing" is another task,
        # whatever either word means.
        self.assertIsNone(self.spans('bing "koustubh is great"'))
        self.assertIsNone(self.spans("search google for sanmayee is great"))
        self.assertIsNone(self.spans("what is the weather"))

    def test_a_second_job_is_not_the_recording(self):
        self.assertIsNone(self.spans('google "koustubh is great" and email it to bob'))
        self.assertIsNone(self.spans('google "koustubh is great" then save the page'))

    def test_a_value_typed_in_another_form_needs_adapting(self):
        spans = self.spans("set the due date to 7 March", "set the due date to 5 March", [("typed", "05/03/2026")])
        self.assertEqual(spans, [("5", "7", False)])

    def test_a_word_that_is_not_a_value_and_not_recorded_is_another_task(self):
        self.assertIsNone(self.spans("set the start date to 5 March", "set the due date to 5 March",
                                     [("typed", "05/03/2026")]))

    def test_a_clicked_control_takes_the_change(self):
        self.assertEqual(self.spans("open invoice 1043", "open invoice 1042", [("control", "Invoice 1042")]),
                         [("1042", "1043", True)])

    def test_a_task_word_that_only_appears_on_the_pages_passed_is_still_the_task(self):
        # Found live: a search was recorded, and its result page has "search"
        # in its address and its controls - so no word of the ticket was left
        # to be the task's shape, and nothing matched, the ticket's own words
        # included.
        places = [("typed", "koustubh rao"),
                  ("control", "URL: https://www.google.com/search?q=koustubh+rao"),
                  ("control", "Koustubh Rao - Quant Developer LinkedIn")]
        recorded = 'search "koustubh rao"'

        self.assertEqual(self.spans('search "sundar pichai"', recorded, places), [("koustubh rao", "sundar pichai", True)])
        self.assertEqual(self.spans(recorded, recorded, places), [])
        # Still another task when the word that is the task's differs.
        self.assertIsNone(self.spans('find "sundar pichai"', recorded, places))
        self.assertIsNone(self.spans('search "sundar pichai" and email it to bob', recorded, places))

    def test_a_quoted_phrase_is_one_value_even_when_it_ends_in_the_recorded_word(self):
        # Found live: the new message ended in the word the recorded one was,
        # so "hello" matched "hello" and the rest of the message was read as
        # part of the person's name - the worker searched for
        # `akshit gupta "decurix auto say` and sent "hello".
        places = [("typed", "linkedin"), ("typed", "sarthak gawade"), ("typed", "hello"),
                  ("control", "Search messages"), ("control", "Send")]
        recorded = 'open linkedin and send sarthak gawade "hello"'

        self.assertEqual(self.spans('open linkedin and send akshit gupta "decurix auto say hello"', recorded, places),
                         [("sarthak gawade", "akshit gupta", True), ("hello", "decurix auto say hello", True)])
        self.assertEqual(self.spans('open linkedin and send akshit gupta "hello there, say hello"', recorded, places),
                         [("sarthak gawade", "akshit gupta", True), ("hello", "hello there, say hello", True)])
        # As before where nothing runs across a quote.
        self.assertEqual(self.spans('open linkedin and send alex doe "hello from decurix"', recorded, places),
                         [("sarthak gawade", "alex doe", True), ("hello", "hello from decurix", True)])
        self.assertEqual(self.spans('open linkedin and send alex doe "hello"', recorded, places),
                         [("sarthak gawade", "alex doe", True)])


class TestPlan(_Recordings):
    def test_a_recorded_ticket_is_mimicked_without_being_taught(self):
        session = self.record('google "koustubh is great"', GOOGLE)
        found = mimic.plan('google "sanmayee is great"')
        self.assertEqual((found["kind"], found["id"]), ("recording", session))
        self.assertIsNone(mimic.plan('bing "sanmayee is great"'))
        self.assertIsNone(mimic.plan("/skill anything"))

    def test_a_renamed_recording_is_matched_by_the_task_it_was_recorded_for(self):
        session = self.record('google "koustubh is great"', GOOGLE)
        recording.update(session, title="Google a phrase")
        self.assertIsNotNone(mimic.plan('google "sanmayee is great"'))

    def test_a_taught_skill_wins_over_the_recording_it_came_from(self):
        session = self.record('google "koustubh is great"', GOOGLE)
        skill, _ = mimic.load_skill({"kind": "recording", "id": session, "text": 'google "koustubh is great"'})
        skills.save(skill)
        found = mimic.plan('google "sanmayee is great"')
        self.assertEqual((found["kind"], found["id"]), ("skill", skill["name"]))

    def test_a_draft_skill_is_never_run(self):
        session = self.record('google "koustubh is great"', GOOGLE)
        skill, _ = mimic.load_skill({"kind": "recording", "id": session, "text": 'google "koustubh is great"'})
        skill["status"] = "draft"
        skills.save(skill)
        self.assertEqual(mimic.plan('google "sanmayee is great"')["kind"], "recording")


class TestRun(_Recordings):
    """The recording's own steps, with only what the request changes put in."""

    def run_request(self, request, model=None, backend=True):
        seen = {}

        def fake_run(program, params=None, **kwargs):
            seen["program"], seen["params"] = program, dict(params or {})
            return {"outcome": replay.VERIFIED, "text": "Replayed.", "step": None, "resume_from": None,
                    "options": [], "trail": []}

        with patch.object(replay, "run", side_effect=fake_run), \
             patch.object(skill_task, "_ask_model", side_effect=model or (lambda s, p: (_ for _ in ()).throw(RuntimeError()))), \
             patch.object(skill_task, "write_final_reply", side_effect=lambda t, **k: seen.setdefault("final", t)), \
             patch.object(skill_task, "ask_for_query", side_effect=lambda t, **k: seen.setdefault("asked", t)), \
             patch.object(skill_task.progress, "emit", side_effect=lambda t: seen.setdefault("said", []).append(t)), \
             patch.object(skill_task, "_engine", side_effect=lambda c, f: seen.setdefault("engine", c)), \
             patch.object(state, "backend_ready", backend):
            skill_task.perform_mimic(request, None)
        state.current_task = None
        state.paused_skill = None
        return seen

    def test_the_search_types_the_new_words_and_expects_their_window(self):
        self.record('google "koustubh is great"', GOOGLE)
        seen = self.run_request('google "Sanmayee is great"')

        self.assertEqual(list(seen["params"].values()), ["Sanmayee is great"])
        steps = seen["program"]["steps"]
        # The window every step was recorded in keeps its name: "Koustubh" there
        # is a browser profile, not the search.
        self.assertTrue(all(step["window"] == "Koustubh - Chrome" for step in steps))
        checks = [c for step in steps for c in step.get("expect") or []]
        self.assertIn({"type": "active_window", "value": "Sanmayee is great - Google Search - Chrome"}, checks)
        self.assertIn('typing "Sanmayee is great" where you typed "koustubh is great"', " ".join(seen["said"]))
        self.assertIn("Done from your recording", seen["final"])
        self.assertEqual(list(skills.ROOT.glob("*.json")) if skills.ROOT.exists() else [], [])

    def test_a_clicked_control_is_the_one_the_request_names(self):
        self.record("open invoice 1042", INVOICE)
        seen = self.run_request("open invoice 1043")
        target = seen["program"]["steps"][0]["target"]
        self.assertEqual(target["name"], "Invoice 1043")
        self.assertNotIn("path", target)

    def test_a_value_typed_in_another_form_is_asked_of_the_model_only(self):
        self.record("set the due date to 5 March", [_event("type", text="05/03/2026")])
        asked = []

        def model(system, prompt):
            asked.append(prompt)
            return "07/03/2026"

        seen = self.run_request("set the due date to 7 March", model=model)
        self.assertEqual(len(asked), 1)
        self.assertIn("In it the person typed: 05/03/2026", asked[0])
        self.assertEqual(list(seen["params"].values()), ["07/03/2026"])

    def test_without_a_model_the_person_is_asked(self):
        self.record("set the due date to 5 March", [_event("type", text="05/03/2026")])
        seen = self.run_request("set the due date to 7 March", backend=False)
        self.assertNotIn("program", seen)
        self.assertIn('recorded as "05/03/2026"', seen["asked"])

    def test_a_model_that_would_change_nothing_hands_it_to_the_engine(self):
        self.record("open the due date in Chrome", [_event("type", text="due date")])
        seen = self.run_request("open the due date in Firefox", model=lambda s, p: "due date")
        self.assertEqual(seen.get("engine"), "open the due date in Firefox")
        self.assertNotIn("program", seen)

    def test_a_request_that_drops_the_value_asks_for_it(self):
        self.record('google "koustubh is great"', GOOGLE)
        seen = self.run_request("google")
        self.assertNotIn("program", seen)
        self.assertIn('recorded as "koustubh is great"', seen["asked"])

    def test_the_same_request_replays_as_recorded(self):
        self.record('google "koustubh is great"', GOOGLE)
        seen = self.run_request("google koustubh is great")
        self.assertEqual(list(seen["params"].values()), ["koustubh is great"])


class TestCleanAnswer(unittest.TestCase):
    def test_a_models_reply_is_read_as_a_value(self):
        self.assertEqual(mimic._clean_answer('"07/03/2026"\nBecause...', "05/03/2026"), "07/03/2026")
        self.assertEqual(mimic._clean_answer("Answer: 07/03/2026", "05/03/2026"), "07/03/2026")
        self.assertIsNone(mimic._clean_answer("", "x"))
        self.assertIsNone(mimic._clean_answer("x" * 500, "05/03/2026"))


if __name__ == "__main__":
    unittest.main()
