"""
A task done once per document whose request brings its documents: the person's first steps - the ones that only lead
to the work - are replayed, not collected by the model; the block of those steps that makes a record from a document's
values (a new customer) runs only for a document whose record the application does not have, put before that
document's block (skill_task._gather_by_replay, _make_missing, _spliced). Measured in the learning curves (2026-11-17):
the model collecting a month's bills was 72% of the bills workflow's tokens, 17 to 54 turns a case, one click a turn.

    PYTHONPATH=agent python3 -m unittest agent.tests.test_gather_replay
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import skill_task
from decurix_agent.computer import skills


def step(n, kind, role, name, url, param=None, expect=None, sid=None):
    out = {"n": n, "id": sid or f"s{n}", "kind": kind, "target": {"role": role, "name": name}, "url": url,
           "expect": expect or []}
    if param:
        out["param"] = param
    return out


def skill():
    reg, new, ledgers, voucher = ("http://books.test/reg", "http://books.test/ledgers/new", "http://books.test/ledgers",
                                  "http://books.test/vouchers/new")
    steps = [
        step(1, "click", "link", "Inbox", "http://mail.test/"),
        step(2, "select", "combobox", "Month", reg, param="month"),
        step(3, "click", "button", "Show", reg + "?month=2026-10"),
        step(4, "click", "link", "Create ledger", reg + "?month=2026-10", expect=[{"type": "url", "value": new}]),
        step(5, "type", "textbox", "Name", new, param="customer_name"),
        step(6, "select", "combobox", "State", new, param="customer_state"),
        step(7, "click", "button", "Save ledger", new),
        step(8, "click", "link", "Create voucher", ledgers),
        step(9, "select", "combobox", "Party", voucher, param="party",
             expect=[{"type": "value", "role": "combobox", "name": "Party", "param": "party"}]),
        step(10, "click", "button", "Save voucher", voucher),
    ]
    params = [{"name": "month", "label": "Month", "step": 2, "default": "October 2026", "example": "October 2026"},
              {"name": "customer_name", "label": "Name", "step": 5, "default": "Kadri", "example": "Kadri"},
              {"name": "customer_state", "label": "State", "step": 6, "default": "29-Karnataka",
               "example": "29-Karnataka"},
              {"name": "party", "label": "Party", "step": 9, "default": "Kadri", "example": "Kadri", "list": True}]
    loops = [{"id": "l1", "enabled": True, "length": 3, "reps": 2, "blocks": [["s8", "s9", "s10"]]}]
    return {"name": "bills", "title": "Enter bills", "steps": steps, "params": params, "loops": loops}


class TheBlockThatMakesARecord(unittest.TestCase):
    def test_is_the_page_filled_with_values_the_request_does_not_give_from_its_opener_to_its_save(self):
        self.assertEqual(skills.maker_blocks(skill(), 8, {"month": "December 2026"}),
                         [{"first": 4, "last": 7, "params": ["customer_name", "customer_state"]}])

    def test_a_page_never_saved_is_no_such_block(self):
        s = skill()
        s["steps"][6]["target"]["name"] = "Next"
        self.assertEqual(skills.maker_blocks(s, 8, {"month": "December 2026"}), [])


class TheFirstStepsReplayed(unittest.TestCase):
    def run_for(self, context="The bills are saved in ~/work/dec/. Enter them."):
        return {"skill": skill(), "values": {"month": "December 2026"}, "start": 1, "context": context,
                "ladder": skill_task._ladder(skill()), "began": 0}

    def test_the_lead_steps_replay_and_what_they_end_on_is_the_report(self):
        run = self.run_for()
        ran = {}

        def replayed(program, params=None, start=1, ladder=None):
            ran["steps"] = [(s["n"], s["target"]["name"]) for s in program["steps"]]
            ran["params"] = params
            return {"outcome": skill_task.replay.VERIFIED, "text": ""}

        with mock.patch.object(skill_task, "_documents_in_hand", return_value=["/h/work/dec/1 A.docx"]), \
                mock.patch.object(skill_task.replay, "run", side_effect=replayed), \
                mock.patch.object(skill_task.core, "observe", return_value={"elements": [], "windows": []}), \
                mock.patch.object(skill_task.el, "render", return_value="row 1001 Kadri 5,000"), \
                mock.patch.object(skill_task, "_after_phase") as after, \
                mock.patch.object(skill_task.progress, "emit"):
            self.assertTrue(skill_task._gather_by_replay(run))
        self.assertEqual(ran["steps"], [(1, "Inbox"), (2, "Month"), (3, "Show")])
        self.assertEqual(ran["params"], {"month": "December 2026"})
        (args, _), = after.call_args_list
        self.assertEqual(args[1:2] + args[3:], ("gather", "final"))
        self.assertIn("row 1001 Kadri 5,000", args[2])
        self.assertEqual([m["numbers"] for m in run["makers"]], [[4, 5, 6, 7]])

    def test_a_replay_that_breaks_leaves_the_collecting_to_the_model(self):
        # Found (2026-11-17): the display failed under the first step and the whole task ended in an error.
        run = self.run_for()
        with mock.patch.object(skill_task, "_documents_in_hand", return_value=["/h/work/dec/1 A.docx"]),                 mock.patch.object(skill_task.replay, "run", side_effect=RuntimeError("X connection broke")),                 mock.patch.object(skill_task, "_after_phase") as after,                 mock.patch.object(skill_task.progress, "emit"):
            self.assertFalse(skill_task._gather_by_replay(run))
        after.assert_not_called()

    def test_no_documents_in_hand_or_a_stopped_replay_leaves_the_collecting_to_the_model(self):
        run = self.run_for(context="The bills came by mail.")
        with mock.patch.object(skill_task, "_documents_in_hand", return_value=[]):
            self.assertFalse(skill_task._gather_by_replay(run))
        run = self.run_for()
        with mock.patch.object(skill_task, "_documents_in_hand", return_value=["/h/work/dec/1 A.docx"]), \
                mock.patch.object(skill_task.replay, "run", return_value={"outcome": skill_task.replay.HALTED,
                                                                          "text": "no Inbox"}), \
                mock.patch.object(skill_task, "_after_phase") as after, \
                mock.patch.object(skill_task.progress, "emit"):
            self.assertFalse(skill_task._gather_by_replay(run))
        after.assert_not_called()


class ARecordMadeWhenMissing(unittest.TestCase):
    def setUp(self):
        s = skill()
        self.program = {"params": [{"name": "month", "step": 2}, {"name": "party__1", "step": 9},
                                   {"name": "party__2", "step": 12}],
                        "steps": [dict(x) for x in s["steps"][:7]]
                        + [dict(s["steps"][7], n=8), dict(s["steps"][8], n=9, param="party__1"),
                           dict(s["steps"][9], n=10),
                           dict(s["steps"][7], n=11), dict(s["steps"][8], n=12, param="party__2"),
                           dict(s["steps"][9], n=13)]}
        self.run = {"skill": s, "values": {"month": "December 2026"}, "gathered": "done", "context": "bills",
                    "documents": ["/h/1 Kadri.docx", "/h/2 Hebri.docx"], "left_out": [], "expanded": self.program,
                    "makers": [{"first": 4, "last": 7, "params": ["customer_name", "customer_state"],
                                "numbers": [4, 5, 6, 7]}], "start": 8}

    def make(self, read):
        executed = []
        with mock.patch.object(skill_task.binding, "fill_document", return_value=read) as fill, \
                mock.patch.object(skill_task, "_declare_made", return_value=False), \
                mock.patch.object(skill_task, "_execute", side_effect=lambda run: executed.append(dict(run))), \
                mock.patch.object(skill_task.progress, "emit"):
            done = skill_task._make_missing(self.run, 12, "gui: combobox \"Party\" (e5) has no option 'Hebri Co'.")
        return done, fill, executed

    def test_the_documents_record_is_made_before_its_block_and_the_replay_goes_on_from_it(self):
        done, fill, executed = self.make(({"customer_name": "Hebri Co"}, {"customer_name": "Bill to"}))
        self.assertTrue(done)
        self.assertEqual(fill.call_args.args[3], "/h/2 Hebri.docx")          # the second item's own document
        self.assertEqual(self.run["splices"], {"2": {"numbers": [4, 5, 6, 7], "document": "/h/2 Hebri.docx",
                                                     "values": {"customer_name": "Hebri Co", "customer_state": ""}}})
        self.assertEqual(len(executed), 1)
        self.assertEqual(self.run["gather_made"], 1)

        program, values = skill_task._spliced(self.run, self.program, {"month": "December 2026", "party__2": "Hebri Co"})
        names = [(s["n"], s["target"]["name"], s.get("param"), s.get("made_for")) for s in program["steps"]]
        self.assertEqual(names[10:16], [(11, "Create ledger", None, 2), (12, "Name", "customer_name__made2", 2),
                                        (13, "State", "customer_state__made2", 2), (14, "Save ledger", None, 2),
                                        (15, "Create voucher", None, None), (16, "Party", "party__2", None)])
        self.assertEqual(values["customer_name__made2"], "Hebri Co")
        self.assertEqual(values["customer_state__made2"], "")             # chosen at its step, never the demo's
        self.assertEqual({s.get("document") for s in program["steps"] if s.get("made_for")}, {"/h/2 Hebri.docx"})
        steps_of = {p["name"]: p["step"] for p in program["params"]}
        self.assertEqual((steps_of["party__2"], steps_of["customer_name__made2"]), (16, 12))

    def test_a_field_the_document_does_not_give_is_left_out_never_typed_with_the_demos_value(self):
        from decurix_agent import binding
        self.run["splices"] = {"2": {"numbers": [4, 5, 6, 7],
                                     "values": {"customer_name": "Hebri Co", "customer_state": binding.EMPTY_ITEM}}}
        program, values = skill_task._spliced(self.run, self.program, {"month": "December 2026"})
        made = [(s["n"], s["target"]["name"]) for s in program["steps"] if s.get("made_for")]
        self.assertEqual(made, [(11, "Create ledger"), (12, "Name"), (13, "Save ledger")])
        self.assertNotIn("customer_state__made2", values)
        self.assertNotIn("customer_state__made2", {p["name"] for p in program["params"]})

    def test_a_document_that_does_not_name_the_missing_entry_makes_nothing(self):
        done, _, executed = self.make(({"customer_name": "Someone Else"}, {}))
        self.assertFalse(done)
        self.assertEqual(executed, [])
        self.assertNotIn("splices", self.run)

    def test_a_value_the_block_types_that_the_document_lacks_makes_nothing(self):
        done, _, _ = self.make(({}, {}))
        self.assertFalse(done)

    def test_another_stop_or_a_record_made_already_is_repaired_as_any_other(self):
        executed = []
        with mock.patch.object(skill_task, "_execute", side_effect=executed.append):
            self.assertFalse(skill_task._make_missing(self.run, 12, "the button Save is not on the screen"))
            self.run["splices"] = {"2": {}}
            self.assertFalse(skill_task._make_missing(self.run, 12, "has no option 'Hebri Co'"))
        self.assertEqual(executed, [])


class AnItemTheyGaveInWords(unittest.TestCase):
    """Measured (2026-11-17): a bill given in an answer took the model 75 turns, entered click by click; the person's own
    per-item steps enter it once with the values the model reads from their words."""

    def test_one_items_block_may_be_run_alone(self):
        from decurix_agent.computer import skill_engine
        s = skill()
        self.assertEqual(skill_engine.item_block(s), (8, 10))
        self.assertTrue(skill_engine._one_item_block(s, {"party": "Hebri Co"}, 8, 10))
        self.assertFalse(skill_engine._one_item_block(s, {"party": "Hebri Co; Kadri"}, 8, 10))
        self.assertFalse(skill_engine._one_item_block(s, {"party": "Hebri Co"}, 4, 10))

    def test_the_rest_phase_is_given_the_command(self):
        s = skill()
        for p in s["params"]:
            p["varies"] = True
        run = {"skill": s, "values": {}, "by_hand": [{"document": "what you gave in your answer", "said": True,
                                                       "why": "Bill 1060 to Hebri Co"}]}
        (line,) = skill_task._given_item_how(run)
        self.assertIn("gui do bills --from 8 --to 10", line)
        self.assertIn("--param 'party=<Party>'", line)
        self.assertIn("(empty)", line)


class TheMadeRecordIsDeclared(unittest.TestCase):
    def test_declared_under_the_entry_it_makes_with_the_labels_its_steps_type_into(self):
        from decurix_agent.computer import records
        with mock.patch.object(records, "declare") as declare:
            ok = skill_task._declare_made(skill(), {"first": 4, "last": 7, "params": ["customer_name", "customer_state"]},
                                          {"customer_name": "Hebri Co", "customer_state": ""}, "Hebri Co",
                                          "/h/2 Hebri.docx")
        self.assertTrue(ok)
        declare.assert_called_once_with("Hebri Co", "/h/2 Hebri.docx", {"Name": "Hebri Co"})

    def test_a_declaration_refused_is_counted_as_the_steps_own_record(self):
        from decurix_agent.computer import records
        with mock.patch.object(records, "declare", side_effect=Exception("not in its document")):
            self.assertFalse(skill_task._declare_made(skill(), {"first": 4, "last": 7, "params": ["customer_name"]},
                                                      {"customer_name": "Hebri Co"}, "Hebri Co", "/h/2 Hebri.docx"))


if __name__ == "__main__":
    unittest.main()
