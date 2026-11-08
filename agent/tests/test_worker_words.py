"""
The person's words on a worker's earlier tasks back a value in a later one (found live 2026-11-08), and a declaration
keyed by a saved record's own number is that record (the same run's second-record finding).

- A rule a person taught on an earlier task ("region 07 goes to FND-3") backs the value the worker derives from it on a
  later task: the value stands in the rule's words, read through the control plane's worker-scoped judgement read
  (person_record.worker_words). Not another worker's words, not the worker's own file, not a worker's or a derived row,
  and nothing when the control plane cannot be read. The eval's scripted expert counts as the person.
- A bill saved under the key "bill" and declared again under its own bill number, to add the field its save left
  undeclared, was taken as a second record: the ledger held two for the application's one.
"""

import urllib.error
from unittest import mock

import config_stub  # noqa: F401

from test_round9_false_done import TASK, Ledger  # noqa: E402

from decurix_agent import person_record  # noqa: E402
from decurix_agent.computer import records  # noqa: E402
from decurix_agent.computer.errors import ComputerError  # noqa: E402

# (The real reads, before any test stands a fake control plane in for them.)
REAL_GET, ANSWERS = person_record._get, person_record.answers
EARLIER = "t00000000000000e1"
RULE = "From now on every invoice from a supplier in region 07 goes to fund FND-3, region 12 to FND-8."
OFFER = ["Choose...", "FND-3", "FND-8", "FND-9"]
WORKER_QUERY = (("kind", "answer"), ("limit", str(person_record.WORKER_WORDS_LIMIT)), ("scope", "worker"))
NEW = {"kind": "click", "outcome": "done", "change": "page is now http://app/invoices/new"}
SAVE = {"kind": "click", "outcome": "done", "change": "saved; page is now http://app/invoices/4?saved=1"}


def pick(option, label="Fund"):
    return {"kind": "select", "outcome": "done", "option": option, "element": {"name": label},
            "url": "http://app/invoices/new", "offered": list(OFFER)}


def said(text, kind="person", task=EARLIER, jid="j_aaaaaaaaaaaa", **extra):
    return dict({"id": jid, "task_id": task, "kind": "answer", "actor": {"kind": kind, "roles": []}, "text": text,
                 "asked": "Which fund does this invoice go to?", "derived_from": "", "secret": False}, **extra)


class WorkerWords(Ledger):
    """A fake control plane that answers each desktop (`caller`) only its own worker's words, as the route binds it."""

    def setUp(self):
        super().setUp()
        self.caller, self.cp, self.reads = "box", {}, []
        self.events = [NEW, pick("FND-3"), SAVE]

        def get(path, query, fresh=False):
            self.reads.append((path, dict(query), fresh))
            return self.cp.get((self.caller, path, tuple(sorted(query.items()))))
        for p in (mock.patch.object(person_record, "_get", get),):
            p.start()
            self.addCleanup(p.stop)
        person_record.clear_cache()

    def words(self, caller, rows):
        self.cp[(caller, person_record.JUDGEMENTS_PATH, WORKER_QUERY)] = {"judgements": rows}


class TaughtOnAnEarlierTask(WorkerWords):
    def test_a_value_in_the_persons_answer_on_an_earlier_task_is_backed(self):
        self.assertEqual(records.unbacked_picks(TASK), ["FND-3"])  # nothing read yet: fail closed
        self.words("box", [said(RULE)])
        person_record.clear_cache()
        self.assertEqual(records.unbacked_picks(TASK), [])
        self.assertIn(RULE, records.given_sources(TASK)["free"])
        # The read is the worker-scoped one, never another task's by id.
        self.assertTrue(any(q.get("scope") == "worker" for _, q, _ in self.reads))
        self.assertFalse(any(q.get("task") == EARLIER for _, q, _ in self.reads))

    def test_a_declaration_takes_the_value_the_rule_gives(self):
        self.words("box", [said(RULE)])
        self.events = [pick("FND-3")]
        rec, _ = records.declare("INV-0001", str(self.bill), {"Invoice no.": "INV-0001", "Fund": "FND-3"})
        self.assertEqual(rec["fields"]["Fund"]["from"], "chosen")

    def test_a_value_appearing_nowhere_in_the_words_is_still_not_backed(self):
        self.words("box", [said(RULE)])
        self.events = [NEW, pick("FND-9"), SAVE]
        self.assertEqual(records.unbacked_picks(TASK), ["FND-9"])

    def test_the_scripted_expert_stands_for_the_person(self):
        self.words("box", [said(RULE, kind="scripted")])
        self.assertEqual(records.unbacked_picks(TASK), [])

    def test_a_workers_or_a_derived_or_a_secret_row_backs_nothing(self):
        for row in (said(RULE, kind="worker"), said(RULE, kind="program"), said(RULE, derived_from="j_bbbbbbbbbbbb"),
                    said(RULE, secret=True)):
            self.words("box", [row])
            person_record.clear_cache()
            self.assertEqual(records.unbacked_picks(TASK), ["FND-3"], row)

    def test_another_workers_words_back_nothing(self):
        self.words("eve", [said(RULE, task="t00000000000000f9")])
        self.assertEqual(records.unbacked_picks(TASK), ["FND-3"])
        self.caller = "eve"  # (the same words do back eve's own pick)
        person_record.clear_cache()
        self.assertEqual(records.unbacked_picks(TASK), [])

    def test_the_workers_own_file_backs_nothing(self):
        rule_file = self.root / "work" / "fund-rule.txt"
        rule_file.parent.mkdir()
        rule_file.write_text(RULE, encoding="utf-8")
        with mock.patch("decurix_agent.computer.evidence.read",
                        lambda task_id=None: f"read from the file {rule_file}: {RULE}"):
            self.assertEqual(records.unbacked_picks(TASK), ["FND-3"])

    def test_a_new_task_reads_the_words_again(self):
        self.assertEqual(records.unbacked_picks(TASK), ["FND-3"])
        self.words("box", [said(RULE)])
        # (Within one task the read is reused; the next task reads afresh and sees what the earlier one was told.)
        person_record.worker_words("t00000000000000a2")
        self.assertEqual(self.reads[-1][2], True)
        self.assertIn(RULE, person_record.worker_words("t00000000000000a2"))


class ATypedValueThePersonGave(WorkerWords):
    """Found live (2026-11-08): a value the person's reply gave, typed into the form rather than picked from a list,
    stood in nothing a declaration read, so the worker wrote the reply into a file of its own to declare from."""

    def test_it_is_declared_from_the_document_and_backed_by_their_words(self):
        self.words("box", [said(RULE)])
        self.events = []
        rec, lines = records.declare("INV-0001", str(self.bill), {"Invoice no.": "INV-0001", "Fund": "FND-3"})
        self.assertEqual(rec["fields"]["Fund"]["from"], "seen")
        # The ledger says which given place backs it (found live: three reviews held it as cited from the document).
        self.assertEqual(rec["fields"]["Fund"]["backed_by"], "the person's words")
        self.assertIn("Fund (the person's words)", "\n".join(lines))
        listing = "\n".join(records.listing(records.task_context.current()))
        self.assertIn("    Fund: FND-3 [seen; backed by the person's words; read back: -]", listing)
        self.assertNotIn("backed by", listing.split("Fund:")[0])   # a field from the document names no other place

    def test_a_picked_entry_their_rule_gives_says_so_too(self):
        self.words("box", [said(RULE)])
        self.events = [pick("FND-3")]
        rec, _ = records.declare("INV-0001", str(self.bill), {"Invoice no.": "INV-0001", "Fund": "FND-3"})
        self.assertEqual((rec["fields"]["Fund"]["from"], rec["fields"]["Fund"].get("backed_by")),
                         ("chosen", "the person's words"))

    def test_without_their_words_it_stands_in_nothing(self):
        self.events = []
        with self.assertRaises(ComputerError) as caught:
            records.declare("INV-0001", str(self.bill), {"Invoice no.": "INV-0001", "Fund": "FND-3"})
        self.assertIn("Fund: FND-3", str(caught.exception))

    def test_a_reply_named_as_the_source_says_how_to_declare(self):
        self.words("box", [said(RULE)])
        with self.assertRaises(ComputerError) as caught:
            records.declare("INV-0001", "human reply", {"Fund": "FND-3"})
        self.assertIn("needs no file", str(caught.exception))
        self.assertIn("backs nothing", str(caught.exception))


class FailClosed(Ledger):
    def test_an_unreadable_control_plane_backs_nothing(self):
        self.events = [NEW, pick("FND-3"), SAVE]
        person_record.clear_cache()
        with mock.patch.object(person_record, "_get", person_record._get.__wrapped__
                               if hasattr(person_record._get, "__wrapped__") else REAL_GET), \
                mock.patch("urllib.request.urlopen", side_effect=urllib.error.URLError("refused")):
            self.assertEqual(person_record.worker_words(TASK), [])
            self.assertEqual(records.unbacked_picks(TASK), ["FND-3"])
        person_record.clear_cache()



class ASavedRecordDeclaredUnderItsNumber(Ledger):
    """The run's second-record finding: a field added to a saved record under its own number joins that record."""

    def save(self, rec, page):
        self.looks = [page]
        return records.readback(rec, "Invoice INV-0001", page, "saved; page is now http://app/invoices/4?saved=1")

    def test_a_declaration_keyed_by_the_saved_records_number_joins_it(self):
        rec, _ = records.declare("invoice", str(self.bill), {"Invoice no.": "INV-0001", "Total": "3,540.00"})
        self.save(rec, "Invoice INV-0001 Total 3,540.00 Notes Paid by cheque 4471")
        ledger = records.load(TASK)
        ledger[0]["undeclared"] = ["Notes=Paid by cheque 4471"]
        records._save(TASK, ledger)
        note = self.root / "note.txt"
        note.write_text("Notes: Paid by cheque 4471\n", encoding="utf-8")

        rec, lines = records.declare("INV-0001", str(note), {"Notes": "Paid by cheque 4471"})
        ledger = records.load(TASK)
        self.assertEqual(len(ledger), 1, ledger)  # one record in the application, one in the ledger
        self.assertEqual(rec["key"], "invoice")
        self.assertEqual(ledger[0]["fields"]["Notes"]["source"], str(note))
        self.assertEqual(ledger[0]["undeclared"], [])
        self.assertIn("Notes", ledger[0]["readback"]["seen"])
        self.assertIn("is saved already", lines[0])
        self.assertIn(str(note), records.declared_sources(TASK))
        self.assertEqual(records.gate_faults(TASK, created=1), [])

    def test_a_field_the_last_page_did_not_show_says_how_to_read_it_back(self):
        # Found live (2026-11-08, learn-v1 bill 2): told only "declare those again", the worker tried a page's title and
        # URL as --from and then misdescribed its own declaration; four reviews held it until the case timed out.
        rec, _ = records.declare("invoice", str(self.bill), {"Invoice no.": "INV-0001", "Total": "3,540.00"})
        self.save(rec, "Invoice INV-0001 Total 3,540.00")
        note = self.root / "note.txt"
        note.write_text("Notes: Paid by cheque 4471\n", encoding="utf-8")
        self.looks = ["Invoices list INV-0001"]
        _, lines = records.declare("INV-0001", str(note), {"Notes": "Paid by cheque 4471"})
        hint = "\n".join(lines)
        self.assertIn("NOT shown on the page the task looked at last: Notes", hint)
        self.assertIn("(gui observe), then run the same declaration again - the same --from, the record's document", hint)
        with self.assertRaises(ComputerError) as caught:
            records.declare("INV-0001", "http://app/invoices/4", {"Notes": "Paid by cheque 4471"})
        self.assertIn("never a page's title or address", str(caught.exception))

    def test_the_same_fields_again_under_the_number_are_refused_not_a_second_record(self):
        rec, _ = records.declare("invoice", str(self.bill), {"Invoice no.": "INV-0001", "Total": "3,540.00"})
        self.save(rec, "Invoice INV-0001 Total 3,540.00")
        with self.assertRaises(ComputerError) as caught:
            records.declare("INV-0001", str(self.bill), {"Invoice no.": "INV-0001", "Total": "3,540.00"})
        self.assertIn("saved in this task already", str(caught.exception))
        self.assertEqual(len(records.load(TASK)), 1)

    def test_another_number_or_a_key_without_a_digit_is_another_record(self):
        rec, _ = records.declare("invoice", str(self.bill), {"Invoice no.": "INV-0001", "Total": "3,540.00"})
        self.save(rec, "Invoice INV-0001 Total 3,540.00")
        self.assertIsNone(records.saved_by_number(records.load(TASK), "INV-0002"))
        self.assertIsNone(records.saved_by_number(records.load(TASK), "Example Vendor Ltd"))
        self.assertEqual(records.saved_by_number(records.load(TASK), "INV-0001")["key"], "invoice")
        # A field that is no number (the total) does not name the record.
        self.assertIsNone(records.saved_by_number(records.load(TASK), "3,540.00"))


class ACodeWrittenWhole(Ledger):
    """Codes that share their letters differ only by their figure: written whole, the code picks out its entry."""

    def test_a_code_written_whole_picks_its_entry_and_nothing_else(self):
        backed, offer = records.option_backed, ["Choose...", "FND-3", "FND-8", "FND-38"]
        self.assertTrue(backed("FND-3", RULE, offer))
        self.assertTrue(backed("FND-8", RULE, offer))  # (the rule names it too: which one applies is the check's to judge)
        self.assertFalse(backed("FND-38", RULE, offer))
        self.assertFalse(backed("FND-3", "region 07, fund 3", offer))  # the figure alone is no code
        self.assertFalse(backed("FND-3", "fund FND-38 for these", offer))  # glued: another entry
        self.assertFalse(backed("FND-3", "fund FND-3 Annex", offer + ["FND-3 Annex"]))  # a longer entry written there

    def test_the_scripted_experts_reply_on_this_task_is_the_persons_too(self):
        cp = {}
        with mock.patch.object(person_record, "answers", ANSWERS), \
                mock.patch.object(person_record, "_get", lambda path, query, fresh=False: cp.get(tuple(sorted(query.items())))), \
                mock.patch("decurix_agent.guidance.kept_replies", lambda task_id=None: [
                    {"asked": "Which fund?", "reply": "FND-3", "judgement": "j_cccccccccccc",
                     "sha256": person_record.text_hash("FND-3")}]):
            person_record.clear_cache()
            self.assertEqual(person_record.answers(TASK), [])
            cp[(("task", TASK),)] = {"judgements": [said("FND-3", kind="scripted", task=TASK, jid="j_cccccccccccc")]}
            self.assertEqual([a["text"] for a in person_record.answers(TASK)], ["FND-3"])
