"""
Small bounds and exact matches in the computer layer (review 2026-11-03, tier 3):

- E-47: a site is "known" only when a named host is it or a domain it is under, on label boundaries.
- D-33: an event nobody waits for is kept only so many, and an impossible frame length is never read.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import approval, cdp
from decurix_agent.computer.errors import ComputerError


class AKnownSiteIsTheSameHost(unittest.TestCase):
    def known(self, host, request):
        with mock.patch("decurix_agent.task_context.current_request", return_value=request), \
                mock.patch.object(approval, "_taught_text", return_value=""):
            return approval.site_known(host)

    def test_a_host_inside_a_named_hosts_name_is_not_known(self):
        self.assertFalse(self.known("ample.com", "log in at example.com"))
        self.assertFalse(self.known("example.com.other.test", "log in at example.com"))

    def test_the_named_host_and_its_subdomains_are_known(self):
        self.assertTrue(self.known("example.com", "log in at example.com"))
        self.assertTrue(self.known("www.example.com", "log in at https://example.com/"))


class BoundedBrowserReads(unittest.TestCase):
    def test_unawaited_events_are_bounded(self):
        page = cdp.Page.__new__(cdp.Page)
        page.seq, page.events = 0, []
        answers = [{"method": "Network.x"}] * (cdp.MAX_EVENTS + 50) + [{"id": 1, "result": {}}]
        page.ws = mock.Mock()
        page.ws.receive.side_effect = [__import__("json").dumps(a) for a in answers]
        page.call("Runtime.evaluate")
        self.assertEqual(len(page.events), cdp.MAX_EVENTS)

    def test_an_impossible_frame_length_is_refused(self):
        sock = cdp._Socket.__new__(cdp._Socket)
        header = bytes([0x81, 127]) + (cdp.MAX_FRAME_BYTES + 1).to_bytes(8, "big")
        sock._need = mock.Mock(side_effect=[header[:2], header[2:]])
        with self.assertRaises(ComputerError):
            sock.receive()


if __name__ == "__main__":
    unittest.main()
