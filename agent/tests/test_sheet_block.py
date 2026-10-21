"""
Rows of values go into a spreadsheet's grid in one action and are read back cell by cell; text can be given verbatim
on stdin; keys with no target never land in a window an earlier task left open. Found live (2026-10-19): a five-row
sheet typed one cell per command ran a task's 25 minutes out, a formula's quotes were eaten by the shell, and a
formula typed with no target replaced A1 of the previous task's sheet.
"""

import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent.computer import cli, core
from decurix_agent.computer.errors import ComputerError

TITLE = "sales_register_oct.xlsx - LibreOffice Calc"


def sheet_cell(name, row, column, focused=False, title=TITLE, ref="e9"):
    return {"role": "cell", "name": name, "value": "", "source": "atspi", "ref": ref,
            "states": ["focused"] if focused else [], "box": [0, 0, 10, 10], "window_title": title,
            "path": f'sheet "Sheet1" > cell "{name}"',
            "handle": {"indices": [0, 1, 2], "raw_role": "table cell", "sheet_cell": True, "row": row,
                       "column": column, "actions": []}}


def look(elements, title=TITLE):
    return {"elements": elements, "groups": [], "windows": [], "frame": None, "active_title": title, "notes": [],
            "page_url": "", "popups": []}


class BlockHelpers(unittest.TestCase):
    def test_a_block_is_rows_of_cells(self):
        self.assertTrue(core._is_block("Item\tQty"))
        self.assertTrue(core._is_block("Item\nTea"))
        self.assertFalse(core._is_block("Item\n"))  # one value, then Enter
        self.assertFalse(core._is_block("Item"))
        self.assertEqual(core._sheet_rows("Item\tQty\nTea\t4\n"), [["Item", "Qty"], ["Tea", "4"]])

    def test_each_cell_is_placed_from_the_start_cell(self):
        cells = core._block_cells(sheet_cell("Y9", 8, 24), [["a", "b", "c"], ["d"]])
        self.assertEqual([c[0] for c in cells], ["Y9", "Z9", "AA9", "Y10"])
        with self.assertRaises(ComputerError):
            core._block_cells({"role": "cell", "name": "Total"}, [["a"]])

    def test_what_a_cell_shows_is_compared_with_what_was_typed(self):
        for typed, shown in (("3120", "3,120.00"), ("₹78", "78"), ("Parle-G 800g", "Parle-G 800g"),
                             ("9%", "0.09"), ("Ahmed - East", "Ahmed – East"), ("=B2*C2", "3120")):
            self.assertTrue(core._cell_holds(typed, shown), (typed, shown))
        for typed, shown in (("Maggi", "Maggi 12-pack"), ("1/2", "01/02/26"), ("total", "Total"),
                             ("=IF(D2=maharashtra,1,0)", "#NAME?"), ("=A1/0", "#DIV/0!"), ("=X", "Err:502"),
                             ("3120", "3210")):
            self.assertFalse(core._cell_holds(typed, shown), (typed, shown))


class BlockIntoASheet(unittest.TestCase):
    def setUp(self):
        self.keys = []
        self.shown = {}
        for patch in (
                mock.patch.object(core.xinput, "key", lambda combo: self.keys.append(("key", combo))),
                mock.patch.object(core.xinput, "type_text", lambda text, delay=12: self.keys.append(("type", text))),
                mock.patch.object(core, "_focus_window", lambda element: None),
                mock.patch.object(core.time, "sleep", lambda s: None),
                mock.patch.object(core.atspi, "sheet_cell_text", lambda e, r, c: self.shown.get((r, c))),
                mock.patch.object(core, "_wait_for_settle", lambda frame: None),
                mock.patch.object(core, "_note_front", lambda: None),
                mock.patch.object(core, "_adopt_front", lambda: None),
                mock.patch.object(core, "_keys_home", lambda: None),
                mock.patch.object(core, "_left_from_before", lambda: ""),
                mock.patch.object(core.audit, "record", lambda *a, **k: None),
        ):
            patch.start()
            self.addCleanup(patch.stop)

    def act(self, start, text, **target):
        with mock.patch.object(core, "observe", return_value=look([start])), \
                mock.patch.object(core.el, "find", return_value=start):
            return core.act("type", text=text, keys_from_task=True, **target)

    def test_rows_go_in_row_by_row_and_every_cell_is_read_back(self):
        start = sheet_cell("A1", 0, 0)
        self.shown = {(0, 0): "Item", (0, 1): "Qty", (1, 0): "Tea", (1, 1): "4", (2, 0): "Total", (2, 1): "4"}
        result = self.act(start, "Item\tQty\nTea\t4\nTotal\t=SUM(B2:B2)", ref="e9")
        # Each row from its first cell, by the Name Box; Tab between cells; Enter ends the row.
        self.assertEqual(self.keys[:9], [("key", "Escape"), ("key", "ctrl+shift+F5"), ("type", "A1"), ("key", "Return"),
                                         ("type", "Item"), ("key", "Tab"), ("type", "Qty"), ("key", "Return"),
                                         ("key", "Escape")])
        self.assertIn(("type", "A3"), self.keys)
        self.assertEqual(result["unverified"], [])
        self.assertIn("A block of 3 row(s) went into A1:B3", result["text"])
        self.assertIn("every cell holds what was typed", result["text"])
        self.assertIn("B3 =SUM(B2:B2) -> \"4\"", result["text"])

    def test_with_no_target_it_starts_at_the_focused_cell_of_the_active_sheet(self):
        other = sheet_cell("A1", 0, 0, focused=True, title="Untitled 1 - LibreOffice Calc", ref="e30")
        mine = sheet_cell("F1", 0, 5, focused=True, ref="e40")
        self.shown = {(0, 5): "CGST", (0, 6): "SGST"}
        with mock.patch.object(core, "observe", return_value=look([other, mine])):
            result = core.act("type", text="CGST\tSGST", keys_from_task=True)
        self.assertIn(("type", "F1"), self.keys)
        self.assertEqual(result["unverified"], [])

    def test_a_cell_the_app_changed_fails_the_check(self):
        start = sheet_cell("A2", 1, 0)
        self.shown = {(1, 0): "Maggi 12-pack", (1, 1): "25"}
        result = self.act(start, "Maggi\t25", ref="e9")
        self.assertEqual(len(result["unverified"]), 1)
        self.assertIn('A2 shows "Maggi 12-pack" for "Maggi"', result["text"])
        with mock.patch.object(cli.core, "act", return_value=result), \
                mock.patch("sys.stdout", new_callable=io.StringIO):
            self.assertEqual(cli.run(["type", "e9", "Maggi\\t25"]), cli.EXIT_VERIFY_FAILED)

    def test_a_form_field_still_takes_one_value(self):
        field = {"role": "textbox", "name": "Username", "source": "atspi", "ref": "e1", "states": ["focused"],
                 "handle": {}, "window_title": TITLE}
        with mock.patch.object(core, "observe", return_value=look([field])), \
                mock.patch.object(core.el, "find", return_value=field), \
                mock.patch.object(core, "_perform") as perform:
            with self.assertRaises(ComputerError) as caught:
                core.act("type", ref="e1", text="Admin\tadmin123")
        perform.assert_not_called()
        self.assertIn("one field at a time", str(caught.exception))


VIEWER = "http://decurix:8080/host-gui/view?id=excel"


def viewer_look(cells, behind=False, box_disabled=False):
    """A look at the host-app viewer: its cells as `row "D2 = 640"`, and the Name Box listed twice (combobox, textbox)."""

    states = ["disabled"] if box_disabled else []
    elements = [{"role": "row", "name": f"{a} = {v}", "source": "cdp", "states": [], "group": "g",
                 "path": f'grid > row "{a}"', "box": [0, 0, 10, 10]} for a, v in cells.items()]
    elements.append({"role": "combobox", "name": "Name Box", "source": "cdp", "states": list(states), "group": "g",
                     "value": "", "path": 'combobox "Name Box"', "box": [0, 0, 10, 10]})
    elements.append({"role": "textbox", "name": "Name Box", "source": "cdp", "states": list(states), "group": "g",
                     "value": "", "path": 'combobox "Name Box" > textbox "Name Box"', "box": [0, 0, 10, 10]})
    return {"elements": elements, "groups": [], "windows": [], "frame": None, "active_title": "Excel",
            "notes": [], "page_url": VIEWER, "popups": [], "behind": behind}


class ViewerBase(unittest.TestCase):
    def setUp(self):
        self.keys = []
        self.set = []
        for patch in (
                mock.patch.object(core.xinput, "key", lambda combo: self.keys.append(("key", combo))),
                mock.patch.object(core.xinput, "type_text", lambda text, delay=12: self.keys.append(("type", text))),
                mock.patch.object(core.cdp, "set_in_app", self.set_in_app),
                mock.patch.object(core.time, "sleep", lambda s: None),
                mock.patch.object(core, "_focus_window", lambda element: None),
                mock.patch.object(core, "_wait_for_settle", lambda frame: None),
                mock.patch.object(core, "_note_front", lambda: None),
                mock.patch.object(core, "_adopt_front", lambda: None),
                mock.patch.object(core, "_keys_home", lambda: None),
                mock.patch.object(core, "_left_from_before", lambda: ""),
                mock.patch.object(core, "_nothing_in_front", lambda wait=0: False),
                mock.patch.object(core.audit, "record", lambda *a, **k: None),
        ):
            patch.start()
            self.addCleanup(patch.stop)
        self.before = {"A1": "Order", "B1": "SKU", "C1": "Qty", "A2": "ORD-1", "B2": "SKU-101", "C2": "51",
                       "A3": "ORD-2", "B3": "SKU-150", "C3": "7"}

    def set_in_app(self, element, text, append=False):
        # As the live viewer answered (2026-10-19): the combobox is not a field of the app, its textbox is.
        if element["role"] == "combobox":
            raise ComputerError(f'combobox "Name Box": not a field of the app')
        self.set.append((element["role"], text))

    def act(self, text, *looks, **kw):
        with mock.patch.object(core, "observe", side_effect=list(looks)):
            return core.act("type", text=text, keys_from_task=True, **kw)


class BlockIntoAHostAppsSheet(ViewerBase):
    """Real Excel through the host-app viewer: entered row by row from the named cell, then read back."""

    def test_rows_begin_at_their_first_cell_by_the_name_box_with_real_tab_and_enter_keys(self):
        after = dict(self.before, D1="Unit Price", E1="Line Total", D2="640", E2="32640", D3="NOT FOUND")
        text = 'Unit Price\tLine Total\n=IFERROR(VLOOKUP(B2,Prices!A:C,3,FALSE),"NOT FOUND")\t=IF(D2="NOT FOUND","",C2*D2)'
        result = self.act(text, viewer_look(self.before), viewer_look(after, behind=True), viewer_look(after),
                          cell="D1")
        # The textbox takes the address (the combobox is tried only when it cannot); each row starts there.
        self.assertEqual(self.set, [("textbox", "D1"), ("textbox", "D2")])
        self.assertEqual(self.keys, [
            ("key", "Escape"),
            ("key", "Return"), ("type", "Unit Price"), ("key", "Tab"), ("type", "Line Total"), ("key", "Return"),
            ("key", "Return"), ("type", '=IFERROR(VLOOKUP(B2,Prices!A:C,3,FALSE),"NOT FOUND")'), ("key", "Tab"),
            ("type", '=IF(D2="NOT FOUND","",C2*D2)'), ("key", "Return")])
        # No text with a Tab in it is ever set or typed as one piece.
        self.assertFalse(any("\t" in t for kind, t in self.keys if kind == "type"))
        self.assertEqual(result["unverified"], [])
        self.assertIn("(D1:E2)", result["text"])
        self.assertIn('D2 =IFERROR(VLOOKUP(B2,Prices!A:C,3,FALSE),"NOT FOUND") -> "640"', result["text"])

    def test_a_cell_being_edited_is_left_first_and_a_box_still_greyed_out_is_refused(self):
        after = dict(self.before, D1="Unit Price")
        result = self.act("Unit Price", viewer_look(self.before, box_disabled=True), viewer_look(self.before),
                          viewer_look(after), cell="D1")
        self.assertEqual(self.keys[0], ("key", "Escape"))
        self.assertEqual(self.set, [("textbox", "D1")])
        self.assertEqual(result["unverified"], [])
        self.keys.clear()
        with self.assertRaises(ComputerError) as caught:
            self.act("Unit Price", viewer_look(self.before, box_disabled=True),
                     viewer_look(self.before, box_disabled=True), cell="D1")
        self.assertIn("greyed out", str(caught.exception))
        self.assertFalse(any(kind == "type" for kind, _ in self.keys))

    def test_several_rows_with_no_cell_named_are_refused(self):
        with self.assertRaises(ComputerError) as caught:
            self.act("Unit Price\tLine Total\n=C2*D2\t=C2", viewer_look(self.before))
        self.assertIn("gui type D1 - <<'EOF'", str(caught.exception))
        self.assertFalse(any(kind == "type" for kind, _ in self.keys))

    def test_one_row_with_no_cell_named_goes_where_the_cursor_is(self):
        after = dict(self.before, D1="Unit Price", E1="Line Total")
        result = self.act("Unit Price\tLine Total", viewer_look(self.before), viewer_look(after))
        self.assertEqual(self.keys, [("type", "Unit Price"), ("key", "Tab"), ("type", "Line Total"), ("key", "Return")])
        self.assertEqual(result["unverified"], [])

    def test_a_cell_that_went_wrong_fails_the_check(self):
        after = dict(self.before, D1="Unit Price", E1="Line Total", D2="#NAME?", E2="FALSE")
        result = self.act("Unit Price\tLine Total\n=VLOOKUP(B2,P,3)\t=C2*D2", viewer_look(self.before),
                          viewer_look(after), cell="D1")
        self.assertEqual(len(result["unverified"]), 1)  # FALSE is a result; #NAME? is not
        self.assertIn('D2 shows "#NAME?"', result["text"])

    def test_keys_that_reached_no_cell_are_said_to(self):
        result = self.act("Unit Price\tLine Total", viewer_look(self.before), viewer_look(self.before))
        self.assertIn("no cell of the sheet changed", result["unverified"][0])

    def table(self, rows, total_at=None):
        """A table of `rows` data rows under a header, columns A to C, and a Total row at `total_at` when given."""

        cells = {"A1": "Order", "B1": "SKU", "C1": "Qty"}
        for r in range(2, rows + 2):
            cells.update({f"A{r}": f"ORD-{r}", f"B{r}": f"SKU-{r}", f"C{r}": str(r)})
        if total_at:
            cells.update({f"A{total_at}": "Total", f"C{total_at}": "999"})
        return cells

    def test_a_block_that_runs_out_of_time_stops_at_a_row_boundary_and_says_what_was_not_typed(self):
        # Found live (2026-10-19): the 25-row block was killed by the operator's 180 s limit before its last row, with
        # nothing read back and "exit 124" read past; the Total then summed a table one row short.
        before = self.table(5)
        typed = {"D1": "Unit Price", "E1": "Line Total", "D2": "640", "E2": "32640"}
        text = "Unit Price\tLine Total\n640\t32640\n640\t32640\n640\t32640\n640\t32640"
        ticks = iter(range(0, 1000, 60))
        real = core._type_relayed_rows
        with mock.patch.object(core, "_type_relayed_rows",
                               lambda b, cell, t: real(b, cell, t, budget=100, clock=lambda: next(ticks))):
            result = self.act(text, viewer_look(before), viewer_look(dict(before, **typed)), cell="D1")
        # The clock reads 0, 60, 120: row 1 is begun at 60 s, row 2 is not (120 s is over the 100 s budget).
        self.assertEqual(self.set, [("textbox", "D1"), ("textbox", "D2")])
        self.assertEqual(len(result["unverified"]), 1)
        said = result["unverified"][0]
        self.assertIn("STOPPED after row 2 of 5", said)
        self.assertIn("the other 3 row(s) were NOT typed - start again at D3", said)
        self.assertIn("Ctrl+D", said)
        self.assertIn("A block of 2 of 5 row(s) was typed (D1:E2)", result["text"])

    def test_the_budget_stops_between_rows_never_inside_one(self):
        before = self.table(5)
        ticks = iter([0, 10, 500, 600, 700])
        done = core._type_relayed_rows(viewer_look(before), "D1", "a\tb\nc\td\ne\tf\ng\th", budget=100,
                                       clock=lambda: next(ticks))
        self.assertEqual(done, 2)
        self.assertEqual([t for kind, t in self.keys if kind == "type"], ["a", "b", "c", "d"])

    def test_a_block_that_stops_short_of_the_table_beside_it_is_noted_not_failed(self):
        before = self.table(5, total_at=7)  # data rows 2-6, the Total in row 7
        after = dict(before, D1="Unit Price", E1="Line Total", D2="640", E2="32640", D3="640", E3="32640")
        result = self.act("Unit Price\tLine Total\n640\t32640\n640\t32640", viewer_look(before), viewer_look(after),
                          cell="D1")
        self.assertEqual(result["unverified"], [])
        self.assertIn("Note: the table to its left (column C) goes on to row 6, but this block ends at row 3 - rows 4 "
                      "to 6 were NOT filled", result["text"])

    def test_a_block_that_reaches_the_end_of_the_table_or_its_total_has_no_note(self):
        before = self.table(2, total_at=4)  # data rows 2-3, a Total in row 4
        after = dict(before, D1="Unit Price", E1="Line Total", D2="1", E2="2", D3="3", E3="4")
        result = self.act("Unit Price\tLine Total\n1\t2\n3\t4", viewer_look(before), viewer_look(after), cell="D1")
        self.assertNotIn("Note:", result["text"])
        # A header typed on its own is not a block that stopped short, whatever follows it.
        before = self.table(5)
        result = self.act("Unit Price\tLine Total", viewer_look(before),
                          viewer_look(dict(before, D1="Unit Price", E1="Line Total")), cell="D1")
        self.assertNotIn("Note:", result["text"])

    def test_the_table_beside_a_block_is_followed_in_a_calc_sheet_too(self):
        shown = {(r, 2): str(r) for r in range(0, 6)}  # column C, rows 1-6 (0-based rows)
        shown.update({(0, 3): "Unit Price", (0, 4): "Line Total", (1, 3): "1", (1, 4): "2", (2, 3): "3", (2, 4): "4"})
        start = sheet_cell("D1", 0, 3)
        with mock.patch.object(core.atspi, "sheet_cell_text", lambda e, r, c: shown.get((r, c), "")):
            lines, found = core._read_back_block(start, "Unit Price\tLine Total\n1\t2\n3\t4")
        self.assertEqual(len(found), 1)
        self.assertTrue(found[0].startswith(core.NOTE))
        self.assertIn("goes on to row 6, but this block ends at row 3", found[0])

    def test_a_viewer_with_no_sheet_still_takes_one_field_at_a_time(self):
        with mock.patch.object(core, "observe", return_value=viewer_look({})), \
                mock.patch.object(core, "_perform") as perform:
            with self.assertRaises(ComputerError) as caught:
                core.act("type", text="Admin\tsecret", keys_from_task=True)
        perform.assert_not_called()
        self.assertIn("one field at a time", str(caught.exception))

    def test_typing_that_shows_nothing_yet_in_a_viewer_is_not_called_lost(self):
        self.assertFalse(core._nothing_typed("type", None, "=IFERROR(A1,0)", "nothing visible changed",
                                             viewer_look(self.before)))


class ACellNamedByItsCoordinate(ViewerBase):
    """`gui type D1 ...`: found live (2026-10-19), a block aimed at D1 in real Excel was refused "one field at a time"."""

    def setUp(self):
        super().setUp()
        self.shown = {}
        patch = mock.patch.object(core.atspi, "sheet_cell_text", lambda e, r, c: self.shown.get((r, c)))
        patch.start()
        self.addCleanup(patch.stop)

    def test_in_a_sheet_this_desktop_reads_it_is_that_cell_even_unlisted(self):
        listed = sheet_cell("B2", 1, 1, focused=True)
        self.shown = {(0, 3): "Unit Price", (0, 4): "Line Total"}
        with mock.patch.object(core, "observe", return_value=look([listed])):
            result = core.act("type", text="Unit Price\tLine Total", keys_from_task=True, cell="D1")
        self.assertIn(("type", "D1"), self.keys)
        self.assertEqual(result["unverified"], [])
        self.assertIn("went into D1:E1", result["text"])

    def test_a_cell_with_no_spreadsheet_in_front_is_refused(self):
        with mock.patch.object(core, "observe", return_value=look([])), \
                mock.patch.object(core, "_perform") as perform:
            with self.assertRaises(ComputerError) as caught:
                core.act("type", text="x", keys_from_task=True, cell="D1")
        perform.assert_not_called()
        self.assertIn("shows no spreadsheet", str(caught.exception))

    def test_the_command_line_reads_a_coordinate_as_a_cell_and_e12_as_a_ref(self):
        with mock.patch.object(cli.shown, "unseen", lambda ref: False), \
                mock.patch.object(cli.core, "act", return_value={"status": "done", "text": "ok"}) as act, \
                mock.patch("sys.stdout", new_callable=io.StringIO):
            cli.run(["type", "D1", "Unit Price\\tLine Total"])
            self.assertEqual(act.call_args.kwargs["cell"], "D1")
            cli.run(["type", "e12", "Unit Price"])
            self.assertEqual(act.call_args.kwargs["ref"], "e12")
            self.assertNotIn("cell", act.call_args.kwargs)
            # A range names where the block starts: its top-left cell.
            cli.run(["type", "F3:F13", "a\\tb\\nc\\td"])
            self.assertEqual(act.call_args.kwargs["cell"], "F3")
            cli.run(["type", "$D$2:$E$25", "x"])
            self.assertEqual(act.call_args.kwargs["cell"], "$D$2")


class VerbatimText(unittest.TestCase):
    def test_text_on_stdin_is_typed_exactly(self):
        formula = '=IF(LOWER(TRIM(D2))="maharashtra",E2*9%,0)\t=E2+F2'
        with mock.patch.object(cli.shown, "unseen", lambda ref: False), \
                mock.patch.object(cli.core, "act", return_value={"status": "done", "text": "ok"}) as act, \
                mock.patch("sys.stdin", io.StringIO(formula + "\n")), \
                mock.patch("sys.stdout", new_callable=io.StringIO):
            self.assertEqual(cli.run(["type", "e9", "-"]), cli.EXIT_OK)
        self.assertEqual(act.call_args.kwargs["text"], formula)
        self.assertTrue(act.call_args.kwargs["keys_from_task"])

    def test_written_marks_are_still_read_on_the_command_line(self):
        with mock.patch.object(cli.shown, "unseen", lambda ref: False), \
                mock.patch.object(cli.core, "act", return_value={"status": "done", "text": "ok"}) as act, \
                mock.patch("sys.stdout", new_callable=io.StringIO):
            cli.run(["type", "e9", "Item\\tQty"])
        self.assertEqual(act.call_args.kwargs["text"], "Item\tQty")


class WindowsFromBefore(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.windows = [{"id": 1, "title": "Untitled 1 - LibreOffice Calc", "active": True}]
        self.task = "t0000000000000001"
        for patch in (mock.patch.object(core, "TASK_WINDOWS_FILE", Path(tmp.name) / "task-windows.json"),
                      mock.patch.object(core, "STATE_DIR", Path(tmp.name)),
                      mock.patch.object(core.screen, "windows", lambda: [dict(w) for w in self.windows]),
                      mock.patch.object(core, "_task_now", lambda: self.task)):
            patch.start()
            self.addCleanup(patch.stop)

    def front(self, wid):
        for w in self.windows:
            w["active"] = w["id"] == wid

    def test_a_window_open_before_the_task_is_not_its_own_until_it_works_there(self):
        core._task_windows()  # the task's first look
        self.windows.append({"id": 2, "title": TITLE, "active": False})
        self.front(1)
        self.assertEqual(core._left_from_before(), "Untitled 1 - LibreOffice Calc")
        self.front(2)  # the task's own workbook
        self.assertEqual(core._left_from_before(), "")
        self.front(1)
        core._adopt_front()  # brought forward or acted in on purpose
        self.assertEqual(core._left_from_before(), "")

    def test_a_new_task_starts_a_new_record(self):
        core._task_windows()
        self.front(1)
        core._adopt_front()
        self.task = "t0000000000000002"
        self.assertEqual(core._left_from_before(), "Untitled 1 - LibreOffice Calc")
        self.assertEqual(json.loads(core.TASK_WINDOWS_FILE.read_text())["task"], self.task)

    def test_with_no_task_nothing_is_refused(self):
        self.task = ""
        self.assertEqual(core._left_from_before(), "")

    def test_keys_with_no_window_in_front_would_go_to_the_desktop(self):
        patch = mock.patch.object(core, "FRONT_WAIT_SECONDS", 0)
        patch.start()
        self.addCleanup(patch.stop)
        self.front(None)
        self.assertTrue(core._nothing_in_front())
        with mock.patch.object(core, "observe", return_value=look([], title="")), \
                mock.patch.object(core, "_keys_home", lambda: None), \
                mock.patch.object(core, "_perform") as perform:
            with self.assertRaises(ComputerError) as caught:
                core.act("key", keys=["alt+F4"], keys_from_task=True)
        perform.assert_not_called()
        self.assertIn("no window is in front", str(caught.exception))
        self.assertIn("If you just opened an app it is still starting", str(caught.exception))
        self.front(1)
        self.assertFalse(core._nothing_in_front())

    def test_keys_with_no_target_are_refused_there(self):
        with mock.patch.object(core, "observe", return_value=look([], title="Untitled 1 - LibreOffice Calc")), \
                mock.patch.object(core, "_keys_home", lambda: None), \
                mock.patch.object(core, "_left_from_before", lambda: "Untitled 1 - LibreOffice Calc"), \
                mock.patch.object(core, "_perform") as perform:
            with self.assertRaises(ComputerError) as caught:
                core.act("type", text="=11,160", keys_from_task=True)
            perform.assert_not_called()
            self.assertIn("open before this task began", str(caught.exception))
            # A replay's or a person's own keys are not asked (keys_from_task is the engine's gui command).
            perform.side_effect = RuntimeError("typed")
            with self.assertRaises(RuntimeError):
                core.act("type", text="=11,160")


if __name__ == "__main__":
    unittest.main()
