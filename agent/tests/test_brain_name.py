"""
What a failure note calls the desktop's model (thebot_harness._brain_name): each hosted brain by its own name, the
local model only for a local brain. An openai brain's failure used to name the Ollama model nobody chose.
"""

import unittest
from unittest import mock

from decurix_agent import config, thebot_harness


class TheBrainsName(unittest.TestCase):
    def name(self, brain, model=""):
        with mock.patch.object(config, "BRAIN", brain), mock.patch.object(config, "BRAIN_MODEL", model), \
                mock.patch.object(config, "OLLAMA_MODEL", "local-model:1b"):
            return thebot_harness._brain_name()

    def test_each_hosted_brain_is_named_and_never_the_local_model(self):
        for brain, word in (("claude", "Claude"), ("azure", "Azure"), ("openai", "OpenAI")):
            said = self.name(brain, "default")
            self.assertIn(word, said)
            self.assertIn("default", said)
            self.assertNotIn("local-model", said)

    def test_a_local_brain_is_its_ollama_model(self):
        self.assertEqual(self.name("ollama"), "local-model:1b")

    def test_an_openai_failure_note_names_the_hosted_model(self):
        with mock.patch.object(config, "BRAIN", "openai"), mock.patch.object(config, "HOSTED_BRAIN", True), \
                mock.patch.object(config, "BRAIN_MODEL", ""), mock.patch.object(config, "OLLAMA_MODEL", "local-model:1b"):
            note = thebot_harness._backend_note("HTTP 502")
        self.assertIn("OpenAI-compatible", note)
        self.assertNotIn("local-model", note)


if __name__ == "__main__":
    unittest.main()
