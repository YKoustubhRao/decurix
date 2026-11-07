import sys

from decurix_agent.computer.cli import main

# `gui __screen_call NAME`: one of the agent's own screen calls, handed to the screen broker in broker mode
# (screen_call.py). Not a command for an engine: it is not in `gui help`, and it takes only what screen_call.SERVED does.
if sys.argv[1:2] == ["__screen_call"]:
    from decurix_agent.computer import screen_call
    sys.exit(screen_call.serve(sys.argv[2:]))

sys.exit(main())
