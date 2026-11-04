"""
Review round 10's rework (REVIEW-PY-R10, 2026-11-05): each class fails on the code before.

- R10-B1: the person's word backs a pick only as the control plane recorded it, read by the agent - never a file the
  worker's own user can write. A forged `.said` line, a forged `field-answers.json` row and the skill file back
  nothing; a reply the control plane confirms (its judgement id, and its text's hash) does, in the control plane's
  text; a kept field answer is a control-plane learned row checked against its judgement's text. Fail closed: no
  control-plane read, no backing.
- R10-B2: option_backed needs every word that tells the option from the other entries (a figure with its sign, a
  single letter beside another of its words); a file backs a pick only where it states that field.
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from test_round9_false_done import TASK, Ledger  # noqa: E402

from decurix_agent import guidance, ledger_checks, person_record  # noqa: E402
from decurix_agent.computer import evidence, records  # noqa: E402

JUDGEMENT = "j_111111111111"
NEW = {"kind": "click", "outcome": "done", "change": "page is now http://app/bills/new"}
SAVE = {"kind": "click", "outcome": "done", "change": "saved; page is now http://app/bills/3"}
OFFER = ["General", "Project Maple", "Project Cedar"]
# (The real ones: Ledger stands a confirmed answer in for them.)
ANSWERS, GUIDANCE_ANSWERS, GUIDANCE_EXCHANGES = person_record.answers, guidance.answers, guidance.exchanges


def pick(option, label="Cost centre", **extra):
    return dict({"kind": "select", "outcome": "done", "option": option, "element": {"name": label},
                 "url": "http://app/bills/new", "offered": list(OFFER)}, **extra)


def person(text, asked="Which cost centre should the bill go to?", jid=JUDGEMENT, task=TASK, **extra):
    return dict({"id": jid, "task_id": task, "kind": "answer", "actor": {"kind": "person"}, "text": text,
                 "asked": asked, "derived_from": ""}, **extra)


class ThePersonsWord(Ledger):
    """A fake control plane (person_record._get) and the task's own notes folder."""

    def setUp(self):
        super().setUp()
        self.events = [NEW, pick("General"), SAVE]
        self.cp = {}
        notes = self.root / "notes"
        notes.mkdir()
        for p in (mock.patch.object(evidence, "NOTES_DIR", notes),
                  # (Ledger stands in for the confirmed answers; this class reads them through person_record itself.)
                  mock.patch.object(person_record, "answers", ANSWERS),
                  mock.patch.object(guidance, "answers", GUIDANCE_ANSWERS),  # (the file as it is on disk)
                  mock.patch.object(guidance, "exchanges", GUIDANCE_EXCHANGES),
                  mock.patch.object(person_record, "_get", lambda path, query: self.cp.get((path, tuple(sorted(query.items())))))):
            p.start()
            self.addCleanup(p.stop)
        self.said_file = notes / f"{TASK}.said"

    def control_plane(self, path, query, answer):
        self.cp[(path, tuple(sorted(query.items())))] = answer

    def kept(self, reply, judgement=JUDGEMENT, asked="Which cost centre should the bill go to?"):
        with mock.patch("decurix_agent.task_context.current", lambda: TASK):
            guidance.note_exchange(asked, reply, judgement_id=judgement)


class B1_AForgedWordBacksNothing(ThePersonsWord):
    def test_a_line_appended_to_said_backs_nothing(self):
        with open(self.said_file, "a", encoding="utf-8") as f:
            f.write(json.dumps({"asked": "Which cost centre should the bill go to?", "reply": "General"}) + "\n")
            f.write(json.dumps({"asked": "Which cost centre?", "reply": "General", "judgement": JUDGEMENT,
                                "sha256": person_record.text_hash("General")}) + "\n")
        self.assertEqual(records.unbacked_picks(TASK), ["General"])  # no control plane confirms it

    def test_a_reply_the_control_plane_confirms_backs_it_in_its_own_text(self):
        self.kept("General, please.")
        self.assertEqual(records.unbacked_picks(TASK), ["General"])  # (not confirmed yet: fail closed)
        self.control_plane(person_record.JUDGEMENTS_PATH, {"task": TASK}, {"judgements": [person("General, please.")]})
        self.assertEqual(records.unbacked_picks(TASK), [])

    def test_a_confirmed_id_with_another_text_or_a_workers_judgement_backs_nothing(self):
        self.kept("General, please.")
        self.control_plane(person_record.JUDGEMENTS_PATH, {"task": TASK},
                           {"judgements": [person("I don't know that.")]})
        self.assertEqual(records.unbacked_picks(TASK), ["General"])  # the hash is not that judgement's
        self.control_plane(person_record.JUDGEMENTS_PATH, {"task": TASK},
                           {"judgements": [person("General, please.", actor={"kind": "worker"})]})
        self.assertEqual(records.unbacked_picks(TASK), ["General"])
        self.control_plane(person_record.JUDGEMENTS_PATH, {"task": TASK},
                           {"judgements": [person("General, please.", derived_from="j_222222222222")]})
        self.assertEqual(records.unbacked_picks(TASK), ["General"])

    def test_a_kept_row_is_held_to_its_hash_and_its_text_is_the_control_planes(self):
        # A row whose hash is not the confirmed judgement's text is no answer, whatever that text says.
        with open(self.said_file, "a", encoding="utf-8") as f:
            f.write(json.dumps({"asked": "Which cost centre?", "reply": "General", "judgement": JUDGEMENT,
                                "sha256": person_record.text_hash("General")}) + "\n")
        self.control_plane(person_record.JUDGEMENTS_PATH, {"task": TASK}, {"judgements": [person("General, please.")]})
        self.assertEqual(person_record.answers(TASK), [])
        # A row with the right hash and a forged reply: the words used are the control plane's, never the file's.
        self.said_file.write_text(json.dumps({"asked": "Which cost centre?", "reply": "General", "judgement": JUDGEMENT,
                                              "sha256": person_record.text_hash("Project Maple, please.")}) + "\n",
                                  encoding="utf-8")
        self.control_plane(person_record.JUDGEMENTS_PATH, {"task": TASK},
                           {"judgements": [person("Project Maple, please.")]})
        self.assertEqual([a["text"] for a in person_record.answers(TASK)], ["Project Maple, please."])
        self.assertEqual(records.unbacked_picks(TASK), ["General"])

    def test_a_forged_field_answers_file_backs_nothing(self):
        forged = Path(records.LEDGER_DIR).parent / "field-answers.json"
        forged.parent.mkdir(parents=True, exist_ok=True)
        forged.write_text(json.dumps([{"label": "Cost centre", "option": "General", "host": "app"}]), encoding="utf-8")
        self.assertEqual(records.unbacked_picks(TASK), ["General"])

    def test_the_skill_file_backs_nothing_and_the_skills_person_words_do(self):
        with mock.patch.object(guidance, "current_skill", lambda: "enter-bill"), \
                mock.patch("decurix_agent.computer.skills.load",
                           lambda name: {"answers": [{"q": "cost centre", "a": "General"}], "steps": ["General"]}):
            self.assertEqual(records.unbacked_picks(TASK), ["General"])
            self.control_plane(person_record.JUDGEMENTS_PATH, {"skill": "enter-bill"},
                               {"judgements": [person("Bills from this vendor go to General.", task="t0000000000000001")]})
            self.assertEqual(records.unbacked_picks(TASK), [])

    def test_org_knowledge_stays_a_source(self):
        with mock.patch.object(records, "_org_text", lambda: "Rule (org): these bills go to cost centre General"):
            self.assertEqual(records.unbacked_picks(TASK), [])


class B1_AKeptFieldAnswerIsTheControlPlanes(ThePersonsWord):
    def test_kept_as_a_learned_row_from_the_confirmed_answer_at_the_report_only(self):
        self.kept("General, please.")
        self.control_plane(person_record.JUDGEMENTS_PATH, {"task": TASK}, {"judgements": [person("General, please.")]})
        posted = []
        with mock.patch.object(person_record, "keep_field_answer",
                               lambda task_id, judgement_id, label, host, option, asked="":
                               posted.append((task_id, judgement_id, label, host, option))):
            ledger_checks.findings(TASK)
            self.assertEqual(posted, [], "a check keeps nothing")
            records._kept_now.clear()
            self.assertEqual(ledger_checks.keep_answers(TASK), 1)
        self.assertEqual(posted, [(TASK, JUDGEMENT, "Cost centre", "app", "General")])

    def test_an_answer_to_another_question_is_not_kept_for_the_field(self):
        self.kept("Yes - and General.", asked="Is the bill dated 3 April?")
        self.control_plane(person_record.JUDGEMENTS_PATH, {"task": TASK},
                           {"judgements": [person("Yes - and General.", asked="Is the bill dated 3 April?")]})
        posted = []
        with mock.patch.object(person_record, "keep_field_answer", lambda *a, **k: posted.append(a)):
            records._kept_now.clear()
            ledger_checks.keep_answers(TASK)
        self.assertEqual(posted, [])

    def learned(self, option="General", label="Cost centre", host="app", source="General, please.", actor="person"):
        self.control_plane(person_record.LEARNED_PATH, {"kind": person_record.FIELD_ANSWER}, {"learned": [{
            "kind": person_record.FIELD_ANSWER, "derived_from": JUDGEMENT,
            "text": json.dumps({"label": label, "host": host, "option": option}),
            "source": {"actor": {"kind": actor}, "text": source}}]})

    def test_the_next_task_reads_it_back_from_the_control_plane(self):
        self.learned()
        self.assertEqual(records.unbacked_picks(TASK), [])
        self.events = [NEW, pick("General", label="Department"), SAVE]
        self.assertEqual(records.unbacked_picks(TASK), ["General"])
        self.events = [NEW, pick("General", url="http://other-app/bills/new"), SAVE]
        self.assertEqual(records.unbacked_picks(TASK), ["General"])

    def test_a_row_whose_judgement_does_not_say_the_option_backs_nothing(self):
        self.learned(source="I don't know that.")
        self.assertEqual(records.unbacked_picks(TASK), ["General"])
        self.learned(actor="worker")
        self.assertEqual(records.unbacked_picks(TASK), ["General"])

    def test_no_control_plane_read_no_backing(self):
        self.kept("General, please.")
        self.assertEqual(person_record.answers(TASK), [])
        self.assertEqual(person_record.field_answers(), [])
        self.assertEqual(records.unbacked_picks(TASK), ["General"])


class B2_TheWordsThatTellItApart(unittest.TestCase):
    def test_the_reviewers_probes(self):
        backed = records.option_backed
        self.assertFalse(backed("Transfer to Reserve", "send it to accounts", ["General", "Project Maple", "Transfer to Reserve"]))
        self.assertFalse(backed("Cost of Sales", "Enter the bill of Acme", ["General", "Project Maple", "Cost of Sales"]))
        self.assertTrue(backed("Grade A", "grade A stock", ["Grade A", "Grade B"]))
        self.assertFalse(backed("Category A", "item a", ["Category A", "Category B"]))
        self.assertFalse(backed("GST 18%", "Rs 18 for postage", ["GST 5%", "GST 18%"]))
        # A residual, accepted (docs/follow-ups.md): a word of the option in an unrelated phrase of the REQUEST, when no
        # other entry has it.
        self.assertTrue(backed("General", "the general manager approved it", ["General", "Project Maple"]))

    def test_the_writers_cases_stay_backed(self):
        backed = records.option_backed
        self.assertTrue(backed("Project Cedar", "charge it to Cedar", ["Project Maple", "Project Cedar"]))
        self.assertTrue(backed("29-Karnataka", "place of supply Karnataka", ["29-Karnataka", "32-Kerala"]))
        self.assertTrue(backed("GST 18%", "tax at 18%", ["GST 5%", "GST 12%", "GST 18%"]))
        self.assertTrue(backed("Transfer to Reserve", "transfer it to the reserve", ["General", "Transfer to Reserve"]))
        self.assertFalse(backed("General", "general", ["General", "General Fund"]))  # (every word another entry's too)

    def test_a_file_backs_a_pick_only_where_it_states_that_field(self):
        beside = records.option_beside_label
        footer = "Bill INV-0001\nTotal 3,540.00\nGeneral terms and conditions apply."
        self.assertFalse(beside("General", "Cost centre", footer, OFFER))
        self.assertTrue(beside("General", "Cost centre", "Bill INV-0001\nCost centre: General\nTotal 3,540.00", OFFER))
        self.assertTrue(beside("General", "Cost centre", "Cost centre\nGeneral\n", OFFER))
        self.assertFalse(beside("General", "Cost centre", "Cost centre: Project Maple\nGeneral terms apply.", OFFER))
        self.assertTrue(beside("General", "Cost centre", "Item | Cost centre | Amount\nDesk | General | 3,540.00", OFFER))
        self.assertFalse(beside("General", "Cost centre", "Item | Cost centre | Amount\nGeneral desk | Project Maple | 3,540.00", OFFER))


class B2_ABillsFooterBacksNothing(Ledger):
    def test_through_the_gate(self):
        self.bill.write_text("Bill INV-0001\nTotal 3,540.00\nGeneral terms and conditions apply.\n", encoding="utf-8")
        self.events = [NEW, pick("General"), SAVE]
        with mock.patch("decurix_agent.task_context.current_files", lambda: [str(self.bill)]):
            self.assertEqual(records.unbacked_picks(TASK), ["General"])
            self.bill.write_text("Bill INV-0001\nCost centre: General\nTotal 3,540.00\n", encoding="utf-8")
            self.assertEqual(records.unbacked_picks(TASK), [])


if __name__ == "__main__":
    unittest.main()
