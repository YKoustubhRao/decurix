"""
A form field's unit, as the person answered it (unit_answers.py; the learning loop's priority in docs/follow-ups.md,
review round 2): a field whose label states no unit (a tax table's "Rate", no "%") made every save of "18%" against
"18" wait on the person, once per task, every task. The save now asks one numbered question of its own, and the
person's recorded reply - their judgement on the control plane, never a note of the agent's - settles that field on
that form of that application, for this task and the later ones (a learned "field_unit" row). A control plane that
cannot be read backs nothing.

The application, its fields and figures are invented.
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, messaging, person_record, state, task_context, unit_answers, watchers
from decurix_agent.computer import records

TASK = "t00000000000000ab"
JUDGED = "j_0123456789abcdef"
HOST, FORM = "erp.example.test", "/app/tax-template"
PLACE = (HOST, FORM)


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        task_context._derived.clear()
        unit_answers.reset()
        self.judgements = []   # what the control plane recorded of the person on this task
        self.learned = []      # the learned field_unit rows it keeps
        self.sent = []
        for p in (mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.object(state, "current_task_id", TASK),
                  mock.patch.object(state, "current_task", "Enter the tax template."),
                  mock.patch.object(state, "asked_query_id", ""),
                  mock.patch.object(records, "field_place", lambda *a, **k: self.place),
                  mock.patch.object(person_record, "judgements", lambda task_id, fresh=False: list(self.judgements)),
                  mock.patch.object(person_record, "field_units", lambda: list(self.learned)),
                  mock.patch.object(messaging, "send_query", lambda *a, **k: self.sent.append((a, k)) or True),
                  mock.patch.object(messaging, "_query_gate", lambda *a: True)):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(task_context._derived.clear)
        self.addCleanup(unit_answers.reset)
        self.place = PLACE

    def rec(self, want="18%"):
        return {"key": "GST 18", "source": "rates.pdf", "source_order": None,
                "fields": {"Rate": {"intent": want, "from": "source"}}}

    def save(self, shown="18", want="18%"):
        return records.presave(self.rec(want), [("Rate", shown)])[0]

    def reply(self, text, judgement=JUDGED):
        """The person's reply, through the watcher; what the engine is handed."""
        path = self.root / "reply.txt"
        path.write_text(text, encoding="utf-8")
        state.message_facts[path.name] = {"secret": False, "judgement": judgement}
        handed = []

        class Started:
            def __init__(self, target=None, args=(), **kw):
                handed.append(args[0] if args else None)

            def start(self):
                pass

        with mock.patch.object(watchers, "is_reply_file", lambda name: True), \
                mock.patch.object(watchers.threading, "Thread", Started), \
                mock.patch.object(watchers.lessons, "note", lambda *a, **k: None), \
                mock.patch.object(watchers.skill_task, "learn_answer", lambda *a, **k: None), \
                mock.patch.object(watchers, "_mark_handled", lambda p: None):
            watchers.answer_waiting(path, text, already_marked=True)
        return handed[-1] if handed else None

    def recorded(self, text, asked=None):
        """The control plane's record of the person's reply to the question that went out."""
        asked = asked if asked is not None else self.sent[-1][1].get("reply") or self.sent[-1][0][1]
        self.judgements.append({"id": JUDGED, "task_id": TASK, "text": text, "asked": asked,
                                "actor": {"kind": "person"}})


class ThePlace(unittest.TestCase):
    def test_the_form_is_the_path_without_its_record(self):
        self.assertEqual(unit_answers.place_of("http://ERP.example.test:8000/app/tax-template/new-tax-template-1?x=1"),
                         ("erp.example.test:8000", "/app/tax-template"))
        self.assertEqual(unit_answers.place_of("https://erp.example.test/settings"), ("erp.example.test", "/settings"))
        self.assertEqual(unit_answers.place_of("https://erp.example.test/"), ("erp.example.test", ""))
        self.assertEqual(unit_answers.place_of(""), ("", ""))


class AskedOnceInTheTask(Base):
    def test_the_save_asks_its_own_question_and_the_recorded_yes_decides_it(self):
        blocking = self.save()
        self.assertEqual(len(blocking), 1)
        self.assertIn("this needs the person", blocking[0])
        q = unit_answers.question(TASK)
        self.assertEqual((q["label"], q["host"], q["form"], q["unit"]), ("Rate", HOST, FORM, "%"))
        messaging.ask_for_query("Which account head? 1. Output tax 2. Input tax")
        self.assertEqual(len(self.sent), 1)
        body = self.sent[-1][0][1]
        self.assertIn(unit_answers.sentence("Rate", HOST, FORM, "%"), body)
        self.assertNotIn("Output tax", body)  # never added to the engine's question
        handed = self.reply("1")
        self.assertIn("Not your question", handed)
        self.assertIn("They answered: yes", handed)
        self.assertIsNone(unit_answers.question(TASK))
        self.recorded("1")
        self.assertEqual(self.save(), [])

    def test_without_the_control_planes_record_nothing_is_backed_and_it_is_not_asked_again(self):
        self.save()
        messaging.ask_for_query("Anything?")
        self.reply("1")
        blocking = self.save()  # no judgement on the control plane (unread, or not there)
        self.assertEqual(len(blocking), 1)
        self.assertIn("cannot be read from the control plane", blocking[0])
        self.assertIsNone(unit_answers.question(TASK))

    def test_a_no_makes_the_figures_differ(self):
        self.save()
        messaging.ask_for_query("Anything?")
        self.assertIn("They answered: no", self.reply("2"))
        self.recorded("2")
        blocking = self.save()
        self.assertEqual(len(blocking), 1)
        self.assertIn("the person said this field does not take its figures in %", blocking[0])

    def test_a_reply_in_words_settles_nothing(self):
        self.save()
        messaging.ask_for_query("Anything?")
        handed = self.reply("yes it is a percentage")
        self.assertIn("picked neither option", handed)
        self.assertNotIn("query_id", unit_answers.question(TASK))
        self.recorded("yes it is a percentage")
        self.assertEqual(len(self.save()), 1)

    def test_a_judgement_on_another_question_backs_nothing(self):
        self.save()
        self.recorded("1", asked="Which account head? 1. Output tax 2. Input tax")
        self.assertEqual(len(self.save()), 1)
        self.recorded("1", asked=unit_answers.sentence("Rate", HOST, "/app/item", "%"))  # another form's field
        self.assertEqual(len(self.save()), 1)

    def test_a_unit_on_both_sides_or_a_labelled_unit_never_asks(self):
        self.assertEqual(records.presave({"key": "k", "source": "s", "source_order": None,
                                          "fields": {"Rate (%)": {"intent": "18%", "from": "source"}}},
                                         [("Rate (%)", "18")])[0], [])
        self.assertEqual(self.save("18 %", "18%"), [])
        self.assertIsNone(unit_answers.question(TASK))


class KeptForTheNextTask(Base):
    def row(self, **change):
        row = {"label": "Rate", "host": HOST, "form": FORM, "unit": "%", "takes": True, "source_text": "1",
               "source_asked": "Before I save: ... " + unit_answers.sentence("Rate", HOST, FORM, "%"),
               "judgement": JUDGED}
        row.update(change)
        return row

    def test_a_kept_answer_settles_the_same_field_on_the_same_form(self):
        self.learned = [self.row()]
        self.assertEqual(self.save(), [])
        self.assertIsNone(unit_answers.question(TASK))  # not asked again

    def test_a_kept_answer_is_held_to_its_field_form_application_and_reply(self):
        for change in ({"form": "/app/item"}, {"host": "other.example.test"}, {"label": "Amount"},
                       {"source_text": "2"}, {"source_asked": "Is Rate in %?"}, {"unit": "kg"}):
            self.learned = [self.row(**change)]
            self.assertEqual(len(self.save()), 1, change)
        self.learned = [self.row()]
        self.place = ("", "")  # no application named: nothing kept applies
        self.assertEqual(len(self.save()), 1)

    def test_keep_posts_each_answer_with_its_judgement(self):
        asked = "Before I save: x. " + unit_answers.sentence("Rate", HOST, FORM, "%")
        self.judgements = [{"id": JUDGED, "text": " 1. ", "asked": asked},
                           {"id": "j_aaaaaaaaaaaaaaaa", "text": "1", "asked": "Which head? 1. A 2. B"},
                           {"id": "j_bbbbbbbbbbbbbbbb", "text": "2",
                            "asked": unit_answers.sentence("Qty", "", "", "kg")}]  # no application: task only
        posted = []
        with mock.patch.object(person_record, "keep_field_unit", lambda *a, **k: posted.append((a, k))):
            self.assertEqual(unit_answers.keep(TASK), 1)
            self.assertEqual(unit_answers.keep(TASK), 0)  # once
        self.assertEqual(posted[0][0], (TASK, JUDGED, "Rate", HOST, FORM, "%", True))
        self.assertEqual(posted[0][1], {"asked": asked})


class TheControlPlanesRows(unittest.TestCase):
    def test_field_units_reads_only_rows_derived_from_a_persons_word(self):
        text = unit_answers.kind_json("Rate", HOST, FORM, "%", True)
        rows = {"learned": [
            {"kind": "field_unit", "derived_from": JUDGED, "text": text,
             "source": {"actor": {"kind": "person"}, "text": "1", "asked": "q"}},
            {"kind": "field_unit", "derived_from": JUDGED, "text": text,
             "source": {"actor": {"kind": "worker"}, "text": "1", "asked": "q"}},
            {"kind": "field_unit", "derived_from": "", "text": text,
             "source": {"actor": {"kind": "person"}, "text": "1", "asked": "q"}},
            {"kind": "field_unit", "derived_from": JUDGED, "text": json.dumps({"label": "Rate", "host": HOST,
                                                                              "unit": "%", "takes": "yes"}),
             "source": {"actor": {"kind": "person"}, "text": "1", "asked": "q"}}]}
        with mock.patch.object(person_record, "_get", lambda *a, **k: rows):
            found = person_record.field_units()
        self.assertEqual(len(found), 1)
        self.assertEqual((found[0]["label"], found[0]["form"], found[0]["takes"]), ("Rate", FORM, True))
        with mock.patch.object(person_record, "_get", lambda *a, **k: None):
            self.assertEqual(person_record.field_units(), [])  # unread: nothing


if __name__ == "__main__":
    unittest.main()
