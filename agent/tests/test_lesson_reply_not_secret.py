"""
A lesson's words are what the worker is taught: a person's reply is marked secret on the desktop only where it answers
a question asked for one ("secret" by structure) or the person marked it, never because it names the OTP, a password
or a code while explaining the work (found live 2026-11-10, build 83f4a53d, two lessons). The control plane decides
the mark (tasks.Store.MarksSecret); here the desktop keeps a reply sent with "secret": false as typed.

Over-withholding stays (review of freeze 2, A1-A2): a plain word after "password:" or "OTP:" is withheld whatever
follows it ("password: letmein then press login" is a password), and a hyphen gives a word of a marked message a
value's shape (a passphrase "correct-horse-battery"). Possessives are still no quote marks, and a ratio or an amount
is still data.
The two replies are the lessons' replies as the person wrote them (fictional clients and apps).
"""

import json
import unittest
from pathlib import Path

import config_stub  # noqa: F401

import test_secret_first  # noqa: E402
from test_secret_first import TASK  # noqa: E402

from decurix_agent import task_context, watchers  # noqa: E402
from decurix_agent.computer import privacy  # noqa: E402

BOOKS = ("Mostly right; corrections. 7: my OK is needed only for Save to portal, File with EVC and Verify (the app asks "
         "each); do NOT stop after generating the summary or before Proceed to file - compare the summary with the "
         "books yourself and carry on. Only the client has the OTP: ask me for it, never guess. 9: if a receiver's "
         "GSTIN is cancelled in the register, do NOT save or file at all: say which invoice and GSTIN and stop. Also: "
         "the export dialog may still point at another client's or last month's folder - always save into THIS "
         "client's own folder for THIS financial year and month; import with Overwrite, never Append (an earlier "
         "partial import would be doubled); TaxNet opens on the current quarter - choose the financial year, the "
         "quarter that holds the month, then the month; the authorised signatory is the one TaxNet shows for that "
         "client (each client has its own); report the ARN after filing. One client and one month per request; never "
         "create a client or a signatory; never file with DSC.")

CHAT = ("Mostly right; corrections. 6: buyers vary by month - a buyer with a GSTIN goes to Table 4A B2B with one row "
        "per invoice; an inter-state sale to an unregistered buyer above 1,00,000 goes to Table 5 B2C Large; other "
        "unregistered sales go to Table 7, one row per place of supply and rate; Table 12 has a B2B tab and a B2C "
        "tab; a month with no invoices at all is filed as a nil return with the portal Nil option, never with made-up "
        "rows. 7: my approval is needed only before File with EVC and before Verify; do NOT stop after generating the "
        "summary or before Proceed to file - compare the summary with the invoices yourself and carry on. The "
        "one-time code goes only to the client phone, so request it from me and never guess it. 8: done when TaxNet "
        "shows the return Filed with its acknowledgement number; report that number. 9: right - never guess a value. "
        "The invoices are tax-inclusive round amounts: the taxable value is the amount times 100/118, with 9 percent "
        "CGST plus 9 percent SGST inside the state or 18 percent IGST between states.")

BOOKS_KEPT = ("(the app", "receiver's", "client's", "month's", "(an earlier", "(each client")
CHAT_KEPT = ("B2B", "B2C", "1,00,000", "100/118", "tax-inclusive", "made-up", "one-time", "inter-state")
# What a marked message holds by shape among them: hyphenated words (A2). Ratios and amounts are data.
CHAT_HYPHENATED = ("tax-inclusive", "made-up", "one-time", "inter-state")

VECTORS = Path(__file__).resolve().parents[2] / "internal" / "withhold" / "testdata" / "marked-values.json"


class SecretFirst(unittest.TestCase):
    """test_secret_first's desktop (a temporary home, the agent's /request served), without its own tests."""

    setUp = test_secret_first.SecretFirst.setUp
    send = test_secret_first.SecretFirst.send


class ALessonReplyIsNotASecret(SecretFirst):
    def test_a_word_running_on_after_a_label_is_still_withheld(self):
        for text, value in (("password: letmein then press login", "letmein"),
                            ("Only the client has the OTP: ask me for it", "ask"),
                            ("password: ask the client for it, never guess", "ask")):
            self.assertIn(value, privacy.request_secrets(text), text)
        self.assertEqual(privacy.withhold_spoken_secrets("password: letmein then press login"),
                         "password: [withheld] then press login")

    def test_a_reply_not_marked_secret_keeps_every_word(self):
        # Through the agent's own /request: the reply the control plane sends with "secret": false.
        path = self.send(BOOKS, secret=False)
        watchers._mark_handled(path)
        kept = (self.root / "command" / (path.name + ".done")).read_text(encoding="utf-8")
        for word in BOOKS_KEPT:
            self.assertIn(word, kept)
        # Only the word after "OTP:" is a secret (labelled, A1); nothing is held by its shape (not marked).
        self.assertEqual(task_context.secret_values(TASK), ["ask"])
        stored = privacy.withhold_known(BOOKS)
        for word in BOOKS_KEPT:
            self.assertIn(word, stored)

    def test_a_reply_not_marked_secret_keeps_its_codes_figures_and_hyphenated_words(self):
        self.send(CHAT, secret=False)
        self.assertEqual(task_context.secret_values(TASK), [])
        stored = privacy.withhold_known(CHAT)
        for word in CHAT_KEPT:
            self.assertIn(word, stored)

    def test_an_apostrophe_between_letters_is_no_quote_mark(self):
        text = "Send the OTP to the client's phone, then check the receiver's GSTIN 'today' and stop."
        self.assertEqual(privacy.request_secrets(text), [])
        self.assertEqual(privacy.withhold_spoken_secrets(text), text)
        # A real quoted value is still one, an apostrophe inside it included.
        self.assertEqual(privacy.withhold_spoken_secrets("the password 'Tom's9' works"), "the password '[withheld]' works")
        self.assertEqual(privacy.withhold_spoken_secrets("otp: '1234' came"), "otp: '[withheld]' came")

    def test_a_marked_message_holds_its_hyphenated_words_but_not_its_ratios_or_amounts(self):
        held = [v[len(privacy.WHOLE_TOKEN):] for v in privacy.marked_values(CHAT) if v.startswith(privacy.WHOLE_TOKEN)]
        for word in CHAT_HYPHENATED:
            self.assertIn(word, held)
        for word in ("1,00,000", "100/118", "B2B", "B2C"):
            self.assertNotIn(word, held)
        self.assertIn(privacy.WHOLE_TOKEN + "correct-horse-battery",
                      privacy.marked_values("sign in with correct-horse-battery"))

    def test_marked_values_follow_the_shared_vectors(self):
        cases = json.loads(VECTORS.read_text(encoding="utf-8"))["cases"]
        for case in cases:
            got = set(privacy.plain_values(privacy.marked_values(case["text"])))
            want = set(case["values"])
            whole = {case["text"].strip(), " ".join(case["text"].split())}
            self.assertLessEqual(want, got, case["text"])
            self.assertLessEqual(got - want, whole, case["text"])


class SecretsAreStillWithheld(SecretFirst):
    def test_a_labelled_value_is_still_one(self):
        for text, value in (("Use OTP: 604217 now", "604217"), ("password: Lift$2026", "Lift$2026"),
                            ("password: letmein then press login", "letmein"),
                            ("pwd=x7#k", "x7#k"), ("the password is Lift$2026", "Lift$2026"),
                            ("password: tiger.", "tiger"), ("password: tiger", "tiger"),
                            ('the password "blue horse" then sign in', "blue horse")):
            self.assertIn(value, privacy.request_secrets(text), text)
            self.assertNotIn(value, privacy.withhold_spoken_secrets(text), text)
        privacy.remember_secrets("Use OTP: 604217 now")
        self.assertEqual(privacy.withhold_known("typed 604217"), "typed [withheld]")

    def test_a_reply_to_a_question_asked_for_a_secret_is_withheld_whole(self):
        reply = "ask me later"  # (plain words: the question's "secret" makes it one, not its shape)
        self.send(reply, secret=True)
        self.assertIn(reply, task_context.secret_values(TASK))
        self.assertEqual(privacy.withhold_known(f"I typed {reply} into the field"), "I typed [withheld] into the field")

    def test_a_reply_the_worker_asked_for_as_a_secret_is_withheld_whole(self):
        task_context.start(TASK, request="file the return")
        watchers._remember_reply_secret("blue horse")
        self.assertEqual(privacy.withhold_known("typed blue horse"), "typed [withheld]")


if __name__ == "__main__":
    unittest.main()
