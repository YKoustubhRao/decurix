# ============================================================
# Agent parser
# ============================================================

def parse_agent_response(response):
    """
    Parse the agent's control signal.

    The model is instructed to put COMMAND, DONE, or HELP
    first, but small models sometimes add text before it.
    We therefore search for the control signal rather than
    requiring it to be the first characters.
    """

    response = response.strip()

    # Prefer the first control signal that appears.
    command_pos = response.find("COMMAND:")
    done_pos = response.find("DONE:")
    help_pos = response.find("HELP:")

    positions = [
        (pos, "command")
        for pos in [command_pos]
        if pos != -1
    ] + [
        (pos, "done")
        for pos in [done_pos]
        if pos != -1
    ] + [
        (pos, "help")
        for pos in [help_pos]
        if pos != -1
    ]

    if not positions:
        return ("invalid", response)

    # Use whichever control signal appears first.
    _, action = min(positions, key=lambda x: x[0])

    if action == "command":
        value = response[
            command_pos + len("COMMAND:"):
        ].strip()

        return ("command", value)

    if action == "done":
        value = response[
            done_pos + len("DONE:"):
        ].strip()

        return ("done", value)

    if action == "help":
        value = response[
            help_pos + len("HELP:"):
        ].strip()

        return ("help", value)

    return ("invalid", response)
