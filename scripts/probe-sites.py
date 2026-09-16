#!/usr/bin/env python3
"""Ask, for a list of candidate guard sites, whether anything in the tree pins them.

The declared-mutant runner answers a narrower question: does the one named test
catch this one named change. This asks the enumeration question instead. Given a
site and a behaviour change at it, does `go test ./...` over every package go red?
A site nothing turns red is a site the tree states in code and pins nowhere.

Not a gate and not committed to the manifest: a scratch instrument for a reading.

  scripts/probe-sites.py sites.json
"""

import json
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent


def run_suite() -> tuple[bool, str]:
    """(everything passed, first failing package or the build error)."""
    proc = subprocess.run(
        ["go", "test", "./..."],
        capture_output=True, text=True, cwd=ROOT,
    )
    if proc.returncode == 0:
        return True, ""
    lines = [l for l in (proc.stdout + proc.stderr).splitlines()
             if l.startswith("FAIL") or l.startswith("---") or "cannot" in l or "undefined" in l]
    return False, "; ".join(lines[:4])


def main() -> int:
    sites = json.loads(pathlib.Path(sys.argv[1]).read_text())
    unpinned = []
    for s in sites:
        path = ROOT / s["file"]
        original = path.read_text()
        if original.count(s["find"]) != 1:
            print(f"SKIP  {s['id']}: `find` occurs {original.count(s['find'])} times")
            continue
        path.write_text(original.replace(s["find"], s["replace"]))
        try:
            ok, detail = run_suite()
        finally:
            path.write_text(original)
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
