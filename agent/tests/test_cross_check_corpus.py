"""
The cross-check's cases in one table (fixtures/cross_check_cases.json): every live line it got wrong or right
(dates, lakh/crore, rounding, signs, identifiers, percentages) and the forms it is likely to meet next, each run
through long_task.model_disagreements with its two places in two files. A new rule cannot silently undo an old one.
"""

import json
import tempfile
import unittest
from pathlib import Path

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths  # noqa: E402 - live-run fixtures live outside the repo

from decurix_agent import long_task

CORPUS_PATH = fixture_paths.fixture("cross_check_cases.json")


def disagreements(case):
    with tempfile.TemporaryDirectory() as tmp:
        outputs, places = [], []
        for side, name in (("a", "first_output.txt"), ("b", "second_output.txt")):
            place = case[side]
            path = Path(tmp) / name
            path.write_text(place.get("doc") or place["snippet"], encoding="utf-8")
            outputs.append((side, str(path)))
            places.append({"file": name, "text": place["snippet"], "value": place["value"]})
        figures = [{"figure": case.get("figure", "a figure"), "places": places}]
        found, _ = long_task.model_disagreements(outputs, ask=lambda system, prompt: {"figures": figures},
                                                 request=case.get("request", ""))
    return found


class TheCorpus(unittest.TestCase):
    def test_every_case(self):
        CORPUS = json.loads(fixture_paths.need("cross_check_cases.json").read_text(encoding="utf-8"))
        self.assertGreaterEqual(len(CORPUS["cases"]), 30)
        for case in CORPUS["cases"]:
            with self.subTest(case["name"]):
                got = "disagree" if disagreements(case) else "agree"
                self.assertEqual(got, case["expect"], f"{case['a']['value']!r} vs {case['b']['value']!r}")


if __name__ == "__main__":
    unittest.main()
