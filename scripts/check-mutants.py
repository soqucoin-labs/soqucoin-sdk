#!/usr/bin/env python3
"""Run the declared mutants and require each one's test to catch it.

A test that cannot fail is not coverage. Each entry in mutants.json names one
behaviour change and the single test that must notice it. For every entry the
script:

  1. runs the test unmutated and requires it to pass, so a test that is already
     red cannot be scored as catching anything;
  2. applies the mutation;
  3. runs the test again and requires it to have run and to fail: a mutated
     tree that does not build proves nothing, so that is a failure, not a catch;
  4. puts the file back.

Step 4 rewrites the file from a copy held in memory rather than from version
control, so the run does not discard uncommitted work in a mutated file. That
copy also goes to a recovery record before the mutation and is removed once the
file is back, so a run killed in between is detected rather than mistaken for a
branch that does not carry the line. The record outlives the run that reports
it, because the reader needs it to put the file back.

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
# Written before a file is mutated and removed once it is back, so a run killed
# in between leaves a record on disk. Without one the next run reads the mutated
# text, matches no `find`, and reports a skip and exit 0 with the injected
# defect live in the working copy.
RECOVERY = ROOT / ".check-mutants-recovery.json"

# Files currently mutated, so a signal puts them back before exit.
_in_flight: dict[pathlib.Path, str] = {}


def _branch() -> str:
    """The current branch, or "" when git cannot say. Only ever for a message."""
    try:
        proc = subprocess.run(
            ["git", "rev-parse", "--abbrev-ref", "HEAD"],
            capture_output=True, text=True, cwd=ROOT,
        )
        return proc.stdout.strip() if proc.returncode == 0 else ""
    except OSError:
        return ""


def _put_back_all() -> None:
    """Put back what this run mutated, and drop only this run's own record.

    The early return matters: this runs on every exit path, including the one
    that refuses because an earlier run left a record behind. Dropping the
    record there would disarm the guard for the next run and destroy the only
    saved copy of the text the refusal message points at.
    """
    if not _in_flight:
        return
    for path, original in list(_in_flight.items()):
        path.write_text(original)
        del _in_flight[path]
    RECOVERY.unlink(missing_ok=True)


def _recovery_report() -> list[str]:
    """Refuse to run while a killed run's mutation may still be in the tree.

    Every unreadable shape is reported rather than raised: a record of the
    wrong type, a missing key and a directory in its place all arrive here,
    and a traceback would say less than the message does.
    """
    try:
        rec = json.loads(RECOVERY.read_text())
        name, ident, saved = rec["file"], rec["id"], rec["original"]
        branch = rec.get("branch", "")
    except (OSError, ValueError, TypeError, KeyError):
        return [
            f"{RECOVERY.name} exists but cannot be read.",
            "  it is left in place; inspect it by hand and delete it.",
        ]
    path = ROOT / name
    current = path.read_text() if path.exists() else ""
    if current == saved:
        RECOVERY.unlink(missing_ok=True)
        return []
    on = f" on {branch}" if branch else ""
    return [
        f"a previous run{on} was killed while {name} was mutated for {ident}, "
        "and the file no longer holds what it held before.",
        f"  the text it held is saved under \"original\" in {RECOVERY.name},",
        "  which is left in place. Put that text back, delete the record, and",
        "  run again. This run is refused rather than reporting on a mutated",
        "  tree.",
    ] + ([f"  the record was written on {branch}; you are not on it now."]
         if branch and branch != _branch() else [])


def _on_signal(signum, _frame):
    _put_back_all()
    sys.exit(128 + signum)


def run_test(package: str, test: str, tags: str = "") -> tuple[bool, bool, str]:
    """Return (ran, passed, output).

    `ran` is its own answer because "the test did not run" and "the test
    failed" are the same exit status to `go test`, and they mean opposite
    things on the two sides of a mutation. `go test -run` also exits 0 when
    the pattern matches nothing, so a misspelled or tag-hidden test would
    otherwise score as passing.
    """
    cmd = ["go", "test"]
    if tags:
        cmd += ["-tags", tags]
    cmd += [package, "-run", f"^{test}$", "-count=1", "-v"]
    proc = subprocess.run(cmd, capture_output=True, text=True, cwd=ROOT)
    output = (proc.stdout + proc.stderr).strip()
    ran = f"=== RUN   {test}\n" in proc.stdout or f"=== RUN   {test}/" in proc.stdout
    return ran, proc.returncode == 0, output


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
    ran, passed, output = run_test(entry["package"], entry["test"], tags)
    if not ran:
        return "fail", (
            f"no test named {entry['test']} ran in {entry['package']}"
            + (f" with -tags {tags}" if tags else "")
            + f"\n{output[-600:]}"
        )
    if not passed:
        return "fail", f"{entry['test']} does not pass unmutated:\n{output[-600:]}"

    _in_flight[path] = original
    try:
        RECOVERY.write_text(json.dumps({
            "id": entry["id"], "file": entry["file"],
            "branch": _branch(), "original": original,
        }))
        path.write_text(original.replace(find, replace, 1))
        ran, passed, output = run_test(entry["package"], entry["test"], tags)
    finally:
        path.write_text(original)
        _in_flight.pop(path, None)
        RECOVERY.unlink(missing_ok=True)

    if not ran:
        # The mutated tree did not build, or the test vanished with it. The run
        # proves nothing: a mutation must change behaviour, not well-formedness.
        return "fail", (
            f"the mutated tree did not run {entry['test']}, so the mutation "
            "changes well-formedness rather than behaviour and proves nothing."
            f"\n  mutation: {find!r} -> {replace!r}\n{output[-600:]}"
        )
    if passed:
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

    if RECOVERY.exists():
        stale = _recovery_report()
        if stale:
            print("check-mutants: refusing to run\n", file=sys.stderr)
            for line in stale:
                print(line, file=sys.stderr)
            return 1

    for name in ("SIGINT", "SIGTERM", "SIGHUP"):
        if hasattr(signal, name):
            signal.signal(getattr(signal, name), _on_signal)

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
