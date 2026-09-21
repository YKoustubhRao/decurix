"""
Unit tests for the parts of decurix_agent.computer that need no screen: the
approval gate's rules and its pending/grant store, and element refs and
rendering. The screen-facing parts are exercised end to end by the GUI
benchmark's oracle run (bench/gui/run.py --engine oracle).

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import tempfile
import unittest
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
