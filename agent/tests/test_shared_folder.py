"""
The worker's shared folder: one folder of the person's computer, mounted at /mnt/pc and linked at ~/PC. A path the person
writes under it - any case, / or \\, or the WSL spelling - is that file here; the gate reads it where it is instead of
asking the host; nothing outside the mount is ever read through it.
"""

import os
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

from decurix_agent import config, produced, shared, verify
from decurix_agent.tools import office

HOST = "C:\\Users\\Administrator\\Documents\\Decurix demo"
SPELLINGS = [
    HOST + "\\orders_oct.xlsx",
    "c:/users/administrator/documents/decurix demo/orders_oct.xlsx",
    "C:/Users/Administrator/Documents/Decurix demo\\orders_oct.xlsx",
    "C:\\USERS\\ADMINISTRATOR\\DOCUMENTS\\DECURIX DEMO\\Orders_Oct.xlsx",
    "/mnt/c/Users/Administrator/Documents/Decurix demo/orders_oct.xlsx",
    "\"" + HOST + "\\orders_oct.xlsx\"",
]


class SharedBase(unittest.TestCase):
    """A worker that shares HOST: the mount is a folder of its own, and ~/PC is that same folder."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(os.path.realpath(tmp.name))
        self.mount = self.root / "mnt-pc"
        self.mount.mkdir()
        env = {shared.ENV_FOLDER: HOST, shared.ENV_MOUNT: str(self.mount), shared.ENV_LINK: str(self.mount)}
        patch = mock.patch.dict(os.environ, env)
        patch.start()
        self.addCleanup(patch.stop)
        # Hermetic: the work folder and the agent's own folder are this test's too, so nothing written elsewhere on the
        # machine meanwhile (the repo's docs, another test's files) can be taken for what a task made.
        for name, value in (("WORK_DIR", self.root / "work"), ("DECURIX_DIR", self.root / "decurix")):
            value.mkdir()
            patch = mock.patch.object(config, name, value)
            patch.start()
            self.addCleanup(patch.stop)

    def put(self, name, data=b"x", age=0):
        path = self.mount / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        when = time.time() - age
        os.utime(path, (when, when))
        return path


class WhereAPathIs(SharedBase):
    def test_every_spelling_of_a_path_under_the_folder_is_the_same_file_here(self):
        for text in SPELLINGS:
            with self.subTest(text=text):
                local = shared.to_local(text)
                self.assertIsNotNone(local)
                self.assertEqual(Path(local).parent, self.mount)
                self.assertEqual(Path(local).name.lower(), "orders_oct.xlsx")

    def test_a_folder_below_it_and_the_folder_itself(self):
        self.assertEqual(shared.to_local(HOST), str(self.mount))
        self.assertEqual(shared.to_local(HOST + "\\"), str(self.mount))
        self.assertEqual(Path(shared.to_local(HOST + "\\2026\\q3\\x.csv")), self.mount / "2026" / "q3" / "x.csv")
        self.assertEqual(shared.to_local(str(self.mount / "x.csv")), str(self.mount / "x.csv"))

    def test_what_is_not_under_the_folder_is_not_mapped(self):
        for text in ("C:\\Users\\Administrator\\Documents\\Other\\x.xlsx",
                     "C:\\Users\\Administrator\\Documents\\Decurix demo2\\x.xlsx",
                     "C:\\Users\\Administrator\\Documents", "C:\\Windows\\win.ini", "/etc/passwd", "", None):
            self.assertIsNone(shared.to_local(text), text)

    def test_a_path_that_goes_up_a_folder_is_never_mapped(self):
        self.assertIsNone(shared.to_local(HOST + "\\..\\Secrets\\x.txt"))
        self.assertIsNone(shared.to_local(HOST + "/a/../../x.txt"))

    def test_a_worker_with_no_shared_folder_maps_nothing(self):
        with mock.patch.dict(os.environ, {shared.ENV_FOLDER: ""}):
            self.assertIsNone(shared.to_local(HOST + "\\orders_oct.xlsx"))
            self.assertFalse(shared.active())

    def test_a_posix_folder_is_matched_as_it_is_written(self):
        with mock.patch.dict(os.environ, {shared.ENV_FOLDER: "/Users/ann/Projects/Shared"}):
            self.assertIsNotNone(shared.to_local("/Users/ann/Projects/Shared/x.xlsx"))
            self.assertIsNone(shared.to_local("/users/ann/projects/shared/x.xlsx"))


class AFolderLinkCannotLeadOut(SharedBase):
    def test_a_link_inside_the_folder_to_somewhere_else_is_not_followed(self):
        outside = self.root / "outside"
        outside.mkdir()
        (outside / "secret.txt").write_text("the person's other files")
        try:
            os.symlink(outside, self.mount / "escape", target_is_directory=True)
        except (OSError, NotImplementedError):
            self.skipTest("this host cannot make symlinks")
        self.assertIsNone(shared.to_local(HOST + "\\escape\\secret.txt"))
        self.assertFalse(shared.contained(self.mount / "escape" / "secret.txt"))
        self.assertTrue(shared.contained(self.mount / "fine.txt"))
        names = {p.name for p in shared.walk()}
        self.assertNotIn("secret.txt", names)
        with self.assertRaises(office.OfficeError):
            office.read_document(HOST + "\\escape\\secret.txt")


class TheLink(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(os.path.realpath(tmp.name))
        self.mount, self.link = self.root / "mnt-pc", self.root / "PC"
        self.mount.mkdir()
        patch = mock.patch.dict(os.environ, {shared.ENV_FOLDER: HOST, shared.ENV_MOUNT: str(self.mount),
                                             shared.ENV_LINK: str(self.link)})
        patch.start()
        self.addCleanup(patch.stop)
        try:
            probe = self.root / "probe"
            os.symlink(self.mount, probe, target_is_directory=True)
            probe.unlink()
        except (OSError, NotImplementedError):
            self.skipTest("this host cannot make symlinks")

    def test_pc_is_linked_to_the_mount_and_files_are_found_through_it(self):
        self.assertIn("linked", shared.ensure_link())
        self.assertTrue(self.link.is_symlink())
        (self.mount / "x.xlsx").write_bytes(b"PK\x03\x04")
        local = shared.to_local(HOST + "\\x.xlsx")
        self.assertEqual(local, str(self.link / "x.xlsx"))
        self.assertTrue(os.path.isfile(local))
        self.assertEqual(shared.ensure_link(), "linked")  # again: nothing to change

    def test_a_stale_link_is_replaced_and_a_real_folder_is_left_alone(self):
        os.symlink(self.root, self.link, target_is_directory=True)
        shared.ensure_link()
        self.assertEqual(os.path.realpath(self.link), os.path.realpath(self.mount))
        self.link.unlink()
        self.link.mkdir()
        (self.link / "mine.txt").write_text("the worker's own")
        self.assertIn("not a link", shared.ensure_link())
        self.assertTrue((self.link / "mine.txt").exists())

    def test_without_a_shared_folder_a_leftover_link_is_removed(self):
        shared.ensure_link()
        with mock.patch.dict(os.environ, {shared.ENV_FOLDER: ""}):
            self.assertIn("removed a link", shared.ensure_link())
        self.assertFalse(self.link.is_symlink())


class TheFileAsTheControlPlaneWouldSayIt(SharedBase):
    def test_stat_answers_like_the_host_route(self):
        self.put("book.xlsx", b"PK\x03\x04rest")
        self.put("pic.png", b"\x89PNG\r\n\x1a\nxx")
        self.put("note.txt", b"hello")
        self.put("old.doc", b"\xd0\xcf\x11\xe0abcd")
        self.put("scan.pdf", b"%PDF-1.7")
        kinds = {n: shared.stat(self.mount / n)["kind"] for n in ("book.xlsx", "pic.png", "note.txt", "old.doc", "scan.pdf")}
        self.assertEqual(kinds, {"book.xlsx": "zip", "pic.png": "other", "note.txt": "text", "old.doc": "ole", "scan.pdf": "pdf"})
        answer = shared.stat(self.mount / "book.xlsx")
        self.assertEqual((answer["exists"], answer["folder"], answer["size"]), (True, False, 8))
        self.assertRegex(answer["modified"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        self.assertEqual(shared.stat(self.mount)["folder"], True)
        self.assertEqual(shared.stat(self.mount / "missing.xlsx"), {"path": str(self.mount / "missing.xlsx"), "exists": False})


class EveryRouteReadsTheSharedFile(SharedBase):
    def test_office_reads_and_writes_the_path_the_person_wrote(self):
        self.put("people.csv", b"name,age\nAnn,31\n")
        for spelling in ("C:\\Users\\Administrator\\Documents\\Decurix demo\\people.csv",
                         "c:/users/administrator/documents/decurix demo/PEOPLE.CSV"):
            self.assertIn("Ann", office.read_document(spelling)["text"], spelling)
        made = office.create_document(HOST + "\\letter.docx", [{"paragraph": "Dear Ann"}])
        self.assertTrue((self.mount / "letter.docx").exists(), made)

    def test_the_gate_asks_the_disk_not_the_host_for_a_file_saved_in_the_folder(self):
        request = ("Make the summary in Excel and save it as report.xlsx in the Decurix demo folder in my Documents (%s)." % HOST)
        self.put("report.xlsx", b"PK\x03\x04data")
        with mock.patch.object(produced, "since", lambda: time.time() - 60), \
                mock.patch("urllib.request.urlopen", side_effect=AssertionError("the host was asked")):
            self.assertEqual(produced.missing_on_host(request), "")
            self.assertEqual(produced.saved_on_host(request), [HOST + "\\report.xlsx"])
            copies = produced.copy_from_host(produced.saved_on_host(request),
                                             fetch=lambda p: self.fail("fetched through the host"))
        self.assertEqual(copies, [self.mount / "report.xlsx"])

    def test_the_gate_still_finds_a_file_not_there_a_wrong_kind_and_an_old_one(self):
        request = ("Save it as report.xlsx in the Decurix demo folder in my Documents (%s)." % HOST)
        with mock.patch.object(produced, "since", lambda: time.time() - 60), \
                mock.patch("urllib.request.urlopen", side_effect=AssertionError("the host was asked")):
            self.assertIn("report.xlsx is not in " + HOST, produced.missing_on_host(request))
            self.put("report.xlsx", b"a,b\n1,2\n")  # a CSV under a workbook's name
            self.assertIn("not a real xlsx file", produced.missing_on_host(request))
            self.put("report.xlsx", b"PK\x03\x04data", age=3600)
            self.assertIn("was last written before this task began", produced.missing_on_host(request))
            self.put("report.xlsx", b"PK\x03\x04data")
            self.assertEqual(produced.missing_on_host(request), "")

    def test_a_path_outside_the_folder_is_still_asked_of_the_host(self):
        asked = []
        with mock.patch.object(produced, "_ask_host", wraps=produced._ask_host) as ask, \
                mock.patch("urllib.request.urlopen", side_effect=lambda *a, **k: asked.append(a) or (_ for _ in ()).throw(OSError("no host"))):
            ask(r"C:\Users\Administrator\Documents\Other\x.xlsx")
        self.assertEqual(len(asked), 1)

    def test_what_the_task_made_in_the_folder_counts_as_what_it_wrote(self):
        self.put("old.xlsx", b"PK\x03\x04", age=3600)
        self.put("new.xlsx", b"PK\x03\x04")
        self.put("sub/also.csv", b"a,b\n")
        found = {Path(p).name for p in produced.files("", started=time.time() - 60)}
        self.assertEqual(found, {"new.xlsx", "also.csv"})

    def test_the_final_check_reads_the_inputs_the_task_was_given_there(self):
        self.put("orders_oct.xlsx", b"PK\x03\x04", age=3600)
        self.put("orders_oct_priced.xlsx", b"PK\x03\x04")  # made by the task: not an input
        request = ("Use the file orders_oct.xlsx from the Decurix demo folder in my Documents (%s) and save it as "
                   "orders_oct_priced.xlsx." % HOST)
        with mock.patch.object(produced, "since", lambda: time.time() - 60):
            self.assertEqual([Path(p).name for p in verify._shared_inputs(request)], ["orders_oct.xlsx"])


class EveryFileFactReadsThemOnce(SharedBase):
    def test_a_file_in_the_folder_is_one_file_not_two(self):
        path = str(self.mount / "report.xlsx")
        with mock.patch.object(produced, "store_results", lambda paths: []), \
                mock.patch.object(produced, "coverage_gaps", return_value=[]) as gaps:
            produced.file_facts("Save report.xlsx.", [path], [Path(path)], root=str(self.root))
        self.assertEqual(gaps.call_args.args[0], [path])


if __name__ == "__main__":
    unittest.main()
