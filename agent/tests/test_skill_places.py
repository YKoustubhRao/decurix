"""A taught skill says which system it works in, so an engine does not run
it for the same kind of work somewhere else (found live: a skill taught in a
local sales app entered a project request there)."""

import unittest

from decurix_agent.computer import skill_engine, skills

SKILL = {
    "name": "enter-a-customer-po", "title": "Enter a customer PO", "status": "ready", "params": [],
    "steps": [
        {"n": 1, "id": "s1", "kind": "open", "open": "file:///home/headless/work/sales.html", "window": "", "summary": "open sales.html"},
        {"n": 2, "id": "s2", "kind": "click", "window": "Sales - Chromium", "summary": "click New"},
        {"n": 3, "id": "s3", "kind": "click", "window": "Sales - Google Chrome", "summary": "click Save"},
        {"n": 4, "id": "s4", "kind": "open", "open": "https://portal.example.com/requests?x=1", "window": "Project Portal - Chromium", "summary": "open portal"},
    ],
}


class WhereASkillWorks(unittest.TestCase):
    def test_places_are_the_sites_files_and_windows_it_uses(self):
        self.assertEqual(skills.places(SKILL), ["sales.html", "Sales", "portal.example.com"])

    def test_the_engine_is_told_where_it_works(self):
        self.assertIn("works in sales.html, Sales", skill_engine.summary_line(SKILL))
        self.assertIn("Works in: sales.html, Sales", skill_engine.render_text(SKILL))

    def test_a_skill_recorded_on_a_site_already_open_names_its_address(self):
        # Signed in before recording: no step opens the site, but the steps were on it.
        steps = [{"n": 1, "id": "s1", "kind": "click", "window": "", "summary": "click 99,416"},
                 {"n": 2, "id": "s2", "kind": "click", "window": "New Request - Chromium",
                  "url": "http://host.docker.internal:8000/requests/new-1", "summary": "click Save"}]
        found = skills.places(dict(SKILL, steps=steps))
        self.assertEqual(found[0], "http://host.docker.internal:8000")
        self.assertIn("New Request", found)

    def test_a_skill_with_no_place_says_nothing(self):
        bare = dict(SKILL, steps=[{"n": 1, "id": "s1", "kind": "key", "window": "", "summary": "press Enter"}])
        self.assertEqual(skills.places(bare), [])
        self.assertNotIn("works in", skill_engine.summary_line(bare))


if __name__ == "__main__":
    unittest.main()


from unittest import mock  # noqa: E402

from decurix_agent import llm_client  # noqa: E402


def answering(same, system=""):
    reply = {"content": '{"same_system": %s, "requested_system": "%s"}' % ("true" if same else "false", system)}
    return mock.patch.object(llm_client, "chat", return_value=(reply, {}))


class ASkillIsNotRunInAnotherSystem(unittest.TestCase):
    PORTAL = ("The source document is /w/request.pdf. Create an editable request in the project portal as a DRAFT "
            "record - it is Project Portal, already open in the browser.")

    def setUp(self):
        p = mock.patch.object(llm_client, "available", return_value=True)
        p.start()
        self.addCleanup(p.stop)

    def test_a_request_for_another_system_is_refused_before_anything_runs(self):
        skill = dict(SKILL, status=skills.READY)
        with answering(False, "Project Portal"), mock.patch.object(skill_engine.task_context, "current_request",
                                                         return_value=self.PORTAL), \
                mock.patch.object(skill_engine.replay, "run") as ran:
            result = skill_engine.run(skill, {})
        ran.assert_not_called()
        self.assertEqual(result["outcome"], "halted_before_effect")
        self.assertIn("for Project Portal", result["text"])
        self.assertIn("sales.html", result["text"])

    def test_the_same_system_or_no_model_asks_nothing_of_it(self):
        with answering(True):
            self.assertEqual(skill_engine.other_system(SKILL, "Enter this PO in the sales app"), "")
        with mock.patch.object(llm_client, "available", return_value=False), \
                mock.patch.object(llm_client, "chat") as chat:
            self.assertEqual(skill_engine.other_system(SKILL, self.PORTAL), "")
        chat.assert_not_called()

    def test_a_model_that_fails_does_not_stop_the_skill(self):
        with mock.patch.object(llm_client, "chat", side_effect=llm_client.LLMError("down")):
            self.assertEqual(skill_engine.other_system(SKILL, self.PORTAL), "")
