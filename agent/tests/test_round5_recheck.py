"""Round-5 recheck: the shell check reads rm word by word, and money paid back out makes a payment file."""

import os
import unittest
from pathlib import PurePosixPath
from unittest import mock

from decurix_agent import config, payees
from decurix_agent.commands import dangerous_reason


class RmReadByWord(unittest.TestCase):
    def setUp(self):
        patches = (mock.patch.dict(os.environ, {"HOME": "/home/headless"}),
                   mock.patch.object(config, "WORK_DIR", PurePosixPath("/home/headless/work")))
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def test_every_spelling_of_a_wipe_waits(self):
        for command in ('rm -rf "$HOME"', "bash -c 'rm -rf ~'", "/bin/rm -rf ~", "sudo /usr/bin/rm -rf /",
                        "env rm -rf ~", "nohup rm -rf ~ &", "timeout 5 rm -rf /etc", 'sh -c "rm -rf $HOME/"',
                        "command rm -rf ~", "\\rm -rf ~", "rm -rf ~/..", "cd / && rm -rf *", "cd ~ && rm -rf ./*",
                        'rm -r -f -- "${HOME}"', "rm -rf ../..", "cd /tmp\ncd ..\nrm -rf etc", "echo $(rm -rf ~)",
                        "echo `rm -rf ~`", "X=1 rm --recursive ~/", "rm -rf /h*", 'bash -lc "sh -c \\"rm -rf ~\\""',
                        "rm -rf ~/.*", "rm -rf /usr/lib", "cat > f <<'EOF'\ndon't\nEOF\nrm -rf ~",
                        "bash <<EOF\nrm -rf ~\nEOF", "sudo -u root rm -rf ~", "xargs -I {} rm -rf ~",
                        "env -S 'rm -rf ~'", "if true; then rm -rf ~; fi", "nice -n 5 timeout 9 /usr/bin/rm -rf /var"):
            with self.subTest(command=command):
                self.assertTrue(dangerous_reason(command), command)

    def test_ordinary_clean_up_runs(self):
        for command in ("rm -rf build/", "rm -rf ~/work/tmp", "rm -rf ~/work/*", 'grep "rm -rf ~" notes.txt',
                        "rm -f ~/x.txt", "rm -rf ../build", "cd /tmp && rm -rf x", 'rm -rf "$TMPDIR/x"',
                        "echo \"don't\"; rm -rf out", "rm -rf ~/Downloads/*.zip", "rm -rf node_modules dist",
                        "rm -rf /tmp/*", "python3 - <<'PY'\nprint('rm -rf ~')\nPY", "echo it's fine && rm -rf out", "echo rm -rf /"):
            with self.subTest(command=command):
                self.assertIsNone(dangerous_reason(command), command)


class MoneyBackOut(unittest.TestCase):
    def test_reimbursement_and_payout_are_paid(self):
        for header in (["Employee", "Acc", "Reimbursement"], ["Employee", "Account No", "Reimbursed (Rs)"],
                       ["Vendor", "A/c", "Payout"], ["Customer", "Account", "Refund"]):
            with self.subTest(header=header):
                self.assertIsNotNone(payees.payment_table(header))

    def test_ledgers_hold_no_bank_details(self):
        # A ledger's or a stock table's "account" is no bank's. (A customer's "Account" beside sales is a bank
        # account, checked by where it comes from since review 2026-11-03, F-01.)
        for header in (["GL Account", "Amount"], ["Item", "Stock Account", "Stock Transfer"]):
            with self.subTest(header=header):
                self.assertIsNone(payees.payment_table(header))


class LocalQuestionFilesWithheld(unittest.TestCase):
    def test_question_and_final_files_hold_no_known_secret(self):
        import tempfile
        from pathlib import Path

        from decurix_agent import messaging
        from decurix_agent.computer import privacy

        def withhold(text):
            return str(text).replace("hunter2-SECRET", "[withheld]")

        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(messaging.config, "QUERYTEXT_DIR", Path(tmp)), \
                mock.patch.object(privacy, "withhold_known", withhold), \
                mock.patch.object(messaging, "send_query", lambda *a, **kw: True), \
                mock.patch.object(messaging, "_query_gate", None), \
                mock.patch.object(messaging, "_final_gate", None, create=True):
            messaging.ask_for_query("Log in with hunter2-SECRET?", kind="query")
            messaging.write_final_reply("Used hunter2-SECRET to log in.", kind="error")
            written = [p.read_text(encoding="utf-8") for p in Path(tmp).iterdir()]
        self.assertTrue(written)
        for text in written:
            self.assertNotIn("hunter2-SECRET", text)


if __name__ == "__main__":
    unittest.main()
