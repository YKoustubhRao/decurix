"""
A taught task done once per document, whose documents are not in hand: the model gathers them (the person's first
steps), the per-item lists are read from what it saved, the plan is put to the person while the skill is still being
learned, and the per-item block replays from its first step - the model on call where it stops (skill_task, "The
per-document part"). Measured before (2026-11-01): the model did every item by hand, 3-8M tokens a month, and the
fourth month cost what the first did.

    PYTHONPATH=agent python3 -m unittest agent.tests.test_per_document_run
"""

import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from test_loop_items import bills  # (imports test_recordings first: it stubs decurix_agent.config)

from decurix_agent import guidance, skill_task, state
from decurix_agent.computer import experience, learning, skills

REQUEST = "Sir, this month's bills are in the mail - 3 bills. Please enter them."


def folded():
    """The bills demonstration taught: one block per bill, its number and party lists."""

    skill = learning.build([bills(new_party_before=2)])
    learning.answer(skill, learning.next_question(skill), "x")
    learning.answer(skill, learning.next_question(skill), "yes")
    learning.answer(skill, learning.next_question(skill), "each")
    # The fresh form's address, typed alike before every bill, is asked "same for every line?" (learning._variable).
    asked = learning.next_question(skill)
    assert asked["kind"] == "variable" and "voucher/new" in asked["text"], asked
    learning.answer(skill, asked, "same")
    skill["status"] = skills.READY
    return skill


def list_param(skill, first_example):
    return next(p["name"] for p in skill["params"] if p.get("list") and p["examples"][0] == first_example)


class APerDocumentRun(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = skills.ROOT
        skills.ROOT = Path(self.tmp.name)
        state.paused_skill = None
        guidance.end()
        self.asked, self.finals, self.by_model, self.executed = [], [], [], []
        self.real_execute = skill_task._execute
        patches = [
            mock.patch.object(skill_task, "ask_for_query", lambda text, kind="query", engine=None, **kw: self.asked.append((text, kind))),
            mock.patch.object(skill_task, "write_final_reply", lambda text, kind="final", engine=None, **kw: self.finals.append((text, kind))),
            mock.patch.object(skill_task, "_by_model", lambda run, **kw: self.by_model.append((run, kw))),
            mock.patch.object(skill_task, "_execute", lambda run: self.executed.append(dict(run))),
            mock.patch.object(skill_task, "_engine_can_drive", return_value=True),
            mock.patch.object(skill_task.binding, "available", return_value=True),
            mock.patch.object(skill_task.progress, "emit"),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        self.skill = folded()
        self.number, self.party = list_param(self.skill, "412"), list_param(self.skill, "Bhadra")

    def tearDown(self):
        skills.ROOT = self.saved
        state.paused_skill = None
        guidance.end()
        self.tmp.cleanup()

    def run_for(self):
        return {"skill": self.skill, "values": skills.initial_values(self.skill, {}), "start": 1, "checked": False,
                "patches": [], "asked": 0, "stuck": 0, "ladder": skill_task._ladder(self.skill), "asked_ids": [],
                "context": REQUEST, "began": time.time()}

    def gathered(self, run):
        """The gather phase done: three bills saved to the month's folder, one of them cancelled."""

        now = time.time()
        folder = "/home/headless/PC/Clients/A/2026-27/9. DECEMBER"
        docs = {f"{folder}/1188 Coastal.docx": now + 1, f"{folder}/1189 Hebri.docx": now + 2, f"{folder}/1190 Mala.docx": now + 3}
        filled = ({self.number: "1188; 1189", self.party: "Coastal; Hebri"},
                  {self.number: "the bills' numbers", self.party: "the bills' Bill to"}, None,
                  [{"document": f"{folder}/1190 Mala.docx", "why": "cancelled - saved, not entered", "kind": "skip"}])
        with mock.patch.object(skill_task, "_document_files", return_value=dict(docs, **{"/home/headless/PC/old.pdf": 1.0})), \
                mock.patch.object(skill_task.binding, "fill_lists", return_value=filled) as fill:
            handled = skill_task._after_phase(run, "gather", "Saved 3 files to the December folder.", "final")
        return handled, fill, sorted(docs, key=docs.get)

    def test_a_gather_whose_documents_are_in_hand_only_prepares_the_application(self):
        # Found in the learning curves (2026-11-15): the request put the month's bills in a folder, and the phase spent
        # 100+ model turns downloading them again from the mail to "save every document".
        run = self.run_for()
        folder = "/home/headless/work/december"
        in_hand = [f"{folder}/1188 Coastal.docx", f"{folder}/1189 Hebri.docx"]
        run["context"] = f"This month's bills are in the mail; they are also saved in {folder}/. Please enter them."
        with mock.patch.object(skill_task, "_documents_in_hand", return_value=in_hand):
            told = skill_task._procedure(run, phase="gather")
        self.assertIn("already in hand - do not collect, download or save them again", told)
        self.assertIn("1188 Coastal.docx", told)
        self.assertIn("FULL PATH - read, not entered in this phase", told)
        self.assertNotIn("save every one of them", told)

    def test_a_request_without_its_documents_gathers_them_first_and_enters_nothing(self):
        run = self.run_for()
        with mock.patch.object(skill_task.binding, "fill", return_value=({}, {}, None)), \
                mock.patch.object(skill_task, "_document_files", return_value={"/home/headless/PC/old.pdf": 1.0}):
            skill_task._prepare(run)

        self.assertEqual(run["gathered"], "started")
        ((_, kw),) = self.by_model
        self.assertEqual(kw.get("phase"), "gather")
        self.assertEqual(self.executed, [])
        self.assertEqual(self.asked, [])
        told = skill_task._procedure(run, phase="gather")
        listed = told.split("The person's steps:")[1].split("How to work")[0]
        self.assertIn("1. click", listed)
        self.assertNotIn("2. ", listed)  # the per-item block is not this phase's
        self.assertIn("Do NOT enter", told)
        self.assertIn("FULL PATH - saved, not entered in this phase", told)
        self.assertIn("collect this request's documents", skill_task._engine_request(run, "gather"))

    def test_the_documents_fill_the_lists_and_the_plan_is_put_to_the_person_once(self):
        run = self.run_for()
        run["gathered"], run["files_before"] = "started", {"/home/headless/PC/old.pdf": 1.0}

        handled, fill, docs = self.gathered(run)

        self.assertTrue(handled)
        self.assertEqual((run["gathered"], run["start"], run["documents"]), ("done", 2, docs))
        self.assertEqual(fill.call_args.args[3], docs)                      # read in the order they were saved
        self.assertIn("1188; 1189", run["values"][self.number])
        self.assertEqual(self.by_model, [])
        self.assertEqual(self.executed, [])
        # Still learning the task: the plan goes to the person first - the documents, what was left out, each item.
        self.assertEqual(state.paused_skill["kind"], "plan")
        plan = self.asked[-1][0]
        self.assertTrue(plan.startswith("Before I change anything"))
        self.assertIn("3 document(s) collected", plan)
        self.assertIn("Left out (cancelled - saved, not entered): 1190 Mala.docx", plan)
        self.assertIn("Item 2: ", plan)
        self.assertIn("1189", plan)
        self.assertIn("from step 2, once per item", plan)

        # A plain yes starts the replay at the block's first step; the setup's values are the gather's, not asked for.
        with mock.patch.object(skill_task.approval, "reading", return_value={"decision": "yes"}):
            skill_task.resume("yes")
        self.assertEqual(len(self.executed), 1)
        self.assertEqual(self.executed[0]["start"], 2)
        self.assertEqual(len(self.asked), 1)
        self.assertEqual(self.by_model, [])

    def test_a_change_to_the_plan_hands_the_rest_to_the_model_with_the_persons_words(self):
        run = self.run_for()
        run["gathered"], run["files_before"] = "started", {}
        self.gathered(run)
        with mock.patch.object(skill_task.approval, "reading", return_value={"decision": "if", "condition": "use Hebri Agro Industries as the party"}):
            skill_task.resume("yes, but the party for 1189 is Hebri Agro Industries")
        self.assertEqual(self.executed, [])
        ((handed, kw),) = self.by_model
        self.assertIs(handed, run)
        self.assertEqual(run["start"], 1)
        self.assertIn("Hebri Agro Industries", kw["note"])
        self.assertTrue(run["asked_person"])

    def test_a_trusted_skill_replays_without_the_plan(self):
        for _ in range(experience.TRUST_AFTER):
            experience.note_verdict(self.skill, "replay", True, checked_by="screen", basis=["screen", "ledger"])
        run = self.run_for()
        run["gathered"], run["files_before"] = "started", {}
        self.gathered(run)
        self.assertEqual(self.asked, [])
        self.assertEqual(len(self.executed), 1)
        self.assertEqual(self.executed[0]["start"], 2)

    def test_a_gather_that_saved_nothing_leaves_the_task_to_the_model(self):
        run = self.run_for()
        run["gathered"], run["files_before"] = "started", {"/home/headless/PC/old.pdf": 1.0}
        with mock.patch.object(skill_task, "_document_files", return_value={"/home/headless/PC/old.pdf": 1.0}):
            handled = skill_task._after_phase(run, "gather", "I could not find the mail.", "final")
        self.assertTrue(handled)
        self.assertEqual(run["gathered"], "failed")
        ((_, kw),) = self.by_model
        self.assertIn("could not find the mail", kw["note"])
        self.assertEqual(self.executed, [])

    def test_a_gather_that_saved_nothing_new_uses_the_documents_the_request_names(self):
        # Found live (2026-11-08): the month's bills sat in the folder the request named, so the collection saved
        # nothing new, and "no document appeared" handed the whole task to the model from its first step.
        with tempfile.TemporaryDirectory() as folder:
            for name in ("1053 Pai.docx", "1054 Sharada.docx", "notes.bin"):
                (Path(folder) / name).write_text("x")
            run = self.run_for()
            run["context"] = REQUEST + f" The attachments are also saved in {folder}/."
            run["gathered"], run["files_before"] = "started", {}
            filled = ({self.number: "1053; 1054", self.party: "Pai; Sharada"}, {}, None, [])
            folder_re = skill_task.re.compile(r"(" + skill_task.re.escape(folder) + r"/?)")
            with mock.patch.object(skill_task, "_document_files", return_value={}), \
                    mock.patch.object(skill_task, "FOLDER_RE", folder_re), \
                    mock.patch.object(skill_task.binding, "fill_lists", return_value=filled) as fill:
                self.assertTrue(skill_task._after_phase(run, "gather", "Opened the mail; the bills are in the folder.", "final"))
        docs = [str(Path(folder) / "1053 Pai.docx"), str(Path(folder) / "1054 Sharada.docx")]
        self.assertEqual((run["gathered"], run["documents"]), ("done", docs))
        self.assertEqual(fill.call_args.args[3], docs)
        self.assertEqual(self.by_model, [])  # not the whole task to the model again
        self.assertEqual(state.paused_skill["kind"], "plan")

    def test_a_gather_that_failed_leaves_the_whole_task_to_the_model(self):
        run = self.run_for()
        run["gathered"] = "started"
        self.assertTrue(skill_task._after_phase(run, "gather", "the model stopped answering", "error"))
        self.assertEqual(run["gathered"], "failed")
        self.assertEqual(len(self.by_model), 1)

    def test_the_final_gate_routes_a_phase_report_to_the_run_and_reports_nothing(self):
        run = self.run_for()
        guidance.begin(skill=self.skill["name"], task="collect", text="", mode="phase", run=run, phase="gather")
        with mock.patch.object(skill_task, "_after_phase", return_value=True) as after:
            self.assertTrue(skill_task._gate("Saved 3 files.", "final", "thebot"))
        after.assert_called_once_with(run, "gather", "Saved 3 files.", "final")
        self.assertIsNone(guidance.get())
        self.assertEqual(self.finals, [])

    def test_the_per_item_block_is_the_largest_loop_and_addresses_are_not_asked_of_the_documents(self):
        # The real recording began with addresses typed into the browser (a 2-step repeat before the bills) and ended
        # by opening a report: neither is the per-item block, and no document holds an address.
        skill = learning.build([bills(nav_first=True)])
        learning.answer(skill, learning.next_question(skill), "x")
        learning.answer(skill, learning.next_question(skill), "yes")
        learning.answer(skill, learning.next_question(skill), "each")  # the address repeat
        learning.answer(skill, learning.next_question(skill), "each")  # the bills
        for param in skill["params"]:
            param["varies"], param["default"] = True, None  # (the rig's teaching answers "all change" to every field)
        skill["status"] = skills.READY
        self.skill = skill
        number, party = list_param(skill, "412"), list_param(skill, "Bhadra")
        block = next(s["n"] for s in skill["steps"] if s.get("param") == number) - 3  # the fresh form's address, Return, click No.
        self.assertEqual(skill_task._loop_start(skill), block)

        run = self.run_for()
        run["gathered"], run["start"] = "done", block
        self.assertEqual({p["name"] for p in skill_task._needed(run)}, {number, party})
        run["values"].update({number: "1188; 1189", party: "Coastal; Hebri"})
        skill_task._same_addresses(run)  # (as the gathered run does: the fresh form's address, once per item)
        # The address repeat has no items, so it is written out zero times and the numbers move: the replay still
        # begins at the block's first step.
        with mock.patch.object(skill_task, "_execute", self.real_execute), \
                mock.patch.object(skill_task.replay, "run", return_value={"outcome": "verified", "text": "ok", "total": 1}) as rep, \
                mock.patch.object(skill_task, "_finish"):
            skill_task._prepare(run)
        self.assertEqual(self.asked, [])
        program = rep.call_args.args[0]
        self.assertEqual(rep.call_args.kwargs["start"], next(s["n"] for s in program["steps"] if s.get("param") == f"{number}__1") - 3)

    def test_a_document_of_another_shape_is_entered_by_the_model_after_the_replay_and_judged_with_it(self):
        run = self.run_for()
        run["gathered"], run["files_before"] = "started", {}
        folder = "/home/headless/PC/Clients/A/2026-27/9. DECEMBER"
        docs = {f"{folder}/1188 Coastal.docx": time.time() + 1, f"{folder}/1189 Kasaragod.docx": time.time() + 2}
        filled = ({self.number: "1188", self.party: "Coastal"}, {}, None,
                  [{"document": f"{folder}/1189 Kasaragod.docx", "kind": "by_hand", "why": "inter-state: two tax rows, the taught shape has three"}])
        with mock.patch.object(skill_task, "_document_files", return_value=docs), \
                mock.patch.object(skill_task.binding, "fill_lists", return_value=filled):
            skill_task._after_phase(run, "gather", "Saved 2 files.", "final")
        self.assertEqual([x["document"] for x in run["by_hand"]], [f"{folder}/1189 Kasaragod.docx"])
        self.assertIn("Entered by hand after the others", self.asked[-1][0])
        self.assertIn("1189 Kasaragod.docx", self.asked[-1][0])

        # The replay ran its one item: before any judging, the set-aside document goes to the model, with the entered
        # ones named as done and the skipped ones as out.
        with mock.patch.object(skill_task, "_judge") as judge:
            skill_task._finish(run, {"text": "1 item entered", "outcome": "verified", "total": 7})
        judge.assert_not_called()
        ((handed, kw),) = self.by_model
        self.assertEqual(kw.get("phase"), "rest")
        told = skill_task._procedure(run, phase="rest")
        self.assertIn("ALREADY been run once per document for these documents, which are entered: 1188 Coastal.docx", told)
        self.assertIn("1189 Kasaragod.docx (inter-state", told)
        self.assertIn("enter by hand only the documents", skill_task._engine_request(run, "rest"))

        # Found live (2026-11-02): the model read the gather's "enter nothing" as a restriction on the whole task and
        # asked whether to undo the replay's vouchers. The phases are told in order.
        self.assertIn("that phase is OVER", told)
        self.assertIn("never undo, question or report them as a breach", told)
        self.assertIn("have since entered 1 of them", skill_task._engine_request(run, "rest"))

        # The model's report and the replay's result are judged as one; the person is asked when the check cannot tell.
        with mock.patch.object(skill_task.verify, "judge", return_value={"verdict": "unsure", "seen": "", "why": ""}) as judge, \
                mock.patch.object(skill_task.verify, "available", return_value=True), \
                mock.patch.object(skill_task, "evidence"):
            skill_task._after_phase(run, "rest", "Entered 1189 as voucher 1189, IGST 26,640.00, read back.", "final")
        self.assertEqual(run["by_hand"], [])
        # The check is given what code found about the files - here, that none of them is on this disk - in a slot of
        # its own, not among the person's words (found live 2026-11-02: there it read as one more claim).
        note = judge.call_args.kwargs["found"]
        self.assertNotIn("CHECKED IN CODE", judge.call_args.kwargs["criteria"])
        self.assertIn("CHECKED IN CODE, NOT CLAIMED", note)
        self.assertIn("0 of 2 present", note)
        self.assertIn("NOT present", note)
        self.assertIn("Entered by the model by hand, not by the taught steps: 1189 Kasaragod.docx (inter-state", note)
        # No third pass by the model over work already entered: the person is asked.
        self.assertEqual(state.paused_skill["kind"], "confirm")
        self.assertEqual(len(self.by_model), 1)
        self.assertIn("Entered by hand, outside the replay (1189 Kasaragod.docx): Entered 1189", self.asked[-1][0])

    def test_the_lists_are_read_without_rows_and_a_misaligned_list_is_dropped(self):
        # Found live (2026-11-02): the rows of one document were written over the lists.
        from decurix_agent import binding
        params = [p for p in self.skill["params"] if p.get("list")]
        answer = {"values": {self.number: ["1188", "1190", "1192"], self.party: ["Coastal"]},
                  "rows": [{"Amount": "5,850.00"}, {"Amount": "5,850.00"}],
                  "left_out": [{"document": "/d/1189.docx", "kind": "by_hand", "why": "inter-state"},
                               {"document": "/d/1185.docx", "kind": "skip", "why": "September"}]}
        files = ["/d/1188.docx", "/d/1189.docx", "/d/1190.docx", "/d/1192.docx", "/d/1185.docx"]
        with mock.patch.object(binding, "_ask", return_value=(answer, ["x"])) as ask:
            values, evidence, rows, left_out = binding.fill_lists(self.skill, params, REQUEST, files, rules="r")
        self.assertEqual(values, {self.number: "1188; 1190; 1192"})  # the party list has 1 item for 3 documents
        self.assertIsNone(rows)
        self.assertEqual([(x["document"], x["kind"]) for x in left_out], [("/d/1189.docx", "by_hand"), ("/d/1185.docx", "skip")])
        self.assertFalse(ask.call_args.kwargs.get("rows", True))

    def test_a_click_left_to_a_person_is_the_models_in_a_gathered_run(self):
        # Found live (2026-11-02): the replay stopped inside item 1 of 4 at "click 129,719 (a person does this one)".
        run = self.run_for()
        run["gathered"], run["start"] = "done", 2
        run["values"].update({self.number: "1188", self.party: "Coastal"})
        step = next(s for s in self.skill["steps"] if s["kind"] == "click" and (s.get("target") or {}).get("name") == "Accept")
        step["kind"], step["by_hand"], step["reason"] = "manual", "click 129,719", "a person does this one (click 129,719)"
        halted = {"outcome": "needs_human", "text": "a person has to do it", "step": step["n"], "resume_from": step["n"] + 1, "total": 7}
        with mock.patch.object(skill_task, "_execute", self.real_execute), \
                mock.patch.object(skill_task.replay, "run", return_value=halted), \
                mock.patch.object(skill_task, "_settle"):
            skill_task._prepare(run)
        ((handed, kw),) = self.by_model
        self.assertEqual(kw.get("from_step"), step["n"])
        # The model is given that one click as a bounded repair (repairs.py), and the replay goes on after it.
        from decurix_agent import repairs
        self.assertEqual(kw.get("phase"), repairs.REPAIR_PHASE)
        self.assertEqual(repairs.job(run)["step"]["by_hand"], "click 129,719")
        self.assertEqual(self.asked, [])  # nobody at the screen is asked to click

        # A secret a person types stays theirs.
        self.by_model.clear()
        step["by_hand"] = "type the portal password"
        with mock.patch.object(skill_task, "_execute", self.real_execute), \
                mock.patch.object(skill_task.replay, "run", return_value=halted), \
                mock.patch.object(skill_task, "_settle"):
            skill_task._prepare(run)
        self.assertEqual(self.by_model, [])
        self.assertEqual(state.paused_skill["kind"], "human")

    def test_an_address_typed_the_same_before_every_item_is_carried_once_per_item(self):
        skill = learning.build([bills(nav_first=True)])
        for q in ("x", "yes", "each", "each"):
            learning.answer(skill, learning.next_question(skill), q)
        for param in skill["params"]:
            param["varies"], param["default"] = True, None  # (the rig's teaching answers "all change" to every field)
        skill["status"] = skills.READY
        self.skill = skill
        number = list_param(skill, "412")
        form = next(p for p in skill["params"] if (p.get("examples") or [p.get("example")])[0] == "http://books.test/voucher/new")
        run = self.run_for()
        run["gathered"], run["start"] = "done", skill_task._loop_start(skill)
        run["values"][number] = "1188; 1189; 1190"
        skill_task._same_addresses(run)
        self.assertEqual(run["values"][form["name"]], "; ".join(["http://books.test/voucher/new"] * 3) if form.get("list") else "http://books.test/voucher/new")
        # The addresses typed differently before the first item (the setup's) are not the block's and stay unfilled.
        self.assertFalse(any(v for k, v in run["values"].items() if "mail.test" in str(v) or "report/monthly" in str(v)))

    def test_the_saved_documents_are_a_fact_code_decides_for_the_check_and_the_report(self):
        # Found live (2026-11-02): three months of four ended with "cannot confirm - the observations do not verify that
        # all six files are in the folder", and a person had to look; the files were there.
        folder = Path(self.tmp.name) / "9. DECEMBER"
        folder.mkdir()
        (folder / "1188 Coastal.docx").write_bytes(b"x" * 1200)
        (folder / "1189 Hebri.docx").write_bytes(b"y" * 800)
        run = self.run_for()
        self.assertEqual(skill_task._documents_note(run), "")
        run["documents"] = [str(folder / "1188 Coastal.docx"), str(folder / "1189 Hebri.docx"), str(folder / "1190 Mala.docx")]
        run["left_out"] = [{"document": run["documents"][2], "why": "cancelled", "kind": "skip"}]
        note = skill_task._documents_note(run)
        self.assertIn("2 of 3 present under " + str(folder), note)
        self.assertIn("1188 Coastal.docx (1,200 bytes)", note)
        self.assertIn("NOT present (saved during the task and gone since): 1190 Mala.docx.", note)
        self.assertIn("Left out under the person's rules (saved, not entered): 1190 Mala.docx (cancelled).", note)

        # The same facts close the report to the person, in plain words.
        run["gathered"], run["start"] = "done", 2
        with mock.patch.object(skill_task.experience, "commit", return_value=[]), \
                mock.patch.object(skill_task.experience, "advice", return_value=""), \
                mock.patch.object(skill_task, "_own_run"):
            skill_task._succeed(run, {"text": "2 items entered", "outcome": "verified", "total": 7}, "screen")
        ((text, kind),) = self.finals
        self.assertEqual(kind, "final")
        self.assertIn("Checked on this desktop: this request's document files", text)
        self.assertNotIn("CHECKED IN CODE", text)

    def test_a_month_the_model_did_itself_after_the_gather_is_judged_with_the_files_too(self):
        folder = Path(self.tmp.name) / "NOV 2026"
        folder.mkdir()
        (folder / "0199.pdf").write_bytes(b"x" * 900)
        run = self.run_for()
        run["gathered"], run["documents"] = "failed", [str(folder / "0199.pdf")]
        with mock.patch.object(skill_task.verify, "judge", return_value={"verdict": "done", "seen": "", "why": ""}) as judge, \
                mock.patch.object(skill_task, "evidence"):
            skill_task._judge(run, "Entered 0199.", note="These values of this run were never typed: x = 1")
        found = judge.call_args.kwargs["found"]
        self.assertIn("never typed: x = 1", found)
        self.assertIn("1 of 1 present under " + str(folder), found)

    def test_the_lists_fill_never_sets_a_document_aside_for_a_party_it_cannot_see_in_the_books(self):
        # Found live (2026-11-02): every document of a run was set aside "by hand" because the party's record could
        # not be seen from the documents, and the model did the whole run itself.
        from decurix_agent import binding
        self.assertIn("never a reason for \"by_hand\" or \"skip\"", binding.LISTS_NOTE)
        self.assertIn("chooses from the application's own lists", binding.LISTS_NOTE)

    def test_a_value_numbered_like_the_example_by_nothing_of_this_run_is_kept_as_an_assumption(self):
        from decurix_agent import binding
        param = {"name": "folder_name", "example": "6. SEPTEMBER"}
        texts = ["Please enter this month's bills for November."]
        self.assertTrue(binding.numbered_like_example("8. NOVEMBER", param, texts))
        values, said = binding._clean([param], {"values": {"folder_name": "8. NOVEMBER"}}, texts)
        self.assertEqual(values, {"folder_name": "8. NOVEMBER"})
        self.assertIn(binding.NUMBERED, said["folder_name"])
        # Held by what the run was given, it is not an assumption; nor a word alone, a date, or the example's own number.
        self.assertFalse(binding.numbered_like_example("8. NOVEMBER", param, texts + ["Folders: 7. OCTOBER, 8. NOVEMBER"]))
        self.assertFalse(binding.numbered_like_example("NOVEMBER", param, texts))
        self.assertFalse(binding.numbered_like_example("12-Nov-2026", {"name": "bill_date", "example": "2-Sep-2026"}, texts))
        self.assertFalse(binding.numbered_like_example("29-Kerala", {"name": "pos", "example": "29-Karnataka"}, texts))

        # The final check is told it is an assumption, in the facts slot, beside nothing of the person's.
        run = self.run_for()
        run["values"]["folder_name"], run["evidence"] = "8. NOVEMBER", said
        with mock.patch.object(skill_task.verify, "judge", return_value={"verdict": "done", "seen": "", "why": ""}) as judge, \
                mock.patch.object(skill_task, "evidence"):
            skill_task._judge(run, "Filed and entered.")
        found = judge.call_args.kwargs["found"]
        self.assertIn("WORKED OUT by the harness", found)
        self.assertIn("folder_name = 8. NOVEMBER", found)

    def test_a_gathered_run_is_the_replays_and_needs_only_the_blocks_values(self):
        run = self.run_for()
        run["gathered"], run["start"] = "done", 2
        self.assertFalse(skill_task._learning_by_model(run))
        self.assertEqual({p["name"] for p in skill_task._needed(run)}, {self.number, self.party})
        run["gathered"] = None
        self.assertTrue(skill_task._learning_by_model(run))


if __name__ == "__main__":
    unittest.main()
