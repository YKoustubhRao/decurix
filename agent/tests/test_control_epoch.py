"""
Who drives the screen, and the epoch that counts every change of hands (computer/control.py; technique from Hermes'
bot-desktop lease and its `_fence`): while a person holds the screen the worker's `gui` looks at nothing and does
nothing; an action or a look that began in one epoch and ended in another is discarded - not recorded, not shown, no
screenshot - and an action whose input had gone out is reported as one that may have happened.

Nothing on a desktop takes the screen yet (no take-over exists to wire): these test the state, the fence and every hook.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_control_epoch.py
"""

import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_action_outcome as base

from decurix_agent.computer import cli, control, core, outcome
from decurix_agent.computer.errors import ActedError, ComputerError


class Isolated(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        patcher = mock.patch.dict(os.environ, {"DECURIX_CONTROL_FILE": str(Path(tmp.name) / "control.json")})
        patcher.start()
        self.addCleanup(patcher.stop)


class TheState(Isolated):
    def test_the_worker_holds_at_first(self):
        self.assertEqual(control.state()["holder"], control.WORKER)
        self.assertEqual(control.admit(), 0)

    def test_every_change_of_hands_moves_the_epoch_and_only_a_change(self):
        control.take("signing in")
        self.assertEqual(control.state()["epoch"], 1)
        control.take("signing in")  # the same hold again
        self.assertEqual(control.state()["epoch"], 1)
        control.give_back()
        control.give_back()  # the worker holds already
        self.assertEqual(control.state(), dict(control.state(), holder=control.WORKER, epoch=2))

    def test_a_person_holding_refuses_the_worker_with_their_reason(self):
        control.take("typing a password")
        with self.assertRaises(control.PersonHasControl) as caught:
            control.admit()
        self.assertIn("typing a password", str(caught.exception))

    def test_a_stale_hand_back_leaves_a_newer_hold(self):
        held = control.take("first")["epoch"]
        control.give_back()
        control.take("second")
        control.give_back(epoch=held)
        self.assertEqual(control.state()["holder"], control.PERSON)

    def test_a_damaged_state_is_a_person_holding(self):
        control.path().parent.mkdir(parents=True, exist_ok=True)
        control.path().write_text("{not json", encoding="utf-8")
        with self.assertRaises(control.PersonHasControl):
            control.admit()

    def test_a_hold_whose_process_is_gone_ends(self):
        control.take("a private step", pid=2 ** 22 + 12345)
        self.assertEqual(control.admit(), 2)  # handed back, the epoch moved on

    def test_the_fence_catches_a_change_even_after_control_came_back(self):
        epoch = control.admit()
        control.take("x")
        control.give_back()
        with self.assertRaises(control.ControlChanged):
            control.fence(epoch)
        self.assertFalse(control.still(epoch))

    def test_a_nested_admission_is_the_commands_turn(self):
        epoch = control.admit()
        with control.scope(epoch):
            self.assertEqual(control.admit(), epoch)
            control.take("x")
            with self.assertRaises(ComputerError):
                control.admit()


class AnActionInAPersonsTurn(base.Act, Isolated):
    def setUp(self):
        base.Act.setUp(self)
        Isolated.setUp(self)

    def test_nothing_is_looked_at_or_done_while_they_hold_it(self):
        control.take("signing in")
        screen = base.Screen(base.field())
        with self.assertRaises(control.PersonHasControl):
            self.act(screen, "click", ref="e5")
        self.assertEqual(screen.looks, 0)
        self.assertEqual(self.recorded, [])

    def test_taken_while_it_ran_it_is_discarded_and_may_have_happened(self):
        screen = base.Screen(base.field(role="button", name="Next"))
        with self.assertRaises(ActedError) as caught:
            self.act(screen, "click", ref="e5", perform=lambda *a, **k: control.take("they took over"))
        self.assertIn("discarded", str(caught.exception))
        self.assertEqual(self.recorded, [])  # no step, no picture, no evidence

    def test_taken_before_the_input_went_nothing_is_sent(self):
        screen = base.Screen(base.field(role="button", name="Next"))
        sent = []
        real = screen.look

        def look_then_take(*a, **k):
            seen = real(*a, **k)
            control.take("they took over")
            return seen

        screen.look = look_then_take
        with self.assertRaises(ComputerError) as caught:
            self.act(screen, "click", ref="e5", perform=lambda *a, **k: sent.append(a))
        self.assertNotIsInstance(caught.exception, ActedError)
        self.assertEqual(sent, [])
        self.assertEqual(self.recorded, [])


class GuiInAPersonsTurn(base.Act, Isolated):
    def setUp(self):
        base.Act.setUp(self)
        Isolated.setUp(self)

    def test_observe_is_refused_and_no_screenshot_is_taken(self):
        control.take("signing in")
        with mock.patch.object(core, "observe") as looked, \
                mock.patch.dict(os.environ, {"DECURIX_GUI_SCREENSHOT": str(self.dir / "shot.png")}), \
                mock.patch("decurix_agent.computer.screen.grab", side_effect=AssertionError("grabbed")):
            code = cli.main(["observe"])
        self.assertEqual(code, cli.EXIT_ERROR)
        looked.assert_not_called()
        self.assertFalse((self.dir / "shot.png").exists())

    def test_a_look_overlapped_by_their_turn_is_not_shown(self):
        screen = base.Screen(base.field())

        def look(*a, **k):
            seen = screen.look()
            control.take("they took over")
            return seen

        printed = []
        with mock.patch.object(core, "observe", side_effect=look), \
                mock.patch.object(cli, "settled_look", lambda o: (o, False)), \
                mock.patch.object(cli.el, "render", side_effect=lambda *a, **k: printed.append(1) or "listing"):
            code = cli.main(["observe"])
        self.assertEqual(code, cli.EXIT_ERROR)
        self.assertEqual(printed, [])

    def test_a_look_answers_the_look_again_marks_and_notes_the_window_in_front(self):
        screen = base.Screen(base.field())
        with mock.patch.object(core, "observe", side_effect=screen.look), \
                mock.patch.object(cli, "settled_look", lambda o: (o, False)), \
                mock.patch.object(core, "_note_front") as noted, \
                mock.patch.object(outcome, "looked") as answered, \
                mock.patch.object(core, "report_observation", lambda o: None), \
                mock.patch.object(cli.el, "render", return_value="listing"), \
                mock.patch.object(cli.door, "note_look", lambda *a: None), \
                mock.patch.object(core, "pending_save_readback", lambda o: ""):
            self.assertEqual(cli.main(["observe"]), cli.EXIT_OK)
        noted.assert_called_once()
        answered.assert_called_once()


if __name__ == "__main__":
    unittest.main()
