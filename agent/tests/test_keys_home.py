"""Keys sent to no element go to the window the task was working in when an older window took the keyboard."""

import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent.computer import core


def win(wid, title, active=False):
    return {"id": wid, "title": title, "active": active}


class KeysGoWhereTheTaskWasWorking(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        for name, value in (("STATE_DIR", Path(self.dir.name)), ("FRONT_FILE", Path(self.dir.name) / "front.json"),
                            ("_task_now", lambda: "t1")):
            patcher = mock.patch.object(core, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.activated = []
        patcher = mock.patch.object(core.xinput, "activate", self.activated.append)
        patcher.start()
        self.addCleanup(patcher.stop)

    def windows(self, wins):
        return mock.patch.object(core.screen, "windows", lambda: wins)

    def test_an_older_window_that_took_the_keyboard_gives_it_back(self):
        with self.windows([win(1, "Untitled 1 - LibreOffice Impress"), win(2, "TallyPrime", active=True)]):
            core._note_front()
        with self.windows([win(2, "TallyPrime"), win(1, "Untitled 1 - LibreOffice Impress", active=True)]):
            self.assertEqual(core._keys_home(), "TallyPrime")
        self.assertEqual(self.activated, [2])

    def test_a_new_window_keeps_the_keys(self):
        # A dialog the app opened after the action is where the keys belong.
        with self.windows([win(2, "Excel", active=True)]):
            core._note_front()
        with self.windows([win(2, "Excel"), win(3, "Save As", active=True)]):
            self.assertIsNone(core._keys_home())
        self.assertEqual(self.activated, [])

    def test_the_window_still_in_front_needs_nothing(self):
        with self.windows([win(2, "Excel", active=True)]):
            core._note_front()
            self.assertIsNone(core._keys_home())

    def test_another_task_or_an_old_note_is_ignored(self):
        with self.windows([win(1, "Impress"), win(2, "TallyPrime", active=True)]):
            core._note_front()
        stolen = [win(2, "TallyPrime"), win(1, "Impress", active=True)]
        with self.windows(stolen), mock.patch.object(core, "_task_now", lambda: "t2"):
            self.assertIsNone(core._keys_home())
        later = time.time() + 3600
        with self.windows(stolen), mock.patch.object(core.time, "time", lambda: later):
            self.assertIsNone(core._keys_home())

    def test_a_window_that_is_gone_is_not_looked_for(self):
        with self.windows([win(1, "Impress"), win(2, "Find", active=True)]):
            core._note_front()
        with self.windows([win(1, "Impress", active=True)]):
            self.assertIsNone(core._keys_home())


if __name__ == "__main__":
    unittest.main()
