"""
A .zip attached to a request is a folder of files: unpacked beside itself with its folders, and the request then names
the files it holds. The control plane takes at most ten files with a request (a month-end close has nineteen); the
archive route carries a folder past that cap without removing it. Nothing in an archive is trusted.
"""

import os
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest import mock

from decurix_agent import attachments, watchers


def make_zip(path, members):
    """A zip whose members are {name: bytes}; a name may carry folders."""

    with zipfile.ZipFile(path, "w") as z:
        for name, data in members.items():
            z.writestr(name, data)
    return str(path)


class ArchiveBase(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.inbox = Path(tmp.name) / "work" / "inbox" / "up-1a2b3c"
        self.inbox.mkdir(parents=True)
        self.root = Path(tmp.name)

    def request(self, *paths):
        return "Do the month-end close.\n\n" + attachments.block([str(p) for p in paths])


class UnpackingAnArchive(ArchiveBase):
    def test_the_files_come_out_with_their_folders_and_the_request_names_them(self):
        members = {f"branches/delhi/sheet{n}.xlsx": b"x" for n in range(1, 13)}
        members.update({"branches/mumbai/sheet1.xlsx": b"y", "notes.txt": b"n", "README.md": b"r"})
        archive = make_zip(self.inbox / "close.zip", members)
        keep = self.inbox / "price_list.xlsx"
        keep.write_bytes(b"p")
        content, notes = attachments.expand_archives(self.request(keep, archive))
        listed = attachments.files(content)
        # Fifteen files from one upload (a close has more than the ten the control plane takes at once), and the
        # plain attachment beside it stays.
        self.assertEqual(len(listed), 16)
        self.assertIn(str(keep), listed)
        self.assertIn((self.inbox / "close" / "branches" / "delhi" / "sheet3.xlsx").as_posix(), listed)
        self.assertTrue(all(os.path.isfile(p) for p in listed))
        self.assertNotIn(str(archive), listed)
        self.assertEqual(attachments.text(content), "Do the month-end close.")
        self.assertEqual(notes, ["Unpacked close.zip: 15 file(s), in their folders."])

    def test_the_same_name_in_two_folders_keeps_two_originals(self):
        archive = make_zip(self.inbox / "close.zip", {"delhi/branch.xlsx": b"DELHI", "mumbai/branch.xlsx": b"MUMBAI"})
        content, _ = attachments.expand_archives(self.request(archive))
        with mock.patch("decurix_agent.config.DECURIX_DIR", self.root / ".decurix"):
            attachments.keep_originals(content, "t9")
            delhi, mumbai = sorted(attachments.files(content))
            Path(mumbai).write_bytes(b"CHANGED")
            self.assertEqual(Path(attachments.original(mumbai, "t9")).read_bytes(), b"MUMBAI")
            self.assertEqual(attachments.original(delhi, "t9"), delhi)

    def test_names_the_readers_of_a_request_can_follow_and_duplicates_are_told_apart(self):
        archive = make_zip(self.inbox / "My Close (final).zip",
                           {"Q3 report (v2).xlsx": b"1", "q3 report (v2).XLSX": b"2", "請求書.pdf": b"3", "a/b/../c.txt": b"x"})
        with self.assertRaises(attachments.ArchiveError):
            attachments.unpack_archive(archive)  # the ".." member refuses the whole archive
        archive = make_zip(self.inbox / "My Close (final).zip",
                           {"Q3 report (v2).xlsx": b"1", "q3 report (v2).XLSX": b"2", "請求書.pdf": b"3"})
        made = attachments.unpack_archive(archive)
        self.assertEqual(sorted(Path(p).name for p in made), ["Q3_report_v2.xlsx", "file.pdf", "q3_report_v2_2.XLSX"])
        self.assertEqual(len(made), 3)
        self.assertTrue(all(" " not in p and "(" not in p for p in made))
        self.assertIn("My_Close_final", made[0])

    def test_macos_and_windows_litter_is_left_out_and_an_xlsx_is_never_unpacked(self):
        archive = make_zip(self.inbox / "c.zip", {"a.csv": b"1", "__MACOSX/._a.csv": b"x", ".DS_Store": b"x",
                                                  "Thumbs.db": b"x", "sub/._b.csv": b"x", "sub/b.csv": b"2"})
        self.assertEqual(sorted(Path(p).name for p in attachments.unpack_archive(archive)), ["a.csv", "b.csv"])
        workbook = make_zip(self.inbox / "book.xlsx", {"xl/workbook.xml": b"<x/>"})
        content, notes = attachments.expand_archives(self.request(workbook))
        self.assertEqual((attachments.files(content), notes), ([workbook], []))

    def test_a_request_with_no_archive_comes_back_as_it_was(self):
        plain = self.request(self.inbox / "a.xlsx")
        self.assertEqual(attachments.expand_archives(plain), (plain, []))
        self.assertEqual(attachments.expand_archives("hello"), ("hello", []))


class AnArchiveIsNotTrusted(ArchiveBase):
    def refused(self, members, **caps):
        archive = make_zip(self.inbox / "bad.zip", members)
        patches = [mock.patch.object(attachments, name, value) for name, value in caps.items()]
        for patch in patches:
            patch.start()
        try:
            with self.assertRaises(attachments.ArchiveError) as caught:
                attachments.unpack_archive(archive)
        finally:
            for patch in patches:
                patch.stop()
        self.assertFalse((self.inbox / "bad").exists() and any((self.inbox / "bad").rglob("*.*")),
                         "nothing is left half unpacked")
        return str(caught.exception)

    def test_a_path_that_leaves_the_folder_refuses_the_whole_archive(self):
        for name in ("../escape.txt", "a/../../escape.txt", "/etc/cron.d/x", "C:\\Windows\\x.txt", "a\\..\\..\\x.txt"):
            with self.subTest(name=name):
                said = self.refused({"fine.txt": b"ok", name: b"bad"})
                self.assertTrue("leaves the folder" in said or "absolute path" in said, said)
        self.assertFalse((self.inbox.parent / "escape.txt").exists())

    def test_a_link_is_refused(self):
        archive = self.inbox / "link.zip"
        with zipfile.ZipFile(archive, "w") as z:
            info = zipfile.ZipInfo("passwd")
            info.external_attr = (0o120777 << 16)
            z.writestr(info, "/etc/passwd")
        with self.assertRaises(attachments.ArchiveError) as caught:
            attachments.unpack_archive(archive)
        self.assertIn("is a link", str(caught.exception))

    def test_more_files_or_bytes_than_the_caps_refuse_it(self):
        said = self.refused({f"f{n}.txt": b"x" for n in range(6)}, MAX_ARCHIVE_FILES=5)
        self.assertIn("holds 6 files; at most 5", said)
        self.assertIn("more than 1 MB", self.refused({"a.bin": b"x" * 600000, "b.bin": b"x" * 600000},
                                                      MAX_ARCHIVE_BYTES=1 << 20))
        self.assertIn("is larger than", self.refused({"big.bin": b"x" * 2000}, MAX_ARCHIVE_MEMBER_BYTES=1000))

    def test_a_header_that_lies_about_its_size_is_a_damaged_archive_and_leaves_nothing(self):
        archive = make_zip(self.inbox / "lie.zip", {"ok.txt": b"fine", "a.bin": b"x" * 5000})
        real = zipfile.ZipFile.infolist

        def understated(z):
            infos = real(z)
            for i in infos:
                if i.filename == "a.bin":
                    i.file_size = 10
            return infos

        with mock.patch.object(zipfile.ZipFile, "infolist", understated):
            with self.assertRaises(attachments.ArchiveError) as caught:
                attachments.unpack_archive(archive)
        self.assertIn("damaged", str(caught.exception))
        self.assertFalse((self.inbox / "lie").exists(), "the member written before the bad one is not left behind")

    def test_the_bytes_that_come_out_are_counted_not_the_size_the_header_claims(self):
        archive = make_zip(self.inbox / "count.zip", {"a.bin": b"x" * 5000})
        with mock.patch.object(attachments, "MAX_ARCHIVE_MEMBER_BYTES", 1000),                 mock.patch.object(attachments, "_safe_member", lambda name: name):
            infos = zipfile.ZipFile(archive).infolist()
            with zipfile.ZipFile(archive) as z:
                with self.assertRaises(attachments.ArchiveError) as caught:
                    attachments._extract(z, [(infos[0], "a.bin")], str(self.inbox / "count"))
        self.assertIn("more than it said", str(caught.exception))

    def test_a_password_an_empty_archive_and_a_non_zip_are_refused(self):
        archive = make_zip(self.inbox / "locked.zip", {"a.txt": b"x"})
        locked = zipfile.ZipFile.infolist

        def encrypted(z):
            infos = locked(z)
            for i in infos:
                i.flag_bits |= 0x1
            return infos

        with mock.patch.object(zipfile.ZipFile, "infolist", encrypted):
            with self.assertRaises(attachments.ArchiveError) as caught:
                attachments.unpack_archive(archive)
        self.assertIn("password protected", str(caught.exception))
        with self.assertRaises(attachments.ArchiveError) as caught:
            attachments.unpack_archive(make_zip(self.inbox / "empty.zip", {"folder/": b""}))
        self.assertIn("holds no files", str(caught.exception))
        fake = self.inbox / "fake.zip"
        fake.write_text("not a zip")
        with self.assertRaises(attachments.ArchiveError):
            attachments.unpack_archive(fake)

    def test_a_refused_archive_stays_listed_and_says_why_and_the_good_ones_are_unpacked(self):
        good = make_zip(self.inbox / "good.zip", {"a.txt": b"1"})
        bad = make_zip(self.inbox / "bad.zip", {"../x.txt": b"1"})
        content, notes = attachments.expand_archives(self.request(good, bad))
        listed = attachments.files(content)
        self.assertIn(bad, listed)
        self.assertIn((self.inbox / "good" / "a.txt").as_posix(), listed)
        self.assertEqual(len(notes), 2)
        self.assertIn("bad.zip was not unpacked: ", notes[1])
        self.assertIn("leaves the folder", notes[1])


class TheWatcherUnpacksBeforeAnythingReadsTheRequest(ArchiveBase):
    def test_a_new_request_goes_to_its_handler_with_the_unpacked_files_named(self):
        archive = make_zip(self.inbox / "close.zip", {"a.xlsx": b"1", "sub/b.xlsx": b"2"})
        seen, said = {}, []
        path = Path(self.root / "20261019_101500_ab12cd34.tfedcba9876543210.txt")
        with mock.patch.object(watchers.task_context, "start", lambda tid, request=None: seen.update(start=request)), \
                mock.patch.object(watchers.task_context, "mark_active", lambda tid, c: seen.update(active=c)), \
                mock.patch.object(watchers.progress, "emit", lambda text: said.append(text)), \
                mock.patch.object(watchers.attachments, "keep_originals", lambda c, t: seen.update(kept=c)), \
                mock.patch.object(watchers, "_mark_handled", lambda p: None), \
                mock.patch.object(watchers.workflow_task, "handles", lambda c: seen.update(handler=c) or True), \
                mock.patch.object(watchers, "_run_handler", lambda *a, **k: None), \
                mock.patch.object(watchers.browser, "warm_soon"), \
                mock.patch.object(watchers.threading, "Thread") as thread:
            watchers._start_request(path, self.request(archive))
        files = attachments.files(seen["start"])
        self.assertEqual(len(files), 2)
        self.assertEqual(seen["start"], seen["active"])
        self.assertEqual(seen["kept"], seen["start"])
        self.assertEqual(seen["handler"], seen["start"])
        self.assertEqual(said, ["Unpacked close.zip: 2 file(s), in their folders."])
        self.assertTrue(thread.called)


if __name__ == "__main__":
    unittest.main()
