import os
import tempfile
import unittest
from unittest import mock

from decurix_agent.computer import demonstration

PATH = "site x > button Proof of identity"


def control(value="No file chosen"):
    return {"role": "button", "name": "Proof of identity", "path": PATH, "value": value, "source": "cdp",
            "handle": {"input_type": "file"}}


def item(kind, element, **action):
    return {"action": dict(action, kind=kind, t=action.get("t", 0)), "element": element, "note": None, "located": None, "frame": None}


def look(title, value="No file chosen", extra=()):
    return {"active_title": title, "elements": [control(value)] + list(extra)}


class APersonsFileChoiceIsAnUpload(unittest.TestCase):
    def setUp(self):
        self.home = tempfile.mkdtemp()
        self.folder = os.path.join(self.home, "work", "inbox", "up-1")
        os.makedirs(self.folder)
        self.file = os.path.join(self.folder, "id_delgado.png")
        open(self.file, "wb").close()
        self.recorder = object.__new__(demonstration.Recorder)
        patcher = mock.patch.object(demonstration, "_chooser_appears", lambda wait=None: False)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_a_click_on_the_control_and_a_path_typed_in_the_chooser_are_one_upload(self):
        opened = self.recorder._file_chooser([item("click", control(), t=1.0)], look("Open File"))
        self.assertEqual(opened, [])  # held while the chooser is open
        typed = self.recorder._file_chooser([item("key", None, keys=["ctrl", "l"]), item("type", None, text=self.file)],
                                            look("Open File"))
        self.assertEqual(typed, [])
        closed = self.recorder._file_chooser([item("key", None, keys=["alt", "o"])],
                                             look("Onboarding - Chromium", "id_delgado.png"))
        self.assertEqual([i["action"]["kind"] for i in closed], ["upload"])
        self.assertEqual(closed[0]["action"]["files"], [self.file])
        self.assertEqual(closed[0]["action"]["t"], 1.0)
        self.assertEqual(closed[0]["element"]["path"], PATH)

    def test_a_folder_typed_and_a_name_clicked_in_its_list_give_the_path(self):
        self.recorder._file_chooser([item("click", control())], look("Open File"))
        self.recorder._file_chooser([item("type", None, text=self.folder + "/")], look("Open File"))
        closed = self.recorder._file_chooser([item("click", {"role": "label", "name": "id_delgado.png", "source": "atspi"})],
                                             look("Page - Chromium", "id_delgado.png"))
        self.assertEqual(closed[0]["action"]["files"], [self.file])

    def test_a_name_alone_is_found_where_a_person_keeps_files(self):
        found = demonstration._chosen_paths("id_delgado.png", [], [], home=self.home)
        self.assertEqual(found, [self.file])
        self.assertEqual(demonstration._chosen_paths("nowhere.pdf", [], [], home=self.home), ["nowhere.pdf"])

    def test_a_chooser_closed_with_nothing_chosen_records_nothing_of_it(self):
        self.recorder._file_chooser([item("click", control())], look("Open File"))
        nxt = item("click", {"role": "button", "name": "Next", "path": "site x > button Next", "source": "cdp"})
        closed = self.recorder._file_chooser([item("key", None, keys=["Escape"]), nxt], look("Page - Chromium"))
        self.assertEqual([i["element"]["name"] for i in closed if i["element"]], ["Next"])

    def test_page_actions_after_the_chooser_closed_are_kept_after_the_upload(self):
        self.recorder._file_chooser([item("click", control())], look("Open File"))
        nxt = item("click", {"role": "button", "name": "Next", "path": "site x > button Next", "source": "cdp"})
        closed = self.recorder._file_chooser([item("type", None, text=self.file), item("key", None, keys=["alt", "o"]), nxt],
                                             look("Page - Chromium", "id_delgado.png"))
        self.assertEqual([i["action"]["kind"] for i in closed], ["upload", "click"])

    def test_other_clicks_are_left_alone(self):
        other = item("click", {"role": "button", "name": "Next", "path": "x", "source": "cdp", "handle": {}})
        self.assertEqual(self.recorder._file_chooser([other], look("Page - Chromium")), [other])

    def test_a_chooser_that_comes_up_after_the_look_is_waited_for(self):
        with mock.patch.object(demonstration, "_chooser_appears", lambda wait=None: True):
            opened = self.recorder._file_chooser([item("click", control())], look("Page - Chromium"))
        self.assertEqual(opened, [])
        closed = self.recorder._file_chooser([item("type", None, text=self.file), item("key", None, keys=["alt", "o"])],
                                             look("Page - Chromium", "id_delgado.png"))
        self.assertEqual([i["action"]["kind"] for i in closed], ["upload"])

    def test_the_key_that_closed_the_chooser_is_part_of_it(self):
        self.recorder._file_chooser([item("click", control())], look("Open File"))
        closed = self.recorder._file_chooser([item("type", None, text=self.file), item("key", control("id_delgado.png"), keys=["alt", "o"])],
                                             look("Page - Chromium", "id_delgado.png"))
        self.assertEqual([i["action"]["kind"] for i in closed], ["upload"])

    def test_the_entry_carries_the_files(self):
        entry = self.recorder._entry({"kind": "upload", "files": [self.file], "t": 0, "t_end": 0.1}, control(), None, None)
        self.assertEqual(entry["files"], [self.file])
        self.assertIn("id_delgado.png", entry["summary"])


if __name__ == "__main__":
    unittest.main()
