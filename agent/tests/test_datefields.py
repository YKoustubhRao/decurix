import unittest

from decurix_agent.computer import datefields
from decurix_agent.computer.errors import ComputerError


class ADateIsPutInOnlyWhenItIsCertain(unittest.TestCase):
    def test_every_unambiguous_way_of_writing_a_date_gives_the_fields_own_format(self):
        for text in ("2026-10-06", "2026/10/6", "6 Oct 2026", "06-Oct-2026", "6th October 2026", "October 6, 2026",
                     "Oct 6 2026", "6/10/2026" if False else "16/10/2026".replace("16", "06").replace("06/10", "06 October")):
            self.assertEqual(datefields.value_for("date", text), "2026-10-06", text)
        self.assertEqual(datefields.value_for("date", "16/10/2026"), "2026-10-16")
        self.assertEqual(datefields.value_for("date", "10/16/2026"), "2026-10-16")
        self.assertEqual(datefields.value_for("datetime-local", "2026-10-06 2:30 pm"), "2026-10-06T14:30")
        self.assertEqual(datefields.value_for("time", "09:05"), "09:05")
        self.assertEqual(datefields.value_for("month", "October 2026"), "2026-10")
        self.assertEqual(datefields.value_for("date", ""), "")

    def test_a_date_that_reads_two_ways_or_none_is_refused_with_the_way_to_write_it(self):
        with self.assertRaises(ComputerError) as said:
            datefields.value_for("date", "06/10/2026")
        self.assertIn("YYYY-MM-DD", str(said.exception))
        for text in ("2026-02-30", "next Friday", "2026-10-06 14:00"):
            with self.assertRaises(ComputerError):
                datefields.value_for("date", text)
        with self.assertRaises(ComputerError):
            datefields.value_for("datetime-local", "2026-10-06")


if __name__ == "__main__":
    unittest.main()
