"""
Where the tests' fixtures made from live runs live: outside the repository (they hold customer data, demo logins and
MBs of files - never committed). DECURIX_TEST_FIXTURES names the folder; by default decurix-tests/fixtures beside the
work tree (C:\\Users\\Administrator\\decurix-tests\\fixtures, or /mnt/c/... under WSL).

A test that needs what is not on this machine (a fixture, the sandbox's namespaces) skips, naming what is missing -
and says so out loud: every such skip is counted, and the suite ends with a summary on stderr (an annotation too on
GitHub Actions). They used to skip silently, so the replay gate, the leak scan and two dozen live regressions were
vacuous everywhere the fixtures were not, with nothing saying so (review 2026-11-02).

Two switches turn the skip into a failure, for a run that must not be vacuous:
  DECURIX_REQUIRE_FIXTURES=1   a missing fixture fails its test (the owner's machine, a pre-release run)
  DECURIX_REQUIRE_SANDBOX=1    a host where the script sandbox does not work fails its test (CI sets it)
CI does not set the first: the fixtures are customer data and stay out of the repository and its CI.
"""

import atexit
import os
import sys
import unittest
from pathlib import Path


def _default():
    if sys.platform == "win32":
        return Path(r"C:\Users\Administrator\decurix-tests\fixtures")
    return Path("/mnt/c/Users/Administrator/decurix-tests/fixtures")


FIXTURES = Path(os.environ.get("DECURIX_TEST_FIXTURES") or _default())

# What was missing, by kind ("fixtures", "sandbox"): reasons, in order, for the summary at the end.
_missing = {}


def _required(kind):
    return os.environ.get("DECURIX_REQUIRE_" + kind.upper(), "").strip() not in ("", "0", "false", "no")


def missing(reason, kind="fixtures"):
    """
    The exception a test raises for what is not on this machine: SkipTest, counted for the summary - or, under
    DECURIX_REQUIRE_<KIND>=1, a failure.
    """

    _missing.setdefault(kind, []).append(reason)
    if _required(kind):
        return AssertionError(f"{reason} - and DECURIX_REQUIRE_{kind.upper()}=1 says this run must have it")
    return unittest.SkipTest(reason)


def skip_unless(condition, reason, kind="fixtures"):
    """
    unittest.skipUnless for what may not be on this machine: counted, and a failure under DECURIX_REQUIRE_<KIND>=1.
    Decides when the test runs, not when the module is imported, so a test that is never run is never counted.
    """

    def decorate(item):
        if condition:
            return item
        if isinstance(item, type):
            def setUpClass(cls):
                raise missing(reason, kind)
            item.setUpClass = classmethod(setUpClass)
            return item

        def test(*args, **kwargs):
            raise missing(reason, kind)
        test.__name__ = getattr(item, "__name__", "test")
        test.__doc__ = getattr(item, "__doc__", None)
        return test
    return decorate


def fixture(*parts):
    """The path of a fixture (whether or not it is there)."""

    return FIXTURES.joinpath(*parts)


def need(*parts):
    """The path of a fixture that must be there; the test skips (naming it, counted) when it is not."""

    path = fixture(*parts)
    if not path.exists():
        raise missing(f"fixture not here: {path} (set DECURIX_TEST_FIXTURES)")
    return path


def _summary():
    if not _missing:
        return
    out = sys.__stderr__ or sys.stderr
    for kind, reasons in sorted(_missing.items()):
        distinct = list(dict.fromkeys(reasons))
        if _required(kind):
            label, head = "FAILED", (f"{len(reasons)} test(s) could not run: {kind} not on this machine, "
                                     f"and DECURIX_REQUIRE_{kind.upper()}=1 requires them")
        else:
            label, head = "SKIPPED, NOT PASSED", (f"{len(reasons)} test(s) did not run: {kind} not on this machine "
                                                  f"(DECURIX_REQUIRE_{kind.upper()}=1 makes this a failure)")
        print("\n" + "=" * 70 + "\n" + label + ": " + head, file=out)
        for reason in distinct[:20]:
            print("  - " + reason, file=out)
        if len(distinct) > 20:
            print(f"  ... and {len(distinct) - 20} more", file=out)
        if os.environ.get("GITHUB_ACTIONS") == "true":
            print(f"::warning title=Tests that did not run ({kind})::{head}", file=out)
    print("=" * 70, file=out)


atexit.register(_summary)
