#!/usr/bin/env python3
"""Mechanical checks on a branch before its pull request opens.

Neither check is about taste. Both answer in a second a question a reading can
miss.

1. The branch is current with main, decided by comparing the patch text of the
   two diffs rather than the ancestry or their line counts. `git merge-base --is-ancestor` is the obvious check and it is
   wrong whenever main squash-merges: the squash commit is not an ancestor of
   the branch, and the branch's own copy of that work is not an ancestor of
   main, so the ancestry test passes on a branch that is stale. What matters is
   that the diff a reviewer is shown (three dot, from the merge base) is the
   diff that will land (two dot, against main's tip). When the two differ, the
   branch is behind and part of what is on screen is already on main.

2. The change is one mechanism, measured in lines of non-test Go. Set
   ALLOW_LARGE_DIFF=1 to override, and say in the pull request body why the
   change is not separable.

Exit status is 0 when every check passes, 1 otherwise.
"""

import argparse
import os
import subprocess
import sys

DEFAULT_MAX_LINES = 600


def git(*args: str) -> str:
    return subprocess.run(
        ["git", *args], capture_output=True, text=True, check=True
    ).stdout


def numstat(rev_range: str) -> dict[str, tuple[int, int]]:
    """Return {path: (added, removed)} for a diff range."""
    out: dict[str, tuple[int, int]] = {}
    for line in git("diff", "--numstat", rev_range).splitlines():
        parts = line.split("\t")
        if len(parts) != 3:
            continue
        added, removed, path = parts
        if added == "-" or removed == "-":
            continue  # binary
        out[path] = (int(added), int(removed))
    return out


def patch(rev_range: str, *paths: str) -> str:
    """The full patch text for a range. Binary files appear as a marker line."""
    return git("diff", "--no-color", rev_range, "--", *paths)


def touched(rev_range: str) -> set[str]:
    """Every path in a diff, including the ones numstat reports as binary."""
    return set(git("diff", "--name-only", rev_range).splitlines())


def is_non_test_go(path: str) -> bool:
    return path.endswith(".go") and not path.endswith("_test.go")


def check_current(base: str) -> list[str]:
    """The rendered diff must be the diff that lands.

    Compared as patch text. Line counts are not enough: a branch that edits a
    line main has also edited shows 1 added and 1 removed on both sides while
    the content differs, and landing it reverts main. Counts also drop the
    files git reports as binary, which hides a branch stale only in those.
    """
    if patch(f"{base}...HEAD") == patch(f"{base}..HEAD"):
        return []
    three, two = numstat(f"{base}...HEAD"), numstat(f"{base}..HEAD")
    three_p, two_p = touched(f"{base}...HEAD"), touched(f"{base}..HEAD")
    extra = sorted(three_p - two_p)
    changed = sorted(
        p for p in three_p & two_p
        if patch(f"{base}...HEAD", p) != patch(f"{base}..HEAD", p)
    )
    lines = [
        f"the branch is behind {base}: what a reviewer sees is not what will land.",
        f"  rendered (three dot): {len(three_p)} files, "
        f"+{sum(a for a, _ in three.values())}/-{sum(r for _, r in three.values())}",
        f"  landing  (two dot):   {len(two_p)} files, "
        f"+{sum(a for a, _ in two.values())}/-{sum(r for _, r in two.values())}",
    ]
    if extra:
        lines.append(f"  files in the rendered diff only: {', '.join(extra[:8])}"
                     + (" ..." if len(extra) > 8 else ""))
    missing = sorted(two_p - three_p)
    if missing:
        lines.append(f"  files in the landing diff only: {', '.join(missing[:8])}"
                     + (" ..." if len(missing) > 8 else ""))
    if changed:
        lines.append(f"  files whose content differs: {', '.join(changed[:8])}"
                     + (" ..." if len(changed) > 8 else ""))
    lines.append(f"  fix: git merge {base}   (a squash merge on {base} makes an "
                 "ancestry check useless; the trees are what matter)")
    return lines


def check_size(base: str, max_lines: int) -> list[str]:
    """One mechanism per pull request, measured in non-test Go."""
    two = numstat(f"{base}..HEAD")
    total = sum(a + r for p, (a, r) in two.items() if is_non_test_go(p))
    if total <= max_lines:
        return []
    if os.environ.get("ALLOW_LARGE_DIFF") == "1":
        print(f"note: {total} lines of non-test Go, over the {max_lines} budget; "
              "ALLOW_LARGE_DIFF=1 set, say why in the pull request body")
        return []
    biggest = sorted(
        ((a + r, p) for p, (a, r) in two.items() if is_non_test_go(p)), reverse=True
    )
    return [
        f"{total} lines of non-test Go, over the {max_lines} budget: split before opening.",
        "  largest: " + ", ".join(f"{p} ({n})" for n, p in biggest[:5]),
        "  override with ALLOW_LARGE_DIFF=1 and say in the body why it is not separable.",
    ]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="origin/main")
    ap.add_argument("--max-lines", type=int, default=DEFAULT_MAX_LINES)
    args = ap.parse_args()

    try:
        git("rev-parse", "--verify", args.base)
    except subprocess.CalledProcessError:
        print(f"check-branch: {args.base} not found; fetch it first", file=sys.stderr)
        return 1

    problems = check_current(args.base) + check_size(args.base, args.max_lines)
    if not problems:
        print(f"check-branch: OK (current with {args.base}, within the line budget)")
        return 0
    print(f"check-branch: {len([p for p in problems if not p.startswith('  ')])} problem(s)\n")
    for line in problems:
        print(line)
    return 1


if __name__ == "__main__":
    sys.exit(main())
