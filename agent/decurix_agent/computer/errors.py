class ComputerError(Exception):
    """
    Something on the screen could not be seen or done, worded for whoever
    asked: an engine reads this as the result of its action, so it says what
    went wrong and what to try next rather than where in the code it failed.
    """


class ActedError(ComputerError):
    """
    The action was carried out, and something after it went wrong (what it typed did not hold, the connection broke
    part way). A replay that meets one must not do the step again as if nothing happened (replay._Halt.acted).
    """
