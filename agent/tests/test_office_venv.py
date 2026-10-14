import os
import sys
import tempfile
import unittest

from decurix_agent.tools import office


class TheOfficeVenvIsFoundWhateverPath(unittest.TestCase):
    def test_its_packages_are_importable_after_the_systems(self):
        with tempfile.TemporaryDirectory() as root:
            site = os.path.join(root, "lib", "python3.12", "site-packages")
            os.makedirs(site)
            before = list(sys.path)
            try:
                office._use_office_venv(root)
                self.assertEqual(sys.path[-1], site)
                self.assertEqual(sys.path[:len(before)], before)
                office._use_office_venv(root)  # once only
                self.assertEqual(sys.path.count(site), 1)
            finally:
                sys.path[:] = before


if __name__ == "__main__":
    unittest.main()
