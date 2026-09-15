#!/usr/bin/env python3
"""Fixtures for the size gate in check-branch.py.

Most cases are refusals, because a refusal is what the gate exists to produce
and what an edit to it can silently remove. The diff sizes are stubbed: the
decision is under test here, and `git diff` is exercised by every real run.

Run: python3 scripts/check-branch-selftest.py
"""

import importlib.util
import pathlib
import sys

# Loading check-branch.py by path would leave a __pycache__ directory in
# scripts/ on every run.
sys.dont_write_bytecode = True

HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("check_branch", HERE / "check-branch.py")
cb = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cb)

BIG = {"electrumx/client.go": (700, 300), "electrumx/client_test.go": (900, 0)}
SMALL = {"deposit/monitor.go": (60, 40)}
TESTS_ONLY = {"electrumx/push_test.go": (2000, 0)}

REASON = ("the connection layer, the refresher and the policy are one mechanism "
          "and no smaller cut leaves a working tree")

CASES = [
    # (name, sizes, body, environment override, passes)
    ("a small change needs nothing", SMALL, None, False, True),
    ("test lines are not counted", TESTS_ONLY, None, False, True),
    ("an oversize change fails", BIG, None, False, False),
    ("a local run may override before the body exists", BIG, None, True, True),
    ("a body without the marker fails, whatever the environment says",
     BIG, "An ordinary body.", True, False),
    ("a bare marker fails", BIG, "ALLOW_LARGE_DIFF:", True, False),
    ("a marker with a word behind it fails", BIG, "ALLOW_LARGE_DIFF: big", True, False),
    ("a marker with a reason passes", BIG, f"Intro.\n\nALLOW_LARGE_DIFF: {REASON}\n", False, True),
]


def main() -> int:
    failures = []
    for name, sizes, body, env, expected_pass in CASES:
        cb.numstat = lambda _range, s=sizes: s
        if env:
            cb.os.environ["ALLOW_LARGE_DIFF"] = "1"
        else:
            cb.os.environ.pop("ALLOW_LARGE_DIFF", None)
        problems = cb.check_size("origin/main", cb.DEFAULT_MAX_LINES, body)
        passed = not problems
        if passed != expected_pass:
            failures.append(f"{name}: expected {'pass' if expected_pass else 'failure'}, "
                            f"got {'pass' if passed else problems[0]}")
        print(f"{'OK  ' if passed == expected_pass else 'FAIL'}  {name}")
    cb.os.environ.pop("ALLOW_LARGE_DIFF", None)
    if failures:
        print("\ncheck-branch-selftest: " + "\n".join(failures), file=sys.stderr)
        return 1
    print(f"\ncheck-branch-selftest: {len(CASES)} cases OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
