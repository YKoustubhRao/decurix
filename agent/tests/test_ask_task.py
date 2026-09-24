import json
import unittest
from unittest.mock import patch

from decurix_agent import ask_task, watchers
from decurix_agent.computer.errors import ComputerError
from decurix_agent.thebot_harness import readable_step


def _asked(content):
    found = ask_task.parse(content)
    return (found[0]["key"], found[1]) if found else None


class TestReadingTheRequest(unittest.TestCase):
    """What names a web app and a question for it, and what is left to the engine."""

    def test_the_request_that_paged_a_person(self):
        # Live: TheBot sent this to chat, then to the operator, which ran curl
        # on an image with no curl and asked a person to install it.
        self.assertEqual(_asked('chatgpt "where is addid ababa"'), ("chatgpt", "where is addid ababa"))

    def test_ways_of_asking(self):
        cases = {
            "ask chatgpt where is addis ababa": ("chatgpt", "where is addis ababa"),
            "Ask ChatGPT: what is 2+2?": ("chatgpt", "what is 2+2?"),
            "use chatgpt to write a haiku about rain": ("chatgpt", "write a haiku about rain"),
            "can you ask chatgpt what the capital of peru is": ("chatgpt", "what the capital of peru is"),
            "on chatgpt, explain quantum tunnelling": ("chatgpt", "explain quantum tunnelling"),
            "chat gpt how tall is everest": ("chatgpt", "how tall is everest"),
            "chatgpt “where is lima”": ("chatgpt", "where is lima"),
            "google best pizza near me": ("google", "best pizza near me"),
            "search google for flights to goa": ("google", "flights to goa"),
            "search for lo-fi beats on youtube": ("youtube", "lo-fi beats"),
            "youtube lofi hip hop": ("youtube", "lofi hip hop"),
            "wiki addis ababa": ("wikipedia", "addis ababa"),
            "look up addis ababa on wikipedia": ("wikipedia", "addis ababa"),
            "duckduckgo privacy browsers": ("duckduckgo", "privacy browsers"),
            "open youtube": ("youtube", ""),
        }
        for content, expected in cases.items():
            with self.subTest(content=content):
                self.assertEqual(_asked(content), expected)

    def test_what_is_not_a_question_for_a_web_app(self):
        for content in [
            "google docs create a new document",     # another Google product
            "google's revenue in 2024",
            "what does google say about x",           # the name does not lead
            "googling is fun",
            "check the disk space",
            "hello",
            "/replay demo-1",
            "chatgpt write a poem and save it to poem.txt",   # a job for the desktop after the answer
            "google x and open the first result",
            "ask chatgpt for a recipe; then email it to me",
        ]:
            with self.subTest(content=content):
                self.assertIsNone(ask_task.parse(content))

    def test_an_assistant_keeps_its_own_instructions(self):
        # "and then summarise it again" is for ChatGPT, not for the desktop.
        self.assertEqual(_asked("ask chatgpt to summarise the french revolution and then summarise it again"),
                         ("chatgpt", "summarise the french revolution and then summarise it again"))

    def test_the_watcher_takes_it_without_the_model(self):
        self.assertTrue(watchers._needs_no_backend('chatgpt "where is addis ababa"'))
        self.assertFalse(watchers._needs_no_backend("check the disk space"))


class FakePage:
    """A page whose reader answers from a script, one entry per poll."""

    def __init__(self, answers):
        self.answers = answers
        self.closed = False

    def evaluate(self, _js):
        answer = self.answers.pop(0) if len(self.answers) > 1 else self.answers[0]
        if isinstance(answer, Exception):
            raise answer
        return json.dumps(answer)

    def close(self):
        self.closed = True


class Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


CHATGPT = next(s for s in ask_task.SERVICES if s["key"] == "chatgpt")
GOOGLE = next(s for s in ask_task.SERVICES if s["key"] == "google")


class TestWaitingForTheAnswer(unittest.TestCase):
    def _read(self, service, pages):
        clock = Clock()
        pages = list(pages)
        return ask_task._read(service, sleep=clock.sleep, clock=clock, page_for=lambda _s: pages.pop(0) if pages else None), clock

    def test_an_answer_is_taken_once_it_stops_changing(self):
        page = FakePage([
            {"state": "pending"},
            {"state": "streaming", "text": "It sits"},
            {"state": "answer", "text": "It sits in Ethiopia."},
            {"state": "answer", "text": "It sits in Ethiopia's highlands."},
            {"state": "answer", "text": "It sits in Ethiopia's highlands."},
        ])
        result, clock = self._read(CHATGPT, [page])
        self.assertEqual(result["text"], "It sits in Ethiopia's highlands.")
        self.assertGreaterEqual(clock.now, ask_task.STABLE_SECONDS)
        self.assertTrue(page.closed)

    def test_a_reply_not_yet_marked_complete_is_waited_for(self):
        # Live: read before ChatGPT marked its chips up, the reply ended with a
        # stray "Mount Kilimanjaro" - the entity chip, as text.
        page = FakePage([{"state": "streaming", "text": "Kilimanjaro. Mount Kilimanjaro"}] * 3
                        + [{"state": "answer", "text": "Kilimanjaro."}])
        result, _ = self._read(CHATGPT, [page])
        self.assertEqual(result["text"], "Kilimanjaro.")

    def test_a_reply_that_never_says_it_is_finished_is_taken_once_it_stalls(self):
        result, clock = self._read(CHATGPT, [FakePage([{"state": "streaming", "text": "Ethiopia."}])])
        self.assertEqual((result["state"], result["text"]), ("answer", "Ethiopia."))
        self.assertLess(clock.now, CHATGPT["seconds"])

    def test_a_page_that_moves_is_attached_again(self):
        # ChatGPT goes to /uc/<id> once it has the question; the old session drops.
        first = FakePage([ComputerError("Chromium did not answer Runtime.evaluate")])
        second = FakePage([{"state": "answer", "text": "Ethiopia."}])
        result, _ = self._read(CHATGPT, [first, second])
        self.assertEqual(result["text"], "Ethiopia.")
        self.assertTrue(first.closed)

    def test_a_challenge_page_ends_the_wait(self):
        result, clock = self._read(GOOGLE, [FakePage([{"state": "blocked", "why": "unusual traffic"}])])
        self.assertEqual(result["state"], "blocked")
        self.assertEqual(clock.now, 0)

    def test_no_answer_in_time_is_said_so(self):
        result, clock = self._read(CHATGPT, [FakePage([{"state": "pending", "excerpt": "Log in"}])])
        self.assertTrue(result.get("timed_out"))
        self.assertGreaterEqual(clock.now, CHATGPT["seconds"])


class TestTheReply(unittest.TestCase):
    def test_an_assistant_is_quoted(self):
        reply, kind = ask_task.render(CHATGPT, "where is addis ababa", {"state": "answer", "text": "In Ethiopia."})
        self.assertEqual(kind, "final")
        self.assertTrue(reply.startswith("ChatGPT says:\n\nIn Ethiopia."))

    def test_a_search_lists_what_it_found(self):
        reply, kind = ask_task.render(GOOGLE, "addis ababa", {"state": "answer", "text": "The capital of Ethiopia.",
                                                               "results": [{"title": "Addis Ababa", "site": "en.wikipedia.org"}]})
        self.assertEqual(kind, "final")
        self.assertIn("AI overview:\nThe capital of Ethiopia.", reply)
        self.assertIn("1. Addis Ababa - en.wikipedia.org", reply)

    def test_a_refusal_and_a_timeout_are_failures(self):
        self.assertEqual(ask_task.render(GOOGLE, "x", {"state": "blocked", "why": "unusual traffic"})[1], "error")
        reply, kind = ask_task.render(CHATGPT, "x", {"state": "pending", "timed_out": True, "excerpt": "Log in to continue"})
        self.assertEqual(kind, "error")
        self.assertIn("Log in to continue", reply)


class TestPerform(unittest.TestCase):
    def _perform(self, content, result=None, open_error=None):
        sent = []
        with patch.object(ask_task.core, "open_target", side_effect=open_error) as opened, \
                patch.object(ask_task, "_read", return_value=result or {}), \
                patch.object(ask_task, "write_final_reply", side_effect=lambda r, **kw: sent.append((r, kw))), \
                patch.object(ask_task.progress, "emit"):
            ask_task.perform(content)
        return sent, opened

    def test_the_question_goes_in_the_address(self):
        sent, opened = self._perform('chatgpt "where is addis ababa"', {"state": "answer", "text": "In Ethiopia."})
        opened.assert_called_once_with("https://chatgpt.com/?q=where%20is%20addis%20ababa", new_tab=True)
        self.assertEqual(sent[0][1], {"kind": "final", "engine": "web"})
        self.assertIn("ChatGPT says:", sent[0][0])

    def test_a_credential_is_never_sent(self):
        sent, opened = self._perform("chatgpt is sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789 a valid key?")
        opened.assert_not_called()
        self.assertEqual(sent[0][1]["kind"], "error")

    def test_a_browser_that_will_not_open_is_reported(self):
        sent, _ = self._perform("google pizza", open_error=ComputerError("Chromium did not start"))
        self.assertEqual(sent[0][1]["kind"], "error")
        self.assertIn("Chromium did not start", sent[0][0])


class TestReadableSteps(unittest.TestCase):
    """soul's job log, as the chat and the desktop's card show it."""

    def test_plumbing_is_left_out(self):
        for line in [
            'scheduler: "chatgpt \\"where is addid ababa\\"" [general] -> skill=general model=qwen2.5-coder:3b',
            "memory: checking L1 for anything relevant...",
            "memory: nothing in L1 yet",
            "skill library: checking for an acquired skill that fits this request...",
            "skill library: no matching acquired skill",
            "scheduler: rerouting to the operator",
            "Answering directly - no task steps needed.",
            "skill:operator -> model replied: COMMAND: ls",
        ]:
            with self.subTest(line=line):
                self.assertIsNone(readable_step(line))

    def test_the_rest_is_put_into_words(self):
        cases = {
            "skill:operator -> $ uname -r": "Running: uname -r",
            "skill:operator -> exit 0 · 6.6.87": "Output: 6.6.87",
            "skill:operator -> exit 1 · xargs: curl: No such file or directory":
                "The command failed (exit 1): xargs: curl: No such file or directory",
            "skill:general -> this needs a look at the machine - handing it to the operator":
                "This needs a look at the machine - working it out command by command",
            "scheduler: step 2/3 [web] find the newest go version": "Step 2 of 3: find the newest go version",
            "atomizer: a question - answering directly": "A question - answering directly",
            "skill:web -> searching the real web for: go": "Searching the real web for: go",
            "Plan ready: 2 step(s) to run.": "Plan ready: 2 step(s) to run.",
        }
        for line, expected in cases.items():
            with self.subTest(line=line):
                self.assertEqual(readable_step(line), expected)


if __name__ == "__main__":
    unittest.main()


class TestSourcePills(unittest.TestCase):
    """ChatGPT's source pills, when a reply is read before they are marked up."""

    def test_the_replies_read_live(self):
        read = ("The tallest mountain in Africa is Mount Kilimanjaro. \nE\neducation.nationalgeographic.org\n+1\n\n"
                "For comparison, Mount Kenya is 5,199 meters tall. \nE\nen.wikipedia.org\n\nE\nE\nSources")
        self.assertEqual(ask_task.strip_source_pills(read),
                         "The tallest mountain in Africa is Mount Kilimanjaro. \n\nFor comparison, Mount Kenya is 5,199 meters tall.")
        read = "It is in Ethiopia. \nW\nWikipedia\n+1\n\nCoordinates: 9.0 N \nM\nmaps.apple.com\n\nW\nM\nSources"
        self.assertEqual(ask_task.strip_source_pills(read), "It is in Ethiopia. \n\nCoordinates: 9.0 N")

    def test_an_answer_of_one_letter_is_kept(self):
        self.assertEqual(ask_task.strip_source_pills("B"), "B")
        self.assertEqual(ask_task.strip_source_pills("The answer is:\nB\nbecause 2+2 is 4."), "The answer is:\nB\nbecause 2+2 is 4.")


class TestRepeatedFailure(unittest.TestCase):
    """The Ollama loop's nudge for a command run twice."""

    def test_a_failed_repeat_does_not_invite_an_answer(self):
        from decurix_agent import task_runner
        nudge = task_runner.repeated_command_nudge("python3 -c 'import requests'", {"exit_code": 1, "output": "ModuleNotFoundError"})
        self.assertIn("do not answer\nfrom memory", nudge)
        self.assertNotIn("reply now with DONE", nudge)
        ok = task_runner.repeated_command_nudge("uname -r", {"exit_code": 0, "output": "6.6.87"})
        self.assertIn("reply now with DONE", ok)
