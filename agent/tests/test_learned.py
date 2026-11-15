"""The learned-artefact ledger, a person's lock, use counts and staleness, and "/skill <verb>" (learned.py)."""

import ast
import json
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import app_skills, learned, methods, playbooks, procedures, skill_commands
from decurix_agent.computer import skills

PACKAGE = Path(__file__).resolve().parents[1] / "decurix_agent"


def a_skill(name="pay-bill", **more):
    return {"schema": skills.SCHEMA, "name": name, "title": "Pay a supplier bill", "goal": "pay a supplier bill",
            "status": skills.READY, "steps": [], "runs": 0, **more}


class Stores(unittest.TestCase):
    """Every store in a folder of the test's own."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        base = Path(self.tmp.name)
        self.dirs = {}
        for module, attr, sub in ((skills, "ROOT", "skills"), (methods, "ROOT", "methods"),
                                  (app_skills, "LEARNED", "app_skills"), (playbooks, "ROOT", "playbooks"),
                                  (playbooks, "TOOLS", "tools"), (procedures, "DIR", "procedures")):
            self.dirs[sub] = base / sub
            patcher = mock.patch.object(module, attr, base / sub)
            patcher.start()
            self.addCleanup(patcher.stop)
        for patcher in (mock.patch("decurix_agent.task_context.current", lambda: "t_1"),):
            patcher.start()
            self.addCleanup(patcher.stop)


class TheLedger(Stores):
    def test_every_write_is_one_entry_with_its_actor_task_and_both_versions_kept(self):
        skills.save(a_skill(), actor="person")
        skill = skills.load("pay-bill")
        skill["title"] = "Pay a bill"
        skills.save(skill, actor="learner", task_id="t_9")
        entries = learned.history("skill", "pay-bill")
        self.assertEqual([e["actor"] for e in entries], ["learner", "person"])
        self.assertEqual(entries[0]["task_id"], "t_9")
        self.assertEqual(entries[1]["task_id"], "t_1", "the task being done is named when the caller names none")
        self.assertIsNone(entries[1]["before"])
        self.assertEqual(entries[0]["before"], entries[1]["after"])
        for digest in (entries[0]["before"], entries[0]["after"]):
            self.assertTrue((self.dirs["skills"] / learned.BLOBS / digest).is_file())
        self.assertEqual(learned.status("skill", "pay-bill")["patch_count"], 2)
        self.assertEqual(learned.status("skill", "pay-bill")["created_by"], "person")
        self.assertEqual([s["name"] for s in skills.all_skills()], ["pay-bill"], "the ledger is not read as a skill")

    def test_an_unchanged_write_is_no_entry(self):
        skills.save(a_skill(), actor="person")
        skills.save(a_skill(), actor="run")
        self.assertEqual(len(learned.history("skill", "pay-bill")), 1)

    def test_a_write_outside_its_store_or_by_no_actor_known_is_refused(self):
        with self.assertRaises(ValueError):
            learned.write("skill", "x", Path(self.tmp.name) / "elsewhere.json", {}, actor="person")
        with self.assertRaises(ValueError):
            skills.save(a_skill(), actor="somebody")

    def test_rollback_puts_back_the_version_before_and_is_itself_on_the_ledger(self):
        skills.save(a_skill(), actor="person")
        first = (self.dirs["skills"] / "pay-bill.json").read_bytes()
        skills.save(a_skill(runs=3), actor="run")
        change = learned.history("skill", "pay-bill")[0]
        undone = learned.rollback(change["id"])
        self.assertEqual((self.dirs["skills"] / "pay-bill.json").read_bytes(), first)
        self.assertEqual(undone["of"], change["id"])
        self.assertEqual(undone["actor"], "rollback")
        self.assertEqual(learned.history("skill", "pay-bill")[0]["id"], undone["id"])

    def test_rollback_fails_closed_when_the_file_changed_since(self):
        skills.save(a_skill(), actor="person")
        skills.save(a_skill(runs=3), actor="run")
        change = learned.history("skill", "pay-bill")[0]
        skills.save(a_skill(runs=4), actor="run")
        now = (self.dirs["skills"] / "pay-bill.json").read_bytes()
        with self.assertRaises(learned.Refused):
            learned.rollback(change["id"])
        self.assertEqual((self.dirs["skills"] / "pay-bill.json").read_bytes(), now)

    def test_rollback_fails_closed_without_an_exact_copy(self):
        skills.save(a_skill(), actor="person")
        skills.save(a_skill(runs=3), actor="run")
        change = learned.history("skill", "pay-bill")[0]
        (self.dirs["skills"] / learned.BLOBS / change["before"]).write_text("{}", encoding="utf-8")
        with self.assertRaises(learned.Refused):
            learned.rollback(change["id"])

    def test_rolling_back_a_making_removes_it_and_a_removal_is_brought_back(self):
        skills.save(a_skill(), actor="person")
        made = learned.history("skill", "pay-bill")[0]
        skills.delete("pay-bill", actor="person")
        learned.rollback(learned.history("skill", "pay-bill")[0]["id"])
        self.assertEqual(skills.load("pay-bill")["name"], "pay-bill")
        learned.rollback(learned.history("skill", "pay-bill")[0]["id"])  # (the restore, undone: gone again)
        self.assertFalse((self.dirs["skills"] / "pay-bill.json").exists())
        self.assertIsNone(made["before"])

    def test_only_a_person_rolls_back(self):
        skills.save(a_skill(), actor="person")
        with self.assertRaises(learned.Locked):
            learned.rollback(learned.history("skill", "pay-bill")[0]["id"], actor="learner")

    def test_a_secret_known_to_the_task_never_reaches_a_copy(self):
        from decurix_agent.computer import privacy
        with mock.patch.object(privacy, "_withhold_known", lambda text, shapes=True: text.replace("Copper71meadow",
                                                                                                    "[withheld]")):
            skills.save(a_skill(note="typed Copper71meadow"), actor="person")
        for blob in (self.dirs["skills"] / learned.BLOBS).iterdir():
            self.assertNotIn("Copper71meadow", blob.read_text(encoding="utf-8"))
        self.assertNotIn("Copper71meadow", (self.dirs["skills"] / learned.LEDGER).read_text(encoding="utf-8"))


class APersonsLock(Stores):
    def setUp(self):
        super().setUp()
        skills.save(a_skill(steps=[{"id": "s1", "n": 1, "kind": "click", "summary": "Save"}]), actor="person")
        learned.set_flag("skill", "pay-bill", "locked", True)
        self.before = (self.dirs["skills"] / "pay-bill.json").read_bytes()

    def unchanged(self):
        self.assertEqual((self.dirs["skills"] / "pay-bill.json").read_bytes(), self.before)

    def test_every_worker_actor_is_refused_and_a_persons_goes_through(self):
        for actor in ("learner", "relearn", "correction", "branch", "agent", "retire", None):
            with self.subTest(actor=actor), self.assertRaises(learned.Locked):
                skills.save(a_skill(title="Changed"), actor=actor)
            self.unchanged()
        with self.assertRaises(learned.Locked):
            skills.delete("pay-bill", actor="retire")
        self.unchanged()
        skills.save(a_skill(title="Changed by the person"), actor="person")
        self.assertEqual(skills.load("pay-bill")["title"], "Changed by the person")

    def test_a_runs_end_keeps_how_it_went_and_nothing_it_would_have_changed(self):
        changed = a_skill(title="Learned title", runs=5, experience={"runs": [{"outcome": "verified"}]})
        skills.save(changed, actor="run")
        kept = skills.load("pay-bill")
        self.assertEqual(kept["runs"], 5)
        self.assertEqual(kept["experience"], {"runs": [{"outcome": "verified"}]})
        self.assertEqual(kept["title"], "Pay a supplier bill")
        self.assertEqual(kept["steps"][0]["summary"], "Save")

    def test_a_branch_is_not_kept(self):
        from decurix_agent import repairs
        run = {"skill": skills.load("pay-bill"), "one_off": False}
        repairs._update_skill(run, lambda s: s.setdefault("branches", {}).update({"1": {"v": 1}}))
        self.unchanged()
        self.assertIn("branches", run["skill"], "this run still has it")

    def test_a_correction_holds_for_the_run_only_and_the_person_is_told(self):
        from decurix_agent import messaging, skill_task, state
        said = []
        run = {"skill": skills.load("pay-bill"), "one_off": False}
        with mock.patch.object(state, "reply_secret", False, create=True), \
                mock.patch("decurix_agent.progress.emit", lambda line, *a, **k: said.append(line)), \
                mock.patch.object(messaging, "post_learned", lambda *a, **k: self.fail("kept a refused correction")):
            skill_task._correct(run, "the supplier code goes in the second box")
        self.unchanged()
        self.assertTrue(run["skill"]["corrections"])
        self.assertIn("locked", said[0])

    def test_a_persons_answer_during_a_run_is_not_written_into_it(self):
        from decurix_agent import guidance, messaging, skill_task
        guided = {"run": {"skill": skills.load("pay-bill"), "one_off": False}, "asked": "Which account pays it?"}
        with mock.patch.object(guidance, "get", lambda: guided), \
                mock.patch.object(messaging, "post_learned", lambda *a, **k: self.fail("kept a refused answer")):
            skill_task.learn_answer("the current account, from now on")
        self.unchanged()

    def test_unlock_lets_the_worker_learn_again(self):
        learned.set_flag("skill", "pay-bill", "locked", False)
        skills.save(a_skill(title="Learned title"), actor="learner")
        self.assertEqual(skills.load("pay-bill")["title"], "Learned title")

    def test_only_a_person_locks(self):
        with self.assertRaises(learned.Locked):
            learned.set_flag("skill", "pay-bill", "locked", False, actor="agent")
        self.assertTrue(learned.is_locked("skill", "pay-bill"))
        ops = [e["op"] for e in learned.history("skill", "pay-bill")]
        self.assertIn("lock", ops)


PLAN = {"scope": "the month's bills", "sources": ["the register"], "outputs": ["the return"],
        "commits": ["file the return"]}


class TheOtherStoresLocked(Stores):
    def test_a_locked_method_is_not_relearned(self):
        methods.begin()
        methods.note_job({"results": [{"Meta": {"plan": PLAN}}]})
        name = methods.keep("File the return for a client from their workbook")
        path = self.dirs["methods"] / f"{name}.json"
        before = path.read_bytes()
        learned.set_flag("method", name, "locked", True)
        methods.note_job({"results": [{"Meta": {"plan": {**PLAN, "scope": "every bill"}}}]})
        self.assertIsNone(methods.keep("File the return for another client from their workbook"))
        self.assertEqual(path.read_bytes(), before)
        self.assertEqual([e["actor"] for e in learned.history("method", name) if e["op"] == "write"], ["learner"])

    def test_a_locked_app_guide_is_not_replaced_by_a_newer_one(self):
        body = "The app keeps its menus on the left. Save with ctrl+s and check the status line before closing. " * 2
        app_skills.learn("ledger-app", "ledger app", "how it is worked", body, task_id="t_a")
        self.assertEqual(app_skills.promote("t_a"), ["ledger-app"])
        guide = self.dirs["app_skills"] / "ledger-app" / "SKILL.md"
        before = guide.read_bytes()
        learned.set_flag("app_guide", "ledger-app", "locked", True)
        app_skills.learn("ledger-app", "ledger app", "how it is worked", body + " Also F2 edits.", task_id="t_b")
        self.assertEqual(app_skills.promote("t_b"), [])
        self.assertEqual(guide.read_bytes(), before)

    def book(self):
        script = self.dirs["tools"] / "file-return.py"
        self.dirs["tools"].mkdir(parents=True, exist_ok=True)
        learned.write("tool_script", "file-return", script, "print('ok')\n", actor="learner", newline="\n")
        book = {"name": "file-return", "request": "file the return", "shape": ["file", "return"], "commands": ["x"],
                "uses": [], "misses": 0, "tool": {"path": str(script), "run": "python3 x", "unused": 0}}
        playbooks.save(book, actor="learner")
        learned.set_flag("playbook", "file-return", "locked", True)
        return book, script

    def test_a_locked_playbook_keeps_its_tool_counts_its_runs_and_is_not_retired(self):
        book, script = self.book()
        playbooks.tool_failed(book, "its result was wrong")
        self.assertTrue(script.exists())
        self.assertIn("tool", book)
        with self.assertRaises(learned.Locked):
            playbooks.save({**book, "commands": ["y"]}, actor="relearn")
        with mock.patch.dict(playbooks._run, {"offered": "file-return", "segments": []}):
            for _ in range(playbooks.RETIRE_AFTER + 1):
                playbooks._run["offered"] = "file-return"
                playbooks.missed("not done")
        kept = playbooks.load("file-return")
        self.assertIsNotNone(kept, "a locked playbook is not retired")
        self.assertEqual(len(kept["uses"]), playbooks.RETIRE_AFTER + 1)
        self.assertEqual(kept["commands"], ["x"])
        with self.assertRaises(learned.Locked):
            learned.remove("tool_script", "file-return", script, actor="retire")

    def test_a_locked_procedure_is_not_replaced(self):
        procedures._save("Weekly intake", "the intake", "1. read the inbox", "said", "t_1")
        path = self.dirs["procedures"] / "weekly-intake.json"
        before = path.read_bytes()
        learned.set_flag("procedure", "weekly-intake", "locked", True)
        with self.assertRaises(learned.Locked):
            procedures._save("Weekly intake", "the intake", "1. something else", "said", "t_2")
        self.assertEqual(path.read_bytes(), before)

    def test_every_call_that_writes_a_store_names_its_actor(self):
        missing = []
        for path in PACKAGE.rglob("*.py"):
            tree = ast.parse(path.read_text(encoding="utf-8"))
            for node in ast.walk(tree):
                if not isinstance(node, ast.Call):
                    continue
                func = node.func
                named = (isinstance(func, ast.Attribute) and func.attr in ("save", "delete")
                         and isinstance(func.value, ast.Name) and func.value.id in ("skills", "playbooks"))
                local = (isinstance(func, ast.Name) and func.id in ("save", "delete", "_drop_tool")
                         and path.name in ("skills.py", "playbooks.py"))
                remote = isinstance(func, ast.Attribute) and func.attr in ("write", "remove") and \
                    isinstance(func.value, ast.Name) and func.value.id == "learned"
                if not (named or local or remote):
                    continue
                if path.name == "playbooks.py" and isinstance(func, ast.Name) and func.id == "_drop_tool":
                    ok = len(node.args) >= 2
                else:
                    ok = any(k.arg == "actor" for k in node.keywords)
                if not ok:
                    missing.append(f"{path.relative_to(PACKAGE)}:{node.lineno}")
        self.assertEqual(missing, [], "a write to a learned store that does not say who made it")


class UseAndStaleness(Stores):
    def test_uses_and_views_are_counted(self):
        skills.save(a_skill(), actor="person")
        learned.note_use("skill", "pay-bill")
        learned.note_use("skill", "pay-bill")
        learned.note_view("skill", "pay-bill")
        shown = learned.status("skill", "pay-bill")
        self.assertEqual((shown["use_count"], shown["view_count"]), (2, 1))
        self.assertIsNotNone(shown["last_used"])
        self.assertEqual(skills.summary(skills.load("pay-bill"))["usage"]["use_count"], 2)

    def test_unused_for_the_stretch_is_stale_unless_pinned_or_a_schedule_names_it(self):
        skills.save(a_skill(), actor="person")
        later = time.time() + 121 * 86400
        self.assertEqual(learned.status("skill", "pay-bill")["state"], learned.ACTIVE)
        self.assertEqual(learned.status("skill", "pay-bill", now=later)["state"], learned.STALE)
        self.assertEqual(learned.status("skill", "pay-bill", now=later, referenced={"pay-bill"})["state"],
                         learned.ACTIVE)
        with mock.patch.dict("os.environ", {"DECURIX_STALE_DAYS": "400"}):
            self.assertEqual(learned.status("skill", "pay-bill", now=later)["state"], learned.ACTIVE)
        learned.set_flag("skill", "pay-bill", "pinned", True)
        self.assertEqual(learned.status("skill", "pay-bill", now=later)["state"], learned.ACTIVE)
        self.assertTrue(skills.load("pay-bill"), "stale is never a deletion")

    def test_a_use_resets_the_stretch(self):
        skills.save(a_skill(), actor="person")
        later = time.time() + 121 * 86400
        with mock.patch("decurix_agent.learned.time.time", lambda: later - 86400):
            learned.note_use("skill", "pay-bill")
        self.assertEqual(learned.status("skill", "pay-bill", now=later)["state"], learned.ACTIVE)

    def test_archived_is_a_persons_is_not_offered_and_comes_back(self):
        skills.save(a_skill(), actor="person")
        learned.set_flag("skill", "pay-bill", "archived", True)
        self.assertEqual(learned.status("skill", "pay-bill")["state"], learned.ARCHIVED)
        self.assertEqual(skills.search("pay supplier bill"), [])
        self.assertEqual(skills.find("pay-bill")[0]["name"], "pay-bill", "a person may still run it by name")
        learned.set_flag("skill", "pay-bill", "archived", False)
        self.assertEqual([s["name"] for s in skills.search("pay supplier bill")], ["pay-bill"])

    def test_an_archived_method_is_not_offered(self):
        methods.begin()
        methods.note_job({"results": [{"Meta": {"plan": PLAN}}]})
        name = methods.keep("File the return for a client from their workbook")
        self.assertIsNotNone(methods.find("File the return for another client from their workbook"))
        learned.set_flag("method", name, "archived", True)
        self.assertIsNone(methods.find("File the return for another client from their workbook"))
        self.assertEqual(methods.context("File the return for another client from their workbook"), "")


class WhatAPersonTypes(Stores):
    def test_the_verbs_need_a_target_so_a_skill_named_like_one_still_runs(self):
        self.assertEqual(skill_commands.parse_manage("/skill lock pay-bill"), ("lock", "pay-bill"))
        self.assertEqual(skill_commands.parse_manage("/skills history method:file-return"),
                         ("history", "method:file-return"))
        self.assertIsNone(skill_commands.parse_manage("/skill history"))
        self.assertIsNone(skill_commands.parse_manage("/skill lock amount=5"))
        self.assertIsNone(skill_commands.parse_manage("/skill pay-bill"))
        self.assertEqual(skill_commands.parse_manage("/skill rollback abc123\nmore lines"), ("rollback", "abc123"))

    def test_lock_history_and_rollback_by_their_words(self):
        skills.save(a_skill(), actor="person")
        skills.save(a_skill(runs=2), actor="run")
        self.assertIn("Locked", learned.answer("lock", "pay-bill"))
        self.assertTrue(learned.is_locked("skill", "pay-bill"))
        shown = learned.answer("history", "pay-bill")
        change = learned.history("skill", "pay-bill")[1]  # (the newest is the lock)
        self.assertIn(change["id"], shown)
        self.assertIn("by run", shown)
        self.assertIn(f"back as it was before change {change['id']}", learned.answer("rollback", change["id"]))
        self.assertEqual(skills.load("pay-bill")["runs"], 0)
        with self.assertRaises(ValueError):
            learned.answer("history", "no-such-skill")
        with self.assertRaises(ValueError):
            learned.answer("lock", "recipe:pay-bill")
        listing = learned.answer("learned", "all")
        self.assertIn("skill `pay-bill`: active, locked", listing)

    def test_the_request_is_answered_without_a_model(self):
        from decurix_agent import skill_task
        skills.save(a_skill(), actor="person")
        replies = []
        with mock.patch.object(skill_task, "write_final_reply", lambda text, **k: replies.append((text, k))), \
                mock.patch.object(skill_task, "_begin", lambda *a: None):
            skill_task.perform("/skill pin pay-bill")
            skill_task.perform("/skill history nothing-here")
        self.assertIn("Pinned", replies[0][0])
        self.assertEqual(replies[1][1].get("kind"), "error")
        self.assertTrue(learned.status("skill", "pay-bill")["pinned"])


class ThePhrasingOfWhatIsKept(unittest.TestCase):
    def test_lessons_are_facts_and_leave_out_dead_ends_and_failures(self):
        from decurix_agent import kept_facts, lessons
        text = lessons.DISTILL_SYSTEM
        self.assertIn("as a fact about the person or their work, not an order", text)
        self.assertIn("Never keep: a way that failed", text)
        self.assertNotIn("as one instruction", text)
        self.assertIn("a claim that a tool, app", kept_facts.SYSTEM)


if __name__ == "__main__":
    unittest.main()
