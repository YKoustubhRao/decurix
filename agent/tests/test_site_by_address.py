"""
A site the request names only by its address (an IP literal) counts as named when the request writes it as a URL's host
(approval._names_host; relayed from the EVAL lane, H6.2, 2026-11-04). Before, only host names with a TLD could be named,
so a password typed into a page at an address was always asked, whatever the request said. An address mentioned as
data names no site, and an address matches only exactly.
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent.computer import approval


class ASiteByItsAddress(unittest.TestCase):
    def test_a_url_with_an_address_names_it(self):
        text = "sign in at http://192.168.1.20:8080/login as mira.k"
        self.assertTrue(approval._names_host(text, "192.168.1.20"))
        self.assertTrue(approval._names_host("open https://10.0.0.5/ and log in", "10.0.0.5"))
        self.assertTrue(approval._names_host("the page http://[::1]:8000/x", "::1"))
        self.assertTrue(approval._names_host("the page http://[::1]:8000/x", "[::1]"))

    def test_only_exactly(self):
        text = "sign in at http://192.168.1.20:8080/login"
        for other in ("192.168.1.2", "192.168.1.200", "92.168.1.20", "10.0.0.5"):
            self.assertFalse(approval._names_host(text, other), other)

    def test_an_address_mentioned_as_data_names_no_site(self):
        self.assertFalse(approval._names_host("the server 10.0.0.5 keeps the logs", "10.0.0.5"))
        self.assertFalse(approval._names_host("ip=10.0.0.5", "10.0.0.5"))

    def test_names_are_as_before(self):
        self.assertTrue(approval._names_host("go to example.com", "www.example.com"))
        self.assertFalse(approval._names_host("go to example.com", "example.com.evil.test"))
        self.assertFalse(approval._names_host("go to example.com", "10.0.0.5"))


if __name__ == "__main__":
    unittest.main()
