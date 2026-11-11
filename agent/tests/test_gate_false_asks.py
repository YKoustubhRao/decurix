"""
The approval gate's false asks found on ERPNext bills (2026-11-08, O-11), each fixed by structure, never by a word
taken off a list, and the real commits beside them still asked:

- a button that opens a new, empty record ("+ Add Purchase Invoice") was let through by its label's shape; since the
  review of freeze 2 (A5) a button is asked whatever its label: "Create Payment Order" in an app that submits by
  script shows no form;
- a press found by eye is judged by the control the page names at the point, and its yes is bound to that control, not
  to the pixel (core._gate_spot) - three looks at one list row asked three times;
- after Ctrl+L and an address typed, Return is Return in the browser's address bar (core._location_bar), not a press
  "that cannot be told" on the page.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_gate_false_asks.py
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import approval, core


def button(name):
    return {"role": "button", "name": name, "path": "", "source": "cdp", "handle": {"backend_id": 7}}


class ANewRecordButton(unittest.TestCase):
    """Review of freeze 2 (A5): a label shaped like "opens a new record" let "Create Payment Order" and "Make Payment
    Entry" through in apps that submit by script (no form the page can show). A button is asked by its label."""

    def test_a_button_that_reads_like_opening_a_record_is_asked(self):
        for name in ("+ Add Purchase Invoice", "Create Payment Order", "Make Payment Entry", "Create Payment Entry"):
            for context in ({"submits": False}, {}, {"submits": False, "navigates": True}):
                self.assertIsNotNone(approval.assess("click", button(name), context=context), (name, context))
        self.assertFalse(hasattr(approval, "opens_new_record"))

    def test_real_commits_still_ask(self):
        for name in ("Pay", "Buy now", "Purchase", "Place order", "Confirm Purchase Order",
                     "Send invoice", "Submit and pay", "+ Pay"):
            self.assertIsNotNone(approval.assess("click", button(name), context={"submits": False}), name)

    def test_the_page_says_whether_a_press_submits(self):
        with mock.patch.object(core.cdp, "submit_form_fields", return_value=None) as asked:
            self.assertFalse(core._press_submits("click", button("Add Purchase Invoice")))
        asked.assert_called_once()
        with mock.patch.object(core.cdp, "submit_form_fields", return_value=core.cdp.UNREADABLE):
            self.assertTrue(core._press_submits("click", button("Add Purchase Invoice")))
        with mock.patch.object(core.cdp, "submit_form_fields", return_value={"fields": [["Amount", "500"]]}):
            self.assertTrue(core._press_submits("click", button("Add Purchase Invoice")))
        self.assertIsNone(core._press_submits("click", {"role": "button", "name": "Add Purchase Invoice"}))


class AFormSentAsAQuery(unittest.TestCase):
    """A form sent with GET asks for a page (a period, a filter) and keeps nothing: found in an eval (2026-11-09), a
    return-prep app's "OK" under Financial year / Return period was held as the agreed plan's save, twice."""

    PERIOD = {"form": "Select return period", "fields": [["Financial year", "2026-27"], ["Return period", "December"]],
              "submit": "OK", "unset": [], "method": "get"}

    def check(self, element, form):
        pressed = []
        with mock.patch.object(core.cdp, "submit_form_fields", return_value=form), \
                mock.patch.object(core, "_form_check", return_value="CHECKED"):
            said = core._commit_check("click", element, None, {"elements": []}, pressed=pressed)
        return said, pressed

    def test_a_query_form_is_no_save(self):
        said, pressed = self.check(button("OK"), self.PERIOD)
        self.assertEqual((said, pressed), ("", []))  # nothing checked, nothing the plan could hold

    def test_a_form_that_posts_is_still_a_save(self):
        said, pressed = self.check(button("OK"), dict(self.PERIOD, method="post"))
        self.assertEqual(said, "CHECKED")
        self.assertEqual(len(pressed), 1)

    def test_a_query_form_whose_control_commits_is_still_a_save(self):
        said, pressed = self.check(button("Pay now"), self.PERIOD)
        self.assertEqual(len(pressed), 1)

    def test_an_unknown_method_is_not_a_query(self):
        self.assertFalse(core._queries(dict(self.PERIOD, method="")))
        self.assertFalse(core._queries(core.cdp.UNREADABLE))
        self.assertTrue(core._queries(self.PERIOD))


class ALinkThatOnlyGoesSomewhere(unittest.TestCase):
    """A link the page says only leads to another page is no commit, whatever its words - what it leads to is gated
    there. Found in an eval (2026-11-09): "Proceed to file / summary", a link to a return's summary, stopped two runs;
    the filing itself ("File with EVC", a form posted) is asked on that page."""

    def link(self, name):
        return {"role": "link", "name": name, "path": "", "source": "cdp", "handle": {"backend_id": 9}}

    def test_navigation_read_from_the_page_is_not_asked(self):
        self.assertIsNone(approval.assess("click", self.link("Proceed to file / summary"), context={"navigates": True}))
        self.assertIsNotNone(approval.assess("click", self.link("Proceed to file / summary"), context={}))
        self.assertIsNotNone(approval.assess("click", self.link("Proceed to file / summary"),
                                             context={"navigates": False}))

    def test_a_link_that_names_an_act_on_its_own_is_still_asked(self):
        # An older app may act on the address alone: a link labelled as the act keeps the vocabulary's verdict.
        for name in ("Pay now", "Send", "Sign", "Approve payment", "Delete invoice",
                     "Continue to payment and pay now", "Buy now", "Go to checkout - buy now"):
            self.assertIsNotNone(approval.assess("click", self.link(name), context={}), name)  # asks today
            self.assertIsNotNone(approval.assess("click", self.link(name), context={"navigates": True}), name)
        for name in ("Proceed to file / summary", "Go to checkout", "Proceed to filing"):
            self.assertTrue(approval.only_steps_into_an_act(name), name)
            self.assertIsNone(approval.assess("click", self.link(name), context={"navigates": True}), name)

    def test_a_step_into_an_act_is_still_asked(self):
        # Review of freeze 2 (A6): the destination is itself the act.
        for name in ("Proceed to pay", "Proceed to submit", "Proceed to transfer", "Continue with purchase",
                     "Continue to payment", "Go to payment", "Proceed to send"):
            self.assertFalse(approval.only_steps_into_an_act(name), name)
            self.assertIsNotNone(approval.assess("click", self.link(name), context={"navigates": True}), name)

    def test_only_a_link_by_its_role_and_never_a_button(self):
        for name in ("File with EVC", "Pay", "Proceed to file / summary", "Send invoice"):
            self.assertIsNotNone(approval.assess("click", button(name), context={}), name)  # asks today
            self.assertIsNotNone(approval.assess("click", button(name), context={"navigates": True}), name)

    def test_the_page_decides(self):
        link = self.link("Proceed to file / summary")
        with mock.patch.object(core.cdp, "link_target", return_value="http://taxnet.test/returns/1/summary"), \
                mock.patch.object(core.cdp, "submit_form_fields", return_value=None):
            self.assertTrue(core._only_navigates("click", link))
        with mock.patch.object(core.cdp, "link_target", return_value=""):  # javascript:, "#", or no anchor
            self.assertFalse(core._only_navigates("click", link))
        with mock.patch.object(core.cdp, "link_target", return_value="http://x.test/a"), \
                mock.patch.object(core.cdp, "submit_form_fields", return_value={"fields": [["Amount", "9"]]}):
            self.assertFalse(core._only_navigates("click", link))  # its press sends a filled form
        with mock.patch.object(core.cdp, "link_target", return_value="http://x.test/a"), \
                mock.patch.object(core.cdp, "submit_form_fields", return_value=core.cdp.UNREADABLE):
            self.assertFalse(core._only_navigates("click", link))
        self.assertFalse(core._only_navigates("click", button("Proceed to file")))
        self.assertFalse(core._only_navigates("click", {"role": "link", "name": "Proceed to file"}))  # not the page's


class ALinkAScriptMayActOn(unittest.TestCase):
    """Review of freeze 2 (A6): a link with a click handler, an inline handler or data-method="post" was read as
    navigation. cdp.link_target gives no address for such a link, so it is asked."""

    def page(self, href, listeners):
        """A fake DevTools page: the anchor's markup check answers href ('' for a refused one); the press path is two
        nodes, whose listeners are `listeners` (a list per node, None for unreadable)."""

        calls = []

        class Page:
            def call(self, method, params=None, timeout=None):
                calls.append(method)
                if method == "Runtime.callFunctionOn" and params.get("returnByValue"):
                    return {"result": {"value": href}}
                if method == "Runtime.callFunctionOn":
                    return {"result": {"objectId": "path"}}
                if method == "Runtime.getProperties":
                    return {"result": [{"name": "0", "value": {"objectId": "n0"}},
                                       {"name": "1", "value": {"objectId": "n1"}},
                                       {"name": "length", "value": {"value": 2}}]}
                if method == "DOMDebugger.getEventListeners":
                    found = listeners[int(params["objectId"][1:])]
                    return {} if found is None else {"listeners": found}
                raise AssertionError(method)

            def close(self):
                pass

        return Page(), calls

    def target(self, href, listeners):
        page, _ = self.page(href, listeners)
        with mock.patch.object(core.cdp, "_page_for", return_value=page), \
                mock.patch.object(core.cdp, "_object", return_value="el"):
            return core.cdp.link_target({"handle": {"backend_id": 3}})

    def test_a_plain_link_is_navigation(self):
        self.assertEqual(self.target("http://x.test/summary", [[], [{"type": "scroll"}]]), "http://x.test/summary")

    def test_a_press_listener_anywhere_on_the_way_is_not(self):
        self.assertEqual(self.target("http://x.test/summary", [[{"type": "click"}], []]), "")
        self.assertEqual(self.target("http://x.test/summary", [[], [{"type": "pointerdown"}]]), "")  # delegated

    def test_unreadable_listeners_are_not_navigation(self):
        self.assertEqual(self.target("http://x.test/summary", [[], None]), "")

    def test_markup_that_makes_a_link_act_is_refused_in_the_page(self):
        for attribute in ("onclick", "data-method", "data-remote", "data-turbo-method", "hx-post"):
            self.assertIn("'" + attribute + "'", core.cdp._LINK_TARGET, attribute)


class ASignIn(unittest.TestCase):
    """Signing in proves who is signing in and keeps nothing: found in an eval (2026-11-09), a tax portal's "Sign in"
    was held as the agreed plan's save and the run ended there. Signing a document or a filing still asks."""

    LOGIN = {"form": "Sign in", "fields": [["User name", "konkan.coast"]], "submit": "Sign in", "unset": [],
             "method": "post", "signs_in": True}

    def check(self, element, form):
        pressed = []
        with mock.patch.object(core.cdp, "submit_form_fields", return_value=form), \
                mock.patch.object(core, "_form_check", return_value="CHECKED"):
            said = core._commit_check("click", element, None, {"elements": []}, pressed=pressed)
        return said, pressed

    def test_a_sign_in_is_no_save(self):
        self.assertEqual(self.check(button("Sign in"), self.LOGIN), ("", []))
        self.assertIsNone(approval.assess("click", button("Sign in")))

    def test_signing_still_asks_and_is_still_a_save(self):
        for name in ("Sign and file", "Sign document", "E-sign"):
            self.assertIsNotNone(approval.assess("click", button(name)), name)
            said, pressed = self.check(button(name), self.LOGIN)
            self.assertEqual(len(pressed), 1, name)

    def test_only_the_page_shape_makes_a_sign_in(self):
        # An OTP to verify a filing, a PIN alone, or a form the page did not mark: not a sign-in.
        for form in (dict(self.LOGIN, signs_in=False), {k: v for k, v in self.LOGIN.items() if k != "signs_in"},
                     core.cdp.UNREADABLE):
            self.assertFalse(core._signs_in(form))
        said, pressed = self.check(button("Verify"), dict(self.LOGIN, signs_in=False))
        self.assertEqual(len(pressed), 1)

    def test_a_form_that_may_hold_a_secret_the_person_gave_in_this_task_is_no_sign_in(self):
        # Review of freeze 2 (A7): an OTP the person gave, typed into a password-type field beside a filled name,
        # has a sign-in's shape; it authorises, and is asked.
        filled = dict(self.LOGIN, secret_filled=True)
        with mock.patch("decurix_agent.task_context.current", return_value="t1"), \
                mock.patch.object(core.privacy, "literals", return_value=["552901"]):
            self.assertFalse(core._signs_in(filled))
            self.assertFalse(core._signs_in(dict(self.LOGIN, fields=[["Code", "552901"]])))
            self.assertTrue(core._authorises(filled, button("Sign in")))
        with mock.patch("decurix_agent.task_context.current", return_value="t1"), \
                mock.patch.object(core.privacy, "literals", return_value=[]):
            self.assertTrue(core._signs_in(filled))  # no secret given in this task: a sign-in


class APressFoundByEye(unittest.TestCase):

    BEFORE = {"page_url": "http://erp.test/desk/purchase-invoice", "active_title": "Purchase Invoice - Chromium",
              "elements": []}
    ROW = {"role": "link", "name": "Bharti Airtel Ltd - Enterprise Business", "source": "cdp",
           "handle": {"backend_id": 42}, "window_title": "Purchase Invoice - Chromium"}

    def test_the_page_names_the_control_and_the_yes_is_bound_to_it(self):
        described = "Bharti Airtel Ltd - Enterprise Business row in Purchase Invoice list"
        with mock.patch.object(core.cdp, "available", return_value=True), \
                mock.patch.object(core.cdp, "hit", return_value=(self.ROW, {"url": "x"})):
            spots = [core._gate_spot("click", None, p, described, self.BEFORE) for p in ((500, 320), (300, 300))]
        for spot, why, bound in spots:
            self.assertEqual(spot["name"], self.ROW["name"])
            self.assertIs(bound, spot)
            self.assertIsNone(approval.assess("click", spot))  # a link to a record: not a purchase
        prints = {approval._fingerprint("click", bound, None, None, p, context={"url": "u"})
                  for (_, _, bound), p in zip(spots, ((500, 320), (300, 300)))}
        self.assertEqual(len(prints), 1)  # one control, one yes - wherever the look put the point on it

    def test_with_nothing_named_there_the_description_is_judged_and_the_pixel_kept(self):
        with mock.patch.object(core.cdp, "available", return_value=True), \
                mock.patch.object(core.cdp, "hit", return_value=(None, {"url": "x"})), \
                mock.patch("decurix_agent.computer.ocr.available", return_value=False):
            spot, why, bound = core._gate_spot("click", None, (10, 10), "the Pay button", self.BEFORE)
        self.assertEqual(spot["name"], "the Pay button")
        self.assertIsNone(bound)
        self.assertIsNotNone(approval.assess("click", spot))


class TheAddressBar(unittest.TestCase):

    BEFORE = {"active_title": "Purchase Invoice - Chromium", "elements": []}

    def events(self, *events):
        return [dict(e, window=e.get("window", "Purchase Invoice - Chromium"), outcome="done") for e in events]

    def located(self, events, page_has=False):
        with mock.patch.object(core.browser, "is_browser_title", return_value=True), \
                mock.patch.object(core.cdp, "available", return_value=True), \
                mock.patch.object(core.cdp, "page_has_keyboard", return_value=page_has), \
                mock.patch("decurix_agent.computer.evidence._task_events", return_value=events):
            return core._location_bar(self.BEFORE)

    def test_after_ctrl_l_and_an_address_return_is_returns_in_the_address_bar(self):
        bar = self.located(self.events({"kind": "key", "keys": ["ctrl+l"]},
                                       {"kind": "type", "element": None, "target": "", "text": "erp.test/app/x"}))
        self.assertTrue(bar and bar["location_bar"])
        self.assertIsNone(approval.press_after_focus_move(["Delete", "Return"], bar))
        context = {"focused": bar, "window": "Purchase Invoice - Chromium", "url": "http://erp.test/app/x",
                   "controls": [{"role": "button", "name": "Pay", "path": ""}]}
        self.assertIsNone(approval.assess("key", None, keys=["Delete", "Return"], context=context))
        self.assertIsNone(core._pressed_by_key("key", None, ["Return"], self.BEFORE, focus=bar))

    def test_not_without_both_facts(self):
        typed = {"kind": "type", "element": None, "target": "", "text": "x"}
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"]}, typed), page_has=True))
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"]}, typed), page_has=None))
        self.assertIsNone(self.located(self.events(typed)))  # nothing put it there
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"]},
                                                   {"kind": "click", "element": {"role": "button"}}, typed)))
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"]},
                                                   {"kind": "key", "keys": ["Tab"]}, typed)))
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"], "window": "Other - Chromium"},
                                                   typed)))

    def test_an_unknown_keyboard_still_asks(self):
        context = {"focused": None, "window": "Purchase Invoice - Chromium", "url": "http://erp.test/app/x",
                   "controls": [{"role": "button", "name": "Pay", "path": ""}]}
        self.assertIsNotNone(approval.assess("key", None, keys=["Tab", "Return"], context=context))


if __name__ == "__main__":
    unittest.main()
