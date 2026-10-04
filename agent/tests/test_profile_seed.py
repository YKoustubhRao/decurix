import os
import tempfile
import shutil
import unittest
from pathlib import Path
from unittest import mock


@unittest.skipUnless(os.name == "nt", "a native Windows worker's browser profile")
class TheWorkersProfileIsItsOwnFromItsFirstStart(unittest.TestCase):
    """
    Found live (2026-10-03): the first launch left 4 files of the person's
    profile behind, the person signed in to their sites in the worker's Chrome,
    and the next launch copied their own profile over the worker's again.
    """

    def setUp(self):
        from decurix_agent.computer import profile_seed

        self.seed = profile_seed
        self.dir = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.dir, True)

        self.source = self.dir / "person" / "User Data"
        (self.source / "Default" / "Network").mkdir(parents=True)
        (self.source / "Local State").write_text('{"profile": {"last_used": "Default"}}', encoding="utf-8")
        (self.source / "Default" / "Preferences").write_text('{"who": "person"}', encoding="utf-8")
        (self.source / "Default" / "Network" / "Cookies").write_text("the person's cookies", encoding="utf-8")
        self.worker = self.dir / "worker" / "browser-chrome"

        for p in (mock.patch.dict(profile_seed.USER_DATA, {"chrome": str(self.source)}),
                  mock.patch.object(profile_seed, "COPY_RETRY_SECONDS", 0)):
            p.start()
            self.addCleanup(p.stop)

    def cookies(self):
        return (self.worker / "Default" / "Network" / "Cookies").read_text(encoding="utf-8")

    def unreadable(self, *names):
        """The person's browser holds these files; everything else copies."""

        real = self.seed._copy

        def copy(src, dst):
            if Path(src).name in names:
                raise OSError(f"cannot open {src}")
            real(src, dst)

        return mock.patch.object(self.seed, "_copy", copy)

    def test_a_new_workers_profile_starts_as_a_copy_of_the_persons(self):
        said = self.seed.seed("chrome", self.worker, "Default")

        self.assertIn("copied the Default profile", said)
        self.assertEqual(self.cookies(), "the person's cookies")

    def test_it_is_not_copied_again_at_the_next_launch(self):
        self.seed.seed("chrome", self.worker, "Default")
        (self.worker / "Default" / "Network" / "Cookies").write_text("signed in, in the worker", encoding="utf-8")

        self.assertEqual(self.seed.seed("chrome", self.worker, "Default"), "")
        self.assertEqual(self.cookies(), "signed in, in the worker")

    def test_files_left_behind_the_first_time_are_not_fetched_over_what_the_worker_has_since(self):
        # As it happened: files left behind, then a second launch.
        with self.unreadable("Preferences"):
            said = self.seed.seed("chrome", self.worker, "Default")
        self.assertIn("left behind", said)

        (self.worker / "Default" / "Network" / "Cookies").write_text("signed in, in the worker", encoding="utf-8")
        (self.worker / "Default" / "Preferences").write_text('{"who": "worker"}', encoding="utf-8")

        self.assertEqual(self.seed.seed("chrome", self.worker, "Default"), "")
        self.assertEqual(self.cookies(), "signed in, in the worker")
        self.assertEqual((self.worker / "Default" / "Preferences").read_text(encoding="utf-8"), '{"who": "worker"}')

    def test_cookies_the_persons_browser_held_are_not_brought_in_later_either(self):
        with self.unreadable("Cookies"):
            said = self.seed.seed("chrome", self.worker, "Default")
        self.assertIn("left behind", said)

        (self.worker / "Default" / "Network").mkdir(parents=True, exist_ok=True)
        (self.worker / "Default" / "Network" / "Cookies").write_text("signed in, in the worker", encoding="utf-8")

        self.assertEqual(self.seed.seed("chrome", self.worker, "Default"), "")
        self.assertEqual(self.cookies(), "signed in, in the worker")

    def test_a_folder_a_browser_has_already_used_is_left_as_it_is(self):
        (self.worker / "Default" / "Network").mkdir(parents=True)
        (self.worker / "Local State").write_text("{}", encoding="utf-8")
        (self.worker / "Default" / "Network" / "Cookies").write_text("the worker's own", encoding="utf-8")

        self.assertEqual(self.seed.seed("chrome", self.worker, "Default"), "")
        self.assertEqual(self.cookies(), "the worker's own")
        self.assertEqual((self.worker / "Local State").read_text(encoding="utf-8"), "{}")

    def test_a_file_busy_for_a_moment_is_tried_again_within_the_one_seeding(self):
        real, tries = self.seed._copy, []

        def copy(src, dst):
            if Path(src).name == "Preferences":
                tries.append(1)
                if len(tries) == 1:
                    raise OSError("busy")
            real(src, dst)

        with mock.patch.object(self.seed, "_copy", copy):
            said = self.seed.seed("chrome", self.worker, "Default")

        self.assertNotIn("left behind", said)
        self.assertEqual((self.worker / "Default" / "Preferences").read_text(encoding="utf-8"), '{"who": "person"}')

    def test_a_seeding_that_died_part_way_is_not_done_again(self):
        def dies(src, dst):
            raise KeyboardInterrupt

        with mock.patch.object(self.seed, "_copy", dies), self.assertRaises(KeyboardInterrupt):
            self.seed._seed("chrome", self.worker, "Default")

        self.assertEqual(self.seed.seed("chrome", self.worker, "Default"), "")


if __name__ == "__main__":
    unittest.main()
