"""Round-4 recheck: shell work that sends or converts files is never taken for read-only, and refund files are payment
files."""

import unittest

from decurix_agent import payees, playbooks


class ShellWorkThatChangesThings(unittest.TestCase):
    def test_senders_converters_and_schedulers_are_changes(self):
        # A list of writers missed these, so a shell-only task that ran one went out without the final check.
        writes = [
            "mutt -s Invoice a@b.example < inv.txt", "msmtp a@b.example < mail.txt", "rclone copy out remote:backup",
            "lp report.pdf", "lpr report.pdf", "pandoc a.md -o a.docx", "convert a.png a.pdf", "swaks --to a@b.example",
            "crontab -l | { cat; echo '0 9 * * * x'; } | crontab -", "at now + 1 minute", "gpg -o x.gpg -e f",
            "/bin/rm -rf build", "\\rm x", "find . -name '*.tmp' | xargs rm", "find . -name x -delete",
            "bash -c 'mutt -s x a@b.example < f'", "echo $(rclone sync a b:c)", "env FOO=1 rclone ls x:",
            "timeout 10 mutt -s x a@b.example", "sort -o out.txt in.txt", "sed -i s/a/b/ f", "uniq in.txt out.txt",
            "awk '{system(\"rm \" $1)}' list", "pdftotext a.pdf",
            "for f in *.md; do pandoc \"$f\" -o \"${f%.md}.pdf\"; done",
        ]
        self.assertEqual([c for c in writes if not playbooks._changes(c)], [])

    def test_a_second_line_is_a_second_command(self):
        playbooks.begin()
        playbooks.note_command("ls ~/work\nmutt -s x a@b.example < f")
        self.assertEqual(len(playbooks.changed_things()), 1)
        playbooks.begin()

    def test_read_only_answers_stay_read_only(self):
        reads = [
            "ls -la ~/work", "cat a.txt | wc -l", "grep -E 'a|b' notes.txt | sort | uniq -c",
            "for f in *.csv; do wc -l \"$f\"; done", "x=$(date +%F); echo \"today $x\"",
            "find . -name '*.py' | xargs grep -n TODO", "pdftotext a.pdf - | head", "df -h && free -m",
            "cd ~/work && ls", "jq .total data.json", "awk -F, '{s+=$3} END {print s}' a.csv", "sed -n 1,20p a.txt",
            "test -f a && echo yes", "if [ -f a ]; then cat a; fi", "while read l; do echo \"$l\"; done < list.txt",
            "head -5 a.csv | column -t -s,", "office read a.xlsx",
        ]
        self.assertEqual([c for c in reads if playbooks._changes(c)], [])

    def test_quoting_that_cannot_be_read_counts_as_a_change(self):
        self.assertTrue(playbooks._changes("echo 'unclosed"))


class RefundFiles(unittest.TestCase):
    def test_a_refund_to_a_customer_is_checked(self):
        # A customer's account beside a refund is checked. Since review 2026-11-03 (F-01) so is one beside any other
        # figure: whether a table pays anyone is not read from its headers - its bank identifiers must come from an
        # input, which a report's do.
        for header in (["Customer", "Account", "Refund"], ["Customer Name", "A/c No", "Refund Amount"],
                       ["Customer", "Account", "Amount"], ["Customer", "Account", "Total Sales"]):
            self.assertIsNotNone(payees.payment_table(header), header)

    def test_acc_is_an_account_column(self):
        self.assertIsNotNone(payees.payment_table(["Payee", "Acc", "Amount"]))
        self.assertIsNotNone(payees.payment_table(["Vendor", "Acc No", "IFSC", "Amount"]))


if __name__ == "__main__":
    unittest.main()
