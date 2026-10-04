"""
The arguments of the `gui` and `office` commands, however they were passed.

On a Linux desktop they are the command line. A native Windows worker's
commands run in Windows PowerShell 5.1, which loses the double quotes inside
an argument it hands to a program - `gui click 'button "Save"'` arrived as
`button Save`. So there the commands are PowerShell functions (TheBot's
internal/hostshell) that put their arguments, exactly as PowerShell parsed
them, into DECURIX_CLI_ARGS, separated by a character no argument contains.
"""

import os
import sys

VARIABLE = "DECURIX_CLI_ARGS"
SEPARATOR = "\x1f"


def from_environment(argv=None):
    """`argv` when one is given; else DECURIX_CLI_ARGS when it is set; else the command line."""

    if argv is not None:
        return list(argv)

    passed = os.environ.get(VARIABLE)
    if passed is not None:
        return passed.split(SEPARATOR) if passed else []

    return sys.argv[1:]
