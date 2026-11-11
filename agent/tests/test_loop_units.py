"""
A loop's item is the smallest block that repeats (learning._holds_two_items): a block holding two whole repetitions of
a shorter loop is several items. And a click on a list straight before choosing from it is part of the choosing
(learning._drop_clicks_before_select). Found live (2026-11-08): a month's five vouchers were learned as two blocks of
two and a half, and one voucher's extra click on the voucher-type list kept the step that brings up each new voucher
out of the loop.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_loop_units.py
"""

import unittest

from test_recordings import event, field  # (stubs decurix_agent.config first)

from decurix_agent.computer import learning

META = {"id": "demo-20261108T000000-aaaaaa", "title": "Enter the month's vouchers", "kind": "demonstration",
        "started_at": 1, "actors": ["human"], "screen": [1280, 720]}
W = "Voucher - Chromium"


def ctl(role, name, y):
    return field(role, name, box=[10, y, 200, 24], path=f'site books.test > {role} "{name}"', window_title=W)


NEW, TYPE, NUMBER, AMOUNT, SAVE = (ctl("link", "Create voucher", 5), ctl("combobox", "Voucher type", 40),
                                   ctl("textbox", "Voucher No.", 80), ctl("textbox", "Amount", 120),
                                   ctl("button", "Save voucher", 160))


def month(n=5, clicked_list_before=(), scroll_after=()):
    """n vouchers, each: Create voucher, choose the type, type its number and amount, save."""

    events, i = [], 0

    def add(kind, element=None, **extra):
        nonlocal i
        events.append(event(i, kind, element, window=W, url="http://books.test/v/new", **extra))
        i += 1

    for v in range(n):
        add("click", NEW)
        if v in clicked_list_before:
            add("click", TYPE)
        add("select", TYPE, option="Sales")
        add("type", NUMBER, text=f"{1043 + v}")
        add("type", AMOUNT, text=f"{(v + 1) * 1000}")
        add("click", SAVE)
        if v in scroll_after:
            add("scroll", None, direction="down")
    return META, events


class TheItemIsTheSmallestRepeat(unittest.TestCase):

    def test_five_vouchers_are_five_items_even_when_pairs_match(self):
        skill = learning.build([month(scroll_after=(1, 3))])
        loops = [l for l in skill["loops"] if l["reps"] >= 2]
        self.assertTrue(loops, skill["loops"])
        self.assertEqual(max(l["reps"] for l in loops), 5, loops)

    def test_a_click_on_the_list_before_choosing_does_not_break_the_items(self):
        skill = learning.build([month(clicked_list_before=(3,))])
        self.assertFalse([s for s in skill["steps"] if s["kind"] == "click" and (s.get("target") or {}).get("name") == "Voucher type"])
        self.assertTrue(any("part of the choosing" in t for t in skill["tidied"]), skill["tidied"])
        self.assertEqual(max(l["reps"] for l in skill["loops"]), 5, skill["loops"])


if __name__ == "__main__":
    unittest.main()
