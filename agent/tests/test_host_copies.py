"""
Check copies of the person's files are kept per full path and per task, and the old ones are removed (review
2026-11-03, E-31, E-43): two files of one name in two folders were one copy, so one was checked twice and the other
never.
"""

import unittest
from unittest import mock

from temp_case import TempDirCase

from decurix_agent import config, produced, task_context


class HostCopies(TempDirCase):
    def setUp(self):
        super().setUp()
        for patch in (mock.patch.object(config, "DECURIX_DIR", self.dir),
                      mock.patch.object(produced.shared, "to_local", lambda path: None),
                      mock.patch.object(task_context, "current", lambda: "t0123456789abcdef")):
            patch.start()
            self.addCleanup(patch.stop)

    def test_two_files_of_one_name_in_two_folders_are_two_copies(self):
        a, b = r"C:\Clients\A\report.xlsx", r"C:\Clients\B\report.xlsx"
        copies = produced.host_copies([a, b], fetch=lambda path: path.encode())
        self.assertEqual(set(copies), {a, b})
        self.assertNotEqual(copies[a], copies[b])
        self.assertEqual(copies[a].read_bytes(), a.encode())
        self.assertEqual(copies[b].read_bytes(), b.encode())
        self.assertEqual((copies[a].name, copies[b].name), ("report.xlsx", "report.xlsx"))

    def test_only_the_last_few_tasks_copies_are_kept(self):
        for n in range(produced.HOST_COPY_TASKS + 3):
            with mock.patch.object(task_context, "current", lambda n=n: f"t{n:016x}"):
                produced.host_copies([rf"C:\x\r{n}.csv"], fetch=lambda path: b"x")
        root = self.dir / "computer" / "host-copies"
        (root / "loose.csv").write_bytes(b"old flat copy")
        produced.host_copies([r"C:\x\again.csv"], fetch=lambda path: b"x")
        kept = [p for p in root.iterdir()]
        self.assertTrue(all(p.is_dir() for p in kept))
        self.assertLessEqual(len(kept), produced.HOST_COPY_TASKS + 1)


if __name__ == "__main__":
    unittest.main()
