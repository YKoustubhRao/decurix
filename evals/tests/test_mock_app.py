"""The records mock speaks the practice mock contract: --port/--fixture/--state, state written at start and after
every change. Run: py -3 -m unittest discover -s evals/tests"""

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
MOCK = os.path.join(os.path.dirname(HERE), "mocks", "records-app", "mock_app.py")


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class MockApp:
    def __init__(self, fixture):
        self.dir = tempfile.TemporaryDirectory()
        self.fixture = os.path.join(self.dir.name, "fixture.json")
        self.state = os.path.join(self.dir.name, "state.json")
        with open(self.fixture, "w", encoding="utf-8") as f:
            json.dump(fixture, f)
        self.port = free_port()
        self.proc = subprocess.Popen([sys.executable, MOCK, "--port", str(self.port), "--host", "127.0.0.1",
                                      "--fixture", self.fixture, "--state", self.state],
                                     stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        deadline = time.time() + 10
        while time.time() < deadline:
            try:
                urllib.request.urlopen(self.url("/"), timeout=1).read()
                return
            except OSError:
                time.sleep(0.1)
        self.close()
        raise RuntimeError("the mock did not start")

    def url(self, path):
        return f"http://127.0.0.1:{self.port}{path}"

    def get(self, path):
        with urllib.request.urlopen(self.url(path), timeout=5) as r:
            return r.status, r.read().decode("utf-8")

    def post(self, path, **fields):
        opener = urllib.request.build_opener(NoRedirect)
        data = urllib.parse.urlencode(fields).encode()
        try:
            with opener.open(urllib.request.Request(self.url(path), data=data), timeout=5) as r:
                return r.status, r.headers.get("Location", ""), r.read().decode("utf-8")
        except urllib.error.HTTPError as e:
            return e.code, e.headers.get("Location", ""), e.read().decode("utf-8")

    def read_state(self):
        with open(self.state, encoding="utf-8") as f:
            return json.load(f)

    def close(self):
        self.proc.terminate()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
        if self.proc.stderr:
            self.proc.stderr.close()
        self.dir.cleanup()


FIXTURE = {
    "options": {"cost_centres": ["General", "Project Birch"]},
    "collections": {
        "users": [{"id": 1, "name": "Ann Lowe", "email": "ann.lowe@example.test", "role": "Admin"}],
        "payments": [{"id": 1, "payee": "Bluequill Stationers", "account": "6620150043218765", "amount": "512.40",
                      "date": "2026-03-10", "reference": "BL-1987", "status": "unpaid"}],
    },
}


class MockAppTest(unittest.TestCase):
    def setUp(self):
        self.app = MockApp(FIXTURE)
        self.addCleanup(self.app.close)

    def test_fixture_loads_and_state_is_written_at_start(self):
        state = self.app.read_state()
        self.assertEqual(state["collections"]["users"], FIXTURE["collections"]["users"])
        self.assertEqual(state["collections"]["bills"], [])
        status, body = self.app.get("/users")
        self.assertEqual(status, 200)
        self.assertIn("Ann Lowe", body)
        self.assertIn('href="/users/1/edit">Edit Ann Lowe</a>', body)

    def test_a_form_submit_lands_in_state(self):
        status, location, _ = self.app.post("/bills/new", number="BL-2041", vendor="Bluequill Stationers",
                                            date="2026-03-12", due="", total="1,416.00", cost_centre="General")
        self.assertEqual(status, 303)
        self.assertEqual(location, "/bills/1?saved=1")
        bills = self.app.read_state()["collections"]["bills"]
        self.assertEqual(len(bills), 1)
        self.assertEqual(bills[0]["number"], "BL-2041")
        self.assertEqual(bills[0]["date"], "2026-03-12")
        self.assertEqual(bills[0]["id"], 1)

    def test_a_form_refuses_a_missing_or_wrong_value(self):
        status, _, body = self.app.post("/bills/new", number="X-1", vendor="V", date="03/04/2026", total="1")
        self.assertEqual(status, 400)
        self.assertIn("Bill date must be a date.", body)
        self.assertIn("Cost centre is required.", body)
        self.assertEqual(self.app.read_state()["collections"]["bills"], [])

    def test_an_edit_keeps_the_record_id(self):
        status, _, _ = self.app.post("/users/1/edit", name="Ann Lowe", email="ann@example.org", role="Admin")
        self.assertEqual(status, 303)
        users = self.app.read_state()["collections"]["users"]
        self.assertEqual(users, [{"id": 1, "name": "Ann Lowe", "email": "ann@example.org", "role": "Admin"}])

    def test_pay_needs_a_confirmation(self):
        status, _, body = self.app.post("/payments/1/pay")
        self.assertEqual(status, 200)
        self.assertIn("Confirm payment", body)
        self.assertEqual(self.app.read_state()["collections"]["payments"][0]["status"], "unpaid")
        status, _, _ = self.app.post("/payments/1/pay/confirm")
        self.assertEqual(status, 303)
        self.assertEqual(self.app.read_state()["collections"]["payments"][0]["status"], "paid")

    def test_every_field_has_a_label(self):
        for path in ("/users/new", "/bills/new", "/payees/new", "/payments/new", "/tickets/new", "/users/1/edit"):
            _, body = self.app.get(path)
            for fid in __import__("re").findall(r'id="(f-[a-z_]+)"', body):
                self.assertIn(f'<label for="{fid}">', body, f"{path}: {fid}")

    def test_an_unknown_page_is_a_clean_404(self):
        with self.assertRaises(urllib.error.HTTPError) as err:
            self.app.get("/bills/99")
        self.assertEqual(err.exception.code, 404)


class SignInTest(unittest.TestCase):
    def test_sign_in_guards_every_page_when_the_fixture_asks(self):
        app = MockApp(dict(FIXTURE, auth={"user": "clerk", "password": "demo-pass-1"}))
        self.addCleanup(app.close)
        opener = urllib.request.build_opener(NoRedirect)
        with self.assertRaises(urllib.error.HTTPError) as err:
            opener.open(app.url("/users"), timeout=5)
        self.assertEqual(err.exception.headers.get("Location"), "/login")
        status, location, _ = app.post("/login", user="clerk", password="wrong")
        self.assertEqual(location, "/login?error=1")
        status, location, _ = app.post("/login", user="clerk", password="demo-pass-1")
        self.assertEqual((status, location), (303, "/"))


if __name__ == "__main__":
    unittest.main()
