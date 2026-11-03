"""
VERIFIED means the run's own values were seen after it saved, and nothing was re-aimed (review 2026-11-03, D-09 and
D-25). A run whose values the screen does not show after its committing step is complete, unverified - it goes to the
final judge. The cheap path stays cheap: a clean run that reads its values back is VERIFIED without the judge.
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent.computer import escalation, replay
from test_recordings import FakeScreen, field

PROGRAM = {"title": "Save a customer", "params": [
    {"name": "customer_name", "step": 1, "secret": False, "default": "Acme", "example": "Acme"}],
    "steps": [
        {"n": 1, "kind": "type", "param": "customer_name", "summary": "type customer",
         "target": {"role": "textbox", "name": "Customer", "path": "site x > textbox Customer"},
         "expect": [{"type": "value", "role": "textbox", "name": "Customer", "param": "customer_name"}]},
        {"n": 2, "kind": "click", "summary": "click Save", "target": {"role": "button", "name": "Save",
                                                                       "path": "site x > button Save"},
         "expect": [{"type": "element", "role": "alert", "name": "Saved"}]}]}


class ClearsOnSave(FakeScreen):
    """A page that goes to an empty list after Save: the record's values are no longer on it."""

    def act(self, kind, **kwargs):
        out = super().act(kind, **kwargs)
        if kind == "click":
            for element in self.elements:
                if element.get("role") == "textbox":
                    element["value"] = ""
        return {"status": "done", "text": "ok", "observation": self.observation()}


def screen(cls, shows):
    return cls([field("textbox", "Customer", path="site x > textbox Customer"),
                field("button", "Save", path="site x > button Save")],
               after={"adds": [field("alert", "Saved")] + ([field("status", shows)] if shows else [])})


class AReadBackAfterTheSave(unittest.TestCase):
    def run_on(self, fake, ladder=None):
        with mock.patch.object(replay, "core", fake), mock.patch.object(replay, "EXPECT_SECONDS", 0), \
                mock.patch.object(replay, "FIND_SECONDS", 0), mock.patch.object(replay, "POLL_SECONDS", 0):
            return replay.run(PROGRAM, params={"customer_name": "Vendor Two"}, report=lambda _: None, ladder=ladder)

    def test_a_value_the_screen_shows_after_the_save_is_verified(self):
        self.assertEqual(self.run_on(screen(ClearsOnSave, "Customer Vendor Two saved"))["outcome"], replay.VERIFIED)

    def test_a_value_not_seen_after_the_save_is_unverified(self):
        self.assertEqual(self.run_on(screen(ClearsOnSave, None))["outcome"], replay.COMPLETED_UNVERIFIED)

    def test_a_page_that_keeps_its_typed_values_after_the_save_is_not_a_read_back(self):
        # The textbox still holds what the replay typed: that says nothing of what was saved (review round 2).
        self.assertEqual(self.run_on(screen(FakeScreen, None))["outcome"], replay.COMPLETED_UNVERIFIED)

    def test_a_value_is_read_within_one_control_never_across_two(self):
        shown = {"elements": [{"role": "cell", "name": "Vendor"}, {"role": "cell", "name": "Two"}]}
        with mock.patch.object(replay, "core", mock.Mock(observe=mock.Mock(return_value=shown))):
            self.assertEqual(replay._not_read_back(PROGRAM, {"customer_name": "Vendor Two"}, {}), ["customer_name"])

    def test_with_no_committing_step_the_values_are_read_back_at_the_end(self):
        shown = {"elements": [{"role": "status", "name": "Vendor Two added"}]}
        with mock.patch.object(replay, "core", mock.Mock(observe=mock.Mock(return_value=shown))):
            self.assertEqual(replay._not_read_back(PROGRAM, {"customer_name": "Vendor Two"}, {}), [])
            self.assertEqual(replay._not_read_back(PROGRAM, {"customer_name": "Vendor Three"}, {}), ["customer_name"])

    def test_a_list_counts_as_not_read_back_and_a_file_by_its_name(self):
        shown = {"elements": [{"role": "link", "name": "report.pdf"}]}
        with mock.patch.object(replay, "core", mock.Mock(observe=mock.Mock(return_value=shown))):
            self.assertEqual(replay._not_read_back(PROGRAM, {"customer_name": ["a", "b"]}, {}), ["customer_name"])
            # A file by its name only when it is a file by structure - given to an upload (R-2: a slash alone made
            # "05/03/2026" read back as "2026").
            uploads = dict(PROGRAM, steps=[{"n": 1, "kind": "upload", "param": "customer_name"}])
            self.assertEqual(replay._not_read_back(uploads, {"customer_name": "/home/x/work/report.pdf"}, {}), [])
            self.assertEqual(replay._not_read_back(PROGRAM, {"customer_name": "/home/x/work/report.pdf"}, {}),
                             ["customer_name"])

    def test_a_run_with_a_repaired_step_is_never_verified(self):
        fake = screen(ClearsOnSave, "Customer Vendor Two saved")
        fake.elements[1] = field("button", "Save!", path="site x > button Save!")
        result = self.run_on(fake, ladder=escalation.Ladder())
        self.assertEqual(result["healed"], 1)
        self.assertEqual(result["outcome"], replay.COMPLETED_UNVERIFIED)


if __name__ == "__main__":
    unittest.main()
