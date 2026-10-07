"""
Unit tests for the parts of decurix_agent.computer that need no screen: the
approval gate's rules and its pending/grant store, and element refs and
rendering. The screen-facing parts are exercised end to end by the GUI
benchmark's oracle run (bench/gui/run.py --engine oracle).

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import json
import tempfile
import sys
import unittest
from unittest.mock import patch
from pathlib import Path

from decurix_agent.computer import approval
from decurix_agent.computer import elements as el


def element(role, name, **extra):
    base = {
        "role": role, "name": name, "value": "", "states": [], "box": [0, 0, 10, 10],
        "path": f"site x > {role} \"{name}\"", "source": "cdp", "group": "g", "window_title": "Page - Chromium",
    }
    base.update(extra)
    return base


class ApprovalRules(unittest.TestCase):

    def test_consequential_controls_need_approval(self):
        for name in ("Send newsletter", "Pay now INV-1042", "Buy now", "Place order", "Publish", "Checkout", "Send"):
            with self.subTest(name=name):
                self.assertIsNotNone(approval.assess("click", element("button", name)))

    def test_ordinary_controls_do_not(self):
        for name in ("Save settings", "Sign in", "Create user", "Sending options", "Posts", "Check out our blog", "Delete user"):
            with self.subTest(name=name):
                self.assertIsNone(approval.assess("click", element("button", name)))

    def test_navigating_to_a_module_is_not_an_action(self):
        # Odoo's menu search lists its apps as options; its navbar as one-word links.
        self.assertIsNone(approval.assess("click", element("option", "Purchase")))
        self.assertIsNone(approval.assess("click", element("link", "Purchase", path='site x > navigation "Main" > link "Purchase"')))
        self.assertIsNone(approval.assess("click", element("menuitem", "Purchase", path='site x > navigation > menuitem "Purchase"')))

    def test_actions_that_look_like_navigation_still_stop(self):
        # A button is an action wherever it is; so is a multi-word label, and a one-word item outside the navigation bar.
        self.assertIsNotNone(approval.assess("click", element("button", "Purchase", path='site x > navigation > button "Purchase"')))
        self.assertIsNotNone(approval.assess("click", element("link", "Pay now", path='site x > navigation > link "Pay now"')))
        self.assertIsNotNone(approval.assess("click", element("menuitem", "Post", path='site x > menu "Action" > menuitem "Post"')))
        self.assertIsNotNone(approval.assess("click", element("link", "Purchase")))
        self.assertIsNotNone(approval.assess("click", element("menuitem", "Send & Print", path='site x > navigation > menuitem "Send & Print"')))

    def test_only_activating_roles_are_gated(self):
        self.assertIsNone(approval.assess("click", element("heading", "Send newsletter")))
        self.assertIsNone(approval.assess("type", element("textbox", "Send")))

    def test_enter_sends_only_in_a_messaging_context(self):
        field = element("textbox", "Message")
        self.assertIsNotNone(approval.assess("key", field, keys=["Return"], context={"url": "https://web.whatsapp.com/"}))
        self.assertIsNone(approval.assess("key", field, keys=["Return"], context={"url": "http://127.0.0.1:8765/users"}))
        self.assertIsNotNone(approval.assess("type", field, text="hi\n", context={"window": "Inbox - Gmail"}))

    def test_is_approval_matches_soul(self):
        for reply in ("yes", "Yes, approved - go ahead and pay INV-1043.", "ok", "go ahead", "lgtm"):
            self.assertTrue(approval.is_approval(reply), reply)
        for reply in ("no", "ok but don't send it yet", "wait", "", "maybe later"):
            self.assertFalse(approval.is_approval(reply), reply)


class ApprovalStore(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = approval.STATE_DIR, approval.STATE_FILE
        approval.STATE_DIR = Path(self.tmp.name)
        approval.STATE_FILE = approval.STATE_DIR / "approval.json"

    def tearDown(self):
        approval.STATE_DIR, approval.STATE_FILE = self.saved
        self.tmp.cleanup()

    def test_stop_approve_then_allow_once(self):
        send = element("button", "Send newsletter")

        first = approval.check("click", send)
        self.assertFalse(first.allowed)
        self.assertIsNotNone(approval.pending())

        self.assertEqual(approval.resolve_reply("yes"), "granted")
        self.assertIsNone(approval.pending())

        again = approval.check("click", send)
        self.assertTrue(again.allowed and again.approved)

        # The grant was one-shot.
        self.assertFalse(approval.check("click", send).allowed)

    def test_a_grant_is_for_that_action_only(self):
        approval.check("click", element("button", "Pay now INV-1043"))
        approval.resolve_reply("yes")
        self.assertFalse(approval.check("click", element("button", "Pay now INV-1042")).allowed)

    def test_a_refusal_withdraws_the_request(self):
        send = element("button", "Send")
        approval.check("click", send)
        self.assertEqual(approval.resolve_reply("no, not yet"), "declined")
        self.assertFalse(approval.has_grant())
        self.assertIsNone(approval.resolve_reply("yes"))

    def test_blocked_command_reads_the_gui_exit_contract(self):
        output = "NEEDS_APPROVAL: Approval needed before I click button \"Send\": x. Nothing was clicked.\n"
        self.assertTrue(approval.blocked_command(3, output).startswith("Approval needed"))
        self.assertIsNone(approval.blocked_command(1, output))
        self.assertIsNone(approval.blocked_command(3, "gui: nothing matches"))

    def test_blocked_command_inside_a_chained_command(self):
        # gui click e5 && gui click e6: the shell reports the last exit code.
        approval.check("click", element("button", "Send"))
        output = "Clicked link \"Users\".\nNEEDS_APPROVAL: Approval needed before I click button \"Send\": x.\n"
        self.assertIsNotNone(approval.blocked_command(0, output))

    def test_reset_forgets_grants(self):
        approval.check("click", element("button", "Send"))
        approval.resolve_reply("yes")
        approval.reset()
        self.assertFalse(approval.has_grant())


class Refs(unittest.TestCase):

    def test_refs_follow_paths_across_observations(self):
        first = [element("link", "Users", path="site a > link Users"), element("button", "Save", path="site a > button Save")]
        next_ref = el.assign_refs(first, None)
        previous = {"elements": first, "next_ref": next_ref}

        # A row inserted in front must not renumber what was already there.
        second = [element("row", "new", path="site a > row new")] + [dict(e) for e in first]
        el.assign_refs(second, previous)

        self.assertEqual(second[1]["ref"], first[0]["ref"])
        self.assertEqual(second[2]["ref"], first[1]["ref"])
        self.assertNotIn(second[0]["ref"], {first[0]["ref"], first[1]["ref"]})

    def test_find_by_role_and_name_and_ambiguity(self):
        items = [element("button", "Save", ref="e1"), element("button", "Save as", ref="e2"), element("link", "Save", ref="e3")]
        self.assertEqual(el.find(items, role="button", name="save")["ref"], "e1")
        with self.assertRaises(Exception):
            el.find(items, name="Save")  # a button and a link, both exact

    def test_find_by_path_raises_on_ambiguity_instead_of_guessing(self):
        # Paths are rooted at the window title, and two windows that happen
        # to share one (two "Untitled - Mousepad" windows, say) can produce
        # the same path - find must not silently act on the first match.
        items = [
            element("button", "Save", ref="e1", path="Untitled - Mousepad > button Save"),
            element("button", "Save", ref="e2", path="Untitled - Mousepad > button Save"),
        ]
        with self.assertRaises(Exception):
            el.find(items, path="Untitled - Mousepad > button Save")

        unique = [element("button", "Save", ref="e1", path="Untitled - Mousepad > button Save")]
        self.assertEqual(el.find(unique, path="Untitled - Mousepad > button Save")["ref"], "e1")

    def test_render_folds_bars_and_respects_budget(self):
        items = [element("button", n, ref=f"e{i}", bar="Standard") for i, n in enumerate(["New", "Open", "Save"], 1)]
        items.append(element("textbox", "Name", ref="e9"))
        observation = {"size": [1280, 720], "active_title": "Page - Chromium", "windows": [],
                       "groups": [{"id": "g", "title": "Chromium tab"}], "elements": items}

        text = el.render(observation)
        self.assertIn('"Standard" bar: e1 "New", e2 "Open", e3 "Save"', text)
        self.assertIn('e9 textbox "Name"', text)

        tight = el.render(observation, budget=120)
        self.assertLessEqual(len(tight), 250)
        self.assertIn("more elements not shown", tight)


if __name__ == "__main__":
    unittest.main()


class BrowserIdentityTests(unittest.TestCase):
    """A recording made on the old Chromium must replay on Chrome, and the reverse."""

    def test_the_product_segment_is_reported_under_one_name(self):
        from decurix_agent.computer import browser

        self.assertEqual(browser.canonical_title("Invoices - Odoo - Google Chrome"), "Invoices - Odoo - Chromium")
        self.assertEqual(browser.canonical_title("Invoices - Odoo - Chromium"), "Invoices - Odoo - Chromium")
        self.assertEqual(browser.canonical_title("Google Chrome"), "Chromium")
        self.assertEqual(browser.canonical_title("Report — Google Chrome for Testing"), "Report - Chromium")

    def test_other_windows_that_mention_the_browser_are_not_browsers(self):
        from decurix_agent.computer import browser

        for title in ("Chromium tips.txt - Mousepad", "chrome-extension notes - Mousepad", "Terminal - "):
            self.assertFalse(browser.is_browser_title(title), title)
            self.assertEqual(browser.canonical_title(title), title)

    def test_a_recorded_window_check_matches_the_new_browser(self):
        from decurix_agent.computer import browser, replay

        recorded = replay._stable_title("Customers - Odoo - Chromium")
        live = browser.canonical_title("Customers - Odoo - Google Chrome")
        self.assertTrue(replay.title_matches(recorded, live))


class OneFieldPerTyping(unittest.TestCase):
    def test_a_tab_in_typed_text_is_refused_before_anything_happens(self):
        from unittest import mock
        from decurix_agent.computer import core
        from decurix_agent.computer.errors import ComputerError

        with mock.patch.object(core, "observe") as observe:
            with self.assertRaises(ComputerError) as caught:
                core.act("type", ref="e1", text="Admin\tadmin123\n")
        observe.assert_not_called()  # refused before the screen was even read
        self.assertIn("one field at a time", str(caught.exception))


class ColumnNamedFields(unittest.TestCase):
    """An unlabelled field in a table cell is named by its column's header (Odoo's order lines)."""

    def test_an_unnamed_field_takes_its_columns_header(self):
        from decurix_agent.computer import cdp

        metrics = {"scx": 0, "scy": 100}
        found = [
            {"role": "textbox", "name": "", "box": [210, 300, 80, 20], "path": 'site x > table "Lines" > row "1" > textbox ""'},
            {"role": "textbox", "name": "Customer", "box": [210, 100, 80, 20], "path": 'site x > textbox "Customer"'},
            {"role": "button", "name": "", "box": [400, 300, 20, 20], "path": 'site x > button ""'},
        ]
        # The page reports the field at (200, 310) in document coordinates, scrolled 100 down, under "Quantity".
        cdp._name_by_column(found, [[200, 310, 80, 20, "Quantity"]], metrics, origin_x=10, origin_y=90, dpr=1)

        self.assertEqual(found[0]["name"], "Quantity")
        self.assertTrue(found[0]["path"].endswith('textbox "Quantity"'))
        self.assertEqual(found[1]["name"], "Customer")   # a labelled field keeps its own name
        self.assertEqual(found[2]["name"], "")           # only fields are named by a column

    def test_nothing_at_that_spot_names_nothing(self):
        from decurix_agent.computer import cdp

        found = [{"role": "textbox", "name": "", "box": [210, 300, 80, 20], "path": 'textbox ""'}]
        cdp._name_by_column(found, [[500, 500, 80, 20, "Quantity"]], {"scx": 0, "scy": 0}, 10, 90, 1)
        self.assertEqual(found[0]["name"], "")


class ColumnNamesFromAHostilePage(unittest.TestCase):
    def test_malformed_entries_are_ignored_not_fatal(self):
        from decurix_agent.computer import cdp

        found = [{"role": "textbox", "name": "", "box": [210, 300, 80, 20], "path": 'textbox ""'}]
        cdp._name_by_column(found, [["x", 1], [1, 2, 3, 4, 5], "junk", None, [200, 210, 80, 20, "Qty"]],
                            {"scx": 0, "scy": 0}, 10, 90, 1)
        self.assertEqual(found[0]["name"], "Qty")
        cdp._name_by_column(found, {"not": "a list"}, {"scx": 0, "scy": 0}, 10, 90, 1)


class BrowserAvailability(unittest.TestCase):
    def test_a_slow_devtools_answer_is_waited_for(self):
        import threading, time as _t
        from http.server import BaseHTTPRequestHandler, HTTPServer
        from unittest import mock
        from decurix_agent.computer import cdp

        class Slow(BaseHTTPRequestHandler):
            def do_GET(self):
                _t.sleep(1.2)  # a busy browser: longer than the old half-second limit
                self.send_response(200); self.end_headers(); self.wfile.write(b'{"Browser": "Chrome/154"}')

            def log_message(self, *a):
                pass

        server = HTTPServer(("127.0.0.1", 0), Slow)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            with mock.patch.object(cdp, "BASE", f"http://127.0.0.1:{server.server_port}"):
                self.assertTrue(cdp.available())
        finally:
            server.shutdown()

    @unittest.skipIf(sys.platform == "win32", "Windows retries a refused loopback connection for about 2 s; "
                                              "the desktops, where this matters, are Linux")
    def test_no_browser_is_known_at_once(self):
        import socket, time as _t
        from unittest import mock
        from decurix_agent.computer import cdp

        s = socket.socket(); s.bind(("127.0.0.1", 0)); port = s.getsockname()[1]; s.close()
        with mock.patch.object(cdp, "BASE", f"http://127.0.0.1:{port}"):
            t0 = _t.time()
            self.assertFalse(cdp.available())
            self.assertLess(_t.time() - t0, 1.0)


class LibreOfficeReportedPositions(unittest.TestCase):
    """What a freshly opened Calc reported, measured on a real desktop."""

    MENU = ("menu bar", [0, 36, 1256, 25])
    STALE = [MENU, ("tool bar", [0, 36, 1262, 39]), ("tool bar", [0, 75, 1280, 39]),
             ("tool bar", [1230, 182, 44, 42]), ("tool bar", None)]
    RIGHT = [MENU, ("tool bar", [0, 61, 1262, 39]), ("tool bar", [0, 100, 1280, 39])]

    def test_a_toolbar_over_the_menu_bar_gives_the_offset(self):
        from decurix_agent.computer import atspi
        self.assertEqual(atspi.libreoffice_offset(self.STALE), 25)

    def test_widgets_reported_where_they_are_are_left_alone(self):
        from decurix_agent.computer import atspi
        self.assertEqual(atspi.libreoffice_offset(self.RIGHT), 0)
        self.assertEqual(atspi.libreoffice_offset([self.MENU]), 0)  # no toolbar shown: nothing to measure by
        self.assertEqual(atspi.libreoffice_offset([("tool bar", [0, 36, 10, 10])]), 0)

    def test_the_sheet_tab_moves_and_the_menu_does_not(self):
        from decurix_agent.computer import atspi
        tab = {"role": "page tab", "name": "Sheet1", "box": [133, 603, 63, 23], "bar": None}
        menu = {"role": "menu", "name": "File", "box": [0, 36, 40, 25], "bar": "menu bar"}
        atspi._correct_libreoffice_boxes([tab, menu], self.STALE)
        self.assertEqual(tab["box"], [133, 628, 63, 23])  # where it is drawn, measured
        self.assertEqual(menu["box"], [0, 36, 40, 25])


class FieldsNamedByTheirGroupLabel(unittest.TestCase):
    """OrangeHRM's shape: labels are sibling elements, dates named only by their placeholder."""

    M = {"scx": 0, "scy": 0}

    def _el(self, role, name, box, value=""):
        return {"role": role, "name": name, "value": value, "box": box, "path": f"site x > {role} \"{name}\""}

    def test_placeholder_only_fields_get_their_labels(self):
        from decurix_agent.computer import cdp
        dob = self._el("textbox", "yyyy-dd-mm", [600, 300, 200, 40])
        expiry = self._el("textbox", "yyyy-dd-mm", [900, 200, 200, 40])
        first = self._el("textbox", "First Name", [100, 100, 200, 40])
        labels = [[600, 300, 200, 40, "Date of Birth", "yyyy-dd-mm", None],
                  [900, 200, 200, 40, "License Expiry Date", "yyyy-dd-mm", None],
                  [100, 100, 200, 40, "Employee Full Name", "First Name", None],
                  "junk", [1, 2, 3, 4, 5, 6, 7]]
        cdp._name_by_group([dob, expiry, first], labels, self.M, 0, 0, 1)
        self.assertEqual(dob["name"], "Date of Birth (yyyy-dd-mm)")
        self.assertEqual(expiry["name"], "License Expiry Date (yyyy-dd-mm)")
        self.assertIn("Date of Birth", dob["path"])
        # A placeholder that is not a format is replaced, not kept.
        self.assertEqual(first["name"], "Employee Full Name")

    def test_a_properly_labelled_field_keeps_its_name(self):
        from decurix_agent.computer import cdp
        f = self._el("textbox", "Email", [0, 0, 100, 20])
        cdp._name_by_group([f], [[0, 0, 100, 20, "Contact", "you@example.com", None]], self.M, 0, 0, 1)
        self.assertEqual(f["name"], "Email")

    def test_a_drawn_dropdown_becomes_a_combobox(self):
        from decurix_agent.computer import cdp
        shown = self._el("text", "-- Select --", [610, 510, 80, 20])
        other = self._el("text", "-- Select --", [610, 610, 80, 20])
        labels = [[600, 500, 300, 40, "Nationality", "", "-- Select --"],
                  [600, 600, 300, 40, "Marital Status", "", "-- Select --"]]
        cdp._name_by_group([shown, other], labels, self.M, 0, 0, 1)
        self.assertEqual((shown["role"], shown["name"], shown["value"]), ("combobox", "Nationality", "-- Select --"))
        self.assertEqual(other["name"], "Marital Status")


class ChoosingInADrawnDropdown(unittest.TestCase):

    def _opt(self, name, role="option"):
        return {"role": role, "name": name, "source": "cdp", "box": [0, 0, 10, 10], "path": f"list > {name}"}

    def _run(self, options, want, role="combobox", page=()):
        from decurix_agent.computer import core
        clicked, typed = [], []
        looks = iter([{"elements": list(page)}] + [{"elements": list(page) + options}] * 50)
        with patch.object(core.cdp, "click", side_effect=lambda e, **k: clicked.append(e.get("name"))), \
             patch.object(core.cdp, "type_text", side_effect=lambda e, t: typed.append(t)), \
             patch.object(core, "observe", side_effect=lambda: next(looks)), \
             patch.object(core, "CUSTOM_OPTIONS_SECONDS", 0), patch.object(core, "CUSTOM_OPEN_SECONDS", 0):
            core._select_custom({"role": role, "name": "Nationality"}, want)
        return clicked, typed

    def test_the_exact_label_is_clicked(self):
        clicked, _ = self._run([self._opt("British Indian Ocean"), self._opt("Indian"), self._opt("Indonesian")], "indian")
        self.assertEqual(clicked, ["Nationality", "Indian"])

    def test_a_variant_named_in_another_order_is_found_and_create_is_never_taken(self):
        opts = [self._opt('Create "Customizable Desk (White, Aluminium)"', "link"),
                self._opt("[FURN_0096] Customizable Desk (Aluminium, White)", "link"),
                self._opt("[FURN_0097] Customizable Desk (Steel, White)", "link")]
        clicked, _ = self._run(opts, "Customizable Desk (White, Aluminium)")
        self.assertEqual(clicked[-1], "[FURN_0096] Customizable Desk (Aluminium, White)")

    def test_searches_go_from_the_whole_value_to_its_first_word(self):
        from decurix_agent.computer import core
        self.assertEqual(core._searches("Customizable Desk (White, Aluminium)"),
                         ["Customizable Desk (White, Aluminium)", "Customizable Desk", "Customizable"])

    def test_searches_also_try_runs_of_words_from_inside_the_value(self):
        from decurix_agent.computer import core
        tries = core._searches("Dedicated Internet Leased Line (ILL) 1 Gbps 1:1 Symmetric Bandwidth")
        self.assertIn("Internet Leased Line", tries)
        self.assertLess(tries.index("Internet Leased Line"), tries.index("Dedicated"))
        self.assertLessEqual(len(tries), core.MAX_SEARCHES)

    def test_a_variant_offered_only_as_its_general_item_is_explained(self):
        from decurix_agent.computer import core
        with self.assertRaises(core.ComputerError) as ctx:
            self._run([self._opt("StoolD_0045", "link")], "Stool (Navy)")
        self.assertIn("general item", str(ctx.exception))
        self.assertIn("select 'Stool'", str(ctx.exception))

    def test_only_what_appeared_is_a_choice(self):
        from decurix_agent.computer import core
        elsewhere = self._opt("Indian", "link")  # on the page before the list opened
        with self.assertRaises(core.ComputerError):
            self._run([], "Indian", role="text", page=[elsewhere])

    def test_two_equal_candidates_are_not_guessed_between(self):
        from decurix_agent.computer import core
        with self.assertRaises(core.ComputerError):
            self._run([self._opt("Indian Ocean"), self._opt("Indian Rupee")], "Indian")

    def test_a_missing_option_lists_what_there_is(self):
        from decurix_agent.computer import core
        with self.assertRaises(core.ComputerError) as ctx:
            self._run([self._opt("Single"), self._opt("Married")], "Divorced", role="text")
        self.assertIn("Married", str(ctx.exception))


class TypingNeedsAField(unittest.TestCase):
    def test_typing_into_a_table_row_is_refused_before_anything_happens(self):
        from decurix_agent.computer import core
        row = {"role": "row", "name": "Stool (Navy) | 6.00 | 500.00", "source": "cdp", "ref": "e9",
               "path": "site x > row a", "box": [0, 380, 900, 30], "states": []}
        with patch.object(core, "observe", return_value={"elements": [row], "frame": None, "windows": []}), \
             patch.object(core, "_perform") as perform:
            with self.assertRaises(core.ComputerError) as ctx:
                core.act("type", point=(179, 392), text="Corner Desk Right Sit")
            perform.assert_not_called()
        self.assertIn("not a field", str(ctx.exception))


class ObservingOneWindow(unittest.TestCase):
    """observe(window=...) with the title the observation itself printed."""

    OBS = {
        "size": [1280, 720], "active_title": "Requests for Quotation - Chromium",
        "windows": [{"title": "Requests for Quotation - Chromium", "active": True}],
        "groups": [{"id": "cdp:1", "title": 'Chromium tab "Requests for Quotation" https://x.odoo.com/odoo/purchase',
                    "window_title": "Requests for Quotation - Chromium"},
                   {"id": "atspi:panel", "title": "xfce4-panel window \"\""}],
        "elements": [{"role": "row", "name": "P00002 | Gemini Furniture", "group": "cdp:1", "ref": "e1",
                      "path": "site x > row a", "box": [0, 600, 900, 30], "states": []},
                     {"role": "button", "name": "Start", "group": "atspi:panel", "ref": "e2",
                      "path": "panel > button Start", "box": [0, 690, 30, 30], "states": []}],
    }

    def test_the_full_window_title_finds_the_tab(self):
        from decurix_agent.computer import elements as el
        for wanted in ("Requests for Quotation - Chromium", "Requests for Quotation", "requests for quotation - Google Chrome"):
            text = el.render(self.OBS, only_group=wanted)
            self.assertIn("P00002", text, wanted)
            self.assertNotIn("Start", text, wanted)

    def test_a_name_that_matches_nothing_shows_everything_and_says_so(self):
        from decurix_agent.computer import elements as el
        text = el.render(self.OBS, only_group="Invoices")
        self.assertIn("P00002", text)
        self.assertIn("no window matches", text)


class ADialogsOwnControl(unittest.TestCase):
    def test_confirm_means_the_open_dialogs_not_the_pages(self):
        from decurix_agent.computer import elements as el
        page = {"role": "button", "name": "Confirm", "ref": "e1", "group": "g", "path": "site > button Confirm", "box": [300, 150, 80, 30], "states": []}
        dialog = {"role": "dialog", "name": "Configure your product", "ref": "e2", "group": "g", "path": "site > dialog", "box": [250, 200, 780, 400], "states": []}
        mine = {"role": "button", "name": "Confirm", "ref": "e3", "group": "g", "path": "site > button Confirm[2]", "box": [900, 560, 80, 30], "states": []}
        self.assertEqual(el.find([page, dialog, mine], role="button", name="Confirm")["ref"], "e3")
        # With no dialog open, two Confirms are still for the engine to tell apart.
        with self.assertRaises(el.ComputerError):
            el.find([page, mine], role="button", name="Confirm")


class ARedrawnControlIsFoundAgain(unittest.TestCase):
    """A page that draws a control anew as it is touched (ERPNext's Supplier box) fails an action with a detached node:
    the same control is read again and the action tried once more - never another control."""

    def _obs(self, name):
        box = {"role": "combobox", "name": name, "source": "cdp", "ref": "e5", "path": "site x > combobox Supplier",
               "box": [300, 240, 280, 30], "states": [], "group": "cdp:1"}
        return {"elements": [box], "frame": None, "windows": [], "groups": [{"id": "cdp:1", "title": "x"}],
                "active_title": "New Purchase Invoice", "size": [1280, 720]}

    def _act(self, after_name):
        from types import SimpleNamespace
        from decurix_agent.computer import approval, audit, core
        looks = [self._obs("Supplier"), self._obs(after_name), self._obs(after_name)]
        stale = core.ComputerError("DOM.scrollIntoViewIfNeeded: Node is detached from document")
        with patch.object(core, "observe", side_effect=lambda *a, **k: looks.pop(0) if looks else self._obs(after_name)), \
             patch.object(core, "_perform", side_effect=[stale, "cdp"]) as perform, \
             patch.object(core, "_wait_for_settle"), patch.object(audit, "record"), \
             patch.object(approval, "check", return_value=SimpleNamespace(allowed=True, approved=False)):
            result = core.act("select", ref="e5", option="NexGen Hardware Labs Pvt Ltd")
        return result, perform

    def test_the_same_control_drawn_anew_is_acted_on(self):
        result, perform = self._act("Supplier")
        self.assertEqual(perform.call_count, 2)
        self.assertEqual(result["status"], "done")

    def test_a_different_control_in_its_place_is_not(self):
        from decurix_agent.computer import core
        with self.assertRaises(core.ComputerError) as ctx:
            self._act("Company")
        self.assertIn("detached", str(ctx.exception))


class TypingIsKeys(unittest.TestCase):
    """A single-line field is typed key by key, as a keyboard does: ERPNext's date field ignored text inserted with
    Input.insertText and stayed empty. A field that ignores keys still gets the text, inserted."""

    class FakePage:
        def __init__(self, takes_keys):
            self.takes_keys, self.value, self.calls = takes_keys, "", []

        def call(self, method, params=None):
            self.calls.append(method)
            params = params or {}
            if method == "Input.dispatchKeyEvent" and params.get("type") == "keyDown" and self.takes_keys:
                self.value += params["text"]
            elif method == "Input.insertText":
                self.value += params["text"]
            elif method == "Runtime.callFunctionOn":
                if "this.value" in params.get("functionDeclaration", "") and "multiline" not in params["functionDeclaration"]:
                    return {"result": {"value": self.value}}
                return {"result": {"value": {"multiline": False}}}
            return {}

        def close(self):
            pass

    def run_type(self, takes_keys):
        from unittest import mock
        from decurix_agent.computer import cdp
        page = self.FakePage(takes_keys)
        element = {"handle": {"backend_id": 7}}
        with mock.patch.object(cdp, "_page_for", return_value=page), mock.patch.object(cdp, "_object", return_value="obj"):
            cdp.type_text(element, "04-10-2026")
        return page

    def test_a_field_that_listens_for_keys_gets_keys(self):
        page = self.run_type(takes_keys=True)
        self.assertEqual(page.value, "04-10-2026")
        self.assertNotIn("Input.insertText", page.calls)
        self.assertEqual(page.calls.count("Input.dispatchKeyEvent"), 20)

    def test_a_field_that_ignores_keys_still_gets_the_text(self):
        page = self.run_type(takes_keys=False)
        self.assertEqual(page.value, "04-10-2026")
        self.assertIn("Input.insertText", page.calls)


class LeavingAPageWithUnsavedWork(unittest.TestCase):
    """A navigation the page answers with "Leave site?" is cancelled - the unsaved work stays - and the address is
    opened in a new tab; it used to freeze every DevTools call behind Chrome's dialog."""

    class FakeSocket:
        def __init__(self, dialog_on_navigate=True, already_showing=False):
            import collections
            self.out, self.sent = collections.deque(), []
            self.dialog_on_navigate, self.already_showing = dialog_on_navigate, already_showing
            self.sock = self

        def settimeout(self, t):
            pass

        def send(self, raw):
            m = json.loads(raw)
            self.sent.append(m["method"])
            if m["method"] == "Page.enable":
                if self.already_showing:
                    self.out.append({"method": "Page.javascriptDialogOpening", "params": {"type": "beforeunload"}})
                self.out.append({"id": m["id"], "result": {}})
            elif m["method"] == "Page.navigate":
                if self.dialog_on_navigate:
                    self.out.append({"method": "Page.javascriptDialogOpening", "params": {"type": "beforeunload"}})
                    self.pending = m["id"]
                else:
                    self.out.append({"id": m["id"], "result": {"frameId": "f"}})
            elif m["method"] == "Page.handleJavaScriptDialog":
                self.out.append({"id": m["id"], "result": {}})
                if getattr(self, "pending", None):
                    self.out.append({"id": self.pending, "result": {"frameId": "f", "errorText": "net::ERR_ABORTED"}})

        def receive(self):
            import socket
            if not self.out:
                raise socket.timeout()
            return json.dumps(self.out.popleft())

    def page(self, **kw):
        from decurix_agent.computer import cdp
        p = object.__new__(cdp.Page)
        p.ws, p.seq, p.events, p.target = self.FakeSocket(**kw), 0, [], {}
        return p

    def test_a_page_that_asks_before_leaving_is_kept(self):
        from decurix_agent.computer import cdp
        p = self.page()
        self.assertIsNone(cdp._navigate_unless_asked_to_stay(p, "http://x/new"))
        self.assertIn("Page.handleJavaScriptDialog", p.ws.sent)

    def test_a_dialog_already_showing_is_answered_first(self):
        from decurix_agent.computer import cdp
        p = self.page(already_showing=True, dialog_on_navigate=False)
        self.assertIsNone(cdp._navigate_unless_asked_to_stay(p, "http://x/new"))
        self.assertNotIn("Page.navigate", p.ws.sent)

    def test_a_page_with_nothing_unsaved_is_simply_navigated(self):
        from decurix_agent.computer import cdp
        p = self.page(dialog_on_navigate=False)
        self.assertEqual(cdp._navigate_unless_asked_to_stay(p, "http://x/new"), {"frameId": "f"})
        self.assertNotIn("Page.handleJavaScriptDialog", p.ws.sent)


class AFieldThePageResetIsTypedAgain(unittest.TestCase):
    """A page still setting itself up can re-draw a field and drop what was just typed (ERPNext's new invoice): the
    field is typed once more. A field that kept it, or a password field, is not."""

    def _act(self, values_after, role="textbox"):
        from types import SimpleNamespace
        from decurix_agent.computer import approval, audit, core
        field = {"role": role, "name": "Supplier Invoice Date", "source": "cdp", "ref": "e9",
                 "path": "site x > textbox Supplier Invoice Date", "box": [700, 490, 400, 30], "states": [], "value": ""}
        looks = [{"elements": [dict(field)], "frame": None, "windows": [], "groups": [], "size": [1280, 720]}]
        for value in values_after:
            looks.append({"elements": [dict(field, value=value)], "frame": None, "windows": [], "groups": [], "size": [1280, 720]})
        with patch.object(core, "observe", side_effect=lambda *a, **k: looks.pop(0) if len(looks) > 1 else looks[0]), \
             patch.object(core, "_perform", return_value="cdp") as perform, \
             patch.object(core, "_wait_for_settle"), patch.object(core, "RETYPE_PAUSE", 0), patch.object(audit, "record"), \
             patch.object(approval, "check", return_value=SimpleNamespace(allowed=True, approved=False)):
            result = core.act("type", ref="e9", text="04-10-2026")
        return result, perform

    def test_a_field_the_page_cleared_is_typed_again(self):
        result, perform = self._act(["", "04-10-2026"])
        self.assertEqual(perform.call_count, 2)
        self.assertIn("typed again", result["text"])

    def test_a_field_that_kept_it_is_typed_once(self):
        _, perform = self._act(["04-10-2026"])
        self.assertEqual(perform.call_count, 1)

    def test_a_password_field_is_never_judged(self):
        _, perform = self._act(["", ""], role="password")
        self.assertEqual(perform.call_count, 1)


class ScrollWords(unittest.TestCase):
    """How `gui scroll` reads what an engine wrote."""

    def test_the_plain_forms(self):
        from decurix_agent.computer.cli import scroll_words
        self.assertEqual(scroll_words([]), ([], "down", None))
        self.assertEqual(scroll_words(["e12", "up", "3"]), (["e12"], "up", 3))
        self.assertEqual(scroll_words(["down"]), ([], "down", None))

    def test_a_negative_amount_scrolls_the_other_way(self):
        from decurix_agent.computer.cli import scroll_words
        self.assertEqual(scroll_words(["1268,248", "-5"]), (["1268,248"], "up", 5))
        self.assertEqual(scroll_words(["up", "-2"]), ([], "down", 2))

    def test_a_large_amount_is_pixels(self):
        from decurix_agent.computer.cli import scroll_words
        self.assertEqual(scroll_words(["1238,664", "420"]), (["1238,664"], "down", 4))
        self.assertEqual(scroll_words(["1268,248", "-522"]), (["1268,248"], "up", 5))
        self.assertEqual(scroll_words(["9000"]), ([], "down", 25))
        self.assertEqual(scroll_words(["0"]), ([], "down", 1))


class ClickingBelowThePage(unittest.TestCase):
    """What an engine is told when it clicks for something below the visible part of a page."""

    def test_several_points_are_refused_and_nothing_is_clicked(self):
        from unittest import mock
        from decurix_agent.computer import cli
        with mock.patch.object(cli.core, "act") as act:
            with self.assertRaises(cli.ComputerError) as caught:
                cli.run(["click", "360,690", "360,710", "360,730"])
        act.assert_not_called()
        self.assertIn("gui scroll", str(caught.exception))

    def test_a_point_on_the_taskbar_is_known(self):
        from unittest import mock
        from decurix_agent.computer import core
        with mock.patch.object(core.screen, "docks", return_value=[(0, 690, 1280, 30)]):
            self.assertTrue(core._on_taskbar((360, 700)))
            self.assertFalse(core._on_taskbar((360, 600)))
        with mock.patch.object(core.screen, "docks", side_effect=OSError("no display")):
            self.assertFalse(core._on_taskbar((360, 700)))


class ABusyBrowsersNewTab(unittest.TestCase):
    """A new tab the browser makes after its answer timed out is taken, not asked for again."""

    def test_the_late_tab_is_used(self):
        from unittest import mock
        from decurix_agent.computer import cdp
        old = {"id": "a", "type": "page", "webSocketDebuggerUrl": "ws://a"}
        new = {"id": "b", "type": "page", "webSocketDebuggerUrl": "ws://b"}
        lists = iter([[old], [old], [old, new]])
        with mock.patch.object(cdp, "_page_targets", side_effect=lambda: next(lists)), \
                mock.patch.object(cdp.time, "sleep"):
            self.assertEqual(cdp._new_tab_after({"a"}, 5), new)

    def test_no_tab_is_none(self):
        from unittest import mock
        from decurix_agent.computer import cdp
        old = {"id": "a", "type": "page", "webSocketDebuggerUrl": "ws://a"}
        with mock.patch.object(cdp, "_page_targets", return_value=[old]):
            self.assertIsNone(cdp._new_tab_after({"a"}, 0.6))
