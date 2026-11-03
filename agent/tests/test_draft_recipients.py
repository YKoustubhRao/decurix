"""
Recipients come from the records, structurally (round 5 P1, 2026-10-20, Findings BD and BE): the model typed
real-looking addresses (accounts@coromandelclinics.in) after a correct attempt, its recipients log listed addresses
the drafts did not go to, and a re-check's drafts sat beside the first attempt's.

    PYTHONPATH="agent;agent/tests" python -m unittest test_draft_recipients
"""

import io
import json
import unittest
from contextlib import redirect_stdout
from unittest import mock

import openpyxl

from temp_case import TempDirCase

from decurix_agent import payees, task_context
from decurix_agent.tools import office, outbox

# The live sales register's customer rows (decurix-tests\wide\process\data\P1), as the stage read them.
CUSTOMERS = [("Coromandel Clinics Pvt Ltd", "Mr. Anand", "accounts@coromandelclin.example.in"),
             ("Konkan Resorts Pvt Ltd", "Mr. Anand", "accounts@konkanreso.example.in"),
             ("Malabar Spices Exports", "Ms. Kavita", "accounts@malabarspic.example.in"),
             ("Sharma Office Mart", "Mr. Anand", "accounts@sharmaoffi.example.in"),
             ("Yamuna Schools Trust", "Mr. Suresh", "accounts@yamunascho.example.in"),
             ("Yamuna Schools Trust (Noida)", "Ms. Rao", "noida@yamunascho.example.in")]
INVENTED = ["accounts@coromandelclinics.in", "finance@konkanresorts.com", "payables@malabarspices.com",
            "accounts@sharmaofficemart.in", "accounts@yamunaschools.org"]


def run(argv, stdin=None):
    out = io.StringIO()
    with redirect_stdout(out), mock.patch("sys.stdin", io.StringIO(stdin or "")):
        code = office.main(list(argv))
    return code, out.getvalue()


class Fixture(TempDirCase):

    def setUp(self):
        super().setUp()
        self.book = self.dir / "customer_master.xlsx"
        wb = openpyxl.Workbook()
        wb.active.append(["Customer", "Contact", "Email"])
        for row in CUSTOMERS:
            wb.active.append(list(row))
        wb.save(self.book)
        self.posted = []

        def urlopen(request, timeout=None):
            if not getattr(request, "full_url", "").endswith("/api/outbox"):
                return mock.MagicMock(__enter__=lambda s: io.BytesIO(b"{}"), __exit__=lambda *a: False)
            body = json.loads(request.data)
            self.posted.append(body)
            answer = json.dumps({"id": f"mail_{len(self.posted)}", "replaced": body.get("replaces") or ""}).encode()
            return mock.MagicMock(__enter__=lambda s: io.BytesIO(answer), __exit__=lambda *a: False)
        for patch in (mock.patch.object(outbox.urllib.request, "urlopen", urlopen),
                      mock.patch.object(outbox, "DRAFTS_DIR", self.dir / "drafts"),
                      mock.patch.object(outbox.progress, "emit"),
                      mock.patch.object(task_context, "CURRENT_FILE", str(self.dir / "current-task")),
                      mock.patch.object(task_context.state, "current_task_id", "t0123456789abcdef"),
                      mock.patch.object(task_context.state, "current_task", f"Reminders, see {self.book}"),
                      mock.patch.object(payees, "_default_roots", lambda: [self.dir])):
            patch.start()
            self.addCleanup(patch.stop)


class ThePartysAddressFromTheRecords(Fixture):

    def test_by_name_exactly_in_any_case_and_punctuation(self):
        self.assertEqual(payees.party_address("Coromandel Clinics Pvt Ltd", self.book), "accounts@coromandelclin.example.in")
        self.assertEqual(payees.party_address("coromandel clinics pvt. ltd.", self.book), "accounts@coromandelclin.example.in")
        self.assertEqual(payees.party_address("Konkan Resorts", self.book), "accounts@konkanreso.example.in")  # part of one name

    def test_ambiguous_and_missing_are_refused_loudly(self):
        with self.assertRaises(payees.PartyError) as caught:
            payees.party_address("Yamuna Schools", self.book)
        # A name only like two rows' names a party of neither: both are named, neither address is taken (F-03).
        self.assertIn("no row named exactly", str(caught.exception))
        self.assertIn("Yamuna Schools Trust (Noida)", str(caught.exception))
        # The exact name picks its own row, though another row's name contains it.
        self.assertEqual(payees.party_address("Yamuna Schools Trust", self.book), "accounts@yamunascho.example.in")
        with self.assertRaises(payees.PartyError) as caught:
            payees.party_address("Indus Coworking Spaces", self.book)
        self.assertIn("has no row for \"Indus Coworking Spaces\"", str(caught.exception))

    def test_office_email_takes_the_party(self):
        code, text = run(["email", "--to-party", "Sharma Office Mart", "--records", str(self.book), "--subject",
                          "Reminder: overdue invoices - Sharma Office Mart"], stdin="Dear Mr. Anand,\n")
        self.assertEqual(code, 0, text)
        self.assertEqual(self.posted[-1]["to"], ["accounts@sharmaoffi.example.in"])
        code, text = run(["email", "--to-party", "Nobody Ltd", "--records", str(self.book), "--subject", "x"], stdin="b")
        self.assertEqual(code, 1)
        self.assertIn("not drafted", text)

    def test_a_typed_invented_address_is_refused_at_creation(self):
        code, text = run(["email", "--to", INVENTED[0], "--subject", "Reminder: overdue invoices - Coromandel Clinics Pvt Ltd"],
                         stdin="Dear Mr. Anand,\n")
        self.assertEqual(code, 1, text)
        self.assertIn("accounts@coromandelclinics.in, which is in no file you gave me", text)
        self.assertIn("customer_master.xlsx has accounts@coromandelclin.example.in for Coromandel Clinics Pvt Ltd", text)
        self.assertIn("--to-party", text)
        self.assertEqual(self.posted, [])
        # The address on record, typed, is taken.
        code, text = run(["email", "--to", "accounts@coromandelclin.example.in", "--subject", "Reminder"], stdin="Dear team,")
        self.assertEqual(code, 0, text)


class TheLogAndTheReplacement(Fixture):

    def test_the_drafts_list_is_what_was_drafted(self):
        for name in ("Coromandel Clinics Pvt Ltd", "Konkan Resorts Pvt Ltd"):
            run(["email", "--to-party", name, "--records", str(self.book), "--subject", f"Reminder - {name}"], stdin="b")
        code, text = run(["drafts"])
        self.assertEqual(code, 0, text)
        listed = json.loads(text)
        self.assertEqual([d["to"] for d in listed], [["accounts@coromandelclin.example.in"], ["accounts@konkanreso.example.in"]])

    def test_two_attempts_of_one_stage_leave_one_draft_per_recipient(self):
        # The stage mark another process reads (office email runs outside the agent): attempt 1, then attempt 2.
        for attempt, subject in ((1, "Reminder: overdue invoices as at 31-Oct-2026"),
                                 (2, "Reminder: overdue invoices as of 31-Oct-2026 - {}")):
            task_context.set_stage("run1:s2-reminders", attempt)
            for name, _contact, _mail in CUSTOMERS[:5]:
                code, text = run(["email", "--to-party", name, "--records", str(self.book), "--subject",
                                  subject.format(name)], stdin="Dear team,")
                self.assertEqual(code, 0, text)
        self.assertEqual(len(outbox.drafted()), 5)
        self.assertEqual([bool(p["replaces"]) for p in self.posted], [False] * 5 + [True] * 5)
        task_context.set_stage("")
        self.assertEqual(task_context.current_stage(), ("", 0))


class AddressesInAFile(Fixture):

    def test_a_recipients_log_with_invented_addresses_is_not_done(self):
        log_file = self.dir / "outputs" / "overdue_reminder_recipients.csv"
        log_file.parent.mkdir()
        log_file.write_text("Customer,Email\n" + "".join(f"{c[0]},{m}\n" for c, m in zip(CUSTOMERS, INVENTED)),
                            encoding="utf-8")
        found = payees.unknown_addresses_in_files("", [str(log_file)],
                                                  known=payees.known_addresses("", found=([str(self.book)], []),
                                                                               documents=[]))
        self.assertEqual(len(found), 1)
        self.assertIn("overdue_reminder_recipients.csv lists accounts@coromandelclinics.in (customer_master.xlsx has "
                      "accounts@coromandelclin.example.in for Coromandel Clinics Pvt Ltd)", found[0])
        log_file.write_text("Customer,Email\n" + "".join(f"{c[0]},{c[2]}\n" for c in CUSTOMERS), encoding="utf-8")
        self.assertEqual(payees.unknown_addresses_in_files("", [str(log_file)],
                                                           known=payees.known_addresses("", found=([str(self.book)], []),
                                                                                        documents=[])), [])


if __name__ == "__main__":
    unittest.main()
