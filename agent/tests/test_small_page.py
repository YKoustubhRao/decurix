import contextlib
import io
import unittest
from unittest import mock

from decurix_agent.computer import cli


def look(*names):
    return {"page_url": "http://host/login", "elements": [{"source": "cdp", "role": "textbox", "name": n} for n in names]}


class ASmallPageIsNotALoadingPage(unittest.TestCase):
    def run_observe(self, looks):
        out = io.StringIO()
        with mock.patch.object(cli.core, "observe", side_effect=looks) as observe, \
                mock.patch.object(cli.core, "report_observation"), \
                mock.patch.object(cli.el, "render", return_value="(elements)"), \
                mock.patch("decurix_agent.computer.evidence.note_look"), \
                mock.patch("decurix_agent.computer.grounder.available", return_value=False), \
                mock.patch.object(cli.time, "sleep"), contextlib.redirect_stdout(out):
            cli.run(["observe"])
        return out.getvalue(), observe.call_count

    def test_a_sign_in_page_that_reads_the_same_twice_has_settled(self):
        said, looks = self.run_observe([look("Username", "Password")] * 5)
        self.assertNotIn("almost nothing", said)
        self.assertEqual(looks, 2)

    def test_a_page_still_drawing_itself_is_still_reported_as_loading(self):
        said, _ = self.run_observe([look(), look("a"), look("a", "b"), look("a", "b", "c"), look("a", "b", "c", "d"), look("a", "b", "c", "d", "e")])
        self.assertIn("almost nothing", said)


if __name__ == "__main__":
    unittest.main()
