"""
The structured verdict (decurix.verdict/v1, R6 of the plan of 2026-11-03; verdict.py, PLAN-P2 §5.2 and C5).

Every final and error callback carries a verdict the validator accepts; a stop's verdict is "stopped"; `checked` is
always `verdict == "done"`. The builders, the validator, the table a report with no verdict of its own is given one
by, and a caller's `checked` that disagrees with its verdict (fail closed).
"""

import ast
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, messaging, state, verdict, verify

ROOT = Path(__file__).resolve().parents[1] / "decurix_agent"


class TheBuilders(unittest.TestCase):
    def test_a_done_with_a_passing_check(self):
        v = verdict.make("done", "verify", why="saved", checks=[verdict.check("judge", "pass", "model", "the row")])
        self.assertEqual(verdict.validate(v), [])
        self.assertEqual(v["basis"], ["model"])
        self.assertTrue(verdict.is_done(v))

    def test_a_done_needs_a_pass_and_no_fail_or_not_made(self):
        self.assertTrue(verdict.validate(verdict.make("done", "verify")))
        for bad in ("fail", "not_made"):
            v = verdict.make("done", "verify", checks=[verdict.check("a", "pass", "code"), verdict.check("b", bad, "code")])
            self.assertTrue(verdict.validate(v), bad)
            self.assertFalse(verdict.is_done(v))

    def test_a_legacy_done_has_no_checks_and_no_basis(self):
        v = verdict.make("done", "legacy")
        self.assertEqual(verdict.validate(v), [])
        self.assertEqual(v["basis"], [])
        self.assertTrue(verdict.validate(dict(v, basis=["code"])))

    def test_enums_and_limits(self):
        v = verdict.make("done", "verify", checks=[verdict.check("j", "pass", "model")])
        for key, value in (("verdict", "maybe"), ("by", "me"), ("schema", "x"), ("basis", ["luck"])):
            self.assertTrue(verdict.validate(dict(v, **{key: value})), key)
        self.assertTrue(verdict.validate(dict(v, checks=[verdict.check("j", "pass", "model")] * 65)))
        self.assertTrue(verdict.validate(dict(v, checks=[dict(v["checks"][0], refs=["r"] * 17)])))
        self.assertTrue(verdict.validate("done"))

    def test_strings_are_cut_and_scrubbed(self):
        with mock.patch.object(verdict.privacy, "scrub", lambda text: text.replace("Hunter22!", "[withheld]")):
            v = verdict.make("not_done", "verify", why="x" * 900 + " Hunter22!",
                             checks=[verdict.check("c", "fail", "code", "the password Hunter22!", refs=["y" * 900])],
                             evidence=["Hunter22! seen"])
        self.assertEqual(len(v["why"]), verdict.MAX_TEXT)
        self.assertNotIn("Hunter22!", json.dumps(v))
        self.assertEqual(len(v["checks"][0]["refs"][0]), verdict.MAX_TEXT)
        self.assertEqual(verdict.validate(v), [])

    def test_unknown_fields_are_kept_valid(self):
        v = dict(verdict.make("unchecked", "none"), later_field={"x": 1})
        self.assertEqual(verdict.validate(v), [])


class TheDeriveTable(unittest.TestCase):
    def test_the_table(self):
        for (checked, kind, gated, stopped), (want, by) in (
                ((True, "final", False, False), ("done", "legacy")),
                ((False, "final", True, False), ("unchecked", "worker")),
                ((False, "error", True, False), ("not_done", "worker")),
                ((False, "error", False, False), ("not_done", "worker")),
                ((True, "error", True, False), ("not_done", "legacy")),
                ((False, "final", False, False), ("unchecked", "none")),
                ((True, "error", True, True), ("stopped", "worker"))):
            v = verdict.derive(checked, kind, gated=gated, stopped=stopped)
            with self.subTest((checked, kind, gated, stopped)):
                self.assertEqual((v["verdict"], v["by"]), (want, by))
                self.assertEqual(verdict.validate(v), [])


class VerifyOutcomeCarriesItsChecks(unittest.TestCase):
    def test_each_kind_of_finding(self):
        d = verify.outcome({"verdict": "done", "confidence": 0.9, "seen": "saved"})
        self.assertEqual([(c["id"], c["result"], c["basis"]) for c in d["checks"]], [("judge", "pass", "model")])
        self.assertEqual(d["basis"], ["model"])
        d = verify.outcome({"verdict": "done", "confidence": 0.9}, problems=["a total is wrong"], not_made=["x"])
        self.assertEqual([(c["id"], c["result"]) for c in d["checks"]],
                         [("code.0", "fail"), ("not_made.0", "not_made"), ("judge", "pass")])
        d = verify.outcome(None, error=RuntimeError("down"))
        self.assertEqual([(c["id"], c["result"]) for c in d["checks"]], [("check.error", "not_made")])
        d = verify.outcome({"verdict": "unsure"})
        self.assertEqual(d["checks"][0]["result"], "not_made")
        self.assertEqual(d["basis"], [])

    def test_the_old_keys_are_unchanged(self):
        d = verify.outcome({"verdict": "not_done", "seen": "empty"})
        self.assertEqual((d["status"], d["checked"], d["kind"]), ("not_done", True, "error"))

    def test_its_verdict_is_valid_and_agrees(self):
        for given in ({"verdict": "done", "confidence": 0.95}, {"verdict": "not_done"}, {"verdict": "unsure"}, None):
            d = verify.outcome(given)
            v = verdict.from_outcome(d, "verify")
            self.assertEqual(verdict.validate(v), [], given)
            self.assertEqual(v["verdict"] == "done", d["status"] == verify.DONE)


class TheCallback(unittest.TestCase):
    """What write_final_reply sends: a verdict always, and checked == (verdict == "done")."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.sent, self.logged = [], []
        for p in (mock.patch.object(messaging, "_post_querytext",
                                    lambda payload: self.sent.append(payload) or ("sent", "")),
                  mock.patch.object(messaging, "_keep_facts", lambda *a, **k: None),
                  mock.patch.object(messaging, "_final_gate", None),
                  mock.patch.object(messaging.progress, "flush", lambda: None),
                  mock.patch.object(messaging, "log", lambda line: self.logged.append(line)),
                  mock.patch.object(config, "QUERYTEXT_DIR", Path(tmp.name)),
                  mock.patch.object(config, "DECURIX_DIR", Path(tmp.name)),
                  mock.patch.object(state, "current_task_id", "t0123456789abcdef")):
            p.start()
            self.addCleanup(p.stop)

    def last(self):
        payload = self.sent[-1]
        self.assertEqual(verdict.validate(payload["verdict"]), [])
        self.assertEqual(payload["checked"], payload["verdict"]["verdict"] == "done")
        return payload

    def test_derived_when_not_given(self):
        messaging.write_final_reply("Done.", kind="final", checked=True)
        self.assertEqual(self.last()["verdict"]["by"], "legacy")
        messaging.write_final_reply("Not checked.", kind="final", gated=True)
        self.assertEqual(self.last()["verdict"]["verdict"], "unchecked")
        messaging.write_final_reply("It broke.", kind="error", gated=True)
        self.assertEqual(self.last()["verdict"]["verdict"], "not_done")

    def test_a_given_verdict_is_sent_and_decides_checked(self):
        v = verdict.make("done", "skill_gate", checks=[verdict.check("files.read_back", "pass", "code")])
        messaging.write_final_reply("Done.", kind="final", checked=True, verdict=v)
        self.assertEqual(self.last()["verdict"]["by"], "skill_gate")
        self.assertTrue(self.last()["checked"])

    def test_a_disagreeing_checked_fails_closed(self):
        v = verdict.make("unchecked", "verify")
        messaging.write_final_reply("Done?", kind="final", checked=True, verdict=v)
        self.assertFalse(self.last()["checked"])
        self.assertTrue(any("sent as not checked" in line for line in self.logged))
        done = verdict.make("done", "verify", checks=[verdict.check("judge", "pass", "model")])
        messaging.write_final_reply("Done.", kind="final", checked=False, verdict=done)
        self.assertEqual(self.last()["verdict"]["verdict"], "unchecked")
        self.assertTrue(any("disagree" in line for line in self.logged))

    def test_a_not_done_seen_by_a_check_is_not_checked_done(self):
        v = verdict.make("not_done", "verify", checks=[verdict.check("code.0", "fail", "code", "a total is wrong")])
        messaging.write_final_reply("Not done.", kind="error", checked=True, verdict=v)
        self.assertEqual(self.last()["verdict"]["verdict"], "not_done")
        messaging.write_final_reply("Not done.", kind="error", checked=True)
        self.assertEqual(self.last()["verdict"]["verdict"], "not_done")

    def test_an_invalid_verdict_is_not_sent_as_done(self):
        bad = {"schema": verdict.SCHEMA, "verdict": "done", "by": "verify", "checks": []}
        messaging.write_final_reply("Done.", kind="final", checked=True, verdict=bad)
        self.assertEqual(self.last()["verdict"]["verdict"], "unchecked")
        self.assertFalse(self.last()["checked"])

    def test_a_stop(self):
        messaging.write_stopped("Stopped, as you said, before step 3.")
        payload = self.last()
        self.assertEqual(payload["verdict"]["verdict"], "stopped")
        self.assertEqual(payload["kind"], "error")
        self.assertFalse(payload["checked"])


class EveryPathSendsAValidVerdict(TheCallback):
    """The emitting paths, driven with fakes, each send a verdict the validator accepts, and checked follows it."""

    def succeed(self, checked_by, files=("/tmp/out.xlsx",), ledger=False, facts=None):
        from decurix_agent import produced, skill_task
        run = {"skill": {"name": "s", "steps": [], "version": 1}, "patches": [], "asked_ids": [],
               "ladder": mock.Mock(), "one_off": False}
        with mock.patch.object(skill_task.produced, "files", lambda *a, **k: list(files)), \
                mock.patch.object(skill_task.produced, "drafts", lambda: []), \
                mock.patch.object(skill_task.source_guard, "is_source", lambda *a: False), \
                mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: facts or produced.FileFacts()), \
                mock.patch("decurix_agent.computer.records.all_read_back", lambda task_id=None: ledger), \
                mock.patch.object(skill_task, "_own_run"), mock.patch.object(skill_task, "_record_verdict"), \
                mock.patch.object(skill_task.experience, "commit", lambda *a, **k: []), \
                mock.patch.object(skill_task.experience, "note_verdict"), \
                mock.patch.object(skill_task.experience, "advice", lambda s: ""), \
                mock.patch.object(skill_task.skills, "save"):
            skill_task._succeed(run, {"text": "Saved it."}, checked_by, seen="the row is there")
        return self.last()

    def test_a_taught_runs_success_by_each_check(self):
        for by, want in (("screen", "screen.every_step"), ("model", "judge"), ("person", "person")):
            payload = self.succeed(by)
            self.assertEqual(payload["verdict"]["verdict"], "done", by)
            self.assertIn(want, [c["id"] for c in payload["verdict"]["checks"]])
        payload = self.succeed("screen", files=(), ledger=True)
        self.assertEqual(payload["verdict"]["basis"], ["ledger", "screen"])

    def test_a_taught_runs_file_fault_is_not_done(self):
        from decurix_agent import produced
        payload = self.succeed("screen", facts=produced.FileFacts(file_faults=["a placeholder left"]))
        self.assertEqual(payload["verdict"]["verdict"], "not_done")
        self.assertFalse(payload["checked"])

    def test_each_stop(self):
        from decurix_agent import skill_task, watchers
        skill_task._stopped("before step 2.")
        self.assertEqual(self.last()["verdict"]["verdict"], "stopped")
        watchers._called_off("stop")
        self.assertEqual(self.last()["verdict"]["verdict"], "stopped")
        self.assertEqual(self.last()["kind"], "error")


def _calls_of(name):
    for path in sorted(ROOT.rglob("*.py")):
        if "thebot" in path.relative_to(ROOT).parts:
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"))
        for func in ast.walk(tree):
            if not isinstance(func, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            for node in ast.walk(func):
                if isinstance(node, ast.Call):
                    f = node.func
                    called = f.attr if isinstance(f, ast.Attribute) else getattr(f, "id", None)
                    if called == name:
                        yield path.relative_to(ROOT).as_posix(), func.name, node


class EveryStopGoesThroughWriteStopped(unittest.TestCase):
    def test_no_write_final_reply_begins_with_stopped(self):
        found = []
        for where, func, node in _calls_of("write_final_reply"):
            if func == "write_stopped" or not node.args:
                continue
            first = node.args[0]
            text = first.value if isinstance(first, ast.Constant) else (
                first.values[0].value if isinstance(first, ast.JoinedStr) and first.values
                and isinstance(first.values[0], ast.Constant) else None)
            if isinstance(text, str) and text.startswith("Stopped"):
                found.append(f"{where}:{node.lineno} ({func})")
        self.assertEqual(found, [])


# The paths that still send checked=True with no verdict of their own, and so get a `legacy` done (no basis, never
# counted towards trust). This list may only shrink: a new path says what it checked (verdict=...).
LEGACY = {
    "tool_task.py:perform",      # a learned tool's run, checked by its own checks (tool_task._draft_once)
    "workflow_task.py:_report",  # a workflow's stage report, checked by the run's artifact checks
}


class TheLegacyCallersOnlyShrink(unittest.TestCase):
    def test_the_list(self):
        found = set()
        for where, func, node in _calls_of("write_final_reply"):
            kw = {k.arg: k.value for k in node.keywords}
            checked = kw.get("checked")
            if "verdict" in kw or checked is None:
                continue
            if isinstance(checked, ast.Constant) and checked.value is False:
                continue
            found.add(f"{where}:{func}")
        self.assertEqual(sorted(found - set(LEGACY)), [], "a new caller relies on the legacy verdict")
        self.assertEqual(sorted(set(LEGACY) - found), [], "a caller left the list: take it out of LEGACY")


if __name__ == "__main__":
    unittest.main()


class AVerdictAlwaysFits(unittest.TestCase):
    """Review round 2, B7: a schema-valid verdict could be ~660 KB; the control plane reads 32 KB, so the outcome was
    refused and dropped. A verdict is now bounded, its verdict never changed, failing checks kept first."""

    def big(self, verdict_kind="not_done"):
        detail, ref = "d" * 600, "r" * 600
        checks = [verdict.check(f"code.{n}", "fail" if n % 3 == 0 else "pass", "code", detail, refs=[ref] * 16)
                  for n in range(64)]
        if verdict_kind == "done":
            checks = [dict(c, result="pass") for c in checks]
        return {"schema": verdict.SCHEMA, "verdict": verdict_kind, "by": "verify", "why": "w" * 600, "basis": ["code"],
                "checks": checks, "evidence": ["e" * 600] * 64}

    def test_bound_keeps_the_verdict_and_every_failing_check_first(self):
        for kind in ("not_done", "done"):
            v = verdict.bound(self.big(kind))
            self.assertLessEqual(len(json.dumps(v, ensure_ascii=False).encode("utf-8")), verdict.MAX_BYTES)
            self.assertEqual(v["verdict"], kind)
            self.assertTrue(v["truncated"])
            self.assertEqual(verdict.validate(v), [], kind)
        v = verdict.bound(self.big("not_done"))
        self.assertTrue(any(c["result"] == "fail" for c in v["checks"]))

    def test_long_refs_are_clipped_before_a_failing_check_is_dropped(self):
        """Review round 3, nit 3: ten failing checks with 600-character refs - only five were listed."""
        checks = [verdict.check(f"code.{n}", "fail", "code", "d" * 600, refs=["r" * 600] * 16) for n in range(10)]
        v = verdict.bound({"schema": verdict.SCHEMA, "verdict": "not_done", "by": "verify", "why": "w",
                           "basis": ["code"], "checks": checks, "evidence": []})
        self.assertLessEqual(len(json.dumps(v, ensure_ascii=False).encode("utf-8")), verdict.MAX_BYTES)
        self.assertEqual([c["id"] for c in v["checks"]], [f"code.{n}" for n in range(10)])
        self.assertTrue(all(len(r) <= 160 for c in v["checks"] for r in c["refs"]))
        self.assertEqual(verdict.validate(v), [])

    def test_send_query_bounds_a_verdict_its_caller_assembled(self):
        """Review round 3, nit 2 (mutation N5): a hand-made verdict over 12 KB goes out bounded on the first post."""
        from decurix_agent import messaging
        posted = []
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(messaging, "_post_querytext", lambda p: (posted.append(p), ("sent", ""))[1]), \
                mock.patch.object(messaging.progress, "flush", lambda: None), \
                mock.patch.object(config, "DECURIX_DIR", Path(tmp)), mock.patch.object(config, "WORK_DIR", Path(tmp)):
            big = self.big("not_done")
            self.assertGreater(len(json.dumps(big).encode("utf-8")), verdict.MAX_BYTES)
            self.assertTrue(messaging.send_query("f.txt", "not done", kind="error", reply="not done", checked=False,
                                                 verdict=big, task_id="t0123456789abcdef"))
        first = posted[0]["verdict"]
        self.assertLessEqual(len(json.dumps(first, ensure_ascii=False).encode("utf-8")), verdict.MAX_BYTES)
        self.assertTrue(first["truncated"])
        self.assertEqual(first["verdict"], "not_done")

    def test_more_than_64_checks_are_summed_up_not_dropped_silently(self):
        checks = [verdict.check(f"c{n}", "pass", "code") for n in range(70)] + [verdict.check("bad", "fail", "code")]
        v = verdict.make("not_done", "verify", checks=checks)
        self.assertLessEqual(len(v["checks"]), verdict.MAX_CHECKS)
        self.assertIn("bad", [c["id"] for c in v["checks"]])
        self.assertTrue(any(c["id"].startswith("more.") for c in v["checks"]))

    def test_evidence_lines_are_counted_like_the_control_plane(self):
        v = dict(verdict.make("unchecked", "none"), evidence=["e"] * 65)
        self.assertTrue(verdict.validate(v))

    def test_the_outcome_is_delivered_by_a_32_kb_control_plane(self):
        from decurix_agent import messaging
        delivered = []

        def poster(payload):
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            if len(body) > 32768:
                return "refused", "too big"
            delivered.append(payload)
            return "sent", ""
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(messaging, "_post_querytext", poster), \
                mock.patch.object(messaging.progress, "flush", lambda: None), \
                mock.patch.object(config, "DECURIX_DIR", Path(tmp)), mock.patch.object(config, "WORK_DIR", Path(tmp)):
            self.assertTrue(messaging.send_query("f.txt", "x" * 40000, kind="error", reply="y" * 40000,
                                                 checked=False, verdict=self.big("not_done"), task_id="t0123456789abcdef"))
        self.assertEqual(delivered[-1]["verdict"]["verdict"], "not_done")
        self.assertEqual(verdict.validate(delivered[-1]["verdict"]), [])

    def test_a_refused_outcome_is_kept_to_send_again(self):
        from decurix_agent import messaging
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(messaging, "_post_querytext", lambda p: ("refused", "no")), \
                mock.patch.object(messaging.progress, "flush", lambda: None), \
                mock.patch.object(config, "DECURIX_DIR", Path(tmp)), mock.patch.object(config, "WORK_DIR", Path(tmp)):
            self.assertFalse(messaging.send_query("f.txt", "done", kind="final", reply="done", checked=False,
                                                  task_id="t0123456789abcdef"))
            self.assertEqual(len(list((Path(tmp) / "unsent").glob("*.json"))), 1)


class TheSharedFixtures(unittest.TestCase):
    """The control plane's own test vectors, read by both languages (H-CP-1)."""

    def test_the_600_character_unicode_verdict_validates(self):
        path = Path(__file__).resolve().parents[2] / "internal" / "tasks" / "testdata" / "verdict-unicode-600.json"
        if not path.exists():
            self.skipTest(f"{path} is not in this tree")
        doc = json.loads(path.read_text(encoding="utf-8"))
        self.assertEqual(verdict.validate(doc), [])
        longer = dict(doc, why=doc["why"] + "x")
        self.assertTrue(verdict.validate(longer))  # 601 characters

    def test_the_control_planes_graders_are_known(self):
        for by in ("readiness", "practice", "eval"):
            v = verdict.make("not_done", by, checks=[verdict.check("grade", "fail", "code")])
            self.assertEqual(verdict.validate(v), [], by)


class MoreOutcomes(unittest.TestCase):
    """Review round 2: a declined workflow stage is a stop; an answer on a message's own task carries a verdict."""

    def test_a_declined_stage_is_stopped(self):
        from decurix_agent import gated_command, workflow_task
        sent = []
        with mock.patch.object(workflow_task, "_awaiting_start", {"run_id": "r1", "step_id": "s2", "material": {},
                                                                 "question": "May I start s2?"}), \
                mock.patch.object(gated_command, "granted", lambda *a: False), \
                mock.patch("decurix_agent.messaging.write_final_reply", lambda t, **k: sent.append(k)):
            workflow_task.answer_start("no", None)
        self.assertTrue(sent[-1]["stopped"])
        self.assertEqual(sent[-1]["kind"], "error")

    def test_reply_on_task_sends_a_verdict(self):
        from decurix_agent import messaging
        sent = []
        with mock.patch.object(messaging, "send_query", lambda *a, **k: sent.append(k) or True):
            messaging.reply_on_task("t0123456789abcdef", "Noted: the change is taken.")
        self.assertEqual(sent[-1]["verdict"]["verdict"], "unchecked")
        self.assertEqual(verdict.validate(sent[-1]["verdict"]), [])
