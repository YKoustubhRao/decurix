"""
A value of this run that already stands in a recorded name leaves it as it is, and the demonstration's value is never
avoided when this run's value holds it; a check learned with a broken placeholder is dropped, and learning never puts a
placeholder inside a word or another placeholder. Found in the learning curves (2026-11-16): "Sharavati" typed in the
demonstration and "Sharavati Builders" this run - the chat's link "Ravi Kamath (Sharavati Builders)" became "...
(Sharavati Builders Builders)", the search result "Sharavati Builders" was avoided, and a unit "NA" inside
"{client_name}" made "{client_{uqc}me}" - so every run stopped at its first part.
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent.computer import learning, mimic, replay  # noqa: E402

SKILL = {"params": [{"name": "client_name", "example": "Sharavati", "step": 10},
                    {"name": "uqc", "example": "NA", "step": 30}]}


class AValueAlreadyThere(unittest.TestCase):
    def setUp(self):
        self.spans = mimic.value_spans(SKILL, {"client_name": "Sharavati Builders"})

    def test_a_name_holding_this_runs_value_is_left_as_it_is(self):
        step = {"n": 4, "kind": "click", "target": {"role": "link", "name": "Ravi Kamath (Sharavati Builders)"}}
        out = mimic.substitute_step(step, self.spans)
        self.assertEqual(out["target"]["name"], "Ravi Kamath (Sharavati Builders)")
        self.assertNotIn("avoid", out["target"])

    def test_the_words_before_the_value_stay(self):
        # "Open TaxNet as Sahyadri Lift Services" for another client: never the bare client name, which a breadcrumb
        # link also holds.
        spans = mimic.value_spans({"params": [{"name": "client_name", "example": "Sahyadri", "step": 18}]},
                                  {"client_name": "Konkan Coast Interiors"})
        step = {"n": 45, "kind": "click", "target": {"role": "link", "name": "Open TaxNet as Sahyadri Lift Services"}}
        out = mimic.substitute_step(step, spans)
        self.assertEqual(out["target"]["name"], "Open TaxNet as Konkan Coast Interiors")
        screen = {"elements": [{"role": "link", "name": "Konkan Coast Interiors", "ref": "e1", "value": "", "states": [],
                                "group": "g", "path": "x"},
                               {"role": "link", "name": "Open TaxNet as Konkan Coast Interiors", "ref": "e2", "value": "",
                                "states": [], "group": "g", "path": "y"}], "windows": [], "active_title": ""}
        found, _, _ = replay.resolve(out, screen)
        self.assertEqual(found["ref"], "e2")

    def test_elsewhere_the_value_still_follows(self):
        self.assertEqual(mimic.substitute("Search results for Sharavati", self.spans),
                         "Search results for Sharavati Builders")

    def test_the_demonstrations_value_is_not_avoided_when_this_runs_holds_it(self):
        steps = [{"n": 1, "kind": "type", "param": "client", "target": {"role": "textbox", "name": "Search"},
                  "expect": []},
                 {"n": 2, "kind": "click", "target": {"role": "link", "name": "Sharavati Builders"}, "expect": []}]
        (_, step) = replay.follow_values(steps, [{"name": "client", "example": "Sharavati", "step": 1}],
                                         {"client": "Sharavati Builders"})
        self.assertEqual(step["target"]["avoid"], [])
        screen = {"elements": [{"role": "link", "name": "Sharavati Builders", "ref": "e7", "value": "", "states": [],
                                "group": "g", "path": "x"}], "windows": [], "active_title": ""}
        found, _, _ = replay.resolve(step, screen)
        self.assertEqual(found["ref"], "e7")


class BrokenPlaceholders(unittest.TestCase):
    def test_learning_puts_a_placeholder_on_whole_words_only(self):
        skill = {"params": [], "steps": [{"n": 4, "expect": [
            {"type": "element", "role": "heading", "name": "{client_name} accounts - NA rows"}]}]}
        learning._follow_param(skill, {"name": "uqc", "example": "NA"})
        self.assertEqual(skill["steps"][0]["expect"][0]["name"], "{client_name} accounts - {uqc} rows")

    def test_a_check_with_a_broken_placeholder_is_dropped(self):
        step = {"n": 4, "expect": [{"type": "url", "value": "http://chat.test/thread/1"},
                                   {"type": "active_window", "value": "{boss} ({client_{uqc}me} Builders) - Chat"},
                                   {"type": "element", "role": "heading", "name": "{client_name} chat"},
                                   {"type": "element", "role": "heading", "name": "{nobody} chat"}]}
        (out,) = replay._sound_checks([step], {"client_name", "uqc", "boss"})
        self.assertEqual([c.get("value") or c.get("name") for c in out["expect"]],
                         ["http://chat.test/thread/1", "{client_name} chat"])

    def test_a_placeholder_glued_to_a_word_is_dropped(self):
        step = {"n": 14, "expect": [{"type": "element", "role": "textbox", "name": "User {uqc}me"},
                                    {"type": "element", "role": "heading", "name": "Saved ({uqc})"}]}
        (out,) = replay._sound_checks([step], {"uqc"})
        self.assertEqual([c["name"] for c in out["expect"]], ["Saved ({uqc})"])


class APlaceholderForPartOfAName(unittest.TestCase):
    def test_the_words_after_it_were_the_demonstrations(self):
        check = {"type": "element", "role": "heading", "name": "GST returns dashboard: {client_name} Lift Services"}
        seen = {"elements": [{"role": "heading", "name": "GST returns dashboard: Konkan Coast Interiors"}],
                "windows": [], "active_title": "GST returns dashboard: Konkan Coast Interiors - FilePrep - Chromium"}
        values = {"client_name": "Konkan Coast Interiors"}
        self.assertEqual(replay.unmet([check], seen, values), [])
        window = {"type": "active_window", "value": "GST returns dashboard: {client_name} Lift Services - FilePrep"}
        self.assertEqual(replay.unmet([window], seen, values), [])

    def test_another_page_or_another_client_still_fails(self):
        check = {"type": "element", "role": "heading", "name": "GST returns dashboard: {client_name} Lift Services"}
        values = {"client_name": "Konkan Coast Interiors"}
        other = {"elements": [{"role": "heading", "name": "Clients: Konkan Coast Interiors"}], "windows": [],
                 "active_title": ""}
        self.assertEqual(len(replay.unmet([check], other, values)), 1)
        wrong = {"elements": [{"role": "heading", "name": "GST returns dashboard: Sahyadri Lift Services"}],
                 "windows": [], "active_title": ""}
        self.assertEqual(len(replay.unmet([check], wrong, values)), 1)


class AValueChosenFromAList(unittest.TestCase):
    def test_strict_keeps_a_relaxed_value_the_texts_do_not_hold(self):
        from decurix_agent import binding
        params = [{"name": "quarter", "example": "Quarter 3 (Oct - Dec)"}, {"name": "folder", "example": "9. DEC"}]
        answer = {"values": {"quarter": "Quarter 3 (Oct - Dec)", "folder": "12. DEC"}}
        texts = ["File the December 2026 GSTR-1."]
        strict, _ = binding._clean(params, answer, texts, strict=True)
        self.assertEqual(strict, {})
        relaxed, _ = binding._clean(params, answer, texts, strict=True, relaxed={"quarter"})
        self.assertEqual(relaxed, {"quarter": "Quarter 3 (Oct - Dec)"})       # the typed folder is still dropped


class AFolderTheRequestNames(unittest.TestCase):
    """Found in the learning curves (2026-11-17): "the invoices are also in <folder>" - nothing in it was read, and
    three parts of a filing went to the model by hand."""

    def test_its_document_files_are_read(self):
        import os
        import tempfile
        from unittest import mock
        from decurix_agent import binding
        home = tempfile.mkdtemp()
        folder = os.path.join(home, "work", "oc-2")
        os.makedirs(os.path.join(folder, "sub"))
        for name in ("204_invoice.pdf", "205_invoice.pdf", "notes.md", ".hidden.pdf"):
            open(os.path.join(folder, name), "w").close()
        real = os.path.expanduser
        with mock.patch.object(os.path, "expanduser", lambda p: p.replace("~", home, 1) if p.startswith("~") else real(p)):
            found = binding.folder_files("Whatever it sent is also in ~/work/oc-2/. Our apps: chat")
        self.assertEqual([os.path.basename(f) for f in found], ["204_invoice.pdf", "205_invoice.pdf"])


class AWordOfAValue(unittest.TestCase):
    """Found in the learning curves (2026-11-17): November's bills replayed from the inbox looked for the
    demonstration's "Sales tax bills October" and stopped - a step before the month is entered."""

    PARAMS = [{"name": "bill_month", "example": "October 2026", "default": "October 2026", "step": 13}]

    def test_a_name_holding_a_word_of_the_value_follows_it_before_the_value_is_entered(self):
        steps = [{"n": 5, "kind": "click", "target": {"role": "link", "name": "Sales tax bills October", "path": "p"},
                  "window": "Inbox - Mailbox", "expect": [{"type": "active_window", "value": "Sales tax bills October - Mailbox"},
                                                          {"type": "value", "name": "Month", "param": "bill_month"}]},
                 {"n": 6, "kind": "click", "target": {"role": "link", "name": "Suvarna Lift Systems 2025-26"}, "expect": []},
                 {"n": 7, "kind": "click", "target": {"role": "button", "name": "You may Octobering"}, "expect": []}]
        out = replay._words_follow_values(steps, self.PARAMS, {"bill_month": "November 2026"})
        self.assertEqual(out[0]["target"]["name"], "Sales tax bills November")
        self.assertIsNone(out[0]["target"]["path"])
        self.assertEqual(out[0]["expect"][0]["value"], "Sales tax bills November - Mailbox")
        self.assertEqual(out[0]["expect"][1], steps[0]["expect"][1])
        self.assertIs(out[1], steps[1])
        self.assertIs(out[2], steps[2])

    def test_numbers_and_ambiguous_words_do_not_follow(self):
        params = self.PARAMS + [{"name": "month_2", "example": "October 2026", "default": "October 2026", "step": 64}]
        self.assertEqual(replay._word_changes(params, {"bill_month": "November 2026", "month_2": "December 2026"}), {})
        self.assertEqual(replay._word_changes(self.PARAMS, {"bill_month": "November 2027"}),
                         {"October": "November"})


class AnotherRecordsPage(unittest.TestCase):
    CHECKS = [{"type": "url", "value": "http://books.test/company/2"},
              {"type": "active_window", "value": "Gateway - Bookwell - Suvarna Lifts & Escalators - Chromium",
               "opened": True}]

    def test_its_title_naming_that_record_is_the_same_page(self):
        seen = {"elements": [], "windows": [], "page_url": "http://books.test/company/3",
                "active_title": "Gateway - Bookwell - Suvarna Lift Systems 2023-24 - Chromium"}
        self.assertEqual(replay.unmet(self.CHECKS, seen), [])

    def test_a_click_that_reached_another_records_page_still_stops(self):
        clicked = [dict(self.CHECKS[0]), {k: v for k, v in self.CHECKS[1].items() if k != "opened"}]
        seen = {"elements": [], "windows": [], "page_url": "http://books.test/company/3",
                "active_title": "Gateway - Bookwell - Suvarna Lift Systems 2023-24 - Chromium"}
        self.assertEqual(len(replay.unmet(clicked, seen)), 1)

    def test_only_the_press_after_a_typed_address_is_marked(self):
        steps = [{"n": 7, "kind": "type", "target": {}, "expect": []},
                 {"n": 8, "kind": "key", "target": {}, "expect": [{"type": "active_window", "value": "A - B - C"}]},
                 {"n": 9, "kind": "click", "target": {"name": "Go"}, "expect": [{"type": "active_window", "value": "x"}]}]
        out = replay._mark_opened(steps)
        self.assertTrue(out[1]["expect"][0]["opened"])
        self.assertNotIn("opened", out[2]["expect"][0])

    def test_another_page_or_the_same_address_with_another_title_still_fails(self):
        other = {"elements": [], "windows": [], "page_url": "http://books.test/company/3",
                 "active_title": "Select company - Bookwell - Suvarna Lift Systems 2023-24 - Chromium"}
        self.assertEqual(len(replay.unmet(self.CHECKS, other)), 1)
        same = {"elements": [], "windows": [], "page_url": "http://books.test/company/2",
                "active_title": "Gateway - Bookwell - Suvarna Lift Systems 2023-24 - Chromium"}
        self.assertEqual(len(replay.unmet(self.CHECKS, same)), 1)


if __name__ == "__main__":
    unittest.main()
