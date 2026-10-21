"""
Finding BF (2026-10-20, P1 on a shared-folder worker): the plan named the deck "Documents/Decurix demo/process/P1/
MIS_Oct2026.pptx" - the person's folder - and it was taken as a folder of the work folder. PowerPoint saved the
person's file where asked (07:01 on the PC); the stage read a stale copy in ~/work/Documents/... and reported that the
save had not taken. A planner's path in the shared folder is now that folder, through the work folder's PC link; and a
save in a host app says which file of the shared folder it wrote, or that none changed.
"""

import os
import sys
import time
import unittest

sys.path.insert(0, os.path.dirname(__file__))

from test_shared_folder import HOST, SharedBase

from decurix_agent import long_task, shared
from decurix_agent.computer import core
from decurix_agent.workflow_runner import artifact_evidence


class APlannedOutputInTheSharedFolder(SharedBase):
    def test_every_way_a_plan_names_the_folder(self):
        for written, want in (("Documents/Decurix demo/process/P1/MIS_Oct2026.pptx", "PC/process/P1/MIS_Oct2026.pptx"),
                              ("Decurix demo/memo.docx", "PC/memo.docx"),
                              (HOST + "\\process\\P1\\MIS_Oct2026.pptx", "PC/process/P1/MIS_Oct2026.pptx"),
                              ("~/PC/out.xlsx", "PC/out.xlsx")):
            self.assertEqual(shared.plan_path(written), want, written)
        for other in ("P1_month_end/outputs/month_end_close_Oct2026.xlsx", "Documents/Other/x.pptx", "process/P1/x.pptx"):
            self.assertIsNone(shared.plan_path(other), other)

    def test_the_plan_the_stage_prompt_and_the_check_all_use_the_persons_file(self):
        plan = {"stages": [{"id": "deck", "title": "MIS deck", "does": "Make the deck.",
                            "outputs": [{"file": "Documents/Decurix demo/process/P1/MIS_Oct2026.pptx"}]},
                           {"id": "wb", "title": "Workbook", "does": "Make it.",
                            "outputs": [{"file": "P1_month_end/outputs/close.xlsx"}]}]}
        spec = long_task.spec_from_plan(plan, request="Save the deck in Documents\\Decurix demo\\process\\P1 on my PC.")
        deck = spec["steps"][0]
        self.assertEqual(deck["outputs"], ["PC/process/P1/MIS_Oct2026.pptx"])
        self.assertEqual(deck["checks"][0]["path"], "PC/process/P1/MIS_Oct2026.pptx")

        # Through the work folder's PC link, the check reads the person's file - not a copy in the work folder.
        work = self.root / "work"
        self.assertIn("linked", shared.ensure_work_link(work))
        stale = work / "Documents" / "Decurix demo" / "process" / "P1"
        stale.mkdir(parents=True)
        (stale / "MIS_Oct2026.pptx").write_bytes(b"old")
        real = self.mount / "process" / "P1"
        real.mkdir(parents=True)
        import zipfile
        with zipfile.ZipFile(real / "MIS_Oct2026.pptx", "w") as z:
            z.writestr("[Content_Types].xml", "<Types/>")
            z.writestr("ppt/presentation.xml", '<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>')
            z.writestr("ppt/slides/slide1.xml", '<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" '
                                                'xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><a:t>MIS October</a:t></p:sld>')
        evidence = artifact_evidence(deck, {}, {"work_dir": str(work)})
        self.assertEqual(evidence[0]["path"], "PC/process/P1/MIS_Oct2026.pptx")
        self.assertGreater(evidence[0]["bytes"], 3)

    def test_a_link_out_of_both_folders_is_still_refused(self):
        work = self.root / "work"
        outside = self.root / "elsewhere"
        outside.mkdir()
        (outside / "x.csv").write_text("a\n1\n", encoding="utf-8")
        (work / "sneaky").symlink_to(outside, target_is_directory=True)
        with self.assertRaises(ValueError):
            artifact_evidence({"checks": [{"path": "sneaky/x.csv", "format": "csv"}]}, {}, {"work_dir": str(work)})


class ASaveInAHostAppSaysWhereItWent(SharedBase):
    def test_the_file_it_wrote_in_the_shared_folder_is_named(self):
        since = time.time()
        self.put("process/P1/MIS_Oct2026.pptx", b"PK new")
        note = core._shared_save_note(since, wait=0)
        self.assertIn("Saved in the person's shared folder: " + HOST + "\\process\\P1\\MIS_Oct2026.pptx", note)

    def test_nothing_changed_is_said_plainly(self):
        self.put("process/P1/MIS_Oct2026.pptx", b"PK old", age=3600)
        note = core._shared_save_note(time.time(), wait=0)
        self.assertIn("No file in the person's shared folder changed with this save", note)
        self.assertIn("a dialog may still be open", note)

    def test_a_file_still_open_in_an_app_is_named(self):
        self.put("process/P1/MIS_Oct2026.pptx", b"PK old", age=3600)
        self.put("process/P1/~$MIS_Oct2026.pptx", b"owner", age=3600)
        note = core._shared_save_note(time.time(), wait=0)
        self.assertIn("MIS_Oct2026.pptx is open in an app (its lock file ~$... is there)", note)

    def test_what_counts_as_a_save(self):
        self.assertTrue(core._saves("key", None, ["ctrl+s"]))
        self.assertTrue(core._saves("click", {"name": "Save"}, None))
        self.assertTrue(core._saves("click", {"name": "Yes"}, None))
        self.assertFalse(core._saves("click", {"name": "OK"}, None))
        self.assertFalse(core._saves("key", None, ["Return"]))


if __name__ == "__main__":
    unittest.main()
