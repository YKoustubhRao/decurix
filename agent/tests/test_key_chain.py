"""A long chain of different keys is refused before any is pressed; repeats of one key are a count."""

import unittest

from decurix_agent.computer import cli
from decurix_agent.computer.errors import ComputerError


class AKeyChainIsShort(unittest.TestCase):
    def test_a_blind_plan_of_many_keys_is_refused(self):
        chain = "ctrl+home shift+space ctrl+b ctrl+shift+down ctrl+shift+right t a b alt+h s s enter".split()
        with self.assertRaises(ComputerError) as caught:
            cli._refuse_blind_walk(chain)
        self.assertIn("at most 6", str(caught.exception))

    def test_a_few_keys_and_a_counted_repeat_are_pressed(self):
        cli._refuse_blind_walk(["ctrl+home", "shift+space", "ctrl+b"])
        cli._refuse_blind_walk(["Down"] * 12)


if __name__ == "__main__":
    unittest.main()
