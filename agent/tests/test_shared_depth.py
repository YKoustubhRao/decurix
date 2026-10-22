import tempfile
import unittest
from pathlib import Path

from decurix_agent import shared


# GST suite T6 (2026-10-21): a workbook made in "GST Returns/<client>/2026-27/6. SEPTEMBER" was four folders down
# and the final check, walking three, said it was not made.
class TestAPersonsFoldersAreWalkedDeepEnough(unittest.TestCase):

    def test_a_file_four_folders_down_is_found(self):
        root = Path(tempfile.mkdtemp())
        deep = root / "GST Returns" / "SRI LAKSHMI ELEVATORS" / "2026-27" / "6. SEPTEMBER"
        deep.mkdir(parents=True)
        (deep / "SLE_Recon_Sep2026.xlsx").write_bytes(b"x")
        (root / "top.txt").write_text("x")
        names = [p.name for p in shared.walk_files(root)]
        self.assertIn("SLE_Recon_Sep2026.xlsx", names)
        self.assertEqual(names[0], "top.txt")  # nearest first


if __name__ == "__main__":
    unittest.main()
