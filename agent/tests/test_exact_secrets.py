"""
The exact bytes of every secret that passed through (privacy.remember_exact, _exact_spans, withhold_registered): kept
as hashes only, matched by a sliding window wherever the word hashes miss it, with a floor that keeps a common word out,
and applied on every path that prints for the worker or hands a text to a model (gui's stdout, office's, a kept
tool's output, a failed proof's, the escalation ladder's prompt).
"""

import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import task_context
from decurix_agent.computer import privacy


class _Store(unittest.TestCase):
    """A secrets file and a task's literal store of this test's own."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        for p in (mock.patch.dict(os.environ, {"DECURIX_DIR": str(self.root)}),
                  mock.patch.object(task_context, "CURRENT_FILE", str(self.root / "current-task"))):
            p.start()
            self.addCleanup(p.stop)
        for cache in (privacy._known_cache, privacy._literal_cache, privacy._unsaved, privacy._unsaved_literals):
            cache.clear()
            self.addCleanup(cache.clear)

    def kept(self):
        return (self.root / "withheld-secrets").read_text(encoding="utf-8")


class TheFloor(unittest.TestCase):
    def test_a_common_word_or_a_short_value_is_never_kept_by_its_bytes(self):
        # Found live (2026-11-10): "OTP: ask me" kept "ask", and every later "ask" read [withheld].
        for word in ("ask", "yes", "the", "letmein", "Please", "client", "abc", "[withheld]9x", "x" * 513):
            self.assertIsNone(privacy.exact_mode(word), word)
        self.assertEqual(privacy._exact_entries(["ask", "letmein", "  yes \r\n"]), [])

    def test_how_each_kind_of_value_is_withheld(self):
        self.assertEqual(privacy.exact_mode("Hunter2!"), "a")              # letters with digits or marks: anywhere
        self.assertEqual(privacy.exact_mode("ab;cd9xyz"), "a")
        self.assertEqual(privacy.exact_mode("4821"), "w")                  # no letter: a PIN, a code - a whole token
        self.assertEqual(privacy.exact_mode("12-34-56"), "w")
        self.assertEqual(privacy.exact_mode("correct horse battery"), "w")  # letters and spaces, long: a whole token

    def test_the_forms_kept(self):
        self.assertEqual(privacy.exact_forms(" Hunter2!\r\n"), [" Hunter2!\r\n", "Hunter2!", " Hunter2!"])
        self.assertIn("line1line2x9", privacy.exact_forms("line1\nline2x9"))
        self.assertIn("line1 line2x9", privacy.exact_forms("line1\nline2x9"))


class ExactBytes(_Store):
    def test_kept_as_hashes_only(self):
        privacy.remember_value("Hunter2!\r\n")
        kept = self.kept()
        self.assertNotIn("Hunter2", kept)
        self.assertTrue(any(line.startswith("x") for line in kept.split()))

    def test_an_echo_the_word_hashes_miss_is_withheld(self):
        privacy.remember_value("Hunter2!")
        for text in ("user=bob,pass:Hunter2!&x", "xxHunter2!yy", '{"pw":"Hunter2!"}', "é Hunter2! ü"):
            out = privacy.withhold_known(text)
            self.assertNotIn("Hunter2", out, text)
            self.assertIn("[withheld]", out, text)
            self.assertTrue(privacy.known_secret_in(text), text)
        self.assertEqual(privacy.withhold_registered("é Hunter2! ü"), "é [withheld] ü")

    def test_a_secret_typed_with_its_line_break_is_found_without_it(self):
        privacy.remember_value("ab;cd9xyz\r\n")
        self.assertEqual(privacy.withhold_registered("field: ab;cd9xyz."), "field: [withheld].")
        privacy.remember_exact(["first9\nsecond"])
        self.assertEqual(privacy.withhold_registered("[first9second]"), "[[withheld]]")
        self.assertEqual(privacy.withhold_registered("first9 second"), "[withheld]")

    def test_a_value_with_no_letter_and_a_passphrase_only_as_a_whole_token(self):
        privacy.remember_value("552901")
        privacy.remember_value("correct horse battery staple")
        self.assertEqual(privacy.withhold_registered("OTP 552901 ok"), "OTP [withheld] ok")
        self.assertEqual(privacy.withhold_registered("INV-5529012"), "INV-5529012")
        self.assertEqual(privacy.withhold_registered("say correct horse battery staple."), "say [withheld].")
        self.assertEqual(privacy.withhold_registered("incorrect horse battery staples"),
                         "incorrect horse battery staples")

    def test_longest_first_one_mark(self):
        privacy.remember_exact(["Hunter2!", "Hunter2!abc"])
        self.assertEqual(privacy.withhold_registered("xHunter2!abcx and Hunter2!"), "x[withheld]x and [withheld]")

    def test_a_marked_word_kept_as_a_whole_token_stays_one(self):
        privacy.remember_exact([privacy.WHOLE_TOKEN + "Quartz58lantern"])
        self.assertEqual(privacy.withhold_registered("sign in with Quartz58lantern"), "sign in with [withheld]")
        self.assertEqual(privacy.withhold_registered("XQuartz58lanternY"), "XQuartz58lanternY")

    def test_the_tasks_values_outlive_the_task_as_hashes(self):
        privacy.remember_literals(["s3cret value;9"], "t1")
        privacy.clear_literals("t1")
        self.assertEqual(privacy.literals(), [])
        self.assertEqual(privacy.withhold_known("it was s3cret value;9 here"), "it was [withheld] here")
        self.assertNotIn("s3cret", self.kept())

    def test_a_requests_secret_and_a_marked_message(self):
        privacy.remember_secrets("log in with password Lift$2026x")
        self.assertEqual(privacy.withhold_registered("echo:Lift$2026x;"), "echo:[withheld];")
        privacy.remember_marked("the code is\nZebra-91 now", "t2")
        privacy.clear_literals("t2")
        self.assertIn("[withheld]", privacy.withhold_registered("got the code is Zebra-91 now"))

    def test_the_word_hashes_and_the_exact_entries_are_bounded_apart(self):
        with mock.patch.object(privacy, "_MAX_KEPT", 3):
            privacy.remember_exact([f"Value{n}x!" for n in range(5)])
            privacy.remember_value("plainword")
            lines = self.kept().split()
        self.assertEqual(len([h for h in lines if h.startswith("x")]), 3)
        self.assertEqual(len([h for h in lines if not h.startswith("x")]), 1)

    def test_no_shape_rule_on_what_a_tool_prints(self):
        # An OTP a page shows the worker is the worker's to read: only what passed through as a secret is withheld.
        self.assertEqual(privacy.withhold_registered("Your OTP is 529749"), "Your OTP is 529749")

    def test_fails_closed(self):
        privacy.remember_value("Hunter2!")
        with mock.patch.object(privacy, "_known", side_effect=privacy._Unreadable("no")):
            self.assertEqual(privacy.withhold_registered("pass:Hunter2!"), privacy.HELD_BACK)
            self.assertEqual(privacy.withhold_registered({"a": "b"}), {"held_back": privacy.HELD_BACK})
            self.assertTrue(privacy.known_secret_in("anything"))

    def test_a_tree_is_withheld_string_by_string(self):
        privacy.remember_value("Hunter2!")
        self.assertEqual(privacy.withhold_registered({"screen": ["a Hunter2! b"], "n": 3}),
                         {"screen": ["a [withheld] b"], "n": 3})


class OutputPaths(_Store):
    """Each path that prints for the worker or hands a text to a model."""

    def setUp(self):
        super().setUp()
        privacy.remember_value("Hunter2!x")

    def test_gui_stdout(self):
        from decurix_agent.computer import cli
        out = io.StringIO()
        kept = cli._Kept(out)
        kept.write('e5 text value="Hunter2!x" [focused]')
        kept.write("\n")
        self.assertEqual(out.getvalue(), 'e5 text value="[withheld]" [focused]\n')
        self.assertNotIn("Hunter2", "".join(kept.kept))

    def test_office_read(self):
        from contextlib import redirect_stdout
        from decurix_agent.tools import office
        doc = self.root / "notes.txt"
        doc.write_text("portal login: staff / Hunter2!x\n", encoding="utf-8")
        out = io.StringIO()
        with redirect_stdout(out):
            code = office.main(["read", str(doc)])
        self.assertEqual(code, 0, out.getvalue())
        self.assertNotIn("Hunter2", out.getvalue())
        self.assertIn("[withheld]", out.getvalue())

    def test_a_failed_proofs_output_goes_back_withheld(self):
        import subprocess
        from decurix_agent import playbooks, script_sandbox
        tool = {"path": str(self.root / "t.py"), "example": ""}
        Path(tool["path"]).write_text("pass\n", encoding="utf-8")
        failed = subprocess.CompletedProcess([], 1, stdout="", stderr="bad password Hunter2!x")
        with mock.patch.object(script_sandbox, "run", lambda *a, **k: failed), \
                mock.patch.object(script_sandbox, "snapshot", lambda *a: {}):
            written, wrong = playbooks._run_in_copy(tool, self.root, self.root / "work")
        self.assertNotIn("Hunter2", wrong)
        self.assertIn("[withheld]", wrong)

    def test_the_escalation_ladders_question(self):
        from decurix_agent.computer import escalation
        asked = []

        def ask_model(system, prompt, purpose=""):
            asked.append(prompt)
            return json.dumps({"achieved": True, "confidence": 0.99})

        ladder = escalation.Ladder(ask_model=ask_model)
        evidence = {"goal": "", "step": "2", "summary": "save", "missing": ["Saved"], "screen": "e1 text \"Hunter2!x\""}
        ladder._consult(escalation.JUDGE, evidence, escalation.judge_prompt(evidence))
        self.assertNotIn("Hunter2", asked[0])
        self.assertEqual(evidence["screen"], "e1 text \"Hunter2!x\"")  # (the decision is read against it as it was)

        handed = []

        def ask_decided(system, prompt, ev, purpose=""):
            handed.append((prompt, ev))
            return json.dumps({"achieved": True, "confidence": 0.99}), None

        escalation.Ladder(ask_decided=ask_decided)._consult(escalation.JUDGE, evidence,
                                                           escalation.judge_prompt(evidence))
        self.assertNotIn("Hunter2", json.dumps(handed))


class AKeptToolsOutput(_Store):
    """What a kept tool printed reaches the judge, the engine's note and the reply withheld (tool_task._run)."""

    def setUp(self):
        super().setUp()
        import sandbox_standin
        from decurix_agent import config
        self.work = self.root / "work"
        self.work.mkdir()
        p = mock.patch.object(config, "WORK_DIR", self.work)
        p.start()
        self.addCleanup(p.stop)
        sandbox_standin.start(self, self.root / "scratch")
        privacy.remember_value("Hunter2!x")

    def test_printed(self):
        from decurix_agent import tool_task
        script = self.root / "tool.py"
        # (A script that holds the secret itself is refused before it runs, playbooks._sound: it reads it from the work.)
        (self.work / "login.txt").write_text("Hunter2!x", encoding="utf-8")
        script.write_text("print('signed in as staff with ' + open('login.txt').read())\n", encoding="utf-8")
        code, printed = tool_task._run({"path": str(script)}, [])
        self.assertEqual(code, 0, printed)
        self.assertEqual(printed, "signed in as staff with [withheld]")


if __name__ == "__main__":
    unittest.main()
