import unittest
from unittest import mock

from decurix_agent import study
from decurix_agent.computer import approval, cdp
from decurix_agent.computer.errors import ComputerError


# ============================================================
# A page that keeps its app in an inner frame (LinkedIn's Messaging,
# 2026-10-03): a look listed 2 elements of a page showing a search box, the
# conversations, a message box and Send; what a person typed there was
# recorded as typed into nothing; and Enter in its search box was stopped as
# "sends a message".
# ============================================================

METRICS = ('{"sx": 0, "sy": 0, "ow": 1000, "oh": 800, "iw": 1000, "ih": 800, "scx": 0, "scy": 0, '
           '"dpr": 1, "title": "Messaging | LinkedIn", "url": "https://www.linkedin.com/messaging/thread/1/"}')


class FramedPage:
    """A tab whose own document is a navigation bar and a frame; the frame holds the app."""

    target = {"id": "T", "title": "Messaging | LinkedIn"}

    def __init__(self, loader="L1", frame_box=(0, 100, 1000, 700), frame_scroll=(0, 0), frame_tree=True):
        self.loader = loader
        self.frame_box = frame_box
        self.frame_scroll = frame_scroll
        self.frame_tree = frame_tree
        self.closed = False

    def evaluate(self, expression):
        if expression == cdp._READY:
            return "complete"
        if expression == cdp._METRICS:
            return METRICS
        return "[]"

    def call(self, method, params=None, timeout=None):
        params = params or {}

        if method == "Page.getFrameTree":
            if not self.frame_tree:
                raise ComputerError("no frame tree")
            return {"frameTree": {
                "frame": {"id": "MAIN", "loaderId": self.loader},
                "childFrames": [
                    {"frame": {"id": "APP"}},
                    {"frame": {"id": "HIDDEN"}, "childFrames": [{"frame": {"id": "INSIDE-HIDDEN"}}]},
                ],
            }}

        if method == "DOM.getFrameOwner":
            return {"backendNodeId": {"APP": 20, "HIDDEN": 21}[params["frameId"]]}

        if method == "DOM.getContentQuads":
            if params["backendNodeId"] == 21:
                return {"quads": [[0, 0, 0, 0, 0, 0, 0, 0]]}  # a tracker's frame: no size
            x, y, w, h = self.frame_box
            return {"quads": [[x, y, x + w, y, x + w, y + h, x, y + h]]}

        if method == "Accessibility.getFullAXTree":
            if params.get("frameId") == "APP":
                # Node ids start again in a frame's own tree.
                return {"nodes": [
                    {"nodeId": "1", "role": {"value": "RootWebArea"}, "childIds": ["2", "3", "4"], "backendDOMNodeId": 100},
                    {"nodeId": "2", "parentId": "1", "role": {"value": "textbox"}, "name": {"value": "Search messages"},
                     "childIds": [], "backendDOMNodeId": 101,
                     "properties": [{"name": "focused", "value": {"value": True}}]},
                    {"nodeId": "3", "parentId": "1", "role": {"value": "textbox"}, "name": {"value": "Write a message…"},
                     "childIds": [], "backendDOMNodeId": 102},
                    {"nodeId": "4", "parentId": "1", "role": {"value": "button"}, "name": {"value": "Send"},
                     "childIds": [], "backendDOMNodeId": 103},
                ]}
            if params.get("frameId"):
                raise AssertionError("a frame nobody sees was read")
            return {"nodes": [
                {"nodeId": "1", "role": {"value": "RootWebArea"}, "childIds": ["2", "3"], "backendDOMNodeId": 1},
                {"nodeId": "2", "parentId": "1", "role": {"value": "combobox"}, "name": {"value": "Search"},
                 "childIds": [], "backendDOMNodeId": 10},
                {"nodeId": "3", "parentId": "1", "role": {"value": "Iframe"}, "childIds": [], "backendDOMNodeId": 20},
            ]}

        if method == "DOMSnapshot.captureSnapshot":
            return {"strings": ["MAIN", "APP", "HIDDEN"], "documents": [
                {"frameId": 0,
                 "nodes": {"backendNodeId": [1, 10, 20, 21], "attributes": [[], [], [], []]},
                 "layout": {"nodeIndex": [0, 1, 2], "bounds": [[0, 0, 1000, 800], [300, 20, 350, 40], [0, 100, 1000, 700]]}},
                {"frameId": 1, "scrollOffsetX": self.frame_scroll[0], "scrollOffsetY": self.frame_scroll[1],
                 "nodes": {"backendNodeId": [100, 101, 102, 103], "attributes": [[], [], [], []]},
                 "layout": {"nodeIndex": [0, 1, 2, 3],
                            "bounds": [[0, 0, 1000, 700], [380, 110, 270, 40], [650, 650, 500, 40], [870, 820, 70, 30]]}},
                {"frameId": 2,
                 "nodes": {"backendNodeId": [200], "attributes": [[]]},
                 "layout": {"nodeIndex": [0], "bounds": [[0, 0, 10, 10]]}},
            ]}

        return {}

    def close(self):
        self.closed = True


def look(page):
    with mock.patch.object(cdp, "active_page", return_value=page), \
            mock.patch.object(cdp, "_viewport", return_value=(0, 80, 1)):
        return cdp.observe()


class TestALookReadsInnerFrames(unittest.TestCase):

    def test_what_a_frame_shows_is_listed_where_it_is_drawn(self):
        elements, group = look(FramedPage())
        by_name = {e["name"]: e for e in elements}

        self.assertIn("Search", by_name)  # the page's own navigation bar
        self.assertIn("Search messages", by_name)
        self.assertIn("Write a message…", by_name)
        self.assertEqual(group["window_title"], "Messaging | LinkedIn - Chromium")

        # The frame is drawn 100 px down the page, the page 80 px down the screen.
        self.assertEqual(by_name["Search messages"]["box"], [380, 80 + 100 + 110, 270, 40])
        self.assertEqual(by_name["Search messages"]["role"], "textbox")
        self.assertIn("focused", by_name["Search messages"]["states"])
        self.assertEqual(by_name["Search messages"]["handle"]["backend_id"], 101)
        self.assertEqual(by_name["Search"]["box"], [300, 100, 350, 40])

    def test_what_is_scrolled_out_of_the_frame_is_marked_offscreen(self):
        elements, _ = look(FramedPage())
        by_name = {e["name"]: e for e in elements}

        # Send is at y 820 of a frame showing 700 px of its document.
        self.assertIn("offscreen", by_name["Send"]["states"])
        self.assertNotIn("offscreen", by_name["Search messages"]["states"])

    def test_a_frames_own_scroll_position_moves_what_is_in_it(self):
        elements, _ = look(FramedPage(frame_scroll=(0, 150)))
        by_name = {e["name"]: e for e in elements}

        self.assertEqual(by_name["Send"]["box"], [870, 80 + 100 + 820 - 150, 70, 30])
        self.assertNotIn("offscreen", by_name["Send"]["states"])
        # ...and the search box is now above the frame's top edge.
        self.assertIn("offscreen", by_name["Search messages"]["states"])

    def test_a_frame_with_no_size_and_what_is_inside_it_are_not_read(self):
        page = FramedPage()
        frames = cdp._frames(page)

        self.assertEqual([f["id"] for f in frames], ["APP"])

    def test_a_browser_that_gives_no_frame_tree_is_read_as_before(self):
        elements, _ = look(FramedPage(frame_tree=False))

        self.assertEqual([e["name"] for e in elements], ["Search"])

    def test_node_ids_of_a_frame_do_not_collide_with_the_pages(self):
        # Both trees number their nodes from 1: the frame's "2" (Search
        # messages) must not replace the page's "2" (Search).
        elements, _ = look(FramedPage())

        self.assertEqual(sorted(e["name"] for e in elements if e["role"] in ("textbox", "combobox")),
                         ["Search", "Search messages", "Write a message…"])


class TestAnElementOfAPageThatIsGone(unittest.TestCase):

    def test_a_look_at_another_document_is_not_taken_for_this_one(self):
        # Found live: a click on LinkedIn in Google's results was recorded as
        # a click on the new tab page's "Ask Google" box - the result's node
        # had the number that box had had on the page before.
        elements, _ = look(FramedPage(loader="L1"))

        same = cdp._known_by_backend(elements, FramedPage(loader="L1"))
        gone = cdp._known_by_backend(elements, FramedPage(loader="L2"))

        self.assertIn(101, same)
        self.assertEqual(gone, {})

    def test_elements_from_before_loaders_were_kept_are_still_known(self):
        elements, _ = look(FramedPage(loader="L1"))
        for element in elements:
            element["handle"].pop("loader")

        self.assertIn(101, cdp._known_by_backend(elements, FramedPage(loader="L2")))


class TestEnterInASearchBox(unittest.TestCase):

    WHERE = {"window": "Messaging | LinkedIn - Chromium", "url": "https://www.linkedin.com/messaging/thread/1/"}

    def field(self, name, role="textbox", **handle):
        return {"role": role, "name": name, "handle": handle}

    def test_enter_in_the_search_box_of_a_messaging_page_searches(self):
        context = dict(self.WHERE, focused=self.field("Search messages"))

        self.assertIsNone(approval.assess("key", None, keys=["Return"], context=context))
        # ...and as the element of a recorded step.
        self.assertIsNone(approval.assess("key", self.field("Search messages"), keys=["Return"], context=self.WHERE))

    def test_enter_in_the_message_box_still_stops(self):
        context = dict(self.WHERE, focused=self.field("Write a message…"))

        self.assertIn("sends", approval.assess("key", None, keys=["Return"], context=context))

    def test_enter_with_the_keyboard_nobody_knows_where_still_stops(self):
        self.assertIn("sends", approval.assess("key", None, keys=["Return"], context=self.WHERE))
        self.assertIn("sends", approval.assess("key", None, keys=["Return"], context=dict(self.WHERE, focused=None)))

    def test_a_field_is_a_search_field_by_its_markup_too(self):
        self.assertTrue(approval.searches(self.field("", role="searchbox")))
        self.assertTrue(approval.searches(self.field("People", input_type="search")))
        self.assertTrue(approval.searches(self.field("Find a conversation", role="combobox")))

    def test_a_field_that_only_mentions_searching_is_not_one(self):
        # "Message the search team" is a message box.
        self.assertFalse(approval.searches(self.field("Message the search team")))
        self.assertFalse(approval.searches(self.field("Search results", role="button")))
        self.assertFalse(approval.searches(None))

    def test_send_is_stopped_whatever_has_the_keyboard(self):
        send = {"role": "button", "name": "Send", "handle": {}}
        context = dict(self.WHERE, focused=self.field("Search messages"))

        self.assertIn("sends", approval.assess("click", send, context=context))


class TestTheLessonSeesEveryPress(unittest.TestCase):
    """study._pictures: said by the person - "some of the clicks I do you don't see where I clicked or why"."""

    def steps(self):
        return [
            {"n": 1, "kind": "open", "summary": "open browser", "window": "about:blank - Chromium"},
            {"n": 2, "kind": "click", "summary": "click 368,19", "point": [368, 19], "window": "about:blank - Chromium"},
            {"n": 3, "kind": "click", "summary": 'click textbox "Search messages"', "point": [475, 235],
             "target": {"role": "textbox", "name": "Search messages"}, "window": "Messaging"},
            {"n": 4, "kind": "type", "summary": "type {name}", "window": "Messaging"},
            {"n": 5, "kind": "key", "summary": "press Return", "window": "Messaging"},
            {"n": 6, "kind": "click", "summary": 'click text "This message has been deleted."',
             "target": {"role": "text", "name": "This message has been deleted."},
             "locate": {"box": [340, 308, 176, 22]}, "window": "Messaging"},
            {"n": 7, "kind": "click", "summary": 'click button "Send"', "point": [1123, 941],
             "target": {"role": "button", "name": "Send"}, "window": "Messaging"},
        ]

    def shown(self, steps, budget=None):
        marked = []

        def mark(png, point):
            marked.append(list(point))
            return png

        patches = [mock.patch.object(study.evidence, "step_frame", lambda step, which="before": b"png"),
                   mock.patch.object(study.evidence, "mark", mark),
                   mock.patch.object(study.llm_client, "image_part", lambda png: {"type": "image_url"})]
        if budget:
            patches.append(mock.patch.object(study, "MAX_PICTURES", budget))
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

        parts = study._pictures({"steps": steps})
        return [p["text"] for p in parts if p["type"] == "text"], marked

    def test_every_press_is_shown_with_the_spot_marked(self):
        captions, marked = self.shown(self.steps())

        for n in (2, 3, 6, 7):
            self.assertTrue(any(c.startswith(f"Screen before step {n} ") and "red ring" in c for c in captions), captions)
        # A press on a control with no point of its own is ringed at the control's middle.
        self.assertEqual(marked, [[368, 19], [475, 235], [428, 319], [1123, 941]])
        self.assertTrue(captions[0].startswith("Screen before step 1 "))
        self.assertTrue(captions[-1].startswith("Screen after the last step (7)"))

    def test_typing_and_keys_are_not_pictured_for_themselves(self):
        captions, _ = self.shown(self.steps())

        self.assertFalse(any(c.startswith(("Screen before step 4 ", "Screen before step 5 ")) for c in captions))

    def test_with_more_presses_than_pictures_the_unnamed_ones_are_kept(self):
        captions, _ = self.shown(self.steps(), budget=4)

        # 3 "before" pictures: the start, the press with no named control, and
        # the first press in a new window - then the end.
        self.assertEqual([c.split(" (")[0] for c in captions[:-1]],
                         ["Screen before step 1", "Screen before step 2", "Screen before step 3"])
        self.assertEqual(len(captions), 4)

    def test_the_caption_says_what_the_step_list_calls_the_press(self):
        captions, _ = self.shown(self.steps())

        self.assertIn('click text "This message has been deleted."', " ".join(captions))


class TestAPressOnWordsInsideAPlainBlock(unittest.TestCase):
    """cdp._describe: a <span> holding only its words, of which the accessibility tree lists no child."""

    class Page:
        target = {"id": "T"}

        def call(self, method, params=None):
            if method == "Accessibility.getPartialAXTree":
                # As measured in Chrome: the pressed node (ignored) and what is
                # above it, none of it named - and not the text inside it.
                return {"nodes": [
                    {"nodeId": "117", "backendDOMNodeId": 117, "ignored": True, "role": {"value": "none"},
                     "parentId": "118", "childIds": ["120"]},
                    {"nodeId": "118", "backendDOMNodeId": 118, "role": {"value": "generic"}, "name": {"value": ""},
                     "childIds": ["117"]},
                ]}
            if method == "DOM.describeNode" and (params or {}).get("depth") == 1:
                return {"node": {"nodeName": "SPAN", "children": [
                    {"nodeType": 3, "backendNodeId": 120, "nodeValue": "  This message has\n been deleted. "}]}}
            if method == "DOM.describeNode":
                return {"node": {"nodeName": "SPAN", "attributes": []}}
            if method == "DOM.getContentQuads":
                return {"quads": [[10, 20, 110, 20, 110, 40, 10, 40]]}
            raise ComputerError(method)

    METRICS = {"url": "https://www.linkedin.com/messaging/", "title": "Messaging", "scx": 0, "scy": 0}

    def test_it_is_the_words_it_holds(self):
        with mock.patch.object(cdp, "_viewport", return_value=(0, 100, 1.0)):
            found = cdp._describe(self.Page(), self.METRICS, 117, {})

        self.assertEqual((found["role"], found["name"]), ("text", "This message has been deleted."))
        self.assertEqual(found["box"], [10, 120, 100, 20])

    def test_words_a_look_listed_are_returned_as_listed(self):
        listed = {"role": "text", "name": "This message has been deleted.", "path": "site x > list > text", "handle": {}}

        found = cdp._describe(self.Page(), self.METRICS, 117, {120: listed})

        self.assertEqual(found["path"], "site x > list > text")

    def test_words_are_not_a_control(self):
        self.assertIsNone(cdp._describe(self.Page(), self.METRICS, 117, {}, controls_only=True))


class TestANoAtAnApprovalEndsTheTask(unittest.TestCase):
    """
    Found live: "no, do not send it" at Send, and the final check - seeing the
    message unsent - sent the worker back to do it all again and ask again.
    """

    def test_what_counts_as_saying_no(self):
        for reply in ("no", "No, do not send it", "don't", "stop", "cancel that"):
            self.assertTrue(approval.is_refusal(reply), reply)
        for reply in ("yes", "ok", "no, send it to Priya instead", "not yet - use the other account", "", "nothing wrong, go"):
            self.assertFalse(approval.is_refusal(reply), reply)

    def test_a_refusal_is_kept_for_the_task_it_was_said_in(self):
        from decurix_agent import guidance

        with mock.patch.object(guidance.task_context, "current", return_value="t1"):
            guidance.note_reply("no, do not send it", "declined")
            self.assertEqual(guidance.refused(), "no, do not send it")
        with mock.patch.object(guidance.task_context, "current", return_value="t2"):
            self.assertIsNone(guidance.refused())

    def test_an_instruction_or_a_later_yes_is_not_a_refusal(self):
        from decurix_agent import guidance

        with mock.patch.object(guidance.task_context, "current", return_value="t1"):
            guidance.note_reply("no, send it to Priya instead", "declined")
            self.assertIsNone(guidance.refused())
            guidance.note_reply("no", "declined")
            guidance.note_reply("yes", "granted")
            self.assertIsNone(guidance.refused())

    def test_the_final_check_reports_the_stop_and_does_not_try_again(self):
        from decurix_agent import guidance, skill_task

        sent = []
        guidance.begin(skill=None, task="send it", text="", mode="engine", run=None)
        with mock.patch.object(skill_task.guidance, "refused", return_value="no, do not send it"),                 mock.patch.object(skill_task, "write_final_reply", lambda text, **kw: sent.append((text, kw))),                 mock.patch.object(skill_task.verify, "judge", side_effect=AssertionError("the screen was judged")),                 mock.patch.object(skill_task, "_retry_engine", side_effect=AssertionError("it tried again")),                 mock.patch.object(skill_task.progress, "emit"):
            took_over = skill_task._gate("I did not send the message.", "final", "thebot")

        self.assertTrue(took_over)
        self.assertEqual(len(sent), 1)
        self.assertIn("Stopped there, as you said", sent[0][0])
        self.assertIn("I did not send the message.", sent[0][0])
        self.assertEqual(sent[0][1]["kind"], "final")
        self.assertIsNone(guidance.get())


if __name__ == "__main__":
    unittest.main()
