import unittest
from unittest.mock import patch
from decurix_agent import study

class StudyBudgets(unittest.TestCase):
    def test_long_recordings_get_room_for_reasoning_and_step_json(self):
        self.assertEqual(study._token_budget({"steps": [{}]*169}), 17520)
        self.assertEqual(study._token_budget({"steps": []}), 6000)
        self.assertEqual(study._token_budget({"steps": [{}]*1000}), 24000)

    def test_json_format_and_budget_are_sent_to_model(self):
        with patch.object(study.llm_client, "chat", return_value=({"content": '{"summary":"saved draft"}'}, {})) as chat:
            self.assertEqual(study._call("system", [], max_tokens=17520)["summary"], "saved draft")
        self.assertEqual(chat.call_args.kwargs["max_tokens"], 17520)
        self.assertEqual(chat.call_args.kwargs["response_format"], {"type": "json_object"})
