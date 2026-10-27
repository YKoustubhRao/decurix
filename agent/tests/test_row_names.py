"""
A table row reads as all its cells, a link cell included. Found live (2026-10-26): a chart of accounts lists each
ledger's name as a link; the row read "Sundry Debtors | 29AAAAS4471J1Z6 | Karnataka | ..." with the name left out (a
link is listed as an element of its own), and the worker took the group for the ledger's name.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import cdp
from decurix_agent.computer.errors import ComputerError

METRICS = ('{"sx": 0, "sy": 0, "ow": 1000, "oh": 800, "iw": 1000, "ih": 800, "scx": 0, "scy": 0, '
           '"dpr": 1, "title": "Chart of Accounts - Ledgers - Books", "url": "http://h:8704/books/ledgers"}')


class TablePage:
    target = {"id": "T", "title": "Chart of Accounts - Ledgers - Books"}

    def __init__(self):
        self.closed = False
        self.events = []

    def evaluate(self, expression):
        if expression == cdp._READY:
            return "complete"
        if expression == cdp._METRICS:
            return METRICS
        return "[]"

    def call(self, method, params=None, timeout=None):
        if method == "Page.getFrameTree":
            raise ComputerError("no frame tree")
        if method == "Accessibility.getFullAXTree":
            def node(i, role, children=(), name=None, parent=None, backend=None):
                d = {"nodeId": str(i), "role": {"value": role}, "childIds": [str(c) for c in children],
                     "backendDOMNodeId": backend if backend is not None else i}
                if name is not None:
                    d["name"] = {"value": name}
                if parent is not None:
                    d["parentId"] = str(parent)
                return d
            return {"nodes": [
                node(1, "RootWebArea", [2]),
                node(2, "table", [3], parent=1),
                node(3, "row", [4, 6, 8], parent=2),
                node(4, "cell", [5], parent=3),
                node(5, "link", [], name="Banavasi Eco Resort", parent=4),
                node(6, "cell", [7], parent=3),
                node(7, "StaticText", [], name="Sundry Debtors", parent=6),
                node(8, "cell", [9], parent=3),
                node(9, "StaticText", [], name="29AAAAS4471J1Z6", parent=8),
            ]}
        if method == "DOMSnapshot.captureSnapshot":
            ids = list(range(1, 10))
            return {"strings": [], "documents": [{"frameId": 0,
                                                  "nodes": {"backendNodeId": ids, "attributes": [[] for _ in ids]},
                                                  "layout": {"nodeIndex": list(range(len(ids))),
                                                             "bounds": [[0, 0, 1000, 600]] + [[10, 100 + 2 * i, 300, 20] for i in range(8)]}}]}
        return {}

    def close(self):
        self.closed = True


class TheRow(unittest.TestCase):
    def test_a_rows_link_cell_is_part_of_its_text(self):
        with mock.patch.object(cdp, "active_page", return_value=TablePage()), \
                mock.patch.object(cdp, "_viewport", return_value=(0, 0, 1)):
            elements, _ = cdp.observe()
        rows = [e for e in elements if e["role"] == "row"]
        self.assertEqual(len(rows), 1, [e["role"] + " " + e["name"] for e in elements])
        self.assertEqual(rows[0]["name"], "Banavasi Eco Resort | Sundry Debtors | 29AAAAS4471J1Z6")
        self.assertTrue(any(e["role"] == "link" and e["name"] == "Banavasi Eco Resort" for e in elements))


if __name__ == "__main__":
    unittest.main()
