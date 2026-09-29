import io
import json
import unittest
import urllib.error
from email.message import Message
from unittest import mock

from decurix_agent import llm_client


def http_error(code, headers=None):
    h = Message()
    for k, v in (headers or {}).items():
        h[k] = v
    return urllib.error.HTTPError("http://cp/api/llm/chat", code, "err", h, io.BytesIO(b'{"error":{}}'))


class Ok:
    def __init__(self, body):
        self.body = json.dumps(body).encode()

    def read(self):
        return self.body

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


ANSWER = {"choices": [{"message": {"content": "hi"}}], "usage": {"prompt_tokens": 3}}


class RateLimits(unittest.TestCase):
    def test_a_rate_limit_waits_as_long_as_the_endpoint_says(self):
        calls = [http_error(429, {"Retry-After": "37"}), Ok(ANSWER)]
        with mock.patch.object(llm_client.urllib.request, "urlopen", side_effect=calls), \
                mock.patch.object(llm_client.time, "sleep") as sleep, \
                mock.patch.object(llm_client.random, "uniform", return_value=0):
            message, usage = llm_client.chat([{"role": "user", "content": "x"}])
        self.assertEqual(message["content"], "hi")
        sleep.assert_called_once_with(37.0)

    def test_milliseconds_are_read_first(self):
        h = Message()
        h["Retry-After-Ms"] = "1500"
        h["Retry-After"] = "9"
        self.assertEqual(llm_client.retry_after(h), 1.5)

    def test_it_gives_up_within_its_budget(self):
        errors = [http_error(429, {"Retry-After": "200"}) for _ in range(5)]
        with mock.patch.object(llm_client.urllib.request, "urlopen", side_effect=errors), \
                mock.patch.object(llm_client.time, "sleep") as sleep, \
                mock.patch.object(llm_client.random, "uniform", return_value=0), \
                mock.patch.object(llm_client, "RETRY_BUDGET_SECONDS", 240):
            with self.assertRaises(llm_client.LLMError):
                llm_client.chat([{"role": "user", "content": "x"}])
        self.assertEqual(sleep.call_count, 1)  # 200 s once; a second 200 s would pass the 240 s budget

    def test_our_own_mistake_is_not_retried(self):
        with mock.patch.object(llm_client.urllib.request, "urlopen", side_effect=[http_error(400)]), \
                mock.patch.object(llm_client.time, "sleep") as sleep:
            with self.assertRaises(llm_client.LLMError):
                llm_client.chat([{"role": "user", "content": "x"}])
        sleep.assert_not_called()


if __name__ == "__main__":
    unittest.main()
