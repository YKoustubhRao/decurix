import contextlib
import io
import unittest
from unittest import mock

from decurix_agent.computer import cli


def look(*names):
    return {"page_url": "http://host/login", "elements": [{"source": "cdp", "role": "textbox", "name": n} for n in names]}


class ASmallPageIsNotALoadingPage(unittest.TestCase):
    """With no browser to ask (cdp.drawing_state is None): the look exactly as before, a pause of 3 s between looks."""

    def run_observe(self, looks):
        out = io.StringIO()
        with mock.patch.object(cli.core, "observe", side_effect=looks) as observe,                 mock.patch.object(cli.cdp, "drawing_state", return_value=None), \
                mock.patch.object(cli.core, "report_observation"), \
                mock.patch.object(cli.el, "render", return_value="(elements)"), \
                mock.patch("decurix_agent.computer.evidence.note_look"), \
                mock.patch("decurix_agent.computer.grounder.available", return_value=False), \
                mock.patch.object(cli.time, "sleep"), contextlib.redirect_stdout(out):
            cli.run(["observe"])
        return out.getvalue(), observe.call_count

    def test_a_sign_in_page_that_reads_the_same_twice_has_settled(self):
        said, looks = self.run_observe([look("Username", "Password")] * 5)
        self.assertNotIn("almost nothing", said)
        self.assertEqual(looks, 2)

    def test_a_page_still_drawing_itself_is_still_reported_as_loading(self):
        said, _ = self.run_observe([look(), look("a"), look("a", "b"), look("a", "b", "c"), look("a", "b", "c", "d"), look("a", "b", "c", "d", "e")])
        self.assertIn("almost nothing", said)

    def test_with_no_browser_to_ask_the_pause_is_the_old_one(self):
        slept = []
        with mock.patch.object(cli.core, "observe", side_effect=[look("a"), look("a")]),                 mock.patch.object(cli.cdp, "drawing_state", return_value=None),                 mock.patch.object(cli.time, "sleep", side_effect=slept.append):
            observation, loading = cli.settled_look(cli.core.observe())
        self.assertFalse(loading)
        self.assertEqual(slept, [cli.EMPTY_PAGE_PAUSE])


QUIET = {"complete": True, "busy": False, "fetched_ago": None}


class Clock:
    """A clock that moves only when slept on, and a look that takes `look_seconds` of it."""

    def __init__(self, looks, look_seconds=0.0, states=None):
        self.now, self.looks, self.look_seconds = 100.0, list(looks), look_seconds
        self.states = states
        self.taken = 0

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds

    def observe(self, *a, **k):
        self.now += self.look_seconds
        self.taken += 1
        return self.looks.pop(0) if len(self.looks) > 1 else self.looks[0]

    def drawing_state(self):
        if callable(self.states):
            return self.states(self)
        return QUIET if self.states is None else self.states


class TheBrowserSaysWhenASmallPageIsDrawn(unittest.TestCase):
    """With the browser to ask, a small page is looked at every EMPTY_PAGE_POLL seconds and settled once it held its
    shape for EMPTY_PAGE_STEADY seconds while the browser says it is quiet (2026-11-07: every small page paid 3 s)."""

    def settle(self, clock):
        start = clock.now
        with mock.patch.object(cli.core, "observe", side_effect=clock.observe),                 mock.patch.object(cli.cdp, "drawing_state", side_effect=clock.drawing_state),                 mock.patch.object(cli.time, "monotonic", side_effect=clock.monotonic),                 mock.patch.object(cli.time, "sleep", side_effect=clock.sleep):
            observation, loading = cli.settled_look(clock.observe())
        return observation, loading, clock.now - start

    def test_a_static_sign_in_page_settles_in_about_a_second_not_three(self):
        page = look("Sign in", "Username", "Password", "Log in")
        _, loading, took = self.settle(Clock([page]))
        self.assertFalse(loading)
        self.assertAlmostEqual(took, cli.EMPTY_PAGE_STEADY, delta=0.05)

    def test_with_real_looks_it_still_settles_well_under_three_seconds(self):
        page = look("Sign in", "Username", "Password", "Log in")
        _, loading, took = self.settle(Clock([page], look_seconds=0.48))
        self.assertFalse(loading)
        # The first look, a window of 1.2 s from its end, and the look that ends the window: 2.16 s, where the old
        # rule took 0.48 + 3 + 0.48.
        self.assertLess(took, 0.48 + cli.EMPTY_PAGE_STEADY + 0.48 + 0.01)

    def test_a_page_whose_elements_keep_changing_is_not_settled_early(self):
        drawing = [look(*[f"item {i}" for i in range(n % 5 + 1)]) for n in range(200)]
        clock = Clock(drawing)
        _, loading, took = self.settle(clock)
        self.assertTrue(loading)
        self.assertGreaterEqual(took, cli.EMPTY_PAGE_PAUSE * cli.EMPTY_PAGE_LOOKS)

    def test_a_page_that_grows_is_reported_as_soon_as_it_has(self):
        grown = look(*[f"field {i}" for i in range(8)])
        _, loading, took = self.settle(Clock([look("Loading"), grown]))
        self.assertFalse(loading)
        self.assertAlmostEqual(took, cli.EMPTY_PAGE_POLL, delta=0.01)

    def test_a_page_not_yet_complete_waits_for_the_old_rule(self):
        page = look("Sign in", "Username")
        _, loading, took = self.settle(Clock([page], states={"complete": False, "busy": False, "fetched_ago": None}))
        self.assertFalse(loading)
        self.assertGreaterEqual(took, cli.EMPTY_PAGE_PAUSE)

    def test_a_page_that_says_it_is_busy_or_just_fetched_is_not_quiet(self):
        page = look("Results")
        for state in ({"complete": True, "busy": True, "fetched_ago": None},
                      {"complete": True, "busy": False, "fetched_ago": 0.3}):
            _, loading, took = self.settle(Clock([page], states=state))
            self.assertFalse(loading)
            self.assertGreaterEqual(took, cli.EMPTY_PAGE_PAUSE, state)

    def test_a_spinner_is_not_quiet(self):
        page = look("Results")
        page["elements"].append({"source": "cdp", "role": "progressbar", "name": ""})
        _, _, took = self.settle(Clock([page]))
        self.assertGreaterEqual(took, cli.EMPTY_PAGE_PAUSE)

    def test_an_empty_page_is_never_settled_by_holding_still(self):
        _, loading, _ = self.settle(Clock([look()]))
        self.assertTrue(loading)

    def test_a_browser_that_stops_answering_leaves_the_old_rule(self):
        _, loading, took = self.settle(Clock([look("a", "b")], states=lambda c: QUIET if c.taken <= 1 else None))
        self.assertFalse(loading)
        self.assertGreaterEqual(took, cli.EMPTY_PAGE_PAUSE)

    def test_a_large_page_is_not_looked_at_again(self):
        clock = Clock([look(*[f"f{i}" for i in range(10)])])
        with mock.patch.object(cli.cdp, "drawing_state") as asked:
            _, loading, took = self.settle(clock)
        self.assertEqual((loading, took, clock.taken), (False, 0.0, 1))


class WhatTheBrowserIsAsked(unittest.TestCase):
    """cdp.drawing_state: the tab's load state, its busy mark and its last fetch - or None when it cannot be asked."""

    class Page:
        def __init__(self, answer):
            self.answer, self.closed = answer, False

        def evaluate(self, expression):
            if isinstance(self.answer, Exception):
                raise self.answer
            return self.answer

        def close(self):
            self.closed = True

    def ask(self, answer):
        page = self.Page(answer)
        with mock.patch.object(cli.cdp, "active_page", return_value=page):
            found = cli.cdp.drawing_state()
        self.assertTrue(page.closed)
        return found

    def test_the_tab_says_how_it_is(self):
        self.assertEqual(self.ask('{"state": "complete", "busy": false, "fetched_ms_ago": 2500}'),
                         {"complete": True, "busy": False, "fetched_ago": 2.5})
        self.assertEqual(self.ask('{"state": "interactive", "busy": true, "fetched_ms_ago": null}'),
                         {"complete": False, "busy": True, "fetched_ago": None})

    def test_a_tab_that_cannot_be_asked_is_none(self):
        self.assertIsNone(self.ask(cli.cdp.ComputerError("Runtime.evaluate: target closed")))
        self.assertIsNone(self.ask("not json"))
        with mock.patch.object(cli.cdp, "active_page", side_effect=cli.cdp.ComputerError("Chromium has no open tab")):
            self.assertIsNone(cli.cdp.drawing_state())


if __name__ == "__main__":
    unittest.main()
