"""The web lookup: what it sends, what it brings back, and how a lesson uses it."""

import unittest
from unittest import mock

from decurix_agent import study
from decurix_agent.tools import lookup

DDG = """
<div><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fhelp.example.com%2Fledger&amp;rut=x">
Create a <b>Ledger</b></a> ... <a class="result__snippet" href="x">Gateway &gt; Create &gt; Ledger</a></div>
<div><a rel="nofollow" class="result__a" href="https://duckduckgo.com/y.js?ad=1">Ad</a> <a class="result__snippet">buy</a></div>
"""
PAGE = """<html><head><script>var x = 1;</script></head><body><nav>Home Products Pricing Contact us today</nav>
<p>Welcome to the help centre for every product we have ever made.</p>
<p>To create a ledger, go to Gateway of Tally, then Create, then Ledger, and press Enter.</p>
<p>In the Under field, select Sundry Creditors from the List of Groups for a supplier.</p></body></html>"""


def fake_get(url, timeout=None):
    return DDG if "duckduckgo" in url else PAGE


class WhatIsSent(unittest.TestCase):
    def test_data_never_leaves(self):
        for q in ("invoice for ap@acme.com", "account 4111 1111 1111 1111", "PO 7731-2209-554"):
            self.assertTrue(lookup.unsafe(q), q)
            with self.assertRaises(lookup.LookupError_):
                lookup.search(q)

    def test_a_question_about_the_app_is_sent(self):
        self.assertEqual(lookup.unsafe("TallyPrime how to create a ledger under Sundry Creditors"), "")


class WhatComesBack(unittest.TestCase):
    def test_results_skip_ads_and_read_the_answering_paragraphs(self):
        with mock.patch.object(lookup, "_get", fake_get):
            text = lookup.search("how to create a ledger under Sundry Creditors")
        self.assertTrue(text.startswith(lookup.HEADER))
        self.assertIn("https://help.example.com/ledger", text)
        self.assertNotIn("y.js", text)
        self.assertIn("Under field, select Sundry Creditors", text)
        self.assertNotIn("var x", text)
        self.assertNotIn("Pricing", text)

    def test_a_failed_search_is_said_not_raised_from_main(self):
        with mock.patch.object(lookup, "results", lambda q: []):
            self.assertEqual(lookup.main(["anything at all"]), 1)


class ALessonLooksTheAppUp(unittest.TestCase):
    def test_a_question_the_web_answers_is_not_asked(self):
        first = {"summary": "Enter a supplier.", "research": ["TallyPrime ledger Under field"],
                 "questions": [{"text": "Which field takes the group?"}, {"text": "Should I create missing suppliers?"}]}
        revised = {"summary": "Enter a supplier as a ledger.", "web_notes": ["The Under field takes the group (TallyHelp)."],
                   "questions": [{"text": "Should I create missing suppliers?"}]}
        calls = []

        def call(system, content, max_tokens=0):
            calls.append(system)
            return first if system == study.STUDY_SYSTEM else revised

        with mock.patch.object(study, "available", lambda: True), mock.patch.object(study, "_call", call), \
                mock.patch.object(study, "_documents", lambda s: []), mock.patch.object(study, "_pictures", lambda s: []), \
                mock.patch.object(study, "_described", lambda s: "steps"), \
                mock.patch.object(lookup, "search", lambda q: "WEB LOOKUP ... Under: select the group"):
            understood = study.study({"name": "t", "steps": [], "params": []})
        self.assertEqual(calls, [study.STUDY_SYSTEM, study.RESEARCH_SYSTEM])
        self.assertEqual([q["text"] for q in understood["questions"]], ["Should I create missing suppliers?"])
        self.assertEqual(understood["web_notes"], ["The Under field takes the group (TallyHelp)."])

    def test_no_research_no_lookup(self):
        def call(system, content, max_tokens=0):
            assert system == study.STUDY_SYSTEM
            return {"summary": "x", "questions": []}

        def never(q):
            raise AssertionError("looked up")

        with mock.patch.object(study, "available", lambda: True), mock.patch.object(study, "_call", call), \
                mock.patch.object(study, "_documents", lambda s: []), mock.patch.object(study, "_pictures", lambda s: []), \
                mock.patch.object(study, "_described", lambda s: "steps"), mock.patch.object(lookup, "search", never):
            understood = study.study({"name": "t", "steps": [], "params": []})
        self.assertEqual(understood["web_notes"], [])


if __name__ == "__main__":
    unittest.main()
