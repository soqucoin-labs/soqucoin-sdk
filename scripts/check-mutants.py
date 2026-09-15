#!/usr/bin/env python3
"""Run the declared mutants and require each one's test to catch it.

A test that cannot fail is not coverage. Each entry in mutants.json names one
behaviour change and the single test that must notice it. For every entry the
script:

  1. runs the test unmutated and requires it to pass, so a test that is already
     red cannot be scored as catching anything;
  2. applies the mutation;
  3. runs the test again and requires it to fail;
  4. puts the file back.

Step 4 rewrites the file from a copy held in memory rather than from version
control, so uncommitted work in a mutated file cannot be destroyed.

A mutant must leave the program in a defined state. Neutering a guard so that
the code below it runs on nonsense makes the failure a property of the
nonsense, not of the line, and such an entry proves nothing: change behaviour,
not well-formedness.

Usage:
  scripts/check-mutants.py                 # every entry
  scripts/check-mutants.py --id some-id    # one entry
  scripts/check-mutants.py --list
"""

import argparse
import json
import pathlib
import signal
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
MANIFEST = ROOT / "scripts" / "mutants.json"

# Files currently mutated, so a signal puts them back before exit.
_in_flight: dict[pathlib.Path, str] = {}


def _put_back_all() -> None:
    for path, original in list(_in_flight.items()):
        path.write_text(original)
        del _in_flight[path]


def _on_signal(signum, _frame):
    _put_back_all()
    sys.exit(128 + signum)


def run_test(package: str, test: str, tags: str = "") -> tuple[bool, str]:
    cmd = ["go", "test"]
    if tags:
        cmd += ["-tags", tags]
    cmd += [package, "-run", f"^{test}$", "-count=1", "-v"]
    proc = subprocess.run(cmd, capture_output=True, text=True, cwd=ROOT)
    output = (proc.stdout + proc.stderr).strip()
    # `go test -run` exits 0 when the pattern matches nothing, so a misspelled
    # or tag-hidden test would score as passing both runs. Report it as absent.
    ran = f"=== RUN   {test}\n" in proc.stdout or f"=== RUN   {test}/" in proc.stdout
    if not ran:
        return False, f"no test named {test} ran in {package}" + (
            f" with -tags {tags}" if tags else "") + f"\n{output[-400:]}"
    return proc.returncode == 0, output


def check(entry: dict) -> tuple[str, str]:
    """Return (status, message) where status is ok, fail or skip."""
    path = ROOT / entry["file"]
    if not path.exists():
        return "skip", f"{entry['file']} is not on this branch"
    original = path.read_text()
    find, replace = entry["find"], entry["replace"]
    occurrences = original.count(find)
    if occurrences == 0:
        # The manifest is shared by branches that stack. An entry for a
        # mechanism a branch does not carry yet is not a failure here; it
        # starts running on the branch that introduces the line.
        return "skip", f"the line is not in {entry['file']} on this branch"
    if occurrences > 1:
        return "fail", (
            f"the text to mutate occurs {occurrences} times in {entry['file']}; "
            "it must occur exactly once so the mutation is unambiguous"
        )

    tags = entry.get("tags", "")
    ok, output = run_test(entry["package"], entry["test"], tags)
    if not ok:
        return "fail", f"{entry['test']} does not pass unmutated:\n{output[-600:]}"

    _in_flight[path] = original
    try:
        path.write_text(original.replace(find, replace, 1))
        caught, _ = run_test(entry["package"], entry["test"], tags)
    finally:
        path.write_text(original)
        _in_flight.pop(path, None)

    if caught:
        return "fail", (
            f"{entry['test']} still passes with the mutation applied, so it does "
            f"not pin this behaviour.\n  mutation: {find!r} -> {replace!r}"
        )
    return "ok", ""


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--id", action="append", help="run only these entries")
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args()

    entries = json.loads(MANIFEST.read_text())["mutants"]
    if args.id:
        wanted = set(args.id)
        entries = [e for e in entries if e["id"] in wanted]
        missing = wanted - {e["id"] for e in entries}
        if missing:
            print(f"check-mutants: no such id: {', '.join(sorted(missing))}", file=sys.stderr)
            return 1

    if args.list:
        for e in entries:
            print(f"{e['id']:<44} {e['package']} {e['test']}")
        return 0

    signal.signal(signal.SIGINT, _on_signal)
    signal.signal(signal.SIGTERM, _on_signal)

    failures, caught, skipped = [], 0, []
    for e in entries:
        status, why = check(e)
        print(f"{status.upper():<5} {e['id']}" + (f": {why}" if status == "skip" else ""))
        if status == "ok":
            caught += 1
        elif status == "skip":
            skipped.append(e)
        else:
            failures.append((e, why))

    print()
    if failures:
        for e, why in failures:
            print(f"--- {e['id']} ({e['file']})")
            print(f"    why it exists: {e['why']}")
            print(f"    {why}")
        print(f"\ncheck-mutants: {len(failures)} not caught, {caught} caught, "
              f"{len(skipped)} skipped")
        return 1
    print(f"check-mutants: {caught} caught, {len(skipped)} skipped")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    finally:
        _put_back_all()
