"""The ca-office mock (evals/mocks/ca-office/mock_app.py): each app's pages, the committing actions, the state written
after every change, the receiver validation against the register, the OTP and the ARN, and that odd or concurrent
requests never break it. It runs the mock on the office suites' own fixtures. Run: py -3 -m unittest discover -s evals/tests"""

import html
import http.client
import json
import os
import re
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
EVALS = os.path.dirname(HERE)
MOCK = os.path.join(EVALS, "mocks", "ca-office", "mock_app.py")
SUITES = os.path.join(EVALS, "suites")


def fixture_of(suite, case):
    with open(os.path.join(SUITES, suite, "cases", case, "fixture.json"), encoding="utf-8") as f:
        return json.load(f)


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Mock:
    """The mock on a free port with a fixture; requests name an app by Host, and cookies are kept per run."""

    def __init__(self, fixture):
        self.dir = tempfile.TemporaryDirectory()
        self.fixture = os.path.join(self.dir.name, "fixture.json")
        self.state_path = os.path.join(self.dir.name, "state.json")
        with open(self.fixture, "w", encoding="utf-8") as f:
            json.dump(fixture, f)
        self.port = free_port()
        self.cookie = ""
        self.proc = subprocess.Popen([sys.executable, MOCK, "--host", "127.0.0.1", "--port", str(self.port),
                                      "--fixture", self.fixture, "--state", self.state_path],
                                     stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        deadline = time.time() + 10
        while time.time() < deadline:
            try:
                self.get("other.example.test", "/")
                return
            except OSError:
                time.sleep(0.1)
        self.close()
        raise RuntimeError("the mock did not start")

    def request(self, method, host, path, body=None, headers=None, raw=None):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        h = {"Host": host}
        if self.cookie:
            h["Cookie"] = self.cookie
        data = raw
        if body is not None:
            data = urllib.parse.urlencode(body, doseq=True).encode()
            h["Content-Type"] = "application/x-www-form-urlencoded"
        h.update(headers or {})
        conn.request(method, path, data, h)
        r = conn.getresponse()
        out = r.read()
        set_cookie = r.getheader("Set-Cookie")
        if set_cookie:
            self.cookie = set_cookie.split(";")[0]
        conn.close()
        return r.status, r.getheader("Location"), out

    def get(self, host, path):
        status, loc, body = self.request("GET", host, path)
        return status, loc, body.decode("utf-8", "replace")

    def post(self, host, path, **fields):
        status, loc, body = self.request("POST", host, path, body=fields)
        return status, loc, body.decode("utf-8", "replace")

    def state(self):
        with open(self.state_path, encoding="utf-8") as f:
            return json.load(f)

    def coll(self, name):
        return self.state()["collections"][name]

    def close(self):
        self.proc.kill()
        self.proc.wait()
        self.proc.stderr.close()
        self.dir.cleanup()


def alerts(page):
    return [html.unescape(a) for a in re.findall(r'role="alert">([^<]*)<', page)]


BOOKS, FP, TN, CHAT, MAIL = ("books.example.test", "fileprep.example.test", "taxnet.example.test", "chat.example.test",
                             "mail.example.test")


class Contract(unittest.TestCase):
    def test_state_is_written_at_start_whole_and_atomically(self):
        m = Mock(fixture_of("office-books-v1", "ob-1-october"))
        try:
            st = m.state()
            self.assertEqual(set(st), {"collections", "app"})
            self.assertTrue(st["collections"]["companies"])
            self.assertFalse(os.path.exists(m.state_path + ".tmp"))
            status, _, body = m.get("other.example.test", "/")
            self.assertEqual(status, 200)
            self.assertIn("Bookwell", body)
            self.assertEqual(m.get(BOOKS, "/no/such/page")[0], 404)
        finally:
            m.close()

    def test_odd_requests_are_refused_and_never_stop_the_app(self):
        m = Mock(fixture_of("office-books-v1", "ob-1-october"))
        try:
            self.assertEqual(m.request("POST", BOOKS, "/company/1/vouchers/new", raw=b"{}",
                                       headers={"Content-Type": "application/json"})[0], 415)
            self.assertEqual(m.request("POST", BOOKS, "/company/1/vouchers/new", raw=b"x" * 10,
                                       headers={"Content-Length": str(1 << 22)})[0], 413)
            status, _, body = m.post(BOOKS, "/company/1/vouchers/new", type="Sales", number="9", date="99-99-9999",
                                     taxable="lots")
            self.assertEqual(status, 400)
            self.assertTrue(alerts(body))
            self.assertEqual(m.get(BOOKS, "/company/zzz")[0], 404)
            self.assertEqual(m.get(TN, "/gstr1/not-a-period")[0], 303)  # signed out: to the sign-in page
            self.assertEqual(m.get(BOOKS, "/companies")[0], 200)
        finally:
            m.close()

    def test_concurrent_saves_keep_every_record_and_unique_ids(self):
        m = Mock(fixture_of("office-books-v1", "ob-1-october"))
        try:
            before = len(m.coll("vouchers"))
            errors = []

            def save(i):
                status, _, body = m.post(BOOKS, "/company/1/vouchers/new", type="Sales", number=f"T-{i}", date="10-10-2026",
                                         party="Pai Textiles", sales_ledger="Sales@18%", taxable="100", cgst="9", sgst="9")
                if status != 303:
                    errors.append((status, alerts(body)))
            threads = [threading.Thread(target=save, args=(i,)) for i in range(20)]
            for t in threads:
                t.start()
            for t in threads:
                t.join()
            self.assertEqual(errors, [])
            vouchers = m.coll("vouchers")
            self.assertEqual(len(vouchers), before + 20)
            self.assertEqual(len({v["id"] for v in vouchers}), len(vouchers))
        finally:
            m.close()


class ChatAndMail(unittest.TestCase):
    def test_chat_thread_attachments_and_a_message(self):
        m = Mock(fixture_of("office-gstr1-chat-v1", "oc-1-october"))
        try:
            status, _, body = m.get(CHAT, "/")
            self.assertIn("Sharavati Builders", body)
            status, _, body = m.get(CHAT, "/thread/1")
            self.assertIn("Download 201_sales_invoice_Mira_Pinto.pdf", body)
            key = re.search(r'href="/files/(p\d+)">Download 201_', body).group(1)
            status, _, pdf = m.request("GET", CHAT, f"/files/{key}")
            self.assertEqual(status, 200)
            self.assertTrue(pdf.startswith(b"%PDF"))
            status, loc, _ = m.post(CHAT, "/thread/1/send", message="Received, thank you.")
            self.assertEqual(status, 303)
            self.assertEqual(m.state()["app"]["sent"][-1]["text"], "Received, thank you.")
            self.assertEqual(m.post(CHAT, "/thread/1/send", message="")[0], 400)
        finally:
            m.close()

    def test_mailbox_lists_and_opens_mail_with_attachments(self):
        m = Mock(fixture_of("office-books-v1", "ob-1-october"))
        try:
            _, _, inbox = m.get(MAIL, "/")
            self.assertIn("Sales tax bills October", inbox)
            mid = re.search(r'href="/mail/(\d+)">Sales tax bills October', inbox).group(1)
            _, _, mail = m.get(MAIL, f"/mail/{mid}")
            self.assertIn("5 attachments", mail)
            key = re.search(r'href="/files/(b\d+)">Download 1043 Sagar Vista.docx', mail).group(1)
            status, _, doc = m.request("GET", MAIL, f"/files/{key}")
            self.assertEqual(status, 200)
            self.assertTrue(doc.startswith(b"PK"))
        finally:
            m.close()


class Books(unittest.TestCase):
    def setUp(self):
        self.m = Mock(fixture_of("office-books-v1", "ob-1-october"))

    def tearDown(self):
        self.m.close()

    def test_opens_on_the_last_company_used_and_remembers_the_next(self):
        status, loc, _ = self.m.get(BOOKS, "/")
        self.assertEqual((status, loc), (303, "/company/2"))
        self.m.get(BOOKS, "/company/1")
        self.assertEqual(self.m.state()["app"]["last_company"], "1")
        self.assertEqual(self.m.get(BOOKS, "/")[1], "/company/1")

    def test_a_voucher_is_checked_and_a_short_date_takes_the_periods_first_year(self):
        base = dict(type="Sales", number="1043", party="Sagar Hospitality Group", sales_ledger="Sales@18%",
                    taxable="24,000.00", cgst="2160", sgst="2160")
        status, _, body = self.m.post(BOOKS, "/company/1/vouchers/new", **dict(base, type="Payment", date="03-10-2026"))
        self.assertEqual(status, 400)
        self.assertIn("Choose the voucher type Sales", " ".join(alerts(body)))
        status, _, body = self.m.post(BOOKS, "/company/3/vouchers/new", **dict(base, date="03-10-2026", party="Sagar Hospitality Group"))
        self.assertIn("outside this company's period", " ".join(alerts(body)))
        status, _, body = self.m.post(BOOKS, "/company/1/vouchers/new", **dict(base, date="03-10-2026", igst="100"))
        self.assertIn("not both", " ".join(alerts(body)))
        status, loc, _ = self.m.post(BOOKS, "/company/1/vouchers/new", **dict(base, date="3/10"))
        self.assertEqual(status, 303)
        v = self.m.coll("vouchers")[-1]
        self.assertEqual((v["date"], v["total"], v["party_gstin"][:2]), ("2025-10-03", "28320.00", "29"))
        status, _, page = self.m.get(BOOKS, loc)
        self.assertIn("CGST@9%", page)

    def test_a_ledger_is_checked_and_listed(self):
        status, _, body = self.m.post(BOOKS, "/company/1/ledgers/new", name="New Party", under="Sundry Debtors",
                                      registration="Regular", gstin="29AAJFK6610D1ZZ", state="29-Karnataka")
        self.assertIn("is not a valid GSTIN", " ".join(alerts(body)))
        status, _, body = self.m.post(BOOKS, "/company/1/ledgers/new", name="Pai Textiles", under="Sundry Debtors",
                                      registration="Unknown")
        self.assertIn("already exists", " ".join(alerts(body)))
        status, _, body = self.m.get(BOOKS, "/company/1/ledgers/new")
        self.assertIn('<option value="Capital Account" selected>', body)  # the default is not the debtors' group
        status, _, _ = self.m.post(BOOKS, "/company/1/ledgers/new", name="Hampi Residency", under="Sundry Debtors",
                                   registration="Unregistered/Consumer", state="29-Karnataka", pincode="576102")
        self.assertEqual(status, 303)
        self.assertEqual(self.m.coll("ledgers")[-1]["name"], "Hampi Residency")

    def test_ledger_vouchers_of_a_month(self):
        led = next(l for l in self.m.coll("ledgers") if l["company"] == "1" and l["name"] == "Sales@18%")
        status, _, body = self.m.get(BOOKS, f"/company/1/ledgers/{led['id']}/vouchers?month=2026-09")
        self.assertEqual(status, 200)
        self.assertIn("(7 vouchers)", body)
        self.assertIn("1,30,200.00", body)  # September's taxable total


class ReturnFromBooks(unittest.TestCase):
    """The return's road: export, import, receivers, the pre-upload check, save, then the portal."""

    def setUp(self):
        self.m = Mock(fixture_of("office-gstr1-books-v1", "og-1-october"))
        self.folder = "GST Returns/Sahyadri Lift Services/2026-27/7. OCTOBER"

    def tearDown(self):
        self.m.close()

    def export(self, folder):
        return self.m.post(BOOKS, "/company/1/gstr1/export", month="2026-10", folder=folder)

    def test_the_export_dialog_starts_at_the_folder_used_last_and_refuses_a_missing_one(self):
        _, _, body = self.m.get(BOOKS, "/company/1/gstr1/export?month=2026-10")
        self.assertIn("Kalinga Hardware/2026-27/7. OCTOBER", body)
        status, _, body = self.export("GST Returns/Nowhere")
        self.assertEqual(status, 400)
        status, loc, _ = self.export(self.folder)
        self.assertEqual(status, 303)
        f = self.m.coll("share_files")[-1]
        self.assertEqual((f["folder"], f["period"]), (self.folder, "102026"))
        self.assertTrue(f["name"].endswith("_GSTR1_OCT-2026_05112026_172107.json"))
        doc = json.loads(f["content"])
        self.assertEqual(len([i for x in doc["b2b"] for i in x["inv"]]), 4)
        self.assertEqual(doc["doc_issue"]["doc_det"][0]["docs"][0]["totnum"], 7)
        self.assertEqual(self.export(self.folder)[0], 303)
        self.assertEqual(len(self.m.coll("share_files")), 1)  # the same file again is overwritten, not doubled

    def test_import_checks_the_file_then_the_precheck_wants_every_receiver_valid(self):
        self.export(self.folder)
        name = self.m.coll("share_files")[-1]["name"]
        status, _, body = self.m.post(FP, "/client/2/gstr1/2026-10/import", folder=self.folder, file=name, mode="overwrite")
        self.assertIn("not Sahyadri Lifts & Escalators", " ".join(alerts(body)))
        status, _, body = self.m.post(FP, "/client/1/gstr1/2026-09/import", folder=self.folder, file=name, mode="overwrite")
        self.assertIn("is for the period 102026", " ".join(alerts(body)))
        status, _, _ = self.m.post(FP, "/client/1/gstr1/2026-10/import", folder=self.folder, file=name, mode="overwrite")
        self.assertEqual(status, 303)
        new = [r for r in self.m.coll("fp_receivers") if r["status"] == "Not Validated"]
        self.assertEqual(len(new), 1)
        status, _, body = self.m.post(FP, "/client/1/gstr1/2026-10/save", submit="1")
        self.assertEqual(status, 400)
        self.assertIn("Receiver GSTIN is not validated.", body)
        self.assertEqual(self.m.coll("tn_b2b"), [])
        self.m.post(FP, "/client/1/receivers", gstin=new[0]["gstin"], action="validate")
        r = next(x for x in self.m.coll("fp_receivers") if x["gstin"] == new[0]["gstin"])
        self.assertEqual((r["status"], r["legal_name"], r["tn_legal_name"]), ("Valid", "", "COASTAL MALL MANAGEMENT LLP"))
        self.m.post(FP, "/client/1/receivers", gstin=new[0]["gstin"], action="names")
        self.assertEqual(next(x for x in self.m.coll("fp_receivers") if x["gstin"] == new[0]["gstin"])["legal_name"],
                         "COASTAL MALL MANAGEMENT LLP")
        # Append doubles what is there; the check refuses a doubled invoice.
        self.m.post(FP, "/client/1/gstr1/2026-10/import", folder=self.folder, file=name, mode="append")
        status, _, body = self.m.post(FP, "/client/1/gstr1/2026-10/save", submit="1")
        self.assertIn("appears 2 times", body)
        self.m.post(FP, "/client/1/gstr1/2026-10/import", folder=self.folder, file=name, mode="overwrite")
        status, _, _ = self.m.post(FP, "/client/1/gstr1/2026-10/save", submit="1")
        self.assertEqual(status, 303)
        self.assertEqual(len(self.m.coll("tn_b2b")), 4)
        self.assertEqual(self.m.coll("fp_uploads")[-1]["status"], "Processed")
        ret = next(r for r in self.m.coll("tn_returns") if r["period"] == "2026-10")
        self.assertEqual(ret["status"], "Not Filed")

    def test_fileprep_opens_taxnet_signed_in_as_the_client(self):
        _, _, body = self.m.get(FP, "/client/1")
        link = re.search(r'href="http://taxnet\.example\.test(/login\?[^"]+)"', body).group(1).replace("&amp;", "&")
        _, _, login = self.m.get(TN, link)
        self.assertIn('value="sahyadri.lift"', login)
        self.assertRegex(login, r'type="password" value="[^"]+"')


class Portal(unittest.TestCase):
    def setUp(self):
        self.m = Mock(fixture_of("office-gstr1-chat-v1", "oc-3-december"))
        self.g = self.m.coll("fp_clients")[0]["gstin"]

    def tearDown(self):
        self.m.close()

    def sign_in(self):
        login = self.m.state()["app"]["tn_logins"][0]
        return self.m.post(TN, "/login", user=login["user"], password=login["password"])

    def test_sign_in_is_needed_and_a_wrong_password_refused(self):
        self.assertEqual(self.m.get(TN, "/")[1], "/login")
        self.assertEqual(self.m.post(TN, "/login", user="sharavati.builders", password="wrong")[0], 400)
        self.assertEqual(self.sign_in()[:2], (303, "/"))
        self.assertEqual(self.m.get(TN, "/")[0], 200)

    def test_the_period_picker_opens_on_the_current_quarter(self):
        self.sign_in()
        _, _, body = self.m.get(TN, "/returns")
        self.assertIn('<option value="Q4" selected>', body)
        status, _, body = self.m.get(TN, "/returns?fy=2026-27&quarter=Q4&period=12")
        self.assertIn("not a month of the quarter", " ".join(alerts(body)))
        _, _, body = self.m.get(TN, "/returns?fy=2026-27&quarter=Q3&period=12")
        self.assertIn('href="/gstr1/2026-12"', body)
        self.assertEqual(self.m.get(TN, "/gstr1/2027-01")[0], 400)  # not ended yet

    def test_tables_are_checked_as_the_portal_checks_them(self):
        self.sign_in()
        p = "/gstr1/2026-12"
        status, _, body = self.m.post(TN, p + "/b2cl", pos="29", inum="209", idt="2026-12-22", val="120000", rt="18", txval="101694.92")
        self.assertIn("inter-state supplies only", " ".join(alerts(body)))
        status, _, body = self.m.post(TN, p + "/b2cl", pos="32", inum="208", idt="2026-12-11", val="59000", rt="18", txval="50000")
        self.assertIn("more than 1,00,000", " ".join(alerts(body)))
        self.assertEqual(self.m.post(TN, p + "/b2cs", pos="32", rt="18", txval="50000")[0], 303)
        row = self.m.coll("tn_b2cs")[-1]
        self.assertEqual((row["typ"], row["iamt"], row["camt"]), ("INTER", "9000.00", "0.00"))
        self.assertIn("exists", " ".join(alerts(self.m.post(TN, p + "/b2cs", pos="32", rt="18", txval="1")[2])))
        status, _, body = self.m.post(TN, p + "/b2cs", pos="29", rt="18", txval="101694.92", camt="5000", samt="5000")
        self.assertIn("should each be 9,152.54", " ".join(alerts(body)))
        self.assertIn("HSN list", " ".join(alerts(self.m.post(TN, p + "/hsn?tab=B2C", tab="B2C", hsn="1234", uqc="NA",
                                                                rt="18", txval="1")[2])))
        status, _, body = self.m.post(TN, p + "/docs", **{"from": "207", "to": "209", "total": "3", "cancelled": "4"})
        self.assertIn("cannot be more than", " ".join(alerts(body)))
        rid = self.m.coll("tn_b2cs")[-1]["id"]
        self.assertEqual(self.m.post(TN, f"{p}/b2cs/{rid}/remove", submit="1")[0], 303)
        self.assertEqual(self.m.coll("tn_b2cs"), [])

    def test_filing_needs_the_summary_the_declaration_and_the_otp_and_gives_an_arn(self):
        self.sign_in()
        p = "/gstr1/2026-12"
        self.assertEqual(self.m.post(TN, p + "/generate", submit="1")[0], 400)  # nothing to summarise
        self.m.post(TN, p + "/b2cs", pos="29", rt="18", txval="101694.92")
        self.assertEqual(self.m.get(TN, p + "/file")[0], 400)  # not ready yet
        self.m.post(TN, p + "/generate", submit="1")
        ret = next(r for r in self.m.coll("tn_returns") if r["period"] == "2026-12")
        self.assertEqual(ret["status"], "Ready to File")
        _, _, page = self.m.get(TN, p + "/file")
        self.assertIn(">File with EVC</button>", page)
        self.assertEqual(self.m.post(TN, p + "/file-evc", signatory="Ravi Kamath")[0], 400)  # no declaration
        self.assertEqual(self.m.post(TN, p + "/file-evc", declare="yes", signatory="Ravi Kamath")[1], p + "/otp")
        _, _, page = self.m.get(TN, p + "/otp")
        self.assertIn('<label for="f-otp">Enter the one-time password (OTP)</label>', page)
        self.assertNotIn('type="password"', page)  # a code the person reads out, not a stored secret field
        self.assertEqual(self.m.post(TN, p + "/verify", otp="000000")[1], p + "/otp?wrong=1")
        self.assertNotEqual(next(r for r in self.m.coll("tn_returns") if r["period"] == "2026-12")["status"], "Filed")
        otp = self.m.state()["app"]["otps"][f"{self.g}:2026-12"]
        self.assertEqual(self.m.post(TN, p + "/verify", otp=otp)[1], p)
        ret = next(r for r in self.m.coll("tn_returns") if r["period"] == "2026-12")
        self.assertEqual((ret["status"], ret["method"], ret["signatory"]), ("Filed", "EVC", "Ravi Kamath"))
        self.assertRegex(ret["arn"], r"^AA291226\d{6}[A-Z]$")
        _, _, page = self.m.get(TN, p)
        self.assertIn(ret["arn"], page)
        self.assertEqual(self.m.post(TN, p + "/b2cs", pos="32", rt="18", txval="50000")[0], 400)  # filed: no changes

    def test_a_nil_return(self):
        self.sign_in()
        p = "/gstr1/2026-07"
        self.assertEqual(self.m.post(TN, p + "/nil", nil="yes")[0], 303)
        self.assertEqual(self.m.post(TN, p + "/b2cs", pos="29", rt="18", txval="1")[0], 400)
        self.assertEqual(self.m.post(TN, p + "/generate", submit="1")[0], 303)


if __name__ == "__main__":
    unittest.main()
