"""
A label that names no control - the caption of a figure a page shows read-only - is listed, so the figure beside it
says what it is; a label that names a control stays that control's name only. Found live (2026-11-08): a saved form's
totals were listed as bare amounts with their captions dropped, and the independent check of the figures set them
against the wrong lines of the source document.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import cdp

METRICS = ('{"sx": 0, "sy": 0, "ow": 1000, "oh": 800, "iw": 1000, "ih": 800, "scx": 0, "scy": 0, '
           '"dpr": 1, "title": "Order 7 - Shop", "url": "http://h:8000/orders/7"}')


def node(i, role, children=(), name=None, parent=None):
    d = {"nodeId": str(i), "role": {"value": role}, "childIds": [str(c) for c in children], "backendDOMNodeId": i}
    if name is not None:
        d["name"] = {"value": name}
    if parent is not None:
        d["parentId"] = str(parent)
    return d


NODES = [
    node(1, "RootWebArea", [2, 4, 6, 8, 10]),
    # An editable field and its label: the label is the field's name.
    node(2, "LabelText", [3], parent=1),
    node(3, "StaticText", name="Customer", parent=2),
    node(4, "textbox", name="Customer", parent=1),
    # A read-only figure and its caption: the caption names nothing else.
    node(6, "LabelText", [7], parent=1),
    node(7, "StaticText", name="Grand Total", parent=6),
    node(8, "generic", [9], parent=1),
    node(9, "StaticText", name="1,180.00", parent=8),
    node(10, "button", name="Save", parent=1),
]


class FormPage:
    target = {"id": "T", "title": "Order 7 - Shop"}
    events = []

    def evaluate(self, expression):
        if expression == cdp._READY:
            return "complete"
        if expression == cdp._METRICS:
            return METRICS
        return "[]"

    def call(self, method, params=None, timeout=None):
        if method == "Accessibility.getFullAXTree":
            return {"nodes": NODES}
        if method == "DOMSnapshot.captureSnapshot":
            ids = [n["backendDOMNodeId"] for n in NODES]
            return {"strings": [], "documents": [{"frameId": 0,
                                                  "nodes": {"backendNodeId": ids, "attributes": [[] for _ in ids]},
                                                  "layout": {"nodeIndex": list(range(len(ids))),
                                                             "bounds": [[0, 0, 1000, 600]] + [[10, 100 + 30 * k, 300, 20]
                                                                                             for k in range(len(ids) - 1)]}}]}
        if method == "Page.getFrameTree":
            raise cdp.ComputerError("no frame tree")
        return {}

    def close(self):
        pass


class Captions(unittest.TestCase):
    def test_a_read_only_figures_caption_is_listed_and_a_fields_label_is_not_repeated(self):
        with mock.patch.object(cdp, "active_page", return_value=FormPage()), \
                mock.patch.object(cdp, "_viewport", return_value=(0, 0, 1)):
            elements, _ = cdp.observe()
        listed = [(e["role"], e["name"]) for e in elements]
        self.assertIn(("text", "Grand Total"), listed)
        self.assertIn(("text", "1,180.00"), listed)
        self.assertLess(listed.index(("text", "Grand Total")), listed.index(("text", "1,180.00")))
        self.assertIn(("textbox", "Customer"), listed)
        self.assertNotIn(("text", "Customer"), listed)


if __name__ == "__main__":
    unittest.main()
