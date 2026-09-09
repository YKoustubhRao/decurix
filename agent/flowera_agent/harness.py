from flowera_agent import config

# The only place that branches on which reasoning engine drives tasks.
# Both branches expose perform_task(task, task_file) and
# continue_after_help(help_response) with identical signatures, so
# watchers.py's command_watcher() doesn't need to know which is active.

if config.AGENT_HARNESS == "claude":
    from flowera_agent.claude_harness import perform_task, continue_after_help
else:
    from flowera_agent.task_runner import perform_task, continue_after_help

__all__ = ["perform_task", "continue_after_help"]
