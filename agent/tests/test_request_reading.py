import unittest
from unittest.mock import patch

from decurix_agent import request_reading, skill_task, state
from decurix_agent.computer import replay, skills

from tests.test_mimic import GOOGLE, INVOICE, _event, _Recordings

LINKEDIN = [
    _event("type", text="linkedin", element={"role": "combobox", "name": "Ask Google"}),
    _event("type", text="sarthak gawade", element={"role": "textbox", "name": "Search messages"}),
    _event("type", text="hello", element={"role": "textbox", "name": "Write a message"}),
    _event("click", element={"role": "button", "name": "Send", "path": "x/button[1]"}, target='button "Send"'),
]
TICKET = 'open linkedin and send sarthak gawade "hello"'


class _Reading(_Recordings):
    """Requests read by a hosted model that answers what `self.says` holds."""

    def setUp(self):
        super().setUp()
        self.says, self.shown = {}, []

        def call(system, content, max_tokens=0):
            self.shown.append(content)
            if isinstance(self.says, Exception):
                raise self.says
            return self.says

        for p in (patch.object(request_reading.llm_client, "available", lambda: True),
                  patch.object(request_reading.study, "_call", call)):
            p.start()
            self.addCleanup(p.stop)

    def names(self, session):
        """The built skill's input names, in the order typed."""

        skill, _ = request_reading.mimic.load_skill({"kind": "recording", "id": session, "text": "", "spans": []})
        return [p["name"] for p in skill["params"]]


class TestRead(_Reading):
    def test_the_model_says_which_task_and_what_to_type(self):
        # Found live: compared word by word, this request was split at the
        # shared word "hello" - the search became `akshit gupta "decurix auto
        # say` and the message "hello".
        session = self.record(TICKET, LINKEDIN)
        site, who, message = self.names(session)
        request = 'open linkedin and send akshit gupta "decurix auto say hello"'
        self.says = {"task": session, "confidence": 0.97, "why": "same task, other person and message",
                     "values": {site: "linkedin", who: "akshit gupta", message: "decurix auto say hello"}}

        read = request_reading.read(request)

        self.assertEqual(read["plan"]["id"], session)
        self.assertEqual(read["values"], {site: "linkedin", who: "akshit gupta", message: "decurix auto say hello"})
        self.assertEqual(read["dropped"], [])
        # The model was shown the task, and each input with the demonstration's value as an example.
        self.assertIn(TICKET, self.shown[0])
        self.assertIn('The demonstration typed: "sarthak gawade"', self.shown[0])
        self.assertIn(request, self.shown[0])

    def test_a_value_that_is_not_in_the_request_is_a_guess_and_is_dropped(self):
        session = self.record(TICKET, LINKEDIN)
        site, who, message = self.names(session)
        self.says = {"task": session, "confidence": 0.9,
                     "values": {site: "linkedin", who: "akshit gupta", message: "Hi Akshit, hope you are well!"}}

        read = request_reading.read("open linkedin and send akshit gupta a greeting")

        self.assertEqual(read["values"], {site: "linkedin", who: "akshit gupta"})
        self.assertEqual(read["dropped"], [message])

    def test_the_demonstrations_own_value_is_never_reused_for_what_the_request_left_out(self):
        session = self.record(TICKET, LINKEDIN)
        site, who, message = self.names(session)
        for said in ("hello", None):
            with self.subTest(said):
                self.says = {"task": session, "confidence": 0.9, "values": {site: "linkedin", who: "akshit gupta", message: said}}
                read = request_reading.read("open linkedin and message akshit gupta")
                self.assertNotIn(message, read["values"])

    def test_a_value_only_an_earlier_task_holds_is_shown_to_the_model_but_asked(self):
        # Review 2026-11-03 (E-28): the lessons are the whole worker's - another client's included - so a value only
        # they hold is not grounded for this request; the person is asked.
        session = self.record(TICKET, LINKEDIN)
        site, who, message = self.names(session)
        self.says = {"task": session, "confidence": 0.9, "values": {site: "linkedin", who: "akshit gupta", message: "On my way"}}

        with patch.object(request_reading.lessons, "known", lambda: '- When no message is given, send "On my way".'):
            read = request_reading.read("open linkedin and message akshit gupta")

        self.assertNotIn(message, read["values"])
        self.assertIn(message, read["dropped"])
        self.assertIn("The person has said before:", self.shown[0])

    def test_a_value_is_in_the_request_only_as_whole_words(self):
        session = self.record(TICKET, LINKEDIN)
        site, who, message = self.names(session)
        self.says = {"task": session, "confidence": 0.9, "values": {site: "linkedin", who: "ram", message: "500"}}
        read = request_reading.read("open linkedin and send ramesh a note about the 5000 due")
        self.assertNotIn(who, read["values"])
        self.assertNotIn(message, read["values"])

    def test_a_value_typed_in_another_form_may_be_adapted(self):
        session = self.record("set the due date to 5 March", [_event("type", text="05/03/2026")])
        (date,) = self.names(session)
        self.says = {"task": session, "confidence": 0.9, "values": {date: "07/03/2026"}}

        read = request_reading.read("set the due date to 7 March")
        self.assertEqual(read["values"], {date: "07/03/2026"})

    def test_no_task_an_unknown_task_or_a_weak_match_is_the_engines(self):
        session = self.record(TICKET, LINKEDIN)
        for says in ({"task": None, "confidence": 0.9}, {"task": "something-else", "confidence": 0.9},
                     {"task": session, "confidence": 0.4, "values": {}}):
            with self.subTest(says):
                self.says = says
                self.assertIsNone(request_reading.read("post an update on linkedin")["plan"])

    def test_a_model_that_cannot_be_asked_decides_nothing(self):
        self.record(TICKET, LINKEDIN)
        self.says = ValueError("not JSON")
        self.assertIsNone(request_reading.read("open linkedin and send akshit gupta \"hi\""))

    def test_nothing_taught_asks_no_model(self):
        self.assertIsNone(request_reading.read("open linkedin"))
        self.assertEqual(self.shown, [])

    def test_only_a_request_that_shares_a_word_with_something_taught_is_worth_reading(self):
        self.record(TICKET, LINKEDIN)
        self.assertTrue(request_reading.worth_reading("message akshit gupta on linkedin saying hi"))
        self.assertFalse(request_reading.worth_reading("what is the kernel version"))
        self.assertFalse(request_reading.worth_reading("/skill linkedin"))


class TestRun(_Reading):
    """A request the model read, carried out."""

    def run_request(self, request):
        seen = {}

        def fake_run(program, params=None, **kwargs):
            seen["program"], seen["params"] = program, dict(params or {})
            return {"outcome": replay.VERIFIED, "text": "Replayed.", "step": None, "resume_from": None,
                    "options": [], "trail": []}

        with patch.object(replay, "run", side_effect=fake_run), \
             patch.object(skill_task, "write_final_reply", side_effect=lambda t, **k: seen.setdefault("final", t)), \
             patch.object(skill_task, "ask_for_query", side_effect=lambda t, **k: seen.setdefault("asked", t)), \
             patch.object(skill_task.progress, "emit", side_effect=lambda t: seen.setdefault("said", []).append(t)), \
             patch.object(skill_task, "_engine", side_effect=lambda c, f: seen.setdefault("engine", c)), \
             patch.object(skill_task, "_engine_can_drive", lambda: False), \
             patch.object(skill_task.verify, "available", lambda: False), \
             patch.object(skill_task.binding, "available", lambda: False), \
             patch.object(state, "backend_ready", True):
            skill_task.perform_mimic(request, None)
        state.current_task = None
        state.paused_skill = None
        return seen

    def test_the_quoted_message_is_typed_whole_and_the_name_is_the_name(self):
        session = self.record(TICKET, LINKEDIN)
        site, who, message = self.names(session)
        self.says = {"task": session, "confidence": 0.97,
                     "values": {site: "linkedin", who: "akshit gupta", message: "decurix auto say hello"}}

        seen = self.run_request('open linkedin and send akshit gupta "decurix auto say hello"')

        self.assertEqual(seen["params"], {site: "linkedin", who: "akshit gupta", message: "decurix auto say hello"})
        self.assertIn('typing "akshit gupta" where you typed "sarthak gawade"', " ".join(seen["said"]))

    def test_a_task_worded_another_way_is_still_done_as_taught(self):
        session = self.record(TICKET, LINKEDIN)
        site, who, message = self.names(session)
        self.says = {"task": session, "confidence": 0.9,
                     "values": {site: "linkedin", who: "Akshit Gupta", message: "see you at 5"}}

        seen = self.run_request('message Akshit Gupta on linkedin: "see you at 5"')

        self.assertEqual(seen["params"][who], "Akshit Gupta")
        self.assertEqual(seen["params"][message], "see you at 5")

    def test_a_value_the_request_does_not_give_is_asked_for_not_guessed(self):
        session = self.record(TICKET, LINKEDIN)
        site, who, message = self.names(session)
        self.says = {"task": session, "confidence": 0.9,
                     "values": {site: "linkedin", who: "akshit gupta", message: "Hello Akshit, how are you?"}}

        seen = self.run_request("open linkedin and send akshit gupta a greeting")

        self.assertNotIn("program", seen)
        self.assertIn("ask rather than guess", " ".join(seen["said"]))
        self.assertIn('recorded as "hello"', seen["asked"])

    def test_what_the_model_calls_another_task_goes_to_the_engine(self):
        self.record(TICKET, LINKEDIN)
        self.says = {"task": None, "confidence": 0.95, "why": "asks for a post, not a message"}

        seen = self.run_request("open linkedin and post an update")

        self.assertEqual(seen.get("engine"), "open linkedin and post an update")
        self.assertNotIn("program", seen)

    def test_a_clicked_control_still_takes_the_requests_word(self):
        session = self.record("open invoice 1042", INVOICE)
        self.says = {"task": session, "confidence": 0.9, "values": {}}

        seen = self.run_request("open invoice 1043")

        self.assertEqual(seen["program"]["steps"][0]["target"]["name"], "Invoice 1043")

    def test_a_model_that_fails_falls_back_to_the_recordings_own_words(self):
        self.record('google "koustubh is great"', GOOGLE)
        self.says = ValueError("the model did not answer in JSON")

        seen = self.run_request('google "Sanmayee is great"')

        self.assertEqual(list(seen["params"].values()), ["Sanmayee is great"])

    def test_a_taught_skill_is_not_changed_by_a_reading(self):
        session = self.record(TICKET, LINKEDIN)
        skill, _ = request_reading.mimic.load_skill({"kind": "recording", "id": session, "text": TICKET, "spans": []})
        skill.update(name="send-a-message", title="Send a message", goal=TICKET, sources=[session])
        for param in skill["params"][1:]:
            param["varies"] = True
        skills.save(skill)
        before = skills.load("send-a-message")
        site, who, message = [p["name"] for p in skill["params"]]
        self.says = {"task": "send-a-message", "confidence": 0.9,
                     "values": {site: "linkedin", who: "akshit gupta", message: "decurix auto say hello"}}

        seen = self.run_request('open linkedin and send akshit gupta "decurix auto say hello"')

        self.assertEqual(seen["params"][message], "decurix auto say hello")
        # The recording it was taught from is not offered beside it.
        self.assertNotIn(f"Task id: {session}", self.shown[0])
        after = skills.load("send-a-message")
        self.assertEqual(after["params"], before["params"])
        self.assertEqual(after["steps"], before["steps"])


if __name__ == "__main__":
    unittest.main()
