"""
Unit tests for the parts of decurix_agent.computer that need no screen: the
approval gate's rules and its pending/grant store, and element refs and
rendering. The screen-facing parts are exercised end to end by the GUI
benchmark's oracle run (bench/gui/run.py --engine oracle).

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import tempfile
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
