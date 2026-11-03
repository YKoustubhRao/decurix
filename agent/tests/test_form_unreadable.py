"""
A form that could not be read is not a page without a form (review 2026-11-03, D-26): a button whose form the browser
could not be asked about waits for a person, because the checks made before a save could not be made.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import cdp, core
from decurix_agent.computer.errors import ComputerError


def control(role, name):
    return {"role": role, "name": name, "source": "cdp", "ref": "e1", "path": f"site x > {role} {name}",
            "group": "g", "window_title": "Site", "handle": {"backend_id": 7}}


class AFormThatCouldNotBeRead(unittest.TestCase):
    def setUp(self):
        p = mock.patch.object(cdp, "_page_for", side_effect=ComputerError("the page did not answer"))
        p.start()
        self.addCleanup(p.stop)

    def test_the_read_says_unreadable_when_asked_to(self):
        self.assertIs(cdp.submit_form_fields(control("button", "Continue"), min_fields=1, unreadable=cdp.UNREADABLE),
                      cdp.UNREADABLE)
        self.assertIsNone(cdp.submit_form_fields(control("button", "Continue"), min_fields=1))

    def test_a_button_whose_form_could_not_be_read_waits_for_a_person(self):
        held = core._commit_check("click", control("button", "Continue"), None, {"elements": []})
        self.assertIsInstance(held, core.AskFirst)
        self.assertIn("could not be read", held)

    def test_a_link_that_commits_nothing_goes_on(self):
        self.assertEqual(core._commit_check("click", control("link", "Help"), None, {"elements": []}), "")


if __name__ == "__main__":
    unittest.main()
