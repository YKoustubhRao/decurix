"""
An entry the person themselves named for a value is not "another thing in its
place": what they taught outranks the guard against substitutes.
"""

import unittest
from unittest import mock

from decurix_agent import guidance
from decurix_agent.computer import core

LESSON = ("Yes. The number and the dates come from the form. A van, a truck and a lorry are all the kind Goods "
          "Vehicle; a coach is Bus and its plate is the reference. Only save - never submit.")


class NamedByThePerson(unittest.TestCase):
    def said(self, wanted, entry, words=LESSON):
        with mock.patch.object(guidance, "persons_words", lambda: words):
            return core._person_said(wanted, entry)

    def test_the_entry_they_named_for_it_is_theirs_to_choose(self):
        self.assertTrue(self.said("lorry", "Goods Vehicle"))
        self.assertTrue(self.said("truck (goods carrier)", "Goods Vehicle"), "the document's wording may carry more words")

    def test_only_in_one_sentence(self):
        # "Bus" and "lorry" are both in the lesson, in different sentences of it.
        self.assertFalse(self.said("lorry", "Bus"))

    def test_an_entry_nobody_named_is_still_a_substitute(self):
        self.assertFalse(self.said("lorry", "Passenger Car"))
        self.assertFalse(self.said("Zenith Stationers", "Apex Office Supplies"))

    def test_a_list_shown_as_name_then_description(self):
        self.assertTrue(self.said("lorry", "Goods Vehicle Goods Vehicle, Commercial"))

    def test_nothing_said_nothing_allowed(self):
        self.assertFalse(self.said("lorry", "Goods Vehicle", words=""))

    def test_a_rule_kept_as_data_counts(self):
        rules = '{"rules": ["Use the form date for both dates", "Map van, truck, and lorry to the kind \'Goods Vehicle\'"]}'
        self.assertTrue(self.said("lorry", "Goods Vehicle", words=rules))
        self.assertFalse(self.said("tractor", "Goods Vehicle", words=rules))

    def test_what_the_guard_would_refuse_is_let_through_once_named(self):
        kept = {"at": 10 ** 12, "task": "", "field": "Kind", "role": "combobox", "wanted": "lorry"}
        element = {"name": "Kind", "role": "combobox"}
        with mock.patch.object(core.FAILED_SEARCH_FILE.__class__, "read_text", lambda self, encoding=None: __import__("json").dumps(kept)), \
                mock.patch.object(core.time, "time", lambda: 10 ** 12), \
                mock.patch.object(core, "_task_now", lambda: ""):
            with mock.patch.object(guidance, "persons_words", lambda: LESSON):
                self.assertIsNone(core._in_place_of(element, "Goods Vehicle"))
            with mock.patch.object(guidance, "persons_words", lambda: ""):
                self.assertEqual(core._in_place_of(element, "Goods Vehicle"), "lorry")


class TheirWordsStayTheirs(unittest.TestCase):
    """This run's values are put into the lesson's text where the demonstration's stood - never into other words."""

    SKILL = {
        "params": [{"name": "kind", "example": "Goods"}, {"name": "record", "example": "REC-3"},
                   {"name": "amount", "example": "120"}],
        "steps": [{"n": 1, "kind": "type", "param": "kind", "target": {"role": "combobox", "name": "Kind"}},
                  {"n": 2, "kind": "click", "target": {"role": "option", "name": "Goods Vehicle"}},
                  {"n": 3, "kind": "type", "param": "record", "target": {"role": "textbox", "name": "ID"}},
                  {"n": 4, "kind": "type", "param": "amount", "target": {"role": "textbox", "name": "Amount"}}],
    }

    def rewrite(self, text, **values):
        from decurix_agent import skill_task
        return skill_task._for_this_run(text, self.SKILL, values)

    def test_letters_typed_to_search_a_list_are_not_replaced_in_what_was_said(self):
        said = "A van, a truck and a lorry are all the kind Goods Vehicle. Open REC-3."
        self.assertEqual(self.rewrite(said, kind="lorry", record="REC-9"),
                         "A van, a truck and a lorry are all the kind Goods Vehicle. Open REC-9.")

    def test_a_value_is_replaced_as_a_whole_word_only(self):
        self.assertEqual(self.rewrite("Enter 120 against REC-3; lot 1200 is another matter.", amount="75", record="REC-9"),
                         "Enter 75 against REC-9; lot 1200 is another matter.")

    def test_an_input_written_in_braces_takes_this_runs_value(self):
        self.assertEqual(self.rewrite("Kind set to the option matching {kind}", kind="lorry"),
                         "Kind set to the option matching lorry")


if __name__ == "__main__":
    unittest.main()
