"""
A replayed step acts only in the window, document and on the party it was recorded for (review 2026-11-03):

- D-10: a step recorded in one window never takes the one same-named control of another window.
- D-17: two windows of the same kind are not chosen between, and a step recorded in one document's window is not done
  in another document's window of that kind - unless the document is a value the run changes.
- D-20: a list entry whose recorded name a run's value covers only in part is found by the whole value, once, never by
  splicing the value into the recorded name.
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent.computer import launchers, mimic, replay


def control(name, title, role="button", path=None):
    return {"role": role, "name": name, "path": path or f"{title} > {role} {name}", "window_title": title,
            "group": "g", "states": []}


class TheWindowComesFirst(unittest.TestCase):
    def test_a_step_from_one_window_does_not_act_in_another(self):
        step = {"n": 3, "kind": "click", "target": {"role": "button", "name": "Save", "path": "doc > button Save",
                                                    "window_title": "Sales Invoice - Browser"}}
        obs = {"active_title": "Payroll Run - Browser", "elements": [control("Save", "Payroll Run - Browser")]}
        element, _, why = replay.resolve(step, obs)
        self.assertIsNone(element)
        self.assertIn("not on the screen", why)

    def test_the_same_window_is_still_found(self):
        step = {"n": 3, "kind": "click", "target": {"role": "button", "name": "Save", "path": "doc > button Save",
                                                    "window_title": "Sales Invoice - Browser"}}
        obs = {"active_title": "Sales Invoice - Browser", "elements": [control("Save", "Sales Invoice - Browser")]}
        self.assertIsNotNone(replay.resolve(step, obs)[0])


class TheDocumentComesFirst(unittest.TestCase):
    def test_two_windows_of_one_kind_are_not_chosen_between(self):
        windows = [{"id": "1", "title": "DOC-50 - Sales Invoice"}, {"id": "2", "title": "DOC-43 - Sales Invoice"}]
        with mock.patch.object(launchers.screen, "windows", return_value=windows), \
                mock.patch.object(launchers.xinput, "activate") as activate:
            self.assertIsNone(launchers.bring_forward("Sales Invoice", replay.title_matches))
        activate.assert_not_called()

    def test_a_step_recorded_in_one_documents_window_is_not_done_in_anothers(self):
        # The document was there when the recording began (its first step's window): it is that document's step.
        known = replay._known_documents([{"n": 1, "kind": "click", "window": "DOC-43 - Sales Invoice"}])
        step = replay._window_ids({"n": 2, "kind": "click", "point": [5, 5], "window": "DOC-43 - Sales Invoice"}, [],
                                  known)
        _, point, why = replay.resolve(step, {"active_title": "DOC-50 - Sales Invoice", "elements": []})
        self.assertIsNone(point)
        self.assertIn("not in front", why)
        _, point, _ = replay.resolve(step, {"active_title": "DOC-43 - Sales Invoice", "elements": []})
        self.assertEqual(point, [5, 5])

    def test_a_number_the_app_gave_after_a_save_is_not_pinned(self):
        # Save, then Submit: the document's number first shows after the save, and the next run gets the next one.
        steps = [{"n": 1, "kind": "click", "window": "New Sales Invoice - Browser",
                  "target": {"role": "button", "name": "Save"}},
                 {"n": 2, "kind": "click", "window": "DOC-42 - Sales Invoice - Browser",
                  "target": {"role": "button", "name": "Submit", "window_title": "DOC-42 - Sales Invoice - Browser"}}]
        step = replay._window_ids(steps[1], [], replay._known_documents(steps))
        self.assertNotIn("window_ids", step)
        obs = {"active_title": "DOC-43 - Sales Invoice - Browser", "elements": [
            control("Submit", "DOC-43 - Sales Invoice - Browser")]}
        self.assertIsNotNone(replay.resolve(step, obs)[0])

    def test_a_new_documents_own_name_is_not_pinned(self):
        steps = [{"n": 1, "kind": "key", "keys": ["ctrl+n"], "window": "Start - Sheets"},
                 {"n": 2, "kind": "click", "point": [5, 5], "window": "Untitled 2 - Sheets"}]
        self.assertNotIn("window_ids", replay._window_ids(steps[1], [], replay._known_documents(steps)))

    def test_a_document_opened_by_name_before_the_save_is_pinned(self):
        steps = [{"n": 1, "kind": "click", "window": "List - Browser", "target": {"role": "link", "name": "DOC-43"}},
                 {"n": 2, "kind": "click", "point": [5, 5], "window": "DOC-43 - Sales Invoice"}]
        self.assertEqual(replay._window_ids(steps[1], [], replay._known_documents(steps))["window_ids"], ["doc-43"])

    def test_a_document_that_is_the_runs_own_value_keeps_the_stable_check(self):
        step = replay._window_ids({"n": 2, "kind": "click", "point": [5, 5], "window": "DOC-43 - Sales Invoice"},
                                  ["DOC-43"])
        self.assertNotIn("window_ids", step)


class TheWindowFollowsTheRunsValues(unittest.TestCase):
    def test_a_window_titled_by_the_recorded_value_is_the_runs_value_window(self):
        params = [{"name": "client", "step": 1, "example": "Client One"}]
        steps = [{"n": 3, "kind": "click", "window": "Client One - Customer - Browser",
                  "target": {"role": "button", "name": "Save", "window_title": "Client One - Customer - Browser",
                             "path": "x > button Save"}}]
        (step,) = replay._titles_follow_values(steps, params, {"client": "Client Two"})
        self.assertEqual(step["target"]["window_title"], "Client Two - Customer - Browser")
        obs = {"active_title": "Client Two - Customer - Browser",
               "elements": [control("Save", "Client Two - Customer - Browser", path="y > button Save")]}
        self.assertIsNotNone(replay.resolve(step, obs)[0])

    def test_a_party_entry_keeps_the_substituted_window_title(self):
        spans = [{"old": "First", "new": "Other", "find": ["first"], "placed": True, "value": True}]
        step = {"kind": "click", "target": {"role": "listitem", "name": "First Last", "path": "x",
                                            "window_title": "First - Chat"}, "summary": "s"}
        target = mimic.substitute_step(step, spans)["target"]
        self.assertEqual(target["window_title"], "Other - Chat")


class APartyIsFoundByItsWholeValue(unittest.TestCase):
    SPANS = [{"old": "First", "new": "Other", "find": ["first"], "placed": True, "value": True}]

    def test_a_value_covering_part_of_an_entry_is_not_spliced_into_it(self):
        step = {"kind": "click", "target": {"role": "listitem", "name": "First Last", "path": "x"}, "summary": "s"}
        target = mimic.substitute_step(step, self.SPANS)["target"]
        self.assertNotEqual(target["name"], "Other Last")
        self.assertTrue(target["contains"])
        self.assertEqual(target["value"], "Other")

    def test_the_entry_holding_the_whole_value_once_is_taken_and_two_are_not_chosen_between(self):
        step = {"n": 1, "kind": "click", "target": {"role": "listitem", "name": "Other", "contains": True,
                                                    "value": "Other Person"}}
        one = {"active_title": "x", "elements": [control("Other Person Two", "x", role="listitem"),
                                                 control("Other Personnel", "x", role="listitem")]}
        self.assertEqual(replay.resolve(step, one)[0]["name"], "Other Person Two")
        two = {"active_title": "x", "elements": [control("Other Person Two", "x", role="listitem"),
                                                 control("Other Person Three", "x", role="listitem")]}
        element, _, why = replay.resolve(step, two)
        self.assertIsNone(element)
        self.assertIn("does not guess", why)


if __name__ == "__main__":
    unittest.main()
