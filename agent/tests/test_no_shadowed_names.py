"""
A module-level name bound twice in one module: the later binding silently replaces the earlier one for every caller.
Found twice in one day (2026-10-20/21): long_task's maturity constant BLOCKING replaced its "ask me" regex of that
name (every check became advisory), and a second _CHANGE_VERB replaced the kept-sheets check's (it stopped seeing
"update" as a change). Only a top-level statement's own binding counts: a name bound in both branches of an `if` or a
`try`/`except` (an import fallback, a platform branch) is one choice, not two.

    PYTHONPATH="agent;agent/tests" python -m unittest test_no_shadowed_names
"""

import ast
import unittest
from pathlib import Path

PACKAGE = Path(__file__).resolve().parents[1] / "decurix_agent"

# Deliberate re-bindings, each with its reason: {(module path relative to the package, name): reason}.
ALLOWED = {
}


def _bound(statement):
    """The names a top-level statement binds directly (not inside an if/try/with/for/while block)."""

    if isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
        return [statement.name]
    targets = []
    if isinstance(statement, ast.Assign):
        targets = statement.targets
    elif isinstance(statement, (ast.AnnAssign, ast.AugAssign)) and statement.value is not None:
        targets = [statement.target] if isinstance(statement, ast.AnnAssign) else []
    names = []
    for target in targets:
        for node in ast.walk(target):
            if isinstance(node, ast.Name):
                names.append(node.id)
    return names


def shadowed(path):
    """[(name, first line, later line)] for each module-level name bound twice in this file."""

    tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    seen, out = {}, []
    for statement in tree.body:
        for name in _bound(statement):
            if name in seen and name != "__all__":
                out.append((name, seen[name], statement.lineno))
            seen.setdefault(name, statement.lineno)
    return out


class NoShadowedNames(unittest.TestCase):

    def test_no_module_binds_a_top_level_name_twice(self):
        found = []
        for path in sorted(PACKAGE.rglob("*.py")):
            rel = path.relative_to(PACKAGE).as_posix()
            for name, first, later in shadowed(path):
                if (rel, name) not in ALLOWED:
                    found.append(f"{rel}: {name} bound at line {first} and again at line {later}")
        self.assertEqual(found, [], "a later binding replaces the earlier one for every caller")

    def test_it_sees_a_shadowed_name(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp) / "m.py"
            p.write_text("import re\nA = re.compile('x')\n\ndef f():\n    return A\n\nA = 'y'\n"
                         "try:\n    import json\nexcept ImportError:\n    json = None\n", encoding="utf-8")
            self.assertEqual(shadowed(p), [("A", 2, 7)])


if __name__ == "__main__":
    unittest.main()
