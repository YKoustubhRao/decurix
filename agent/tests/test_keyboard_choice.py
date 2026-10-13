import unittest

from decurix_agent.computer import demonstration


def field(path, editable, value=""):
    return {"role": "combobox", "name": path, "path": path, "value": value, "source": "cdp",
            "handle": {"editable": editable}}


def item(kind, element, **action):
    return {"action": dict(action, kind=kind, t=action.get("t", 0)), "element": element, "note": None, "located": None, "frame": None}


class AChoiceMadeWithTheKeyboardIsAChoice(unittest.TestCase):
    def test_open_type_enter_in_a_drop_down_is_one_select_of_the_option_it_shows(self):
        product = field("site x > combobox Product line 1", editable=False)
        after = {"elements": [field("site x > combobox Product line 1", False, "MUG-12 - Ceramic mug 12oz")]}
        ready = [item("click", product, t=1.0), item("type", product, text="MUG", t=2.0), item("key", product, keys=["Return"], t=2.5)]
        out = demonstration._keyboard_choices(ready, after)
        self.assertEqual([i["action"]["kind"] for i in out], ["select"])
        self.assertEqual(out[0]["action"]["option"], "MUG-12 - Ceramic mug 12oz")
        self.assertNotIn("text", out[0]["action"])
        self.assertEqual(out[0]["action"]["t"], 1.0)

    def test_typing_into_a_field_that_takes_text_is_still_typing(self):
        customer = field("site x > combobox Customer", editable=True)
        after = {"elements": [field("site x > combobox Customer", True, "Caf")]}
        ready = [item("click", customer), item("type", customer, text="Caf")]
        self.assertEqual([i["action"]["kind"] for i in demonstration._keyboard_choices(ready, after)], ["click", "type"])


    def test_keys_typed_into_a_date_field_are_kept_as_the_date_it_then_holds(self):
        date = {"role": "textbox", "name": "Order date", "path": "site x > textbox Order date", "value": "", "source": "cdp",
                "states": ["date"], "handle": {"editable": True}}
        after = {"elements": [dict(date, value="2026-10-06")]}
        out = demonstration._dates_entered([item("type", date, text="10062026")], after)
        self.assertEqual(out[0]["action"]["text"], "2026-10-06")


if __name__ == "__main__":
    unittest.main()
