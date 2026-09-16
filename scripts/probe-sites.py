#!/usr/bin/env python3
"""Ask, for a list of candidate guard sites, whether anything in the tree pins them.

check-mutants.py answers a narrower question: does the one named test catch the
one named change. This asks the enumeration question instead. Given a site and a
behaviour change at it, does `go test ./...` over every package go red? A site
nothing turns red is a site the tree states in code and pins nowhere.

A red site is the answer "pinned", so a tree that is already red answers
"pinned" for every site and the instrument agrees with itself. The unmutated
tree is run once first and a red one refuses the run, which is what
check-mutants.py establishes per entry before it scores a catch.

Not a gate and not part of `make gates`: a scratch instrument for a reading.

It mutates tracked source in place, so it takes the same precaution
check-mutants.py does. The file's original text goes to a recovery record before
the mutation and the record is removed once the file is back, so a run killed in
between leaves a marker on disk rather than an injected defect that reads as
ordinary source. Signals restore the file before exiting, and a leftover record
stops the next run until it is dealt with.

  scripts/probe-sites.py sites.json
"""

import json
import pathlib
import signal
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
RECOVERY = ROOT / ".probe-sites-recovery.json"

_in_flight: dict[pathlib.Path, str] = {}


def _restore_all() -> None:
    for path, original in list(_in_flight.items()):
        path.write_text(original)
        _in_flight.pop(path, None)
    RECOVERY.unlink(missing_ok=True)


def _on_signal(signum, _frame):
    _restore_all()
    sys.exit(128 + signum)


def run_suite() -> tuple[bool, str]:
    """(everything passed, first failing package or the build error)."""
    proc = subprocess.run(
        ["go", "test", "./..."],
        capture_output=True, text=True, cwd=ROOT, timeout=900,
    )
    if proc.returncode == 0:
        return True, ""
    lines = [l for l in (proc.stdout + proc.stderr).splitlines()
             if l.startswith("FAIL") or l.startswith("---") or "cannot" in l or "undefined" in l]
    return False, "; ".join(lines[:4])


def main() -> int:
    if RECOVERY.exists():
        print(f"a previous run was killed mid-mutation; {RECOVERY} holds the original text.\n"
              f"put the file back and remove the record before running again.", file=sys.stderr)
        return 2
    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, _on_signal)

    sites = json.loads(pathlib.Path(sys.argv[1]).read_text())
    ok, detail = run_suite()
    if not ok:
        print(f"the unmutated tree is red, so every site would read as pinned: {detail}",
              file=sys.stderr)
        return 2
    unpinned = []
    for s in sites:
        path = ROOT / s["file"]
        original = path.read_text()
        if original.count(s["find"]) != 1:
            print(f"SKIP  {s['id']}: `find` occurs {original.count(s['find'])} times")
            continue
        RECOVERY.write_text(json.dumps({"file": s["file"], "original": original}))
        _in_flight[path] = original
        try:
            # Inside the try: a write that raises part way through leaves the
            # file mutated, and the restore below is what puts it back.
            path.write_text(original.replace(s["find"], s["replace"]))
            ok, detail = run_suite()
        finally:
            path.write_text(original)
            _in_flight.pop(path, None)
            RECOVERY.unlink(missing_ok=True)
        if ok:
            print(f"UNPINNED  {s['id']}")
            unpinned.append(s["id"])
        else:
            print(f"pinned    {s['id']}  ({detail[:110]})")
    print(f"\n{len(sites)} sites, {len(unpinned)} pinned by nothing in ./...")
    for u in unpinned:
        print(f"  {u}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
