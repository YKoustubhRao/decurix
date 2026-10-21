import unittest

from decurix_agent.computer import approval, core, healing, learning, replay


def element(role, name, ref="e1", value="", group="g"):
    return {"role": role, "name": name, "ref": ref, "value": value, "states": [], "group": group,
            "path": f'site web.whatsapp.com > {role} "{name}"'}


def target(role, name):
    return {"role": role, "name": name, "path": f'site web.whatsapp.com > {role} "{name}"'}


# The skill taught on 2026-10-16: search a chat, pick the result, type, send.
STEPS = [
    {"n": 1, "kind": "click", "target": target("textbox", "Search or start a new chat"), "expect": []},
    {"n": 2, "kind": "type", "param": "contact", "target": target("textbox", "Search or start a new chat"),
     "expect": []},
    {"n": 3, "kind": "click", "target": target("row", "10/9/2026"), "point": [227, 277], "expect": []},
    {"n": 4, "kind": "click", "target": target("textbox", "Type a message to Koustubh OG"), "expect": []},
    {"n": 5, "kind": "type", "param": "message", "target": target("textbox", "Type a message to Koustubh OG"),
     "expect": [{"type": "element", "role": "button", "name": "Context menu for message from {contact}"}]},
    {"n": 6, "kind": "click", "target": target("button", "Send"), "consequential": True, "expect": []},
]
PARAMS = [{"name": "contact", "example": "Koustubh OG", "step": 2},
          {"name": "message", "example": "hello man", "step": 5}]

# Searching "Happy Family" with the previous chat still open behind the results.
SCREEN = {"elements": [
    element("textbox", "Search or start a new chat", "e1"),
    element("row", "Chats", "e2"),
    element("row", "Happy family 9/14/2026", "e3"),
    element("textbox", "Type a message to Koustubh OG", "e4"),
], "windows": [], "active_title": "web.whatsapp.com"}


class FollowValues(unittest.TestCase):

    def followed(self, contact="Happy Family"):
        return replay.follow_values(STEPS, PARAMS, {"contact": contact, "message": "hello hello, test"})

    def test_the_message_box_named_for_the_demonstration_is_never_this_runs(self):
        step = self.followed()[4]
        self.assertEqual(step["target"]["name"], "Type a message to Happy Family")
        found, _, why = replay.resolve(step, SCREEN)
        self.assertIsNone(found, "the previous chat's box must not be taken")
        self.assertIn("not on the screen", why)

    def test_it_is_found_once_the_right_chat_is_open(self):
        step = self.followed()[4]
        screen = {"elements": [element("textbox", "Type a message to group Happy family ❤️🤍", "e9")],
                  "windows": [], "active_title": ""}
        found, _, _ = replay.resolve(step, screen)
        self.assertEqual(found["ref"], "e9")

    def test_healing_never_offers_the_demonstrations_box(self):
        step = self.followed()[4]
        names = [c["element"]["name"] for c in healing.rank(step, SCREEN)]
        self.assertNotIn("Type a message to Koustubh OG", names)

    def test_a_result_named_by_a_date_is_the_result_for_what_was_typed(self):
        step = self.followed()[2]
        found, _, _ = replay.resolve(step, SCREEN)
        self.assertEqual(found["ref"], "e3")

    def test_the_same_value_keeps_the_recorded_target(self):
        steps = self.followed(contact="Koustubh OG")
        self.assertEqual(steps[4]["target"]["name"], "Type a message to Koustubh OG")
        self.assertNotIn("contains", steps[4]["target"])

    def test_typing_is_not_held_to_what_else_appeared(self):
        self.assertEqual(replay._typing_checks(STEPS[4])["expect"], [])


class Detour(unittest.TestCase):

    def test_the_choice_right_after_typing_is_never_left_out(self):
        steps = replay.follow_values(STEPS, PARAMS, {"contact": "Happy Family", "message": "x"})
        self.assertIsNotNone(replay._detour_refused(steps, 2, 4, SCREEN))

    def test_a_detour_needs_its_landing_control_on_the_screen(self):
        steps = replay.follow_values(STEPS, PARAMS, {"contact": "Happy Family", "message": "x"})
        self.assertIsNotNone(replay._detour_refused(steps, 3, 4, SCREEN))


class NamePoints(unittest.TestCase):

    def test_a_press_where_the_next_step_typed_is_on_that_field(self):
        steps = [
            {"n": 1, "id": "s1", "kind": "click", "point": [664, 709], "window": "w", "screen": [1536, 793],
             "expect": []},
            {"n": 2, "id": "s2", "kind": "type", "param": "message", "window": "w",
             "target": target("textbox", "Type a message"),
             "locate": {"box": [628, 698, 844, 22], "screen": [1536, 793]}, "expect": []},
        ]
        notes = learning._name_points(steps)
        self.assertEqual(steps[0]["target"]["name"], "Type a message")
        self.assertTrue(notes)

    def test_a_press_elsewhere_stays_a_point(self):
        steps = [
            {"n": 1, "id": "s1", "kind": "click", "point": [10, 10], "window": "w", "expect": []},
            {"n": 2, "id": "s2", "kind": "type", "param": "m", "window": "w", "target": target("textbox", "T"),
             "locate": {"box": [628, 698, 844, 22]}, "expect": []},
        ]
        learning._name_points(steps)
        self.assertNotIn("target", steps[0])


class RenameParams(unittest.TestCase):

    def test_values_are_called_what_they_are(self):
        skill = {"params": [{"name": "type_a_message_to_koustubh_og", "step": 1}],
                 "steps": [{"n": 1, "kind": "type", "param": "type_a_message_to_koustubh_og",
                            "target": target("textbox", "T"),
                            "expect": [{"type": "element", "role": "x", "name": "{type_a_message_to_koustubh_og}"}]}],
                 "understanding": {"inputs": {"type_a_message_to_koustubh_og": "the message"},
                                   "summary": "send {type_a_message_to_koustubh_og}"}}
        learning.rename_params(skill, {"type_a_message_to_koustubh_og": "message"})
        self.assertEqual(skill["params"][0]["name"], "message")
        self.assertEqual(skill["steps"][0]["param"], "message")
        self.assertEqual(skill["steps"][0]["expect"][0]["name"], "{message}")
        self.assertEqual(skill["understanding"]["summary"], "send {message}")
        self.assertIn("message", skill["understanding"]["inputs"])

    def test_a_bad_name_is_left_alone(self):
        skill = {"params": [{"name": "a_b", "step": 1}], "steps": []}
        learning.rename_params(skill, {"a_b": "Not A Name!"})
        self.assertEqual(skill["params"][0]["name"], "a_b")


class ApprovalSaysWhere(unittest.TestCase):

    def test_the_filled_fields_beside_send_are_named(self):
        screen = {"elements": [element("textbox", "Type a message to Koustubh OG", "e4", value="hello hello, test"),
                               element("button", "Send", "e5")]}
        about = core._what_it_acts_on(screen, screen["elements"][1])
        self.assertIn("Type a message to Koustubh OG", about)
        self.assertIn("hello hello, test", about)

    def test_it_is_in_the_request_text(self):
        text = approval.request_text({"action": "click button Send", "reason": "it sends",
                                      "about": "It would act on what is filled in: x"})
        self.assertIn("It would act on what is filled in: x.", text)
        self.assertIn("Nothing was clicked", text)

    def test_the_keep_name_reply_takes_perma(self):
        self.assertTrue(learning.KEEP_NAME.match("perma"))


if __name__ == "__main__":
    unittest.main()
