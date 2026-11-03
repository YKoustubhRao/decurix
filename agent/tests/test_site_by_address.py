"""
A site the request names only by its address (an IP literal) counts as named when the request writes it as a URL, and
then only that endpoint: its scheme, address and port, all three, the scheme's default port when none is written
(approval._names_host; EVAL relay H6.2, and review round 2, B10: naming one port of 127.0.0.1 named every listener on
it). An address mentioned as data names no site; text before an "@" is never the host.
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent.computer import approval

TEXT = "sign in at http://192.168.1.20:8080/login as mira.k"


def named(text, url):
    return approval._names_host(text.lower(), approval._host(url), url)


class ASiteByItsAddress(unittest.TestCase):
    def test_the_endpoint_the_url_writes(self):
        self.assertTrue(named(TEXT, "http://192.168.1.20:8080/login"))
        self.assertTrue(named(TEXT, "http://192.168.1.20:8080/other/page"))
        self.assertTrue(named("open https://10.0.0.5/ and log in", "https://10.0.0.5:443/x"))  # the default port
        self.assertTrue(named("the page http://[::1]:8000/x", "http://[::1]:8000/y"))

    def test_another_port_scheme_or_address_is_not_named(self):
        self.assertFalse(named(TEXT, "http://192.168.1.20:9090/login"))     # another listener on the same address
        self.assertFalse(named(TEXT, "https://192.168.1.20:8080/login"))    # another scheme
        self.assertFalse(named(TEXT, "http://192.168.1.2:8080/login"))
        self.assertFalse(named("go to http://127.0.0.1:8000/", "http://127.0.0.1:4900/"))  # loopback: only that port
        self.assertFalse(named("open https://10.0.0.5/", "http://10.0.0.5/"))

    def test_an_address_mentioned_as_data_names_no_site(self):
        self.assertFalse(named("the server 10.0.0.5 keeps the logs", "http://10.0.0.5/"))
        self.assertFalse(named("ip=10.0.0.5", "http://10.0.0.5/"))

    def test_text_before_an_at_sign_is_not_the_host(self):
        self.assertFalse(named("see http://10.0.0.5:80@portal.example.test/", "http://10.0.0.5/"))

    def test_the_site_of_an_address_is_its_endpoint(self):
        self.assertEqual(approval._host("http://127.0.0.1:9999/form"), "http://127.0.0.1:9999")
        self.assertEqual(approval._host("https://10.0.0.5/"), "https://10.0.0.5:443")
        self.assertEqual(approval._host("https://Portal.Example.test/x"), "portal.example.test")

    def test_names_are_as_before(self):
        self.assertTrue(approval._names_host("go to example.com", "www.example.com"))
        self.assertFalse(approval._names_host("go to example.com", "example.com.evil.test"))
        self.assertFalse(approval._names_host("go to example.com", "10.0.0.5"))


if __name__ == "__main__":
    unittest.main()
