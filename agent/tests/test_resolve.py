"""
Party resolution as a capability (computer/resolve.py, core._select_custom with ids, core._unsearched_record).

Found live (2026-10-26, twice): a bill named its customer otherwise than the books did - a trade name, a title before
the name - while both held one tax number; the worker searched the list by the bill's name, found nothing, and made
a second record. `gui select REF "name" --id IDENTIFIER` now searches by the identifier first: an entry holding it is
the record whatever its name, and what each identifier resolved to is one fact the save gate reads back.

    PYTHONPATH=agent python -m unittest test_resolve
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import cli, core, evidence, resolve
from decurix_agent.computer.errors import ComputerError

TASK = "t0123456789abcdef"
ID = "27ABCDE1234F1Z5"
OTHER = "29ZYXWV9876Q1Z8"
RECORD = "Harbour Works Pvt Ltd"       # the application's name for the party
WRITTEN = "Harbour Marine Services"    # the document's name for it


def element(role, name, y=0, x=0, **extra):
    base = {"role": role, "name": name, "value": "", "states": [], "box": [x, y, 100, 10],
            "path": f"site x > {role} \"{name}\"", "source": "cdp", "handle": {"backend_id": hash(name) % 1000}}
    base.update(extra)
    return base


def opened(url):
    return {"kind": "click", "outcome": "done", "change": f"page is now {url}; 12 element(s) appeared"}


def typed(label, text):
    return {"kind": "type", "outcome": "done", "text": text, "change": "", "element": {"name": label}}


NEW_PARTY = [opened("http://h:8704/party/new"), typed("Name", WRITTEN), typed("Address", "Plot 12, Dockyard Road"),
             typed("Tax ID", ID)]


class Shape(unittest.TestCase):
    def test_searches_go_from_the_whole_identifier_to_its_digits(self):
        self.assertEqual(resolve.searches(ID), [ID, "1234F1Z5", "34F1Z5", "27123415"])
        self.assertEqual(resolve.searches("sneha.iyer@example.com"), ["sneha.iyer@example.com", "sneha.iyer"])
        self.assertEqual(resolve.searches("AC-4471"), ["AC-4471", "C-4471"])   # too few digits to search alone

    def test_an_identifier_is_held_whatever_the_spaces_dashes_and_case(self):
        self.assertTrue(resolve.holds("Tax no. 27-ABCDE-1234-F1Z5 (verified)", ID))
        self.assertTrue(resolve.holds("27abcde 1234 f1z5", ID))
        self.assertFalse(resolve.holds("27ABCDE1234F1Z9", ID))
        self.assertFalse(resolve.holds("", ID))

    def test_fact_lines_are_read_back_as_written(self):
        for line, status, name in ((resolve.matched_line(ID, RECORD), "matched", RECORD),
                                   (resolve.none_line(ID, [ID], [WRITTEN], []), "none", None),
                                   (resolve.ambiguous_line(ID, ["A", "B"]), "ambiguous", None),
                                   (resolve.unanswered_line(ID, [ID], 'textbox "Party"'), "unanswered", None)):
            fact = resolve.parse(line)
            self.assertEqual((fact["identifier"], fact["status"], fact["name"]), (ID, status, name), line)
        self.assertIsNone(resolve.parse("a look at the screen then. resolved nothing"))
        self.assertEqual(resolve.matched_line(ID, RECORD), f'resolved {ID}: matched "{RECORD}" by identifier')
        self.assertEqual(resolve.none_line(ID, [ID, "27123415"], [WRITTEN], [RECORD]),
                         f'resolved {ID}: no record holds it (searched: "{ID}", "27123415"; name searches: "{WRITTEN}"; '
                         f'offered: "{RECORD}")')

    def test_a_row_is_read_whole_and_a_line_about_the_search_is_no_record(self):
        link, cell = element("link", RECORD, y=40, x=10), element("cell", ID, y=40, x=200)
        heading = element("heading", f"Results for {ID}", y=5)
        echo = element("text", ID, y=5, x=300)
        found = resolve.matches([heading, echo, link, cell, element("cell", "Debtors", y=40, x=120)], ID)
        self.assertEqual(found, [(link, RECORD)])
        self.assertEqual(resolve.matches([heading, echo], ID), [])
        # Two rows holding one identifier are two matches.
        twice = [link, cell, element("link", "Harbour Marine Services", y=60, x=10), element("cell", ID, y=60, x=200)]
        self.assertEqual([n for _, n in resolve.matches(twice, ID)], [RECORD, "Harbour Marine Services"])


class Notes(unittest.TestCase):
    """Every test that writes or reads resolution facts works in a notes folder of its own, as task TASK."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)
        for p in (mock.patch.object(evidence, "NOTES_DIR", self.tmp / "evidence"),
                  mock.patch.object(core, "ID_REFUSALS_DIR", self.tmp / "id-refusals"),
                  mock.patch.object(core, "FAILED_SEARCH_FILE", self.tmp / "failed.json"),
                  mock.patch("decurix_agent.task_context.current", lambda: TASK),
                  mock.patch.object(core, "CUSTOM_OPTIONS_SECONDS", 0), mock.patch.object(core, "CUSTOM_OPEN_SECONDS", 0),
                  mock.patch.object(core, "MIN_LIST_ANSWER", 0), mock.patch("time.sleep", lambda s: None)):
            p.start()
            self.addCleanup(p.stop)

    def notes(self):
        return evidence._notes(TASK)

    def save(self, events, looks=()):
        with mock.patch("decurix_agent.computer.recording.load", lambda s: ({}, events)), \
                mock.patch("decurix_agent.computer.evidence.looks", lambda task_id=None: list(looks)):
            return core._unsearched_record({"name": "Accept"})


class ADrawnList(Notes):
    """A field whose list suggests once typed into (most component kits)."""

    def run_select(self, field, answers, ids, option=WRITTEN):
        """
        Select with `ids`; `answers` maps a typed search to the elements its list then shows (others show nothing).
        Returns (what was clicked, what was typed).
        """

        clicked, typed_texts, page = [], [], {"shown": []}

        def type_text(e, t):
            typed_texts.append(t)
            page["shown"] = answers.get(t, [])

        with mock.patch.object(core, "observe", side_effect=lambda **k: {"elements": [field] + page["shown"]}), \
                mock.patch.object(core.cdp, "click", side_effect=lambda e, **k: clicked.append(e.get("name"))), \
                mock.patch.object(core.cdp, "type_text", side_effect=type_text), \
                mock.patch.object(core.cdp, "submit_query", return_value=False), \
                mock.patch.object(core.cdp, "close_list"):
            core._select_custom(field, option, ids=ids)
        return clicked, typed_texts

    def test_the_entry_holding_the_identifier_is_taken_whatever_its_name(self):
        field = element("combobox", "Party")
        entry = element("option", RECORD, y=20, x=10)
        subtitle = element("text", f"Tax no. {ID}", y=20, x=150)
        clicked, typed_texts = self.run_select(field, {ID: [entry, subtitle]}, [ID])
        self.assertEqual(clicked, ["Party", RECORD])
        self.assertEqual(typed_texts, [ID])          # found by the first search: the name was never needed
        self.assertEqual({k: core._selected.get(k) for k in ("name", "why", "by_id")},
                         {"name": RECORD, "why": f"matched by identifier {ID}", "by_id": True})
        self.assertEqual(self.notes(), [f'resolved {ID}: matched "{RECORD}" by identifier'])
        self.assertEqual(core._resolved, self.notes())
        self.assertEqual(resolve.resolution(TASK, ID), {"status": "matched", "name": RECORD, "line": self.notes()[0]})

    def test_a_list_that_only_answers_the_digits_is_found_by_them(self):
        field = element("combobox", "Party")
        entry = element("option", f"{RECORD} - {ID}", y=20)
        clicked, typed_texts = self.run_select(field, {"27123415": [entry]}, [ID])
        self.assertEqual(typed_texts, [ID, "1234F1Z5", "34F1Z5", "27123415"])
        self.assertEqual(clicked[-1], f"{RECORD} - {ID}")
        self.assertEqual(resolve.resolution(TASK, ID)["status"], "matched")

    def test_the_second_identifier_can_be_the_one_that_matches(self):
        field = element("combobox", "Party")
        entry = element("option", f"{RECORD} ({OTHER})", y=20)
        clicked, _ = self.run_select(field, {OTHER: [entry]}, [ID, OTHER])
        self.assertEqual(clicked[-1], f"{RECORD} ({OTHER})")
        self.assertEqual(resolve.resolution(TASK, OTHER)["status"], "matched")
        # The first was searched for, by every form of it, and nothing held it.
        first = resolve.resolution(TASK, ID)
        self.assertEqual(first["status"], "none")
        self.assertIn(f'searched: "{ID}", "1234F1Z5", "34F1Z5", "27123415"', first["line"])

    def test_no_record_holding_it_is_a_fact_and_the_save_then_passes(self):
        field = element("combobox", "Party")
        nothing = element("listitem", "No results found", y=20)
        offered = element("option", RECORD, y=20)
        with self.assertRaises(ComputerError) as caught:
            self.run_select(field, {ID: [nothing], "Harbour": [offered]}, [ID])
        said = str(caught.exception)
        self.assertIn(f"has no option {WRITTEN!r}", said)
        self.assertIn(f"resolved {ID}: no record holds it (searched: ", said)
        self.assertIn(f'offered: "{RECORD}"', said)
        fact = resolve.resolution(TASK, ID)
        self.assertEqual(fact["status"], "none")
        self.assertIn(f'name searches: "{WRITTEN}"', fact["line"])
        # The new record's save: the identifier was resolved to nothing, so this is a new one.
        self.assertEqual(self.save(NEW_PARTY), "")

    def test_a_field_that_showed_nothing_for_any_search_has_not_answered(self):
        field = element("combobox", "Party")
        with self.assertRaises(ComputerError) as caught:
            self.run_select(field, {}, [ID])
        self.assertIn(f"resolved {ID}: unanswered - combobox \"Party\" showed nothing for it (searched: ", str(caught.exception))
        self.assertEqual(resolve.resolution(TASK, ID)["status"], "unanswered")
        said = self.save(NEW_PARTY)
        self.assertIn("the field you searched showed nothing for it", said)
        self.assertIn(f'gui select REF "<the name as the document writes it>" --id {ID}', said)
        self.assertEqual(self.save(NEW_PARTY), "")   # once

    def test_two_entries_holding_one_identifier_are_not_chosen_between(self):
        field = element("combobox", "Party")
        a, b = element("option", f"{RECORD} - {ID}", y=20), element("option", f"{WRITTEN} - {ID}", y=30)
        with self.assertRaises(ComputerError) as caught:
            self.run_select(field, {ID: [a, b]}, [ID])
        self.assertIn(f"2 records hold {ID}", str(caught.exception))
        self.assertEqual(resolve.resolution(TASK, ID)["status"], "ambiguous")
        self.assertIn("more than one record holding it", self.save(NEW_PARTY))

    def test_without_identifiers_nothing_changes(self):
        field = element("combobox", "Party")
        entry = element("option", WRITTEN, y=20)
        clicked, typed_texts = self.run_select(field, {WRITTEN: [entry]}, [])
        self.assertEqual(clicked[-1], WRITTEN)
        self.assertEqual(self.notes(), [])
        self.assertEqual(core._resolved, [])


class AListPage(Notes):
    """A list page with a search box that answers once sent (a form sent by GET): the rows are its answer."""

    def run_find(self, before_rows, after_rows, submits=True, ids=(ID,)):
        field = element("textbox", "Find", y=0)
        head = [element("heading", "Accounts", y=-20), element("link", "New account", y=-10)]
        page = {"now": head + [field] + before_rows}
        clicked = []

        def submit_query(e):
            if submits:
                page["now"] = head + [field] + after_rows
            return submits

        with mock.patch.object(core, "observe", side_effect=lambda **k: {"elements": list(page["now"])}), \
                mock.patch.object(core.cdp, "click", side_effect=lambda e, **k: clicked.append(e.get("name"))), \
                mock.patch.object(core.cdp, "type_text"), mock.patch.object(core.cdp, "close_list"), \
                mock.patch.object(core.cdp, "submit_query", side_effect=submit_query) as sent:
            core._select_custom(field, WRITTEN, ids=list(ids))
        return clicked, sent

    @staticmethod
    def row(name, ident, y):
        return [element("link", name, y=y, x=10), element("cell", "Debtors", y=y, x=150), element("cell", ident, y=y, x=250)]

    def test_the_rows_after_the_search_are_read_and_the_one_holding_it_is_opened(self):
        # The page shows a first page of rows; the one holding the identifier is on another, until the search is sent.
        before = self.row("Another Firm", OTHER, 60) + self.row("Third Firm", "33TTTTT1111T1Z1", 80)
        clicked, sent = self.run_find(before, self.row(RECORD, ID, 40))
        self.assertTrue(sent.called)
        self.assertEqual(clicked, ["Find", RECORD])
        self.assertEqual(core._selected["why"], f"matched by identifier {ID}")
        self.assertEqual(resolve.resolution(TASK, ID), {"status": "matched", "name": RECORD, "line": resolve.matched_line(ID, RECORD)})

    def test_a_search_that_answers_with_no_rows_is_none_and_the_name_is_then_searched(self):
        before = self.row("Another Firm", OTHER, 60)
        with self.assertRaises(ComputerError) as caught:
            self.run_find(before, [element("text", "No accounts match", y=40)])
        fact = resolve.resolution(TASK, ID)
        self.assertEqual(fact["status"], "none", str(caught.exception))
        self.assertIn(f'name searches: "{WRITTEN}"', fact["line"])
        self.assertEqual(self.save(NEW_PARTY), "")

    def test_a_field_whose_form_saves_is_never_sent(self):
        before = self.row("Another Firm", OTHER, 60)
        with self.assertRaises(ComputerError):
            clicked, sent = self.run_find(before, [], submits=False)
        self.assertEqual(resolve.resolution(TASK, ID)["status"], "unanswered")

    def test_a_row_already_on_the_page_holding_it_is_the_match_without_sending(self):
        # A search box that narrows the rows as it is typed into: the rows were there before, none "appears".
        before = self.row(RECORD, ID, 40) + self.row("Another Firm", OTHER, 60)
        clicked, sent = self.run_find(before, [], submits=False)
        self.assertFalse(sent.called)
        self.assertEqual(clicked, ["Find", RECORD])
        self.assertEqual(resolve.resolution(TASK, ID)["status"], "matched")


class TheGate(Notes):
    def test_a_match_refuses_the_new_record_every_time_naming_it(self):
        resolve.note(resolve.matched_line(ID, RECORD))
        said = self.save(NEW_PARTY)
        self.assertIn(f"Not pressed: this makes a new record holding {ID}, and this task resolved {ID}: the record \"{RECORD}\" holds it already", said)
        # Again: a second record holding an identifier a record holds is never the worker's to decide (2026-10-27).
        again = self.save(NEW_PARTY)
        self.assertIn(f"the record \"{RECORD}\" holds it already", again)
        self.assertIn("ask the person (HELP)", again)

    def test_nothing_resolved_refuses_once_and_names_the_tool(self):
        said = self.save(NEW_PARTY)
        self.assertIn(f"nothing in this task resolved {ID}", said)
        self.assertIn(f'gui select REF "<the name as the document writes it>" --id {ID}', said)
        self.assertEqual(self.save(NEW_PARTY), "")

    def test_a_look_that_showed_it_beside_another_name_is_a_trigger_too(self):
        look = (f"{evidence.LOOK_LEAD}. What every field of the page held (in view or not):\n(none)\n"
                f"The words the page showed outside its fields, in its order (in view or not):\n"
                f"Accounts | Name | Tax number | {RECORD} | {ID} | Another Firm | {OTHER}\nThe part in view, as the tool listed it:\n")
        said = self.save(NEW_PARTY, [look])
        self.assertIn(f"a page you looked at shows {ID} already", said)
        self.assertIn(RECORD, said)

    def test_an_edit_form_is_never_refused(self):
        resolve.note(resolve.matched_line(ID, RECORD))
        alter = [opened("http://h:8704/party/alter?id=3"), typed("Name", RECORD), typed("Tax number", ID)]
        self.assertEqual(self.save(alter), "")
        self.assertEqual(core.unsearched_identifier(alter, []), [])

    def test_a_value_resolved_with_id_counts_whatever_its_shape(self):
        short = "AC-4471"
        form = [opened("http://h:8704/party/new"), typed("Name", WRITTEN), typed("Account", short)]
        self.assertEqual(core.unsearched_identifier(form, []), [])            # not identifier-shaped on its own
        self.assertEqual(core.unsearched_identifier(form, [], [short]), [(short, "")])
        resolve.note(resolve.matched_line(short, RECORD))
        self.assertIn(f'the record "{RECORD}" holds it already', self.save(form))
        resolve.note(resolve.none_line(short, [short], [WRITTEN], []))
        self.assertEqual(self.save(form), "")      # the latest fact counts

    def test_the_latest_fact_for_an_identifier_wins(self):
        resolve.note(resolve.unanswered_line(ID, [ID], "x"))
        resolve.note(resolve.none_line(ID, [ID], [], []))
        self.assertEqual(resolve.resolution(TASK, ID)["status"], "none")
        self.assertEqual(resolve.identifiers(TASK), [ID])


class ANativeList(Notes):
    def test_a_select_element_is_chosen_by_the_label_holding_the_identifier(self):
        field = element("combobox", "Party")
        labels = ["-- Select --", f"{RECORD} ({ID})", f"Another Firm ({OTHER})"]
        with mock.patch.object(core.cdp, "native_options", return_value=labels), \
                mock.patch.object(core.cdp, "select_option") as chosen:
            backend = core._perform("select", field, None, None, None, None, None, WRITTEN, False, ids=[ID])
        self.assertEqual(backend, "cdp")
        chosen.assert_called_once_with(field, f"{RECORD} ({ID})")
        self.assertEqual(core._selected["why"], f"matched by identifier {ID}")
        self.assertEqual(resolve.resolution(TASK, ID)["status"], "matched")

    def test_no_label_holding_it_is_none_and_the_name_is_chosen(self):
        field = element("combobox", "Party")
        with mock.patch.object(core.cdp, "native_options", return_value=["Another Firm", WRITTEN]), \
                mock.patch.object(core.cdp, "select_option") as chosen:
            core._perform("select", field, None, None, None, None, None, WRITTEN, False, ids=[ID])
        chosen.assert_called_once_with(field, WRITTEN)
        fact = resolve.resolution(TASK, ID)
        self.assertEqual(fact["status"], "none")
        self.assertIn('offered: "Another Firm"', fact["line"])


class TheCommand(unittest.TestCase):
    def test_id_is_repeatable_and_reaches_act(self):
        args = cli.build_parser().parse_args(["select", "e5", WRITTEN, "--id", ID, "--id", "a@example.com"])
        self.assertEqual(args.ids, [ID, "a@example.com"])
        with mock.patch.object(cli.core, "act", return_value={"text": "ok", "status": "done"}) as act, \
                mock.patch.object(cli.shown, "unseen", return_value=False), \
                mock.patch.object(cli, "_refuse_native_dropdown", create=True):
            try:
                cli.run(["select", "e5", WRITTEN, "--id", ID])
            except Exception:
                pass
        self.assertTrue(act.called)
        self.assertEqual(act.call_args.kwargs.get("ids"), [ID])
        self.assertEqual(act.call_args.kwargs.get("option"), WRITTEN)

    def test_the_usage_names_the_form(self):
        self.assertIn("--id IDENTIFIER", cli.USAGE)


if __name__ == "__main__":
    unittest.main()


class TheNameGate(Notes):
    """A new party under a name a list showed already, titles aside, is refused (found live 2026-10-27)."""

    def save_named(self, events, looks=()):
        with mock.patch("decurix_agent.computer.recording.load", lambda s: ({}, events)), \
                mock.patch("decurix_agent.computer.evidence.looks", lambda task_id=None: list(looks)):
            return core._same_named_record({"name": "Accept"})

    def test_a_title_does_not_make_another_record(self):
        events = [opened("http://h:8704/party/new"), typed("Name", "Mr. Ravi Kumar"), typed("Address", "Somewhere")]
        look = (f"{evidence.LOOK_LEAD}. What every field of the page held (in view or not):\n(none)\n"
                f"The words the page showed outside its fields, in its order (in view or not):\n"
                f"Accounts | Name | Ravi Kumar | Sundry Debtors | Another Firm\nThe part in view, as the tool listed it:\n"
                'e12 row "Ravi Kumar | Sundry Debtors | Karnataka | 4,93,266.00 Dr"\n')
        said = self.save_named(events, [look])
        self.assertIn('a list you looked at shows "Ravi Kumar" already', said)
        self.assertIn("ask the person (HELP)", said)
        self.assertIn('a list you looked at shows', self.save_named(events, [look]))  # it stands every time
        self.assertEqual(self.save_named(events, []), "")
        other = [opened("http://h:8704/party/new"), typed("Name", "Ravi Kumar Traders"), typed("Address", "Somewhere")]
        self.assertEqual(self.save_named(other, [look]), "")
        # A document that quotes the party by name (a voucher's party field) is not a party record (2026-10-27).
        voucher = [opened("http://h:8704/voucher/new"), typed("No.", "1190"), typed("Party A/c name", "Mr. Ravi Kumar"),
                   typed("Amount 1", "7350.50")]
        self.assertEqual(self.save_named(voucher, [look]), "")
        party_field_only = [opened("http://h:8704/party/new"), typed("Party A/c name", "Mr. Ravi Kumar")]
        self.assertEqual(self.save_named(party_field_only, [look]), "")
        self.assertEqual(core.plain_name("M/s. Coastal Builders"), "coastal builders")
