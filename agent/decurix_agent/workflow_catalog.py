"""Editable examples loaded as data, using the ordinary custom-spec contract.

No task-specific execution logic lives here. Any valid workflow can run
without appearing in the example catalog.
"""
import copy
import json
from pathlib import Path


def load_catalog(path=None):
    from decurix_agent.workflow_runner import validate
    definitions = json.loads(Path(path or Path(__file__).with_name("workflow_examples.json")).read_text(encoding="utf-8"))
    if not isinstance(definitions, list):
        raise ValueError("workflow catalog must be a list")
    catalog = {}
    for spec in definitions:
        validate(spec)
        if spec["id"] in catalog:
            raise ValueError("duplicate workflow example id")
        catalog[spec["id"]] = spec
    return catalog


CATALOG = load_catalog()


def get(workflow_id):
    return copy.deepcopy(CATALOG[workflow_id])
