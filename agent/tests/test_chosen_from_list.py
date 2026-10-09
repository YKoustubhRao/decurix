import unittest
from unittest.mock import patch

from decurix_agent import skill_task, verify


def step(kind, param=None, role=None):
    found = {"kind": kind}
    if param:
        found["param"] = param
    if role:
        found["target"] = {"role": role, "name": "x"}
    return found


# A value typed to search with, then picked from the list that appeared - and
# values that are simply typed.
SKILL = {"name": "enter-a-record", "steps": [
    step("type", "company", "combobox"), step("click", role="option"),
    step("type", "reference", "textbox"), step("click", role="textbox"),
    step("type", "product__2", "combobox"), step("click", role="listitem"),
    step("type", "quantity", "textbox"), step("key"),
]}


class TestValuesChosenFromAList(unittest.TestCase):
    def test_a_value_typed_and_then_picked_from_a_list_is_told_apart_from_one_that_is_typed(self):
        self.assertEqual(skill_task._chosen_from_list(SKILL), {"company", "product"})
        self.assertEqual(skill_task._chosen_from_list({"steps": []}), set())

    def test_the_check_is_told_which_values_are_the_applications_own_records(self):
        # Found live: a correct entry was refused because the record picked
        # from the list was not named as the document words the thing.
        seen = {}

        def chat(messages, **_):
            seen["text"] = messages[1]["content"][0]["text"]
            return {"content": '{"verdict": "done", "confidence": 0.9, "seen": "", "why": ""}'}, {}

        with patch.object(verify.llm_client, "available", return_value=True), \
                patch.object(verify.llm_client, "chat", side_effect=chat), \
                patch.object(verify, "_documents", return_value=("", [])), \
                patch.object(verify, "_count"):
            verify.judge("enter it", values={"company": "Wood Corner Ltd", "reference": "R-1"}, chosen={"company"})

        values = next(line for line in seen["text"].split("\n") if line.startswith("This run's values"))
        self.assertIn("reference = R-1", values)
        self.assertNotIn("Wood Corner", values)                 # not among the values compared by name
        found = next(line for line in seen["text"].split("\n") if line.startswith("Things this run had to find"))
        self.assertIn("for company, the document says: Wood Corner Ltd", found)
        self.assertIn("NOT values to compare by name", found)

    def test_the_reader_of_documents_is_told_which_values_are_searched_for(self):
        # Found live: a shortened form was given for such a value, and the record could no longer be found in it.
        from decurix_agent import binding
        text = binding._prompt(SKILL, [{"name": "company", "label": "Company"}, {"name": "reference", "label": "Ref"}], "")
        described = {d["name"]: d for d in __import__("json").loads(text.split("Values to fill:\n")[1].split("\n\nThe request")[0])}
        self.assertTrue(described["company"].get("lookup"))
        self.assertNotIn("lookup", described["reference"])


if __name__ == "__main__":
    unittest.main()
