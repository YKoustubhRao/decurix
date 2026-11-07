import unittest


class TestInferenceEndpointSelection(unittest.TestCase):
    def test_is_openai_endpoint(self):
        from decurix_agent.ollama_client import is_openai_endpoint
        self.assertTrue(is_openai_endpoint("http://vllm-server:8000/v1"))
        self.assertTrue(is_openai_endpoint("http://vllm-server:8000/v1/"))
        self.assertFalse(is_openai_endpoint("http://localhost:11434"))

    def test_get_optimal_threads(self):
        from decurix_agent.ollama_client import get_optimal_threads
        threads = get_optimal_threads()
        self.assertIsInstance(threads, int)
        self.assertGreater(threads, 0)


if __name__ == "__main__":
    unittest.main()
