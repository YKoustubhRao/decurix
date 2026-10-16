import unittest
from unittest import mock

from decurix_agent.computer import cdp, core
from decurix_agent.computer.errors import ComputerError

FIELD = {"role": "textbox", "name": "Name Box", "source": "cdp", "ref": "e5", "states": [], "handle": {"backend_id": 1}}


class SetInApp(unittest.TestCase):
    def run_type(self, text, relayed=True, refuses=False, element=FIELD, why="NOSET read-only"):
        set_in_app = mock.Mock(side_effect=ComputerError(why) if refuses else None, return_value=text)
        with mock.patch.object(core, "_relayed", return_value=relayed), \
                mock.patch.object(core, "_load_state", return_value={}), \
                mock.patch.object(cdp, "set_in_app", set_in_app), \
                mock.patch.object(cdp, "type_text") as typed:
            backend = core._perform("type", element, None, text, None, None, None, None, False)
        return backend, set_in_app, typed

    def test_a_line_into_a_host_app_field_is_set_not_typed(self):
        backend, set_in_app, typed = self.run_type("D20")
        self.assertEqual(backend, "relayed:set")
        set_in_app.assert_called_once()
        typed.assert_not_called()

    def test_a_field_the_app_will_not_set_is_typed(self):
        backend, _, typed = self.run_type("D20", refuses=True)
        self.assertEqual(backend, "cdp")
        typed.assert_called_once()

    def test_a_field_that_may_hold_the_text_is_never_typed_again(self):
        with self.assertRaises(ComputerError):
            self.run_type("D20", refuses=True, why="HELD D2")

    def test_several_lines_and_a_web_page_are_typed(self):
        self.assertEqual(self.run_type("a\nb")[0], "cdp")
        self.assertEqual(self.run_type("D20", relayed=False)[0], "cdp")

    def test_a_secret_field_is_typed(self):
        secret = dict(FIELD, name="Password", states=["protected"])
        with mock.patch.object(core.privacy, "secret_field", return_value=True):
            self.assertEqual(self.run_type("x", element=secret)[0], "cdp")


if __name__ == "__main__":
    unittest.main()
