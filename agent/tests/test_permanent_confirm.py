import unittest

from decurix_agent.computer import approval


def button(name, path):
    return {"role": "button", "name": name, "path": path}


class AYesToAPermanentQuestion(unittest.TestCase):
    """A confirm dialog's own question is what decides whether its plain Yes is consequential."""

    def test_the_live_case_is_stopped(self):
        # As the page read it on 2026-10-13, the dialog's name truncated as paths keep it.
        yes = button("Yes", 'site host.docker.internal:8000 > dialog "Confirm Permanently Submit PINV-26-0…" > button "Yes"')
        why = approval.assess("click", yes)
        self.assertIsNotNone(why)
        self.assertIn("Permanently Submit", why)

    def test_other_questions_that_cannot_be_taken_back(self):
        for dialog in ("Send this email to 4 people?", "This cannot be undone. Continue?", "Delete 3 records?",
                       "Pay INR 40,000 now?", "Publish the post?"):
            for answer in ("Yes", "OK", "Confirm", "Continue", "Yes, delete"):
                self.assertIsNotNone(approval.assess("click", button(answer, f'dialog "{dialog}" > button "{answer}"')),
                                     (dialog, answer))

    def test_a_no_and_an_ordinary_question_go_through(self):
        no = button("No", 'dialog "Confirm Permanently Submit PINV-26-0…" > button "No"')
        self.assertIsNone(approval.assess("click", no))
        for dialog in ("Leave site?", "Discard unsaved changes?", "Reload the page?", "Confirm"):
            self.assertIsNone(approval.assess("click", button("Yes", f'dialog "{dialog}" > button "Yes"')), dialog)
        # A Yes that is not in a dialog says nothing about what it answers.
        self.assertIsNone(approval.assess("click", button("Yes", 'form "Survey" > button "Yes"')))
        # A button whose own label is the act is judged by its label, not by this rule.
        self.assertIsNone(approval.confirms_permanently(button("Submit feedback", 'dialog "Send feedback?" > button "Submit feedback"')))


if __name__ == "__main__":
    unittest.main()
