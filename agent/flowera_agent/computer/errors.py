class ComputerError(Exception):
    """
    Something on the screen could not be seen or done, worded for whoever
    asked: an engine reads this as the result of its action, so it says what
    went wrong and what to try next rather than where in the code it failed.
    """
