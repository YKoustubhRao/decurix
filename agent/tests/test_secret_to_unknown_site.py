import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import config, task_context
from decurix_agent.computer import approval


# ============================================================
# Found live (2026-10-21): the engine opened a real site of the same name
# from its own memory and typed the client's password into it.
# ============================================================

PASSWORD = {"role": "password text", "name": "Password *", "handle": {"input_type": "password"}}
NAME_BOX = {"role": "textbox", "name": "Username *"}


class TestASecretGoesOnlyWhereThePersonSent(unittest.TestCase):

    def setUp(self):
        self.home = Path(tempfile.mkdtemp())
        (self.home / "skills").mkdir()
        (self.home / "skills" / "gstr1-prepare.json").write_text(json.dumps(
            {"steps": [{"path": "site host.docker.internal:8703 > textbox \"Password *\""}]}), encoding="utf-8")
        for patcher in (mock.patch.object(config, "DECURIX_DIR", self.home),
                        mock.patch.object(task_context, "current_request", return_value="Prepare the GSTR-1 on the GST portal"),
                        mock.patch.object(approval.modes, "get_stage", return_value=None)):
            patcher.start()
            self.addCleanup(patcher.stop)
        approval._KNOWN_CACHE.update(at=0.0, text="")

    def reason(self, url, element=PASSWORD, text="Kavya@2026"):
        return approval.assess("type", element, text=text, context={"url": url, "window": "Login"})

    def test_a_password_into_a_site_nobody_named_needs_a_yes(self):
        why = self.reason("https://services.gst.gov.in/services/login")
        self.assertIsNotNone(why)
        self.assertIn("services.gst.gov.in", why)

    def test_the_site_a_person_taught_on_is_fine(self):
        self.assertIsNone(self.reason("http://host.docker.internal:8703/login"))

    def test_a_site_the_request_names_is_fine(self):
        with mock.patch.object(task_context, "current_request", return_value="log in at https://portal.example.org"):
            self.assertIsNone(self.reason("https://portal.example.org/login"))

    def test_typing_into_an_ordinary_field_is_not_stopped(self):
        self.assertIsNone(self.reason("https://services.gst.gov.in/services/login", element=NAME_BOX, text="kavyacon"))

    def test_a_desktop_app_with_no_address_is_not_stopped(self):
        self.assertIsNone(self.reason(""))


class TestATypedSecretStaysOutOfReplies(unittest.TestCase):
    """Found live: the report listed the client's portal password."""

    def test_a_password_spelt_out_after_its_name_is_withheld(self):
        from decurix_agent.computer import privacy
        text = privacy.withhold_spoken_secrets("Credentials tried:  - username: srilakshmilift  - password: Lift$2026")
        self.assertNotIn("Lift$2026", text)
        self.assertIn("srilakshmilift", text)
        self.assertIn("PIN: 576104", privacy.withhold_spoken_secrets("Manipal, PIN: 576104"))

    def test_a_value_typed_into_a_password_field_is_withheld_afterwards(self):
        from decurix_agent.computer import privacy
        home = Path(tempfile.mkdtemp())
        with mock.patch.object(privacy, "_secrets_file", return_value=home / "secrets"):
            privacy._known_cache.clear()
            privacy.remember_value("Kavya@2026")
            self.assertNotIn("Kavya@2026", privacy.withhold_known("I logged in with Kavya@2026 and filed."))


if __name__ == "__main__":
    unittest.main()
