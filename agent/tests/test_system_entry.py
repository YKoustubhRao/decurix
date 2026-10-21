"""
CR (round 5 P2 on pc3, 2026-10-20, run 5dd27a2c): "Enter every approved order in the portal and log entries" was
reported done, "checked by reading the files back" - the portal had no order and the run no screen action; the worker
had written a log claiming the entries (its instruction had been restated after an answer as only "Include Cafe Bloom
... Exclude Harbour ..."). A record entered in a system is done only with that system's own evidence from the tools.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import long_task

REQUEST = ("Today's orders please. Enter every approved order in our Back Office portal (http://host.docker.internal:8701, "
           "user clerk, Orders) and log what you entered.")
STEP = {"id": "s4-enter-approved-orders", "title": "Enter every approved order in the portal and log entries",
        "instruction": "Include Cafe Bloom - WA-1810 because you said: \"Release Cafe Bloom this once\"."}
# The live log (portal_entry_log.csv), its rows as the stage wrote them.
LOG = """source_order_identifier,customer,customer_po,order_date,portal_action,credit_decision
HH/PO/7781,Harbour Hotels Group,HH/PO/7781,18/10/2026,Excluded from portal entry,Held
WA-1810-CB,Cafe Bloom,WA-1810-CB,18/10/2026,Include in portal entry,Released
WA-1810-MLB,Maple Leaf Bakery,WA-1810-MLB,18/10/2026,Include in portal entry,Approved
WA-1810-LYC,Lotus Yoga Centre,WA-1810-LYC,18/10/2026,Include in portal entry,Approved
WA-1910-SFG,Shaw Fitness Gym,WA-1910-SFG,19/10/2026,Include in portal entry,Approved
OS/PUR/2026/118,Oakwood School,OS/PUR/2026/118,19/10/2026,Include in portal entry,Approved
"""
PORTAL = {"kind": "click", "outcome": "done", "summary": "click \"Save order\"",
          "expect": {"active_window": "New order - Northwind Back Office - Chromium"}}
LIST = ("Orders | SO-5101 | Cafe Bloom | WA-1810-CB | 18/10/2026 || SO-5102 | Maple Leaf Bakery | WA-1810-MLB || "
        "SO-5103 | Lotus Yoga Centre | WA-1810-LYC || SO-5104 | Shaw Fitness Gym | WA-1910-SFG || SO-5105 | Oakwood School "
        "| OS/PUR/2026/118")


ENTERED = """customer_po,customer,portal_status
WA-1810-CB,Cafe Bloom,Entered SO-5101
WA-1810-MLB,Maple Leaf Bakery,Entered SO-5102
WA-1810-LYC,Lotus Yoga Centre,Entered SO-5103
WA-1910-SFG,Shaw Fitness Gym,Entered SO-5104
OS/PUR/2026/118,Oakwood School,Entered SO-5105
HH/PO/7781,Harbour Hotels Group,Not entered - held
"""


class SystemEntry(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.log = Path(tmp.name) / "portal_entry_log.csv"
        self.log.write_text(LOG, encoding="utf-8")
        self.entered = Path(tmp.name) / "entered_log.csv"
        self.entered.write_text(ENTERED, encoding="utf-8")

    def check(self, record):
        return long_task.system_entry_problems("run", STEP, [str(self.log)], REQUEST, {"t": 0}, record=record)

    def test_a_log_with_no_action_on_the_portal_is_refused(self):
        found = self.check(([], ""))
        self.assertEqual(len(found), 1, found)
        self.assertIn("the tools took no action on Back Office portal", found[0])
        # (Its log claims no entry - "Include in portal entry" is a plan - and the stage still entered nothing.)

    def test_actions_and_a_read_back_showing_each_order_pass(self):
        found = long_task.system_entry_problems("run", STEP, [str(self.entered)], REQUEST, {"t": 0}, record=([PORTAL], LIST))
        self.assertEqual(found, [])

    def test_a_read_back_missing_one_is_refused_naming_it(self):
        found = long_task.system_entry_problems("run", STEP, [str(self.entered)], REQUEST, {"t": 0},
                                                record=([PORTAL], LIST.replace("|| SO-5105 | Oakwood School | OS/PUR/2026/118", "")))
        self.assertEqual(len(found), 1, found)
        self.assertIn("does not show: OS/PUR/2026/118", found[0])

    def test_a_stage_entering_nothing_in_a_system_is_not_checked(self):
        step = {"id": "s3", "title": "Make the dispatch plan", "instruction": "Allocate stock and make dispatch_plan.xlsx."}
        self.assertEqual(long_task.system_entry_problems("run", step, [str(self.log)], REQUEST, {"t": 0},
                                                         record=([], "")), [])
        self.assertEqual(long_task.system_entry_problems("run", STEP, [str(self.log)], "Make a log of the orders.",
                                                         {"t": 0}, record=([], "")), [])

    def test_the_onboarding_portal_shape_passes_with_its_entries(self):
        # P4: four joiners onboarded in the HR portal, each read back from its list.
        request = "Onboard them in our HR portal (http://host.docker.internal:8702) and keep a log."
        step = {"id": "s4", "title": "Onboard everyone whose papers are complete in the portal",
                "instruction": "Create each joiner's record in the HR portal and log the employee ids."}
        log = Path(self.log.parent) / "portal_onboarding_log.csv"
        log.write_text("Employee ID,Name,Status\nEMP-2041,Ananya Deshpande,Created\nEMP-2042,Rohit Kulkarni,Created\n",
                       encoding="utf-8")
        action = {"kind": "type", "outcome": "done", "summary": "type e12 \"Ananya Deshpande\"",
                  "expect": {"active_window": "Employees - HR - Chromium"}}
        seen = "Employees | EMP-2041 Ananya Deshpande | EMP-2042 Rohit Kulkarni"
        self.assertEqual(long_task.system_entry_problems("run", step, [str(log)], request, {"t": 0},
                                                         record=([action], seen)), [])
        self.assertEqual(len(long_task.system_entry_problems("run", step, [str(log)], request, {"t": 0},
                                                             record=([], ""))), 1)

    def test_only_the_stages_own_order_to_enter_makes_it_an_entry_stage(self):
        # Round 5 P2 on pc3 (2026-10-21): stage 1's own text, refused twice for entering nothing.
        stage1 = {"id": "s1", "title": "Extract every order line and pricing basis into a working workbook",
                  "instruction": "Read the 4 WhatsApp chat exports ... and create /home/headless/work/P2_orders_19Oct/"
                                 "orders_working.xlsx with a sheet for every order line ... Use the request's order "
                                 "scope of every order line and every customer PO or chat; do not allocate stock yet and "
                                 "do not enter anything in the portal yet."}
        stage2 = {"id": "s2", "title": "List orders that must be held for credit-limit approval and ask the user",
                  "instruction": "Do not allocate stock, do not enter any order in the portal, and do not write to any "
                                 "customer in this stage."}
        mention = {"id": "s3", "title": "Make the dispatch plan",
                   "instruction": "Keep the portal's SO number column for later portal entry; look up each order in the "
                                  "portal if needed."}
        for step in (stage1, stage2, mention):
            self.assertEqual(long_task.enters_into_system(step, REQUEST), "", step["id"])
            self.assertEqual(long_task.system_entry_problems("run", step, [str(self.log)], REQUEST, {"t": 0},
                                                             record=([], "")), [])
        stage4 = {"id": "s4", "title": "Enter every approved order in the portal and save an entry log",
                  "instruction": "Read orders_working.xlsx and dispatch_plan.xlsx, then enter every approved order in "
                                 "the Northwind Back Office portal (http://host.docker.internal:8701, user clerk)."}
        self.assertTrue(long_task.enters_into_system(stage4, REQUEST))
        self.assertTrue(long_task.enters_into_system(STEP, REQUEST))  # the CR stage, by its title

    def test_only_rows_a_status_says_were_entered_are_claimed(self):
        # The live CR log's "Include in portal entry" is a plan: nothing is claimed from it.
        self.assertEqual(long_task._claimed_records([str(self.log)]), [])
        done = Path(self.log.parent) / "entered.csv"
        done.write_text("Order,Customer,Portal status\nWA-1810-CB,Cafe Bloom,Entered as SO-5101\n"
                        "HH/PO/7781,Harbour Hotels Group,Not entered - held\n", encoding="utf-8")
        self.assertEqual([c[0] for c in long_task._claimed_records([str(done)])], ["WA-1810-CB (Cafe Bloom)"])

    def test_it_blocks(self):
        self.assertTrue(long_task.is_blocking("system_entry"))


class TheRestatementKeepsTheWork(unittest.TestCase):
    def test_a_restatement_that_drops_the_entry_is_a_note_on_it(self):
        before = "Enter every approved order in the portal (http://host.docker.internal:8701) and log what was entered."
        state = {"steps": {"s4": {"status": "pending"}}, "spec": {"steps": [{"id": "s4", "title": "Enter orders",
                                                                          "instruction": before}]}}
        revised = {}

        class Runner:
            def status(self, run_id):
                return state

            def revise(self, run_id, step_id, title=None, instruction=None):
                revised[step_id] = instruction
        restated = {"stages": [{"id": "s4", "does": "Include Cafe Bloom - WA-1810. Exclude Harbour Hotels Group."}]}
        with mock.patch.object(long_task, "_runner", lambda: Runner()), \
                mock.patch.object(long_task, "_ask_json", lambda *a, **k: restated), \
                mock.patch.object(long_task, "_charge", lambda *a: None), \
                mock.patch.object(long_task, "_update", lambda run_id, change: None), \
                mock.patch.object(long_task, "restatement_adds", lambda *a: []):
            long_task.rescope_after_answer("run", "Release Cafe Bloom?", "Release Cafe Bloom this once")
        self.assertTrue(revised["s4"].startswith(before), revised)
        self.assertIn("After the person's answer: Include Cafe Bloom", revised["s4"])


if __name__ == "__main__":
    unittest.main()
