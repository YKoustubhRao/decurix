"""
What a person answers a worker's question is kept, and the worker does not ask it again - end to end.

Found live (2026-10-04): "always choose first", answered while a task was running, reached that task
and nothing else; the next task met the same doubt and asked again.

    python bench/native/lessons_e2e.py --base http://127.0.0.1:8180 --desktop msg1

What it does, through the chat API:
  1. asks for something only the person knows (their message sign-off, written into a file): the
     worker must stop and ask, and with the answer do it;
  2. the answer must then be in the worker's memory as a lesson (category "lesson");
  3. asks for the same thing again: the worker must do it without asking.

Needs a control plane on this computer with a hosted brain set up and a running native worker (its
files are read from its home folder). Takes about two minutes.
"""

import argparse
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from messaging_e2e import ControlPlane  # noqa: E402

SIGN_OFF = "Warm regards, Koustubh"
REQUEST = ("Use the shell to write my usual message sign-off into the file signoff.txt in your work folder. "
           "Only I know what my sign-off is, so ask me for it first.")


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--base", default="http://127.0.0.1:8080")
    parser.add_argument("--desktop", required=True)
    args = parser.parse_args()

    cp = ControlPlane(args.base, args.desktop)
    home = cp.home()
    memory, note = home / ".decurix" / "memory", home / "work" / "signoff.txt"
    failures = []

    def check(ok, what):
        print(("  ok    " if ok else "  FAIL  ") + what)
        if not ok:
            failures.append(what)

    def lessons():
        found = []
        for path in sorted(memory.glob("*.txt")):
            text = path.read_text(encoding="utf-8", errors="replace")
            if "Category: lesson" in text:
                found.append(text)
        return found

    def run(replies):
        cp.idle()
        note.unlink(missing_ok=True)
        asked, entry = [], cp.send(REQUEST)
        for _ in range(4):
            if entry.get("kind") not in ("query", "approval"):
                break
            asked.append(entry["content"])
            if not replies:
                break
            entry = cp.send(replies.pop(0))
        return entry, asked

    def written():
        return note.exists() and SIGN_OFF in note.read_text(encoding="utf-8", errors="replace")

    if any(SIGN_OFF in text for text in lessons()):
        raise SystemExit(f"this worker already knows the sign-off: delete that lesson from {memory} first")

    print("1. Something only the person knows: the worker asks")
    before = len(lessons())
    entry, asked = run([f"always '{SIGN_OFF}' - that is my sign-off, remember it"])
    check(len(asked) == 1, f"it asked once ({len(asked)} question(s))")
    check(written(), f"and wrote the answer into the file ({entry.get('content', '')[:100]!r})")

    print("2. The answer is kept")
    deadline = time.time() + 60
    while time.time() < deadline and len(lessons()) == before:
        time.sleep(1)
    kept = lessons()[before:]
    check(any(SIGN_OFF in text for text in kept), f"a lesson in the worker's memory holds it: {kept}")

    print("3. Asked for the same thing again")
    entry, asked = run([])
    check(not asked, f"it does not ask ({len(asked)} question(s): {asked})")
    check(written(), f"and the file holds what it was told last time ({entry.get('content', '')[:100]!r})")

    print("\n" + ("PASSED" if not failures else f"FAILED: {len(failures)}"))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
