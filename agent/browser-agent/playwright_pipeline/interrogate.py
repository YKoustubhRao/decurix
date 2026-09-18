"""Read-only Q&A about the current page state -- no code generation, no actions.
Thin wrapper around reasoning.answer_from_page (the same call the REASONING
routing path uses), just returning the snapshot too so the UI can show what
page the answer came from."""
import _pathfix  # noqa: F401
import models
import reasoning
import runner


def ask(cdp, question, model_key=models.DEFAULT_MODEL, vision=False):
    use_vision = vision and models.MODELS[model_key]["vision"]
    snap = runner.run_worker(cdp, "snapshot", vision=use_vision)

    answer = reasoning.answer_from_page(
        model_key, question, snap["url"], snap["html"],
        screenshot_b64=snap.get("screenshot_b64") if use_vision else None,
        max_tokens=300,
    )
    return answer, snap
